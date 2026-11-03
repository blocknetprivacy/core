package p2p

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log"
	"math/big"
	"sync"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Dandelion++ parameters
const (
	// StemProbability is the probability of continuing stem phase vs fluffing
	// Higher = longer stem, more privacy, but more latency
	// Paper recommends 0.9 (90% continue stem)
	StemProbability = 0.9

	// EpochDuration is how often stem routing changes
	// Shorter = more privacy, but more routing churn
	EpochDuration = 10 * time.Minute

	// StemTimeout is maximum time a tx can stay in stem phase
	// After this, it automatically fluffs to prevent tx getting stuck
	StemTimeout = 30 * time.Second

	// EmbargoTimeout is how long to wait for tx to return before fluffing
	// If we haven't seen our tx back in this time, assume stem failed
	EmbargoTimeout = 45 * time.Second

	// txRejectWindow and txRejectLimit bound the validation spent on one peer's
	// rejected transactions. Once a peer reaches the limit its new transactions
	// are ignored until the window ends. The connection stays up: relays on older
	// software forward transactions they never validated, and blocks must keep
	// flowing over those links.
	txRejectWindow = 10 * time.Minute
	txRejectLimit  = 32
	// maxTxRejectPeers bounds the tracking map against peer ID churn.
	maxTxRejectPeers = 4096

	// maxPendingStemBytes bounds the transaction data held while waiting to fluff.
	// Only stem records keep their data; everything else needs just the hash.
	maxPendingStemBytes = 256 << 20
)

// TxState tracks the state of a transaction in Dandelion
type TxState int

const (
	TxStateStem  TxState = iota // In stem phase (routing through single peer)
	TxStateFluff                // In fluff phase (broadcast to all)
	TxStateSeen                 // Already seen/processed
)

// txRecord tracks a transaction we're handling
type txRecord struct {
	hash      [32]byte
	data      []byte
	state     TxState
	stemPeer  peer.ID // Who to route to in stem phase
	createdAt time.Time
	fromPeer  peer.ID // Who sent it to us (for stem routing)
}

// txRejects counts one peer's rejected transactions in the current window.
type txRejects struct {
	count      int
	windowEnds time.Time
}

// DandelionRouter implements Dandelion++ transaction routing
type DandelionRouter struct {
	mu sync.RWMutex

	node *Node

	// Epoch-based stem routing
	currentEpoch  int64
	stemNeighbors []peer.ID // Current epoch's stem routing targets
	outboundStem  peer.ID   // Our outbound stem peer for this epoch

	// Transaction tracking
	txCache     map[[32]byte]*txRecord
	txCacheSize int
	txRejects   map[peer.ID]*txRejects
	// stemBytes is the total data held by stem records.
	stemBytes int

	// Handler for fluffed transactions. It reports whether the transaction was
	// accepted; only accepted transactions are relayed.
	onTx func(from peer.ID, data []byte) bool
	// Lightweight stem-phase transaction sanity check callback.
	// If unset, no extra stem validation is performed.
	stemSanityCheck func(data []byte) bool
	// Drops stem payloads that can't be relayed, without blaming the sender.
	// If unset, every payload that passes stemSanityCheck continues.
	stemRelayFilter func(data []byte) bool

	ctx    context.Context
	cancel context.CancelFunc
}

// NewDandelionRouter creates a new Dandelion++ router
func NewDandelionRouter(node *Node) *DandelionRouter {
	return &DandelionRouter{
		node:        node,
		txCache:     make(map[[32]byte]*txRecord),
		txCacheSize: 10000, // Track last 10k transactions
		txRejects:   make(map[peer.ID]*txRejects),
	}
}

// Start begins Dandelion++ operations
func (d *DandelionRouter) Start(ctx context.Context) {
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Initialize first epoch
	d.rotateEpoch()

	// Start background loops
	go d.epochLoop()
	go d.embargoLoop()
}

// Stop halts Dandelion operations
func (d *DandelionRouter) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

// SetTxHandler sets the callback for transactions that reach fluff phase. It
// reports whether the transaction was accepted and may be relayed.
func (d *DandelionRouter) SetTxHandler(handler func(from peer.ID, data []byte) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onTx = handler
}

// SetStemSanityValidator sets a lightweight validator for stem payloads.
// Returning false rejects the stem transaction before cache/relay.
func (d *DandelionRouter) SetStemSanityValidator(validator func(data []byte) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stemSanityCheck = validator
}

// SetStemRelayFilter sets a check for stem payloads a well-behaved older relay
// may still forward. Returning false drops the payload without penalizing the
// sender.
func (d *DandelionRouter) SetStemRelayFilter(filter func(data []byte) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stemRelayFilter = filter
}

// BroadcastTx starts the Dandelion++ process for a local transaction
func (d *DandelionRouter) BroadcastTx(data []byte) {
	hash := sha256.Sum256(data)

	d.mu.Lock()

	if _, exists := d.txCache[hash]; exists {
		d.mu.Unlock()
		return
	}

	rec := &txRecord{
		hash:      hash,
		data:      data,
		state:     TxStateStem,
		createdAt: time.Now(),
		fromPeer:  d.node.PeerID(),
	}

	stemPeer := d.getOutboundStemPeer()
	if stemPeer == "" {
		rec.state = TxStateFluff
		d.txCache[hash] = rec
		d.enforceTxCacheLimitLocked()
		d.mu.Unlock()
		d.fluffTx(rec)
		return
	}

	rec.stemPeer = stemPeer
	d.txCache[hash] = rec
	d.enforceTxCacheLimitLocked()
	d.mu.Unlock()

	d.sendStemAsync(stemPeer, data)
}

// HandleStemStream handles incoming stem phase transactions
func (d *DandelionRouter) HandleStemStream(s network.Stream) {
	defer s.Close()

	// Read transaction data
	data, err := readLengthPrefixedWithLimit(s, MaxDandelionStreamPayloadSize)
	if err != nil {
		return
	}

	fromPeer := s.Conn().RemotePeer()
	d.mu.RLock()
	stemSanityCheck := d.stemSanityCheck
	stemRelayFilter := d.stemRelayFilter
	d.mu.RUnlock()
	if stemSanityCheck != nil && !stemSanityCheck(data) {
		d.node.PenalizePeer(fromPeer, ScorePenaltyInvalid, "invalid stem tx payload")
		return
	}
	if stemRelayFilter != nil && !stemRelayFilter(data) {
		return
	}

	d.handleStemTx(fromPeer, data)
}

// handleStemTx processes a transaction received in stem phase
func (d *DandelionRouter) handleStemTx(from peer.ID, data []byte) {
	hash := sha256.Sum256(data)

	d.mu.Lock()

	if _, exists := d.txCache[hash]; exists {
		d.mu.Unlock()
		return
	}

	randFloat, randErr := cryptoRandFloat64()
	shouldFluff := randErr != nil || randFloat > StemProbability

	rec := &txRecord{
		hash:      hash,
		data:      data,
		createdAt: time.Now(),
		fromPeer:  from,
	}

	if shouldFluff {
		rec.state = TxStateFluff
		d.txCache[hash] = rec
		d.enforceTxCacheLimitLocked()
		d.mu.Unlock()
		d.fluffTx(rec)
	} else {
		rec.state = TxStateStem

		stemPeer := d.getStemPeerExcluding(from)
		if stemPeer == "" {
			rec.state = TxStateFluff
			d.txCache[hash] = rec
			d.enforceTxCacheLimitLocked()
			d.mu.Unlock()
			d.fluffTx(rec)
			return
		}

		rec.stemPeer = stemPeer
		d.txCache[hash] = rec
		d.enforceTxCacheLimitLocked()
		d.mu.Unlock()

		d.sendStemAsync(stemPeer, data)
	}
}

// fluffTx validates a transaction leaving stem phase and, if it is accepted,
// broadcasts it to all peers (fluff phase).
func (d *DandelionRouter) fluffTx(rec *txRecord) {
	d.mu.RLock()
	handler := d.onTx
	d.mu.RUnlock()

	self := d.node.PeerID()
	d.mu.Lock()
	data := rec.data
	from := rec.fromPeer
	rec.data = nil // Fluffed: from here on the record only needs its hash.
	d.mu.Unlock()
	if len(data) == 0 {
		return
	}

	go func() {
		if handler != nil && !handler(self, data) {
			return
		}
		for _, p := range d.node.host.Network().Peers() {
			// Don't send back to sender (if from remote peer)
			if p == from && from != self {
				continue
			}

			d.node.sendToPeerAsync(p, protocolID(params.ProtocolTx), data)
		}
	}()
}

// sendStem sends a transaction to a stem peer
func (d *DandelionRouter) sendStem(p peer.ID, data []byte) error {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()

	s, err := d.node.host.NewStream(ctx, p, protocolID(params.ProtocolDandelion))
	if err != nil {
		// Stem failed, fluff instead
		d.stemFailed(data)
		return err
	}
	defer s.Close()

	if err := writeLengthPrefixed(s, data); err != nil {
		d.stemFailed(data)
		return err
	}

	return nil
}

func (d *DandelionRouter) sendStemAsync(p peer.ID, data []byte) {
	go func(pid peer.ID, payload []byte) {
		if err := d.sendStem(pid, payload); err != nil {
			log.Printf("failed to send stem tx to %s: %v", pid, err)
		}
	}(p, data)
}

// stemFailed handles stem routing failure - transitions to fluff
func (d *DandelionRouter) stemFailed(data []byte) {
	hash := sha256.Sum256(data)

	d.mu.Lock()
	rec, exists := d.txCache[hash]
	if !exists {
		d.mu.Unlock()
		return
	}

	rec.state = TxStateFluff
	d.mu.Unlock()

	d.fluffTx(rec)
}

// getOutboundStemPeer returns the stem peer for outbound transactions
func (d *DandelionRouter) getOutboundStemPeer() peer.ID {
	if d.outboundStem != "" {
		// Verify still connected
		if d.node.host.Network().Connectedness(d.outboundStem) == network.Connected {
			return d.outboundStem
		}
	}

	// Pick a new stem peer
	peers := d.node.host.Network().Peers()
	if len(peers) == 0 {
		return ""
	}

	idx, err := cryptoRandIntn(len(peers))
	if err != nil {
		d.outboundStem = peers[0]
		return d.outboundStem
	}
	d.outboundStem = peers[idx]
	return d.outboundStem
}

// getStemPeerExcluding returns a stem peer excluding a specific peer
func (d *DandelionRouter) getStemPeerExcluding(exclude peer.ID) peer.ID {
	peers := d.node.host.Network().Peers()

	// Filter out excluded peer
	candidates := make([]peer.ID, 0, len(peers))
	for _, p := range peers {
		if p != exclude {
			candidates = append(candidates, p)
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	idx, err := cryptoRandIntn(len(candidates))
	if err != nil {
		return candidates[0]
	}
	return candidates[idx]
}

// rotateEpoch changes stem routing for a new epoch
func (d *DandelionRouter) rotateEpoch() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.currentEpoch = time.Now().Unix() / int64(EpochDuration.Seconds())

	// Clear old stem peer - will be reselected on next tx
	d.outboundStem = ""

	// Build stem neighbor set from current peers
	peers := d.node.host.Network().Peers()
	d.stemNeighbors = make([]peer.ID, 0, len(peers))

	for _, p := range peers {
		// Each peer has 50% chance of being a stem neighbor
		// This provides anonymity set while limiting routing graph complexity
		randFloat, err := cryptoRandFloat64()
		if err != nil {
			if len(p) > 0 && p[len(p)-1]%2 == 0 {
				d.stemNeighbors = append(d.stemNeighbors, p)
			}
			continue
		}
		if randFloat < 0.5 {
			d.stemNeighbors = append(d.stemNeighbors, p)
		}
	}
}

// epochLoop rotates stem routing periodically
func (d *DandelionRouter) epochLoop() {
	ticker := time.NewTicker(EpochDuration)
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.rotateEpoch()
		}
	}
}

// embargoLoop checks for stuck transactions and fluffs them
func (d *DandelionRouter) embargoLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.checkEmbargoes()
		}
	}
}

// checkEmbargoes fluffs transactions that have been in stem too long
func (d *DandelionRouter) checkEmbargoes() {
	d.mu.Lock()

	now := time.Now()
	var toFluff []*txRecord

	for hash, rec := range d.txCache {
		if rec.state == TxStateStem {
			if now.Sub(rec.createdAt) > EmbargoTimeout {
				rec.state = TxStateFluff
				toFluff = append(toFluff, rec)
			}
		}

		// Cleanup very old entries
		if now.Sub(rec.createdAt) > 30*time.Minute {
			delete(d.txCache, hash)
		}
	}

	d.mu.Unlock()

	// Fluff outside the lock
	for _, rec := range toFluff {
		d.fluffTx(rec)
	}
}

// HandleFluffTx processes a transaction received in fluff phase
func (d *DandelionRouter) HandleFluffTx(from peer.ID, data []byte) {
	hash := sha256.Sum256(data)

	d.mu.Lock()

	// Check if already seen
	if rec, exists := d.txCache[hash]; exists {
		// If we sent this in stem and now see it in fluff, it made it!
		if rec.state == TxStateStem && rec.fromPeer == d.node.PeerID() {
			rec.state = TxStateSeen
		}
		d.mu.Unlock()
		return
	}

	// Left out of the cache so the same transaction from another peer still counts.
	if d.txRejectLimitReachedLocked(from, time.Now()) {
		d.mu.Unlock()
		return
	}

	// New transaction in fluff phase
	rec := &txRecord{
		hash:      hash,
		state:     TxStateSeen,
		createdAt: time.Now(),
		fromPeer:  from,
	}
	d.txCache[hash] = rec
	d.enforceTxCacheLimitLocked()

	handler := d.onTx
	d.mu.Unlock()

	// Only an accepted transaction moves on.
	if handler != nil && !handler(from, data) {
		d.noteTxRejected(from, time.Now())
		return
	}

	// Rebroadcast to peers (except sender)
	d.rebroadcast(from, data)
}

// txRejectLimitReachedLocked reports whether a peer has used up its rejected
// transactions for the current window. Caller must hold d.mu.
func (d *DandelionRouter) txRejectLimitReachedLocked(p peer.ID, now time.Time) bool {
	rejects, ok := d.txRejects[p]
	if !ok {
		return false
	}
	if !now.Before(rejects.windowEnds) {
		delete(d.txRejects, p)
		return false
	}
	return rejects.count >= txRejectLimit
}

// noteTxRejected counts a rejected transaction against the peer that sent it.
func (d *DandelionRouter) noteTxRejected(p peer.ID, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rejects, ok := d.txRejects[p]
	if !ok || !now.Before(rejects.windowEnds) {
		if !ok && len(d.txRejects) >= maxTxRejectPeers {
			d.dropExpiredTxRejectsLocked(now)
		}
		rejects = &txRejects{windowEnds: now.Add(txRejectWindow)}
		d.txRejects[p] = rejects
	}
	rejects.count++
}

// dropExpiredTxRejectsLocked makes room in the tracking map, removing ended
// windows and then, if peer ID churn filled it with live ones, all of them.
// Caller must hold d.mu.
func (d *DandelionRouter) dropExpiredTxRejectsLocked(now time.Time) {
	for p, rejects := range d.txRejects {
		if !now.Before(rejects.windowEnds) {
			delete(d.txRejects, p)
		}
	}
	if len(d.txRejects) >= maxTxRejectPeers {
		clear(d.txRejects)
	}
}

// trackStemBytesLocked recounts stem data and drops the oldest remote stem records
// past the limit, so an unverified flood can't pin memory until the embargo ends.
// Caller must hold d.mu.
func (d *DandelionRouter) trackStemBytesLocked() {
	total := 0
	for _, rec := range d.txCache {
		total += len(rec.data)
	}
	self := peer.ID("")
	if d.node != nil && d.node.host != nil {
		self = d.node.PeerID()
	}
	for total > maxPendingStemBytes {
		var oldest *txRecord
		for _, rec := range d.txCache {
			if len(rec.data) > 0 && rec.fromPeer != self && (oldest == nil || rec.createdAt.Before(oldest.createdAt)) {
				oldest = rec
			}
		}
		if oldest == nil {
			break
		}
		total -= len(oldest.data)
		delete(d.txCache, oldest.hash)
	}
	d.stemBytes = total
}

// enforceTxCacheLimitLocked evicts oldest entries until cache is within limit.
// Caller must hold d.mu.
func (d *DandelionRouter) enforceTxCacheLimitLocked() {
	d.trackStemBytesLocked()
	if d.txCacheSize <= 0 {
		return
	}
	for len(d.txCache) > d.txCacheSize {
		var (
			oldestHash [32]byte
			oldestAt   time.Time
			oldestSet  bool
		)
		for hash, rec := range d.txCache {
			if !oldestSet {
				oldestHash = hash
				oldestAt = rec.createdAt
				oldestSet = true
				continue
			}
			if rec.createdAt.Before(oldestAt) {
				oldestHash = hash
				oldestAt = rec.createdAt
				continue
			}
			if rec.createdAt.Equal(oldestAt) && bytes.Compare(hash[:], oldestHash[:]) < 0 {
				oldestHash = hash
			}
		}
		if !oldestSet {
			return
		}
		delete(d.txCache, oldestHash)
	}
}

// rebroadcast sends a fluff transaction to all peers except the sender
func (d *DandelionRouter) rebroadcast(exclude peer.ID, data []byte) {
	peers := d.node.host.Network().Peers()

	for _, p := range peers {
		if p == exclude {
			continue
		}
		d.node.sendToPeerAsync(p, protocolID(params.ProtocolTx), data)
	}
}

// SeenTx checks if a transaction has been seen
func (d *DandelionRouter) SeenTx(data []byte) bool {
	hash := sha256.Sum256(data)

	d.mu.RLock()
	defer d.mu.RUnlock()

	_, exists := d.txCache[hash]
	return exists
}

// SeenTxHash checks if a transaction hash has been seen
func (d *DandelionRouter) SeenTxHash(hash [32]byte) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	_, exists := d.txCache[hash]
	return exists
}

// TxCacheSize returns the number of cached transactions
func (d *DandelionRouter) TxCacheSize() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.txCache)
}

// cryptoRandIntn returns a cryptographically random int in [0, n).
func cryptoRandIntn(n int) (int, error) {
	if n <= 0 {
		return 0, errors.New("cryptoRandIntn: n must be > 0")
	}
	val, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(val.Int64()), nil
}

// cryptoRandFloat64 returns a cryptographically random float64 in [0.0, 1.0).
func cryptoRandFloat64() (float64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return float64(binary.LittleEndian.Uint64(b[:])>>11) / (1 << 53), nil
}

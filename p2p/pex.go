package p2p

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/netip"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func filterRoutableAddrs(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
	out := addrs[:0]
	for _, a := range addrs {
		if isRoutablePEXAddr(a) {
			out = append(out, a)
		}
	}
	return out
}

func isRoutablePEXAddr(a multiaddr.Multiaddr) bool {
	// Strict: only accept direct IP addrs from peers (no /dns multiaddrs learned via PEX).
	if a == nil {
		return false
	}

	if ip4, err := a.ValueForProtocol(multiaddr.P_IP4); err == nil && ip4 != "" {
		addr, err := netip.ParseAddr(ip4)
		if err != nil {
			return false
		}
		return isRoutableIP(addr)
	}
	if ip6, err := a.ValueForProtocol(multiaddr.P_IP6); err == nil && ip6 != "" {
		addr, err := netip.ParseAddr(ip6)
		if err != nil {
			return false
		}
		return isRoutableIP(addr)
	}
	return false
}

func isRoutableIP(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	// Fast rejects.
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	// Requires global unicast.
	if !a.IsGlobalUnicast() {
		return false
	}

	if a.Is4() {
		b := a.As4()
		// CGNAT 100.64.0.0/10
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return false
		}
		// Benchmark 198.18.0.0/15
		if b[0] == 198 && (b[1] == 18 || b[1] == 19) {
			return false
		}
		// Documentation ranges (RFC 5737).
		if (b[0] == 192 && b[1] == 0 && b[2] == 2) || (b[0] == 198 && b[1] == 51 && b[2] == 100) || (b[0] == 203 && b[1] == 0 && b[2] == 113) {
			return false
		}
		// Reserved / future use 240.0.0.0/4
		if b[0] >= 240 {
			return false
		}
	} else {
		// IPv6 documentation range.
		if a.Is6() {
			if p, _ := netip.ParsePrefix("2001:db8::/32"); p.Contains(a) {
				return false
			}
		}
	}

	return true
}

// PEX message types
const (
	PEXMsgGetPeers byte = 0x01
	PEXMsgPeers    byte = 0x02
	PEXMsgAnnounce byte = 0x03
)

// MaxPeersPerExchange is the maximum peers to send in one exchange
const MaxPeersPerExchange = 32

const (
	// MaxPeerRecordsPerResponse bounds inbound peer records per PEX exchange response.
	MaxPeerRecordsPerResponse = MaxPeersPerExchange
	// MaxPeerAddrsPerRecord bounds inbound multiaddrs accepted per peer record.
	MaxPeerAddrsPerRecord = 16
	// MaxKnownPeers bounds memory growth from peer churn.
	MaxKnownPeers = 2048
)

// PeerRecord represents a known peer's address info
type PeerRecord struct {
	ID       string   `json:"id"`
	Addrs    []string `json:"addrs"`
	LastSeen int64    `json:"last_seen"`
	Score    int      `json:"score"` // Higher = better peer
}

// Ban thresholds and durations
const (
	ScoreThresholdBan      = 0   // Score at or below triggers ban
	ScoreThresholdWarn     = 20  // Score below this is concerning
	ScorePenaltyInvalid    = -10 // Penalty for invalid data
	ScorePenaltyTimeout    = -5  // Penalty for timeout
	ScorePenaltyMisbehave  = -25 // Penalty for misbehavior
	ScoreRewardGood        = 1   // Reward for good behavior
	BanDurationShort       = 15 * time.Minute
	BanDurationMedium      = 2 * time.Hour
	BanDurationLong        = 24 * time.Hour
	MaxBansBeforePermanent = 5 // After this many bans, permanent ban
)

const (
	// MaxPermanentBans bounds memory growth from adversarial peer-id churn.
	// If the cap is exceeded, we evict the oldest permanent bans (by BannedAt).
	MaxPermanentBans = 4096

	// PermanentBanRetention is a best-effort aging policy for "permanent" bans.
	// It exists to ensure very old bans don't accumulate forever in long-lived processes.
	PermanentBanRetention = 180 * 24 * time.Hour
)

// BanRecord tracks a banned peer
type BanRecord struct {
	PeerID    peer.ID
	Reason    string
	BannedAt  time.Time
	ExpiresAt time.Time
	BanCount  int // How many times this peer has been banned
	Permanent bool
	Addrs     []string // Last known multiaddrs at ban time
}

// PeerExchange manages peer discovery without public DHT
type PeerExchange struct {
	mu sync.RWMutex

	node       *Node
	seedNodes  []peer.AddrInfo
	knownPeers map[peer.ID]*PeerRecord

	// Ban list
	bannedPeers map[peer.ID]*BanRecord

	// Connection tracking
	lastExchange map[peer.ID]time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// NewPeerExchange creates a new peer exchange manager
func NewPeerExchange(node *Node, seedAddrs []string) *PeerExchange {
	seeds := make([]peer.AddrInfo, 0, len(seedAddrs))
	for _, addr := range seedAddrs {
		ma, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			continue
		}
		pi, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			continue
		}
		seeds = append(seeds, *pi)
	}

	return &PeerExchange{
		node:         node,
		seedNodes:    seeds,
		knownPeers:   make(map[peer.ID]*PeerRecord),
		bannedPeers:  make(map[peer.ID]*BanRecord),
		lastExchange: make(map[peer.ID]time.Time),
	}
}

// Start begins peer exchange operations
func (pex *PeerExchange) Start(ctx context.Context) error {
	pex.ctx, pex.cancel = context.WithCancel(ctx)

	// Connect to seed nodes
	if err := pex.connectToSeeds(); err != nil {
		return err
	}

	// Start background tasks
	go pex.exchangeLoop()
	go pex.cleanupLoop()
	go pex.reconnectLoop()

	return nil
}

// Stop halts peer exchange
func (pex *PeerExchange) Stop() {
	if pex.cancel != nil {
		pex.cancel()
	}
}

// connectToSeeds attempts to connect to all seed nodes
func (pex *PeerExchange) connectToSeeds() error {
	if len(pex.seedNodes) == 0 {
		return nil // No seeds configured
	}

	ourID := pex.node.PeerID()
	// Retry logic: try 3 times with 2 second delays
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		var connected int
		var skipped int

		for _, seed := range pex.seedNodes {
			// Don't connect to ourselves
			if seed.ID == ourID {
				skipped++
				continue
			}

			ctx, cancel := context.WithTimeout(pex.ctx, 10*time.Second)
			err := pex.node.host.Connect(ctx, seed)
			cancel()

			if err == nil {
				connected++
				pex.addKnownPeer(seed.ID, seed.Addrs)
			}
		}

		// If we connected to at least one seed, or all seeds were ourselves, we're good
		if connected > 0 || len(pex.seedNodes) == skipped {
			if attempt > 1 {
				log.Printf("Connected to %d seed node(s) on attempt %d", connected, attempt)
			}
			return nil
		}

		// Not the last attempt - wait and retry
		if attempt < maxRetries {
			time.Sleep(2 * time.Second)
		}
	}

	// All retries failed
	return fmt.Errorf("failed to connect to any seed nodes after %d attempts", maxRetries)
}

// exchangeLoop periodically exchanges peers with connected nodes
func (pex *PeerExchange) exchangeLoop() {
	// Initial exchange after short delay
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-pex.ctx.Done():
			return
		case <-ticker.C:
			pex.doExchange()
		}
	}
}

// doExchange performs peer exchange with a random subset of connected peers
func (pex *PeerExchange) doExchange() {
	peers := pex.node.host.Network().Peers()
	if len(peers) == 0 {
		// No peers, try seeds again
		if err := pex.connectToSeeds(); err != nil {
			log.Printf("PEX seed reconnect failed: %v", err)
		}
		return
	}

	// Pick random peers to exchange with (max 3, crypto/rand)
	for i := len(peers) - 1; i > 0; i-- {
		jBig, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		j := int(jBig.Int64())
		peers[i], peers[j] = peers[j], peers[i]
	}

	count := 3
	if len(peers) < count {
		count = len(peers)
	}

	for _, p := range peers[:count] {
		pex.exchangeWith(p)
	}
}

// exchangeWith performs peer exchange with a specific peer
func (pex *PeerExchange) exchangeWith(p peer.ID) {
	ctx, cancel := context.WithTimeout(pex.ctx, 30*time.Second)
	defer cancel()

	s, err := pex.node.host.NewStream(ctx, p, ProtocolPEX)
	if err != nil {
		return
	}
	defer s.Close()

	// Send GetPeers request
	if err := writeMessage(s, PEXMsgGetPeers, nil); err != nil {
		return
	}

	// Read response
	msgType, data, err := readPEXMessage(s)
	if err != nil {
		return
	}

	if msgType != PEXMsgPeers {
		return
	}

	if err := ensureJSONArrayMaxItems(data, MaxPeerRecordsPerResponse); err != nil {
		return
	}
	if err := ensurePEXRecordAddressBounds(data, MaxPeerRecordsPerResponse, MaxPeerAddrsPerRecord); err != nil {
		return
	}

	// Parse peer records
	var records []PeerRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return
	}

	// Add new peers
	for _, rec := range records {
		pid, err := peer.Decode(rec.ID)
		if err != nil {
			continue
		}

		// Don't add ourselves
		if pid == pex.node.PeerID() {
			continue
		}

		addrs := make([]multiaddr.Multiaddr, 0, len(rec.Addrs))
		for _, a := range rec.Addrs {
			ma, err := multiaddr.NewMultiaddr(a)
			if err != nil {
				continue
			}
			addrs = append(addrs, ma)
		}
		addrs = filterRoutableAddrs(addrs)

		if len(addrs) > 0 {
			pex.addKnownPeer(pid, addrs)

			// Try to connect if we need more peers
			if len(pex.node.Peers()) < pex.node.config.MaxOutbound {
				go pex.tryConnect(pid, addrs)
			}
		}
	}

	pex.mu.Lock()
	pex.lastExchange[p] = time.Now()
	pex.mu.Unlock()
}

// ensurePEXRecordAddressBounds validates peer record and address counts before full decode.
func ensurePEXRecordAddressBounds(data []byte, maxRecords int, maxAddrsPerRecord int) error {
	if maxRecords < 0 {
		maxRecords = 0
	}
	if maxAddrsPerRecord < 0 {
		maxAddrsPerRecord = 0
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '[' {
		return fmt.Errorf("expected JSON array")
	}

	recordCount := 0
	for dec.More() {
		recordCount++
		if recordCount > maxRecords {
			return fmt.Errorf("peer record count %d exceeds max %d", recordCount, maxRecords)
		}

		tok, err := dec.Token()
		if err != nil {
			return err
		}
		objStart, ok := tok.(json.Delim)
		if !ok || objStart != '{' {
			return fmt.Errorf("expected peer record object")
		}

		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("expected peer record field name")
			}

			if key != "addrs" {
				if err := skipJSONValue(dec); err != nil {
					return err
				}
				continue
			}

			tok, err := dec.Token()
			if err != nil {
				return err
			}
			addrsStart, ok := tok.(json.Delim)
			if !ok || addrsStart != '[' {
				return fmt.Errorf("expected addrs array")
			}

			addrCount := 0
			for dec.More() {
				addrCount++
				if addrCount > maxAddrsPerRecord {
					return fmt.Errorf("peer record address count %d exceeds max %d", addrCount, maxAddrsPerRecord)
				}
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}

			tok, err = dec.Token()
			if err != nil {
				return err
			}
			addrsEnd, ok := tok.(json.Delim)
			if !ok || addrsEnd != ']' {
				return fmt.Errorf("malformed addrs array")
			}
		}

		tok, err = dec.Token()
		if err != nil {
			return err
		}
		objEnd, ok := tok.(json.Delim)
		if !ok || objEnd != '}' {
			return fmt.Errorf("malformed peer record object")
		}
	}

	tok, err = dec.Token()
	if err != nil {
		return err
	}
	delim, ok = tok.(json.Delim)
	if !ok || delim != ']' {
		return fmt.Errorf("malformed JSON array")
	}

	return nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}

	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		endTok, err := dec.Token()
		if err != nil {
			return err
		}
		endDelim, ok := endTok.(json.Delim)
		if !ok || endDelim != '}' {
			return fmt.Errorf("malformed object")
		}
		return nil
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		endTok, err := dec.Token()
		if err != nil {
			return err
		}
		endDelim, ok := endTok.(json.Delim)
		if !ok || endDelim != ']' {
			return fmt.Errorf("malformed array")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

// tryConnect attempts to connect to a peer
func (pex *PeerExchange) tryConnect(pid peer.ID, addrs []multiaddr.Multiaddr) {
	// Skip if already connected
	if pex.node.host.Network().Connectedness(pid) == network.Connected {
		return
	}

	addrs = filterRoutableAddrs(addrs)
	if len(addrs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(pex.ctx, 30*time.Second)
	defer cancel()

	pi := peer.AddrInfo{ID: pid, Addrs: addrs}
	if err := pex.node.host.Connect(ctx, pi); err == nil {
		pex.updatePeerScore(pid, 1) // Successful connection
	}
}

// HandleStream handles incoming PEX protocol streams
func (pex *PeerExchange) HandleStream(s network.Stream) {
	defer s.Close()

	// Read message
	msgType, _, err := readPEXMessage(s)
	if err != nil {
		return
	}

	switch msgType {
	case PEXMsgGetPeers:
		pex.handleGetPeers(s)
	case PEXMsgAnnounce:
		// Peer is announcing themselves - we already know them from the connection
	}
}

// handleGetPeers responds to a peer request
func (pex *PeerExchange) handleGetPeers(s network.Stream) {
	records := pex.getPeerRecords(MaxPeersPerExchange)

	data, err := json.Marshal(records)
	if err != nil {
		return
	}

	if err := writeMessage(s, PEXMsgPeers, data); err != nil {
		log.Printf("PEX failed to write peers response: %v", err)
	}
}

func readPEXMessage(r network.Stream) (byte, []byte, error) {
	return readMessageWithLimit(r, pexMessageMaxSize)
}

func pexMessageMaxSize(msgType byte) (uint32, error) {
	switch msgType {
	case PEXMsgGetPeers, PEXMsgPeers, PEXMsgAnnounce:
		return MaxPEXMessageSize, nil
	default:
		return 0, fmt.Errorf("unknown pex message type: %d", msgType)
	}
}

// getPeerRecords returns a subset of known peers
func (pex *PeerExchange) getPeerRecords(max int) []PeerRecord {
	pex.mu.RLock()
	defer pex.mu.RUnlock()

	records := make([]PeerRecord, 0, max)

	// Add connected peers first (they're definitely reachable)
	for _, p := range pex.node.Peers() {
		if len(records) >= max {
			break
		}

		addrs := pex.node.host.Peerstore().Addrs(p)
		addrs = filterRoutableAddrs(addrs)
		if len(addrs) == 0 {
			continue
		}

		addrStrs := make([]string, len(addrs))
		for i, a := range addrs {
			addrStrs[i] = a.String()
		}

		records = append(records, PeerRecord{
			ID:       p.String(),
			Addrs:    addrStrs,
			LastSeen: time.Now().Unix(),
			Score:    10,
		})
	}

	// Add from known peers
	for pid, rec := range pex.knownPeers {
		if len(records) >= max {
			break
		}

		// Skip if already added
		alreadyAdded := false
		for _, r := range records {
			if r.ID == pid.String() {
				alreadyAdded = true
				break
			}
		}
		if alreadyAdded {
			continue
		}

		records = append(records, *rec)
	}

	return records
}

// addKnownPeer adds or updates a known peer
func (pex *PeerExchange) addKnownPeer(pid peer.ID, addrs []multiaddr.Multiaddr) {
	pex.mu.Lock()
	defer pex.mu.Unlock()

	addrs = filterRoutableAddrs(addrs)
	if len(addrs) == 0 {
		return
	}

	addrStrs := make([]string, len(addrs))
	for i, a := range addrs {
		addrStrs[i] = a.String()
	}

	if existing, ok := pex.knownPeers[pid]; ok {
		existing.Addrs = addrStrs
		existing.LastSeen = time.Now().Unix()
	} else {
		pex.knownPeers[pid] = &PeerRecord{
			ID:       pid.String(),
			Addrs:    addrStrs,
			LastSeen: time.Now().Unix(),
			Score:    5,
		}
	}

	// Also add to peerstore for libp2p
	pex.node.host.Peerstore().AddAddrs(pid, addrs, time.Hour)

	pex.evictKnownPeersLocked(MaxKnownPeers)
}

func (pex *PeerExchange) evictKnownPeersLocked(max int) {
	if max <= 0 {
		max = 1
	}
	for len(pex.knownPeers) > max {
		var victim peer.ID
		var victimRec *PeerRecord
		for pid, rec := range pex.knownPeers {
			// Prefer evicting non-connected peers; keep currently connected ones.
			if pex.node != nil && pex.node.host != nil && pex.node.host.Network().Connectedness(pid) == network.Connected {
				continue
			}
			if victimRec == nil {
				victim = pid
				victimRec = rec
				continue
			}
			// Deterministic eviction: lowest score, then oldest lastSeen, then lexicographic ID.
			if rec.Score < victimRec.Score ||
				(rec.Score == victimRec.Score && rec.LastSeen < victimRec.LastSeen) ||
				(rec.Score == victimRec.Score && rec.LastSeen == victimRec.LastSeen && pid.String() < victim.String()) {
				victim = pid
				victimRec = rec
			}
		}
		if victimRec == nil {
			// All peers appear connected; fall back to evicting by deterministic rule anyway.
			for pid, rec := range pex.knownPeers {
				if victimRec == nil ||
					rec.Score < victimRec.Score ||
					(rec.Score == victimRec.Score && rec.LastSeen < victimRec.LastSeen) ||
					(rec.Score == victimRec.Score && rec.LastSeen == victimRec.LastSeen && pid.String() < victim.String()) {
					victim = pid
					victimRec = rec
				}
			}
		}
		if victimRec == nil {
			return
		}
		delete(pex.knownPeers, victim)
		if pex.node != nil && pex.node.host != nil {
			pex.node.host.Peerstore().ClearAddrs(victim)
		}
	}
}

// updatePeerScore adjusts a peer's score and may trigger a ban
func (pex *PeerExchange) updatePeerScore(pid peer.ID, delta int) {
	pex.mu.Lock()
	defer pex.mu.Unlock()

	if rec, ok := pex.knownPeers[pid]; ok {
		rec.Score += delta
		if rec.Score < 0 {
			rec.Score = 0
		}
		if rec.Score > 100 {
			rec.Score = 100
		}

		// Check if should ban
		if rec.Score <= ScoreThresholdBan {
			pex.banPeerLocked(pid, "score dropped to zero", BanDurationMedium)
		}
	}
}

// IsBanned checks if a peer is currently banned
func (pex *PeerExchange) IsBanned(pid peer.ID) bool {
	pex.mu.RLock()
	defer pex.mu.RUnlock()

	ban, exists := pex.bannedPeers[pid]
	if !exists {
		return false
	}

	if ban.Permanent {
		return true
	}

	// Check if ban expired
	if time.Now().After(ban.ExpiresAt) {
		return false
	}

	return true
}

// BanPeer bans a peer for the specified duration
func (pex *PeerExchange) BanPeer(pid peer.ID, reason string, duration time.Duration) {
	pex.mu.Lock()
	defer pex.mu.Unlock()
	pex.banPeerLocked(pid, reason, duration)
}

// banPeerLocked bans a peer (caller must hold lock)
func (pex *PeerExchange) banPeerLocked(pid peer.ID, reason string, duration time.Duration) {
	for _, seed := range pex.seedNodes {
		if seed.ID == pid {
			return
		}
	}
	now := time.Now()

	// Check existing ban record
	existing, exists := pex.bannedPeers[pid]
	banCount := 1
	if exists {
		banCount = existing.BanCount + 1
	}

	// Escalate ban duration for repeat offenders
	if banCount >= 3 {
		duration = BanDurationLong
	}

	permanent := banCount >= MaxBansBeforePermanent

	var addrStrs []string
	if pex.node != nil && pex.node.host != nil {
		for _, conn := range pex.node.host.Network().ConnsToPeer(pid) {
			addrStrs = append(addrStrs, conn.RemoteMultiaddr().String())
		}
	}

	pex.bannedPeers[pid] = &BanRecord{
		PeerID:    pid,
		Reason:    reason,
		BannedAt:  now,
		ExpiresAt: now.Add(duration),
		BanCount:  banCount,
		Permanent: permanent,
		Addrs:     addrStrs,
	}

	pex.enforceBanRetentionLocked(now)

	// Disconnect immediately
	if pex.node != nil && pex.node.host != nil {
		if err := pex.node.host.Network().ClosePeer(pid); err != nil {
			log.Printf("PEX failed to disconnect banned peer %s: %v", pid, err)
		}
	}

	// Remove from known peers
	delete(pex.knownPeers, pid)
}

// enforceBanRetentionLocked bounds permanent ban retention.
// Caller must hold pex.mu.
func (pex *PeerExchange) enforceBanRetentionLocked(now time.Time) {
	// Age out permanent bans first.
	if PermanentBanRetention > 0 {
		for pid, ban := range pex.bannedPeers {
			if ban.Permanent && now.Sub(ban.BannedAt) > PermanentBanRetention {
				delete(pex.bannedPeers, pid)
			}
		}
	}

	if MaxPermanentBans <= 0 {
		return
	}

	// Cap permanent bans by evicting oldest.
	permanentCount := 0
	for _, ban := range pex.bannedPeers {
		if ban.Permanent {
			permanentCount++
		}
	}
	for permanentCount > MaxPermanentBans {
		var oldestPID peer.ID
		var oldestAt time.Time
		found := false
		for pid, ban := range pex.bannedPeers {
			if !ban.Permanent {
				continue
			}
			if !found || ban.BannedAt.Before(oldestAt) {
				oldestPID = pid
				oldestAt = ban.BannedAt
				found = true
			}
		}
		if !found {
			return
		}
		delete(pex.bannedPeers, oldestPID)
		permanentCount--
	}
}

// UnbanPeer removes a ban (for manual intervention)
func (pex *PeerExchange) UnbanPeer(pid peer.ID) {
	pex.mu.Lock()
	defer pex.mu.Unlock()
	delete(pex.bannedPeers, pid)
}

// GetBannedPeers returns list of currently banned peers
func (pex *PeerExchange) GetBannedPeers() []*BanRecord {
	pex.mu.RLock()
	defer pex.mu.RUnlock()

	var bans []*BanRecord
	now := time.Now()
	for _, ban := range pex.bannedPeers {
		if ban.Permanent || now.Before(ban.ExpiresAt) {
			bans = append(bans, ban)
		}
	}
	return bans
}

// PenalizePeer applies a penalty to a peer's score
func (pex *PeerExchange) PenalizePeer(pid peer.ID, penalty int, reason string) {
	pex.mu.Lock()
	defer pex.mu.Unlock()

	rec, ok := pex.knownPeers[pid]
	if !ok {
		// Create record with starting score
		rec = &PeerRecord{
			ID:       pid.String(),
			Score:    50, // Start at middle
			LastSeen: time.Now().Unix(),
		}
		pex.knownPeers[pid] = rec
	}

	rec.Score += penalty
	if rec.Score < 0 {
		rec.Score = 0
	}

	// Ban only once the peer's score is exhausted.
	// Callers that want deterministic immediate bans should use BanPeer directly.
	if rec.Score <= ScoreThresholdBan {
		pex.banPeerLocked(pid, reason, BanDurationMedium)
	}
}

// RewardPeer increases a peer's score for good behavior
func (pex *PeerExchange) RewardPeer(pid peer.ID) {
	pex.updatePeerScore(pid, ScoreRewardGood)
}

// cleanupLoop removes stale peers
func (pex *PeerExchange) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-pex.ctx.Done():
			return
		case <-ticker.C:
			pex.cleanup()
		}
	}
}

// reconnectLoop aggressively reconnects to seeds when we have no peers
func (pex *PeerExchange) reconnectLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pex.ctx.Done():
			return
		case <-ticker.C:
			peers := pex.node.host.Network().Peers()
			if len(peers) == 0 {
				if err := pex.connectToSeeds(); err != nil {
					log.Printf("PEX reconnect loop seed dial failed: %v", err)
				}
			}
		}
	}
}

// cleanup removes peers not seen in a long time and expired bans
func (pex *PeerExchange) cleanup() {
	pex.mu.Lock()
	defer pex.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour).Unix()

	// Cleanup stale peers
	for pid, rec := range pex.knownPeers {
		if rec.LastSeen < cutoff {
			delete(pex.knownPeers, pid)
		}
	}

	// Cleanup expired bans (keep permanent bans)
	for pid, ban := range pex.bannedPeers {
		if !ban.Permanent && now.After(ban.ExpiresAt) {
			delete(pex.bannedPeers, pid)
		}
	}

	pex.enforceBanRetentionLocked(now)
	pex.evictKnownPeersLocked(MaxKnownPeers)
}

// KnownPeerCount returns the number of known peers
func (pex *PeerExchange) KnownPeerCount() int {
	pex.mu.RLock()
	defer pex.mu.RUnlock()
	return len(pex.knownPeers)
}

// BannedPeerCount returns the number of currently banned peers
func (pex *PeerExchange) BannedPeerCount() int {
	pex.mu.RLock()
	defer pex.mu.RUnlock()

	count := 0
	now := time.Now()
	for _, ban := range pex.bannedPeers {
		if ban.Permanent || now.Before(ban.ExpiresAt) {
			count++
		}
	}
	return count
}

// GetPeerScore returns a peer's current score (-1 if unknown)
func (pex *PeerExchange) GetPeerScore(pid peer.ID) int {
	pex.mu.RLock()
	defer pex.mu.RUnlock()

	if rec, ok := pex.knownPeers[pid]; ok {
		return rec.Score
	}
	return -1
}

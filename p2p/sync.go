package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Sync message types
const (
	SyncMsgGetHeaders        byte = 0x01 // Request block headers from a height
	SyncMsgHeaders           byte = 0x02 // Response with headers
	SyncMsgGetBlocks         byte = 0x03 // Request full blocks by hash
	SyncMsgBlocks            byte = 0x04 // Response with blocks
	SyncMsgStatus            byte = 0x05 // Exchange chain status
	SyncMsgNewBlock          byte = 0x06 // Announce new block
	SyncMsgGetMempool        byte = 0x07 // Request mempool transactions
	SyncMsgMempool           byte = 0x08 // Response with mempool txs
	SyncMsgGetBlocksByHeight byte = 0x09 // Request blocks by height range
)

// MaxHeadersPerRequest is the maximum headers to request at once
const MaxHeadersPerRequest = 500

// MaxBlocksPerRequest is the maximum blocks to request at once
const MaxBlocksPerRequest = 100

const (
	syncMaxDownloaders = 6
	syncReadAhead      = 20 * MaxBlocksPerRequest
	syncHedgeMin       = 750 * time.Millisecond
	syncHedgeMax       = 3 * time.Second
	syncHedgePeers     = 3
	syncGiveUpAfter    = 2 * time.Minute
	// syncClaimGiveUpAfter ends a run following work only a minority of peers
	// advertise, when no block data arrives at all. A transfer still sending
	// data gets the full syncGiveUpAfter.
	syncClaimGiveUpAfter = 30 * time.Second
	// syncClaimCooldown is how long a peer's status is ignored after a run we asked
	// it to serve ran out of data short of what it advertised. It's short because
	// honest peers can stall too, and false claimers change identity anyway.
	syncClaimCooldown = 2 * time.Minute
	// syncBehindProofBlocks is how many blocks above the tip a run must deliver
	// before peers we didn't choose can mark the node as behind. It exceeds the
	// template tolerance, so one real block can't stop templates.
	syncBehindProofBlocks = 3
	// defaultSyncRunTimeLimit ends a run however it's going. The next run picks up
	// where it stopped.
	defaultSyncRunTimeLimit = 10 * time.Minute
	// syncForkFetchDepth is how far back a run looks when a peer claims more work
	// at or below our height, which only a heavier fork can justify.
	syncForkFetchDepth = 50
	// syncMempoolPeers is how many peers a finished run asks for their mempool.
	syncMempoolPeers = 3
	// minBlockDataBytes is about the size of the smallest possible block.
	minBlockDataBytes = 1024
	// syncMaxConcurrentServes and syncMaxServesPerPeer bound block and mempool
	// responses in progress, so one peer can't take them all.
	syncMaxConcurrentServes = 64
	syncMaxServesPerPeer    = 6
	// syncServeReservedShare is one over the part of those slots kept for peers we
	// dialed or that have delivered a block.
	syncServeReservedShare = 4
	// syncMinDialedQuorum is how many dialed peers must answer before they can mark
	// the node as behind: one or two can be wrong, or be the attacker.
	syncMinDialedQuorum = 3
)

const (
	// Keep sync responses bounded even when request count is large.
	// Budgets are for raw []byte entries before JSON/base64 expansion.
	SyncHeadersResponseByteBudget = 2 * 1024 * 1024
	SyncBlocksResponseByteBudget  = 8 * 1024 * 1024
	SyncMempoolResponseByteBudget = 4 * 1024 * 1024
)

// ChainStatus represents a peer's chain status
type ChainStatus struct {
	BestHash  [32]byte `json:"best_hash"`
	Height    uint64   `json:"height"`
	TotalWork uint64   `json:"total_work"`
	Version   uint32   `json:"version"`
	NetworkID string   `json:"network_id"`
	ChainID   uint32   `json:"chain_id"`
}

// HeadersRequest requests headers starting from a height
type HeadersRequest struct {
	StartHeight uint64 `json:"start_height"`
	MaxHeaders  int    `json:"max_headers"`
}

// BlocksRequest requests specific blocks by hash
type BlocksRequest struct {
	Hashes [][32]byte `json:"hashes"`
}

// BlocksByHeightRequest requests blocks by height range
type BlocksByHeightRequest struct {
	StartHeight uint64 `json:"start_height"`
	MaxBlocks   int    `json:"max_blocks"`
}

// PeerStatus combines peer ID with their chain status
type PeerStatus struct {
	Peer   peer.ID
	Status ChainStatus
}

// SyncWork represents a batch of blocks to download
type SyncWork struct {
	Start uint64
	End   uint64
}

// DownloadedBlock represents a block downloaded from a peer
type DownloadedBlock struct {
	Height uint64
	Data   []byte
	Peer   peer.ID
}

// SyncManager handles chain synchronization
type SyncManager struct {
	mu sync.RWMutex

	fetchEWMA atomic.Int64
	// mempoolSyncing keeps one background mempool fetch at a time.
	mempoolSyncing atomic.Bool
	// serveSlots holds one entry per large response being served.
	serveSlots   chan struct{}
	serveMu      sync.Mutex
	servesByPeer map[peer.ID]int
	// runAsked records the peers the current run has requested blocks from.
	runAsked atomic.Pointer[sync.Map]
	// lastBlockDataAt is the UnixNano time block data last arrived from a peer.
	lastBlockDataAt atomic.Int64

	node *Node

	// Callbacks for chain operations
	getStatus             func() ChainStatus
	getHeaders            func(startHeight uint64, max int) ([][]byte, error)
	getBlocks             func(hashes [][32]byte) ([][]byte, error)
	getBlocksByHeight     func(startHeight uint64, max int) ([][]byte, error)
	processBlock          func(data []byte) error
	processHeader         func(data []byte) error
	getMempool            func() [][]byte
	processTx             func(data []byte) error
	onBlockAccepted       func(data []byte)
	processAnnouncedBlock func(from peer.ID, data []byte) error
	isOrphanError         func(error) bool
	isDuplicateError      func(error) bool
	getBlockMeta          func(data []byte) (height uint64, prevHash [32]byte, err error)
	getBlockHash          func(data []byte) (hash [32]byte, err error)
	fetchBlocksByHash     func(context.Context, peer.ID, [][32]byte) ([][]byte, error)

	// Sync state
	syncing        bool
	syncPeer       peer.ID
	syncStartTime  time.Time
	syncTarget     uint64                     // Target height we're syncing to
	syncProgress   uint64                     // Current height we've processed to
	downloadBuffer map[uint64]DownloadedBlock // Buffer for out-of-order blocks

	// peerTipChecked is set once a sync check has compared the local chain
	// with at least one peer's status; see PeerTipChecked.
	peerTipChecked atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc

	// statusSyncCh serializes/rate-limits sync checks triggered by inbound status
	// messages (and other external triggers). This prevents goroutine churn when
	// peers spam status updates.
	statusSyncCh          chan struct{}
	statusSyncMinInterval time.Duration

	resendMu       sync.Mutex
	resends        map[peer.ID]map[uint64]*rangeResend
	resendsPruneAt time.Time

	// syncClaimUnverified marks a run whose target rests only on a status from
	// a minority of peers. It clears when a block above the local tip arrives.
	syncClaimUnverified bool
	// syncRunTimeLimit overrides defaultSyncRunTimeLimit.
	syncRunTimeLimit time.Duration
	// syncRunCorroborated marks a run most peers we dialed agree with.
	syncRunCorroborated bool
	// syncBehindTarget is the height this node counts as being behind, once proven.
	syncBehindTarget uint64
	// recheckAfterRun records a sync check that arrived during a run.
	recheckAfterRun bool
	// pauseCorroboration makes the next run prove itself.
	pauseCorroboration bool
	claimGiveUpAfter   time.Duration
	claimCooldowns     map[peer.ID]time.Time
}

const (
	rangeResendWindow     = 30 * time.Minute
	rangeResendLimit      = 10
	rangeResendMaxPerPeer = 1024
)

type rangeResend struct {
	count int
	first time.Time
}

func (sm *SyncManager) rangeResendAllowed(pid peer.ID, start uint64, now time.Time) bool {
	if sm.node != nil && sm.node.IsWhitelisted(pid) {
		return true
	}
	sm.resendMu.Lock()
	defer sm.resendMu.Unlock()
	r := sm.resends[pid][start]
	return r == nil || now.Sub(r.first) > rangeResendWindow || r.count < rangeResendLimit
}

func (sm *SyncManager) recordRangeSent(pid peer.ID, start uint64, now time.Time) {
	sm.resendMu.Lock()
	defer sm.resendMu.Unlock()
	if sm.resends == nil {
		sm.resends = make(map[peer.ID]map[uint64]*rangeResend)
	}
	if now.After(sm.resendsPruneAt) {
		for p, byStart := range sm.resends {
			for h, r := range byStart {
				if now.Sub(r.first) > rangeResendWindow {
					delete(byStart, h)
				}
			}
			if len(byStart) == 0 {
				delete(sm.resends, p)
			}
		}
		sm.resendsPruneAt = now.Add(time.Minute)
	}
	byStart := sm.resends[pid]
	if byStart == nil || len(byStart) >= rangeResendMaxPerPeer {
		byStart = make(map[uint64]*rangeResend)
		sm.resends[pid] = byStart
	}
	r := byStart[start]
	if r == nil || now.Sub(r.first) > rangeResendWindow {
		byStart[start] = &rangeResend{count: 1, first: now}
		return
	}
	r.count++
	if r.count == rangeResendLimit {
		log.Printf("[sync] peer %s got blocks from %d %d times in %s; sending it empty replies for that range", pid, start, r.count, rangeResendWindow)
	}
}

func (sm *SyncManager) penalizeInvalidBlockPeer(pid peer.ID, reason string) {
	if pid == "" || sm.node == nil {
		return
	}
	sm.node.PenalizePeer(pid, ScorePenaltyMisbehave, reason)
}

func (sm *SyncManager) banInvalidBlockPeer(pid peer.ID, reason string) {
	if pid == "" || sm.node == nil {
		return
	}
	sm.node.BanPeer(pid, reason)
}

// SyncConfig configures the sync manager
type SyncConfig struct {
	GetStatus         func() ChainStatus
	GetHeaders        func(startHeight uint64, max int) ([][]byte, error)
	GetBlocks         func(hashes [][32]byte) ([][]byte, error)
	GetBlocksByHeight func(startHeight uint64, max int) ([][]byte, error)
	ProcessBlock      func(data []byte) error
	ProcessHeader     func(data []byte) error
	GetMempool        func() [][]byte         // Get all mempool transactions
	ProcessTx         func(data []byte) error // Process a mempool transaction
	OnBlockAccepted   func(data []byte)       // Called when a new block announcement is accepted
	// ProcessAnnouncedBlock handles a block a peer announced. If unset, ProcessBlock does.
	ProcessAnnouncedBlock func(from peer.ID, data []byte) error
	IsOrphanError         func(error) bool                                             // Orphan classifier for sync recovery (required for recovery to work)
	IsDuplicateError      func(error) bool                                             // Duplicate classifier — block already known, treat as success
	GetBlockMeta          func(data []byte) (uint64, [32]byte, error)                  // Extract (height, prevHash) from serialized block; required for orphan recovery
	GetBlockHash          func(data []byte) ([32]byte, error)                          // Extract block hash from serialized block; required for parent hash verification in orphan recovery
	FetchBlocksByHash     func(context.Context, peer.ID, [][32]byte) ([][]byte, error) // Override block-by-hash fetching (default: p2p FetchBlocks)
}

// NewSyncManager creates a new sync manager
func NewSyncManager(node *Node, cfg SyncConfig) *SyncManager {
	sm := &SyncManager{
		node:                  node,
		getStatus:             cfg.GetStatus,
		getHeaders:            cfg.GetHeaders,
		getBlocks:             cfg.GetBlocks,
		getBlocksByHeight:     cfg.GetBlocksByHeight,
		processBlock:          cfg.ProcessBlock,
		processHeader:         cfg.ProcessHeader,
		getMempool:            cfg.GetMempool,
		processTx:             cfg.ProcessTx,
		onBlockAccepted:       cfg.OnBlockAccepted,
		processAnnouncedBlock: cfg.ProcessAnnouncedBlock,
		isOrphanError:         cfg.IsOrphanError,
		isDuplicateError:      cfg.IsDuplicateError,
		getBlockMeta:          cfg.GetBlockMeta,
		getBlockHash:          cfg.GetBlockHash,
		downloadBuffer:        make(map[uint64]DownloadedBlock),
		statusSyncCh:          make(chan struct{}, 1),
		// Debounce status-triggered sync checks; periodic syncLoop still runs.
		statusSyncMinInterval: 2 * time.Second,
		claimGiveUpAfter:      syncClaimGiveUpAfter,
		serveSlots:            make(chan struct{}, syncMaxConcurrentServes),
		servesByPeer:          make(map[peer.ID]int),
	}
	if cfg.FetchBlocksByHash != nil {
		sm.fetchBlocksByHash = cfg.FetchBlocksByHash
	} else if node != nil {
		sm.fetchBlocksByHash = sm.FetchBlocks
	}
	return sm
}

// Start begins sync operations
func (sm *SyncManager) Start(ctx context.Context) {
	sm.ctx, sm.cancel = context.WithCancel(ctx)

	// Update the node's sync handler
	sm.node.host.SetStreamHandler(protocolID(params.ProtocolSync), sm.HandleStream)

	// Start sync loop
	go sm.syncLoop()
	go sm.statusSyncLoop()
}

// Stop halts sync operations
func (sm *SyncManager) Stop() {
	if sm.cancel != nil {
		sm.cancel()
	}
}

// HandleStream handles incoming sync protocol streams
func (sm *SyncManager) HandleStream(s network.Stream) {
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return
	}

	msgType, data, err := readSyncMessage(s)
	if err != nil {
		return
	}

	switch msgType {
	case SyncMsgStatus:
		sm.handleStatus(s, data)
	case SyncMsgGetHeaders:
		sm.handleGetHeaders(s, data)
	case SyncMsgGetBlocks:
		if release, ok := sm.acquireServeSlot(s.Conn().RemotePeer()); ok {
			defer release()
			sm.handleGetBlocks(s, data)
		}
	case SyncMsgGetBlocksByHeight:
		release, ok := sm.acquireServeSlot(s.Conn().RemotePeer())
		if !ok {
			// An empty answer tells an honest peer to ask elsewhere, at no cost.
			_ = writeMessage(s, SyncMsgBlocks, []byte("[]"))
			return
		}
		defer release()
		sm.handleGetBlocksByHeight(s, data)
	case SyncMsgNewBlock:
		sm.handleNewBlock(s.Conn().RemotePeer(), data)
	case SyncMsgGetMempool:
		if release, ok := sm.acquireServeSlot(s.Conn().RemotePeer()); ok {
			defer release()
			sm.handleGetMempool(s)
		}
	}
}

// acquireServeSlot bounds how many large responses are built and sent at once, in
// total and per peer, and keeps a share for trusted peers, so requests for old
// blocks can't use up memory or starve other peers. A request past the limit is
// refused.
func (sm *SyncManager) acquireServeSlot(pid peer.ID) (func(), bool) {
	if sm.serveSlots == nil {
		return func() {}, true
	}
	trusted := sm.node != nil && sm.node.host != nil && sm.node.IsTrustedBlockSource(pid)
	sm.serveMu.Lock()
	defer sm.serveMu.Unlock()
	if sm.servesByPeer[pid] >= syncMaxServesPerPeer {
		return nil, false
	}
	// Peers without a track record can't take the last slots, so a few free
	// connections can't starve the peers we chose.
	if !trusted && len(sm.serveSlots) >= cap(sm.serveSlots)-cap(sm.serveSlots)/syncServeReservedShare {
		return nil, false
	}
	select {
	case sm.serveSlots <- struct{}{}:
	default:
		return nil, false
	}
	sm.servesByPeer[pid]++
	return func() {
		sm.serveMu.Lock()
		defer sm.serveMu.Unlock()
		<-sm.serveSlots
		if sm.servesByPeer[pid]--; sm.servesByPeer[pid] <= 0 {
			delete(sm.servesByPeer, pid)
		}
	}, true
}

// handleStatus processes a status message
func (sm *SyncManager) handleStatus(s network.Stream, data []byte) {
	var status ChainStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return
	}
	if status.NetworkID != params.NetworkID || status.ChainID != params.ChainID {
		// Hard split: don't respond to or sync from other networks/epochs.
		sm.node.PenalizePeer(s.Conn().RemotePeer(), ScorePenaltyInvalid, "chain status mismatch")
		return
	}

	// Send our status back
	ourStatus := sm.getStatus()
	statusData, _ := json.Marshal(ourStatus)
	if err := writeMessage(s, SyncMsgStatus, statusData); err != nil {
		return
	}

	// Check if we need to sync from this peer
	if status.TotalWork > ourStatus.TotalWork {
		// Don't spawn sync goroutines per inbound status message. Instead trigger
		// a serialized, multi-peer arbitration pass.
		sm.requestSyncCheck()
	}
}

// handleGetHeaders responds to a headers request
func (sm *SyncManager) handleGetHeaders(s network.Stream, data []byte) {
	var req HeadersRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	if req.MaxHeaders > MaxHeadersPerRequest {
		req.MaxHeaders = MaxHeadersPerRequest
	}

	headers, err := sm.getHeaders(req.StartHeight, req.MaxHeaders)
	if err != nil {
		return
	}

	// Send headers
	headers = trimByteSliceBatch(headers, MaxHeadersPerRequest, SyncHeadersResponseByteBudget)
	headersData, _ := json.Marshal(headers)
	if err := writeMessage(s, SyncMsgHeaders, headersData); err != nil {
		return
	}
}

// handleGetBlocks responds to a blocks request
func (sm *SyncManager) handleGetBlocks(s network.Stream, data []byte) {
	var req BlocksRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	if len(req.Hashes) > MaxBlocksPerRequest {
		req.Hashes = req.Hashes[:MaxBlocksPerRequest]
	}

	blocks, err := sm.getBlocks(req.Hashes)
	if err != nil {
		return
	}

	// Send blocks
	blocks = trimByteSliceBatch(blocks, MaxBlocksPerRequest, SyncBlocksResponseByteBudget)
	blocksData, _ := json.Marshal(blocks)
	if err := writeMessage(s, SyncMsgBlocks, blocksData); err != nil {
		return
	}
}

// handleGetBlocksByHeight handles requests for blocks by height range
func (sm *SyncManager) handleGetBlocksByHeight(s network.Stream, data []byte) {
	var req BlocksByHeightRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	if req.MaxBlocks <= 0 {
		if err := writeMessage(s, SyncMsgBlocks, []byte("[]")); err != nil {
			return
		}
		return
	}

	if req.MaxBlocks > MaxBlocksPerRequest {
		req.MaxBlocks = MaxBlocksPerRequest
	}

	localTip := sm.getStatus().Height
	if req.StartHeight > localTip {
		if err := writeMessage(s, SyncMsgBlocks, []byte("[]")); err != nil {
			return
		}
		return
	}

	available := localTip - req.StartHeight + 1
	if uint64(req.MaxBlocks) > available {
		req.MaxBlocks = int(available)
	}

	from := s.Conn().RemotePeer()
	if !sm.rangeResendAllowed(from, req.StartHeight, time.Now()) {
		_ = writeMessage(s, SyncMsgBlocks, []byte("[]"))
		return
	}

	blocks, err := sm.getBlocksByHeight(req.StartHeight, req.MaxBlocks)
	if err != nil {
		return
	}

	blocks = trimByteSliceBatch(blocks, MaxBlocksPerRequest, SyncBlocksResponseByteBudget)
	blocksData, _ := json.Marshal(blocks)
	if err := writeMessage(s, SyncMsgBlocks, blocksData); err != nil {
		return
	}
	if len(blocks) > 0 {
		sm.recordRangeSent(from, req.StartHeight, time.Now())
	}
}

// handleNewBlock processes a new block announcement and relays to other peers
func (sm *SyncManager) handleNewBlock(from peer.ID, data []byte) {
	if sm.processBlock != nil || sm.processAnnouncedBlock != nil {
		var err error
		if sm.processAnnouncedBlock != nil {
			err = sm.processAnnouncedBlock(from, data)
		} else {
			err = sm.processBlock(data)
		}
		if err != nil {
			switch {
			case sm.isOrphanErr(err):
				// We missed its parent. Waiting for the next poll leaves the
				// node on a stale tip, and later blocks would be orphans too.
				sm.requestSyncCheck()
			case !sm.isDuplicateErr(err):
				sm.banInvalidBlockPeer(from, "invalid new block announcement")
			}
			return // duplicate, orphan, or invalid — don't relay
		}
		// First delivery of a block we accepted builds a track record.
		sm.node.RewardPeer(from)
		// Notify daemon (miner restart, wallet subscribers)
		if sm.onBlockAccepted != nil {
			sm.onBlockAccepted(data)
		}
		// Relay to other peers (exclude sender)
		sm.relayBlock(from, data)
	}
}

// relayBlock sends a block to all peers except the sender
func (sm *SyncManager) relayBlock(from peer.ID, data []byte) {
	peers := sm.node.Peers()
	for _, p := range peers {
		if p == from {
			continue
		}
		go func(pid peer.ID) {
			ctx, cancel := context.WithTimeout(sm.ctx, 10*time.Second)
			defer cancel()

			s, err := sm.node.host.NewStream(ctx, pid, protocolID(params.ProtocolSync))
			if err != nil {
				return
			}
			defer s.Close()

			if err := writeMessage(s, SyncMsgNewBlock, data); err != nil {
				return
			}
		}(p)
	}
}

// handleGetMempool responds to a mempool request
func (sm *SyncManager) handleGetMempool(s network.Stream) {
	if sm.getMempool == nil {
		if err := writeMessage(s, SyncMsgMempool, []byte("[]")); err != nil {
			return
		}
		return
	}

	txs := sm.getMempool()
	txs = trimByteSliceBatch(txs, MaxSyncMempoolTxCount, SyncMempoolResponseByteBudget)
	if txs == nil {
		txs = [][]byte{}
	}
	data, err := json.Marshal(txs)
	if err != nil {
		if err := writeMessage(s, SyncMsgMempool, []byte("[]")); err != nil {
			return
		}
		return
	}

	if err := writeMessage(s, SyncMsgMempool, data); err != nil {
		return
	}
}

// syncLoop periodically checks if we need to sync
func (sm *SyncManager) syncLoop() {
	// Initial sync after short delay
	time.Sleep(5 * time.Second)
	sm.checkSync()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-ticker.C:
			sm.checkSync()
		}
	}
}

// TriggerSync forces a sync check (called when new peer connects)
func (sm *SyncManager) TriggerSync() {
	sm.requestSyncCheck()
}

// RequestSyncCheck asks for a status check soon, for callers that learned this
// node may be behind.
func (sm *SyncManager) RequestSyncCheck() {
	sm.requestSyncCheck()
}

func (sm *SyncManager) requestSyncCheck() {
	// Coalesce triggers (buffer size 1).
	select {
	case sm.statusSyncCh <- struct{}{}:
	default:
	}
}

func (sm *SyncManager) statusSyncLoop() {
	// ctx may be nil when Start wasn't called; in that case this loop can't run.
	if sm.ctx == nil {
		return
	}

	var lastRun time.Time
	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-sm.statusSyncCh:
			// Debounce to bound how often status spam can force work.
			if !lastRun.IsZero() {
				if wait := sm.statusSyncMinInterval - time.Since(lastRun); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-sm.ctx.Done():
						timer.Stop()
						return
					}
				}
			}
			lastRun = time.Now()
			sm.checkSync()
		}
	}
}

// checkSync checks if we're behind and need to sync
func (sm *SyncManager) checkSync() {
	sm.mu.Lock()
	if sm.syncing {
		// Don't lose the request: whatever prompted it may be missed by this run.
		sm.recheckAfterRun = true
		sm.mu.Unlock()
		return
	}
	sm.mu.Unlock()

	peers := sm.node.Peers()
	if len(peers) == 0 {
		return
	}

	ourStatus := sm.getStatus()

	// Query status from all peers concurrently
	type statusResult struct {
		ps  PeerStatus
		err error
	}
	resultCh := make(chan statusResult, len(peers))
	for _, p := range peers {
		go func(pid peer.ID) {
			status, err := sm.getStatusFrom(pid)
			resultCh <- statusResult{ps: PeerStatus{Peer: pid, Status: status}, err: err}
		}(p)
	}

	var peerStatuses []PeerStatus
	statusTimeout := time.After(15 * time.Second)
collectStatuses:
	for range len(peers) {
		select {
		case r := <-resultCh:
			if r.err == nil {
				peerStatuses = append(peerStatuses, r.ps)
			}
		case <-statusTimeout:
			break collectStatuses
		case <-sm.ctx.Done():
			return
		}
	}

	if len(peerStatuses) == 0 {
		return
	}

	// Peers that recently failed to deliver the work they advertised don't
	// count; they can't start another run until their cooldown ends.
	responded := peerStatuses
	peerStatuses = sm.withoutCooledDownPeers(peerStatuses, time.Now())

	// Find peers with the most work
	var maxWork uint64
	for _, ps := range peerStatuses {
		if ps.Status.TotalWork > maxWork {
			maxWork = ps.Status.TotalWork
		}
	}

	// Collect all peers with max work (or close to it)
	var syncPeers []PeerStatus
	for _, ps := range peerStatuses {
		if ps.Status.TotalWork >= maxWork {
			syncPeers = append(syncPeers, ps)
		}
	}

	if len(syncPeers) == 0 {
		return
	}

	// Only sync when peers have strictly more cumulative work.
	// Height alone is not sufficient: a longer chain with less work should
	// not trigger sync in a heaviest-chain protocol. Using height as a
	// trigger causes infinite sync loops when our fork is heavier but
	// shorter — downloaded blocks land as side-chain entries, the chain
	// never advances, and the retry fires endlessly.
	if maxWork <= ourStatus.TotalWork {
		sm.peerTipChecked.Store(true)
		return
	}

	// Use the highest height among max work peers as target
	targetHeight := syncPeers[0].Status.Height
	for _, ps := range syncPeers {
		if ps.Status.Height > targetHeight {
			targetHeight = ps.Status.Height
		}
	}

	// Status messages prove nothing, so every peer ahead of us takes part: a false
	// claim at the top can't keep an honest peer with fewer blocks from serving them.
	aheadPeers := make([]PeerStatus, 0, len(peerStatuses))
	for _, ps := range peerStatuses {
		if ps.Status.TotalWork > ourStatus.TotalWork {
			aheadPeers = append(aheadPeers, ps)
		}
	}
	corroborated := sm.mostDialedPeersAhead(responded, peerStatuses, ourStatus.TotalWork) && !sm.takeCorroborationPause()

	// Peers we dialed first, then the most work: those are the likeliest to hold the
	// blocks, and the first few peers get the downloaders. Shuffled beforehand so
	// equal peers share the load.
	cryptoShuffle(aheadPeers)
	sort.SliceStable(aheadPeers, func(i, j int) bool {
		di, dj := sm.isDialedPeer(aheadPeers[i].Peer), sm.isDialedPeer(aheadPeers[j].Peer)
		if di != dj {
			return di
		}
		return aheadPeers[i].Status.TotalWork > aheadPeers[j].Status.TotalWork
	})
	go sm.parallelSyncFrom(aheadPeers, targetHeight, corroborated)
}

func (sm *SyncManager) runTimeLimit() time.Duration {
	if sm.syncRunTimeLimit > 0 {
		return sm.syncRunTimeLimit
	}
	return defaultSyncRunTimeLimit
}

// isDialedPeer reports whether we dialed this peer.
func (sm *SyncManager) isDialedPeer(pid peer.ID) bool {
	return sm.node != nil && sm.node.host != nil && sm.node.isDialedPeer(pid)
}

// mostDialedPeersAhead reports whether most responding peers we dialed have more
// work than us, and enough of them answered for that to mean something. Anyone can
// connect to us for free, so inbound peers don't count. Peers in cooldown count as
// answering but not as ahead.
func (sm *SyncManager) mostDialedPeersAhead(responded, eligible []PeerStatus, ourWork uint64) bool {
	dialed, ahead := 0, 0
	for _, ps := range responded {
		if sm.isDialedPeer(ps.Peer) {
			dialed++
		}
	}
	for _, ps := range eligible {
		if sm.isDialedPeer(ps.Peer) && ps.Status.TotalWork > ourWork {
			ahead++
		}
	}
	return dialed >= syncMinDialedQuorum && ahead*2 > dialed
}

// takeCorroborationPause reports whether the last corroborated run delivered
// nothing, and clears it: the next run then has to prove itself, which lets peers
// that keep claiming and delivering nothing cool down.
func (sm *SyncManager) takeCorroborationPause() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	paused := sm.pauseCorroboration
	sm.pauseCorroboration = false
	return paused
}

// dialedPeerTarget returns the height most dialed peers in this run agree on (the
// lower median of their claims), or the run's target if none is in it. A
// corroborated run is only behind up to here: one far higher claim, from a stranger
// or a dialed peer, mustn't borrow the others' credibility.
func (sm *SyncManager) dialedPeerTarget(peers []PeerStatus, targetHeight uint64) uint64 {
	heights := make([]uint64, 0, len(peers))
	for _, ps := range peers {
		if sm.isDialedPeer(ps.Peer) {
			heights = append(heights, ps.Status.Height)
		}
	}
	if len(heights) == 0 {
		return targetHeight
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	return min(heights[(len(heights)-1)/2], targetHeight)
}

// syncStalled reports whether a run should give up. Any run gets syncGiveUpAfter
// between processed blocks. A run most dialed peers don't back also ends after
// claimGiveUpAfter with no block data at all, so a peer sending nothing is dropped
// quickly while a slow transfer of a large batch isn't mistaken for one.
func (sm *SyncManager) syncStalled(lastProgress, now time.Time) bool {
	if now.Sub(lastProgress) > syncGiveUpAfter {
		return true
	}
	sm.mu.RLock()
	corroborated := sm.syncRunCorroborated
	behindTarget := sm.syncBehindTarget
	sm.mu.RUnlock()
	// Dialed peers' backing only runs up to the height they claim.
	if corroborated && sm.getStatus().Height < behindTarget {
		return false
	}
	lastActivity := lastProgress
	if at := time.Unix(0, sm.lastBlockDataAt.Load()); at.After(lastActivity) {
		lastActivity = at
	}
	return now.Sub(lastActivity) > sm.claimGiveUpAfter
}

// blockDataReader records when block data arrives. A reply too small to hold a
// block doesn't count, so an empty answer can't keep a run alive.
type blockDataReader struct {
	r    io.Reader
	at   *atomic.Int64
	seen int
}

func (b *blockDataReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.seen += n
	if n > 0 && b.seen >= minBlockDataBytes {
		b.at.Store(time.Now().UnixNano())
	}
	return n, err
}

// withoutCooledDownPeers drops peers whose advertised work is being ignored and
// forgets cooldowns that have ended.
func (sm *SyncManager) withoutCooledDownPeers(statuses []PeerStatus, now time.Time) []PeerStatus {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for p, until := range sm.claimCooldowns {
		if !now.Before(until) {
			delete(sm.claimCooldowns, p)
		}
	}
	if len(sm.claimCooldowns) == 0 {
		return statuses
	}
	kept := make([]PeerStatus, 0, len(statuses))
	for _, ps := range statuses {
		if _, cooling := sm.claimCooldowns[ps.Peer]; !cooling {
			kept = append(kept, ps)
		}
	}
	return kept
}

// syncRunUnverified reports whether the active run's target is still unproven.
func (sm *SyncManager) syncRunUnverified() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.syncClaimUnverified
}

// peerIDsForLog names up to three of a run's peers for its log line.
func peerIDsForLog(peers []PeerStatus) string {
	const maxLogged = 3
	ids := make([]string, 0, maxLogged)
	for i, ps := range peers {
		if i == maxLogged {
			break
		}
		ids = append(ids, ps.Peer.String())
	}
	return strings.Join(ids, ",")
}

// getStatusFrom requests status from a peer
func (sm *SyncManager) getStatusFrom(p peer.ID) (ChainStatus, error) {
	ctx, cancel := context.WithTimeout(sm.ctx, 30*time.Second)
	defer cancel()

	s, err := sm.node.host.NewStream(ctx, p, protocolID(params.ProtocolSync))
	if err != nil {
		return ChainStatus{}, err
	}
	defer func() {
		s.Close()
	}()
	if err := s.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return ChainStatus{}, err
	}

	// Send our status
	ourStatus := sm.getStatus()
	statusData, _ := json.Marshal(ourStatus)
	if err := writeMessage(s, SyncMsgStatus, statusData); err != nil {
		return ChainStatus{}, err
	}

	// Read their status
	msgType, data, err := readSyncMessage(s)
	if err != nil {
		return ChainStatus{}, err
	}

	if msgType != SyncMsgStatus {
		return ChainStatus{}, fmt.Errorf("unexpected message type: %d", msgType)
	}

	var status ChainStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return ChainStatus{}, err
	}
	if status.NetworkID != params.NetworkID || status.ChainID != params.ChainID {
		sm.node.PenalizePeer(p, ScorePenaltyInvalid, "chain status mismatch")
		return ChainStatus{}, fmt.Errorf("chain status mismatch")
	}

	return status, nil
}

// parallelSyncFrom performs parallel chain sync from multiple peers. corroborated
// says most peers we dialed are ahead, so the node really is behind from the start.
// Otherwise the target is only a claim, and the run counts as being behind once it
// has delivered syncBehindProofBlocks blocks above the local tip.
func (sm *SyncManager) parallelSyncFrom(peers []PeerStatus, targetHeight uint64, corroborated bool) {
	sm.mu.Lock()
	// A peer is ahead. Mark the tip checked only under the lock that registers
	// the sync run, so PeerTipChecked never reads true while IsSyncing and
	// IsSyncingFarBehind still describe the pre-sync tip as current.
	sm.peerTipChecked.Store(true)
	if sm.syncing {
		sm.mu.Unlock()
		return
	}
	sm.syncing = true
	if len(peers) > 0 {
		sm.syncPeer = peers[0].Peer // Primary peer for display
	}
	sm.syncStartTime = time.Now()
	sm.syncTarget = targetHeight
	sm.syncClaimUnverified = !corroborated
	sm.syncRunCorroborated = corroborated
	sm.syncBehindTarget = targetHeight
	if corroborated {
		sm.syncBehindTarget = sm.dialedPeerTarget(peers, targetHeight)
	}
	asked := &sync.Map{}
	sm.runAsked.Store(asked)
	sm.downloadBuffer = make(map[uint64]DownloadedBlock) // Reset buffer
	sm.mu.Unlock()

	defer func() {
		sm.mu.Lock()
		sm.syncing = false
		sm.syncPeer = ""
		sm.syncTarget = 0
		sm.syncClaimUnverified = false
		sm.syncRunCorroborated = false
		sm.syncBehindTarget = 0
		if sm.recheckAfterRun {
			sm.recheckAfterRun = false
			sm.requestSyncCheck()
		}
		sm.syncProgress = 0
		sm.downloadBuffer = make(map[uint64]DownloadedBlock) // Clear buffer
		sm.mu.Unlock()
	}()

	// Get our current height
	ourStatus := sm.getStatus()
	var claimedWork uint64
	for _, ps := range peers {
		claimedWork = max(claimedWork, ps.Status.TotalWork)
	}
	startHeight := computeSyncStartHeight(ourStatus.Height, targetHeight)
	if targetHeight <= ourStatus.Height && claimedWork > ourStatus.TotalWork {
		// More work at or below our height: only a heavier fork can justify it.
		startHeight = 1
		if targetHeight > syncForkFetchDepth {
			startHeight = targetHeight - syncForkFetchDepth
		}
	}

	sm.mu.Lock()
	sm.syncProgress = ourStatus.Height
	behindTarget := sm.syncBehindTarget
	sm.mu.Unlock()

	numDownloaders := min(len(peers)*syncMaxDownloaders, syncMaxDownloaders)

	// Channel to receive downloaded blocks
	blockChan := make(chan DownloadedBlock, MaxBlocksPerRequest*numDownloaders)

	// Channel to distribute work
	workChan := make(chan SyncWork, numDownloaders)
	retryChan := make(chan SyncWork, numDownloaders)
	var processed atomic.Uint64
	processed.Store(startHeight)

	// Context for downloaders
	ctx, cancel := context.WithCancel(sm.ctx)
	defer cancel()

	var downloadWg sync.WaitGroup

	// Written by the processor only, and read after it finishes.
	delivered := make(map[peer.ID]struct{}) // peers whose block above our tip was accepted
	deliveredBlocks := 0
	ranOutOfData := false

	var timedOut sync.Map
	penalizeTimeout := func(p peer.ID) {
		if _, seen := timedOut.LoadOrStore(p, struct{}{}); !seen {
			sm.node.PenalizePeer(p, ScorePenaltyTimeout, "fetch blocks timeout")
		}
	}

	// Start downloader goroutines
	for i := 0; i < numDownloaders; i++ {
		downloadWg.Add(1)
		peerID := peers[i%len(peers)].Peer
		go func(p peer.ID) {
			defer downloadWg.Done()
			sm.downloader(ctx, p, workChan, retryChan, blockChan, penalizeTimeout)
		}(peerID)
	}

	// Start work distributor
	go func() {
		currentHeight := startHeight
		for currentHeight <= targetHeight {
			for currentHeight > processed.Load()+syncReadAhead {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-ctx.Done():
					return
				}
			}
			batchSize := uint64(MaxBlocksPerRequest)
			if currentHeight+batchSize-1 > targetHeight {
				batchSize = targetHeight - currentHeight + 1
			}

			select {
			case workChan <- SyncWork{Start: currentHeight, End: currentHeight + batchSize - 1}:
				currentHeight += batchSize
			case <-ctx.Done():
				return
			}
		}
		close(workChan)
	}()

	// Process blocks sequentially in order
	nextHeight := startHeight
	processorDone := make(chan struct{})

	go func() {
		defer close(processorDone)
		stall := time.NewTimer(sm.hedgeAfter())
		defer stall.Stop()
		lastProgress := time.Now()
		hedgeNext := 0
		var hedging atomic.Bool
		for nextHeight <= targetHeight {
			var blockData []byte
			var sourcePeer peer.ID

			// Try to get block from buffer first
			sm.mu.Lock()
			buffered, inBuffer := sm.downloadBuffer[nextHeight]
			if inBuffer {
				delete(sm.downloadBuffer, nextHeight)
			}
			sm.mu.Unlock()
			if inBuffer {
				blockData = buffered.Data
				sourcePeer = buffered.Peer
			}

			if !inBuffer {
				// Wait for block to arrive
				select {
				case block := <-blockChan:
					if block.Height < nextHeight {
						continue
					}
					if block.Height == nextHeight {
						blockData = block.Data
						sourcePeer = block.Peer
					} else {
						// Out of order - buffer it
						sm.mu.Lock()
						sm.downloadBuffer[block.Height] = block
						sm.mu.Unlock()
						continue
					}
				case <-ctx.Done():
					return
				case <-stall.C:
					if sm.syncStalled(lastProgress, time.Now()) {
						ranOutOfData = true
						return
					}
					if hedging.CompareAndSwap(false, true) {
						sm.hedgeFetch(ctx, hedgePeers(peers, &hedgeNext), nextHeight, targetHeight, blockChan, func() { hedging.Store(false) })
					}
					stall.Reset(sm.hedgeAfter())
					continue
				}
			}

			// Process block (with orphan recovery for reorg sync).
			accepted, err := sm.ProcessBlockWithRecoveryCtx(ctx, blockData, peers)
			if err != nil {
				// Non-orphan rejection here indicates invalid block proof/data.
				// Attribute to source peer when available and penalize deterministically.
				if sourcePeer != "" && !sm.isOrphanErr(err) && !sm.isDuplicateErr(err) {
					sm.penalizeInvalidBlockPeer(sourcePeer, "invalid block during sync")
				}
				log.Printf("[sync] block %d failed: %v", nextHeight, err)
				return
			}
			// Only reward after a block has been accepted (not merely downloaded/queued).
			if accepted && sourcePeer != "" {
				sm.node.RewardPeer(sourcePeer)
			}

			// Update progress
			sm.mu.Lock()
			sm.syncProgress = nextHeight
			if accepted && nextHeight > ourStatus.Height {
				delivered[sourcePeer] = struct{}{}
				deliveredBlocks++
				if deliveredBlocks >= syncBehindProofBlocks {
					sm.syncClaimUnverified = false
				}
				// Blocks arriving well past what dialed peers claim prove the higher
				// target is real, so the node keeps counting as behind until it's reached.
				if corroborated && nextHeight >= behindTarget+syncBehindProofBlocks {
					sm.syncBehindTarget = targetHeight
				}
			}
			delete(sm.downloadBuffer, nextHeight)
			sm.mu.Unlock()

			nextHeight++
			processed.Store(nextHeight)
			lastProgress = time.Now()
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(sm.hedgeAfter())

		}
	}()

	// Wait for processing to complete or timeout
	select {
	case <-processorDone:
		// log.Printf("[sync] sync complete at height %d", nextHeight-1)
	case <-time.After(sm.runTimeLimit()):
		// log.Printf("[sync] sync timeout")
	case <-ctx.Done():
		// log.Printf("[sync] sync cancelled")
	}

	// Signal downloaders to stop
	cancel()
	downloadWg.Wait()
	<-processorDone

	final := sm.getStatus()
	claimUnmet := final.Height < targetHeight || (targetHeight <= ourStatus.Height && final.TotalWork < claimedWork)
	if claimUnmet && sm.ctx.Err() == nil {
		log.Printf("[sync] incomplete block sync: local=%d target=%d next=%d peers=%d (%s) delivered=%d elapsed=%s",
			final.Height, targetHeight, nextHeight, len(peers), peerIDsForLog(peers), len(delivered), time.Since(sm.syncStartTime).Round(time.Millisecond))
		// Only a run that ran out of data says anything about its peers: not the
		// run's time cap, and not a rejected block, which costs only its sender.
		// Short of what most dialed peers claim, the fault is probably ours.
		if ranOutOfData {
			if !corroborated || final.Height >= behindTarget {
				now := time.Now()
				for _, ps := range unmetClaims(peers, asked, final.TotalWork) {
					sm.coolDownPeer(ps.Peer, now)
				}
			} else if len(delivered) == 0 {
				sm.mu.Lock()
				sm.pauseCorroboration = true
				sm.mu.Unlock()
			}
		}
	}

	// Fetch the mempool outside the run: peers that never answer shouldn't hold the
	// sync slot. An incomplete run must retry block sync instead.
	if !claimUnmet && sm.ctx.Err() == nil && len(peers) > 0 && sm.getMempool != nil && sm.processTx != nil {
		go sm.syncMempoolFrom(peers[:min(syncMempoolPeers, len(peers))])
	}

	// If we didn't reach target, immediately re-check instead of waiting
	// for the 30s periodic tick. Covers transient peer failures.
	if claimUnmet && sm.ctx != nil && sm.ctx.Err() == nil {
		go func() {
			select {
			case <-time.After(3 * time.Second):
				sm.checkSync()
			case <-sm.ctx.Done():
			}
		}()
	}
}

// unmetClaims returns the peers this run asked for blocks that advertised more work
// than we now have. Peers it never asked have proved nothing either way.
func unmetClaims(peers []PeerStatus, asked *sync.Map, work uint64) []PeerStatus {
	out := make([]PeerStatus, 0, len(peers))
	for _, ps := range peers {
		if _, ok := asked.Load(ps.Peer); ok && ps.Status.TotalWork > work {
			out = append(out, ps)
		}
	}
	return out
}

// syncMempoolFrom fetches the mempool from the first of these peers that answers.
func (sm *SyncManager) syncMempoolFrom(peers []PeerStatus) {
	if !sm.mempoolSyncing.CompareAndSwap(false, true) {
		return
	}
	defer sm.mempoolSyncing.Store(false)
	for _, ps := range peers {
		if sm.ctx.Err() != nil {
			return
		}
		if err := sm.fetchAndProcessMempool(ps.Peer); err == nil {
			return
		}
	}
	log.Printf("[sync] mempool sync failed from all %d peers", len(peers))
}

// coolDownPeer ignores a peer's advertised work for syncClaimCooldown.
func (sm *SyncManager) coolDownPeer(pid peer.ID, now time.Time) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.claimCooldowns == nil {
		sm.claimCooldowns = make(map[peer.ID]time.Time)
	}
	sm.claimCooldowns[pid] = now.Add(syncClaimCooldown)
}

func (sm *SyncManager) observeFetch(d time.Duration) {
	old := sm.fetchEWMA.Load()
	if old == 0 {
		sm.fetchEWMA.Store(int64(d))
		return
	}
	sm.fetchEWMA.Store(old + (int64(d)-old)/8)
}

func (sm *SyncManager) hedgeAfter() time.Duration {
	ewma := time.Duration(sm.fetchEWMA.Load())
	if ewma == 0 {
		return syncHedgeMax
	}
	return min(max(3*ewma, syncHedgeMin), syncHedgeMax)
}

func hedgePeers(peers []PeerStatus, next *int) []PeerStatus {
	n := min(syncHedgePeers, len(peers))
	out := make([]PeerStatus, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, peers[(*next+i)%len(peers)])
	}
	*next += n
	return out
}

func (sm *SyncManager) hedgeFetch(ctx context.Context, peers []PeerStatus, from, targetHeight uint64, blockChan chan<- DownloadedBlock, done func()) {
	if len(peers) == 0 || from > targetHeight {
		done()
		return
	}
	count := int(min(targetHeight-from+1, uint64(MaxBlocksPerRequest)))
	go func() {
		defer done()
		blocks, p := sm.fetchBlocksFromAnyPeerCtx(ctx, peers, from, count)
		if len(blocks) > count {
			blocks = blocks[:count]
		}
		for i, data := range blocks {
			select {
			case blockChan <- DownloadedBlock{Height: from + uint64(i), Data: data, Peer: p}:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func computeSyncStartHeight(ourHeight uint64, targetHeight uint64) uint64 {
	if targetHeight <= ourHeight {
		return ourHeight + 1
	}
	startHeight := ourHeight + 1
	// Keep existing overlap semantics for near-tip sync.
	if gap := targetHeight - ourHeight; gap <= 50 && ourHeight > 10 {
		startHeight = ourHeight - 10
	}
	return startHeight
}

// downloader fetches blocks for assigned ranges
func (sm *SyncManager) downloader(ctx context.Context, p peer.ID, workChan <-chan SyncWork, retryChan chan SyncWork, blockChan chan<- DownloadedBlock, penalizeTimeout func(peer.ID)) {
	for {
		var work SyncWork
		select {
		case work = <-retryChan:
		default:
			select {
			case work = <-retryChan:
			case w, ok := <-workChan:
				if !ok {
					// Stay available for another downloader's retry until the run
					// ends, so a lagging peer doesn't leave work waiting for a hedge.
					workChan = nil
					continue
				}
				work = w
			case <-ctx.Done():
				return
			}
		}

		for start := work.Start; start <= work.End; {
			batchSize := int(work.End - start + 1)
			if batchSize > MaxBlocksPerRequest {
				batchSize = MaxBlocksPerRequest
			}

			fetchStart := time.Now()
			blocks, err := sm.fetchBlocksByHeightCtx(ctx, p, start, batchSize)
			if err != nil || len(blocks) == 0 {
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					penalizeTimeout(p)
				}
				select {
				case retryChan <- SyncWork{Start: start, End: work.End}:
				default:
				}
				return
			}
			sm.observeFetch(time.Since(fetchStart))
			if len(blocks) > batchSize {
				blocks = blocks[:batchSize]
			}

			for i, blockData := range blocks {
				select {
				case blockChan <- DownloadedBlock{Height: start + uint64(i), Data: blockData, Peer: p}:
				case <-ctx.Done():
					return
				}
			}
			start += uint64(len(blocks))
		}
	}
}

func (sm *SyncManager) isOrphanErr(err error) bool {
	if err == nil || sm.isOrphanError == nil {
		return false
	}
	return sm.isOrphanError(err)
}

func (sm *SyncManager) isDuplicateErr(err error) bool {
	if err == nil || sm.isDuplicateError == nil {
		return false
	}
	return sm.isDuplicateError(err)
}

func (sm *SyncManager) ProcessBlockWithRecovery(blockData []byte, peers []PeerStatus) (bool, error) {
	return sm.ProcessBlockWithRecoveryCtx(sm.ctx, blockData, peers)
}

func (sm *SyncManager) ProcessBlockWithRecoveryCtx(ctx context.Context, blockData []byte, peers []PeerStatus) (bool, error) {
	if sm.processBlock == nil {
		return false, nil
	}

	err := sm.processBlock(blockData)
	if err == nil {
		return true, nil
	}
	if sm.isDuplicateErr(err) {
		// Duplicate blocks are not rewarded (peer can otherwise farm reputation by
		// serving already-known blocks), but they also aren't fatal to sync.
		return false, nil
	}
	if !sm.isOrphanErr(err) {
		return false, err
	}

	// Recover by fetching and connecting the missing parent chain by hash.
	if recErr := sm.recoverOrphanChain(ctx, blockData, peers); recErr != nil {
		return false, recErr
	}
	// Orphan recovery only returns nil after the pending chain (including blockData)
	// has been connected/processed successfully.
	return true, nil
}

func (sm *SyncManager) recoverOrphanChain(ctx context.Context, blockData []byte, peers []PeerStatus) error {
	const maxRecoveryDepth = 512
	const recoveryTimeout = 10 * time.Minute

	if len(peers) == 0 {
		return fmt.Errorf("orphan recovery failed: no peers available")
	}
	if sm.getBlockMeta == nil {
		return fmt.Errorf("orphan recovery failed: GetBlockMeta callback not configured")
	}
	if sm.getBlockHash == nil {
		return fmt.Errorf("orphan recovery failed: GetBlockHash callback not configured")
	}
	if sm.fetchBlocksByHash == nil {
		return fmt.Errorf("orphan recovery failed: FetchBlocksByHash callback not configured")
	}

	baseCtx := ctx
	if baseCtx == nil {
		baseCtx = sm.ctx
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, recoveryTimeout)
	defer cancel()

	pending := make([][]byte, 0, 8)
	pending = append(pending, blockData)
	current := blockData
	seenParents := make(map[[32]byte]struct{})

outer:
	for depth := 0; depth < maxRecoveryDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("orphan recovery timed out after fetching %d parents: %w", depth, err)
		}

		height, prevHash, err := sm.getBlockMeta(current)
		if err != nil {
			return fmt.Errorf("orphan recovery failed to decode block metadata: %w", err)
		}
		if height == 0 {
			return fmt.Errorf("orphan recovery reached genesis without finding connectable parent")
		}

		parentHash := prevHash
		if _, exists := seenParents[parentHash]; exists {
			return fmt.Errorf("orphan recovery detected parent cycle at %x", parentHash[:8])
		}
		seenParents[parentHash] = struct{}{}

		invalidPeers := make(map[peer.ID]struct{})
		var lastProcessErr error
		for len(invalidPeers) < len(peers) {
			parentData, sourcePeer, err := sm.fetchBlockByHashFromAnyPeer(ctx, peers, parentHash, invalidPeers)
			if err != nil {
				if lastProcessErr != nil {
					return fmt.Errorf(
						"orphan recovery failed while processing parent %x after %d invalid peer responses: %w",
						parentHash[:8], len(invalidPeers), lastProcessErr,
					)
				}
				return fmt.Errorf("orphan recovery failed to fetch parent %x (child height %d): %w", parentHash[:8], height, err)
			}

			// If the parent is known, this returns nil (accepted) or duplicate (already have it).
			if err := sm.processBlock(parentData); err != nil && !sm.isDuplicateErr(err) {
				if sm.isOrphanErr(err) {
					pending = append(pending, parentData)
					current = parentData
					continue outer
				}

				// At this point the response was non-empty, decodable, and hash-matching.
				// A processBlock failure here can still come from our local branch context
				// (for example, a competing-fork spend that conflicts only with the
				// current canonical branch above the fork point), so do not ban the
				// serving peer on this path. Keep trying other peers for the same parent
				// hash before failing the recovery attempt.
				invalidPeers[sourcePeer] = struct{}{}
				lastProcessErr = err
				continue
			}

			// Parent chain is now connected; replay queued children from oldest to newest.
			for i := len(pending) - 1; i >= 0; i-- {
				if err := sm.processBlock(pending[i]); err != nil && !sm.isDuplicateErr(err) {
					return fmt.Errorf("orphan recovery replay failed: %w", err)
				}
			}
			return nil
		}

		if lastProcessErr != nil {
			return fmt.Errorf(
				"orphan recovery failed while processing parent %x after %d invalid peer responses: %w",
				parentHash[:8], len(invalidPeers), lastProcessErr,
			)
		}
		return fmt.Errorf("orphan recovery failed: no peers left to try for parent %x", parentHash[:8])
	}

	return fmt.Errorf("orphan recovery exceeded max depth (%d)", maxRecoveryDepth)
}

func (sm *SyncManager) fetchBlockByHashFromAnyPeer(
	ctx context.Context, peers []PeerStatus, hash [32]byte, exclude map[peer.ID]struct{},
) ([]byte, peer.ID, error) {
	type result struct {
		block []byte
		peer  peer.ID
		err   error
	}
	if sm.fetchBlocksByHash == nil {
		return nil, "", fmt.Errorf("FetchBlocksByHash callback not configured")
	}
	if sm.getBlockHash == nil {
		return nil, "", fmt.Errorf("GetBlockHash callback not configured")
	}

	// ctx may be nil when called outside of Start() (e.g. unit tests).
	if ctx == nil {
		ctx = context.Background()
	}

	activePeers := make([]PeerStatus, 0, len(peers))
	for _, ps := range peers {
		if exclude != nil {
			if _, skip := exclude[ps.Peer]; skip {
				continue
			}
		}
		activePeers = append(activePeers, ps)
	}
	if len(activePeers) == 0 {
		return nil, "", fmt.Errorf("no peers available for block %x", hash[:8])
	}

	// Cancel remaining goroutines once we get a successful result.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan result, len(activePeers))
	for _, ps := range activePeers {
		go func(p peer.ID) {
			blocks, err := sm.fetchBlocksByHash(ctx, p, [][32]byte{hash})
			if err != nil {
				ch <- result{peer: p, err: err}
				return
			}
			if len(blocks) == 0 || len(blocks[0]) == 0 {
				sm.banInvalidBlockPeer(p, fmt.Sprintf("empty block response for requested hash %x", hash[:8]))
				ch <- result{peer: p, err: fmt.Errorf("peer %s returned no blocks for %x", p, hash[:8])}
				return
			}
			blockHash, err := sm.getBlockHash(blocks[0])
			if err != nil {
				sm.banInvalidBlockPeer(p, fmt.Sprintf("undecodable block response for requested hash %x", hash[:8]))
				ch <- result{peer: p, err: fmt.Errorf("peer %s returned undecodable block for %x: %w", p, hash[:8], err)}
				return
			}
			if blockHash != hash {
				sm.banInvalidBlockPeer(p, fmt.Sprintf("mismatched block hash response for requested hash %x", hash[:8]))
				ch <- result{peer: p, err: fmt.Errorf("peer %s returned mismatched block hash %x for %x", p, blockHash[:8], hash[:8])}
				return
			}
			ch <- result{peer: p, block: blocks[0]}
		}(ps.Peer)
	}

	var lastErr error
	for i := 0; i < len(activePeers); i++ {
		select {
		case r := <-ch:
			if r.err == nil && len(r.block) > 0 {
				return r.block, r.peer, nil
			}
			if r.err != nil {
				lastErr = r.err
			} else {
				lastErr = fmt.Errorf("peer returned empty block for %x", hash[:8])
			}
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}

	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", fmt.Errorf("block %x not found on sync peers", hash[:8])
}

// FetchBlocks fetches full blocks from a peer
func (sm *SyncManager) FetchBlocks(ctx context.Context, p peer.ID, hashes [][32]byte) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	s, err := sm.node.host.NewStream(ctx, p, protocolID(params.ProtocolSync))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
		return nil, err
	}

	req := BlocksRequest{Hashes: hashes}
	reqData, _ := json.Marshal(req)
	if err := writeMessage(s, SyncMsgGetBlocks, reqData); err != nil {
		return nil, err
	}

	msgType, data, err := readSyncMessage(s)
	if err != nil {
		return nil, err
	}

	if msgType != SyncMsgBlocks {
		return nil, fmt.Errorf("unexpected message type: %d", msgType)
	}
	if err := ensureJSONArrayMaxItems(data, MaxBlocksPerRequest); err != nil {
		return nil, fmt.Errorf("invalid blocks response: %w", err)
	}

	var blocks [][]byte
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, err
	}

	return blocks, nil
}

// fetchBlocksFromAnyPeer fires concurrent requests to ALL peers for the same
// block range and returns the first successful batch. This ensures a single
// slow or dead peer can't stall the sync.
func (sm *SyncManager) fetchBlocksFromAnyPeer(peers []PeerStatus, startHeight uint64, count int) ([][]byte, peer.ID) {
	return sm.fetchBlocksFromAnyPeerCtx(sm.ctx, peers, startHeight, count)
}

func (sm *SyncManager) fetchBlocksFromAnyPeerCtx(parent context.Context, peers []PeerStatus, startHeight uint64, count int) ([][]byte, peer.ID) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		blocks [][]byte
		peer   peer.ID
	}

	ch := make(chan result, len(peers))
	for _, ps := range peers {
		go func(p peer.ID) {
			blocks, err := sm.fetchBlocksByHeightCtx(ctx, p, startHeight, count)
			if err != nil || len(blocks) == 0 {
				ch <- result{}
			} else {
				ch <- result{blocks: blocks, peer: p}
			}
		}(ps.Peer)
	}

	for i := 0; i < len(peers); i++ {
		select {
		case r := <-ch:
			if len(r.blocks) > 0 {
				return r.blocks, r.peer
			}
		case <-ctx.Done():
			return nil, ""
		}
	}

	return nil, ""
}

// fetchBlocksByHeight requests blocks by height range (internal for sync)
func (sm *SyncManager) fetchBlocksByHeight(p peer.ID, startHeight uint64, max int) ([][]byte, error) {
	return sm.fetchBlocksByHeightCtx(sm.ctx, p, startHeight, max)
}

func (sm *SyncManager) fetchBlocksByHeightCtx(parent context.Context, p peer.ID, startHeight uint64, max int) ([][]byte, error) {
	if asked := sm.runAsked.Load(); asked != nil {
		asked.Store(p, struct{}{})
	}
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()

	s, err := sm.node.host.NewStream(ctx, p, protocolID(params.ProtocolSync))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	// NewStream's context only bounds opening the stream. Reset it as well to
	// interrupt an outstanding read when this sync run or hedge is canceled.
	stopReset := context.AfterFunc(ctx, func() { _ = s.Reset() })
	defer stopReset()
	if err := s.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
		return nil, err
	}

	req := BlocksByHeightRequest{
		StartHeight: startHeight,
		MaxBlocks:   max,
	}
	reqData, _ := json.Marshal(req)
	if err := writeMessage(s, SyncMsgGetBlocksByHeight, reqData); err != nil {
		return nil, err
	}

	msgType, data, err := readMessageWithLimit(&blockDataReader{r: s, at: &sm.lastBlockDataAt}, syncMessageMaxSize)
	if err != nil {
		return nil, err
	}

	if msgType != SyncMsgBlocks {
		return nil, fmt.Errorf("unexpected message type: %d", msgType)
	}
	if err := ensureJSONArrayMaxItems(data, MaxBlocksPerRequest); err != nil {
		return nil, fmt.Errorf("invalid blocks-by-height response: %w", err)
	}

	var blocks [][]byte
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, err
	}

	return blocks, nil
}

// BroadcastBlock announces a new block to all peers
func (sm *SyncManager) BroadcastBlock(blockData []byte) {
	peers := sm.node.Peers()

	for _, p := range peers {
		go func(pid peer.ID) {
			ctx, cancel := context.WithTimeout(sm.ctx, 10*time.Second)
			defer cancel()

			s, err := sm.node.host.NewStream(ctx, pid, protocolID(params.ProtocolSync))
			if err != nil {
				return
			}
			defer s.Close()

			if err := writeMessage(s, SyncMsgNewBlock, blockData); err != nil {
				return
			}
		}(p)
	}
}

// IsSyncing returns whether we're currently syncing
func (sm *SyncManager) IsSyncing() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.syncing
}

// IsSyncingFarBehind reports whether an active sync run is targeting a tip
// more than tolerance blocks ahead of the local chain. Any peer that briefly
// advertises more cumulative work — for example after losing a propagation
// race by fractions of a second — triggers a sync run that re-downloads a
// small overlap window and the mempool, holding IsSyncing true for many
// seconds even though the local tip is current the whole time. Callers that
// only need to avoid acting on a genuinely stale chain (such as mining
// template serving) should use this instead of IsSyncing. A run most dialed peers
// don't back counts only once it has delivered blocks above the local tip, and a
// backed run only up to the height those peers claim: a status message alone
// must not stop template serving.
func (sm *SyncManager) IsSyncingFarBehind(tolerance uint64) bool {
	sm.mu.RLock()
	syncing := sm.syncing
	target := sm.syncBehindTarget
	if target == 0 {
		target = sm.syncTarget
	}
	unverified := sm.syncClaimUnverified
	sm.mu.RUnlock()
	if !syncing || unverified {
		return false
	}
	// Compare against the live chain tip rather than syncProgress: the
	// overlap re-download makes syncProgress regress below the tip, while
	// the tip itself only moves forward.
	height := sm.getStatus().Height
	return target > height && target-height > tolerance
}

// PeerTipChecked reports whether a sync check has compared the local chain
// with at least one peer's status since this manager was created. When a peer
// had more work, it turns true only together with the resulting sync run, so
// callers that also consult IsSyncingFarBehind see a consistent picture. Until
// then the local tip may be one the network has already passed.
func (sm *SyncManager) PeerTipChecked() bool {
	return sm.peerTipChecked.Load()
}

// SyncProgress returns sync progress info (current, target, elapsed)
func (sm *SyncManager) SyncProgress() (uint64, uint64, time.Duration) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if !sm.syncing {
		return 0, 0, 0
	}

	return sm.syncProgress, sm.syncTarget, time.Since(sm.syncStartTime)
}

// fetchAndProcessMempool requests mempool transactions from a peer and processes them
func (sm *SyncManager) fetchAndProcessMempool(p peer.ID) error {
	ctx, cancel := context.WithTimeout(sm.ctx, 60*time.Second)
	defer cancel()

	s, err := sm.node.host.NewStream(ctx, p, protocolID(params.ProtocolSync))
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return err
	}

	// Request mempool
	if err := writeMessage(s, SyncMsgGetMempool, []byte{}); err != nil {
		return err
	}

	// Read response
	msgType, data, err := readSyncMessage(s)
	if err != nil {
		return err
	}

	if msgType != SyncMsgMempool {
		return fmt.Errorf("unexpected message type: %d", msgType)
	}

	// Some older peers may encode an empty mempool as JSON `null` (marshaled nil slice).
	// Treat it as empty for compatibility.
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}

	if err := ensureJSONArrayMaxItems(data, MaxSyncMempoolTxCount); err != nil {
		return err
	}

	var txs [][]byte
	if err := json.Unmarshal(data, &txs); err != nil {
		return err
	}

	// Enforce entry count and decoded byte budget before processing.
	txs = trimByteSliceBatch(txs, MaxSyncMempoolTxCount, SyncMempoolResponseByteBudget)

	// Process each transaction
	attempted := 0
	invalid := 0
	for _, txData := range txs {
		if sm.processTx != nil {
			attempted++
			if err := sm.processTx(txData); err != nil {
				// Some errors are due to our local resource limits (e.g. mempool full).
				// Those should not penalize the peer.
				if isBenignMempoolProcessErr(err) {
					continue
				}
				invalid++
				continue
			}
		}
	}

	if attempted > 0 && sm.node != nil {
		if penalty, reason, ok := mempoolInvalidPenalty(attempted, invalid); ok {
			sm.node.PenalizePeer(p, penalty, reason)
		}
	}

	return nil
}

func isBenignMempoolProcessErr(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	// Local saturation errors should not count as "peer sent invalid tx".
	if strings.Contains(s, "mempool full") || strings.Contains(s, "mempool size limit exceeded") {
		return true
	}
	return false
}

// mempoolInvalidPenalty decides if a peer should be penalized based on the ratio of invalid txs
// in a mempool sync response.
func mempoolInvalidPenalty(total, invalid int) (penalty int, reason string, ok bool) {
	if total <= 0 || invalid <= 0 {
		return 0, "", false
	}
	// Avoid penalizing on small samples (random noise / transient races).
	if total < 20 || invalid < 5 {
		return 0, "", false
	}

	// Use integer arithmetic: invalid/total in basis points.
	// ratioBp = invalid*10000/total
	ratioBp := (invalid * 10000) / total

	// Severe: mostly-invalid batch.
	if ratioBp >= 8000 {
		return ScorePenaltyMisbehave, fmt.Sprintf("mempool sync abusive: %d/%d invalid txs", invalid, total), true
	}
	// Moderate: clearly high invalid fraction.
	if ratioBp >= 3000 {
		return ScorePenaltyInvalid, fmt.Sprintf("mempool sync high invalid ratio: %d/%d invalid txs", invalid, total), true
	}
	return 0, "", false
}

func readSyncMessage(r network.Stream) (byte, []byte, error) {
	return readMessageWithLimit(r, syncMessageMaxSize)
}

func syncMessageMaxSize(msgType byte) (uint32, error) {
	switch msgType {
	case SyncMsgStatus:
		return MaxSyncStatusMessageSize, nil
	case SyncMsgGetHeaders:
		return MaxSyncGetHeadersReqSize, nil
	case SyncMsgHeaders:
		return MaxSyncHeadersMessageSize, nil
	case SyncMsgGetBlocks:
		return MaxSyncGetBlocksReqSize, nil
	case SyncMsgBlocks:
		return MaxSyncBlocksMessageSize, nil
	case SyncMsgNewBlock:
		return MaxBlockStreamPayloadSize, nil
	case SyncMsgGetMempool:
		return MaxSyncGetMempoolReqSize, nil
	case SyncMsgMempool:
		return MaxSyncMempoolMessageSize, nil
	case SyncMsgGetBlocksByHeight:
		return MaxSyncGetBlocksByHeightSz, nil
	default:
		return 0, fmt.Errorf("unknown sync message type: %d", msgType)
	}
}

func trimByteSliceBatch(items [][]byte, maxItems int, byteBudget int) [][]byte {
	if maxItems < 0 {
		maxItems = 0
	}
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	if byteBudget <= 0 || len(items) == 0 {
		return nil
	}

	total := 0
	keep := 0
	for _, item := range items {
		if total+len(item) > byteBudget {
			break
		}
		total += len(item)
		keep++
	}

	return items[:keep]
}

// ensureJSONArrayMaxItems rejects JSON arrays that exceed maxItems.
// This validates element count before full decode into [][]byte structures.
func ensureJSONArrayMaxItems(data []byte, maxItems int) error {
	if maxItems < 0 {
		maxItems = 0
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

	count := 0
	for dec.More() {
		count++
		if count > maxItems {
			return fmt.Errorf("array contains %d items (max %d)", count, maxItems)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
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

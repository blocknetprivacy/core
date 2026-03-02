package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
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

	node *Node

	// Callbacks for chain operations
	getStatus         func() ChainStatus
	getHeaders        func(startHeight uint64, max int) ([][]byte, error)
	getBlocks         func(hashes [][32]byte) ([][]byte, error)
	getBlocksByHeight func(startHeight uint64, max int) ([][]byte, error)
	processBlock      func(data []byte) error
	processHeader     func(data []byte) error
	getMempool        func() [][]byte
	processTx         func(data []byte) error
	onBlockAccepted   func(data []byte)
	isOrphanError     func(error) bool
	isDuplicateError  func(error) bool
	getBlockMeta      func(data []byte) (height uint64, prevHash [32]byte, err error)
	getBlockHash      func(data []byte) (hash [32]byte, err error)
	fetchBlocksByHash func(context.Context, peer.ID, [][32]byte) ([][]byte, error)

	// Sync state
	syncing        bool
	syncPeer       peer.ID
	syncStartTime  time.Time
	syncTarget     uint64            // Target height we're syncing to
	syncProgress   uint64            // Current height we've processed to
	downloadBuffer map[uint64]DownloadedBlock // Buffer for out-of-order blocks

	ctx    context.Context
	cancel context.CancelFunc

	// statusSyncCh serializes/rate-limits sync checks triggered by inbound status
	// messages (and other external triggers). This prevents goroutine churn when
	// peers spam status updates.
	statusSyncCh          chan struct{}
	statusSyncMinInterval time.Duration
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
	GetMempool        func() [][]byte                                              // Get all mempool transactions
	ProcessTx         func(data []byte) error                                      // Process a mempool transaction
	OnBlockAccepted   func(data []byte)                                            // Called when a new block announcement is accepted
	IsOrphanError     func(error) bool                                             // Orphan classifier for sync recovery (required for recovery to work)
	IsDuplicateError  func(error) bool                                             // Duplicate classifier — block already known, treat as success
	GetBlockMeta      func(data []byte) (uint64, [32]byte, error)                  // Extract (height, prevHash) from serialized block; required for orphan recovery
	GetBlockHash      func(data []byte) ([32]byte, error)                          // Extract block hash from serialized block; required for parent hash verification in orphan recovery
	FetchBlocksByHash func(context.Context, peer.ID, [][32]byte) ([][]byte, error) // Override block-by-hash fetching (default: p2p FetchBlocks)
}

// NewSyncManager creates a new sync manager
func NewSyncManager(node *Node, cfg SyncConfig) *SyncManager {
	sm := &SyncManager{
		node:              node,
		getStatus:         cfg.GetStatus,
		getHeaders:        cfg.GetHeaders,
		getBlocks:         cfg.GetBlocks,
		getBlocksByHeight: cfg.GetBlocksByHeight,
		processBlock:      cfg.ProcessBlock,
		processHeader:     cfg.ProcessHeader,
		getMempool:        cfg.GetMempool,
		processTx:         cfg.ProcessTx,
		onBlockAccepted:   cfg.OnBlockAccepted,
		isOrphanError:     cfg.IsOrphanError,
		isDuplicateError:  cfg.IsDuplicateError,
		getBlockMeta:      cfg.GetBlockMeta,
		getBlockHash:      cfg.GetBlockHash,
		downloadBuffer:    make(map[uint64]DownloadedBlock),
		statusSyncCh:      make(chan struct{}, 1),
		// Debounce status-triggered sync checks; periodic syncLoop still runs.
		statusSyncMinInterval: 2 * time.Second,
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
		sm.handleGetBlocks(s, data)
	case SyncMsgGetBlocksByHeight:
		sm.handleGetBlocksByHeight(s, data)
	case SyncMsgNewBlock:
		sm.handleNewBlock(s.Conn().RemotePeer(), data)
	case SyncMsgGetMempool:
		sm.handleGetMempool(s)
	}
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

	blocks, err := sm.getBlocksByHeight(req.StartHeight, req.MaxBlocks)
	if err != nil {
		return
	}

	blocks = trimByteSliceBatch(blocks, MaxBlocksPerRequest, SyncBlocksResponseByteBudget)
	blocksData, _ := json.Marshal(blocks)
	if err := writeMessage(s, SyncMsgBlocks, blocksData); err != nil {
		return
	}
}

// handleNewBlock processes a new block announcement and relays to other peers
func (sm *SyncManager) handleNewBlock(from peer.ID, data []byte) {
	if sm.processBlock != nil {
		if err := sm.processBlock(data); err != nil {
			if !sm.isDuplicateErr(err) && !sm.isOrphanErr(err) {
				sm.banInvalidBlockPeer(from, "invalid new block announcement")
			}
			return // duplicate, orphan, or invalid — don't relay
		}
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
	sm.mu.RLock()
	if sm.syncing {
		sm.mu.RUnlock()
		return
	}
	sm.mu.RUnlock()

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
		return
	}

	// Use the highest height among max work peers as target
	targetHeight := syncPeers[0].Status.Height
	for _, ps := range syncPeers {
		if ps.Status.Height > targetHeight {
			targetHeight = ps.Status.Height
		}
	}

	go sm.parallelSyncFrom(syncPeers, targetHeight)
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

// parallelSyncFrom performs parallel chain sync from multiple peers
func (sm *SyncManager) parallelSyncFrom(peers []PeerStatus, targetHeight uint64) {
	sm.mu.Lock()
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
	sm.downloadBuffer = make(map[uint64]DownloadedBlock) // Reset buffer
	sm.mu.Unlock()

	defer func() {
		sm.mu.Lock()
		sm.syncing = false
		sm.syncPeer = ""
		sm.syncTarget = 0
		sm.syncProgress = 0
		sm.downloadBuffer = make(map[uint64]DownloadedBlock) // Clear buffer
		sm.mu.Unlock()
	}()

	// Get our current height
	ourStatus := sm.getStatus()
	startHeight := computeSyncStartHeight(ourStatus.Height, targetHeight)

	sm.mu.Lock()
	sm.syncProgress = ourStatus.Height
	sm.mu.Unlock()


	// Use up to 3 peers for parallel download
	numDownloaders := min(len(peers), 3)

	// Channel to receive downloaded blocks
	blockChan := make(chan DownloadedBlock, MaxBlocksPerRequest*numDownloaders)

	// Channel to distribute work
	workChan := make(chan SyncWork, 10)

	// Context for downloaders
	ctx, cancel := context.WithCancel(sm.ctx)
	defer cancel()

	var downloadWg sync.WaitGroup

	// Start downloader goroutines
	for i := 0; i < numDownloaders && i < len(peers); i++ {
		downloadWg.Add(1)
		peerID := peers[i].Peer
		go func(p peer.ID) {
			defer downloadWg.Done()
			sm.downloader(ctx, p, workChan, blockChan)
		}(peerID)
	}

	// Start work distributor
	go func() {
		currentHeight := startHeight
		for currentHeight <= targetHeight {
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
				case <-time.After(30 * time.Second):
					// Downloader stalled - fan out to ALL peers concurrently for a batch
					batchSize := int(targetHeight - nextHeight + 1)
					if batchSize > MaxBlocksPerRequest {
						batchSize = MaxBlocksPerRequest
					}

					var rescue [][]byte
					var rescuePeer peer.ID
					for attempt := 0; attempt < 3; attempt++ {
						if ctx.Err() != nil {
							return
						}
						rescue, rescuePeer = sm.fetchBlocksFromAnyPeer(peers, nextHeight, batchSize)
						if len(rescue) > 0 {
							break
						}
						// Brief pause before retry
						select {
						case <-time.After(5 * time.Second):
						case <-ctx.Done():
							return
						}
					}

					if len(rescue) == 0 {
						return
					}
					blockData = rescue[0]
					sourcePeer = rescuePeer
					// Buffer the rest so we don't stall again immediately
					if len(rescue) > 1 {
						sm.mu.Lock()
						for i := 1; i < len(rescue); i++ {
							sm.downloadBuffer[nextHeight+uint64(i)] = DownloadedBlock{
								Height: nextHeight + uint64(i),
								Data:   rescue[i],
								Peer:   rescuePeer,
							}
						}
						sm.mu.Unlock()
					}
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
			sm.mu.Unlock()

			nextHeight++

			
		}
	}()

	// Wait for processing to complete or timeout
	select {
	case <-processorDone:
		// log.Printf("[sync] sync complete at height %d", nextHeight-1)
	case <-time.After(10 * time.Minute):
		// log.Printf("[sync] sync timeout")
	case <-ctx.Done():
		// log.Printf("[sync] sync cancelled")
	}

	// Signal downloaders to stop
	cancel()
	downloadWg.Wait()

	// After syncing blocks, request mempool — try each sync peer until one succeeds.
	if len(peers) > 0 && sm.getMempool != nil && sm.processTx != nil {
		var mempoolOK bool
		for _, ps := range peers {
			if err := sm.fetchAndProcessMempool(ps.Peer); err != nil {
				continue
			}
			mempoolOK = true
			break
		}
		if !mempoolOK {
			log.Printf("[sync] mempool sync failed from all %d peers", len(peers))
		}
	}

	// If we didn't reach target, immediately re-check instead of waiting
	// for the 30s periodic tick. Covers transient peer failures.
	finalHeight := sm.getStatus().Height
	if finalHeight < targetHeight && sm.ctx != nil && sm.ctx.Err() == nil {
		go func() {
			select {
			case <-time.After(3 * time.Second):
				sm.checkSync()
			case <-sm.ctx.Done():
			}
		}()
	}
}

func computeSyncStartHeight(ourHeight uint64, targetHeight uint64) uint64 {
	startHeight := ourHeight + 1
	// Keep existing overlap semantics for near-tip sync.
	if gap := targetHeight - ourHeight; gap <= 50 && ourHeight > 10 {
		startHeight = ourHeight - 10
	}
	return startHeight
}

// downloader fetches blocks for assigned ranges
func (sm *SyncManager) downloader(ctx context.Context, p peer.ID, workChan <-chan SyncWork, blockChan chan<- DownloadedBlock) {
	for {
		select {
		case work, ok := <-workChan:
			if !ok {
				return
			}

			// Calculate batch size
			batchSize := int(work.End - work.Start + 1)
			if batchSize > MaxBlocksPerRequest {
				batchSize = MaxBlocksPerRequest
			}

			// Fetch blocks
			blocks, err := sm.fetchBlocksByHeight(p, work.Start, batchSize)
			if err != nil {
				// log.Printf("[sync] peer %s failed to fetch blocks %d-%d: %v", p.String()[:8], work.Start, work.End, err)
				sm.node.PenalizePeer(p, ScorePenaltyTimeout, "fetch blocks timeout")
				// Exit - work distributor will handle missing blocks via timeout
				return
			}

			if len(blocks) == 0 {
				// log.Printf("[sync] peer %s returned no blocks for range %d-%d", p.String()[:8], work.Start, work.End)
				return
			}

			// Send blocks to processor
			for i, blockData := range blocks {
				height := work.Start + uint64(i)
				select {
				case blockChan <- DownloadedBlock{Height: height, Data: blockData, Peer: p}:
				case <-ctx.Done():
					return
				}
			}

		case <-ctx.Done():
			return
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

				// This peer returned hash-matching data that still fails validation.
				// Keep trying other peers for the same parent hash before failing.
				sm.banInvalidBlockPeer(sourcePeer, "invalid parent block data during orphan recovery")
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
	type result struct {
		blocks [][]byte
		peer   peer.ID
	}

	ch := make(chan result, len(peers))
	for _, ps := range peers {
		go func(p peer.ID) {
			blocks, err := sm.fetchBlocksByHeight(p, startHeight, count)
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
		case <-sm.ctx.Done():
			return nil, ""
		}
	}

	return nil, ""
}

// fetchBlocksByHeight requests blocks by height range (internal for sync)
func (sm *SyncManager) fetchBlocksByHeight(p peer.ID, startHeight uint64, max int) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(sm.ctx, 120*time.Second)
	defer cancel()

	s, err := sm.node.host.NewStream(ctx, p, protocolID(params.ProtocolSync))
	if err != nil {
		return nil, err
	}
	defer s.Close()
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

	msgType, data, err := readSyncMessage(s)
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

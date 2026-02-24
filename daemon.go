package main

import (
	"container/list"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"blocknet/p2p"
	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/peer"
)

// ErrDuplicateBlock is returned by processBlockData when the block is already known.
// ErrSideChainBlock is returned when the block is valid but landed on a fork, not the main chain.
// Both tell callers to skip relay and notification.
var (
	ErrDuplicateBlock = errors.New("duplicate block")
	ErrSideChainBlock = errors.New("side-chain block")
)

type Daemon struct {
	mu sync.RWMutex

	// Core components
	chain   *Chain
	mempool *Mempool
	miner   *Miner

	// P2P layer
	node    *p2p.Node
	syncMgr *p2p.SyncManager

	// Identity
	stealthKeys *StealthKeys

	// Block notifications for wallet auto-sync
	blockSubs   []chan *Block
	blockSubsMu sync.Mutex

	// Mined block notifications (blocks we mined)
	minedSubs   []chan *Block
	minedSubsMu sync.Mutex

	// Explorer
	explorerAddr string

	// Checkpoints
	saveCheckpoints       bool
	checkpointsFile       string
	lastCheckpointWritten uint64

	// Self-heal info (set during NewDaemon if chain was truncated)
	repairTruncatedTo uint64
	repairViolations  int
	repairFailed      bool

	// State
	ctx    context.Context
	cancel context.CancelFunc

	// Guards expensive gossip block validation to bound CPU/RAM pressure.
	gossipBlockGateMu      sync.Mutex
	gossipBlockInFlight    int
	gossipBlockLastAttempt *gossipAttemptLRU
}

const (
	// Keep expensive gossip validation bounded under announcement floods.
	maxConcurrentGossipBlockValidations = 1
	minGossipBlockValidationInterval    = 3 * time.Second

	// Bound per-peer rate limit memory under peer-id churn.
	// LRU eviction keeps active peers hot while bounding growth.
	maxGossipBlockAttemptEntries = 4096
	gossipBlockAttemptTTL        = 30 * time.Minute
)

func verifyStoredGenesisMatchesRelaunchGenesis(chain *Chain) error {
	if chain == nil {
		return fmt.Errorf("nil chain")
	}
	have := chain.GetBlockByHeight(0)
	if have == nil {
		return fmt.Errorf("chain has stored tip but no height-0 block found")
	}
	expected, err := GetGenesisBlock()
	if err != nil {
		return fmt.Errorf("failed to construct expected genesis: %w", err)
	}

	// Required by relaunch runbook: exact height-0 match (hash + header fields).
	if have.Header != expected.Header {
		return fmt.Errorf("genesis mismatch: stored header does not match relaunch genesis header")
	}
	if haveHash, expHash := have.Hash(), expected.Hash(); haveHash != expHash {
		return fmt.Errorf("genesis mismatch: stored hash %x != expected %x", haveHash[:8], expHash[:8])
	}
	// Defensive: also ensure stored genesis satisfies the current validator rules.
	if err := validateGenesisBlock(have); err != nil {
		return fmt.Errorf("genesis mismatch: stored genesis fails validation: %w", err)
	}
	return nil
}

type gossipAttemptEntry struct {
	pid  peer.ID
	last time.Time
}

type gossipAttemptLRU struct {
	cap   int
	lru   *list.List // front=MRU, back=LRU; values are gossipAttemptEntry
	index map[peer.ID]*list.Element
}

func newGossipAttemptLRU(cap int) *gossipAttemptLRU {
	if cap < 1 {
		cap = 1
	}
	return &gossipAttemptLRU{
		cap:   cap,
		lru:   list.New(),
		index: make(map[peer.ID]*list.Element, min(cap, 1024)),
	}
}

func (c *gossipAttemptLRU) PurgeBefore(cutoff time.Time) {
	if c == nil {
		return
	}
	for {
		back := c.lru.Back()
		if back == nil {
			return
		}
		ent := back.Value.(gossipAttemptEntry)
		if ent.last.After(cutoff) {
			return
		}
		c.lru.Remove(back)
		delete(c.index, ent.pid)
	}
}

func (c *gossipAttemptLRU) Get(pid peer.ID) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	if elem, ok := c.index[pid]; ok {
		c.lru.MoveToFront(elem)
		ent := elem.Value.(gossipAttemptEntry)
		return ent.last, true
	}
	return time.Time{}, false
}

func (c *gossipAttemptLRU) Set(pid peer.ID, t time.Time) {
	if c == nil {
		return
	}
	if elem, ok := c.index[pid]; ok {
		elem.Value = gossipAttemptEntry{pid: pid, last: t}
		c.lru.MoveToFront(elem)
	} else {
		c.index[pid] = c.lru.PushFront(gossipAttemptEntry{pid: pid, last: t})
	}
	for c.lru.Len() > c.cap {
		back := c.lru.Back()
		if back == nil {
			break
		}
		ent := back.Value.(gossipAttemptEntry)
		c.lru.Remove(back)
		delete(c.index, ent.pid)
	}
}

// DaemonConfig configures the daemon
type DaemonConfig struct {
	// P2P settings
	ListenAddrs []string
	SeedNodes   []string
	// Optional P2P peer limits (0 uses p2p defaults)
	P2PMaxInbound  int
	P2PMaxOutbound int

	// Mining
	EnableMining bool

	// Data directory
	DataDir string

	// ExplorerAddr is the HTTP address for the block explorer (empty = disabled)
	ExplorerAddr string

	// Checkpoints
	SaveCheckpoints bool
	FullSync        bool
}

// DefaultSeedNodes are the hardcoded bootstrap nodes
var DefaultSeedNodes = []string{
	"/ip4/46.62.203.242/tcp/28080/p2p/12D3KooWQUNGJrsU5nRXNk45FT3ZumdtWC9Sg9Xt2AgU3XkP382R",
	"/ip4/46.62.243.192/tcp/28080/p2p/12D3KooWSQTy8rav5nmapgxomAMpSrigJTUXHmjH25dtHhGU3BAM",
	"/ip4/46.62.252.254/tcp/28080/p2p/12D3KooWCJzbwahELLrssLMbvxCXAp2HeeH1nPTYHH1kFERZxxb8",
	"/ip4/46.62.202.165/tcp/28080/p2p/12D3KooWSAWJofx3Gk9Rbgs5UQFsUVCDkMDybr85v2j5gN3ocnUL",
	"/ip4/46.62.249.240/tcp/28080/p2p/12D3KooWKSgY1tDEfTNXo7AEE3tzsrnieEHmcUjYJfGz1LF72UJt",
	"/ip4/46.62.201.220/tcp/28080/p2p/12D3KooWPMeQZB8pJTavN6KXJ12LpMYfBSYDkV1md2xsYaWC8VMa",
}

// DefaultDaemonConfig returns sensible defaults
func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{
		ListenAddrs:     []string{"/ip4/0.0.0.0/tcp/28080"},
		SeedNodes:       DefaultSeedNodes,
		EnableMining:    false,
		DataDir:         DefaultDataDir,
		SaveCheckpoints: false,
		FullSync:        false,
	}
}

// NewDaemon creates a new blockchain daemon
// If stealthKeys is nil, new keys are generated
func NewDaemon(cfg DaemonConfig, stealthKeys *StealthKeys) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Use provided keys or generate new ones
	if stealthKeys == nil {
		var err error
		stealthKeys, err = GenerateStealthKeys()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to generate stealth keys: %w", err)
		}
	}

	// Create chain with persistent storage
	chain, err := NewChain(cfg.DataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create chain: %w", err)
	}

	// If chain state already exists, fail fast if the stored genesis does not
	// match the current hardcoded relaunch genesis.
	if chain.HasGenesis() {
		if err := verifyStoredGenesisMatchesRelaunchGenesis(chain); err != nil {
			if closeErr := chain.Close(); closeErr != nil {
				log.Printf("Warning: failed to close chain after genesis mismatch: %v", closeErr)
			}
			cancel()
			return nil, err
		}
	}

	// Create genesis block if chain is empty (no blocks exist)
	if !chain.HasGenesis() {
		genesis, err := GetGenesisBlock()
		if err != nil {
			if closeErr := chain.Close(); closeErr != nil {
				log.Printf("Warning: failed to close chain after genesis create error: %v", closeErr)
			}
			cancel()
			return nil, fmt.Errorf("failed to create genesis: %w", err)
		}
		if err := chain.addGenesisBlock(genesis); err != nil {
			if closeErr := chain.Close(); closeErr != nil {
				log.Printf("Warning: failed to close chain after genesis add error: %v", closeErr)
			}
			cancel()
			return nil, fmt.Errorf("failed to add genesis: %w", err)
		}
	}

	// Track self-heal info for caller to report.
	var repairTruncatedTo uint64
	var repairViolations int
	var repairFailed bool

	// Checkpoints: best-effort download/load for faster VerifyChain.
	// This is an optimization (arithmetic-only) and should never prevent startup.
	var checkpoints map[uint64][32]byte
	var checkpointHeights []uint64
	var checkpointsMaxHeight uint64
	if !cfg.FullSync {
		cpPath := checkpointsPath(cfg.DataDir)
		if _, err := ensureCheckpointsFile(cpPath); err != nil {
			// best-effort; checkpoint download failures are non-fatal
		}
		if cps, heights, maxH, err := loadCheckpointsFile(cpPath); err == nil {
			checkpoints = cps
			checkpointHeights = heights
			checkpointsMaxHeight = maxH
		} else if !errors.Is(err, os.ErrNotExist) {
			// non-fatal: proceed without checkpoints
		}
	}

	// Enable checkpoint fast-sync (skip expensive PoW up to last checkpoint) while
	// we're still below that height. This is trust-based and can be bypassed via
	// --full-sync.
	if !cfg.FullSync && len(checkpoints) > 0 && checkpointsMaxHeight > 0 {
		chain.SetTrustedCheckpoints(checkpoints, checkpointsMaxHeight)
	}

	// Verify chain integrity — truncate to last clean block if violations found.
	if chain.Height() > 0 {
		var violations []ChainViolation
		if cfg.FullSync || len(checkpoints) == 0 {
			violations = chain.VerifyChain()
		} else {
			var usedHeight uint64
			violations, usedHeight = chain.VerifyChainWithCheckpoints(checkpoints, checkpointHeights)
			if usedHeight == 0 {
				violations = chain.VerifyChain()
			}
		}
		if len(violations) > 0 {
			first := violations[0].Height
			truncateTo := first - 1
			repairViolations = len(violations)
			if err := chain.TruncateToHeight(truncateTo); err != nil {
				repairFailed = true
			} else {
				repairTruncatedTo = truncateTo
			}
		}
	}

	// Create mempool (uses chain's key image checker)
	mempool := NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)

	// Create miner (peer count wired up after node creation)
	minerCfg := MinerConfig{
		MinerSpendPub: stealthKeys.SpendPubKey,
		MinerViewPub:  stealthKeys.ViewPubKey,
	}
	miner := NewMiner(chain, mempool, minerCfg)

	// Create P2P node
	nodeCfg := p2p.DefaultNodeConfig()
	nodeCfg.ListenAddrs = cfg.ListenAddrs
	nodeCfg.SeedNodes = cfg.SeedNodes
	nodeCfg.UserAgent = "blocknet/" + Version
	if cfg.P2PMaxInbound > 0 {
		nodeCfg.MaxInbound = cfg.P2PMaxInbound
	}
	if cfg.P2PMaxOutbound > 0 {
		nodeCfg.MaxOutbound = cfg.P2PMaxOutbound
	}

	node, err := p2p.NewNode(nodeCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create P2P node: %w", err)
	}

	// Wire up peer count for miner (skip mining when no peers)
	miner.config.PeerCount = func() int { return len(node.Peers()) }

	d := &Daemon{
		chain:                  chain,
		mempool:                mempool,
		miner:                  miner,
		node:                   node,
		stealthKeys:            stealthKeys,
		saveCheckpoints:        cfg.SaveCheckpoints,
		checkpointsFile:        checkpointsPath(cfg.DataDir),
		repairTruncatedTo:      repairTruncatedTo,
		repairViolations:       repairViolations,
		repairFailed:           repairFailed,
		ctx:                    ctx,
		cancel:                 cancel,
		gossipBlockLastAttempt: newGossipAttemptLRU(maxGossipBlockAttemptEntries),
	}

	// Create sync manager with callbacks
	syncCfg := p2p.SyncConfig{
		GetStatus:         d.getChainStatus,
		GetHeaders:        d.getHeaders,
		GetBlocks:         d.getBlocks,
		GetBlocksByHeight: d.getBlocksByHeight,
		ProcessBlock:      d.processBlockData,
		ProcessHeader:     nil, // Full-block sync, no header processing
		GetMempool:        d.getMempoolTxs,
		ProcessTx:         d.processTxData,
		IsOrphanError: func(err error) bool {
			if errors.Is(err, ErrOrphanBlock) {
				return true
			}
			// "incomplete chain" means we're missing ancestor blocks locally —
			// this is our problem, not the peer's. Don't penalize them.
			if err != nil && strings.Contains(err.Error(), "incomplete chain") {
				return true
			}
			return false
		},
		IsDuplicateError: func(err error) bool {
			return errors.Is(err, ErrDuplicateBlock) || errors.Is(err, ErrSideChainBlock)
		},
		GetBlockMeta: func(data []byte) (uint64, [32]byte, error) {
			var block Block
			if err := json.Unmarshal(data, &block); err != nil {
				return 0, [32]byte{}, err
			}
			return block.Header.Height, block.Header.PrevHash, nil
		},
		GetBlockHash: func(data []byte) ([32]byte, error) {
			var block Block
			if err := json.Unmarshal(data, &block); err != nil {
				return [32]byte{}, err
			}
			return block.Hash(), nil
		},
		OnBlockAccepted: func(data []byte) {
			d.miner.NotifyNewBlock()
			var block Block
			if err := json.Unmarshal(data, &block); err == nil {
				d.notifyBlock(&block)
			}
		},
	}
	d.syncMgr = p2p.NewSyncManager(node, syncCfg)

	// Set up P2P handlers
	node.SetBlockHandler(d.handleBlock)
	node.SetTxHandler(d.handleTx)
	node.SetStemSanityValidator(func(data []byte) bool {
		_, err := DeserializeTx(data)
		return err == nil
	})

	// Store explorer config
	d.explorerAddr = cfg.ExplorerAddr

	// Initialize checkpoint writer state (best-effort).
	if d.saveCheckpoints {
		if _, _, maxH, err := loadCheckpointsFile(d.checkpointsFile); err == nil && maxH > 0 {
			d.lastCheckpointWritten = maxH
		}
	}

	return d, nil
}

// SubscribeBlocks returns a channel that receives new blocks
func (d *Daemon) SubscribeBlocks() chan *Block {
	d.blockSubsMu.Lock()
	defer d.blockSubsMu.Unlock()
	ch := make(chan *Block, 10)
	d.blockSubs = append(d.blockSubs, ch)
	return ch
}

// UnsubscribeBlocks removes a previously subscribed block channel.
func (d *Daemon) UnsubscribeBlocks(ch chan *Block) {
	if ch == nil {
		return
	}
	d.blockSubsMu.Lock()
	defer d.blockSubsMu.Unlock()
	for i, sub := range d.blockSubs {
		if sub == ch {
			d.blockSubs = append(d.blockSubs[:i], d.blockSubs[i+1:]...)
			return
		}
	}
}

// notifyBlock sends block to all subscribers
func (d *Daemon) notifyBlock(block *Block) {
	d.blockSubsMu.Lock()
	defer d.blockSubsMu.Unlock()
	for _, ch := range d.blockSubs {
		select {
		case ch <- block:
		default: // Don't block if subscriber is slow
		}
	}
}

// SubscribeMinedBlocks returns a channel that receives blocks we mined
func (d *Daemon) SubscribeMinedBlocks() chan *Block {
	d.minedSubsMu.Lock()
	defer d.minedSubsMu.Unlock()
	ch := make(chan *Block, 10)
	d.minedSubs = append(d.minedSubs, ch)
	return ch
}

// UnsubscribeMinedBlocks removes a previously subscribed mined-block channel.
func (d *Daemon) UnsubscribeMinedBlocks(ch chan *Block) {
	if ch == nil {
		return
	}
	d.minedSubsMu.Lock()
	defer d.minedSubsMu.Unlock()
	for i, sub := range d.minedSubs {
		if sub == ch {
			d.minedSubs = append(d.minedSubs[:i], d.minedSubs[i+1:]...)
			return
		}
	}
}

// notifyMinedBlock sends mined block to all subscribers
func (d *Daemon) notifyMinedBlock(block *Block) {
	d.minedSubsMu.Lock()
	defer d.minedSubsMu.Unlock()
	for _, ch := range d.minedSubs {
		select {
		case ch <- block:
		default:
		}
	}
}

// Start begins daemon operations
func (d *Daemon) Start() error {
	// Start P2P node
	if err := d.node.Start(); err != nil {
		// Bootstrap assist: if enabled, write peer.txt even when we couldn't connect to seeds.
		// This lets operators capture the peer ID for seed lists.
		if strings.TrimSpace(os.Getenv("BLOCKNET_EXPORT_PEER_ON_START_FAIL")) != "" {
			if werr := d.node.WritePeerFile("peer.txt"); werr != nil {
				log.Printf("Warning: failed to write peer.txt on start failure: %v", werr)
			}
		}
		return fmt.Errorf("failed to start P2P: %w", err)
	}

	// Start sync manager
	d.syncMgr.Start(d.ctx)

	// Start explorer if configured
	if d.explorerAddr != "" {
		explorer := NewExplorer(d)
		go func() {
			if err := explorer.Start(d.explorerAddr); err != nil {
				log.Printf("Explorer error: %v", err)
			}
		}()
	}

	

	return nil
}

// Stop gracefully shuts down the daemon
func (d *Daemon) Stop() error {
	d.cancel()

	// Stop miner
	d.miner.Stop()

	// Stop sync
	d.syncMgr.Stop()

	// Stop P2P
	if err := d.node.Stop(); err != nil {
		return err
	}

	// Close chain storage
	if err := d.chain.Close(); err != nil {
		return err
	}

	return nil
}

// StartMining begins mining blocks
func (d *Daemon) StartMining() {
	blockChan := make(chan *Block, 10)

	go func() {
		for {
			select {
			case <-d.ctx.Done():
				return
			case block := <-blockChan:
				d.handleMinedBlock(block)
			}
		}
	}()

	d.miner.Start(d.ctx, blockChan)
}

// handleMinedBlock processes a block we mined
func (d *Daemon) handleMinedBlock(block *Block) {
	if err := d.SubmitBlock(block); err != nil {
		log.Printf("Mined block rejected: %v", err)
		return
	}
}

// handleBlock processes a block from a peer
func (d *Daemon) handleBlock(from peer.ID, data []byte) {
	var block Block
	if err := json.Unmarshal(data, &block); err != nil {
		d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, "malformed block payload")
		return
	}
	if err := validateBlockCheapPrefilters(&block); err != nil {
		d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, fmt.Sprintf("block prefilter failed: %v", err))
		return
	}
	if err := d.acquireGossipBlockValidationSlot(from); err != nil {
		d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, err.Error())
		return
	}
	defer d.releaseGossipBlockValidationSlot()

	d.mu.Lock()
	defer d.mu.Unlock()

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(&block)
	if err != nil || !accepted {
		if err != nil {
			log.Printf("Rejected announced block at height %d from %s: %v", block.Header.Height, from.String()[:8], err)
			d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, fmt.Sprintf("invalid block: %v", err))
		}
		return
	}

	if !isMainChain {
		return
	}

	d.updateMempoolForAcceptedMainChain(&block, prevBest)
	d.maybeAppendCheckpointLocked(&block)

	// Relay to other peers (exclude sender)
	d.node.RelayBlock(from, data)

	// Notify wallet subscribers
	d.notifyBlock(&block)

	// Signal miner to restart with new tip
	d.miner.NotifyNewBlock()
}

// handleTx processes a transaction from a peer (fluff phase)
func (d *Daemon) handleTx(from peer.ID, data []byte) {
	tx, err := DeserializeTx(data)
	if err != nil {
		d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, "malformed transaction payload")
		return
	}
	txID, err := tx.TxID()
	if err != nil {
		d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, "invalid transaction id")
		return
	}
	if d.mempool.HasTransaction(txID) {
		return
	}

	if err := d.mempool.AddTransaction(tx, data); err != nil {
		if shouldPenalizeTxGossipRejection(err) {
			d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, fmt.Sprintf("invalid transaction: %v", err))
		}
		return
	}
}

func validateBlockCheapPrefilters(block *Block) error {
	if block == nil {
		return fmt.Errorf("nil block")
	}
	if block.Header.Version == 0 {
		return fmt.Errorf("invalid block version")
	}
	if block.Size() > MaxBlockSize {
		return fmt.Errorf("block too large: %d > %d", block.Size(), MaxBlockSize)
	}
	if len(block.Transactions) == 0 {
		return fmt.Errorf("block has no transactions")
	}
	if !block.Transactions[0].IsCoinbase() {
		return fmt.Errorf("first transaction is not coinbase")
	}
	for i := 1; i < len(block.Transactions); i++ {
		if block.Transactions[i].IsCoinbase() {
			return fmt.Errorf("multiple coinbase transactions")
		}
	}

	// Memo-era wire format requires a fixed-size encrypted memo per output.
	// When decoding block JSON into Go fixed arrays, omitted `encrypted_memo`
	// silently defaults to all-zero bytes. Reject that at the cheap prefilter
	// boundary so we don't waste expensive validation on policy-invalid blocks.
	for ti, tx := range block.Transactions {
		if tx == nil {
			return fmt.Errorf("nil transaction at index %d", ti)
		}
		for oi, out := range tx.Outputs {
			allZero := true
			for _, b := range out.EncryptedMemo[:] {
				if b != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				return fmt.Errorf("tx %d output %d: encrypted memo must not be all-zero", ti, oi)
			}
		}
	}
	return nil
}

func shouldPenalizeTxGossipRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "validation failed:") ||
		strings.Contains(msg, "coinbase transaction cannot be added to mempool") ||
		strings.Contains(msg, "double-spend: key image already in mempool")
}

func (d *Daemon) penalizeInvalidGossipPeer(pid peer.ID, penalty int, reason string) {
	if d.node == nil || pid == "" {
		return
	}
	// Gossip validation failures are treated as severe misbehavior: ban immediately.
	// (Tests and connection gating rely on deterministic bans here.)
	if penalty <= p2p.ScorePenaltyMisbehave {
		d.node.BanPeer(pid, reason)
		return
	}
	d.node.PenalizePeer(pid, penalty, reason)
}

func (d *Daemon) acquireGossipBlockValidationSlot(pid peer.ID) error {
	d.gossipBlockGateMu.Lock()
	defer d.gossipBlockGateMu.Unlock()

	if d.gossipBlockLastAttempt == nil {
		d.gossipBlockLastAttempt = newGossipAttemptLRU(maxGossipBlockAttemptEntries)
	}

	now := time.Now()
	d.gossipBlockLastAttempt.PurgeBefore(now.Add(-gossipBlockAttemptTTL))

	if last, ok := d.gossipBlockLastAttempt.Get(pid); ok && now.Sub(last) < minGossipBlockValidationInterval {
		return fmt.Errorf("block gossip rate limit exceeded")
	}
	if d.gossipBlockInFlight >= maxConcurrentGossipBlockValidations {
		return fmt.Errorf("block gossip validation busy")
	}

	d.gossipBlockLastAttempt.Set(pid, now)
	d.gossipBlockInFlight++
	return nil
}

func (d *Daemon) releaseGossipBlockValidationSlot() {
	d.gossipBlockGateMu.Lock()
	defer d.gossipBlockGateMu.Unlock()
	if d.gossipBlockInFlight > 0 {
		d.gossipBlockInFlight--
	}
}

// Chain status callbacks for sync manager

func (d *Daemon) getChainStatus() p2p.ChainStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return p2p.ChainStatus{
		BestHash:  d.chain.BestHash(),
		Height:    d.chain.Height(),
		TotalWork: d.chain.TotalWork(),
		Version:   1,
		NetworkID: params.NetworkID,
		ChainID:   params.ChainID,
	}
}

func (d *Daemon) getHeaders(startHeight uint64, max int) ([][]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var headers [][]byte
	for h := startHeight; h <= d.chain.Height() && len(headers) < max; h++ {
		block := d.chain.GetBlockByHeight(h)
		if block == nil {
			break
		}

		headerData, err := json.Marshal(block.Header)
		if err != nil {
			continue
		}
		headers = append(headers, headerData)
	}

	return headers, nil
}

func (d *Daemon) getBlocks(hashes [][32]byte) ([][]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var blocks [][]byte
	for _, hash := range hashes {
		block := d.chain.GetBlock(hash)
		if block == nil {
			continue
		}

		blockData, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks = append(blocks, blockData)
	}

	return blocks, nil
}

func (d *Daemon) processBlockData(data []byte) error {
	var block Block
	if err := json.Unmarshal(data, &block); err != nil {
		return err
	}
	if err := validateBlockCheapPrefilters(&block); err != nil {
		return fmt.Errorf("rejected p2p block at height %d: %w", block.Header.Height, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(&block)
	if err != nil {
		return fmt.Errorf("rejected p2p block at height %d: %w", block.Header.Height, err)
	}
	if !accepted {
		return ErrDuplicateBlock
	}

	if !isMainChain {
		return ErrSideChainBlock
	}

	d.updateMempoolForAcceptedMainChain(&block, prevBest)
	d.maybeAppendCheckpointLocked(&block)
	return nil
}

func (d *Daemon) maybeAppendCheckpointLocked(block *Block) {
	if !d.saveCheckpoints || block == nil {
		return
	}
	h := block.Header.Height
	if h == 0 || (h%100) != 0 {
		return
	}
	if h <= d.lastCheckpointWritten {
		return
	}
	if d.checkpointsFile == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(d.checkpointsFile), 0o755); err != nil {
		log.Printf("Warning: failed to create checkpoints dir: %v", err)
		return
	}
	f, err := os.OpenFile(d.checkpointsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("Warning: failed to append checkpoint: %v", err)
		return
	}
	defer f.Close()

	hash := block.Hash()
	line := fmt.Sprintf("%d:%s\n", h, strings.ToUpper(hex.EncodeToString(hash[:]))) // matches repo format examples
	if _, err := io.WriteString(f, line); err != nil {
		log.Printf("Warning: failed to write checkpoint: %v", err)
		return
	}
	d.lastCheckpointWritten = h
}

// updateMempoolForAcceptedMainChain updates mempool contents for a newly
// accepted main-chain block. During reorg, this applies both sides:
//   - disconnected old-main-chain blocks are re-queued (if still valid)
//   - connected new-main-chain blocks are removed as confirmed
func (d *Daemon) updateMempoolForAcceptedMainChain(block *Block, previousBest [32]byte) {
	if d.mempool == nil {
		return
	}

	// Fast path: direct extension with a single new main-chain block.
	if block.Header.PrevHash == previousBest {
		d.mempool.OnBlockConnected(block)
		return
	}

	newBest := d.chain.BestHash()
	disconnected, connected := d.collectReorgDiffBlocks(previousBest, newBest)
	if len(connected) == 0 {
		// Fallback to the accepted block if chain traversal failed unexpectedly.
		log.Printf("[reorg] mempool fallback: could not walk reorg diff (prevBest=%x newBest=%x), only connecting tip block", previousBest[:8], newBest[:8])
		d.mempool.OnBlockConnected(block)
		return
	}

	// Disconnect first so old-chain transactions are visible to conflict removal
	// when new-chain blocks are connected.
	for _, b := range disconnected {
		d.mempool.OnBlockDisconnected(b, d.txDataMapForBlock(b))
	}

	for _, b := range connected {
		d.mempool.OnBlockConnected(b)
	}
}

func (d *Daemon) collectReorgDiffBlocks(oldTip, newTip [32]byte) (disconnected []*Block, connected []*Block) {
	const maxReorgDepth = 1000

	// Walk both chains in lockstep-bounded fashion rather than loading the
	// entire old chain into memory.  We walk the new chain first (up to
	// maxReorgDepth) collecting blocks, then walk the old chain looking for
	// the common ancestor among those plus the new-chain set.

	// Step 1: walk new chain collecting blocks until we find one that is on
	// the old chain or we exhaust the depth budget.
	reversed := make([]*Block, 0, 8)
	newChainSet := make(map[[32]byte]struct{}, 64)
	for hash := newTip; ; {
		newChainSet[hash] = struct{}{}
		if hash == oldTip {
			// newTip extends oldTip (shouldn't reach here due to fast-path,
			// but handle gracefully).
			break
		}
		block := d.chain.GetBlock(hash)
		if block == nil {
			log.Printf("[reorg] failed to load new-chain block %x during reorg diff", hash[:8])
			return nil, nil
		}
		reversed = append(reversed, block)
		if block.Header.Height == 0 || len(reversed) >= maxReorgDepth {
			break
		}
		hash = block.Header.PrevHash
	}

	// Step 2: walk old chain looking for common ancestor.
	var commonAncestor [32]byte
	foundCommon := false
	oldAncestors := make(map[[32]byte]*Block, len(reversed))
	for hash := oldTip; ; {
		if _, ok := newChainSet[hash]; ok {
			commonAncestor = hash
			foundCommon = true
			break
		}
		block := d.chain.GetBlock(hash)
		if block == nil {
			log.Printf("[reorg] failed to load old-chain block %x during reorg diff", hash[:8])
			return nil, nil
		}
		oldAncestors[hash] = block
		if block.Header.Height == 0 || len(oldAncestors) >= maxReorgDepth {
			log.Printf("[reorg] no common ancestor found within %d blocks", maxReorgDepth)
			return nil, nil
		}
		hash = block.Header.PrevHash
	}
	if !foundCommon {
		return nil, nil
	}

	// Filter reversed to only blocks after the common ancestor.
	// reversed is newest-first; trim any entries at/before the ancestor.
	trimmed := reversed[:0]
	for _, b := range reversed {
		if b.Hash() == commonAncestor {
			break
		}
		trimmed = append(trimmed, b)
	}
	reversed = trimmed

	for hash := oldTip; hash != commonAncestor; {
		block, ok := oldAncestors[hash]
		if !ok || block == nil {
			log.Printf("[reorg] old-chain block %x missing from ancestors map", hash[:8])
			return nil, nil
		}
		disconnected = append(disconnected, block)
		hash = block.Header.PrevHash
	}

	connected = make([]*Block, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		connected = append(connected, reversed[i])
	}
	return disconnected, connected
}

// txDataMapForBlock builds serialized tx payloads for a block.
func (d *Daemon) txDataMapForBlock(block *Block) map[[32]byte][]byte {
	if block == nil || len(block.Transactions) == 0 {
		return nil
	}

	txDataMap := make(map[[32]byte][]byte, len(block.Transactions))
	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			continue
		}

		txDataMap[txID] = tx.Serialize()
	}

	return txDataMap
}

// getBlocksByHeight returns blocks in a height range for sync requests
func (d *Daemon) getBlocksByHeight(startHeight uint64, max int) ([][]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var blocks [][]byte
	for i := 0; i < max; i++ {
		height := startHeight + uint64(i)
		block := d.chain.GetBlockByHeight(height)
		if block == nil {
			break // Reached end of chain
		}

		blockData, err := json.Marshal(block)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, blockData)
	}

	return blocks, nil
}

// getMempoolTxs returns all serialized transactions in the mempool.
func (d *Daemon) getMempoolTxs() [][]byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.mempool.GetAllTransactionData()
}

// processTxData handles an incoming transaction from a peer
func (d *Daemon) processTxData(data []byte) error {
	tx, err := DeserializeTx(data)
	if err != nil {
		return fmt.Errorf("invalid transaction: %w", err)
	}

	// Add to mempool (validates the transaction)
	if err := d.mempool.AddTransaction(tx, data); err != nil {
		// Not necessarily an error - might be duplicate or already spent
		return nil
	}

	return nil
}

// Stats returns daemon statistics
type DaemonStats struct {
	PeerID       string `json:"peer_id"`
	Peers        int    `json:"peers"`
	ChainHeight  uint64 `json:"chain_height"`
	BestHash     string `json:"best_hash"`
	TotalWork    uint64 `json:"total_work"`
	MempoolSize  int    `json:"mempool_size"`
	MempoolBytes int    `json:"mempool_bytes"`
	Syncing      bool   `json:"syncing"`
	SyncProgress uint64 `json:"sync_progress,omitempty"`
	SyncTarget   uint64 `json:"sync_target,omitempty"`
	SyncPercent  string `json:"sync_percent,omitempty"`
	IdentityAge  string `json:"identity_age"`
}

func (d *Daemon) Stats() DaemonStats {
	d.mu.RLock()
	defer d.mu.RUnlock()

	bestHash := d.chain.BestHash()

	stats := DaemonStats{
		PeerID:       d.node.PeerID().String(),
		Peers:        len(d.node.Peers()),
		ChainHeight:  d.chain.Height(),
		BestHash:     fmt.Sprintf("%x", bestHash[:8]),
		TotalWork:    d.chain.TotalWork(),
		MempoolSize:  d.mempool.Size(),
		MempoolBytes: d.mempool.SizeBytes(),
		Syncing:      d.syncMgr.IsSyncing(),
		IdentityAge:  d.node.IdentityAge().Round(time.Second).String(),
	}

	// Add sync progress if syncing
	if stats.Syncing {
		progress, target, _ := d.syncMgr.SyncProgress()
		stats.SyncProgress = progress
		stats.SyncTarget = target
		if target > 0 {
			pct := float64(progress) / float64(target) * 100
			stats.SyncPercent = fmt.Sprintf("%.1f%%", pct)
		}
	}

	return stats
}

// Getters for components
func (d *Daemon) Chain() *Chain     { return d.chain }
func (d *Daemon) Mempool() *Mempool { return d.mempool }
func (d *Daemon) Node() *p2p.Node   { return d.node }
func (d *Daemon) Miner() *Miner     { return d.miner }
func (d *Daemon) TriggerSync()      { d.syncMgr.TriggerSync() }

// IsMining returns whether the miner is running
func (d *Daemon) IsMining() bool {
	return d.miner.IsRunning()
}

// StopMining stops the miner
func (d *Daemon) StopMining() {
	d.miner.Stop()
}

// MinerStats returns current mining statistics
func (d *Daemon) MinerStats() MinerStats {
	return d.miner.Stats()
}

// SubmitTransaction adds a transaction to mempool and broadcasts to peers.
// SubmitBlock validates a mined block, adds it to the chain, and broadcasts to peers.
func (d *Daemon) SubmitBlock(block *Block) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Validate block (PoW, difficulty, merkle root, transactions, etc.)
	if err := ValidateBlock(block, d.chain); err != nil {
		return fmt.Errorf("invalid block: %w", err)
	}

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(block)
	if err != nil {
		return fmt.Errorf("failed to process block: %w", err)
	}
	if !accepted {
		return fmt.Errorf("block not accepted (duplicate or stale)")
	}

	if isMainChain {
		d.updateMempoolForAcceptedMainChain(block, prevBest)
		d.maybeAppendCheckpointLocked(block)
		d.miner.NotifyNewBlock()
	}

	// Broadcast to peers
	blockData, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("failed to marshal block: %w", err)
	}
	d.syncMgr.BroadcastBlock(blockData)

	// Notify subscribers
	d.notifyBlock(block)
	d.notifyMinedBlock(block)

	return nil
}

func (d *Daemon) SubmitTransaction(txData []byte) error {
	tx, err := DeserializeTx(txData)
	if err != nil {
		return fmt.Errorf("invalid transaction data: %w", err)
	}

	// Validate and add to mempool
	if err := d.mempool.AddTransaction(tx, txData); err != nil {
		return fmt.Errorf("mempool rejected: %w", err)
	}

	// Broadcast via Dandelion++ for privacy.
	d.node.BroadcastTx(txData)

	return nil
}

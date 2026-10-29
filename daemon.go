package main

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	explorerMu   sync.Mutex
	explorer     *Explorer
	explorerSrv  *http.Server

	// Checkpoints
	saveCheckpoints        atomic.Bool
	checkpointsFile        string
	lastCheckpointWritten  uint64
	checkpointMu           sync.Mutex
	checkpointsInitialized bool
	fullSync               bool

	// Self-heal info (set during NewDaemon if chain was truncated)
	repairTruncatedTo uint64
	repairViolations  int
	repairFailed      bool

	// Seed mode
	seedMode     bool
	listenAddrs  []string
	peerIDServer *http.Server

	// State
	ctx    context.Context
	cancel context.CancelFunc

	// Guards expensive gossip block validation to bound CPU/RAM pressure.
	gossipBlockGateMu      sync.Mutex
	gossipBlockInFlight    int
	gossipBlockLastAttempt *gossipAttemptLRU

	// Startup tip-freshness gate for mining templates (templateTipChecked).
	syncStartedAt  atomic.Pointer[time.Time] // set when Start launches sync
	tipGateOpen    atomic.Bool
	tipGateNow     func() time.Time // nil means time.Now
	peerTipChecked func() bool      // nil means syncMgr.PeerTipChecked

	// Supplies seed nodes right before P2P dialing (DaemonConfig.ResolveSeedNodes).
	resolveSeedNodes func() []string
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
	// ResolveSeedNodes, when set, is called by Start right before P2P dialing
	// and its result replaces SeedNodes. Seed discovery can take seconds, and
	// nothing before dialing needs it, so it no longer delays the API.
	ResolveSeedNodes  func() []string
	P2PWhitelistPeers []string
	// Optional P2P peer limits (0 uses p2p defaults)
	P2PMaxInbound  int
	P2PMaxOutbound int
	SeedMode       bool

	// Mining
	EnableMining bool

	// Data directory
	DataDir string

	// ExplorerAddr is the HTTP address for the block explorer (empty = disabled)
	ExplorerAddr string

	// Checkpoints
	SaveCheckpoints bool
	FullSync        bool

	IdentityKeyPath string
	ChainCacheCap   int
}

// DefaultSeedNodes are the hardcoded bootstrap nodes
var DefaultSeedNodes = []string{
	"/ip4/46.62.203.242/tcp/28080/p2p/12D3KooWQUNGJrsU5nRXNk45FT3ZumdtWC9Sg9Xt2AgU3XkP382R",
	"/ip4/46.62.243.192/tcp/28080/p2p/12D3KooWSQTy8rav5nmapgxomAMpSrigJTUXHmjH25dtHhGU3BAM",
	"/ip4/135.181.47.224/tcp/28080/p2p/12D3KooWLRJWWeBK3Fcd4zuw2GAC3SQEbMXpMFYedAkSWCShPeeM",
	"/ip4/69.164.251.242/tcp/28080/p2p/12D3KooWJrLG1Y71bzVSTCpwyhjbJNUB8LjpWCYNQsMgEuWkH4LE",
	"/ip4/74.208.222.246/tcp/28080/p2p/12D3KooWSTdNQFi3NuP9EgTHBSSDiPuk3uMT97JqYaqQuuyhBgJx",
}

// DefaultTestnetSeedNodes are the hardcoded testnet bootstrap nodes.
var DefaultTestnetSeedNodes = []string{
	"/ip4/46.62.203.242/tcp/38080/p2p/12D3KooWCEsiN7zKfWWq1rWVC9bseiDtnA1ohykXqJmmBvkQs7Sd",
	"/ip4/46.62.243.192/tcp/38080/p2p/12D3KooWRowneDC78ZuMJEZiZ39x39yPZweh3CGKgo49C6KacL2H",
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

	startup := newStartupTimer()
	defer startup.done()

	// Create chain with persistent storage
	chain, err := NewChainWithCacheCap(cfg.DataDir, cfg.ChainCacheCap)
	startup.stage("open_chain")
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

	startup.stage("genesis_check")

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

	startup.stage("load_checkpoints")

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

	startup.stage("verify_chain")

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
	nodeCfg.BanWhitelist = cfg.P2PWhitelistPeers
	nodeCfg.UserAgent = "blocknet/" + Version
	if cfg.P2PMaxInbound > 0 {
		nodeCfg.MaxInbound = cfg.P2PMaxInbound
	}
	if cfg.P2PMaxOutbound > 0 {
		nodeCfg.MaxOutbound = cfg.P2PMaxOutbound
	}
	nodeCfg.SeedMode = cfg.SeedMode
	nodeCfg.Identity.KeyPath = cfg.IdentityKeyPath

	node, err := p2p.NewNode(nodeCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create P2P node: %w", err)
	}

	startup.stage("p2p_node")

	// Wire up peer count for miner (skip mining when no peers)
	miner.config.PeerCount = func() int { return len(node.Peers()) }

	d := &Daemon{
		chain:                  chain,
		mempool:                mempool,
		miner:                  miner,
		node:                   node,
		stealthKeys:            stealthKeys,
		checkpointsFile:        checkpointsPath(cfg.DataDir),
		fullSync:               cfg.FullSync,
		repairTruncatedTo:      repairTruncatedTo,
		repairViolations:       repairViolations,
		repairFailed:           repairFailed,
		ctx:                    ctx,
		cancel:                 cancel,
		seedMode:               cfg.SeedMode,
		listenAddrs:            cfg.ListenAddrs,
		gossipBlockLastAttempt: newGossipAttemptLRU(maxGossipBlockAttemptEntries),
		resolveSeedNodes:       cfg.ResolveSeedNodes,
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

	d.SetSaveCheckpoints(cfg.SaveCheckpoints)

	startup.stage("daemon_wiring")
	d.maybeSaveCheckpoints()

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
	if d.resolveSeedNodes != nil {
		seedsStartedAt := time.Now()
		d.node.SetSeedNodes(d.resolveSeedNodes())
		log.Printf("[perf] startup stage=resolve_seed_nodes waited=%s", time.Since(seedsStartedAt).Round(time.Millisecond))
	}

	// Start P2P node
	if err := d.node.Start(); err != nil {
		// Bootstrap assist: if enabled, write peer.txt even when we couldn't connect to seeds.
		// This lets operators capture the peer ID for seed lists.
		if d.seedMode {
			if werr := d.node.WritePeerFile("peer.txt"); werr != nil {
				log.Printf("Warning: failed to write peer.txt on start failure: %v", werr)
			}
		}
		return fmt.Errorf("failed to start P2P: %w", err)
	}

	// Start peer ID endpoint for seed nodes
	if d.seedMode && len(d.listenAddrs) > 0 {
		port := peerIDPortFromMultiaddr(d.listenAddrs[0])
		d.peerIDServer = startPeerIDServer(d.node, port)
		log.Printf("Peer ID endpoint listening on :%d", port)
	}

	// Start sync manager
	syncStartedAt := time.Now()
	d.syncStartedAt.Store(&syncStartedAt)
	d.syncMgr.Start(d.ctx)

	if !d.fullSync {
		go d.checkpointRefreshLoop()
	}

	if err := d.SetExplorerAddr(d.explorerAddr); err != nil {
		log.Printf("Explorer error: %v", err)
	}

	return nil
}

func (d *Daemon) checkpointRefreshLoop() {
	ticker := time.NewTicker(checkpointRefreshInterval)
	defer ticker.Stop()
	for {
		d.refreshCheckpoints()
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) refreshCheckpoints() {
	d.checkpointMu.Lock()
	defer d.checkpointMu.Unlock()
	if d.saveCheckpoints.Load() || d.checkpointsFile == "" {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	res, err := refreshCheckpointsFile(ctx, d.checkpointsFile)
	if err != nil {
		log.Printf("Checkpoints refresh failed: %v", err)
		return
	}
	if res.replaced {
		d.checkpointsInitialized = false
		d.chain.SetTrustedCheckpoints(res.checkpoints, res.maxHeight)
		log.Printf("Checkpoints updated from %s (%d entries, max %d, old file saved as %s.bak)", checkpointsURL(), len(res.checkpoints), res.maxHeight, d.checkpointsFile)
	}
}

// Stop gracefully shuts down the daemon
func (d *Daemon) Stop() error {
	d.cancel()

	_ = d.SetExplorerAddr("")

	if d.peerIDServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		d.peerIDServer.Shutdown(ctx)
		cancel()
	}

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

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(&block)
	if err != nil || !accepted {
		d.mu.Unlock()
		if err != nil {
			log.Printf("Rejected announced block at height %d from %s: %v", block.Header.Height, from.String()[:8], err)
			d.penalizeInvalidGossipPeer(from, p2p.ScorePenaltyMisbehave, fmt.Sprintf("invalid block: %v", err))
		}
		return
	}

	if !isMainChain {
		d.mu.Unlock()
		return
	}

	d.updateMempoolForAcceptedMainChain(&block, prevBest)
	d.mu.Unlock()

	d.maybeSaveCheckpoints()
	d.node.RelayBlock(from, data)
	d.notifyBlock(&block)
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
	height, bestHash, totalWork := d.chain.TipFast()
	if bestHash == ([32]byte{}) {
		bestHash, height, totalWork = d.chain.BestHash(), d.chain.Height(), d.chain.TotalWork()
	}

	return p2p.ChainStatus{
		BestHash:  bestHash,
		Height:    height,
		TotalWork: totalWork,
		Version:   1,
		NetworkID: params.NetworkID,
		ChainID:   params.ChainID,
	}
}

func (d *Daemon) getHeaders(startHeight uint64, max int) ([][]byte, error) {
	stored, err := d.chain.Storage().GetMainChainHeaders(startHeight, max)
	if err != nil {
		return nil, err
	}

	headers := make([][]byte, 0, len(stored))
	for i := range stored {
		headerData, err := json.Marshal(stored[i])
		if err != nil {
			continue
		}
		headers = append(headers, headerData)
	}

	return headers, nil
}

func (d *Daemon) getBlocks(hashes [][32]byte) ([][]byte, error) {
	var blocks [][]byte
	for _, hash := range hashes {
		block := d.chain.GetBlockFast(hash)
		if block == nil {
			continue
		}

		blocks = append(blocks, marshalBlockJSON(block))
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

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(&block)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("rejected p2p block at height %d: %w", block.Header.Height, err)
	}
	if !accepted {
		d.mu.Unlock()
		return ErrDuplicateBlock
	}

	if !isMainChain {
		d.mu.Unlock()
		return ErrSideChainBlock
	}

	d.updateMempoolForAcceptedMainChain(&block, prevBest)
	d.mu.Unlock()

	d.maybeSaveCheckpoints()
	return nil
}

func (d *Daemon) SetSaveCheckpoints(enabled bool) {
	d.saveCheckpoints.Store(enabled)
}

func (d *Daemon) SetExplorerAddr(addr string) error {
	d.explorerMu.Lock()
	defer d.explorerMu.Unlock()
	if d.explorerSrv != nil && addr == d.explorerAddr {
		return nil
	}
	var ln net.Listener
	if addr != "" {
		var err error
		if ln, err = net.Listen("tcp", addr); err != nil {
			return fmt.Errorf("explorer listen %s: %w", addr, err)
		}
	}
	if d.explorerSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = d.explorerSrv.Shutdown(ctx)
		cancel()
		d.explorerSrv = nil
	}
	d.explorerAddr = addr
	if addr == "" {
		return nil
	}
	if d.explorer == nil {
		d.explorer = NewExplorer(d)
	}
	srv := d.explorer.httpServer(addr)
	d.explorerSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Explorer error: %v", err)
		}
	}()
	return nil
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
		d.mempool.RemoveExpired()
		return
	}

	newBest := d.chain.BestHash()
	disconnected, connected := d.collectReorgDiffBlocks(previousBest, newBest)
	if len(connected) == 0 {
		// Fallback to the accepted block if chain traversal failed unexpectedly.
		log.Printf("[reorg] mempool fallback: could not walk reorg diff (prevBest=%x newBest=%x), only connecting tip block", previousBest[:8], newBest[:8])
		d.mempool.OnBlockConnected(block)
		d.mempool.RemoveExpired()
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

	d.mempool.RemoveExpired()
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
	stored, err := d.chain.Storage().GetMainChainBlocks(startHeight, max)
	if err != nil {
		return nil, err
	}

	blocks := make([][]byte, len(stored))
	workers := min(len(stored), runtime.GOMAXPROCS(0)/2+1)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < len(stored); i = int(next.Add(1) - 1) {
				blocks[i] = marshalBlockJSON(stored[i])
			}
		}()
	}
	wg.Wait()

	return blocks, nil
}

// getMempoolTxs returns all serialized transactions in the mempool.
func (d *Daemon) getMempoolTxs() [][]byte {
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
	PID               int    `json:"pid"`
	PeerID            string `json:"peer_id"`
	Peers             int    `json:"peers"`
	ChainHeight       uint64 `json:"chain_height"`
	BestHash          string `json:"best_hash"`
	TotalWork         uint64 `json:"total_work"`
	MempoolSize       int    `json:"mempool_size"`
	MempoolBytes      int    `json:"mempool_bytes"`
	MempoolGeneration uint64 `json:"mempool_generation"`
	Syncing           bool   `json:"syncing"`
	SyncProgress      uint64 `json:"sync_progress,omitempty"`
	SyncTarget        uint64 `json:"sync_target,omitempty"`
	SyncPercent       string `json:"sync_percent,omitempty"`
	IdentityAge       string `json:"identity_age"`
}

func (d *Daemon) Stats() DaemonStats {
	height, bestHash, totalWork := d.chain.TipFast()
	mempoolStats := d.mempool.Stats()

	stats := DaemonStats{
		PID:               os.Getpid(),
		PeerID:            d.node.PeerID().String(),
		Peers:             len(d.node.Peers()),
		ChainHeight:       height,
		BestHash:          fmt.Sprintf("%x", bestHash[:8]),
		TotalWork:         totalWork,
		MempoolSize:       mempoolStats.Count,
		MempoolBytes:      mempoolStats.SizeBytes,
		MempoolGeneration: mempoolStats.Generation,
		Syncing:           d.syncMgr.IsSyncing(),
		IdentityAge:       d.node.IdentityAge().Round(time.Second).String(),
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

// templateTipChecked reports whether mining templates may be served yet. The
// API comes up before the node has heard from any peer, and a template built
// then may extend a tip the network has already passed: a block found on it is
// accepted locally, announced, and orphaned once the node syncs. The gate
// opens on the first comparison of the local tip with a peer's chain status,
// or blockTemplatePeerTipWait after sync starts, and then stays open; later
// sync lag is handled by IsSyncingFarBehind.
func (d *Daemon) templateTipChecked() bool {
	if d.tipGateOpen.Load() {
		return true
	}
	checked := d.peerTipChecked
	if checked == nil {
		checked = d.syncMgr.PeerTipChecked
	}
	open := checked()
	if !open {
		if started := d.syncStartedAt.Load(); started != nil {
			now := d.tipGateNow
			if now == nil {
				now = time.Now
			}
			open = now().Sub(*started) >= blockTemplatePeerTipWait
		}
	}
	if open {
		d.tipGateOpen.Store(true)
	}
	return open
}

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
	if err := d.validateSubmitBlockStaleLocked(block); err != nil {
		d.mu.Unlock()
		return err
	}

	prevBest := d.chain.BestHash()
	accepted, isMainChain, err := d.chain.ProcessBlock(block)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("failed to process block: %w", err)
	}
	if !accepted {
		d.mu.Unlock()
		return fmt.Errorf("block not accepted (duplicate or stale)")
	}

	if isMainChain {
		d.updateMempoolForAcceptedMainChain(block, prevBest)
		d.miner.NotifyNewBlock()
	}
	d.mu.Unlock()

	d.maybeSaveCheckpoints()

	// Broadcast to peers
	d.syncMgr.BroadcastBlock(marshalBlockJSON(block))

	// Notify wallet/UI subscribers only for blocks that became main-chain.
	// Side-chain blocks may be accepted for fork choice, but scanning them would
	// record outputs the wallet cannot spend on the canonical chain.
	if isMainChain {
		d.notifyBlock(block)
	}
	d.notifyMinedBlock(block)

	return nil
}

func (d *Daemon) validateSubmitBlockStaleLocked(block *Block) error {
	if block == nil {
		return fmt.Errorf("invalid block: nil block")
	}
	tipHeight := d.chain.Height()
	expectedHeight := tipHeight + 1
	if block.Header.Height <= tipHeight {
		return fmt.Errorf("%w: expected height %d, got %d", ErrStaleBlock, expectedHeight, block.Header.Height)
	}
	if block.Header.Height == expectedHeight && block.Header.PrevHash != d.chain.BestHash() {
		return fmt.Errorf("%w: does not build on current tip", ErrStaleBlock)
	}
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
	if d.node != nil {
		d.node.BroadcastTx(txData)
	}

	return nil
}

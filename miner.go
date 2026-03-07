package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Emission curve: smooth exponential decay
// Target supply: ~100M coins over 4 years
// Tail emission: 2.0 coins/block forever
const (
	InitialReward = 72_325_093_035 // 723.25 coins in smallest units
	TailEmission  = 200_000_000    // 2.0 coins
	MonthsToTail  = 48
	DecayRate     = 0.75 // per year
)

// BlocksPerMonth is derived from block interval (defined in block.go)
// ~8766 blocks/month at 5-minute intervals
const BlocksPerMonth = (30 * 24 * 60 * 60) / BlockIntervalSec

// MinerConfig holds mining configuration
type MinerConfig struct {
	// MinerSpendPub is the spend public key for coinbase rewards
	MinerSpendPub [32]byte
	// MinerViewPub is the view public key for coinbase rewards
	MinerViewPub [32]byte
	// Threads is the number of mining threads (0 = auto)
	Threads int
	// PeerCount returns the number of connected peers (nil = skip check)
	PeerCount func() int
}

// MinerStats holds mining statistics
type MinerStats struct {
	HashCount    uint64
	BlocksFound  uint64
	StartTime    time.Time
	LastHashTime time.Time
}

// Miner handles proof of work mining
type Miner struct {
	config   MinerConfig
	chain    *Chain
	mempool  *Mempool
	stats    MinerStats
	threads  atomic.Int32
	running  atomic.Bool
	cancel   context.CancelFunc
	newBlock chan struct{} // signals miner to restart on new chain tip
}

// NewMiner creates a new miner
func NewMiner(chain *Chain, mempool *Mempool, config MinerConfig) *Miner {
	threads := config.Threads
	if threads < 1 {
		threads = 1
	}

	m := &Miner{
		config:   config,
		chain:    chain,
		mempool:  mempool,
		newBlock: make(chan struct{}, 1),
		stats: MinerStats{
			StartTime: time.Now(),
		},
	}
	m.threads.Store(int32(threads))
	return m
}

// SetRewardKeys updates the mining reward destination keys.
// Call this after loading a wallet so mined blocks pay to the wallet.
func (m *Miner) SetRewardKeys(spendPub, viewPub [32]byte) {
	m.config.MinerSpendPub = spendPub
	m.config.MinerViewPub = viewPub
}

// NotifyNewBlock tells the miner a new block arrived so it should
// abandon the current stale solve and rebuild against the new tip.
func (m *Miner) NotifyNewBlock() {
	select {
	case m.newBlock <- struct{}{}:
	default: // already signalled, don't block
	}
}

// errNewBlock is returned by MineBlock when a new block arrived and
// the current solve should be abandoned in favour of a fresh template.
var errNewBlock = fmt.Errorf("new block received, restarting")

// MineBlock attempts to mine a single block.
// Returns the mined block or nil if cancelled.
func (m *Miner) MineBlock(ctx context.Context, mempool []*Transaction) (*Block, error) {
	// Create coinbase transaction
	nextHeight := m.chain.Height() + 1
	coinbase, err := CreateCoinbase(
		m.config.MinerSpendPub,
		m.config.MinerViewPub,
		GetBlockReward(nextHeight),
		nextHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create coinbase: %w", err)
	}

	// Build transaction list (coinbase first)
	txs := make([]*Transaction, 0, len(mempool)+1)
	txs = append(txs, coinbase.Tx)
	txs = append(txs, mempool...)

	// Create block template
	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     m.chain.Height() + 1,
			PrevHash:   m.chain.BestHash(),
			Timestamp:  time.Now().Unix(),
			Difficulty: m.chain.NextDifficulty(),
			Nonce:      0,
		},
		Transactions: txs,
	}

	// Compute merkle root
	merkleRoot, err := block.ComputeMerkleRoot()
	if err != nil {
		return nil, fmt.Errorf("failed to compute merkle root: %w", err)
	}
	block.Header.MerkleRoot = merkleRoot

	// Get target
	target := DifficultyToTarget(block.Header.Difficulty)

	// Serialize header for PoW (without nonce)
	headerBytes := block.Header.SerializeForPoW()

	// Number of mining threads
	// Default to 1 for Argon2id since each hash uses 2GB RAM
	// User can override with config.Threads
	numThreads := m.Threads()

	// Channel to receive winning result
	type mineResult struct {
		nonce     uint64
		timestamp int64
	}
	resultChan := make(chan mineResult, 1)
	mineCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	// Start mining threads
	for t := 0; t < numThreads; t++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()
			nonce := uint64(threadID)
			step := uint64(numThreads)
			localHeader := make([]byte, len(headerBytes))
			copy(localHeader, headerBytes)
			lastTimestamp := block.Header.Timestamp

			for {
				select {
				case <-mineCtx.Done():
					return
				default:
				}

				// Yield to scheduler periodically to keep CLI responsive
				if nonce%uint64(numThreads*10) == uint64(threadID) {
					runtime.Gosched()
				}

				// Compute Argon2id hash
				hash, err := PowHash(localHeader, nonce)
				if err != nil {
					nonce += step
					continue
				}

				atomic.AddUint64(&m.stats.HashCount, 1)

				// Check if we found a valid block
				if PowCheckTarget(hash, target) {
					select {
					case resultChan <- mineResult{nonce: nonce, timestamp: lastTimestamp}:
					default:
					}
					return
				}

				nonce += step

				// Update timestamp periodically
				if nonce%(step*10) == uint64(threadID) {
					newTime := time.Now().Unix()
					if newTime != lastTimestamp {
						lastTimestamp = newTime
						// Update local header with new timestamp (offset 76 in SerializeForPoW)
						binary.LittleEndian.PutUint64(localHeader[76:84], uint64(newTime))
					}
				}
			}
		}(t)
	}

	stopWorkers := func() {
		cancel()
		wg.Wait()
	}

	// Wait for result, new-block signal, or cancellation
	select {
	case <-ctx.Done():
		stopWorkers()
		return nil, ctx.Err()
	case <-m.newBlock:
		// Chain tip changed -- abandon this stale solve
		stopWorkers()
		return nil, errNewBlock
	case win := <-resultChan:
		stopWorkers()
		block.Header.Nonce = win.nonce
		block.Header.Timestamp = win.timestamp
		atomic.AddUint64(&m.stats.BlocksFound, 1)
		m.stats.LastHashTime = time.Now()
		return block, nil
	}
}

// Start begins mining in a background goroutine
func (m *Miner) Start(ctx context.Context, blockChan chan<- *Block) {
	if !MiningSupported {
		return
	}
	if m.running.Swap(true) {
		return // Already running
	}

	// Reset statistics so old data from a previous mining session is not included
	atomic.StoreUint64(&m.stats.HashCount, 0)
	atomic.StoreUint64(&m.stats.BlocksFound, 0)
	m.stats.StartTime = time.Now()
	m.stats.LastHashTime = time.Time{}

	// Create a cancellable context for this mining session
	mineCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	go func() {
		defer m.running.Store(false)
		defer cancel()

		for {
			select {
			case <-mineCtx.Done():
				return
			default:
			}

			// Wait for peers before mining (avoid divergent chains)
			if m.config.PeerCount != nil {
				for m.config.PeerCount() == 0 {
					select {
					case <-mineCtx.Done():
						return
					case <-time.After(5 * time.Second):
						fmt.Println("Waiting for peers before mining...")
					}
				}
			}

			// Drain any pending new-block signal before grabbing mempool
			select {
			case <-m.newBlock:
			default:
			}

			// Get transactions from mempool
			var txs []*Transaction
			if m.mempool != nil {
				txs = m.mempool.GetTransactionsForBlock(MaxBlockSize-1000, 1000) // Leave room for coinbase, max 1000 txs
			}

			block, err := m.MineBlock(mineCtx, txs)
			if err != nil {
				if mineCtx.Err() != nil {
					return // Context cancelled
				}
				if err == errNewBlock {
					// New tip arrived -- loop back and rebuild template
					continue
				}
				fmt.Printf("Mining error: %v\n", err)
				time.Sleep(time.Second)
				continue
			}

			// Send mined block
			select {
			case blockChan <- block:
			case <-mineCtx.Done():
				return
			}
		}
	}()
}

// Stop stops the miner
func (m *Miner) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.running.Store(false)
}

// IsRunning returns true if miner is running
func (m *Miner) IsRunning() bool {
	return m.running.Load()
}

// SetThreads updates the number of mining threads.
// If mining is active, the current block attempt is restarted so new
// thread count applies immediately.
func (m *Miner) SetThreads(n int) {
	if n < 1 {
		n = 1
	}
	maxThreads := runtime.NumCPU()
	maxThreads = max(maxThreads, 1)
	if n > maxThreads {
		n = maxThreads
	}
	prev := int(m.threads.Swap(int32(n)))
	if prev != n && m.IsRunning() {
		m.NotifyNewBlock()
	}
}

// Threads returns the current thread count
func (m *Miner) Threads() int {
	n := int(m.threads.Load())
	if n < 1 {
		return 1
	}
	return n
}

// Stats returns current mining statistics
func (m *Miner) Stats() MinerStats {
	return MinerStats{
		HashCount:    atomic.LoadUint64(&m.stats.HashCount),
		BlocksFound:  atomic.LoadUint64(&m.stats.BlocksFound),
		StartTime:    m.stats.StartTime,
		LastHashTime: m.stats.LastHashTime,
	}
}

// HashRate returns the current hash rate (hashes per second)
func (m *Miner) HashRate() float64 {
	stats := m.Stats()
	elapsed := time.Since(stats.StartTime).Seconds()
	if elapsed < 1 {
		return 0
	}
	return float64(stats.HashCount) / elapsed
}

// GetBlockReward returns the block reward for a given height
// Uses smooth exponential decay to tail emission
func GetBlockReward(height uint64) uint64 {
	month := height / BlocksPerMonth
	if month >= MonthsToTail {
		return TailEmission
	}

	years := float64(month) / 12.0
	decay := math.Exp(-DecayRate * years)
	reward := float64(InitialReward-TailEmission)*decay + float64(TailEmission)

	if reward < float64(TailEmission) {
		return TailEmission
	}
	return uint64(reward)
}

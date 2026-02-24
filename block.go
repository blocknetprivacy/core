package main

import (
	"container/list"
	"crypto/rand"
	"crypto/sha3"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

const (
	// BLOCKNET_CHAIN_CACHE_CAP caps the number of blocks/work entries kept in RAM.
	// Nodes can lower this to reduce memory or raise it to trade memory for speed.
	chainCacheCapEnv      = "BLOCKNET_CHAIN_CACHE_CAP"
	defaultChainCacheCap  = 512
	chainCacheCapMinSlack = 64
	chainCacheCapHardMax  = 100_000
)

// ErrOrphanBlock is returned when a block's parent is not found
var ErrOrphanBlock = errors.New("orphan block")

// ErrStaleBlock is returned when a block is valid in isolation but was built
// against an old tip (e.g. miner solved work after the chain advanced).
var ErrStaleBlock = errors.New("stale block")

func addCumulativeWork(parentWork, blockDifficulty uint64) (uint64, error) {
	if blockDifficulty > math.MaxUint64-parentWork {
		return 0, fmt.Errorf("cumulative work overflow: parent=%d difficulty=%d", parentWork, blockDifficulty)
	}
	return parentWork + blockDifficulty, nil
}

// ============================================================================
// Constants
// ============================================================================

const (
	// Block timing
	BlockInterval       = 5 * time.Minute // Target block time
	BlockIntervalSec    = 300             // 5 minutes in seconds
	SafeConfirmations   = 10              // ~50 minutes for "safe"
	CoinbaseMaturity    = 60              // ~5 hours before mined coins spendable
	MaxReorgDepth       = 100             // Hard finality (~8 hours)
	TimestampFuturLimit = 2 * time.Hour   // Max timestamp ahead of now

	// Block size
	MaxBlockSize = 1 << 20 // 1MB hard cap

	// LWMA Difficulty adjustment (Linear Weighted Moving Average)
	// Based on Zawy's LWMA used by TurtleCoin, Monero, etc.
	LWMAWindow       = 60                   // N blocks to look back
	MinDifficulty    = uint64(4)            // Floor difficulty (Argon2id 2GB is slow, ~15s/hash)
	LWMAMinSolvetime = 1                    // Minimum solvetime (prevent div by zero, timestamp attacks)
	LWMAMaxSolvetime = BlockIntervalSec * 6 // Max solvetime (6x target, prevents difficulty drops from stuck blocks)

	// Canonical block-header serialization sizes.
	blockHeaderSerializedSize        = 4 + 8 + 32 + 32 + 8 + 8 + 8
	blockHeaderSerializedSizeNoNonce = 4 + 8 + 32 + 32 + 8 + 8
)

// ============================================================================
// Block Header
// ============================================================================

// BlockHeader contains the immutable header of a block
type BlockHeader struct {
	Version    uint32   // Protocol version
	Height     uint64   // Block height (explicit)
	PrevHash   [32]byte // Hash of previous block
	MerkleRoot [32]byte // Root of transaction merkle tree
	Timestamp  int64    // Unix timestamp
	Difficulty uint64   // Target difficulty for this block
	Nonce      uint64   // PoW nonce
}

// Hash returns the SHA3-256 hash of the block header
func (h *BlockHeader) Hash() [32]byte {
	data := h.Serialize()
	return sha3.Sum256(data)
}

// Serialize converts header to canonical bytes for hashing.
func (h *BlockHeader) Serialize() []byte {
	return h.serializeFull()
}

// serializeFull serializes all header fields
func (h *BlockHeader) serializeFull() []byte {
	buf := make([]byte, blockHeaderSerializedSize)

	offset := 0
	binary.LittleEndian.PutUint32(buf[offset:], h.Version)
	offset += 4

	binary.LittleEndian.PutUint64(buf[offset:], h.Height)
	offset += 8

	copy(buf[offset:], h.PrevHash[:])
	offset += 32

	copy(buf[offset:], h.MerkleRoot[:])
	offset += 32

	binary.LittleEndian.PutUint64(buf[offset:], uint64(h.Timestamp))
	offset += 8

	binary.LittleEndian.PutUint64(buf[offset:], h.Difficulty)
	offset += 8

	binary.LittleEndian.PutUint64(buf[offset:], h.Nonce)
	offset += 8
	if offset != len(buf) {
		panic("block header serialization invariant violated")
	}

	return buf
}

// SerializeForPoW serializes header WITHOUT nonce for Argon2id input
// The nonce is passed separately to the PoW function
func (h *BlockHeader) SerializeForPoW() []byte {
	buf := make([]byte, blockHeaderSerializedSizeNoNonce)

	offset := 0
	binary.LittleEndian.PutUint32(buf[offset:], h.Version)
	offset += 4

	binary.LittleEndian.PutUint64(buf[offset:], h.Height)
	offset += 8

	copy(buf[offset:], h.PrevHash[:])
	offset += 32

	copy(buf[offset:], h.MerkleRoot[:])
	offset += 32

	binary.LittleEndian.PutUint64(buf[offset:], uint64(h.Timestamp))
	offset += 8

	binary.LittleEndian.PutUint64(buf[offset:], h.Difficulty)
	offset += 8
	if offset != len(buf) {
		panic("block header PoW serialization invariant violated")
	}

	return buf
}

// ============================================================================
// Block
// ============================================================================

// Block represents a complete block with header and transactions
type Block struct {
	Header       BlockHeader    `json:"header"`
	Transactions []*Transaction `json:"transactions"`
}

// Hash returns the block hash (header hash)
func (b *Block) Hash() [32]byte {
	return b.Header.Hash()
}

// Size returns the approximate serialized size of the block
func (b *Block) Size() int {
	size := blockHeaderSerializedSize // Canonical serialized header size
	for _, tx := range b.Transactions {
		size += tx.Size()
	}
	return size
}

// ComputeMerkleRoot computes the merkle root of transactions
func (b *Block) ComputeMerkleRoot() ([32]byte, error) {
	if len(b.Transactions) == 0 {
		return [32]byte{}, nil
	}

	// Get transaction hashes
	hashes := make([][32]byte, len(b.Transactions))
	for i, tx := range b.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			return [32]byte{}, fmt.Errorf("failed to hash tx %d: %w", i, err)
		}
		hashes[i] = txID
	}

	return computeMerkleRoot(hashes), nil
}

// computeMerkleRoot builds merkle tree and returns root
func computeMerkleRoot(hashes [][32]byte) [32]byte {
	if len(hashes) == 0 {
		return [32]byte{}
	}
	if len(hashes) == 1 {
		return hashes[0]
	}

	// Pad to even number by duplicating last hash
	if len(hashes)%2 == 1 {
		hashes = append(hashes, hashes[len(hashes)-1])
	}

	// Build next level
	nextLevel := make([][32]byte, len(hashes)/2)
	for i := 0; i < len(hashes); i += 2 {
		combined := make([]byte, 64)
		copy(combined[0:32], hashes[i][:])
		copy(combined[32:64], hashes[i+1][:])
		nextLevel[i/2] = sha3.Sum256(combined)
	}

	return computeMerkleRoot(nextLevel)
}

// ============================================================================
// Block Validation
// ============================================================================

// ValidateBlock validates a block against the chain state
func ValidateBlock(block *Block, chain *Chain) error {
	header := &block.Header

	// Version check
	if header.Version == 0 {
		return fmt.Errorf("invalid block version")
	}

	// Height check
	expectedHeight := chain.Height() + 1
	if header.Height != expectedHeight {
		// Treat "behind tip" as stale work so API can report it cleanly.
		if header.Height <= chain.Height() {
			return fmt.Errorf("%w: expected height %d, got %d", ErrStaleBlock, expectedHeight, header.Height)
		}
		return fmt.Errorf("invalid height: expected %d, got %d", expectedHeight, header.Height)
	}

	// Previous hash check
	if header.PrevHash != chain.BestHash() {
		// For mining submitblock, this almost always means the tip advanced.
		return fmt.Errorf("%w: does not build on current tip", ErrStaleBlock)
	}

	// Timestamp validation
	if err := validateTimestamp(header, chain); err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}

	// Difficulty check
	expectedDiff := chain.NextDifficulty()
	if header.Difficulty != expectedDiff {
		return fmt.Errorf("invalid difficulty: expected %d, got %d", expectedDiff, header.Difficulty)
	}

	// PoW check
	if !validatePoW(header) {
		return fmt.Errorf("invalid proof of work")
	}

	// Block size check
	if block.Size() > MaxBlockSize {
		return fmt.Errorf("block too large: %d > %d", block.Size(), MaxBlockSize)
	}

	// Must have at least one transaction (coinbase)
	if len(block.Transactions) == 0 {
		return fmt.Errorf("block has no transactions")
	}

	// First transaction must be coinbase
	if !block.Transactions[0].IsCoinbase() {
		return fmt.Errorf("first transaction is not coinbase")
	}

	// No other transaction can be coinbase
	for i := 1; i < len(block.Transactions); i++ {
		if block.Transactions[i].IsCoinbase() {
			return fmt.Errorf("multiple coinbase transactions")
		}
	}

	// Merkle root check
	merkleRoot, err := block.ComputeMerkleRoot()
	if err != nil {
		return fmt.Errorf("failed to compute merkle root: %w", err)
	}
	if merkleRoot != header.MerkleRoot {
		return fmt.Errorf("invalid merkle root")
	}

	// Validate all transactions
	for i, tx := range block.Transactions {
		if err := ValidateTransaction(tx, chain.IsKeyImageSpent, chain.IsCanonicalRingMember); err != nil {
			return fmt.Errorf("invalid transaction %d: %w", i, err)
		}
	}
	if err := validateCoinbaseConsensus(block.Transactions[0], header.Height); err != nil {
		return fmt.Errorf("invalid coinbase consensus commitment: %w", err)
	}

	return nil
}

// validateTimestamp checks block timestamp against rules
func validateTimestamp(header *BlockHeader, chain *Chain) error {
	// Must be greater than median of last 11 blocks
	medianTime := chain.MedianTimestamp(11)
	return validateTimestampWithMedian(header, medianTime)
}

func validateTimestampWithMedian(header *BlockHeader, medianTime int64) error {
	if header.Timestamp <= medianTime {
		return fmt.Errorf("timestamp %d <= median %d", header.Timestamp, medianTime)
	}

	// Must not be too far in future
	maxTime := time.Now().Add(TimestampFuturLimit).Unix()
	if header.Timestamp > maxTime {
		return fmt.Errorf("timestamp too far in future")
	}

	return nil
}

// validatePoW checks if block meets PoW difficulty using Argon2id
// This is computationally expensive (~2-3 seconds with 2GB memory)
func validatePoW(header *BlockHeader) bool {
	// Compute Argon2id hash
	headerBytes := header.SerializeForPoW()
	hash, err := PowHash(headerBytes, header.Nonce)
	if err != nil {
		return false
	}

	// Check against target
	target := DifficultyToTarget(header.Difficulty)
	return PowCheckTarget(hash, target)
}

// ValidateBlockP2P validates a block received from a peer over P2P.
// This validation is chain-aware and enforces core consensus rules, while
// still allowing side-chain/fork blocks that do not extend the current tip.
func ValidateBlockP2P(block *Block, chain *Chain) error {
	if chain == nil {
		return fmt.Errorf("chain is nil")
	}

	extendsTip := false
	if block != nil {
		extendsTip = block.Header.PrevHash == chain.BestHash()
	}
	skipPoW := false
	if block != nil {
		chain.mu.RLock()
		skipPoW = chain.shouldSkipPoWLocked(block.Header.Height, extendsTip)
		chain.mu.RUnlock()
	}

	return validateBlockWithContext(
		block,
		chain.BestHash(),
		chain.Height(),
		chain.GetBlock,
		chain.IsKeyImageSpent,
		chain.IsCanonicalRingMember,
		skipPoW,
	)
}

func validateBlockWithContext(
	block *Block,
	bestHash [32]byte,
	tipHeight uint64,
	getParent func([32]byte) *Block,
	isKeyImageSpent func([32]byte) bool,
	isCanonicalRingMember RingMemberChecker,
	skipPoW bool,
) error {
	header := &block.Header

	if header.Version == 0 {
		return fmt.Errorf("invalid block version")
	}
	if header.Height == 0 {
		return validateGenesisBlock(block)
	}

	// Parent/height consistency checks (for non-genesis blocks).
	parent := getParent(header.PrevHash)
	if parent == nil {
		return ErrOrphanBlock
	}
	if header.Height != parent.Header.Height+1 {
		return fmt.Errorf("invalid height linkage: parent=%d child=%d", parent.Header.Height, header.Height)
	}
	// Basic timestamp sanity for non-tip extensions.
	if header.Timestamp <= parent.Header.Timestamp {
		return fmt.Errorf("timestamp %d <= parent timestamp %d", header.Timestamp, parent.Header.Timestamp)
	}

	// For blocks extending the current tip, height must align with our tip.
	if header.PrevHash == bestHash {
		if header.Height != tipHeight+1 {
			return fmt.Errorf("invalid height: expected %d, got %d", tipHeight+1, header.Height)
		}
	}

	// Enforce parent-branch difficulty + median-time rules for all non-genesis blocks,
	// including non-tip side-chain/fork blocks.
	expectedDifficulty, err := expectedDifficultyFromParent(parent, getParent)
	if err != nil {
		return fmt.Errorf("failed to derive expected difficulty from parent context: %w", err)
	}
	if header.Difficulty != expectedDifficulty {
		return fmt.Errorf("invalid difficulty: expected %d, got %d", expectedDifficulty, header.Difficulty)
	}

	medianTime, err := medianTimestampFromParent(parent, getParent, 11)
	if err != nil {
		return fmt.Errorf("failed to derive median timestamp from parent context: %w", err)
	}
	if err := validateTimestampWithMedian(header, medianTime); err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}

	if header.Difficulty < MinDifficulty {
		return fmt.Errorf("difficulty %d below minimum %d", header.Difficulty, MinDifficulty)
	}

	// Always reject excessively-future timestamps.
	maxTime := time.Now().Add(TimestampFuturLimit).Unix()
	if header.Timestamp > maxTime {
		return fmt.Errorf("timestamp too far in future")
	}

	if !skipPoW {
		if !validatePoW(header) {
			return fmt.Errorf("invalid proof of work")
		}
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

	// Validate all transactions and enforce no duplicated key images within the block.
	seenKeyImages := make(map[[32]byte]struct{})
	for i, tx := range block.Transactions {
		if err := ValidateTransaction(tx, isKeyImageSpent, isCanonicalRingMember); err != nil {
			return fmt.Errorf("invalid transaction %d: %w", i, err)
		}
		if tx.IsCoinbase() {
			continue
		}
		for j, input := range tx.Inputs {
			if _, exists := seenKeyImages[input.KeyImage]; exists {
				return fmt.Errorf("invalid transaction %d input %d: duplicate key image in block", i, j)
			}
			seenKeyImages[input.KeyImage] = struct{}{}
		}
	}
	if err := validateCoinbaseConsensus(block.Transactions[0], header.Height); err != nil {
		return fmt.Errorf("invalid coinbase consensus commitment: %w", err)
	}

	merkleRoot, err := block.ComputeMerkleRoot()
	if err != nil {
		return fmt.Errorf("failed to compute merkle root: %w", err)
	}
	if merkleRoot != header.MerkleRoot {
		return fmt.Errorf("invalid merkle root")
	}

	return nil
}

func validateGenesisBlock(block *Block) error {
	if block == nil {
		return fmt.Errorf("nil block")
	}
	header := &block.Header

	if header.Height != 0 {
		return fmt.Errorf("invalid genesis height: expected 0, got %d", header.Height)
	}
	if header.PrevHash != GenesisPrevHash() {
		return fmt.Errorf("invalid genesis prev hash")
	}
	if header.Timestamp != GenesisTimestamp {
		return fmt.Errorf("invalid genesis timestamp: expected %d, got %d", GenesisTimestamp, header.Timestamp)
	}
	if header.Difficulty != MinDifficulty {
		return fmt.Errorf("invalid genesis difficulty: expected %d, got %d", MinDifficulty, header.Difficulty)
	}
	if header.Nonce != 0 {
		return fmt.Errorf("invalid genesis nonce: expected 0, got %d", header.Nonce)
	}
	if len(block.Transactions) != 0 {
		return fmt.Errorf("invalid genesis transactions: expected 0, got %d", len(block.Transactions))
	}
	if header.MerkleRoot != [32]byte{} {
		return fmt.Errorf("invalid genesis merkle root")
	}
	if block.Size() > MaxBlockSize {
		return fmt.Errorf("block too large: %d > %d", block.Size(), MaxBlockSize)
	}

	return nil
}

func validateCoinbaseConsensus(coinbase *Transaction, blockHeight uint64) error {
	if coinbase == nil {
		return fmt.Errorf("coinbase missing")
	}
	if len(coinbase.Outputs) != 1 {
		return fmt.Errorf("expected exactly one output, got %d", len(coinbase.Outputs))
	}

	expectedReward := GetBlockReward(blockHeight)
	expectedBlinding := deriveCoinbaseConsensusBlinding(coinbase.TxPublicKey, blockHeight, 0)
	expectedCommitment, err := CreatePedersenCommitmentWithBlinding(expectedReward, expectedBlinding)
	if err != nil {
		return fmt.Errorf("failed to derive expected commitment: %w", err)
	}

	output := coinbase.Outputs[0]
	if output.Commitment != expectedCommitment {
		return fmt.Errorf("output commitment does not match expected reward commitment")
	}

	expectedEncryptedAmount := EncryptAmount(expectedReward, expectedBlinding, 0)
	if output.EncryptedAmount != expectedEncryptedAmount {
		return fmt.Errorf("encrypted amount does not match expected reward commitment")
	}

	// Coinbase memo semantics are consensus-decryptable because the coinbase
	// shared secret is derived deterministically from public data (tx pubkey + height).
	// Enforce empty-envelope-only policy to prevent miners from using coinbase
	// memos as an on-chain metadata channel.
	if memo, ok := wallet.DecryptMemo(output.EncryptedMemo, expectedBlinding, 0); !ok || len(memo) != 0 {
		return fmt.Errorf("coinbase output 0: memo payload must be empty")
	}

	return nil
}

// ChainViolation describes a single integrity violation found during chain verification.
type ChainViolation struct {
	Height  uint64
	Message string
}

// VerifyChain walks the entire chain and checks every block's difficulty
// against what LWMA should have produced, plus timestamp rules. This is a
// fast arithmetic-only check (no Argon2id), so it finishes in seconds. It
// returns all violations found, or nil if the chain is clean.
func (c *Chain) VerifyChain() []ChainViolation {
	c.mu.RLock()
	defer c.mu.RUnlock()

	height := c.height
	if height == 0 {
		return nil
	}

	// Load all blocks we need into a flat slice so lookups are fast.
	blocks := make([]*Block, height+1)
	for h := uint64(0); h <= height; h++ {
		blocks[h] = c.getBlockByHeightLocked(h)
		if blocks[h] == nil {
			return []ChainViolation{{Height: h, Message: "block missing from storage"}}
		}
	}

	var violations []ChainViolation

	for h := uint64(1); h <= height; h++ {
		block := blocks[h]
		prev := blocks[h-1]

		// --- Hash linkage check ---
		prevHash := prev.Hash()
		if block.Header.PrevHash != prevHash {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("PrevHash mismatch: block points to %x, actual parent hash is %x", block.Header.PrevHash[:8], prevHash[:8]),
			})
		}

		// --- Difficulty check ---
		var expectedDiff uint64
		// Consensus derives a block's difficulty from its parent context:
		// the LWMA window ends at height (h-1), not at h.
		parentHeight := h - 1
		if parentHeight < uint64(LWMAWindow) {
			expectedDiff = MinDifficulty
		} else {
			expectedDiff = computeLWMA(blocks, parentHeight)
		}
		if block.Header.Difficulty != expectedDiff {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("difficulty mismatch: stored %d, expected %d", block.Header.Difficulty, expectedDiff),
			})
		}

		// --- Timestamp checks ---
		// Must be > median of last 11
		medianCount := 11
		if int(h) < medianCount {
			medianCount = int(h)
		}
		timestamps := make([]int64, medianCount)
		for i := 0; i < medianCount; i++ {
			timestamps[i] = blocks[h-uint64(medianCount)+uint64(i)].Header.Timestamp
		}
		for i := 0; i < len(timestamps); i++ {
			for j := i + 1; j < len(timestamps); j++ {
				if timestamps[i] > timestamps[j] {
					timestamps[i], timestamps[j] = timestamps[j], timestamps[i]
				}
			}
		}
		median := timestamps[len(timestamps)/2]
		if block.Header.Timestamp <= median {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("timestamp %d <= median %d", block.Header.Timestamp, median),
			})
		}

		// Suspiciously fast blocks (< 1 second apart)
		blockTime := block.Header.Timestamp - prev.Header.Timestamp
		if blockTime < 1 {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("block time %ds (prev timestamp %d, this %d)", blockTime, prev.Header.Timestamp, block.Header.Timestamp),
			})
		}
	}

	return violations
}

// VerifyChainWithCheckpoints verifies the chain starting from the first block
// after a trusted checkpoint height. This is an optimization for startup: it
// reduces the number of blocks we need to load and check while keeping the
// same arithmetic-only validation (difficulty + timestamp rules) for the tail.
//
// If no checkpoint matches the local chain, it returns (nil, 0) and callers
// should fall back to VerifyChain().
func (c *Chain) VerifyChainWithCheckpoints(checkpoints map[uint64][32]byte, checkpointHeights []uint64) (violations []ChainViolation, usedCheckpointHeight uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	height := c.height
	if height == 0 || len(checkpoints) == 0 {
		return nil, 0
	}

	// Find the highest checkpoint that matches our stored chain.
	// checkpointHeights is expected sorted ascending; scan backward.
	for i := len(checkpointHeights) - 1; i >= 0; i-- {
		h := checkpointHeights[i]
		if h == 0 || h > height {
			continue
		}
		want, ok := checkpoints[h]
		if !ok {
			continue
		}
		b := c.getBlockByHeightLocked(h)
		if b == nil {
			return []ChainViolation{{Height: h, Message: "checkpoint height missing from storage"}}, 0
		}
		if b.Hash() == want {
			usedCheckpointHeight = h
			break
		}
	}
	if usedCheckpointHeight == 0 {
		return nil, 0
	}

	startVerify := usedCheckpointHeight + 1
	if startVerify > height {
		return nil, usedCheckpointHeight
	}

	// We still need lookback history to compute LWMA and median timestamp rules.
	needBack := uint64(11) // median window
	if uint64(LWMAWindow+1) > needBack {
		needBack = uint64(LWMAWindow + 1)
	}

	startLoad := uint64(0)
	if startVerify > needBack {
		startLoad = startVerify - needBack
	}

	// Load the required block range into memory.
	blocks := make([]*Block, height-startLoad+1)
	for h := startLoad; h <= height; h++ {
		b := c.getBlockByHeightLocked(h)
		if b == nil {
			return []ChainViolation{{Height: h, Message: "block missing from storage"}}, usedCheckpointHeight
		}
		blocks[h-startLoad] = b
	}

	get := func(h uint64) *Block {
		if h < startLoad || h > height {
			return nil
		}
		return blocks[h-startLoad]
	}

	// Local LWMA calculator over the [startLoad..height] window.
	computeLWMAFrom := func(parentHeight uint64) (uint64, bool) {
		var weightedSolvetimeSum int64
		var difficultySum uint64
		weightSum := int64(LWMAWindow * (LWMAWindow + 1) / 2)

		for i := 1; i <= LWMAWindow; i++ {
			idx := parentHeight - uint64(LWMAWindow) + uint64(i)
			cur := get(idx)
			prev := get(idx - 1)
			if cur == nil || prev == nil {
				return 0, false
			}
			solvetime := cur.Header.Timestamp - prev.Header.Timestamp
			if solvetime < LWMAMinSolvetime {
				solvetime = LWMAMinSolvetime
			}
			if solvetime > LWMAMaxSolvetime {
				solvetime = LWMAMaxSolvetime
			}
			weightedSolvetimeSum += solvetime * int64(i)
			difficultySum += cur.Header.Difficulty
		}

		avgDifficulty := difficultySum / uint64(LWMAWindow)
		expectedWeightedSum := int64(BlockIntervalSec) * weightSum
		if weightedSolvetimeSum < 1 {
			weightedSolvetimeSum = 1
		}
		newDiff := avgDifficulty * uint64(expectedWeightedSum) / uint64(weightedSolvetimeSum)
		if newDiff < MinDifficulty {
			newDiff = MinDifficulty
		}
		return newDiff, true
	}

	for h := startVerify; h <= height; h++ {
		block := get(h)
		prev := get(h - 1)
		if block == nil || prev == nil {
			return []ChainViolation{{Height: h, Message: "block missing from storage"}}, usedCheckpointHeight
		}

		// --- Hash linkage check ---
		prevHash := prev.Hash()
		if block.Header.PrevHash != prevHash {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("PrevHash mismatch: block points to %x, actual parent hash is %x", block.Header.PrevHash[:8], prevHash[:8]),
			})
		}

		// --- Difficulty check ---
		var expectedDiff uint64
		parentHeight := h - 1
		if parentHeight < uint64(LWMAWindow) {
			expectedDiff = MinDifficulty
		} else {
			d, ok := computeLWMAFrom(parentHeight)
			if !ok {
				return []ChainViolation{{Height: h, Message: "insufficient history for LWMA check"}}, usedCheckpointHeight
			}
			expectedDiff = d
		}
		if block.Header.Difficulty != expectedDiff {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("difficulty mismatch: stored %d, expected %d", block.Header.Difficulty, expectedDiff),
			})
		}

		// --- Timestamp checks ---
		medianCount := 11
		if int(h) < medianCount {
			medianCount = int(h)
		}
		timestamps := make([]int64, medianCount)
		for i := 0; i < medianCount; i++ {
			b := get(h - uint64(medianCount) + uint64(i))
			if b == nil {
				return []ChainViolation{{Height: h, Message: "insufficient history for median timestamp check"}}, usedCheckpointHeight
			}
			timestamps[i] = b.Header.Timestamp
		}
		for i := 0; i < len(timestamps); i++ {
			for j := i + 1; j < len(timestamps); j++ {
				if timestamps[i] > timestamps[j] {
					timestamps[i], timestamps[j] = timestamps[j], timestamps[i]
				}
			}
		}
		median := timestamps[len(timestamps)/2]
		if block.Header.Timestamp <= median {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("timestamp %d <= median %d", block.Header.Timestamp, median),
			})
		}

		blockTime := block.Header.Timestamp - prev.Header.Timestamp
		if blockTime < 1 {
			violations = append(violations, ChainViolation{
				Height:  h,
				Message: fmt.Sprintf("block time %ds (prev timestamp %d, this %d)", blockTime, prev.Header.Timestamp, block.Header.Timestamp),
			})
		}
	}

	return violations, usedCheckpointHeight
}

// SetTrustedCheckpoints configures the chain to use checkpoints for fast sync.
// This is a trust-based optimization: blocks at checkpoint heights must match
// the provided hash, and PoW verification may be skipped for tip-extensions up
// to the last checkpoint height while the node is still below that height.
func (c *Chain) SetTrustedCheckpoints(checkpoints map[uint64][32]byte, maxHeight uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(checkpoints) == 0 || maxHeight == 0 {
		c.trustedCheckpoints = nil
		c.fastSyncUntilHeight = 0
		return
	}
	cp := make(map[uint64][32]byte, len(checkpoints))
	for h, v := range checkpoints {
		cp[h] = v
	}
	c.trustedCheckpoints = cp
	c.fastSyncUntilHeight = maxHeight
}

func (c *Chain) shouldSkipPoWLocked(height uint64, extendsTip bool) bool {
	if !extendsTip {
		return false
	}
	if c.trustedCheckpoints == nil || c.fastSyncUntilHeight == 0 {
		return false
	}
	// Only skip while we're still syncing up to the last checkpoint.
	if c.height >= c.fastSyncUntilHeight {
		return false
	}
	return height > 0 && height <= c.fastSyncUntilHeight
}

// computeLWMA replicates the LWMA difficulty calculation for a given height,
// using a pre-loaded block slice. height must be >= LWMAWindow.
func computeLWMA(blocks []*Block, height uint64) uint64 {
	var weightedSolvetimeSum int64
	var difficultySum uint64
	weightSum := int64(LWMAWindow * (LWMAWindow + 1) / 2)

	for i := 1; i <= LWMAWindow; i++ {
		idx := height - uint64(LWMAWindow) + uint64(i)
		solvetime := blocks[idx].Header.Timestamp - blocks[idx-1].Header.Timestamp
		if solvetime < LWMAMinSolvetime {
			solvetime = LWMAMinSolvetime
		}
		if solvetime > LWMAMaxSolvetime {
			solvetime = LWMAMaxSolvetime
		}
		weightedSolvetimeSum += solvetime * int64(i)
		difficultySum += blocks[idx].Header.Difficulty
	}

	avgDifficulty := difficultySum / uint64(LWMAWindow)
	expectedWeightedSum := int64(BlockIntervalSec) * weightSum
	if weightedSolvetimeSum < 1 {
		weightedSolvetimeSum = 1
	}
	newDiff := avgDifficulty * uint64(expectedWeightedSum) / uint64(weightedSolvetimeSum)
	if newDiff < MinDifficulty {
		newDiff = MinDifficulty
	}
	return newDiff
}

// ============================================================================
// Chain State
// ============================================================================

// Chain represents the blockchain state
type Chain struct {
	mu sync.RWMutex

	// Persistent storage (bbolt)
	storage *Storage

	// In-memory caches for performance
	blocks map[[32]byte]*Block // hash -> block (recent blocks cache)
	workAt map[[32]byte]uint64 // hash -> cumulative work at this block

	// Cache eviction (LRU). This is strictly a performance/memory control and
	// must not affect consensus behaviour.
	cacheCap   int
	cacheLRU   *list.List                 // front=MRU, back=LRU; values are [32]byte hashes
	cacheIndex map[[32]byte]*list.Element // hash -> element in cacheLRU

	// Main chain only
	byHeight   map[uint64][32]byte // height -> hash (main chain only)
	timestamps []int64             // recent timestamps for median calc

	// Chain state
	bestHash  [32]byte
	height    uint64
	totalWork uint64

	// Key image set (for double-spend checking)
	keyImages map[[32]byte]uint64 // key_image -> height spent

	// Indexed canonical ring-member membership for main-chain outputs.
	// Key format: pubkey(32) || commitment(32).
	canonicalRingIndex      map[[64]byte]struct{}
	canonicalRingIndexDirty bool
	canonicalRingIndexTip   [32]byte
	canonicalRingIndexReady bool

	// Trusted checkpoints are an optional performance optimization for initial
	// sync: when enabled, we can skip expensive PoW verification for historical
	// blocks up to the last checkpoint height, while still pinning consensus by
	// requiring checkpoint-height hashes to match.
	trustedCheckpoints  map[uint64][32]byte
	fastSyncUntilHeight uint64
}

func chainProtectedBack() uint64 {
	// Keep enough history to safely serve reorg/finality + LWMA difficulty windows.
	return max(uint64(MaxReorgDepth), uint64(LWMAWindow+10))
}

func chainCacheCapFromEnv() int {
	cap := defaultChainCacheCap
	if v := os.Getenv(chainCacheCapEnv); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			cap = parsed
		}
	}
	// Clamp to a reasonable hard max to avoid accidental runaway allocations.
	if cap > chainCacheCapHardMax {
		cap = chainCacheCapHardMax
	}
	minCap := int(chainProtectedBack()) + chainCacheCapMinSlack
	if cap < minCap {
		cap = minCap
	}
	return cap
}

func (c *Chain) cacheTouchLocked(hash [32]byte) {
	if c.cacheLRU == nil {
		return
	}
	if elem, ok := c.cacheIndex[hash]; ok {
		c.cacheLRU.MoveToFront(elem)
		return
	}
	c.cacheIndex[hash] = c.cacheLRU.PushFront(hash)
}

func (c *Chain) cacheForgetLocked(hash [32]byte) {
	if elem, ok := c.cacheIndex[hash]; ok {
		c.cacheLRU.Remove(elem)
		delete(c.cacheIndex, hash)
	}
	delete(c.blocks, hash)
	delete(c.workAt, hash)
}

func (c *Chain) cachePinnedLocked(hash [32]byte) bool {
	b := c.blocks[hash]
	if b == nil {
		return false
	}
	h := b.Header.Height
	mainHash, ok := c.byHeight[h]
	if !ok || mainHash != hash {
		return false
	}
	protectedBack := chainProtectedBack()
	if c.height <= protectedBack {
		return true
	}
	return h >= c.height-protectedBack
}

func (c *Chain) cacheTrimLocked() {
	if c.cacheCap <= 0 || c.cacheLRU == nil {
		return
	}

	// Trim based on the LRU/index size as the canonical bound.
	//
	// The cacheCap is defined as "caps the number of blocks/work entries kept in RAM".
	// Invariant we want: cacheIndex/cacheLRU must never grow without bound, even if
	// some call path touches hashes that aren't currently present in c.blocks.
	for len(c.cacheIndex) > c.cacheCap || len(c.blocks) > c.cacheCap {
		back := c.cacheLRU.Back()
		if back == nil {
			return
		}
		hash := back.Value.([32]byte)
		if c.cachePinnedLocked(hash) {
			// Pinned blocks must not be evicted; move it to the front so we
			// make progress. This relies on cacheCap always being > pinned set size.
			c.cacheLRU.MoveToFront(back)
			continue
		}
		c.cacheForgetLocked(hash)
	}
}

func (c *Chain) cumulativeWorkAtLocked(hash [32]byte) (uint64, error) {
	if work, ok := c.workAt[hash]; ok {
		return work, nil
	}
	block := c.getBlockByHashLocked(hash)
	if block == nil {
		return 0, fmt.Errorf("block not found for workAt: %x", hash[:8])
	}

	var parentWork uint64
	if block.Header.Height > 0 {
		w, err := c.cumulativeWorkAtLocked(block.Header.PrevHash)
		if err != nil {
			return 0, err
		}
		parentWork = w
	}
	work, err := addCumulativeWork(parentWork, block.Header.Difficulty)
	if err != nil {
		return 0, err
	}
	c.workAt[hash] = work
	c.cacheTouchLocked(hash)
	c.cacheTrimLocked()
	return work, nil
}

// NewChain creates a new chain, loading state from storage
func NewChain(dataDir string) (*Chain, error) {
	storage, err := NewStorage(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open storage: %w", err)
	}

	cacheCap := chainCacheCapFromEnv()
	c := &Chain{
		storage:                 storage,
		blocks:                  make(map[[32]byte]*Block),
		workAt:                  make(map[[32]byte]uint64),
		cacheCap:                cacheCap,
		cacheLRU:                list.New(),
		cacheIndex:              make(map[[32]byte]*list.Element, cacheCap),
		byHeight:                make(map[uint64][32]byte),
		timestamps:              make([]int64, 0, LWMAWindow+1),
		keyImages:               make(map[[32]byte]uint64),
		canonicalRingIndex:      make(map[[64]byte]struct{}),
		canonicalRingIndexDirty: true,
		canonicalRingIndexReady: false,
	}

	// Load chain state from storage
	if err := c.loadFromStorage(); err != nil {
		if closeErr := storage.Close(); closeErr != nil {
			return nil, fmt.Errorf("failed to load chain state: %w (additionally failed to close storage: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("failed to load chain state: %w", err)
	}

	return c, nil
}

// loadFromStorage loads chain state from disk
func (c *Chain) loadFromStorage() error {
	tipHash, tipHeight, tipWork, found := c.storage.GetTip()
	if !found {
		// Fresh chain - no state to load
		return nil
	}

	c.bestHash = tipHash
	c.height = tipHeight
	c.totalWork = tipWork

	// Load recent blocks for LWMA calculation and height index
	startHeight := uint64(0)
	// Keep enough main-chain history in memory to support reorg/finality
	// paths that can legitimately touch the last `MaxReorgDepth` blocks.
	// This also covers the LWMA window (+ small buffer) used for difficulty.
	preloadBack := max(uint64(LWMAWindow+10), uint64(MaxReorgDepth))
	if tipHeight > preloadBack {
		startHeight = tipHeight - preloadBack
	}

	for h := startHeight; h <= tipHeight; h++ {
		hash, found := c.storage.GetBlockHashByHeight(h)
		if !found {
			return fmt.Errorf("block at height %d not found", h)
		}

		block, err := c.storage.GetBlock(hash)
		if err != nil {
			return fmt.Errorf("failed to load block %d: %w", h, err)
		}
		if block == nil {
			return fmt.Errorf("block %x not found", hash[:8])
		}

		c.blocks[hash] = block
		c.cacheTouchLocked(hash)
		c.byHeight[h] = hash

		// Calculate work
		var parentWork uint64
		if h > 0 {
			parentWork = c.workAt[block.Header.PrevHash]
		}
		blockWork, err := addCumulativeWork(parentWork, block.Header.Difficulty)
		if err != nil {
			return fmt.Errorf("failed to compute cumulative work at height %d: %w", h, err)
		}
		c.workAt[hash] = blockWork

		// Track timestamps for LWMA
		c.timestamps = append(c.timestamps, block.Header.Timestamp)
		if len(c.timestamps) > LWMAWindow+1 {
			c.timestamps = c.timestamps[1:]
		}

		// Load key images from transactions
		for _, tx := range block.Transactions {
			if !tx.IsCoinbase() {
				for _, input := range tx.Inputs {
					c.keyImages[input.KeyImage] = h
				}
			}
		}
	}
	c.cacheTrimLocked()

	// Fix cumulative work: the loop above computed workAt relative to 0
	// because the parent of startHeight wasn't loaded. Offset every entry
	// so workAt[tipHash] == totalWork (the real value from storage).
	computedTipWork := c.workAt[c.bestHash]
	if c.totalWork < computedTipWork {
		return fmt.Errorf("invalid stored cumulative work: stored tip work %d below computed tip work %d", c.totalWork, computedTipWork)
	}
	if offset := c.totalWork - computedTipWork; offset > 0 {
		for h := range c.workAt {
			adjustedWork, err := addCumulativeWork(c.workAt[h], offset)
			if err != nil {
				return fmt.Errorf("failed to offset cumulative work for loaded chain state: %w", err)
			}
			c.workAt[h] = adjustedWork
		}
	}

	c.canonicalRingIndexDirty = true

	return nil
}

// Close closes the chain storage
func (c *Chain) Close() error {
	if c.storage != nil {
		return c.storage.Close()
	}
	return nil
}

// Storage returns the underlying storage (for direct access if needed)
func (c *Chain) Storage() *Storage {
	return c.storage
}

// Height returns current chain height
func (c *Chain) Height() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.height
}

// HasGenesis returns true if the chain has at least the genesis block
func (c *Chain) HasGenesis() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if we have a tip stored
	_, _, _, found := c.storage.GetTip()
	return found
}

// BestHash returns the hash of the best block
func (c *Chain) BestHash() [32]byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bestHash
}

// GetBlock retrieves a block by hash
func (c *Chain) GetBlock(hash [32]byte) *Block {
	c.mu.RLock()
	block, ok := c.blocks[hash]
	c.mu.RUnlock()
	if ok {
		// Best-effort LRU touch without changing read-lock semantics.
		c.mu.Lock()
		if c.blocks[hash] != nil {
			c.cacheTouchLocked(hash)
		}
		c.mu.Unlock()
		return block
	}

	// Fall back to storage without holding chain lock.
	block, _ = c.storage.GetBlock(hash)
	if block != nil {
		c.mu.Lock()
		// Re-check in case another goroutine already cached it.
		if cached, exists := c.blocks[hash]; exists {
			c.cacheTouchLocked(hash)
			c.mu.Unlock()
			return cached
		}
		c.blocks[hash] = block
		c.cacheTouchLocked(hash)
		c.cacheTrimLocked()
		c.mu.Unlock()
	}
	return block
}

// GetBlockByHeight retrieves a block by height
func (c *Chain) GetBlockByHeight(height uint64) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.getBlockByHeightLocked(height)
}

// GetBlocksByHeightRange returns main-chain blocks in [startHeight, endHeight],
// stopping early if any height is missing.
//
// This holds a single read lock for the duration so callers can snapshot blocks
// without paying RWMutex writer-preference overhead per height.
func (c *Chain) GetBlocksByHeightRange(startHeight, endHeight uint64) []*Block {
	if startHeight > endHeight {
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Pre-size to the full range; caller may get fewer if we stop early.
	blocks := make([]*Block, 0, int(endHeight-startHeight+1))
	for h := startHeight; h <= endHeight; h++ {
		b := c.getBlockByHeightLocked(h)
		if b == nil {
			break
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// getBlockByHeightLocked is the lock-safe inner implementation.
// Caller must hold at least c.mu.RLock. It never mutates caches.
func (c *Chain) getBlockByHeightLocked(height uint64) *Block {
	// Check memory cache first
	if hash, ok := c.byHeight[height]; ok {
		if block, ok := c.blocks[hash]; ok {
			return block
		}
	}

	// Fall back to storage
	hash, found := c.storage.GetBlockHashByHeight(height)
	if !found {
		return nil
	}
	block, _ := c.storage.GetBlock(hash)
	return block
}

// MedianTimestamp returns median of last n block timestamps
func (c *Chain) MedianTimestamp(n int) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.medianTimestampLocked(n)
}

// NextDifficulty calculates the difficulty for the next block
// NextDifficulty calculates difficulty using LWMA (Linear Weighted Moving Average)
// Based on Zawy's algorithm used by TurtleCoin, Monero, and many other coins.
//
// LWMA gives more weight to recent blocks, making it responsive to hashrate changes
// while being resistant to timestamp manipulation and oscillation attacks.
func (c *Chain) NextDifficulty() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nextDifficultyLocked()
}

// addGenesisBlock adds the genesis block to an empty chain.
// For all non-genesis blocks, use ProcessBlock.
func (c *Chain) addGenesisBlock(block *Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if block == nil {
		return fmt.Errorf("nil block")
	}
	if block.Header.Height != 0 {
		return fmt.Errorf("addGenesisBlock only accepts genesis blocks (height 0), got %d", block.Header.Height)
	}
	if _, _, _, found := c.storage.GetTip(); found {
		return fmt.Errorf("genesis already exists; refusing unvalidated write path")
	}
	if err := c.validateBlockForProcessLocked(block); err != nil {
		return fmt.Errorf("invalid genesis block: %w", err)
	}

	return c.addBlockInternal(block)
}

// addBlockInternal adds block to main chain (caller holds lock)
func (c *Chain) addBlockInternal(block *Block) error {
	if block == nil {
		return fmt.Errorf("nil block")
	}
	if block.Header.Height != 0 {
		return fmt.Errorf("refusing unvalidated non-genesis block at height %d; use ProcessBlock", block.Header.Height)
	}
	if _, _, _, found := c.storage.GetTip(); found {
		return fmt.Errorf("refusing unvalidated add on non-empty chain; use ProcessBlock")
	}

	hash := block.Hash()

	// Calculate cumulative work
	var parentWork uint64
	if block.Header.Height > 0 {
		parentWork = c.workAt[block.Header.PrevHash]
	}
	blockWork, err := addCumulativeWork(parentWork, block.Header.Difficulty)
	if err != nil {
		return fmt.Errorf("failed to compute cumulative work: %w", err)
	}

	// Collect outputs and key images from transactions
	var newOutputs []*UTXO
	var spentKeyImages [][32]byte

	for _, tx := range block.Transactions {
		txid, _ := tx.TxID()

		// Collect outputs
		for idx, out := range tx.Outputs {
			newOutputs = append(newOutputs, &UTXO{
				TxID:        txid,
				OutputIndex: uint32(idx),
				Output:      out,
				BlockHeight: block.Header.Height,
			})
		}

		// Collect key images (except coinbase)
		if !tx.IsCoinbase() {
			for _, input := range tx.Inputs {
				spentKeyImages = append(spentKeyImages, input.KeyImage)
				c.keyImages[input.KeyImage] = block.Header.Height
			}
		}
	}

	// Commit to storage atomically
	commit := &BlockCommit{
		Block:        block,
		Height:       block.Header.Height,
		Hash:         hash,
		Work:         blockWork,
		IsMainTip:    true,
		NewOutputs:   newOutputs,
		SpentKeyImgs: spentKeyImages,
	}

	if err := c.storage.CommitBlock(commit); err != nil {
		return fmt.Errorf("failed to persist block: %w", err)
	}

	// Update in-memory state
	c.blocks[hash] = block
	c.cacheTouchLocked(hash)
	c.workAt[hash] = blockWork
	c.byHeight[block.Header.Height] = hash
	c.bestHash = hash
	c.height = block.Header.Height
	c.totalWork = blockWork
	c.canonicalRingIndexDirty = true
	c.cacheTrimLocked()

	// Track timestamp for LWMA
	c.timestamps = append(c.timestamps, block.Header.Timestamp)
	if len(c.timestamps) > LWMAWindow+1 {
		c.timestamps = c.timestamps[1:]
	}

	return nil
}

// ProcessBlock validates and adds a block, handling fork choice
// Returns true if block was accepted (even if not on main chain)
func (c *Chain) ProcessBlock(block *Block) (accepted bool, isMainChain bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	hash := block.Hash()

	// Already have this block? Check memory and storage
	if _, exists := c.blocks[hash]; exists {
		return false, false, nil
	}
	if c.storage.HasBlock(hash) {
		return false, false, nil
	}

	if err := c.validateBlockForProcessLocked(block); err != nil {
		return false, false, err
	}

	// Calculate work at this block
	var parentWork uint64
	if block.Header.Height > 0 {
		var err error
		parentWork, err = c.cumulativeWorkAtLocked(block.Header.PrevHash)
		if err != nil {
			return false, false, fmt.Errorf("failed to compute parent work: %w", err)
		}
	}
	blockWork, err := addCumulativeWork(parentWork, block.Header.Difficulty)
	if err != nil {
		return false, false, fmt.Errorf("failed to compute cumulative work: %w", err)
	}

	// Store block in memory (always, even if not main chain)
	c.blocks[hash] = block
	c.cacheTouchLocked(hash)
	c.workAt[hash] = blockWork
	c.cacheTrimLocked()

	// Does this create a heavier chain?
	if blockWork > c.totalWork {
		if err := c.reorganizeTo(hash); err != nil {
			// Reorg failed - remove from memory
			c.cacheForgetLocked(hash)
			return false, false, fmt.Errorf("reorg failed: %w", err)
		}
		return true, true, nil
	}

	// Block accepted but not on main chain (fork) - still save to storage
	if err := c.storage.SaveBlock(block); err != nil {
		return false, false, fmt.Errorf("failed to save fork block: %w", err)
	}

	return true, false, nil
}

func (c *Chain) validateBlockForProcessLocked(block *Block) error {
	isSpent := c.isKeyImageSpentLocked
	if block != nil && block.Header.Height > 0 {
		branchAwareSpent, err := c.branchAwareSpentCheckerLocked(block.Header.PrevHash)
		if err != nil {
			return err
		}
		isSpent = branchAwareSpent
	}

	// If checkpoints are enabled for fast sync, enforce checkpoint pinning.
	// This ensures skipping PoW cannot be exploited to feed an alternate history.
	if block != nil && c.trustedCheckpoints != nil && c.fastSyncUntilHeight > 0 && c.height < c.fastSyncUntilHeight {
		if block.Header.Height <= c.fastSyncUntilHeight {
			if want, ok := c.trustedCheckpoints[block.Header.Height]; ok {
				have := block.Hash()
				if have != want {
					return fmt.Errorf("checkpoint mismatch at height %d: have %x want %x", block.Header.Height, have[:8], want[:8])
				}
			}
		}
	}

	extendsTip := block != nil && block.Header.PrevHash == c.bestHash
	skipPoW := false
	if block != nil {
		skipPoW = c.shouldSkipPoWLocked(block.Header.Height, extendsTip)
	}

	return validateBlockWithContext(
		block,
		c.bestHash,
		c.height,
		c.getBlockByHashLocked,
		isSpent,
		c.isCanonicalRingMemberLocked,
		skipPoW,
	)
}

// branchAwareSpentCheckerLocked returns a spent-check closure that includes
// key images used on the candidate block's parent branch, even when those
// ancestors are currently off-main-chain.
func (c *Chain) branchAwareSpentCheckerLocked(parentHash [32]byte) (KeyImageChecker, error) {
	branchSpent := make(map[[32]byte]struct{})
	currentHash := parentHash

	for {
		parent := c.getBlockByHashLocked(currentHash)
		if parent == nil {
			return nil, ErrOrphanBlock
		}

		mainHashAtHeight, onMainHeight := c.byHeight[parent.Header.Height]
		if onMainHeight && mainHashAtHeight == currentHash {
			break
		}

		for _, tx := range parent.Transactions {
			if tx.IsCoinbase() {
				continue
			}
			for _, input := range tx.Inputs {
				branchSpent[input.KeyImage] = struct{}{}
			}
		}

		if parent.Header.Height == 0 {
			break
		}
		currentHash = parent.Header.PrevHash
	}

	return func(keyImage [32]byte) bool {
		if _, exists := branchSpent[keyImage]; exists {
			return true
		}
		return c.isKeyImageSpentLocked(keyImage)
	}, nil
}

func expectedDifficultyFromParent(parent *Block, getParent func([32]byte) *Block) (uint64, error) {
	if parent == nil {
		return 0, fmt.Errorf("nil parent")
	}

	// Child height <= LWMAWindow => minimum difficulty epoch.
	if parent.Header.Height < uint64(LWMAWindow) {
		return MinDifficulty, nil
	}

	// Build the same LWMA window shape as nextDifficultyLocked:
	// blocks[0..LWMAWindow] are consecutive heights ending at parent.
	blocks := make([]*Block, LWMAWindow+1)
	current := parent
	for i := LWMAWindow; ; i-- {
		blocks[i] = current
		if i == 0 {
			break
		}
		if current.Header.Height == 0 {
			return 0, fmt.Errorf("insufficient ancestors for LWMA window")
		}
		current = getParent(current.Header.PrevHash)
		if current == nil {
			return 0, fmt.Errorf("missing ancestor at height %d", blocks[i].Header.Height-1)
		}
	}

	var weightedSolvetimeSum int64
	var difficultySum uint64
	weightSum := int64(LWMAWindow * (LWMAWindow + 1) / 2)

	for i := 1; i <= LWMAWindow; i++ {
		solvetime := blocks[i].Header.Timestamp - blocks[i-1].Header.Timestamp
		if solvetime < LWMAMinSolvetime {
			solvetime = LWMAMinSolvetime
		}
		if solvetime > LWMAMaxSolvetime {
			solvetime = LWMAMaxSolvetime
		}
		weightedSolvetimeSum += solvetime * int64(i)
		difficultySum += blocks[i].Header.Difficulty
	}

	avgDifficulty := difficultySum / uint64(LWMAWindow)
	expectedWeightedSum := int64(BlockIntervalSec) * weightSum
	if weightedSolvetimeSum < 1 {
		weightedSolvetimeSum = 1
	}

	newDiff := avgDifficulty * uint64(expectedWeightedSum) / uint64(weightedSolvetimeSum)
	if newDiff < MinDifficulty {
		newDiff = MinDifficulty
	}

	return newDiff, nil
}

func medianTimestampFromParent(parent *Block, getParent func([32]byte) *Block, n int) (int64, error) {
	if parent == nil {
		return 0, fmt.Errorf("nil parent")
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid median window: %d", n)
	}

	timestamps := make([]int64, 0, n)
	current := parent
	for i := 0; i < n && current != nil; i++ {
		timestamps = append(timestamps, current.Header.Timestamp)
		if current.Header.Height == 0 {
			break
		}
		current = getParent(current.Header.PrevHash)
	}

	if len(timestamps) == 0 {
		return 0, fmt.Errorf("no timestamps available")
	}

	// Small bounded list; keep local sort to avoid extra imports.
	for i := 0; i < len(timestamps); i++ {
		for j := i + 1; j < len(timestamps); j++ {
			if timestamps[i] > timestamps[j] {
				timestamps[i], timestamps[j] = timestamps[j], timestamps[i]
			}
		}
	}

	return timestamps[len(timestamps)/2], nil
}

func (c *Chain) getBlockByHashLocked(hash [32]byte) *Block {
	if block, ok := c.blocks[hash]; ok {
		c.cacheTouchLocked(hash)
		return block
	}
	block, _ := c.storage.GetBlock(hash)
	if block != nil {
		c.blocks[hash] = block
		c.cacheTouchLocked(hash)
		c.cacheTrimLocked()
	}
	return block
}

func (c *Chain) isKeyImageSpentLocked(keyImage [32]byte) bool {
	if _, exists := c.keyImages[keyImage]; exists {
		return true
	}
	spent, _ := c.storage.IsKeyImageSpent(keyImage)
	return spent
}

func (c *Chain) isCanonicalRingMemberLocked(pubKey, commitment [32]byte) bool {
	if err := c.ensureCanonicalRingIndexLocked(); err != nil {
		return false
	}
	_, ok := c.canonicalRingIndex[canonicalRingIndexKey(pubKey, commitment)]
	return ok
}

func canonicalRingIndexKey(pubKey, commitment [32]byte) [64]byte {
	var key [64]byte
	copy(key[:32], pubKey[:])
	copy(key[32:], commitment[:])
	return key
}

func (c *Chain) ensureCanonicalRingIndexLocked() error {
	tipHash, tipHeight, _, found := c.storage.GetTip()
	if !found {
		c.canonicalRingIndex = make(map[[64]byte]struct{})
		c.canonicalRingIndexTip = [32]byte{}
		c.canonicalRingIndexReady = true
		c.canonicalRingIndexDirty = false
		return nil
	}

	if !c.canonicalRingIndexDirty && c.canonicalRingIndexReady && c.canonicalRingIndexTip == tipHash {
		return nil
	}

	index := make(map[[64]byte]struct{})
	for h := uint64(0); h <= tipHeight; h++ {
		hash, hasHeight := c.storage.GetBlockHashByHeight(h)
		if !hasHeight {
			return fmt.Errorf("canonical ring index missing block hash at height %d", h)
		}

		block := c.getBlockByHashLocked(hash)
		if block == nil {
			return fmt.Errorf("canonical ring index missing block data at height %d", h)
		}

		for _, tx := range block.Transactions {
			for _, out := range tx.Outputs {
				index[canonicalRingIndexKey(out.PublicKey, out.Commitment)] = struct{}{}
			}
		}
	}

	c.canonicalRingIndex = index
	c.canonicalRingIndexTip = tipHash
	c.canonicalRingIndexReady = true
	c.canonicalRingIndexDirty = false
	return nil
}

func (c *Chain) medianTimestampLocked(n int) int64 {
	if len(c.timestamps) == 0 {
		return 0
	}

	count := n
	if count > len(c.timestamps) {
		count = len(c.timestamps)
	}

	recent := make([]int64, count)
	copy(recent, c.timestamps[len(c.timestamps)-count:])
	for i := 0; i < len(recent); i++ {
		for j := i + 1; j < len(recent); j++ {
			if recent[i] > recent[j] {
				recent[i], recent[j] = recent[j], recent[i]
			}
		}
	}

	return recent[len(recent)/2]
}

func (c *Chain) nextDifficultyLocked() uint64 {
	// Not enough blocks for LWMA - use minimum difficulty
	if c.height < uint64(LWMAWindow) {
		return MinDifficulty
	}

	// Collect the last LWMAWindow+1 blocks (need N+1 to get N solve times)
	blocks := make([]*Block, LWMAWindow+1)
	for i := 0; i <= LWMAWindow; i++ {
		height := c.height - uint64(LWMAWindow) + uint64(i)
		hash := c.byHeight[height]
		blocks[i] = c.blocks[hash]
	}

	// Calculate weighted sum of solve times and sum of difficulties
	var weightedSolvetimeSum int64
	var difficultySum uint64
	weightSum := int64(LWMAWindow * (LWMAWindow + 1) / 2) // Sum of 1..N

	for i := 1; i <= LWMAWindow; i++ {
		solvetime := blocks[i].Header.Timestamp - blocks[i-1].Header.Timestamp
		if solvetime < LWMAMinSolvetime {
			solvetime = LWMAMinSolvetime
		}
		if solvetime > LWMAMaxSolvetime {
			solvetime = LWMAMaxSolvetime
		}
		weight := int64(i)
		weightedSolvetimeSum += solvetime * weight
		difficultySum += blocks[i].Header.Difficulty
	}

	avgDifficulty := difficultySum / uint64(LWMAWindow)
	expectedWeightedSum := int64(BlockIntervalSec) * weightSum
	if weightedSolvetimeSum < 1 {
		weightedSolvetimeSum = 1
	}

	newDiff := avgDifficulty * uint64(expectedWeightedSum) / uint64(weightedSolvetimeSum)
	if newDiff < MinDifficulty {
		newDiff = MinDifficulty
	}
	return newDiff
}

// reorganizeTo switches the main chain to end at newTip
func (c *Chain) reorganizeTo(newTip [32]byte) error {
	newBlock := c.blocks[newTip]
	if newBlock == nil {
		if loaded, _ := c.storage.GetBlock(newTip); loaded != nil {
			c.blocks[newTip] = loaded
			c.cacheTouchLocked(newTip)
			c.cacheTrimLocked()
			newBlock = loaded
		} else {
			return fmt.Errorf("reorganizeTo: new tip block not found")
		}
	}

	// Fast path: simple chain extension (parent is current tip).
	// Avoids the O(height) ancestor walks that dominate sync time.
	if newBlock.Header.PrevHash == c.bestHash {
		newTipWork, err := c.cumulativeWorkAtLocked(newTip)
		if err != nil {
			return fmt.Errorf("failed to compute new tip work: %w", err)
		}
		reorgCommit := &ReorgCommit{
			Connect:   []*Block{newBlock},
			NewTip:    newTip,
			NewHeight: newBlock.Header.Height,
			NewWork:   newTipWork,
		}
		if err := c.storage.CommitReorg(reorgCommit); err != nil {
			return fmt.Errorf("failed to persist chain extension: %w", err)
		}
		c.bestHash = newTip
		c.height = newBlock.Header.Height
		c.totalWork = newTipWork
		c.byHeight[newBlock.Header.Height] = newTip
		c.cacheTouchLocked(newTip)
		c.timestamps = append(c.timestamps, newBlock.Header.Timestamp)
		if len(c.timestamps) > LWMAWindow+1 {
			c.timestamps = c.timestamps[1:]
		}
		for _, tx := range newBlock.Transactions {
			if !tx.IsCoinbase() {
				for _, input := range tx.Inputs {
					c.keyImages[input.KeyImage] = newBlock.Header.Height
				}
			}
		}
		c.canonicalRingIndexDirty = true
		c.cacheTrimLocked()
		return nil
	}

	// Slow path: actual reorg — need ancestor walks.
	if err := c.enforceReorgFinalityLocked(newTip); err != nil {
		return err
	}

	// Find common ancestor between current tip and new tip without requiring
	// a full tip->genesis path build.
	commonHash, commonHeight, err := c.findCommonAncestorLocked(c.bestHash, newTip)
	if err != nil {
		return err
	}

	// Collect blocks to disconnect
	var disconnect []*Block
	for h := c.height; h > commonHeight; h-- {
		block := c.getBlockByHeightLocked(h)
		if block == nil {
			return fmt.Errorf("incomplete chain: cannot build ancestor path for reorg")
		}
		disconnect = append(disconnect, block)
	}

	// Collect blocks to connect
	connect, err := c.collectConnectPathLocked(commonHash, newTip)
	if err != nil {
		return err
	}

	newTipWork, err := c.cumulativeWorkAtLocked(newTip)
	if err != nil {
		return fmt.Errorf("failed to compute reorg tip work: %w", err)
	}

	// Commit reorg atomically to storage
	reorgCommit := &ReorgCommit{
		Disconnect: disconnect,
		Connect:    connect,
		NewTip:     newTip,
		NewHeight:  newBlock.Header.Height,
		NewWork:    newTipWork,
	}

	if err := c.storage.CommitReorg(reorgCommit); err != nil {
		return fmt.Errorf("failed to persist reorg: %w", err)
	}

	// Update in-memory state

	// Remove disconnected blocks from height index and key images
	for _, block := range disconnect {
		delete(c.byHeight, block.Header.Height)
		for _, tx := range block.Transactions {
			if !tx.IsCoinbase() {
				for _, input := range tx.Inputs {
					delete(c.keyImages, input.KeyImage)
				}
			}
		}
	}

	// Add connected blocks to height index and key images
	for _, block := range connect {
		hash := block.Hash()
		c.cacheTouchLocked(hash)
		c.byHeight[block.Header.Height] = hash
		c.timestamps = append(c.timestamps, block.Header.Timestamp)
		if len(c.timestamps) > LWMAWindow+1 {
			c.timestamps = c.timestamps[1:]
		}
		for _, tx := range block.Transactions {
			if !tx.IsCoinbase() {
				for _, input := range tx.Inputs {
					c.keyImages[input.KeyImage] = block.Header.Height
				}
			}
		}
	}

	// Update chain state
	c.bestHash = newTip
	c.height = newBlock.Header.Height
	c.totalWork = newTipWork
	c.canonicalRingIndexDirty = true
	c.cacheTouchLocked(newTip)
	c.cacheTrimLocked()

	return nil
}

// enforceReorgFinalityLocked rejects reorgs that would disconnect finalized blocks.
// Caller must hold c.mu.Lock.
func (c *Chain) enforceReorgFinalityLocked(newTip [32]byte) error {
	if c.height < MaxReorgDepth {
		return nil
	}

	if c.getBlockByHashLocked(newTip) == nil {
		return fmt.Errorf("unknown reorg tip")
	}

	_, commonHeight, err := c.findCommonAncestorLocked(c.bestHash, newTip)
	if err != nil {
		return err
	}

	finalizedBoundary := c.height - MaxReorgDepth
	if commonHeight < finalizedBoundary {
		return fmt.Errorf(
			"reorg crosses finalized boundary: fork at height %d, finalized boundary %d",
			commonHeight,
			finalizedBoundary,
		)
	}

	return nil
}

func (c *Chain) findCommonAncestorLocked(aTip, bTip [32]byte) ([32]byte, uint64, error) {
	aHash := aTip
	aBlock := c.getBlockByHashLocked(aHash)
	if aBlock == nil {
		return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", aHash[:8])
	}

	bHash := bTip
	bBlock := c.getBlockByHashLocked(bHash)
	if bBlock == nil {
		return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", bHash[:8])
	}

	for aBlock.Header.Height > bBlock.Header.Height {
		if aBlock.Header.Height == 0 {
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg")
		}
		aHash = aBlock.Header.PrevHash
		aBlock = c.getBlockByHashLocked(aHash)
		if aBlock == nil {
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", aHash[:8])
		}
	}

	for bBlock.Header.Height > aBlock.Header.Height {
		if bBlock.Header.Height == 0 {
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg")
		}
		bHash = bBlock.Header.PrevHash
		bBlock = c.getBlockByHashLocked(bHash)
		if bBlock == nil {
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", bHash[:8])
		}
	}

	for aHash != bHash {
		if aBlock.Header.Height == 0 || bBlock.Header.Height == 0 {
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg")
		}

		aHash = aBlock.Header.PrevHash
		bHash = bBlock.Header.PrevHash
		aBlock = c.getBlockByHashLocked(aHash)
		bBlock = c.getBlockByHashLocked(bHash)
		if aBlock == nil || bBlock == nil {
			if aBlock == nil {
				return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", aHash[:8])
			}
			return [32]byte{}, 0, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", bHash[:8])
		}
	}

	return aHash, aBlock.Header.Height, nil
}

func (c *Chain) collectConnectPathLocked(commonHash, newTip [32]byte) ([]*Block, error) {
	if newTip == commonHash {
		return nil, nil
	}

	connectRev := make([]*Block, 0, 8)
	current := newTip
	for current != commonHash {
		block := c.getBlockByHashLocked(current)
		if block == nil {
			return nil, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg (missing hash %x)", current[:8])
		}
		if block.Header.Height == 0 {
			return nil, fmt.Errorf("incomplete chain: cannot build ancestor path for reorg")
		}
		connectRev = append(connectRev, block)
		current = block.Header.PrevHash
	}

	connect := make([]*Block, len(connectRev))
	for i := range connectRev {
		connect[len(connectRev)-1-i] = connectRev[i]
	}

	return connect, nil
}

// getAncestorPath returns array of block hashes from genesis to the given hash
func (c *Chain) getAncestorPath(tip [32]byte) [][32]byte {
	block, exists := c.blocks[tip]
	if !exists {
		if b, _ := c.storage.GetBlock(tip); b != nil {
			c.blocks[tip] = b
			c.cacheTouchLocked(tip)
			c.cacheTrimLocked()
			block = b
		} else {
			return nil
		}
	} else {
		c.cacheTouchLocked(tip)
	}

	path := make([][32]byte, block.Header.Height+1)
	current := tip
	for {
		b, exists := c.blocks[current]
		if !exists {
			if loaded, _ := c.storage.GetBlock(current); loaded != nil {
				c.blocks[current] = loaded
				c.cacheTouchLocked(current)
				c.cacheTrimLocked()
				b = loaded
			} else {
				return nil
			}
		} else {
			c.cacheTouchLocked(current)
		}
		path[b.Header.Height] = current
		if b.Header.Height == 0 {
			break
		}
		current = b.Header.PrevHash
	}
	return path
}

// TruncateToHeight removes all main-chain blocks above keepHeight,
// rolling back key images, height index, and tip metadata. The chain
// will re-sync the removed portion from peers on next sync cycle.
func (c *Chain) TruncateToHeight(keepHeight uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if keepHeight >= c.height {
		return nil // nothing to do
	}

	// Disconnect blocks from top down
	for h := c.height; h > keepHeight; h-- {
		block := c.getBlockByHeightLocked(h)
		if block == nil {
			// Already missing, just clean up the index
			if err := c.storage.RemoveMainChainBlock(h); err != nil {
				return fmt.Errorf("failed to remove main-chain index at height %d: %w", h, err)
			}
			delete(c.byHeight, h)
			continue
		}

		// Remove key images from this block
		for _, tx := range block.Transactions {
			if !tx.IsCoinbase() {
				for _, input := range tx.Inputs {
					delete(c.keyImages, input.KeyImage)
					if err := c.storage.UnmarkKeyImageSpent(input.KeyImage); err != nil {
						return fmt.Errorf("failed to unmark key image at height %d: %w", h, err)
					}
				}
			}
		}

		// Remove from height index
		if err := c.storage.RemoveMainChainBlock(h); err != nil {
			return fmt.Errorf("failed to remove main-chain block at height %d: %w", h, err)
		}
		delete(c.byHeight, h)
		hash := block.Hash()
		c.cacheForgetLocked(hash)
	}

	// Set new tip
	newTip := c.getBlockByHeightLocked(keepHeight)
	if newTip == nil {
		return fmt.Errorf("block at height %d not found after truncation", keepHeight)
	}
	newHash := newTip.Hash()
	newWork, err := c.cumulativeWorkAtLocked(newHash)
	if err != nil {
		return fmt.Errorf("failed to compute cumulative work for new tip: %w", err)
	}

	c.bestHash = newHash
	c.height = keepHeight
	c.totalWork = newWork
	c.canonicalRingIndexDirty = true
	c.cacheTouchLocked(newHash)
	c.cacheTrimLocked()

	// Rebuild timestamps window from the kept chain
	c.timestamps = nil
	start := uint64(0)
	if keepHeight > uint64(LWMAWindow) {
		start = keepHeight - uint64(LWMAWindow)
	}
	for h := start; h <= keepHeight; h++ {
		b := c.getBlockByHeightLocked(h)
		if b != nil {
			c.timestamps = append(c.timestamps, b.Header.Timestamp)
		}
	}

	// Persist new tip
	if err := c.storage.SetTip(newHash, keepHeight, newWork); err != nil {
		return fmt.Errorf("failed to persist new tip: %w", err)
	}

	return nil
}

// TotalWork returns the cumulative work of the main chain
func (c *Chain) TotalWork() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.totalWork
}

// BlockTemplateParams holds the chain state needed to build a block template,
// read atomically under a single lock to prevent TOCTOU races during reorgs.
type BlockTemplateParams struct {
	Height     uint64
	PrevHash   [32]byte
	Difficulty uint64
}

// TemplateParams returns the next-block height, previous hash, and difficulty
// as a consistent snapshot.
func (c *Chain) TemplateParams() BlockTemplateParams {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return BlockTemplateParams{
		Height:     c.height + 1,
		PrevHash:   c.bestHash,
		Difficulty: c.nextDifficultyLocked(),
	}
}

// HasBlock returns true if the block is known (on any chain)
func (c *Chain) HasBlock(hash [32]byte) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, exists := c.blocks[hash]; exists {
		return true
	}
	return c.storage.HasBlock(hash)
}

// IsKeyImageSpent checks if a key image has been used (double-spend check)
func (c *Chain) IsKeyImageSpent(keyImage [32]byte) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check memory first
	if _, exists := c.keyImages[keyImage]; exists {
		return true
	}

	// Check storage
	spent, _ := c.storage.IsKeyImageSpent(keyImage)
	return spent
}

// IsCanonicalRingMember checks whether a ring member+commitment pair exists in canonical chain outputs.
func (c *Chain) IsCanonicalRingMember(pubKey, commitment [32]byte) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isCanonicalRingMemberLocked(pubKey, commitment)
}

// GetAllOutputs returns all outputs for ring member selection
func (c *Chain) GetAllOutputs() ([]*UTXO, error) {
	return c.storage.GetAllOutputs()
}

// SelectRingMembersWithCommitments selects ring members for a transaction input
func (c *Chain) SelectRingMembersWithCommitments(realPubKey, realCommitment [32]byte) (*RingMemberData, error) {
	allOutputs, err := c.storage.GetAllOutputs()
	if err != nil {
		return nil, fmt.Errorf("failed to get outputs: %w", err)
	}

	ringSize := RingSize

	// Filter out the real key from decoy selection
	decoyPool := make([]*UTXO, 0, len(allOutputs))
	for _, utxo := range allOutputs {
		if utxo.Output.PublicKey != realPubKey {
			decoyPool = append(decoyPool, utxo)
		}
	}

	if len(decoyPool) < ringSize-1 {
		return nil, fmt.Errorf("not enough outputs for ring (need %d, have %d)", ringSize-1, len(decoyPool))
	}

	// Cryptographically secure shuffle using Fisher-Yates with crypto/rand
	for i := len(decoyPool) - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, fmt.Errorf("secure random failed: %w", err)
		}
		j := int(jBig.Int64())
		decoyPool[i], decoyPool[j] = decoyPool[j], decoyPool[i]
	}
	decoys := decoyPool[:ringSize-1]

	// Create ring with random position for real key (crypto/rand)
	secretIdxBig, err := rand.Int(rand.Reader, big.NewInt(int64(ringSize)))
	if err != nil {
		return nil, fmt.Errorf("secure random failed: %w", err)
	}
	secretIndex := int(secretIdxBig.Int64())
	keys := make([][32]byte, ringSize)
	commitments := make([][32]byte, ringSize)

	decoyIdx := 0
	for i := 0; i < ringSize; i++ {
		if i == secretIndex {
			keys[i] = realPubKey
			commitments[i] = realCommitment
		} else {
			keys[i] = decoys[decoyIdx].Output.PublicKey
			commitments[i] = decoys[decoyIdx].Output.Commitment
			decoyIdx++
		}
	}

	return &RingMemberData{
		Keys:        keys,
		Commitments: commitments,
		SecretIndex: secretIndex,
	}, nil
}

// IsFinalized returns true if a block at given height is beyond reorg depth
func (c *Chain) IsFinalized(height uint64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.height < height {
		return false
	}
	return c.height-height >= MaxReorgDepth
}

// FindTxByHashStr searches for a transaction by hex hash string, scanning
// blocks from tip backwards. Returns the transaction, block height, and
// whether it was found.
func (c *Chain) FindTxByHashStr(hashStr string) (*Transaction, uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for h := c.height; ; h-- {
		block := c.getBlockByHeightLocked(h)
		if block == nil {
			if h == 0 {
				break
			}
			continue
		}

		for _, tx := range block.Transactions {
			txID, _ := tx.TxID()
			if fmt.Sprintf("%x", txID) == hashStr {
				return tx, h, true
			}
		}

		if h == 0 {
			break
		}
	}
	return nil, 0, false
}

// ============================================================================
// Genesis Block
// ============================================================================

// Source: https://www.wired.com/story/the-nothing-that-has-the-potential-to-be-anything/
const GenesisMessage = "Wired 2026/02/15 - The Nothing That Has the Potential to Be Anything"

// GenesisPrevHash returns SHA3-256(GenesisMessage)
func GenesisPrevHash() [32]byte {
	return sha3.Sum256([]byte(GenesisMessage))
}

// GenesisTimestamp is the fixed genesis block timestamp (unix seconds, UTC).
// Derived from the relaunch time unix nanoseconds: 1771207118829000000.
const GenesisTimestamp int64 = 1771207118

// GetGenesisBlock returns the hardcoded genesis block (same for all nodes)
func GetGenesisBlock() (*Block, error) {
	// Genesis has no transactions - no coinbase, no burned coins
	// First real block at height 1 will have the first coinbase
	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     0,
			PrevHash:   GenesisPrevHash(),
			Timestamp:  GenesisTimestamp,
			Difficulty: MinDifficulty,
			Nonce:      0,
		},
		Transactions: []*Transaction{},
	}

	// Empty merkle root for no transactions
	block.Header.MerkleRoot = [32]byte{}

	return block, nil
}

// CreateGenesisBlock creates a genesis block (for testing - use GetGenesisBlock for mainnet)
func CreateGenesisBlock(minerSpendPub, minerViewPub [32]byte, reward uint64) (*Block, error) {
	// Create coinbase for genesis
	coinbase, err := CreateCoinbase(minerSpendPub, minerViewPub, reward, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create genesis coinbase: %w", err)
	}

	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     0,
			PrevHash:   GenesisPrevHash(),
			Timestamp:  time.Now().Unix(),
			Difficulty: MinDifficulty,
			Nonce:      0,
		},
		Transactions: []*Transaction{coinbase.Tx},
	}

	// Compute merkle root
	merkleRoot, err := block.ComputeMerkleRoot()
	if err != nil {
		return nil, fmt.Errorf("failed to compute merkle root: %w", err)
	}
	block.Header.MerkleRoot = merkleRoot

	return block, nil
}

// ============================================================================
// Transaction Size (add to transaction.go or here)
// ============================================================================

// Size returns the canonical serialized size of a transaction in bytes.
// This must track `(*Transaction).Serialize()` exactly because block size
// enforcement (`MaxBlockSize`) is consensus-critical.
func (tx *Transaction) Size() int {
	// Prefix: version + txPubKey + inputCount + outputCount + fee
	size := 1 + 32 + 4 + 4 + 8

	// Outputs: pubkey + commitment + encrypted_amount + encrypted_memo + range_proof_len + range_proof
	for _, out := range tx.Outputs {
		size += 32 + 32 + 8 + params.MemoSize + 4 + len(out.RangeProof)
	}

	// Inputs:
	// key_image + pseudo_output + ring_size + ring_members + ring_commitments + sig_len + signature
	for _, inp := range tx.Inputs {
		ringSize := len(inp.RingMembers)
		size += 32 + 32 + 4 + ringSize*32 + ringSize*32 + 4 + len(inp.RingSignature)
	}

	return size
}

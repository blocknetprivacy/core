package main

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// MempoolConfig configures the mempool
type MempoolConfig struct {
	// MaxSize is the maximum number of transactions
	MaxSize int

	// MaxSizeBytes is the maximum total size in bytes
	MaxSizeBytes int

	// MinFeeRate is the minimum fee per byte to accept
	MinFeeRate uint64

	// ExpirationTime is how long a tx stays in mempool
	ExpirationTime time.Duration

	ReplaceRateStep uint64
}

// DefaultMempoolConfig returns sensible defaults
func DefaultMempoolConfig() MempoolConfig {
	return MempoolConfig{
		MaxSize:         5000,
		MaxSizeBytes:    100 * 1024 * 1024, // 100 MB
		MinFeeRate:      1,                 // 1 unit per byte
		ExpirationTime:  24 * time.Hour,
		ReplaceRateStep: sendFeePerByte,
	}
}

// MempoolEntry represents a transaction in the mempool
type MempoolEntry struct {
	Tx      *Transaction
	TxID    [32]byte  // Transaction ID hash
	TxData  []byte    // Serialized transaction
	Fee     uint64    // Transaction fee
	FeeRate uint64    // Fee per byte
	Size    int       // Size in bytes
	AddedAt time.Time // When added to mempool
	Height  uint64    // Block height when added

	// For priority queue
	index int
}

// Mempool stores unconfirmed transactions
type Mempool struct {
	mu sync.RWMutex

	config MempoolConfig

	// Transaction storage
	txByID    map[[32]byte]*MempoolEntry // TxID -> Entry
	txByImage map[[32]byte][32]byte      // KeyImage -> TxID (for double-spend detection)

	// Priority queue (highest fee rate first)
	priorityQueue txPriorityQueue

	// Key image checker for validation (provided by chain)
	isKeyImageSpent KeyImageChecker
	// Ring member checker for canonical output binding.
	isCanonicalRingMember RingMemberChecker

	// Stats
	totalSize  int    // Total bytes in mempool
	generation uint64 // Monotonic content version for mining-template refreshes
}

// NewMempool creates a new mempool
func NewMempool(cfg MempoolConfig, isSpent KeyImageChecker, isCanonicalRingMember RingMemberChecker) *Mempool {
	return &Mempool{
		config:                cfg,
		txByID:                make(map[[32]byte]*MempoolEntry),
		txByImage:             make(map[[32]byte][32]byte),
		priorityQueue:         make(txPriorityQueue, 0),
		isKeyImageSpent:       isSpent,
		isCanonicalRingMember: isCanonicalRingMember,
	}
}

// AddTransaction adds a transaction to the mempool.
func (m *Mempool) AddTransaction(tx *Transaction, txData []byte) error {
	parsedTx, err := DeserializeTx(txData)
	if err != nil {
		return fmt.Errorf("invalid transaction data: %w", err)
	}

	if parsedTx.IsCoinbase() {
		return fmt.Errorf("coinbase transaction cannot be added to mempool")
	}

	txID, err := parsedTx.TxID()
	if err != nil {
		return fmt.Errorf("failed to get tx ID: %w", err)
	}

	if err := ValidateTransaction(parsedTx, m.isKeyImageSpent, m.isCanonicalRingMember); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	size := len(txData)
	feeRate := tx.Fee / uint64(size)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.txByID[txID]; exists {
		return nil
	}

	// A block can commit while validation runs without the lock, and
	// OnBlockConnected won't see a transaction that isn't in the pool yet.
	for _, input := range parsedTx.Inputs {
		if m.isKeyImageSpent(input.KeyImage) {
			return fmt.Errorf("validation failed: key image already spent on chain")
		}
	}

	replaced, err := m.replacementTarget(parsedTx, feeRate, size)
	if err != nil {
		return err
	}

	if feeRate < m.config.MinFeeRate {
		return fmt.Errorf("fee rate too low: %d < %d", feeRate, m.config.MinFeeRate)
	}

	freed := 0
	if replaced != nil {
		freed = replaced.Size
	}
	if replaced == nil && len(m.txByID) >= m.config.MaxSize {
		if !m.evictLowest(feeRate) {
			return fmt.Errorf("mempool full")
		}
	}

	if m.totalSize-freed+size > m.config.MaxSizeBytes {
		for m.totalSize-freed+size > m.config.MaxSizeBytes && len(m.priorityQueue) > 0 {
			if !m.evictLowest(feeRate) {
				return fmt.Errorf("mempool size limit exceeded")
			}
			if replaced != nil {
				if _, still := m.txByID[replaced.TxID]; !still {
					freed = 0
				}
			}
		}
	}

	if replaced != nil {
		m.removeTxByID(replaced.TxID)
	}

	entry := &MempoolEntry{
		Tx:      parsedTx,
		TxID:    txID,
		TxData:  txData,
		Fee:     parsedTx.Fee,
		FeeRate: feeRate,
		Size:    size,
		AddedAt: time.Now(),
	}
	m.txByID[txID] = entry
	for _, input := range parsedTx.Inputs {
		m.txByImage[input.KeyImage] = txID
	}
	heap.Push(&m.priorityQueue, entry)
	m.totalSize += size
	m.generation++

	return nil
}

func (m *Mempool) replacementTarget(tx *Transaction, feeRate uint64, size int) (*MempoolEntry, error) {
	var oldID [32]byte
	conflicts := 0
	for _, input := range tx.Inputs {
		id, exists := m.txByImage[input.KeyImage]
		if !exists || (conflicts > 0 && id == oldID) {
			continue
		}
		oldID = id
		conflicts++
	}
	if conflicts == 0 {
		return nil, nil
	}
	if conflicts > 1 {
		return nil, fmt.Errorf("double-spend: key images already in mempool in %d transactions; a replacement may replace only one", conflicts)
	}
	old := m.txByID[oldID]
	newImages := make(map[[32]byte]struct{}, len(tx.Inputs))
	for _, input := range tx.Inputs {
		newImages[input.KeyImage] = struct{}{}
	}
	for _, input := range old.Tx.Inputs {
		if _, ok := newImages[input.KeyImage]; !ok {
			return nil, fmt.Errorf("double-spend: key image already in mempool (tx %x...); a replacement must spend every coin of the transaction it replaces", oldID[:8])
		}
	}
	if needRate := old.FeeRate + m.config.ReplaceRateStep; feeRate < needRate {
		return nil, fmt.Errorf("double-spend: key image already in mempool (tx %x...); a replacement needs a fee rate of at least %d per byte, this one pays %d", oldID[:8], needRate, feeRate)
	}
	if needFee := old.Fee + m.config.ReplaceRateStep*uint64(size); tx.Fee < needFee {
		return nil, fmt.Errorf("double-spend: key image already in mempool (tx %x...); a replacement needs a fee of at least %d, this one pays %d", oldID[:8], needFee, tx.Fee)
	}
	return old, nil
}

// evictLowest removes the transaction paying the lowest fee rate (the oldest
// of equals) if it pays less per byte than feeRate, the newcomer's rate.
func (m *Mempool) evictLowest(feeRate uint64) bool {
	var lowest *MempoolEntry
	for _, entry := range m.txByID {
		if lowest == nil || entry.FeeRate < lowest.FeeRate ||
			(entry.FeeRate == lowest.FeeRate && entry.AddedAt.Before(lowest.AddedAt)) {
			lowest = entry
		}
	}
	if lowest == nil || lowest.FeeRate >= feeRate {
		return false
	}
	m.removeTxByID(lowest.TxID)
	return true
}

// FeeRateFor returns the fee per byte a new transaction of txSize bytes needs
// to fall within the next blocks blocks of the mempool, ranked as block
// templates rank it (fee rate, highest first; blockSpace bytes and maxCount
// transactions a block). It is floor while those blocks have room for it.
// Packing gaps are ignored, so it is an estimate.
func (m *Mempool) FeeRateFor(blocks, blockSpace, maxCount, txSize int, floor uint64) uint64 {
	m.mu.RLock()
	rates := make([]struct {
		rate uint64
		size int
	}, 0, len(m.txByID))
	for _, entry := range m.txByID {
		if entry.FeeRate >= floor {
			rates = append(rates, struct {
				rate uint64
				size int
			}{entry.FeeRate, entry.Size})
		}
	}
	m.mu.RUnlock()
	sort.Slice(rates, func(i, j int) bool { return rates[i].rate > rates[j].rate })

	room, slots := blocks*blockSpace-txSize, blocks*maxCount-1
	for i := 0; i < len(rates); {
		// Paying one more than a rate puts the transaction ahead of all of
		// that rate's transactions; equal rates are ranked by TxID.
		rate, size, j := rates[i].rate, 0, i
		for ; j < len(rates) && rates[j].rate == rate; j++ {
			size += rates[j].size
		}
		if size > room || j-i > slots {
			return rate + 1
		}
		room, slots, i = room-size, slots-(j-i), j
	}
	return floor
}

// removeTxByID removes a transaction from mempool by ID
func (m *Mempool) removeTxByID(txID [32]byte) {
	entry, exists := m.txByID[txID]
	if !exists {
		return
	}

	delete(m.txByID, txID)
	for _, input := range entry.Tx.Inputs {
		delete(m.txByImage, input.KeyImage)
	}
	m.totalSize -= entry.Size
	m.generation++

	// Remove from priority queue
	if entry.index >= 0 && entry.index < len(m.priorityQueue) {
		heap.Remove(&m.priorityQueue, entry.index)
	}
}

// RemoveTransaction removes a transaction by ID
func (m *Mempool) RemoveTransaction(txID [32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeTxByID(txID)
}

// RemoveTransactions removes multiple transactions
func (m *Mempool) RemoveTransactions(txIDs [][32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, txID := range txIDs {
		m.removeTxByID(txID)
	}
}

// RemoveByKeyImage removes any transaction using a specific key image
func (m *Mempool) RemoveByKeyImage(keyImage [32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if txID, exists := m.txByImage[keyImage]; exists {
		m.removeTxByID(txID)
	}
}

// GetTransaction returns a transaction by ID
func (m *Mempool) GetTransaction(txID [32]byte) (*Transaction, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, exists := m.txByID[txID]
	if !exists {
		return nil, false
	}
	return entry.Tx, true
}

func (m *Mempool) GetEntry(txID [32]byte) (tx *Transaction, fee uint64, size int, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, exists := m.txByID[txID]
	if !exists {
		return nil, 0, 0, false
	}
	return entry.Tx, entry.Fee, entry.Size, true
}

func (m *Mempool) ReplaceRateStep() uint64 {
	return m.config.ReplaceRateStep
}

// HasTransaction checks if a transaction is in the mempool
func (m *Mempool) HasTransaction(txID [32]byte) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.txByID[txID]
	return exists
}

// HasKeyImage checks if a key image is used by any mempool tx
func (m *Mempool) HasKeyImage(keyImage [32]byte) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.txByImage[keyImage]
	return exists
}

// GetTransactionsForBlock returns transactions for mining sorted by fee rate.
func (m *Mempool) GetTransactionsForBlock(maxSize int, maxCount int) []*Transaction {
	txs, _ := m.GetTransactionsForBlockSnapshot(maxSize, maxCount)
	return txs
}

// GetTransactionsForBlockSnapshot returns a mining transaction selection and
// the exact mempool generation from the same read lock. Callers can attach the
// generation to a block template without racing a concurrent mempool change.
func (m *Mempool) GetTransactionsForBlockSnapshot(maxSize int, maxCount int) ([]*Transaction, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]*MempoolEntry, 0, len(m.txByID))
	for _, entry := range m.txByID {
		entries = append(entries, entry)
	}

	// Fee rate descending, then TxID. Selection is miner policy, not a
	// validation rule, but the tie-break makes a template reproducible instead
	// of depending on map iteration order.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FeeRate != entries[j].FeeRate {
			return entries[i].FeeRate > entries[j].FeeRate
		}
		return bytes.Compare(entries[i].TxID[:], entries[j].TxID[:]) < 0
	})

	// Select transactions
	result := make([]*Transaction, 0, maxCount)
	totalSize := 0

	for _, entry := range entries {
		if len(result) >= maxCount {
			break
		}
		if totalSize+entry.Size > maxSize {
			continue // Skip, try next
		}

		result = append(result, entry.Tx)
		totalSize += entry.Size

	}

	return result, m.generation
}

// spendableOnChain reports whether the chain's current state would still accept
// this transaction's key images and ring members. Those are the admission checks
// a tip change or reorg can invalidate.
func (m *Mempool) spendableOnChain(tx *Transaction) bool {
	for _, input := range tx.Inputs {
		if m.isKeyImageSpent(input.KeyImage) {
			return false
		}
		for j := range input.RingMembers {
			if j >= len(input.RingCommitments) || !m.isCanonicalRingMember(input.RingMembers[j], input.RingCommitments[j]) {
				return false
			}
		}
	}
	return true
}

// KeepSpendableOnChain returns the transactions the chain would still accept and
// removes the rest from the mempool, so one bad entry can't make every block
// built from a template invalid.
func (m *Mempool) KeepSpendableOnChain(txs []*Transaction) []*Transaction {
	kept := txs[:0:0]
	for _, tx := range txs {
		if m.spendableOnChain(tx) {
			kept = append(kept, tx)
			continue
		}
		txID, err := tx.TxID()
		if err != nil {
			continue
		}
		log.Printf("[mempool] removing %x: no longer valid on the current chain", txID[:8])
		m.RemoveTransaction(txID)
	}
	return kept
}

// RemoveUnspendableOnChain rechecks every entry against the current chain. It
// runs after a reorg, which can remove the outputs a ring refers to.
func (m *Mempool) RemoveUnspendableOnChain() {
	m.mu.RLock()
	txs := make([]*Transaction, 0, len(m.txByID))
	for _, entry := range m.txByID {
		txs = append(txs, entry.Tx)
	}
	m.mu.RUnlock()
	m.KeepSpendableOnChain(txs)
}

// Size returns the number of transactions in mempool
func (m *Mempool) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.txByID)
}

// SizeBytes returns the total size in bytes
func (m *Mempool) SizeBytes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalSize
}

// Clear removes all transactions
func (m *Mempool) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.txByID) == 0 {
		return
	}

	m.txByID = make(map[[32]byte]*MempoolEntry)
	m.txByImage = make(map[[32]byte][32]byte)
	m.priorityQueue = make(txPriorityQueue, 0)
	m.totalSize = 0
	m.generation++
}

// RemoveExpired removes transactions that have been in mempool too long
func (m *Mempool) RemoveExpired() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-m.config.ExpirationTime)
	removed := 0

	// Collect IDs to remove (can't modify map while iterating)
	var toRemove [][32]byte
	for txID, entry := range m.txByID {
		if entry.AddedAt.Before(cutoff) {
			toRemove = append(toRemove, txID)
		}
	}

	for _, txID := range toRemove {
		m.removeTxByID(txID)
		removed++
	}

	return removed
}

// OnBlockConnected updates mempool when a block is connected
// Removes transactions that were included in the block
func (m *Mempool) OnBlockConnected(block *Block) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			continue
		}
		if _, exists := m.txByID[txID]; exists {
			m.removeTxByID(txID)
		}

		// Also remove by key images (in case of double-spend attempts)
		for _, input := range tx.Inputs {
			if existingTxID, exists := m.txByImage[input.KeyImage]; exists {
				m.removeTxByID(existingTxID)
			}
		}
	}
}

// OnBlockDisconnected updates mempool when a block is disconnected (reorg)
// Re-adds valid transactions from the disconnected block
func (m *Mempool) OnBlockDisconnected(block *Block, txDataMap map[[32]byte][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, tx := range block.Transactions {
		// Skip coinbase - can't go back to mempool
		if tx.IsCoinbase() {
			continue
		}

		txID, err := tx.TxID()
		if err != nil {
			continue
		}
		// Already present (e.g. gossiped while block was on-chain) - keep existing
		// entry/heap state unchanged.
		if _, exists := m.txByID[txID]; exists {
			continue
		}

		// Get serialized tx data if available
		txData, ok := txDataMap[txID]
		if !ok {
			txData = tx.Serialize()
		}
		// Check if any inputs are now spent (by another chain)
		// Skip if key images are spent
		valid := true
		for _, input := range tx.Inputs {
			// Keep mempool free of double-spends.
			if existingTxID, exists := m.txByImage[input.KeyImage]; exists && existingTxID != txID {
				valid = false
				break
			}
			if m.isKeyImageSpent(input.KeyImage) {
				valid = false
				break
			}
		}

		if valid {
			// Re-add to mempool (using internal method to avoid double-lock)
			size := len(txData)
			if size == 0 {
				continue
			}
			feeRate := tx.Fee / uint64(size)

			entry := &MempoolEntry{
				Tx:      tx,
				TxID:    txID,
				TxData:  txData,
				Fee:     tx.Fee,
				FeeRate: feeRate,
				Size:    size,
				AddedAt: time.Now(),
			}

			m.txByID[txID] = entry
			for _, input := range tx.Inputs {
				m.txByImage[input.KeyImage] = txID
			}
			heap.Push(&m.priorityQueue, entry)
			m.totalSize += size
			m.generation++
		}
	}
}

// GetAllTransactionData returns all serialized transactions
func (m *Mempool) GetAllTransactionData() [][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([][]byte, 0, len(m.txByID))
	for _, entry := range m.txByID {
		result = append(result, entry.TxData)
	}
	return result
}

// Stats returns mempool statistics
type MempoolStats struct {
	Count      int     `json:"count"`
	SizeBytes  int     `json:"size_bytes"`
	Generation uint64  `json:"generation"`
	MinFee     uint64  `json:"min_fee"`
	MaxFee     uint64  `json:"max_fee"`
	AvgFee     float64 `json:"avg_fee"`
}

func (m *Mempool) Stats() MempoolStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := MempoolStats{
		Count:      len(m.txByID),
		SizeBytes:  m.totalSize,
		Generation: m.generation,
	}

	if stats.Count == 0 {
		return stats
	}

	var totalFee uint64
	for _, entry := range m.txByID {
		if stats.MinFee == 0 || entry.Fee < stats.MinFee {
			stats.MinFee = entry.Fee
		}
		if entry.Fee > stats.MaxFee {
			stats.MaxFee = entry.Fee
		}
		totalFee += entry.Fee
	}
	stats.AvgFee = float64(totalFee) / float64(stats.Count)

	return stats
}

// GetAllEntries returns all mempool entries sorted by fee rate (highest first)
func (m *Mempool) GetAllEntries() []*MempoolEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Copies, not the live entries: MempoolEntry.index is mutated by the
	// priority queue, so a caller reading after the lock drops would race.
	entries := make([]*MempoolEntry, 0, len(m.txByID))
	for _, entry := range m.txByID {
		snapshot := *entry
		entries = append(entries, &snapshot)
	}

	// Fee rate descending, then TxID, so equal-fee transactions keep a stable
	// order instead of following map iteration.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FeeRate != entries[j].FeeRate {
			return entries[i].FeeRate > entries[j].FeeRate
		}
		return bytes.Compare(entries[i].TxID[:], entries[j].TxID[:]) < 0
	})

	return entries
}

// MarshalJSON serializes mempool for debugging
func (m *Mempool) MarshalJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data := struct {
		Count     int      `json:"count"`
		SizeBytes int      `json:"size_bytes"`
		TxIDs     []string `json:"tx_ids"`
	}{
		Count:     len(m.txByID),
		SizeBytes: m.totalSize,
		TxIDs:     make([]string, 0, len(m.txByID)),
	}

	for txID := range m.txByID {
		data.TxIDs = append(data.TxIDs, fmt.Sprintf("%x", txID))
	}

	return json.Marshal(data)
}

// Priority queue implementation for mempool

type txPriorityQueue []*MempoolEntry

func (pq txPriorityQueue) Len() int { return len(pq) }

func (pq txPriorityQueue) Less(i, j int) bool {
	// Higher fee rate = higher priority
	return pq[i].FeeRate > pq[j].FeeRate
}

func (pq txPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *txPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	entry := x.(*MempoolEntry)
	entry.index = n
	*pq = append(*pq, entry)
}

func (pq *txPriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	entry.index = -1
	*pq = old[0 : n-1]
	return entry
}

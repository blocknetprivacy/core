package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"

	bolt "go.etcd.io/bbolt"
)

// bucketTxIndex maps a main-chain transaction id to the height of the block
// that contains it. Without it, looking up a transaction by hash costs a walk
// from the tip to genesis, decoding every block on the way.
var bucketTxIndex = []byte("tx_index")

// metaKeyTxIndexBuilt marks the index as fully backfilled for the chain that
// was on disk at the time. Absent on a database written by an older core.
var metaKeyTxIndexBuilt = []byte("tx_index_built")

// indexBlockTxsLocked adds every transaction in the block to the index at the
// given height. Caller must be inside a bolt write transaction.
func indexBlockTxsLocked(txIndex *bolt.Bucket, block *Block, heightBytes []byte) error {
	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			return fmt.Errorf("failed to hash transaction for index: %w", err)
		}
		if err := txIndex.Put(txID[:], heightBytes); err != nil {
			return err
		}
	}
	return nil
}

// unindexBlockTxsLocked removes a disconnected block's transactions from the
// index. Caller must be inside a bolt write transaction, and must run this
// before indexing the replacement blocks so a transaction present on both
// branches keeps its new height.
func unindexBlockTxsLocked(txIndex *bolt.Bucket, block *Block) error {
	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			return fmt.Errorf("failed to hash transaction for index removal: %w", err)
		}
		if err := txIndex.Delete(txID[:]); err != nil {
			return err
		}
	}
	return nil
}

// GetTxHeight returns the main-chain height of the block containing txid.
func (s *Storage) GetTxHeight(txid [32]byte) (uint64, bool) {
	var height uint64
	var found bool

	if err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketTxIndex).Get(txid[:])
		if len(data) == 8 {
			height = binary.BigEndian.Uint64(data)
			found = true
		}
		return nil
	}); err != nil {
		log.Printf("storage GetTxHeight view failed: %v", err)
		return 0, false
	}

	return height, found
}

// UnindexBlockTxs drops a block's transactions from the index, for callers
// outside CommitReorg that take a block off the main chain.
func (s *Storage) UnindexBlockTxs(block *Block) error {
	if block == nil {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return unindexBlockTxsLocked(tx.Bucket(bucketTxIndex), block)
	})
}

// TxIndexReady reports whether the index covers the whole main chain. Until it
// does, a lookup miss must fall back to scanning rather than report not-found.
func (s *Storage) TxIndexReady() bool {
	s.txIndexMu.RLock()
	defer s.txIndexMu.RUnlock()
	return s.txIndexBuilt
}

// findTxByIndex resolves a hex transaction id through the index. It returns
// false when the id is malformed, unindexed, or its block cannot be read, so
// the caller can decide whether to fall back to a scan.
func (c *Chain) findTxByIndex(hashStr string) (*Transaction, uint64, bool) {
	if len(hashStr) != 64 {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(hashStr)
	if err != nil {
		return nil, 0, false
	}
	var txID [32]byte
	copy(txID[:], raw)

	height, found := c.storage.GetTxHeight(txID)
	if !found {
		return nil, 0, false
	}

	hash, found := c.storage.GetBlockHashByHeight(height)
	if !found {
		return nil, 0, false
	}
	block, err := c.storage.GetBlock(hash)
	if err != nil || block == nil {
		return nil, 0, false
	}

	for _, tx := range block.Transactions {
		id, err := tx.TxID()
		if err == nil && id == txID {
			return tx, height, true
		}
	}
	return nil, 0, false
}

// FindTxHeights resolves a batch of transaction ids to their main-chain
// heights. Unresolved ids are simply absent from the result. Falls back to a
// single tip-backwards scan that resolves the whole batch at once while the
// index is still being built.
func (c *Chain) FindTxHeights(txIDs map[[32]byte]struct{}) map[[32]byte]uint64 {
	heights := make(map[[32]byte]uint64, len(txIDs))
	if len(txIDs) == 0 {
		return heights
	}

	if c.storage.TxIndexReady() {
		for txID := range txIDs {
			if height, found := c.storage.GetTxHeight(txID); found {
				heights[txID] = height
			}
		}
		return heights
	}

	pending := make(map[[32]byte]struct{}, len(txIDs))
	for txID := range txIDs {
		pending[txID] = struct{}{}
	}

	c.mu.RLock()
	tipHeight := c.height
	c.mu.RUnlock()

	for h := tipHeight; ; h-- {
		hash, found := c.storage.GetBlockHashByHeight(h)
		if found {
			block, err := c.storage.GetBlock(hash)
			if err == nil && block != nil {
				for _, tx := range block.Transactions {
					txID, err := tx.TxID()
					if err != nil {
						continue
					}
					if _, ok := pending[txID]; !ok {
						continue
					}
					heights[txID] = h
					delete(pending, txID)
				}
			}
		}
		if h == 0 || len(pending) == 0 {
			break
		}
	}

	return heights
}

// backfillTxIndex indexes every main-chain block once, for databases written by
// a core that predates the index. It is a no-op on an already-built index.
//
// Runs synchronously at open: the chain is not serving requests yet, and a
// half-built index would have to be tracked across concurrent commits.
func (s *Storage) backfillTxIndex() error {
	var built bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		built = tx.Bucket(bucketMeta).Get(metaKeyTxIndexBuilt) != nil
		return nil
	}); err != nil {
		return err
	}
	if built {
		s.setTxIndexBuilt(true)
		return nil
	}

	_, tipHeight, _, found := s.GetTip()
	if !found {
		if err := s.markTxIndexBuilt(); err != nil {
			return err
		}
		s.setTxIndexBuilt(true)
		return nil
	}

	log.Printf("building transaction index for %d blocks (one time)", tipHeight+1)

	for start := uint64(0); start <= tipHeight; start += txIndexBackfillBatch {
		end := start + txIndexBackfillBatch - 1
		if end > tipHeight {
			end = tipHeight
		}
		if err := s.backfillTxIndexRange(start, end); err != nil {
			return err
		}
	}

	if err := s.markTxIndexBuilt(); err != nil {
		return err
	}
	s.setTxIndexBuilt(true)
	log.Printf("transaction index built")
	return nil
}

// txIndexBackfillBatch bounds how many blocks are indexed per bolt write
// transaction, so the backfill does not hold one giant transaction in memory.
const txIndexBackfillBatch = 1000

func (s *Storage) backfillTxIndexRange(startHeight, endHeight uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		heights := tx.Bucket(bucketHeights)
		blocks := tx.Bucket(bucketBlocks)
		txIndex := tx.Bucket(bucketTxIndex)

		for h := startHeight; h <= endHeight; h++ {
			hashData := heights.Get(heightKey(h))
			if hashData == nil {
				continue
			}
			blockData := blocks.Get(hashData)
			if blockData == nil {
				continue
			}

			block, err := decodeBlock(blockData)
			if err != nil {
				return fmt.Errorf("failed to decode block at height %d for index: %w", h, err)
			}
			if err := indexBlockTxsLocked(txIndex, block, heightKey(h)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Storage) markTxIndexBuilt() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(metaKeyTxIndexBuilt, []byte{1})
	})
}

func (s *Storage) setTxIndexBuilt(built bool) {
	s.txIndexMu.Lock()
	s.txIndexBuilt = built
	s.txIndexMu.Unlock()
}

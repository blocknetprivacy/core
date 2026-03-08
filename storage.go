package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

// Bucket names
var (
	bucketBlocks    = []byte("blocks")     // hash -> block bytes
	bucketHeights   = []byte("heights")    // height (big-endian) -> hash (main chain only)
	bucketOutputs   = []byte("outputs")    // outpoint -> output bytes (ALL outputs, for ring selection)
	bucketKeyImages = []byte("key_images") // key_image -> block height (spent tracking)
	bucketMeta      = []byte("meta")       // metadata: tip, height, etc.

	metaKeyTip    = []byte("tip")
	metaKeyHeight = []byte("height")
	metaKeyWork   = []byte("work")
)

// Storage wraps bbolt for chain persistence
type Storage struct {
	db *bolt.DB
}

func heightKey(height uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, height)
	return key
}

func readTipMeta(meta *bolt.Bucket) (hash [32]byte, height uint64, found bool, err error) {
	tipData := meta.Get(metaKeyTip)
	heightData := meta.Get(metaKeyHeight)

	if tipData == nil {
		if heightData != nil {
			return hash, 0, false, fmt.Errorf("height metadata present without tip metadata")
		}
		return hash, 0, false, nil
	}
	if len(tipData) != 32 {
		return hash, 0, false, fmt.Errorf("invalid tip hash length: got %d", len(tipData))
	}
	if len(heightData) != 8 {
		return hash, 0, false, fmt.Errorf("invalid tip height length: got %d", len(heightData))
	}

	copy(hash[:], tipData)
	height = binary.BigEndian.Uint64(heightData)
	return hash, height, true, nil
}

// NewStorage opens or creates the chain database
func NewStorage(dataDir string) (*Storage, error) {
	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, DefaultChainDBFilename)
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		NoSync: false, // Ensure durability
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketBlocks, bucketHeights, bucketOutputs, bucketKeyImages, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("failed to create buckets: %w (additionally failed to close db: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("failed to create buckets: %w", err)
	}

	return &Storage{db: db}, nil
}

// Close closes the database
func (s *Storage) Close() error {
	return s.db.Close()
}

// ============================================================================
// Block Operations
// ============================================================================

// SaveBlock stores a block by its hash
func (s *Storage) SaveBlock(block *Block) error {
	if block == nil {
		return fmt.Errorf("cannot save nil block")
	}

	hash := block.Hash()
	data, err := json.Marshal(block)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		if block.Header.Height > 0 && blocks.Get(block.Header.PrevHash[:]) == nil {
			return fmt.Errorf("block %x at height %d has missing parent %x", hash[:8], block.Header.Height, block.Header.PrevHash[:8])
		}
		return blocks.Put(hash[:], data)
	})
}

// GetBlock retrieves a block by hash
func (s *Storage) GetBlock(hash [32]byte) (*Block, error) {
	var block *Block

	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketBlocks).Get(hash[:])
		if data == nil {
			return nil // Not found
		}
		block = &Block{}
		return json.Unmarshal(data, block)
	})

	return block, err
}

// HasBlock checks if a block exists
func (s *Storage) HasBlock(hash [32]byte) bool {
	var exists bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		exists = tx.Bucket(bucketBlocks).Get(hash[:]) != nil
		return nil
	}); err != nil {
		log.Printf("storage HasBlock view failed: %v", err)
		return false
	}
	return exists
}

// ============================================================================
// Height Index (Main Chain Only)
// ============================================================================

// SetMainChainBlock sets the block hash at a height (main chain)
func (s *Storage) SetMainChainBlock(height uint64, hash [32]byte) error {
	key := heightKey(height)

	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHeights).Put(key, hash[:])
	})
}

// GetBlockHashByHeight gets the main chain block hash at height
func (s *Storage) GetBlockHashByHeight(height uint64) ([32]byte, bool) {
	key := heightKey(height)

	var hash [32]byte
	var found bool

	if err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketHeights).Get(key)
		if data != nil {
			copy(hash[:], data)
			found = true
		}
		return nil
	}); err != nil {
		log.Printf("storage GetBlockHashByHeight view failed: %v", err)
		return [32]byte{}, false
	}

	return hash, found
}

// RemoveMainChainBlock removes a height from main chain index (for reorgs)
func (s *Storage) RemoveMainChainBlock(height uint64) error {
	key := heightKey(height)

	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHeights).Delete(key)
	})
}

// ============================================================================
// Output Operations (Privacy Coin Model)
// In a privacy coin, we store ALL outputs ever created (for ring member selection)
// and track spent key images separately.
// ============================================================================

// outpointKey creates a key from txid and output index
func outpointKey(txid [32]byte, index uint32) []byte {
	key := make([]byte, 36)
	copy(key[:32], txid[:])
	binary.BigEndian.PutUint32(key[32:], index)
	return key
}

// SaveOutput stores an output (never deleted - needed for ring selection)
func (s *Storage) SaveOutput(output *UTXO) error {
	key := outpointKey(output.TxID, output.OutputIndex)
	data, err := json.Marshal(output)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketOutputs).Put(key, data)
	})
}

// GetOutput retrieves an output by txid and index
func (s *Storage) GetOutput(txid [32]byte, index uint32) (*UTXO, error) {
	key := outpointKey(txid, index)
	var output *UTXO

	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketOutputs).Get(key)
		if data == nil {
			return nil
		}
		output = &UTXO{}
		return json.Unmarshal(data, output)
	})

	return output, err
}

// GetAllOutputs returns all outputs for ring member selection
func (s *Storage) GetAllOutputs() ([]*UTXO, error) {
	var outputs []*UTXO

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOutputs)
		return b.ForEach(func(k, v []byte) error {
			output := &UTXO{}
			if err := json.Unmarshal(v, output); err != nil {
				return err
			}
			outputs = append(outputs, output)
			return nil
		})
	})

	return outputs, err
}

// CountOutputs returns total number of outputs
func (s *Storage) CountOutputs() int {
	var count int
	if err := s.db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket(bucketOutputs).Stats().KeyN
		return nil
	}); err != nil {
		log.Printf("storage CountOutputs view failed: %v", err)
		return 0
	}
	return count
}

// ============================================================================
// Key Image Operations (Double-Spend Prevention)
// ============================================================================

// MarkKeyImageSpent records a key image as spent at a block height
func (s *Storage) MarkKeyImageSpent(keyImage [32]byte, height uint64) error {
	heightBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, height)

	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKeyImages).Put(keyImage[:], heightBytes)
	})
}

// IsKeyImageSpent checks if a key image has been used
func (s *Storage) IsKeyImageSpent(keyImage [32]byte) (spent bool, height uint64) {
	if err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketKeyImages).Get(keyImage[:])
		if data != nil {
			spent = true
			if len(data) == 8 {
				height = binary.BigEndian.Uint64(data)
			}
		}
		return nil
	}); err != nil {
		log.Printf("storage IsKeyImageSpent view failed: %v", err)
		return false, 0
	}
	return
}

// UnmarkKeyImageSpent removes a key image (for reorgs)
func (s *Storage) UnmarkKeyImageSpent(keyImage [32]byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKeyImages).Delete(keyImage[:])
	})
}

// GetSpentKeyImageCount returns number of spent key images
func (s *Storage) GetSpentKeyImageCount() int {
	var count int
	if err := s.db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket(bucketKeyImages).Stats().KeyN
		return nil
	}); err != nil {
		log.Printf("storage GetSpentKeyImageCount view failed: %v", err)
		return 0
	}
	return count
}

// ============================================================================
// Metadata Operations
// ============================================================================

// GetTip returns the best block hash and height
func (s *Storage) GetTip() (hash [32]byte, height uint64, work uint64, found bool) {
	if err := s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)

		if data := meta.Get(metaKeyTip); data != nil {
			copy(hash[:], data)
			found = true
		}

		if data := meta.Get(metaKeyHeight); len(data) == 8 {
			height = binary.BigEndian.Uint64(data)
		}

		if data := meta.Get(metaKeyWork); len(data) == 8 {
			work = binary.BigEndian.Uint64(data)
		}

		return nil
	}); err != nil {
		log.Printf("storage GetTip view failed: %v", err)
		return [32]byte{}, 0, 0, false
	}
	return
}

// SetTip updates the chain tip
func (s *Storage) SetTip(hash [32]byte, height, work uint64) error {
	heightBytes := make([]byte, 8)
	workBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, height)
	binary.BigEndian.PutUint64(workBytes, work)

	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(metaKeyTip, hash[:]); err != nil {
			return err
		}
		if err := meta.Put(metaKeyHeight, heightBytes); err != nil {
			return err
		}
		return meta.Put(metaKeyWork, workBytes)
	})
}

// ============================================================================
// Batch Operations (for atomic block commits)
// Privacy coin model: outputs are never deleted, key images are tracked
// ============================================================================

// BlockCommit represents an atomic block commit with all changes
type BlockCommit struct {
	Block        *Block
	Height       uint64
	Hash         [32]byte
	Work         uint64
	IsMainTip    bool       // Update tip?
	NewOutputs   []*UTXO    // Outputs to add
	SpentKeyImgs [][32]byte // Key images that are spent
}

// CommitBlock atomically writes a block and all related changes
func (s *Storage) CommitBlock(commit *BlockCommit) error {
	if commit == nil {
		return fmt.Errorf("nil block commit")
	}
	if commit.Block == nil {
		return fmt.Errorf("nil block in block commit")
	}
	if commit.Height != commit.Block.Header.Height {
		return fmt.Errorf("commit height mismatch: commit=%d block=%d", commit.Height, commit.Block.Header.Height)
	}
	if commit.Hash != commit.Block.Hash() {
		return fmt.Errorf("commit hash mismatch with block header hash")
	}

	blockData, err := json.Marshal(commit.Block)
	if err != nil {
		return err
	}

	heightBytes := heightKey(commit.Height)

	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		heights := tx.Bucket(bucketHeights)
		outputs := tx.Bucket(bucketOutputs)
		keyImages := tx.Bucket(bucketKeyImages)
		meta := tx.Bucket(bucketMeta)

		if commit.Block.Header.Height > 0 && blocks.Get(commit.Block.Header.PrevHash[:]) == nil {
			return fmt.Errorf("main-chain commit block parent missing: height=%d prev=%x", commit.Block.Header.Height, commit.Block.Header.PrevHash[:8])
		}

		if commit.IsMainTip {
			tipHash, tipHeight, found, err := readTipMeta(meta)
			if err != nil {
				return fmt.Errorf("invalid tip metadata: %w", err)
			}

			if !found {
				if commit.Height != 0 {
					return fmt.Errorf("cannot commit non-genesis tip to empty chain: height=%d", commit.Height)
				}
			} else {
				if commit.Height != tipHeight+1 {
					return fmt.Errorf("tip height linkage mismatch: current=%d new=%d", tipHeight, commit.Height)
				}
				if commit.Block.Header.PrevHash != tipHash {
					return fmt.Errorf("tip hash linkage mismatch: expected prev %x got %x", tipHash[:8], commit.Block.Header.PrevHash[:8])
				}
			}
		}

		// Store block
		if err := blocks.Put(commit.Hash[:], blockData); err != nil {
			return err
		}

		// If this is the new main chain tip
		if commit.IsMainTip {
			// Set height -> hash mapping
			if err := heights.Put(heightBytes, commit.Hash[:]); err != nil {
				return err
			}

			// Add new outputs (never deleted)
			for _, out := range commit.NewOutputs {
				key := outpointKey(out.TxID, out.OutputIndex)
				data, err := json.Marshal(out)
				if err != nil {
					return err
				}
				if err := outputs.Put(key, data); err != nil {
					return err
				}
			}

			// Mark key images as spent
			for _, ki := range commit.SpentKeyImgs {
				if err := keyImages.Put(ki[:], heightBytes); err != nil {
					return err
				}
			}

			// Update tip metadata
			workBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(workBytes, commit.Work)

			if err := meta.Put(metaKeyTip, commit.Hash[:]); err != nil {
				return err
			}
			if err := meta.Put(metaKeyHeight, heightBytes); err != nil {
				return err
			}
			if err := meta.Put(metaKeyWork, workBytes); err != nil {
				return err
			}
		}

		return nil
	})
}

// ReorgCommit handles rolling back and applying blocks atomically
type ReorgCommit struct {
	// Blocks to disconnect (key images unmarked, height index removed)
	Disconnect []*Block
	// Blocks to connect (outputs added, key images marked)
	Connect []*Block
	// New tip after reorg
	NewTip    [32]byte
	NewHeight uint64
	NewWork   uint64
}

// CommitReorg atomically performs a chain reorganization
func (s *Storage) CommitReorg(commit *ReorgCommit) error {
	if commit == nil {
		return fmt.Errorf("nil reorg commit")
	}
	if len(commit.Connect) == 0 {
		return fmt.Errorf("reorg commit requires at least one block to connect")
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		heights := tx.Bucket(bucketHeights)
		outputs := tx.Bucket(bucketOutputs)
		keyImages := tx.Bucket(bucketKeyImages)
		meta := tx.Bucket(bucketMeta)

		currentTip, currentHeight, found, err := readTipMeta(meta)
		if err != nil {
			return fmt.Errorf("invalid current tip metadata: %w", err)
		}
		if !found {
			return fmt.Errorf("cannot apply reorg on empty chain")
		}
		if len(commit.Disconnect) > int(currentHeight+1) {
			return fmt.Errorf("disconnect set too deep for current height: disconnect=%d currentHeight=%d", len(commit.Disconnect), currentHeight)
		}

		expectedNewHeight := currentHeight - uint64(len(commit.Disconnect)) + uint64(len(commit.Connect))
		if commit.NewHeight != expectedNewHeight {
			return fmt.Errorf("reorg new height mismatch: expected=%d got=%d", expectedNewHeight, commit.NewHeight)
		}

		baseHash := currentTip
		baseHeight := currentHeight
		if len(commit.Disconnect) > 0 {
			for i, block := range commit.Disconnect {
				if block == nil {
					return fmt.Errorf("disconnect[%d] is nil", i)
				}
				hash := block.Hash()
				expectedHeight := currentHeight - uint64(i)
				if block.Header.Height != expectedHeight {
					return fmt.Errorf("disconnect[%d] height mismatch: expected=%d got=%d", i, expectedHeight, block.Header.Height)
				}
				mainHash := heights.Get(heightKey(expectedHeight))
				if mainHash == nil {
					return fmt.Errorf("main-chain height %d missing during disconnect", expectedHeight)
				}
				var indexedHash [32]byte
				copy(indexedHash[:], mainHash)
				if indexedHash != hash {
					return fmt.Errorf("disconnect[%d] hash mismatch with height index at %d", i, expectedHeight)
				}
				if i > 0 {
					prev := commit.Disconnect[i-1]
					if prev.Header.PrevHash != hash {
						return fmt.Errorf("disconnect linkage mismatch between heights %d and %d", prev.Header.Height, block.Header.Height)
					}
				}
			}

			lowestDisconnected := commit.Disconnect[len(commit.Disconnect)-1]
			if lowestDisconnected.Header.Height == 0 {
				return fmt.Errorf("reorg cannot disconnect genesis block")
			}
			baseHash = lowestDisconnected.Header.PrevHash
			baseHeight = lowestDisconnected.Header.Height - 1
		}

		if blocks.Get(baseHash[:]) == nil {
			return fmt.Errorf("reorg base block not found: %x", baseHash[:8])
		}

		expectedPrevHash := baseHash
		expectedConnectHeight := baseHeight + 1
		for i, block := range commit.Connect {
			if block == nil {
				return fmt.Errorf("connect[%d] is nil", i)
			}
			hash := block.Hash()
			if block.Header.Height != expectedConnectHeight {
				return fmt.Errorf("connect[%d] height mismatch: expected=%d got=%d", i, expectedConnectHeight, block.Header.Height)
			}
			if block.Header.PrevHash != expectedPrevHash {
				return fmt.Errorf("connect[%d] parent mismatch: expected prev %x got %x", i, expectedPrevHash[:8], block.Header.PrevHash[:8])
			}
			expectedPrevHash = hash
			expectedConnectHeight++
		}
		if commit.NewTip != expectedPrevHash {
			return fmt.Errorf("reorg new tip mismatch: expected=%x got=%x", expectedPrevHash[:8], commit.NewTip[:8])
		}

		// Disconnect blocks (reverse order, unmark key images)
		for i := len(commit.Disconnect) - 1; i >= 0; i-- {
			block := commit.Disconnect[i]

			// Remove from height index
			if err := heights.Delete(heightKey(block.Header.Height)); err != nil {
				return err
			}

			// Unmark key images from this block's transactions
			for _, txn := range block.Transactions {
				if !txn.IsCoinbase() {
					for _, input := range txn.Inputs {
						if err := keyImages.Delete(input.KeyImage[:]); err != nil {
							return err
						}
					}
				}
			}

			// Outputs are NOT deleted — they may still be referenced by other blocks.
			// Ring selection filters to canonical-only via IsCanonicalRingMember.
		}

		// Connect new blocks (forward order)
		for _, block := range commit.Connect {
			hKey := heightKey(block.Header.Height)
			hash := block.Hash()

			// Save block data
			blockData, err := json.Marshal(block)
			if err != nil {
				return fmt.Errorf("failed to marshal block: %w", err)
			}
			if err := blocks.Put(hash[:], blockData); err != nil {
				return fmt.Errorf("failed to save block: %w", err)
			}

			// Add to height index
			if err := heights.Put(hKey, hash[:]); err != nil {
				return err
			}

			// Add outputs
			for _, txn := range block.Transactions {
				txid, _ := txn.TxID()
				for idx, out := range txn.Outputs {
					newOutput := &UTXO{
						TxID:        txid,
						OutputIndex: uint32(idx),
						Output:      out,
						BlockHeight: block.Header.Height,
					}
					data, err := json.Marshal(newOutput)
					if err != nil {
						return fmt.Errorf("failed to marshal output: %w", err)
					}
					key := outpointKey(txid, uint32(idx))
					if err := outputs.Put(key, data); err != nil {
						return err
					}
				}
			}

			// Mark key images as spent
			for _, txn := range block.Transactions {
				if !txn.IsCoinbase() {
					for _, input := range txn.Inputs {
						if err := keyImages.Put(input.KeyImage[:], hKey); err != nil {
							return err
						}
					}
				}
			}
		}

		// Update tip
		heightBytes := make([]byte, 8)
		workBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(heightBytes, commit.NewHeight)
		binary.BigEndian.PutUint64(workBytes, commit.NewWork)

		if err := meta.Put(metaKeyTip, commit.NewTip[:]); err != nil {
			return err
		}
		if err := meta.Put(metaKeyHeight, heightBytes); err != nil {
			return err
		}
		if err := meta.Put(metaKeyWork, workBytes); err != nil {
			return err
		}

		return nil
	})
}

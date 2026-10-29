package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// chainDBLockTimeout bounds the wait for the database file lock. Without it
// bbolt blocks forever, so a second node on the same data dir hangs silently
// instead of failing with a message anyone can act on.
const chainDBLockTimeout = 5 * time.Second

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

	// tipMu guards the cached tip. Storage is the only writer of the tip
	// metadata, so the cache is authoritative once loaded at open.
	tipMu     sync.RWMutex
	tipHash   [32]byte
	tipHeight uint64
	tipWork   uint64
	tipFound  bool

	txIndexMu    sync.RWMutex
	txIndexBuilt bool
}

// decodeBlock is the single seam for reading a stored block. It accepts either
// the current binary format or the legacy JSON one, so an upgraded node reads
// its existing database in place with no resync.
func decodeBlock(data []byte) (*Block, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("cannot decode empty block record")
	}

	switch data[0] {
	case blockFormatBinaryV1:
		return decodeBlockBinaryV1(data)
	case blockFormatJSONMark:
		block := &Block{}
		if err := json.Unmarshal(data, block); err != nil {
			return nil, err
		}
		return block, nil
	default:
		return nil, fmt.Errorf("unknown stored block format tag 0x%02x", data[0])
	}
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
		NoSync:  false, // Ensure durability
		Timeout: chainDBLockTimeout,
	})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, fmt.Errorf("another blocknet process is already using %s; stop it first", dataDir)
		}
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketBlocks, bucketHeights, bucketOutputs, bucketKeyImages, bucketMeta, bucketTxIndex, bucketCanonicalRing} {
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

	s := &Storage{db: db}
	if err := s.loadTipCache(); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("failed to load tip metadata: %w (additionally failed to close db: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("failed to load tip metadata: %w", err)
	}

	if err := s.backfillTxIndex(); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("failed to build transaction index: %w (additionally failed to close db: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("failed to build transaction index: %w", err)
	}

	if err := s.ensureTransactionLocations(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize transaction lookup index: %w", err)
	}
	return s, nil
}

// loadTipCache reads the persisted tip into the in-memory cache. Called once at
// open; afterwards the cache is refreshed by every path that writes the tip.
func (s *Storage) loadTipCache() error {
	var hash [32]byte
	var height, work uint64
	var found bool

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
		return err
	}

	s.setTipCache(hash, height, work, found)
	return nil
}

func (s *Storage) setTipCache(hash [32]byte, height, work uint64, found bool) {
	s.tipMu.Lock()
	s.tipHash = hash
	s.tipHeight = height
	s.tipWork = work
	s.tipFound = found
	s.tipMu.Unlock()
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
	data, err := encodeBlock(block)
	if err != nil {
		return err
	}
	locations, err := transactionLocations(block)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		if block.Header.Height > 0 && blocks.Get(block.Header.PrevHash[:]) == nil {
			return fmt.Errorf("block %x at height %d has missing parent %x", hash[:8], block.Header.Height, block.Header.PrevHash[:8])
		}
		if err := blocks.Put(hash[:], data); err != nil {
			return err
		}
		return putTransactionLocations(tx, locations)
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
		decoded, err := decodeBlock(data)
		if err != nil {
			return err
		}
		block = decoded
		return nil
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
		if err := invalidateCanonicalRingIndexLocked(tx); err != nil {
			return err
		}
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

func decodeBlockHeader(data []byte) (BlockHeader, error) {
	if len(data) > 0 && data[0] == blockFormatBinaryV1 {
		if len(data) < 1+blockHeaderSerializedSize {
			return BlockHeader{}, fmt.Errorf("stored block too short: %d bytes", len(data))
		}
		return deserializeBlockHeader(data[1 : 1+blockHeaderSerializedSize])
	}
	block, err := decodeBlock(data)
	if err != nil {
		return BlockHeader{}, err
	}
	return block.Header, nil
}

func (s *Storage) GetAncestorHeaders(hash [32]byte, n int) (map[[32]byte]*Block, error) {
	out := make(map[[32]byte]*Block, n)
	err := s.db.View(func(tx *bolt.Tx) error {
		stored := tx.Bucket(bucketBlocks)
		for range n {
			data := stored.Get(hash[:])
			if data == nil {
				return nil
			}
			header, err := decodeBlockHeader(data)
			if err != nil {
				return err
			}
			out[hash] = &Block{Header: header}
			if header.Height == 0 {
				return nil
			}
			hash = header.PrevHash
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Storage) GetMainChainHeaders(startHeight uint64, max int) ([]BlockHeader, error) {
	if max <= 0 {
		return nil, nil
	}
	headers := make([]BlockHeader, 0, max)
	err := s.db.View(func(tx *bolt.Tx) error {
		heights := tx.Bucket(bucketHeights)
		stored := tx.Bucket(bucketBlocks)
		for i := 0; i < max; i++ {
			hash := heights.Get(heightKey(startHeight + uint64(i)))
			if hash == nil {
				return nil
			}
			data := stored.Get(hash)
			if data == nil {
				return nil
			}
			header, err := decodeBlockHeader(data)
			if err != nil {
				log.Printf("storage: undecodable block at height %d: %v", startHeight+uint64(i), err)
				return nil
			}
			headers = append(headers, header)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return headers, nil
}

func (s *Storage) GetMainChainBlocks(startHeight uint64, max int) ([]*Block, error) {
	if max <= 0 {
		return nil, nil
	}
	blocks := make([]*Block, 0, max)
	err := s.db.View(func(tx *bolt.Tx) error {
		heights := tx.Bucket(bucketHeights)
		stored := tx.Bucket(bucketBlocks)
		for i := 0; i < max; i++ {
			hash := heights.Get(heightKey(startHeight + uint64(i)))
			if hash == nil {
				return nil
			}
			data := stored.Get(hash)
			if data == nil {
				return nil
			}
			block, err := decodeBlock(data)
			if err != nil {
				log.Printf("storage: undecodable block at height %d: %v", startHeight+uint64(i), err)
				return nil
			}
			blocks = append(blocks, block)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}

// RemoveMainChainBlock removes a height from main chain index (for reorgs)
func (s *Storage) RemoveMainChainBlock(height uint64) error {
	key := heightKey(height)

	return s.db.Update(func(tx *bolt.Tx) error {
		if err := invalidateCanonicalRingIndexLocked(tx); err != nil {
			return err
		}
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
	data, err := encodeUTXO(output)
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
		decoded, err := decodeUTXO(data)
		if err != nil {
			return err
		}
		output = decoded
		return nil
	})

	return output, err
}

// GetAllOutputs returns all outputs for ring member selection
func (s *Storage) GetAllOutputs() ([]*UTXO, error) {
	var outputs []*UTXO

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOutputs)
		return b.ForEach(func(k, v []byte) error {
			output, err := decodeUTXO(v)
			if err != nil {
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

// GetTip returns the best block hash and height. Served from the in-memory tip
// cache: it is called once per ring member during transaction validation, so a
// bbolt read transaction per call is far too expensive.
func (s *Storage) GetTip() (hash [32]byte, height uint64, work uint64, found bool) {
	s.tipMu.RLock()
	defer s.tipMu.RUnlock()
	return s.tipHash, s.tipHeight, s.tipWork, s.tipFound
}

// SetTip updates the chain tip
func (s *Storage) SetTip(hash [32]byte, height, work uint64) error {
	heightBytes := make([]byte, 8)
	workBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, height)
	binary.BigEndian.PutUint64(workBytes, work)

	if err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := invalidateCanonicalRingIndexLocked(tx); err != nil {
			return err
		}
		if err := meta.Put(metaKeyTip, hash[:]); err != nil {
			return err
		}
		if err := meta.Put(metaKeyHeight, heightBytes); err != nil {
			return err
		}
		return meta.Put(metaKeyWork, workBytes)
	}); err != nil {
		return err
	}

	s.setTipCache(hash, height, work, true)
	return nil
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

	blockData, err := encodeBlock(commit.Block)
	if err != nil {
		return err
	}
	locations, err := transactionLocations(commit.Block)
	if err != nil {
		return err
	}

	type serializedOutput struct {
		key  []byte
		data []byte
	}
	var outputEntries []serializedOutput
	if commit.IsMainTip {
		for _, out := range commit.NewOutputs {
			data, err := encodeUTXO(out)
			if err != nil {
				return err
			}
			outputEntries = append(outputEntries, serializedOutput{
				key:  outpointKey(out.TxID, out.OutputIndex),
				data: data,
			})
		}
	}

	heightBytes := heightKey(commit.Height)

	if err := s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		heights := tx.Bucket(bucketHeights)
		outputs := tx.Bucket(bucketOutputs)
		keyImages := tx.Bucket(bucketKeyImages)
		meta := tx.Bucket(bucketMeta)

		if commit.Block.Header.Height > 0 && blocks.Get(commit.Block.Header.PrevHash[:]) == nil {
			return fmt.Errorf("main-chain commit block parent missing: height=%d prev=%x", commit.Block.Header.Height, commit.Block.Header.PrevHash[:8])
		}

		var prevTip [32]byte
		var prevTipFound bool
		if commit.IsMainTip {
			tipHash, tipHeight, found, err := readTipMeta(meta)
			if err != nil {
				return fmt.Errorf("invalid tip metadata: %w", err)
			}
			prevTip, prevTipFound = tipHash, found

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

		if err := blocks.Put(commit.Hash[:], blockData); err != nil {
			return err
		}
		if err := putTransactionLocations(tx, locations); err != nil {
			return err
		}

		if commit.IsMainTip {
			if err := heights.Put(heightBytes, commit.Hash[:]); err != nil {
				return err
			}

			if err := indexBlockTxsLocked(tx.Bucket(bucketTxIndex), commit.Block, heightBytes); err != nil {
				return err
			}

			for _, entry := range outputEntries {
				if err := outputs.Put(entry.key, entry.data); err != nil {
					return err
				}
			}

			for _, ki := range commit.SpentKeyImgs {
				if err := keyImages.Put(ki[:], heightBytes); err != nil {
					return err
				}
			}

			workBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(workBytes, commit.Work)

			if err := applyCanonicalRingChangesLocked(tx, prevTip, prevTipFound, nil, []*Block{commit.Block}, commit.Hash); err != nil {
				return fmt.Errorf("update canonical ring index: %w", err)
			}

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
	}); err != nil {
		return err
	}

	if commit.IsMainTip {
		s.setTipCache(commit.Hash, commit.Height, commit.Work, true)
	}
	return nil
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

	type serializedOutput struct {
		key  []byte
		data []byte
	}
	type serializedBlock struct {
		hash      [32]byte
		height    uint64
		data      []byte
		outputs   []serializedOutput
		locations []txLocationEntry
	}

	prepared := make([]serializedBlock, len(commit.Connect))
	for i, block := range commit.Connect {
		blockData, err := encodeBlock(block)
		if err != nil {
			return fmt.Errorf("failed to marshal block: %w", err)
		}
		sb := serializedBlock{
			hash:   block.Hash(),
			height: block.Header.Height,
			data:   blockData,
		}
		sb.locations, err = transactionLocations(block)
		if err != nil {
			return err
		}
		for _, txn := range block.Transactions {
			txid, _ := txn.TxID()
			for idx, out := range txn.Outputs {
				newOutput := &UTXO{
					TxID:        txid,
					OutputIndex: uint32(idx),
					Output:      out,
					BlockHeight: block.Header.Height,
				}
				data, err := encodeUTXO(newOutput)
				if err != nil {
					return fmt.Errorf("failed to marshal output: %w", err)
				}
				sb.outputs = append(sb.outputs, serializedOutput{
					key:  outpointKey(txid, uint32(idx)),
					data: data,
				})
			}
		}
		prepared[i] = sb
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket(bucketBlocks)
		heights := tx.Bucket(bucketHeights)
		outputs := tx.Bucket(bucketOutputs)
		keyImages := tx.Bucket(bucketKeyImages)
		meta := tx.Bucket(bucketMeta)
		txIndex := tx.Bucket(bucketTxIndex)

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

			if err := unindexBlockTxsLocked(txIndex, block); err != nil {
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

		// Connect new blocks (forward order) using pre-serialized data
		for i, sb := range prepared {
			block := commit.Connect[i]
			hKey := heightKey(sb.height)

			if err := blocks.Put(sb.hash[:], sb.data); err != nil {
				return fmt.Errorf("failed to save block: %w", err)
			}
			if err := putTransactionLocations(tx, sb.locations); err != nil {
				return err
			}

			if err := heights.Put(hKey, sb.hash[:]); err != nil {
				return err
			}

			if err := indexBlockTxsLocked(txIndex, block, hKey); err != nil {
				return err
			}

			for _, entry := range sb.outputs {
				if err := outputs.Put(entry.key, entry.data); err != nil {
					return err
				}
			}

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

		if err := applyCanonicalRingChangesLocked(tx, currentTip, true, commit.Disconnect, commit.Connect, commit.NewTip); err != nil {
			return fmt.Errorf("update canonical ring index: %w", err)
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
	}); err != nil {
		return err
	}

	s.setTipCache(commit.NewTip, commit.NewHeight, commit.NewWork, true)
	return nil
}

// TruncateCommit removes main-chain blocks above a height and repoints the tip.
type TruncateCommit struct {
	// Disconnect are the blocks being removed; their key images and index
	// entries are dropped.
	Disconnect []*Block
	// Heights are every main-chain height to unindex, including heights whose
	// block data is already missing.
	Heights   []uint64
	NewTip    [32]byte
	NewHeight uint64
	NewWork   uint64
}

// CommitTruncate applies a truncation in one write transaction. Doing it per
// height meant a separate bbolt commit — and fsync — for every height and every
// key image, with the chain lock held throughout.
func (s *Storage) CommitTruncate(commit *TruncateCommit) error {
	if commit == nil {
		return fmt.Errorf("nil truncate commit")
	}

	heightBytes := make([]byte, 8)
	workBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, commit.NewHeight)
	binary.BigEndian.PutUint64(workBytes, commit.NewWork)

	if err := s.db.Update(func(tx *bolt.Tx) error {
		heights := tx.Bucket(bucketHeights)
		keyImages := tx.Bucket(bucketKeyImages)
		meta := tx.Bucket(bucketMeta)
		txIndex := tx.Bucket(bucketTxIndex)

		if tx.Bucket(bucketBlocks).Get(commit.NewTip[:]) == nil {
			return fmt.Errorf("truncate target block not found: %x", commit.NewTip[:8])
		}

		// Truncation can drop heights whose block data is missing, so the
		// persisted ring index cannot be updated exactly; the next start
		// rebuilds it.
		if err := invalidateCanonicalRingIndexLocked(tx); err != nil {
			return err
		}

		for _, height := range commit.Heights {
			if err := heights.Delete(heightKey(height)); err != nil {
				return err
			}
		}

		for _, block := range commit.Disconnect {
			if block == nil {
				continue
			}
			if err := unindexBlockTxsLocked(txIndex, block); err != nil {
				return err
			}
			for _, txn := range block.Transactions {
				if txn.IsCoinbase() {
					continue
				}
				for _, input := range txn.Inputs {
					if err := keyImages.Delete(input.KeyImage[:]); err != nil {
						return err
					}
				}
			}
		}

		if err := meta.Put(metaKeyTip, commit.NewTip[:]); err != nil {
			return err
		}
		if err := meta.Put(metaKeyHeight, heightBytes); err != nil {
			return err
		}
		return meta.Put(metaKeyWork, workBytes)
	}); err != nil {
		return err
	}

	s.setTipCache(commit.NewTip, commit.NewHeight, commit.NewWork, true)
	return nil
}

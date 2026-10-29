package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"

	bolt "go.etcd.io/bbolt"
)

var bucketTxLocations = []byte("tx_locations_v1")
var metaTxLocationsReady = []byte("tx_locations_v1_ready")

type txLocationEntry struct {
	key   [64]byte // transaction ID + containing block hash (includes side chains)
	value [12]byte // height + position in the block
}

func transactionLocations(block *Block) ([]txLocationEntry, error) {
	hash := block.Hash()
	entries := make([]txLocationEntry, 0, len(block.Transactions))
	seen := make(map[[32]byte]struct{}, len(block.Transactions))
	for position, txn := range block.Transactions {
		if txn == nil {
			return nil, fmt.Errorf("index transaction %d: nil transaction", position)
		}
		id, err := txn.TxID()
		if err != nil {
			return nil, fmt.Errorf("index transaction %d: %w", position, err)
		}
		if _, duplicate := seen[id]; duplicate {
			continue // Preserve the first matching transaction in a block.
		}
		seen[id] = struct{}{}
		entry := txLocationEntry{}
		copy(entry.key[:32], id[:])
		copy(entry.key[32:], hash[:])
		binary.BigEndian.PutUint64(entry.value[:8], block.Header.Height)
		binary.BigEndian.PutUint32(entry.value[8:], uint32(position))
		entries = append(entries, entry)
	}
	return entries, nil
}

func putTransactionLocations(tx *bolt.Tx, entries []txLocationEntry) error {
	bucket := tx.Bucket(bucketTxLocations)
	for _, entry := range entries {
		if err := bucket.Put(entry.key[:], entry.value[:]); err != nil {
			return err
		}
	}
	return nil
}

// Build once before serving requests. All subsequent block writes maintain the
// index atomically. Canonical membership is checked against the height index
// in the reader's transaction, so reorgs never require deleting fork entries.
func (s *Storage) ensureTransactionLocations() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketTxLocations) != nil && bytes.Equal(tx.Bucket(bucketMeta).Get(metaTxLocationsReady), []byte{1}) {
			return nil
		}
		if _, err := tx.CreateBucketIfNotExists(bucketTxLocations); err != nil {
			return err
		}
		log.Printf("building transaction lookup index")
		var count uint64
		if err := tx.Bucket(bucketBlocks).ForEach(func(key, data []byte) error {
			block, err := decodeBlock(data)
			if err != nil {
				return fmt.Errorf("index block %x: %w", key, err)
			}
			entries, err := transactionLocations(block)
			if err != nil {
				return err
			}
			if err := putTransactionLocations(tx, entries); err != nil {
				return err
			}
			count += uint64(len(entries))
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put(metaTxLocationsReady, []byte{1}); err != nil {
			return err
		}
		log.Printf("transaction lookup index ready: %d locations", count)
		return nil
	})
}

func canonicalTransactionLocation(tx *bolt.Tx, id [32]byte) (hash [32]byte, height uint64, position uint32, found bool, err error) {
	cursor := tx.Bucket(bucketTxLocations).Cursor()
	for key, value := cursor.Seek(id[:]); key != nil && bytes.HasPrefix(key, id[:]); key, value = cursor.Next() {
		if len(key) != 64 || len(value) != 12 {
			return hash, 0, 0, false, fmt.Errorf("invalid transaction location entry")
		}
		candidateHeight := binary.BigEndian.Uint64(value[:8])
		if !bytes.Equal(tx.Bucket(bucketHeights).Get(heightKey(candidateHeight)), key[32:]) ||
			tx.Bucket(bucketBlocks).Get(key[32:]) == nil {
			continue
		}
		if !found || candidateHeight > height {
			copy(hash[:], key[32:])
			height = candidateHeight
			position = binary.BigEndian.Uint32(value[8:])
			found = true
		}
	}
	return
}

func (s *Storage) FindCanonicalTransaction(id [32]byte) (*Transaction, uint64, error) {
	var result *Transaction
	var height uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		hash, at, position, found, err := canonicalTransactionLocation(tx, id)
		if err != nil || !found {
			return err
		}
		block, err := decodeBlock(tx.Bucket(bucketBlocks).Get(hash[:]))
		if err != nil {
			return err
		}
		if uint64(position) >= uint64(len(block.Transactions)) {
			return fmt.Errorf("transaction location outside block")
		}
		actualID, err := block.Transactions[position].TxID()
		if err != nil || actualID != id {
			return fmt.Errorf("transaction location does not match indexed ID")
		}
		result, height = block.Transactions[position], at
		return nil
	})
	return result, height, err
}

func (s *Storage) CanonicalTransactionHeights(ids [][32]byte) (map[[32]byte]uint64, error) {
	result := make(map[[32]byte]uint64, len(ids))
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, id := range ids {
			_, height, _, found, err := canonicalTransactionLocation(tx, id)
			if err != nil {
				return err
			}
			if found {
				result[id] = height
			}
		}
		return nil
	})
	return result, err
}

func (s *Storage) CanonicalBlockHashes(heights []uint64) (map[uint64][32]byte, uint64, error) {
	result := make(map[uint64][32]byte, len(heights))
	var tipHeight uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		_, height, _, err := readTipMeta(tx.Bucket(bucketMeta))
		if err != nil {
			return err
		}
		tipHeight = height
		for _, at := range heights {
			data := tx.Bucket(bucketHeights).Get(heightKey(at))
			if len(data) == 32 && at <= tipHeight {
				var hash [32]byte
				copy(hash[:], data)
				result[at] = hash
			}
		}
		return nil
	})
	return result, tipHeight, err
}

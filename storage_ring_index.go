package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// The canonical ring-member index (output public key + commitment → first
// canonical height) used to take a full-chain block decode on every start,
// over 20 s at height ~70k. It is now persisted beside the chain and kept in
// step inside the same bbolt transactions that move the canonical tip, so a
// restart only loads it.
//
// metaCanonicalRingTip records the tip the persisted index reflects. Writes
// that move the tip any other way (truncation, manual height edits) clear it,
// and a missing or stale marker makes startup rebuild from blocks as before.
var (
	bucketCanonicalRing  = []byte("canonical_ring_v1")
	metaCanonicalRingTip = []byte("canonical_ring_v1_tip")
)

const canonicalRingKeySize = 64

// canonicalRingIndexValidLocked reports whether the persisted index reflects
// prevTip, the canonical tip before the transaction's changes. An empty chain
// is valid while the bucket is empty.
func canonicalRingIndexValidLocked(tx *bolt.Tx, prevTip [32]byte, prevTipFound bool) bool {
	bucket := tx.Bucket(bucketCanonicalRing)
	if bucket == nil {
		return false
	}
	marker := tx.Bucket(bucketMeta).Get(metaCanonicalRingTip)
	if !prevTipFound {
		key, _ := bucket.Cursor().First()
		return marker == nil && key == nil
	}
	return bytes.Equal(marker, prevTip[:])
}

// applyCanonicalRingChangesLocked mirrors updateCanonicalRingIndexForConnect
// and ...ForDisconnect: disconnected outputs are deleted, connected outputs
// keep the first height they were seen at. When the persisted index does not
// reflect prevTip it is left invalid (marker cleared) for a startup rebuild.
func applyCanonicalRingChangesLocked(tx *bolt.Tx, prevTip [32]byte, prevTipFound bool, disconnect, connect []*Block, newTip [32]byte) error {
	meta := tx.Bucket(bucketMeta)
	if !canonicalRingIndexValidLocked(tx, prevTip, prevTipFound) {
		return meta.Delete(metaCanonicalRingTip)
	}
	bucket := tx.Bucket(bucketCanonicalRing)
	for _, block := range disconnect {
		for _, txn := range block.Transactions {
			for _, out := range txn.Outputs {
				key := canonicalRingIndexKey(out.PublicKey, out.Commitment)
				if err := bucket.Delete(key[:]); err != nil {
					return err
				}
			}
		}
	}
	for _, block := range connect {
		heightBytes := heightKey(block.Header.Height)
		for _, txn := range block.Transactions {
			for _, out := range txn.Outputs {
				key := canonicalRingIndexKey(out.PublicKey, out.Commitment)
				if bucket.Get(key[:]) != nil {
					continue
				}
				if err := bucket.Put(key[:], heightBytes); err != nil {
					return err
				}
			}
		}
	}
	return meta.Put(metaCanonicalRingTip, newTip[:])
}

// invalidateCanonicalRingIndexLocked marks the persisted index stale; the
// next start rebuilds it from blocks.
func invalidateCanonicalRingIndexLocked(tx *bolt.Tx) error {
	return tx.Bucket(bucketMeta).Delete(metaCanonicalRingTip)
}

// LoadCanonicalRingIndex returns the persisted index when it reflects the
// stored tip. ok is false when it must be rebuilt.
func (s *Storage) LoadCanonicalRingIndex() (heights map[[64]byte]uint64, tip [32]byte, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		tipHash, _, found, err := readTipMeta(meta)
		if err != nil || !found {
			return err
		}
		if !canonicalRingIndexValidLocked(tx, tipHash, true) {
			return nil
		}
		bucket := tx.Bucket(bucketCanonicalRing)
		heights = make(map[[64]byte]uint64, bucket.Stats().KeyN)
		if err := bucket.ForEach(func(k, v []byte) error {
			if len(k) != canonicalRingKeySize || len(v) != 8 {
				return fmt.Errorf("invalid canonical ring index entry (key %d bytes, value %d bytes)", len(k), len(v))
			}
			var key [64]byte
			copy(key[:], k)
			heights[key] = binary.BigEndian.Uint64(v)
			return nil
		}); err != nil {
			return err
		}
		tip = tipHash
		ok = true
		return nil
	})
	if err != nil || !ok {
		return nil, [32]byte{}, false, err
	}
	return heights, tip, true, nil
}

// SaveCanonicalRingIndex replaces the persisted index with a freshly rebuilt
// one for tip. The marker is only set if tip is still the stored tip.
func (s *Storage) SaveCanonicalRingIndex(heights map[[64]byte]uint64, tip [32]byte) error {
	keys := make([][64]byte, 0, len(heights))
	for key := range heights {
		keys = append(keys, key)
	}
	// Sorted inserts keep bbolt pages dense and the write fast.
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })

	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := meta.Delete(metaCanonicalRingTip); err != nil {
			return err
		}
		if err := tx.DeleteBucket(bucketCanonicalRing); err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		bucket, err := tx.CreateBucket(bucketCanonicalRing)
		if err != nil {
			return err
		}
		bucket.FillPercent = 1.0
		// bbolt keeps references to keys and values until commit.
		values := make([]byte, 8*len(keys))
		for i := range keys {
			value := values[8*i : 8*i+8]
			binary.BigEndian.PutUint64(value, heights[keys[i]])
			if err := bucket.Put(keys[i][:], value); err != nil {
				return err
			}
		}
		storedTip, _, found, err := readTipMeta(meta)
		if err != nil {
			return err
		}
		if !found || storedTip != tip {
			return nil
		}
		return meta.Put(metaCanonicalRingTip, tip[:])
	})
}

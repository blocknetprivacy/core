package main

import (
	"encoding/json"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func indexedTestBlock(height uint64, parent [32]byte, marker byte) *Block {
	return &Block{Header: BlockHeader{Height: height, PrevHash: parent, Nonce: uint64(marker), Difficulty: 1},
		Transactions: []*Transaction{{Version: 1, TxPublicKey: [32]byte{marker}}}}
}

func TestTransactionLocationsCanonicalReorgAndBackfill(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if storage != nil {
			storage.Close()
		}
	}()
	genesis := indexedTestBlock(0, [32]byte{}, 1)
	first := indexedTestBlock(1, genesis.Hash(), 2)
	second := indexedTestBlock(2, first.Hash(), 3)
	fork := indexedTestBlock(1, genesis.Hash(), 4)
	for _, block := range []*Block{genesis, first, second} {
		if err := storage.CommitBlock(&BlockCommit{Block: block, Hash: block.Hash(), Height: block.Header.Height, Work: block.Header.Height + 1, IsMainTip: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.SaveBlock(fork); err != nil {
		t.Fatal(err)
	}
	check := func(block *Block, want bool) {
		t.Helper()
		id, _ := block.Transactions[0].TxID()
		got, height, err := storage.FindCanonicalTransaction(id)
		if err != nil || (got != nil) != want || (want && height != block.Header.Height) {
			t.Fatalf("lookup marker %d: got=%v height=%d err=%v want=%v", block.Header.Nonce, got, height, err, want)
		}
		heights, err := storage.CanonicalTransactionHeights([][32]byte{id})
		_, found := heights[id]
		if err != nil || found != want {
			t.Fatalf("batch lookup: %v %v", heights, err)
		}
	}
	check(first, true)
	check(second, true)
	check(fork, false)
	invalid := indexedTestBlock(3, genesis.Hash(), 9)
	if err := storage.CommitBlock(&BlockCommit{Block: invalid, Hash: invalid.Hash(), Height: 3, Work: 4, IsMainTip: true}); err == nil {
		t.Fatal("invalid parent committed")
	}
	check(invalid, false)
	if err := storage.CommitReorg(&ReorgCommit{Disconnect: []*Block{second, first}, Connect: []*Block{fork}, NewTip: fork.Hash(), NewHeight: 1, NewWork: 4}); err != nil {
		t.Fatal(err)
	}
	check(first, false)
	check(second, false)
	check(fork, true)
	hashes, tip, err := storage.CanonicalBlockHashes([]uint64{0, 1, 2, 100})
	if err != nil || tip != 1 || len(hashes) != 2 || hashes[1] != fork.Hash() {
		t.Fatalf("batch hashes: %v tip=%d err=%v", hashes, tip, err)
	}
	// A database upgraded from the previous release has both JSON and binary
	// records. Rebuilding the index must decode both formats.
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(first)
		if err != nil {
			return err
		}
		hash := first.Hash()
		return tx.Bucket(bucketBlocks).Put(hash[:], data)
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate upgrading an old database, including a stale ready marker with
	// a missing derived bucket. Reopening must rebuild before serving reads.
	if err := storage.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(bucketTxLocations) }); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage = nil
	storage, err = NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	check(first, false)
	check(second, false)
	check(fork, true)
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage = nil
	storage, err = NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	check(fork, true)
}

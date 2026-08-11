package main

import (
	"encoding/binary"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func persistedTipForTest(t *testing.T, s *Storage) (hash [32]byte, height, work uint64, found bool) {
	t.Helper()

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
		t.Fatalf("failed to read persisted tip: %v", err)
	}
	return
}

func assertTipCacheMatchesDisk(t *testing.T, s *Storage, after string) {
	t.Helper()

	wantHash, wantHeight, wantWork, wantFound := persistedTipForTest(t, s)
	gotHash, gotHeight, gotWork, gotFound := s.GetTip()

	if gotFound != wantFound || gotHash != wantHash || gotHeight != wantHeight || gotWork != wantWork {
		t.Fatalf("tip cache diverged from disk after %s: cache=(%x,%d,%d,%t) disk=(%x,%d,%d,%t)",
			after, gotHash[:8], gotHeight, gotWork, gotFound, wantHash[:8], wantHeight, wantWork, wantFound)
	}
}

// Every path that writes tip metadata must refresh the in-memory cache that
// GetTip now serves. A new tip-write path that forgets to do so fails here.
func TestStorageTipCacheTracksEveryTipWritePath(t *testing.T) {
	dataDir := t.TempDir()

	storage, err := NewStorage(dataDir)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}

	assertTipCacheMatchesDisk(t, storage, "open on empty chain")

	genesis, err := GetGenesisBlock()
	if err != nil {
		t.Fatalf("failed to load genesis block: %v", err)
	}
	genesisHash := genesis.Hash()
	if err := storage.CommitBlock(&BlockCommit{
		Block:      genesis,
		Height:     0,
		Hash:       genesisHash,
		Work:       1,
		IsMainTip:  true,
		NewOutputs: blockOutputsForCommit(t, genesis),
	}); err != nil {
		t.Fatalf("failed to commit genesis: %v", err)
	}
	assertTipCacheMatchesDisk(t, storage, "CommitBlock")

	pub, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate output key: %v", err)
	}
	blockA := makeOutputOnlyTestBlock(1, genesisHash, genesis.Header.Timestamp+BlockIntervalSec, []TxOutput{
		{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
	})
	if err := storage.CommitBlock(&BlockCommit{
		Block:      blockA,
		Height:     1,
		Hash:       blockA.Hash(),
		Work:       2,
		IsMainTip:  true,
		NewOutputs: blockOutputsForCommit(t, blockA),
	}); err != nil {
		t.Fatalf("failed to commit blockA: %v", err)
	}

	blockB := makeOutputOnlyTestBlock(1, genesisHash, genesis.Header.Timestamp+BlockIntervalSec+1, []TxOutput{
		{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
	})
	if err := storage.CommitReorg(&ReorgCommit{
		Disconnect: []*Block{blockA},
		Connect:    []*Block{blockB},
		NewTip:     blockB.Hash(),
		NewHeight:  1,
		NewWork:    3,
	}); err != nil {
		t.Fatalf("failed to reorg to blockB: %v", err)
	}
	assertTipCacheMatchesDisk(t, storage, "CommitReorg")

	if err := storage.SetTip(genesisHash, 0, 1); err != nil {
		t.Fatalf("failed to set tip: %v", err)
	}
	assertTipCacheMatchesDisk(t, storage, "SetTip")

	if err := storage.Close(); err != nil {
		t.Fatalf("failed to close storage: %v", err)
	}

	reopened, err := NewStorage(dataDir)
	if err != nil {
		t.Fatalf("failed to reopen storage: %v", err)
	}
	defer reopened.Close()
	assertTipCacheMatchesDisk(t, reopened, "reopen")
}

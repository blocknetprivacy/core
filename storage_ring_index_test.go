package main

import (
	"reflect"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func ringTestOutput(seed byte) TxOutput {
	var out TxOutput
	out.PublicKey[0], out.PublicKey[1] = 0xa1, seed
	out.Commitment[0], out.Commitment[1] = 0xc1, seed
	return out
}

// expectedCanonicalRingHeights rebuilds the index the slow way from the
// stored canonical chain, the reference the persisted copy must match.
func expectedCanonicalRingHeights(t *testing.T, chain *Chain) map[[64]byte]uint64 {
	t.Helper()
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.canonicalRingIndexDirty = true
	if err := chain.ensureCanonicalRingIndexLocked(); err != nil {
		t.Fatalf("rebuild canonical ring index: %v", err)
	}
	out := make(map[[64]byte]uint64, len(chain.canonicalRingHeights))
	for key, height := range chain.canonicalRingHeights {
		out[key] = height
	}
	return out
}

func mustLoadPersistedRingIndex(t *testing.T, storage *Storage) map[[64]byte]uint64 {
	t.Helper()
	heights, tip, ok, err := storage.LoadCanonicalRingIndex()
	if err != nil {
		t.Fatalf("load persisted canonical ring index: %v", err)
	}
	if !ok {
		t.Fatal("persisted canonical ring index should be valid for the stored tip")
	}
	storedTip, _, _, _ := storage.GetTip()
	if tip != storedTip {
		t.Fatalf("persisted index tip %x, stored tip %x", tip[:8], storedTip[:8])
	}
	return heights
}

// buildRingIndexTestChain commits genesis plus count blocks with two outputs
// each through the production CommitBlock path.
func buildRingIndexTestChain(t *testing.T, dataDir string, count int) (*Chain, []*Block) {
	t.Helper()
	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)
	storage := chain.Storage()
	tip, _, work, _ := storage.GetTip()
	timestamp := GenesisTimestamp
	var blocks []*Block
	for h := 1; h <= count; h++ {
		timestamp += BlockIntervalSec
		block := makeOutputOnlyTestBlock(uint64(h), tip, timestamp, []TxOutput{
			ringTestOutput(byte(2 * h)),
			ringTestOutput(byte(2*h + 1)),
		})
		work += MinDifficulty
		commitMainChainBlockForTest(t, chain, storage, block, work)
		tip = block.Hash()
		blocks = append(blocks, block)
	}
	return chain, blocks
}

func TestCanonicalRingIndexPersistsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	chain, _ := buildRingIndexTestChain(t, dataDir, 3)
	want := expectedCanonicalRingHeights(t, chain)
	if got := mustLoadPersistedRingIndex(t, chain.Storage()); !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted index diverged from rebuild:\n got %v\nwant %v", got, want)
	}
	if err := chain.Close(); err != nil {
		t.Fatalf("close chain: %v", err)
	}

	reopened, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("reopen chain: %v", err)
	}
	defer reopened.Close()
	if !reopened.canonicalRingMemberIndexUsableRLocked() {
		t.Fatal("reopened chain should have a usable ring index")
	}
	if !reflect.DeepEqual(reopened.canonicalRingHeights, want) {
		t.Fatalf("loaded index diverged from rebuild:\n got %v\nwant %v", reopened.canonicalRingHeights, want)
	}
	out := ringTestOutput(4)
	if !reopened.IsCanonicalRingMember(out.PublicKey, out.Commitment) {
		t.Fatal("output from height 2 should be a canonical ring member after restart")
	}
}

func TestCanonicalRingIndexRebuildsAndPersistsForOldDataDir(t *testing.T) {
	dataDir := t.TempDir()
	chain, _ := buildRingIndexTestChain(t, dataDir, 3)
	want := expectedCanonicalRingHeights(t, chain)
	// A data dir written before the index was persisted has neither the
	// bucket nor the marker.
	if err := chain.Storage().db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(bucketCanonicalRing); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).Delete(metaCanonicalRingTip)
	}); err != nil {
		t.Fatalf("simulate old data dir: %v", err)
	}
	if err := chain.Close(); err != nil {
		t.Fatalf("close chain: %v", err)
	}

	reopened, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("reopen old data dir: %v", err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(reopened.canonicalRingHeights, want) {
		t.Fatalf("rebuilt index wrong:\n got %v\nwant %v", reopened.canonicalRingHeights, want)
	}
	if got := mustLoadPersistedRingIndex(t, reopened.Storage()); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuild was not persisted:\n got %v\nwant %v", got, want)
	}
}

func TestCanonicalRingIndexFollowsReorg(t *testing.T) {
	dataDir := t.TempDir()
	chain, blocks := buildRingIndexTestChain(t, dataDir, 3)
	defer chain.Close()
	storage := chain.Storage()
	_, _, work, _ := storage.GetTip()

	// Replace heights 2 and 3 with a heavier two-block branch whose first
	// block re-uses one output of the disconnected height-3 block.
	parent := blocks[0]
	alt2 := makeOutputOnlyTestBlock(2, parent.Hash(), parent.Header.Timestamp+BlockIntervalSec+1, []TxOutput{ringTestOutput(200), ringTestOutput(7)})
	alt3 := makeOutputOnlyTestBlock(3, alt2.Hash(), alt2.Header.Timestamp+BlockIntervalSec, []TxOutput{ringTestOutput(201)})
	if err := storage.CommitReorg(&ReorgCommit{
		Disconnect: []*Block{blocks[2], blocks[1]},
		Connect:    []*Block{alt2, alt3},
		NewTip:     alt3.Hash(),
		NewHeight:  3,
		NewWork:    work + 1,
	}); err != nil {
		t.Fatalf("commit reorg: %v", err)
	}

	got := mustLoadPersistedRingIndex(t, storage)
	for _, seed := range []byte{4, 5, 6} {
		out := ringTestOutput(seed)
		if _, ok := got[canonicalRingIndexKey(out.PublicKey, out.Commitment)]; ok {
			t.Fatalf("disconnected output %d still in persisted index", seed)
		}
	}
	for seed, height := range map[byte]uint64{2: 1, 3: 1, 200: 2, 7: 2, 201: 3} {
		out := ringTestOutput(seed)
		if gotHeight, ok := got[canonicalRingIndexKey(out.PublicKey, out.Commitment)]; !ok || gotHeight != height {
			t.Fatalf("output %d: height %d present=%v, want height %d", seed, gotHeight, ok, height)
		}
	}

	// The reorged chain reopened from disk loads the same index a rebuild gives.
	chain.mu.Lock()
	chain.byHeight[2], chain.byHeight[3] = alt2.Hash(), alt3.Hash()
	chain.blocks[alt2.Hash()], chain.blocks[alt3.Hash()] = alt2, alt3
	chain.bestHash, chain.height = alt3.Hash(), 3
	chain.mu.Unlock()
	if want := expectedCanonicalRingHeights(t, chain); !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted index after reorg diverged from rebuild:\n got %v\nwant %v", got, want)
	}
}

func TestCanonicalRingIndexInvalidatedByOtherTipWrites(t *testing.T) {
	for name, rewrite := range map[string]func(*Chain, [32]byte, uint64, uint64) error{
		"set_tip": func(c *Chain, tip [32]byte, height, work uint64) error {
			return c.Storage().SetTip(tip, height, work)
		},
		"remove_main_chain_block": func(c *Chain, _ [32]byte, height, _ uint64) error {
			return c.Storage().RemoveMainChainBlock(height)
		},
		"set_main_chain_block": func(c *Chain, tip [32]byte, height, _ uint64) error {
			return c.Storage().SetMainChainBlock(height, tip)
		},
		"truncate": func(c *Chain, _ [32]byte, height, _ uint64) error {
			return c.TruncateToHeight(height - 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			chain, _ := buildRingIndexTestChain(t, t.TempDir(), 2)
			defer chain.Close()
			storage := chain.Storage()
			tip, height, work, _ := storage.GetTip()
			if err := rewrite(chain, tip, height, work); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if _, _, ok, err := storage.LoadCanonicalRingIndex(); err != nil || ok {
				t.Fatalf("persisted index should be invalid after %s (ok=%v err=%v)", name, ok, err)
			}
			// Later commits must not revalidate a stale index.
			tip, height, work, _ = storage.GetTip()
			next := makeOutputOnlyTestBlock(height+1, tip, GenesisTimestamp+int64(height+1)*BlockIntervalSec+1, []TxOutput{ringTestOutput(99)})
			if err := storage.CommitBlock(&BlockCommit{Block: next, Height: height + 1, Hash: next.Hash(), Work: work + MinDifficulty, IsMainTip: true, NewOutputs: blockOutputsForCommit(t, next)}); err != nil {
				t.Fatalf("commit after rewrite: %v", err)
			}
			if _, _, ok, _ := storage.LoadCanonicalRingIndex(); ok {
				t.Fatalf("a commit after %s revalidated the stale index", name)
			}
		})
	}
}

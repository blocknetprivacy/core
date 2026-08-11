package main

import (
	"fmt"
	"testing"
)

// The tx index must agree with the tip-backwards scan it replaces: a main-chain
// transaction resolves to its height, a reorged-out one stops resolving, and the
// replacement resolves instead.
func TestTxIndexTracksMainChainAcrossReorg(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	pubA, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate key A: %v", err)
	}
	blockA := makeOutputOnlyTestBlock(1, genesis.Hash(), genesis.Header.Timestamp+BlockIntervalSec, []TxOutput{
		{PublicKey: pubA.PublicKey, Commitment: pubA.PublicKey},
	})
	commitMainChainBlockForTest(t, chain, storage, blockA, 2)

	txA := blockA.Transactions[0]
	txAID, err := txA.TxID()
	if err != nil {
		t.Fatalf("failed to hash txA: %v", err)
	}
	txAHex := fmt.Sprintf("%x", txAID)

	if height, found := storage.GetTxHeight(txAID); !found || height != 1 {
		t.Fatalf("txA index lookup = (%d, %t), want (1, true)", height, found)
	}
	if _, height, found := chain.FindTxByHashStr(txAHex); !found || height != 1 {
		t.Fatalf("FindTxByHashStr(txA) = (%d, %t), want (1, true)", height, found)
	}

	pubB, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate key B: %v", err)
	}
	blockB := makeOutputOnlyTestBlock(1, genesis.Hash(), genesis.Header.Timestamp+BlockIntervalSec+1, []TxOutput{
		{PublicKey: pubB.PublicKey, Commitment: pubB.PublicKey},
	})
	if err := storage.CommitReorg(&ReorgCommit{
		Disconnect: []*Block{blockA},
		Connect:    []*Block{blockB},
		NewTip:     blockB.Hash(),
		NewHeight:  1,
		NewWork:    3,
	}); err != nil {
		t.Fatalf("failed to reorg from blockA to blockB: %v", err)
	}

	if _, found := storage.GetTxHeight(txAID); found {
		t.Fatal("txA still indexed after being reorged out")
	}

	txBID, err := blockB.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash txB: %v", err)
	}
	if height, found := storage.GetTxHeight(txBID); !found || height != 1 {
		t.Fatalf("txB index lookup = (%d, %t), want (1, true)", height, found)
	}
	if _, height, found := chain.FindTxByHashStr(fmt.Sprintf("%x", txBID)); !found || height != 1 {
		t.Fatalf("FindTxByHashStr(txB) = (%d, %t), want (1, true)", height, found)
	}
}

// A database written before the index exists must be backfilled at open, so an
// upgraded node answers tx lookups without a resync.
func TestTxIndexBackfillsLegacyDatabaseOnOpen(t *testing.T) {
	dataDir := t.TempDir()

	storage, err := NewStorage(dataDir)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}

	genesis, err := GetGenesisBlock()
	if err != nil {
		t.Fatalf("failed to load genesis: %v", err)
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

	pub, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate output key: %v", err)
	}
	block := makeOutputOnlyTestBlock(1, genesisHash, genesis.Header.Timestamp+BlockIntervalSec, []TxOutput{
		{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
	})
	if err := storage.CommitBlock(&BlockCommit{
		Block:      block,
		Height:     1,
		Hash:       block.Hash(),
		Work:       2,
		IsMainTip:  true,
		NewOutputs: blockOutputsForCommit(t, block),
	}); err != nil {
		t.Fatalf("failed to commit block: %v", err)
	}

	txID, err := block.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash transaction: %v", err)
	}

	mustClearTxIndexForTest(t, storage)
	if _, found := storage.GetTxHeight(txID); found {
		t.Fatal("index should be empty after simulating a pre-index database")
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("failed to close storage: %v", err)
	}

	reopened, err := NewStorage(dataDir)
	if err != nil {
		t.Fatalf("failed to reopen storage: %v", err)
	}
	defer reopened.Close()

	if !reopened.TxIndexReady() {
		t.Fatal("index not marked ready after backfill")
	}
	if height, found := reopened.GetTxHeight(txID); !found || height != 1 {
		t.Fatalf("backfilled index lookup = (%d, %t), want (1, true)", height, found)
	}
}

// walletSendChainStates now resolves through the index instead of scanning.
// Both paths must produce the same answer, or an upgraded node and a
// mid-backfill node would disagree about which sends are confirmed.
func TestFindTxHeightsAgreesBetweenIndexAndScan(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	prev := chain.GetBlockByHeight(0)
	if prev == nil {
		t.Fatal("expected genesis block")
	}

	want := make(map[[32]byte]uint64)
	batch := make(map[[32]byte]struct{})
	for height := uint64(1); height <= 3; height++ {
		pub, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate key at height %d: %v", height, err)
		}
		block := makeOutputOnlyTestBlock(height, prev.Hash(), prev.Header.Timestamp+BlockIntervalSec, []TxOutput{
			{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
		})
		commitMainChainBlockForTest(t, chain, storage, block, height+1)

		txID, err := block.Transactions[0].TxID()
		if err != nil {
			t.Fatalf("failed to hash tx at height %d: %v", height, err)
		}
		want[txID] = height
		batch[txID] = struct{}{}
		prev = block
	}

	var unknown [32]byte
	unknown[0] = 0xff
	batch[unknown] = struct{}{}

	indexed := chain.FindTxHeights(batch)
	if len(indexed) != len(want) {
		t.Fatalf("index path returned %d heights, want %d", len(indexed), len(want))
	}
	for txID, height := range want {
		if indexed[txID] != height {
			t.Fatalf("index path: tx %x at height %d, want %d", txID[:8], indexed[txID], height)
		}
	}

	mustClearTxIndexForTest(t, storage)
	scanned := chain.FindTxHeights(batch)

	if len(scanned) != len(indexed) {
		t.Fatalf("scan path returned %d heights, index path %d", len(scanned), len(indexed))
	}
	for txID, height := range indexed {
		if scanned[txID] != height {
			t.Fatalf("paths disagree for tx %x: index %d, scan %d", txID[:8], height, scanned[txID])
		}
	}
}

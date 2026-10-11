package main

import (
	"bytes"
	"encoding/json"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func richTestBlockForCodec(t *testing.T) *Block {
	t.Helper()

	genesis, err := GetGenesisBlock()
	if err != nil {
		t.Fatalf("failed to load genesis: %v", err)
	}

	return &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     7,
			PrevHash:   genesis.Hash(),
			MerkleRoot: [32]byte{0xab, 0xcd},
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
			Nonce:      123456789,
		},
		Transactions: []*Transaction{mustBuildValidRingCTBindingTestTx(t)},
	}
}

// The stored format must round-trip a block exactly. It is not consensus
// visible, but a lossy decode would silently change the block a node serves and
// the transaction ids it indexes.
func TestEncodeBlockRoundTripsExactly(t *testing.T) {
	block := richTestBlockForCodec(t)

	encoded, err := encodeBlock(block)
	if err != nil {
		t.Fatalf("failed to encode block: %v", err)
	}
	if encoded[0] != blockFormatBinaryV1 {
		t.Fatalf("encoded block starts with 0x%02x, want the binary tag", encoded[0])
	}

	decoded, err := decodeBlock(encoded)
	if err != nil {
		t.Fatalf("failed to decode block: %v", err)
	}

	if decoded.Header != block.Header {
		t.Fatalf("header changed across round trip:\n got %+v\nwant %+v", decoded.Header, block.Header)
	}
	if decoded.Hash() != block.Hash() {
		t.Fatalf("block hash changed across round trip")
	}
	if len(decoded.Transactions) != len(block.Transactions) {
		t.Fatalf("round trip returned %d transactions, want %d", len(decoded.Transactions), len(block.Transactions))
	}
	for i, tx := range block.Transactions {
		want, err := tx.TxID()
		if err != nil {
			t.Fatalf("failed to hash source tx %d: %v", i, err)
		}
		got, err := decoded.Transactions[i].TxID()
		if err != nil {
			t.Fatalf("failed to hash decoded tx %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("tx %d id changed across round trip: got %x want %x", i, got[:8], want[:8])
		}
	}

	legacy, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("failed to marshal legacy form: %v", err)
	}
	if len(encoded) >= len(legacy) {
		t.Fatalf("binary encoding is %d bytes, not smaller than JSON at %d", len(encoded), len(legacy))
	}
}

// Nodes auto-upgrade with no rollback, so a database full of JSON blocks must
// stay readable in place. Reading one back through Storage is the upgrade path.
func TestGetBlockReadsLegacyJSONRecords(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer storage.Close()

	block := richTestBlockForCodec(t)
	mustPutLegacyJSONBlockForTest(t, storage, block)

	loaded, err := storage.GetBlock(block.Hash())
	if err != nil {
		t.Fatalf("failed to read legacy block: %v", err)
	}
	if loaded == nil {
		t.Fatal("legacy block not found")
	}
	if loaded.Hash() != block.Hash() {
		t.Fatal("legacy block hash changed on read")
	}

	want, err := block.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash source tx: %v", err)
	}
	got, err := loaded.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash loaded tx: %v", err)
	}
	if got != want {
		t.Fatalf("legacy tx id changed on read: got %x want %x", got[:8], want[:8])
	}
}

// A future RingSize change must not leave a node unable to read its own
// history. Consensus keeps the strict ceiling; only the storage decoder is
// tolerant, and only for records we already validated and wrote ourselves.
func TestStoredTxDecodeToleratesRingSizeBeyondConsensus(t *testing.T) {
	const oversized = RingSize + 4

	tx := &Transaction{
		Version: 1,
		Fee:     1,
		Inputs: []TxInput{{
			RingMembers:     make([][32]byte, oversized),
			RingCommitments: make([][32]byte, oversized),
			RingSignature:   make([]byte, 96+64*oversized),
		}},
		Outputs: []TxOutput{{}},
	}
	tx.Outputs[0].EncryptedMemo[0] = 0x01
	data := tx.Serialize()

	if _, err := DeserializeTx(data); err == nil {
		t.Fatalf("consensus decoder accepted ring size %d; it must stay bounded by RingSize %d", oversized, RingSize)
	}

	stored, err := DeserializeStoredTx(data)
	if err != nil {
		t.Fatalf("storage decoder rejected its own history: %v", err)
	}
	if got := len(stored.Inputs[0].RingMembers); got != oversized {
		t.Fatalf("storage decoder returned ring size %d, want %d", got, oversized)
	}
}

func richTestOutputForCodec(t *testing.T) *UTXO {
	t.Helper()

	tx := mustBuildValidRingCTBindingTestTx(t)
	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to hash tx: %v", err)
	}
	return &UTXO{
		TxID:        txID,
		OutputIndex: 3,
		Output:      tx.Outputs[0],
		BlockHeight: 4321,
	}
}

// Stored outputs moved to the same tag-dispatched binary format as blocks. They
// are never deleted, so a lossy encode would corrupt ring selection permanently,
// and a legacy record must still read back after an upgrade.
func TestOutputCodecRoundTripsAndReadsLegacyJSON(t *testing.T) {
	output := richTestOutputForCodec(t)

	encoded, err := encodeUTXO(output)
	if err != nil {
		t.Fatalf("failed to encode output: %v", err)
	}
	if encoded[0] != utxoFormatBinaryV1 {
		t.Fatalf("encoded output starts with 0x%02x, want the binary tag", encoded[0])
	}

	decoded, err := decodeUTXO(encoded)
	if err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if decoded.TxID != output.TxID || decoded.OutputIndex != output.OutputIndex || decoded.BlockHeight != output.BlockHeight {
		t.Fatalf("output identity changed across round trip: %+v", decoded)
	}
	if decoded.Output.PublicKey != output.Output.PublicKey ||
		decoded.Output.Commitment != output.Output.Commitment ||
		decoded.Output.EncryptedAmount != output.Output.EncryptedAmount ||
		decoded.Output.EncryptedMemo != output.Output.EncryptedMemo {
		t.Fatal("output fixed fields changed across round trip")
	}
	if !bytes.Equal(decoded.Output.RangeProof, output.Output.RangeProof) {
		t.Fatal("range proof changed across round trip")
	}

	legacy, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal legacy output: %v", err)
	}
	if len(encoded) >= len(legacy) {
		t.Fatalf("binary output is %d bytes, not smaller than JSON at %d", len(encoded), len(legacy))
	}

	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer storage.Close()

	mustPutLegacyJSONOutputForTest(t, storage, output)
	loaded, err := storage.GetOutput(output.TxID, output.OutputIndex)
	if err != nil {
		t.Fatalf("failed to read legacy output: %v", err)
	}
	if loaded == nil {
		t.Fatal("legacy output not found")
	}
	if loaded.Output.PublicKey != output.Output.PublicKey || loaded.BlockHeight != output.BlockHeight {
		t.Fatal("legacy output changed on read")
	}
}

func TestMainChainRangeReadsServePrefixBeforeCorruptRecord(t *testing.T) {
	chain, _, _ := newPrecheckTestChain(t, 5)
	st := chain.Storage()
	bad, ok := st.GetBlockHashByHeight(3)
	if !ok {
		t.Fatal("missing height 3")
	}
	if err := st.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlocks).Put(bad[:], []byte{blockFormatBinaryV1, 1, 2, 3})
	}); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}

	blocks, err := st.GetMainChainBlocks(1, 5)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("GetMainChainBlocks: got %d blocks, err %v; want 2 blocks, nil", len(blocks), err)
	}
	headers, err := st.GetMainChainHeaders(1, 5)
	if err != nil || len(headers) != 2 {
		t.Fatalf("GetMainChainHeaders: got %d headers, err %v; want 2 headers, nil", len(headers), err)
	}
}

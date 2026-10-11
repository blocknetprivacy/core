package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"blocknet/protocol/params"
)

func TestJSONUnmarshalOmittedEncryptedMemoIsRejected_NonCoinbase(t *testing.T) {
	// This matches the daemon ingest reality: blocks/txs are JSON unmarshaled into
	// Go structs with fixed-size arrays; omitted fields default to zero arrays.
	//
	// We don't need a fully valid RingCT tx here because memo policy is checked
	// before expensive signature/proof verification.
	txJSON := []byte(`{
	  "version": 1,
	  "tx_public_key": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	  "fee": 0,
	  "inputs": [{}],
	  "outputs": [
	    {
	      "commitment": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	      "public_key": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	      "encrypted_amount": [0,0,0,0,0,0,0,0]
	    }
	  ]
	}`)

	var tx Transaction
	if err := json.Unmarshal(txJSON, &tx); err != nil {
		t.Fatalf("failed to unmarshal tx JSON: %v", err)
	}
	if tx.Outputs[0].EncryptedMemo != ([params.MemoSize]byte{}) {
		t.Fatal("expected omitted encrypted_memo to default to all-zero array")
	}

	err := ValidateTransaction(
		&tx,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	if err == nil {
		t.Fatal("expected tx with omitted encrypted_memo (zero default) to be rejected")
	}
	if !strings.Contains(err.Error(), "encrypted memo must not be all-zero") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestJSONUnmarshalOmittedEncryptedMemoIsRejected_Coinbase(t *testing.T) {
	txJSON := []byte(`{
	  "version": 1,
	  "tx_public_key": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	  "fee": 0,
	  "inputs": [],
	  "outputs": [
	    {
	      "commitment": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	      "public_key": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],
	      "encrypted_amount": [0,0,0,0,0,0,0,0]
	    }
	  ]
	}`)

	var tx Transaction
	if err := json.Unmarshal(txJSON, &tx); err != nil {
		t.Fatalf("failed to unmarshal coinbase-like tx JSON: %v", err)
	}

	err := ValidateTransaction(&tx, func(_ [32]byte) bool { return false }, nil)
	if err == nil {
		t.Fatal("expected coinbase with omitted encrypted_memo (zero default) to be rejected")
	}
	if !strings.Contains(err.Error(), "encrypted memo must not be all-zero") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncServeBytesMatchPlainBlockJSON(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	mustBuildChainToHeight(t, chain, storage, 40)

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	want, err := storage.GetMainChainBlocks(3, 30)
	if err != nil || len(want) != 30 {
		t.Fatalf("GetMainChainBlocks: %d blocks, err %v", len(want), err)
	}
	blocks, err := d.getBlocksByHeight(3, 30)
	if err != nil {
		t.Fatal(err)
	}
	headers, err := d.getHeaders(3, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != len(want) || len(headers) != len(want) {
		t.Fatalf("got %d blocks and %d headers, want %d", len(blocks), len(headers), len(want))
	}
	for i, b := range want {
		wantBlock, _ := json.Marshal(b)
		wantHeader, _ := json.Marshal(b.Header)
		if !bytes.Equal(blocks[i], wantBlock) {
			t.Fatalf("block %d bytes differ from plain json.Marshal", i)
		}
		if !bytes.Equal(headers[i], wantHeader) {
			t.Fatalf("header %d bytes differ from plain json.Marshal", i)
		}
	}
}

func TestMarshalBlockJSONMatchesEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	bytesOrNil := func() []byte {
		switch rng.Intn(3) {
		case 0:
			return nil
		case 1:
			return []byte{}
		}
		b := make([]byte, 1+rng.Intn(300))
		rng.Read(b)
		return b
	}
	hashes := func() [][32]byte {
		switch rng.Intn(3) {
		case 0:
			return nil
		case 1:
			return [][32]byte{}
		}
		h := make([][32]byte, 1+rng.Intn(16))
		for i := range h {
			rng.Read(h[i][:])
		}
		return h
	}
	for iter := 0; iter < 300; iter++ {
		b := &Block{Header: BlockHeader{
			Version:    rng.Uint32(),
			Height:     rng.Uint64(),
			Timestamp:  rng.Int63() - rng.Int63(),
			Difficulty: rng.Uint64(),
			Nonce:      rng.Uint64(),
		}}
		rng.Read(b.Header.PrevHash[:])
		rng.Read(b.Header.MerkleRoot[:])
		switch rng.Intn(4) {
		case 0:
			b.Transactions = nil
		case 1:
			b.Transactions = []*Transaction{}
		default:
			for n := rng.Intn(5); n >= 0; n-- {
				if rng.Intn(8) == 0 {
					b.Transactions = append(b.Transactions, nil)
					continue
				}
				tx := &Transaction{Version: uint8(rng.Intn(256)), Fee: rng.Uint64()}
				rng.Read(tx.TxPublicKey[:])
				if rng.Intn(4) > 0 {
					tx.Inputs = make([]TxInput, rng.Intn(3))
					for i := range tx.Inputs {
						in := &tx.Inputs[i]
						rng.Read(in.KeyImage[:])
						rng.Read(in.PseudoOutput[:])
						in.RingMembers = hashes()
						in.RingCommitments = hashes()
						in.RingSignature = bytesOrNil()
					}
				}
				if rng.Intn(4) > 0 {
					tx.Outputs = make([]TxOutput, rng.Intn(3))
					for i := range tx.Outputs {
						out := &tx.Outputs[i]
						rng.Read(out.Commitment[:])
						rng.Read(out.PublicKey[:])
						rng.Read(out.EncryptedAmount[:])
						rng.Read(out.EncryptedMemo[:])
						out.RangeProof = bytesOrNil()
					}
				}
				b.Transactions = append(b.Transactions, tx)
			}
		}
		want, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if got := marshalBlockJSON(b); !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: fast encoder differs\n got: %.300s\nwant: %.300s", iter, got, want)
		}
	}
}

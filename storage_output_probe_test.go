package main

import (
	"encoding/json"
	bolt "go.etcd.io/bbolt"
	"reflect"
	"testing"

	"blocknet/wallet"
)

func TestIndexedWalletOutputAuditMatchesPerOutputScan(t *testing.T) {
	chain, blocks := buildRingIndexTestChain(t, t.TempDir(), 4)
	defer chain.Close()

	// Preserve a legacy JSON record alongside newly written binary blocks.
	if err := chain.Storage().db.Update(func(tx *bolt.Tx) error {
		block := blocks[1]
		data, err := json.Marshal(block)
		if err != nil {
			return err
		}
		hash := block.Hash()
		return tx.Bucket(bucketBlocks).Put(hash[:], data)
	}); err != nil {
		t.Fatal(err)
	}

	var outputs []*wallet.OwnedOutput
	for _, block := range blocks {
		for _, tx := range block.Transactions {
			id, err := tx.TxID()
			if err != nil {
				t.Fatal(err)
			}
			for i, out := range tx.Outputs {
				outputs = append(outputs, &wallet.OwnedOutput{TxID: id, OutputIndex: i, Amount: 1, BlockHeight: block.Header.Height, OneTimePubKey: out.PublicKey, Commitment: out.Commitment})
			}
		}
	}
	valid := len(outputs)
	corrupt := func(i int, edit func(*wallet.OwnedOutput)) {
		out := *outputs[i]
		edit(&out)
		outputs = append(outputs, &out)
	}
	corrupt(0, func(o *wallet.OwnedOutput) { o.OneTimePubKey[5] ^= 1 })
	corrupt(1, func(o *wallet.OwnedOutput) { o.Commitment[5] ^= 1 })
	corrupt(2, func(o *wallet.OwnedOutput) { o.OutputIndex = 9 })
	corrupt(3, func(o *wallet.OwnedOutput) { o.TxID[5] ^= 1 })
	corrupt(4, func(o *wallet.OwnedOutput) { o.BlockHeight = 40 })
	corrupt(5, func(o *wallet.OwnedOutput) { o.BlockHeight = 1 })
	corrupt(6, func(o *wallet.OwnedOutput) { o.OutputIndex = -1 })
	outputs = append(outputs, nil)

	probes := make([]canonicalOutputProbe, valid)
	for i := 0; i < valid; i++ {
		probes[i] = canonicalOutputProbe{TxID: outputs[i].TxID, Height: outputs[i].BlockHeight, Index: outputs[i].OutputIndex}
	}
	results, err := chain.Storage().ProbeCanonicalOutputs(probes)
	if err != nil {
		t.Fatalf("probe canonical outputs: %v", err)
	}
	for i, result := range results {
		if !walletOutputMatchesProbe(outputs[i], result) {
			t.Fatalf("valid output %d not cleared by the index probe: %+v", i, result)
		}
	}

	var want []walletCanonicalOutputIssue
	for _, out := range outputs {
		if out == nil {
			continue
		}
		if issue, ok := auditWalletCanonicalOutput(chain, out); ok {
			want = append(want, issue)
		}
	}
	got := auditWalletCanonicalOutputs(chain, outputs)
	if len(want) != 7 || !reflect.DeepEqual(got, want) {
		t.Fatalf("indexed audit diverged from per-output scan (want %d issues):\n got %+v\nwant %+v", len(want), got, want)
	}
}

package main

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"blocknet/wallet"
	bolt "go.etcd.io/bbolt"
)

// Synthetic wallet: 8,192 outputs across 128 blocks, with eight transactions
// and eight outputs per transaction. Half the records use the legacy JSON
// encoding; half use binary storage. No PoW or signature validation is timed.
func BenchmarkWalletCanonicalOutputAudit(b *testing.B) {
	storage, err := NewStorage(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()
	chain := &Chain{storage: storage, height: 127}
	var parent [32]byte
	var outputs []*wallet.OwnedOutput
	for h := uint64(0); h < 128; h++ {
		block := &Block{Header: BlockHeader{Height: h, PrevHash: parent, Difficulty: 1}}
		for j := uint64(0); j < 8; j++ {
			tx := &Transaction{Version: 1}
			binary.LittleEndian.PutUint64(tx.TxPublicKey[:], h*8+j+1)
			for k := uint64(0); k < 8; k++ {
				out := TxOutput{RangeProof: make([]byte, 672)}
				binary.LittleEndian.PutUint64(out.PublicKey[:], (h*8+j)*8+k+1)
				out.Commitment = out.PublicKey
				tx.Outputs = append(tx.Outputs, out)
			}
			id, err := tx.TxID()
			if err != nil {
				b.Fatal(err)
			}
			for k, out := range tx.Outputs {
				outputs = append(outputs, &wallet.OwnedOutput{TxID: id, OutputIndex: k, BlockHeight: h, OneTimePubKey: out.PublicKey, Commitment: out.Commitment})
			}
			block.Transactions = append(block.Transactions, tx)
		}
		hash := block.Hash()
		if err := storage.CommitBlock(&BlockCommit{Block: block, Hash: hash, Height: h, Work: h + 1, IsMainTip: true}); err != nil {
			b.Fatal(err)
		}
		if h%2 == 0 {
			if err := storage.db.Update(func(tx *bolt.Tx) error {
				raw, err := json.Marshal(block)
				if err != nil {
					return err
				}
				return tx.Bucket(bucketBlocks).Put(hash[:], raw)
			}); err != nil {
				b.Fatal(err)
			}
		}
		parent = hash
	}
	b.Run("per_output", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for _, out := range outputs {
				if issue, bad := auditWalletCanonicalOutput(chain, out); bad {
					b.Fatal(issue)
				}
			}
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if issues := auditWalletCanonicalOutputs(chain, outputs); len(issues) != 0 {
				b.Fatal(issues)
			}
		}
	})
}

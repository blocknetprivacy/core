package main

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func BenchmarkSelectRingMembersWithCommitments(b *testing.B) {
	for _, outputCount := range []int{64, 1024, 8192, 65536} {
		b.Run(fmt.Sprintf("outputs_%d", outputCount), func(b *testing.B) {
			chain, storage := newRingSelectionBenchmarkChain(b)
			defer func() {
				if err := chain.Close(); err != nil {
					b.Fatalf("failed to close chain: %v", err)
				}
			}()

			genesis := chain.GetBlockByHeight(0)
			if genesis == nil {
				b.Fatal("expected genesis block")
			}

			outputs := make([]TxOutput, outputCount)
			for i := range outputs {
				outputs[i] = TxOutput{
					PublicKey:  ringSelectionBenchmarkKey(byte(0x10), uint64(i)),
					Commitment: ringSelectionBenchmarkKey(byte(0x20), uint64(i)),
				}
			}

			block := &Block{
				Header: BlockHeader{
					Version:    1,
					Height:     1,
					PrevHash:   genesis.Hash(),
					Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
					Difficulty: MinDifficulty,
				},
				Transactions: []*Transaction{
					{
						Version: 1,
						Outputs: outputs,
					},
				},
			}
			commitRingSelectionBenchmarkBlock(b, chain, storage, block)

			realPubKey := outputs[0].PublicKey
			realCommitment := outputs[0].Commitment
			if _, err := chain.SelectRingMembersWithCommitments(realPubKey, realCommitment); err != nil {
				b.Fatalf("warmup ring selection failed: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := chain.SelectRingMembersWithCommitments(realPubKey, realCommitment); err != nil {
					b.Fatalf("ring selection failed: %v", err)
				}
			}
		})
	}
}

func newRingSelectionBenchmarkChain(b *testing.B) (*Chain, *Storage) {
	b.Helper()

	chain, err := NewChain(b.TempDir())
	if err != nil {
		b.Fatalf("failed to create chain: %v", err)
	}
	genesis, err := GetGenesisBlock()
	if err != nil {
		b.Fatalf("failed to load genesis block: %v", err)
	}
	if err := chain.addGenesisBlock(genesis); err != nil {
		b.Fatalf("failed to add genesis block: %v", err)
	}
	return chain, chain.Storage()
}

func commitRingSelectionBenchmarkBlock(b *testing.B, chain *Chain, storage *Storage, block *Block) {
	b.Helper()

	txID, err := block.Transactions[0].TxID()
	if err != nil {
		b.Fatalf("failed to hash tx: %v", err)
	}
	newOutputs := make([]*UTXO, 0, len(block.Transactions[0].Outputs))
	for idx, out := range block.Transactions[0].Outputs {
		newOutputs = append(newOutputs, &UTXO{
			TxID:        txID,
			OutputIndex: uint32(idx),
			Output:      out,
			BlockHeight: block.Header.Height,
		})
	}

	hash := block.Hash()
	work := uint64(2)
	if err := storage.CommitBlock(&BlockCommit{
		Block:      block,
		Height:     block.Header.Height,
		Hash:       hash,
		Work:       work,
		IsMainTip:  true,
		NewOutputs: newOutputs,
	}); err != nil {
		b.Fatalf("failed to commit block: %v", err)
	}

	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.blocks[hash] = block
	chain.workAt[hash] = work
	chain.byHeight[block.Header.Height] = hash
	chain.bestHash = hash
	chain.height = block.Header.Height
	chain.totalWork = work
	chain.canonicalRingIndexDirty = true
}

func ringSelectionBenchmarkKey(prefix byte, n uint64) [32]byte {
	var key [32]byte
	key[0] = prefix
	binary.LittleEndian.PutUint64(key[8:], n)
	return key
}

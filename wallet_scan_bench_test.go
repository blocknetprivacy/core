package main

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

// buildSyntheticScanBatch builds nBlocks blocks, each with txPerBlock
// transactions of outsPerTx outputs. All outputs are non-owned (random-looking
// pubkeys) — the realistic sync case where the wallet owns almost nothing but
// must still derive against every output. Transaction public keys are valid
// Ristretto points (required by the batch matcher); output pubkeys are arbitrary
// bytes (compared, never decompressed). Heights are past the indexed-derivation
// activation so the indexed path is exercised.
func buildSyntheticScanBatch(b *testing.B, nBlocks, txPerBlock, outsPerTx int) []*wallet.BlockData {
	b.Helper()

	// A small pool of valid tx public keys to cycle through.
	const nPubs = 32
	txPubs := make([][32]byte, nPubs)
	for i := range txPubs {
		kp, err := GenerateRistrettoKeypair()
		if err != nil {
			b.Fatalf("keypair: %v", err)
		}
		txPubs[i] = kp.PublicKey
	}

	blocks := make([]*wallet.BlockData, nBlocks)
	var counter uint64
	for bi := 0; bi < nBlocks; bi++ {
		blk := &wallet.BlockData{
			Height:       params.IndexedDerivationHeight + 1000 + uint64(bi),
			Transactions: make([]wallet.TxData, txPerBlock),
		}
		for ti := 0; ti < txPerBlock; ti++ {
			tx := wallet.TxData{
				TxPubKey: txPubs[(bi*txPerBlock+ti)%nPubs],
				Outputs:  make([]wallet.OutputData, outsPerTx),
			}
			binary.LittleEndian.PutUint64(tx.TxID[:], counter)
			for oi := 0; oi < outsPerTx; oi++ {
				out := wallet.OutputData{Index: oi}
				counter++
				binary.LittleEndian.PutUint64(out.PubKey[:], counter)
				binary.LittleEndian.PutUint64(out.PubKey[16:], counter*2654435761)
				tx.Outputs[oi] = out
			}
			blk.Transactions[ti] = tx
		}
		blocks[bi] = blk
	}
	return blocks
}

func benchScan(b *testing.B, withBatch bool) {
	walletFile := filepath.Join(b.TempDir(), "bench.wallet")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		b.Fatalf("NewWallet: %v", err)
	}

	cfg := defaultScannerConfig()
	if !withBatch {
		cfg.ScanOutputsBatch = nil // force the per-output composed path
	}

	// 2000 blocks x 4 txs x 8 outputs = 64k outputs per iteration.
	blocks := buildSyntheticScanBatch(b, 2000, 4, 8)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc := wallet.NewScanner(w, cfg)
		sc.ScanBlocksReport(blocks, nil)
	}
}

func BenchmarkScan_BatchFFI(b *testing.B)  { benchScan(b, true) }
func BenchmarkScan_PerOutput(b *testing.B) { benchScan(b, false) }

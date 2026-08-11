package main

import (
	"bytes"
	"testing"
	"time"
)

func mustSeedMempoolEntries(t *testing.T, mempool *Mempool, feeRates []uint64) {
	t.Helper()

	for i, feeRate := range feeRates {
		var txID [32]byte
		txID[0] = byte(i + 1)
		mempool.txByID[txID] = &MempoolEntry{
			TxID:    txID,
			Fee:     feeRate * 100,
			FeeRate: feeRate,
			Size:    100,
			AddedAt: time.Now(),
			index:   i,
		}
	}
}

// GetAllEntries must order by fee rate and hand back copies. The live entries
// carry a priority-queue index that the heap mutates, so returning them would
// let a caller read a field that changes after the lock drops.
func TestGetAllEntriesSortsAndCopies(t *testing.T) {
	mempool := NewMempool(DefaultMempoolConfig(), nil, nil)
	mustSeedMempoolEntries(t, mempool, []uint64{5, 40, 40, 1, 12})

	entries := mempool.GetAllEntries()
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}

	for i := 1; i < len(entries); i++ {
		prev, cur := entries[i-1], entries[i]
		if cur.FeeRate > prev.FeeRate {
			t.Fatalf("entry %d fee rate %d exceeds previous %d; not sorted descending", i, cur.FeeRate, prev.FeeRate)
		}
		if cur.FeeRate == prev.FeeRate && bytes.Compare(prev.TxID[:], cur.TxID[:]) >= 0 {
			t.Fatalf("equal fee rates at %d are not tie-broken by TxID", i)
		}
	}

	for _, entry := range entries {
		if entry == mempool.txByID[entry.TxID] {
			t.Fatalf("entry %x is the live mempool entry, not a copy", entry.TxID[:4])
		}
	}

	entries[0].FeeRate = 999
	if mempool.txByID[entries[0].TxID].FeeRate == 999 {
		t.Fatal("mutating a returned entry changed mempool state")
	}
}

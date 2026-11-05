package main

import (
	"testing"
	"time"
)

func seedSizedEntries(m *Mempool, entries ...[2]int) {
	for i, e := range entries {
		var id [32]byte
		id[0], id[1] = byte(i), byte(i>>8)
		m.txByID[id] = &MempoolEntry{Tx: &Transaction{}, TxID: id, FeeRate: uint64(e[0]), Size: e[1], AddedAt: time.Unix(int64(1000+i), 0), index: -1}
		m.totalSize += e[1]
	}
}

// A full mempool makes room by dropping the cheapest transaction (oldest of
// equals), and only for one that pays more.
func TestMempoolEvictsLowestFeeRate(t *testing.T) {
	m := NewMempool(DefaultMempoolConfig(), nil, nil)
	seedSizedEntries(m, [2]int{30, 100}, [2]int{5, 100}, [2]int{5, 100}, [2]int{40, 100})
	if m.evictLowest(5) {
		t.Fatal("evicted for a newcomer paying no more")
	}
	if !m.evictLowest(6) {
		t.Fatal("did not evict for a higher rate")
	}
	var oldest5, newer5 [32]byte
	oldest5[0], newer5[0] = 1, 2
	if _, ok := m.txByID[oldest5]; ok {
		t.Fatal("kept the oldest of the cheapest")
	}
	if _, ok := m.txByID[newer5]; !ok {
		t.Fatal("dropped the newer cheapest one first")
	}
	if m.totalSize != 300 {
		t.Fatalf("size after eviction: %d", m.totalSize)
	}
}

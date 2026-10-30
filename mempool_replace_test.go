package main

import (
	"container/heap"
	"strings"
	"testing"
	"time"

	"blocknet/wallet"
)

func replaceTestEntry(id byte, fee uint64, size int, images ...byte) *MempoolEntry {
	tx := &Transaction{Fee: fee}
	for _, ki := range images {
		tx.Inputs = append(tx.Inputs, TxInput{KeyImage: [32]byte{ki}})
	}
	return &MempoolEntry{Tx: tx, TxID: [32]byte{id}, Fee: fee, FeeRate: fee / uint64(size), Size: size}
}

func replaceTestMempool(entries ...*MempoolEntry) *Mempool {
	m := NewMempool(DefaultMempoolConfig(), nil, nil)
	for _, e := range entries {
		m.txByID[e.TxID] = e
		for _, in := range e.Tx.Inputs {
			m.txByImage[in.KeyImage] = e.TxID
		}
	}
	return m
}

func TestReplacementRules(t *testing.T) {
	step := DefaultMempoolConfig().ReplaceRateStep
	old := replaceTestEntry(1, 10*1000, 1000, 1, 2)
	other := replaceTestEntry(2, 10*1000, 1000, 3)

	cases := []struct {
		name    string
		new     *MempoolEntry
		replace bool
		want    string
	}{
		{"no conflict", replaceTestEntry(9, 10*1000, 1000, 7), false, ""},
		{"higher rate replaces", replaceTestEntry(9, (10+step)*1000, 1000, 1, 2), true, ""},
		{"extra coin is allowed", replaceTestEntry(9, (10+step)*1200, 1200, 1, 2, 8), true, ""},
		{"rate one short", replaceTestEntry(9, (10+step-1)*1000, 1000, 1, 2), false, "fee rate of at least 20 per byte"},
		{"same rate", replaceTestEntry(9, 10*1000, 1000, 1, 2), false, "fee rate of at least"},
		{"misses a coin", replaceTestEntry(9, 50*1000, 1000, 1), false, "must spend every coin"},
		{"two conflicts", replaceTestEntry(9, 50*1000, 1000, 1, 2, 3), false, "may replace only one"},
		{"smaller tx pays too little in total", replaceTestEntry(9, 20*600, 600, 1, 2), false, "fee of at least"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := replaceTestMempool(old, other)
			got, err := m.replacementTarget(tc.new.Tx, tc.new.FeeRate, tc.new.Size)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "double-spend") {
					t.Fatalf("got %v, want an error with %q that still says double-spend", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tc.replace || (got != nil && got.TxID != old.TxID) {
				t.Fatalf("replacement target %v, want replace=%v of the old tx", got, tc.replace)
			}
		})
	}
}

func buildReplaceTestTxs(t *testing.T, extras ...uint64) (*Mempool, [][]byte) {
	t.Helper()
	api, w, d := newFeePriorityTestAPI(t)
	keys := w.Keys()
	var txs [][]byte
	for _, extra := range extras {
		res, err := api.createTxBuilder().Transfer([]wallet.Recipient{{SpendPubKey: keys.SpendPubKey, ViewPubKey: keys.ViewPubKey, Amount: 20_000_000}}, sendFeePerByte+extra, d.chain.Height())
		if err != nil {
			t.Fatal(err)
		}
		w.ReleaseInputLease(res.InputLease)
		txs = append(txs, res.TxData)
	}
	return d.mempool, txs
}

func mustAddTx(t *testing.T, m *Mempool, data []byte) [32]byte {
	t.Helper()
	tx, err := DeserializeTx(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTransaction(tx, data); err != nil {
		t.Fatal(err)
	}
	id, _ := tx.TxID()
	return id
}

func TestMempoolReplacesStuckSendWithHigherFee(t *testing.T) {
	m, txs := buildReplaceTestTxs(t, 0, 10)
	oldID := mustAddTx(t, m, txs[0])
	gen := m.Stats().Generation
	newID := mustAddTx(t, m, txs[1])

	if m.HasTransaction(oldID) || !m.HasTransaction(newID) || m.Size() != 1 {
		t.Fatalf("after replace: old present %v, new present %v, size %d", m.HasTransaction(oldID), m.HasTransaction(newID), m.Size())
	}
	if m.SizeBytes() != len(txs[1]) || m.Stats().Generation == gen {
		t.Fatalf("mempool bytes %d (want %d) or generation unchanged", m.SizeBytes(), len(txs[1]))
	}
	newTx, _ := DeserializeTx(txs[1])
	for _, in := range newTx.Inputs {
		if m.txByImage[in.KeyImage] != newID {
			t.Fatal("key image still points at the replaced tx")
		}
	}
	block := m.GetTransactionsForBlock(MaxBlockSize, 10)
	if len(block) != 1 {
		t.Fatalf("block template holds %d txs, want only the replacement", len(block))
	}
}

func TestMempoolRefusesSmallBumpAndKeepsOldTx(t *testing.T) {
	m, txs := buildReplaceTestTxs(t, 0, 5)
	oldID := mustAddTx(t, m, txs[0])
	tx, _ := DeserializeTx(txs[1])
	err := m.AddTransaction(tx, txs[1])
	if err == nil || !strings.Contains(err.Error(), "key image already in mempool") || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("small bump: %v", err)
	}
	if !m.HasTransaction(oldID) || m.Size() != 1 {
		t.Fatal("refused bump removed the old tx")
	}
}

func TestMempoolReplacesInFullMempool(t *testing.T) {
	m, txs := buildReplaceTestTxs(t, 0, 10)
	pinned := replaceTestEntry(200, 1_000_000, 1000, 200)
	pinned.AddedAt = time.Now().Add(-time.Hour)
	m.txByID[pinned.TxID] = pinned
	m.txByImage[pinned.Tx.Inputs[0].KeyImage] = pinned.TxID
	heap.Push(&m.priorityQueue, pinned)
	m.totalSize += pinned.Size
	mustAddTx(t, m, txs[0])

	m.config.MaxSize = 2
	m.config.MaxSizeBytes = pinned.Size + max(len(txs[0]), len(txs[1]))
	newID := mustAddTx(t, m, txs[1])
	if !m.HasTransaction(newID) || !m.HasTransaction(pinned.TxID) || m.Size() != 2 {
		t.Fatal("a full mempool refused a valid replacement, or evicted another tx for it")
	}
}

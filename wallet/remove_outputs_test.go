package wallet

import "testing"

func TestRemoveOutputsRemovesMatchesAndReservations(t *testing.T) {
	txid1 := [32]byte{0x01}
	txid2 := [32]byte{0x02}
	txid3 := [32]byte{0x03}

	w := &Wallet{
		data: WalletData{
			Outputs: []*OwnedOutput{
				{TxID: txid1, OutputIndex: 0, Amount: 100, Memo: []byte{0x01, 0x02}},
				{TxID: txid2, OutputIndex: 1, Amount: 200},
				{TxID: txid3, OutputIndex: 2, Amount: 300},
			},
		},
		inputReservations: map[reservedOutpoint]inputReservation{
			{TxID: txid1, OutputIndex: 0}: {},
			{TxID: txid2, OutputIndex: 1}: {},
		},
	}

	removed := w.RemoveOutputs([]OutputRef{
		{TxID: txid1, OutputIndex: 0},
		{TxID: txid2, OutputIndex: 1},
		{TxID: [32]byte{0xff}, OutputIndex: 9},
	})

	if len(removed) != 2 {
		t.Fatalf("expected 2 removed outputs, got %d", len(removed))
	}
	if len(w.data.Outputs) != 1 || w.data.Outputs[0].TxID != txid3 {
		t.Fatalf("unexpected kept outputs: %#v", w.data.Outputs)
	}
	if _, ok := w.inputReservations[reservedOutpoint{TxID: txid1, OutputIndex: 0}]; ok {
		t.Fatal("expected reservation for removed output 1 to be cleared")
	}
	if _, ok := w.inputReservations[reservedOutpoint{TxID: txid2, OutputIndex: 1}]; ok {
		t.Fatal("expected reservation for removed output 2 to be cleared")
	}

	removed[0].Memo[0] = 0xff
	if w.data.Outputs[0].Amount != 300 {
		t.Fatalf("kept output was mutated: %#v", w.data.Outputs[0])
	}
}

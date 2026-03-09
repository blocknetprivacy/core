package wallet

import (
	"crypto/sha3"
	"testing"
)

func mockKeyImage(priv [32]byte) ([32]byte, error) {
	return sha3.Sum256(priv[:]), nil
}

func TestScanBlock_PromotesUnconfirmedSpendToConfirmed(t *testing.T) {
	var txID [32]byte
	txID[0] = 0xAA
	var spendTxID [32]byte
	spendTxID[0] = 0xBB

	var privKey [32]byte
	privKey[0] = 0x01
	var pubKey [32]byte
	pubKey[0] = 0x02

	w := &Wallet{
		data: WalletData{
			Outputs: []*OwnedOutput{
				{
					TxID:           txID,
					OutputIndex:    0,
					Amount:         1000,
					OneTimePrivKey: privKey,
					OneTimePubKey:  pubKey,
					BlockHeight:    10,
					Spent:          true,
					SpentHeight:    0,
					SpentTxID:      spendTxID,
				},
			},
		},
	}

	scanner := NewScanner(w, ScannerConfig{
		GenerateKeyImage: mockKeyImage,
	})

	keyImage, _ := mockKeyImage(privKey)

	block := &BlockData{
		Height: 20,
		Transactions: []TxData{
			{KeyImages: [][32]byte{keyImage}},
		},
	}

	_, spent := scanner.ScanBlock(block)
	if spent != 1 {
		t.Fatalf("ScanBlock spent=%d, want 1", spent)
	}

	out := w.data.Outputs[0]
	if !out.Spent {
		t.Fatal("output should still be spent")
	}
	if out.SpentHeight != 20 {
		t.Fatalf("SpentHeight=%d, want 20", out.SpentHeight)
	}
	if out.SpentTxID != ([32]byte{}) {
		t.Fatal("SpentTxID should be zero after block confirmation")
	}

	restored := w.ReconcileUnconfirmedSpends(func([32]byte) bool {
		return false
	})
	if restored != 0 {
		t.Fatalf("ReconcileUnconfirmedSpends restored=%d, want 0", restored)
	}
	if !out.Spent {
		t.Fatal("output should remain spent after reconcile")
	}
	if out.SpentHeight != 20 {
		t.Fatalf("SpentHeight=%d after reconcile, want 20", out.SpentHeight)
	}
}

func TestScanBlock_UnconfirmedSpendNotInBlock_ReconcileRestores(t *testing.T) {
	var txID [32]byte
	txID[0] = 0xAA
	var spendTxID [32]byte
	spendTxID[0] = 0xBB

	var privKey [32]byte
	privKey[0] = 0x01
	var pubKey [32]byte
	pubKey[0] = 0x02

	w := &Wallet{
		data: WalletData{
			Outputs: []*OwnedOutput{
				{
					TxID:           txID,
					OutputIndex:    0,
					Amount:         1000,
					OneTimePrivKey: privKey,
					OneTimePubKey:  pubKey,
					BlockHeight:    10,
					Spent:          true,
					SpentHeight:    0,
					SpentTxID:      spendTxID,
				},
			},
		},
	}

	scanner := NewScanner(w, ScannerConfig{
		GenerateKeyImage: mockKeyImage,
	})

	block := &BlockData{
		Height:       20,
		Transactions: []TxData{{}},
	}

	_, spent := scanner.ScanBlock(block)
	if spent != 0 {
		t.Fatalf("ScanBlock spent=%d, want 0", spent)
	}

	restored := w.ReconcileUnconfirmedSpends(func([32]byte) bool {
		return false
	})
	if restored != 1 {
		t.Fatalf("ReconcileUnconfirmedSpends restored=%d, want 1", restored)
	}

	out := w.data.Outputs[0]
	if out.Spent {
		t.Fatal("output should be restored to unspent")
	}
}

func TestScanBlock_ConfirmedSpendExcludedFromKeyImageIndex(t *testing.T) {
	var txID [32]byte
	txID[0] = 0xAA

	var privKey [32]byte
	privKey[0] = 0x01
	var pubKey [32]byte
	pubKey[0] = 0x02

	w := &Wallet{
		data: WalletData{
			Outputs: []*OwnedOutput{
				{
					TxID:           txID,
					OutputIndex:    0,
					Amount:         1000,
					OneTimePrivKey: privKey,
					OneTimePubKey:  pubKey,
					BlockHeight:    10,
					Spent:          true,
					SpentHeight:    15,
					SpentTxID:      [32]byte{},
				},
			},
		},
	}

	scanner := NewScanner(w, ScannerConfig{
		GenerateKeyImage: mockKeyImage,
	})

	keyImage, _ := mockKeyImage(privKey)

	block := &BlockData{
		Height: 20,
		Transactions: []TxData{
			{KeyImages: [][32]byte{keyImage}},
		},
	}

	_, spent := scanner.ScanBlock(block)
	if spent != 0 {
		t.Fatalf("ScanBlock spent=%d, want 0 (already confirmed spent)", spent)
	}
}

package wallet

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sharedKeyTestHeight = 1000

var (
	sharedTestPriv = [32]byte{1}
	sharedTestPub  = [32]byte{2}
)

func sharedOutput(txByte byte, index int, amount, height uint64) *OwnedOutput {
	return &OwnedOutput{
		TxID:           [32]byte{txByte},
		OutputIndex:    index,
		Amount:         amount,
		OneTimePrivKey: sharedTestPriv,
		OneTimePubKey:  sharedTestPub,
		BlockHeight:    height,
	}
}

func walletWith(outputs ...*OwnedOutput) *Wallet {
	w := &Wallet{}
	for _, out := range outputs {
		w.AddOutput(out)
	}
	return w
}

func wantBalances(t *testing.T, w *Wallet, total, spendable, pending uint64) {
	t.Helper()
	if got := w.Balance(); got != total {
		t.Errorf("Balance = %d, want %d", got, total)
	}
	if got := w.SpendableBalance(sharedKeyTestHeight); got != spendable {
		t.Errorf("SpendableBalance = %d, want %d", got, spendable)
	}
	if got := w.PendingBalance(sharedKeyTestHeight); got != pending {
		t.Errorf("PendingBalance = %d, want %d", got, pending)
	}
}

func TestSharedKeyCountsAsOneCoin(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 300, 10), sharedOutput(0xAA, 1, 300, 10), sharedOutput(0xAA, 2, 300, 10))
	wantBalances(t, w, 300, 300, 0)

	if total, unspent := w.OutputCount(); total != 3 || unspent != 1 {
		t.Errorf("OutputCount = %d, %d, want 3, 1", total, unspent)
	}
	if got := len(w.SpendableOutputs()); got != 1 {
		t.Errorf("SpendableOutputs = %d, want 1", got)
	}
	if got := len(w.MatureOutputs(sharedKeyTestHeight)); got != 1 {
		t.Errorf("MatureOutputs = %d, want 1", got)
	}

	if _, _, err := w.ReserveMatureInputs(sharedKeyTestHeight, 500, time.Minute); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("reserve 500 of a 300 coin: err = %v, want insufficient funds", err)
	}
	_, inputs, err := w.ReserveAllMatureInputs(sharedKeyTestHeight, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Errorf("sweep selected %d inputs, want 1", len(inputs))
	}
}

func TestSharedKeyKeepsLargestInEitherOrder(t *testing.T) {
	small := func() *OwnedOutput { return sharedOutput(0xAA, 0, 100_000_000, 20) }
	large := func() *OwnedOutput { return sharedOutput(0xBB, 0, 40_000_000_000, 10) }

	for name, w := range map[string]*Wallet{
		"large first": walletWith(large(), small()),
		"small first": walletWith(small(), large()),
	} {
		t.Run(name, func(t *testing.T) {
			wantBalances(t, w, 40_000_000_000, 40_000_000_000, 0)
			// A send the small output could cover must still spend the large one.
			lease, inputs, err := w.ReserveMatureInputs(sharedKeyTestHeight, 50_000_000, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			w.ReleaseInputLease(lease)
			if len(inputs) != 1 || inputs[0].Amount != 40_000_000_000 {
				t.Fatalf("selected %+v, want the large output", inputs)
			}
		})
	}
}

func TestSharedKeyEqualAmountsChooseSameOutputInAnyOrder(t *testing.T) {
	a := sharedOutput(0xAA, 0, 300, 10)
	b := sharedOutput(0xBB, 0, 300, 10)
	first := walletWith(a, b)
	second := walletWith(sharedOutput(0xBB, 0, 300, 10), sharedOutput(0xAA, 0, 300, 10))
	if first.sharedKeys[sharedTestPub].TxID != second.sharedKeys[sharedTestPub].TxID {
		t.Fatal("kept output depends on discovery order")
	}
}

func TestSharedKeyWaitsForLargestToMature(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 100, 10), sharedOutput(0xBB, 0, 500, sharedKeyTestHeight-1))
	wantBalances(t, w, 500, 0, 500)
	if _, _, err := w.ReserveMatureInputs(sharedKeyTestHeight, 50, time.Minute); err == nil {
		t.Fatal("selected the small output while the large one is immature")
	}
	if got := len(w.MatureOutputs(sharedKeyTestHeight)); got != 0 {
		t.Fatalf("MatureOutputs = %d, want 0", got)
	}
}

func TestSharedKeyScannerSpendsAllTogether(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 300, 10), sharedOutput(0xAA, 1, 300, 10), sharedOutput(0xAA, 2, 300, 10))
	scanner := NewScanner(w, ScannerConfig{GenerateKeyImage: mockKeyImage})
	keyImage, _ := mockKeyImage(sharedTestPriv)
	block := func(height uint64) *BlockData {
		return &BlockData{Height: height, Transactions: []TxData{{KeyImages: [][32]byte{keyImage}}}}
	}

	if _, spent := scanner.ScanBlock(block(1001)); spent != 1 {
		t.Errorf("first scan marked %d, want 1", spent)
	}
	wantBalances(t, w, 0, 0, 0)
	for _, out := range w.AllOutputs() {
		if !out.Spent || out.SpentHeight != 1001 || out.Unspendable {
			t.Errorf("output %d: spent=%v height=%d unspendable=%v", out.OutputIndex, out.Spent, out.SpentHeight, out.Unspendable)
		}
	}
	if _, spent := scanner.ScanBlock(block(1002)); spent != 0 {
		t.Errorf("repeat scan marked %d, want 0", spent)
	}
	if _, _, err := w.ReserveMatureInputs(sharedKeyTestHeight, 1, time.Minute); err == nil {
		t.Error("selected an output whose key is already spent")
	}
}

func TestSharedKeyUnconfirmedSpendLifecycle(t *testing.T) {
	spendTx := [32]byte{0xCC}
	w := walletWith(sharedOutput(0xAA, 0, 300, 10), sharedOutput(0xBB, 0, 100, 10))

	if !w.MarkSpentByTx(sharedTestPub, spendTx) {
		t.Fatal("MarkSpentByTx reported no change")
	}
	wantBalances(t, w, 0, 0, 0)

	// The copy found while the spend is in flight follows the same spend.
	w.AddOutput(sharedOutput(0xDD, 0, 50, 20))
	for _, out := range w.AllOutputs() {
		if !out.Spent || out.SpentTxID != spendTx {
			t.Fatalf("output %x not in flight: %+v", out.TxID[:1], out)
		}
	}

	if restored := w.ReconcileUnconfirmedSpends(func([32]byte) bool { return false }); restored != 3 {
		t.Fatalf("restored %d, want 3", restored)
	}
	wantBalances(t, w, 300, 300, 0)

	if !w.MarkSpentByTx(sharedTestPub, spendTx) {
		t.Fatal("second MarkSpentByTx reported no change")
	}
	if !w.MarkSpent(sharedTestPub, 1001) {
		t.Fatal("MarkSpent reported no promotion")
	}
	for _, out := range w.AllOutputs() {
		if !out.Spent || out.SpentHeight != 1001 || out.SpentTxID != ([32]byte{}) {
			t.Fatalf("output %x not promoted: %+v", out.TxID[:1], out)
		}
	}
	if w.MarkSpent(sharedTestPub, 1002) {
		t.Fatal("MarkSpent changed already confirmed outputs")
	}
}

func TestSharedKeyLateCopyInheritsConfirmedSpend(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 300, 10))
	w.MarkSpent(sharedTestPub, 50)

	late := sharedOutput(0xBB, 0, 900, 60)
	w.AddOutput(late)
	if !late.Spent || late.SpentHeight != 50 {
		t.Fatalf("late copy spent=%v height=%d, want spent at 50", late.Spent, late.SpentHeight)
	}
	wantBalances(t, w, 0, 0, 0)

	// Undoing the block that held the late copy leaves the spend in place.
	w.RewindToHeight(55)
	if total, unspent := w.OutputCount(); total != 1 || unspent != 0 {
		t.Fatalf("after rewind OutputCount = %d, %d, want 1, 0", total, unspent)
	}
	// Undoing the spend itself frees the coin.
	w.RewindToHeight(40)
	wantBalances(t, w, 300, 300, 0)
}

func TestSharedKeyRewindAndRemoveRechooseKeptOutput(t *testing.T) {
	small := sharedOutput(0xAA, 0, 100, 10)
	w := walletWith(small, sharedOutput(0xBB, 0, 900, 60))
	wantBalances(t, w, 900, 900, 0)

	w.RewindToHeight(55)
	wantBalances(t, w, 100, 100, 0)
	if w.shadowedLocked(small) || len(w.sharedKeys) != 0 {
		t.Fatal("index kept a key that is no longer shared")
	}

	w.AddOutput(sharedOutput(0xBB, 0, 900, 60))
	wantBalances(t, w, 900, 900, 0)
	removed := w.RemoveOutputs([]OutputRef{{TxID: [32]byte{0xBB}, OutputIndex: 0}})
	if len(removed) != 1 {
		t.Fatalf("removed %d, want 1", len(removed))
	}
	wantBalances(t, w, 100, 100, 0)
}

func TestSharedKeyCoinControl(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 100, 10), sharedOutput(0xBB, 0, 900, 10))

	_, _, err := w.ReserveSpecificInputs([]OutputRef{{TxID: [32]byte{0xAA}, OutputIndex: 0}}, sharedKeyTestHeight, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "unspendable") {
		t.Fatalf("reserving the smaller output: err = %v", err)
	}
	_, inputs, err := w.ReserveSpecificInputs([]OutputRef{{TxID: [32]byte{0xBB}, OutputIndex: 0}}, sharedKeyTestHeight, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].Amount != 900 {
		t.Fatalf("selected %+v", inputs)
	}
}

func TestSharedKeySnapshotFlagsOnlyTheShadowedOutput(t *testing.T) {
	w := walletWith(sharedOutput(0xAA, 0, 100, 10), sharedOutput(0xBB, 0, 900, 10))
	for _, out := range w.AllOutputs() {
		if want := out.Amount == 100; out.Unspendable != want {
			t.Errorf("amount %d: Unspendable = %v, want %v", out.Amount, out.Unspendable, want)
		}
	}
}

func TestDistinctKeysAreUnaffected(t *testing.T) {
	other := sharedOutput(0xBB, 0, 200, 10)
	other.OneTimePubKey = [32]byte{3}
	w := walletWith(sharedOutput(0xAA, 0, 100, 10), other)
	wantBalances(t, w, 300, 300, 0)
	if w.sharedKeys != nil {
		t.Fatal("index built without a shared key")
	}
	if !w.MarkSpent(sharedTestPub, 50) {
		t.Fatal("MarkSpent reported no change")
	}
	wantBalances(t, w, 200, 200, 0)
}

func TestLoadWalletRepairsSharedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.dat")
	password := []byte("test password")
	w, err := NewWalletFromStealthKeys(path, password, StealthKeys{}, WalletConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// A file written by an older version: one output spent, its copy still open,
	// and a second key that was never spent.
	spent := sharedOutput(0xAA, 0, 300, 10)
	spent.Spent, spent.SpentHeight = true, 50
	stale := sharedOutput(0xAA, 1, 300, 10)
	openA := sharedOutput(0xBB, 0, 700, 10)
	openA.OneTimePubKey = [32]byte{4}
	openB := sharedOutput(0xBB, 1, 200, 10)
	openB.OneTimePubKey = [32]byte{4}
	w.data.Outputs = []*OwnedOutput{spent, stale, openA, openB}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadWallet(path, password, WalletConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wantBalances(t, loaded, 700, 700, 0)
	for _, out := range loaded.AllOutputs() {
		if out.OneTimePubKey == sharedTestPub && (!out.Spent || out.SpentHeight != 50) {
			t.Errorf("output %d not repaired: spent=%v height=%d", out.OutputIndex, out.Spent, out.SpentHeight)
		}
	}
}

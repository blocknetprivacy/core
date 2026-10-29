package wallet

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReserveMatureInputsPrefersFewInputsAmongEligibleOutputs(t *testing.T) {
	w := &Wallet{}
	add := func(amount, height uint64, spent bool) *OwnedOutput {
		out := &OwnedOutput{Amount: amount, BlockHeight: height, Spent: spent, OutputIndex: len(w.data.Outputs)}
		w.data.Outputs = append(w.data.Outputs, out)
		return out
	}
	for i := 0; i < 200; i++ {
		add(1, 1, false)
	}
	add(200, 1, true)   // Spent exact match.
	add(201, 95, false) // Immature single output.
	filtered := add(202, 1, false)
	w.inputFilter = func(out *OwnedOutput) bool { return out.OutputIndex == filtered.OutputIndex }
	reserved := add(203, 1, false)
	lease, _, err := w.ReserveSpecificInputs([]OutputRef{{OutputIndex: reserved.OutputIndex}}, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer w.ReleaseInputLease(lease)
	eligible := add(1000, 1, false)
	selectionLease, selected, err := w.ReserveMatureInputs(100, 200, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer w.ReleaseInputLease(selectionLease)
	if len(selected) != 1 || selected[0].OutputIndex != eligible.OutputIndex {
		t.Fatalf("expected the single eligible large output, got %d inputs: %+v", len(selected), selected)
	}
	if _, _, err := w.ReserveSpecificInputs([]OutputRef{{OutputIndex: eligible.OutputIndex}}, 100, time.Minute); err == nil {
		t.Fatal("selected output was not reserved")
	}
}

func TestInputReservationLease_PreventsConcurrentSelectionAndReleases(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xBB

	// Single mature output: any concurrent reservation for a target <= Amount
	// must contend for the same outpoint.
	w.data.Outputs = []*OwnedOutput{
		{
			TxID:        txid,
			OutputIndex: 0,
			Amount:      100,
			BlockHeight: 1,
			IsCoinbase:  false,
			Spent:       false,
		},
	}

	currentHeight := uint64(100)

	start := make(chan struct{})
	type res struct {
		lease  uint64
		inputs []*OwnedOutput
		err    error
	}
	out := make(chan res, 2)

	tryReserve := func() {
		<-start
		lease, inputs, err := w.ReserveMatureInputs(currentHeight, 50, 200*time.Millisecond)
		out <- res{lease: lease, inputs: inputs, err: err}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tryReserve() }()
	go func() { defer wg.Done(); tryReserve() }()

	close(start)
	wg.Wait()

	r1 := <-out
	r2 := <-out

	successes := 0
	var success res
	if r1.err == nil {
		successes++
		success = r1
	}
	if r2.err == nil {
		successes++
		success = r2
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful reservation, got %d (err1=%v err2=%v)", successes, r1.err, r2.err)
	}
	if success.lease == 0 || len(success.inputs) != 1 {
		t.Fatalf("expected successful reservation to return 1 input and non-zero lease (lease=%d inputs=%d)", success.lease, len(success.inputs))
	}

	// Releasing the lease should allow reservation again immediately.
	w.ReleaseInputLease(success.lease)
	lease2, inputs2, err := w.ReserveMatureInputs(currentHeight, 50, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("expected reserve after release to succeed, got %v", err)
	}
	if lease2 == 0 || len(inputs2) != 1 {
		t.Fatalf("expected reserve after release to return 1 input and non-zero lease (lease=%d inputs=%d)", lease2, len(inputs2))
	}
	w.ReleaseInputLease(lease2)
}

func TestInputReservationLease_ExpiresByTTL(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xCC

	w.data.Outputs = []*OwnedOutput{
		{
			TxID:        txid,
			OutputIndex: 1,
			Amount:      100,
			BlockHeight: 1,
			IsCoinbase:  false,
			Spent:       false,
		},
	}

	currentHeight := uint64(100)

	lease, _, err := w.ReserveMatureInputs(currentHeight, 50, 10*time.Millisecond)
	if err != nil || lease == 0 {
		t.Fatalf("expected initial reserve to succeed, lease=%d err=%v", lease, err)
	}

	// Without release, a second reservation should fail while TTL is active.
	lease2, _, err2 := w.ReserveMatureInputs(currentHeight, 50, 10*time.Millisecond)
	if err2 == nil || lease2 != 0 {
		t.Fatalf("expected reserve to fail while reserved; lease=%d err=%v", lease2, err2)
	}

	time.Sleep(20 * time.Millisecond)

	// After TTL, reservation should be purged and succeed.
	lease3, inputs3, err3 := w.ReserveMatureInputs(currentHeight, 50, 10*time.Millisecond)
	if err3 != nil || lease3 == 0 || len(inputs3) != 1 {
		t.Fatalf("expected reserve to succeed after TTL expiry; lease=%d inputs=%d err=%v", lease3, len(inputs3), err3)
	}
	w.ReleaseInputLease(lease3)
}

func TestReserveSpecificInputs_RejectsDuplicateRefs(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 1, Spent: false},
	}

	refs := []OutputRef{
		{TxID: txid, OutputIndex: 0},
		{TxID: txid, OutputIndex: 0},
	}

	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for duplicate refs")
	}
	if !strings.Contains(err.Error(), "duplicate of input 0") {
		t.Fatalf("expected duplicate error, got: %v", err)
	}
}

func TestReserveSpecificInputs_RejectsSpentOutput(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 1, Spent: true},
	}

	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for spent output")
	}
	if !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("expected spent error, got: %v", err)
	}
}

func TestReserveSpecificInputs_RejectsImmatureRegular(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 95, IsCoinbase: false, Spent: false},
	}

	// currentHeight=100, BlockHeight=95 → 5 confirmations, needs SafeConfirmations(10)
	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for immature output")
	}
	if !strings.Contains(err.Error(), "has 5 confirmations, needs 10") {
		t.Fatalf("expected immature error with confirmation counts, got: %v", err)
	}
}

func TestReserveSpecificInputs_RejectsImmatureCoinbase(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 50, IsCoinbase: true, Spent: false},
	}

	// currentHeight=100, BlockHeight=50 → 50 confirmations, needs CoinbaseMaturity(60)
	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for immature coinbase")
	}
	if !strings.Contains(err.Error(), "has 50 confirmations, needs 60") {
		t.Fatalf("expected immature coinbase error with confirmation counts, got: %v", err)
	}
}

func TestReserveSpecificInputs_RejectsAlreadyReserved(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 1, Spent: false},
	}

	// Reserve via ReserveMatureInputs first.
	lease1, _, err := w.ReserveMatureInputs(100, 50, time.Minute)
	if err != nil || lease1 == 0 {
		t.Fatalf("setup: expected ReserveMatureInputs to succeed, got lease=%d err=%v", lease1, err)
	}

	// ReserveSpecificInputs for the same output should fail.
	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err = w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for already-reserved output")
	}
	if !strings.Contains(err.Error(), "reserved by another") {
		t.Fatalf("expected reserved error, got: %v", err)
	}

	w.ReleaseInputLease(lease1)
}

func TestReserveSpecificInputs_RejectsFilteredOutput(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid, OutputIndex: 0, Amount: 100, BlockHeight: 1, Spent: false},
	}

	// Simulate a mempool key-image filter that rejects this output.
	w.SetInputFilter(func(out *OwnedOutput) bool {
		return out.TxID == txid && out.OutputIndex == 0
	})

	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for filtered output")
	}
	if !strings.Contains(err.Error(), "key image already in mempool") {
		t.Fatalf("expected mempool filter error, got: %v", err)
	}

	w.SetInputFilter(nil)
}

func TestReserveSpecificInputs_RejectsNotFound(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid [32]byte
	txid[0] = 0xDD

	// Wallet has no outputs at all.
	refs := []OutputRef{{TxID: txid, OutputIndex: 0}}
	_, _, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for missing output")
	}
	if !strings.Contains(err.Error(), "output not found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestReserveSpecificInputs_SuccessReturnsDeepCopies(t *testing.T) {
	w := &Wallet{
		inputReservations: make(map[reservedOutpoint]inputReservation),
	}

	var txid1, txid2 [32]byte
	txid1[0] = 0xDD
	txid2[0] = 0xEE

	w.data.Outputs = []*OwnedOutput{
		{TxID: txid1, OutputIndex: 0, Amount: 100, BlockHeight: 1, Spent: false, Memo: []byte{0x01, 0x02}},
		{TxID: txid2, OutputIndex: 1, Amount: 200, BlockHeight: 2, Spent: false},
	}

	refs := []OutputRef{
		{TxID: txid1, OutputIndex: 0},
		{TxID: txid2, OutputIndex: 1},
	}

	lease, inputs, err := w.ReserveSpecificInputs(refs, 100, time.Minute)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if lease == 0 {
		t.Fatal("expected non-zero lease")
	}
	if len(inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(inputs))
	}
	if inputs[0].Amount != 100 || inputs[1].Amount != 200 {
		t.Fatalf("unexpected amounts: %d, %d", inputs[0].Amount, inputs[1].Amount)
	}

	// Verify deep copy: mutating returned inputs must not affect wallet state.
	inputs[0].Amount = 999
	inputs[0].Memo[0] = 0xFF
	if w.data.Outputs[0].Amount != 100 {
		t.Fatal("returned input is not a deep copy (amount)")
	}
	if w.data.Outputs[0].Memo[0] != 0x01 {
		t.Fatal("returned input is not a deep copy (memo)")
	}

	// The output should now be reserved — a second call should fail.
	_, _, err = w.ReserveSpecificInputs(refs[:1], 100, time.Minute)
	if err == nil {
		t.Fatal("expected error for already-reserved output after successful reservation")
	}

	w.ReleaseInputLease(lease)

	// After release, reservation should succeed again.
	lease2, _, err := w.ReserveSpecificInputs(refs[:1], 100, time.Minute)
	if err != nil {
		t.Fatalf("expected success after release, got: %v", err)
	}
	w.ReleaseInputLease(lease2)
}

package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForEachParallelVisitsEveryIndexOnce(t *testing.T) {
	const n = 1000
	var seen [n]atomic.Int32
	forEachParallel(n, func(i int) { seen[i].Add(1) })
	for i := range seen {
		if got := seen[i].Load(); got != 1 {
			t.Fatalf("index %d visited %d times", i, got)
		}
	}
	forEachParallel(0, func(int) { t.Fatal("no index to visit") })
}

func TestFirstErrorIsLowestIndexed(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	if got := firstError([]error{nil, first, second}); got != first {
		t.Fatalf("got %v, want %v", got, first)
	}
	if got := firstError([]error{nil, nil}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// The signing hash covers the outputs, so a tampered range proof also breaks
// the input's signature. Both checks fail, and the input is reported, as the
// one-at-a-time check did.
func TestValidateTransactionReportsFirstFailingProof(t *testing.T) {
	notSpent := func([32]byte) bool { return false }
	canonical := func(_, _ [32]byte) bool { return true }

	tx := mustBuildValidRingCTBindingTestTx(t)
	proof := append([]byte(nil), tx.Outputs[0].RangeProof...)
	proof[10] ^= 0x01
	if err := VerifyRangeProof(tx.Outputs[0].Commitment, &RangeProof{Proof: proof}); err == nil {
		t.Fatal("tampered range proof should not verify")
	}
	tx.Outputs[0].RangeProof = proof
	err := ValidateTransaction(tx, notSpent, canonical)
	if err == nil || !strings.Contains(err.Error(), "input 0: invalid RingCT signature") {
		t.Fatalf("got %v, want the input signature to be reported first", err)
	}
}

// A spent key image is only reported for a transaction whose proofs verify.
func TestValidateTransactionChecksSpendsAfterProofs(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	spent := func([32]byte) bool { return true }
	canonical := func(_, _ [32]byte) bool { return true }
	err := ValidateTransaction(tx, spent, canonical)
	if err == nil || !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("got %v, want a double-spend", err)
	}
	tx.Inputs[0].RingSignature = append([]byte(nil), tx.Inputs[0].RingSignature...)
	tx.Inputs[0].RingSignature[40] ^= 0x01
	err = ValidateTransaction(tx, spent, canonical)
	if err == nil || !strings.Contains(err.Error(), "invalid RingCT signature") {
		t.Fatalf("got %v, want the signature failure", err)
	}
}

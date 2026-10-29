package wallet

import (
	"errors"
	"math/bits"
	"math/rand"
	"slices"
	"testing"
)

func TestSelectInputsPrefersFewerInputs(t *testing.T) {
	tests := []struct {
		name    string
		amounts []uint64
		target  uint64
		want    []uint64
	}{
		{"single before exact pair", []uint64{40, 60, 110, 200}, 100, []uint64{110}},
		{"exact single", []uint64{40, 60, 100, 110}, 100, []uint64{100}},
		{"exact pair when single unavailable", []uint64{40, 60, 70}, 100, []uint64{40, 60}},
		{"reduce change without adding inputs", []uint64{5, 15, 30, 80, 90}, 100, []uint64{15, 90}},
		{"three inputs required", []uint64{1, 2, 3, 40, 50, 60}, 140, []uint64{40, 50, 60}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			available := make([]*OwnedOutput, len(tt.amounts))
			for i, amount := range tt.amounts {
				available[i] = &OwnedOutput{Amount: amount}
			}
			before := slices.Clone(available)
			selected, err := SelectInputs(available, tt.target)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]uint64, len(selected))
			for i, out := range selected {
				got[i] = out.Amount
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("selected amounts %v, want %v", got, tt.want)
			}
			if !slices.Equal(available, before) {
				t.Fatal("selection reordered caller's outputs")
			}
		})
	}
}

func TestSelectInputsFragmentedBalance(t *testing.T) {
	available := make([]*OwnedOutput, 256)
	for i := range available {
		available[i] = &OwnedOutput{Amount: 1}
	}
	large := &OwnedOutput{Amount: 1000}
	available = append(available, large)
	selected, err := SelectInputs(available, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != large {
		t.Fatalf("selected %d inputs, want the single large output", len(selected))
	}
}

func TestSelectInputsLimitsAndSpentOutputs(t *testing.T) {
	if _, err := SelectInputs(nil, 1); !errors.Is(err, ErrNoSpendableOutputs) {
		t.Fatalf("empty wallet: %v", err)
	}
	if _, err := SelectInputs([]*OwnedOutput{{Amount: 100, Spent: true}, {Amount: 1}}, 2); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("spent balance must be excluded: %v", err)
	}
	available := make([]*OwnedOutput, maxSelectedInputs+1)
	for i := range available {
		available[i] = &OwnedOutput{Amount: 1}
	}
	selected, err := SelectInputs(available, maxSelectedInputs)
	if err != nil || len(selected) != maxSelectedInputs {
		t.Fatalf("at input cap: selected %d inputs, err=%v", len(selected), err)
	}
	if _, err := SelectInputs(available, maxSelectedInputs+1); !errors.Is(err, ErrInputLimitExceeded) {
		t.Fatalf("above input cap: %v", err)
	}
}

// Compare the selector against every subset of small generated wallets. This
// checks the minimum-count contract independently of the selection strategy.
func TestSelectInputsMinimumCount(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		available := make([]*OwnedOutput, 8)
		for i := range available {
			available[i] = &OwnedOutput{Amount: uint64(1 + rng.Intn(50)), Spent: rng.Intn(5) == 0}
		}
		target := uint64(1 + rng.Intn(250))
		wantCount := len(available) + 1
		for mask := 1; mask < 1<<len(available); mask++ {
			var total uint64
			valid := true
			for i, out := range available {
				if mask&(1<<i) != 0 {
					total += out.Amount
					valid = valid && !out.Spent
				}
			}
			if valid && total >= target {
				wantCount = min(wantCount, bits.OnesCount(uint(mask)))
			}
		}
		selected, err := SelectInputs(available, target)
		if wantCount > len(available) {
			if !errors.Is(err, ErrInsufficientFunds) {
				t.Fatalf("trial %d: expected insufficient funds, got %v", trial, err)
			}
			continue
		}
		if err != nil || len(selected) != wantCount {
			t.Fatalf("trial %d: selected %d inputs, want %d, err=%v", trial, len(selected), wantCount, err)
		}
		seen := make(map[*OwnedOutput]bool)
		var total uint64
		for _, out := range selected {
			if seen[out] || out.Spent || !slices.Contains(available, out) {
				t.Fatalf("trial %d: duplicate or unavailable input", trial)
			}
			seen[out] = true
			total += out.Amount
		}
		if total < target {
			t.Fatalf("trial %d: selected %d, need %d", trial, total, target)
		}
	}
}

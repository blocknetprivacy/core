package wallet

import (
	"crypto/rand"
	"errors"
	"math/big"
	"sort"
)

var (
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrNoSpendableOutputs = errors.New("no spendable outputs")
	ErrInputLimitExceeded = errors.New("input limit exceeded")
)

const maxSelectedInputs = 256

// SelectInputs covers the target with the fewest inputs, then tries to reduce
// change without increasing that count. Equal-amount candidates are randomized.
func SelectInputs(available []*OwnedOutput, targetAmount uint64) ([]*OwnedOutput, error) {
	if len(available) == 0 {
		return nil, ErrNoSpendableOutputs
	}

	// Filter to only unspent
	var spendable []*OwnedOutput
	var totalAvailable uint64
	for _, out := range available {
		if !out.Spent {
			spendable = append(spendable, out)
			totalAvailable += out.Amount
		}
	}

	if totalAvailable < targetAmount {
		return nil, ErrInsufficientFunds
	}

	// Randomize iteration order to avoid deterministic/fingerprintable selection patterns
	// from stable wallet storage ordering. This is best-effort; if crypto/rand fails,
	// RandomShuffle no-ops and we fall back to deterministic behavior.
	RandomShuffle(spendable)

	// Prefer one input even when a pair could avoid a change output. Among
	// sufficient single outputs, choose the smallest; the shuffle breaks ties.
	var single *OwnedOutput
	for _, out := range spendable {
		if out.Amount >= targetAmount && (single == nil || out.Amount < single.Amount) {
			single = out
		}
	}
	if single != nil {
		return []*OwnedOutput{single}, nil
	}

	// With no sufficient single output, an exact pair has the minimum input
	// count and avoids change.
	if exact := findExactMatch(spendable, targetAmount); exact != nil {
		return exact, nil
	}

	// Largest-first establishes the minimum count. The mixed strategy swaps
	// individual inputs to reduce change without adding more inputs.
	selected := selectMixed(spendable, targetAmount, maxSelectedInputs)
	if selected != nil {
		return selected, nil
	}

	return nil, ErrInputLimitExceeded
}

func randIndex(n int) (int, bool) {
	if n <= 0 {
		return 0, false
	}
	jBig, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, false
	}
	return int(jBig.Int64()), true
}

// findExactMatch tries to find a combination that exactly matches target
// Only checks single outputs and pairs for efficiency
func findExactMatch(outputs []*OwnedOutput, target uint64) []*OwnedOutput {
	// Check single outputs (random tie-break when multiple candidates exist)
	var singles []*OwnedOutput
	for _, out := range outputs {
		if out.Amount == target {
			singles = append(singles, out)
		}
	}
	if len(singles) > 0 {
		if j, ok := randIndex(len(singles)); ok {
			return []*OwnedOutput{singles[j]}
		}
		return []*OwnedOutput{singles[0]}
	}

	// Check pairs (random tie-break when multiple candidates exist)
	var pairs [][2]*OwnedOutput
	for i, a := range outputs {
		for j := i + 1; j < len(outputs); j++ {
			b := outputs[j]
			if a.Amount+b.Amount == target {
				pairs = append(pairs, [2]*OwnedOutput{a, b})
			}
		}
	}
	if len(pairs) > 0 {
		if j, ok := randIndex(len(pairs)); ok {
			return []*OwnedOutput{pairs[j][0], pairs[j][1]}
		}
		return []*OwnedOutput{pairs[0][0], pairs[0][1]}
	}

	return nil
}

// selectMixed starts with a capped largest-first set (fast to build),
// then replaces some large inputs with smaller ones when possible.
func selectMixed(outputs []*OwnedOutput, target uint64, maxInputs int) []*OwnedOutput {
	base := selectLargestFirstCapped(outputs, target, maxInputs)
	if base == nil {
		return nil
	}

	selected := make([]*OwnedOutput, len(base))
	copy(selected, base)

	var total uint64
	chosen := make(map[*OwnedOutput]struct{}, len(selected))
	for _, out := range selected {
		total += out.Amount
		chosen[out] = struct{}{}
	}

	// Replacement candidates from smallest upward.
	candidates := make([]*OwnedOutput, 0, len(outputs))
	for _, out := range outputs {
		if _, ok := chosen[out]; ok {
			continue
		}
		candidates = append(candidates, out)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Amount != candidates[j].Amount {
			return candidates[i].Amount < candidates[j].Amount
		}
		return false
	})

	// Keep selected ordered by largest first to replace expensive inputs first.
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Amount != selected[j].Amount {
			return selected[i].Amount > selected[j].Amount
		}
		return false
	})

	for _, small := range candidates {
		// Find first replaceable large input.
		for i := 0; i < len(selected); i++ {
			large := selected[i]
			if large.Amount <= small.Amount {
				continue
			}
			newTotal := total - large.Amount + small.Amount
			if newTotal < target {
				continue
			}
			selected[i] = small
			total = newTotal
			break
		}
	}

	return selected
}

func selectLargestFirstCapped(outputs []*OwnedOutput, target uint64, maxInputs int) []*OwnedOutput {
	if maxInputs <= 0 {
		return nil
	}

	sorted := make([]*OwnedOutput, len(outputs))
	copy(sorted, outputs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Amount != sorted[j].Amount {
			return sorted[i].Amount > sorted[j].Amount
		}
		return false
	})

	var selected []*OwnedOutput
	var total uint64
	for _, out := range sorted {
		if len(selected) >= maxInputs {
			return nil
		}
		selected = append(selected, out)
		total += out.Amount
		if total >= target {
			return selected
		}
	}
	return nil
}

// SelectInputsWithDecoys is an alternative that also considers decoy availability
// Prefers outputs that have good decoy options in the UTXO set
func SelectInputsWithDecoys(
	available []*OwnedOutput,
	targetAmount uint64,
	countDecoys func(commitment [32]byte) int,
	minDecoys int,
) ([]*OwnedOutput, error) {
	if len(available) == 0 {
		return nil, ErrNoSpendableOutputs
	}

	// Filter to spendable outputs with sufficient decoys
	var spendable []*OwnedOutput
	var totalAvailable uint64
	for _, out := range available {
		if !out.Spent && countDecoys(out.Commitment) >= minDecoys {
			spendable = append(spendable, out)
			totalAvailable += out.Amount
		}
	}

	if totalAvailable < targetAmount {
		return nil, ErrInsufficientFunds
	}

	return SelectInputs(spendable, targetAmount)
}

// RandomShuffle shuffles outputs using cryptographically secure randomness
// This prevents output order from revealing which is the change output
func RandomShuffle(outputs []*OwnedOutput) {
	n := len(outputs)
	if n <= 1 {
		return
	}

	// Fisher-Yates shuffle with crypto/rand using unbiased bounded draws.
	for i := n - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			// If crypto/rand fails, don't shuffle (fail safe).
			return
		}
		j := int(jBig.Int64())
		outputs[i], outputs[j] = outputs[j], outputs[i]
	}
}

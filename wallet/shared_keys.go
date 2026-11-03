package wallet

import "bytes"

// Outputs that hold the same one-time key also share a key image, so at most one
// of them can ever be spent. The wallet treats them as a single coin: it keeps the
// largest for balances and input selection, and marks them all spent together.
//
// Callers of the *Locked helpers must hold w.mu.

// keepsOverSharedKey reports whether a is kept over b. The order is total, so the
// choice does not depend on the order in which outputs were found.
func keepsOverSharedKey(a, b *OwnedOutput) bool {
	if a.Amount != b.Amount {
		return a.Amount > b.Amount
	}
	if a.BlockHeight != b.BlockHeight {
		return a.BlockHeight < b.BlockHeight
	}
	if c := bytes.Compare(a.TxID[:], b.TxID[:]); c != 0 {
		return c < 0
	}
	return a.OutputIndex < b.OutputIndex
}

// shadowedLocked reports whether out shares its one-time key with the output the
// wallet keeps. Such an output is never counted or selected.
func (w *Wallet) shadowedLocked(out *OwnedOutput) bool {
	if len(w.sharedKeys) == 0 {
		return false
	}
	kept, shared := w.sharedKeys[out.OneTimePubKey]
	return shared && kept != out
}

// indexSharedKeyLocked records which output is kept for a key known to be shared.
func (w *Wallet) indexSharedKeyLocked(key [32]byte) {
	var kept *OwnedOutput
	for _, out := range w.data.Outputs {
		if out == nil || out.OneTimePubKey != key {
			continue
		}
		if kept == nil || keepsOverSharedKey(out, kept) {
			kept = out
		}
	}
	if w.sharedKeys == nil {
		w.sharedKeys = make(map[[32]byte]*OwnedOutput)
	}
	w.sharedKeys[key] = kept
}

// sharedKeyGroupsLocked returns the outputs of every one-time key held more than
// once.
func (w *Wallet) sharedKeyGroupsLocked() map[[32]byte][]*OwnedOutput {
	first := make(map[[32]byte]*OwnedOutput, len(w.data.Outputs))
	var groups map[[32]byte][]*OwnedOutput
	for _, out := range w.data.Outputs {
		if out == nil {
			continue
		}
		prior, seen := first[out.OneTimePubKey]
		if !seen {
			first[out.OneTimePubKey] = out
			continue
		}
		if groups == nil {
			groups = make(map[[32]byte][]*OwnedOutput)
		}
		if len(groups[out.OneTimePubKey]) == 0 {
			groups[out.OneTimePubKey] = append(groups[out.OneTimePubKey], prior)
		}
		groups[out.OneTimePubKey] = append(groups[out.OneTimePubKey], out)
	}
	return groups
}

// rebuildSharedKeysLocked recomputes the index after outputs were removed.
func (w *Wallet) rebuildSharedKeysLocked() {
	groups := w.sharedKeyGroupsLocked()
	if len(groups) == 0 {
		w.sharedKeys = nil
		return
	}
	w.sharedKeys = make(map[[32]byte]*OwnedOutput, len(groups))
	for key, group := range groups {
		kept := group[0]
		for _, out := range group[1:] {
			if keepsOverSharedKey(out, kept) {
				kept = out
			}
		}
		w.sharedKeys[key] = kept
	}
}

// repairSharedKeysLocked indexes outputs read from a wallet file and spreads a
// recorded spend across its shared key. Wallet files written before outputs were
// marked spent together can hold a spent output whose copies still look unspent.
func (w *Wallet) repairSharedKeysLocked() {
	for _, group := range w.sharedKeyGroupsLocked() {
		for _, out := range group {
			for _, other := range group {
				if other != out {
					inheritSpendState(out, other)
				}
			}
		}
	}
	w.rebuildSharedKeysLocked()
}

// inheritSpendState copies from's spend onto out, which holds the same one-time
// key. A confirmed spend replaces an unconfirmed one.
func inheritSpendState(out, from *OwnedOutput) {
	if !from.Spent {
		return
	}
	outConfirmed := out.Spent && out.SpentTxID == ([32]byte{})
	if outConfirmed {
		return
	}
	out.Spent = true
	out.SpentHeight = from.SpentHeight
	out.SpentTxID = from.SpentTxID
}

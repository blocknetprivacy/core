package main

import "testing"

func TestTemplateCoinbasePrebuildIsUsedOnce(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	var tc templateCoinbases
	key := coinbaseKey{keys.SpendPubKey, keys.ViewPubKey, 101}
	tc.prebuild(key)
	tc.prebuild(key) // already pending: no second build

	tc.mu.Lock()
	p := tc.pending[key]
	pending := len(tc.pending)
	tc.mu.Unlock()
	if p == nil || pending != 1 {
		t.Fatalf("want one pending prebuild, have %d", pending)
	}

	got, err := tc.take(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != p.coinbase {
		t.Fatal("take did not return the prebuilt coinbase")
	}
	if err := validateCoinbaseConsensus(got.Tx, key.height); err != nil {
		t.Fatalf("prebuilt coinbase invalid at its height: %v", err)
	}

	again, err := tc.take(key)
	if err != nil {
		t.Fatal(err)
	}
	if again == got || again.TxPubKey == got.TxPubKey {
		t.Fatal("a prebuilt coinbase must not be handed out twice")
	}
}

func TestTemplateCoinbasePrebuildDropsOtherHeights(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	var tc templateCoinbases
	at := func(h uint64) coinbaseKey { return coinbaseKey{keys.SpendPubKey, keys.ViewPubKey, h} }
	tc.prebuild(at(10))
	tc.prebuild(at(11))
	tc.prebuild(at(13))

	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, ok := tc.pending[at(13)]; !ok || len(tc.pending) != 1 {
		t.Fatalf("want only height 13 pending, have %d entries", len(tc.pending))
	}
}

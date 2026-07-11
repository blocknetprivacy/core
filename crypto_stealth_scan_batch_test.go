package main

import (
	"testing"
)

// ownedOnetimePub composes the one-time public key the wallet would own for a
// given shared secret: onetime = secret*G + spendPub. This mirrors the
// receiver-side composition the scanner verifies against.
func ownedOnetimePub(t *testing.T, secret, spendPub [32]byte) [32]byte {
	t.Helper()
	pt, err := ScalarToPubKey(secret)
	if err != nil {
		t.Fatalf("ScalarToPubKey: %v", err)
	}
	onetime, err := CommitmentAdd(pt, spendPub)
	if err != nil {
		t.Fatalf("CommitmentAdd: %v", err)
	}
	return onetime
}

// TestStealthScanBatch_MatchesComposedDerivation verifies the batched FFI
// matcher produces byte-identical results to the legacy per-output composition
// (DeriveStealthSecret[Indexed] + ScalarToPubKey + CommitmentAdd) for owned and
// non-owned outputs, in indexed, legacy, and "both" modes.
func TestStealthScanBatch_MatchesComposedDerivation(t *testing.T) {
	const (
		modeLegacy  = uint32(0)
		modeIndexed = uint32(1)
		modeBoth    = uint32(2)
	)

	spend, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("spend keypair: %v", err)
	}
	view, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("view keypair: %v", err)
	}
	txKey, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("tx keypair: %v", err)
	}
	R := txKey.PublicKey

	// Receiver-side secrets the scanner should recover.
	legacySecret, err := DeriveStealthSecret(R, view.PrivateKey)
	if err != nil {
		t.Fatalf("DeriveStealthSecret: %v", err)
	}
	idxSecret2, err := DeriveStealthSecretIndexed(R, view.PrivateKey, 2)
	if err != nil {
		t.Fatalf("DeriveStealthSecretIndexed: %v", err)
	}

	ownedLegacy := ownedOnetimePub(t, legacySecret, spend.PublicKey)
	ownedIndexed2 := ownedOnetimePub(t, idxSecret2, spend.PublicKey)

	// A pubkey that is not ours.
	stranger, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("stranger keypair: %v", err)
	}
	notOwned := stranger.PublicKey

	t.Run("indexed", func(t *testing.T) {
		// ownedIndexed2 sits at output index 2; also probe a non-owned output.
		outs := [][32]byte{ownedIndexed2, notOwned}
		idxs := []uint32{2, 0}
		matched, secrets, err := StealthScanBatch(R, view.PrivateKey, spend.PublicKey, outs, idxs, modeIndexed)
		if err != nil {
			t.Fatalf("StealthScanBatch: %v", err)
		}
		if !matched[0] || secrets[0] != idxSecret2 {
			t.Fatalf("indexed owned: matched=%v secret=%x want secret %x", matched[0], secrets[0], idxSecret2)
		}
		if matched[1] {
			t.Fatalf("indexed non-owned should not match")
		}
		// An indexed-mode scan must NOT match a legacy-derived output.
		m2, _, _ := StealthScanBatch(R, view.PrivateKey, spend.PublicKey, [][32]byte{ownedLegacy}, []uint32{0}, modeIndexed)
		if m2[0] {
			t.Fatalf("indexed mode should not match a legacy output")
		}
	})

	t.Run("legacy", func(t *testing.T) {
		outs := [][32]byte{ownedLegacy, notOwned}
		idxs := []uint32{0, 1}
		matched, secrets, err := StealthScanBatch(R, view.PrivateKey, spend.PublicKey, outs, idxs, modeLegacy)
		if err != nil {
			t.Fatalf("StealthScanBatch: %v", err)
		}
		if !matched[0] || secrets[0] != legacySecret {
			t.Fatalf("legacy owned: matched=%v secret=%x want %x", matched[0], secrets[0], legacySecret)
		}
		if matched[1] {
			t.Fatalf("legacy non-owned should not match")
		}
	})

	t.Run("both", func(t *testing.T) {
		// One legacy output at index 0, one indexed output at index 2, one stranger.
		outs := [][32]byte{ownedLegacy, ownedIndexed2, notOwned}
		idxs := []uint32{0, 2, 5}
		matched, secrets, err := StealthScanBatch(R, view.PrivateKey, spend.PublicKey, outs, idxs, modeBoth)
		if err != nil {
			t.Fatalf("StealthScanBatch: %v", err)
		}
		if !matched[0] || secrets[0] != legacySecret {
			t.Fatalf("both: legacy output not matched correctly")
		}
		if !matched[1] || secrets[1] != idxSecret2 {
			t.Fatalf("both: indexed output not matched correctly")
		}
		if matched[2] {
			t.Fatalf("both: stranger should not match")
		}
	})
}

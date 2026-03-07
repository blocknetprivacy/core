package main

import (
	"testing"
)

func TestPedersenCommitmentAndRangeProof(t *testing.T) {
	amount := uint64(1_000_000)

	commit, err := CreatePedersenCommitment(amount)
	if err != nil {
		t.Fatalf("CreatePedersenCommitment: %v", err)
	}
	if err := commit.Verify(); err != nil {
		t.Fatalf("PedersenCommitment.Verify: %v", err)
	}

	proof, err := CreateRangeProof(amount, commit.Blinding)
	if err != nil {
		t.Fatalf("CreateRangeProof: %v", err)
	}
	if err := VerifyRangeProof(commit.Commitment, proof); err != nil {
		t.Fatalf("VerifyRangeProof: %v", err)
	}
}

func TestStealthAddressFlow(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	out, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}

	if !CheckStealthOutput(keys.SpendPubKey, keys.ViewPrivKey, out.TxPubKey, out.OnetimePubKey) {
		t.Fatal("CheckStealthOutput: expected output to be recognized as ours")
	}

	if _, err := DeriveStealthPrivKey(keys.SpendPrivKey, keys.ViewPrivKey, out.TxPubKey); err != nil {
		t.Fatalf("DeriveStealthPrivKey: %v", err)
	}
}

func TestIndexedStealthSecretAgreement(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	out, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}

	for _, idx := range []uint32{0, 1, 5, 100} {
		sender, err := DeriveStealthSecretSenderIndexed(out.TxPrivKey, keys.ViewPubKey, idx)
		if err != nil {
			t.Fatalf("DeriveStealthSecretSenderIndexed(idx=%d): %v", idx, err)
		}
		receiver, err := DeriveStealthSecretIndexed(out.TxPubKey, keys.ViewPrivKey, idx)
		if err != nil {
			t.Fatalf("DeriveStealthSecretIndexed(idx=%d): %v", idx, err)
		}
		if sender != receiver {
			t.Fatalf("indexed secret mismatch at index %d", idx)
		}
	}
}

func TestIndexedStealthSecretDiffersByIndex(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	out, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}

	s0, _ := DeriveStealthSecretSenderIndexed(out.TxPrivKey, keys.ViewPubKey, 0)
	s1, _ := DeriveStealthSecretSenderIndexed(out.TxPrivKey, keys.ViewPubKey, 1)
	if s0 == s1 {
		t.Fatal("different indices must produce different secrets")
	}
}

func TestSelfSendDistinctOneTimeKeys(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	out, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}

	s0, _ := DeriveStealthSecretSenderIndexed(out.TxPrivKey, keys.ViewPubKey, 0)
	s1, _ := DeriveStealthSecretSenderIndexed(out.TxPrivKey, keys.ViewPubKey, 1)

	p0, _ := ScalarToPubKey(s0)
	p1, _ := ScalarToPubKey(s1)

	otp0, _ := CommitmentAdd(p0, keys.SpendPubKey)
	otp1, _ := CommitmentAdd(p1, keys.SpendPubKey)

	if otp0 == otp1 {
		t.Fatal("self-send outputs must get distinct one-time public keys")
	}
}

func TestRingSignatureSignVerify(t *testing.T) {
	var ring [][32]byte
	var privs [][32]byte

	for i := 0; i < RingSize; i++ {
		kp, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("GenerateRistrettoKeypair(%d): %v", i, err)
		}
		ring = append(ring, kp.PublicKey)
		privs = append(privs, kp.PrivateKey)
	}

	secretIndex := 2
	msg := []byte("ring signature test message")

	sig, err := SignRing(ring, secretIndex, privs[secretIndex], msg)
	if err != nil {
		t.Fatalf("SignRing: %v", err)
	}
	if err := VerifyRing(ring, msg, sig); err != nil {
		t.Fatalf("VerifyRing: %v", err)
	}
}

func TestRingCTSignVerifyAndRejectInflation(t *testing.T) {
	ringAmount := uint64(1_000_000)

	var ringKeys [][32]byte
	var ringCommitments [][32]byte
	var ringPrivKeys [][32]byte
	var ringBlindings [][32]byte

	for i := 0; i < RingSize; i++ {
		kp, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("GenerateRistrettoKeypair(%d): %v", i, err)
		}
		ringKeys = append(ringKeys, kp.PublicKey)
		ringPrivKeys = append(ringPrivKeys, kp.PrivateKey)

		commit, err := CreatePedersenCommitment(ringAmount)
		if err != nil {
			t.Fatalf("CreatePedersenCommitment(%d): %v", i, err)
		}
		ringCommitments = append(ringCommitments, commit.Commitment)
		ringBlindings = append(ringBlindings, commit.Blinding)
	}

	secretIndex := 3
	pseudoCommit, err := CreatePedersenCommitment(ringAmount)
	if err != nil {
		t.Fatalf("CreatePedersenCommitment(pseudo): %v", err)
	}
	msg := []byte("ringct test message")

	sig, err := SignRingCT(
		ringKeys,
		ringCommitments,
		secretIndex,
		ringPrivKeys[secretIndex],
		ringBlindings[secretIndex],
		pseudoCommit.Commitment,
		pseudoCommit.Blinding,
		msg,
	)
	if err != nil {
		t.Fatalf("SignRingCT: %v", err)
	}
	if err := VerifyRingCT(ringKeys, ringCommitments, msg, sig); err != nil {
		t.Fatalf("VerifyRingCT(valid): %v", err)
	}

	// Inflation attempt: pseudo-output commits to different amount.
	wrongPseudo, err := CreatePedersenCommitment(ringAmount * 2)
	if err != nil {
		t.Fatalf("CreatePedersenCommitment(wrong pseudo): %v", err)
	}
	badSig, err := SignRingCT(
		ringKeys,
		ringCommitments,
		secretIndex,
		ringPrivKeys[secretIndex],
		ringBlindings[secretIndex],
		wrongPseudo.Commitment,
		wrongPseudo.Blinding,
		msg,
	)
	if err != nil {
		t.Fatalf("SignRingCT(bad): %v", err)
	}
	if err := VerifyRingCT(ringKeys, ringCommitments, msg, badSig); err == nil {
		t.Fatal("VerifyRingCT(bad): expected failure for mismatched pseudo-output amount")
	}
}

func TestCreateCoinbaseConsensusBlindingDoesNotFail(t *testing.T) {
	// This is a regression guard for "hash-derived blinding" scalar parsing.
	// If the underlying curve scalar parsing is too strict, CreateCoinbase can fail.
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	const iterations = 25
	for i := 0; i < iterations; i++ {
		_, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, 50_000_000, uint64(i))
		if err != nil {
			t.Fatalf("CreateCoinbase(iter=%d): %v", i, err)
		}
	}
}


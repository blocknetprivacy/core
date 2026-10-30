package main

import (
	"strings"
	"testing"

	"blocknet/wallet"
)

func mustBuildValidRingCTBindingTestTx(t *testing.T) *Transaction {
	t.Helper()

	const amount uint64 = 42
	secretIndex := 5

	secretKP, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate secret keypair: %v", err)
	}

	inputBlinding, err := GenerateBlinding()
	if err != nil {
		t.Fatalf("failed to generate input blinding: %v", err)
	}
	realInputCommitment, err := CreatePedersenCommitmentWithBlinding(amount, inputBlinding)
	if err != nil {
		t.Fatalf("failed to create real input commitment: %v", err)
	}

	pseudoBlinding, err := GenerateBlinding()
	if err != nil {
		t.Fatalf("failed to generate pseudo blinding: %v", err)
	}
	pseudoOutput, err := CreatePedersenCommitmentWithBlinding(amount, pseudoBlinding)
	if err != nil {
		t.Fatalf("failed to create pseudo-output commitment: %v", err)
	}

	keyImage, err := GenerateKeyImage(secretKP.PrivateKey)
	if err != nil {
		t.Fatalf("failed to generate key image: %v", err)
	}

	ringMembers := make([][32]byte, RingSize)
	ringCommitments := make([][32]byte, RingSize)
	for i := 0; i < RingSize; i++ {
		if i == secretIndex {
			ringMembers[i] = secretKP.PublicKey
			ringCommitments[i] = realInputCommitment
			continue
		}

		memberKP, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate ring member keypair %d: %v", i, err)
		}
		ringMembers[i] = memberKP.PublicKey

		decoyCommitment, err := CreatePedersenCommitment(uint64(i + 100))
		if err != nil {
			t.Fatalf("failed to create decoy commitment %d: %v", i, err)
		}
		ringCommitments[i] = decoyCommitment.Commitment
	}

	outKP, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate output keypair: %v", err)
	}
	rangeProof, err := CreateRangeProof(amount, pseudoBlinding)
	if err != nil {
		t.Fatalf("failed to create range proof: %v", err)
	}

	tx := &Transaction{
		Version: 1,
		Inputs: []TxInput{
			{
				KeyImage:        keyImage,
				RingMembers:     ringMembers,
				RingCommitments: ringCommitments,
				PseudoOutput:    pseudoOutput,
			},
		},
		Outputs: []TxOutput{
			{
				Commitment: pseudoOutput,
				PublicKey:  outKP.PublicKey,
				RangeProof: rangeProof.Proof,
			},
		},
		Fee: 0,
	}
	// Ensure baseline tx satisfies memo ciphertext invariants enforced at consensus boundary.
	tx.Outputs[0].EncryptedMemo[0] = 0x01

	sigHash := tx.SigningHash()
	ringSig, err := SignRingCT(
		ringMembers,
		ringCommitments,
		secretIndex,
		secretKP.PrivateKey,
		inputBlinding,
		pseudoOutput,
		pseudoBlinding,
		sigHash[:],
	)
	if err != nil {
		t.Fatalf("failed to sign RingCT input: %v", err)
	}
	tx.Inputs[0].RingSignature = ringSig.Signature

	if err := ValidateTransaction(
		tx,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	); err != nil {
		t.Fatalf("failed to build valid baseline RingCT tx: %v", err)
	}

	return tx
}

func TestIsCanonicalRingMember_RejectsReorgedOutOutput(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	makeTxWithOutput := func() (*Transaction, [32]byte, [32]byte, error) {
		pub, err := GenerateRistrettoKeypair()
		if err != nil {
			return nil, [32]byte{}, [32]byte{}, err
		}
		commit, err := GenerateRistrettoKeypair()
		if err != nil {
			return nil, [32]byte{}, [32]byte{}, err
		}
		tx := &Transaction{
			Version: 1,
			Inputs:  nil, // coinbase-like shape; sufficient for storage-level canonicality checks
			Outputs: []TxOutput{
				{
					PublicKey:  pub.PublicKey,
					Commitment: commit.PublicKey,
				},
			},
			Fee: 0,
		}
		return tx, pub.PublicKey, commit.PublicKey, nil
	}

	txA, pubA, commA, err := makeTxWithOutput()
	if err != nil {
		t.Fatalf("failed to build txA: %v", err)
	}
	blockA := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{txA},
	}
	hashA := blockA.Hash()
	txAID, err := txA.TxID()
	if err != nil {
		t.Fatalf("failed to hash txA: %v", err)
	}

	err = storage.CommitBlock(&BlockCommit{
		Block:     blockA,
		Height:    1,
		Hash:      hashA,
		Work:      2,
		IsMainTip: true,
		NewOutputs: []*UTXO{
			{
				TxID:        txAID,
				OutputIndex: 0,
				Output:      txA.Outputs[0],
				BlockHeight: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to commit blockA: %v", err)
	}

	if !chain.IsCanonicalRingMember(pubA, commA) {
		t.Fatal("expected output from current main-chain blockA to be canonical")
	}

	txB, pubB, commB, err := makeTxWithOutput()
	if err != nil {
		t.Fatalf("failed to build txB: %v", err)
	}
	blockB := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec + 1,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{txB},
	}
	hashB := blockB.Hash()

	err = storage.CommitReorg(&ReorgCommit{
		Disconnect: []*Block{blockA},
		Connect:    []*Block{blockB},
		NewTip:     hashB,
		NewHeight:  1,
		NewWork:    2,
	})
	if err != nil {
		t.Fatalf("failed to reorg from blockA to blockB: %v", err)
	}

	if chain.IsCanonicalRingMember(pubA, commA) {
		t.Fatal("expected blockA output to be non-canonical after reorg")
	}
	if !chain.IsCanonicalRingMember(pubB, commB) {
		t.Fatal("expected blockB output to be canonical after reorg")
	}
}

func TestValidateTransactionRejectsUnsupportedTxVersion(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	tx.Version = 2

	err := ValidateTransaction(
		tx,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	if err == nil {
		t.Fatal("expected unsupported tx version to be rejected")
	}
	if !strings.Contains(err.Error(), "unsupported tx version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoinbaseConsensusRejectsNonEmptyMemoPayload(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("failed to generate stealth keys: %v", err)
	}
	cb, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, GetBlockReward(1), 1)
	if err != nil {
		t.Fatalf("failed to create coinbase: %v", err)
	}

	// Create a ciphertext that decrypts to a non-empty payload under the consensus blinding.
	blinding := deriveCoinbaseConsensusBlinding(cb.Tx.TxPublicKey, 1, 0)
	enc, err := wallet.EncryptMemo([]byte("miner-note"), blinding, 0)
	if err != nil {
		t.Fatalf("failed to encrypt coinbase memo payload: %v", err)
	}
	cb.Tx.Outputs[0].EncryptedMemo = enc

	err = validateCoinbaseConsensus(cb.Tx, 1, GetBlockReward(1))
	if err == nil {
		t.Fatal("expected coinbase with non-empty memo payload to be rejected")
	}
	if !strings.Contains(err.Error(), "memo payload must be empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTransactionRejectsTamperedRingCTExternalKeyImage(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)

	original := tx.Inputs[0].KeyImage
	tx.Inputs[0].KeyImage[0] ^= 0x01
	if tx.Inputs[0].KeyImage == original {
		t.Fatal("failed to tamper key image")
	}

	err := ValidateTransaction(
		tx,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	if err == nil {
		t.Fatal("expected tampered external key image to be rejected")
	}
	if !strings.Contains(err.Error(), "key image does not match signed RingCT payload") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTransactionRejectsTamperedRingCTExternalPseudoOutput(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)

	original := tx.Inputs[0].PseudoOutput
	tx.Inputs[0].PseudoOutput[0] ^= 0x01
	if tx.Inputs[0].PseudoOutput == original {
		t.Fatal("failed to tamper pseudo-output")
	}

	err := ValidateTransaction(
		tx,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	if err == nil {
		t.Fatal("expected tampered external pseudo-output to be rejected")
	}
	if !strings.Contains(err.Error(), "pseudo-output does not match signed RingCT payload") {
		t.Fatalf("unexpected error: %v", err)
	}
}

package main

import (
	"path/filepath"
	"testing"

	"blocknet/wallet"
)

func assertAllOutputsHaveNonZeroEncryptedMemo(t *testing.T, tx *Transaction) {
	t.Helper()
	if tx == nil {
		t.Fatal("tx is nil")
	}
	if len(tx.Outputs) == 0 {
		t.Fatal("tx has no outputs")
	}
	var zero [wallet.MemoSize]byte
	for i := range tx.Outputs {
		if tx.Outputs[i].EncryptedMemo == zero {
			t.Fatalf("output %d has all-zero encrypted memo", i)
		}
	}
}

func TestMemoCiphertextPolicy_WalletBuilderTransferProducesNonZeroMemos(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	prevHash := genesis.Hash()
	var cumulativeWork uint64 = 1

	// Populate canonical blocks with enough decoy outputs for ring selection.
	for i := 0; i < RingSize*3; i++ {
		kp, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate decoy keypair %d: %v", i, err)
		}
		commit, err := CreatePedersenCommitment(uint64(i + 1000))
		if err != nil {
			t.Fatalf("failed to create decoy commitment %d: %v", i, err)
		}
		var memo [wallet.MemoSize]byte
		memo[0] = 0x01

		height := uint64(i + 1)
		tx := &Transaction{
			Version: 1,
			Outputs: []TxOutput{{
				PublicKey:     kp.PublicKey,
				Commitment:    commit.Commitment,
				EncryptedMemo: memo,
			}},
		}
		block := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     height,
				PrevHash:   prevHash,
				Timestamp:  genesis.Header.Timestamp + int64(height)*BlockIntervalSec,
				Difficulty: MinDifficulty,
			},
			Transactions: []*Transaction{tx},
		}
		blockHash := block.Hash()
		txid, _ := tx.TxID()
		cumulativeWork += MinDifficulty

		if err := storage.CommitBlock(&BlockCommit{
			Block:     block,
			Height:    height,
			Hash:      blockHash,
			Work:      cumulativeWork,
			IsMainTip: true,
			NewOutputs: []*UTXO{{
				TxID:        txid,
				OutputIndex: 0,
				Output:      tx.Outputs[0],
				BlockHeight: height,
			}},
		}); err != nil {
			t.Fatalf("failed to commit decoy block %d: %v", i, err)
		}
		prevHash = blockHash
	}

	// Update in-memory chain height to match committed blocks + maturity buffer.
	topHeight := uint64(RingSize * 3)
	chain.mu.Lock()
	chain.height = topHeight + 100
	chain.canonicalRingIndexDirty = true
	chain.mu.Unlock()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	// Add a spendable, mature owned output to fund the transfer.
	inKP, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate input keypair: %v", err)
	}
	inCommit, err := CreatePedersenCommitment(1_000_000_000) // 10 BNT in atomic units
	if err != nil {
		t.Fatalf("failed to create input commitment: %v", err)
	}
	w.AddOutput(&wallet.OwnedOutput{
		TxID:           [32]byte{0xAA},
		OutputIndex:    0,
		Amount:         1_000_000_000,
		Blinding:       inCommit.Blinding,
		OneTimePrivKey: inKP.PrivateKey,
		OneTimePubKey:  inKP.PublicKey,
		Commitment:     inCommit.Commitment,
		BlockHeight:    0,
		IsCoinbase:     false,
		Spent:          false,
	})

	// Build a tx using the real production wiring from API server.
	s := &APIServer{daemon: daemon, wallet: w}
	builder := s.createTxBuilder()

	keys := w.Keys()
	res, err := builder.Transfer([]wallet.Recipient{
		{
			SpendPubKey: keys.SpendPubKey,
			ViewPubKey:  keys.ViewPubKey,
			Amount:      1,
			Memo:        []byte("hello"),
		},
	}, 1000, chain.Height())
	if err != nil {
		t.Fatalf("wallet builder transfer failed: %v", err)
	}

	tx, err := DeserializeTx(res.TxData)
	if err != nil {
		t.Fatalf("failed to deserialize built tx: %v", err)
	}
	assertAllOutputsHaveNonZeroEncryptedMemo(t, tx)
}

func TestMemoCiphertextPolicy_LegacyTxBuilderProducesNonZeroMemos(t *testing.T) {
	utxoSet := NewUTXOSet()

	// Real input UTXO + owned secret material.
	realKP, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate real input keypair: %v", err)
	}
	realCommit, err := CreatePedersenCommitment(100)
	if err != nil {
		t.Fatalf("failed to create real commitment: %v", err)
	}
	var realTxID [32]byte
	realTxID[0] = 0x01
	realOut := TxOutput{
		PublicKey:  realKP.PublicKey,
		Commitment: realCommit.Commitment,
	}
	utxoSet.Add(realTxID, 0, realOut, 1)

	owned := &OwnedOutput{
		UTXO:     &UTXO{TxID: realTxID, OutputIndex: 0, Output: realOut, BlockHeight: 1},
		Amount:   100,
		Blinding: realCommit.Blinding,
		PrivKey:  realKP.PrivateKey,
	}

	// Add enough decoys for ring selection.
	for i := 0; i < RingSize*2; i++ {
		kp, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate decoy keypair %d: %v", i, err)
		}
		commit, err := CreatePedersenCommitment(uint64(i + 200))
		if err != nil {
			t.Fatalf("failed to create decoy commitment %d: %v", i, err)
		}
		var txid [32]byte
		txid[0] = byte(i + 2)
		utxoSet.Add(txid, 0, TxOutput{PublicKey: kp.PublicKey, Commitment: commit.Commitment}, 1)
	}

	// Build via legacy TxBuilder path (should still satisfy memo ciphertext policy).
	recipientKeys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("failed to generate recipient stealth keys: %v", err)
	}
	b := NewTxBuilder()
	b.AddInput(owned)
	b.AddOutput(recipientKeys.SpendPubKey, recipientKeys.ViewPubKey, 99)
	b.SetFee(1)

	tx, err := b.Build(utxoSet)
	if err != nil {
		t.Fatalf("legacy tx builder failed: %v", err)
	}
	assertAllOutputsHaveNonZeroEncryptedMemo(t, tx)
}

func TestMemoCiphertextPolicy_CoinbaseConstructorProducesNonZeroMemo(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("failed to generate stealth keys: %v", err)
	}
	cb, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, GetBlockReward(1), 1)
	if err != nil {
		t.Fatalf("failed to create coinbase: %v", err)
	}
	assertAllOutputsHaveNonZeroEncryptedMemo(t, cb.Tx)
}


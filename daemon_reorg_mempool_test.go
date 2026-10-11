package main

import "testing"

func TestUpdateMempoolForAcceptedMainChain_ReorgRequeuesDisconnectedAndRemovesConnected(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	makeTxWithKeyImage := func(seed byte) *Transaction {
		var ki [32]byte
		ki[0] = seed
		return &Transaction{
			Version: 1,
			Inputs: []TxInput{
				{KeyImage: ki},
			},
			Outputs: []TxOutput{
				{PublicKey: [32]byte{seed + 1}, Commitment: [32]byte{seed + 2}},
			},
			Fee: 1,
		}
	}

	txOld := makeTxWithKeyImage(0x11) // lives on old chain, should be requeued
	txNew := makeTxWithKeyImage(0x22) // lives on new chain, should be removed

	blockA1 := &Block{Header: BlockHeader{
		Version: 1, Height: 1, PrevHash: genesis.Hash(),
		Timestamp: genesis.Header.Timestamp + BlockIntervalSec, Difficulty: MinDifficulty,
	}}
	blockA2 := &Block{Header: BlockHeader{
		Version: 1, Height: 2, PrevHash: blockA1.Hash(),
		Timestamp: blockA1.Header.Timestamp + BlockIntervalSec, Difficulty: MinDifficulty,
	}, Transactions: []*Transaction{txOld}}

	blockB1 := &Block{Header: BlockHeader{
		Version: 1, Height: 1, PrevHash: genesis.Hash(),
		Timestamp: genesis.Header.Timestamp + BlockIntervalSec + 1, Difficulty: MinDifficulty,
	}}
	blockB2 := &Block{Header: BlockHeader{
		Version: 1, Height: 2, PrevHash: blockB1.Hash(),
		Timestamp: blockB1.Header.Timestamp + BlockIntervalSec + 1, Difficulty: MinDifficulty,
	}, Transactions: []*Transaction{txNew}}

	hashA1 := blockA1.Hash()
	hashA2 := blockA2.Hash()
	hashB1 := blockB1.Hash()
	hashB2 := blockB2.Hash()

	chain.mu.Lock()
	chain.blocks[hashA1] = blockA1
	chain.blocks[hashA2] = blockA2
	chain.blocks[hashB1] = blockB1
	chain.blocks[hashB2] = blockB2
	chain.byHeight[1] = hashB1
	chain.byHeight[2] = hashB2
	chain.bestHash = hashB2
	chain.height = 2
	chain.publishTipLocked()
	chain.mu.Unlock()

	txNewID, err := txNew.TxID()
	if err != nil {
		t.Fatalf("failed to hash txNew: %v", err)
	}
	txNewData := txNew.Serialize()
	d.mempool.txByID[txNewID] = &MempoolEntry{
		Tx:      txNew,
		TxID:    txNewID,
		TxData:  txNewData,
		Size:    len(txNewData),
		Fee:     txNew.Fee,
		FeeRate: 1,
		index:   -1,
	}
	d.mempool.totalSize += len(txNewData)
	for _, in := range txNew.Inputs {
		d.mempool.txByImage[in.KeyImage] = txNewID
	}

	d.updateMempoolForAcceptedMainChain(blockB2, hashA2)

	if _, exists := d.mempool.GetTransaction(txNewID); exists {
		t.Fatal("expected tx from connected new-main-chain block to be removed from mempool")
	}

	txOldID, err := txOld.TxID()
	if err != nil {
		t.Fatalf("failed to hash txOld: %v", err)
	}
	if _, exists := d.mempool.GetTransaction(txOldID); !exists {
		t.Fatal("expected tx from disconnected old-main-chain block to be requeued into mempool")
	}
}

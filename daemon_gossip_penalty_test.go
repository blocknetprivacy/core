package main

import (
	"encoding/json"
	"testing"
)

func TestHandleTxDoesNotBanForMalformedPayload(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	sender, receiver, stopNodes := mustStartLinkedTestNodes(t)
	defer stopNodes()

	d := &Daemon{
		chain:   chain,
		mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember),
		node:    receiver,
	}

	if d.handleTx(sender.PeerID(), []byte("not-a-valid-serialized-tx")) {
		t.Fatal("a malformed payload must not be relayed")
	}
	if receiver.IsBanned(sender.PeerID()) {
		t.Fatal("a relay may have forwarded this payload in good faith")
	}
}

func TestHandleBlockPenalizesCheapPrefilterFailure(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	sender, receiver, stopNodes := mustStartLinkedTestNodes(t)
	defer stopNodes()

	d := &Daemon{
		chain:   chain,
		mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember),
		node:    receiver,
	}

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
		Transactions: nil, // triggers cheap prefilter before expensive validation
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("failed to marshal block: %v", err)
	}

	d.handleBlock(sender.PeerID(), data)

	assertPeerPenalized(t, receiver, sender.PeerID(), 1)
}

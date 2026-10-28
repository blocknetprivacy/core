package main

import (
	"blocknet/p2p"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	bolt "go.etcd.io/bbolt"
)

func mustCreateTestChain(t *testing.T) (*Chain, *Storage, func()) {
	t.Helper()

	dataDir := t.TempDir()
	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}

	cleanup := func() {
		if err := chain.Close(); err != nil {
			t.Fatalf("failed to close chain: %v", err)
		}
	}

	return chain, chain.Storage(), cleanup
}

func mustAddGenesisBlock(t *testing.T, chain *Chain) {
	t.Helper()

	genesis, err := GetGenesisBlock()
	if err != nil {
		t.Fatalf("failed to load canonical genesis block: %v", err)
	}

	if err := chain.addGenesisBlock(genesis); err != nil {
		t.Fatalf("failed to add genesis block: %v", err)
	}
}

func assertTipUnchanged(t *testing.T, chain *Chain, wantHash [32]byte, wantHeight uint64) {
	t.Helper()

	if gotHeight := chain.Height(); gotHeight != wantHeight {
		t.Fatalf("tip height changed: got %d, want %d", gotHeight, wantHeight)
	}
	if gotHash := chain.BestHash(); gotHash != wantHash {
		t.Fatalf("tip hash changed: got %x, want %x", gotHash[:8], wantHash[:8])
	}

	tipHash, tipHeight, _, found := chain.Storage().GetTip()
	if !found {
		t.Fatalf("expected storage tip to exist")
	}
	if tipHeight != wantHeight {
		t.Fatalf("storage tip height changed: got %d, want %d", tipHeight, wantHeight)
	}
	if tipHash != wantHash {
		t.Fatalf("storage tip hash changed: got %x, want %x", tipHash[:8], wantHash[:8])
	}
}

func mustStartTestDaemon(t *testing.T, chain *Chain) (*Daemon, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	mempool := NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)
	miner := NewMiner(chain, mempool, MinerConfig{})

	d := &Daemon{
		chain:   chain,
		mempool: mempool,
		miner:   miner,
		ctx:     ctx,
		cancel:  cancel,
	}
	// Tests run without peers; skip the startup tip-freshness wait.
	d.tipGateOpen.Store(true)

	cleanup := func() {
		cancel()
	}

	return d, cleanup
}

func mustMakeHTTPJSONRequest(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body []byte,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func mustCraftMalformedTxVariant(t *testing.T, kind string) []byte {
	t.Helper()

	switch kind {
	case "empty-ringct-signature":
		ringMembers := make([][32]byte, RingSize)
		ringCommitments := make([][32]byte, RingSize)
		tx := &Transaction{
			Version: 1,
			Fee:     1,
			Inputs: []TxInput{
				{
					RingMembers:     ringMembers,
					RingCommitments: ringCommitments,
					RingSignature:   nil,
				},
			},
			Outputs: []TxOutput{
				{
					Commitment: [32]byte{},
					PublicKey:  [32]byte{},
				},
			},
		}
		// Ensure we hit the intended RingCT validation branch, not memo policy.
		tx.Outputs[0].EncryptedMemo[0] = 0x01
		return tx.Serialize()
	default:
		t.Fatalf("unknown malformed tx variant: %s", kind)
		return nil
	}
}

func mustStartLinkedTestNodes(t *testing.T) (*p2p.Node, *p2p.Node, func()) {
	t.Helper()

	cfg := p2p.DefaultNodeConfig()
	cfg.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.SeedNodes = nil

	a, err := p2p.NewNode(cfg)
	if err != nil {
		t.Fatalf("failed to create node A: %v", err)
	}
	b, err := p2p.NewNode(cfg)
	if err != nil {
		_ = a.Stop()
		t.Fatalf("failed to create node B: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.PeerID(), Addrs: b.Addrs()}); err != nil {
		_ = a.Stop()
		_ = b.Stop()
		t.Fatalf("failed to connect A->B: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.Host().Network().Connectedness(b.PeerID()) == network.Connected {
			cleanup := func() {
				_ = a.Stop()
				_ = b.Stop()
			}
			return a, b, cleanup
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = a.Stop()
	_ = b.Stop()
	t.Fatal("nodes failed to connect in time")
	return nil, nil, func() {}
}

func assertPeerPenalized(t *testing.T, n *p2p.Node, pid peer.ID, minPenalty int) {
	t.Helper()
	if n == nil {
		t.Fatal("node is nil")
	}
	// Severe penalties in this codebase are expected to ban.
	if !n.IsBanned(pid) {
		t.Fatalf("expected peer %s to be penalized/banned (minPenalty=%d)", pid, minPenalty)
	}
}
func makeOutputOnlyTestBlock(height uint64, prevHash [32]byte, timestamp int64, outputs []TxOutput) *Block {
	return &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     height,
			PrevHash:   prevHash,
			Timestamp:  timestamp,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Outputs: outputs,
			},
		},
	}
}

func blockOutputsForCommit(t *testing.T, block *Block) []*UTXO {
	t.Helper()

	var outputs []*UTXO
	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			t.Fatalf("failed to hash tx outputs: %v", err)
		}
		for idx, out := range tx.Outputs {
			outputs = append(outputs, &UTXO{
				TxID:        txID,
				OutputIndex: uint32(idx),
				Output:      out,
				BlockHeight: block.Header.Height,
			})
		}
	}
	return outputs
}

func commitMainChainBlockForTest(t *testing.T, chain *Chain, storage *Storage, block *Block, work uint64) {
	t.Helper()

	hash := block.Hash()
	if err := storage.CommitBlock(&BlockCommit{
		Block:      block,
		Height:     block.Header.Height,
		Hash:       hash,
		Work:       work,
		IsMainTip:  true,
		NewOutputs: blockOutputsForCommit(t, block),
	}); err != nil {
		t.Fatalf("failed to commit block at height %d: %v", block.Header.Height, err)
	}

	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.blocks[hash] = block
	chain.workAt[hash] = work
	chain.byHeight[block.Header.Height] = hash
	chain.bestHash = hash
	chain.height = block.Header.Height
	chain.totalWork = work
	chain.canonicalRingIndexDirty = true
	// The real commit paths publish the tip snapshot on every tip change, and
	// lock-free readers are served from it. A helper that skips this leaves
	// them reading height 0.
	chain.publishTipLocked()
}

func mustClearTxIndexForTest(t *testing.T, s *Storage) {
	t.Helper()

	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(bucketTxIndex); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(bucketTxIndex); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).Delete(metaKeyTxIndexBuilt)
	}); err != nil {
		t.Fatalf("failed to clear tx index: %v", err)
	}
	s.setTxIndexBuilt(false)
}

func mustEvictBlockFromChainCacheForTest(t *testing.T, chain *Chain, height uint64) {
	t.Helper()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	hash, ok := chain.byHeight[height]
	if !ok {
		t.Fatalf("height %d not in the chain height index", height)
	}
	chain.cacheForgetLocked(hash)
}

func mustPutLegacyJSONBlockForTest(t *testing.T, s *Storage, block *Block) {
	t.Helper()

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("failed to marshal legacy block: %v", err)
	}
	hash := block.Hash()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlocks).Put(hash[:], data)
	}); err != nil {
		t.Fatalf("failed to write legacy block: %v", err)
	}
}

func mustDeleteMainChainHeightForTest(t *testing.T, s *Storage, height uint64) {
	t.Helper()

	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHeights).Delete(heightKey(height))
	}); err != nil {
		t.Fatalf("failed to delete height %d: %v", height, err)
	}
}

func mustPutLegacyJSONOutputForTest(t *testing.T, s *Storage, output *UTXO) {
	t.Helper()

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal legacy output: %v", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketOutputs).Put(outpointKey(output.TxID, output.OutputIndex), data)
	}); err != nil {
		t.Fatalf("failed to write legacy output: %v", err)
	}
}

func mustSeedMempoolTx(t *testing.T, mempool *Mempool, tx *Transaction) [32]byte {
	t.Helper()

	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to hash mempool tx: %v", err)
	}
	data := tx.Serialize()

	mempool.mu.Lock()
	defer mempool.mu.Unlock()
	mempool.txByID[txID] = &MempoolEntry{
		Tx:      tx,
		TxID:    txID,
		TxData:  data,
		Fee:     tx.Fee,
		FeeRate: 1,
		Size:    len(data),
		AddedAt: time.Now(),
	}
	return txID
}

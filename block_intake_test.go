package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"blocknet/p2p"
	"blocknet/wallet"
)

func TestNullTransactionsAreRejectedWithoutPanic(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	sender, receiver, stop := mustStartLinkedTestNodes(t)
	defer stop()
	d := &Daemon{
		chain:   chain,
		mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember),
		node:    receiver,
	}

	payloads := map[string]string{
		"only":   `{"header":{"version":1,"height":1},"transactions":[null]}`,
		"second": `{"header":{"version":1,"height":1},"transactions":[{"version":1},null]}`,
		"none":   `{"header":{"version":1,"height":1},"transactions":null}`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			d.handleBlock(sender.PeerID(), []byte(payload))
			if err := d.processBlockData([]byte(payload)); err == nil {
				t.Fatal("expected the block to be rejected")
			}
		})
	}
}

func TestPrefilterRejectsNullTransactionsAtAnyIndex(t *testing.T) {
	block := &Block{Header: BlockHeader{Version: 1}, Transactions: []*Transaction{nil}}
	if err := validateBlockCheapPrefilters(block); err == nil {
		t.Fatal("expected an error for a null coinbase")
	}
	block.Transactions = []*Transaction{{Version: 1}, nil}
	if err := validateBlockCheapPrefilters(block); err == nil {
		t.Fatal("expected an error for a later null transaction")
	}
}

func intakeTemplateServer(t *testing.T, chain *Chain) (*Daemon, func(string) *httptest.ResponseRecorder, func()) {
	t.Helper()
	daemon, stop := mustStartTestDaemon(t, chain)
	daemon.syncMgr = new(p2p.SyncManager)
	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "w.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)
	h := authMiddleware("tok", mux)
	return daemon, func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.30:1234"
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}, stop
}

func intakeFillerTx(key byte, outputs, proofLen int) *Transaction {
	tx := &Transaction{Version: 2, TxPublicKey: [32]byte{key, 1}, Fee: 1 << 40}
	tx.Inputs = []TxInput{{
		KeyImage:        [32]byte{key, 2},
		RingMembers:     make([][32]byte, RingSize),
		RingCommitments: make([][32]byte, RingSize),
		RingSignature:   bytes.Repeat([]byte{1}, 96+64*RingSize),
	}}
	for i := 0; i < outputs; i++ {
		out := TxOutput{PublicKey: [32]byte{1}, Commitment: [32]byte{1}, RangeProof: bytes.Repeat([]byte{1}, proofLen)}
		for j := range out.EncryptedMemo {
			out.EncryptedMemo[j] = 1
		}
		tx.Outputs = append(tx.Outputs, out)
	}
	return tx
}

// A mempool that fills the old selection limit exactly must not produce a block
// over the consensus limit.
func TestTemplateStaysWithinTheBlockSizeLimit(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	daemon, do, stop := intakeTemplateServer(t, chain)
	defer stop()

	target := MaxBlockSize - 1000
	total := 0
	add := func(tx *Transaction) {
		data := tx.Serialize()
		id, err := tx.TxID()
		if err != nil {
			t.Fatalf("txid: %v", err)
		}
		daemon.mempool.mu.Lock()
		daemon.mempool.txByID[id] = &MempoolEntry{Tx: tx, TxID: id, TxData: data, Fee: tx.Fee, FeeRate: tx.Fee / uint64(len(data)), Size: len(data), AddedAt: time.Now()}
		daemon.mempool.totalSize += len(data)
		daemon.mempool.mu.Unlock()
		total += len(data)
	}
	key := byte(1)
	for target-total > 200000 {
		add(intakeFillerTx(key, 250, 650))
		key++
	}
	remaining := target - total
	base := len(intakeFillerTx(key, 1, 0).Serialize())
	perOutput := len(intakeFillerTx(key, 2, 0).Serialize()) - base
	last := intakeFillerTx(key, 1+(remaining-base)/(perOutput+650), 650)
	last.Outputs[0].RangeProof = bytes.Repeat([]byte{1}, 650+remaining-len(last.Serialize()))
	add(last)
	if total != target {
		t.Fatalf("mempool holds %d bytes, want %d", total, target)
	}

	rr := do("/api/mining/blocktemplate")
	if rr.Code != http.StatusOK {
		t.Fatalf("template status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Block *Block `json:"block"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if size := got.Block.Size(); size > MaxBlockSize {
		t.Fatalf("template block is %d bytes, over the %d limit", size, MaxBlockSize)
	}
	if len(got.Block.Transactions) < 2 {
		t.Fatal("the template should still carry transactions")
	}
	if err := validateBlockCheapPrefilters(got.Block); err != nil {
		t.Fatalf("template block fails the prefilter: %v", err)
	}
}

func TestTemplateOnAFutureDatedParentStaysValid(t *testing.T) {
	dir := t.TempDir()
	chain, err := NewChain(dir)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)
	st := chain.Storage()
	prevHash, _, prevWork, _ := st.GetTip()
	commit := func(b *Block) {
		t.Helper()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatalf("work: %v", err)
		}
		hash := b.Hash()
		if err := st.CommitBlock(&BlockCommit{Block: b, Height: b.Header.Height, Hash: hash, Work: work, IsMainTip: true}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		prevHash, prevWork = hash, work
	}
	ts := GenesisTimestamp
	for h := uint64(1); h <= 80; h++ {
		ts += BlockIntervalSec
		commit(&Block{Header: BlockHeader{Version: 1, Height: h, PrevHash: prevHash, MerkleRoot: [32]byte{0xB0, byte(h)}, Timestamp: ts, Difficulty: MinDifficulty}})
	}
	future := time.Now().Add(TimestampFuturLimit - time.Minute).Unix()
	commit(precheckCandidate(t, chain, prevHash, 81, future))
	if err := chain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	chain, err = NewChain(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer chain.Close()

	_, do, stop := intakeTemplateServer(t, chain)
	defer stop()
	rr := do("/api/mining/blocktemplate")
	if rr.Code != http.StatusOK {
		t.Fatalf("template status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Block *Block `json:"block"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Block.Header.Timestamp != future+1 {
		t.Fatalf("template timestamp %d, want parent+1 = %d", got.Block.Header.Timestamp, future+1)
	}
	if !chain.passesCheapPrecheck(got.Block) {
		t.Fatal("a block built on this template would be rejected")
	}
}

func TestTemplateTimestampUsesNowWhenTheParentIsOlder(t *testing.T) {
	chain, tip, tipTime := newPrecheckTestChain(t, 20)
	now := tipTime + 1000
	if got := chain.templateTimestamp(tip, now); got != now {
		t.Fatalf("got %d, want now %d", got, now)
	}
	if got := chain.templateTimestamp(tip, tipTime-500); got != tipTime+1 {
		t.Fatalf("got %d, want parent+1 %d", got, tipTime+1)
	}
	if got := chain.templateTimestamp([32]byte{1}, now); got != now {
		t.Fatalf("unknown parent: got %d, want now %d", got, now)
	}
}

package main

import (
	"container/heap"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"blocknet/p2p"
	"blocknet/protocol/params"
	"blocknet/wallet"
)

func setFaradayHeight(t *testing.T, h uint64) {
	t.Helper()
	old := params.FaradayHeight
	params.FaradayHeight = h
	t.Cleanup(func() { params.FaradayHeight = old })
}

func TestUpgradeActive(t *testing.T) {
	cases := []struct {
		height, activation uint64
		want               bool
	}{{0, 0, false}, {500, 0, false}, {99, 100, false}, {100, 100, true}, {101, 100, true}}
	for _, tc := range cases {
		if got := upgradeActive(tc.height, tc.activation); got != tc.want {
			t.Errorf("upgradeActive(%d, %d) = %v, want %v", tc.height, tc.activation, got, tc.want)
		}
	}
}

func TestFaradayHeightIsTheSingleSource(t *testing.T) {
	if params.FaradayHeight != 103680 {
		t.Fatalf("mainnet Faraday height %d, want 103680", params.FaradayHeight)
	}
	setFaradayHeight(t, 200)
	if faradayBlocksLeft(150) != 50 || faradayBlocksLeft(200) != 0 {
		t.Fatal("the explorer countdown does not follow params.FaradayHeight")
	}
	if coinbaseClaimsFees(199) || !coinbaseClaimsFees(200) {
		t.Fatal("the fee rule does not follow params.FaradayHeight")
	}
}

func TestCoinbaseAmountClaimsFeesFromTheForkHeight(t *testing.T) {
	setFaradayHeight(t, 100)
	if got, _ := coinbaseAmount(99, 5000); got != GetBlockReward(99) {
		t.Fatalf("before the fork the coinbase claimed %d, want the bare reward %d", got, GetBlockReward(99))
	}
	if got, _ := coinbaseAmount(100, 5000); got != GetBlockReward(100)+5000 {
		t.Fatalf("at the fork the coinbase claimed %d, want reward + fees %d", got, GetBlockReward(100)+5000)
	}
	if _, err := coinbaseAmount(100, math.MaxUint64); err == nil {
		t.Fatal("reward + fees overflow was not refused")
	}
	if _, err := sumTxFees([]*Transaction{{Fee: math.MaxUint64}, {Fee: 1}}); err == nil {
		t.Fatal("fee sum overflow was not refused")
	}
}

func feeBlock(t *testing.T, height, coinbaseAmount uint64, fees ...uint64) *Block {
	t.Helper()
	kp, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CreateCoinbase(kp.PublicKey, kp.PublicKey, coinbaseAmount, height)
	if err != nil {
		t.Fatal(err)
	}
	b := &Block{Header: BlockHeader{Height: height}, Transactions: []*Transaction{cb.Tx}}
	for _, f := range fees {
		b.Transactions = append(b.Transactions, &Transaction{Fee: f})
	}
	return b
}

func TestBlockCoinbaseRuleOnBothSidesOfTheFork(t *testing.T) {
	setFaradayHeight(t, 100)
	const fees = 3000 + 4000
	cases := []struct {
		name    string
		height  uint64
		claim   uint64
		wantErr bool
	}{
		{"before fork, bare reward", 99, GetBlockReward(99), false},
		{"before fork, claiming fees", 99, GetBlockReward(99) + fees, true},
		{"at fork, reward plus fees", 100, GetBlockReward(100) + fees, false},
		{"at fork, bare reward", 100, GetBlockReward(100), true},
		{"at fork, one too many", 100, GetBlockReward(100) + fees + 1, true},
		{"at fork, one too few", 100, GetBlockReward(100) + fees - 1, true},
		{"after fork, reward plus fees", 5000, GetBlockReward(5000) + fees, false},
	}
	for _, tc := range cases {
		err := validateBlockCoinbase(feeBlock(t, tc.height, tc.claim, 3000, 4000))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: error %v, want error %v", tc.name, err, tc.wantErr)
		}
	}
	if err := validateBlockCoinbase(feeBlock(t, 100, GetBlockReward(100), 0, 0)); err != nil {
		t.Errorf("a block with no fees at the fork: %v", err)
	}
}

func TestBlockTemplateCoinbasePaysFeesAfterTheFork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		faraday  uint64
		withFees bool
	}{{"before fork", 1000, false}, {"at fork", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			setFaradayHeight(t, tc.faraday)
			chain, _, cleanup := mustCreateTestChain(t)
			defer cleanup()
			mustAddGenesisBlock(t, chain)
			daemon, stop := mustStartTestDaemon(t, chain)
			defer stop()
			daemon.syncMgr = new(p2p.SyncManager)
			w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "w.dat"), []byte("pw"), defaultWalletConfig())
			if err != nil {
				t.Fatal(err)
			}
			const fee = 123_456
			entry := &MempoolEntry{Tx: &Transaction{Version: 1, Fee: fee}, TxID: [32]byte{7}, Fee: fee, FeeRate: 50, Size: 1000}
			daemon.mempool.mu.Lock()
			daemon.mempool.txByID[entry.TxID] = entry
			heap.Push(&daemon.mempool.priorityQueue, entry)
			daemon.mempool.mu.Unlock()

			api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/mining/blocktemplate", nil)
			req.RemoteAddr = "198.51.100.40:1234"
			api.handleBlockTemplate(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("template: %d %s", rr.Code, rr.Body.String())
			}
			var got struct {
				Block *Block `json:"block"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			h := got.Block.Header.Height
			want := GetBlockReward(h)
			if tc.withFees {
				want += fee
			}
			if err := validateCoinbaseConsensus(got.Block.Transactions[0], h, want); err != nil {
				t.Fatalf("template coinbase does not pay %d: %v", want, err)
			}
			if err := validateBlockCoinbase(got.Block); err != nil {
				t.Fatalf("template block fails the coinbase rule: %v", err)
			}
			sc := wallet.NewScanner(w, defaultScannerConfig())
			sc.ScanBlock(mustBlockToWalletScanData(t, got.Block))
			outs := w.AllOutputs()
			if len(outs) != 1 || outs[0].Amount != want {
				t.Fatalf("the wallet reads the coinbase as %v, want one output of %d", outs, want)
			}
			if coinbaseReward(got.Block) != want {
				t.Fatalf("reported reward %d, want %d", coinbaseReward(got.Block), want)
			}
		})
	}
}

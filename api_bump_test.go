package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"blocknet/wallet"
)

type bumpFixture struct {
	api *APIServer
	w   *wallet.Wallet
	d   *Daemon
	n   int
}

func newBumpFixture(t *testing.T, amounts ...uint64) *bumpFixture {
	t.Helper()
	chain, storage, cleanup := mustCreateTestChain(t)
	t.Cleanup(cleanup)
	mustAddGenesisBlock(t, chain)
	chain.mu.Lock()
	chain.height = 100
	chain.publishTipLocked()
	chain.mu.Unlock()
	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatal(err)
	}
	seedCanonicalSpendableOutputs(t, chain, storage, w, amounts...)
	d := &Daemon{chain: chain, mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)}
	return &bumpFixture{api: NewAPIServer(d, w, nil, t.TempDir(), []byte("pw")), w: w, d: d}
}

func (f *bumpFixture) post(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	f.n++
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100." + strconv.Itoa(f.n) + ":1234"
	rr := httptest.NewRecorder()
	switch path {
	case "/api/wallet/send":
		f.api.handleSend(rr, req)
	case "/api/wallet/send/bump":
		f.api.handleSendBump(rr, req)
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func (f *bumpFixture) send(t *testing.T, amount uint64) string {
	t.Helper()
	code, out := f.post(t, "/api/wallet/send", `{"recipients":[{"address":"`+f.w.Address()+`","amount":`+strconv.FormatUint(amount, 10)+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("send: %d %v", code, out)
	}
	return out["txid"].(string)
}

func (f *bumpFixture) bump(t *testing.T, txid, extra string, dryRun bool) (int, map[string]any) {
	t.Helper()
	return f.post(t, "/api/wallet/send/bump", `{"txid":"`+txid+`","extra_fee_per_byte":`+extra+`,"dry_run":`+strconv.FormatBool(dryRun)+`}`)
}

func mustTxID(t *testing.T, s string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		t.Fatalf("bad txid %q", s)
	}
	var id [32]byte
	copy(id[:], raw)
	return id
}

func TestBumpReplacesStuckSendAndKeepsRings(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	oldID := mustTxID(t, oldHex)
	oldTx, oldFee, oldSize, _ := f.d.mempool.GetEntry(oldID)

	code, out := f.bump(t, oldHex, "10", false)
	if code != http.StatusOK {
		t.Fatalf("bump: %d %v", code, out)
	}
	newID := mustTxID(t, out["txid"].(string))
	if f.d.mempool.HasTransaction(oldID) || !f.d.mempool.HasTransaction(newID) || f.d.mempool.Size() != 1 {
		t.Fatal("mempool did not swap the old tx for the bump")
	}
	newTx, newFee, newSize, _ := f.d.mempool.GetEntry(newID)
	if newFee/uint64(newSize) < oldFee/uint64(oldSize)+sendFeePerByte {
		t.Fatalf("bump rate %d, old rate %d", newFee/uint64(newSize), oldFee/uint64(oldSize))
	}
	if uint64(out["old_fee"].(float64)) != oldFee || uint64(out["bump_cost"].(float64)) != newFee-oldFee {
		t.Fatalf("response fees: %v", out)
	}
	if len(newTx.Inputs) != len(oldTx.Inputs) {
		t.Fatalf("inputs %d, want %d", len(newTx.Inputs), len(oldTx.Inputs))
	}
	for i := range oldTx.Inputs {
		if newTx.Inputs[i].KeyImage != oldTx.Inputs[i].KeyImage {
			t.Fatal("bump spends different coins")
		}
		for j := range oldTx.Inputs[i].RingMembers {
			if newTx.Inputs[i].RingMembers[j] != oldTx.Inputs[i].RingMembers[j] || newTx.Inputs[i].RingCommitments[j] != oldTx.Inputs[i].RingCommitments[j] {
				t.Fatalf("input %d ring member %d changed", i, j)
			}
		}
	}

	oldRec, newRec := f.w.GetSendRecord(oldID), f.w.GetSendRecord(newID)
	if oldRec == nil || newRec == nil || oldRec.ReplacedBy == nil || *oldRec.ReplacedBy != newID || newRec.Replaces == nil || *newRec.Replaces != oldID || newRec.Fee != newFee {
		t.Fatalf("records: old %+v new %+v", oldRec, newRec)
	}
	if spends := f.w.UnconfirmedSpendsOf(newID); len(spends) != len(newTx.Inputs) || len(f.w.UnconfirmedSpendsOf(oldID)) != 0 {
		t.Fatal("spent coins did not move to the bump")
	}

	rr := httptest.NewRecorder()
	f.api.handleSends(rr, httptest.NewRequest(http.MethodGet, "/api/wallet/sends", nil))
	var list struct {
		Sends []struct {
			TxID       string `json:"txid"`
			Bumps      int    `json:"bumps"`
			ChainState string `json:"chain_state"`
		} `json:"sends"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Sends) != 1 || list.Sends[0].TxID != out["txid"] || list.Sends[0].Bumps != 1 || list.Sends[0].ChainState != "mempool" {
		t.Fatalf("sends list: %+v", list.Sends)
	}

	code, again := f.bump(t, oldHex, "40", false)
	if code != http.StatusConflict || !strings.Contains(again["error"].(string), "already sped up") {
		t.Fatalf("bumping the replaced txid: %d %v", code, again)
	}
	code, twice := f.bump(t, out["txid"].(string), "40", false)
	if code != http.StatusOK {
		t.Fatalf("second bump: %d %v", code, twice)
	}
}

func TestBumpDryRunChangesNothing(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	code, out := f.bump(t, oldHex, "40", true)
	if code != http.StatusOK || out["dry_run"] != true || out["bump_cost"].(float64) <= 0 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if !f.d.mempool.HasTransaction(mustTxID(t, oldHex)) || f.d.mempool.Size() != 1 || f.w.GetSendRecord(mustTxID(t, oldHex)).ReplacedBy != nil {
		t.Fatal("dry run changed the mempool or the wallet")
	}
	if code, out := f.bump(t, oldHex, "10", false); code != http.StatusOK {
		t.Fatalf("real bump after dry run: %d %v", code, out)
	}
}

func TestBumpAddsACoinWhenChangeIsTooSmall(t *testing.T) {
	f := newBumpFixture(t, 30_000_000, 30_000_000)
	oldHex := f.send(t, 29_940_000)
	oldTx, _, _, _ := f.d.mempool.GetEntry(mustTxID(t, oldHex))
	code, out := f.bump(t, oldHex, "40", false)
	if code != http.StatusOK {
		t.Fatalf("bump: %d %v", code, out)
	}
	newTx, _, _, _ := f.d.mempool.GetEntry(mustTxID(t, out["txid"].(string)))
	if len(newTx.Inputs) != len(oldTx.Inputs)+1 {
		t.Fatalf("bump has %d inputs, want %d", len(newTx.Inputs), len(oldTx.Inputs)+1)
	}
	for _, old := range oldTx.Inputs {
		found := false
		for _, in := range newTx.Inputs {
			if in.KeyImage == old.KeyImage {
				found = true
				for j := range old.RingMembers {
					if in.RingMembers[j] != old.RingMembers[j] {
						t.Fatal("old input ring changed when a coin was added")
					}
				}
			}
		}
		if !found {
			t.Fatal("bump dropped an old coin")
		}
	}
}

func TestBumpRefusals(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"bad txid", `{"txid":"xyz"}`, http.StatusBadRequest, "64 hex"},
		{"unknown txid", `{"txid":"` + strings.Repeat("ab", 32) + `"}`, http.StatusNotFound, "no send"},
		{"bad extra", `{"txid":"` + oldHex + `","extra_fee_per_byte":1.5}`, http.StatusBadRequest, "whole number"},
	}
	for _, tc := range cases {
		code, out := f.post(t, "/api/wallet/send/bump", tc.body)
		if code != tc.code || !strings.Contains(out["error"].(string), tc.want) {
			t.Fatalf("%s: %d %v", tc.name, code, out)
		}
	}

	f.d.mempool.RemoveTransaction(mustTxID(t, oldHex))
	code, out := f.bump(t, oldHex, "10", false)
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "not in the mempool") {
		t.Fatalf("not in mempool: %d %v", code, out)
	}
}

func TestBumpRefusesHandleSendWithoutSavedAddress(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	rec := f.w.GetSendRecord(mustTxID(t, oldHex))
	rec.Recipients[0].Address = "$alice"
	rec.Recipients[0].ResolvedAddress = ""
	code, out := f.bump(t, oldHex, "10", false)
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "handle") {
		t.Fatalf("handle send: %d %v", code, out)
	}
	rec.Recipients[0].ResolvedAddress = f.w.Address()
	if code, out := f.bump(t, oldHex, "10", false); code != http.StatusOK {
		t.Fatalf("handle send with a saved address: %d %v", code, out)
	}
}

func TestSendsListShowsOldTxWhenItWinsAfterBump(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	oldID := mustTxID(t, oldHex)
	oldTx, _, _, _ := f.d.mempool.GetEntry(oldID)
	oldData := oldTx.Serialize()
	_, out := f.bump(t, oldHex, "10", false)
	newID := mustTxID(t, out["txid"].(string))

	f.d.mempool.RemoveTransaction(newID)
	if err := f.d.mempool.AddTransaction(oldTx, oldData); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	f.api.handleSends(rr, httptest.NewRequest(http.MethodGet, "/api/wallet/sends", nil))
	if !strings.Contains(rr.Body.String(), `"txid":"`+oldHex+`"`) || !strings.Contains(rr.Body.String(), `"chain_state":"mempool"`) {
		t.Fatalf("sends list after the old tx won: %s", rr.Body.String())
	}

	f.w.ReconcileUnconfirmedSpends(f.d.mempool.HasTransaction)
	if f.w.PendingUnconfirmedBalance() != 0 {
		t.Fatalf("pending change of the dropped bump stayed: %d", f.w.PendingUnconfirmedBalance())
	}
}

func TestBumpRaisesTooLowRequestToTheMinimum(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	_, oldFee, oldSize, _ := f.d.mempool.GetEntry(mustTxID(t, oldHex))
	code, out := f.bump(t, oldHex, "0", false)
	if code != http.StatusOK {
		t.Fatalf("bump at normal: %d %v", code, out)
	}
	want := (oldFee+uint64(oldSize)-1)/uint64(oldSize) + sendFeePerByte
	if uint64(out["fee_per_byte"].(float64)) != want {
		t.Fatalf("bump rate %v, want %d", out["fee_per_byte"], want)
	}
}

func mempoolSubmit(m *Mempool) func([]byte) error {
	return func(data []byte) error {
		tx, err := DeserializeTx(data)
		if err != nil {
			return err
		}
		return m.AddTransaction(tx, data)
	}
}

func (f *bumpFixture) restart() *Mempool {
	f.d.mempool = NewMempool(DefaultMempoolConfig(), f.d.chain.IsKeyImageSpent, f.d.chain.IsCanonicalRingMember)
	return f.d.mempool
}

func TestPendingSendsComeBackAfterRestart(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	txHex := f.send(t, 20_000_000)
	id := mustTxID(t, txHex)
	m := f.restart()
	if n := restorePendingSends(mempoolSubmit(m), m.HasTransaction, f.w); n != 1 || !m.HasTransaction(id) {
		t.Fatalf("restored %d, in mempool %v", n, m.HasTransaction(id))
	}
	f.w.ReconcileUnconfirmedSpends(m.HasTransaction)
	if len(f.w.UnconfirmedSpendsOf(id)) == 0 || f.w.PendingUnconfirmedBalance() == 0 {
		t.Fatal("the restored send lost its spent coins or pending change")
	}
	if n := restorePendingSends(mempoolSubmit(m), m.HasTransaction, f.w); n != 0 {
		t.Fatalf("restored %d again, want 0", n)
	}
}

func TestOnlyTheNewestBumpComesBackAfterRestart(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	oldHex := f.send(t, 20_000_000)
	_, out := f.bump(t, oldHex, "10", false)
	newID := mustTxID(t, out["txid"].(string))
	m := f.restart()
	if n := restorePendingSends(mempoolSubmit(m), m.HasTransaction, f.w); n != 1 || !m.HasTransaction(newID) || m.HasTransaction(mustTxID(t, oldHex)) {
		t.Fatalf("restored %d; new present %v", n, m.HasTransaction(newID))
	}
}

func TestRestoreClearsDeadSendsAndKeepsOnesThatMayStillFit(t *testing.T) {
	f := newBumpFixture(t, 30_000_000)
	f.send(t, 20_000_000)

	full := func([]byte) error { return errMempoolFullForTest }
	if n := restorePendingSends(full, func([32]byte) bool { return false }, f.w); n != 0 || len(f.w.PendingRawSends()) != 1 {
		t.Fatal("a full mempool cleared the saved send")
	}
	invalid := func([]byte) error { return errSpentForTest }
	restorePendingSends(invalid, func([32]byte) bool { return false }, f.w)
	if len(f.w.PendingRawSends()) != 0 {
		t.Fatal("a send whose coins are spent stayed saved")
	}

	g := newBumpFixture(t, 30_000_000)
	gid := mustTxID(t, g.send(t, 20_000_000))
	g.w.GetSendRecord(gid).Timestamp -= int64(DefaultMempoolConfig().ExpirationTime.Seconds()) + 60
	submitted := false
	restorePendingSends(func([]byte) error { submitted = true; return nil }, func([32]byte) bool { return false }, g.w)
	if submitted || len(g.w.PendingRawSends()) != 0 {
		t.Fatal("an expired send was submitted or kept")
	}
}

var (
	errMempoolFullForTest = errString("mempool rejected: mempool full")
	errSpentForTest       = errString("mempool rejected: validation failed: input 0: key image already spent (double-spend)")
)

type errString string

func (e errString) Error() string { return string(e) }

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

const (
	explorerStatsMaxPoints      = 1000
	explorerStatsMaxTraversal   = 20000
	explorerStatsRefreshEvery   = 30 * time.Second
	explorerStatsStaleAfter     = 90 * time.Second
	explorerHashrateSampleCount = 10
)

// Explorer serves the block explorer web interface
type Explorer struct {
	daemon      *Daemon
	mux         *http.ServeMux
	testnetAddr string // address of the local testnet explorer (e.g. "127.0.0.1:18080")

	statsMu         sync.RWMutex
	statsSnapshot   explorerStatsSnapshot
	statsReady      bool
	statsRefreshing bool

	supplyMu      sync.Mutex
	supplyHeight  uint64
	supplyEmitted uint64
}

type chartPoint struct {
	H  uint64 `json:"h"`
	D  uint64 `json:"d"`
	N  uint64 `json:"n"`
	Tx int    `json:"tx"`
	S  int    `json:"s"`
	Bt int64  `json:"bt"`
}

type explorerStatsSnapshot struct {
	Height       uint64
	Difficulty   uint64
	Hashrate     string
	AvgBlockTime string
	TotalTx      int
	Emitted      string
	Remaining    string
	PctEmitted   string
	Peers        int
	DataJSON     template.JS
	GenesisTs    int64
	ComputedAt   time.Time
}

// NewExplorer creates a new explorer server
func NewExplorer(daemon *Daemon) *Explorer {
	e := &Explorer{
		daemon:      daemon,
		mux:         http.NewServeMux(),
		testnetAddr: "127.0.0.1:18080",
	}
	e.mux.HandleFunc("/", e.handleIndex)
	e.mux.HandleFunc("/block/", e.handleBlock)
	e.mux.HandleFunc("/tx/", e.handleTx)
	e.mux.HandleFunc("/search", e.handleSearch)
	e.mux.HandleFunc("/stats", e.handleStats)
	e.mux.HandleFunc("/prove", e.handleProve)
	e.mux.HandleFunc("/prove-send", e.handleProveSend)
	e.startStatsPrecompute()
	return e
}

// ServeHTTP implements http.Handler.
// On mainnet, if the client has a net=testnet cookie, the request is
// reverse-proxied to the co-located testnet explorer daemon.
func (e *Explorer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !params.IsTestnet {
		if c, err := r.Cookie("net"); err == nil && c.Value == "testnet" {
			e.proxyToTestnet(w, r)
			return
		}
	}
	e.mux.ServeHTTP(w, r)
}

func (e *Explorer) proxyToTestnet(w http.ResponseWriter, r *http.Request) {
	u := "http://" + e.testnetAddr + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	resp, err := http.Get(u)
	if err != nil {
		http.Error(w, "testnet explorer unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "testnet explorer read error", http.StatusBadGateway)
		return
	}
	// Inject our egg script so the toggle works even if the testnet
	// daemon is running an older build with stale JS/templates.
	fix := explorerProxyFixScript + "\n" + explorerEggScript
	body = bytes.Replace(body, []byte("</body>"), []byte(fix+"\n</body>"), 1)
	for k, vals := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func (e *Explorer) httpServer(addr string) *http.Server {
	handler := maxBodySize(e, maxRequestBodyBytes)
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// Start starts the explorer HTTP server
func (e *Explorer) Start(addr string) error {
	return e.httpServer(addr).ListenAndServe()
}

// Supply info
func (e *Explorer) getSupplyInfo() (emitted, remaining uint64, pctEmitted float64) {
	height := e.daemon.chain.Height()

	e.supplyMu.Lock()
	defer e.supplyMu.Unlock()

	// Handle chain rewind/reorg conservatively by rebuilding from zero.
	if e.supplyHeight > height {
		e.supplyHeight = 0
		e.supplyEmitted = 0
	}
	for h := e.supplyHeight + 1; h <= height; h++ {
		e.supplyEmitted += GetBlockReward(h)
	}
	e.supplyHeight = height
	emitted = e.supplyEmitted

	// Target supply is ~100M coins (before tail emission)
	targetSupply := uint64(100_000_000 * 100_000_000) // in smallest units
	if emitted >= targetSupply {
		remaining = 0
		pctEmitted = 100.0
	} else {
		remaining = targetSupply - emitted
		pctEmitted = float64(emitted) / float64(targetSupply) * 100
	}
	return
}

func (e *Explorer) startStatsPrecompute() {
	e.refreshStatsSnapshotAsync()
	go func() {
		ticker := time.NewTicker(explorerStatsRefreshEvery)
		defer ticker.Stop()
		for range ticker.C {
			e.refreshStatsSnapshotAsync()
		}
	}()
}

func (e *Explorer) getStatsSnapshot() (explorerStatsSnapshot, bool) {
	e.statsMu.RLock()
	defer e.statsMu.RUnlock()
	return e.statsSnapshot, e.statsReady
}

func (e *Explorer) shouldRefreshStats(s explorerStatsSnapshot) bool {
	currentHeight := e.daemon.chain.Height()
	if s.Height != currentHeight {
		return true
	}
	return time.Since(s.ComputedAt) > explorerStatsStaleAfter
}

func (e *Explorer) refreshStatsSnapshotAsync() {
	e.statsMu.Lock()
	if e.statsRefreshing {
		e.statsMu.Unlock()
		return
	}
	e.statsRefreshing = true
	e.statsMu.Unlock()

	go func() {
		snapshot := e.buildStatsSnapshot()
		e.statsMu.Lock()
		e.statsSnapshot = snapshot
		e.statsReady = true
		e.statsRefreshing = false
		e.statsMu.Unlock()
	}()
}

func (e *Explorer) refreshStatsSnapshotSync() explorerStatsSnapshot {
	snapshot := e.buildStatsSnapshot()
	e.statsMu.Lock()
	e.statsSnapshot = snapshot
	e.statsReady = true
	e.statsRefreshing = false
	e.statsMu.Unlock()
	return snapshot
}

func (e *Explorer) buildStatsSnapshot() explorerStatsSnapshot {
	chain := e.daemon.chain
	height := chain.Height()

	startHeight := uint64(0)
	if height+1 > explorerStatsMaxTraversal {
		startHeight = height + 1 - explorerStatsMaxTraversal
	}

	sampleSpan := height - startHeight + 1
	step := uint64(1)
	if sampleSpan > uint64(explorerStatsMaxPoints) {
		step = sampleSpan / uint64(explorerStatsMaxPoints)
		if step == 0 {
			step = 1
		}
	}

	var points []chartPoint
	var totalTx int
	var btSum float64
	var btCount int

	prevTs := int64(0)
	if startHeight > 0 {
		prevBlock := chain.GetBlockByHeight(startHeight - 1)
		if prevBlock != nil {
			prevTs = prevBlock.Header.Timestamp
		}
	}

	for h := startHeight; h <= height; h++ {
		block := chain.GetBlockByHeight(h)
		if block == nil {
			continue
		}
		totalTx += len(block.Transactions)

		bt := int64(0)
		if prevTs > 0 {
			bt = block.Header.Timestamp - prevTs
			if bt > 0 {
				btSum += float64(bt)
				btCount++
			}
		}
		prevTs = block.Header.Timestamp

		if (h-startHeight)%step == 0 || h == height {
			points = append(points, chartPoint{
				H:  h,
				D:  block.Header.Difficulty,
				N:  block.Header.Nonce,
				Tx: len(block.Transactions),
				S:  block.Size(),
				Bt: bt,
			})
		}
	}

	jsonData, _ := json.Marshal(points)

	var avgBt float64
	if btCount > 0 {
		avgBt = btSum / float64(btCount)
	}

	var hashrate float64
	if height >= 2 {
		var totalTime int64
		var count int
		for h := height; h > 0 && count < explorerHashrateSampleCount; h-- {
			block := chain.GetBlockByHeight(h)
			prevBlock := chain.GetBlockByHeight(h - 1)
			if block != nil && prevBlock != nil {
				blockTime := block.Header.Timestamp - prevBlock.Header.Timestamp
				if blockTime > 0 {
					totalTime += blockTime
					count++
				}
			}
		}
		if count > 0 && totalTime > 0 {
			hashrate = float64(chain.NextDifficulty()) / (float64(totalTime) / float64(count))
		}
	}

	emitted, remaining, pctEmitted := e.getSupplyInfo()
	genesisTs := int64(0)
	if genesis := chain.GetBlockByHeight(0); genesis != nil {
		genesisTs = genesis.Header.Timestamp
	}

	return explorerStatsSnapshot{
		Height:       height,
		Difficulty:   chain.NextDifficulty(),
		Hashrate:     fmt.Sprintf("%.2f", hashrate),
		AvgBlockTime: fmt.Sprintf("%.0f", avgBt),
		TotalTx:      totalTx,
		Emitted:      fmtAmountComma(emitted),
		Remaining:    fmtAmountComma(remaining),
		PctEmitted:   fmt.Sprintf("%.4f", pctEmitted),
		Peers: func() int {
			if e.daemon == nil || e.daemon.node == nil {
				return 0
			}
			return len(e.daemon.node.Peers())
		}(),
		DataJSON:   template.JS(jsonData),
		GenesisTs:  genesisTs,
		ComputedAt: time.Now(),
	}
}

func (e *Explorer) handleIndex(w http.ResponseWriter, r *http.Request) {
	chain := e.daemon.chain
	height := chain.Height()

	// Get recent blocks
	type blockSummary struct {
		Height     uint64
		Hash       string
		Time       string
		Ago        string
		TxCount    int
		Difficulty uint64
	}

	var blocks []blockSummary

	for h := height; h > 0 && len(blocks) < 20; h-- {
		block := chain.GetBlockByHeight(h)
		if block == nil {
			continue
		}
		hash := block.Hash()
		blocks = append(blocks, blockSummary{
			Height:     h,
			Hash:       fmt.Sprintf("%x", hash[:]),
			Time:       time.Unix(block.Header.Timestamp, 0).UTC().Format("2006-01-02 15:04:05"),
			Ago:        timeAgo(block.Header.Timestamp),
			TxCount:    len(block.Transactions),
			Difficulty: block.Header.Difficulty,
		})
	}

	emitted, remaining, pctEmitted := e.getSupplyInfo()

	// Get mempool transactions
	type mempoolTx struct {
		Hash    string
		Fee     float64
		FeeRate uint64
		Size    int
		Ago     string
	}
	var mempoolTxs []mempoolTx
	mempoolEntries := e.daemon.mempool.GetAllEntries()
	for _, entry := range mempoolEntries {
		mempoolTxs = append(mempoolTxs, mempoolTx{
			Hash:    fmt.Sprintf("%x", entry.TxID[:]),
			Fee:     float64(entry.Fee) / 100_000_000,
			FeeRate: entry.FeeRate,
			Size:    entry.Size,
			Ago:     timeAgo(entry.AddedAt.Unix()),
		})
	}

	snapshot, ready := e.getStatsSnapshot()
	if !ready {
		snapshot = e.refreshStatsSnapshotSync()
	} else if e.shouldRefreshStats(snapshot) {
		e.refreshStatsSnapshotAsync()
	}

	data := map[string]interface{}{
		"IsTestnet":   params.IsTestnet,
		"Height":      height,
		"Difficulty":  snapshot.Difficulty,
		"Peers":       snapshot.Peers,
		"Hashrate":    snapshot.Hashrate,
		"Emitted":     fmtAmountComma(emitted),
		"Remaining":   fmtAmountComma(remaining),
		"PctEmitted":  fmt.Sprintf("%.4f", pctEmitted),
		"TailStarted": pctEmitted >= 100,
		"MempoolTxs":  mempoolTxs,
		"Blocks":      blocks,
	}

	renderTemplate(w, explorerIndexTmpl, data)
}

func (e *Explorer) handleBlock(w http.ResponseWriter, r *http.Request) {
	// Extract block ID from path: /block/{id}
	path := strings.TrimPrefix(r.URL.Path, "/block/")
	if path == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	chain := e.daemon.chain
	var block *Block

	// Try as height first
	if height, parseErr := strconv.ParseUint(path, 10, 64); parseErr == nil {
		block = chain.GetBlockByHeight(height)
	} else {
		// Try as hash
		if len(path) >= 64 {
			hashBytes, err := hex.DecodeString(path[:64])
			if err == nil && len(hashBytes) == 32 {
				var hash [32]byte
				copy(hash[:], hashBytes)
				block = chain.GetBlock(hash)
			}
		}
	}

	if block == nil {
		http.Error(w, "Block not found", http.StatusNotFound)
		return
	}

	hash := block.Hash()

	type txSummary struct {
		Hash       string
		IsCoinbase bool
		Inputs     int
		Outputs    int
	}

	var txs []txSummary
	for i, tx := range block.Transactions {
		txHash, _ := tx.TxID()
		txs = append(txs, txSummary{
			Hash:       fmt.Sprintf("%x", txHash[:]),
			IsCoinbase: i == 0 && block.Header.Height > 0,
			Inputs:     len(tx.Inputs),
			Outputs:    len(tx.Outputs),
		})
	}

	// Genesis block has a special message in the hash preimage
	genesisMsg := ""
	if block.Header.Height == 0 {
		genesisMsg = `sha3.256("CNBC 02/Feb/2026 Bitcoin is coming off a brutal week")`
	}

	data := map[string]interface{}{
		"IsTestnet":  params.IsTestnet,
		"Height":     block.Header.Height,
		"Hash":       fmt.Sprintf("%x", hash[:]),
		"PrevHash":   fmt.Sprintf("%x", block.Header.PrevHash[:]),
		"MerkleRoot": fmt.Sprintf("%x", block.Header.MerkleRoot[:]),
		"Time":       time.Unix(block.Header.Timestamp, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
		"Difficulty": block.Header.Difficulty,
		"Nonce":      block.Header.Nonce,
		"TxCount":    len(block.Transactions),
		"Txs":        txs,
		"HasPrev":    block.Header.Height > 0,
		"PrevHeight": block.Header.Height - 1,
		"NextHeight": block.Header.Height + 1,
		"HasNext":    block.Header.Height < chain.Height(),
		"Reward":     float64(GetBlockReward(block.Header.Height)) / 100_000_000,
		"IsGenesis":  block.Header.Height == 0,
		"GenesisMsg": genesisMsg,
	}

	renderTemplate(w, explorerBlockTmpl, data)
}

func (e *Explorer) handleTx(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/tx/")
	if path == "" || len(path) < 64 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Search for tx in blocks first
	tx, blockHeight, found := e.findTx(path)

	// If not found in blocks, check mempool
	inMempool := false
	if !found {
		hashBytes, err := hex.DecodeString(path[:64])
		if err == nil && len(hashBytes) == 32 {
			var txID [32]byte
			copy(txID[:], hashBytes)
			tx, found = e.daemon.mempool.GetTransaction(txID)
			if found {
				inMempool = true
			}
		}
	}

	if !found {
		http.Error(w, "Transaction not found, wait 60s and try again", http.StatusNotFound)
		return
	}

	txID, _ := tx.TxID()
	isCoinbase := len(tx.Inputs) == 0

	// Build inputs data
	var inputs []map[string]interface{}
	for i, inp := range tx.Inputs {
		inputs = append(inputs, map[string]interface{}{
			"Index":        i,
			"KeyImage":     fmt.Sprintf("%x", inp.KeyImage),
			"RingSize":     len(inp.RingMembers),
			"PseudoOutput": fmt.Sprintf("%x", inp.PseudoOutput),
		})
	}

	// Build outputs data
	var outputs []map[string]interface{}
	for i, out := range tx.Outputs {
		entry := map[string]interface{}{
			"Index":           i,
			"Commitment":      fmt.Sprintf("%x", out.Commitment),
			"PublicKey":       fmt.Sprintf("%x", out.PublicKey),
			"RangeProof":      len(out.RangeProof),
			"EncMemo":         fmt.Sprintf("%x", out.EncryptedMemo),
			"EncryptedAmount": fmt.Sprintf("%x", out.EncryptedAmount),
		}
		outputs = append(outputs, entry)
	}

	data := map[string]interface{}{
		"IsTestnet":   params.IsTestnet,
		"Hash":        fmt.Sprintf("%x", txID),
		"IsCoinbase":  isCoinbase,
		"Fee":         float64(tx.Fee) / 100_000_000,
		"TxPubKey":    fmt.Sprintf("%x", tx.TxPublicKey),
		"InputCount":  len(tx.Inputs),
		"OutputCount": len(tx.Outputs),
		"Inputs":      inputs,
		"Outputs":     outputs,
		"InMempool":   inMempool,
	}

	if inMempool {
		data["BlockHeight"] = "Pending"
		data["Confirmations"] = 0
	} else {
		confirmations := e.daemon.chain.Height() - blockHeight + 1
		data["BlockHeight"] = blockHeight
		data["Confirmations"] = confirmations
	}

	renderTemplate(w, explorerTxTmpl, data)
}

func (e *Explorer) handleStats(w http.ResponseWriter, r *http.Request) {
	snapshot, ready := e.getStatsSnapshot()
	if !ready {
		snapshot = e.refreshStatsSnapshotSync()
	} else if e.shouldRefreshStats(snapshot) {
		e.refreshStatsSnapshotAsync()
	}

	data := map[string]interface{}{
		"IsTestnet":    params.IsTestnet,
		"Height":       snapshot.Height,
		"Difficulty":   snapshot.Difficulty,
		"Hashrate":     snapshot.Hashrate,
		"AvgBlockTime": snapshot.AvgBlockTime,
		"TotalTx":      snapshot.TotalTx,
		"Emitted":      snapshot.Emitted,
		"Remaining":    snapshot.Remaining,
		"PctEmitted":   snapshot.PctEmitted,
		"Peers":        snapshot.Peers,
		"DataJSON":     snapshot.DataJSON,
		"GenesisTs":    snapshot.GenesisTs,
	}

	renderTemplate(w, explorerStatsTmpl, data)
}

// findTx searches for a transaction by hash in the blockchain.
func (e *Explorer) findTx(hashStr string) (*Transaction, uint64, bool) {
	return e.daemon.chain.FindTxByHashStr(hashStr)
}

func (e *Explorer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Try as block height
	if height, err := strconv.ParseUint(q, 10, 64); err == nil {
		if height <= e.daemon.chain.Height() {
			http.Redirect(w, r, "/block/"+q, http.StatusFound)
			return
		}
	}

	// Try as block or tx hash (64 hex chars)
	if len(q) == 64 {
		// First check if it's a block hash
		var hash [32]byte
		if decoded, err := hex.DecodeString(q); err == nil && len(decoded) == 32 {
			copy(hash[:], decoded)
			if e.daemon.chain.GetBlock(hash) != nil {
				http.Redirect(w, r, "/block/"+q, http.StatusFound)
				return
			}
		}

		// Then check if it's a tx hash in blockchain
		if _, _, found := e.findTx(q); found {
			http.Redirect(w, r, "/tx/"+q, http.StatusFound)
			return
		}

		// Check mempool
		if decoded, err := hex.DecodeString(q); err == nil && len(decoded) == 32 {
			var txID [32]byte
			copy(txID[:], decoded)
			if e.daemon.mempool.HasTransaction(txID) {
				http.Redirect(w, r, "/tx/"+q, http.StatusFound)
				return
			}
		}

		// Default to block (will show not found if invalid)
		http.Redirect(w, r, "/block/"+q, http.StatusFound)
		return
	}

	http.Error(w, "Not found: "+q, http.StatusNotFound)
}

type proveResult struct {
	Index  int     `json:"index"`
	Match  bool    `json:"match"`
	Amount float64 `json:"amount,omitempty"`
	Memo   string  `json:"memo,omitempty"`
}

func (e *Explorer) findTxForProve(txHash string) (tx *Transaction, blockHeight uint64, isCoinbase bool, err error) {
	tx, blockHeight, found := e.findTx(txHash)
	if !found {
		hashBytes, _ := hex.DecodeString(txHash)
		if len(hashBytes) == 32 {
			var txID [32]byte
			copy(txID[:], hashBytes)
			tx, found = e.daemon.mempool.GetTransaction(txID)
		}
	}
	if !found {
		return nil, 0, false, fmt.Errorf("transaction not found")
	}
	return tx, blockHeight, len(tx.Inputs) == 0, nil
}

func proveJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (e *Explorer) handleProve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	txHash := strings.TrimSpace(r.FormValue("tx"))
	spendPubHex := strings.TrimSpace(r.FormValue("spend_pub"))
	viewPrivHex := strings.TrimSpace(r.FormValue("view_priv"))

	if len(txHash) != 64 || len(spendPubHex) != 64 || len(viewPrivHex) != 64 {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "tx, spend_pub, and view_priv must each be 64 hex characters"})
		return
	}

	spendPubBytes, err := hex.DecodeString(spendPubHex)
	if err != nil {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid spend_pub hex"})
		return
	}
	viewPrivBytes, err := hex.DecodeString(viewPrivHex)
	if err != nil {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid view_priv hex"})
		return
	}

	var spendPub, viewPriv [32]byte
	copy(spendPub[:], spendPubBytes)
	copy(viewPriv[:], viewPrivBytes)

	tx, blockHeight, isCoinbase, err := e.findTxForProve(txHash)
	if err != nil {
		proveJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	var results []proveResult
	for i, out := range tx.Outputs {
		if !CheckStealthOutput(spendPub, viewPriv, tx.TxPublicKey, out.PublicKey) {
			results = append(results, proveResult{Index: i})
			continue
		}

		po := proveResult{Index: i, Match: true}
		var blinding [32]byte
		if isCoinbase {
			blinding = wallet.DeriveCoinbaseConsensusBlinding(tx.TxPublicKey, blockHeight, i)
		} else {
			secret, serr := DeriveStealthSecret(tx.TxPublicKey, viewPriv)
			if serr == nil {
				blinding = wallet.DeriveBlinding(secret, i)
				if memo, ok := wallet.DecryptMemo(out.EncryptedMemo, secret, i); ok && len(memo) > 0 {
					po.Memo = string(memo)
				}
			}
		}
		po.Amount = float64(wallet.DecryptAmount(out.EncryptedAmount, blinding, i)) / 100_000_000
		results = append(results, po)
	}

	proveJSON(w, http.StatusOK, results)
}

func (e *Explorer) handleProveSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	txHash := strings.TrimSpace(r.FormValue("tx"))
	txPrivHex := strings.TrimSpace(r.FormValue("tx_priv"))
	spendPubHex := strings.TrimSpace(r.FormValue("spend_pub"))
	viewPubHex := strings.TrimSpace(r.FormValue("view_pub"))

	if len(txHash) != 64 || len(txPrivHex) != 64 || len(spendPubHex) != 64 || len(viewPubHex) != 64 {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "all fields must be 64 hex characters"})
		return
	}

	txPrivBytes, err := hex.DecodeString(txPrivHex)
	if err != nil {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tx_priv hex"})
		return
	}
	spendPubBytes, err := hex.DecodeString(spendPubHex)
	if err != nil {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid spend_pub hex"})
		return
	}
	viewPubBytes, err := hex.DecodeString(viewPubHex)
	if err != nil {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid view_pub hex"})
		return
	}

	var txPriv, spendPub, viewPub [32]byte
	copy(txPriv[:], txPrivBytes)
	copy(spendPub[:], spendPubBytes)
	copy(viewPub[:], viewPubBytes)

	tx, blockHeight, isCoinbase, err := e.findTxForProve(txHash)
	if err != nil {
		proveJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	// Verify the prover actually knows r: r * G must equal the on-chain tx public key
	derivedPub, err := ScalarToPubKey(txPriv)
	if err != nil || derivedPub != tx.TxPublicKey {
		proveJSON(w, http.StatusBadRequest, map[string]string{"error": "tx private key does not match this transaction's public key"})
		return
	}

	// ECDH is symmetric: H(tx_priv * view_pub) == H(view_priv * tx_pub)
	// So we can reuse CheckStealthOutput by swapping the scalar/point roles.
	var results []proveResult
	for i, out := range tx.Outputs {
		if !CheckStealthOutput(spendPub, txPriv, viewPub, out.PublicKey) {
			results = append(results, proveResult{Index: i})
			continue
		}

		po := proveResult{Index: i, Match: true}
		var blinding [32]byte
		if isCoinbase {
			blinding = wallet.DeriveCoinbaseConsensusBlinding(tx.TxPublicKey, blockHeight, i)
		} else {
			secret, serr := DeriveStealthSecretSender(txPriv, viewPub)
			if serr == nil {
				blinding = wallet.DeriveBlinding(secret, i)
				if memo, ok := wallet.DecryptMemo(out.EncryptedMemo, secret, i); ok && len(memo) > 0 {
					po.Memo = string(memo)
				}
			}
		}
		po.Amount = float64(wallet.DecryptAmount(out.EncryptedAmount, blinding, i)) / 100_000_000
		results = append(results, po)
	}

	proveJSON(w, http.StatusOK, results)
}

func timeAgo(timestamp int64) string {
	diff := time.Now().Unix() - timestamp
	if diff < 60 {
		return fmt.Sprintf("%ds ago", diff)
	} else if diff < 3600 {
		return fmt.Sprintf("%dm ago", diff/60)
	} else if diff < 86400 {
		return fmt.Sprintf("%dh ago", diff/3600)
	}
	return fmt.Sprintf("%dd ago", diff/86400)
}

func fmtAmountComma(satoshis uint64) string {
	whole := satoshis / 100_000_000
	frac := satoshis % 100_000_000
	s := fmt.Sprintf("%d", whole)
	if len(s) > 3 {
		var buf []byte
		for i, c := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, byte(c))
		}
		s = string(buf)
	}
	return fmt.Sprintf("%s.%02d", s, frac/1_000_000)
}

func renderTemplate(w http.ResponseWriter, tmplStr string, data interface{}) {
	tmpl, err := template.New("page").Parse(tmplStr)
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("explorer template render failed: %v", err)
	}
}

// Base CSS matches website exactly, with explorer-specific additions
const explorerCSS = `*{margin:0;padding:0;box-sizing:border-box}
:root{--ac:#af0;--ac-h:#cf3}
body{background:#000;color:#b0b0b0;font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;padding:32px;max-width:800px;margin:0 auto}
a{color:var(--ac)}
a:hover{color:var(--ac-h)}
h1,h2{color:#eee;font-weight:normal;margin:48px 0 16px}
h1{font-size:24px;margin-top:0}
h2{font-size:18px;border-bottom:1px dashed #333;padding-bottom:8px}
p{margin:16px 0}
.g{color:var(--ac)}
.d{color:#555}
.box{border:1px solid #333;padding:20px;margin:24px 0;background:#000}
.stats{display:flex;justify-content:space-between}
.spec{display:flex;justify-content:space-between;padding:8px 0;border-bottom:1px solid #1a1a1a}
.spec:last-child{border:0}
.spec-k{color:#666}
footer{margin-top:64px;padding-top:24px;border-top:1px dashed #333;color:#444;font-size:13px}
@media(max-width:600px){body{padding:16px}h1{font-size:20px}}
.stat{text-align:center}
.stat-v{font-size:24px;color:#eee}
.stat-k{font-size:12px;color:#666;text-transform:uppercase}
table{width:100%;border-collapse:collapse;margin:16px 0}
th,td{text-align:left;padding:12px;border-bottom:1px solid #222}
th{color:#666;font-weight:normal;font-size:13px;text-transform:uppercase}
tr:hover{background:#111}
.hash{color:#666;font-size:13px}
.search{display:flex;gap:8px;margin:24px 0}
.search input{flex:1;background:#000;border:1px solid #333;color:#eee;padding:12px;font:inherit}
.search input:focus{outline:none;border-color:var(--ac)}
.search button{background:var(--ac);border:0;color:#000;padding:12px 24px;cursor:pointer;font:inherit}
.search button:hover{background:var(--ac-h)}
.nav{margin:24px 0}
.nav a{margin-right:16px}
.prop{display:flex;padding:8px 0;border-bottom:1px solid #1a1a1a}
.prop:last-child{border:0}
.prop-k{width:140px;color:#666}
.prop-v{flex:1;word-break:break-all}
.prop-v.mono{font-size:12px;color:#888}
.topnav{font-size:13px;margin:4px 0 0}`

const explorerProxyFixScript = `<script>
(function(){
var aa=document.querySelectorAll('.topnav a,div a,h1 span');
for(var i=0;i<aa.length;i++){var t=aa[i].textContent.trim();
if(t==='mainnet'||t==='testnet'||t==='TESTNET'){aa[i].remove();}}
})();
</script>`

const explorerEggScript = `<script>
(function(){
var egg=document.getElementById('egg');if(!egg)return;
var ne=egg.cloneNode(true);egg.parentNode.replaceChild(ne,egg);egg=ne;
var active=false,hex='0123456789abcdef';
function px(c){c=c.replace('#','');if(c.length===3)c=c[0]+c[0]+c[1]+c[1]+c[2]+c[2];return[parseInt(c.slice(0,2),16),parseInt(c.slice(2,4),16),parseInt(c.slice(4,6),16)];}
function hx(r,g,b){return'#'+((1<<24)+(r<<16)+(g<<8)+b).toString(16).slice(1);}
function lp(a,b,t){return Math.round(a+(b-a)*t);}
function tw(from,to,t){return hx(lp(from[0],to[0],t),lp(from[1],to[1],t),lp(from[2],to[2],t));}
var tn=document.cookie.indexOf('net=testnet')!==-1;
var acO=px(tn?'#f0a':'#af0'),acT=px(tn?'#af0':'#f0a');
var ahO=px(tn?'#f5c':'#cf3'),ahT=px(tn?'#cf3':'#f5c');
egg.addEventListener('click',function(ev){
ev.preventDefault();ev.stopPropagation();
if(active)return;active=true;
document.cookie='net='+(tn?'mainnet':'testnet')+';path=/;max-age=31536000';
var els=document.querySelectorAll('h1,h2,.topnav a,.stat-k,.stat-v,.prop-k,.prop-v,td,th,footer a,button,.nav a');
var saved=[];
for(var i=0;i<els.length;i++){
saved.push(els[i].textContent);
var s='';for(var j=0;j<els[i].textContent.length;j++){var c=els[i].textContent[j];s+=/\s/.test(c)?c:hex[Math.random()*16|0];}
els[i].textContent=s;
}
var t0=Date.now(),dur=1800;
var iv=setInterval(function(){
var t=Math.min(1,(Date.now()-t0)/dur);
document.documentElement.style.setProperty('--ac',tw(acO,acT,t));
document.documentElement.style.setProperty('--ac-h',tw(ahO,ahT,t));
if(t>0.45){var dt=(t-0.45)/0.55;
for(var i=0;i<els.length;i++){var orig=saved[i],cur='';
for(var j=0;j<orig.length;j++){if(Math.random()<dt)cur+=orig[j];else cur+=/\s/.test(orig[j])?orig[j]:hex[Math.random()*16|0];}
els[i].textContent=cur;}
}else{for(var i=0;i<els.length;i++){var s='';
for(var j=0;j<els[i].textContent.length;j++){var c=els[i].textContent[j];s+=/\s/.test(c)?c:hex[Math.random()*16|0];}
els[i].textContent=s;}}
if(t>=1){clearInterval(iv);location.reload();}
},60);
});
})();
</script>`

const explorerIndexTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="blocknet explorer">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<div style="display:flex;justify-content:space-between;align-items:baseline"><h1 style="margin-bottom:0"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></h1><div style="display:flex;gap:12px"><a href="/stats" style="font-size:13px">network stats</a></div></div>

<form class="search" action="/search" method="get">
<input type="text" name="q" placeholder="Search by block height or hash...">
<button type="submit">Search</button>
</form>

<div class="box stats">
<div class="stat"><div class="stat-v">{{.Height}}</div><div class="stat-k">Block Height</div></div>
<div class="stat"><div class="stat-v">{{.Peers}}</div><div class="stat-k">Peers</div></div>
<div class="stat"><div class="stat-v">{{.Difficulty}}</div><div class="stat-k">Difficulty</div></div>
<div class="stat"><div class="stat-v">{{.Hashrate}} H/s</div><div class="stat-k">Network Hashrate</div></div>
</div>

<h2><span class="g">#</span> supply</h2>
<div class="box stats">
<div class="stat"><div class="stat-v">{{.Emitted}}</div><div class="stat-k">Coins Emitted</div></div>
<div class="stat"><div class="stat-v">{{.Remaining}}</div><div class="stat-k">Remaining (pre-tail)</div></div>
<div class="stat"><div class="stat-v">{{.PctEmitted}}%</div><div class="stat-k">Emission Progress</div></div>
{{if .TailStarted}}<div class="stat"><div class="stat-v" style="color:var(--ac)">Active</div><div class="stat-k">Tail Emission</div></div>{{end}}
</div>

<h2><span class="g">#</span> mempool</h2>
{{if .MempoolTxs}}
<table>
<tr><th>Hash</th><th>Fee</th><th>Fee/byte</th><th>Size</th><th>Age</th></tr>
{{range .MempoolTxs}}
<tr>
<td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 16}}...</a></td>
<td>{{printf "%.8f" .Fee}}</td>
<td>{{.FeeRate}}</td>
<td>{{.Size}} B</td>
<td>{{.Ago}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="d" style="padding:20px 0">Mempool is empty</p>
{{end}}

<h2><span class="g">#</span> recent blocks</h2>
<table>
<tr><th>Height</th><th>Hash</th><th>Age</th><th>Txs</th></tr>
{{range .Blocks}}
<tr>
<td><a href="/block/{{.Height}}">{{.Height}}</a></td>
<td class="hash"><a href="/block/{{.Hash}}">{{slice .Hash 0 16}}...</a></td>
<td>{{.Ago}}</td>
<td>{{.TxCount}}</td>
</tr>
{{end}}
</table>

<footer><a href="https://blocknetcrypto.com">← blocknetcrypto.com</a>{{if .IsTestnet}} <span class="d">·</span> <span class="d">testnet</span>{{end}}</footer>
` + explorerEggScript + `
</body>
</html>`

const explorerBlockTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Block {{.Height}} - blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="Block {{.Height}} - blocknet">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/block/{{.Height}}">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1>
<div class="topnav"><a href="/">blocks</a>  <a href="/stats">stats</a></div>

<div class="nav">
{{if .HasPrev}}<a href="/block/{{.PrevHeight}}">← Block {{.PrevHeight}}</a>{{end}}
{{if .HasNext}}<a href="/block/{{.NextHeight}}">Block {{.NextHeight}} →</a>{{end}}
</div>

<h2><span class="g">#</span> block {{.Height}}</h2>
<div class="box">
<div class="prop"><div class="prop-k">Hash</div><div class="prop-v mono">{{if .IsGenesis}}<span title="{{.GenesisMsg}}" style="cursor:help;border-bottom:1px dashed var(--ac)">{{.Hash}}</span>{{else}}{{.Hash}}{{end}}</div></div>
<div class="prop"><div class="prop-k">Previous</div><div class="prop-v mono"><a href="/block/{{.PrevHash}}">{{.PrevHash}}</a></div></div>
<div class="prop"><div class="prop-k">Merkle Root</div><div class="prop-v mono">{{.MerkleRoot}}</div></div>
<div class="prop"><div class="prop-k">Time</div><div class="prop-v">{{.Time}}</div></div>
<div class="prop"><div class="prop-k">Difficulty</div><div class="prop-v">{{.Difficulty}}</div></div>
<div class="prop"><div class="prop-k">Nonce</div><div class="prop-v">{{.Nonce}}</div></div>
<div class="prop"><div class="prop-k">Block Reward</div><div class="prop-v">{{printf "%.8f" .Reward}} BNT</div></div>
<div class="prop"><div class="prop-k">Transactions</div><div class="prop-v">{{.TxCount}}</div></div>
</div>

<h2><span class="g">#</span> transactions</h2>
<table>
<tr><th>Hash</th><th>Type</th><th>Inputs</th><th>Outputs</th></tr>
{{range .Txs}}
<tr>
<td class="hash"><a href="/tx/{{.Hash}}">{{slice .Hash 0 24}}...</a></td>
<td>{{if .IsCoinbase}}<span class="g">coinbase</span>{{else}}transfer{{end}}</td>
<td>{{.Inputs}}</td>
<td>{{.Outputs}}</td>
</tr>
{{end}}
</table>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>
` + explorerEggScript + `
</body>
</html>`

const explorerTxTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Transaction - blocknet explorer</title>
<meta name="description" content="Zero-knowledge money">
<meta property="og:title" content="Transaction - blocknet">
<meta property="og:description" content="Zero-knowledge money">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000000">
<meta http-equiv="refresh" content="300">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>` + explorerCSS + `</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1>
<div class="topnav"><a href="/">blocks</a>   <a href="/stats">stats</a></div>

<h2><span class="g">#</span> transaction</h2>
<div class="box">
<div class="prop"><div class="prop-k">Hash</div><div class="prop-v mono">{{.Hash}}</div></div>
<div class="prop"><div class="prop-k">Block</div><div class="prop-v">{{if .InMempool}}<span class="d">Pending (in mempool)</span>{{else}}<a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a> ({{.Confirmations}} confirmations){{end}}</div></div>
<div class="prop"><div class="prop-k">Type</div><div class="prop-v">{{if .IsCoinbase}}<span class="g">coinbase</span>{{else}}transfer{{end}}</div></div>
{{if not .IsCoinbase}}<div class="prop"><div class="prop-k">Fee</div><div class="prop-v">{{printf "%.8f" .Fee}} BNT</div></div>{{end}}
<div class="prop"><div class="prop-k">Inputs</div><div class="prop-v">{{.InputCount}}</div></div>
<div class="prop"><div class="prop-k">Outputs</div><div class="prop-v">{{.OutputCount}}</div></div>
<div class="prop"><div class="prop-k">Tx Public Key</div><div class="prop-v mono">{{.TxPubKey}}</div></div>
</div>

{{if .Inputs}}
<h2><span class="g">#</span> inputs (ring signatures)</h2>
<p class="d" style="font-size:13px;margin-bottom:16px">Each input uses a ring of {{(index .Inputs 0).RingSize}} public keys. The real spender is hidden among decoys.</p>
{{range .Inputs}}
<div class="box">
<div class="prop"><div class="prop-k">Input #{{.Index}}</div><div class="prop-v"></div></div>
<div class="prop"><div class="prop-k">Key Image</div><div class="prop-v mono">{{.KeyImage}}</div></div>
<div class="prop"><div class="prop-k">Ring Size</div><div class="prop-v">{{.RingSize}}</div></div>
<div class="prop"><div class="prop-k">Pseudo Output</div><div class="prop-v mono">{{.PseudoOutput}}</div></div>
</div>
{{end}}
{{end}}

<h2><span class="g">#</span> outputs (stealth addresses)</h2>
<p class="d" style="font-size:13px;margin-bottom:16px">Amounts are hidden by Pedersen commitments. Range proofs ensure validity without revealing values.</p>
{{range .Outputs}}
<div class="box">
<div class="prop"><div class="prop-k">Output #{{.Index}}</div><div class="prop-v"></div></div>
<div class="prop"><div class="prop-k">Stealth Address</div><div class="prop-v mono">{{.PublicKey}}</div></div>
<div class="prop"><div class="prop-k">Commitment</div><div class="prop-v mono">{{.Commitment}}</div></div>
<div class="prop"><div class="prop-k">Range Proof</div><div class="prop-v">{{.RangeProof}} bytes (Bulletproof)</div></div>
{{if .EncMemo}}<div class="prop"><div class="prop-k">Memo</div><div class="prop-v mono"><span class="g">{{.EncMemo}}</span> <span class="d">(encrypted)</span></div></div>{{end}}
</div>
{{end}}

<h2><span class="g">#</span> TX Proof</h2>
<div style="display:flex;gap:0;margin-bottom:0">
<button id="tab-recv" onclick="switchProveTab('recv')" style="background:var(--ac);color:#000;border:1px solid #333;border-bottom:0;padding:8px 20px;cursor:pointer;font:13px/1.4 monospace">TX Received</button>
<button id="tab-send" onclick="switchProveTab('send')" style="background:#000;color:#666;border:1px solid #333;border-bottom:0;padding:8px 20px;cursor:pointer;font:13px/1.4 monospace">TX Sent</button>
</div>
<div class="box" style="margin-top:0;border-top:1px solid #333">
<div id="prove-recv">
<p class="d" style="font-size:13px;margin-bottom:12px">Check if an output belongs to you. Your view privkey is sent to this server.</p>
<div style="display:flex;flex-direction:column;gap:8px">
<input type="text" id="recv-spend" placeholder="Spend pubkey (64 hex)" maxlength="64" spellcheck="false" style="background:#000;border:1px solid #333;color:#eee;padding:10px;font:13px/1.4 monospace;width:100%">
<input type="text" id="recv-view" placeholder="View privkey (64 hex)" maxlength="64" spellcheck="false" style="background:#000;border:1px solid #333;color:#eee;padding:10px;font:13px/1.4 monospace;width:100%">
<button id="recv-btn" onclick="doRecvProve()" style="background:var(--ac);border:0;color:#000;padding:10px 24px;cursor:pointer;font:inherit;align-self:flex-start">Check</button>
</div>
</div>
<div id="prove-send" style="display:none">
<p class="d" style="font-size:13px;margin-bottom:12px">Prove participation in a TX. Provide a view privkey and the recipient's public address. Anyone can independently verify this proof.</p>
<div style="display:flex;flex-direction:column;gap:8px">
<input type="text" id="send-txpriv" placeholder="View privkey (64 hex)" maxlength="64" spellcheck="false" style="background:#000;border:1px solid #333;color:#eee;padding:10px;font:13px/1.4 monospace;width:100%">
<input type="text" id="send-spend" placeholder="Spend pubkey (64 hex)" maxlength="64" spellcheck="false" style="background:#000;border:1px solid #333;color:#eee;padding:10px;font:13px/1.4 monospace;width:100%">
<input type="text" id="send-view" placeholder="View pubkey (64 hex)" maxlength="64" spellcheck="false" style="background:#000;border:1px solid #333;color:#eee;padding:10px;font:13px/1.4 monospace;width:100%">
<button id="send-btn" onclick="doSendProve()" style="background:var(--ac);border:0;color:#000;padding:10px 24px;cursor:pointer;font:inherit;align-self:flex-start">Prove</button>
</div>
</div>
<div id="prove-result" style="margin-top:16px;display:none"></div>
</div>
<script>
function switchProveTab(tab){
var rt=document.getElementById('tab-recv'),st=document.getElementById('tab-send');
var rp=document.getElementById('prove-recv'),sp=document.getElementById('prove-send');
document.getElementById('prove-result').style.display='none';
if(tab==='recv'){
rt.style.background='var(--ac)';rt.style.color='#000';
st.style.background='#000';st.style.color='#666';
rp.style.display='';sp.style.display='none';
}else{
st.style.background='var(--ac)';st.style.color='#000';
rt.style.background='#000';rt.style.color='#666';
sp.style.display='';rp.style.display='none';
}
}
function renderProveResult(data){
var res=document.getElementById('prove-result');
res.style.display='block';
if(data.error){res.innerHTML='<span style="color:#f44">'+data.error+'</span>';return;}
var matched=data.filter(function(o){return o.match;});
if(matched.length===0){res.innerHTML='<span class="d">No outputs in this transaction match the provided keys.</span>';return;}
var html='<div style="color:var(--ac);margin-bottom:12px">'+matched.length+' output'+(matched.length>1?'s':'')+' matched</div>';
for(var i=0;i<matched.length;i++){
var o=matched[i];
html+='<div style="border:1px solid #333;padding:12px;margin-bottom:8px">';
html+='<div class="prop"><div class="prop-k">Output #'+o.index+'</div><div class="prop-v" style="color:var(--ac);font-size:18px">'+o.amount.toFixed(8)+' BNT</div></div>';
if(o.memo){html+='<div class="prop"><div class="prop-k">Memo</div><div class="prop-v">'+o.memo.replace(/</g,'&lt;')+'</div></div>';}
html+='</div>';
}
res.innerHTML=html;
}
function proveRequest(url,body,btn){
btn.disabled=true;btn.textContent='Checking...';
fetch(url,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body})
.then(function(r){return r.json();})
.then(function(data){btn.disabled=false;btn.textContent=btn.dataset.label;renderProveResult(data);})
.catch(function(){btn.disabled=false;btn.textContent=btn.dataset.label;var r=document.getElementById('prove-result');r.style.display='block';r.innerHTML='<span style="color:#f44">Request failed</span>';});
}
document.getElementById('recv-btn').dataset.label='Check';
document.getElementById('send-btn').dataset.label='Prove';
function doRecvProve(){
var sp=document.getElementById('recv-spend').value.trim();
var vp=document.getElementById('recv-view').value.trim();
var res=document.getElementById('prove-result');
if(sp.length!==64||vp.length!==64){res.style.display='block';res.innerHTML='<span style="color:#f44">Both keys must be 64 hex characters</span>';return;}
proveRequest('/prove','tx={{.Hash}}&spend_pub='+sp+'&view_priv='+vp,document.getElementById('recv-btn'));
}
function doSendProve(){
var tp=document.getElementById('send-txpriv').value.trim();
var sp=document.getElementById('send-spend').value.trim();
var vp=document.getElementById('send-view').value.trim();
var res=document.getElementById('prove-result');
if(tp.length!==64||sp.length!==64||vp.length!==64){res.style.display='block';res.innerHTML='<span style="color:#f44">All keys must be 64 hex characters</span>';return;}
proveRequest('/prove-send','tx={{.Hash}}&tx_priv='+tp+'&spend_pub='+sp+'&view_pub='+vp,document.getElementById('send-btn'));
}
</script>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>
` + explorerEggScript + `
</body>
</html>`

const explorerStatsTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Network Stats - blocknet explorer</title>
<meta name="description" content="blocknet network statistics">
<meta property="og:title" content="Network Stats - blocknet">
<meta property="og:description" content="Historical blockchain data">
<meta property="og:image" content="https://blocknetcrypto.com/blocknet.png">
<meta property="og:image:width" content="1024">
<meta property="og:image:height" content="1024">
<meta property="og:url" content="https://explorer.blocknetcrypto.com/stats">
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="#000">
<link rel="icon" type="image/x-icon" href="https://blocknetcrypto.com/favicon.ico">
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{--ac:#af0;--ac-h:#cf3}
body{background:#000;color:#b0b0b0;font:15px/1.6 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;padding:32px;max-width:900px;margin:0 auto}
a{color:var(--ac)}
a:hover{color:var(--ac-h)}
h1,h2{color:#eee;font-weight:normal;margin:48px 0 16px}
h1{font-size:24px;margin-top:0}
h2{font-size:18px;border-bottom:1px dashed #333;padding-bottom:8px}
.g{color:var(--ac)}
.d{color:#555}
.topnav{font-size:13px;margin:4px 0 0}
.box{border:1px solid #222;padding:20px;margin:24px 0;background:#000}
.stats{display:flex;justify-content:space-between;flex-wrap:wrap;gap:8px}
.stat{text-align:center;flex:1;min-width:100px}
.stat-v{font-size:24px;color:#eee}
.stat-k{font-size:12px;color:#666;text-transform:uppercase}
.chart-box{border:1px solid #222;margin:24px 0;background:#000;padding:12px 12px 8px}
canvas{width:100%;height:280px;display:block}
footer{margin-top:64px;padding-top:24px;border-top:1px dashed #333;color:#444;font-size:13px}
@media(max-width:600px){body{padding:16px}h1{font-size:20px}.stats{flex-direction:column}.stat{min-width:auto}}
</style>
{{if .IsTestnet}}<style>:root{--ac:#f0a;--ac-h:#f5c}</style>{{end}}
</head>
<body>
<h1><a href="/" style="text-decoration:none;color:#eee"><span class="g" id="egg" style="-webkit-user-select:none;user-select:none">$</span> blocknet <span class="d">explorer</span></a></h1>
<div class="topnav"><a href="/">← blocks</a></div>

<h2><span class="g">#</span> network overview</h2>
<div class="box stats">
<div class="stat"><div class="stat-v">{{.Height}}</div><div class="stat-k">Block Height</div></div>
<div class="stat"><div class="stat-v">{{.Hashrate}} H/s</div><div class="stat-k">Hashrate</div></div>
<div class="stat"><div class="stat-v">{{.Difficulty}}</div><div class="stat-k">Difficulty</div></div>
<div class="stat"><div class="stat-v">{{.AvgBlockTime}}s</div><div class="stat-k">Avg Block Time</div></div>
</div>
<div class="box stats">
<div class="stat"><div class="stat-v">{{.TotalTx}}</div><div class="stat-k">Window Transactions</div></div>
<div class="stat"><div class="stat-v">{{.Emitted}}</div><div class="stat-k">Coins Emitted</div></div>
<div class="stat"><div class="stat-v">{{.PctEmitted}}%</div><div class="stat-k">Emission Progress</div></div>
<div class="stat"><div class="stat-v">{{.Peers}}</div><div class="stat-k">Peers</div></div>
</div>

<h2><span class="g">#</span> difficulty</h2>
<div class="chart-box"><canvas id="c-diff"></canvas></div>

<h2 style="color:#a0f"><span style="color:#a0f">#</span> difficulty delta</h2>
<div class="chart-box"><canvas id="c-ddelta"></canvas></div>

<h2 style="color:#fa0"><span style="color:#fa0">#</span> estimated hashrate</h2>
<div class="chart-box"><canvas id="c-hash"></canvas></div>

<h2 style="color:#f0a"><span style="color:#f0a">#</span> block time</h2>
<div class="chart-box"><canvas id="c-bt"></canvas></div>

<h2 style="color:#a0f"><span style="color:#a0f">#</span> nonce distribution</h2>
<div class="chart-box"><canvas id="c-nonce"></canvas></div>

<h2 style="color:#0fa"><span style="color:#0fa">#</span> transactions per block</h2>
<div class="chart-box"><canvas id="c-tx"></canvas></div>

<h2 style="color:#0af"><span style="color:#0af">#</span> block size</h2>
<div class="chart-box"><canvas id="c-size"></canvas></div>

<h2><span class="g">#</span> emission schedule</h2>
<div class="chart-box"><canvas id="c-emission"></canvas></div>

<footer><a href="/">← explorer</a>   <a href="https://blocknetcrypto.com">blocknetcrypto.com</a></footer>

<script>
var D={{.DataJSON}};
var AC=getComputedStyle(document.documentElement).getPropertyValue('--ac').trim()||'#af0';
(function(){
if(!D||D.length<2)return;

var tip=document.createElement('div');
tip.style.cssText='position:fixed;background:#111;border:1px solid #333;color:#eee;padding:8px 12px;font:12px/1.5 monospace;pointer-events:none;display:none;z-index:10;white-space:nowrap';
document.body.appendChild(tip);

function showTip(e,html){
tip.innerHTML=html;tip.style.display='block';
var tx=e.clientX+14,ty=e.clientY-44;
if(tx+tip.offsetWidth>window.innerWidth-8)tx=e.clientX-tip.offsetWidth-14;
if(ty<8)ty=e.clientY+20;
tip.style.left=tx+'px';tip.style.top=ty+'px';
}
function hideTip(){tip.style.display='none';}

function fmt(n){
if(Math.abs(n)>=1e12)return(n/1e12).toFixed(1)+'T';
if(Math.abs(n)>=1e9)return(n/1e9).toFixed(1)+'G';
if(Math.abs(n)>=1e6)return(n/1e6).toFixed(1)+'M';
if(Math.abs(n)>=1e3)return(n/1e3).toFixed(1)+'K';
return n%1?n.toFixed(2):n.toFixed(0);
}

function xhex(c){if(c.length===4)return'#'+c[1]+c[1]+c[2]+c[2]+c[3]+c[3];return c;}

function draw(id,getY,color,opts){
opts=opts||{};
color=xhex(color);
var c=document.getElementById(id);
if(!c)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;
c.height=rect.height*dpr;
var ctx=c.getContext('2d');
ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;

var src=opts.data||D;
var pts=[];
for(var i=0;i<src.length;i++){
var y=getY(src[i],i);
if(y!==null&&y!==undefined&&isFinite(y))pts.push({x:src[i].h,y:y});
}
if(pts.length<2){
ctx.fillStyle='#333';ctx.font='13px monospace';ctx.textAlign='center';
ctx.fillText('Not enough data',W/2,H/2);return;
}

var xMin=pts[0].x,xMax=pts[pts.length-1].x;
var yMin=Infinity,yMax=-Infinity;
for(var i=0;i<pts.length;i++){
if(pts[i].y<yMin)yMin=pts[i].y;
if(pts[i].y>yMax)yMax=pts[i].y;
}
if(opts.refLine!==undefined){yMin=Math.min(yMin,opts.refLine);yMax=Math.max(yMax,opts.refLine);}
if(opts.refLines&&opts.refLines.length){
for(var i=0;i<opts.refLines.length;i++){
var rl=opts.refLines[i];
if(rl&&rl.value!==undefined){yMin=Math.min(yMin,rl.value);yMax=Math.max(yMax,rl.value);}
}
}
if(opts.yMin!==undefined)yMin=Math.min(yMin,opts.yMin);
var yP=(yMax-yMin)*0.08||1;
yMin-=yP;yMax+=yP;
if(opts.yMin!==undefined)yMin=Math.max(yMin,opts.yMin);
if(xMin===xMax)xMax=xMin+1;

// measure widest y-axis label and adjust left padding
ctx.font='11px monospace';
var maxLW=0;
for(var i=0;i<=4;i++){
var val=yMax-(yMax-yMin)*i/4;
var tw=ctx.measureText(opts.fmtY?opts.fmtY(val):fmt(val)).width;
if(tw>maxLW)maxLW=tw;
}
pad.l=Math.max(60,Math.ceil(maxLW+16));
pw=W-pad.l-pad.r;

function sx(x){return pad.l+(x-xMin)/(xMax-xMin)*pw;}
function sy(y){return pad.t+ph-(y-yMin)/(yMax-yMin)*ph;}

// grid
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){
var y=pad.t+ph*i/4;
ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();
}

// y-axis legend
if(opts.yLabel){
ctx.fillStyle=color;ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText(opts.yLabel,pad.l,pad.t-10);
}

// y tick labels
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){
var val=yMax-(yMax-yMin)*i/4;
ctx.fillText(opts.fmtY?opts.fmtY(val):fmt(val),pad.l-8,pad.t+ph*i/4+4);
}

// x tick labels
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){
var val=xMin+(xMax-xMin)*i/4;
ctx.fillText(Math.round(val).toString(),pad.l+pw*i/4,H-6);
}

// reference line
if(opts.refLine!==undefined){
ctx.save();ctx.strokeStyle=opts.refColor||'#444';ctx.setLineDash([4,4]);ctx.lineWidth=1;
var ry=sy(opts.refLine);
ctx.beginPath();ctx.moveTo(pad.l,ry);ctx.lineTo(W-pad.r,ry);ctx.stroke();
ctx.restore();
if(opts.refLabel){ctx.fillStyle=opts.refLabelColor||'#666';ctx.textAlign='left';ctx.fillText(opts.refLabel,pad.l+4,ry-6);}
}
if(opts.refLines&&opts.refLines.length){
for(var i=0;i<opts.refLines.length;i++){
var rl=opts.refLines[i];
if(!rl||rl.value===undefined)continue;
ctx.save();
ctx.strokeStyle=rl.color||'#444';
ctx.setLineDash(rl.dash||[4,4]);
ctx.lineWidth=1;
var y=sy(rl.value);
ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();
ctx.restore();
if(rl.label){ctx.fillStyle=rl.labelColor||'#666';ctx.textAlign='left';ctx.fillText(rl.label,pad.l+4,y-6);}
}
}

// line + fill (supports splitAt to change color mid-line)
var spIdx=-1,sc=color;
if(opts.splitAt!==undefined){
sc=opts.splitColor?xhex(opts.splitColor):'#555';
for(var i=pts.length-1;i>=0;i--){if(pts[i].x<=opts.splitAt){spIdx=i;break;}}
}
if(spIdx>=0&&spIdx<pts.length-1){
ctx.strokeStyle=color;ctx.lineWidth=1.5;ctx.beginPath();
for(var i=0;i<=spIdx;i++){var x=sx(pts[i].x),y=sy(pts[i].y);i===0?ctx.moveTo(x,y):ctx.lineTo(x,y);}
ctx.stroke();
ctx.beginPath();ctx.moveTo(sx(pts[0].x),sy(pts[0].y));
for(var i=1;i<=spIdx;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.lineTo(sx(pts[0].x),pad.t+ph);ctx.closePath();
var g1=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
g1.addColorStop(0,color+'18');g1.addColorStop(1,color+'03');ctx.fillStyle=g1;ctx.fill();
ctx.strokeStyle=sc;ctx.lineWidth=1.5;ctx.beginPath();
ctx.moveTo(sx(pts[spIdx].x),sy(pts[spIdx].y));
for(var i=spIdx+1;i<pts.length;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.stroke();
ctx.beginPath();ctx.moveTo(sx(pts[spIdx].x),sy(pts[spIdx].y));
for(var i=spIdx+1;i<pts.length;i++)ctx.lineTo(sx(pts[i].x),sy(pts[i].y));
ctx.lineTo(sx(pts[pts.length-1].x),pad.t+ph);ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.closePath();
var g2=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
g2.addColorStop(0,sc+'10');g2.addColorStop(1,sc+'03');ctx.fillStyle=g2;ctx.fill();
ctx.save();ctx.strokeStyle=color;ctx.setLineDash([4,4]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(sx(pts[spIdx].x),pad.t);ctx.lineTo(sx(pts[spIdx].x),pad.t+ph);ctx.stroke();ctx.restore();
}else{
ctx.strokeStyle=color;ctx.lineWidth=1.5;ctx.beginPath();
for(var i=0;i<pts.length;i++){var x=sx(pts[i].x),y=sy(pts[i].y);i===0?ctx.moveTo(x,y):ctx.lineTo(x,y);}
ctx.stroke();
var grad=ctx.createLinearGradient(0,pad.t,0,pad.t+ph);
grad.addColorStop(0,color+'18');grad.addColorStop(1,color+'03');
ctx.fillStyle=grad;
ctx.lineTo(sx(pts[pts.length-1].x),pad.t+ph);ctx.lineTo(sx(pts[0].x),pad.t+ph);ctx.closePath();ctx.fill();
}

// trend line
if(opts.trendLine&&pts.length>=2){
var tn=pts.length,tsX=0,tsY=0,tsXY=0,tsX2=0;
for(var i=0;i<tn;i++){tsX+=pts[i].x;tsY+=pts[i].y;tsXY+=pts[i].x*pts[i].y;tsX2+=pts[i].x*pts[i].x;}
var tDen=tn*tsX2-tsX*tsX;
if(tDen!==0){
var tM=(tn*tsXY-tsX*tsY)/tDen,tB=(tsY-tM*tsX)/tn;
ctx.save();
ctx.beginPath();
ctx.moveTo(sx(xMin),sy(tB+tM*xMin));
ctx.lineTo(sx(xMax),sy(tB+tM*xMax));
ctx.strokeStyle='#0AF';ctx.globalAlpha=0.25;ctx.lineWidth=2;ctx.setLineDash([8,6]);
ctx.stroke();
ctx.restore();
}}

// snapshot for hover redraw
var base=ctx.getImageData(0,0,c.width,c.height);

c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();
var mx=e.clientX-r.left;
var best=null,bd=Infinity;
for(var i=0;i<pts.length;i++){
var dx=Math.abs(sx(pts[i].x)-mx);
if(dx<bd){bd=dx;best=pts[i];}
}
if(!best||bd>Math.max(20,pw/pts.length*2)){
ctx.putImageData(base,0,0);hideTip();return;
}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var px=sx(best.x),py=sy(best.y);
// crosshair
ctx.strokeStyle='#333';ctx.setLineDash([2,2]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(px,pad.t);ctx.lineTo(px,pad.t+ph);ctx.stroke();
ctx.beginPath();ctx.moveTo(pad.l,py);ctx.lineTo(pad.l+pw,py);ctx.stroke();
ctx.setLineDash([]);
// dot
ctx.beginPath();ctx.arc(px,py,5,0,Math.PI*2);
ctx.fillStyle='#000';ctx.fill();
ctx.beginPath();ctx.arc(px,py,4,0,Math.PI*2);
var hc=(spIdx>=0&&spIdx<pts.length-1&&best.x>pts[spIdx].x)?sc:color;
ctx.fillStyle=hc;ctx.fill();
ctx.restore();
var fv=opts.fmtY?opts.fmtY(best.y):fmt(best.y);
var extra=opts.tipExtra?opts.tipExtra(best):'';
showTip(e,'<span style="color:'+hc+'">\u25CF '+(opts.yLabel||'value')+'</span> <b style="color:#eee">'+fv+'</b><br><span style="color:#555">block '+Math.round(best.x)+'</span>'+extra);
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
}

// line charts
draw('c-diff',function(d){return d.d;},AC,{yLabel:'difficulty',trendLine:true});

// difficulty delta bar chart
(function(){
var c=document.getElementById('c-ddelta');
if(!c||D.length<2)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;c.height=rect.height*dpr;
var ctx=c.getContext('2d');ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var dd=[];
for(var i=1;i<D.length;i++){
if(D[i-1].d>0&&D[i].h>100)dd.push({h:D[i].h,v:(D[i].d-D[i-1].d)/D[i-1].d*100});
}
if(dd.length<2){ctx.fillStyle='#333';ctx.font='13px monospace';ctx.textAlign='center';ctx.fillText('Not enough data',W/2,H/2);return;}
var yMin=0,yMax=0;
for(var i=0;i<dd.length;i++){if(dd[i].v<yMin)yMin=dd[i].v;if(dd[i].v>yMax)yMax=dd[i].v;}
var yP=(yMax-yMin)*0.08||1;yMin-=yP;yMax+=yP;
ctx.font='11px monospace';
var maxLW=0;
for(var i=0;i<=4;i++){var val=yMax-(yMax-yMin)*i/4;var tw=ctx.measureText(val.toFixed(1)+'%').width;if(tw>maxLW)maxLW=tw;}
pad.l=Math.max(60,Math.ceil(maxLW+16));
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;
var xMin=dd[0].h,xMax=dd[dd.length-1].h;
if(xMin===xMax)xMax=xMin+1;
function dsx(x){return pad.l+(x-xMin)/(xMax-xMin)*pw;}
function dsy(y){return pad.t+ph-(y-yMin)/(yMax-yMin)*ph;}
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){var y=pad.t+ph*i/4;ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();}
var zy=dsy(0);
ctx.strokeStyle='#333';ctx.lineWidth=1;ctx.beginPath();ctx.moveTo(pad.l,zy);ctx.lineTo(W-pad.r,zy);ctx.stroke();
ctx.fillStyle='#a0f';ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText('% change',pad.l,pad.t-10);
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){var val=yMax-(yMax-yMin)*i/4;ctx.fillText(val.toFixed(1)+'%',pad.l-8,pad.t+ph*i/4+4);}
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){var val=xMin+(xMax-xMin)*i/4;ctx.fillText(Math.round(val).toString(),pad.l+pw*i/4,H-6);}
var bw=Math.max(1,pw/dd.length);
for(var i=0;i<dd.length;i++){
var x=dsx(dd[i].h)-bw/2,pos=dd[i].v>=0;
var top,bh;
if(pos){top=dsy(dd[i].v);bh=zy-top;}else{top=zy;bh=dsy(dd[i].v)-zy;}
ctx.fillStyle=pos?'#aa00ff50':'#ff00aa50';
ctx.fillRect(x,top,Math.max(1,bw-1),Math.max(1,bh));
ctx.fillStyle=pos?'#a0f':'#f0a';
ctx.fillRect(x,pos?top:top+bh-Math.min(2,bh),Math.max(1,bw-1),Math.min(2,bh));
}
var base=ctx.getImageData(0,0,c.width,c.height);
c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();var mx=e.clientX-r.left;
var best=null,bd=Infinity;
for(var i=0;i<dd.length;i++){var dx=Math.abs(dsx(dd[i].h)-mx);if(dx<bd){bd=dx;best=dd[i];}}
if(!best||bd>Math.max(20,pw/dd.length*2)){ctx.putImageData(base,0,0);hideTip();return;}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var px=dsx(best.h),py=dsy(best.v);
ctx.strokeStyle='#333';ctx.setLineDash([2,2]);ctx.lineWidth=1;
ctx.beginPath();ctx.moveTo(px,pad.t);ctx.lineTo(px,pad.t+ph);ctx.stroke();
ctx.beginPath();ctx.moveTo(pad.l,py);ctx.lineTo(pad.l+pw,py);ctx.stroke();
ctx.setLineDash([]);
ctx.beginPath();ctx.arc(px,py,5,0,Math.PI*2);ctx.fillStyle='#000';ctx.fill();
ctx.beginPath();ctx.arc(px,py,4,0,Math.PI*2);
var hc=best.v>=0?'#a0f':'#f0a';
ctx.fillStyle=hc;ctx.fill();ctx.restore();
var sign=best.v>=0?'+':'';
showTip(e,'<span style="color:'+hc+'">\u25CF difficulty delta</span> <b style="color:#eee">'+sign+best.v.toFixed(2)+'%</b><br><span style="color:#555">block '+Math.round(best.h)+'</span>');
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
})();

var hrD=[],hrW=20;
for(var i=1;i<D.length;i++){
if(D[i].bt<=0)continue;
var sd=0,st=0;
for(var j=Math.max(1,i-hrW+1);j<=i;j++){if(D[j].bt>0){sd+=D[j].d;st+=D[j].bt;}}
if(st>0)hrD.push({h:D[i].h,hr:sd/st});
}
draw('c-hash',function(d){return d.hr;},'#fa0',{data:hrD,yLabel:'H/s ('+hrW+'-block avg)',yMin:0,trendLine:true});
var btSum=0,btCnt=0;
for(var i=0;i<D.length;i++){if(D[i].h>100&&D[i].bt>0){btSum+=D[i].bt;btCnt++;}}
var btAvg=btCnt>0?btSum/btCnt:0;
draw('c-bt',function(d,i){return d.h>1&&d.bt>0?d.bt:null;},'#f0a',{yLabel:'seconds',refLine:300,refLabel:'5m target',refColor:'#fa0',refLabelColor:'#fa0',refLines:[{value:btAvg,label:'avg '+Math.round(btAvg)+'s',color:'#0af',labelColor:'#0af',dash:[2,3]}],yMin:0});

// nonce histogram
(function(){
var c=document.getElementById('c-nonce');
if(!c||D.length<2)return;
var dpr=window.devicePixelRatio||1;
var rect=c.getBoundingClientRect();
c.width=rect.width*dpr;c.height=rect.height*dpr;
var ctx=c.getContext('2d');ctx.scale(dpr,dpr);
var W=rect.width,H=rect.height;
var pad={t:24,r:16,b:32,l:60};
var pw=W-pad.l-pad.r,ph=H-pad.t-pad.b;
var lnonces=[];for(var i=0;i<D.length;i++)lnonces.push(Math.log10(D[i].n+1));
var nMin=Infinity,nMax=-Infinity;
for(var i=0;i<lnonces.length;i++){if(lnonces[i]<nMin)nMin=lnonces[i];if(lnonces[i]>nMax)nMax=lnonces[i];}
if(nMin===nMax)nMax=nMin+1;
var bins=Math.min(64,lnonces.length);
var counts=[];for(var i=0;i<bins;i++)counts[i]=0;
var range=nMax-nMin;
for(var i=0;i<lnonces.length;i++){var b=Math.floor((lnonces[i]-nMin)/range*bins);if(b>=bins)b=bins-1;counts[b]++;}
var maxC=0;for(var i=0;i<bins;i++){if(counts[i]>maxC)maxC=counts[i];}
if(maxC===0)return;

// grid
ctx.strokeStyle='#1a1a1a';ctx.lineWidth=1;
for(var i=0;i<=4;i++){var y=pad.t+ph*i/4;ctx.beginPath();ctx.moveTo(pad.l,y);ctx.lineTo(W-pad.r,y);ctx.stroke();}

// y legend
ctx.fillStyle='#aa00ff';ctx.font='11px monospace';ctx.textAlign='left';
ctx.fillText('block count',pad.l,pad.t-10);

// y ticks
ctx.fillStyle='#555';ctx.font='11px monospace';ctx.textAlign='right';
for(var i=0;i<=4;i++){var val=maxC-maxC*i/4;ctx.fillText(Math.round(val).toString(),pad.l-8,pad.t+ph*i/4+4);}

// x ticks
ctx.textAlign='center';ctx.fillStyle='#444';
for(var i=0;i<=4;i++){var lv=nMin+(nMax-nMin)*i/4;var rv=Math.pow(10,lv);ctx.fillText(fmt(rv),pad.l+pw*i/4,H-6);}

// x legend
ctx.fillStyle='#555';ctx.textAlign='right';ctx.fillText('nonce (log)',W-pad.r,pad.t-10);

// bars
var bw=pw/bins;
for(var i=0;i<bins;i++){
var bh=counts[i]/maxC*ph;var x=pad.l+i*bw;var y=pad.t+ph-bh;
ctx.fillStyle='#aa00ff50';ctx.fillRect(x+1,y,bw-2,bh);
ctx.fillStyle='#aa00ff';ctx.fillRect(x+1,y,bw-2,Math.min(2,bh));
}

// snapshot for hover
var base=ctx.getImageData(0,0,c.width,c.height);

c.addEventListener('mousemove',function(e){
var r=c.getBoundingClientRect();
var mx=e.clientX-r.left;
var bi=Math.floor((mx-pad.l)/bw);
if(bi<0||bi>=bins){ctx.putImageData(base,0,0);hideTip();return;}
ctx.putImageData(base,0,0);
ctx.save();ctx.setTransform(dpr,0,0,dpr,0,0);
var bh=counts[bi]/maxC*ph;var bx=pad.l+bi*bw;var by=pad.t+ph-bh;
ctx.fillStyle='#aa00ff90';ctx.fillRect(bx+1,by,bw-2,bh);
ctx.restore();
var loN=Math.pow(10,nMin+range*bi/bins);
var hiN=Math.pow(10,nMin+range*(bi+1)/bins);
showTip(e,'<span style="color:#aa00ff">\u25CF block count</span> <b style="color:#eee">'+counts[bi]+'</b><br><span style="color:#555">nonce '+fmt(loN)+' \u2013 '+fmt(hiN)+'</span>');
});
c.addEventListener('mouseleave',function(){ctx.putImageData(base,0,0);hideTip();});
})();

// remaining line charts
draw('c-tx',function(d){return d.tx;},'#0fa',{yLabel:'transactions',yMin:0,fmtY:function(v){return Math.round(v).toString();}});
draw('c-size',function(d){return d.s;},'#0af',{yLabel:'bytes',yMin:0,fmtY:function(v){return v>=1024?(v/1024).toFixed(1)+'KB':v.toFixed(0)+'B';}});

// emission
var IR=72325093035,TE=200000000,BPM=8640,MTT=48,DR=0.75;
var emD=[];
var maxH=MTT*BPM+BPM;
var eStep=Math.max(1,Math.floor(maxH/500));
for(var h=0;h<=maxH;h+=eStep){
var mo=Math.floor(h/BPM);
var r;
if(mo>=MTT){r=TE;}else{var yr=mo/12;r=(IR-TE)*Math.exp(-DR*yr)+TE;}
emD.push({h:h,r:r/100000000});
}
var genTs={{.GenesisTs}};
draw('c-emission',function(d){return d.r;},'#f0a',{data:emD,yLabel:'BNT/block',splitAt:{{.Height}},splitColor:AC,fmtY:function(v){return v.toFixed(2)+' BNT';},tipExtra:function(pt){var d=new Date((genTs+pt.x*300)*1000);return '<br><span style="color:#555">~'+d.toISOString().slice(0,10)+'</span>';}});

})();
</script>
` + explorerEggScript + `
</body>
</html>`

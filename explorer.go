package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"blocknet/protocol/params"
	"blocknet/wallet"

	"golang.org/x/time/rate"
)

const (
	explorerIndexListLimit      = 20
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

	statsPageMu  sync.Mutex
	statsPageAt  time.Time
	statsPageFor uint64
	statsPage    []byte
	statsPageGz  []byte

	// proveLimiter guards /prove and /prove-send, which do a chain read plus a
	// per-output ECDH. Same limits as the API's proofLimiter.
	proveLimiter *perIPLimiter
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
		daemon:       daemon,
		mux:          http.NewServeMux(),
		testnetAddr:  "127.0.0.1:18080",
		proveLimiter: newPerIPLimiter(rate.Limit(2), 4, 10*time.Minute),
	}
	e.mux.HandleFunc("/", e.handleIndex)
	e.mux.HandleFunc("/block/", e.handleBlock)
	e.mux.HandleFunc("/tx/", e.handleTx)
	e.mux.HandleFunc("/search", e.handleSearch)
	e.mux.HandleFunc("/stats", e.handleStats)
	e.mux.HandleFunc("/prove", e.handleProve)
	e.mux.HandleFunc("/prove-send", e.handleProveSend)
	e.mux.HandleFunc("/events", e.handleEvents)
	e.startStatsPrecompute()
	return e
}

// ServeHTTP implements http.Handler.
// On mainnet, if the client has a net=testnet cookie, the request is
// reverse-proxied to the co-located testnet explorer daemon.
func (e *Explorer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !params.IsTestnet {
		if c, err := r.Cookie("net"); err == nil && c.Value == "testnet" {
			if r.URL.Path == "/events" {
				e.proxyStreamToTestnet(w, r)
				return
			}
			e.proxyToTestnet(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	e.mux.ServeHTTP(w, r)
}

func (e *Explorer) httpServer(addr string) *http.Server {
	handler := maxBodySize(gzipMiddleware(e), maxRequestBodyBytes)
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

// Supply info. Emitted/remaining/percent are a deterministic function of chain
// height, computed by the shared SupplyBreakdown so the explorer and the
// GET /api/stats endpoint never disagree.
func (e *Explorer) getSupplyInfo() (emitted, remaining uint64, pctEmitted float64) {
	return SupplyBreakdown(e.daemon.chain.HeightFast())
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
	currentHeight := e.daemon.chain.HeightFast()
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

// statsForRequest never builds the snapshot inline. buildStatsSnapshot walks up
// to explorerStatsMaxTraversal blocks, so a handler that waited for it would
// hold the connection for the whole walk on the first request after a restart.
func (e *Explorer) statsForRequest() explorerStatsSnapshot {
	snapshot, ready := e.getStatsSnapshot()
	if !ready || e.shouldRefreshStats(snapshot) {
		e.refreshStatsSnapshotAsync()
	}
	if !ready {
		return e.warmingStatsSnapshot()
	}
	return snapshot
}

// warmingStatsSnapshot is served until the background build lands. Every field
// here is cheap: no per-block traversal.
func (e *Explorer) warmingStatsSnapshot() explorerStatsSnapshot {
	chain := e.daemon.chain
	emitted, remaining, pctEmitted := e.getSupplyInfo()

	peers := 0
	if e.daemon != nil && e.daemon.node != nil {
		peers = len(e.daemon.node.Peers())
	}

	return explorerStatsSnapshot{
		Height:       chain.HeightFast(),
		Difficulty:   chain.NextDifficultyFast(),
		Hashrate:     "\u2014",
		AvgBlockTime: "\u2014",
		Emitted:      fmtAmountComma(emitted),
		Remaining:    fmtAmountComma(remaining),
		PctEmitted:   fmt.Sprintf("%.4f", pctEmitted),
		Peers:        peers,
		DataJSON:     template.JS("[]"),
	}
}

func (e *Explorer) buildStatsSnapshot() explorerStatsSnapshot {
	chain := e.daemon.chain
	height := chain.HeightFast()

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
		prevBlock := chain.GetBlockByHeightFast(startHeight - 1)
		if prevBlock != nil {
			prevTs = prevBlock.Header.Timestamp
		}
	}

	for h := startHeight; h <= height; h++ {
		block := chain.GetBlockByHeightFast(h)
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

	hashrate, _ := chain.RecentBlockStats(explorerHashrateSampleCount)

	emitted, remaining, pctEmitted := e.getSupplyInfo()
	genesisTs := int64(0)
	if genesis := chain.GetBlockByHeightFast(0); genesis != nil {
		genesisTs = genesis.Header.Timestamp
	}

	return explorerStatsSnapshot{
		Height:       height,
		Difficulty:   chain.NextDifficultyFast(),
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
	height := chain.HeightFast()

	// Get recent blocks
	type blockSummary struct {
		Height     uint64
		Hash       string
		Time       string
		Ago        string
		TxCount    int
		Difficulty uint64
		Ts         int64
	}

	var blocks []blockSummary

	for h := height; len(blocks) < explorerIndexListLimit; h-- {
		block := chain.GetBlockByHeightFast(h)
		if block != nil {
			hash := block.Hash()
			blocks = append(blocks, blockSummary{
				Height:     h,
				Hash:       fmt.Sprintf("%x", hash[:]),
				Time:       time.Unix(block.Header.Timestamp, 0).UTC().Format("2006-01-02 15:04:05"),
				Ago:        timeAgo(block.Header.Timestamp),
				TxCount:    len(block.Transactions),
				Difficulty: block.Header.Difficulty,
				Ts:         block.Header.Timestamp,
			})
		}
		if h == 0 {
			break
		}
	}

	emitted, remaining, pctEmitted := e.getSupplyInfo()

	// Get mempool transactions
	type mempoolTx struct {
		Hash    string
		Fee     float64
		FeeRate uint64
		Size    int
		Ago     string
		Ts      int64
	}
	var mempoolTxs []mempoolTx
	mempoolEntries := e.daemon.mempool.GetAllEntries()
	for _, entry := range mempoolEntries {
		if len(mempoolTxs) >= explorerIndexListLimit {
			break
		}
		mempoolTxs = append(mempoolTxs, mempoolTx{
			Hash:    fmt.Sprintf("%x", entry.TxID[:]),
			Fee:     float64(entry.Fee) / 100_000_000,
			FeeRate: entry.FeeRate,
			Size:    entry.Size,
			Ago:     timeAgo(entry.AddedAt.Unix()),
			Ts:      entry.AddedAt.Unix(),
		})
	}

	snapshot := e.statsForRequest()

	var tipTs int64
	if len(blocks) > 0 {
		tipTs = blocks[0].Ts
	}

	data := map[string]interface{}{
		"IsTestnet":    params.IsTestnet,
		"Height":       height,
		"Difficulty":   snapshot.Difficulty,
		"Peers":        snapshot.Peers,
		"Hashrate":     snapshot.Hashrate,
		"Emitted":      fmtAmountComma(emitted),
		"Remaining":    fmtAmountComma(remaining),
		"PctEmitted":   fmt.Sprintf("%.4f", pctEmitted),
		"TailStarted":  pctEmitted >= 100,
		"MempoolTxs":   mempoolTxs,
		"MempoolTotal": len(mempoolEntries),
		"Blocks":       blocks,
		"TipTs":        tipTs,
		"AvgBlockTime": snapshot.AvgBlockTime,
	}

	renderTemplate(w, explorerIndexPage, data)
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
		block = chain.GetBlockByHeightFast(height)
	} else {
		// Try as hash
		if len(path) >= 64 {
			hashBytes, err := hex.DecodeString(path[:64])
			if err == nil && len(hashBytes) == 32 {
				var hash [32]byte
				copy(hash[:], hashBytes)
				block = chain.GetBlockFast(hash)
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

	chainHeight := chain.HeightFast()
	setImmutableCacheIfFinal(w, block.Header.Height, chainHeight)

	var confirmations uint64
	if chainHeight >= block.Header.Height {
		confirmations = chainHeight - block.Header.Height + 1
	}

	data := map[string]interface{}{
		"IsTestnet":     params.IsTestnet,
		"Height":        block.Header.Height,
		"Hash":          fmt.Sprintf("%x", hash[:]),
		"PrevHash":      fmt.Sprintf("%x", block.Header.PrevHash[:]),
		"MerkleRoot":    fmt.Sprintf("%x", block.Header.MerkleRoot[:]),
		"Time":          time.Unix(block.Header.Timestamp, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
		"Difficulty":    block.Header.Difficulty,
		"Nonce":         block.Header.Nonce,
		"TxCount":       len(block.Transactions),
		"Txs":           txs,
		"HasPrev":       block.Header.Height > 0,
		"PrevHeight":    block.Header.Height - 1,
		"NextHeight":    block.Header.Height + 1,
		"HasNext":       block.Header.Height < chainHeight,
		"Ts":            block.Header.Timestamp,
		"Confirmations": confirmations,
		"Final":         confirmations > uint64(MaxReorgDepth),
		"Reward":        float64(GetBlockReward(block.Header.Height)) / 100_000_000,
		"IsGenesis":     block.Header.Height == 0,
		"GenesisMsg":    genesisMsg,
	}

	renderTemplate(w, explorerBlockPage, data)
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
		chainHeight := e.daemon.chain.HeightFast()
		data["BlockHeight"] = blockHeight
		data["Confirmations"] = chainHeight - blockHeight + 1
		setImmutableCacheIfFinal(w, blockHeight, chainHeight)
	}

	renderTemplate(w, explorerTxPage, data)
}

func (e *Explorer) handleStats(w http.ResponseWriter, r *http.Request) {
	snapshot := e.statsForRequest()

	if !snapshot.ComputedAt.IsZero() {
		e.statsPageMu.Lock()
		page, pageGz := e.statsPage, e.statsPageGz
		hit := page != nil && e.statsPageAt.Equal(snapshot.ComputedAt) && e.statsPageFor == snapshot.Height
		e.statsPageMu.Unlock()
		if !hit {
			var buf bytes.Buffer
			if err := explorerStatsPage.Execute(&buf, statsPageData(snapshot)); err != nil {
				log.Printf("explorer template render failed: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			page = buf.Bytes()
			pageGz = gzipBytes(page)
			e.statsPageMu.Lock()
			e.statsPage, e.statsPageGz, e.statsPageAt, e.statsPageFor = page, pageGz, snapshot.ComputedAt, snapshot.Height
			e.statsPageMu.Unlock()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if gw, ok := w.(*gzipResponseWriter); ok {
			gw.writeGzipped(pageGz)
			return
		}
		w.Write(page)
		return
	}

	renderTemplate(w, explorerStatsPage, statsPageData(snapshot))
}

func statsPageData(snapshot explorerStatsSnapshot) map[string]interface{} {
	return map[string]interface{}{
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
		if height <= e.daemon.chain.HeightFast() {
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
			if e.daemon.chain.GetBlockFast(hash) != nil {
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
	if !e.proveLimiter.allow(clientIP(r)) {
		proveJSON(w, http.StatusTooManyRequests, map[string]string{"error": "prove rate limit exceeded"})
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
	if !e.proveLimiter.allow(clientIP(r)) {
		proveJSON(w, http.StatusTooManyRequests, map[string]string{"error": "prove rate limit exceeded"})
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

func renderTemplate(w http.ResponseWriter, tmpl *template.Template, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("explorer template render failed: %v", err)
	}
}

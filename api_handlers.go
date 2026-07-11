package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"blocknet/wallet"
)

const (
	sendMinFee     = uint64(1000) // 0.00001 BNT minimum fee
	sendFeePerByte = uint64(10)   // 0.0000001 BNT per byte

	// blockTemplateSyncHeightTolerance is how many blocks behind an active
	// sync target the local tip may be while still serving mining templates.
	// Near-tip sync runs (propagation races, overlap re-downloads) finish in
	// seconds and do not make templates meaningfully stale; hard-failing them
	// starves pools of work for the whole run.
	blockTemplateSyncHeightTolerance = uint64(2)
)

// ============================================================================
// Public handlers (no wallet needed)
// ============================================================================

// handleStatus returns daemon stats plus wallet diagnostics (if unlocked).
func (s *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		DaemonStats
		Wallet *wallet.WalletDiagnostics `json:"wallet,omitempty"`
	}{
		DaemonStats: s.daemon.Stats(),
	}
	if s.wallet != nil && !s.locked {
		diag := s.wallet.Diagnostics()
		resp.Wallet = &diag
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBlock returns a block by hash (hex) or height (integer).
// GET /api/block/{id}
func (s *APIServer) handleBlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing block id")
		return
	}

	chain := s.daemon.Chain()
	var block *Block

	// Try as height first
	if height, err := strconv.ParseUint(id, 10, 64); err == nil {
		block = chain.GetBlockByHeight(height)
	} else if len(id) == 64 {
		// Try as hex hash
		hashBytes, err := hex.DecodeString(id)
		if err != nil || len(hashBytes) != 32 {
			writeError(w, http.StatusBadRequest, "invalid block hash")
			return
		}
		var hash [32]byte
		copy(hash[:], hashBytes)
		block = chain.GetBlock(hash)
	} else {
		writeError(w, http.StatusBadRequest, "id must be a height or 64-char hex hash")
		return
	}

	if block == nil {
		writeError(w, http.StatusNotFound, "block not found")
		return
	}

	writeJSON(w, http.StatusOK, blockToJSON(block, chain.Height()))
}

// handleTx returns a transaction by hash (searches chain then mempool).
// GET /api/tx/{hash}
func (s *APIServer) handleTx(w http.ResponseWriter, r *http.Request) {
	hashStr := r.PathValue("hash")
	if len(hashStr) != 64 {
		writeError(w, http.StatusBadRequest, "hash must be 64 hex characters")
		return
	}

	hashBytes, err := hex.DecodeString(hashStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid hex hash")
		return
	}
	var txID [32]byte
	copy(txID[:], hashBytes)

	// Check mempool first (fast)
	if tx, ok := s.daemon.Mempool().GetTransaction(txID); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"tx":            tx,
			"confirmations": 0,
			"in_mempool":    true,
		})
		return
	}

	// Search chain (slow — scans blocks from tip backwards)
	tx, blockHeight, found := s.findChainTx(hashStr)
	if !found {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}

	confirmations := s.daemon.Chain().Height() - blockHeight + 1
	writeJSON(w, http.StatusOK, map[string]any{
		"tx":            tx,
		"block_height":  blockHeight,
		"confirmations": confirmations,
		"in_mempool":    false,
	})
}

// handleMempool returns mempool stats.
// GET /api/mempool
func (s *APIServer) handleMempool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.daemon.Mempool().Stats())
}

// handleMempoolTxs returns all mempool transactions as full tx objects.
// GET /api/mempool/txs
func (s *APIServer) handleMempoolTxs(w http.ResponseWriter, r *http.Request) {
	entries := s.daemon.Mempool().GetAllEntries()
	txs := make([]*Transaction, 0, len(entries))
	for _, entry := range entries {
		txs = append(txs, entry.Tx)
	}
	writeJSON(w, http.StatusOK, txs)
}

// handlePeers returns connected peers.
// GET /api/peers
func (s *APIServer) handlePeers(w http.ResponseWriter, r *http.Request) {
	infos := s.daemon.Node().PeerInfos()

	type peerEntry struct {
		PeerID string   `json:"peer_id"`
		Addrs  []string `json:"addrs"`
	}

	entries := make([]peerEntry, len(infos))
	for i, info := range infos {
		entries[i] = peerEntry{
			PeerID: info.ID.String(),
			Addrs:  info.Addrs,
		}
		if entries[i].Addrs == nil {
			entries[i].Addrs = []string{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(entries),
		"peers": entries,
	})
}

// handleBannedPeers returns banned peers.
// GET /api/peers/banned
func (s *APIServer) handleBannedPeers(w http.ResponseWriter, r *http.Request) {
	bans := s.daemon.Node().GetBannedPeers()

	type banEntry struct {
		PeerID    string   `json:"peer_id"`
		Addrs     []string `json:"addrs"`
		Reason    string   `json:"reason"`
		BanCount  int      `json:"ban_count"`
		Permanent bool     `json:"permanent"`
		ExpiresAt string   `json:"expires_at,omitempty"`
	}

	entries := make([]banEntry, len(bans))
	for i, b := range bans {
		addrs := b.Addrs
		if addrs == nil {
			addrs = []string{}
		}
		entry := banEntry{
			PeerID:    b.PeerID.String(),
			Addrs:     addrs,
			Reason:    b.Reason,
			BanCount:  b.BanCount,
			Permanent: b.Permanent,
		}
		if !b.Permanent {
			entry.ExpiresAt = b.ExpiresAt.Format(time.RFC3339)
		}
		entries[i] = entry
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(entries),
		"banned": entries,
	})
}

// handleVerify verifies a signature against a Blocknet stealth address.
// POST /api/verify
func (s *APIServer) handleVerify(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.verifyLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "verify rate limit exceeded")
		return
	}

	var req struct {
		Address   string `json:"address"`
		Message   string `json:"message"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(req.Message) > 1024 {
		writeError(w, http.StatusBadRequest, "message must be <= 1024 bytes")
		return
	}
	if req.Signature == "" {
		writeError(w, http.StatusBadRequest, "signature is required")
		return
	}

	spendPub, _, err := parseValidatedAddress(req.Address)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}

	sigBytes, err := hex.DecodeString(req.Signature)
	if err != nil || len(sigBytes) != 64 {
		writeError(w, http.StatusBadRequest, "invalid signature: must be 64 bytes hex-encoded")
		return
	}

	if err := SchnorrVerify(spendPub[:], []byte(req.Message), sigBytes); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

// statsHashrateSamples is the number of recent block intervals sampled when
// estimating network hashrate and average block time for GET /api/stats.
const statsHashrateSamples = 12

// handleStats returns network and emission/economics statistics as numeric
// JSON, with all amounts in smallest units. No wallet is required; the values
// are a deterministic function of the chain tip.
// GET /api/stats
func (s *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	chain := s.daemon.Chain()
	tip := chain.BestHash()

	// The response depends only on the chain tip, so serve a memoized copy
	// while the tip is unchanged and recompute only when a block/reorg lands.
	s.statsMu.Lock()
	if s.statsValid && s.statsTip == tip {
		cached := s.statsCache
		s.statsMu.Unlock()
		writeJSON(w, http.StatusOK, cached)
		return
	}
	s.statsMu.Unlock()

	height := chain.Height()
	emitted, remaining, pctEmitted := SupplyBreakdown(height)
	hashrate, avgBlockTime := chain.RecentBlockStats(statsHashrateSamples)

	resp := map[string]any{
		"height":             height,
		"difficulty":         chain.NextDifficulty(),
		"network_hashrate":   hashrate,     // hashes/sec, recent sample
		"avg_block_time":     avgBlockTime, // seconds, recent sample
		"block_reward":       GetBlockReward(height),
		"emitted":            emitted,
		"remaining":          remaining,
		"target_supply":      uint64(TargetSupply),
		"pct_emitted":        pctEmitted,
		"tail_emission":      uint64(TailEmission),
		"months_to_tail":     int(MonthsToTail),
		"blocks_per_month":   uint64(BlocksPerMonth),
		"block_interval_sec": int(BlockIntervalSec),
	}

	s.statsMu.Lock()
	s.statsCache = resp
	s.statsTip = tip
	s.statsValid = true
	s.statsMu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// findTxForProof locates a transaction by hex id, checking the chain then the
// mempool. blockHeight is 0 for mempool txs (which are never coinbase).
func (s *APIServer) findTxForProof(txHash string) (tx *Transaction, blockHeight uint64, isCoinbase bool, found bool) {
	tx, blockHeight, found = s.daemon.Chain().FindTxByHashStr(txHash)
	if !found {
		hashBytes, err := hex.DecodeString(txHash)
		if err == nil && len(hashBytes) == 32 {
			var txID [32]byte
			copy(txID[:], hashBytes)
			tx, found = s.daemon.Mempool().GetTransaction(txID)
		}
	}
	if !found {
		return nil, 0, false, false
	}
	return tx, blockHeight, len(tx.Inputs) == 0, true
}

// handleVerifyProof verifies a sender's payment proof. Given a txid, the
// deterministic tx private key (tx_key from POST /api/wallet/prove), and a
// recipient address, it confirms the tx_key genuinely belongs to the
// transaction and reveals which of its outputs that address received, with
// amounts (smallest units) and any memo. Stateless — no wallet required.
// POST /api/verify-proof
func (s *APIServer) handleVerifyProof(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.proofLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "verify-proof rate limit exceeded")
		return
	}

	var req struct {
		TxID    string `json:"txid"`
		TxKey   string `json:"tx_key"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.TxID) != 64 {
		writeError(w, http.StatusBadRequest, "txid must be 64 hex characters")
		return
	}
	if _, err := hex.DecodeString(req.TxID); err != nil {
		writeError(w, http.StatusBadRequest, "txid must be 64 hex characters")
		return
	}
	if len(req.TxKey) != 64 {
		writeError(w, http.StatusBadRequest, "tx_key must be 64 hex characters")
		return
	}
	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}

	txKeyBytes, err := hex.DecodeString(req.TxKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tx_key hex")
		return
	}
	var txPriv [32]byte
	copy(txPriv[:], txKeyBytes)

	spendPub, viewPub, err := parseValidatedAddress(req.Address)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}

	tx, blockHeight, isCoinbase, found := s.findTxForProof(req.TxID)
	if !found {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}

	// The proof holds only if the prover knows r such that r*G equals the
	// transaction's on-chain public key. A well-formed but non-matching tx_key
	// is a legitimate "invalid proof" result (200), mirroring POST /api/verify.
	derivedPub, err := ScalarToPubKey(txPriv)
	if err != nil || derivedPub != tx.TxPublicKey {
		writeJSON(w, http.StatusOK, map[string]any{
			"txid":          req.TxID,
			"valid":         false,
			"total_matched": uint64(0),
			"outputs":       []any{},
		})
		return
	}

	// ECDH is symmetric: H(tx_priv * view_pub) == H(view_priv * tx_pub), so the
	// recipient's outputs can be identified from the sender side.
	outputs, totalMatched := matchProofOutputs(tx, txPriv, spendPub, viewPub, isCoinbase, blockHeight)

	writeJSON(w, http.StatusOK, map[string]any{
		"txid":          req.TxID,
		"valid":         true,
		"total_matched": totalMatched,
		"outputs":       outputs,
	})
}

// matchProofOutputs reports, for each output of tx, whether the recipient
// (spendPub/viewPub) received it and — for matches — the amount (atomic units)
// and any memo, using the sender's tx private key. It mirrors the explorer
// prove-send crypto and is factored out so the match logic is unit-testable
// without a live chain lookup.
func matchProofOutputs(tx *Transaction, txPriv, spendPub, viewPub [32]byte, isCoinbase bool, blockHeight uint64) (outputs []map[string]any, totalMatched uint64) {
	outputs = make([]map[string]any, 0, len(tx.Outputs))
	for i, out := range tx.Outputs {
		if !CheckStealthOutput(spendPub, txPriv, viewPub, out.PublicKey) {
			outputs = append(outputs, map[string]any{"index": i, "match": false})
			continue
		}

		var blinding [32]byte
		var memo string
		if isCoinbase {
			blinding = wallet.DeriveCoinbaseConsensusBlinding(tx.TxPublicKey, blockHeight, i)
		} else {
			secret, serr := DeriveStealthSecretSender(txPriv, viewPub)
			if serr == nil {
				blinding = wallet.DeriveBlinding(secret, i)
				if m, ok := wallet.DecryptMemo(out.EncryptedMemo, secret, i); ok && len(m) > 0 {
					memo = string(m)
				}
			}
		}

		amount := wallet.DecryptAmount(out.EncryptedAmount, blinding, i)
		totalMatched += amount

		entry := map[string]any{"index": i, "match": true, "amount": amount}
		if memo != "" {
			entry["memo"] = memo
		}
		outputs = append(outputs, entry)
	}
	return outputs, totalMatched
}

// ============================================================================
// Wallet handlers (require loaded + unlocked wallet)
// ============================================================================

// handleBalance returns wallet balance breakdown.
// GET /api/wallet/balance
func (s *APIServer) handleBalance(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	chainHeight := s.daemon.Chain().Height()
	syncedHeight := s.wallet.SyncedHeight()

	if chainHeight-syncedHeight > uint64(MaxReorgDepth) {
		writeJSON(w, http.StatusOK, map[string]any{
			"scanning":      true,
			"synced_height": syncedHeight,
			"chain_height":  chainHeight,
		})
		return
	}

	total, unspent := s.wallet.OutputCount()
	pendingUnconfirmed := s.wallet.PendingUnconfirmedBalance()

	etaSeconds := int64(0)
	if pendingUnconfirmed > 0 {
		eta := time.Duration(wallet.SafeConfirmations+1) * wallet.EstimatedBlockInterval
		etaSeconds = int64(eta.Seconds())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scanning":                 false,
		"spendable":                s.wallet.SpendableBalance(chainHeight),
		"pending":                  s.wallet.PendingBalance(chainHeight),
		"pending_unconfirmed":      pendingUnconfirmed,
		"pending_unconfirmed_eta":  etaSeconds,
		"total":                    s.wallet.Balance(),
		"outputs_total":            total,
		"outputs_unspent":          unspent,
		"chain_height":             chainHeight,
		"synced_height":            syncedHeight,
		"memo_decrypt_failures":    s.wallet.MemoDecryptFailureCount(),
		"memo_decrypt_last_height": s.wallet.MemoDecryptLastFailureHeight(),
	})
}

// handleAddress returns the wallet's stealth address.
// GET /api/wallet/address
func (s *APIServer) handleAddress(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"address":   s.wallet.Address(),
		"view_only": s.wallet.IsViewOnly(),
	})
}

// handleHistory returns wallet output history.
// GET /api/wallet/history
func (s *APIServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	outputs := s.wallet.AllOutputs()

	type outputEntry struct {
		TxID        string `json:"txid"`
		OutputIndex int    `json:"output_index"`
		Amount      uint64 `json:"amount"`
		BlockHeight uint64 `json:"block_height"`
		IsCoinbase  bool   `json:"is_coinbase"`
		Spent       bool   `json:"spent"`
		SpentHeight uint64 `json:"spent_height,omitempty"`
		MemoHex     string `json:"memo_hex,omitempty"`
	}

	entries := make([]outputEntry, len(outputs))
	for i, out := range outputs {
		entries[i] = outputEntry{
			TxID:        fmt.Sprintf("%x", out.TxID),
			OutputIndex: out.OutputIndex,
			Amount:      out.Amount,
			BlockHeight: out.BlockHeight,
			IsCoinbase:  out.IsCoinbase,
			Spent:       out.Spent,
			SpentHeight: out.SpentHeight,
		}
		if len(out.Memo) > 0 {
			entries[i].MemoHex = hex.EncodeToString(out.Memo)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(entries),
		"outputs": entries,
	})
}

func parseWalletSendsIntQuery(r *http.Request, name string, defaultValue, minValue, maxValue int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue {
		return 0, fmt.Errorf("%s must be an integer >= %d", name, minValue)
	}
	if maxValue > 0 && value > maxValue {
		return maxValue, nil
	}
	return value, nil
}

// handleSends returns wallet-recorded outbound sends.
// GET /api/wallet/sends
func (s *APIServer) handleSends(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	limit, err := parseWalletSendsIntQuery(r, "limit", 50, 0, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := parseWalletSendsIntQuery(r, "offset", 0, 0, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	order := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("order")))
	if order == "" {
		order = "desc"
	}
	if order != "desc" && order != "asc" {
		writeError(w, http.StatusBadRequest, "order must be asc or desc")
		return
	}

	type sendRecipientEntry struct {
		Address string `json:"address"`
		Amount  uint64 `json:"amount"`
		MemoHex string `json:"memo_hex,omitempty"`
	}

	type sendEntry struct {
		TxID           string               `json:"txid"`
		Timestamp      int64                `json:"timestamp"`
		RecordedHeight uint64               `json:"recorded_height"`
		ChainState     string               `json:"chain_state"`
		Confirmations  uint64               `json:"confirmations"`
		InMempool      bool                 `json:"in_mempool"`
		Fee            uint64               `json:"fee"`
		TotalAmount    uint64               `json:"total_amount"`
		Recipients     []sendRecipientEntry `json:"recipients"`
	}

	type sendRecordView struct {
		record *wallet.SendRecord
		txid   string
	}

	records := s.wallet.SendRecords()
	views := make([]sendRecordView, 0, len(records))
	seen := make(map[[32]byte]struct{}, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		if _, ok := seen[record.TxID]; ok {
			continue
		}
		seen[record.TxID] = struct{}{}
		views = append(views, sendRecordView{
			record: record,
			txid:   fmt.Sprintf("%x", record.TxID),
		})
	}

	sort.SliceStable(views, func(i, j int) bool {
		left := views[i].record
		right := views[j].record
		if left.Timestamp == right.Timestamp {
			if order == "asc" {
				return views[i].txid < views[j].txid
			}
			return views[i].txid > views[j].txid
		}
		if order == "asc" {
			return left.Timestamp < right.Timestamp
		}
		return left.Timestamp > right.Timestamp
	})

	total := len(views)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	chainHeight := s.daemon.Chain().Height()
	pageViews := views[start:end]
	pageTxIDs := make([][32]byte, 0, len(pageViews))
	for _, view := range pageViews {
		pageTxIDs = append(pageTxIDs, view.record.TxID)
	}
	txStates := s.walletSendChainStates(pageTxIDs, chainHeight)

	sends := make([]sendEntry, 0, end-start)
	for _, view := range pageViews {
		record := view.record
		txState := txStates[record.TxID]

		rawRecipients := record.GetRecipients()
		recipients := make([]sendRecipientEntry, 0, len(rawRecipients))
		for _, recipient := range rawRecipients {
			entry := sendRecipientEntry{
				Address: recipient.Address,
				Amount:  recipient.Amount,
			}
			if len(recipient.Memo) > 0 {
				entry.MemoHex = hex.EncodeToString(recipient.Memo)
			}
			recipients = append(recipients, entry)
		}

		sends = append(sends, sendEntry{
			TxID:           view.txid,
			Timestamp:      record.Timestamp,
			RecordedHeight: record.BlockHeight,
			ChainState:     txState.chainState,
			Confirmations:  txState.confirmations,
			InMempool:      txState.inMempool,
			Fee:            record.Fee,
			TotalAmount:    record.TotalAmount(),
			Recipients:     recipients,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(sends),
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"order":  order,
		"sends":  sends,
	})
}

// handleOutputs returns a comprehensive view of all wallet outputs.
// GET /api/wallet/outputs
func (s *APIServer) handleOutputs(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	outputs := s.wallet.AllOutputs()
	chainHeight := s.daemon.Chain().Height()
	syncedHeight := s.wallet.SyncedHeight()

	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].BlockHeight == outputs[j].BlockHeight {
			if outputs[i].TxID == outputs[j].TxID {
				return outputs[i].OutputIndex < outputs[j].OutputIndex
			}
			return strings.Compare(fmt.Sprintf("%x", outputs[i].TxID), fmt.Sprintf("%x", outputs[j].TxID)) < 0
		}
		return outputs[i].BlockHeight < outputs[j].BlockHeight
	})

	type outputEntry struct {
		TxID          string `json:"txid"`
		OutputIndex   int    `json:"output_index"`
		Amount        uint64 `json:"amount"`
		Status        string `json:"status"`
		Type          string `json:"type"`
		Confirmations uint64 `json:"confirmations"`
		BlockHeight   uint64 `json:"block_height"`
		SpentHeight   uint64 `json:"spent_height,omitempty"`
		OneTimePub    string `json:"one_time_pub"`
		Commitment    string `json:"commitment"`
		MemoHex       string `json:"memo_hex,omitempty"`
	}

	var spentCount, unspentCount, pendingCount int

	entries := make([]outputEntry, len(outputs))
	for i, out := range outputs {
		status := "unspent"
		if out.Spent {
			status = "spent"
		} else if !wallet.IsOutputMature(out, chainHeight) {
			status = "pending"
		}

		switch status {
		case "spent":
			spentCount++
		case "unspent":
			unspentCount++
		case "pending":
			pendingCount++
		}

		conf := uint64(0)
		if chainHeight >= out.BlockHeight {
			conf = chainHeight - out.BlockHeight
		}

		outType := "regular"
		if out.IsCoinbase {
			outType = "coinbase"
		}

		entries[i] = outputEntry{
			TxID:          fmt.Sprintf("%x", out.TxID),
			OutputIndex:   out.OutputIndex,
			Amount:        out.Amount,
			Status:        status,
			Type:          outType,
			Confirmations: conf,
			BlockHeight:   out.BlockHeight,
			SpentHeight:   out.SpentHeight,
			OneTimePub:    fmt.Sprintf("%x", out.OneTimePubKey),
			Commitment:    fmt.Sprintf("%x", out.Commitment),
		}
		if len(out.Memo) > 0 {
			entries[i].MemoHex = hex.EncodeToString(out.Memo)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"chain_height":  chainHeight,
		"synced_height": syncedHeight,
		"total":         len(entries),
		"spent":         spentCount,
		"unspent":       unspentCount,
		"pending":       pendingCount,
		"outputs":       entries,
	})
}

// handleSign signs an arbitrary message with the wallet's spend private key.
// POST /api/wallet/sign
func (s *APIServer) handleSign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}
	if s.wallet.IsViewOnly() {
		writeError(w, http.StatusForbidden, "view-only wallet cannot sign")
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(req.Message) > 1024 {
		writeError(w, http.StatusBadRequest, "message must be <= 1024 bytes")
		return
	}

	keys := s.wallet.Keys()
	sig, err := SchnorrSign(keys.SpendPrivKey[:], []byte(req.Message))
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	w.Header().Set("Cache-Control", "no-store")

	writeJSON(w, http.StatusOK, map[string]any{
		"address":   s.wallet.Address(),
		"signature": hex.EncodeToString(sig),
		"message":   req.Message,
	})
}

// recipientRequest is the per-recipient JSON input for send endpoints.
type recipientRequest struct {
	Address  string `json:"address"`
	Amount   uint64 `json:"amount"`
	MemoText string `json:"memo_text,omitempty"`
	MemoHex  string `json:"memo_hex,omitempty"`
}

// validatedRecipient is a recipientRequest that passed all checks.
type validatedRecipient struct {
	wallet.Recipient
	RawAddress   string
	ResolvedInfo *resolvedHandle
	Memo         []byte
}

const maxChangeSplit = 4

// validateRecipients resolves addresses, parses memos,
// and returns wallet.Recipient values ready for the builder.
func (s *APIServer) validateRecipients(raw []recipientRequest) ([]validatedRecipient, uint64, error) {
	if len(raw) == 0 {
		return nil, 0, errors.New("recipients array is required")
	}

	var totalSend uint64
	out := make([]validatedRecipient, len(raw))

	for i, rr := range raw {
		addr := sanitizeInput(rr.Address)
		if addr == "" {
			return nil, 0, fmt.Errorf("recipient %d: address is required", i)
		}

		resolvedAddr, resolvedInfo, err := resolveRecipientAddress(addr)
		if err != nil {
			return nil, 0, fmt.Errorf("recipient %d: %v", i, err)
		}

		spendPub, viewPub, err := parseValidatedAddress(resolvedAddr)
		if err != nil {
			return nil, 0, fmt.Errorf("recipient %d: invalid address", i)
		}

		if rr.Amount == 0 {
			return nil, 0, fmt.Errorf("recipient %d: amount must be greater than 0", i)
		}

		var ok bool
		totalSend, ok = wallet.AddU64(totalSend, rr.Amount)
		if !ok {
			return nil, 0, errors.New("recipient amount sum overflows")
		}

		var memo []byte
		if rr.MemoText != "" && rr.MemoHex != "" {
			return nil, 0, fmt.Errorf("recipient %d: provide either memo_text or memo_hex, not both", i)
		}
		if rr.MemoHex != "" {
			decoded, err := hex.DecodeString(rr.MemoHex)
			if err != nil {
				return nil, 0, fmt.Errorf("recipient %d: invalid memo_hex", i)
			}
			memo = decoded
		} else if rr.MemoText != "" {
			memo = []byte(rr.MemoText)
		}
		if len(memo) > wallet.MemoSize-4 {
			return nil, 0, fmt.Errorf("recipient %d: memo too long (max %d bytes)", i, wallet.MemoSize-4)
		}

		out[i] = validatedRecipient{
			Recipient: wallet.Recipient{
				SpendPubKey: spendPub,
				ViewPubKey:  viewPub,
				Amount:      rr.Amount,
				Memo:        memo,
			},
			RawAddress:   addr,
			ResolvedInfo: resolvedInfo,
			Memo:         memo,
		}
	}
	return out, totalSend, nil
}

// buildRecipientResults converts validated recipients to the JSON response array.
func buildRecipientResults(validated []validatedRecipient) []map[string]any {
	results := make([]map[string]any, len(validated))
	for i, v := range validated {
		entry := map[string]any{
			"address": v.RawAddress,
			"amount":  v.Amount,
		}
		if v.ResolvedInfo != nil {
			entry["resolved_handle"] = v.ResolvedInfo.Handle
			entry["resolved_address"] = v.ResolvedInfo.Address
			entry["resolver_verified"] = v.ResolvedInfo.Verified
		}
		if len(v.Memo) > 0 {
			entry["memo_hex"] = hex.EncodeToString(v.Memo)
		}
		results[i] = entry
	}
	return results
}

// buildSendRecipients converts validated recipients to wallet.SendRecipient records.
func buildSendRecipients(validated []validatedRecipient) []wallet.SendRecipient {
	out := make([]wallet.SendRecipient, len(validated))
	for i, v := range validated {
		out[i] = wallet.SendRecipient{
			Address: v.RawAddress,
			Amount:  v.Amount,
			Memo:    v.Memo,
		}
	}
	return out
}

// toWalletRecipients extracts the wallet.Recipient slice from validated recipients.
func toWalletRecipients(validated []validatedRecipient) []wallet.Recipient {
	out := make([]wallet.Recipient, len(validated))
	for i, v := range validated {
		out[i] = v.Recipient
	}
	return out
}

// handleSendStatus returns the persisted status for a standard send idempotency key.
// GET /api/wallet/send/status?idempotency_key=...
func (s *APIServer) handleSendStatus(w http.ResponseWriter, r *http.Request) {
	s.handleIdempotentSendStatus(w, r, "send:")
}

// handleSendAdvancedStatus returns the persisted status for an advanced send idempotency key.
// GET /api/wallet/send/advanced/status?idempotency_key=...
func (s *APIServer) handleSendAdvancedStatus(w http.ResponseWriter, r *http.Request) {
	s.handleIdempotentSendStatus(w, r, "send-advanced:")
}

func (s *APIServer) handleIdempotentSendStatus(w http.ResponseWriter, r *http.Request, prefix string) {
	idemKey := strings.TrimSpace(r.URL.Query().Get("idempotency_key"))
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key is required")
		return
	}
	if len(idemKey) > 128 {
		writeError(w, http.StatusBadRequest, "idempotency key too long")
		return
	}

	entry, ok := s.sendIdem.lookup(time.Now(), prefix+idemKey)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"idempotency_key": idemKey,
			"state":           "not_found",
		})
		return
	}

	resp := map[string]any{
		"idempotency_key": idemKey,
		"state":           string(entry.State),
		"created_at":      entry.CreatedAt,
		"updated_at":      entry.UpdatedAt,
	}

	switch entry.State {
	case idempotencyStateCompleted:
		resp["original_status"] = entry.Result.status
		if decoded, ok := decodeCachedJSONBody(entry.Result.body); ok {
			resp["result"] = decoded
		}
	case idempotencyStateFailed:
		resp["original_status"] = entry.Result.status
		resp["error"] = extractCachedError(entry.Result.body)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleSend builds and broadcasts a transaction.
// POST /api/wallet/send
func (s *APIServer) handleSend(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}
	if s.wallet.IsViewOnly() {
		writeError(w, http.StatusForbidden, "view-only wallet cannot send")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	cacheKey := ""
	var reqHash [32]byte
	if idemKey != "" {
		if len(idemKey) > 128 {
			writeError(w, http.StatusBadRequest, "idempotency key too long")
			return
		}
		reqHash = hashRequestBody(bodyBytes)
		cacheKey = "send:" + idemKey
		state, res := s.sendIdem.getOrStart(time.Now(), cacheKey, reqHash)
		switch state {
		case "replay":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.status)
			_, _ = w.Write(res.body)
			return
		case "mismatch":
			writeError(w, http.StatusConflict, "idempotency key reuse with different request")
			return
		case "inflight":
			writeError(w, http.StatusConflict, "idempotency key in progress")
			return
		case "start":
			// proceed (complete on return)
		default:
			writeError(w, http.StatusInternalServerError, "idempotency state error")
			return
		}

		cw := newCapturingResponseWriter(w)
		w = cw
		defer func() {
			if cw.status == http.StatusTooManyRequests {
				s.sendIdem.abandon(cacheKey)
				return
			}
			if cw.wroteAny {
				s.sendIdem.complete(time.Now(), cacheKey, reqHash, cw.status, cw.buf.Bytes())
			} else {
				s.sendIdem.abandon(cacheKey)
			}
		}()
	}

	ip := clientIP(r)
	if !s.sendLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "send rate limit exceeded")
		return
	}

	select {
	case s.sendSem <- struct{}{}:
		defer func() { <-s.sendSem }()
	default:
		writeError(w, http.StatusTooManyRequests, "send busy, retry later")
		return
	}

	var req struct {
		Recipients []recipientRequest `json:"recipients"`
		DryRun     bool               `json:"dry_run"`
	}
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	validated, totalSend, err := s.validateRecipients(req.Recipients)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	height := s.daemon.Chain().Height()
	spendable := s.wallet.SpendableBalance(height)
	if spendable < totalSend {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("insufficient spendable balance: have %d, need %d", spendable, totalSend))
		return
	}

	if req.DryRun {
		fee := sendMinFee
		for i := 0; i < 4; i++ {
			need, ok := wallet.AddU64(totalSend, fee)
			if !ok {
				writeError(w, http.StatusBadRequest, "send amount + fee overflows")
				return
			}
			lease, inputs, err := s.wallet.ReserveMatureInputs(height, need, 2*time.Second)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("insufficient funds: %v", err))
				return
			}
			s.wallet.ReleaseInputLease(lease)

			var inputTotal uint64
			for _, inp := range inputs {
				inputTotal, ok = wallet.AddU64(inputTotal, inp.Amount)
				if !ok {
					writeError(w, http.StatusBadRequest, "input amount sum overflows")
					return
				}
			}

			outputCount := len(validated) + 1
			if inputTotal == totalSend+fee {
				outputCount = len(validated)
			}
			estimatedSize := wallet.EstimateTxSizeBytes(len(inputs), outputCount, RingSize)
			requiredFee := max(sendMinFee, uint64(estimatedSize)*sendFeePerByte)
			if requiredFee <= fee {
				change := inputTotal - totalSend - fee
				writeJSON(w, http.StatusOK, map[string]any{
					"dry_run":     true,
					"fee":         fee,
					"change":      change,
					"input_total": inputTotal,
					"input_count": len(inputs),
					"recipients":  buildRecipientResults(validated),
				})
				return
			}
			fee = requiredFee
		}
		writeError(w, http.StatusBadRequest, "fee estimation did not converge")
		return
	}

	builder := s.createTxBuilder()
	result, err := builder.Transfer(toWalletRecipients(validated), sendFeePerByte, height)
	if err != nil {
		writeSendError(w, r, err)
		return
	}

	if err := s.daemon.SubmitTransaction(result.TxData); err != nil {
		s.wallet.ReleaseInputLease(result.InputLease)
		writeSendError(w, r, err)
		return
	}

	for _, spent := range result.SpentOutputs {
		s.wallet.MarkSpentByTx(spent.OneTimePubKey, result.TxID)
	}

	s.wallet.RecordSend(&wallet.SendRecord{
		TxID:        result.TxID,
		Timestamp:   time.Now().Unix(),
		Recipients:  buildSendRecipients(validated),
		Fee:         result.Fee,
		BlockHeight: height,
	})
	if result.Change > 0 {
		s.wallet.AddPendingCredit(result.TxID, result.Change)
	}
	if err := s.wallet.Save(); err != nil {
		log.Printf("Warning: wallet persistence failed after send %x: %v", result.TxID, err)
	}

	resp := map[string]any{
		"txid":       fmt.Sprintf("%x", result.TxID),
		"fee":        result.Fee,
		"change":     result.Change,
		"dry_run":    false,
		"recipients": buildRecipientResults(validated),
	}
	respBody, err := encodeJSONResponse(resp)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}
	if cacheKey != "" {
		s.sendIdem.complete(time.Now(), cacheKey, reqHash, http.StatusOK, respBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(respBody); err != nil {
		log.Printf("Warning: failed to write JSON response: %v", err)
	}
}

// handleSendAdvanced builds a transaction using caller-specified inputs (coin control).
// POST /api/wallet/send/advanced
func (s *APIServer) handleSendAdvanced(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}
	if s.wallet.IsViewOnly() {
		writeError(w, http.StatusForbidden, "view-only wallet cannot send")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var reqHash [32]byte
	if idemKey != "" {
		if len(idemKey) > 128 {
			writeError(w, http.StatusBadRequest, "idempotency key too long")
			return
		}
		reqHash = hashRequestBody(bodyBytes)
		cacheKey := "send-advanced:" + idemKey
		state, res := s.sendIdem.getOrStart(time.Now(), cacheKey, reqHash)
		switch state {
		case "replay":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.status)
			_, _ = w.Write(res.body)
			return
		case "mismatch":
			writeError(w, http.StatusConflict, "idempotency key reuse with different request")
			return
		case "inflight":
			writeError(w, http.StatusConflict, "idempotency key in progress")
			return
		case "start":
		default:
			writeError(w, http.StatusInternalServerError, "idempotency state error")
			return
		}

		cw := newCapturingResponseWriter(w)
		w = cw
		defer func() {
			if cw.status == http.StatusTooManyRequests {
				s.sendIdem.abandon(cacheKey)
				return
			}
			if cw.wroteAny {
				s.sendIdem.complete(time.Now(), cacheKey, reqHash, cw.status, cw.buf.Bytes())
			} else {
				s.sendIdem.abandon(cacheKey)
			}
		}()
	}

	ip := clientIP(r)
	if !s.sendLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "send rate limit exceeded")
		return
	}

	select {
	case s.sendSem <- struct{}{}:
		defer func() { <-s.sendSem }()
	default:
		writeError(w, http.StatusTooManyRequests, "send busy, retry later")
		return
	}

	var req struct {
		Recipients  []recipientRequest `json:"recipients"`
		DryRun      bool               `json:"dry_run"`
		ChangeSplit int                `json:"change_split"`
		Inputs      []struct {
			TxID        string `json:"txid"`
			OutputIndex int    `json:"output_index"`
		} `json:"inputs"`
	}
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if len(req.Inputs) == 0 {
		writeError(w, http.StatusBadRequest, "inputs array is required for coin control")
		return
	}
	if len(req.Inputs) > 256 {
		writeError(w, http.StatusBadRequest, "too many inputs (max 256)")
		return
	}

	changeSplit := req.ChangeSplit
	if changeSplit < 1 {
		changeSplit = 1
	}
	if changeSplit > maxChangeSplit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("change_split must be 1-%d", maxChangeSplit))
		return
	}

	validated, totalSend, err := s.validateRecipients(req.Recipients)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	refs := make([]wallet.OutputRef, len(req.Inputs))
	for i, inp := range req.Inputs {
		if len(inp.TxID) != 64 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("input %d: txid must be 64 hex characters", i))
			return
		}
		txidBytes, err := hex.DecodeString(inp.TxID)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("input %d: invalid txid hex", i))
			return
		}
		copy(refs[i].TxID[:], txidBytes)
		refs[i].OutputIndex = inp.OutputIndex
	}

	height := s.daemon.Chain().Height()

	lease, inputs, err := s.wallet.ReserveSpecificInputs(refs, height, 2*time.Minute)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	releaseLease := true
	defer func() {
		if releaseLease {
			s.wallet.ReleaseInputLease(lease)
		}
	}()

	var inputTotal uint64
	for _, inp := range inputs {
		var ok bool
		inputTotal, ok = wallet.AddU64(inputTotal, inp.Amount)
		if !ok {
			writeError(w, http.StatusBadRequest, "input amount sum overflows")
			return
		}
	}

	if req.DryRun {
		outputCount := len(validated) + changeSplit
		estimatedSize := wallet.EstimateTxSizeBytes(len(inputs), outputCount, RingSize)
		fee := max(sendMinFee, uint64(estimatedSize)*sendFeePerByte)

		need, ok := wallet.AddU64(totalSend, fee)
		if !ok || inputTotal < need {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"insufficient input amount: inputs total %d, need at least %d (amount %d + fee %d)",
				inputTotal, totalSend+fee, totalSend, fee))
			return
		}

		change := inputTotal - totalSend - fee
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run":      true,
			"fee":          fee,
			"change":       change,
			"change_split": changeSplit,
			"input_total":  inputTotal,
			"input_count":  len(inputs),
			"recipients":   buildRecipientResults(validated),
		})
		return
	}

	releaseLease = false
	builder := s.createTxBuilder()
	result, err := builder.TransferWithInputs(inputs, lease, toWalletRecipients(validated), sendFeePerByte, changeSplit, height)
	if err != nil {
		writeSendError(w, r, err)
		return
	}

	if err := s.daemon.SubmitTransaction(result.TxData); err != nil {
		s.wallet.ReleaseInputLease(result.InputLease)
		writeSendError(w, r, err)
		return
	}

	for _, spent := range result.SpentOutputs {
		s.wallet.MarkSpentByTx(spent.OneTimePubKey, result.TxID)
	}

	s.wallet.RecordSend(&wallet.SendRecord{
		TxID:        result.TxID,
		Timestamp:   time.Now().Unix(),
		Recipients:  buildSendRecipients(validated),
		Fee:         result.Fee,
		BlockHeight: height,
	})
	if result.Change > 0 {
		s.wallet.AddPendingCredit(result.TxID, result.Change)
	}
	if err := s.wallet.Save(); err != nil {
		log.Printf("Warning: wallet persistence failed after send %x: %v", result.TxID, err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"txid":         fmt.Sprintf("%x", result.TxID),
		"fee":          result.Fee,
		"change":       result.Change,
		"change_split": changeSplit,
		"input_total":  inputTotal,
		"input_count":  len(inputs),
		"dry_run":      false,
		"recipients":   buildRecipientResults(validated),
	})
}

type capturingResponseWriter struct {
	w        http.ResponseWriter
	status   int
	wroteAny bool
	buf      bytes.Buffer
}

func newCapturingResponseWriter(w http.ResponseWriter) *capturingResponseWriter {
	return &capturingResponseWriter{w: w, status: http.StatusOK}
}

func (c *capturingResponseWriter) Header() http.Header { return c.w.Header() }

func (c *capturingResponseWriter) WriteHeader(statusCode int) {
	c.status = statusCode
	c.wroteAny = true
	c.w.WriteHeader(statusCode)
}

func (c *capturingResponseWriter) Write(p []byte) (int, error) {
	c.wroteAny = true
	_, _ = c.buf.Write(p)
	return c.w.Write(p)
}

// handleUnloadWallet locks and unloads the currently loaded wallet.
// POST /api/wallet/unload
func (s *APIServer) handleUnloadWallet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.wallet == nil {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "no wallet loaded")
		return
	}
	s.wallet = nil
	s.scanner = nil
	s.locked = true
	s.passwordHash = [32]byte{}
	s.passwordHashSet = false
	s.walletLoading = false
	s.mu.Unlock()

	// Clear mining reward keys
	s.daemon.Miner().SetRewardKeys([32]byte{}, [32]byte{})

	// Clear CLI wallet references
	s.cli.mu.Lock()
	s.cli.wallet = nil
	s.cli.scanner = nil
	s.cli.passwordHash = [32]byte{}
	s.cli.passwordHashSet = false
	s.cli.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"unloaded": true})
}

// handleLock locks the wallet.
// POST /api/wallet/lock
func (s *APIServer) handleLock(w http.ResponseWriter, r *http.Request) {
	if s.wallet == nil {
		writeError(w, http.StatusServiceUnavailable, "no wallet loaded")
		return
	}

	s.mu.Lock()
	s.locked = true
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"locked": true})
}

// handleUnlock unlocks the wallet.
// POST /api/wallet/unlock
func (s *APIServer) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if s.wallet == nil {
		writeError(w, http.StatusServiceUnavailable, "no wallet loaded")
		return
	}

	ip := clientIP(r)
	if wait, lockedUntil := s.unlockAttempts.precheck(ip); !lockedUntil.IsZero() {
		retryAfter := int(time.Until(lockedUntil).Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "too many unlock attempts; try again later")
		return
	} else if wait > 0 {
		retryAfter := int(wait.Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "unlock backoff active; retry later")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	s.mu.RLock()
	hashSet := s.passwordHashSet
	expectedHash := s.passwordHash
	s.mu.RUnlock()
	if !hashSet {
		writeError(w, http.StatusServiceUnavailable, "unlock unavailable: password state not initialized")
		return
	}

	pw := []byte(req.Password)
	actualHash := passwordHash(pw)
	wipeBytes(pw)
	if subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) != 1 {
		delay, lockedUntil := s.unlockAttempts.recordFailure(ip)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
			}
		}
		if !lockedUntil.IsZero() {
			retryAfter := int(time.Until(lockedUntil).Seconds())
			retryAfter = max(retryAfter, 1)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "too many unlock attempts; try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	s.unlockAttempts.recordSuccess(ip)

	s.mu.Lock()
	s.locked = false
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"locked": false})
}

// handleLoadWallet loads an existing wallet from disk at runtime.
// Used in daemon mode where the app starts without a wallet.
// POST /api/wallet/load
func (s *APIServer) handleLoadWallet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.wallet != nil || s.walletLoading {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "wallet already loaded")
		return
	}
	s.walletLoading = true
	s.mu.Unlock()
	committed := false
	defer func() {
		if committed {
			return
		}
		s.mu.Lock()
		s.walletLoading = false
		s.mu.Unlock()
	}()

	var req struct {
		Password string `json:"password"`
		Filepath string `json:"filepath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Password) < 3 {
		writeError(w, http.StatusBadRequest, "password must be at least 3 characters")
		return
	}

	walletPath := s.cli.walletFile
	if req.Filepath != "" {
		base := filepath.Base(req.Filepath)
		if base == "." || base == "/" {
			writeError(w, http.StatusBadRequest, "invalid filepath")
			return
		}
		walletPath = filepath.Join(filepath.Dir(s.cli.walletFile), base)
	}

	password := []byte(req.Password)
	passHash := passwordHash(password)
	wl, err := wallet.LoadWallet(walletPath, password, defaultWalletConfig())
	wipeBytes(password)
	if err != nil {
		if status, msg, ok := walletLoadClientError(err); ok {
			writeError(w, status, msg)
		} else {
			writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		}
		return
	}

	scanner := wallet.NewScanner(wl, defaultScannerConfig())

	// Point miner rewards at this wallet
	s.daemon.Miner().SetRewardKeys(wl.Keys().SpendPubKey, wl.Keys().ViewPubKey)

	// Handle chain-ahead-of-wallet (chain was reset while wallet was offline)
	chainHeight := s.daemon.Chain().Height()
	walletHeight := wl.SyncedHeight()
	if walletHeight > chainHeight {
		if removed := wl.RewindToHeight(chainHeight); removed > 0 {
			if err := wl.Save(); err != nil {
				writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
				return
			}
		}
		walletHeight = wl.SyncedHeight()
	}

	// Conservative reorg recovery: when wallet and chain heights match exactly,
	// rewind one block and rescan tip. This clears stale same-height branch data
	// even for wallets that predate tip-hash sync metadata.
	if walletHeight == chainHeight && chainHeight > 0 {
		wl.RewindToHeight(chainHeight - 1)
		walletHeight = wl.SyncedHeight()
	}

	wl.SetInputFilter(func(out *wallet.OwnedOutput) bool {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			return false
		}
		return s.daemon.Mempool().HasKeyImage(ki)
	})

	// Publish to API server — unlock since the caller proved the password.
	s.mu.Lock()
	s.wallet = wl
	s.scanner = scanner
	s.locked = false
	s.passwordHash = passHash
	s.passwordHashSet = true
	s.walletLoading = false
	s.mu.Unlock()
	committed = true

	// Publish to CLI (for autoScanBlocks / shutdown)
	s.cli.mu.Lock()
	s.cli.wallet = wl
	s.cli.scanner = scanner
	s.cli.passwordHash = passHash
	s.cli.passwordHashSet = true
	s.cli.mu.Unlock()

	go s.catchUpScan()

	writeJSON(w, http.StatusOK, map[string]any{
		"loaded":        true,
		"address":       wl.Address(),
		"filename":      filepath.Base(walletPath),
		"synced_height": wl.SyncedHeight(),
		"chain_height":  chainHeight,
	})
}

// handleCreateWallet generates a new wallet with a fresh BIP39 seed.
// POST /api/wallet/create
func (s *APIServer) handleCreateWallet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.wallet != nil || s.walletLoading {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "wallet already loaded")
		return
	}
	s.walletLoading = true
	s.mu.Unlock()
	committed := false
	defer func() {
		if committed {
			return
		}
		s.mu.Lock()
		s.walletLoading = false
		s.mu.Unlock()
	}()

	var req struct {
		Password string `json:"password"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Password) < 3 {
		writeError(w, http.StatusBadRequest, "password must be at least 3 characters")
		return
	}

	walletPath := s.cli.walletFile
	if req.Filename != "" {
		base := filepath.Base(req.Filename)
		if base == "." || base == "/" {
			writeError(w, http.StatusBadRequest, "invalid filename")
			return
		}
		walletPath = filepath.Join(filepath.Dir(s.cli.walletFile), base)
	}

	if _, err := os.Stat(walletPath); err == nil {
		writeError(w, http.StatusConflict, "wallet file already exists: "+filepath.Base(walletPath))
		return
	}

	password := []byte(req.Password)
	passHash := passwordHash(password)
	wl, err := wallet.NewWallet(walletPath, password, defaultWalletConfig())
	wipeBytes(password)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	scanner := wallet.NewScanner(wl, defaultScannerConfig())

	s.daemon.Miner().SetRewardKeys(wl.Keys().SpendPubKey, wl.Keys().ViewPubKey)

	chainHeight := s.daemon.Chain().Height()
	if chainHeight > 0 {
		wl.SetSyncedHeight(chainHeight)
		if err := wl.Save(); err != nil {
			writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
			return
		}
	}

	wl.SetInputFilter(func(out *wallet.OwnedOutput) bool {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			return false
		}
		return s.daemon.Mempool().HasKeyImage(ki)
	})

	s.mu.Lock()
	s.wallet = wl
	s.scanner = scanner
	s.locked = false
	s.passwordHash = passHash
	s.passwordHashSet = true
	s.walletLoading = false
	s.mu.Unlock()
	committed = true

	s.cli.mu.Lock()
	s.cli.wallet = wl
	s.cli.scanner = scanner
	s.cli.passwordHash = passHash
	s.cli.passwordHashSet = true
	s.cli.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"created":  true,
		"address":  wl.Address(),
		"filename": filepath.Base(walletPath),
	})
}

// handleHealth checks if the API is healthy.
// GET /api/health
func (s *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "timestamp": time.Now().Format(time.RFC3339)})
}

// handleImportWallet creates a new wallet from a BIP39 recovery seed.
// POST /api/wallet/import
func (s *APIServer) handleImportWallet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.wallet != nil || s.walletLoading {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "wallet already loaded")
		return
	}
	s.walletLoading = true
	s.mu.Unlock()
	committed := false
	defer func() {
		if committed {
			return
		}
		s.mu.Lock()
		s.walletLoading = false
		s.mu.Unlock()
	}()

	var req struct {
		Mnemonic string `json:"mnemonic"`
		Password string `json:"password"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Mnemonic == "" {
		writeError(w, http.StatusBadRequest, "mnemonic is required")
		return
	}
	if len(req.Password) < 3 {
		writeError(w, http.StatusBadRequest, "password must be at least 3 characters")
		return
	}
	if !wallet.ValidateMnemonic(req.Mnemonic) {
		writeError(w, http.StatusBadRequest, "invalid mnemonic phrase")
		return
	}

	// Resolve wallet path: basename only, same directory as configured --wallet path
	walletPath := s.cli.walletFile
	if req.Filename != "" {
		base := filepath.Base(req.Filename)
		if base == "." || base == "/" {
			writeError(w, http.StatusBadRequest, "invalid filename")
			return
		}
		walletPath = filepath.Join(filepath.Dir(s.cli.walletFile), base)
	}

	// Don't overwrite an existing file
	if _, err := os.Stat(walletPath); err == nil {
		writeError(w, http.StatusConflict, "wallet file already exists: "+filepath.Base(walletPath))
		return
	}

	password := []byte(req.Password)
	passHash := passwordHash(password)
	wl, err := wallet.NewWalletFromMnemonic(walletPath, password, req.Mnemonic, defaultWalletConfig())
	wipeBytes(password)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	scanner := wallet.NewScanner(wl, defaultScannerConfig())

	// Point miner rewards at this wallet
	s.daemon.Miner().SetRewardKeys(wl.Keys().SpendPubKey, wl.Keys().ViewPubKey)

	chainHeight := s.daemon.Chain().Height()

	wl.SetInputFilter(func(out *wallet.OwnedOutput) bool {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			return false
		}
		return s.daemon.Mempool().HasKeyImage(ki)
	})

	// Publish to API server — unlock since the caller proved the password.
	s.mu.Lock()
	s.wallet = wl
	s.scanner = scanner
	s.locked = false
	s.passwordHash = passHash
	s.passwordHashSet = true
	s.walletLoading = false
	s.mu.Unlock()
	committed = true

	// Publish to CLI (for autoScanBlocks / shutdown)
	s.cli.mu.Lock()
	s.cli.wallet = wl
	s.cli.scanner = scanner
	s.cli.passwordHash = passHash
	s.cli.passwordHashSet = true
	s.cli.mu.Unlock()

	go s.catchUpScan()

	writeJSON(w, http.StatusOK, map[string]any{
		"imported":      true,
		"address":       wl.Address(),
		"filename":      filepath.Base(walletPath),
		"synced_height": wl.SyncedHeight(),
		"chain_height":  chainHeight,
	})
}

// handleSeed returns the wallet recovery seed (BIP39 mnemonic).
// POST /api/wallet/seed
func (s *APIServer) handleSeed(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	ip := clientIP(r)
	if wait, lockedUntil := s.unlockAttempts.precheck(ip); !lockedUntil.IsZero() {
		retryAfter := int(time.Until(lockedUntil).Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	} else if wait > 0 {
		retryAfter := int(wait.Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "attempt backoff active; retry later")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	s.mu.RLock()
	hashSet := s.passwordHashSet
	expectedHash := s.passwordHash
	s.mu.RUnlock()
	if !hashSet {
		writeError(w, http.StatusServiceUnavailable, "seed unavailable: password state not initialized")
		return
	}
	pw := []byte(req.Password)
	actualHash := passwordHash(pw)
	wipeBytes(pw)
	if subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) != 1 {
		delay, lockedUntil := s.unlockAttempts.recordFailure(ip)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
			}
		}
		if !lockedUntil.IsZero() {
			retryAfter := int(time.Until(lockedUntil).Seconds())
			retryAfter = max(retryAfter, 1)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "too many attempts; try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	s.unlockAttempts.recordSuccess(ip)

	mnemonic, err := s.wallet.Mnemonic()
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}
	if mnemonic == "" {
		writeError(w, http.StatusNotFound, "no recovery seed available")
		return
	}

	// Sensitive response: discourage caching.
	w.Header().Set("Cache-Control", "no-store")

	writeJSON(w, http.StatusOK, map[string]any{
		"mnemonic": mnemonic,
		"words":    strings.Fields(mnemonic),
	})
}

// handleWalletSync rescans blocks from the wallet's synced height to the chain
// tip, scanning for owned outputs and spent key images.
// POST /api/wallet/sync
func (s *APIServer) handleWalletSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	chainHeight := s.daemon.Chain().Height()
	walletHeight := s.wallet.SyncedHeight()

	if walletHeight >= chainHeight {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "already synced",
			"synced_height": walletHeight,
			"chain_height":  chainHeight,
		})
		return
	}

	go s.catchUpScan()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "scanning",
		"synced_height": walletHeight,
		"chain_height":  chainHeight,
	})
}

// handleProve derives the deterministic tx private key to prove a transaction
// was sent by this wallet. Works on both full and view-only wallets (uses ViewPrivKey).
// POST /api/wallet/prove
func (s *APIServer) handleProve(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	var req struct {
		TxID string `json:"txid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.TxID) != 64 {
		writeError(w, http.StatusBadRequest, "txid must be 64 hex characters")
		return
	}

	tx, _, found := s.daemon.Chain().FindTxByHashStr(req.TxID)
	if !found {
		writeError(w, http.StatusNotFound, "transaction not found on chain")
		return
	}
	if len(tx.Inputs) == 0 {
		writeError(w, http.StatusBadRequest, "coinbase transactions have no sender proof")
		return
	}

	keyImages := make([][32]byte, len(tx.Inputs))
	for i, inp := range tx.Inputs {
		keyImages[i] = inp.KeyImage
	}

	keys := s.wallet.Keys()
	txPriv, err := DeriveDeterministicTxKey(keys.ViewPrivKey, keyImages)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to derive tx key", err)
		return
	}

	derivedPub, err := ScalarToPubKey(txPriv)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to derive public key", err)
		return
	}

	if derivedPub != tx.TxPublicKey {
		writeError(w, http.StatusConflict, "transaction was not sent by this wallet")
		return
	}

	w.Header().Set("Cache-Control", "no-store")

	writeJSON(w, http.StatusOK, map[string]any{
		"txid":   req.TxID,
		"tx_key": hex.EncodeToString(txPriv[:]),
	})
}

// handleAudit scans all wallet outputs for duplicate key images, which indicate
// burned (permanently unspendable) funds from a historical self-send bug.
// POST /api/wallet/audit
func (s *APIServer) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	outputs := s.wallet.AllOutputs()

	type keyImageGroup struct {
		keyImage [32]byte
		outputs  []*wallet.OwnedOutput
	}

	groups := make(map[[32]byte]*keyImageGroup)
	var failedCount int

	for _, out := range outputs {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			failedCount++
			continue
		}
		g, ok := groups[ki]
		if !ok {
			g = &keyImageGroup{keyImage: ki}
			groups[ki] = g
		}
		g.outputs = append(g.outputs, out)
	}

	var duplicates []map[string]any
	var totalBurned uint64
	var burnedOutputs int

	for _, g := range groups {
		if len(g.outputs) <= 1 {
			continue
		}

		var groupTotal uint64
		var maxAmount uint64
		outs := make([]map[string]any, len(g.outputs))
		for i, out := range g.outputs {
			groupTotal += out.Amount
			if out.Amount > maxAmount {
				maxAmount = out.Amount
			}
			outs[i] = map[string]any{
				"txid":         hex.EncodeToString(out.TxID[:]),
				"output_index": out.OutputIndex,
				"amount":       out.Amount,
				"spent":        out.Spent,
				"block_height": out.BlockHeight,
			}
		}
		burned := groupTotal - maxAmount
		totalBurned += burned
		burnedOutputs += len(g.outputs) - 1

		duplicates = append(duplicates, map[string]any{
			"key_image":         hex.EncodeToString(g.keyImage[:]),
			"outputs":           outs,
			"burned_amount":     burned,
			"unspendable_count": len(g.outputs) - 1,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_outputs":     len(outputs),
		"unique_key_images": len(groups),
		"failed_key_images": failedCount,
		"duplicate_groups":  duplicates,
		"total_burned":      totalBurned,
		"burned_outputs":    burnedOutputs,
	})
}

// handleViewKeys returns the wallet's view-only keys (spend public, view private,
// view public). Requires password confirmation and brute-force protection, like seed.
// POST /api/wallet/viewkeys
func (s *APIServer) handleViewKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}
	if s.wallet.IsViewOnly() {
		writeError(w, http.StatusForbidden, "this is already a view-only wallet")
		return
	}

	ip := clientIP(r)
	if wait, lockedUntil := s.unlockAttempts.precheck(ip); !lockedUntil.IsZero() {
		retryAfter := int(time.Until(lockedUntil).Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	} else if wait > 0 {
		retryAfter := int(wait.Seconds())
		retryAfter = max(retryAfter, 1)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "attempt backoff active; retry later")
		return
	}

	var req struct {
		Password     string `json:"password"`
		CreateFile   bool   `json:"create_file"`
		FilePassword string `json:"file_password"`
		Filename     string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	s.mu.RLock()
	hashSet := s.passwordHashSet
	expectedHash := s.passwordHash
	s.mu.RUnlock()
	if !hashSet {
		writeError(w, http.StatusServiceUnavailable, "viewkeys unavailable: password state not initialized")
		return
	}
	pw := []byte(req.Password)
	actualHash := passwordHash(pw)
	wipeBytes(pw)
	if subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) != 1 {
		delay, lockedUntil := s.unlockAttempts.recordFailure(ip)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
			}
		}
		if !lockedUntil.IsZero() {
			retryAfter := int(time.Until(lockedUntil).Seconds())
			retryAfter = max(retryAfter, 1)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "too many attempts; try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	s.unlockAttempts.recordSuccess(ip)

	keys := s.wallet.ExportViewOnlyKeys()

	w.Header().Set("Cache-Control", "no-store")

	resp := map[string]any{
		"spend_pub": hex.EncodeToString(keys.SpendPubKey[:]),
		"view_priv": hex.EncodeToString(keys.ViewPrivKey[:]),
		"view_pub":  hex.EncodeToString(keys.ViewPubKey[:]),
	}

	// Optionally also write a loadable view-only wallet file (encrypted with its
	// own file_password, never the main wallet password), so a single call can
	// both reveal the keys and produce a watch-only wallet to move elsewhere.
	if req.CreateFile {
		if len(req.FilePassword) < 3 {
			writeError(w, http.StatusBadRequest, "file_password must be at least 3 characters")
			return
		}

		walletPath := deriveViewWalletFilename(s.cli.walletFile)
		if req.Filename != "" {
			base := filepath.Base(req.Filename)
			if base == "." || base == "/" {
				writeError(w, http.StatusBadRequest, "invalid filename")
				return
			}
			if !strings.HasSuffix(base, ".wallet.dat") {
				base += ".wallet.dat"
			}
			walletPath = filepath.Join(filepath.Dir(s.cli.walletFile), base)
		}

		if _, err := os.Stat(walletPath); err == nil {
			writeError(w, http.StatusConflict, "wallet file already exists: "+filepath.Base(walletPath))
			return
		}

		filePw := []byte(req.FilePassword)
		viewWallet, err := wallet.NewViewOnlyWallet(walletPath, filePw, keys, defaultWalletConfig())
		wipeBytes(filePw)
		if err != nil {
			writeInternal(w, r, http.StatusInternalServerError, "failed to create view-only wallet", err)
			return
		}

		resp["created"] = true
		resp["filename"] = filepath.Base(walletPath)
		resp["path"] = walletPath
		resp["address"] = viewWallet.Address()
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCertify verifies the entire chain for difficulty, timestamp, and linkage
// violations. This is an arithmetic-only check (no PoW re-hashing).
// GET /api/certify
func (s *APIServer) handleCertify(w http.ResponseWriter, r *http.Request) {
	chain := s.daemon.Chain()
	height := chain.Height()

	violations := chain.VerifyChain()

	result := map[string]any{
		"height": height,
		"clean":  len(violations) == 0,
	}

	if len(violations) > 0 {
		vList := make([]map[string]any, len(violations))
		for i, v := range violations {
			vList[i] = map[string]any{
				"height":  v.Height,
				"message": v.Message,
			}
		}
		result["violations"] = vList
	}

	writeJSON(w, http.StatusOK, result)
}

// ============================================================================
// Mining handlers
// ============================================================================

// handleMiningStatus returns mining status and stats.
// GET /api/mining
func (s *APIServer) handleMiningStatus(w http.ResponseWriter, r *http.Request) {
	running := s.daemon.IsMining()

	resp := map[string]any{
		"running": running,
		"threads": s.daemon.Miner().Threads(),
	}

	if running {
		stats := s.daemon.MinerStats()
		resp["hashrate"] = s.daemon.Miner().HashRate()
		resp["hash_count"] = stats.HashCount
		resp["blocks_found"] = stats.BlocksFound
		resp["started_at"] = stats.StartTime.Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleMiningStart starts the miner.
// POST /api/mining/start
func (s *APIServer) handleMiningStart(w http.ResponseWriter, r *http.Request) {
	if s.daemon.IsMining() {
		writeError(w, http.StatusConflict, "mining already running")
		return
	}
	s.daemon.StartMining()
	writeJSON(w, http.StatusOK, map[string]any{"running": true})
}

// handleMiningStop stops the miner.
// POST /api/mining/stop
func (s *APIServer) handleMiningStop(w http.ResponseWriter, r *http.Request) {
	if !s.daemon.IsMining() {
		writeError(w, http.StatusConflict, "mining not running")
		return
	}
	s.daemon.StopMining()
	writeJSON(w, http.StatusOK, map[string]any{"running": false})
}

// handleBlockTemplate returns a block template for pool mining.
// The template includes a pre-built coinbase (using the wallet's keys),
// all selected mempool transactions, and the computed merkle root.
// Pool software distributes the header to miners; they find a valid nonce
// and submit back via POST /api/mining/submitblock.
// GET /api/mining/blocktemplate
func (s *APIServer) handleBlockTemplate(w http.ResponseWriter, r *http.Request) {
	if s.wallet == nil {
		writeError(w, http.StatusServiceUnavailable, "no wallet loaded")
		return
	}

	if s.daemon.syncMgr.IsSyncingFarBehind(blockTemplateSyncHeightTolerance) {
		writeError(w, http.StatusServiceUnavailable, "node is syncing")
		return
	}

	// Read height, prevHash, and difficulty as a single atomic snapshot so a
	// concurrent reorg cannot produce an inconsistent template.
	tp := s.daemon.Chain().TemplateParams()
	reward := GetBlockReward(tp.Height)

	// Optionally override the coinbase destination (pool/dev-fee switching).
	recipientSpendPub := s.wallet.SpendPubKey()
	recipientViewPub := s.wallet.ViewPubKey()
	rewardAddrUsed := s.wallet.Address()
	if addr := sanitizeInput(r.URL.Query().Get("address")); addr != "" {
		spendPub, viewPub, err := parseValidatedAddress(addr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid address")
			return
		}
		recipientSpendPub = spendPub
		recipientViewPub = viewPub
		rewardAddrUsed = addr
	}

	// Create coinbase paying to the selected reward address
	coinbase, err := CreateCoinbase(recipientSpendPub, recipientViewPub, reward, tp.Height)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	// Get mempool transactions sorted by fee rate
	txs := s.daemon.Mempool().GetTransactionsForBlock(MaxBlockSize-1000, 1000)

	// Build transaction list (coinbase first)
	allTxs := make([]*Transaction, 0, len(txs)+1)
	allTxs = append(allTxs, coinbase.Tx)
	allTxs = append(allTxs, txs...)

	// Build block template (nonce = 0, to be solved by pool miners)
	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     tp.Height,
			PrevHash:   tp.PrevHash,
			Timestamp:  time.Now().Unix(),
			Difficulty: tp.Difficulty,
			Nonce:      0,
		},
		Transactions: allTxs,
	}

	// Compute merkle root
	merkleRoot, err := block.ComputeMerkleRoot()
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}
	block.Header.MerkleRoot = merkleRoot

	// Compute target for PoW validation
	target := DifficultyToTarget(block.Header.Difficulty)
	templateID, expiresAt, err := s.rememberMiningTemplateLease(block)
	if err != nil {
		switch {
		case errors.Is(err, errMiningTemplateCacheFull):
			var capacityErr *miningTemplateCacheFullError
			if errors.As(err, &capacityErr) {
				w.Header().Set("Retry-After", strconv.FormatInt(capacityErr.retryAfterSeconds, 10))
			}
			writeCodedError(w, http.StatusServiceUnavailable, "mining_template_cache_full", "mining template cache full, retry after a lease expires or the chain tip changes")
		case errors.Is(err, errMiningTemplateStale):
			writeCodedError(w, http.StatusServiceUnavailable, "mining_template_tip_changed", "chain tip changed while building template, retry")
		default:
			writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"block":                       block,
		"target":                      fmt.Sprintf("%x", target),
		"header_base":                 fmt.Sprintf("%x", block.Header.SerializeForPoW()),
		"reward_address_used":         rewardAddrUsed,
		"template_id":                 templateID,
		"template_expires_at_unix_ms": expiresAt.UnixMilli(),
	})
}

// handleRenewBlockTemplate extends the compact-submission lease for a template
// that is still live and still builds directly on the current canonical tip.
// POST /api/mining/renewtemplate
func (s *APIServer) handleRenewBlockTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TemplateID string `json:"template_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "template_id is required")
		return
	}

	expiresAt, err := s.renewMiningTemplate(templateID)
	if err != nil {
		switch {
		case errors.Is(err, errMiningTemplateUnavailable):
			writeError(w, http.StatusNotFound, "unknown or expired template_id")
		case errors.Is(err, errMiningTemplateStale):
			writeError(w, http.StatusConflict, "template no longer builds on current tip")
		default:
			writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"template_id":                 templateID,
		"template_expires_at_unix_ms": expiresAt.UnixMilli(),
	})
}

// handleSubmitBlock accepts a solved block from pool mining and adds it to the chain.
// POST /api/mining/submitblock
func (s *APIServer) handleSubmitBlock(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.submitBlockLimiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "submitblock rate limit exceeded")
		return
	}

	select {
	case s.submitBlockSem <- struct{}{}:
		defer func() { <-s.submitBlockSem }()
	default:
		writeError(w, http.StatusTooManyRequests, "submitblock busy, retry later")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var compact struct {
		TemplateID string  `json:"template_id"`
		Nonce      *uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(body, &compact); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var block Block
	if strings.TrimSpace(compact.TemplateID) != "" {
		if compact.Nonce == nil {
			writeError(w, http.StatusBadRequest, "nonce is required with template_id")
			return
		}
		tpl, ok := s.getMiningTemplate(strings.TrimSpace(compact.TemplateID))
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown or expired template_id")
			return
		}
		block = *tpl
		block.Header.Nonce = *compact.Nonce
	} else {
		if err := json.Unmarshal(body, &block); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	if err := s.daemon.SubmitBlock(&block); err != nil {
		if errors.Is(err, ErrStaleBlock) {
			writeError(w, http.StatusBadRequest, "block rejected as stale")
			return
		}
		writeInternal(w, r, http.StatusBadRequest, "block rejected", err)
		return
	}

	hash := block.Hash()
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
		"hash":     fmt.Sprintf("%x", hash),
		"height":   block.Header.Height,
	})
}

// handleMiningThreads sets the mining thread count.
// POST /api/mining/threads
func (s *APIServer) handleMiningThreads(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Threads int `json:"threads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Threads < 1 {
		writeError(w, http.StatusBadRequest, "threads must be >= 1")
		return
	}
	maxThreads := runtime.NumCPU()
	maxThreads = max(maxThreads, 1)
	if req.Threads > maxThreads {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("threads must be <= %d", maxThreads))
		return
	}

	s.daemon.Miner().SetThreads(req.Threads)
	writeJSON(w, http.StatusOK, map[string]any{"threads": s.daemon.Miner().Threads()})
}

// catchUpScan scans blocks from the wallet's synced height to the chain tip
// in the background. Called after wallet load/import so the HTTP response
// returns immediately. autoScanBlocks handles new blocks going forward.
func (s *APIServer) catchUpScan() {
	if s.cli == nil || s.cli.ctx == nil || s.daemon == nil {
		return
	}

	s.mu.RLock()
	w := s.wallet
	sc := s.scanner
	s.mu.RUnlock()
	if w == nil || sc == nil {
		return
	}

	ctx := s.cli.ctx

	walletHeight := w.SyncedHeight()
	chainHeight := s.daemon.Chain().Height()
	if walletHeight >= chainHeight {
		return
	}

	for h := walletHeight + 1; h <= chainHeight; h++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		block := s.daemon.Chain().GetBlockByHeight(h)
		if block == nil {
			break
		}
		applyBlockToWallet(s.daemon.Chain(), w, sc, block)
		w.ReconcileUnconfirmedSpends(func(txID [32]byte) bool {
			return s.daemon.Mempool().HasTransaction(txID)
		})

		if h%100 == 0 || h == chainHeight {
			if err := w.Save(); err != nil {
				log.Printf("Warning: catchUpScan save at height %d: %v", h, err)
			}
		}

		// Re-read chain height in case new blocks arrived during scan
		if h == chainHeight {
			chainHeight = s.daemon.Chain().Height()
		}
	}
}

// ============================================================================
// Dangerous operations
// ============================================================================

// handlePurgeData deletes all blockchain data from disk.
// POST /api/purge
func (s *APIServer) handlePurgeData(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "confirmation required (set confirm: true)")
		return
	}

	// Stop daemon first to release database locks
	if err := s.daemon.Stop(); err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	// Remove data directory
	if err := os.RemoveAll(s.dataDir); err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "blockchain data purged successfully, restart required",
	})

	// Shut down the API server since daemon is stopped
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Stop()
	}()
}

// ============================================================================
// Helpers
// ============================================================================

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("Warning: failed to write JSON response: %v", err)
	}
}

func encodeJSONResponse(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeCodedError adds a stable machine-readable code while preserving the
// existing human-readable error field for backward-compatible clients.
func writeCodedError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "error": msg})
}

// writeInternal logs err server-side and returns a generic client-facing error.
// The client message should not include internal details (paths/state/etc).
func writeInternal(w http.ResponseWriter, r *http.Request, status int, clientMsg string, err error) {
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	method := ""
	if r != nil {
		method = r.Method
	}
	log.Printf("API internal error: %s %s: %v", method, path, err)
	writeError(w, status, clientMsg)
}

func decodeCachedJSONBody(body []byte) (any, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func extractCachedError(body []byte) string {
	if len(body) == 0 {
		return "request failed"
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(body))
}

func walletLoadClientError(err error) (int, string, bool) {
	if err == nil {
		return 0, "", false
	}
	msg := err.Error()
	if strings.Contains(msg, "wrong password") || strings.Contains(msg, "message authentication failed") {
		return http.StatusUnauthorized, "wrong password", true
	}
	if strings.Contains(msg, "failed to read wallet file") {
		if errors.Is(err, os.ErrNotExist) {
			return http.StatusNotFound, "wallet file not found", true
		}
		if errors.Is(err, os.ErrPermission) {
			return http.StatusForbidden, "wallet file not readable (permission denied)", true
		}
	}
	if strings.Contains(msg, "failed to parse wallet data") {
		return http.StatusUnprocessableEntity, "wallet file is corrupted", true
	}
	if strings.Contains(msg, "ciphertext too short") {
		return http.StatusUnprocessableEntity, "wallet file is corrupted or truncated", true
	}
	return 0, "", false
}

func walletSendClientError(err error) (int, string, bool) {
	if err == nil {
		return 0, "", false
	}
	msg := err.Error()

	if errors.Is(err, wallet.ErrNoSpendableOutputs) ||
		errors.Is(err, wallet.ErrInsufficientFunds) ||
		errors.Is(err, wallet.ErrInputLimitExceeded) ||
		strings.Contains(msg, "insufficient funds") ||
		strings.Contains(msg, "no spendable outputs") ||
		strings.Contains(msg, "input limit exceeded") ||
		strings.Contains(msg, "balance too small to cover fee") ||
		strings.Contains(msg, "too many outputs to sweep") ||
		strings.Contains(msg, "no recipients specified") ||
		strings.Contains(msg, "no inputs specified") ||
		strings.Contains(msg, "overflows") {
		return http.StatusBadRequest, msg, true
	}

	if strings.Contains(msg, "key image already") ||
		strings.Contains(msg, "double-spend") {
		return http.StatusConflict, msg, true
	}

	if strings.Contains(msg, "mempool rejected") {
		return http.StatusUnprocessableEntity, msg, true
	}

	if strings.Contains(msg, "failed to select ring members") ||
		strings.Contains(msg, "not enough outputs for ring") ||
		strings.Contains(msg, "failed to sign") ||
		strings.Contains(msg, "failed to create range proof") ||
		strings.Contains(msg, "failed to derive") ||
		strings.Contains(msg, "failed to compute tx ID") ||
		strings.Contains(msg, "failed to sum output blindings") ||
		strings.Contains(msg, "failed to distribute blindings") ||
		strings.Contains(msg, "invalid transaction data") {
		return http.StatusInternalServerError, msg, true
	}

	return 0, "", false
}

func writeSendError(w http.ResponseWriter, r *http.Request, err error) {
	if status, msg, ok := walletSendClientError(err); ok {
		if status >= 500 {
			writeInternal(w, r, status, msg, err)
		} else {
			writeError(w, status, msg)
		}
		return
	}
	writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
}

// blockToJSON builds a JSON-friendly block representation.
func blockToJSON(block *Block, chainHeight uint64) map[string]any {
	hash := block.Hash()

	txs := make([]map[string]any, len(block.Transactions))
	for i, tx := range block.Transactions {
		txHash, _ := tx.TxID()
		txs[i] = map[string]any{
			"hash":        fmt.Sprintf("%x", txHash),
			"is_coinbase": i == 0 && block.Header.Height > 0,
			"inputs":      len(tx.Inputs),
			"outputs":     len(tx.Outputs),
			"fee":         tx.Fee,
		}
	}

	confirmations := uint64(0)
	if chainHeight >= block.Header.Height {
		confirmations = chainHeight - block.Header.Height + 1
	}

	return map[string]any{
		"height":        block.Header.Height,
		"hash":          fmt.Sprintf("%x", hash),
		"prev_hash":     fmt.Sprintf("%x", block.Header.PrevHash),
		"merkle_root":   fmt.Sprintf("%x", block.Header.MerkleRoot),
		"timestamp":     block.Header.Timestamp,
		"difficulty":    block.Header.Difficulty,
		"nonce":         block.Header.Nonce,
		"tx_count":      len(block.Transactions),
		"transactions":  txs,
		"confirmations": confirmations,
		"reward":        GetBlockReward(block.Header.Height),
	}
}

// findChainTx searches for a tx by hash string in the blockchain (tip backwards).
func (s *APIServer) findChainTx(hashStr string) (*Transaction, uint64, bool) {
	return s.daemon.Chain().FindTxByHashStr(hashStr)
}

type walletSendChainState struct {
	chainState    string
	confirmations uint64
	inMempool     bool
}

func (s *APIServer) walletSendChainStates(txIDs [][32]byte, chainHeight uint64) map[[32]byte]walletSendChainState {
	states := make(map[[32]byte]walletSendChainState, len(txIDs))
	pending := make(map[[32]byte]struct{}, len(txIDs))

	for _, txID := range txIDs {
		if s.daemon.Mempool().HasTransaction(txID) {
			states[txID] = walletSendChainState{chainState: "mempool", inMempool: true}
			continue
		}
		pending[txID] = struct{}{}
	}

	storage := s.daemon.Chain().Storage()
	if storage != nil && len(pending) > 0 {
	scan:
		for height := chainHeight; ; height-- {
			hash, found := storage.GetBlockHashByHeight(height)
			if !found {
				if height == 0 {
					break
				}
				continue
			}

			block, err := storage.GetBlock(hash)
			if err == nil && block != nil {
				for _, tx := range block.Transactions {
					txID, _ := tx.TxID()
					if _, ok := pending[txID]; !ok {
						continue
					}

					confirmations := uint64(0)
					if chainHeight >= height {
						confirmations = chainHeight - height + 1
					}
					states[txID] = walletSendChainState{
						chainState:    "confirmed",
						confirmations: confirmations,
					}
					delete(pending, txID)
					if len(pending) == 0 {
						break scan
					}
				}
			}

			if height == 0 {
				break
			}
		}
	}

	for txID := range pending {
		states[txID] = walletSendChainState{chainState: "not_found"}
	}
	return states
}

// createTxBuilder creates a transaction builder wired to the daemon (same as CLI).
func (s *APIServer) createTxBuilder() *wallet.Builder {
	cfg := wallet.TransferConfig{
		SelectRingMembers: func(realPubKey, realCommitment [32]byte) (keys, commitments [][32]byte, secretIndex int, err error) {
			ringData, err := s.daemon.Chain().SelectRingMembersWithCommitments(realPubKey, realCommitment)
			if err != nil {
				return nil, nil, 0, err
			}
			return ringData.Keys, ringData.Commitments, ringData.SecretIndex, nil
		},
		CreateCommitment: func(amount uint64, blinding [32]byte) [32]byte {
			commitment, _ := CreatePedersenCommitmentWithBlinding(amount, blinding)
			return commitment
		},
		CreateRangeProof: func(amount uint64, blinding [32]byte) ([]byte, error) {
			proof, err := CreateRangeProof(amount, blinding)
			if err != nil {
				return nil, err
			}
			return proof.Proof, nil
		},
		SignRingCT: func(ringKeys, ringCommitments [][32]byte, secretIndex int, privateKey, realBlinding, pseudoCommitment, pseudoBlinding [32]byte, message []byte) ([]byte, [32]byte, error) {
			sig, err := SignRingCT(ringKeys, ringCommitments, secretIndex, privateKey, realBlinding, pseudoCommitment, pseudoBlinding, message)
			if err != nil {
				return nil, [32]byte{}, err
			}
			return sig.Signature, sig.KeyImage, nil
		},
		GenerateBlinding: func() [32]byte {
			blinding, _ := GenerateBlinding()
			return blinding
		},
		ComputeTxID: func(txData []byte) ([32]byte, error) {
			tx, err := DeserializeTx(txData)
			if err != nil {
				return [32]byte{}, err
			}
			return tx.TxID()
		},
		DeriveStealthAddress: func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error) {
			output, err := DeriveStealthAddress(spendPub, viewPub)
			if err != nil {
				return txPriv, txPub, oneTimePub, err
			}
			return output.TxPrivKey, output.TxPubKey, output.OnetimePubKey, nil
		},
		DeriveStealthAddressWithKey: DeriveStealthAddressWithKey,
		DeriveDeterministicTxKey:    DeriveDeterministicTxKey,
		GenerateKeyImage:            GenerateKeyImage,
		DeriveSharedSecret:          DeriveStealthSecretSender,
		DeriveSharedSecretIndexed:   DeriveStealthSecretSenderIndexed,
		ScalarToPoint:               ScalarToPubKey,
		PointAdd: func(p1, p2 [32]byte) ([32]byte, error) {
			return CommitmentAdd(p1, p2)
		},
		BlindingAdd: BlindingAdd,
		BlindingSub: BlindingSub,
		RingSize:    RingSize,
		MinFee:      sendMinFee,
		FeePerByte:  sendFeePerByte,
	}

	return wallet.NewBuilder(s.wallet, cfg)
}

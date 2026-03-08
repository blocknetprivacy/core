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
)

// ============================================================================
// Public handlers (no wallet needed)
// ============================================================================

// handleStatus returns daemon stats.
func (s *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.daemon.Stats())
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

	spendPub, _, err := wallet.ParseAddress(req.Address)
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

// ============================================================================
// Wallet handlers (require loaded + unlocked wallet)
// ============================================================================

// handleBalance returns wallet balance breakdown.
// GET /api/wallet/balance
func (s *APIServer) handleBalance(w http.ResponseWriter, r *http.Request) {
	if !s.requireWallet(w, r) {
		return
	}

	height := s.daemon.Chain().Height()
	total, unspent := s.wallet.OutputCount()
	pendingUnconfirmed := s.wallet.PendingUnconfirmedBalance()

	// UX-only estimate: assume ~5 minute blocks and require next block + SafeConfirmations.
	// (This mirrors the CLI behavior.)
	etaSeconds := int64(0)
	if pendingUnconfirmed > 0 {
		eta := time.Duration(wallet.SafeConfirmations+1) * wallet.EstimatedBlockInterval
		etaSeconds = int64(eta.Seconds())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"spendable":                s.wallet.SpendableBalance(height),
		"pending":                  s.wallet.PendingBalance(height),
		"pending_unconfirmed":      pendingUnconfirmed,
		"pending_unconfirmed_eta":  etaSeconds,
		"total":                    s.wallet.Balance(),
		"outputs_total":            total,
		"outputs_unspent":          unspent,
		"chain_height":             height,
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

		spendPub, viewPub, err := wallet.ParseAddress(resolvedAddr)
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
		if status, msg, ok := walletSendClientError(err); ok {
			writeError(w, status, msg)
			return
		}
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	if err := s.daemon.SubmitTransaction(result.TxData); err != nil {
		s.wallet.ReleaseInputLease(result.InputLease)
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	for _, spent := range result.SpentOutputs {
		s.wallet.MarkSpent(spent.OneTimePubKey, height)
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
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	if err := s.daemon.SubmitTransaction(result.TxData); err != nil {
		s.wallet.ReleaseInputLease(result.InputLease)
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
		return
	}

	for _, spent := range result.SpentOutputs {
		s.wallet.MarkSpent(spent.OneTimePubKey, height)
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

// handleLoadWallet loads (or creates) a wallet at runtime.
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Password) < 3 {
		writeError(w, http.StatusBadRequest, "password must be at least 3 characters")
		return
	}

	password := []byte(req.Password)
	passHash := passwordHash(password)
	wl, err := wallet.LoadOrCreateWallet(s.cli.walletFile, password, defaultWalletConfig())
	wipeBytes(password)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
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

	// Catch up on blocks that arrived before the wallet was loaded
	if walletHeight < chainHeight {
		scannedTo := walletHeight
		for h := walletHeight + 1; h <= chainHeight; h++ {
			block := s.daemon.Chain().GetBlockByHeight(h)
			if block == nil {
				break
			}
			scanner.ScanBlock(blockToScanData(block))
			scannedTo = h
		}
		if scannedTo > walletHeight {
			wl.SetSyncedHeight(scannedTo)
			if err := wl.Save(); err != nil {
				writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
				return
			}
		}
	}

	wl.SetInputFilter(func(out *wallet.OwnedOutput) bool {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			return false
		}
		return s.daemon.Mempool().HasKeyImage(ki)
	})

	// Publish to API server
	s.mu.Lock()
	s.wallet = wl
	s.scanner = scanner
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

	writeJSON(w, http.StatusOK, map[string]any{
		"loaded":  true,
		"address": wl.Address(),
	})
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

	// Scan the entire chain to find outputs belonging to this seed
	chainHeight := s.daemon.Chain().Height()
	if chainHeight > 0 {
		scannedTo := uint64(0)
		for h := uint64(1); h <= chainHeight; h++ {
			block := s.daemon.Chain().GetBlockByHeight(h)
			if block == nil {
				break
			}
			scanner.ScanBlock(blockToScanData(block))
			scannedTo = h
		}
		if scannedTo > 0 {
			wl.SetSyncedHeight(scannedTo)
			if err := wl.Save(); err != nil {
				writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
				return
			}
		}
	}

	wl.SetInputFilter(func(out *wallet.OwnedOutput) bool {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			return false
		}
		return s.daemon.Mempool().HasKeyImage(ki)
	})

	// Publish to API server
	s.mu.Lock()
	s.wallet = wl
	s.scanner = scanner
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

	writeJSON(w, http.StatusOK, map[string]any{
		"imported": true,
		"address":  wl.Address(),
		"filename": filepath.Base(walletPath),
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

	blocks := s.daemon.Chain().GetBlocksByHeightRange(walletHeight+1, chainHeight)

	totalFound, totalSpent := 0, 0
	scannedTo := walletHeight
	for _, block := range blocks {
		if block == nil {
			break
		}
		blockData := blockToScanData(block)
		found, spent := s.scanner.ScanBlock(blockData)
		totalFound += found
		totalSpent += spent
		scannedTo = block.Header.Height
	}

	if scannedTo > walletHeight {
		s.wallet.SetSyncedHeight(scannedTo)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "synced",
		"synced_height": scannedTo,
		"chain_height":  chainHeight,
		"blocks_scanned": scannedTo - walletHeight,
		"outputs_found": totalFound,
		"outputs_spent": totalSpent,
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

	writeJSON(w, http.StatusOK, map[string]any{
		"spend_pub": hex.EncodeToString(keys.SpendPubKey[:]),
		"view_priv": hex.EncodeToString(keys.ViewPrivKey[:]),
		"view_pub":  hex.EncodeToString(keys.ViewPubKey[:]),
	})
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

	if s.daemon.syncMgr.IsSyncing() {
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
		spendPub, viewPub, err := wallet.ParseAddress(addr)
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
	templateID := s.rememberMiningTemplate(block)

	writeJSON(w, http.StatusOK, map[string]any{
		"block":               block,
		"target":              fmt.Sprintf("%x", target),
		"header_base":         fmt.Sprintf("%x", block.Header.SerializeForPoW()),
		"reward_address_used": rewardAddrUsed,
		"template_id":         templateID,
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

// ============================================================================
// Dangerous operations
// ============================================================================

// handlePurgeData deletes all blockchain data from disk.
// POST /api/purge
func (s *APIServer) handlePurgeData(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Confirm  bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Fail closed if destructive auth state is not initialized.
	if s.wallet == nil {
		writeError(w, http.StatusServiceUnavailable, "purge unavailable: no wallet loaded")
		return
	}
	s.mu.RLock()
	hashSet := s.passwordHashSet
	expectedHash := s.passwordHash
	s.mu.RUnlock()
	if !hashSet {
		writeError(w, http.StatusServiceUnavailable, "purge unavailable: password state not initialized")
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

	// Require password verification
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

	// Require explicit confirmation
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

func walletSendClientError(err error) (int, string, bool) {
	if err == nil {
		return 0, "", false
	}
	msg := err.Error()
	if errors.Is(err, wallet.ErrNoSpendableOutputs) ||
		errors.Is(err, wallet.ErrInsufficientFunds) ||
		strings.Contains(msg, "insufficient funds") ||
		strings.Contains(msg, "no spendable outputs") {
		return http.StatusBadRequest, msg, true
	}
	return 0, "", false
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

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"blocknet/wallet"
)

type bumpRing struct {
	keys        [][32]byte
	commitments [][32]byte
	secretIndex int
}

func (s *APIServer) handleSendBump(w http.ResponseWriter, r *http.Request) {
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
		cacheKey = "bump:" + idemKey
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
		case "interrupted":
			writeError(w, http.StatusConflict, idempotencyInterruptedMessage)
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

	if !s.sendLimiter.allow(clientIP(r)) {
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
		TxID            string          `json:"txid"`
		ExtraFeePerByte json.RawMessage `json:"extra_fee_per_byte"`
		DryRun          bool            `json:"dry_run"`
	}
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	feeRate, err := sendFeeRate(req.ExtraFeePerByte)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var oldID [32]byte
	if raw, err := hex.DecodeString(req.TxID); err != nil || len(raw) != 32 {
		writeError(w, http.StatusBadRequest, "txid must be 64 hex characters")
		return
	} else {
		copy(oldID[:], raw)
	}

	record := s.wallet.GetSendRecord(oldID)
	if record == nil {
		writeError(w, http.StatusNotFound, "this wallet has no send with that txid")
		return
	}
	if record.ReplacedBy != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("this send was already sped up; speed up its newest txid %x instead", *record.ReplacedBy))
		return
	}
	oldTx, oldFee, oldSize, ok := s.daemon.Mempool().GetEntry(oldID)
	if !ok || oldSize == 0 {
		writeError(w, http.StatusConflict, "this send is not in the mempool, so it cannot be sped up. It may have confirmed or dropped out")
		return
	}

	recipientReqs := make([]recipientRequest, 0, len(record.GetRecipients()))
	for _, rr := range record.GetRecipients() {
		addr := rr.ResolvedAddress
		if addr == "" {
			if isHandleRecipient(rr.Address) {
				writeError(w, http.StatusConflict, "this send went to a handle and has no saved address, so it cannot be sped up")
				return
			}
			addr = rr.Address
		}
		rq := recipientRequest{Address: addr, Amount: rr.Amount}
		if len(rr.Memo) > 0 {
			rq.MemoHex = hex.EncodeToString(rr.Memo)
		}
		recipientReqs = append(recipientReqs, rq)
	}
	validated, totalSend, err := s.validateRecipients(recipientReqs)
	if err != nil {
		writeError(w, http.StatusConflict, "the recipients of this send cannot be rebuilt: "+err.Error())
		return
	}

	oldInputs := s.wallet.UnconfirmedSpendsOf(oldID)
	byImage := make(map[[32]byte]*wallet.OwnedOutput, len(oldInputs))
	for _, out := range oldInputs {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			writeInternal(w, r, http.StatusInternalServerError, "internal error", err)
			return
		}
		byImage[ki] = out
	}
	rings := make(map[[32]byte]bumpRing, len(oldTx.Inputs))
	inputs := make([]*wallet.OwnedOutput, 0, len(oldTx.Inputs))
	for _, in := range oldTx.Inputs {
		out, ok := byImage[in.KeyImage]
		if !ok {
			writeError(w, http.StatusConflict, "the wallet cannot find the coins this send spent, so it cannot be sped up")
			return
		}
		idx := -1
		for i, key := range in.RingMembers {
			if key == out.OneTimePubKey {
				idx = i
				break
			}
		}
		if idx < 0 || len(in.RingCommitments) != len(in.RingMembers) {
			writeError(w, http.StatusConflict, "the ring of this send does not hold its own coin, so it cannot be sped up")
			return
		}
		rings[out.OneTimePubKey] = bumpRing{
			keys:        append([][32]byte(nil), in.RingMembers...),
			commitments: append([][32]byte(nil), in.RingCommitments...),
			secretIndex: idx,
		}
		inputs = append(inputs, out)
	}

	step := s.daemon.Mempool().ReplaceRateStep()
	if minRate := (oldFee+uint64(oldSize)-1)/uint64(oldSize) + step; feeRate < minRate {
		feeRate = minRate
	}
	minFee := max(sendMinFee, oldFee+step*uint64(oldSize))
	height := s.daemon.Chain().Height()

	var oldTotal uint64
	for _, in := range inputs {
		oldTotal += in.Amount
	}
	var lease uint64
	releaseLease := true
	defer func() {
		if releaseLease && lease != 0 {
			s.wallet.ReleaseInputLease(lease)
		}
	}()
	var extra []*wallet.OwnedOutput
	fee := minFee
	for i := 0; ; i++ {
		est := wallet.EstimateTxSizeBytes(len(inputs)+len(extra), len(validated)+1, RingSize)
		fee = max(minFee, uint64(est)*feeRate)
		var extraTotal uint64
		for _, e := range extra {
			extraTotal += e.Amount
		}
		need, ok := wallet.AddU64(totalSend, fee)
		if !ok {
			writeError(w, http.StatusBadRequest, "send amount + fee overflows")
			return
		}
		if oldTotal+extraTotal >= need {
			break
		}
		if i >= 16 {
			msg, _ := s.sendFundsError(wallet.ErrInsufficientFunds, height, totalSend, fee, feeRate)
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if lease != 0 {
			s.wallet.ReleaseInputLease(lease)
			lease = 0
		}
		newLease, more, err := s.wallet.ReserveMatureInputs(height, need-oldTotal, 2*time.Minute)
		if err != nil {
			msg, ok := s.sendFundsError(err, height, totalSend, fee, feeRate)
			if !ok {
				msg = "insufficient funds: " + err.Error()
			}
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		lease, extra = newLease, more
	}

	builder := s.createTxBuilderWith(func(cfg *wallet.TransferConfig) {
		cfg.MinFee = minFee
		chooseRing := cfg.SelectRingMembers
		cfg.SelectRingMembers = func(realPubKey, realCommitment [32]byte) ([][32]byte, [][32]byte, int, error) {
			if ring, ok := rings[realPubKey]; ok {
				return ring.keys, ring.commitments, ring.secretIndex, nil
			}
			return chooseRing(realPubKey, realCommitment)
		}
	})
	all := append(append([]*wallet.OwnedOutput(nil), inputs...), extra...)
	releaseLease = false
	res, err := builder.TransferWithInputs(all, lease, toWalletRecipients(validated), feeRate, 1, height)
	if err != nil {
		if msg, ok := s.sendFundsError(err, height, totalSend, 0, feeRate); ok {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		writeSendError(w, r, err)
		return
	}

	resp := map[string]any{
		"old_txid":     req.TxID,
		"fee":          res.Fee,
		"fee_per_byte": feeRate,
		"old_fee":      oldFee,
		"bump_cost":    res.Fee - min(res.Fee, oldFee),
		"change":       res.Change,
		"input_count":  len(all),
		"dry_run":      req.DryRun,
	}
	if req.DryRun {
		s.wallet.ReleaseInputLease(res.InputLease)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if err := s.daemon.SubmitTransaction(res.TxData); err != nil {
		s.wallet.ReleaseInputLease(res.InputLease)
		writeSendError(w, r, err)
		return
	}
	spent := make([][32]byte, len(all))
	for i, in := range all {
		spent[i] = in.OneTimePubKey
	}
	s.wallet.ReplaceSend(oldID, res.TxID, res.Fee, spent, res.Change, res.TxData)
	if err := s.wallet.Save(); err != nil {
		log.Printf("Warning: wallet persistence failed after bump %x: %v", res.TxID, err)
	}
	resp["txid"] = fmt.Sprintf("%x", res.TxID)
	writeJSON(w, http.StatusOK, resp)
}

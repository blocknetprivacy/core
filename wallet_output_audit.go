package main

import (
	"encoding/hex"
	"log"

	"blocknet/wallet"
)

const (
	walletOutputIssueMissingBlock       = "missing_canonical_block"
	walletOutputIssueMissingTx          = "tx_not_in_canonical_block"
	walletOutputIssueIndexOutOfRange    = "output_index_out_of_range"
	walletOutputIssuePublicKeyMismatch  = "output_public_key_mismatch"
	walletOutputIssueCommitmentMismatch = "output_commitment_mismatch"
	walletOutputIssueNegativeIndex      = "negative_output_index"
)

type walletCanonicalOutputIssue struct {
	Ref                wallet.OutputRef `json:"-"`
	TxID               string           `json:"txid"`
	OutputIndex        int              `json:"output_index"`
	Amount             uint64           `json:"amount"`
	Spent              bool             `json:"spent"`
	BlockHeight        uint64           `json:"block_height"`
	Reason             string           `json:"reason"`
	CanonicalBlockHash string           `json:"canonical_block_hash,omitempty"`
	OneTimePub         string           `json:"one_time_pub"`
	Commitment         string           `json:"commitment"`
}

func auditWalletCanonicalOutputs(chain *Chain, outputs []*wallet.OwnedOutput) []walletCanonicalOutputIssue {
	if chain == nil || len(outputs) == 0 {
		return nil
	}

	// Probe all outputs through the storage indexes in one pass. Wallets
	// holding ~100k outputs took over a minute here with one full block
	// decode per output, which held up wallet load (and so pool templates)
	// after every daemon restart. The per-output scan remains the fallback.
	audited := make([]*wallet.OwnedOutput, 0, len(outputs))
	probes := make([]canonicalOutputProbe, 0, len(outputs))
	for _, out := range outputs {
		if out == nil || out.OutputIndex < 0 {
			continue
		}
		audited = append(audited, out)
		probes = append(probes, canonicalOutputProbe{TxID: out.TxID, Height: out.BlockHeight, Index: out.OutputIndex})
	}
	var results []canonicalOutputProbeResult
	if storage := chain.Storage(); storage != nil && len(probes) > 0 {
		var err error
		if results, err = storage.ProbeCanonicalOutputs(probes); err != nil {
			log.Printf("Warning: indexed wallet output audit failed, scanning blocks instead: %v", err)
			results = nil
		}
	}

	issues := make([]walletCanonicalOutputIssue, 0)
	next := 0
	for _, out := range outputs {
		if out == nil {
			continue
		}
		if results != nil && out.OutputIndex >= 0 {
			probe := results[next]
			next++
			// The index only clears outputs it finds exactly as recorded;
			// anything else is judged by the original scan, so every issue
			// reported is the one auditWalletCanonicalOutput would give.
			if walletOutputMatchesProbe(out, probe) {
				continue
			}
		}
		if issue, ok := auditWalletCanonicalOutput(chain, out); ok {
			issues = append(issues, issue)
		}
	}
	return issues
}

// walletOutputMatchesProbe reports whether the canonical block at the
// output's height holds its transaction with this exact output.
func walletOutputMatchesProbe(out *wallet.OwnedOutput, probe canonicalOutputProbeResult) bool {
	return probe.BlockFound && probe.TxFound &&
		out.OutputIndex < probe.OutputCount &&
		probe.PublicKey == out.OneTimePubKey &&
		probe.Commitment == out.Commitment
}

func newWalletCanonicalOutputIssue(out *wallet.OwnedOutput) walletCanonicalOutputIssue {
	return walletCanonicalOutputIssue{
		Ref:         wallet.OutputRef{TxID: out.TxID, OutputIndex: out.OutputIndex},
		TxID:        hex.EncodeToString(out.TxID[:]),
		OutputIndex: out.OutputIndex,
		Amount:      out.Amount,
		Spent:       out.Spent,
		BlockHeight: out.BlockHeight,
		OneTimePub:  hex.EncodeToString(out.OneTimePubKey[:]),
		Commitment:  hex.EncodeToString(out.Commitment[:]),
	}
}

func auditWalletCanonicalOutput(chain *Chain, out *wallet.OwnedOutput) (walletCanonicalOutputIssue, bool) {
	base := newWalletCanonicalOutputIssue(out)

	if out.OutputIndex < 0 {
		base.Reason = walletOutputIssueNegativeIndex
		return base, true
	}

	block := chain.GetBlockByHeight(out.BlockHeight)
	if block == nil {
		base.Reason = walletOutputIssueMissingBlock
		return base, true
	}
	blockHash := block.Hash()
	base.CanonicalBlockHash = hex.EncodeToString(blockHash[:])

	for i := range block.Transactions {
		tx := block.Transactions[i]
		if tx == nil {
			continue
		}
		txID, err := tx.TxID()
		if err != nil || txID != out.TxID {
			continue
		}
		if out.OutputIndex >= len(tx.Outputs) {
			base.Reason = walletOutputIssueIndexOutOfRange
			return base, true
		}
		txOut := tx.Outputs[out.OutputIndex]
		if txOut.PublicKey != out.OneTimePubKey {
			base.Reason = walletOutputIssuePublicKeyMismatch
			return base, true
		}
		if txOut.Commitment != out.Commitment {
			base.Reason = walletOutputIssueCommitmentMismatch
			return base, true
		}
		return walletCanonicalOutputIssue{}, false
	}

	base.Reason = walletOutputIssueMissingTx
	return base, true
}

func repairableWalletOutputIssues(issues []walletCanonicalOutputIssue, includeMissingBlocks bool) []walletCanonicalOutputIssue {
	if len(issues) == 0 {
		return nil
	}

	repairable := make([]walletCanonicalOutputIssue, 0, len(issues))
	for _, issue := range issues {
		if issue.Reason == walletOutputIssueMissingBlock && !includeMissingBlocks {
			continue
		}
		repairable = append(repairable, issue)
	}
	return repairable
}

func removeWalletOutputIssues(w *wallet.Wallet, issues []walletCanonicalOutputIssue) []*wallet.OwnedOutput {
	if w == nil || len(issues) == 0 {
		return nil
	}

	refs := make([]wallet.OutputRef, len(issues))
	for i, issue := range issues {
		refs[i] = issue.Ref
	}
	return w.RemoveOutputs(refs)
}

func sumCanonicalOutputIssues(issues []walletCanonicalOutputIssue) uint64 {
	var total uint64
	for _, issue := range issues {
		total += issue.Amount
	}
	return total
}

func sumOwnedOutputs(outputs []*wallet.OwnedOutput) uint64 {
	var total uint64
	for _, out := range outputs {
		if out != nil {
			total += out.Amount
		}
	}
	return total
}

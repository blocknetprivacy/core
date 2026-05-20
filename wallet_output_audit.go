package main

import (
	"encoding/hex"

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

	issues := make([]walletCanonicalOutputIssue, 0)
	for _, out := range outputs {
		if out == nil {
			continue
		}
		if issue, ok := auditWalletCanonicalOutput(chain, out); ok {
			issues = append(issues, issue)
		}
	}
	return issues
}

func auditWalletCanonicalOutput(chain *Chain, out *wallet.OwnedOutput) (walletCanonicalOutputIssue, bool) {
	base := walletCanonicalOutputIssue{
		Ref:         wallet.OutputRef{TxID: out.TxID, OutputIndex: out.OutputIndex},
		TxID:        hex.EncodeToString(out.TxID[:]),
		OutputIndex: out.OutputIndex,
		Amount:      out.Amount,
		Spent:       out.Spent,
		BlockHeight: out.BlockHeight,
		OneTimePub:  hex.EncodeToString(out.OneTimePubKey[:]),
		Commitment:  hex.EncodeToString(out.Commitment[:]),
	}

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

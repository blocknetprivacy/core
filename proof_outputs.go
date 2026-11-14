package main

import "blocknet/wallet"

// decodedProofOutput keeps amounts in atomic units until the HTTP response is
// formatted. Both the API and explorer use the same matching/decryption path.
type decodedProofOutput struct {
	Index  int
	Match  bool
	Amount uint64
	Memo   string
}

// decodeProofOutputs identifies outputs from an ECDH scalar/point pair. For a
// sender proof these are txPriv/viewPub; for a receiver proof viewPriv/txPub.
// Callers verifying a sender proof must first check txPriv*G == tx.TxPublicKey.
func decodeProofOutputs(tx *Transaction, scalar, point, spendPub [32]byte, isCoinbase bool, blockHeight uint64) []decodedProofOutput {
	outputs := make([]decodedProofOutput, len(tx.Outputs))
	pubs := make([][32]byte, len(tx.Outputs))
	indices := make([]uint32, len(tx.Outputs))
	for i, out := range tx.Outputs {
		outputs[i].Index = i
		pubs[i] = out.PublicKey
		indices[i] = uint32(i)
	}

	// Proofs detect the derivation cryptographically, not from the inclusion
	// height. A legacy transaction can be mined after activation, and pending
	// transactions have no block height. Trying indexed first, then legacy,
	// preserves old proofs while also recognizing current transactions. This
	// does not change transaction validation or the wallet scanner's policy.
	const (
		legacyOnly       uint32 = 0
		indexedAndLegacy uint32 = 2
	)
	mode := indexedAndLegacy
	if isCoinbase {
		mode = legacyOnly
	}
	matched, secrets, err := StealthScanBatch(point, scalar, spendPub, pubs, indices, mode)
	if err != nil {
		// Invalid key material must never reach amount/memo decryption with a
		// zero or otherwise uninitialized shared secret.
		return outputs
	}
	for i, out := range tx.Outputs {
		if !matched[i] {
			continue
		}
		outputs[i].Match = true
		var blinding [32]byte
		if isCoinbase {
			blinding = wallet.DeriveCoinbaseConsensusBlinding(tx.TxPublicKey, blockHeight, i)
		} else {
			// Reuse the secret of the derivation that actually matched. Using a
			// legacy secret here after an indexed match would corrupt the amount
			// and prevent memo decryption.
			blinding = wallet.DeriveBlinding(secrets[i], i)
			if memo, ok := wallet.DecryptMemo(out.EncryptedMemo, secrets[i], i); ok {
				outputs[i].Memo = string(memo)
			}
		}
		outputs[i].Amount = wallet.DecryptAmount(out.EncryptedAmount, blinding, i)
	}
	return outputs
}

func explorerProofResults(outputs []decodedProofOutput) []proveResult {
	// Preserve the explorer's existing null response for a transaction with no
	// outputs, as well as its BNT (rather than atomic-unit) amount convention.
	var results []proveResult
	for _, out := range outputs {
		results = append(results, proveResult{
			Index: out.Index, Match: out.Match,
			Amount: float64(out.Amount) / 100_000_000, Memo: out.Memo,
		})
	}
	return results
}

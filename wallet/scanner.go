package wallet

import (
	"crypto/sha3"
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// BlockData is the minimal block info needed for scanning
type BlockData struct {
	Height       uint64
	Transactions []TxData
}

// TxData is the minimal tx info needed for scanning
type TxData struct {
	TxID       [32]byte
	TxPubKey   [32]byte
	IsCoinbase bool // True if this is a coinbase (mining reward) transaction
	Outputs    []OutputData
	KeyImages  [][32]byte // For detecting spent outputs
}

// OutputData is the minimal output info for scanning
type OutputData struct {
	Index           int
	PubKey          [32]byte
	Commitment      [32]byte
	EncryptedAmount [8]byte
	EncryptedMemo   [MemoSize]byte
}

// ScannerConfig holds callbacks for cryptographic operations
type ScannerConfig struct {
	GenerateKeyImage func(privKey [32]byte) ([32]byte, error)

	// CreateCommitment recomputes a Pedersen commitment for (amount, blinding).
	// If non-nil, the scanner verifies the decrypted amount matches the on-chain
	// commitment before recording the output. This prevents garbage balances when
	// amount decryption produces wrong results (e.g. legacy broken transactions).
	CreateCommitment func(amount uint64, blinding [32]byte) ([32]byte, error)

	// Primitives used to compose stealth derivation in the scanner.
	ScalarToPoint func(scalar [32]byte) ([32]byte, error)
	PointAdd      func(p1, p2 [32]byte) ([32]byte, error)
	BlindingAdd   func(a, b [32]byte) ([32]byte, error)
}

// Scanner scans blocks for wallet-relevant transactions
type Scanner struct {
	wallet *Wallet
	config ScannerConfig
}

// NewScanner creates a scanner for a wallet
func NewScanner(w *Wallet, cfg ScannerConfig) *Scanner {
	return &Scanner{
		wallet: w,
		config: cfg,
	}
}

// ScanBlock scans a block for owned outputs and spent outputs.
// It tries indexed derivation first, then falls back to non-indexed.
func (s *Scanner) ScanBlock(block *BlockData) (found int, spent int) {
	keys := s.wallet.Keys()
	spendableByKeyImage := s.buildSpendableKeyImageIndex()

	canCompose := s.config.ScalarToPoint != nil && s.config.PointAdd != nil && s.config.BlindingAdd != nil

	for _, tx := range block.Transactions {
		for _, out := range tx.Outputs {
			var secret [32]byte
			var matched bool

			if canCompose {
				// Try indexed derivation first.
				if s.wallet.deriveOutputSecretIndexed != nil {
					indexed, err := s.wallet.deriveOutputSecretIndexed(tx.TxPubKey, keys.ViewPrivKey, uint32(out.Index))
					if err == nil {
						point, err2 := s.config.ScalarToPoint(indexed)
						if err2 == nil {
							expected, err3 := s.config.PointAdd(point, keys.SpendPubKey)
							if err3 == nil && expected == out.PubKey {
								secret = indexed
								matched = true
							}
						}
					}
				}

				// Fall back to non-indexed.
				if !matched {
					legacy, err := s.wallet.deriveOutputSecret(tx.TxPubKey, keys.ViewPrivKey)
					if err == nil {
						point, err2 := s.config.ScalarToPoint(legacy)
						if err2 == nil {
							expected, err3 := s.config.PointAdd(point, keys.SpendPubKey)
							if err3 == nil && expected == out.PubKey {
								secret = legacy
								matched = true
							}
						}
					}
				}
			} else {
				// Legacy path for callers that haven't wired the primitives.
				if s.wallet.checkStealthOutput(tx.TxPubKey, out.PubKey, keys.ViewPrivKey, keys.SpendPubKey) {
					sec, err := s.wallet.deriveOutputSecret(tx.TxPubKey, keys.ViewPrivKey)
					if err == nil {
						secret = sec
						matched = true
					}
				}
			}

			if !matched {
				continue
			}

			// Derive one-time private key: secret + spendPriv (scalar add).
			var oneTimePriv [32]byte
			if canCompose {
				otp, err := s.config.BlindingAdd(secret, keys.SpendPrivKey)
				if err != nil {
					continue
				}
				oneTimePriv = otp
			} else {
				otp, err := s.wallet.deriveSpendKey(tx.TxPubKey, keys.ViewPrivKey, keys.SpendPrivKey)
				if err != nil {
					continue
				}
				oneTimePriv = otp
			}

			var outputSecret [32]byte
			var blinding [32]byte
			if tx.IsCoinbase {
				blinding = DeriveCoinbaseConsensusBlinding(tx.TxPubKey, block.Height, out.Index)
			} else {
				outputSecret = secret
				blinding = DeriveBlinding(outputSecret, out.Index)
			}

			amount := DecryptAmount(out.EncryptedAmount, blinding, out.Index)

			if s.config.CreateCommitment != nil {
				commitment, err := s.config.CreateCommitment(amount, blinding)
				if err != nil {
					continue
				}
				if commitment != out.Commitment {
					continue
				}
			}

			owned := &OwnedOutput{
				TxID:           tx.TxID,
				OutputIndex:    out.Index,
				Amount:         amount,
				Blinding:       blinding,
				OneTimePrivKey: oneTimePriv,
				OneTimePubKey:  out.PubKey,
				Commitment:     out.Commitment,
				BlockHeight:    block.Height,
				IsCoinbase:     tx.IsCoinbase,
				Spent:          false,
			}

			if !tx.IsCoinbase {
				if memo, ok := DecryptMemo(out.EncryptedMemo, outputSecret, out.Index); ok {
					owned.Memo = memo
				} else {
					s.wallet.recordMemoDecryptFailure(block.Height)
				}
			}

			s.wallet.AddOutput(owned)
			if keyImage, err := s.config.GenerateKeyImage(owned.OneTimePrivKey); err == nil {
				spendableByKeyImage[keyImage] = append(spendableByKeyImage[keyImage], owned.OneTimePubKey)
			}
			found++
		}

		// Check key images - did we spend something?
		for _, keyImage := range tx.KeyImages {
			ownedPubKeys := spendableByKeyImage[keyImage]
			for _, ownedPubKey := range ownedPubKeys {
				if s.wallet.MarkSpent(ownedPubKey, block.Height) {
					spent++
				}
			}
			delete(spendableByKeyImage, keyImage)
		}
	}

	return found, spent
}

func (s *Scanner) buildSpendableKeyImageIndex() map[[32]byte][][32]byte {
	spendableByKeyImage := make(map[[32]byte][][32]byte)
	for _, out := range s.wallet.SpendableOutputs() {
		keyImage, err := s.config.GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			continue
		}
		spendableByKeyImage[keyImage] = append(spendableByKeyImage[keyImage], out.OneTimePubKey)
	}
	return spendableByKeyImage
}

// ScanBlocks scans multiple blocks
func (s *Scanner) ScanBlocks(blocks []*BlockData) (totalFound, totalSpent int) {
	for _, block := range blocks {
		found, spent := s.ScanBlock(block)
		totalFound += found
		totalSpent += spent

		s.wallet.SetSyncedHeight(block.Height)
	}
	return totalFound, totalSpent
}

// DeriveBlinding derives a blinding factor from the shared secret and output index.
// blinding = Hash("blocknet_blinding" || shared_secret || output_index)
func DeriveBlinding(sharedSecret [32]byte, outputIndex int) [32]byte {
	var outputIndexBytes [4]byte
	binary.LittleEndian.PutUint32(outputIndexBytes[:], uint32(outputIndex))
	const tag = "blocknet_blinding"
	b := make([]byte, 0, len(tag)+len(sharedSecret)+len(outputIndexBytes))
	b = append(b, tag...)
	b = append(b, sharedSecret[:]...)
	b = append(b, outputIndexBytes[:]...)
	blinding := sha3.Sum256(b)

	// Reduce modulo the curve order to ensure it's a valid scalar
	// For Ristretto255, scalars are mod 2^252 + 27742317777372353535851937790883648493
	// The hash output is already 32 bytes, which is fine for this purpose
	// as the Rust side handles canonical reduction
	return blinding
}

// DeriveCoinbaseConsensusBlinding derives the deterministic consensus blinding
// for coinbase outputs from public transaction data.
func DeriveCoinbaseConsensusBlinding(txPubKey [32]byte, blockHeight uint64, outputIndex int) [32]byte {
	var blockHeightBytes [8]byte
	binary.LittleEndian.PutUint64(blockHeightBytes[:], blockHeight)
	var outputIndexBytes [4]byte
	binary.LittleEndian.PutUint32(outputIndexBytes[:], uint32(outputIndex))
	const tag = "blocknet_coinbase_consensus_blinding"
	b := make([]byte, 0, len(tag)+len(txPubKey)+len(blockHeightBytes)+len(outputIndexBytes))
	b = append(b, tag...)
	b = append(b, txPubKey[:]...)
	b = append(b, blockHeightBytes[:]...)
	b = append(b, outputIndexBytes[:]...)
	return sha3.Sum256(b)
}

// BlockToScanData converts a serialized block to scanner format
func BlockToScanData(blockJSON []byte) (*BlockData, error) {
	var raw struct {
		Header struct {
			Height uint64 `json:"height"`
		} `json:"header"`
		Transactions []struct {
			TxID        [32]byte `json:"tx_id"`
			TxPublicKey [32]byte `json:"tx_public_key"`
			Outputs     []struct {
				PublicKey       [32]byte `json:"public_key"`
				Commitment      [32]byte `json:"commitment"`
				EncryptedAmount [8]byte  `json:"encrypted_amount"`
				EncryptedMemo   [MemoSize]byte `json:"encrypted_memo"`
			} `json:"outputs"`
			Inputs []struct {
				KeyImage [32]byte `json:"key_image"`
			} `json:"inputs"`
		} `json:"transactions"`
	}

	if err := json.Unmarshal(blockJSON, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse block: %w", err)
	}

	block := &BlockData{
		Height:       raw.Header.Height,
		Transactions: make([]TxData, len(raw.Transactions)),
	}

	for i, tx := range raw.Transactions {
		block.Transactions[i] = TxData{
			TxID:     tx.TxID,
			TxPubKey: tx.TxPublicKey,
			Outputs:  make([]OutputData, len(tx.Outputs)),
		}

		for j, out := range tx.Outputs {
			block.Transactions[i].Outputs[j] = OutputData{
				Index:           j,
				PubKey:          out.PublicKey,
				Commitment:      out.Commitment,
				EncryptedAmount: out.EncryptedAmount,
				EncryptedMemo:   out.EncryptedMemo,
			}
		}

		for _, inp := range tx.Inputs {
			block.Transactions[i].KeyImages = append(block.Transactions[i].KeyImages, inp.KeyImage)
		}
	}

	return block, nil
}

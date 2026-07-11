package wallet

import (
	"crypto/sha3"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"blocknet/protocol/params"
)

// derivationBoundaryWindow is the number of blocks above IndexedDerivationHeight
// within which the scanner attempts both the indexed and legacy stealth
// derivations. A transaction built just before the activation height can be
// mined just after it, so near the boundary its outputs may use either
// derivation. Outside this window the derivation is unambiguous from the block
// height (and coinbase outputs are always legacy), so only one is attempted.
const derivationBoundaryWindow = 128

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

	// ScanOutputsBatch, if set, matches all of a transaction's outputs against
	// the wallet keys in a single call — computing the ECDH shared point once
	// per transaction instead of once per output. mode is one of scanMode*.
	// It returns, per output, whether it is owned and the shared secret for
	// owned outputs. When nil the scanner falls back to the per-output
	// composed/legacy path. This is the hot-path fast lane for sync scanning.
	ScanOutputsBatch func(txPubKey, viewPriv, spendPub [32]byte, outPubKeys [][32]byte, outIndices []uint32, mode uint32) (matched []bool, secrets [][32]byte, err error)
}

// Stealth-derivation modes for ScanOutputsBatch.
const (
	scanModeLegacy  uint32 = 0 // H(shared) — coinbase, pre-activation outputs
	scanModeIndexed uint32 = 1 // H(shared || index) — post-activation outputs
	scanModeBoth    uint32 = 2 // indexed first, then legacy — boundary window
)

// outputMatch records an owned output found during the (read-only) match phase:
// its position in the transaction's Outputs slice and the recovered shared
// secret, consumed later by the (stateful, ordered) apply phase.
type outputMatch struct {
	pos    int
	secret [32]byte
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
func (s *Scanner) ScanBlock(block *BlockData) (found int, spent int) {
	spendableByKeyImage := s.buildSpendableKeyImageIndex()
	return s.scanBlockWithIndex(block, spendableByKeyImage)
}

// scanBlockWithIndex scans a block using a caller-supplied spendable-key-image
// index so a batch sync can build the index once instead of per block. The
// index is mutated in place: newly found outputs are added and confirmed spends
// are removed, so it stays correct across consecutive blocks.
func (s *Scanner) scanBlockWithIndex(block *BlockData, spendableByKeyImage map[[32]byte][][32]byte) (found int, spent int) {
	keys := s.wallet.Keys()
	for i := range block.Transactions {
		tx := &block.Transactions[i]
		matches := s.matchTxOutputs(tx, block.Height, keys)
		f, sp := s.applyTxMatches(tx, block.Height, keys, matches, spendableByKeyImage)
		found += f
		spent += sp
	}
	return found, spent
}

// txDerivationMode returns the ScanOutputsBatch mode for a transaction: which
// stealth derivation(s) its outputs may use. Coinbase outputs always use the
// non-indexed (legacy) derivation. Regular outputs use indexed derivation
// at/after the activation height and legacy before it; near the boundary a tx
// built just before the switch can be mined just after, so both are tried.
func txDerivationMode(isCoinbase bool, height uint64) uint32 {
	if isCoinbase || height < params.IndexedDerivationHeight {
		return scanModeLegacy
	}
	if height < params.IndexedDerivationHeight+derivationBoundaryWindow {
		return scanModeBoth
	}
	return scanModeIndexed
}

// matchTxOutputs finds which of a transaction's outputs are owned by the wallet.
// It is read-only with respect to wallet state (it only reads the immutable key
// material passed in), so it is safe to run concurrently across transactions.
func (s *Scanner) matchTxOutputs(tx *TxData, height uint64, keys StealthKeys) []outputMatch {
	if len(tx.Outputs) == 0 {
		return nil
	}

	mode := txDerivationMode(tx.IsCoinbase, height)
	tryIndexed := mode == scanModeIndexed || mode == scanModeBoth
	tryLegacy := mode == scanModeLegacy || mode == scanModeBoth

	// Fast path: one FFI call per transaction, shared point computed once.
	if s.config.ScanOutputsBatch != nil {
		outPubs := make([][32]byte, len(tx.Outputs))
		outIdx := make([]uint32, len(tx.Outputs))
		for j, out := range tx.Outputs {
			outPubs[j] = out.PubKey
			outIdx[j] = uint32(out.Index)
		}
		matched, secrets, err := s.config.ScanOutputsBatch(tx.TxPubKey, keys.ViewPrivKey, keys.SpendPubKey, outPubs, outIdx, mode)
		if err == nil {
			var ms []outputMatch
			for j := range tx.Outputs {
				if j < len(matched) && matched[j] {
					ms = append(ms, outputMatch{pos: j, secret: secrets[j]})
				}
			}
			return ms
		}
		// On error, fall through to the per-output path.
	}

	canCompose := s.config.ScalarToPoint != nil && s.config.PointAdd != nil && s.config.BlindingAdd != nil

	var ms []outputMatch
	for j, out := range tx.Outputs {
		var secret [32]byte
		var matched bool

		if canCompose {
			if tryIndexed && s.wallet.deriveOutputSecretIndexed != nil {
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

			if !matched && tryLegacy {
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
		} else if s.wallet.checkStealthOutput != nil {
			// Legacy path for callers that haven't wired the primitives.
			if s.wallet.checkStealthOutput(tx.TxPubKey, out.PubKey, keys.ViewPrivKey, keys.SpendPubKey) {
				sec, err := s.wallet.deriveOutputSecret(tx.TxPubKey, keys.ViewPrivKey)
				if err == nil {
					secret = sec
					matched = true
				}
			}
		}

		if matched {
			ms = append(ms, outputMatch{pos: j, secret: secret})
		}
	}
	return ms
}

// applyTxMatches records the wallet outputs found by matchTxOutputs and checks
// the transaction's key images for spends. It mutates wallet state and the
// key-image index, so it must run serially in block/transaction order.
func (s *Scanner) applyTxMatches(tx *TxData, height uint64, keys StealthKeys, matches []outputMatch, spendableByKeyImage map[[32]byte][][32]byte) (found int, spent int) {
	canCompose := s.config.BlindingAdd != nil

	for _, m := range matches {
		out := tx.Outputs[m.pos]
		secret := m.secret

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
			blinding = DeriveCoinbaseConsensusBlinding(tx.TxPubKey, height, out.Index)
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
			BlockHeight:    height,
			IsCoinbase:     tx.IsCoinbase,
			Spent:          false,
		}

		if !tx.IsCoinbase {
			if memo, ok := DecryptMemo(out.EncryptedMemo, outputSecret, out.Index); ok {
				owned.Memo = memo
			} else {
				s.wallet.recordMemoDecryptFailure(height)
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
			if s.wallet.MarkSpent(ownedPubKey, height) {
				spent++
			}
		}
		delete(spendableByKeyImage, keyImage)
	}

	return found, spent
}

func (s *Scanner) buildSpendableKeyImageIndex() map[[32]byte][][32]byte {
	spendableByKeyImage := make(map[[32]byte][][32]byte)
	for _, out := range s.wallet.outputsForKeyImageScan() {
		keyImage, err := s.config.GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			continue
		}
		spendableByKeyImage[keyImage] = append(spendableByKeyImage[keyImage], out.OneTimePubKey)
	}
	return spendableByKeyImage
}

// ScanBlocks scans multiple blocks. The spendable-key-image index is built once
// for the whole batch and maintained incrementally, rather than rebuilt (and its
// key images regenerated) per block.
func (s *Scanner) ScanBlocks(blocks []*BlockData) (totalFound, totalSpent int) {
	return s.scanBatch(blocks, func(block *BlockData, found, spent int) {
		s.wallet.SetSyncedHeight(block.Height)
	})
}

// ScanBlocksReport scans a batch of blocks, building the spendable-key-image
// index once for the whole batch. report, if non-nil, is called after each
// block with its height and the per-block found/spent counts. Scanning stops at
// the first nil block. It does not update the wallet's synced height; the caller
// decides when to persist that.
func (s *Scanner) ScanBlocksReport(blocks []*BlockData, report func(height uint64, found, spent int)) (totalFound, totalSpent int) {
	return s.scanBatch(blocks, func(block *BlockData, found, spent int) {
		if report != nil {
			report(block.Height, found, spent)
		}
	})
}

// scanBatch scans a run of blocks in two phases: a parallel, read-only match
// phase across every transaction, followed by a serial apply phase in block
// order (which mutates the wallet and the shared key-image index). Splitting the
// phases lets the expensive per-output crypto run concurrently while keeping the
// stateful bookkeeping ordered and lock-free. onBlock runs after each block's
// apply, in order. Scanning stops at the first nil block.
func (s *Scanner) scanBatch(blocks []*BlockData, onBlock func(block *BlockData, found, spent int)) (totalFound, totalSpent int) {
	// Truncate at the first gap so we never scan past a missing block.
	end := len(blocks)
	for i, b := range blocks {
		if b == nil {
			end = i
			break
		}
	}
	blocks = blocks[:end]
	if len(blocks) == 0 {
		return 0, 0
	}

	keys := s.wallet.Keys()
	spendableByKeyImage := s.buildSpendableKeyImageIndex()

	// Phase 1 (parallel): match every transaction's outputs.
	matches := s.matchBlocksParallel(blocks, keys)

	// Phase 2 (serial, ordered): apply matches and detect spends.
	for bi, block := range blocks {
		var bf, bsp int
		for ti := range block.Transactions {
			tx := &block.Transactions[ti]
			f, sp := s.applyTxMatches(tx, block.Height, keys, matches[bi][ti], spendableByKeyImage)
			bf += f
			bsp += sp
		}
		totalFound += bf
		totalSpent += bsp
		if onBlock != nil {
			onBlock(block, bf, bsp)
		}
	}
	return totalFound, totalSpent
}

// matchBlocksParallel runs matchTxOutputs for every transaction across all
// blocks, distributing the work over the available CPUs. The result is indexed
// [blockIndex][txIndex]; each worker writes a distinct element, so no
// synchronization is needed beyond the final barrier.
func (s *Scanner) matchBlocksParallel(blocks []*BlockData, keys StealthKeys) [][][]outputMatch {
	result := make([][][]outputMatch, len(blocks))
	type task struct{ bi, ti int }
	var tasks []task
	for bi, block := range blocks {
		result[bi] = make([][]outputMatch, len(block.Transactions))
		for ti := range block.Transactions {
			tasks = append(tasks, task{bi, ti})
		}
	}
	if len(tasks) == 0 {
		return result
	}

	// Respect GOMAXPROCS (honors container/cgroup CPU limits) rather than the
	// raw core count.
	workers := runtime.GOMAXPROCS(0)
	if workers > len(tasks) {
		workers = len(tasks)
	}

	// Small batches aren't worth the goroutine overhead.
	if workers <= 1 {
		for _, t := range tasks {
			tx := &blocks[t.bi].Transactions[t.ti]
			result[t.bi][t.ti] = s.matchTxOutputs(tx, blocks[t.bi].Height, keys)
		}
		return result
	}

	var next int64 = -1
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(tasks) {
					return
				}
				t := tasks[i]
				tx := &blocks[t.bi].Transactions[t.ti]
				result[t.bi][t.ti] = s.matchTxOutputs(tx, blocks[t.bi].Height, keys)
			}
		}()
	}
	wg.Wait()
	return result
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

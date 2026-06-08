package main

import (
	"crypto/rand"
	"crypto/sha3"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"math/big"
	"sync"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

func addU64(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	return sum, carry == 0
}

// TxOutput represents a transaction output (UTXO)
type TxOutput struct {
	// Commitment is a Pedersen commitment to the amount (hides value)
	Commitment [32]byte `json:"commitment"`

	// PublicKey is the one-time stealth address for this output
	PublicKey [32]byte `json:"public_key"`

	// RangeProof proves the committed amount is in valid range [0, 2^64)
	RangeProof []byte `json:"range_proof,omitempty"`

	// EncryptedAmount is the amount encrypted with ECDH shared secret
	// The recipient can decrypt this with their view key
	EncryptedAmount [8]byte `json:"encrypted_amount"`

	// EncryptedMemo is a fixed-size encrypted memo envelope.
	EncryptedMemo [params.MemoSize]byte `json:"encrypted_memo"`
}

// OwnedOutput contains the secret data the owner needs to spend an output
type OwnedOutput struct {
	UTXO     *UTXO
	Amount   uint64
	Blinding [32]byte
	PrivKey  [32]byte // One-time private key to spend
}

// TxInput references a previous output being spent
type TxInput struct {
	// KeyImage is used to prevent double-spending without revealing which output is spent
	KeyImage [32]byte `json:"key_image"`

	// RingMembers are public keys used in the ring signature (includes real + decoys)
	RingMembers [][32]byte `json:"ring_members"`

	// RingCommitments are the Pedersen commitments for each ring member
	// Used for RingCT commitment linking
	RingCommitments [][32]byte `json:"ring_commitments"`

	// PseudoOutput is a commitment to the same amount as the real input
	// but with a different blinding factor. Used for balance verification.
	PseudoOutput [32]byte `json:"pseudo_output"`

	// RingSignature is a RingCT CLSAG signature proving:
	// 1. Ownership of one ring member's private key
	// 2. PseudoOutput commits to the same amount as that ring member
	RingSignature []byte `json:"ring_signature"`
}

// Transaction represents a privacy-preserving transaction
type Transaction struct {
	// Version for future upgrades
	Version uint8 `json:"version"`

	// Inputs being spent
	Inputs []TxInput `json:"inputs"`

	// Outputs being created
	Outputs []TxOutput `json:"outputs"`

	// Fee is public (can't hide this without complexity)
	Fee uint64 `json:"fee"`

	// TxPublicKey is used for stealth address derivation
	TxPublicKey [32]byte `json:"tx_public_key"`
}

// TxID returns the transaction ID (sha3-256 of canonical binary serialization).
func (tx *Transaction) TxID() ([32]byte, error) {
	return sha3.Sum256(tx.Serialize()), nil
}

// IsCoinbase returns true if this is a coinbase (mining reward) transaction
func (tx *Transaction) IsCoinbase() bool {
	return len(tx.Inputs) == 0
}

// Serialize encodes the transaction into the canonical binary wire format.
// This matches serializeTx in wallet/builder.go exactly.
func (tx *Transaction) Serialize() []byte {
	// Calculate prefix size
	size := 1 + 32 + 4 + 4 + 8 // version + txPubKey + inputCount + outputCount + fee
	for _, out := range tx.Outputs {
		size += 32 + 32 + 8 + params.MemoSize + 4 + len(out.RangeProof)
	}
	// Calculate input sizes
	for _, inp := range tx.Inputs {
		ringSize := len(inp.RingMembers)
		size += 32 + 32 + 4 + ringSize*32 + ringSize*32 + 4 + len(inp.RingSignature)
	}

	buf := make([]byte, size)
	off := 0

	// -- Prefix --
	buf[off] = tx.Version
	off++

	copy(buf[off:], tx.TxPublicKey[:])
	off += 32

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(tx.Inputs)))
	off += 4

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(tx.Outputs)))
	off += 4

	binary.LittleEndian.PutUint64(buf[off:], tx.Fee)
	off += 8

	for _, out := range tx.Outputs {
		copy(buf[off:], out.PublicKey[:])
		off += 32

		copy(buf[off:], out.Commitment[:])
		off += 32

		copy(buf[off:], out.EncryptedAmount[:])
		off += 8

		copy(buf[off:], out.EncryptedMemo[:])
		off += params.MemoSize

		binary.LittleEndian.PutUint32(buf[off:], uint32(len(out.RangeProof)))
		off += 4

		copy(buf[off:], out.RangeProof)
		off += len(out.RangeProof)
	}

	// -- Inputs --
	for _, inp := range tx.Inputs {
		copy(buf[off:], inp.KeyImage[:])
		off += 32

		copy(buf[off:], inp.PseudoOutput[:])
		off += 32

		ringSize := len(inp.RingMembers)
		binary.LittleEndian.PutUint32(buf[off:], uint32(ringSize))
		off += 4

		for _, pk := range inp.RingMembers {
			copy(buf[off:], pk[:])
			off += 32
		}

		for _, c := range inp.RingCommitments {
			copy(buf[off:], c[:])
			off += 32
		}

		binary.LittleEndian.PutUint32(buf[off:], uint32(len(inp.RingSignature)))
		off += 4

		copy(buf[off:], inp.RingSignature)
		off += len(inp.RingSignature)
	}

	return buf
}

// SigningHash returns the sha3-256 hash of the binary tx prefix (everything
// except input signatures). This is the message that RingCT signatures are
// created and verified against. The encoding mirrors serializeTxPrefix in
// wallet/builder.go exactly.
func (tx *Transaction) SigningHash() [32]byte {
	size := 1 + 32 + 4 + 4 + 8 // version + txPubKey + inputCount + outputCount + fee
	for _, out := range tx.Outputs {
		size += 32 + 32 + 8 + params.MemoSize + 4 + len(out.RangeProof)
	}

	buf := make([]byte, size)
	off := 0

	buf[off] = tx.Version
	off++

	copy(buf[off:], tx.TxPublicKey[:])
	off += 32

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(tx.Inputs)))
	off += 4

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(tx.Outputs)))
	off += 4

	binary.LittleEndian.PutUint64(buf[off:], tx.Fee)
	off += 8

	for _, out := range tx.Outputs {
		copy(buf[off:], out.PublicKey[:])
		off += 32

		copy(buf[off:], out.Commitment[:])
		off += 32

		copy(buf[off:], out.EncryptedAmount[:])
		off += 8

		copy(buf[off:], out.EncryptedMemo[:])
		off += params.MemoSize

		binary.LittleEndian.PutUint32(buf[off:], uint32(len(out.RangeProof)))
		off += 4

		copy(buf[off:], out.RangeProof)
		off += len(out.RangeProof)
	}

	return sha3.Sum256(buf)
}

// DeserializeTx parses a transaction from the binary wire format produced by the wallet builder.
// Format: version(1) | txPubKey(32) | inputCount(4) | outputCount(4) | fee(8)
//
//	then outputCount outputs, then inputCount inputs.
func DeserializeTx(data []byte) (*Transaction, error) {
	const (
		maxInputs     = 256
		maxOutputs    = 256
		// Practical protocol ceilings:
		// - ring size is fixed at RingSize for relaunch format
		// - Bulletproof range proofs are bounded well below 1024 bytes in this implementation
		// - RingCT signature is fixed-size given ringSize: 96 + 64*n
		maxRingSize   = RingSize
		maxProofSize  = 1024
		maxSigSize    = 96 + 64*RingSize
		minHeaderSize = 1 + 32 + 4 + 4 + 8 // 49 bytes
	)

	if len(data) < minHeaderSize {
		return nil, fmt.Errorf("tx data too short: %d bytes", len(data))
	}

	off := 0

	// Version
	version := data[off]
	off++

	// Tx public key
	var txPubKey [32]byte
	copy(txPubKey[:], data[off:off+32])
	off += 32

	// Input count
	inputCount := binary.LittleEndian.Uint32(data[off:])
	off += 4
	if inputCount > maxInputs {
		return nil, fmt.Errorf("input count %d exceeds max %d", inputCount, maxInputs)
	}

	// Output count
	outputCount := binary.LittleEndian.Uint32(data[off:])
	off += 4
	if outputCount > maxOutputs {
		return nil, fmt.Errorf("output count %d exceeds max %d", outputCount, maxOutputs)
	}

	// Fee
	fee := binary.LittleEndian.Uint64(data[off:])
	off += 8

	// Outputs
	var outputs []TxOutput
	if outputCount > 0 {
		outputs = make([]TxOutput, outputCount)
	}
	for i := uint32(0); i < outputCount; i++ {
		if off+32+32+8+params.MemoSize+4 > len(data) {
			return nil, fmt.Errorf("unexpected end of data in output %d", i)
		}

		copy(outputs[i].PublicKey[:], data[off:off+32])
		off += 32

		copy(outputs[i].Commitment[:], data[off:off+32])
		off += 32

		copy(outputs[i].EncryptedAmount[:], data[off:off+8])
		off += 8

		copy(outputs[i].EncryptedMemo[:], data[off:off+params.MemoSize])
		off += params.MemoSize

		proofLen := binary.LittleEndian.Uint32(data[off:])
		off += 4
		if proofLen > maxProofSize {
			return nil, fmt.Errorf("range proof size %d exceeds max %d in output %d", proofLen, maxProofSize, i)
		}
		if off+int(proofLen) > len(data) {
			return nil, fmt.Errorf("unexpected end of data in output %d range proof", i)
		}
		if proofLen > 0 {
			outputs[i].RangeProof = make([]byte, proofLen)
			copy(outputs[i].RangeProof, data[off:off+int(proofLen)])
		}
		off += int(proofLen)
	}

	// Inputs
	var inputs []TxInput
	if inputCount > 0 {
		inputs = make([]TxInput, inputCount)
	}
	for i := uint32(0); i < inputCount; i++ {
		if off+32+32+4 > len(data) {
			return nil, fmt.Errorf("unexpected end of data in input %d", i)
		}

		copy(inputs[i].KeyImage[:], data[off:off+32])
		off += 32

		copy(inputs[i].PseudoOutput[:], data[off:off+32])
		off += 32

		ringSize := binary.LittleEndian.Uint32(data[off:])
		off += 4
		if ringSize > maxRingSize {
			return nil, fmt.Errorf("ring size %d exceeds max %d in input %d", ringSize, maxRingSize, i)
		}

		// Ring member public keys
		needed := int(ringSize) * 32
		if off+needed > len(data) {
			return nil, fmt.Errorf("unexpected end of data in input %d ring members", i)
		}
		inputs[i].RingMembers = make([][32]byte, ringSize)
		for j := uint32(0); j < ringSize; j++ {
			copy(inputs[i].RingMembers[j][:], data[off:off+32])
			off += 32
		}

		// Ring member commitments
		if off+needed > len(data) {
			return nil, fmt.Errorf("unexpected end of data in input %d ring commitments", i)
		}
		inputs[i].RingCommitments = make([][32]byte, ringSize)
		for j := uint32(0); j < ringSize; j++ {
			copy(inputs[i].RingCommitments[j][:], data[off:off+32])
			off += 32
		}

		// Signature
		if off+4 > len(data) {
			return nil, fmt.Errorf("unexpected end of data in input %d signature length", i)
		}
		sigLen := binary.LittleEndian.Uint32(data[off:])
		off += 4
		if sigLen > maxSigSize {
			return nil, fmt.Errorf("signature size %d exceeds max %d in input %d", sigLen, maxSigSize, i)
		}
		if off+int(sigLen) > len(data) {
			return nil, fmt.Errorf("unexpected end of data in input %d signature", i)
		}
		if sigLen > 0 {
			inputs[i].RingSignature = make([]byte, sigLen)
			copy(inputs[i].RingSignature, data[off:off+int(sigLen)])
		}
		off += int(sigLen)
	}

	// Enforce canonical parse: successful decode must consume all bytes.
	// Aux trailers were removed for relaunch; accepting trailing bytes is non-canonical.
	if off != len(data) {
		return nil, fmt.Errorf("trailing bytes after tx parse: %d", len(data)-off)
	}

	return &Transaction{
		Version:     version,
		TxPublicKey: txPubKey,
		Inputs:      inputs,
		Outputs:     outputs,
		Fee:         fee,
	}, nil
}

// UTXO represents an unspent transaction output in the UTXO set
type UTXO struct {
	TxID        [32]byte `json:"tx_id"`
	OutputIndex uint32   `json:"output_index"`
	Output      TxOutput `json:"output"`
	BlockHeight uint64   `json:"block_height"`
}

// UTXOKey returns the unique key for this UTXO
func (u *UTXO) UTXOKey() string {
	return fmt.Sprintf("%x:%d", u.TxID, u.OutputIndex)
}

// UTXOSet manages the set of unspent transaction outputs
type UTXOSet struct {
	mu        sync.RWMutex
	utxos     map[string]*UTXO
	keyImages map[string]bool

	// Index by public key for faster scanning
	byPubKey map[string][]*UTXO
}

// NewUTXOSet creates a new empty UTXO set
func NewUTXOSet() *UTXOSet {
	return &UTXOSet{
		utxos:     make(map[string]*UTXO),
		keyImages: make(map[string]bool),
		byPubKey:  make(map[string][]*UTXO),
	}
}

// Add adds a new UTXO to the set
func (s *UTXOSet) Add(txID [32]byte, index uint32, output TxOutput, blockHeight uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	utxo := &UTXO{
		TxID:        txID,
		OutputIndex: index,
		Output:      output,
		BlockHeight: blockHeight,
	}

	key := utxo.UTXOKey()
	s.utxos[key] = utxo

	// Index by public key
	pkHex := hex.EncodeToString(output.PublicKey[:])
	s.byPubKey[pkHex] = append(s.byPubKey[pkHex], utxo)
}

// MarkSpent marks a key image as spent (prevents double-spending)
func (s *UTXOSet) MarkSpent(keyImage [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hex.EncodeToString(keyImage[:])
	if s.keyImages[key] {
		return fmt.Errorf("key image already spent (double-spend attempt)")
	}
	s.keyImages[key] = true
	return nil
}

// IsSpent checks if a key image has been used
func (s *UTXOSet) IsSpent(keyImage [32]byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := hex.EncodeToString(keyImage[:])
	return s.keyImages[key]
}

// GetAllUTXOs returns all UTXOs (for ring member selection)
func (s *UTXOSet) GetAllUTXOs() []*UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*UTXO, 0, len(s.utxos))
	for _, utxo := range s.utxos {
		result = append(result, utxo)
	}
	return result
}

// GetByPublicKey returns UTXOs for a specific public key
func (s *UTXOSet) GetByPublicKey(pubKey [32]byte) []*UTXO {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pkHex := hex.EncodeToString(pubKey[:])
	return s.byPubKey[pkHex]
}

// SelectRingMembers selects random decoy UTXOs for ring signature
// Returns the selected public keys and the index where realPubKey should be inserted
func (s *UTXOSet) SelectRingMembers(realPubKey [32]byte) ([][32]byte, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ringSize := RingSize

	// Get all available UTXOs as potential decoys
	allUTXOs := make([]*UTXO, 0, len(s.utxos))
	for _, utxo := range s.utxos {
		// Don't include our own output as a decoy
		if utxo.Output.PublicKey != realPubKey {
			allUTXOs = append(allUTXOs, utxo)
		}
	}

	if len(allUTXOs) < ringSize-1 {
		return nil, 0, fmt.Errorf("not enough UTXOs for ring (need %d, have %d)", ringSize-1, len(allUTXOs))
	}

	// Shuffle and pick decoys (crypto/rand for anonymity)
	for i := len(allUTXOs) - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, 0, fmt.Errorf("crypto/rand failed: %w", err)
		}
		j := int(jBig.Int64())
		allUTXOs[i], allUTXOs[j] = allUTXOs[j], allUTXOs[i]
	}

	decoys := allUTXOs[:ringSize-1]

	// Create ring with random position for real key (crypto/rand)
	idxBig, err := rand.Int(rand.Reader, big.NewInt(int64(ringSize)))
	if err != nil {
		return nil, 0, fmt.Errorf("crypto/rand failed: %w", err)
	}
	secretIndex := int(idxBig.Int64())
	ring := make([][32]byte, ringSize)

	decoyIdx := 0
	for i := 0; i < ringSize; i++ {
		if i == secretIndex {
			ring[i] = realPubKey
		} else {
			ring[i] = decoys[decoyIdx].Output.PublicKey
			decoyIdx++
		}
	}

	return ring, secretIndex, nil
}

// RingMemberData contains both public key and commitment for ring members
type RingMemberData struct {
	Keys        [][32]byte
	Commitments [][32]byte
	SecretIndex int
}

// SelectRingMembersWithCommitments selects ring members and returns their commitments too
// Required for RingCT to prove amount equality
func (s *UTXOSet) SelectRingMembersWithCommitments(realPubKey, realCommitment [32]byte) (*RingMemberData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ringSize := RingSize

	// Get all available UTXOs as potential decoys
	allUTXOs := make([]*UTXO, 0, len(s.utxos))
	for _, utxo := range s.utxos {
		if utxo.Output.PublicKey != realPubKey {
			allUTXOs = append(allUTXOs, utxo)
		}
	}

	if len(allUTXOs) < ringSize-1 {
		return nil, fmt.Errorf("not enough UTXOs for ring (need %d, have %d)", ringSize-1, len(allUTXOs))
	}

	// Shuffle and pick decoys (crypto/rand for anonymity)
	for i := len(allUTXOs) - 1; i > 0; i-- {
		jBig, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, fmt.Errorf("crypto/rand failed: %w", err)
		}
		j := int(jBig.Int64())
		allUTXOs[i], allUTXOs[j] = allUTXOs[j], allUTXOs[i]
	}

	decoys := allUTXOs[:ringSize-1]

	// Create ring with random position for real key (crypto/rand)
	idxBig, err := rand.Int(rand.Reader, big.NewInt(int64(ringSize)))
	if err != nil {
		return nil, fmt.Errorf("crypto/rand failed: %w", err)
	}
	secretIndex := int(idxBig.Int64())
	keys := make([][32]byte, ringSize)
	commitments := make([][32]byte, ringSize)

	decoyIdx := 0
	for i := 0; i < ringSize; i++ {
		if i == secretIndex {
			keys[i] = realPubKey
			commitments[i] = realCommitment
		} else {
			keys[i] = decoys[decoyIdx].Output.PublicKey
			commitments[i] = decoys[decoyIdx].Output.Commitment
			decoyIdx++
		}
	}

	return &RingMemberData{
		Keys:        keys,
		Commitments: commitments,
		SecretIndex: secretIndex,
	}, nil
}

// Count returns the number of UTXOs in the set
func (s *UTXOSet) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.utxos)
}

// KeyImageCount returns the number of spent key images
func (s *UTXOSet) KeyImageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keyImages)
}

// ============================================================================
// Transaction Building
// ============================================================================

// TxBuilder helps construct privacy-preserving transactions
type TxBuilder struct {
	inputs  []txBuilderInput
	outputs []txBuilderOutput
	fee     uint64
}

type txBuilderInput struct {
	ownedOutput *OwnedOutput // Contains UTXO, amount, blinding, privkey
}

type txBuilderOutput struct {
	recipientSpendPub [32]byte
	recipientViewPub  [32]byte
	amount            uint64
	blinding          [32]byte // Set during Build() to balance
	commitment        [32]byte // Set during Build()
}

// NewTxBuilder creates a new transaction builder
func NewTxBuilder() *TxBuilder {
	return &TxBuilder{}
}

// AddInput adds an input to spend (using owned output data)
func (b *TxBuilder) AddInput(owned *OwnedOutput) {
	b.inputs = append(b.inputs, txBuilderInput{
		ownedOutput: owned,
	})
}

// AddOutput adds an output to create
func (b *TxBuilder) AddOutput(spendPubKey, viewPubKey [32]byte, amount uint64) {
	b.outputs = append(b.outputs, txBuilderOutput{
		recipientSpendPub: spendPubKey,
		recipientViewPub:  viewPubKey,
		amount:            amount,
	})
}

// SetFee sets the transaction fee
func (b *TxBuilder) SetFee(fee uint64) {
	b.fee = fee
}

// Build constructs the transaction with balanced commitments
func (b *TxBuilder) Build(utxoSet *UTXOSet) (*Transaction, error) {
	if len(b.inputs) == 0 {
		return nil, fmt.Errorf("transaction must have at least one input")
	}
	if len(b.outputs) == 0 {
		return nil, fmt.Errorf("transaction must have at least one output")
	}

	// Verify amounts balance
	var inputSum, outputSum uint64
	for _, in := range b.inputs {
		var ok bool
		inputSum, ok = addU64(inputSum, in.ownedOutput.Amount)
		if !ok {
			return nil, fmt.Errorf("input amount sum overflows uint64")
		}
	}
	for _, out := range b.outputs {
		var ok bool
		outputSum, ok = addU64(outputSum, out.amount)
		if !ok {
			return nil, fmt.Errorf("output amount sum overflows uint64")
		}
	}
	outputPlusFee, ok := addU64(outputSum, b.fee)
	if !ok {
		return nil, fmt.Errorf("output amount + fee overflows uint64")
	}
	if inputSum != outputPlusFee {
		return nil, fmt.Errorf("amounts don't balance: inputs=%d, outputs=%d, fee=%d",
			inputSum, outputSum, b.fee)
	}

	tx := &Transaction{
		Version: 1,
		Fee:     b.fee,
	}

	// Calculate sum of input blinding factors
	var inputBlindingSum [32]byte
	for i, in := range b.inputs {
		if i == 0 {
			inputBlindingSum = in.ownedOutput.Blinding
		} else {
			sum, err := BlindingAdd(inputBlindingSum, in.ownedOutput.Blinding)
			if err != nil {
				return nil, fmt.Errorf("failed to sum blindings: %w", err)
			}
			inputBlindingSum = sum
		}
	}

	// Create outputs with balanced blinding factors
	// For all outputs except the last, use random blinding
	// For the last output, compute blinding so that sum(outputs) = sum(inputs)
	var outputBlindingSum [32]byte
	outputBlindings := make([][32]byte, len(b.outputs))

	for i := range b.outputs {
		if i < len(b.outputs)-1 {
			// Random blinding for non-last outputs
			commitment, err := CreatePedersenCommitment(b.outputs[i].amount)
			if err != nil {
				return nil, fmt.Errorf("failed to create commitment: %w", err)
			}
			outputBlindings[i] = commitment.Blinding
			b.outputs[i].blinding = commitment.Blinding
			b.outputs[i].commitment = commitment.Commitment

			if i == 0 {
				outputBlindingSum = outputBlindings[i]
			} else {
				sum, err := BlindingAdd(outputBlindingSum, outputBlindings[i])
				if err != nil {
					return nil, fmt.Errorf("failed to sum blindings: %w", err)
				}
				outputBlindingSum = sum
			}
		} else {
			// Last output: blinding = sum(input_blindings) - sum(other_output_blindings)
			lastBlinding, err := BlindingSub(inputBlindingSum, outputBlindingSum)
			if err != nil {
				return nil, fmt.Errorf("failed to compute last blinding: %w", err)
			}
			outputBlindings[i] = lastBlinding
			b.outputs[i].blinding = lastBlinding

			// Create commitment with this specific blinding
			commitment, err := CreatePedersenCommitmentWithBlinding(b.outputs[i].amount, lastBlinding)
			if err != nil {
				return nil, fmt.Errorf("failed to create last commitment: %w", err)
			}
			b.outputs[i].commitment = commitment
		}
	}

	// Build outputs FIRST (so we can sign the complete transaction)
	for i, out := range b.outputs {
		// Derive one-time stealth address
		stealthOut, err := DeriveStealthAddress(out.recipientSpendPub, out.recipientViewPub)
		if err != nil {
			return nil, fmt.Errorf("failed to derive stealth address: %w", err)
		}

		// Set transaction public key (from first output)
		if tx.TxPublicKey == [32]byte{} {
			tx.TxPublicKey = stealthOut.TxPubKey
		}

		// Create range proof
		rangeProof, err := CreateRangeProof(out.amount, outputBlindings[i])
		if err != nil {
			return nil, fmt.Errorf("failed to create range proof: %w", err)
		}

		// Ensure memo bytes are always a real encrypted envelope (never all-zero).
		// This builder path is legacy and does not yet implement full wallet-compatible
		// shared-secret derivation, but we still enforce memo ciphertext uniformity by
		// producing a padded envelope encrypted under the per-output blinding key.
		encryptedMemo, err := wallet.EncryptMemo(nil, out.blinding, i)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt memo for output %d: %w", i, err)
		}

		tx.Outputs = append(tx.Outputs, TxOutput{
			Commitment:    out.commitment,
			PublicKey:     stealthOut.OnetimePubKey,
			RangeProof:    rangeProof.Proof,
			EncryptedMemo: encryptedMemo,
		})
	}

	// Build inputs with RingCT signatures
	// Each input needs a pseudo-output that commits to the same amount as the real input

	// Calculate sum of output blindings (we already have this from above)
	// For proper balance: sum(pseudo_outputs) = sum(outputs) + fee*G
	// Since pseudo_output[i] = amount[i]*H + pseudo_blinding[i]*G
	// We need: sum(pseudo_blindings) = sum(output_blindings)
	// (fee doesn't need blinding since it's public)

	// Calculate total output blinding
	var totalOutputBlinding [32]byte
	for i, blinding := range outputBlindings {
		if i == 0 {
			totalOutputBlinding = blinding
		} else {
			sum, err := BlindingAdd(totalOutputBlinding, blinding)
			if err != nil {
				return nil, fmt.Errorf("failed to sum output blindings: %w", err)
			}
			totalOutputBlinding = sum
		}
	}

	// Generate pseudo-output blindings
	// For all inputs except the last, use random blindings
	// For the last input, compute so that sum(pseudo_blindings) = sum(output_blindings)
	pseudoBlindings := make([][32]byte, len(b.inputs))
	var pseudoBlindingSum [32]byte

	for i := range b.inputs {
		if i < len(b.inputs)-1 {
			// Random blinding for non-last inputs (crypto/rand)
			var randBlinding [32]byte
			if _, err := rand.Read(randBlinding[:]); err != nil {
				return nil, fmt.Errorf("crypto/rand failed: %w", err)
			}
			pseudoBlindings[i] = randBlinding

			if i == 0 {
				pseudoBlindingSum = pseudoBlindings[i]
			} else {
				sum, err := BlindingAdd(pseudoBlindingSum, pseudoBlindings[i])
				if err != nil {
					return nil, fmt.Errorf("failed to sum pseudo blindings: %w", err)
				}
				pseudoBlindingSum = sum
			}
		} else {
			// Last input: blinding = sum(output_blindings) - sum(other_pseudo_blindings)
			if len(b.inputs) == 1 {
				pseudoBlindings[i] = totalOutputBlinding
			} else {
				lastBlinding, err := BlindingSub(totalOutputBlinding, pseudoBlindingSum)
				if err != nil {
					return nil, fmt.Errorf("failed to compute last pseudo blinding: %w", err)
				}
				pseudoBlindings[i] = lastBlinding
			}
		}
	}

	// First pass: create input structures with ring members and pseudo-outputs
	ringData := make([]*RingMemberData, len(b.inputs))
	for i, in := range b.inputs {
		owned := in.ownedOutput

		// Get key image
		keyImage, err := GenerateKeyImage(owned.PrivKey)
		if err != nil {
			return nil, fmt.Errorf("failed to generate key image: %w", err)
		}

		// Select ring members with commitments
		ring, err := utxoSet.SelectRingMembersWithCommitments(
			owned.UTXO.Output.PublicKey,
			owned.UTXO.Output.Commitment,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to select ring members: %w", err)
		}
		ringData[i] = ring

		// Create pseudo-output commitment (same amount, different blinding)
		pseudoOutput, err := CreatePedersenCommitmentWithBlinding(owned.Amount, pseudoBlindings[i])
		if err != nil {
			return nil, fmt.Errorf("failed to create pseudo-output: %w", err)
		}

		// Build input without signature first
		tx.Inputs = append(tx.Inputs, TxInput{
			KeyImage:        keyImage,
			RingMembers:     ring.Keys,
			RingCommitments: ring.Commitments,
			PseudoOutput:    pseudoOutput,
		})
	}

	// Second pass: sign each input with RingCT
	for i, in := range b.inputs {
		owned := in.ownedOutput
		ring := ringData[i]

		// Sign with RingCT (proves key ownership AND amount equality)
		sigHash := tx.SigningHash()
		ringSig, err := SignRingCT(
			ring.Keys,
			ring.Commitments,
			ring.SecretIndex,
			owned.PrivKey,
			owned.Blinding,            // Real input blinding
			tx.Inputs[i].PseudoOutput, // Pseudo-output commitment
			pseudoBlindings[i],        // Pseudo-output blinding
			sigHash[:],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create RingCT signature: %w", err)
		}

		tx.Inputs[i].RingSignature = ringSig.Signature
	}

	return tx, nil
}

// ============================================================================
// Transaction Validation
// ============================================================================

// KeyImageChecker is a function that checks if a key image is spent
type KeyImageChecker func(keyImage [32]byte) bool

// RingMemberChecker verifies that a ring member+commitment pair exists in canonical chain state.
type RingMemberChecker func(pubKey, commitment [32]byte) bool

// ValidateTransaction validates a transaction with full cryptographic
// verification (RingCT signatures and range proofs).
func ValidateTransaction(tx *Transaction, isSpent KeyImageChecker, isCanonicalRingMember RingMemberChecker) error {
	return validateTransaction(tx, isSpent, isCanonicalRingMember, false)
}

// validateTransaction is the implementation behind ValidateTransaction.
//
// When skipCrypto is true, the expensive RingCT signature verification
// (VerifyRingCT) and output range-proof verification (VerifyRangeProof, including
// the coinbase's) are bypassed. This is ONLY set for blocks at or below a trusted
// checkpoint height during fast sync — see Chain.validateBlockForProcessLocked.
// The checkpoint hash already attests that this history is valid, so re-verifying
// every proof is redundant work and is the dominant cost of a fresh sync. All
// structural, binding, double-spend, and commitment-balance checks still run, and
// outputs/key images are still recorded into the indexes — so wallet scanning and
// balance calculation are completely unaffected.
func validateTransaction(tx *Transaction, isSpent KeyImageChecker, isCanonicalRingMember RingMemberChecker, skipCrypto bool) error {
	// Transaction.Version gates the entire transaction template / wire format.
	// Memo has its own plaintext-envelope version; this version is for the tx itself.
	if tx.Version != 1 {
		return fmt.Errorf("unsupported tx version: %d", tx.Version)
	}

	// Coinbase transactions have no inputs
	if tx.IsCoinbase() {
		return validateCoinbase(tx, skipCrypto)
	}
	if isCanonicalRingMember == nil {
		return fmt.Errorf("ring member checker is required")
	}

	// Memo ciphertext is consensus-visible but not consensus-decryptable (requires wallet secrets).
	// Enforce only the invariants we can check deterministically without decryption.
	var zeroMemo [params.MemoSize]byte
	for i := range tx.Outputs {
		if tx.Outputs[i].EncryptedMemo == zeroMemo {
			return fmt.Errorf("output %d: encrypted memo must not be all-zero", i)
		}
	}

	// Reject duplicate key images within the same transaction.
	if len(tx.Inputs) > 1 {
		seen := make(map[[32]byte]struct{}, len(tx.Inputs))
		for i, input := range tx.Inputs {
			if _, dup := seen[input.KeyImage]; dup {
				return fmt.Errorf("input %d: duplicate key image within transaction", i)
			}
			seen[input.KeyImage] = struct{}{}
		}
	}

	// Check each input
	for i, input := range tx.Inputs {
		// Verify ring size is exactly RingSize
		if len(input.RingMembers) != RingSize {
			return fmt.Errorf("input %d: ring size must be %d, got %d", i, RingSize, len(input.RingMembers))
		}
		if len(input.RingCommitments) != RingSize {
			return fmt.Errorf("input %d: ring commitments size must be %d, got %d", i, RingSize, len(input.RingCommitments))
		}
		for j := range input.RingMembers {
			if !isCanonicalRingMember(input.RingMembers[j], input.RingCommitments[j]) {
				return fmt.Errorf("input %d ring member %d: not a canonical on-chain output", i, j)
			}
		}

		ringSig := &RingCTSignature{
			Signature: input.RingSignature,
			RingSize:  len(input.RingMembers),
		}

		// Verify RingCT signature (proves key ownership AND amount equality).
		// Skipped below a trusted checkpoint during fast sync — the checkpoint
		// already attests this history is valid. The binding and double-spend
		// checks below still run regardless.
		if !skipCrypto {
			sigHash := tx.SigningHash()
			if err := VerifyRingCT(input.RingMembers, input.RingCommitments, sigHash[:], ringSig); err != nil {
				return fmt.Errorf("input %d: invalid RingCT signature: %w", i, err)
			}
		}

		sigKeyImage, sigPseudoOutput, err := ExtractRingCTBinding(ringSig)
		if err != nil {
			return fmt.Errorf("input %d: invalid RingCT signature payload: %w", i, err)
		}
		if sigKeyImage != input.KeyImage {
			return fmt.Errorf("input %d: key image does not match signed RingCT payload", i)
		}
		if sigPseudoOutput != input.PseudoOutput {
			return fmt.Errorf("input %d: pseudo-output does not match signed RingCT payload", i)
		}

		// Check key image not already spent only after binding it to signature payload.
		if isSpent(sigKeyImage) {
			return fmt.Errorf("input %d: key image already spent (double-spend)", i)
		}
	}

	// Verify each output range proof. Skipped below a trusted checkpoint during
	// fast sync (see skipCrypto) — this is the single largest cost of a sync, as
	// it is paid on every output including every block's coinbase.
	if !skipCrypto {
		for i, output := range tx.Outputs {
			rangeProof := &RangeProof{Proof: output.RangeProof}
			if err := VerifyRangeProof(output.Commitment, rangeProof); err != nil {
				return fmt.Errorf("output %d: invalid range proof: %w", i, err)
			}
		}
	}

	// Verify commitment balance using pseudo-outputs (RingCT)
	// sum(pseudo_outputs) = sum(outputs) + fee
	if err := verifyCommitmentBalance(tx); err != nil {
		return fmt.Errorf("commitment balance: %w", err)
	}

	return nil
}

// verifyCommitmentBalance verifies that input commitments equal output commitments + fee
//
// With RingCT, we verify: sum(pseudo_outputs) = sum(outputs) + fee*G
//
// The RingCT signatures prove that each pseudo_output commits to the same amount
// as the corresponding real input (without revealing which ring member is real).
// Therefore if pseudo_outputs balance with outputs + fee, the real inputs do too.
func verifyCommitmentBalance(tx *Transaction) error {
	if len(tx.Outputs) == 0 {
		return fmt.Errorf("transaction has no outputs")
	}
	if len(tx.Inputs) == 0 {
		return fmt.Errorf("transaction has no inputs")
	}

	// Sum pseudo-output commitments
	var pseudoSum [32]byte
	for i, input := range tx.Inputs {
		if i == 0 {
			pseudoSum = input.PseudoOutput
		} else {
			sum, err := CommitmentAdd(pseudoSum, input.PseudoOutput)
			if err != nil {
				return fmt.Errorf("input %d: invalid pseudo-output commitment: %w", i, err)
			}
			pseudoSum = sum
		}
	}

	// Sum output commitments
	var outputSum [32]byte
	for i, output := range tx.Outputs {
		if i == 0 {
			outputSum = output.Commitment
		} else {
			sum, err := CommitmentAdd(outputSum, output.Commitment)
			if err != nil {
				return fmt.Errorf("output %d: invalid commitment point: %w", i, err)
			}
			outputSum = sum
		}
	}

	// Add fee commitment to outputs (fee is public, blinding = 0)
	feeCommitment, err := CreateFeeCommitment(tx.Fee)
	if err != nil {
		return fmt.Errorf("failed to create fee commitment: %w", err)
	}

	outputPlusFee, err := CommitmentAdd(outputSum, feeCommitment)
	if err != nil {
		return fmt.Errorf("failed to add fee commitment: %w", err)
	}

	// Verify: sum(pseudo_outputs) - sum(outputs) - fee*G = 0 (identity)
	// Equivalent to: sum(pseudo_outputs) = sum(outputs) + fee*G
	diff, err := CommitmentSub(pseudoSum, outputPlusFee)
	if err != nil {
		return fmt.Errorf("failed to compute balance difference: %w", err)
	}

	if !CommitmentIsZero(diff) {
		return fmt.Errorf("commitment balance failed: sum(pseudo_outputs) != sum(outputs) + fee")
	}

	return nil
}

func validateCoinbase(tx *Transaction, skipCrypto bool) error {
	if len(tx.Inputs) != 0 {
		return fmt.Errorf("coinbase must have no inputs")
	}
	if tx.Fee != 0 {
		return fmt.Errorf("coinbase fee must be zero")
	}
	if len(tx.Outputs) != 1 {
		return fmt.Errorf("coinbase must have exactly one output")
	}

	// Same memo invariant as non-coinbase transactions.
	if tx.Outputs[0].EncryptedMemo == ([params.MemoSize]byte{}) {
		return fmt.Errorf("coinbase output 0: encrypted memo must not be all-zero")
	}

	// Verify range proofs on outputs. Skipped below a trusted checkpoint during
	// fast sync (see skipCrypto): one coinbase range proof is verified on every
	// single block, so this is the largest single contributor to sync time.
	if !skipCrypto {
		for i, output := range tx.Outputs {
			rangeProof := &RangeProof{Proof: output.RangeProof}
			if err := VerifyRangeProof(output.Commitment, rangeProof); err != nil {
				return fmt.Errorf("coinbase output %d: invalid range proof: %w", i, err)
			}
		}
	}

	return nil
}

func deriveCoinbaseConsensusBlinding(txPublicKey [32]byte, blockHeight uint64, outputIndex int) [32]byte {
	var heightBuf [8]byte
	binary.LittleEndian.PutUint64(heightBuf[:], blockHeight)
	var indexBuf [4]byte
	binary.LittleEndian.PutUint32(indexBuf[:], uint32(outputIndex))
	const tag = "blocknet_coinbase_consensus_blinding"
	b := make([]byte, 0, len(tag)+len(txPublicKey)+len(heightBuf)+len(indexBuf))
	b = append(b, tag...)
	b = append(b, txPublicKey[:]...)
	b = append(b, heightBuf[:]...)
	b = append(b, indexBuf[:]...)
	return sha3.Sum256(b)
}

// ============================================================================
// Coinbase Transaction
// ============================================================================

// CoinbaseResult contains the coinbase transaction and the data needed to spend it
type CoinbaseResult struct {
	Tx       *Transaction
	Amount   uint64
	Blinding [32]byte
	TxPubKey [32]byte // Needed for deriving one-time private key
}

// CreateCoinbase creates a coinbase (mining reward) transaction.
// The output commitment blinding is deterministic from public data so
// validators can enforce amount consensus without access to wallet secrets.
func CreateCoinbase(recipientSpendPub, recipientViewPub [32]byte, reward uint64, blockHeight uint64) (*CoinbaseResult, error) {
	// Derive stealth address for reward
	stealthOut, err := DeriveStealthAddress(recipientSpendPub, recipientViewPub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive stealth address: %w", err)
	}

	blinding := deriveCoinbaseConsensusBlinding(stealthOut.TxPubKey, blockHeight, 0)

	// Create commitment with derived blinding (so scanner can reproduce it)
	commitment, err := CreatePedersenCommitmentWithBlinding(reward, blinding)
	if err != nil {
		return nil, fmt.Errorf("failed to create commitment: %w", err)
	}

	// Create range proof
	rangeProof, err := CreateRangeProof(reward, blinding)
	if err != nil {
		return nil, fmt.Errorf("failed to create range proof: %w", err)
	}

	// Encrypt with the same consensus blinding so wallet scanners can recover
	// coinbase amounts with the deterministic consensus derivation.
	encryptedAmount := EncryptAmount(reward, blinding, 0)
	encryptedMemo, err := wallet.EncryptMemo(nil, blinding, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt coinbase memo: %w", err)
	}

	tx := &Transaction{
		Version:     1,
		Inputs:      []TxInput{},
		TxPublicKey: stealthOut.TxPubKey,
		Outputs: []TxOutput{
			{
				Commitment:      commitment,
				PublicKey:       stealthOut.OnetimePubKey,
				RangeProof:      rangeProof.Proof,
				EncryptedAmount: encryptedAmount,
				EncryptedMemo:   encryptedMemo,
			},
		},
		Fee: 0,
	}

	return &CoinbaseResult{
		Tx:       tx,
		Amount:   reward,
		Blinding: blinding,
		TxPubKey: stealthOut.TxPubKey,
	}, nil
}

// ============================================================================
// Apply Transaction to UTXO Set
// ============================================================================

// ApplyTransaction applies a validated transaction to the UTXO set
func (s *UTXOSet) ApplyTransaction(tx *Transaction, blockHeight uint64) error {
	txID, err := tx.TxID()
	if err != nil {
		return fmt.Errorf("failed to get tx ID: %w", err)
	}

	// Mark key images as spent
	for _, input := range tx.Inputs {
		if err := s.MarkSpent(input.KeyImage); err != nil {
			return err
		}
	}

	// Add new outputs to UTXO set
	for i, output := range tx.Outputs {
		s.Add(txID, uint32(i), output, blockHeight)
	}

	return nil
}

// UnapplyTransaction reverses the UTXO changes from a transaction
// Used during chain reorganization
func (s *UTXOSet) UnapplyTransaction(tx *Transaction) error {
	txID, err := tx.TxID()
	if err != nil {
		return fmt.Errorf("failed to get tx ID: %w", err)
	}

	// Remove outputs that were added
	for i, output := range tx.Outputs {
		s.Remove(txID, uint32(i), output.PublicKey)
	}

	// Unmark key images (they're no longer spent)
	for _, input := range tx.Inputs {
		s.UnmarkSpent(input.KeyImage)
	}

	return nil
}

// Remove removes a UTXO from the set (used during reorg)
func (s *UTXOSet) Remove(txID [32]byte, index uint32, pubKey [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%x:%d", txID, index)
	delete(s.utxos, key)

	// Remove from pubkey index
	pkHex := hex.EncodeToString(pubKey[:])
	utxos := s.byPubKey[pkHex]
	filtered := make([]*UTXO, 0, len(utxos))
	for _, u := range utxos {
		if u.TxID != txID || u.OutputIndex != index {
			filtered = append(filtered, u)
		}
	}
	s.byPubKey[pkHex] = filtered
}

// UnmarkSpent removes a key image from the spent set (used during reorg)
func (s *UTXOSet) UnmarkSpent(keyImage [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hex.EncodeToString(keyImage[:])
	delete(s.keyImages, key)
}

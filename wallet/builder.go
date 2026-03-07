package wallet

import (
	"crypto/sha3"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"time"

	"blocknet/protocol/params"
)

// AddU64 returns a + b and true if the result does not overflow uint64.
func AddU64(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	return sum, carry == 0
}

func addU64(a, b uint64) (uint64, bool) { return AddU64(a, b) }

// Recipient represents a transaction recipient
type Recipient struct {
	SpendPubKey [32]byte
	ViewPubKey  [32]byte
	Amount      uint64
	Memo        []byte
}

// TransferResult contains the result of building a transaction
type TransferResult struct {
	TxData       []byte         // Serialized transaction
	TxID         [32]byte       // Transaction ID
	TxPrivKey    [32]byte       // Transaction private key (for sender-side shared secret derivation)
	SpentOutputs []*OwnedOutput // Outputs that were spent
	InputLease   uint64         // Lease id holding reserved inputs (cleared on MarkSpent / TTL / ReleaseInputLease)
	Fee          uint64         // Fee paid
	Change       uint64         // Change returned
}

// TransferConfig holds dependencies for transaction building
type TransferConfig struct {
	// Ring member selection
	SelectRingMembers func(realPubKey, realCommitment [32]byte) (keys, commitments [][32]byte, secretIndex int, err error)

	// Cryptographic operations
	CreateCommitment func(amount uint64, blinding [32]byte) [32]byte
	CreateRangeProof func(amount uint64, blinding [32]byte) ([]byte, error)
	SignRingCT       func(
		ringKeys, ringCommitments [][32]byte,
		secretIndex int,
		privateKey, realBlinding [32]byte,
		pseudoCommitment, pseudoBlinding [32]byte,
		message []byte,
	) (signature []byte, keyImage [32]byte, err error)
	GenerateBlinding func() [32]byte
	ComputeTxID      func(txData []byte) ([32]byte, error)

	// Scalar arithmetic for blinding factors
	BlindingAdd func(a, b [32]byte) ([32]byte, error)
	BlindingSub func(a, b [32]byte) ([32]byte, error)

	// Stealth address derivation
	DeriveStealthAddress func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error)

	// Stealth address derivation with a caller-provided tx private key
	DeriveStealthAddressWithKey func(spendPub, viewPub, txPriv [32]byte) (txPub, oneTimePub [32]byte, err error)

	// Deterministic tx key derivation from view privkey + key images
	DeriveDeterministicTxKey func(viewPriv [32]byte, keyImages [][32]byte) ([32]byte, error)

	// Key image generation for deterministic tx key derivation
	GenerateKeyImage func(privKey [32]byte) ([32]byte, error)

	// ECDH shared secret derivation (sender side): H(txPriv * viewPub)
	DeriveSharedSecret func(txPriv, viewPub [32]byte) ([32]byte, error)

	// Indexed shared secret derivation (sender side): H(txPriv * viewPub || outputIndex)
	DeriveSharedSecretIndexed func(txPriv, viewPub [32]byte, outputIndex uint32) ([32]byte, error)

	// Point operations for deriving one-time keys from an existing txPriv
	ScalarToPoint func(scalar [32]byte) ([32]byte, error) // scalar * G
	PointAdd      func(p1, p2 [32]byte) ([32]byte, error) // p1 + p2

	// Constants
	RingSize   int
	MinFee     uint64
	FeePerByte uint64
}

// Builder constructs transactions
type Builder struct {
	wallet *Wallet
	config TransferConfig
}

// deriveDeterministicR computes key images for the selected inputs and derives
// a deterministic tx private key r and public key R = r * G. Falls back to
// random r via DeriveStealthAddress if deterministic callbacks are not configured.
func (b *Builder) deriveDeterministicR(inputs []*OwnedOutput) (txPriv, txPub [32]byte, err error) {
	if b.config.DeriveDeterministicTxKey == nil || b.config.GenerateKeyImage == nil || b.config.ScalarToPoint == nil {
		keys := b.wallet.Keys()
		rPriv, rPub, _, fallbackErr := b.config.DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
		return rPriv, rPub, fallbackErr
	}

	keyImages := make([][32]byte, len(inputs))
	for i, inp := range inputs {
		ki, kiErr := b.config.GenerateKeyImage(inp.OneTimePrivKey)
		if kiErr != nil {
			return txPriv, txPub, fmt.Errorf("key image for input %d: %w", i, kiErr)
		}
		keyImages[i] = ki
	}

	viewPriv := b.wallet.Keys().ViewPrivKey
	txPriv, err = b.config.DeriveDeterministicTxKey(viewPriv, keyImages)
	if err != nil {
		return txPriv, txPub, fmt.Errorf("deterministic tx key: %w", err)
	}

	txPub, err = b.config.ScalarToPoint(txPriv)
	if err != nil {
		return txPriv, txPub, fmt.Errorf("scalar to point for R: %w", err)
	}
	return txPriv, txPub, nil
}

// NewBuilder creates a transaction builder
func NewBuilder(w *Wallet, cfg TransferConfig) *Builder {
	return &Builder{
		wallet: w,
		config: cfg,
	}
}

// Transfer creates a transaction sending to recipients
func (b *Builder) Transfer(recipients []Recipient, feeRate uint64, currentHeight uint64) (res *TransferResult, retErr error) {
	if len(recipients) == 0 {
		return nil, errors.New("no recipients specified")
	}

	// Calculate total amount needed
	var totalSend uint64
	for _, r := range recipients {
		var ok bool
		totalSend, ok = addU64(totalSend, r.Amount)
		if !ok {
			return nil, errors.New("recipient amount sum overflows uint64")
		}
	}

	// Estimate fee before building by using a conservative size model and iterating
	// through input selection (fee affects how many inputs we need).
	fee := b.config.MinFee
	var inputs []*OwnedOutput
	var err error
	var lease uint64
	defer func() {
		if retErr != nil && lease != 0 {
			b.wallet.ReleaseInputLease(lease)
		}
	}()
	for i := 0; i < 4; i++ {
		need, ok := addU64(totalSend, fee)
		if !ok {
			return nil, errors.New("send amount + fee overflows uint64")
		}

		if lease != 0 {
			b.wallet.ReleaseInputLease(lease)
			lease = 0
		}

		lease, inputs, err = b.wallet.ReserveMatureInputs(currentHeight, need, 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("insufficient funds: %w", err)
		}

		var totalInput uint64
		for _, inp := range inputs {
			totalInput, ok = addU64(totalInput, inp.Amount)
			if !ok {
				return nil, errors.New("input amount sum overflows uint64")
			}
		}
		if totalInput < need {
			return nil, fmt.Errorf("insufficient funds after selection: have %d need %d", totalInput, need)
		}

		change := totalInput - totalSend - fee
		outputCount := len(recipients)
		if change > 0 {
			outputCount++
		}

		estimatedSize := estimateTxSizeBytes(len(inputs), outputCount, b.config.RingSize)
		requiredFee := max(b.config.MinFee, uint64(estimatedSize)*feeRate)
		if requiredFee <= fee {
			break
		}
		fee = requiredFee
	}

	return b.buildTransaction(inputs, recipients, totalSend, fee, lease, 1, currentHeight)
}

// TransferWithInputs builds a transaction using caller-specified inputs (coin control).
// The inputs must already be reserved via ReserveSpecificInputs; the lease is released
// on build failure. changeSplit controls how many outputs the change is distributed
// across (1-4, clamped internally).
func (b *Builder) TransferWithInputs(inputs []*OwnedOutput, lease uint64, recipients []Recipient, feeRate uint64, changeSplit int, currentHeight uint64) (res *TransferResult, retErr error) {
	defer func() {
		if retErr != nil && lease != 0 {
			b.wallet.ReleaseInputLease(lease)
		}
	}()

	if len(recipients) == 0 {
		return nil, errors.New("no recipients specified")
	}
	if len(inputs) == 0 {
		return nil, errors.New("no inputs specified")
	}

	var totalSend uint64
	for _, r := range recipients {
		var ok bool
		totalSend, ok = addU64(totalSend, r.Amount)
		if !ok {
			return nil, errors.New("recipient amount sum overflows uint64")
		}
	}

	if changeSplit < 1 {
		changeSplit = 1
	}

	// With fixed inputs the fee is computed once (no iteration). Use
	// changeSplit for conservative size estimate; if change ends up zero
	// the slightly higher fee is simply absorbed.
	outputCount := len(recipients) + changeSplit
	estimatedSize := estimateTxSizeBytes(len(inputs), outputCount, b.config.RingSize)
	fee := max(b.config.MinFee, uint64(estimatedSize)*feeRate)

	return b.buildTransaction(inputs, recipients, totalSend, fee, lease, changeSplit, currentHeight)
}

// buildTransaction is the shared core that constructs outputs, ring signatures,
// and serializes the transaction. Both Transfer and TransferWithInputs delegate here
// after input selection and fee computation. changeSplit controls how many outputs
// the change is distributed across (clamped to min(changeSplit, change) and >= 1).
func (b *Builder) buildTransaction(inputs []*OwnedOutput, recipients []Recipient, totalSend, fee, lease uint64, changeSplit int, currentHeight uint64) (*TransferResult, error) {
	var totalInput uint64
	for _, inp := range inputs {
		var ok bool
		totalInput, ok = addU64(totalInput, inp.Amount)
		if !ok {
			return nil, errors.New("input amount sum overflows uint64")
		}
	}
	need, ok := addU64(totalSend, fee)
	if !ok {
		return nil, errors.New("send amount + fee overflows uint64")
	}
	if totalInput < need {
		return nil, fmt.Errorf("insufficient funds after fee adjustment: have %d need %d", totalInput, need)
	}
	change := totalInput - totalSend - fee

	if changeSplit < 1 {
		changeSplit = 1
	}

	useIndexed := currentHeight >= params.IndexedDerivationHeight && b.config.DeriveSharedSecretIndexed != nil

	outputs := make([]outputData, 0, len(recipients)+changeSplit)

	txPrivKey, txPubKey, err := b.deriveDeterministicR(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to derive tx key: %w", err)
	}

	var allBlindings [][32]byte

	// deriveSecret picks the right derivation based on activation height.
	deriveSecret := func(viewPub [32]byte, outputIndex int) ([32]byte, error) {
		if useIndexed {
			return b.config.DeriveSharedSecretIndexed(txPrivKey, viewPub, uint32(outputIndex))
		}
		return b.config.DeriveSharedSecret(txPrivKey, viewPub)
	}

	for i, r := range recipients {
		sharedSecret, err := deriveSecret(r.ViewPubKey, i)
		if err != nil {
			return nil, fmt.Errorf("failed to derive shared secret for recipient %d: %w", i, err)
		}

		sharedPoint, err := b.config.ScalarToPoint(sharedSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to derive shared point for recipient %d: %w", i, err)
		}
		oneTimePub, err := b.config.PointAdd(sharedPoint, r.SpendPubKey)
		if err != nil {
			return nil, fmt.Errorf("failed to derive one-time key for recipient %d: %w", i, err)
		}

		blinding := DeriveBlinding(sharedSecret, i)
		commitment := b.config.CreateCommitment(r.Amount, blinding)
		rangeProof, err := b.config.CreateRangeProof(r.Amount, blinding)
		if err != nil {
			return nil, fmt.Errorf("failed to create range proof: %w", err)
		}

		encryptedAmount := encryptAmount(r.Amount, blinding, i)
		encryptedMemo, err := EncryptMemo(r.Memo, sharedSecret, i)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt memo for recipient %d: %w", i, err)
		}

		outputs = append(outputs, outputData{
			pubKey:          oneTimePub,
			commitment:      commitment,
			rangeProof:      rangeProof,
			encryptedAmount: encryptedAmount,
			encryptedMemo:   encryptedMemo,
			blinding:        blinding,
			amount:          r.Amount,
		})
		allBlindings = append(allBlindings, blinding)
	}

	if change > 0 {
		keys := b.wallet.Keys()
		n := changeSplit
		if uint64(n) > change {
			n = int(change)
		}
		perOutput := change / uint64(n)
		remainder := change % uint64(n)

		for c := 0; c < n; c++ {
			outputIndex := len(outputs)
			amt := perOutput
			if c == 0 {
				amt += remainder
			}

			changeSecret, err := deriveSecret(keys.ViewPubKey, outputIndex)
			if err != nil {
				return nil, fmt.Errorf("failed to derive shared secret for change output %d: %w", c, err)
			}

			sharedPoint, err := b.config.ScalarToPoint(changeSecret)
			if err != nil {
				return nil, fmt.Errorf("failed to derive shared point for change output %d: %w", c, err)
			}
			oneTimePub, err := b.config.PointAdd(sharedPoint, keys.SpendPubKey)
			if err != nil {
				return nil, fmt.Errorf("failed to derive one-time key for change output %d: %w", c, err)
			}

			blinding := DeriveBlinding(changeSecret, outputIndex)
			commitment := b.config.CreateCommitment(amt, blinding)
			rangeProof, err := b.config.CreateRangeProof(amt, blinding)
			if err != nil {
				return nil, fmt.Errorf("failed to create range proof for change output %d: %w", c, err)
			}

			encryptedAmount := encryptAmount(amt, blinding, outputIndex)
			encryptedMemo, err := EncryptMemo(nil, changeSecret, outputIndex)
			if err != nil {
				return nil, fmt.Errorf("failed to encrypt memo for change output %d: %w", c, err)
			}

			outputs = append(outputs, outputData{
				pubKey:          oneTimePub,
				commitment:      commitment,
				rangeProof:      rangeProof,
				encryptedAmount: encryptedAmount,
				encryptedMemo:   encryptedMemo,
				blinding:        blinding,
				amount:          amt,
			})
			allBlindings = append(allBlindings, blinding)
		}
	}

	totalOutputBlinding, err := b.sumBlindings(allBlindings)
	if err != nil {
		return nil, fmt.Errorf("failed to sum output blindings: %w", err)
	}

	inputsData := make([]inputData, len(inputs))

	pseudoBlindings, err := b.distributeBlindings(totalOutputBlinding, len(inputs))
	if err != nil {
		return nil, fmt.Errorf("failed to distribute blindings: %w", err)
	}

	txPrefix := serializeTxPrefix(txPubKey, len(inputs), outputs, fee)
	txPrefixHash := sha3.Sum256(txPrefix)

	for i, inp := range inputs {
		ringKeys, ringCommitments, secretIndex, err := b.config.SelectRingMembers(inp.OneTimePubKey, inp.Commitment)
		if err != nil {
			return nil, fmt.Errorf("failed to select ring members: %w", err)
		}

		pseudoCommitment := b.config.CreateCommitment(inp.Amount, pseudoBlindings[i])

		sig, keyImage, err := b.config.SignRingCT(
			ringKeys, ringCommitments,
			secretIndex,
			inp.OneTimePrivKey, inp.Blinding,
			pseudoCommitment, pseudoBlindings[i],
			txPrefixHash[:],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign input %d: %w", i, err)
		}

		inputsData[i] = inputData{
			keyImage:        keyImage,
			ringMembers:     ringKeys,
			ringCommitments: ringCommitments,
			pseudoOutput:    pseudoCommitment,
			signature:       sig,
		}
	}

	txData := serializeTx(txPubKey, inputsData, outputs, fee)
	txID, err := b.config.ComputeTxID(txData)
	if err != nil {
		return nil, fmt.Errorf("failed to compute tx ID: %w", err)
	}

	return &TransferResult{
		TxData:       txData,
		TxID:         txID,
		TxPrivKey:    txPrivKey,
		SpentOutputs: inputs,
		InputLease:   lease,
		Fee:          fee,
		Change:       change,
	}, nil
}

// TransferAll sweeps all spendable outputs to a single recipient, minus the fee.
func (b *Builder) TransferAll(recipient Recipient, feeRate uint64, currentHeight uint64) (res *TransferResult, retErr error) {
	var lease uint64
	defer func() {
		if retErr != nil && lease != 0 {
			b.wallet.ReleaseInputLease(lease)
		}
	}()

	var inputs []*OwnedOutput
	var err error
	lease, inputs, err = b.wallet.ReserveAllMatureInputs(currentHeight, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("no spendable outputs: %w", err)
	}

	if len(inputs) > 256 {
		return nil, fmt.Errorf("too many outputs to sweep in one transaction (%d, max 256)", len(inputs))
	}

	var totalInput uint64
	for _, inp := range inputs {
		var ok bool
		totalInput, ok = addU64(totalInput, inp.Amount)
		if !ok {
			return nil, errors.New("input amount sum overflows uint64")
		}
	}

	estimatedSize := estimateTxSizeBytes(len(inputs), 1, b.config.RingSize)
	fee := max(b.config.MinFee, uint64(estimatedSize)*feeRate)

	if totalInput <= fee {
		return nil, fmt.Errorf("balance too small to cover fee: have %d, fee %d", totalInput, fee)
	}

	sendAmount := totalInput - fee

	txPrivKey, txPubKey, err := b.deriveDeterministicR(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to derive tx key: %w", err)
	}

	useIndexed := currentHeight >= params.IndexedDerivationHeight && b.config.DeriveSharedSecretIndexed != nil

	var sharedSecret [32]byte
	if useIndexed {
		sharedSecret, err = b.config.DeriveSharedSecretIndexed(txPrivKey, recipient.ViewPubKey, 0)
	} else {
		sharedSecret, err = b.config.DeriveSharedSecret(txPrivKey, recipient.ViewPubKey)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	sharedPoint, err := b.config.ScalarToPoint(sharedSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared point: %w", err)
	}
	oneTimePub, err := b.config.PointAdd(sharedPoint, recipient.SpendPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive one-time key: %w", err)
	}

	blinding := DeriveBlinding(sharedSecret, 0)
	commitment := b.config.CreateCommitment(sendAmount, blinding)
	rangeProof, err := b.config.CreateRangeProof(sendAmount, blinding)
	if err != nil {
		return nil, fmt.Errorf("failed to create range proof: %w", err)
	}

	encryptedAmount := encryptAmount(sendAmount, blinding, 0)
	encryptedMemo, err := EncryptMemo(recipient.Memo, sharedSecret, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt memo: %w", err)
	}

	outputs := []outputData{{
		pubKey:          oneTimePub,
		commitment:      commitment,
		rangeProof:      rangeProof,
		encryptedAmount: encryptedAmount,
		encryptedMemo:   encryptedMemo,
		blinding:        blinding,
		amount:          sendAmount,
	}}

	totalOutputBlinding, err := b.sumBlindings([][32]byte{blinding})
	if err != nil {
		return nil, fmt.Errorf("failed to sum output blindings: %w", err)
	}

	inputsData := make([]inputData, len(inputs))
	pseudoBlindings, err := b.distributeBlindings(totalOutputBlinding, len(inputs))
	if err != nil {
		return nil, fmt.Errorf("failed to distribute blindings: %w", err)
	}

	txPrefix := serializeTxPrefix(txPubKey, len(inputs), outputs, fee)
	txPrefixHash := sha3.Sum256(txPrefix)

	for i, inp := range inputs {
		ringKeys, ringCommitments, secretIndex, err := b.config.SelectRingMembers(inp.OneTimePubKey, inp.Commitment)
		if err != nil {
			return nil, fmt.Errorf("failed to select ring members: %w", err)
		}

		pseudoCommitment := b.config.CreateCommitment(inp.Amount, pseudoBlindings[i])

		sig, keyImage, err := b.config.SignRingCT(
			ringKeys, ringCommitments,
			secretIndex,
			inp.OneTimePrivKey, inp.Blinding,
			pseudoCommitment, pseudoBlindings[i],
			txPrefixHash[:],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign input %d: %w", i, err)
		}

		inputsData[i] = inputData{
			keyImage:        keyImage,
			ringMembers:     ringKeys,
			ringCommitments: ringCommitments,
			pseudoOutput:    pseudoCommitment,
			signature:       sig,
		}
	}

	txData := serializeTx(txPubKey, inputsData, outputs, fee)
	txID, err := b.config.ComputeTxID(txData)
	if err != nil {
		return nil, fmt.Errorf("failed to compute tx ID: %w", err)
	}

	return &TransferResult{
		TxData:       txData,
		TxID:         txID,
		TxPrivKey:    txPrivKey,
		SpentOutputs: inputs,
		InputLease:   lease,
		Fee:          fee,
		Change:       0,
	}, nil
}

// EstimateTxSizeBytes returns a conservative byte-size estimate for a transaction.
func EstimateTxSizeBytes(inputCount, outputCount, ringSize int) int {
	return estimateTxSizeBytes(inputCount, outputCount, ringSize)
}

func estimateTxSizeBytes(inputCount, outputCount, ringSize int) int {
	// Prefix: version (1) + txPubKey (32) + inputCount (4) + outputCount (4) + fee (8)
	size := 1 + 32 + 4 + 4 + 8

	// Conservative fixed upper bounds:
	// - Range proofs are usually smaller than this, but we over-estimate to avoid fee underpayment.
	const maxRangeProofBytes = 1024

	// Each output: pubkey + commitment + encrypted_amount + encrypted_memo + range_proof_len + range_proof
	size += outputCount * (32 + 32 + 8 + MemoSize + 4 + maxRangeProofBytes)

	// Signature payload size is fixed for RingCT given ringSize:
	// 32 + n*32 + n*32 + 32 + 32 = 96 + 64*n
	ringSigBytes := 96 + 64*ringSize

	// Each input: key_image + pseudo_output + ring_size + ring_members + ring_commitments + sig_len + signature
	size += inputCount * (32 + 32 + 4 + ringSize*32 + ringSize*32 + 4 + ringSigBytes)
	return size
}

// sumBlindings adds blinding factors using proper scalar arithmetic
func (b *Builder) sumBlindings(blindings [][32]byte) ([32]byte, error) {
	if len(blindings) == 0 {
		return [32]byte{}, nil
	}
	if len(blindings) == 1 {
		return blindings[0], nil
	}

	sum := blindings[0]
	for i := 1; i < len(blindings); i++ {
		var err error
		sum, err = b.config.BlindingAdd(sum, blindings[i])
		if err != nil {
			return [32]byte{}, fmt.Errorf("scalar add failed at index %d: %w", i, err)
		}
	}
	return sum, nil
}

// distributeBlindings creates pseudo-output blindings that sum to target
func (b *Builder) distributeBlindings(target [32]byte, count int) ([][32]byte, error) {
	if count == 0 {
		return nil, nil
	}
	if count == 1 {
		return [][32]byte{target}, nil
	}

	// Generate random blindings for first n-1, compute last to balance
	result := make([][32]byte, count)
	sum := [32]byte{} // Start with zero

	for i := 0; i < count-1; i++ {
		result[i] = b.config.GenerateBlinding()
		var err error
		sum, err = b.config.BlindingAdd(sum, result[i])
		if err != nil {
			return nil, fmt.Errorf("scalar add failed: %w", err)
		}
	}

	// Last blinding = target - sum
	var err error
	result[count-1], err = b.config.BlindingSub(target, sum)
	if err != nil {
		return nil, fmt.Errorf("scalar sub failed: %w", err)
	}

	return result, nil
}

// encryptAmount encrypts an amount using the blinding factor as shared secret
// Format: amount XOR first 8 bytes of Hash("amount" || blinding || output_index)
func encryptAmount(amount uint64, blinding [32]byte, outputIndex int) [8]byte {
	mask := amountMask(blinding, outputIndex)

	var amountBytes [8]byte
	binary.LittleEndian.PutUint64(amountBytes[:], amount)

	var encrypted [8]byte
	for i := 0; i < 8; i++ {
		encrypted[i] = amountBytes[i] ^ mask[i]
	}
	return encrypted
}

// DecryptAmount decrypts an encrypted amount using the blinding factor
func DecryptAmount(encrypted [8]byte, blinding [32]byte, outputIndex int) uint64 {
	mask := amountMask(blinding, outputIndex)

	var amountBytes [8]byte
	for i := range 8 {
		amountBytes[i] = encrypted[i] ^ mask[i]
	}
	return binary.LittleEndian.Uint64(amountBytes[:])
}

func amountMask(blinding [32]byte, outputIndex int) [32]byte {
	const tag = "blocknet_amount"
	var outputIndexBytes [4]byte
	binary.LittleEndian.PutUint32(outputIndexBytes[:], uint32(outputIndex))

	// sha3.Sum256 never fails; build explicit input bytes to avoid unchecked Write() errors.
	b := make([]byte, 0, len(tag)+32+4)
	b = append(b, tag...)
	b = append(b, blinding[:]...)
	b = append(b, outputIndexBytes[:]...)
	return sha3.Sum256(b)
}

// serializeTxPrefix creates the transaction prefix (everything except signatures)
func serializeTxPrefix(txPubKey [32]byte, inputCount int, outputs []outputData, fee uint64) []byte {
	// Calculate size
	size := 1 + // version
		32 + // tx public key
		4 + // input count
		4 + // output count
		8 // fee

	// Each output: pubkey + commitment + encrypted_amount + encrypted_memo + range_proof_len + range_proof
	for _, out := range outputs {
		size += 32 + 32 + 8 + MemoSize + 4 + len(out.rangeProof)
	}

	buf := make([]byte, size)
	offset := 0

	// Version
	buf[offset] = 1
	offset++

	// Tx public key
	copy(buf[offset:], txPubKey[:])
	offset += 32

	// Input count
	binary.LittleEndian.PutUint32(buf[offset:], uint32(inputCount))
	offset += 4

	// Output count
	binary.LittleEndian.PutUint32(buf[offset:], uint32(len(outputs)))
	offset += 4

	// Fee
	binary.LittleEndian.PutUint64(buf[offset:], fee)
	offset += 8

	// Outputs
	for _, out := range outputs {
		copy(buf[offset:], out.pubKey[:])
		offset += 32

		copy(buf[offset:], out.commitment[:])
		offset += 32

		copy(buf[offset:], out.encryptedAmount[:])
		offset += 8

		copy(buf[offset:], out.encryptedMemo[:])
		offset += MemoSize

		binary.LittleEndian.PutUint32(buf[offset:], uint32(len(out.rangeProof)))
		offset += 4

		copy(buf[offset:], out.rangeProof)
		offset += len(out.rangeProof)
	}

	return buf
}

// serializeTx creates the full transaction bytes
func serializeTx(txPubKey [32]byte, inputs []inputData, outputs []outputData, fee uint64) []byte {
	// Start with prefix
	prefix := serializeTxPrefix(txPubKey, len(inputs), outputs, fee)

	// Calculate input section size
	inputSize := 0
	for _, inp := range inputs {
		// key_image + pseudo_output + ring_size + ring_members + ring_commitments + sig_len + signature
		inputSize += 32 + 32 + 4 + len(inp.ringMembers)*32 + len(inp.ringCommitments)*32 + 4 + len(inp.signature)
	}

	buf := make([]byte, len(prefix)+inputSize)
	offset := 0

	// Copy prefix
	copy(buf[offset:], prefix)
	offset += len(prefix)

	// Serialize inputs
	for _, inp := range inputs {
		// Key image
		copy(buf[offset:], inp.keyImage[:])
		offset += 32

		// Pseudo-output commitment
		copy(buf[offset:], inp.pseudoOutput[:])
		offset += 32

		// Ring size
		binary.LittleEndian.PutUint32(buf[offset:], uint32(len(inp.ringMembers)))
		offset += 4

		// Ring member public keys
		for _, pk := range inp.ringMembers {
			copy(buf[offset:], pk[:])
			offset += 32
		}

		// Ring member commitments
		for _, c := range inp.ringCommitments {
			copy(buf[offset:], c[:])
			offset += 32
		}

		// Signature length
		binary.LittleEndian.PutUint32(buf[offset:], uint32(len(inp.signature)))
		offset += 4

		// Signature
		copy(buf[offset:], inp.signature)
		offset += len(inp.signature)
	}

	return buf
}

type inputData struct {
	keyImage        [32]byte
	ringMembers     [][32]byte
	ringCommitments [][32]byte
	pseudoOutput    [32]byte
	signature       []byte
}

type outputData struct {
	pubKey          [32]byte
	commitment      [32]byte
	rangeProof      []byte
	encryptedAmount [8]byte
	encryptedMemo   [MemoSize]byte
	blinding        [32]byte // Not serialized, just for building
	amount          uint64   // Not serialized, just for building
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

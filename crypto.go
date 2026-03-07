package main

/*
#cgo LDFLAGS: ${SRCDIR}/crypto-rs/target/release/libblocknet_crypto.a -lm
#cgo linux,!android LDFLAGS: -ldl -lpthread
#cgo android LDFLAGS: -ldl -llog
#cgo darwin LDFLAGS: -ldl -lpthread -framework Security
#cgo windows LDFLAGS: -lws2_32 -luserenv -lbcrypt -lntdll
#include "crypto-rs/blocknet_crypto.h"
*/
import "C"
import (
	"crypto/sha3"
	"fmt"
	"unsafe"
)

// RustKeypair represents an ed25519 keypair from Rust
type RustKeypair struct {
	PrivateKey [32]byte
	PublicKey  [32]byte
}

// GenerateKeypairRust generates a new ed25519 keypair using Rust crypto
func GenerateKeypairRust() (*RustKeypair, error) {
	var output [64]byte
	result := C.blocknet_generate_keypair((*C.uint8_t)(unsafe.Pointer(&output[0])))

	if result != 0 {
		return nil, fmt.Errorf("failed to generate keypair")
	}

	keypair := &RustKeypair{}
	copy(keypair.PrivateKey[:], output[:32])
	copy(keypair.PublicKey[:], output[32:])

	return keypair, nil
}

// SignRust signs a message using Rust crypto
func SignRust(privateKey []byte, message []byte) ([]byte, error) {
	if len(privateKey) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes")
	}

	signature := make([]byte, 64)

	result := C.blocknet_sign(
		(*C.uint8_t)(unsafe.Pointer(&privateKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&signature[0])),
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to sign message")
	}

	return signature, nil
}

// VerifyRust verifies a signature using Rust crypto
func VerifyRust(publicKey []byte, message []byte, signature []byte) error {
	if len(publicKey) != 32 {
		return fmt.Errorf("public key must be 32 bytes")
	}
	if len(signature) != 64 {
		return fmt.Errorf("signature must be 64 bytes")
	}

	result := C.blocknet_verify(
		(*C.uint8_t)(unsafe.Pointer(&publicKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&signature[0])),
	)

	if result != 0 {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// SchnorrSign signs a message with a Ristretto private key (Schnorr over Ristretto255).
// This is compatible with the wallet's spend key and the public key encoded in stealth addresses.
func SchnorrSign(privateKey []byte, message []byte) ([]byte, error) {
	if len(privateKey) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes")
	}

	signature := make([]byte, 64)

	result := C.blocknet_schnorr_sign(
		(*C.uint8_t)(unsafe.Pointer(&privateKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&signature[0])),
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to sign message")
	}

	return signature, nil
}

// SchnorrVerify verifies a Schnorr signature against a Ristretto public key.
func SchnorrVerify(publicKey []byte, message []byte, signature []byte) error {
	if len(publicKey) != 32 {
		return fmt.Errorf("public key must be 32 bytes")
	}
	if len(signature) != 64 {
		return fmt.Errorf("signature must be 64 bytes")
	}

	result := C.blocknet_schnorr_verify(
		(*C.uint8_t)(unsafe.Pointer(&publicKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&signature[0])),
	)

	if result != 0 {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// ============================================================================
// Pedersen Commitments
// ============================================================================

// PedersenCommitment represents a commitment to a hidden value
type PedersenCommitment struct {
	Commitment [32]byte
	Blinding   [32]byte
	Value      uint64 // Keep this private/secret in real usage
}

// CreatePedersenCommitment creates a Pedersen commitment to a value
func CreatePedersenCommitment(value uint64) (*PedersenCommitment, error) {
	var blinding [32]byte
	var commitment [32]byte

	result := C.blocknet_pedersen_commit(
		C.uint64_t(value),
		(*C.uint8_t)(unsafe.Pointer(&blinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&commitment[0])),
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to create Pedersen commitment")
	}

	return &PedersenCommitment{
		Commitment: commitment,
		Blinding:   blinding,
		Value:      value,
	}, nil
}

// CreatePedersenCommitmentWithBlinding creates a commitment with a specific blinding factor
func CreatePedersenCommitmentWithBlinding(value uint64, blinding [32]byte) ([32]byte, error) {
	var commitment [32]byte

	ret := C.blocknet_pedersen_commit_with_blinding(
		C.uint64_t(value),
		(*C.uint8_t)(unsafe.Pointer(&blinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&commitment[0])),
	)

	if ret != 0 {
		return commitment, fmt.Errorf("failed to create commitment with blinding")
	}

	return commitment, nil
}

// Verify checks if the commitment opens to the stored value
func (pc *PedersenCommitment) Verify() error {
	result := C.blocknet_pedersen_verify(
		C.uint64_t(pc.Value),
		(*C.uint8_t)(unsafe.Pointer(&pc.Blinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&pc.Commitment[0])),
	)

	if result != 0 {
		return fmt.Errorf("pedersen commitment verification failed")
	}

	return nil
}

// CommitmentAdd adds two commitments: result = c1 + c2
func CommitmentAdd(c1, c2 [32]byte) ([32]byte, error) {
	var result [32]byte

	ret := C.blocknet_commitment_add(
		(*C.uint8_t)(unsafe.Pointer(&c1[0])),
		(*C.uint8_t)(unsafe.Pointer(&c2[0])),
		(*C.uint8_t)(unsafe.Pointer(&result[0])),
	)

	if ret != 0 {
		return result, fmt.Errorf("commitment add failed")
	}

	return result, nil
}

// CommitmentSub subtracts two commitments: result = c1 - c2
func CommitmentSub(c1, c2 [32]byte) ([32]byte, error) {
	var result [32]byte

	ret := C.blocknet_commitment_sub(
		(*C.uint8_t)(unsafe.Pointer(&c1[0])),
		(*C.uint8_t)(unsafe.Pointer(&c2[0])),
		(*C.uint8_t)(unsafe.Pointer(&result[0])),
	)

	if ret != 0 {
		return result, fmt.Errorf("commitment sub failed")
	}

	return result, nil
}

// CommitmentIsZero checks if a commitment is the identity (zero point)
func CommitmentIsZero(c [32]byte) bool {
	result := C.blocknet_commitment_is_zero((*C.uint8_t)(unsafe.Pointer(&c[0])))
	return result == 0
}

// CreateFeeCommitment creates a commitment to the fee (fee * G)
func CreateFeeCommitment(fee uint64) ([32]byte, error) {
	var result [32]byte

	ret := C.blocknet_fee_commitment(
		C.uint64_t(fee),
		(*C.uint8_t)(unsafe.Pointer(&result[0])),
	)

	if ret != 0 {
		return result, fmt.Errorf("fee commitment failed")
	}

	return result, nil
}

// BlindingAdd adds two blinding factors: result = b1 + b2
func BlindingAdd(b1, b2 [32]byte) ([32]byte, error) {
	var result [32]byte

	ret := C.blocknet_blinding_add(
		(*C.uint8_t)(unsafe.Pointer(&b1[0])),
		(*C.uint8_t)(unsafe.Pointer(&b2[0])),
		(*C.uint8_t)(unsafe.Pointer(&result[0])),
	)

	if ret != 0 {
		return result, fmt.Errorf("blinding add failed")
	}

	return result, nil
}

// BlindingSub subtracts blinding factors: result = b1 - b2
func BlindingSub(b1, b2 [32]byte) ([32]byte, error) {
	var result [32]byte

	ret := C.blocknet_blinding_sub(
		(*C.uint8_t)(unsafe.Pointer(&b1[0])),
		(*C.uint8_t)(unsafe.Pointer(&b2[0])),
		(*C.uint8_t)(unsafe.Pointer(&result[0])),
	)

	if ret != 0 {
		return result, fmt.Errorf("blinding sub failed")
	}

	return result, nil
}

// GenerateBlinding generates a random blinding factor
func GenerateBlinding() ([32]byte, error) {
	// Use the Pedersen commit function which generates a random blinding
	commit, err := CreatePedersenCommitment(0)
	if err != nil {
		return [32]byte{}, err
	}
	return commit.Blinding, nil
}

// ComputeTxHash computes SHA3-256 hash of transaction data
func ComputeTxHash(txData []byte) [32]byte {
	return sha3.Sum256(txData)
}

// ============================================================================
// Bulletproofs Range Proofs
// ============================================================================

// RangeProof represents a bulletproof range proof
type RangeProof struct {
	Proof []byte
}

// CreateRangeProof creates a bulletproof that value is in [0, 2^64)
func CreateRangeProof(value uint64, blinding [32]byte) (*RangeProof, error) {
	proofBuf := make([]byte, 1024) // Max proof size
	var proofLen C.size_t

	result := C.blocknet_range_proof_create(
		C.uint64_t(value),
		(*C.uint8_t)(unsafe.Pointer(&blinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&proofBuf[0])),
		&proofLen,
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to create range proof")
	}

	return &RangeProof{
		Proof: proofBuf[:proofLen],
	}, nil
}

// VerifyRangeProof verifies a bulletproof for a commitment
func VerifyRangeProof(commitment [32]byte, proof *RangeProof) error {
	if proof == nil {
		return fmt.Errorf("range proof is required")
	}
	if len(proof.Proof) == 0 {
		return fmt.Errorf("range proof must not be empty")
	}

	result := C.blocknet_range_proof_verify(
		(*C.uint8_t)(unsafe.Pointer(&commitment[0])),
		(*C.uint8_t)(unsafe.Pointer(&proof.Proof[0])),
		C.size_t(len(proof.Proof)),
	)

	if result != 0 {
		return fmt.Errorf("range proof verification failed")
	}

	return nil
}

// ============================================================================
// Hashing
// ============================================================================

// SHA256 computes SHA256 hash using Rust
func SHA256(data []byte) ([32]byte, error) {
	var hash [32]byte

	if len(data) == 0 {
		return hash, fmt.Errorf("cannot hash empty data")
	}

	result := C.blocknet_sha256(
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)),
		(*C.uint8_t)(unsafe.Pointer(&hash[0])),
	)

	if result != 0 {
		return hash, fmt.Errorf("failed to compute SHA256")
	}

	return hash, nil
}

// ============================================================================
// Stealth Addresses
// ============================================================================

// StealthKeys holds the spend and view keypairs for stealth addresses
type StealthKeys struct {
	SpendPrivKey [32]byte
	SpendPubKey  [32]byte
	ViewPrivKey  [32]byte
	ViewPubKey   [32]byte
}

// GenerateStealthKeys generates a new stealth keypair (spend + view)
func GenerateStealthKeys() (*StealthKeys, error) {
	var output [128]byte
	result := C.blocknet_stealth_keygen((*C.uint8_t)(unsafe.Pointer(&output[0])))

	if result != 0 {
		return nil, fmt.Errorf("failed to generate stealth keys")
	}

	keys := &StealthKeys{}
	copy(keys.SpendPrivKey[:], output[0:32])
	copy(keys.SpendPubKey[:], output[32:64])
	copy(keys.ViewPrivKey[:], output[64:96])
	copy(keys.ViewPubKey[:], output[96:128])

	return keys, nil
}

// StealthOutput represents a one-time stealth address output
type StealthOutput struct {
	TxPrivKey     [32]byte // Keep secret (sender only)
	TxPubKey      [32]byte // Include in transaction
	OnetimePubKey [32]byte // The one-time address
}

// DeriveStealthAddress creates a one-time address for a recipient (sender side)
func DeriveStealthAddress(spendPubKey, viewPubKey [32]byte) (*StealthOutput, error) {
	var txPriv, txPub, onetimePub [32]byte

	result := C.blocknet_stealth_derive_address(
		(*C.uint8_t)(unsafe.Pointer(&spendPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPriv[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPub[0])),
		(*C.uint8_t)(unsafe.Pointer(&onetimePub[0])),
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to derive stealth address")
	}

	return &StealthOutput{
		TxPrivKey:     txPriv,
		TxPubKey:      txPub,
		OnetimePubKey: onetimePub,
	}, nil
}

// CheckStealthOutput checks if an output belongs to us (receiver side)
func CheckStealthOutput(spendPubKey, viewPrivKey, txPubKey, onetimePubKey [32]byte) bool {
	result := C.blocknet_stealth_check_output(
		(*C.uint8_t)(unsafe.Pointer(&spendPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&onetimePubKey[0])),
	)

	return result == 0
}

// DeriveStealthPrivKey derives the private key to spend an output (receiver side)
func DeriveStealthPrivKey(spendPrivKey, viewPrivKey, txPubKey [32]byte) ([32]byte, error) {
	var onetimePriv [32]byte

	result := C.blocknet_stealth_derive_privkey(
		(*C.uint8_t)(unsafe.Pointer(&spendPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&onetimePriv[0])),
	)

	if result != 0 {
		return onetimePriv, fmt.Errorf("failed to derive stealth private key")
	}

	return onetimePriv, nil
}

// DeriveStealthSpendKey is an alias for DeriveStealthPrivKey (wallet compatibility)
func DeriveStealthSpendKey(txPubKey, viewPrivKey, spendPrivKey [32]byte) ([32]byte, error) {
	return DeriveStealthPrivKey(spendPrivKey, viewPrivKey, txPubKey)
}

// DeriveStealthSecret derives the shared secret from tx pubkey and view privkey
// shared_secret = H(view_privkey * tx_pubkey)
// Used for amount decryption and blinding factor derivation (receiver side)
func DeriveStealthSecret(txPubKey, viewPrivKey [32]byte) ([32]byte, error) {
	var secret [32]byte

	result := C.blocknet_stealth_derive_secret(
		(*C.uint8_t)(unsafe.Pointer(&txPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&secret[0])),
	)

	if result != 0 {
		return secret, fmt.Errorf("failed to derive stealth secret")
	}

	return secret, nil
}

// DeriveStealthSecretSender derives the shared secret from tx privkey and view pubkey
// shared_secret = H(tx_privkey * view_pubkey)
// Used for amount encryption (sender side) - computes same secret as receiver
func DeriveStealthSecretSender(txPrivKey, viewPubKey [32]byte) ([32]byte, error) {
	var secret [32]byte

	result := C.blocknet_stealth_derive_secret_sender(
		(*C.uint8_t)(unsafe.Pointer(&txPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&secret[0])),
	)

	if result != 0 {
		return secret, fmt.Errorf("failed to derive stealth secret (sender)")
	}

	return secret, nil
}

// ScalarToPubKey converts a scalar private key to its public key
func ScalarToPubKey(privKey [32]byte) ([32]byte, error) {
	var pubKey [32]byte

	result := C.blocknet_scalar_to_pubkey(
		(*C.uint8_t)(unsafe.Pointer(&privKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&pubKey[0])),
	)

	if result != 0 {
		return pubKey, fmt.Errorf("failed to derive public key from scalar")
	}

	return pubKey, nil
}

// DeriveStealthAddressWithKey creates a one-time address using a caller-provided tx private key.
// Same math as DeriveStealthAddress but uses txPrivKey instead of generating a random r.
func DeriveStealthAddressWithKey(spendPubKey, viewPubKey, txPrivKey [32]byte) (txPubKey, onetimePubKey [32]byte, err error) {
	result := C.blocknet_stealth_derive_address_with_key(
		(*C.uint8_t)(unsafe.Pointer(&spendPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&viewPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&txPubKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&onetimePubKey[0])),
	)
	if result != 0 {
		return txPubKey, onetimePubKey, fmt.Errorf("failed to derive stealth address with key")
	}
	return txPubKey, onetimePubKey, nil
}

// DeriveDeterministicTxKey derives a deterministic tx private key from view_privkey and key images.
// The same wallet seed + same spent inputs always produces the same r, making it recoverable.
func DeriveDeterministicTxKey(viewPrivKey [32]byte, keyImages [][32]byte) ([32]byte, error) {
	var txPrivKey [32]byte
	if len(keyImages) == 0 {
		return txPrivKey, fmt.Errorf("no key images provided")
	}

	flat := make([]byte, len(keyImages)*32)
	for i, ki := range keyImages {
		copy(flat[i*32:], ki[:])
	}

	result := C.blocknet_derive_deterministic_tx_key(
		(*C.uint8_t)(unsafe.Pointer(&viewPrivKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&flat[0])),
		C.size_t(len(keyImages)),
		(*C.uint8_t)(unsafe.Pointer(&txPrivKey[0])),
	)
	if result != 0 {
		return txPrivKey, fmt.Errorf("failed to derive deterministic tx key")
	}
	return txPrivKey, nil
}

// ============================================================================
// CLSAG Ring Signatures
// ============================================================================

// RingSize is the fixed ring size for all transactions
// Fixed to prevent information leakage from variable ring sizes
const RingSize = 16

// RistrettoKeypair represents a Ristretto keypair for ring signatures
type RistrettoKeypair struct {
	PrivateKey [32]byte
	PublicKey  [32]byte
}

// GenerateRistrettoKeypair generates a Ristretto keypair for ring signatures
func GenerateRistrettoKeypair() (*RistrettoKeypair, error) {
	var output [64]byte
	result := C.blocknet_ristretto_keygen((*C.uint8_t)(unsafe.Pointer(&output[0])))

	if result != 0 {
		return nil, fmt.Errorf("failed to generate Ristretto keypair")
	}

	kp := &RistrettoKeypair{}
	copy(kp.PrivateKey[:], output[:32])
	copy(kp.PublicKey[:], output[32:])

	return kp, nil
}

// GenerateRistrettoKeypairFromSeed generates a deterministic keypair from a 32-byte seed
// Used for BIP39 mnemonic recovery
func GenerateRistrettoKeypairFromSeed(seed [32]byte) (*RistrettoKeypair, error) {
	var output [64]byte
	result := C.blocknet_ristretto_keygen_from_seed(
		(*C.uint8_t)(unsafe.Pointer(&seed[0])),
		(*C.uint8_t)(unsafe.Pointer(&output[0])),
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to generate Ristretto keypair from seed")
	}

	kp := &RistrettoKeypair{}
	copy(kp.PrivateKey[:], output[:32])
	copy(kp.PublicKey[:], output[32:])

	return kp, nil
}

// RingSignature represents a CLSAG ring signature
type RingSignature struct {
	Signature []byte
	KeyImage  [32]byte
	RingSize  int
}

// GenerateKeyImage generates a key image for double-spend detection
func GenerateKeyImage(privateKey [32]byte) ([32]byte, error) {
	var keyImage [32]byte

	result := C.blocknet_key_image(
		(*C.uint8_t)(unsafe.Pointer(&privateKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&keyImage[0])),
	)

	if result != 0 {
		return keyImage, fmt.Errorf("failed to generate key image")
	}

	return keyImage, nil
}

// SignRing creates a CLSAG ring signature
// ring: slice of public keys (decoys + real signer)
// secretIndex: index of our key in the ring
// privateKey: our private key
// message: data to sign
func SignRing(ring [][32]byte, secretIndex int, privateKey [32]byte, message []byte) (*RingSignature, error) {
	ringSize := len(ring)
	if ringSize != RingSize {
		return nil, fmt.Errorf("ring size must be exactly %d, got %d", RingSize, ringSize)
	}
	if secretIndex < 0 || secretIndex >= ringSize {
		return nil, fmt.Errorf("secret index out of range")
	}

	// Flatten ring keys
	ringBytes := make([]byte, ringSize*32)
	for i, pk := range ring {
		copy(ringBytes[i*32:(i+1)*32], pk[:])
	}

	// Signature buffer: c0 (32) + responses (32*n) + key_image (32)
	sigLen := 32 + ringSize*32 + 32
	sigBuf := make([]byte, sigLen)
	var actualLen C.size_t

	result := C.blocknet_clsag_sign(
		(*C.uint8_t)(unsafe.Pointer(&ringBytes[0])),
		C.size_t(ringSize),
		C.size_t(secretIndex),
		(*C.uint8_t)(unsafe.Pointer(&privateKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&sigBuf[0])),
		&actualLen,
	)

	if result != 0 {
		return nil, fmt.Errorf("failed to create ring signature (error %d)", result)
	}

	sig := &RingSignature{
		Signature: sigBuf[:actualLen],
		RingSize:  ringSize,
	}

	// Extract key image
	copy(sig.KeyImage[:], sigBuf[32+ringSize*32:])

	return sig, nil
}

// VerifyRing verifies a CLSAG ring signature
func VerifyRing(ring [][32]byte, message []byte, sig *RingSignature) error {
	if sig == nil {
		return fmt.Errorf("ring signature is required")
	}
	ringSize := len(ring)
	if ringSize == 0 {
		return fmt.Errorf("ring must not be empty")
	}
	if ringSize != sig.RingSize {
		return fmt.Errorf("ring size mismatch")
	}
	if len(message) == 0 {
		return fmt.Errorf("ring signature message must not be empty")
	}
	if len(sig.Signature) == 0 {
		return fmt.Errorf("ring signature must not be empty")
	}

	// Flatten ring keys
	ringBytes := make([]byte, ringSize*32)
	for i, pk := range ring {
		copy(ringBytes[i*32:(i+1)*32], pk[:])
	}

	result := C.blocknet_clsag_verify(
		(*C.uint8_t)(unsafe.Pointer(&ringBytes[0])),
		C.size_t(ringSize),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&sig.Signature[0])),
		C.size_t(len(sig.Signature)),
	)

	if result != 0 {
		return fmt.Errorf("ring signature verification failed")
	}

	return nil
}

// ============================================================================
// RingCT (Ring Confidential Transactions)
// ============================================================================

// RingCTSignature contains a RingCT CLSAG signature with commitment linking
type RingCTSignature struct {
	Signature    []byte   // Full signature bytes
	RingSize     int      // Number of ring members
	KeyImage     [32]byte // Key image for double-spend detection
	PseudoOutput [32]byte // Pseudo-output commitment
}

// SignRingCT creates a RingCT signature that proves both key ownership
// and that the pseudo-output commits to the same amount as the real input
func SignRingCT(
	ringKeys [][32]byte, // Public keys of ring members
	ringCommitments [][32]byte, // Commitments of ring members
	secretIndex int, // Which ring member is ours
	privateKey [32]byte, // Our private key
	inputBlinding [32]byte, // Blinding factor of our real input
	pseudoOutput [32]byte, // Our pseudo-output commitment
	pseudoBlinding [32]byte, // Blinding factor of pseudo-output
	message []byte, // Message to sign
) (*RingCTSignature, error) {
	ringSize := len(ringKeys)
	if ringSize != len(ringCommitments) {
		return nil, fmt.Errorf("ring keys and commitments must have same length")
	}
	if ringSize != RingSize {
		return nil, fmt.Errorf("ring size must be exactly %d, got %d", RingSize, ringSize)
	}
	if secretIndex < 0 || secretIndex >= ringSize {
		return nil, fmt.Errorf("secret index out of range")
	}

	// Flatten ring keys and commitments
	ringKeyBytes := make([]byte, ringSize*32)
	ringCommitBytes := make([]byte, ringSize*32)
	for i := 0; i < ringSize; i++ {
		copy(ringKeyBytes[i*32:(i+1)*32], ringKeys[i][:])
		copy(ringCommitBytes[i*32:(i+1)*32], ringCommitments[i][:])
	}

	// Signature buffer: 32 + n*32 + n*32 + 32 + 32 = 96 + 64*n
	sigLen := 96 + 64*ringSize
	sigBuf := make([]byte, sigLen)
	var actualLen C.size_t

	result := C.blocknet_ringct_sign(
		(*C.uint8_t)(unsafe.Pointer(&ringKeyBytes[0])),
		(*C.uint8_t)(unsafe.Pointer(&ringCommitBytes[0])),
		C.size_t(ringSize),
		C.size_t(secretIndex),
		(*C.uint8_t)(unsafe.Pointer(&privateKey[0])),
		(*C.uint8_t)(unsafe.Pointer(&inputBlinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&pseudoOutput[0])),
		(*C.uint8_t)(unsafe.Pointer(&pseudoBlinding[0])),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&sigBuf[0])),
		&actualLen,
	)

	if result != 0 {
		return nil, fmt.Errorf("RingCT sign failed (error %d)", result)
	}

	sig := &RingCTSignature{
		Signature: sigBuf[:actualLen],
		RingSize:  ringSize,
	}

	// Extract key image (at offset 32 + 2*ringSize*32)
	kiOffset := 32 + ringSize*32 + ringSize*32
	copy(sig.KeyImage[:], sigBuf[kiOffset:kiOffset+32])

	// Extract pseudo-output
	copy(sig.PseudoOutput[:], pseudoOutput[:])

	return sig, nil
}

// VerifyRingCT verifies a RingCT signature
// Verifies both key ownership AND that pseudo-output matches a real input
func VerifyRingCT(
	ringKeys [][32]byte,
	ringCommitments [][32]byte,
	message []byte,
	sig *RingCTSignature,
) error {
	if sig == nil {
		return fmt.Errorf("RingCT signature is required")
	}
	ringSize := len(ringKeys)
	if ringSize == 0 {
		return fmt.Errorf("RingCT ring must not be empty")
	}
	if ringSize != len(ringCommitments) {
		return fmt.Errorf("ring keys and commitments must have same length")
	}
	if ringSize != sig.RingSize {
		return fmt.Errorf("ring size mismatch")
	}
	if len(message) == 0 {
		return fmt.Errorf("RingCT message must not be empty")
	}
	if len(sig.Signature) == 0 {
		return fmt.Errorf("RingCT signature must not be empty")
	}

	// Flatten ring keys and commitments
	ringKeyBytes := make([]byte, ringSize*32)
	ringCommitBytes := make([]byte, ringSize*32)
	for i := 0; i < ringSize; i++ {
		copy(ringKeyBytes[i*32:(i+1)*32], ringKeys[i][:])
		copy(ringCommitBytes[i*32:(i+1)*32], ringCommitments[i][:])
	}

	result := C.blocknet_ringct_verify(
		(*C.uint8_t)(unsafe.Pointer(&ringKeyBytes[0])),
		(*C.uint8_t)(unsafe.Pointer(&ringCommitBytes[0])),
		C.size_t(ringSize),
		(*C.uint8_t)(unsafe.Pointer(&message[0])),
		C.size_t(len(message)),
		(*C.uint8_t)(unsafe.Pointer(&sig.Signature[0])),
		C.size_t(len(sig.Signature)),
	)

	if result != 0 {
		return fmt.Errorf("RingCT verification failed: commitment mismatch or invalid signature")
	}

	return nil
}

// ExtractRingCTBinding extracts key image and pseudo-output from RingCT signature payload.
func ExtractRingCTBinding(sig *RingCTSignature) ([32]byte, [32]byte, error) {
	var keyImage [32]byte
	var pseudoOutput [32]byte

	if sig == nil {
		return keyImage, pseudoOutput, fmt.Errorf("RingCT signature is required")
	}
	if sig.RingSize <= 0 {
		return keyImage, pseudoOutput, fmt.Errorf("RingCT ring size must be positive")
	}

	expectedLen := 32 + sig.RingSize*32 + sig.RingSize*32 + 32 + 32
	if len(sig.Signature) != expectedLen {
		return keyImage, pseudoOutput, fmt.Errorf(
			"invalid RingCT signature length: got %d, expected %d",
			len(sig.Signature),
			expectedLen,
		)
	}

	kiOffset := 32 + sig.RingSize*32 + sig.RingSize*32
	copy(keyImage[:], sig.Signature[kiOffset:kiOffset+32])
	copy(pseudoOutput[:], sig.Signature[kiOffset+32:kiOffset+64])
	return keyImage, pseudoOutput, nil
}

// ============================================================================
// Proof of Work (Argon2id)
// ============================================================================

// PowHash computes an Argon2id hash for proof of work
// Uses 2GB memory, making it ASIC-resistant
func PowHash(header []byte, nonce uint64) ([32]byte, error) {
	var output [32]byte

	result := C.blocknet_pow_hash(
		(*C.uint8_t)(unsafe.Pointer(&header[0])),
		C.size_t(len(header)),
		C.uint64_t(nonce),
		(*C.uint8_t)(unsafe.Pointer(&output[0])),
	)

	if result != 0 {
		return [32]byte{}, fmt.Errorf("pow hash failed (error %d)", result)
	}

	return output, nil
}

// PowCheckTarget checks if a hash meets the difficulty target
// Returns true if hash < target (valid block)
func PowCheckTarget(hash, target [32]byte) bool {
	result := C.blocknet_pow_check_target(
		(*C.uint8_t)(unsafe.Pointer(&hash[0])),
		(*C.uint8_t)(unsafe.Pointer(&target[0])),
	)
	return result == 1
}

// DifficultyToTarget converts a difficulty value to a target hash
func DifficultyToTarget(difficulty uint64) [32]byte {
	var target [32]byte

	C.blocknet_difficulty_to_target(
		C.uint64_t(difficulty),
		(*C.uint8_t)(unsafe.Pointer(&target[0])),
	)

	return target
}

// EncryptAmount encrypts an amount using the shared secret and output index
// The amount can be decrypted by anyone who knows the shared secret
func EncryptAmount(amount uint64, sharedSecret [32]byte, outputIndex int) [8]byte {
	// Derive mask from shared secret
	var indexBuf [4]byte
	indexBuf[0] = byte(outputIndex)
	indexBuf[1] = byte(outputIndex >> 8)
	indexBuf[2] = byte(outputIndex >> 16)
	indexBuf[3] = byte(outputIndex >> 24)
	const tag = "blocknet_amount"
	b := make([]byte, 0, len(tag)+len(sharedSecret)+len(indexBuf))
	b = append(b, tag...)
	b = append(b, sharedSecret[:]...)
	b = append(b, indexBuf[:]...)
	mask := sha3.Sum256(b)

	// XOR amount with mask
	var amountBytes [8]byte
	amountBytes[0] = byte(amount)
	amountBytes[1] = byte(amount >> 8)
	amountBytes[2] = byte(amount >> 16)
	amountBytes[3] = byte(amount >> 24)
	amountBytes[4] = byte(amount >> 32)
	amountBytes[5] = byte(amount >> 40)
	amountBytes[6] = byte(amount >> 48)
	amountBytes[7] = byte(amount >> 56)

	var encrypted [8]byte
	for i := 0; i < 8; i++ {
		encrypted[i] = amountBytes[i] ^ mask[i]
	}
	return encrypted
}

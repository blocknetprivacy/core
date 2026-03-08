#ifndef BLOCKNET_CRYPTO_H
#define BLOCKNET_CRYPTO_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

// ============================================================================
// Key Generation & Signatures
// ============================================================================

// Generate a new ed25519 keypair
// output: 64-byte buffer (32-byte private key || 32-byte public key)
// Returns: 0 on success, -1 on error
int32_t blocknet_generate_keypair(uint8_t* output);

// Sign a message with ed25519
int32_t blocknet_sign(
    const uint8_t* private_key,
    const uint8_t* message,
    size_t message_len,
    uint8_t* signature_out
);

// Verify an ed25519 signature
int32_t blocknet_verify(
    const uint8_t* public_key,
    const uint8_t* message,
    size_t message_len,
    const uint8_t* signature
);

// Schnorr sign over Ristretto255 (compatible with wallet spend keys)
// signature_out: 64-byte buffer (32-byte R || 32-byte s)
int32_t blocknet_schnorr_sign(
    const uint8_t* private_key,
    const uint8_t* message,
    size_t message_len,
    uint8_t* signature_out
);

// Schnorr verify over Ristretto255
int32_t blocknet_schnorr_verify(
    const uint8_t* public_key,
    const uint8_t* message,
    size_t message_len,
    const uint8_t* signature
);

// ============================================================================
// Pedersen Commitments
// ============================================================================

// Create a Pedersen commitment to a value
// value: 64-bit amount to commit
// blinding_out: 32-byte buffer for random blinding factor
// commitment_out: 32-byte buffer for commitment
int32_t blocknet_pedersen_commit(
    uint64_t value,
    uint8_t* blinding_out,
    uint8_t* commitment_out
);

// Create Pedersen commitment with specific blinding factor
int32_t blocknet_pedersen_commit_with_blinding(
    uint64_t value,
    const uint8_t* blinding,
    uint8_t* commitment_out
);

// Verify a Pedersen commitment opens to a value
int32_t blocknet_pedersen_verify(
    uint64_t value,
    const uint8_t* blinding,
    const uint8_t* commitment
);

// Add two commitments: result = c1 + c2
int32_t blocknet_commitment_add(
    const uint8_t* c1,
    const uint8_t* c2,
    uint8_t* result_out
);

// Subtract two commitments: result = c1 - c2
int32_t blocknet_commitment_sub(
    const uint8_t* c1,
    const uint8_t* c2,
    uint8_t* result_out
);

// Check if commitment is zero (identity point)
// Returns 0 if zero, -1 if not
int32_t blocknet_commitment_is_zero(const uint8_t* commitment);

// Create fee commitment (fee * value_generator)
int32_t blocknet_fee_commitment(
    uint64_t fee,
    uint8_t* commitment_out
);

// Add two blinding factors: result = b1 + b2
int32_t blocknet_blinding_add(
    const uint8_t* b1,
    const uint8_t* b2,
    uint8_t* result_out
);

// Subtract blinding factors: result = b1 - b2
int32_t blocknet_blinding_sub(
    const uint8_t* b1,
    const uint8_t* b2,
    uint8_t* result_out
);

// ============================================================================
// Bulletproofs Range Proofs
// ============================================================================

// Create a bulletproof range proof
// value: the secret value
// blinding: 32-byte blinding factor
// proof_out: buffer for proof (max 1024 bytes recommended)
// proof_len_out: actual length of proof
int32_t blocknet_range_proof_create(
    uint64_t value,
    const uint8_t* blinding,
    uint8_t* proof_out,
    size_t* proof_len_out
);

// Verify a bulletproof range proof
// commitment: 32-byte Pedersen commitment
// proof: proof bytes
// proof_len: length of proof
int32_t blocknet_range_proof_verify(
    const uint8_t* commitment,
    const uint8_t* proof,
    size_t proof_len
);

// ============================================================================
// Hashing
// ============================================================================

// SHA256 hash
int32_t blocknet_sha256(
    const uint8_t* data,
    size_t data_len,
    uint8_t* hash_out
);

// ============================================================================
// Stealth Addresses
// ============================================================================

// Generate stealth keypair (spend + view keys)
// output: 128-byte buffer
//   [0..32]   spend private key
//   [32..64]  spend public key
//   [64..96]  view private key
//   [96..128] view public key
int32_t blocknet_stealth_keygen(uint8_t* output);

// Derive one-time stealth address (sender side)
// spend_pubkey: receiver's 32-byte spend public key
// view_pubkey: receiver's 32-byte view public key
// tx_privkey_out: 32-byte buffer for transaction private key
// tx_pubkey_out: 32-byte buffer for transaction public key (include in tx)
// onetime_pubkey_out: 32-byte buffer for one-time address
int32_t blocknet_stealth_derive_address(
    const uint8_t* spend_pubkey,
    const uint8_t* view_pubkey,
    uint8_t* tx_privkey_out,
    uint8_t* tx_pubkey_out,
    uint8_t* onetime_pubkey_out
);

// Check if output belongs to us (receiver side)
// Returns 0 if ours, -1 if not
int32_t blocknet_stealth_check_output(
    const uint8_t* spend_pubkey,
    const uint8_t* view_privkey,
    const uint8_t* tx_pubkey,
    const uint8_t* onetime_pubkey
);

// Derive private key to spend an output (receiver side)
int32_t blocknet_stealth_derive_privkey(
    const uint8_t* spend_privkey,
    const uint8_t* view_privkey,
    const uint8_t* tx_pubkey,
    uint8_t* onetime_privkey_out
);

// Derive shared secret from tx_pubkey and view_privkey (receiver side)
// shared_secret = H(view_privkey * tx_pubkey)
// Used for amount decryption and blinding factor derivation
int32_t blocknet_stealth_derive_secret(
    const uint8_t* tx_pubkey,
    const uint8_t* view_privkey,
    uint8_t* secret_out
);

// Derive shared secret from tx_privkey and view_pubkey (sender side)
// shared_secret = H(tx_privkey * view_pubkey)
// Computes same secret as receiver side
int32_t blocknet_stealth_derive_secret_sender(
    const uint8_t* tx_privkey,
    const uint8_t* view_pubkey,
    uint8_t* secret_out
);

// Derive one-time stealth address using a caller-provided tx private key
// Same as blocknet_stealth_derive_address but uses the given r instead of random
int32_t blocknet_stealth_derive_address_with_key(
    const uint8_t* spend_pubkey,
    const uint8_t* view_pubkey,
    const uint8_t* tx_privkey,
    uint8_t* tx_pubkey_out,
    uint8_t* onetime_pubkey_out
);

// Derive deterministic tx private key from view_privkey and key images
// r = hash_to_scalar("blocknet_deterministic_tx_key" || view_priv || sorted(key_images))
// key_images: n_images * 32 bytes
int32_t blocknet_derive_deterministic_tx_key(
    const uint8_t* view_privkey,
    const uint8_t* key_images,
    size_t n_images,
    uint8_t* tx_privkey_out
);

// Derive indexed shared secret from tx_pubkey and view_privkey (receiver side)
// shared_secret = H(view_privkey * tx_pubkey || output_index)
int32_t blocknet_stealth_derive_secret_indexed(
    const uint8_t* tx_pubkey,
    const uint8_t* view_privkey,
    uint32_t output_index,
    uint8_t* secret_out
);

// Derive indexed shared secret from tx_privkey and view_pubkey (sender side)
// shared_secret = H(tx_privkey * view_pubkey || output_index)
int32_t blocknet_stealth_derive_secret_sender_indexed(
    const uint8_t* tx_privkey,
    const uint8_t* view_pubkey,
    uint32_t output_index,
    uint8_t* secret_out
);

// Convert scalar private key to public key
int32_t blocknet_scalar_to_pubkey(
    const uint8_t* privkey,
    uint8_t* pubkey_out
);

// ============================================================================
// CLSAG Ring Signatures
// ============================================================================

// Generate Ristretto keypair for ring signatures
// output: 64-byte buffer (32-byte private key || 32-byte public key)
int32_t blocknet_ristretto_keygen(uint8_t* output);

// Generate Ristretto keypair from seed (deterministic, for BIP39 recovery)
// seed: 32-byte seed
// output: 64-byte buffer (32-byte private key || 32-byte public key)
int32_t blocknet_ristretto_keygen_from_seed(
    const uint8_t* seed,
    uint8_t* output
);

// Generate key image from private key (for double-spend detection)
// I = x * Hp(P)
int32_t blocknet_key_image(
    const uint8_t* private_key,
    uint8_t* key_image_out
);

// Sign with CLSAG ring signature
// ring_keys: n * 32 bytes of public keys
// ring_size: number of ring members (2-16)
// secret_index: which ring member is ours
// private_key: our 32-byte private key
// message: message to sign
// message_len: length of message
// signature_out: buffer for signature (32 + 32*n + 32 bytes)
// signature_len_out: actual signature length
int32_t blocknet_clsag_sign(
    const uint8_t* ring_keys,
    size_t ring_size,
    size_t secret_index,
    const uint8_t* private_key,
    const uint8_t* message,
    size_t message_len,
    uint8_t* signature_out,
    size_t* signature_len_out
);

// Verify a CLSAG ring signature
int32_t blocknet_clsag_verify(
    const uint8_t* ring_keys,
    size_t ring_size,
    const uint8_t* message,
    size_t message_len,
    const uint8_t* signature,
    size_t signature_len
);

// Extract key image from signature
int32_t blocknet_clsag_key_image(
    const uint8_t* signature,
    size_t ring_size,
    uint8_t* key_image_out
);

// ============================================================================
// RingCT CLSAG (with commitment linking)
// ============================================================================

// Sign with RingCT CLSAG
// Proves: key ownership AND pseudo-output matches real input amount
// ring_keys: n * 32 bytes of public keys
// ring_commitments: n * 32 bytes of commitment points (from UTXOs)
// ring_size: number of ring members
// secret_index: which ring member is ours
// private_key: our 32-byte private key
// input_blinding: blinding factor of our real input commitment
// pseudo_output: our pseudo-output commitment (32 bytes)
// pseudo_blinding: blinding factor of pseudo-output
// message: message to sign
// message_len: length of message
// signature_out: buffer for signature (96 + 64*n bytes)
// signature_len_out: actual signature length
int32_t blocknet_ringct_sign(
    const uint8_t* ring_keys,
    const uint8_t* ring_commitments,
    size_t ring_size,
    size_t secret_index,
    const uint8_t* private_key,
    const uint8_t* input_blinding,
    const uint8_t* pseudo_output,
    const uint8_t* pseudo_blinding,
    const uint8_t* message,
    size_t message_len,
    uint8_t* signature_out,
    size_t* signature_len_out
);

// Verify a RingCT CLSAG signature
int32_t blocknet_ringct_verify(
    const uint8_t* ring_keys,
    const uint8_t* ring_commitments,
    size_t ring_size,
    const uint8_t* message,
    size_t message_len,
    const uint8_t* signature,
    size_t signature_len
);

// Extract key image from RingCT signature
int32_t blocknet_ringct_key_image(
    const uint8_t* signature,
    size_t ring_size,
    uint8_t* key_image_out
);

// Extract pseudo-output from RingCT signature
int32_t blocknet_ringct_pseudo_output(
    const uint8_t* signature,
    size_t ring_size,
    uint8_t* pseudo_out
);

// ============================================================================
// Proof of Work (Argon2id)
// ============================================================================

// Compute Argon2id hash for proof of work
// Uses 2GB memory, 1 iteration, parallelism 1
// header: block header bytes (used as salt)
// header_len: length of header
// nonce: 64-bit nonce (used as password)
// output: 32-byte buffer for hash result
int32_t blocknet_pow_hash(
    const uint8_t* header,
    size_t header_len,
    uint64_t nonce,
    uint8_t* output
);

// Check if hash meets difficulty target
// Returns 1 if hash < target (valid), 0 otherwise
int32_t blocknet_pow_check_target(
    const uint8_t* hash,
    const uint8_t* target
);

// Convert difficulty to target bytes
// difficulty: 64-bit difficulty value
// target: 32-byte buffer for target
int32_t blocknet_difficulty_to_target(
    uint64_t difficulty,
    uint8_t* target
);

#ifdef __cplusplus
}
#endif

#endif // BLOCKNET_CRYPTO_H

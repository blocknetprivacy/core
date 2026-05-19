//! CLSAG Ring Signatures with RingCT
//!
//! Concise Linkable Spontaneous Anonymous Group Signatures
//! Extended with commitment linking for RingCT (Confidential Transactions).
//!
//! Key properties:
//! - Anonymity: Can't tell which ring member signed
//! - Linkability: Key image prevents double-spending
//! - Unforgeability: Only owner of private key can sign
//! - Commitment linking: Proves pseudo-output matches real input amount

use bulletproofs::PedersenGens;
use curve25519_dalek::constants::{RISTRETTO_BASEPOINT_POINT, RISTRETTO_BASEPOINT_TABLE};
use curve25519_dalek::ristretto::{CompressedRistretto, RistrettoPoint};
use curve25519_dalek::scalar::Scalar;
use curve25519_dalek::traits::VartimeMultiscalarMul;
use sha2::{Digest, Sha512};
use std::slice;

// Get the blinding generator from bulletproofs (must match commitment module)
fn blinding_generator() -> RistrettoPoint {
    PedersenGens::default().B_blinding
}

/// Fixed ring size for all transactions (privacy requirement)
/// All transactions must use exactly this ring size
pub const RING_SIZE: usize = 16;

/// Hash data to a Ristretto point (for key image generation)
fn hash_to_point(data: &[u8]) -> RistrettoPoint {
    let mut hasher = Sha512::new();
    hasher.update(b"blocknet_hash_to_point");
    hasher.update(data);
    let hash = hasher.finalize();
    RistrettoPoint::from_uniform_bytes(&hash.into())
}

/// Compute challenge hash for ring signature
fn challenge_hash(
    message: &[u8],
    l: &RistrettoPoint,
    r: &RistrettoPoint,
) -> Scalar {
    let mut hasher = Sha512::new();
    hasher.update(b"blocknet_clsag_c");
    hasher.update(message);
    hasher.update(l.compress().as_bytes());
    hasher.update(r.compress().as_bytes());
    let hash = hasher.finalize();
    Scalar::from_bytes_mod_order_wide(&hash.into())
}

/// Generate a Ristretto keypair for ring signatures
/// Output: 32-byte private key || 32-byte public key (64 bytes total)
#[no_mangle]
pub extern "C" fn blocknet_ristretto_keygen(output: *mut u8) -> i32 {
    if output.is_null() {
        return -1;
    }

    let privkey = Scalar::random(&mut rand::thread_rng());
    let pubkey = &privkey * RISTRETTO_BASEPOINT_TABLE;

    unsafe {
        let out = slice::from_raw_parts_mut(output, 64);
        out[0..32].copy_from_slice(privkey.as_bytes());
        out[32..64].copy_from_slice(pubkey.compress().as_bytes());
    }

    0
}

/// Generate a Ristretto keypair from a 32-byte seed (deterministic)
/// The seed is hashed to ensure it's a valid scalar
#[no_mangle]
pub extern "C" fn blocknet_ristretto_keygen_from_seed(
    seed: *const u8,
    output: *mut u8,
) -> i32 {
    if seed.is_null() || output.is_null() {
        return -1;
    }

    unsafe {
        let seed_bytes = slice::from_raw_parts(seed, 32);

        // Hash the seed to get a valid scalar
        // Using SHA512 and reducing mod l (curve order)
        use sha2::{Digest, Sha512};
        let mut hasher = Sha512::new();
        hasher.update(b"blocknet_keygen");
        hasher.update(seed_bytes);
        let hash = hasher.finalize();

        // Scalar::from_bytes_mod_order_wide takes 64 bytes and reduces mod l
        let privkey = Scalar::from_bytes_mod_order_wide(&hash.into());
        let pubkey = &privkey * RISTRETTO_BASEPOINT_TABLE;

        let out = slice::from_raw_parts_mut(output, 64);
        out[0..32].copy_from_slice(privkey.as_bytes());
        out[32..64].copy_from_slice(pubkey.compress().as_bytes());
    }

    0
}

/// Return 1 when pubkey is a canonical compressed Ristretto point, otherwise 0.
#[no_mangle]
pub extern "C" fn blocknet_ristretto_pubkey_is_valid(pubkey: *const u8) -> i32 {
    if pubkey.is_null() {
        return 0;
    }

    unsafe {
        let pubkey_bytes = slice::from_raw_parts(pubkey, 32);
        match CompressedRistretto::from_slice(pubkey_bytes)
            .expect("slice length")
            .decompress()
        {
            Some(_) => 1,
            None => 0,
        }
    }
}

/// Generate key image from private key
/// I = x * Hp(P) where P = x*G
#[no_mangle]
pub extern "C" fn blocknet_key_image(
    private_key: *const u8,
    key_image_out: *mut u8,
) -> i32 {
    if private_key.is_null() || key_image_out.is_null() {
        return -1;
    }

    unsafe {
        let priv_bytes = slice::from_raw_parts(private_key, 32);

        let x = match Scalar::from_canonical_bytes(priv_bytes.try_into().expect("len"))
            .into_option()
        {
            Some(s) => s,
            None => return -1,
        };

        // P = x * G
        let public_key = &x * RISTRETTO_BASEPOINT_TABLE;

        // Hp = hash_to_point(P)
        let hp = hash_to_point(public_key.compress().as_bytes());

        // I = x * Hp
        let key_image = x * hp;

        let out = slice::from_raw_parts_mut(key_image_out, 32);
        out.copy_from_slice(key_image.compress().as_bytes());
    }

    0
}

/// Sign a message with CLSAG ring signature
///
/// ring_keys: n * 32 bytes of public keys (ring members)
/// ring_size: number of ring members
/// secret_index: index of our key in the ring
/// private_key: our 32-byte private key
/// message: message to sign
/// message_len: length of message
/// signature_out: buffer for signature (32 + 32*n + 32 bytes: c0 || s0..sn-1 || key_image)
/// signature_len_out: actual signature length
#[no_mangle]
pub extern "C" fn blocknet_clsag_sign(
    ring_keys: *const u8,
    ring_size: usize,
    secret_index: usize,
    private_key: *const u8,
    message: *const u8,
    message_len: usize,
    signature_out: *mut u8,
    signature_len_out: *mut usize,
) -> i32 {
    if ring_keys.is_null()
        || private_key.is_null()
        || message.is_null()
        || signature_out.is_null()
        || signature_len_out.is_null()
    {
        return -1;
    }

    if ring_size != RING_SIZE || secret_index >= ring_size {
        return -2;
    }

    unsafe {
        let ring_bytes = slice::from_raw_parts(ring_keys, ring_size * 32);
        let priv_bytes = slice::from_raw_parts(private_key, 32);
        let msg = slice::from_raw_parts(message, message_len);

        // Parse private key
        let x = match Scalar::from_canonical_bytes(priv_bytes.try_into().expect("len"))
            .into_option()
        {
            Some(s) => s,
            None => return -1,
        };

        // Parse ring public keys
        let mut ring: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        for i in 0..ring_size {
            let pk_bytes = &ring_bytes[i * 32..(i + 1) * 32];
            let compressed = CompressedRistretto::from_slice(pk_bytes).expect("len");
            match compressed.decompress() {
                Some(p) => ring.push(p),
                None => return -1,
            }
        }

        // Verify our public key matches
        let our_pubkey = &x * RISTRETTO_BASEPOINT_TABLE;
        if ring[secret_index].compress() != our_pubkey.compress() {
            return -3; // Private key doesn't match ring member
        }

        // Compute key image: I = x * Hp(P)
        let hp_secret = hash_to_point(ring[secret_index].compress().as_bytes());
        let key_image = x * hp_secret;

        // Generate random nonce
        let alpha = Scalar::random(&mut rand::thread_rng());

        // Generate random responses for all except secret_index
        let mut s: Vec<Scalar> = (0..ring_size)
            .map(|_| Scalar::random(&mut rand::thread_rng()))
            .collect();

        // Compute challenges
        let mut c: Vec<Scalar> = vec![Scalar::ZERO; ring_size];

        // Start: L_π = α*G, R_π = α*Hp(P_π)
        let l_start = &alpha * RISTRETTO_BASEPOINT_TABLE;
        let r_start = alpha * hp_secret;

        // c_{π+1} = H(m, L_π, R_π)
        let mut next_idx = (secret_index + 1) % ring_size;
        c[next_idx] = challenge_hash(msg, &l_start, &r_start);

        // Go around the ring from π+1 back to π
        for _ in 0..(ring_size - 1) {
            let hp_i = hash_to_point(ring[next_idx].compress().as_bytes());

            // L_i = s_i*G + c_i*P_i
            let l_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[next_idx], c[next_idx]],
                &[RISTRETTO_BASEPOINT_POINT, ring[next_idx]],
            );

            // R_i = s_i*Hp(P_i) + c_i*I
            let r_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[next_idx], c[next_idx]],
                &[hp_i, key_image],
            );

            let after_idx = (next_idx + 1) % ring_size;
            c[after_idx] = challenge_hash(msg, &l_i, &r_i);
            next_idx = after_idx;
        }

        // Close the ring: s_π = α - c_π * x
        s[secret_index] = alpha - c[secret_index] * x;

        // Build signature: c_0 || s_0 || s_1 || ... || s_{n-1} || key_image
        let sig_len = 32 + ring_size * 32 + 32;
        let out = slice::from_raw_parts_mut(signature_out, sig_len);

        out[0..32].copy_from_slice(c[0].as_bytes());
        for i in 0..ring_size {
            out[32 + i * 32..32 + (i + 1) * 32].copy_from_slice(s[i].as_bytes());
        }
        out[32 + ring_size * 32..].copy_from_slice(key_image.compress().as_bytes());

        *signature_len_out = sig_len;
    }

    0
}

/// Verify a CLSAG ring signature
#[no_mangle]
pub extern "C" fn blocknet_clsag_verify(
    ring_keys: *const u8,
    ring_size: usize,
    message: *const u8,
    message_len: usize,
    signature: *const u8,
    signature_len: usize,
) -> i32 {
    if ring_keys.is_null() || message.is_null() || signature.is_null() {
        return -1;
    }

    let expected_len = 32 + ring_size * 32 + 32;
    if signature_len != expected_len || ring_size != RING_SIZE {
        return -2;
    }

    unsafe {
        let ring_bytes = slice::from_raw_parts(ring_keys, ring_size * 32);
        let msg = slice::from_raw_parts(message, message_len);
        let sig = slice::from_raw_parts(signature, signature_len);

        // Parse ring
        let mut ring: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        for i in 0..ring_size {
            let pk_bytes = &ring_bytes[i * 32..(i + 1) * 32];
            let compressed = CompressedRistretto::from_slice(pk_bytes).expect("len");
            match compressed.decompress() {
                Some(p) => ring.push(p),
                None => return -1,
            }
        }

        // Parse c_0
        let c0 = match Scalar::from_canonical_bytes(sig[0..32].try_into().expect("len"))
            .into_option()
        {
            Some(s) => s,
            None => return -1,
        };

        // Parse responses
        let mut s: Vec<Scalar> = Vec::with_capacity(ring_size);
        for i in 0..ring_size {
            let s_bytes = &sig[32 + i * 32..32 + (i + 1) * 32];
            match Scalar::from_canonical_bytes(s_bytes.try_into().expect("len")).into_option() {
                Some(scalar) => s.push(scalar),
                None => return -1,
            }
        }

        // Parse key image
        let key_image = match CompressedRistretto::from_slice(
            &sig[32 + ring_size * 32..32 + ring_size * 32 + 32],
        )
        .expect("len")
        .decompress()
        {
            Some(p) => p,
            None => return -1,
        };

        // Verify ring: recompute all challenges
        let mut c = c0;
        for i in 0..ring_size {
            let hp_i = hash_to_point(ring[i].compress().as_bytes());

            // L_i = s_i*G + c_i*P_i
            let l_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[i], c],
                &[RISTRETTO_BASEPOINT_POINT, ring[i]],
            );

            // R_i = s_i*Hp(P_i) + c_i*I
            let r_i = RistrettoPoint::vartime_multiscalar_mul(&[s[i], c], &[hp_i, key_image]);

            c = challenge_hash(msg, &l_i, &r_i);
        }

        // Ring should close: final c should equal c0
        if c == c0 {
            0
        } else {
            -1
        }
    }
}

/// Extract key image from a CLSAG signature
#[no_mangle]
pub extern "C" fn blocknet_clsag_key_image(
    signature: *const u8,
    ring_size: usize,
    key_image_out: *mut u8,
) -> i32 {
    if signature.is_null() || key_image_out.is_null() {
        return -1;
    }

    let sig_len = 32 + ring_size * 32 + 32;

    unsafe {
        let sig = slice::from_raw_parts(signature, sig_len);
        let out = slice::from_raw_parts_mut(key_image_out, 32);
        out.copy_from_slice(&sig[32 + ring_size * 32..]);
    }

    0
}

// ============================================================================
// RingCT CLSAG - CLSAG with Commitment Linking
// ============================================================================

/// Challenge hash for RingCT (includes commitment terms)
fn ringct_challenge_hash(
    message: &[u8],
    l: &RistrettoPoint,
    r: &RistrettoPoint,
    d: &RistrettoPoint, // Commitment difference term
) -> Scalar {
    let mut hasher = Sha512::new();
    hasher.update(b"blocknet_ringct_c");
    hasher.update(message);
    hasher.update(l.compress().as_bytes());
    hasher.update(r.compress().as_bytes());
    hasher.update(d.compress().as_bytes());
    let hash = hasher.finalize();
    Scalar::from_bytes_mod_order_wide(&hash.into())
}

/// Sign with RingCT CLSAG (includes pseudo-output commitment linking)
///
/// This proves:
/// 1. Signer knows private key for one ring member
/// 2. Pseudo-output commitment contains same amount as that ring member's commitment
///
/// ring_keys: n * 32 bytes of public keys
/// ring_commitments: n * 32 bytes of commitment points (from the UTXOs)
/// ring_size: number of ring members
/// secret_index: which ring member is ours
/// private_key: our 32-byte private key (x such that P = x*G)
/// input_blinding: blinding factor of our real input commitment
/// pseudo_output: our pseudo-output commitment (computed by caller)
/// pseudo_blinding: blinding factor of pseudo-output
/// message: message to sign
/// message_len: length of message
/// signature_out: buffer for signature
/// signature_len_out: actual signature length
#[no_mangle]
pub extern "C" fn blocknet_ringct_sign(
    ring_keys: *const u8,
    ring_commitments: *const u8,
    ring_size: usize,
    secret_index: usize,
    private_key: *const u8,
    input_blinding: *const u8,
    pseudo_output: *const u8,
    pseudo_blinding: *const u8,
    message: *const u8,
    message_len: usize,
    signature_out: *mut u8,
    signature_len_out: *mut usize,
) -> i32 {
    if ring_keys.is_null()
        || ring_commitments.is_null()
        || private_key.is_null()
        || input_blinding.is_null()
        || pseudo_output.is_null()
        || pseudo_blinding.is_null()
        || message.is_null()
        || signature_out.is_null()
        || signature_len_out.is_null()
    {
        return -1;
    }

    if ring_size != RING_SIZE || secret_index >= ring_size {
        return -2;
    }

    unsafe {
        let ring_key_bytes = slice::from_raw_parts(ring_keys, ring_size * 32);
        let ring_commit_bytes = slice::from_raw_parts(ring_commitments, ring_size * 32);
        let priv_bytes = slice::from_raw_parts(private_key, 32);
        let input_blind_bytes = slice::from_raw_parts(input_blinding, 32);
        let pseudo_out_bytes = slice::from_raw_parts(pseudo_output, 32);
        let pseudo_blind_bytes = slice::from_raw_parts(pseudo_blinding, 32);
        let msg = slice::from_raw_parts(message, message_len);

        // Parse private key x
        let x = match Scalar::from_canonical_bytes(priv_bytes.try_into().expect("len"))
            .into_option()
        {
            Some(s) => s,
            None => return -1,
        };

        // Parse blinding factors (use from_bytes_mod_order for hash-derived blindings)
        let r_input = Scalar::from_bytes_mod_order(input_blind_bytes.try_into().expect("len"));
        let r_pseudo = Scalar::from_bytes_mod_order(pseudo_blind_bytes.try_into().expect("len"));

        // z = r_pseudo - r_input (the blinding difference we're proving knowledge of)
        let z = r_pseudo - r_input;

        // Parse pseudo-output commitment
        let c_pseudo = match CompressedRistretto::from_slice(pseudo_out_bytes)
            .expect("len")
            .decompress()
        {
            Some(p) => p,
            None => return -1,
        };

        // Parse ring public keys and commitments
        let mut ring_keys_parsed: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        let mut ring_commits_parsed: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        
        for i in 0..ring_size {
            let pk_bytes = &ring_key_bytes[i * 32..(i + 1) * 32];
            let c_bytes = &ring_commit_bytes[i * 32..(i + 1) * 32];
            
            let pk = match CompressedRistretto::from_slice(pk_bytes)
                .expect("len")
                .decompress()
            {
                Some(p) => p,
                None => return -1,
            };
            
            let c = match CompressedRistretto::from_slice(c_bytes)
                .expect("len")
                .decompress()
            {
                Some(p) => p,
                None => return -1,
            };
            
            ring_keys_parsed.push(pk);
            ring_commits_parsed.push(c);
        }

        // Verify our public key matches
        let our_pubkey = &x * RISTRETTO_BASEPOINT_TABLE;
        if ring_keys_parsed[secret_index].compress() != our_pubkey.compress() {
            return -3;
        }

        // Compute key image: I = x * Hp(P)
        let hp_secret = hash_to_point(ring_keys_parsed[secret_index].compress().as_bytes());
        let key_image = x * hp_secret;

        // Precompute commitment differences: D_i = C_pseudo - C_i
        // At secret_index, D_π = z * B_blinding (since amounts are equal, only blinding differs)
        let commit_diffs: Vec<RistrettoPoint> = ring_commits_parsed
            .iter()
            .map(|c_i| c_pseudo - c_i)
            .collect();

        // Get the blinding generator (must match commitment module)
        let b_blinding = blinding_generator();

        // Generate random nonces
        let alpha = Scalar::random(&mut rand::thread_rng()); // For key proof
        let alpha_z = Scalar::random(&mut rand::thread_rng()); // For commitment proof

        // Generate random responses for all except secret_index
        let mut s: Vec<Scalar> = (0..ring_size)
            .map(|_| Scalar::random(&mut rand::thread_rng()))
            .collect();
        let mut t: Vec<Scalar> = (0..ring_size)
            .map(|_| Scalar::random(&mut rand::thread_rng()))
            .collect();

        // Compute challenges
        let mut c_arr: Vec<Scalar> = vec![Scalar::ZERO; ring_size];

        // Start: L_π = α*G, R_π = α*Hp(P_π), D_π = α_z*B_blinding
        let l_start = &alpha * RISTRETTO_BASEPOINT_TABLE;
        let r_start = alpha * hp_secret;
        let d_start = alpha_z * b_blinding;

        // c_{π+1} = H(m, L_π, R_π, D_π)
        let mut next_idx = (secret_index + 1) % ring_size;
        c_arr[next_idx] = ringct_challenge_hash(msg, &l_start, &r_start, &d_start);

        // Go around the ring
        for _ in 0..(ring_size - 1) {
            let hp_i = hash_to_point(ring_keys_parsed[next_idx].compress().as_bytes());

            // L_i = s_i*G + c_i*P_i
            let l_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[next_idx], c_arr[next_idx]],
                &[RISTRETTO_BASEPOINT_POINT, ring_keys_parsed[next_idx]],
            );

            // R_i = s_i*Hp(P_i) + c_i*I
            let r_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[next_idx], c_arr[next_idx]],
                &[hp_i, key_image],
            );

            // D_i = t_i*B_blinding + c_i*(C_pseudo - C_i)
            let d_i = RistrettoPoint::vartime_multiscalar_mul(
                &[t[next_idx], c_arr[next_idx]],
                &[b_blinding, commit_diffs[next_idx]],
            );

            let after_idx = (next_idx + 1) % ring_size;
            c_arr[after_idx] = ringct_challenge_hash(msg, &l_i, &r_i, &d_i);
            next_idx = after_idx;
        }

        // Close the ring
        s[secret_index] = alpha - c_arr[secret_index] * x;
        t[secret_index] = alpha_z - c_arr[secret_index] * z;

        // Build signature: c_0 || s_0..s_{n-1} || t_0..t_{n-1} || key_image || pseudo_output
        // Total: 32 + n*32 + n*32 + 32 + 32 = 96 + 64*n bytes
        let sig_len = 32 + ring_size * 32 + ring_size * 32 + 32 + 32;
        let out = slice::from_raw_parts_mut(signature_out, sig_len);

        let mut offset = 0;
        
        // c_0
        out[offset..offset + 32].copy_from_slice(c_arr[0].as_bytes());
        offset += 32;
        
        // s values
        for i in 0..ring_size {
            out[offset..offset + 32].copy_from_slice(s[i].as_bytes());
            offset += 32;
        }
        
        // t values
        for i in 0..ring_size {
            out[offset..offset + 32].copy_from_slice(t[i].as_bytes());
            offset += 32;
        }
        
        // key image
        out[offset..offset + 32].copy_from_slice(key_image.compress().as_bytes());
        offset += 32;
        
        // pseudo-output (included so verifier has it)
        out[offset..offset + 32].copy_from_slice(c_pseudo.compress().as_bytes());

        *signature_len_out = sig_len;
    }

    0
}

/// Verify a RingCT CLSAG signature
///
/// Verifies both key ownership AND commitment linking.
/// The pseudo-output is extracted from the signature.
#[no_mangle]
pub extern "C" fn blocknet_ringct_verify(
    ring_keys: *const u8,
    ring_commitments: *const u8,
    ring_size: usize,
    message: *const u8,
    message_len: usize,
    signature: *const u8,
    signature_len: usize,
) -> i32 {
    if ring_keys.is_null()
        || ring_commitments.is_null()
        || message.is_null()
        || signature.is_null()
    {
        return -1;
    }

    let expected_len = 32 + ring_size * 32 + ring_size * 32 + 32 + 32;
    if signature_len != expected_len || ring_size != RING_SIZE {
        return -2;
    }

    unsafe {
        let ring_key_bytes = slice::from_raw_parts(ring_keys, ring_size * 32);
        let ring_commit_bytes = slice::from_raw_parts(ring_commitments, ring_size * 32);
        let msg = slice::from_raw_parts(message, message_len);
        let sig = slice::from_raw_parts(signature, signature_len);

        // Parse ring
        let mut ring_keys_parsed: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        let mut ring_commits_parsed: Vec<RistrettoPoint> = Vec::with_capacity(ring_size);
        
        for i in 0..ring_size {
            let pk_bytes = &ring_key_bytes[i * 32..(i + 1) * 32];
            let c_bytes = &ring_commit_bytes[i * 32..(i + 1) * 32];
            
            let pk = match CompressedRistretto::from_slice(pk_bytes)
                .expect("len")
                .decompress()
            {
                Some(p) => p,
                None => return -1,
            };
            
            let c = match CompressedRistretto::from_slice(c_bytes)
                .expect("len")
                .decompress()
            {
                Some(p) => p,
                None => return -1,
            };
            
            ring_keys_parsed.push(pk);
            ring_commits_parsed.push(c);
        }

        // Parse signature
        let mut offset = 0;
        
        // c_0
        let c0 = match Scalar::from_canonical_bytes(sig[offset..offset + 32].try_into().expect("len"))
            .into_option()
        {
            Some(s) => s,
            None => return -1,
        };
        offset += 32;

        // s values
        let mut s: Vec<Scalar> = Vec::with_capacity(ring_size);
        for _ in 0..ring_size {
            match Scalar::from_canonical_bytes(sig[offset..offset + 32].try_into().expect("len"))
                .into_option()
            {
                Some(scalar) => s.push(scalar),
                None => return -1,
            }
            offset += 32;
        }

        // t values
        let mut t: Vec<Scalar> = Vec::with_capacity(ring_size);
        for _ in 0..ring_size {
            match Scalar::from_canonical_bytes(sig[offset..offset + 32].try_into().expect("len"))
                .into_option()
            {
                Some(scalar) => t.push(scalar),
                None => return -1,
            }
            offset += 32;
        }

        // key image
        let key_image = match CompressedRistretto::from_slice(&sig[offset..offset + 32])
            .expect("len")
            .decompress()
        {
            Some(p) => p,
            None => return -1,
        };
        offset += 32;

        // pseudo-output
        let c_pseudo = match CompressedRistretto::from_slice(&sig[offset..offset + 32])
            .expect("len")
            .decompress()
        {
            Some(p) => p,
            None => return -1,
        };

        // Precompute commitment differences
        let commit_diffs: Vec<RistrettoPoint> = ring_commits_parsed
            .iter()
            .map(|c_i| c_pseudo - c_i)
            .collect();

        // Get the blinding generator (must match commitment module)
        let b_blinding = blinding_generator();

        // Verify ring
        let mut c = c0;
        for i in 0..ring_size {
            let hp_i = hash_to_point(ring_keys_parsed[i].compress().as_bytes());

            // L_i = s_i*G + c_i*P_i
            let l_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[i], c],
                &[RISTRETTO_BASEPOINT_POINT, ring_keys_parsed[i]],
            );

            // R_i = s_i*Hp(P_i) + c_i*I
            let r_i = RistrettoPoint::vartime_multiscalar_mul(
                &[s[i], c],
                &[hp_i, key_image],
            );

            // D_i = t_i*B_blinding + c_i*(C_pseudo - C_i)
            let d_i = RistrettoPoint::vartime_multiscalar_mul(
                &[t[i], c],
                &[b_blinding, commit_diffs[i]],
            );

            c = ringct_challenge_hash(msg, &l_i, &r_i, &d_i);
        }

        // Ring should close
        if c == c0 {
            0
        } else {
            -1
        }
    }
}

/// Extract key image from RingCT signature
#[no_mangle]
pub extern "C" fn blocknet_ringct_key_image(
    signature: *const u8,
    ring_size: usize,
    key_image_out: *mut u8,
) -> i32 {
    if signature.is_null() || key_image_out.is_null() {
        return -1;
    }

    // Key image is at offset: 32 + ring_size*32 + ring_size*32
    let ki_offset = 32 + ring_size * 32 + ring_size * 32;

    unsafe {
        let sig = slice::from_raw_parts(signature, ki_offset + 32);
        let out = slice::from_raw_parts_mut(key_image_out, 32);
        out.copy_from_slice(&sig[ki_offset..ki_offset + 32]);
    }

    0
}

/// Extract pseudo-output from RingCT signature
#[no_mangle]
pub extern "C" fn blocknet_ringct_pseudo_output(
    signature: *const u8,
    ring_size: usize,
    pseudo_out: *mut u8,
) -> i32 {
    if signature.is_null() || pseudo_out.is_null() {
        return -1;
    }

    // Pseudo-output is at the end
    let po_offset = 32 + ring_size * 32 + ring_size * 32 + 32;

    unsafe {
        let sig = slice::from_raw_parts(signature, po_offset + 32);
        let out = slice::from_raw_parts_mut(pseudo_out, 32);
        out.copy_from_slice(&sig[po_offset..po_offset + 32]);
    }

    0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn generate_keypair() -> (Scalar, RistrettoPoint) {
        let privkey = Scalar::random(&mut rand::thread_rng());
        let pubkey = &privkey * RISTRETTO_BASEPOINT_TABLE;
        (privkey, pubkey)
    }

    #[test]
    fn test_key_image() {
        let (privkey, _pubkey) = generate_keypair();
        let mut key_image = [0u8; 32];

        let result = blocknet_key_image(privkey.as_bytes().as_ptr(), key_image.as_mut_ptr());
        assert_eq!(result, 0);
        assert_ne!(key_image, [0u8; 32]);

        // Same key should produce same image
        let mut key_image2 = [0u8; 32];
        blocknet_key_image(privkey.as_bytes().as_ptr(), key_image2.as_mut_ptr());
        assert_eq!(key_image, key_image2);
    }

    #[test]
    fn test_clsag_sign_verify() {
        // Generate ring with fixed size
        let ring_size = RING_SIZE;
        let secret_index = 7;

        let mut keypairs: Vec<(Scalar, RistrettoPoint)> = Vec::new();
        for _ in 0..ring_size {
            keypairs.push(generate_keypair());
        }

        // Build ring bytes
        let mut ring_bytes = vec![0u8; ring_size * 32];
        for (i, (_, pubkey)) in keypairs.iter().enumerate() {
            ring_bytes[i * 32..(i + 1) * 32].copy_from_slice(pubkey.compress().as_bytes());
        }

        let message = b"Test transaction";
        let mut signature = vec![0u8; 32 + ring_size * 32 + 32];
        let mut sig_len: usize = 0;

        // Sign
        let sign_result = blocknet_clsag_sign(
            ring_bytes.as_ptr(),
            ring_size,
            secret_index,
            keypairs[secret_index].0.as_bytes().as_ptr(),
            message.as_ptr(),
            message.len(),
            signature.as_mut_ptr(),
            &mut sig_len,
        );
        assert_eq!(sign_result, 0);
        assert_eq!(sig_len, 32 + ring_size * 32 + 32);

        // Verify
        let verify_result = blocknet_clsag_verify(
            ring_bytes.as_ptr(),
            ring_size,
            message.as_ptr(),
            message.len(),
            signature.as_ptr(),
            sig_len,
        );
        assert_eq!(verify_result, 0);

        // Wrong message should fail
        let wrong_message = b"Wrong message!!!";
        let wrong_verify = blocknet_clsag_verify(
            ring_bytes.as_ptr(),
            ring_size,
            wrong_message.as_ptr(),
            wrong_message.len(),
            signature.as_ptr(),
            sig_len,
        );
        assert_eq!(wrong_verify, -1);
    }

    #[test]
    fn test_different_ring_positions() {
        let ring_size = RING_SIZE;

        // Test signing from each position
        for secret_index in 0..ring_size {
            let mut keypairs: Vec<(Scalar, RistrettoPoint)> = Vec::new();
            for _ in 0..ring_size {
                keypairs.push(generate_keypair());
            }

            let mut ring_bytes = vec![0u8; ring_size * 32];
            for (i, (_, pubkey)) in keypairs.iter().enumerate() {
                ring_bytes[i * 32..(i + 1) * 32].copy_from_slice(pubkey.compress().as_bytes());
            }

            let message = b"Test";
            let mut signature = vec![0u8; 32 + ring_size * 32 + 32];
            let mut sig_len: usize = 0;

            let sign_result = blocknet_clsag_sign(
                ring_bytes.as_ptr(),
                ring_size,
                secret_index,
                keypairs[secret_index].0.as_bytes().as_ptr(),
                message.as_ptr(),
                message.len(),
                signature.as_mut_ptr(),
                &mut sig_len,
            );
            assert_eq!(sign_result, 0, "Failed to sign at index {}", secret_index);

            let verify_result = blocknet_clsag_verify(
                ring_bytes.as_ptr(),
                ring_size,
                message.as_ptr(),
                message.len(),
                signature.as_ptr(),
                sig_len,
            );
            assert_eq!(
                verify_result, 0,
                "Failed to verify at index {}",
                secret_index
            );
        }
    }

    #[test]
    fn test_ringct_sign_verify() {
        // Generate ring with fixed size
        let ring_size = RING_SIZE;
        let secret_index = 5;
        let amount = 1000u64;

        // Use bulletproofs generators (must match commitment module)
        let pc_gens = PedersenGens::default();
        
        let mut keypairs: Vec<(Scalar, RistrettoPoint)> = Vec::new();
        let mut commitments: Vec<(Scalar, RistrettoPoint)> = Vec::new(); // (blinding, commitment)
        
        for i in 0..ring_size {
            // Generate key pair
            let (priv_key, pub_key) = generate_keypair();
            keypairs.push((priv_key, pub_key));
            
            // Generate commitment with random blinding
            let blinding = Scalar::random(&mut rand::thread_rng());
            // For secret_index, use the real amount; others can have different amounts
            let val = if i == secret_index { 
                Scalar::from(amount) 
            } else { 
                Scalar::from(5000u64 + i as u64 * 1000) // Different amounts for decoys
            };
            // Commitment = value * B + blinding * B_blinding
            let commitment = pc_gens.commit(val, blinding);
            commitments.push((blinding, commitment));
        }

        // Create pseudo-output with same amount but different blinding
        let pseudo_blinding = Scalar::random(&mut rand::thread_rng());
        let pseudo_output = pc_gens.commit(Scalar::from(amount), pseudo_blinding);

        // Build ring bytes
        let mut ring_key_bytes = vec![0u8; ring_size * 32];
        let mut ring_commit_bytes = vec![0u8; ring_size * 32];
        for i in 0..ring_size {
            ring_key_bytes[i * 32..(i + 1) * 32].copy_from_slice(keypairs[i].1.compress().as_bytes());
            ring_commit_bytes[i * 32..(i + 1) * 32].copy_from_slice(commitments[i].1.compress().as_bytes());
        }

        let message = b"Test RingCT transaction";
        let sig_len = 32 + ring_size * 32 + ring_size * 32 + 32 + 32;
        let mut signature = vec![0u8; sig_len];
        let mut actual_sig_len: usize = 0;

        // Sign
        let sign_result = blocknet_ringct_sign(
            ring_key_bytes.as_ptr(),
            ring_commit_bytes.as_ptr(),
            ring_size,
            secret_index,
            keypairs[secret_index].0.as_bytes().as_ptr(),
            commitments[secret_index].0.as_bytes().as_ptr(),
            pseudo_output.compress().as_bytes().as_ptr(),
            pseudo_blinding.as_bytes().as_ptr(),
            message.as_ptr(),
            message.len(),
            signature.as_mut_ptr(),
            &mut actual_sig_len,
        );
        assert_eq!(sign_result, 0, "Signing failed");
        assert_eq!(actual_sig_len, sig_len);

        // Verify
        let verify_result = blocknet_ringct_verify(
            ring_key_bytes.as_ptr(),
            ring_commit_bytes.as_ptr(),
            ring_size,
            message.as_ptr(),
            message.len(),
            signature.as_ptr(),
            actual_sig_len,
        );
        assert_eq!(verify_result, 0, "Verification failed");

        // Wrong message should fail
        let wrong_message = b"Wrong message!";
        let wrong_verify = blocknet_ringct_verify(
            ring_key_bytes.as_ptr(),
            ring_commit_bytes.as_ptr(),
            ring_size,
            wrong_message.as_ptr(),
            wrong_message.len(),
            signature.as_ptr(),
            actual_sig_len,
        );
        assert_eq!(wrong_verify, -1, "Should fail on wrong message");
    }

    #[test]
    fn test_ringct_wrong_amount_fails() {
        // Try to create a signature where pseudo-output has different amount
        // This should create an invalid signature
        let ring_size = RING_SIZE;
        let secret_index = 3;
        let real_amount = 1000u64;
        let fake_amount = 9999u64; // Trying to inflate!

        // Use bulletproofs generators (must match commitment module)
        let pc_gens = PedersenGens::default();
        
        let mut keypairs: Vec<(Scalar, RistrettoPoint)> = Vec::new();
        let mut commitments: Vec<(Scalar, RistrettoPoint)> = Vec::new();
        
        for i in 0..ring_size {
            let (priv_key, pub_key) = generate_keypair();
            keypairs.push((priv_key, pub_key));
            
            let blinding = Scalar::random(&mut rand::thread_rng());
            let val = if i == secret_index { 
                Scalar::from(real_amount) 
            } else { 
                Scalar::from(5000u64)
            };
            let commitment = pc_gens.commit(val, blinding);
            commitments.push((blinding, commitment));
        }

        // Create FRAUDULENT pseudo-output with DIFFERENT amount
        let pseudo_blinding = Scalar::random(&mut rand::thread_rng());
        let pseudo_output = pc_gens.commit(Scalar::from(fake_amount), pseudo_blinding);

        let mut ring_key_bytes = vec![0u8; ring_size * 32];
        let mut ring_commit_bytes = vec![0u8; ring_size * 32];
        for i in 0..ring_size {
            ring_key_bytes[i * 32..(i + 1) * 32].copy_from_slice(keypairs[i].1.compress().as_bytes());
            ring_commit_bytes[i * 32..(i + 1) * 32].copy_from_slice(commitments[i].1.compress().as_bytes());
        }

        let message = b"Fraudulent transaction";
        let sig_len = 32 + ring_size * 32 + ring_size * 32 + 32 + 32;
        let mut signature = vec![0u8; sig_len];
        let mut actual_sig_len: usize = 0;

        // Sign (will succeed - signing doesn't check amount equality)
        let sign_result = blocknet_ringct_sign(
            ring_key_bytes.as_ptr(),
            ring_commit_bytes.as_ptr(),
            ring_size,
            secret_index,
            keypairs[secret_index].0.as_bytes().as_ptr(),
            commitments[secret_index].0.as_bytes().as_ptr(),
            pseudo_output.compress().as_bytes().as_ptr(),
            pseudo_blinding.as_bytes().as_ptr(),
            message.as_ptr(),
            message.len(),
            signature.as_mut_ptr(),
            &mut actual_sig_len,
        );
        assert_eq!(sign_result, 0);

        // But verification MUST FAIL because the commitment difference won't match
        let verify_result = blocknet_ringct_verify(
            ring_key_bytes.as_ptr(),
            ring_commit_bytes.as_ptr(),
            ring_size,
            message.as_ptr(),
            message.len(),
            signature.as_ptr(),
            actual_sig_len,
        );
        assert_eq!(verify_result, -1, "Fraudulent amount must be rejected!");
    }

    #[test]
    fn test_key_image_linkability() {
        let (privkey, pubkey) = generate_keypair();

        // Create two different rings with same signer
        let ring_size = RING_SIZE;

        // Ring 1
        let mut ring1: Vec<RistrettoPoint> = vec![pubkey];
        for _ in 1..ring_size {
            ring1.push(generate_keypair().1);
        }

        // Ring 2 (different decoys)
        let mut ring2: Vec<RistrettoPoint> = vec![pubkey];
        for _ in 1..ring_size {
            ring2.push(generate_keypair().1);
        }

        let mut ring1_bytes = vec![0u8; ring_size * 32];
        let mut ring2_bytes = vec![0u8; ring_size * 32];
        for i in 0..ring_size {
            ring1_bytes[i * 32..(i + 1) * 32].copy_from_slice(ring1[i].compress().as_bytes());
            ring2_bytes[i * 32..(i + 1) * 32].copy_from_slice(ring2[i].compress().as_bytes());
        }

        let message = b"Transaction";
        let mut sig1 = vec![0u8; 32 + ring_size * 32 + 32];
        let mut sig2 = vec![0u8; 32 + ring_size * 32 + 32];
        let mut len1: usize = 0;
        let mut len2: usize = 0;

        // Sign with both rings
        blocknet_clsag_sign(
            ring1_bytes.as_ptr(),
            ring_size,
            0,
            privkey.as_bytes().as_ptr(),
            message.as_ptr(),
            message.len(),
            sig1.as_mut_ptr(),
            &mut len1,
        );

        blocknet_clsag_sign(
            ring2_bytes.as_ptr(),
            ring_size,
            0,
            privkey.as_bytes().as_ptr(),
            message.as_ptr(),
            message.len(),
            sig2.as_mut_ptr(),
            &mut len2,
        );

        // Extract key images
        let mut ki1 = [0u8; 32];
        let mut ki2 = [0u8; 32];
        blocknet_clsag_key_image(sig1.as_ptr(), ring_size, ki1.as_mut_ptr());
        blocknet_clsag_key_image(sig2.as_ptr(), ring_size, ki2.as_mut_ptr());

        // Key images should be identical! (linkability)
        assert_eq!(ki1, ki2);
    }
}

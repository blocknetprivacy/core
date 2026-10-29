//! Blocknet Crypto Library
//! 
//! Privacy-focused cryptographic primitives for the Blocknet blockchain.
//! All functions are exported via C FFI for use from Go.
//!
//! ## Modules
//! - `keys`: Ed25519 key generation, signing, verification
//! - `commitment`: Pedersen commitments and arithmetic
//! - `rangeproof`: Bulletproofs range proofs
//! - `stealth`: Stealth addresses (receiver privacy)
//! - `ring`: CLSAG ring signatures and RingCT (sender privacy + amount hiding)
//! - `hash`: SHA256/SHA3 hashing
//! - `pow`: Argon2id proof of work (ASIC-resistant mining)

mod commitment;
mod hash;
mod keys;
mod pow;
mod pow_memory;
mod rangeproof;
mod ring;
mod stealth;

// Re-export all FFI functions
pub use commitment::*;
pub use hash::*;
pub use keys::*;
pub use pow::*;
pub use rangeproof::*;
pub use ring::*;
pub use stealth::*;

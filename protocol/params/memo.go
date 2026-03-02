package params

// Protocol-level memo constants shared by consensus and wallet code.
//
// Keep these out of the wallet package to avoid consensus-layer coupling to
// wallet refactors.
const (
	// MemoSize is the fixed encrypted memo size per output (bytes).
	MemoSize = 128

	// MemoEnvelopeVersion is the version byte of the plaintext memo envelope
	// before encryption (wallet-visible only; consensus treats ciphertext as opaque).
	MemoEnvelopeVersion = byte(0x01)

	// MemoPayloadMax is the max plaintext payload length (bytes) inside the envelope.
	// Layout: version(1) + length(1) + checksum(2) + payload(n) + padding(...)
	MemoPayloadMax = 124
)

// MemoBlockDomainSep is a public domain separator string for memo KDFs.
// Overwritten by InitTestnet().
var MemoBlockDomainSep = NetworkID


package params

// IsTestnet is true when the daemon is running in testnet mode.
var IsTestnet bool

// NetworkID is a public network identifier used as a domain separator in
// wallet/protocol constructions (e.g. memo KDFs, address checksums).
var NetworkID = "blocknet_mainnet"

// ChainID is a fixed relaunch epoch identifier used in P2P status handshakes.
// It is intentionally a constant (not derived) for auditability and to avoid
// accidental changes if genesis mechanics are refactored later.
var ChainID uint32 = 0x20260215

// InitTestnet switches all network parameters to testnet values.
// Must be called before any other package reads these variables.
func InitTestnet() {
	IsTestnet = true
	NetworkID = "blocknet_testnet"
	ChainID = 0x20260216

	P2PProtocolBase = "/blocknet/testnet"
	ProtocolPEX = P2PProtocolBase + "/pex/1.0.0"
	ProtocolBlock = P2PProtocolBase + "/block/1.0.0"
	ProtocolTx = P2PProtocolBase + "/tx/1.0.0"
	ProtocolSync = P2PProtocolBase + "/sync/1.0.0"
	ProtocolDandelion = P2PProtocolBase + "/dandelion/1.0.0"

	MemoBlockDomainSep = NetworkID
}

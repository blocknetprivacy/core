package params

// P2P protocol identifiers (libp2p stream protocol IDs).
//
// These are string variables (not protocol.ID) so the params package stays
// dependency-light and can be reused across layers. Overwritten by InitTestnet().
var (
	P2PProtocolBase = "/blocknet/mainnet"

	ProtocolPEX       = P2PProtocolBase + "/pex/1.0.0"
	ProtocolBlock     = P2PProtocolBase + "/block/1.0.0"
	ProtocolTx        = P2PProtocolBase + "/tx/1.0.0"
	ProtocolSync      = P2PProtocolBase + "/sync/1.0.0"
	ProtocolDandelion = P2PProtocolBase + "/dandelion/1.0.0"
)

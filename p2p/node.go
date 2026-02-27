package p2p

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
)

// Protocol IDs for blocknet
const (
	ProtocolPEX       protocol.ID = params.ProtocolPEX
	ProtocolBlock     protocol.ID = params.ProtocolBlock
	ProtocolTx        protocol.ID = params.ProtocolTx
	ProtocolSync      protocol.ID = params.ProtocolSync
	ProtocolDandelion protocol.ID = params.ProtocolDandelion
)

// NodeConfig configures the P2P node
type NodeConfig struct {
	// ListenAddrs are the multiaddrs to listen on
	// Default: ["/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0"]
	ListenAddrs []string

	// SeedNodes are bootstrap peers to connect to initially
	SeedNodes []string

	// MaxInbound is the maximum number of inbound connections
	MaxInbound int

	// MaxOutbound is the maximum number of outbound connections
	MaxOutbound int

	// IdentityConfig for peer ID management
	Identity IdentityConfig

	// UserAgent is announced to peers
	UserAgent string
}

// DefaultNodeConfig returns sensible defaults
func DefaultNodeConfig() NodeConfig {
	return NodeConfig{
		ListenAddrs: []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip6/::/tcp/0",
		},
		SeedNodes:   []string{},
		MaxInbound:  64,
		MaxOutbound: 16,
		Identity:    DefaultIdentityConfig(),
		UserAgent:   "blocknet",
	}
}

// Node represents a P2P network node
type Node struct {
	mu sync.RWMutex

	host     host.Host
	identity *IdentityManager
	pex      *PeerExchange
	dandel   *DandelionRouter
	config   NodeConfig

	// Handlers for received messages
	onBlock func(from peer.ID, data []byte)
	onTx    func(from peer.ID, data []byte)

	// Pending identity after rotation (applied on restart)
	pendingKey crypto.PrivKey
	pendingID  peer.ID

	// Lifecycle
	ctx       context.Context
	cancel    context.CancelFunc
	stopFuncs []func()
}

// IsBanned checks if a peer is banned
func (n *Node) IsBanned(pid peer.ID) bool {
	if n.pex == nil {
		return false
	}
	return n.pex.IsBanned(pid)
}

// BanPeer bans a peer for misbehavior
func (n *Node) BanPeer(pid peer.ID, reason string) {
	if n.pex == nil {
		return
	}
	n.pex.BanPeer(pid, reason, BanDurationMedium)
}

// PenalizePeer reduces a peer's reputation score
func (n *Node) PenalizePeer(pid peer.ID, penalty int, reason string) {
	if n.pex == nil {
		return
	}
	n.pex.PenalizePeer(pid, penalty, reason)
}

// RewardPeer increases a peer's reputation score
func (n *Node) RewardPeer(pid peer.ID) {
	if n.pex == nil {
		return
	}
	n.pex.RewardPeer(pid)
}

// GetBannedPeers returns the list of currently banned peers
func (n *Node) GetBannedPeers() []*BanRecord {
	if n.pex == nil {
		return nil
	}
	return n.pex.GetBannedPeers()
}

// BannedCount returns the number of banned peers
func (n *Node) BannedCount() int {
	if n.pex == nil {
		return 0
	}
	return n.pex.BannedPeerCount()
}

// NewNode creates and starts a new P2P node
func NewNode(cfg NodeConfig) (*Node, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Create identity manager
	identity, err := NewIdentityManager(cfg.Identity)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create identity: %w", err)
	}

	// Get current identity
	privKey, _ := identity.CurrentIdentity()

	// Parse listen addresses
	listenAddrs := make([]multiaddr.Multiaddr, 0, len(cfg.ListenAddrs))
	for _, addr := range cfg.ListenAddrs {
		ma, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("invalid listen address %s: %w", addr, err)
		}
		listenAddrs = append(listenAddrs, ma)
	}

	// Create connection manager with limits
	connMgr, err := connmgr.NewConnManager(
		cfg.MaxOutbound,                // low water
		cfg.MaxInbound+cfg.MaxOutbound, // high water
		connmgr.WithGracePeriod(time.Minute),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create connection manager: %w", err)
	}

	// Create the node first (we need a reference for the gater)
	node := &Node{
		identity: identity,
		config:   cfg,
		ctx:      ctx,
		cancel:   cancel,
	}

	// Create peer exchange before host so ban state is available to connection gating.
	node.pex = NewPeerExchange(node, cfg.SeedNodes)
	banGater := NewBanGater(func(pid peer.ID) bool {
		return node.IsBanned(pid)
	})

	// Create libp2p host with banned-peer admission gating enabled.
	h, err := libp2p.New(
		libp2p.Identity(privKey),
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.ConnectionManager(connMgr),
		libp2p.ConnectionGater(banGater),
		libp2p.UserAgent(cfg.UserAgent),
		// Enable NAT port mapping
		libp2p.NATPortMap(),
		// Enable hole punching for NAT traversal
		libp2p.EnableHolePunching(),
		// Disable relay (we don't want to route others' traffic)
		libp2p.DisableRelay(),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	node.host = h

	// Set up identity rotation callback
	identity.SetRotationCallback(func(newKey crypto.PrivKey, newID peer.ID) {
		
		node.mu.Lock()
		node.pendingKey = newKey
		node.pendingID = newID
		node.mu.Unlock()
	})

	// Create Dandelion router
	node.dandel = NewDandelionRouter(node)

	// Register protocol handlers
	node.registerProtocols()

	// Start background tasks
	node.stopFuncs = append(node.stopFuncs, identity.StartRotationLoop())

	return node, nil
}

// registerProtocols sets up stream handlers for all protocols
func (n *Node) registerProtocols() {
	// PEX protocol
	n.host.SetStreamHandler(ProtocolPEX, n.pex.HandleStream)

	// Block announcement protocol
	n.host.SetStreamHandler(ProtocolBlock, n.handleBlockStream)

	// Transaction protocol (fluff phase ingress)
	n.host.SetStreamHandler(ProtocolTx, n.handleTxStream)

	// Dandelion stem protocol
	n.host.SetStreamHandler(ProtocolDandelion, n.dandel.HandleStemStream)

	// Sync protocol
	n.host.SetStreamHandler(ProtocolSync, n.handleSyncStream)
}

// handleBlockStream handles incoming block announcements
func (n *Node) handleBlockStream(s network.Stream) {
	defer s.Close()
	if err := s.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}

	// Read block data
	data, err := readLengthPrefixedWithLimit(s, MaxBlockStreamPayloadSize)
	if err != nil {
		return
	}

	n.mu.RLock()
	handler := n.onBlock
	n.mu.RUnlock()

	if handler != nil {
		handler(s.Conn().RemotePeer(), data)
	}
}

// handleTxStream handles incoming transaction announcements (fluff phase).
// All ProtocolTx ingress must pass through Dandelion fluff semantics.
func (n *Node) handleTxStream(s network.Stream) {
	defer s.Close()
	if err := s.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}

	data, err := readLengthPrefixedWithLimit(s, MaxTxStreamPayloadSize)
	if err != nil {
		return
	}

	n.dandel.HandleFluffTx(s.Conn().RemotePeer(), data)
}

// handleSyncStream handles chain sync requests
func (n *Node) handleSyncStream(s network.Stream) {
	defer s.Close()
	// Sync protocol will be implemented with the sync handler
}

// Start begins P2P operations (connecting to seeds, etc.)
func (n *Node) Start() error {
	// Start peer exchange (connects to seeds)
	if err := n.pex.Start(n.ctx); err != nil {
		return fmt.Errorf("failed to start peer exchange: %w", err)
	}

	// Start Dandelion router
	n.dandel.Start(n.ctx)

	return nil
}

// Stop gracefully shuts down the node
func (n *Node) Stop() error {
	// Cancel context
	n.cancel()

	// Run all stop functions
	for _, stop := range n.stopFuncs {
		stop()
	}

	// Stop Dandelion
	n.dandel.Stop()

	// Stop PEX
	n.pex.Stop()

	// Close host
	return n.host.Close()
}

// Host returns the underlying libp2p host
func (n *Node) Host() host.Host {
	return n.host
}

// PeerID returns the current peer ID
func (n *Node) PeerID() peer.ID {
	return n.identity.CurrentPeerID()
}

// Addrs returns the listen addresses
func (n *Node) Addrs() []multiaddr.Multiaddr {
	return n.host.Addrs()
}

// Peers returns connected peer IDs
func (n *Node) Peers() []peer.ID {
	return n.host.Network().Peers()
}

// PeerInfo holds a connected peer's ID and observed addresses.
type PeerInfo struct {
	ID    peer.ID
	Addrs []string
}

// PeerInfos returns connected peers with their remote multiaddrs.
func (n *Node) PeerInfos() []PeerInfo {
	pids := n.host.Network().Peers()
	out := make([]PeerInfo, len(pids))
	for i, pid := range pids {
		info := PeerInfo{ID: pid}
		for _, conn := range n.host.Network().ConnsToPeer(pid) {
			info.Addrs = append(info.Addrs, conn.RemoteMultiaddr().String())
		}
		out[i] = info
	}
	return out
}

// Connect attempts to connect to a peer
func (n *Node) Connect(ctx context.Context, pi peer.AddrInfo) error {
	return n.host.Connect(ctx, pi)
}

// SetBlockHandler sets the callback for received blocks
func (n *Node) SetBlockHandler(handler func(from peer.ID, data []byte)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onBlock = handler
}

// SetTxHandler sets the callback for received transactions
func (n *Node) SetTxHandler(handler func(from peer.ID, data []byte)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onTx = handler

	// Also set on Dandelion router
	n.dandel.SetTxHandler(handler)
}

// SetStemSanityValidator sets a lightweight stem-phase transaction validator.
// Invalid stem payloads are rejected before cache/relay in Dandelion.
func (n *Node) SetStemSanityValidator(validator func(data []byte) bool) {
	n.dandel.SetStemSanityValidator(validator)
}

// BroadcastBlock sends a block to all connected peers
func (n *Node) BroadcastBlock(data []byte) {
	peers := n.host.Network().Peers()
	for _, p := range peers {
		n.sendToPeerAsync(p, ProtocolBlock, data)
	}
}

// RelayBlock relays a block to all peers except the sender
func (n *Node) RelayBlock(sender peer.ID, data []byte) {
	peers := n.host.Network().Peers()
	for _, p := range peers {
		if p != sender {
			n.sendToPeerAsync(p, ProtocolBlock, data)
		}
	}
}

// BroadcastTx broadcasts a transaction via Dandelion++
// This is the main entry point for sending transactions
func (n *Node) BroadcastTx(data []byte) {
	n.dandel.BroadcastTx(data)
}

// sendToPeer sends data to a specific peer using the given protocol
func (n *Node) sendToPeer(p peer.ID, proto protocol.ID, data []byte) error {
	ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
	defer cancel()

	s, err := n.host.NewStream(ctx, p, proto)
	if err != nil {
		return err
	}
	defer s.Close()

	return writeLengthPrefixed(s, data)
}

func (n *Node) sendToPeerAsync(p peer.ID, proto protocol.ID, data []byte) {
	go func(pid peer.ID, pr protocol.ID, payload []byte) {
		if err := n.sendToPeer(pid, pr, payload); err != nil {
			if strings.Contains(err.Error(), "protocols not supported") {
				return
			}
			log.Printf("failed to send %s message to peer %s: %v", pr, pid, err)
		}
	}(p, proto, data)
}

// IdentityAge returns how long the current identity has been active
func (n *Node) IdentityAge() time.Duration {
	return n.identity.Age()
}

// RotateIdentity forces an identity rotation
// Note: This requires restarting connections
func (n *Node) RotateIdentity() (peer.ID, error) {
	return n.identity.Rotate()
}

// FullMultiaddrs returns the complete multiaddrs including peer ID
// These are the addresses other nodes need to connect to this node
func (n *Node) FullMultiaddrs() []string {
	pid := n.PeerID()
	addrs := n.Addrs()

	full := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		s := addr.String()
		// Skip localhost addresses for external sharing
		if strings.HasPrefix(s, "/ip4/127.") || strings.HasPrefix(s, "/ip6/::1") {
			continue
		}
		full = append(full, fmt.Sprintf("%s/p2p/%s", s, pid.String()))
	}
	return full
}

// WritePeerFile writes the node's multiaddrs to peer.txt for sharing
func (n *Node) WritePeerFile(filename string) error {
	addrs := n.FullMultiaddrs()
	if len(addrs) == 0 {
		return fmt.Errorf("no external addresses available")
	}

	content := ""
	for _, addr := range addrs {
		content += addr + "\n"
	}

	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		return err
	}

	return nil
}

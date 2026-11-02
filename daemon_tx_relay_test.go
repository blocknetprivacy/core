package main

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/p2p"
	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// zeroInputTx is the smallest payload that deserializes: no inputs, no outputs.
func zeroInputTx(key byte) []byte {
	tx := &Transaction{Version: 2, TxPublicKey: [32]byte{key}, Fee: 1}
	return tx.Serialize()
}

func mustStartRelayTestNode(t *testing.T) *p2p.Node {
	t.Helper()
	cfg := p2p.DefaultNodeConfig()
	cfg.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.SeedNodes = nil
	n, err := p2p.NewNode(cfg)
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}
	if err := n.Start(); err != nil {
		t.Fatalf("failed to start node: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop() })
	return n
}

func mustConnectRelayTestNodes(t *testing.T, from, to *p2p.Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := from.Connect(ctx, peer.AddrInfo{ID: to.PeerID(), Addrs: to.Addrs()}); err != nil {
		t.Fatalf("failed to connect nodes: %v", err)
	}
}

// mustStartTxInjector connects a bare libp2p host, standing in for any peer.
func mustStartTxInjector(t *testing.T, to *p2p.Node) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs, libp2p.DisableRelay())
	if err != nil {
		t.Fatalf("failed to create injector: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Connect(ctx, peer.AddrInfo{ID: to.PeerID(), Addrs: to.Addrs()}); err != nil {
		t.Fatalf("failed to connect injector: %v", err)
	}
	return h
}

func sendTxOverProtocol(t *testing.T, h host.Host, to peer.ID, proto string, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := h.NewStream(network.WithAllowLimitedConn(ctx, "test"), to, protocol.ID(proto))
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer s.Close()
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	if _, err := s.Write(append(size[:], data...)); err != nil {
		t.Fatalf("failed to write tx: %v", err)
	}
}

func newRelayTestDaemon(node *p2p.Node) *Daemon {
	// Isolate relay behavior from canonical ring-member/storage coupling.
	cfg := DefaultMempoolConfig()
	cfg.MinFeeRate = 0
	return &Daemon{
		node: node,
		mempool: NewMempool(cfg,
			func(_ [32]byte) bool { return false },
			func(_, _ [32]byte) bool { return true },
		),
	}
}

// relayTopology is injector -> relay -> neighbor, with real transaction handling
// on the relay and the neighbor.
type relayTopology struct {
	injector       host.Host
	relay          *p2p.Node
	neighbor       *p2p.Node
	neighborDaemon *Daemon
	neighborSaw    *atomic.Int32
}

func mustStartRelayTopology(t *testing.T) relayTopology {
	t.Helper()
	relay := mustStartRelayTestNode(t)
	neighbor := mustStartRelayTestNode(t)
	mustConnectRelayTestNodes(t, relay, neighbor)

	relay.SetTxHandler(newRelayTestDaemon(relay).handleTx)
	neighborDaemon := newRelayTestDaemon(neighbor)
	var saw atomic.Int32
	neighbor.SetTxHandler(func(from peer.ID, data []byte) bool {
		saw.Add(1)
		return neighborDaemon.handleTx(from, data)
	})
	return relayTopology{
		injector:       mustStartTxInjector(t, relay),
		relay:          relay,
		neighbor:       neighbor,
		neighborDaemon: neighborDaemon,
		neighborSaw:    &saw,
	}
}

func TestRejectedTxIsNotRelayedAndBansNobody(t *testing.T) {
	tampered := mustBuildValidRingCTBindingTestTx(t)
	tampered.Inputs[0].KeyImage[0] ^= 0x01

	payloads := map[string][]byte{
		"no inputs or outputs": zeroInputTx(1),
		"malformed":            []byte("not-a-valid-serialized-tx"),
		"fails validation":     tampered.Serialize(),
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			net := mustStartRelayTopology(t)
			sendTxOverProtocol(t, net.injector, net.relay.PeerID(), params.ProtocolTx, payload)

			time.Sleep(500 * time.Millisecond)
			if got := net.neighborSaw.Load(); got != 0 {
				t.Fatalf("the rejected transaction was relayed to the neighbor %d times", got)
			}
			if net.neighbor.IsBanned(net.relay.PeerID()) {
				t.Fatal("the neighbor banned the relay")
			}
			if net.relay.IsBanned(net.injector.ID()) {
				t.Fatal("the sender may be a relay on older software and must not be banned")
			}
			if net.neighbor.Host().Network().Connectedness(net.relay.PeerID()) != network.Connected {
				t.Fatal("the relay and neighbor lost their connection")
			}
		})
	}
}

func TestAcceptedTxIsRelayed(t *testing.T) {
	net := mustStartRelayTopology(t)
	tx := mustBuildValidRingCTBindingTestTx(t)
	sendTxOverProtocol(t, net.injector, net.relay.PeerID(), params.ProtocolTx, tx.Serialize())

	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to compute txid: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !net.neighborDaemon.mempool.HasTransaction(txID) {
		if time.Now().After(deadline) {
			t.Fatalf("the neighbor never received the accepted transaction (saw %d)", net.neighborSaw.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSubmittedTxReachesThePeerThroughTheStem(t *testing.T) {
	sender := mustStartRelayTestNode(t)
	receiver := mustStartRelayTestNode(t)
	mustConnectRelayTestNodes(t, sender, receiver)
	senderDaemon := newRelayTestDaemon(sender)
	receiverDaemon := newRelayTestDaemon(receiver)
	sender.SetTxHandler(senderDaemon.handleTx)
	receiver.SetTxHandler(receiverDaemon.handleTx)
	receiver.SetStemRelayFilter(isRelayableStemTx)

	tx := mustBuildValidRingCTBindingTestTx(t)
	if err := senderDaemon.SubmitTransaction(tx.Serialize()); err != nil {
		t.Fatalf("failed to submit transaction: %v", err)
	}
	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to compute txid: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !receiverDaemon.mempool.HasTransaction(txID) {
		if time.Now().After(deadline) {
			t.Fatal("the peer never received the submitted transaction")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandleTxAllowsRelayOfATxAlreadyInTheMempool(t *testing.T) {
	d := newRelayTestDaemon(nil)
	tx := mustBuildValidRingCTBindingTestTx(t)
	data := tx.Serialize()
	if err := d.mempool.AddTransaction(tx, data); err != nil {
		t.Fatalf("failed to add transaction: %v", err)
	}
	if !d.handleTx(peer.ID("peer"), data) {
		t.Fatal("a transaction this node already accepted must stay relayable")
	}
}

func TestIsRelayableStemTx(t *testing.T) {
	if isRelayableStemTx(zeroInputTx(1)) {
		t.Fatal("a transaction with no inputs can never enter a mempool")
	}
	if isRelayableStemTx([]byte("not-a-valid-serialized-tx")) {
		t.Fatal("a malformed payload must not continue")
	}
	if !isRelayableStemTx(mustBuildValidRingCTBindingTestTx(t).Serialize()) {
		t.Fatal("a transaction with inputs must continue along the stem")
	}
}

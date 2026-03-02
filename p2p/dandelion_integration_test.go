package p2p

import (
	"context"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func mustNewTestNode(t *testing.T) *Node {
	t.Helper()

	cfg := DefaultNodeConfig()
	cfg.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.SeedNodes = nil

	n, err := NewNode(cfg)
	if err != nil {
		t.Fatalf("failed to create test node: %v", err)
	}
	return n
}

func mustConnectNodes(t *testing.T, from, to *Node) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := from.Connect(ctx, peer.AddrInfo{ID: to.PeerID(), Addrs: to.Addrs()}); err != nil {
		t.Fatalf("failed to connect nodes: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if from.host.Network().Connectedness(to.PeerID()) == network.Connected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nodes did not reach connected state in time")
}

func TestHandleStemStream_PenalizesMalformedStemPayload(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() {
		if err := a.Stop(); err != nil {
			t.Errorf("failed to stop node a: %v", err)
		}
	}()
	b := mustNewTestNode(t)
	defer func() {
		if err := b.Stop(); err != nil {
			t.Errorf("failed to stop node b: %v", err)
		}
	}()

	mustConnectNodes(t, a, b)
	b.SetStemSanityValidator(func(data []byte) bool { return false })
	senderID := a.PeerID()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := a.host.NewStream(ctx, b.PeerID(), protocolID(params.ProtocolDandelion))
	if err != nil {
		t.Fatalf("failed to open dandelion stream: %v", err)
	}
	if err := writeLengthPrefixed(s, []byte("malformed-stem")); err != nil {
		t.Fatalf("failed to write stem payload: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("failed to close dandelion stream: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		score := b.pex.GetPeerScore(senderID)
		if score >= 0 && score < 50 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected malformed stem sender score to be penalized, got score=%d", b.pex.GetPeerScore(senderID))
}

func TestHandleFluffTx_UsesCanonicalFluffSemanticsAndCacheCap(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() {
		if err := n.Stop(); err != nil {
			t.Errorf("failed to stop node: %v", err)
		}
	}()

	n.dandel.txCacheSize = 2

	callbacks := 0
	n.SetTxHandler(func(from peer.ID, data []byte) {
		callbacks++
	})

	n.dandel.HandleFluffTx(peer.ID("12D3KooWFluffPeer00000001"), []byte("tx-1"))
	n.dandel.HandleFluffTx(peer.ID("12D3KooWFluffPeer00000002"), []byte("tx-2"))
	n.dandel.HandleFluffTx(peer.ID("12D3KooWFluffPeer00000003"), []byte("tx-3"))

	if callbacks != 3 {
		t.Fatalf("expected fluff handler callback for each new tx, got %d", callbacks)
	}
	if size := n.dandel.TxCacheSize(); size != 2 {
		t.Fatalf("expected dandelion cache cap enforcement to keep 2 entries, got %d", size)
	}
	if n.dandel.SeenTx([]byte("tx-1")) {
		t.Fatal("expected oldest tx to be evicted at cache cap")
	}
	if !n.dandel.SeenTx([]byte("tx-2")) || !n.dandel.SeenTx([]byte("tx-3")) {
		t.Fatal("expected recent fluff transactions to remain cached")
	}
}

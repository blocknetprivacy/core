package p2p

import (
	"context"
	"testing"

	"github.com/multiformats/go-multiaddr"
)

func withTestProber(t *testing.T, pex *PeerExchange) {
	t.Helper()
	prober, err := newProbeHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prober.Close() })
	pex.prober = prober
	pex.ctx = context.Background()
}

func TestPEX_DialBackChecksAddressAndPeerID(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	other := mustNewTestNode(t)
	defer func() { _ = other.Stop() }()
	gone := mustNewTestNode(t)
	goneAddr := gone.Addrs()[0]
	_ = gone.Stop()

	withTestProber(t, a.pex)
	bAddr := b.Addrs()[0]

	if !a.pex.dialBack(b.PeerID(), bAddr) {
		t.Fatal("expected dial-back to a peer's own address to pass")
	}
	if a.pex.dialBack(other.PeerID(), bAddr) {
		t.Fatal("expected dial-back to fail when the address belongs to a different peer")
	}
	if a.pex.dialBack(gone.PeerID(), goneAddr) {
		t.Fatal("expected dial-back to fail when nothing listens at the address")
	}
}

func TestPEX_ConnectedPeerSharedOnlyAfterDialBack(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	mustConnectNodes(t, a, b)

	shared := func() bool {
		for _, r := range a.pex.getPeerRecords(MaxPeersPerExchange) {
			if r.ID == b.PeerID().String() {
				return true
			}
		}
		return false
	}
	heard, err := multiaddr.NewMultiaddr("/ip4/8.8.4.4/tcp/28080")
	if err != nil {
		t.Fatal(err)
	}
	a.pex.addKnownPeer(b.PeerID(), []multiaddr.Multiaddr{heard})
	if shared() {
		t.Fatal("a connected peer must not be shared before a dial-back succeeds")
	}

	ma, err := multiaddr.NewMultiaddr("/ip4/8.8.8.8/tcp/28080")
	if err != nil {
		t.Fatal(err)
	}
	a.pex.markReachedAddrs(b.PeerID(), []multiaddr.Multiaddr{ma})
	if !shared() {
		t.Fatal("expected the peer to be shared after a dial-back succeeded")
	}
}

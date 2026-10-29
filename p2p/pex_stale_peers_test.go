package p2p

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func mustTestMA(t *testing.T, s string) multiaddr.Multiaddr {
	t.Helper()
	ma, err := multiaddr.NewMultiaddr(s)
	if err != nil {
		t.Fatalf("invalid multiaddr %q: %v", s, err)
	}
	return ma
}

func TestPEX_GossipDoesNotRefreshLastSeen(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	pex := n.pex

	pid := peer.ID("heard")
	ma := mustTestMA(t, "/ip4/8.8.8.8/tcp/28080")
	pex.addKnownPeer(pid, []multiaddr.Multiaddr{ma})

	old := time.Now().Add(-2 * KnownPeerTTL).Unix()
	pex.mu.Lock()
	pex.knownPeers[pid].LastSeen = old
	pex.mu.Unlock()

	pex.addKnownPeer(pid, []multiaddr.Multiaddr{ma})

	pex.mu.RLock()
	got := pex.knownPeers[pid].LastSeen
	pex.mu.RUnlock()
	if got != old {
		t.Fatalf("expected gossip to leave LastSeen at %d, got %d", old, got)
	}
}

func TestPEX_GossipKeepsDialedAddrs(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	pex := n.pex

	pid := peer.ID("dialed")
	dialed := mustTestMA(t, "/ip4/8.8.8.8/tcp/28080")
	pex.markReachedAddrs(pid, []multiaddr.Multiaddr{dialed})
	pex.addKnownPeer(pid, []multiaddr.Multiaddr{mustTestMA(t, "/ip4/1.1.1.1/tcp/28080")})

	pex.mu.RLock()
	addrs := pex.knownPeers[pid].Addrs
	pex.mu.RUnlock()
	if len(addrs) != 1 || addrs[0] != dialed.String() {
		t.Fatalf("expected dialed addr %s to be kept, got %v", dialed, addrs)
	}
}

func TestPEX_SharesOnlyRecentlyReachedPeers(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	pex := n.pex

	now := time.Now()
	recent := now.Add(-10 * time.Minute).Unix()
	pex.mu.Lock()
	pex.knownPeers[peer.ID("heard")] = &PeerRecord{
		ID: peer.ID("heard").String(), Addrs: []string{"/ip4/8.8.8.8/tcp/28080"}, LastSeen: now.Unix(), Score: 5,
	}
	pex.knownPeers[peer.ID("expired")] = &PeerRecord{
		ID: peer.ID("expired").String(), Addrs: []string{"/ip4/8.8.4.4/tcp/28080"}, LastSeen: now.Unix(), Score: 5,
		lastContact: now.Add(-2 * KnownPeerTTL).Unix(),
	}
	pex.knownPeers[peer.ID("recent")] = &PeerRecord{
		ID: peer.ID("recent").String(), Addrs: []string{"/ip4/1.1.1.1/tcp/28080"}, LastSeen: now.Unix(), Score: 5,
		lastContact: recent,
	}
	pex.mu.Unlock()

	records := pex.getPeerRecords(MaxPeersPerExchange)
	if len(records) != 1 || records[0].ID != peer.ID("recent").String() {
		t.Fatalf("expected only the recently reached peer to be shared, got %+v", records)
	}
	if records[0].LastSeen != recent {
		t.Fatalf("expected shared LastSeen to be the contact time %d, got %d", recent, records[0].LastSeen)
	}
}

func TestPEX_CleanupDropsPeersWithoutRecentContact(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	pex := n.pex

	now := time.Now()
	pex.mu.Lock()
	pex.knownPeers[peer.ID("stale")] = &PeerRecord{
		ID: peer.ID("stale").String(), Addrs: []string{"/ip4/8.8.8.8/tcp/28080"}, LastSeen: now.Add(-2 * KnownPeerTTL).Unix(), Score: 5,
	}
	pex.knownPeers[peer.ID("fresh")] = &PeerRecord{
		ID: peer.ID("fresh").String(), Addrs: []string{"/ip4/1.1.1.1/tcp/28080"}, LastSeen: now.Unix(), Score: 5,
	}
	pex.probedAt[peer.ID("stale")] = now.Add(-2 * KnownPeerTTL)
	pex.mu.Unlock()

	pex.cleanup()

	pex.mu.RLock()
	_, hasStale := pex.knownPeers[peer.ID("stale")]
	_, hasFresh := pex.knownPeers[peer.ID("fresh")]
	_, hasProbe := pex.probedAt[peer.ID("stale")]
	pex.mu.RUnlock()
	if hasStale || !hasFresh {
		t.Fatalf("expected stale peer dropped and fresh peer kept (stale=%v fresh=%v)", hasStale, hasFresh)
	}
	if hasProbe {
		t.Fatal("expected old probe entry to be dropped")
	}
}

func TestPEX_ProbeRateLimited(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	pex := n.pex

	pid := peer.ID("inbound")
	if !pex.shouldProbe(pid) {
		t.Fatal("expected first probe to be allowed")
	}
	if pex.shouldProbe(pid) {
		t.Fatal("expected second probe within KnownPeerTTL/2 to be skipped")
	}
}

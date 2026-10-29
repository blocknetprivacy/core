package p2p

import (
	"context"
	"slices"
	"testing"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func seedInfo(n *Node) peer.AddrInfo {
	return peer.AddrInfo{ID: n.PeerID(), Addrs: n.Addrs()}
}

func connectedSeeds(n *Node, seeds []*Node) int {
	count := 0
	for _, s := range seeds {
		if n.host.Network().Connectedness(s.PeerID()) == network.Connected {
			count++
		}
	}
	return count
}

func TestPEX_ConnectToSeedsPicksTargetCount(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()

	var seeds []*Node
	for i := 0; i < 4; i++ {
		s := mustNewTestNode(t)
		defer func() { _ = s.Stop() }()
		seeds = append(seeds, s)
	}

	pex := n.pex
	pex.ctx = context.Background()
	for _, s := range seeds {
		pex.seedNodes = append(pex.seedNodes, seedInfo(s))
	}

	if err := pex.connectToSeeds(); err != nil {
		t.Fatal(err)
	}
	if got := connectedSeeds(n, seeds); got != SeedConnectTarget {
		t.Fatalf("connected to %d seeds, want %d", got, SeedConnectTarget)
	}
}

func TestPEX_ConnectToSeedsSkipsDeadSeeds(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()

	dead := mustNewTestNode(t)
	deadInfo := seedInfo(dead)
	_ = dead.Stop()

	var live []*Node
	for i := 0; i < 2; i++ {
		s := mustNewTestNode(t)
		defer func() { _ = s.Stop() }()
		live = append(live, s)
	}

	pex := n.pex
	pex.ctx = context.Background()
	pex.seedNodes = []peer.AddrInfo{deadInfo, seedInfo(live[0]), seedInfo(live[1])}

	if err := pex.connectToSeeds(); err != nil {
		t.Fatal(err)
	}
	if got := connectedSeeds(n, live); got != 2 {
		t.Fatalf("connected to %d live seeds, want 2", got)
	}
}

func TestCryptoShuffleKeepsElements(t *testing.T) {
	in := []int{1, 2, 3, 4, 5, 6, 7, 8}
	s := slices.Clone(in)
	cryptoShuffle(s)
	slices.Sort(s)
	if !slices.Equal(s, in) {
		t.Fatalf("shuffle changed the elements: %v", s)
	}
}

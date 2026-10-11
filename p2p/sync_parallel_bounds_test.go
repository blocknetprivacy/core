package p2p

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
)

func testChainStatus(height uint64) ChainStatus {
	return ChainStatus{Height: height, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}
}

func TestParallelSyncFromTargetBelowOurHeightDoesNotPanic(t *testing.T) {
	n := newPenaltyTestNode()
	sm := NewSyncManager(n, SyncConfig{
		GetStatus:        func() ChainStatus { return testChainStatus(1000) },
		ProcessBlock:     func([]byte) error { return nil },
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	defer sm.cancel()

	peers := []PeerStatus{{Peer: "12D3KooWHeavierShorterPeer", Status: testChainStatus(990)}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.parallelSyncFrom(peers, 990)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parallelSyncFrom did not return for a target below our height")
	}
}

func TestParallelSyncFromPenalizesTimedOutPeerOncePerSync(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	b.host.SetStreamHandler(protocolID(params.ProtocolSync), func(s network.Stream) { _ = s.Reset() })
	mustConnectNodes(t, a, b)

	sm := NewSyncManager(a, SyncConfig{
		GetStatus:        func() ChainStatus { return testChainStatus(0) },
		ProcessBlock:     func([]byte) error { return nil },
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})
	sm.ctx, sm.cancel = context.WithCancel(context.Background())

	a.pex.PenalizePeer(b.PeerID(), 0, "seed record")
	before := a.pex.GetPeerScore(b.PeerID())

	peers := []PeerStatus{{Peer: b.PeerID(), Status: testChainStatus(2000)}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.parallelSyncFrom(peers, 2000)
	}()

	time.Sleep(1500 * time.Millisecond)
	after := a.pex.GetPeerScore(b.PeerID())
	sm.cancel()
	<-done

	if after != before+ScorePenaltyTimeout {
		t.Fatalf("expected one timeout penalty: before=%d after=%d", before, after)
	}
}

func TestParallelSyncFromKeepsOneHedgeInFlight(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	mustConnectNodes(t, a, b)

	var requests atomic.Int32
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	smB := NewSyncManager(b, SyncConfig{
		GetStatus: func() ChainStatus { return testChainStatus(100) },
		GetBlocksByHeight: func(startHeight uint64, max int) ([][]byte, error) {
			requests.Add(1)
			<-release
			return [][]byte{{1}}, nil
		},
	})
	smB.Start(ctx)
	defer smB.Stop()

	sm := NewSyncManager(a, SyncConfig{
		GetStatus:        func() ChainStatus { return testChainStatus(0) },
		ProcessBlock:     func([]byte) error { return nil },
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})
	sm.ctx, sm.cancel = context.WithCancel(ctx)
	sm.fetchEWMA.Store(int64(250 * time.Millisecond))

	peers := []PeerStatus{{Peer: b.PeerID(), Status: testChainStatus(100)}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.parallelSyncFrom(peers, 100)
	}()

	time.Sleep(3 * time.Second)
	got := requests.Load()
	sm.cancel()
	close(release)
	<-done

	if got != 2 {
		t.Fatalf("expected one downloader fetch and one hedge in flight, got %d requests", got)
	}
}

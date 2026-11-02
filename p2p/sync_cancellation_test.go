package p2p

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
)

func TestParallelSyncCancellationInterruptsOpenBlockRead(t *testing.T) {
	a := mustNewTestNode(t)
	defer a.Stop()
	b := mustNewTestNode(t)
	defer b.Stop()
	mustConnectNodes(t, a, b)
	requested := make(chan struct{}, 8)
	b.host.SetStreamHandler(protocolID(params.ProtocolSync), func(s network.Stream) {
		defer s.Close()
		if _, _, err := readSyncMessage(s); err != nil {
			return
		}
		requested <- struct{}{}
		// The requester must reset the open stream when its run is canceled.
		var data [1]byte
		_, _ = s.Read(data[:])
	})
	sm := NewSyncManager(a, SyncConfig{GetStatus: func() ChainStatus { return testChainStatus(0) }})
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	defer sm.cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.parallelSyncFrom([]PeerStatus{{Peer: b.PeerID(), Status: testChainStatus(1)}}, 1)
	}()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("block request did not reach peer")
	}
	sm.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled sync waited for the block stream deadline")
	}
	if sm.IsSyncing() {
		t.Fatal("canceled sync retained its active state")
	}
}

func TestParallelSyncOnlyFetchesMempoolAfterReachingTarget(t *testing.T) {
	for _, complete := range []bool{false, true} {
		name := "incomplete"
		if complete {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			a := mustNewTestNode(t)
			defer a.Stop()
			b := mustNewTestNode(t)
			defer b.Stop()
			mustConnectNodes(t, a, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mempoolRequests atomic.Int32
			remote := NewSyncManager(b, SyncConfig{
				GetStatus:         func() ChainStatus { return testChainStatus(1) },
				GetBlocksByHeight: func(uint64, int) ([][]byte, error) { return [][]byte{{1}}, nil },
				GetMempool:        func() [][]byte { mempoolRequests.Add(1); return nil },
			})
			remote.Start(ctx)
			defer remote.Stop()
			var height atomic.Uint64
			sm := NewSyncManager(a, SyncConfig{
				GetStatus: func() ChainStatus { return testChainStatus(height.Load()) },
				ProcessBlock: func([]byte) error {
					if !complete {
						return errors.New("invalid block")
					}
					height.Store(1)
					return nil
				},
				GetMempool: func() [][]byte { return nil },
				ProcessTx:  func([]byte) error { return nil },
			})
			sm.ctx = ctx
			sm.parallelSyncFrom([]PeerStatus{{Peer: b.PeerID(), Status: testChainStatus(1)}}, 1)
			want := int32(0)
			if complete {
				want = 1
			}
			if got := mempoolRequests.Load(); got != want {
				t.Fatalf("mempool requests = %d, want %d", got, want)
			}
		})
	}
}

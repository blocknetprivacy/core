package p2p

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestRangeResendLimit(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	sm := NewSyncManager(n, SyncConfig{})

	looping := mustDecodePeerID(t, "12D3KooWQLYYe5jVgeQ8qrqtZitD8r7HmdPvyM2n4RQ6wRMeZuk7")
	trusted := mustDecodePeerID(t, testWhitelistPeerID)
	if err := n.pex.SetBanWhitelist([]string{testWhitelistPeerID}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for _, pid := range []peer.ID{looping, trusted} {
		for i := 0; i < rangeResendLimit; i++ {
			if !sm.rangeResendAllowed(pid, 63800, now) {
				t.Fatalf("send %d of the same range refused too early", i+1)
			}
			sm.recordRangeSent(pid, 63800, now)
		}
	}

	if sm.rangeResendAllowed(looping, 63800, now) {
		t.Fatal("expected the range to be refused after the limit")
	}
	if !sm.rangeResendAllowed(looping, 63900, now) {
		t.Fatal("expected a different range to stay allowed")
	}
	if !sm.rangeResendAllowed(trusted, 63800, now) {
		t.Fatal("expected a whitelisted peer to stay allowed")
	}
	if !sm.rangeResendAllowed(looping, 63800, now.Add(rangeResendWindow+time.Second)) {
		t.Fatal("expected the range to be allowed again after the window")
	}

	sm.recordRangeSent(looping, 1, now.Add(rangeResendWindow+2*time.Minute))
	if _, ok := sm.resends[looping][63800]; ok {
		t.Fatal("expected old entries to be pruned")
	}
}

func TestHandleGetBlocksByHeightSendsEmptyAfterResendLimit(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	mustConnectNodes(t, a, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	smB := NewSyncManager(b, SyncConfig{
		GetStatus: func() ChainStatus { return testChainStatus(1000) },
		GetBlocksByHeight: func(startHeight uint64, max int) ([][]byte, error) {
			out := make([][]byte, max)
			for i := range out {
				out[i] = []byte{1}
			}
			return out, nil
		},
	})
	smB.Start(ctx)
	defer smB.Stop()

	smA := NewSyncManager(a, SyncConfig{GetStatus: func() ChainStatus { return testChainStatus(0) }})
	smA.ctx, smA.cancel = context.WithCancel(ctx)

	for i := 0; i < rangeResendLimit; i++ {
		blocks, err := smA.fetchBlocksByHeight(b.PeerID(), 500, 10)
		if err != nil || len(blocks) != 10 {
			t.Fatalf("fetch %d: got %d blocks, err %v", i+1, len(blocks), err)
		}
	}
	blocks, err := smA.fetchBlocksByHeight(b.PeerID(), 500, 10)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("after the limit: got %d blocks, err %v; want an empty reply", len(blocks), err)
	}
	blocks, err = smA.fetchBlocksByHeight(b.PeerID(), 600, 10)
	if err != nil || len(blocks) != 10 {
		t.Fatalf("other range: got %d blocks, err %v", len(blocks), err)
	}
}

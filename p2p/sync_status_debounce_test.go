package p2p

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
)

func TestStatusTriggeredSync_IsDebouncedAndSerialized(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	mustConnectNodes(t, a, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set both sides to report identical status so that checkSync() does NOT
	// spawn parallelSyncFrom(). We only want to test the status-triggered
	// debounce/serialization behavior.
	our := ChainStatus{BestHash: [32]byte{}, Height: 10, TotalWork: 100, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}

	smA := NewSyncManager(a, SyncConfig{
		GetStatus: func() ChainStatus { return our },
	})
	smA.ctx, smA.cancel = context.WithCancel(ctx)

	var inboundStatusRequests atomic.Int64
	a.host.SetStreamHandler(protocolID(params.ProtocolSync), func(s network.Stream) {
		// Any checkSync() run on B will query peer status by opening a Sync stream
		// and sending SyncMsgStatus. Count those inbound status requests here.
		inboundStatusRequests.Add(1)
		smA.HandleStream(s)
	})

	smB := NewSyncManager(b, SyncConfig{
		GetStatus: func() ChainStatus { return our },
	})
	smB.ctx, smB.cancel = context.WithCancel(ctx)
	// Tighten debounce window so the test runs quickly and deterministically.
	smB.statusSyncMinInterval = 200 * time.Millisecond
	b.host.SetStreamHandler(protocolID(params.ProtocolSync), smB.HandleStream)
	go smB.statusSyncLoop()

	sendStatus := func(st ChainStatus) {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		s, err := a.host.NewStream(ctx, b.PeerID(), protocolID(params.ProtocolSync))
		if err != nil {
			t.Fatalf("failed to open sync stream: %v", err)
		}
		defer func() { _ = s.Close() }()

		data, _ := json.Marshal(st)
		if err := writeMessage(s, SyncMsgStatus, data); err != nil {
			t.Fatalf("failed to write status message: %v", err)
		}
		// b.handleStatus will reply with its status; read and ignore to keep the stream clean.
		_, _, _ = readSyncMessage(s)
	}

	// Burst 1: lots of status spam that claims higher work to trigger requestSyncCheck().
	lie := our
	lie.TotalWork = our.TotalWork + 1
	for i := 0; i < 50; i++ {
		sendStatus(lie)
	}
	// Burst 2 shortly after: should be coalesced (statusSyncCh has cap 1) and debounced.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 50; i++ {
		sendStatus(lie)
	}
	// Burst 3 after debounce interval: may trigger at most one additional checkSync().
	time.Sleep(250 * time.Millisecond)
	for i := 0; i < 50; i++ {
		sendStatus(lie)
	}

	// Allow any debounced runs to complete.
	time.Sleep(500 * time.Millisecond)

	// With a single peer, each checkSync() run produces ~1 inbound status request on A.
	// We expect a very small number here (not proportional to the 150 inbound status messages).
	got := inboundStatusRequests.Load()
	if got > 4 {
		t.Fatalf("expected debounced status-triggered checkSync runs; got %d inbound status requests", got)
	}
}


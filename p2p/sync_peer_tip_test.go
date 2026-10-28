package p2p

import (
	"context"
	"testing"

	"blocknet/protocol/params"
)

func TestPeerTipCheckedAfterFirstPeerComparison(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	our := ChainStatus{Height: 10, TotalWork: 100, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}
	smA := NewSyncManager(a, SyncConfig{GetStatus: func() ChainStatus { return our }})
	smA.ctx, smA.cancel = context.WithCancel(ctx)
	a.host.SetStreamHandler(protocolID(params.ProtocolSync), smA.HandleStream)

	smB := NewSyncManager(b, SyncConfig{GetStatus: func() ChainStatus { return our }})
	smB.ctx, smB.cancel = context.WithCancel(ctx)

	// No peers: nothing to compare against.
	smB.checkSync()
	if smB.PeerTipChecked() {
		t.Fatal("tip must not count as checked without any peer status")
	}

	mustConnectNodes(t, b, a)
	smB.checkSync()
	if !smB.PeerTipChecked() {
		t.Fatal("tip should count as checked after comparing with a peer at equal work")
	}
}

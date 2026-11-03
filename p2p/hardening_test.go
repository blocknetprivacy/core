package p2p

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestOnePeerCannotHoldUnlimitedInboundStreams(t *testing.T) {
	target := mustNewTestNode(t)
	defer func() { _ = target.Stop() }()
	flooder := mustNewTestNode(t)
	defer func() { _ = flooder.Stop() }()
	honest := mustNewTestNode(t)
	defer func() { _ = honest.Stop() }()
	mustConnectNodes(t, flooder, target)
	mustConnectNodes(t, honest, target)

	var held []network.Stream
	defer func() {
		for _, s := range held {
			_ = s.Reset()
		}
	}()
	for i := 0; i < maxInboundStreamsPerPeer*3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s, err := flooder.host.NewStream(ctx, target.PeerID(), protocolID(params.ProtocolPEX))
		cancel()
		if err != nil {
			continue
		}
		held = append(held, s)
		// Half a message: the far side accepts the stream and waits on the rest.
		_ = s.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = s.Write([]byte{PEXMsgGetPeers})
	}
	time.Sleep(200 * time.Millisecond)
	stat := target.host.Network().ConnsToPeer(flooder.PeerID())
	inbound := 0
	for _, c := range stat {
		for _, s := range c.GetStreams() {
			if s.Stat().Direction == network.DirInbound {
				inbound++
			}
		}
	}
	if inbound > maxInboundStreamsPerPeer {
		t.Fatalf("one peer holds %d inbound streams, limit %d", inbound, maxInboundStreamsPerPeer)
	}
	if inbound < maxInboundStreamsPerPeer/2 {
		t.Fatalf("only %d streams were held, so this test isn't exercising the limit", inbound)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := honest.host.NewStream(ctx, target.PeerID(), protocolID(params.ProtocolPEX))
	if err != nil {
		t.Fatalf("another peer could not open a stream: %v", err)
	}
	_ = s.Reset()
}

func TestOutboundPeersSurviveConnectionTrimming(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	dialed := mustNewTestNode(t)
	defer func() { _ = dialed.Stop() }()
	inbound := mustNewTestNode(t)
	defer func() { _ = inbound.Stop() }()
	mustConnectNodes(t, local, dialed)
	mustConnectNodes(t, inbound, local)

	waitForCondition(t, "the dialed peer to be protected", func() bool {
		return local.host.ConnManager().IsProtected(dialed.PeerID(), chosenPeerProtectTag)
	})
	if local.host.ConnManager().IsProtected(inbound.PeerID(), chosenPeerProtectTag) {
		t.Fatal("an inbound peer should not be protected")
	}
	if got := local.outboundPeerCount(); got != 1 {
		t.Fatalf("outbound peers = %d, want 1", got)
	}

	if err := local.host.Network().ClosePeer(dialed.PeerID()); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitForCondition(t, "protection to end with the connection", func() bool {
		return !local.host.ConnManager().IsProtected(dialed.PeerID(), chosenPeerProtectTag)
	})
}

func TestDialKnownPeersPrefersPeersWeHaveReached(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	addrs := []string{"/ip4/203.0.113.1/tcp/28080"}
	reached := peer.ID("12D3KooWReached")
	local.pex.mu.Lock()
	for _, id := range []string{"12D3KooWHeardA", "12D3KooWHeardB", "12D3KooWHeardC", "12D3KooWHeardD"} {
		local.pex.knownPeers[peer.ID(id)] = &PeerRecord{ID: id, Addrs: addrs, Score: peerStartingScore}
	}
	local.pex.knownPeers[reached] = &PeerRecord{ID: string(reached), Addrs: addrs, Score: peerStartingScore, lastContact: time.Now().Unix()}
	local.pex.mu.Unlock()

	for i := 0; i < 20; i++ {
		got := local.pex.knownPeersToDial()
		if len(got) != 5 {
			t.Fatalf("got %d candidates, want 5", len(got))
		}
		if got[0].pid != reached {
			t.Fatal("a peer we have reached before should be dialed first")
		}
	}
}

func TestStemFloodCannotPinUnboundedMemory(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	d := n.dandel
	remote := peer.ID("12D3KooWRemote")
	big := make([]byte, 64<<20)
	d.mu.Lock()
	for i := byte(0); i < 8; i++ {
		data := append([]byte{i}, big...)
		hash := sha256.Sum256(data)
		d.txCache[hash] = &txRecord{hash: hash, data: data, state: TxStateStem, createdAt: time.Now().Add(time.Duration(i) * time.Second), fromPeer: remote}
		d.enforceTxCacheLimitLocked()
	}
	bytesHeld := d.stemBytes
	d.mu.Unlock()
	if bytesHeld > maxPendingStemBytes {
		t.Fatalf("holding %d bytes of stem data, limit %d", bytesHeld, maxPendingStemBytes)
	}
}

func TestFluffedRecordsDropTheirData(t *testing.T) {
	relay, sender, _, _, _ := mustStartRelayNodes(t)
	rec := &txRecord{data: []byte("good"), fromPeer: sender.PeerID()}
	relay.dandel.fluffTx(rec)
	relay.dandel.mu.RLock()
	defer relay.dandel.mu.RUnlock()
	if rec.data != nil {
		t.Fatal("a fluffed record kept its transaction data")
	}
}

func TestServeSlotsAreBoundedPerPeerAndInTotal(t *testing.T) {
	// A quarter of the slots stay free for trusted peers: 6 of 8 for anyone else.
	sm := &SyncManager{serveSlots: make(chan struct{}, 8), servesByPeer: make(map[peer.ID]int)}
	busy := peer.ID("busy")
	var releases []func()
	for i := 0; i < syncMaxServesPerPeer; i++ {
		release, ok := sm.acquireServeSlot(busy)
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := sm.acquireServeSlot(busy); ok {
		t.Fatal("one peer took more than its share")
	}
	if _, ok := sm.acquireServeSlot(peer.ID("other")); ok {
		t.Fatal("a peer with no track record took a reserved slot")
	}
	for _, release := range releases {
		release()
	}
	if len(sm.servesByPeer) != 0 {
		t.Fatalf("per-peer counts left behind: %v", sm.servesByPeer)
	}
	if _, ok := sm.acquireServeSlot(busy); !ok {
		t.Fatal("a released slot should be reusable")
	}
}

func TestReservedServeSlotsStayOpenForPeersWeChose(t *testing.T) {
	server := mustNewTestNode(t)
	defer func() { _ = server.Stop() }()
	chosen := mustNewTestNode(t)
	defer func() { _ = chosen.Stop() }()
	mustConnectNodes(t, server, chosen)
	sm := &SyncManager{node: server, serveSlots: make(chan struct{}, 8), servesByPeer: make(map[peer.ID]int)}
	for i := 0; i < 6; i++ {
		if _, ok := sm.acquireServeSlot(peer.ID("stranger-" + string(rune('a'+i)))); !ok {
			t.Fatalf("slot %d refused", i)
		}
	}
	if _, ok := sm.acquireServeSlot(peer.ID("one-more-stranger")); ok {
		t.Fatal("strangers took a reserved slot")
	}
	if _, ok := sm.acquireServeSlot(chosen.PeerID()); !ok {
		t.Fatal("a peer we dialed should get a reserved slot")
	}
}

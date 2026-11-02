package p2p

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/peer"
)

// mustStartRelayNodes returns a relay connected to a sender and a neighbor. The
// other two count the transactions they receive.
func mustStartRelayNodes(t *testing.T) (relay, sender, neighbor *Node, senderSaw, neighborSaw *atomic.Int32) {
	t.Helper()
	relay = mustNewTestNode(t)
	t.Cleanup(func() { _ = relay.Stop() })
	nodes := connectedTestNodes(t, relay, 2)
	sender, neighbor = nodes[0], nodes[1]
	senderSaw, neighborSaw = &atomic.Int32{}, &atomic.Int32{}
	count := func(c *atomic.Int32) func(peer.ID, []byte) bool {
		return func(peer.ID, []byte) bool {
			c.Add(1)
			return true
		}
	}
	sender.SetTxHandler(count(senderSaw))
	neighbor.SetTxHandler(count(neighborSaw))
	relay.SetTxHandler(func(_ peer.ID, data []byte) bool { return string(data) == "good" })
	return relay, sender, neighbor, senderSaw, neighborSaw
}

func TestHandleFluffTxRelaysOnlyAcceptedTransactions(t *testing.T) {
	relay, sender, _, _, neighborSaw := mustStartRelayNodes(t)

	relay.dandel.HandleFluffTx(sender.PeerID(), []byte("bad"))
	time.Sleep(300 * time.Millisecond)
	if got := neighborSaw.Load(); got != 0 {
		t.Fatalf("a rejected transaction reached the neighbor %d times", got)
	}

	relay.dandel.HandleFluffTx(sender.PeerID(), []byte("good"))
	waitForCondition(t, "the accepted transaction to reach the neighbor", func() bool { return neighborSaw.Load() == 1 })
}

func TestFluffTxBroadcastsOnlyAcceptedTransactions(t *testing.T) {
	relay, sender, _, senderSaw, neighborSaw := mustStartRelayNodes(t)

	relay.dandel.fluffTx(&txRecord{data: []byte("bad"), fromPeer: sender.PeerID()})
	time.Sleep(300 * time.Millisecond)
	if got := neighborSaw.Load() + senderSaw.Load(); got != 0 {
		t.Fatalf("a rejected transaction was broadcast %d times", got)
	}

	relay.dandel.fluffTx(&txRecord{data: []byte("good"), fromPeer: sender.PeerID()})
	waitForCondition(t, "the accepted transaction to reach the neighbor", func() bool { return neighborSaw.Load() == 1 })
	if got := senderSaw.Load(); got != 0 {
		t.Fatalf("the transaction went back to its sender %d times", got)
	}
}

func TestStemRelayFilterDropsWithoutPenalty(t *testing.T) {
	relay, sender, _, _, _ := mustStartRelayNodes(t)
	relay.dandel.Start(context.Background()) // The kept payload may continue along the stem.
	relay.SetStemSanityValidator(func([]byte) bool { return true })
	relay.SetStemRelayFilter(func(data []byte) bool { return string(data) != "dropped" })
	before := relay.pex.GetPeerScore(sender.PeerID())

	send := func(payload string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := sender.host.NewStream(ctx, relay.PeerID(), protocolID(params.ProtocolDandelion))
		if err != nil {
			t.Fatalf("failed to open dandelion stream: %v", err)
		}
		if err := writeLengthPrefixed(s, []byte(payload)); err != nil {
			t.Fatalf("failed to write stem payload: %v", err)
		}
		_ = s.Close()
	}

	send("dropped")
	send("kept")
	waitForCondition(t, "the kept payload to be handled", func() bool { return relay.dandel.SeenTx([]byte("kept")) })
	if relay.dandel.SeenTx([]byte("dropped")) {
		t.Fatal("a filtered stem payload entered the router")
	}
	if got := relay.pex.GetPeerScore(sender.PeerID()); got != before {
		t.Fatalf("sender score changed from %d to %d", before, got)
	}
}

func TestRejectedTransactionsLimitThePeerNotTheTransaction(t *testing.T) {
	n := mustNewTestNode(t)
	defer func() { _ = n.Stop() }()
	var handled []peer.ID
	n.SetTxHandler(func(from peer.ID, _ []byte) bool {
		handled = append(handled, from)
		return false
	})
	spammer := peer.ID("12D3KooWSpammer")
	other := peer.ID("12D3KooWOther")

	for i := 0; i < txRejectLimit+5; i++ {
		n.dandel.HandleFluffTx(spammer, []byte(fmt.Sprintf("tx-%d", i)))
	}
	if len(handled) != txRejectLimit {
		t.Fatalf("validated %d transactions from one peer, want the limit of %d", len(handled), txRejectLimit)
	}

	// What the limited peer sent can still arrive from anyone else.
	unheard := []byte(fmt.Sprintf("tx-%d", txRejectLimit))
	if n.dandel.SeenTx(unheard) {
		t.Fatal("a transaction skipped by the limit must stay out of the cache")
	}
	handled = nil
	n.dandel.HandleFluffTx(other, unheard)
	if len(handled) != 1 || handled[0] != other {
		t.Fatalf("handled %v, want the transaction from the other peer", handled)
	}

	// The limit ends with its window.
	n.dandel.txRejects[spammer].windowEnds = time.Now().Add(-time.Second)
	handled = nil
	n.dandel.HandleFluffTx(spammer, []byte("after-the-window"))
	if len(handled) != 1 {
		t.Fatal("a peer's window ended but its transactions are still ignored")
	}
}

func TestTxRejectTrackingIsBounded(t *testing.T) {
	d := NewDandelionRouter(nil)
	now := time.Now()
	fill := func(windowEnds time.Time) {
		clear(d.txRejects)
		for i := 0; i < maxTxRejectPeers; i++ {
			d.txRejects[peer.ID(fmt.Sprintf("peer-%d", i))] = &txRejects{count: 1, windowEnds: windowEnds}
		}
	}

	fill(now.Add(-time.Second))
	d.noteTxRejected(peer.ID("new"), now)
	if got := len(d.txRejects); got != 1 {
		t.Fatalf("ended windows left %d entries, want 1", got)
	}

	fill(now.Add(time.Hour))
	d.noteTxRejected(peer.ID("new"), now)
	if got := len(d.txRejects); got > maxTxRejectPeers {
		t.Fatalf("tracking grew to %d entries", got)
	}
	if d.txRejects[peer.ID("new")] == nil {
		t.Fatal("the newest peer was not tracked")
	}
}

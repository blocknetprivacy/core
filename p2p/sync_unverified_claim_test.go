package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
)

func claimStatus(height, work uint64) ChainStatus {
	status := testChainStatus(height)
	status.TotalWork = work
	return status
}

// serveChain makes a test node answer status requests with status and pass
// block range requests to onBlocks.
func serveChain(n *Node, status ChainStatus, onBlocks func(network.Stream, BlocksByHeightRequest)) {
	n.host.SetStreamHandler(protocolID(params.ProtocolSync), func(s network.Stream) {
		defer s.Close()
		msgType, data, err := readSyncMessage(s)
		if err != nil {
			return
		}
		switch msgType {
		case SyncMsgStatus:
			payload, _ := json.Marshal(status)
			_ = writeMessage(s, SyncMsgStatus, payload)
		case SyncMsgGetBlocksByHeight:
			var req BlocksByHeightRequest
			if json.Unmarshal(data, &req) == nil && onBlocks != nil {
				onBlocks(s, req)
			}
		}
	})
}

// holdOpen leaves a request unanswered until the requester gives up.
func holdOpen(s network.Stream) {
	var data [1]byte
	_, _ = s.Read(data[:])
}

func connectedTestNodes(t *testing.T, local *Node, count int) []*Node {
	t.Helper()
	nodes := make([]*Node, count)
	for i := range nodes {
		nodes[i] = mustNewTestNode(t)
		t.Cleanup(func() { _ = nodes[i].Stop() })
		mustConnectNodes(t, local, nodes[i])
	}
	return nodes
}

func newClaimTestManager(t *testing.T, local *Node, cfg SyncConfig) *SyncManager {
	t.Helper()
	sm := NewSyncManager(local, cfg)
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	t.Cleanup(sm.cancel)
	sm.claimGiveUpAfter = 200 * time.Millisecond
	return sm
}

func waitForCondition(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (sm *SyncManager) claimCoolingDown(n *Node) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, cooling := sm.claimCooldowns[n.PeerID()]
	return cooling
}

func TestFarBehindIgnoresUnverifiedClaim(t *testing.T) {
	sm := newFarBehindTestManager(100)
	sm.syncing = true
	sm.syncTarget = 5000
	sm.syncClaimUnverified = true
	if sm.IsSyncingFarBehind(2) {
		t.Fatal("an unverified claim must not stop template serving")
	}
	sm.syncClaimUnverified = false
	if !sm.IsSyncingFarBehind(2) {
		t.Fatal("a verified target far ahead must count as behind")
	}
}

func TestUndeliveredMinorityClaimEndsAndCoolsDownThePeer(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)
	claimer := nodes[0]

	var requests atomic.Int32
	serveChain(claimer, claimStatus(13, 130), func(s network.Stream, _ BlocksByHeightRequest) {
		requests.Add(1)
		holdOpen(s)
	})
	serveChain(nodes[1], claimStatus(10, 100), nil)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	sm := newClaimTestManager(t, local, SyncConfig{GetStatus: func() ChainStatus { return claimStatus(10, 100) }})

	sm.checkSync()
	waitForCondition(t, "the sync run to start", sm.IsSyncing)
	if sm.IsSyncingFarBehind(0) {
		t.Fatal("a minority claim must not stop template serving")
	}
	waitForCondition(t, "the block request", func() bool { return requests.Load() > 0 })

	waitForCondition(t, "the run to give up", func() bool { return !sm.IsSyncing() })
	if !sm.claimCoolingDown(claimer) {
		t.Fatal("peer that did not deliver its claim was not cooled down")
	}
	if got, want := local.pex.GetPeerScore(claimer.PeerID()), 50+ScorePenaltyTimeout; got != want {
		t.Fatalf("peer score = %d, want %d", got, want)
	}

	seen := requests.Load()
	sm.checkSync()
	time.Sleep(100 * time.Millisecond)
	if sm.IsSyncing() || requests.Load() != seen {
		t.Fatal("a peer in cooldown started another sync run")
	}

	sm.mu.Lock()
	sm.claimCooldowns[claimer.PeerID()] = time.Now().Add(-time.Second)
	sm.claimRunsResumeAt = time.Time{}
	sm.mu.Unlock()
	sm.checkSync()
	waitForCondition(t, "a run after the cooldown ended", func() bool { return requests.Load() > seen })
	waitForCondition(t, "the second run to give up", func() bool { return !sm.IsSyncing() })
}

func TestFreshPeerCannotRestartAClaimRunImmediately(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)
	first := nodes[0]

	serveChain(first, claimStatus(13, 130), func(s network.Stream, _ BlocksByHeightRequest) { holdOpen(s) })
	serveChain(nodes[1], claimStatus(10, 100), nil)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	sm := newClaimTestManager(t, local, SyncConfig{GetStatus: func() ChainStatus { return claimStatus(10, 100) }})
	sm.checkSync()
	waitForCondition(t, "the sync run to start", sm.IsSyncing)
	waitForCondition(t, "the run to give up", func() bool { return !sm.IsSyncing() })

	// A new identity making the same claim must wait out the spacing too.
	second := connectedTestNodes(t, local, 1)[0]
	var requests atomic.Int32
	serveChain(second, claimStatus(13, 130), func(s network.Stream, _ BlocksByHeightRequest) {
		requests.Add(1)
		holdOpen(s)
	})
	sm.checkSync()
	time.Sleep(100 * time.Millisecond)
	if sm.IsSyncing() || requests.Load() != 0 {
		t.Fatal("a new peer's claim started a run during the spacing")
	}

	sm.mu.Lock()
	sm.claimRunsResumeAt = time.Time{}
	sm.mu.Unlock()
	sm.checkSync()
	waitForCondition(t, "a run after the spacing ended", func() bool { return requests.Load() > 0 })
	waitForCondition(t, "the second run to give up", func() bool { return !sm.IsSyncing() })
}

func TestCorroboratedClaimCountsAsBehindImmediately(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)

	requested := make(chan struct{}, 8)
	claim := func(s network.Stream, _ BlocksByHeightRequest) {
		requested <- struct{}{}
		holdOpen(s)
	}
	serveChain(nodes[0], claimStatus(13, 130), claim)
	serveChain(nodes[1], claimStatus(13, 130), claim)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	sm := newClaimTestManager(t, local, SyncConfig{GetStatus: func() ChainStatus { return claimStatus(10, 100) }})
	// A claim most peers make isn't held back by a recent failed minority run.
	sm.claimRunsResumeAt = time.Now().Add(time.Hour)

	sm.checkSync()
	waitForCondition(t, "the sync run to start", sm.IsSyncing)
	if !sm.IsSyncingFarBehind(2) {
		t.Fatal("a majority claim must count as behind immediately")
	}
	<-requested
	time.Sleep(2 * sm.claimGiveUpAfter)
	if !sm.IsSyncing() || !sm.IsSyncingFarBehind(2) {
		t.Fatal("a majority claim must keep the full give-up time")
	}
	sm.cancel()
	waitForCondition(t, "the canceled run to end", func() bool { return !sm.IsSyncing() })
	if sm.claimCoolingDown(nodes[0]) || sm.claimCoolingDown(nodes[1]) {
		t.Fatal("peers in a majority claim must not be cooled down")
	}
}

func TestDeliveredBlockVerifiesAClaim(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)
	claimer := nodes[0]

	serveChain(claimer, claimStatus(13, 130), func(s network.Stream, req BlocksByHeightRequest) {
		if req.StartHeight != 11 {
			holdOpen(s)
			return
		}
		payload, _ := json.Marshal([][]byte{[]byte("block-11")})
		_ = writeMessage(s, SyncMsgBlocks, payload)
	})
	serveChain(nodes[1], claimStatus(10, 100), nil)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	var height atomic.Uint64
	height.Store(10)
	sm := newClaimTestManager(t, local, SyncConfig{
		GetStatus:        func() ChainStatus { return claimStatus(height.Load(), 100) },
		ProcessBlock:     func([]byte) error { height.Store(11); return nil },
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})

	sm.checkSync()
	waitForCondition(t, "the delivered block to verify the claim", func() bool {
		return sm.IsSyncing() && !sm.syncRunUnverified() && height.Load() == 11
	})
	if !sm.IsSyncingFarBehind(1) || sm.IsSyncingFarBehind(2) {
		t.Fatal("a verified claim must count the two blocks still missing")
	}
	sm.cancel()
	waitForCondition(t, "the canceled run to end", func() bool { return !sm.IsSyncing() })
}

func TestClaimOvertakenByGossipDoesNotCoolDownThePeer(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)
	claimer := nodes[0]

	var height atomic.Uint64
	height.Store(10)
	serveChain(claimer, claimStatus(11, 110), func(s network.Stream, _ BlocksByHeightRequest) {
		// The block reaches the node over gossip while this request is open.
		height.Store(11)
		holdOpen(s)
	})
	serveChain(nodes[1], claimStatus(10, 100), nil)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	sm := newClaimTestManager(t, local, SyncConfig{GetStatus: func() ChainStatus { return claimStatus(height.Load(), 100) }})

	sm.checkSync()
	waitForCondition(t, "the block request", func() bool { return height.Load() == 11 })
	waitForCondition(t, "the run to end", func() bool { return !sm.IsSyncing() })
	if sm.claimCoolingDown(claimer) {
		t.Fatal("peer whose claim came true was cooled down")
	}
}

func TestSlowTransferIsNotTreatedAsASilentPeer(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	nodes := connectedTestNodes(t, local, 3)
	claimer := nodes[0]

	// One response delivered over several times claimGiveUpAfter.
	serveChain(claimer, claimStatus(13, 130), func(s network.Stream, req BlocksByHeightRequest) {
		if req.StartHeight != 11 {
			holdOpen(s)
			return
		}
		payload, _ := json.Marshal([][]byte{bytes.Repeat([]byte("b"), 3000)})
		var msg bytes.Buffer
		_ = writeMessage(&msg, SyncMsgBlocks, payload)
		for msg.Len() > 0 {
			if _, err := s.Write(msg.Next(200)); err != nil {
				return
			}
			time.Sleep(60 * time.Millisecond)
		}
	})
	serveChain(nodes[1], claimStatus(10, 100), nil)
	serveChain(nodes[2], claimStatus(10, 100), nil)

	var height atomic.Uint64
	height.Store(10)
	sm := newClaimTestManager(t, local, SyncConfig{
		GetStatus:        func() ChainStatus { return claimStatus(height.Load(), 100) },
		ProcessBlock:     func([]byte) error { height.Store(11); return nil },
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})

	// The give-up check runs on the hedge timer; keep it at its shortest so it
	// fires during the transfer.
	sm.fetchEWMA.Store(int64(time.Millisecond))
	started := time.Now()
	sm.checkSync()
	waitForCondition(t, "the slow block to arrive", func() bool { return height.Load() == 11 })
	if elapsed := time.Since(started); elapsed <= syncHedgeMin {
		t.Fatalf("transfer took %s; it must outlast the first give-up check at %s", elapsed, syncHedgeMin)
	}
	if sm.claimCoolingDown(claimer) {
		t.Fatal("a peer still sending data was cooled down")
	}
	sm.cancel()
	waitForCondition(t, "the canceled run to end", func() bool { return !sm.IsSyncing() })
}

func TestSyncStalled(t *testing.T) {
	sm := newFarBehindTestManager(100)
	sm.claimGiveUpAfter = 30 * time.Second
	now := time.Now()
	dataAt := func(ago time.Duration) { sm.lastBlockDataAt.Store(now.Add(-ago).UnixNano()) }

	sm.syncClaimUnverified = true
	dataAt(time.Hour)
	if !sm.syncStalled(now.Add(-31*time.Second), now) {
		t.Fatal("an unverified run with no data must give up after claimGiveUpAfter")
	}
	if sm.syncStalled(now.Add(-29*time.Second), now) {
		t.Fatal("an unverified run gave up early")
	}
	dataAt(time.Second)
	if sm.syncStalled(now.Add(-31*time.Second), now) {
		t.Fatal("an unverified run still receiving data must continue")
	}
	if !sm.syncStalled(now.Add(-syncGiveUpAfter-time.Second), now) {
		t.Fatal("data must not extend a run past syncGiveUpAfter")
	}

	sm.syncClaimUnverified = false
	dataAt(time.Hour)
	if sm.syncStalled(now.Add(-31*time.Second), now) {
		t.Fatal("a corroborated run must keep syncGiveUpAfter")
	}
	if !sm.syncStalled(now.Add(-syncGiveUpAfter-time.Second), now) {
		t.Fatal("a corroborated run must still give up")
	}
}

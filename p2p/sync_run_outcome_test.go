package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func outcomeStatus(height, work uint64) ChainStatus {
	s := testChainStatus(height)
	s.TotalWork = work
	return s
}

// outcomePeer answers status and block requests the way a real peer does.
type outcomePeer struct {
	node     *Node
	status   ChainStatus
	silent   atomic.Bool
	requests atomic.Int32
	blocks   func(start uint64, max int) [][]byte
}

func startOutcomePeers(t *testing.T, local *Node, count int, status ChainStatus) []*outcomePeer {
	t.Helper()
	peers := make([]*outcomePeer, count)
	for i := range peers {
		node := mustNewTestNode(t)
		t.Cleanup(func() { _ = node.Stop() })
		mustConnectNodes(t, local, node)
		p := &outcomePeer{node: node, status: status}
		node.host.SetStreamHandler(protocolID(params.ProtocolSync), p.serve)
		peers[i] = p
	}
	return peers
}

func (p *outcomePeer) serve(s network.Stream) {
	defer s.Close()
	msgType, data, err := readSyncMessage(s)
	if err != nil {
		return
	}
	switch msgType {
	case SyncMsgStatus:
		payload, _ := json.Marshal(p.status)
		_ = writeMessage(s, SyncMsgStatus, payload)
	case SyncMsgGetBlocksByHeight:
		p.requests.Add(1)
		if p.silent.Load() {
			var b [1]byte
			_, _ = s.Read(b[:])
			return
		}
		var req BlocksByHeightRequest
		_ = json.Unmarshal(data, &req)
		blocks := [][]byte{}
		if p.blocks != nil {
			blocks = p.blocks(req.StartHeight, req.MaxBlocks)
		}
		payload, _ := json.Marshal(blocks)
		_ = writeMessage(s, SyncMsgBlocks, payload)
	}
}

type outcomeBlock struct {
	Height uint64 `json:"h"`
	Bad    bool   `json:"bad,omitempty"`
	Pad    string `json:"pad"`
}

func outcomeBlocksUpTo(tip uint64) func(uint64, int) [][]byte {
	return func(start uint64, max int) [][]byte {
		out := [][]byte{}
		for h := start; h <= tip && len(out) < max; h++ {
			b, _ := json.Marshal(outcomeBlock{Height: h, Pad: string(make([]byte, 1500))})
			out = append(out, b)
		}
		return out
	}
}

// outcomeChain is the local chain: it applies blocks in order and fails bad ones.
type outcomeChain struct {
	mu      sync.Mutex
	height  uint64
	work    uint64
	failAt  uint64
	process time.Duration
}

func (c *outcomeChain) status() ChainStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return outcomeStatus(c.height, c.work)
}

func (c *outcomeChain) processBlock(data []byte) error {
	var b outcomeBlock
	_ = json.Unmarshal(data, &b)
	if c.process > 0 {
		time.Sleep(c.process)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b.Bad || (c.failAt != 0 && b.Height == c.failAt) {
		return errors.New("invalid block")
	}
	if b.Height > c.height {
		c.work += (b.Height - c.height) * 10
		c.height = b.Height
	}
	return nil
}

func newOutcomeManager(t *testing.T, local *Node, chain *outcomeChain, giveUp time.Duration) *SyncManager {
	t.Helper()
	sm := NewSyncManager(local, SyncConfig{
		GetStatus:    chain.status,
		ProcessBlock: chain.processBlock,
	})
	sm.ctx, sm.cancel = context.WithCancel(context.Background())
	t.Cleanup(sm.cancel)
	sm.claimGiveUpAfter = giveUp
	return sm
}

func waitOutcome(t *testing.T, what string, within time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cooledCount(sm *SyncManager, peers []*outcomePeer) int {
	cooled := 0
	for _, p := range peers {
		if sm.claimCoolingDown(p.node) {
			cooled++
		}
	}
	return cooled
}

func TestBadBlockCostsOnlyItsSender(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	const tip = 500
	ahead := startOutcomePeers(t, local, 3, outcomeStatus(tip, tip*10))
	for _, p := range ahead {
		p.blocks = outcomeBlocksUpTo(tip)
	}
	// A minority of our dialed peers are ahead, so this run proves nothing by itself.
	startOutcomePeers(t, local, 5, outcomeStatus(10, 100))

	chain := &outcomeChain{height: 10, work: 100, failAt: 12}
	sm := newOutcomeManager(t, local, chain, 300*time.Millisecond)
	sm.checkSync()
	waitOutcome(t, "the run to start", 5*time.Second, sm.IsSyncing)
	waitOutcome(t, "the run to end", 10*time.Second, func() bool { return !sm.IsSyncing() })

	if got := cooledCount(sm, ahead); got != 0 {
		t.Fatalf("one rejected block cooled %d honest peers", got)
	}
}

func TestRunTimeLimitCoolsNobody(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	const tip = 100000
	ahead := startOutcomePeers(t, local, 4, outcomeStatus(tip, tip*10))
	for _, p := range ahead {
		p.blocks = outcomeBlocksUpTo(tip)
	}
	startOutcomePeers(t, local, 6, outcomeStatus(10, 100))

	chain := &outcomeChain{height: 10, work: 100, process: 10 * time.Millisecond}
	sm := newOutcomeManager(t, local, chain, 300*time.Millisecond)
	sm.syncRunTimeLimit = 1200 * time.Millisecond
	sm.checkSync()
	waitOutcome(t, "the run to start", 5*time.Second, sm.IsSyncing)
	waitOutcome(t, "the run to end", 10*time.Second, func() bool { return !sm.IsSyncing() })

	if chain.status().Height <= 11 {
		t.Fatalf("the run made no progress: height %d", chain.status().Height)
	}
	if got := cooledCount(sm, ahead); got != 0 {
		t.Fatalf("a run making progress cooled %d honest peers", got)
	}
	for _, p := range ahead {
		if score := local.pex.GetPeerScore(p.node.PeerID()); score != -1 && score < peerStartingScore {
			t.Fatalf("an honest peer lost score: %d", score)
		}
	}
}

func TestFirstBadReplyDoesNotCoolHonestPeers(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	liar := startOutcomePeers(t, local, 1, outcomeStatus(100000, 1000000))[0]
	liar.blocks = func(start uint64, _ int) [][]byte {
		b, _ := json.Marshal(outcomeBlock{Height: start, Bad: true})
		return [][]byte{b}
	}
	honest := startOutcomePeers(t, local, 2, outcomeStatus(1000, 10000))
	for _, p := range honest {
		p.silent.Store(true) // Slow to answer right now, but they hold the real chain.
	}
	startOutcomePeers(t, local, 4, outcomeStatus(10, 100))

	chain := &outcomeChain{height: 10, work: 100}
	sm := newOutcomeManager(t, local, chain, 5*time.Second)
	sm.checkSync()
	waitOutcome(t, "the liar's bad reply", 5*time.Second, func() bool { return liar.requests.Load() > 0 })
	waitOutcome(t, "the run to end", 15*time.Second, func() bool { return !sm.IsSyncing() })
	if got := cooledCount(sm, honest); got != 0 {
		t.Fatalf("a false claimer's bad block cooled %d honest peers", got)
	}
}

func TestOnlyAskedPeersCanCoolDown(t *testing.T) {
	asked := &sync.Map{}
	a, b, c := peer.ID("asked-unmet"), peer.ID("never-asked"), peer.ID("asked-met")
	asked.Store(a, struct{}{})
	asked.Store(c, struct{}{})
	got := unmetClaims([]PeerStatus{
		{Peer: a, Status: outcomeStatus(500, 5000)},
		{Peer: b, Status: outcomeStatus(500, 5000)},
		{Peer: c, Status: outcomeStatus(100, 1000)},
	}, asked, 1000)
	if len(got) != 1 || got[0].Peer != a {
		t.Fatalf("got %v, want only the asked peer whose claim went unmet", got)
	}
}

func TestLaggingPeersDoNotSlowNearTipCatchUp(t *testing.T) {
	for round := 0; round < 8; round++ {
		func() {
			local := mustNewTestNode(t)
			defer func() { _ = local.Stop() }()
			const ours, lagging, top = 100, 101, 105
			startOutcomePeers(t, local, 1, outcomeStatus(top, top*10))[0].blocks = outcomeBlocksUpTo(top)
			for _, p := range startOutcomePeers(t, local, 5, outcomeStatus(lagging, lagging*10)) {
				p.blocks = outcomeBlocksUpTo(lagging)
			}
			chain := &outcomeChain{height: ours, work: ours * 10}
			sm := newOutcomeManager(t, local, chain, 5*time.Second)
			started := time.Now()
			sm.checkSync()
			waitOutcome(t, "the top height", 10*time.Second, func() bool { return chain.status().Height == top })
			// Waiting on a hedged request would take at least syncHedgeMin.
			if elapsed := time.Since(started); elapsed >= syncHedgeMin {
				t.Fatalf("round %d: reaching the top took %s", round, elapsed)
			}
		}()
	}
}

func TestOnePeerCannotTakeEveryServeSlot(t *testing.T) {
	server := mustNewTestNode(t)
	defer func() { _ = server.Stop() }()
	flooder := mustNewTestNode(t)
	defer func() { _ = flooder.Stop() }()
	honest := mustNewTestNode(t)
	defer func() { _ = honest.Stop() }()
	mustConnectNodes(t, flooder, server)
	mustConnectNodes(t, honest, server)

	serveSM := NewSyncManager(server, SyncConfig{
		GetStatus:         func() ChainStatus { return outcomeStatus(100, 1000) },
		GetBlocksByHeight: func(start uint64, max int) ([][]byte, error) { return outcomeBlocksUpTo(100)(start, max), nil },
	})
	serveSM.ctx, serveSM.cancel = context.WithCancel(context.Background())
	defer serveSM.cancel()
	server.host.SetStreamHandler(protocolID(params.ProtocolSync), serveSM.HandleStream)
	// Eight slots, so a flooder's share is all that strangers may use.
	serveSM.serveSlots = make(chan struct{}, 8)

	held := make([]func(), 0, syncMaxServesPerPeer)
	for i := 0; i < syncMaxServesPerPeer; i++ {
		release, ok := serveSM.acquireServeSlot(flooder.PeerID())
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		held = append(held, release)
	}
	defer func() {
		for _, release := range held {
			release()
		}
	}()

	// With no slot it may use, a stranger's request gets an empty answer, not an
	// error, so it costs the peer nothing and it can ask elsewhere.
	client := NewSyncManager(honest, SyncConfig{GetStatus: func() ChainStatus { return outcomeStatus(10, 100) }})
	client.ctx, client.cancel = context.WithCancel(context.Background())
	defer client.cancel()
	blocks, err := client.fetchBlocksByHeightCtx(client.ctx, server.PeerID(), 11, 5)
	if err != nil {
		t.Fatalf("a refused request should still get an answer: %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("got %d blocks while every slot was taken", len(blocks))
	}
}

func TestOutboundRefillStaysWithinItsBudget(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	local.pex.ctx, local.pex.cancel = context.WithCancel(context.Background())
	defer local.pex.cancel()
	local.config.MaxOutbound = 2
	local.pex.mu.Lock()
	for i := byte(0); i < 20; i++ {
		id := peer.ID("12D3KooWKnown" + string(rune('a'+i)))
		local.pex.knownPeers[id] = &PeerRecord{ID: string(id), Addrs: []string{"/ip4/203.0.113.9/tcp/28080"}, Score: peerStartingScore}
	}
	local.pex.mu.Unlock()

	local.pex.dialsInFlight.Store(2)
	local.pex.refillOutbound()
	if got := local.pex.dialsInFlight.Load(); got != 2 {
		t.Fatalf("started dials with enough in flight: %d", got)
	}
	local.pex.dialsInFlight.Store(0)
	local.pex.refillOutbound()
	if got := local.pex.dialsInFlight.Load(); got > 2 {
		t.Fatalf("%d dials for 2 missing peers", got)
	}
}

// startInboundOutcomePeers connects peers that dialed us.
func startInboundOutcomePeers(t *testing.T, local *Node, count int, status ChainStatus) []*outcomePeer {
	t.Helper()
	peers := make([]*outcomePeer, count)
	for i := range peers {
		node := mustNewTestNode(t)
		t.Cleanup(func() { _ = node.Stop() })
		mustConnectNodes(t, node, local)
		p := &outcomePeer{node: node, status: status}
		node.host.SetStreamHandler(protocolID(params.ProtocolSync), p.serve)
		peers[i] = p
	}
	return peers
}

func TestStrangerCannotBorrowDialedPeersCredibility(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	// Normal propagation: most of our dialed peers just got the next block.
	dialed := startOutcomePeers(t, local, 3, outcomeStatus(11, 110))
	for _, p := range dialed {
		p.silent.Store(true) // Still sending it, so the run stays open.
	}
	liar := startInboundOutcomePeers(t, local, 1, outcomeStatus(100000, 1000000))[0]
	liar.silent.Store(true)

	chain := &outcomeChain{height: 10, work: 100}
	sm := newOutcomeManager(t, local, chain, 500*time.Millisecond)
	sm.fetchEWMA.Store(int64(time.Millisecond))
	sm.checkSync()
	waitOutcome(t, "the run to start", 5*time.Second, sm.IsSyncing)
	if !sm.IsSyncingFarBehind(0) || sm.IsSyncingFarBehind(1) {
		t.Fatal("the node should count as the 1 block behind our dialed peers claim")
	}
	if sm.IsSyncingFarBehind(2) {
		t.Fatal("a stranger's claim stopped template serving")
	}
	sm.cancel()
	waitOutcome(t, "the run to end", 5*time.Second, func() bool { return !sm.IsSyncing() })
}

func TestLiarIsCooledOnceDialedPeersClaimIsMet(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	dialed := startOutcomePeers(t, local, 3, outcomeStatus(11, 110))
	for _, p := range dialed {
		p.blocks = outcomeBlocksUpTo(11)
	}
	liar := startOutcomePeers(t, local, 1, outcomeStatus(100000, 1000000))[0]
	liar.silent.Store(true)
	startOutcomePeers(t, local, 1, outcomeStatus(10, 100))

	chain := &outcomeChain{height: 10, work: 100}
	sm := newOutcomeManager(t, local, chain, 400*time.Millisecond)
	sm.fetchEWMA.Store(int64(time.Millisecond))
	started := time.Now()
	sm.checkSync()
	waitOutcome(t, "the dialed peers' block", 5*time.Second, func() bool { return chain.status().Height == 11 })
	waitOutcome(t, "the run to give up", 10*time.Second, func() bool { return !sm.IsSyncing() })
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the run took %s", elapsed)
	}
	if !sm.claimCoolingDown(liar.node) {
		t.Fatal("a peer claiming far past what our dialed peers claim should cool down")
	}
	if got := cooledCount(sm, dialed); got != 0 {
		t.Fatalf("%d dialed peers cooled though their claim was met", got)
	}
}

func TestFewDialedPeersCannotMarkTheNodeBehind(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	// One dialed peer, the attacker, and everyone else level with us.
	liar := startOutcomePeers(t, local, 1, outcomeStatus(5000, 50000))[0]
	liar.silent.Store(true)
	startInboundOutcomePeers(t, local, 5, outcomeStatus(10, 100))

	chain := &outcomeChain{height: 10, work: 100}
	sm := newOutcomeManager(t, local, chain, 300*time.Millisecond)
	sm.checkSync()
	waitOutcome(t, "the run to start", 5*time.Second, sm.IsSyncing)
	if sm.IsSyncingFarBehind(2) {
		t.Fatal("a single dialed peer stopped template serving")
	}
	waitOutcome(t, "the run to end", 10*time.Second, func() bool { return !sm.IsSyncing() })
	if !sm.claimCoolingDown(liar.node) {
		t.Fatal("the claimer should cool down")
	}
}

func TestDialedMajorityThatDeliversNothingMustProveItselfNext(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	liars := startOutcomePeers(t, local, 3, outcomeStatus(5000, 50000))
	for _, p := range liars {
		p.silent.Store(true)
	}
	chain := &outcomeChain{height: 10, work: 100}
	sm := newOutcomeManager(t, local, chain, 200*time.Millisecond)
	sm.fetchEWMA.Store(int64(time.Millisecond))
	sm.pauseCorroboration = true

	sm.checkSync()
	waitOutcome(t, "the run to start", 5*time.Second, sm.IsSyncing)
	if sm.IsSyncingFarBehind(2) {
		t.Fatal("the run after an empty corroborated run must prove itself")
	}
	waitOutcome(t, "the run to end", 10*time.Second, func() bool { return !sm.IsSyncing() })
	if got := cooledCount(sm, liars); got == 0 {
		t.Fatal("peers that keep delivering nothing should cool down")
	}
}

func TestCheckDuringARunIsNotLost(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	sm := newOutcomeManager(t, local, &outcomeChain{height: 10, work: 100}, time.Second)
	sm.mu.Lock()
	sm.syncing = true
	sm.mu.Unlock()
	sm.checkSync()
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if !sm.recheckAfterRun {
		t.Fatal("a check during a run should be remembered")
	}
}

func TestHighestWorkPeerGetsADownloader(t *testing.T) {
	for round := 0; round < 8; round++ {
		func() {
			local := mustNewTestNode(t)
			defer func() { _ = local.Stop() }()
			const ours, lagging, top = 100, 101, 105
			// More lagging peers than downloaders, so the top peer needs its place.
			startOutcomePeers(t, local, 1, outcomeStatus(top, top*10))[0].blocks = outcomeBlocksUpTo(top)
			for _, p := range startOutcomePeers(t, local, 11, outcomeStatus(lagging, lagging*10)) {
				p.blocks = outcomeBlocksUpTo(lagging)
			}
			chain := &outcomeChain{height: ours, work: ours * 10}
			sm := newOutcomeManager(t, local, chain, 5*time.Second)
			started := time.Now()
			sm.checkSync()
			waitOutcome(t, "the top height", 15*time.Second, func() bool { return chain.status().Height == top })
			if elapsed := time.Since(started); elapsed >= syncHedgeMin {
				t.Fatalf("round %d: reaching the top took %s", round, elapsed)
			}
		}()
	}
}

func TestBlocksPastTheDialedMedianKeepTheNodeBehind(t *testing.T) {
	local := mustNewTestNode(t)
	defer func() { _ = local.Stop() }()
	const ours, median, top = 100, 101, 1000
	// Most dialed peers lag just ahead of us; one holds the real chain.
	for _, p := range startOutcomePeers(t, local, 2, outcomeStatus(median, median*10)) {
		p.blocks = outcomeBlocksUpTo(median)
	}
	holder := startOutcomePeers(t, local, 1, outcomeStatus(top, top*10))[0]
	holder.blocks = outcomeBlocksUpTo(top)

	chain := &outcomeChain{height: ours, work: ours * 10, process: 5 * time.Millisecond}
	sm := newOutcomeManager(t, local, chain, 5*time.Second)
	sm.checkSync()
	waitOutcome(t, "blocks past the median", 10*time.Second, func() bool { return chain.status().Height >= median+syncBehindProofBlocks+2 })
	if !sm.IsSyncingFarBehind(2) {
		t.Fatalf("at height %d of %d the node should count as behind", chain.status().Height, top)
	}
	sm.cancel()
	waitOutcome(t, "the canceled run to end", 5*time.Second, func() bool { return !sm.IsSyncing() })
}

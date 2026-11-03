package main

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"blocknet/p2p"

	"github.com/libp2p/go-libp2p/core/peer"
)

// announcementFixture is a chain with a tip, a block that passes every cheap
// check on it, and a linked pair of nodes. The block's PoW isn't real, so tests
// replace the expensive step.
type announcementFixture struct {
	chain    *Chain
	daemon   *Daemon
	block    *Block
	data     []byte
	sender   *p2p.Node
	receiver *p2p.Node
}

func mustAnnouncementBlock(t *testing.T, chain *Chain, tip [32]byte, height uint64, ts int64) *Block {
	t.Helper()
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	coinbase, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, GetBlockReward(height), height)
	if err != nil {
		t.Fatalf("coinbase: %v", err)
	}
	b := precheckCandidate(t, chain, tip, height, ts)
	b.Transactions = []*Transaction{coinbase.Tx}
	root, err := b.ComputeMerkleRoot()
	if err != nil {
		t.Fatalf("merkle: %v", err)
	}
	b.Header.MerkleRoot = root
	return b
}

func newAnnouncementFixture(t *testing.T) *announcementFixture {
	t.Helper()
	chain, tip, tipTime := newPrecheckTestChain(t, 80)
	block := mustAnnouncementBlock(t, chain, tip, 81, tipTime+BlockIntervalSec)
	if !chain.passesCheapPrecheck(block) {
		t.Fatal("fixture block should pass the cheap checks")
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// mustStartLinkedTestNodes dials from its first node, so the sender is
	// inbound to the receiver: a peer with no track record.
	sender, receiver, stop := mustStartLinkedTestNodes(t)
	t.Cleanup(stop)
	return &announcementFixture{
		chain:    chain,
		block:    block,
		data:     data,
		sender:   sender,
		receiver: receiver,
		daemon: &Daemon{
			chain:   chain,
			mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember),
			node:    receiver,
		},
	}
}

// stubPreverify replaces the PoW step for one test. It signals when entered and
// holds until released.
func stubPreverify(t *testing.T, result error) (entered <-chan struct{}, release func(), calls *atomic.Int32) {
	t.Helper()
	in := make(chan struct{}, 16)
	out := make(chan struct{})
	var n atomic.Int32
	previous := runBlockPreverify
	runBlockPreverify = func(*Block) error {
		n.Add(1)
		in <- struct{}{}
		<-out
		return result
	}
	released := false
	rel := func() {
		if !released {
			released = true
			close(out)
		}
	}
	t.Cleanup(func() {
		rel()
		runBlockPreverify = previous
	})
	return in, rel, &n
}

func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verification never started")
	}
}

func TestAnnouncedBlockHoldsNoLockDuringVerification(t *testing.T) {
	f := newAnnouncementFixture(t)
	entered, release, _ := stubPreverify(t, errors.New("invalid proof of work"))
	done := make(chan error, 1)
	go func() { done <- announceBlock(f.daemon, f.sender.PeerID(), f.data) }()
	waitEntered(t, entered)

	if !f.daemon.mu.TryLock() {
		t.Fatal("the daemon lock is held during verification")
	}
	f.daemon.mu.Unlock()
	if !f.chain.mu.TryLock() {
		t.Fatal("the chain lock is held during verification")
	}
	f.chain.mu.Unlock()

	release()
	if err := <-done; err == nil {
		t.Fatal("a block that fails verification must be rejected")
	}
}

func TestSubmitBlockHoldsNoLockDuringVerification(t *testing.T) {
	f := newAnnouncementFixture(t)
	entered, release, _ := stubPreverify(t, errors.New("invalid proof of work"))
	done := make(chan error, 1)
	go func() {
		done <- f.daemon.SubmitBlock(f.block)
	}()
	waitEntered(t, entered)
	if !f.daemon.mu.TryLock() {
		t.Fatal("the daemon lock is held during verification")
	}
	f.daemon.mu.Unlock()
	release()
	if err := <-done; err == nil {
		t.Fatal("a block that fails verification must be rejected")
	}
}

func TestUntrustedPeersShareOneVerificationSlot(t *testing.T) {
	f := newAnnouncementFixture(t)
	entered, release, calls := stubPreverify(t, errors.New("invalid proof of work"))
	first := make(chan error, 1)
	go func() { first <- announceBlock(f.daemon, f.sender.PeerID(), f.data) }()
	waitEntered(t, entered)

	// A different block from another untrusted peer is dropped, not queued.
	other := mustAnnouncementBlock(t, f.chain, f.block.Header.PrevHash, 81, f.block.Header.Timestamp+1)
	data, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = announceBlock(f.daemon, f.sender.PeerID(), data)
	if !errors.Is(err, errBlockIntakeBusy) {
		t.Fatalf("got %v, want the intake to report busy", err)
	}
	if !errors.Is(err, ErrDuplicateBlock) {
		t.Fatal("a busy intake must not read as an invalid block")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ran %d verifications, want 1", got)
	}

	// A peer we dialed still gets verified. The sender dialed the receiver, so
	// from the sender's side the receiver is outbound.
	if !f.sender.IsTrustedBlockSource(f.receiver.PeerID()) {
		t.Fatal("a peer we dialed should be trusted")
	}
	if f.receiver.IsTrustedBlockSource(f.sender.PeerID()) {
		t.Fatal("an inbound peer with no history should not be trusted")
	}
	release()
	if err := <-first; err == nil {
		t.Fatal("expected the first block to fail verification")
	}
	if f.daemon.blockIntakeUntrusted {
		t.Fatal("the untrusted slot was not released")
	}
}

func TestOwnBlockDoesNotWaitForAVerificationSlot(t *testing.T) {
	f := newAnnouncementFixture(t)
	for i := 0; i < cap(blockVerifySlots); i++ {
		blockVerifySlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(blockVerifySlots); i++ {
			<-blockVerifySlots
		}
	}()
	previous := runBlockPreverify
	runBlockPreverify = func(*Block) error { return errors.New("invalid proof of work") }
	defer func() { runBlockPreverify = previous }()

	done := make(chan error, 1)
	go func() {
		done <- f.daemon.SubmitBlock(f.block)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the stubbed verification to reject the block")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("our own block waited behind other verifications")
	}
}

func TestPendingVerificationsAreBounded(t *testing.T) {
	f := newAnnouncementFixture(t)
	f.daemon.blockIntakePending = maxPendingBlockVerifications
	_, _, calls := stubPreverify(t, nil)
	// Even a peer we trust is dropped once too much work is queued.
	err := announceBlock(f.daemon, f.sender.PeerID(), f.data)
	if !errors.Is(err, errBlockIntakeBusy) {
		t.Fatalf("got %v, want the intake to report busy", err)
	}
	if calls.Load() != 0 {
		t.Fatal("a full queue should not start a verification")
	}
	if f.daemon.blockIntakePending != maxPendingBlockVerifications {
		t.Fatal("a dropped block must not change the pending count")
	}
}

func TestDeliveredBlocksEarnTrust(t *testing.T) {
	f := newAnnouncementFixture(t)
	if f.receiver.IsTrustedBlockSource(f.sender.PeerID()) {
		t.Fatal("no history yet")
	}
	f.receiver.RewardPeer(f.sender.PeerID())
	if !f.receiver.IsTrustedBlockSource(f.sender.PeerID()) {
		t.Fatal("a peer that delivered a block should earn trust")
	}
}

func TestSameBlockIsVerifiedOnceAtATime(t *testing.T) {
	f := newAnnouncementFixture(t)
	entered, release, calls := stubPreverify(t, errors.New("invalid proof of work"))
	first := make(chan error, 1)
	go func() { first <- announceBlock(f.daemon, f.sender.PeerID(), f.data) }()
	waitEntered(t, entered)

	if err := announceBlock(f.daemon, f.receiver.PeerID(), f.data); !errors.Is(err, ErrDuplicateBlock) {
		t.Fatalf("got %v, want a duplicate", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ran %d verifications, want 1", got)
	}
	release()
	<-first
	if len(f.daemon.blockIntakeInFlight) != 0 {
		t.Fatal("the in-flight entry was not cleared")
	}
}

func TestGossipBansOnlyForInvalidBlocks(t *testing.T) {
	t.Run("orphan", func(t *testing.T) {
		f := newAnnouncementFixture(t)
		orphan := *f.block
		orphan.Header.PrevHash = [32]byte{0xEE}
		data, err := json.Marshal(&orphan)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		f.daemon.handleBlock(f.sender.PeerID(), data)
		if f.receiver.IsBanned(f.sender.PeerID()) {
			t.Fatal("an orphan means we missed a block, not that the peer misbehaved")
		}
	})
	t.Run("busy", func(t *testing.T) {
		f := newAnnouncementFixture(t)
		f.daemon.blockIntakeUntrusted = true
		_, _, calls := stubPreverify(t, nil)
		f.daemon.handleBlock(f.sender.PeerID(), f.data)
		if f.receiver.IsBanned(f.sender.PeerID()) {
			t.Fatal("a busy node must not ban the peer relaying to it")
		}
		if calls.Load() != 0 {
			t.Fatal("a busy slot should not start a verification")
		}
	})
	t.Run("invalid proof of work", func(t *testing.T) {
		f := newAnnouncementFixture(t)
		entered, release, _ := stubPreverify(t, errors.New("invalid proof of work"))
		done := make(chan struct{})
		go func() {
			f.daemon.handleBlock(f.sender.PeerID(), f.data)
			close(done)
		}()
		waitEntered(t, entered)
		release()
		<-done
		if !f.receiver.IsBanned(f.sender.PeerID()) {
			t.Fatal("a forged block should still cost its sender")
		}
	})
}

func TestCheapChecksRunBeforeProofOfWork(t *testing.T) {
	chain, tip, tipTime := newPrecheckTestChain(t, 80)
	block := mustAnnouncementBlock(t, chain, tip, 81, tipTime+BlockIntervalSec)
	block.Header.MerkleRoot[0] ^= 0xFF
	if chain.NeedsPreverification(block) {
		t.Fatal("a wrong merkle root should not need verification")
	}
	started := time.Now()
	_, _, err := chain.ProcessBlock(block)
	if err == nil || err.Error() != "invalid merkle root" {
		t.Fatalf("got %v, want the merkle check to fail first", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("rejecting took %s, which suggests PoW ran", elapsed)
	}
}

// announceBlock runs an announced block through the daemon as the sync manager
// does, including the work that follows the relay.
func announceBlock(d *Daemon, from peer.ID, data []byte) error {
	afterRelay, err := d.processAnnouncedBlock(from, data)
	if afterRelay != nil {
		afterRelay()
	}
	return err
}

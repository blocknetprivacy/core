package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func newPenaltyTestNode() *Node {
	n := &Node{}
	n.pex = NewPeerExchange(n, nil, nil)
	return n
}

func TestHandleNewBlock_PenalizesInvalidAnnouncement(t *testing.T) {
	n := newPenaltyTestNode()
	sm := NewSyncManager(n, SyncConfig{
		ProcessBlock: func(data []byte) error {
			return errors.New("invalid block")
		},
		IsOrphanError:    func(error) bool { return false },
		IsDuplicateError: func(error) bool { return false },
	})

	pid := peer.ID("12D3KooWInvalidAnnouncePeer")
	sm.handleNewBlock(pid, []byte("bad-block"))

	if got := n.BannedCount(); got != 1 {
		t.Fatalf("expected invalid announcement peer to be banned, bannedCount=%d", got)
	}
}

func TestHandleNewBlock_DoesNotPenalizeOrphanOrDuplicate(t *testing.T) {
	t.Run("orphan", func(t *testing.T) {
		n := newPenaltyTestNode()
		orph := errors.New("orphan")
		sm := NewSyncManager(n, SyncConfig{
			ProcessBlock:     func(data []byte) error { return orph },
			IsOrphanError:    func(err error) bool { return errors.Is(err, orph) },
			IsDuplicateError: func(error) bool { return false },
		})
		sm.handleNewBlock(peer.ID("12D3KooWOrphanPeer000001"), []byte("orphan"))
		if got := n.BannedCount(); got != 0 {
			t.Fatalf("orphan announcement should not be penalized, bannedCount=%d", got)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		n := newPenaltyTestNode()
		dup := errors.New("duplicate")
		sm := NewSyncManager(n, SyncConfig{
			ProcessBlock:     func(data []byte) error { return dup },
			IsOrphanError:    func(error) bool { return false },
			IsDuplicateError: func(err error) bool { return errors.Is(err, dup) },
		})
		sm.handleNewBlock(peer.ID("12D3KooWDuplicatePeer0001"), []byte("dup"))
		if got := n.BannedCount(); got != 0 {
			t.Fatalf("duplicate announcement should not be penalized, bannedCount=%d", got)
		}
	})
}

func TestFetchBlockByHashFromAnyPeer_PenalizesEmptyUndecodableAndMismatched(t *testing.T) {
	targetHash := [32]byte{0xAA}
	pid := peer.ID("12D3KooWFetchPenaltyPeer01")
	peers := []PeerStatus{{Peer: pid}}

	t.Run("empty response", func(t *testing.T) {
		n := newPenaltyTestNode()
		sm := &SyncManager{
			node: n,
			fetchBlocksByHash: func(context.Context, peer.ID, [][32]byte) ([][]byte, error) {
				return [][]byte{}, nil
			},
			getBlockHash: func(data []byte) ([32]byte, error) {
				return [32]byte{}, nil
			},
		}

		_, _, err := sm.fetchBlockByHashFromAnyPeer(context.Background(), peers, targetHash, nil)
		if err == nil {
			t.Fatal("expected error from empty block response")
		}
		if got := n.BannedCount(); got != 1 {
			t.Fatalf("expected empty response peer penalty, bannedCount=%d", got)
		}
	})

	t.Run("undecodable response", func(t *testing.T) {
		n := newPenaltyTestNode()
		sm := &SyncManager{
			node: n,
			fetchBlocksByHash: func(context.Context, peer.ID, [][32]byte) ([][]byte, error) {
				return [][]byte{[]byte("not-a-block")}, nil
			},
			getBlockHash: func(data []byte) ([32]byte, error) {
				return [32]byte{}, errors.New("decode failed")
			},
		}

		_, _, err := sm.fetchBlockByHashFromAnyPeer(context.Background(), peers, targetHash, nil)
		if err == nil {
			t.Fatal("expected error from undecodable block response")
		}
		if got := n.BannedCount(); got != 1 {
			t.Fatalf("expected undecodable response peer penalty, bannedCount=%d", got)
		}
	})

	t.Run("mismatched hash response", func(t *testing.T) {
		n := newPenaltyTestNode()
		sm := &SyncManager{
			node: n,
			fetchBlocksByHash: func(context.Context, peer.ID, [][32]byte) ([][]byte, error) {
				return [][]byte{[]byte("fake-block")}, nil
			},
			getBlockHash: func(data []byte) ([32]byte, error) {
				return [32]byte{0xBB}, nil // deliberately mismatched
			},
		}

		_, _, err := sm.fetchBlockByHashFromAnyPeer(context.Background(), peers, targetHash, nil)
		if err == nil {
			t.Fatal("expected error from mismatched hash response")
		}
		if got := n.BannedCount(); got != 1 {
			t.Fatalf("expected mismatched-hash response peer penalty, bannedCount=%d", got)
		}
	})
}

func TestProcessBlockWithRecoveryCtx_DoesNotBanPeerForHashMatchingParentValidationFailure(t *testing.T) {
	errOrphan := errors.New("orphan")
	errParent := errors.New("double-spend on competing fork")
	parentHash := [32]byte{0xAA}
	childHash := [32]byte{0xBB}

	parentData := encodeRecoveryBlock(t, recoveryTestBlock{
		Height: 10,
		Hash:   parentHash,
		Prev:   [32]byte{0x09},
	})
	childData := encodeRecoveryBlock(t, recoveryTestBlock{
		Height: 11,
		Hash:   childHash,
		Prev:   parentHash,
	})

	n := newPenaltyTestNode()
	pid := peer.ID("12D3KooWRecoveryNoBanPeer1")
	sm := NewSyncManager(n, SyncConfig{
		ProcessBlock: func(data []byte) error {
			var b recoveryTestBlock
			if err := json.Unmarshal(data, &b); err != nil {
				return err
			}
			switch b.Hash {
			case childHash:
				return errOrphan
			case parentHash:
				return errParent
			default:
				return nil
			}
		},
		IsOrphanError:    func(err error) bool { return errors.Is(err, errOrphan) },
		IsDuplicateError: func(error) bool { return false },
		GetBlockMeta: func(data []byte) (uint64, [32]byte, error) {
			var b recoveryTestBlock
			if err := json.Unmarshal(data, &b); err != nil {
				return 0, [32]byte{}, err
			}
			return b.Height, b.Prev, nil
		},
		GetBlockHash: func(data []byte) ([32]byte, error) {
			var b recoveryTestBlock
			if err := json.Unmarshal(data, &b); err != nil {
				return [32]byte{}, err
			}
			return b.Hash, nil
		},
		FetchBlocksByHash: func(ctx context.Context, p peer.ID, hashes [][32]byte) ([][]byte, error) {
			if p != pid {
				return nil, errors.New("unexpected peer")
			}
			if len(hashes) == 1 && hashes[0] == parentHash {
				return [][]byte{parentData}, nil
			}
			return nil, errors.New("unexpected hash request")
		},
	})

	peers := []PeerStatus{{Peer: pid}}
	accepted, err := sm.ProcessBlockWithRecoveryCtx(context.Background(), childData, peers)
	if err == nil {
		t.Fatal("expected orphan recovery to fail on parent validation error")
	}
	if accepted {
		t.Fatal("expected failed orphan recovery to not report acceptance")
	}
	if !strings.Contains(err.Error(), errParent.Error()) {
		t.Fatalf("expected orphan recovery error to retain parent validation failure, got: %v", err)
	}
	if got := n.BannedCount(); got != 0 {
		t.Fatalf("expected hash-matching parent validation failure to avoid peer ban, bannedCount=%d", got)
	}
}

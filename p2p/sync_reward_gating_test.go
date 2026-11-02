package p2p

import (
	"context"
	"errors"
	"testing"
	"time"

	"blocknet/protocol/params"
)

func TestSyncRewardsPeerOnlyAfterAccepted_NotOnInvalidOrDuplicate(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()
	b := mustNewTestNode(t)
	defer func() { _ = b.Stop() }()
	mustConnectNodes(t, a, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Minimal "block" bytes for height 1 (sync works on raw []byte; validity is enforced by ProcessBlock callback).
	const h1 uint64 = 1
	blockAtHeight := func(h uint64) []byte { return []byte{byte(h), 0xAB, 0xCD} }

	// Node B: serve blocks-by-height over the real sync protocol.
	smB := NewSyncManager(b, SyncConfig{
		GetStatus: func() ChainStatus {
			return ChainStatus{Height: h1, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}
		},
		GetBlocksByHeight: func(startHeight uint64, max int) ([][]byte, error) {
			out := make([][]byte, 0, max)
			for i := 0; i < max; i++ {
				h := startHeight + uint64(i)
				if h > h1 {
					break
				}
				out = append(out, blockAtHeight(h))
			}
			return out, nil
		},
	})
	smB.Start(ctx)
	defer smB.Stop()

	// Node A: sync from B, and track rewards via peer score deltas.
	dupErr := errors.New("duplicate block")
	invalidErr := errors.New("invalid block")

	ourHeight := uint64(0)
	processMode := "accept"

	smA := NewSyncManager(a, SyncConfig{
		GetStatus: func() ChainStatus {
			return ChainStatus{Height: ourHeight, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}
		},
		ProcessBlock: func(data []byte) error {
			switch processMode {
			case "accept":
				return nil
			case "duplicate":
				return dupErr
			case "invalid":
				return invalidErr
			default:
				return nil
			}
		},
		IsDuplicateError: func(err error) bool {
			return errors.Is(err, dupErr)
		},
		IsOrphanError: func(err error) bool { return false },
	})
	smA.Start(ctx)
	defer smA.Stop()

	peers := []PeerStatus{{Peer: b.PeerID(), Status: ChainStatus{Height: h1, Version: 1, NetworkID: params.NetworkID, ChainID: params.ChainID}}}

	t.Run("invalid_not_rewarded_and_penalized", func(t *testing.T) {
		// Ensure score record exists via the invalid-path penalty creation.
		processMode = "invalid"
		smA.parallelSyncFrom(peers, h1, true)

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			score := a.pex.GetPeerScore(b.PeerID())
			if score == 50+ScorePenaltyMisbehave {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("expected invalid block peer to be penalized (score=%d)", a.pex.GetPeerScore(b.PeerID()))
	})

	t.Run("accepted_rewarded_once", func(t *testing.T) {
		// Reset score to a known baseline by creating a record at score=50.
		a.pex.PenalizePeer(b.PeerID(), 0, "seed record for reward")

		before := a.pex.GetPeerScore(b.PeerID())
		if before < 0 {
			t.Fatalf("expected known peer score record to exist")
		}

		processMode = "accept"
		smA.parallelSyncFrom(peers, h1, true)

		after := a.pex.GetPeerScore(b.PeerID())
		if after != before+ScoreRewardGood {
			t.Fatalf("expected reward after accepted block: before=%d after=%d", before, after)
		}
	})

	t.Run("duplicate_not_rewarded", func(t *testing.T) {
		before := a.pex.GetPeerScore(b.PeerID())
		if before < 0 {
			t.Fatalf("expected known peer score record to exist")
		}

		processMode = "duplicate"
		smA.parallelSyncFrom(peers, h1, true)

		after := a.pex.GetPeerScore(b.PeerID())
		if after != before {
			t.Fatalf("expected no reward for duplicate: before=%d after=%d", before, after)
		}
	})
}

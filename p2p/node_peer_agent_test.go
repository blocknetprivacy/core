package p2p

import (
	"testing"
	"time"
)

func TestNode_PeerInfosReportsAgent(t *testing.T) {
	a := mustNewTestNode(t)
	defer func() { _ = a.Stop() }()

	cfg := DefaultNodeConfig()
	cfg.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.SeedNodes = nil
	cfg.UserAgent = "blocknet/test"
	b, err := NewNode(cfg)
	if err != nil {
		t.Fatalf("failed to create test node: %v", err)
	}
	defer func() { _ = b.Stop() }()

	mustConnectNodes(t, a, b)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, info := range a.PeerInfos() {
			if info.ID == b.PeerID() && info.Agent == "blocknet/test" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("peer agent not reported: %+v", a.PeerInfos())
}

package p2p

import "testing"

func TestSetSeedNodesReplacesSeedsBeforeStart(t *testing.T) {
	n := mustNewTestNode(t)
	defer n.Stop()

	const seed = "/ip4/127.0.0.1/tcp/28080/p2p/12D3KooWJrLG1Y71bzVSTCpwyhjbJNUB8LjpWCYNQsMgEuWkH4LE"
	n.SetSeedNodes([]string{"not-a-multiaddr", seed})

	n.pex.mu.RLock()
	defer n.pex.mu.RUnlock()
	if len(n.pex.seedNodes) != 1 {
		t.Fatalf("seed nodes = %v, want only the valid seed", n.pex.seedNodes)
	}
	if got := n.pex.seedNodes[0].ID.String(); got != "12D3KooWJrLG1Y71bzVSTCpwyhjbJNUB8LjpWCYNQsMgEuWkH4LE" {
		t.Fatalf("seed peer ID = %s", got)
	}
}

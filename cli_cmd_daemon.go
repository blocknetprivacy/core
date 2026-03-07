package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func (c *CLI) cmdVersion() {
	fmt.Printf("\n%s\n", c.sectionHead("Version "+Version))
}

func (c *CLI) cmdLicense() {
	fmt.Printf("\n%s\n", c.sectionHead("License"))
	for _, line := range strings.Split(strings.TrimSpace(licenseText), "\n") {
		fmt.Printf("  %s\n", line)
	}
}

func (c *CLI) cmdAbout() {
	fmt.Printf("\n%s\n", c.sectionHead("About"))
	fmt.Printf("  Blocknet v%s\n", Version)
	fmt.Println("  Zero-knowledge money. Made in USA.")
	fmt.Println()
	fmt.Println("  BSD 3-Clause License")
	fmt.Println("  Copyright (c) 2026, Blocknet Privacy")
	fmt.Println()
	fmt.Println("  https://blocknetcrypto.com")
	fmt.Println("  https://explorer.blocknetcrypto.com")
	fmt.Println("  https://github.com/blocknetprivacy")
	fmt.Printf("\n%s\n", c.sectionHead("Third-Party Libraries"))
	fmt.Println("  libp2p/go-libp2p             MIT          P2P networking")
	fmt.Println("  pion/webrtc                  MIT          WebRTC transport")
	fmt.Println("  quic-go/quic-go              MIT          QUIC transport")
	fmt.Println("  multiformats/go-multiaddr    MIT          Network addressing")
	fmt.Println("  etcd-io/bbolt                MIT          Key-value storage")
	fmt.Println("  lukechampine/blake3          MIT          Hashing")
	fmt.Println("  uber-go/fx                   MIT          Dependency injection")
	fmt.Println("  uber-go/zap                  MIT          Logging")
	fmt.Println("  btcsuite/btcutil             ISC          Base58 encoding")
	fmt.Println("  flynn/noise                  ISC          Noise protocol")
	fmt.Println("  gorilla/websocket            ISC          WebSocket")
	fmt.Println("  golang.org/x/crypto          BSD-3-Clause Argon2, SHA-3")
	fmt.Println("  golang.org/x/term            BSD-3-Clause Terminal I/O")
	fmt.Println("  golang.org/x/time            BSD-3-Clause Rate limiting")
	fmt.Println("  google.golang.org/protobuf   BSD-3-Clause Serialization")
	fmt.Println("  gogo/protobuf                BSD-3-Clause Serialization")
	fmt.Println("  prometheus/client_golang     Apache-2.0   Metrics")
	fmt.Println("  hashicorp/golang-lru         MPL-2.0      LRU cache")
	fmt.Println("  libp2p/go-yamux              MPL-2.0      Stream multiplexing")
}

func (c *CLI) cmdStatus() {
	stats := c.daemon.Stats()
	total, unspent := c.wallet.OutputCount()

	walletType := "Full"
	if c.wallet.IsViewOnly() {
		walletType = "View-Only (cannot spend)"
	}

	height := stats.ChainHeight
	spendable := c.wallet.SpendableBalance(height)
	pending := c.wallet.PendingBalance(height)

	balanceStr := formatAmount(spendable)
	if pending > 0 {
		balanceStr += fmt.Sprintf(" + %s pending", formatAmount(pending))
	}

	fmt.Printf(`
%s
  Peer ID:     %s
  Peers:       %d
  Height:      %d
  Best Hash:   %s
  Syncing:     %v
  Uptime:      %s

%s
  Type:        %s
  Balance:     %s
  Outputs:     %d unspent / %d total
  Synced To:   %d
  Address:     %s
`,
		c.sectionHead("Node"),
		stats.PeerID,
		stats.Peers,
		stats.ChainHeight,
		stats.BestHash,
		stats.Syncing,
		time.Since(c.startTime).Round(time.Second),
		c.sectionHead("Wallet"),
		walletType,
		balanceStr,
		unspent, total,
		c.wallet.SyncedHeight(),
		c.wallet.Address(),
	)
}

func (c *CLI) cmdPeers() {
	infos := c.daemon.Node().PeerInfos()
	banned := c.daemon.Node().BannedCount()

	fmt.Printf("\n%s", c.sectionHead("Peers"))
	if len(infos) == 0 {
		fmt.Println(" (0)")
		fmt.Println("  None connected")
	} else {
		fmt.Printf(" (%d)\n", len(infos))
		for _, info := range infos {
			fmt.Printf("  %s\n", info.ID.String())
			for _, addr := range info.Addrs {
				fmt.Printf("    %s\n", addr)
			}
		}
	}

	if banned > 0 {
		fmt.Printf("\n  %d banned (use 'banned' to see details)\n", banned)
	}
}

func (c *CLI) cmdBanned() {
	bans := c.daemon.Node().GetBannedPeers()
	fmt.Printf("\n%s", c.sectionHead("Banned"))
	if len(bans) == 0 {
		fmt.Println(" (0)")
		fmt.Println("  None")
		return
	}

	fmt.Printf(" (%d)\n", len(bans))
	for _, ban := range bans {
		durStr := "permanent"
		if !ban.Permanent {
			remaining := time.Until(ban.ExpiresAt).Round(time.Minute)
			durStr = fmt.Sprintf("expires in %s", remaining)
		}
		fmt.Printf("  %s\n", ban.PeerID.String())
		for _, addr := range ban.Addrs {
			fmt.Printf("    addr:   %s\n", addr)
		}
		fmt.Printf("    reason: %s\n    count:  %dx, %s\n",
			ban.Reason,
			ban.BanCount,
			durStr,
		)
	}
}

func (c *CLI) cmdExportPeer() error {
	if err := c.daemon.Node().WritePeerFile("peer.txt"); err != nil {
		return err
	}

	fmt.Printf("\n%s\n", c.sectionHead("Export"))
	fmt.Println("  Peer addresses written to peer.txt")
	fmt.Println("  Share this file or its contents with other nodes.")
	fmt.Println("\n  Other nodes can connect with:")
	for _, addr := range c.daemon.Node().FullMultiaddrs() {
		fmt.Printf("    ./blocknet %s\n", addr)
	}
	return nil
}

func (c *CLI) cmdMining(args []string) error {
	if len(args) == 0 {
		if c.daemon.IsMining() {
			stats := c.daemon.MinerStats()
			hashRate := c.daemon.Miner().HashRate()
			elapsed := time.Since(stats.StartTime).Round(time.Second)
			fmt.Printf("\n%s — active (%s)\n", c.sectionHead("Mining"), elapsed)
			fmt.Printf("  Hashrate:     %.2f H/s\n", hashRate)
			fmt.Printf("  Total hashes: %d\n", stats.HashCount)
			fmt.Printf("  Chain height: %d\n", c.daemon.Chain().Height())
		} else {
			fmt.Printf("\n%s — stopped\n", c.sectionHead("Mining"))
		}
		return nil
	}

	switch args[0] {
	case "start":
		fmt.Printf("\n%s\n", c.sectionHead("Mining"))
		if !MiningSupported {
			fmt.Println("  Mining is not supported on this platform")
			return nil
		}
		if c.daemon.IsMining() {
			fmt.Println("  Already running")
			return nil
		}
		threads := c.daemon.Miner().Threads()
		c.daemon.StartMining()
		fmt.Printf("  Started with %d threads (~%dGB RAM)\n", threads, threads*2)
	case "stop":
		fmt.Printf("\n%s\n", c.sectionHead("Mining"))
		if !c.daemon.IsMining() {
			fmt.Println("  Not running")
			return nil
		}
		c.daemon.StopMining()
		fmt.Println("  Stopped")
	case "threads", "thrads", "thread", "thrad", "t":
		fmt.Printf("\n%s\n", c.sectionHead("Mining"))
		if len(args) < 2 {
			fmt.Printf("  Threads: %d\n", c.daemon.Miner().Threads())
			return nil
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return fmt.Errorf("usage: mining threads <N> (N >= 1)")
		}
		c.daemon.Miner().SetThreads(n)
		fmt.Printf("  Threads set to %d (~%dGB RAM)\n", n, n*2)
		if c.daemon.IsMining() {
			fmt.Println("  Restarting current block attempt")
		}
	default:
		return fmt.Errorf("usage: mining [start|stop|threads <N>]")
	}
	return nil
}

func (c *CLI) cmdCertify() {
	fmt.Printf("\n%s\n", c.sectionHead("Certify"))
	chain := c.daemon.Chain()
	height := chain.Height()
	fmt.Printf("  Checking %d blocks...\n", height)

	violations := chain.VerifyChain()
	if len(violations) == 0 {
		fmt.Printf("  Chain is clean. All %d blocks passed.\n", height)
		return
	}

	fmt.Printf("\n  %s\n", c.errorHead(fmt.Sprintf("%d violation(s)", len(violations))))
	for _, v := range violations {
		fmt.Printf("    Height %d: %s\n", v.Height, v.Message)
	}
	fmt.Println("\n  Consider purging chain data and re-syncing from trusted peers.")
}

func (c *CLI) cmdPurgeData() error {
	fmt.Printf("\n%s\n", c.errorHead("Purge"))
	fmt.Printf("  This will delete all blockchain data from %s\n", c.dataDir)
	fmt.Println("  This includes all blocks, chain state, and sync progress.")
	fmt.Println("  Your wallet will NOT be deleted.")
	fmt.Println("  This action CANNOT be undone.")
	fmt.Print("\n  Confirm purge? [y/N]: ")

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		fmt.Println("  Cancelled")
		return nil
	}

	fmt.Println("  Stopping daemon...")
	if err := c.daemon.Stop(); err != nil {
		return fmt.Errorf("failed to stop daemon before purge: %w", err)
	}

	fmt.Printf("  Purging blockchain data from %s...\n", c.dataDir)
	if err := os.RemoveAll(c.dataDir); err != nil {
		return fmt.Errorf("failed to purge blockchain data: %w", err)
	}

	fmt.Println("  Blockchain data purged. Restart to resync from genesis.")

	// Exit after purge since daemon is stopped
	return fmt.Errorf("quit")
}

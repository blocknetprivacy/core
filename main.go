package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"

	"blocknet/protocol/params"
)

const Version = "0.21.0"

type peerIDListFlag []string

func (f *peerIDListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *peerIDListFlag) Set(value string) error {
	for _, raw := range strings.Split(value, ",") {
		id := strings.TrimSpace(raw)
		if id == "" {
			return fmt.Errorf("peer ID must not be empty")
		}
		parsed, err := peer.Decode(id)
		if err != nil {
			return fmt.Errorf("invalid peer ID %q: %w", id, err)
		}
		*f = append(*f, parsed.String())
	}
	return nil
}

func loadWhitelistFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("expected JSON array of peer ID strings: %w", err)
	}
	for i, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("entry %d: peer ID must not be empty", i)
		}
		if _, err := peer.Decode(id); err != nil {
			return nil, fmt.Errorf("entry %d: invalid peer ID %q: %w", i, id, err)
		}
	}
	return ids, nil
}

func main() {
	// Parse command line flags
	version := flag.Bool("version", false, "Print version and exit")
	testnet := flag.Bool("testnet", false, "Run on testnet (isolated network, different ports/data)")
	walletFile := flag.String("wallet", DefaultWalletFilename, "Path to wallet file")
	dataDir := flag.String("data", DefaultDataDir, "Data directory")
	listen := flag.String("listen", "/ip4/0.0.0.0/tcp/28080", "P2P listen address")
	p2pMaxInbound := flag.Int("p2p-max-inbound", 0, "Max inbound P2P peers (0 = default)")
	p2pMaxOutbound := flag.Int("p2p-max-outbound", 0, "Max outbound P2P peers (0 = default)")
	seedMode := flag.Bool("seed", false, "Run as seed node (persistent P2P identity)")
	explorerAddr := flag.String("explorer", "", "HTTP address for block explorer (e.g. :8080)")
	apiAddr := flag.String("api", "", "API listen address (e.g. 127.0.0.1:8332)")
	noColor := flag.Bool("nocolor", false, "Disable colored output")
	flag.Bool("no-version-check", false, "Disable remote version check on startup")
	saveCheckpoints := flag.Bool("save-checkpoints", false, "Append a record to checkpoints.dat every 100 blocks (writes to data dir)")
	fullSync := flag.Bool("full-sync", false, "Bypass checkpoints (download + verification) and sync naturally from peers")
	var p2pWhitelistPeers peerIDListFlag
	flag.Var(&p2pWhitelistPeers, "p2p-whitelist-peer", "Peer ID to exempt from peer bans (repeatable or comma-separated)")
	p2pWhitelistFile := flag.String("p2p-whitelist", "", "Path to JSON file containing peer IDs to exempt from bans")
	registerRemovedCLIFlags(flag.CommandLine)
	flag.Parse()

	if name, replacement, ok := removedCLIFlagUse(flag.CommandLine); ok {
		printRemovedCLIFlag(os.Stderr, name, replacement)
		os.Exit(1)
	}

	if *version {
		fmt.Println(Version)
		return
	}

	if *testnet {
		params.InitTestnet()
		if *dataDir == DefaultDataDir {
			*dataDir = TestnetDataDir
		}
		if *walletFile == DefaultWalletFilename {
			*walletFile = TestnetWalletFilename
		}
		if *listen == "/ip4/0.0.0.0/tcp/28080" {
			*listen = "/ip4/0.0.0.0/tcp/38080"
		}
		if !*fullSync {
			*fullSync = true
		}
		// Clear any mainnet P2P key from the environment so testnet
		// generates its own identity in the testnet data dir.
		_ = os.Unsetenv("BLOCKNET_P2P_KEY")
	}

	if *p2pMaxInbound < 0 {
		fmt.Fprintln(os.Stderr, "Error: --p2p-max-inbound must be >= 0")
		os.Exit(1)
	}
	if *p2pMaxOutbound < 0 {
		fmt.Fprintln(os.Stderr, "Error: --p2p-max-outbound must be >= 0")
		os.Exit(1)
	}

	// Seed mode: ensure P2P identity is persistent (and stable across restarts).
	// Store in XDG config with network separation so mainnet/testnet don't clobber.
	if *seedMode && strings.TrimSpace(os.Getenv("BLOCKNET_P2P_KEY")) == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to get config dir for seed identity: %v\n", err)
		} else {
			network := "mainnet"
			if *testnet {
				network = "testnet"
			}
			keyPath := filepath.Join(configDir, "blocknet", network, "identity.key")
			if err := os.Setenv("BLOCKNET_P2P_KEY", keyPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to set BLOCKNET_P2P_KEY: %v\n", err)
			}
		}
	}
	// Seed bootstrap assist: if we fail to start due to missing seed reachability,
	// still export our peer addresses so operators can bake updated seed IDs.
	if *seedMode && strings.TrimSpace(os.Getenv("BLOCKNET_EXPORT_PEER_ON_START_FAIL")) == "" {
		_ = os.Setenv("BLOCKNET_EXPORT_PEER_ON_START_FAIL", "1")
	}

	// Resolve seed nodes: try dynamic peer ID fetch, fall back to hardcoded.
	p2pPort := MainnetP2PPort
	peerIDPort := MainnetPeerIDPort
	fallbackSeeds := DefaultSeedNodes
	if *testnet {
		p2pPort = TestnetP2PPort
		peerIDPort = TestnetPeerIDPort
		fallbackSeeds = DefaultTestnetSeedNodes
	}
	seedNodes := ResolveSeedNodes(DefaultSeedHosts, p2pPort, peerIDPort)
	if len(seedNodes) == 0 {
		seedNodes = fallbackSeeds
	}
	if len(flag.Args()) > 0 {
		seedNodes = append(seedNodes, flag.Args()...)
	}

	whitelistPeers := []string(p2pWhitelistPeers)
	whitelistPath := *p2pWhitelistFile
	if whitelistPath == "" {
		if configDir, err := os.UserConfigDir(); err == nil {
			network := "mainnet"
			if *testnet {
				network = "testnet"
			}
			candidate := filepath.Join(configDir, "blocknet", network, "whitelist.json")
			if _, err := os.Stat(candidate); err == nil {
				whitelistPath = candidate
			}
		}
	}
	if whitelistPath != "" {
		filePeers, err := loadWhitelistFile(whitelistPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading whitelist file %s: %v\n", whitelistPath, err)
			os.Exit(1)
		}
		whitelistPeers = append(whitelistPeers, filePeers...)
	}

	cfg := CLIConfig{
		WalletFile:        *walletFile,
		DataDir:           *dataDir,
		ListenAddrs:       []string{*listen},
		SeedNodes:         seedNodes,
		P2PWhitelistPeers: whitelistPeers,
		P2PMaxInbound:     *p2pMaxInbound,
		P2PMaxOutbound:    *p2pMaxOutbound,
		ExplorerAddr:      *explorerAddr,
		APIAddr:           *apiAddr,
		NoColor:           *noColor,
		SaveCheckpoints:   *saveCheckpoints,
		FullSync:          *fullSync,
		SeedMode:          *seedMode,
	}

	cli, err := NewCLI(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := cli.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

var removedCLIFlags = []struct{ name, replacement string }{
	{"cli", "blocknet attach"},
	{"daemon", "blocknet attach"},
	{"recover", "blocknet attach, then: import (option 1, recovery seed)"},
	{"viewonly", "blocknet attach, then: import (option 3, view-only)"},
	{"spend-pub", "blocknet attach, then: import (option 3, view-only)"},
	{"view-priv", "blocknet attach, then: import (option 3, view-only)"},
	{"view-priv-env", "blocknet attach, then: import (option 3, view-only)"},
	{"output-peer-address", "blocknet attach, then: export-peer"},
}

func registerRemovedCLIFlags(fs *flag.FlagSet) {
	fs.Bool("cli", false, "Removed: use the Blocknet CLI (mantle)")
	fs.Bool("daemon", true, "Always on: the core runs headless")
	fs.Bool("recover", false, "Removed: use the Blocknet CLI (mantle)")
	fs.Bool("viewonly", false, "Removed: use the Blocknet CLI (mantle)")
	fs.String("spend-pub", "", "Removed: use the Blocknet CLI (mantle)")
	fs.String("view-priv", "", "Removed: use the Blocknet CLI (mantle)")
	fs.String("view-priv-env", "", "Removed: use the Blocknet CLI (mantle)")
	fs.Bool("output-peer-address", false, "Removed: use the Blocknet CLI (mantle)")
}

func removedCLIFlagUse(fs *flag.FlagSet) (name, replacement string, ok bool) {
	set := map[string]*flag.Flag{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = f })
	for _, r := range removedCLIFlags {
		f, used := set[r.name]
		if !used || (r.name == "daemon" && f.Value.String() == "true") {
			continue
		}
		return r.name, r.replacement, true
	}
	return "", "", false
}

func printRemovedCLIFlag(w io.Writer, name, replacement string) {
	fmt.Fprintln(w, "The Blocknet core command-line interface has been removed.")
	fmt.Fprintln(w, "Use the Blocknet CLI (mantle) instead:")
	fmt.Fprintln(w, "  https://github.com/blocknetprivacy/blocknet/releases/latest")
	fmt.Fprintln(w, "Source and docs: https://github.com/blocknetprivacy/blocknet")
	fmt.Fprintf(w, "Replacement for --%s: %s\n", name, replacement)
}

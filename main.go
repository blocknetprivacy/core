package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"

	"blocknet/protocol/params"
)

const Version = "0.23.0"

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
	version := flag.Bool("version", false, "Print version and exit")
	testnet := flag.Bool("testnet", false, "Run on testnet (isolated network, different ports/data)")
	dataDir := flag.String("data", DefaultDataDir, "Data directory")
	apiAddr := flag.String("api", "", "API listen address (e.g. 127.0.0.1:8332)")
	powCache := flag.Bool("pow-cache", true, "Keep the 2 GiB PoW buffer between hashes (set false for low-RAM servers)")
	legacy := registerLegacySettingsFlags(flag.CommandLine)
	registerRemovedCLIFlags(flag.CommandLine)
	flag.Parse()

	if name, replacement, ok := removedCLIFlagUse(flag.CommandLine); ok {
		printRemovedCLIFlag(os.Stderr, name, replacement)
		os.Exit(1)
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\nAdd peers with extra_peers in PATCH /api/settings.\n", strings.Join(flag.Args(), " "))
		os.Exit(1)
	}

	if *version {
		fmt.Println(Version)
		return
	}

	if err := PowSetCacheEnabled(*powCache); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if *testnet {
		params.InitTestnet()
		if *dataDir == DefaultDataDir {
			*dataDir = TestnetDataDir
		}
	}

	settings, exists, err := loadSettings(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	switch {
	case legacySettingsFlagsUsed(flag.CommandLine):
		if settings, err = legacy.apply(settings); err == nil {
			err = settings.validate()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := saveSettings(*dataDir, settings); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save settings: %v\n", err)
		}
		fmt.Fprintln(os.Stderr, "Warning: startup flags other than --data, --testnet, --api and --pow-cache are deprecated. Their values were saved to settings.json. Use GET and PATCH /api/settings.")
	case !exists:
		ids, err := configDirWhitelist()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		settings.P2PWhitelist = ids
		if err := saveSettings(*dataDir, settings); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save settings: %v\n", err)
		}
	}

	p2pPort := MainnetP2PPort
	peerIDPort := MainnetPeerIDPort
	fallbackSeeds := DefaultSeedNodes
	seedHosts := DefaultSeedHosts
	if *testnet {
		p2pPort = TestnetP2PPort
		peerIDPort = TestnetPeerIDPort
		fallbackSeeds = DefaultTestnetSeedNodes
		seedHosts = DefaultTestnetSeedHosts
	}
	// Seed lookups wait up to 3s on the slowest host, so they run alongside
	// chain loading and are only awaited before P2P dialing, after the API is
	// already serving.
	seedsResolved := make(chan []string, 1)
	go func() {
		seedNodes := ResolveSeedNodes(seedHosts, p2pPort, peerIDPort)
		if len(seedNodes) == 0 {
			seedNodes = fallbackSeeds
		}
		seedsResolved <- seedNodes
	}()
	var resolveSeedsOnce sync.Once
	var seedNodes []string
	resolveSeeds := func() []string {
		resolveSeedsOnce.Do(func() { seedNodes = <-seedsResolved })
		return seedNodes
	}

	logSinceProcessStart("new_daemon_start")
	cli, err := NewCLI(CLIConfig{
		DataDir:          *dataDir,
		APIAddr:          *apiAddr,
		ResolveSeedNodes: resolveSeeds,
		Settings:         settings,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := cli.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

var legacySettingsFlagNames = []string{
	"wallet", "listen", "p2p-max-inbound", "p2p-max-outbound", "seed", "explorer",
	"save-checkpoints", "full-sync", "p2p-whitelist-peer", "p2p-whitelist", "nocolor", "no-version-check",
}

type legacySettingsFlags struct {
	walletFile      *string
	listen          *string
	explorer        *string
	whitelistFile   *string
	maxInbound      *int
	maxOutbound     *int
	seed            *bool
	saveCheckpoints *bool
	fullSync        *bool
	whitelistPeers  peerIDListFlag
}

func registerLegacySettingsFlags(fs *flag.FlagSet) *legacySettingsFlags {
	l := &legacySettingsFlags{}
	l.walletFile = fs.String("wallet", "", "Deprecated: use wallet_file in /api/settings")
	l.listen = fs.String("listen", "", "Deprecated: use listen in /api/settings")
	l.maxInbound = fs.Int("p2p-max-inbound", 0, "Deprecated: use p2p_max_inbound in /api/settings")
	l.maxOutbound = fs.Int("p2p-max-outbound", 0, "Deprecated: use p2p_max_outbound in /api/settings")
	l.seed = fs.Bool("seed", false, "Deprecated: use seed in /api/settings")
	l.explorer = fs.String("explorer", "", "Deprecated: use explorer_addr in /api/settings")
	l.saveCheckpoints = fs.Bool("save-checkpoints", false, "Deprecated: use save_checkpoints in /api/settings")
	l.fullSync = fs.Bool("full-sync", false, "Deprecated: use full_sync in /api/settings")
	fs.Var(&l.whitelistPeers, "p2p-whitelist-peer", "Deprecated: use p2p_whitelist in /api/settings")
	l.whitelistFile = fs.String("p2p-whitelist", "", "Deprecated: use p2p_whitelist in /api/settings")
	fs.Bool("nocolor", false, "Deprecated: does nothing")
	fs.Bool("no-version-check", false, "Deprecated: does nothing")
	return l
}

func legacySettingsFlagsUsed(fs *flag.FlagSet) bool {
	used := false
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains(legacySettingsFlagNames, f.Name) {
			used = true
		}
	})
	return used
}

func (l *legacySettingsFlags) apply(base Settings) (Settings, error) {
	s := defaultSettings()
	s.ExtraPeers = base.ExtraPeers
	s.IdentityKeyPath = base.IdentityKeyPath
	s.ChainCacheCap = base.ChainCacheCap
	if *l.walletFile != "" {
		s.WalletFile = *l.walletFile
	}
	if *l.listen != "" {
		s.Listen = *l.listen
	}
	s.P2PMaxInbound = *l.maxInbound
	s.P2PMaxOutbound = *l.maxOutbound
	s.Seed = *l.seed
	s.ExplorerAddr = *l.explorer
	s.SaveCheckpoints = *l.saveCheckpoints
	s.FullSync = s.FullSync || *l.fullSync
	s.P2PWhitelist = append([]string{}, l.whitelistPeers...)
	if *l.whitelistFile != "" {
		ids, err := loadWhitelistFile(*l.whitelistFile)
		if err != nil {
			return s, fmt.Errorf("whitelist file %s: %w", *l.whitelistFile, err)
		}
		s.P2PWhitelist = append(s.P2PWhitelist, ids...)
	} else {
		ids, err := configDirWhitelist()
		if err != nil {
			return s, err
		}
		s.P2PWhitelist = append(s.P2PWhitelist, ids...)
	}
	return s, nil
}

func configDirWhitelist() ([]string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return []string{}, nil
	}
	network := "mainnet"
	if params.IsTestnet {
		network = "testnet"
	}
	path := filepath.Join(configDir, "blocknet", network, "whitelist.json")
	if _, err := os.Stat(path); err != nil {
		return []string{}, nil
	}
	ids, err := loadWhitelistFile(path)
	if err != nil {
		return nil, fmt.Errorf("whitelist file %s: %w", path, err)
	}
	return ids, nil
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

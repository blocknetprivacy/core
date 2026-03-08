package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blocknet/p2p"
	"blocknet/protocol/params"
	"blocknet/wallet"

	"github.com/libp2p/go-libp2p/core/peer"
)

const Version = "0.8.1"

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
	recover := flag.Bool("recover", false, "Recover wallet from mnemonic seed")
	daemonMode := flag.Bool("daemon", true, "Run headless (no interactive shell)")
	cliMode := flag.Bool("cli", false, "Run with interactive shell")
	explorerAddr := flag.String("explorer", "", "HTTP address for block explorer (e.g. :8080)")
	apiAddr := flag.String("api", "", "API listen address (e.g. 127.0.0.1:8332)")
	noColor := flag.Bool("nocolor", false, "Disable colored output")
	noVersionCheck := flag.Bool("no-version-check", false, "Disable remote version check on startup")
	saveCheckpoints := flag.Bool("save-checkpoints", false, "Append a record to checkpoints.dat every 100 blocks (writes to data dir)")
	fullSync := flag.Bool("full-sync", false, "Bypass checkpoints (download + verification) and sync naturally from peers")
	outputPeerAddr := flag.Bool("output-peer-address", false, "Load identity key, resolve public IP, write peer.txt and exit")
	var p2pWhitelistPeers peerIDListFlag
	flag.Var(&p2pWhitelistPeers, "p2p-whitelist-peer", "Peer ID to exempt from peer bans (repeatable or comma-separated)")
	p2pWhitelistFile := flag.String("p2p-whitelist", "", "Path to JSON file containing peer IDs to exempt from bans")
	viewOnly := flag.Bool("viewonly", false, "Create a view-only wallet")
	spendPub := flag.String("spend-pub", "", "Spend public key (hex) for view-only wallet")
	// Deprecated: secrets on argv are visible via process inspection (ps, /proc).
	viewPrivDeprecated := flag.String("view-priv", "", "DEPRECATED (insecure): do not pass view private key via CLI; use --view-priv-env/BLOCKNET_VIEW_PRIV")
	viewPrivEnv := flag.String("view-priv-env", "BLOCKNET_VIEW_PRIV", "Environment variable name containing view private key (hex) for view-only wallet")
	flag.Parse()

	if *cliMode {
		*daemonMode = false
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

	if *outputPeerAddr {
		if err := outputPeerAddress(*dataDir, *listen); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// View-only wallet creation mode
	if *viewOnly {
		if *viewPrivDeprecated != "" {
			fmt.Fprintln(os.Stderr, "Error: --view-priv no longer accepts a private key on the command line.")
			fmt.Fprintln(os.Stderr, "Set the key in an environment variable and use --view-priv-env to pick the name (default: BLOCKNET_VIEW_PRIV).")
			fmt.Fprintln(os.Stderr, "Example: BLOCKNET_VIEW_PRIV=<hex> blocknet --viewonly --spend-pub <hex>")
			os.Exit(1)
		}

		if *spendPub == "" {
			fmt.Fprintln(os.Stderr, "Error: --viewonly requires --spend-pub")
			fmt.Fprintln(os.Stderr, "Usage: BLOCKNET_VIEW_PRIV=<hex> blocknet --viewonly --spend-pub <hex>")
			os.Exit(1)
		}

		envName := strings.TrimSpace(*viewPrivEnv)
		if envName == "" {
			fmt.Fprintln(os.Stderr, "Error: --view-priv-env must not be empty")
			os.Exit(1)
		}
		viewPrivHex := strings.TrimSpace(os.Getenv(envName))
		if viewPrivHex == "" {
			fmt.Fprintf(os.Stderr, "Error: environment variable %s is not set (expected 64 hex chars)\n", envName)
			fmt.Fprintln(os.Stderr, "Usage: BLOCKNET_VIEW_PRIV=<hex> blocknet --viewonly --spend-pub <hex>")
			os.Exit(1)
		}

		// Reduce lifetime in-process (does not remove from parent shell env).
		_ = os.Unsetenv(envName)

		if err := createViewOnlyWallet(*walletFile, *spendPub, viewPrivHex); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
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
		RecoverMode:       *recover,
		DaemonMode:        *daemonMode,
		ExplorerAddr:      *explorerAddr,
		APIAddr:           *apiAddr,
		NoColor:           *noColor,
		NoVersionCheck:    *noVersionCheck,
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

func createViewOnlyWallet(filename, spendPubHex, viewPrivHex string) error {
	// Parse hex keys
	spendPubBytes, err := hex.DecodeString(spendPubHex)
	if err != nil || len(spendPubBytes) != 32 {
		return fmt.Errorf("invalid spend public key (expected 64 hex chars)")
	}

	viewPrivBytes, err := hex.DecodeString(viewPrivHex)
	if err != nil || len(viewPrivBytes) != 32 {
		return fmt.Errorf("invalid view private key (expected 64 hex chars)")
	}

	var keys wallet.ViewOnlyKeys
	copy(keys.SpendPubKey[:], spendPubBytes)
	copy(keys.ViewPrivKey[:], viewPrivBytes)

	// Derive view public key from view private key
	kp, err := GenerateRistrettoKeypairFromSeed(keys.ViewPrivKey)
	if err != nil {
		return fmt.Errorf("failed to derive view public key: %w", err)
	}
	keys.ViewPubKey = kp.PublicKey

	// Prompt for password
	fmt.Print("Enter password for new view-only wallet: ")
	var password string
	if _, err := fmt.Scanln(&password); err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}
	if len(password) < 3 {
		return fmt.Errorf("password must be at least 3 characters")
	}

	// Create wallet config (CheckStealthOutput needs parameter reordering to match
	// WalletConfig callback signature: func(txPub, outputPub, viewPriv, spendPub) bool)
	cfg := wallet.WalletConfig{
		CheckStealthOutput: func(txPub, outputPub, viewPriv, spendPub [32]byte) bool {
			return CheckStealthOutput(spendPub, viewPriv, txPub, outputPub)
		},
		DeriveOutputSecret: DeriveStealthSecret,
		DeriveOutputSecretIndexed: func(txPub, viewPriv [32]byte, outputIndex uint32) ([32]byte, error) {
			return DeriveStealthSecretIndexed(txPub, viewPriv, outputIndex)
		},
	}

	// Create view-only wallet
	w, err := wallet.NewViewOnlyWallet(filename, []byte(password), keys, cfg)
	if err != nil {
		return fmt.Errorf("failed to create view-only wallet: %w", err)
	}

	fmt.Printf("View-only wallet created: %s\n", filename)
	fmt.Printf("Address: %s\n", w.Address())
	fmt.Println("\nThis wallet can monitor incoming funds but cannot spend.")

	return nil
}

func outputPeerAddress(dataDir, listenAddr string) error {
	keyPath := filepath.Join(dataDir, "identity.key")
	if envKey := strings.TrimSpace(os.Getenv("BLOCKNET_P2P_KEY")); envKey != "" {
		keyPath = envKey
	}
	if err := os.Setenv("BLOCKNET_P2P_KEY", keyPath); err != nil {
		return fmt.Errorf("failed to set BLOCKNET_P2P_KEY: %w", err)
	}

	mgr, err := p2p.NewIdentityManager(p2p.DefaultIdentityConfig())
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}
	_, peerID := mgr.CurrentIdentity()

	port := "28080"
	if parts := strings.Split(listenAddr, "/"); len(parts) >= 5 {
		port = parts[len(parts)-1]
	}

	publicIP, err := detectPublicIP()
	if err != nil {
		return fmt.Errorf("failed to detect public IP: %w", err)
	}

	multiaddr := fmt.Sprintf("/ip4/%s/tcp/%s/p2p/%s", publicIP, port, peerID.String())

	if err := os.WriteFile("peer.txt", []byte(multiaddr+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write peer.txt: %w", err)
	}

	fmt.Println(multiaddr)
	return nil
}

func detectPublicIP() (string, error) {
	services := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, svc := range services {
		resp, err := client.Get(svc)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("all IP detection services failed")
}

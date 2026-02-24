package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"blocknet/wallet"

	"golang.org/x/term"
)

//go:embed LICENSE
var licenseText string

// CLI handles the interactive command-line interface
type CLI struct {
	daemon          *Daemon
	wallet          *wallet.Wallet
	scanner         *wallet.Scanner
	ctx             context.Context
	cancel          context.CancelFunc
	reader          *bufio.Reader
	locked          bool
	passwordHash    [32]byte
	passwordHashSet bool
	startTime       time.Time
	daemonMode      bool
	noColor         bool
	noVersionCheck  bool
	api             *APIServer
	apiAddr         string
	dataDir         string
	walletFile      string
	mu              sync.RWMutex // protects wallet/scanner/passwordHash during hot-load
}

// CLIConfig holds CLI configuration
type CLIConfig struct {
	WalletFile      string
	DataDir         string
	ListenAddrs     []string
	SeedNodes       []string
	P2PMaxInbound   int // If >0, override default max inbound peers
	P2PMaxOutbound  int // If >0, override default max outbound peers
	Mining          bool
	MineThreads     int
	RecoverMode     bool   // If true, prompt for mnemonic to recover wallet
	DaemonMode      bool   // If true, run headless (no interactive prompts)
	ExplorerAddr    string // HTTP address for block explorer (empty = disabled)
	APIAddr         string // API listen address (empty = disabled)
	NoColor         bool   // If true, disable colored output
	NoVersionCheck  bool   // If true, skip remote version check on startup
	SaveCheckpoints bool   // If true, append checkpoints every 100 blocks
	FullSync        bool   // If true, bypass checkpoints and verify chain naturally
}

// DefaultCLIConfig returns default CLI configuration
func DefaultCLIConfig() CLIConfig {
	return CLIConfig{
		WalletFile:  DefaultWalletFilename,
		DataDir:     DefaultDataDir,
		ListenAddrs: []string{"/ip4/0.0.0.0/tcp/28080"},
		SeedNodes:   DefaultSeedNodes,
		Mining:      false,
		MineThreads: 1,
	}
}

// defaultWalletConfig returns the wallet config with crypto callbacks.
func defaultWalletConfig() wallet.WalletConfig {
	return wallet.WalletConfig{
		GenerateStealthKeys: func() (*wallet.StealthKeys, error) {
			keys, err := GenerateStealthKeys()
			if err != nil {
				return nil, err
			}
			return &wallet.StealthKeys{
				SpendPrivKey: keys.SpendPrivKey,
				SpendPubKey:  keys.SpendPubKey,
				ViewPrivKey:  keys.ViewPrivKey,
				ViewPubKey:   keys.ViewPubKey,
			}, nil
		},
		DeriveStealthAddress: func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error) {
			output, err := DeriveStealthAddress(spendPub, viewPub)
			if err != nil {
				return txPriv, txPub, oneTimePub, err
			}
			return output.TxPrivKey, output.TxPubKey, output.OnetimePubKey, nil
		},
		CheckStealthOutput: func(txPub, outputPub, viewPriv, spendPub [32]byte) bool {
			return CheckStealthOutput(spendPub, viewPriv, txPub, outputPub)
		},
		DeriveSpendKey: func(txPub, viewPriv, spendPriv [32]byte) ([32]byte, error) {
			return DeriveStealthSpendKey(txPub, viewPriv, spendPriv)
		},
		DeriveOutputSecret: func(txPub, viewPriv [32]byte) ([32]byte, error) {
			return DeriveStealthSecret(txPub, viewPriv)
		},
		GenerateKeypairFromSeed: func(seed [32]byte) (priv, pub [32]byte, err error) {
			kp, err := GenerateRistrettoKeypairFromSeed(seed)
			if err != nil {
				return priv, pub, err
			}
			return kp.PrivateKey, kp.PublicKey, nil
		},
	}
}

// defaultScannerConfig returns the scanner config with crypto callbacks.
func defaultScannerConfig() wallet.ScannerConfig {
	return wallet.ScannerConfig{
		GenerateKeyImage: GenerateKeyImage,
		CreateCommitment: func(amount uint64, blinding [32]byte) ([32]byte, error) {
			return CreatePedersenCommitmentWithBlinding(amount, blinding)
		},
	}
}

// NewCLI creates and initializes the CLI
func NewCLI(cfg CLIConfig) (*CLI, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cli := &CLI{
		ctx:            ctx,
		cancel:         cancel,
		reader:         bufio.NewReader(os.Stdin),
		startTime:      time.Now(),
		daemonMode:     cfg.DaemonMode,
		noColor:        cfg.NoColor,
		noVersionCheck: cfg.NoVersionCheck,
		dataDir:        cfg.DataDir,
		walletFile:     cfg.WalletFile,
	}

	// Create daemon config (shared by both modes)
	daemonCfg := DaemonConfig{
		DataDir:         cfg.DataDir,
		ListenAddrs:     cfg.ListenAddrs,
		SeedNodes:       cfg.SeedNodes,
		P2PMaxInbound:   cfg.P2PMaxInbound,
		P2PMaxOutbound:  cfg.P2PMaxOutbound,
		ExplorerAddr:    cfg.ExplorerAddr,
		SaveCheckpoints: cfg.SaveCheckpoints,
		FullSync:        cfg.FullSync,
	}

	// Daemon mode: start without a wallet, user loads one via API
	if cfg.DaemonMode {
		daemon, err := NewDaemon(daemonCfg, nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("daemon error: %w", err)
		}
		cli.daemon = daemon

		if cfg.APIAddr != "" {
			cli.api = NewAPIServer(daemon, nil, nil, cfg.DataDir, nil)
			cli.api.cli = cli
			cli.apiAddr = cfg.APIAddr
		}

		return cli, nil
	}

	// Interactive mode: prompt for password and load wallet

	// Backfill XDG backups for any .dat files we find but haven't backed up yet.
	wallet.BackfillWalletBackups(".", filepath.Dir(cfg.WalletFile))

	// Check if wallet exists
	walletExists := fileExists(cfg.WalletFile)

	// If wallet is missing and we have XDG backups, offer to restore one
	if !walletExists && !cfg.RecoverMode {
		if backups := wallet.ListBackups(); len(backups) > 0 {
			fmt.Printf("\n%s\n", cli.sectionHead("Wallet Recovery"))
			fmt.Println("  Your wallet file is missing, but backups were found.")
			fmt.Println("  Select an address to restore, or press Enter to create a new wallet:")
			fmt.Println()
			for i, b := range backups {
				label := b.Address
				if label == "unknown" {
					label = "unknown (created " + b.Timestamp + ")"
				} else if len(label) > 12 {
					label = label[:6] + "..." + label[len(label)-6:]
				}
				fmt.Printf("  %d. %s\n", i+1, label)
			}
			fmt.Print("\n  > ")
			line, _ := cli.reader.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				idx := 0
				if _, err := fmt.Sscanf(line, "%d", &idx); err == nil && idx >= 1 && idx <= len(backups) {
					chosen := backups[idx-1].Address
					if err := wallet.RestoreBackup(chosen, cfg.WalletFile); err != nil {
						cancel()
						return nil, fmt.Errorf("restore failed: %w", err)
					}
					fmt.Println("  Wallet restored")
					walletExists = true
				}
			}
		}
	}

	// Handle recovery mode
	var recoverMnemonic string
	if cfg.RecoverMode {
		if walletExists {
			cancel()
			return nil, fmt.Errorf("wallet already exists at %s - delete it first to recover", cfg.WalletFile)
		}

		fmt.Printf("\n%s\n", cli.sectionHead("Recovery"))
		fmt.Println("  Enter your 12-word recovery seed (words separated by spaces):")
		fmt.Print("\n> ")

		line, err := cli.reader.ReadString('\n')
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to read mnemonic: %w", err)
		}
		recoverMnemonic = strings.TrimSpace(line)

		words := strings.Fields(recoverMnemonic)
		if len(words) != 12 {
			cancel()
			return nil, fmt.Errorf("expected 12 words, got %d", len(words))
		}

		if !wallet.ValidateMnemonic(recoverMnemonic) {
			cancel()
			return nil, fmt.Errorf("invalid mnemonic (checksum failed)")
		}

		fmt.Println("\n  Mnemonic validated. Creating recovered wallet...")
	}

	// Get password
	var password []byte
	var err error

	if walletExists {
		fmt.Printf("\n  Opening wallet: %s\n", cfg.WalletFile)
		password, err = cli.promptPassword("  Password: ")
	} else {
		if cfg.RecoverMode {
			fmt.Printf("\n  Creating recovered wallet: %s\n", cfg.WalletFile)
		} else {
			fmt.Printf("\n  Creating new wallet: %s\n", cfg.WalletFile)
		}
		password, err = cli.promptNewPassword()
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("password error: %w", err)
	}
	cli.passwordHash = passwordHash(password)
	cli.passwordHashSet = true

	walletCfg := defaultWalletConfig()

	// Load, create, or recover wallet
	var w *wallet.Wallet
	if recoverMnemonic != "" {
		w, err = wallet.NewWalletFromMnemonic(cfg.WalletFile, password, recoverMnemonic, walletCfg)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("recovery failed: %w", err)
		}
		fmt.Println("  Wallet recovered. Sync the blockchain to see your balance.")
	} else {
		w, err = wallet.LoadOrCreateWallet(cfg.WalletFile, password, walletCfg)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("wallet error: %w", err)
		}
	}
	cli.wallet = w
	cli.scanner = wallet.NewScanner(w, defaultScannerConfig())

	// Create daemon with wallet's stealth keys for mining rewards
	daemon, err := NewDaemon(daemonCfg, &StealthKeys{
		SpendPrivKey: w.Keys().SpendPrivKey,
		SpendPubKey:  w.Keys().SpendPubKey,
		ViewPrivKey:  w.Keys().ViewPrivKey,
		ViewPubKey:   w.Keys().ViewPubKey,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("daemon error: %w", err)
	}
	cli.daemon = daemon

	if daemon.repairFailed {
		fmt.Printf("\n%s\n", cli.errorHead("Chain Repair Failed"))
		fmt.Printf("  Found %d integrity violation(s) but could not auto-repair\n", daemon.repairViolations)
		fmt.Println("  Your wallet is safe. Purge chain data and re-sync from peers?")
		fmt.Print("\n  Purge and resync? [y/N]: ")
		confirm, _ := cli.reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(confirm)) == "y" {
			if err := daemon.Stop(); err != nil {
				cancel()
				return nil, fmt.Errorf("failed to stop daemon: %w", err)
			}
			if err := os.RemoveAll(cfg.DataDir); err != nil {
				cancel()
				return nil, fmt.Errorf("failed to purge chain data: %w", err)
			}
			cancel()
			return nil, fmt.Errorf("chain data purged — restart to resync from genesis")
		}
	} else if daemon.repairViolations > 0 {
		fmt.Printf("\n%s\n", cli.errorHead("Chain Repair"))
		fmt.Printf("  Found %d integrity violation(s), truncated to height %d\n", daemon.repairViolations, daemon.repairTruncatedTo)
		fmt.Println("  Will re-sync the remaining blocks from peers")
	}

	// Create API server if --api is set
	if cfg.APIAddr != "" {
		cli.api = NewAPIServer(daemon, w, cli.scanner, cfg.DataDir, password)
		cli.api.cli = cli
		cli.apiAddr = cfg.APIAddr
	}

	// Best-effort: zero caller-owned password bytes. The wallet retains its own
	// copy for re-encryption on save.
	wipeBytes(password)

	return cli, nil
}

// Run starts the CLI
func (c *CLI) Run() error {
	// Handle Ctrl+C gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		if err := c.shutdown(); err != nil {
			fmt.Printf("  Warning: %v\n", err)
		}
		os.Exit(0)
	}()

	fmt.Println("  Connecting to network...")
	if err := c.daemon.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Check if wallet is ahead of chain (chain was reset/reorged)
	if c.wallet != nil {
		chainHeight := c.daemon.Chain().Height()
		walletHeight := c.wallet.SyncedHeight()
		if walletHeight > chainHeight {
			removed := c.wallet.RewindToHeight(chainHeight)
			if removed > 0 {
				fmt.Printf("  Chain reset: removed %d orphaned outputs, rewound to height %d\n", removed, chainHeight)
				if err := c.wallet.Save(); err != nil {
					fmt.Printf("  Warning: failed to persist rewound wallet: %v\n", err)
				}
			}
		}
	}

	// Start API server if configured
	if c.api != nil {
		if err := c.api.Start(c.apiAddr); err != nil {
			return fmt.Errorf("failed to start API: %w", err)
		}
		if isInsecureAPIBindAddress(c.apiAddr) {
			fmt.Printf("\n%s\n  API bind address %q is not loopback\n  Place behind trusted network boundaries or TLS\n", c.errorHead("Warning"), c.apiAddr)
		}
	}

	// Auto-scan new blocks for wallet
	go c.autoScanBlocks()

	// Watch for blocks we mined and print explorer links
	go c.watchMinedBlocks()

	// Daemon mode: just wait for shutdown signal
	if c.daemonMode {
		c.printLogo()
		height := c.daemon.Chain().Height()

		c.mu.RLock()
		w := c.wallet
		c.mu.RUnlock()

		if w != nil {
			fmt.Printf("  Address: %s\n", w.Address())
			spendable := w.SpendableBalance(height)
			pending := w.PendingBalance(height)
			balanceStr := formatAmount(spendable)
			if pending > 0 {
				balanceStr += fmt.Sprintf(" + %s pending", formatAmount(pending))
			}
			fmt.Printf("  Balance: %s\n", balanceStr)
			fmt.Printf("  Height:  %d\n", height)
		} else {
			fmt.Printf("  Peer ID: %s\n", c.daemon.Node().PeerID())
			fmt.Printf("  Height:  %d\n", height)
			fmt.Println("  Wallet:  not loaded (use API /api/wallet/load)")
		}
		fmt.Println()
		fmt.Printf("  %s\n", c.sectionHead("Daemon mode (Ctrl+C to stop)"))

		// Block until shutdown
		<-c.ctx.Done()
		return c.shutdown()
	}

	// Print welcome
	c.printWelcome()

	if !c.noVersionCheck {
		go func() {
			if latest, err := fetchLatestVersion(); err == nil && isNewerVersion(latest, Version) {
				c.printUpdateNotice(latest)
			}
		}()
		go c.periodicVersionCheck()
	}

	// Main command loop
	for {
		select {
		case <-c.ctx.Done():
			return c.shutdown()
		default:
		}

		// Print prompt
		fmt.Print("\n> ")

		// Read command
		line, err := c.reader.ReadString('\n')
		if err != nil {
			// EOF means stdin closed - wait for shutdown
			<-c.ctx.Done()
			return c.shutdown()
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse and execute
		if err := c.executeCommand(line); err != nil {
			if err.Error() == "quit" {
				return c.shutdown()
			}
			fmt.Printf("\n%s\n  %v\n", c.errorHead("Error"), err)
		}
	}
}

func (c *CLI) printLogo() {
	green := "\033[38;5;118m"
	reset := "\033[0m"
	if c.noColor {
		green = ""
		reset = ""
	}

	logo := []string{
		`        ▄███████▄`,
		`        ▀█████████▄`,
		`          ▀█████████▄`,
		`            ▀█████████▄`,
		`              ▀█████████▄`,
		`                ▀█████████▄`,
		`                  ▀███████▀`,
		``,
		`  ▄████████████████████████████▄`,
		fmt.Sprintf(`  ████    BLOCKNET v%s   ████`, Version),
		`  ▀████████████████████████████▀`,
	}

	fmt.Println()
	for _, line := range logo {
		fmt.Printf("%s%s%s\n", green, line, reset)
		time.Sleep(40 * time.Millisecond)
	}
	fmt.Println()
}

func (c *CLI) printWelcome() {
	height := c.daemon.Chain().Height()
	spendable := c.wallet.SpendableBalance(height)
	pending := c.wallet.PendingBalance(height)

	balanceStr := formatAmount(spendable)
	if pending > 0 {
		balanceStr += fmt.Sprintf(" + %s pending", formatAmount(pending))
	}

	c.printLogo()
	fmt.Printf("  Address: %s\n", c.wallet.Address())
	fmt.Printf("  Balance: %s\n", balanceStr)
	fmt.Printf("  Height:  %d\n", height)
	fmt.Println()
	fmt.Println("  Type 'help' for available commands")
}

func fetchLatestVersion() (string, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("https://raw.githubusercontent.com/blocknetprivacy/blocknet/refs/heads/master/main.go")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Extract: const Version = "x.y.z"
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "const Version") {
			parts := strings.SplitN(line, `"`, 3)
			if len(parts) == 3 {
				return parts[1], nil
			}
		}
	}
	return "", fmt.Errorf("version not found in remote main.go")
}

func parseVersionParts(v string) [3]int {
	var parts [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		parts[i], _ = strconv.Atoi(s)
	}
	return parts
}

func isNewerVersion(latest, current string) bool {
	l := parseVersionParts(latest)
	c := parseVersionParts(current)
	for i := range l {
		if l[i] > c[i] {
			return true
		}
		if l[i] < c[i] {
			return false
		}
	}
	return false
}

func (c *CLI) periodicVersionCheck() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if latest, err := fetchLatestVersion(); err == nil && isNewerVersion(latest, Version) {
				c.printUpdateNotice(latest)
			}
		}
	}
}

func (c *CLI) printUpdateNotice(latest string) {
	amber := "\033[38;2;255;170;0m"
	rst := "\033[0m"
	if c.noColor {
		amber = ""
		rst = ""
	}
	fmt.Printf("\n%s# Update available%s\n", amber, rst)
	fmt.Printf("  %sv%s -> v%s%s\n", amber, Version, latest, rst)
	fmt.Printf("  %shttps://github.com/blocknetprivacy/blocknet/releases/latest%s\n", amber, rst)
}

func (c *CLI) executeCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}

	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	// Check if locked
	if c.locked && cmd != "unlock" && cmd != "quit" && cmd != "exit" {
		return fmt.Errorf("wallet is locked, use 'unlock' first")
	}

	switch cmd {
	case "help", "?":
		c.cmdHelp()
	case "version":
		c.cmdVersion()
	case "status":
		c.cmdStatus()
	case "balance", "bal", "b":
		c.cmdBalance()
	case "address", "addr", "a":
		c.cmdAddress()
	case "send":
		return c.cmdSend(args)
	case "sign":
		return c.cmdSign()
	case "verify":
		return c.cmdVerifyMsg()
	case "history", "hist", "h":
		c.cmdHistory()
	case "peers":
		c.cmdPeers()
	case "banned":
		c.cmdBanned()
	case "export-peer":
		return c.cmdExportPeer()
	case "mining":
		return c.cmdMining(args)
	case "sync", "scan":
		c.cmdSync()
	case "seed":
		return c.cmdSeed()
	case "import":
		return c.cmdImport()
	case "viewkeys":
		return c.cmdViewKeys()
	case "lock":
		c.cmdLock()
	case "unlock":
		return c.cmdUnlock()
	case "save":
		return c.cmdSave()
	case "purge":
		return c.cmdPurgeData()
	case "certify":
		c.cmdCertify()
	case "license":
		c.cmdLicense()
	case "about":
		c.cmdAbout()
	case "quit", "exit", "q":
		return fmt.Errorf("quit")
	default:
		return fmt.Errorf("unknown command: %s (type 'help' for commands)", cmd)
	}

	return nil
}

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

func (c *CLI) cmdHelp() {
	viewOnlyNote := ""
	if c.wallet.IsViewOnly() {
		viewOnlyNote = " [VIEW-ONLY]"
	}

	sendNote := ""
	if c.wallet.IsViewOnly() {
		sendNote = " (disabled)"
	}

	fmt.Printf(`
%s%s
  balance           Show wallet balance
  address           Show receiving address
  send <addr> <amt> [memo|hex:<memo_hex>] Send funds with optional memo%s
  sign              Sign a message with your spend key
  verify            Verify a signed message against an address
  history           Show transaction history
  seed              Show wallet recovery seed (careful!)
  import            Create wallet file from seed or spend/view keys
  viewkeys          Create a view-only wallet file
  lock              Lock wallet
  unlock            Unlock wallet
  save              Save wallet to disk
  sync              Rescan blocks for outputs

%s
  status            Show node and wallet status
  peers             List connected peers
  banned            List banned peers
  export-peer       Export peer address to peer.txt
  mining start|stop|threads Control mining
  certify           Check chain integrity (difficulty + timestamps)
  purge             Delete all blockchain data (cannot be undone)
  version           Print version
  about             About this software
  license           Show license
  quit              Exit (saves automatically)
`, c.sectionHead("Wallet"), viewOnlyNote, sendNote, c.sectionHead("Daemon"))
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

func (c *CLI) cmdBalance() {
	height := c.daemon.Chain().Height()
	spendable := c.wallet.SpendableBalance(height)
	pending := c.wallet.PendingBalance(height)
	pendingUnconfirmed := c.wallet.PendingUnconfirmedBalance()
	total, unspent := c.wallet.OutputCount()

	fmt.Printf("\n%s\n", c.sectionHead("Balance"))
	fmt.Printf("  spendable:  %s\n", formatAmount(spendable))
	fmt.Printf("  confirming: %s\n", formatAmount(pending))
	if pendingUnconfirmed > 0 {
		eta := time.Duration(wallet.SafeConfirmations+1) * wallet.EstimatedBlockInterval
		fmt.Printf("  pending:    %s (est unlock ~%s)\n", formatAmount(pendingUnconfirmed), eta.Round(time.Minute))
	}
	fmt.Printf("  total:      %s\n", formatAmount(spendable+pending))
	fmt.Printf("  outputs:    %d unspent", unspent)
	if total > unspent {
		fmt.Printf(", %d spent", total-unspent)
	}
	fmt.Println()
}

func (c *CLI) cmdAddress() {
	fmt.Printf("\n%s\n", c.sectionHead("Address"))
	fmt.Printf("  %s\n", c.wallet.Address())
}

func (c *CLI) cmdSend(args []string) error {
	// View-only wallets cannot send
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("cannot send from a view-only wallet")
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: send <address> <amount> [memo|hex:<memo_hex>]")
	}

	// Parse optional memo. Plain text is UTF-8 bytes. Hex can be passed as hex:<hex>.
	var memo []byte
	if len(args) >= 3 {
		// strings.Fields() doesn't preserve quoting, so join the remainder back
		// together to support memos with spaces.
		raw := strings.TrimSpace(strings.Join(args[2:], " "))
		if strings.HasPrefix(raw, "hex:") {
			decoded, err := hex.DecodeString(strings.TrimPrefix(raw, "hex:"))
			if err != nil {
				return fmt.Errorf("invalid memo hex")
			}
			memo = decoded
		} else {
			// Best-effort strip of single/double quotes for UX when user types:
			// send <addr> <amt> "memo with spaces"
			if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
				raw = raw[1 : len(raw)-1]
			}
			memo = []byte(raw)
		}
		if len(memo) > wallet.MemoSize-4 {
			return fmt.Errorf("memo too long: max %d bytes", wallet.MemoSize-4)
		}
	}

	// Parse recipient address (strip control characters from copy-paste)
	recipientInput := sanitizeInput(args[0])
	recipientAddr, resolvedInfo, err := resolveRecipientAddress(recipientInput)
	if err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}
	if resolvedInfo != nil && resolvedInfo.Verified {
		if c.noColor {
			fmt.Printf("  Resolved %s -> %s [verified]\n", recipientInput, recipientAddr)
		} else {
			fmt.Printf("  Resolved %s -> %s \033[38;2;170;255;0m✓ verified\033[0m\n", recipientInput, recipientAddr)
		}
	}
	spendPub, viewPub, err := wallet.ParseAddress(recipientAddr)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	// Parse amount
	amount, err := parseAmount(args[1])
	if err != nil {
		return fmt.Errorf("invalid amount: %w", err)
	}

	// Check spendable balance (excludes immature coinbase and unconfirmed)
	height := c.daemon.Chain().Height()
	spendable := c.wallet.SpendableBalance(height)
	if spendable < amount {
		pending := c.wallet.PendingBalance(height)
		if pending > 0 {
			return fmt.Errorf("insufficient spendable balance: have %s spendable + %s pending, need %s",
				formatAmount(spendable), formatAmount(pending), formatAmount(amount))
		}
		return fmt.Errorf("insufficient balance: have %s, need %s",
			formatAmount(spendable), formatAmount(amount))
	}

	fmt.Printf("\n%s\n", c.sectionHead("Send"))
	fmt.Println("  Building transaction...")

	recipient := wallet.Recipient{
		SpendPubKey: spendPub,
		ViewPubKey:  viewPub,
		Amount:      amount,
		Memo:        memo,
	}

	builder := c.createTxBuilder()
	chainHeight := c.daemon.Chain().Height()
	result, err := builder.Transfer([]wallet.Recipient{recipient}, 10, chainHeight)
	if err != nil {
		return fmt.Errorf("failed to build transaction: %w", err)
	}

	// Confirm
	recipientLabel := recipientAddr
	if resolvedInfo != nil {
		recipientLabel = recipientInput
	}
	fmt.Printf("\n  Send %s to %s?\n", formatAmount(amount), recipientLabel)
	fmt.Printf("  Fee:     %s\n", formatAmount(result.Fee))
	if resolvedInfo != nil {
		fmt.Printf("  Address: %s\n", recipientAddr)
	}
	if result.Change > 0 {
		blocksUntilSpendable := uint64(wallet.SafeConfirmations + 1)
		arrivalBlock := chainHeight + blocksUntilSpendable
		eta := time.Duration(blocksUntilSpendable) * wallet.EstimatedBlockInterval
		fmt.Printf("  Change:  %s — spendable in ~%d blocks (block %d, ~%s from now)\n",
			formatAmount(result.Change), blocksUntilSpendable, arrivalBlock, formatDuration(eta))
	}
	if len(memo) > 0 {
		if memoText, ok := memoTextIfPrintable(memo); ok {
			fmt.Printf("  Memo:    %s\n", strconv.QuoteToASCII(memoText))
		} else {
			fmt.Printf("  Memo:    %s\n", strings.ToUpper(hex.EncodeToString(memo)))
		}
	}
	fmt.Print("  Confirm [y/N]: ")

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		c.wallet.ReleaseInputLease(result.InputLease)
		fmt.Println("  Cancelled")
		return nil
	}

	// Submit to mempool via Dandelion++
	if err := c.daemon.SubmitTransaction(result.TxData); err != nil {
		c.wallet.ReleaseInputLease(result.InputLease)
		return fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Mark outputs as spent
	for _, spent := range result.SpentOutputs {
		c.wallet.MarkSpent(spent.OneTimePubKey, c.daemon.Chain().Height())
	}

	// Record send for history tracking
	c.wallet.RecordSend(&wallet.SendRecord{
		TxID:        result.TxID,
		Timestamp:   time.Now().Unix(),
		Recipient:   recipientLabel,
		Amount:      amount,
		Fee:         result.Fee,
		BlockHeight: c.daemon.Chain().Height(),
		Memo:        memo,
	})
	if result.Change > 0 {
		// UX: surface expected change immediately until it is confirmed/scanned.
		c.wallet.AddPendingCredit(result.TxID, result.Change)
	}
	if err := c.wallet.Save(); err != nil {
		fmt.Printf("  Warning: wallet persistence failed: %v\n", err)
	}

	txHex := fmt.Sprintf("%x", result.TxID)
	fmt.Printf("  Sent: %s\n", txHex)
	fmt.Printf("  Explorer: https://explorer.blocknetcrypto.com/tx/%s\n", txHex)

	return nil
}

func (c *CLI) cmdSign() error {
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("view-only wallet cannot sign")
	}

	fmt.Printf("\n%s\n", c.sectionHead("Sign"))
	fmt.Println("  Enter the text to sign, press ENTER when you're done.")
	fmt.Print("\n> ")

	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	message := strings.TrimSpace(line)
	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}
	if len(message) > 1024 {
		return fmt.Errorf("message must be <= 1024 bytes")
	}

	keys := c.wallet.Keys()
	sig, err := SchnorrSign(keys.SpendPrivKey[:], []byte(message))
	if err != nil {
		return fmt.Errorf("signing failed: %w", err)
	}

	fmt.Printf("\n  %s\n", hex.EncodeToString(sig))
	return nil
}

func (c *CLI) cmdVerifyMsg() error {
	fmt.Printf("\n%s\n", c.sectionHead("Verify"))
	fmt.Println("  Enter the address:")
	fmt.Print("\n> ")
	addrLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	address := strings.TrimSpace(addrLine)
	if address == "" {
		return fmt.Errorf("address cannot be empty")
	}

	fmt.Println("  Enter the message that was signed:")
	fmt.Print("\n> ")
	msgLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	message := strings.TrimSpace(msgLine)
	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}

	fmt.Println("  Enter the signature (hex):")
	fmt.Print("\n> ")
	sigLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	sigHex := strings.TrimSpace(sigLine)

	spendPub, _, err := wallet.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != 64 {
		return fmt.Errorf("invalid signature: must be 64 bytes hex-encoded")
	}

	if err := SchnorrVerify(spendPub[:], []byte(message), sigBytes); err != nil {
		fmt.Printf("\n  %s\n", c.errorHead("Signature is INVALID"))
		return nil
	}

	fmt.Printf("\n  %s\n", c.sectionHead("Signature is VALID"))
	return nil
}

func (c *CLI) cmdHistory() {
	// Get all outputs (both spent and unspent)
	outputs := c.wallet.AllOutputs()
	sendRecords := c.wallet.SendRecords()

	if len(outputs) == 0 && len(sendRecords) == 0 {
		fmt.Printf("\n%s\n", c.sectionHead("History"))
		fmt.Println("  No transactions yet")
		return
	}

	// Color codes
	green := "\033[38;2;170;255;0m" // #AAFF00 - incoming
	red := "\033[38;2;255;68;68m"   // #FF4444 - outgoing
	dim := "\033[2;33m"             // dim yellow - pending
	reset := "\033[0m"

	if c.noColor {
		green = ""
		red = ""
		dim = ""
		reset = ""
	}

	// Build history events (both IN and OUT)
	type historyEvent struct {
		timestamp int64
		direction string
		amount    uint64
		height    uint64
		color     string
		txHash    [32]byte
		memo      []byte
		pending   bool
	}

	var events []historyEvent

	// Add incoming events
	for _, out := range outputs {
		block := c.daemon.Chain().GetBlockByHeight(out.BlockHeight)
		if block == nil {
			continue
		}
		events = append(events, historyEvent{
			timestamp: block.Header.Timestamp,
			direction: "IN",
			amount:    out.Amount,
			height:    out.BlockHeight,
			color:     green,
			txHash:    out.TxID,
			memo:      out.Memo,
		})
	}

	// Add outgoing events from the wallet's persisted send history.
	// This is the authoritative source for outbound txids; owned outputs only
	// contain the txid that created them (not the txid that spent them).
	seenTxIDs := make(map[[32]byte]bool)
	for _, sendRecord := range sendRecords {
		if sendRecord == nil || seenTxIDs[sendRecord.TxID] {
			continue
		}

		pending := sendRecord.BlockHeight == 0

		// Prefer chain timestamp when the block is available, otherwise fall back
		// to the local timestamp captured at send time.
		ts := sendRecord.Timestamp
		if sendRecord.BlockHeight > 0 {
			if block := c.daemon.Chain().GetBlockByHeight(sendRecord.BlockHeight); block != nil {
				ts = block.Header.Timestamp
			}
		}

		color := red
		if pending {
			color = dim
		}

		events = append(events, historyEvent{
			timestamp: ts,
			direction: "OUT",
			amount:    sendRecord.Amount,
			height:    sendRecord.BlockHeight,
			color:     color,
			txHash:    sendRecord.TxID,
			memo:      sendRecord.Memo,
			pending:   pending,
		})
		seenTxIDs[sendRecord.TxID] = true
	}

	// Sort by timestamp (oldest first)
	sort.Slice(events, func(i, j int) bool {
		return events[i].timestamp < events[j].timestamp
	})

	fmt.Printf("\n%s\n", c.sectionHead("History"))
	for _, evt := range events {
		tm := time.Unix(evt.timestamp, 0)
		dateStr := tm.Format("060102-15:04:05")

		amountStr := formatAmount(evt.amount)
		if evt.amount == 0 && evt.direction == "OUT" {
			amountStr = "??? BNT"
		}

		pendingTag := ""
		if evt.pending {
			pendingTag = " [mempool]"
		}

		memoStr := ""
		if len(evt.memo) > 0 {
			memoHex := hex.EncodeToString(evt.memo)
			if memoText, ok := memoTextIfPrintable(evt.memo); ok {
				memoStr = "\n    memo: " + strconv.QuoteToASCII(memoText)
			} else {
				memoStr = "\n    memo: " + memoHex
			}
		}

		fmt.Printf("  %s %s%-3s%s %-16s %x%s%s\n",
			dateStr,
			evt.color,
			evt.direction,
			reset,
			amountStr,
			evt.txHash,
			pendingTag,
			memoStr,
		)
	}
}

func memoTextIfPrintable(b []byte) (string, bool) {
	// Trim trailing NUL padding (common for fixed-size memo fields).
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	b = b[:end]
	if len(b) == 0 {
		return "", false
	}

	if !utf8.Valid(b) {
		return "", false
	}

	s := string(b)
	for _, r := range s {
		// Avoid breaking the one-line history output.
		if r == '\n' || r == '\r' {
			return "", false
		}
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return s, true
}

func (c *CLI) cmdPeers() {
	peers := c.daemon.Node().Peers()
	banned := c.daemon.Node().BannedCount()

	fmt.Printf("\n%s", c.sectionHead("Peers"))
	if len(peers) == 0 {
		fmt.Println(" (0)")
		fmt.Println("  None connected")
	} else {
		fmt.Printf(" (%d)\n", len(peers))
		for _, p := range peers {
			fmt.Printf("  %s\n", p.String())
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
		fmt.Printf("  %s\n    reason: %s\n    count:  %dx, %s\n",
			ban.PeerID.String(),
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

func (c *CLI) cmdSync() {
	chainHeight := c.daemon.Chain().Height()
	walletHeight := c.wallet.SyncedHeight()

	fmt.Printf("\n%s\n", c.sectionHead("Sync"))
	fmt.Printf("  Known blocks:   %d\n", chainHeight)
	fmt.Printf("  Blocks scanned: %d\n", walletHeight)

	if walletHeight >= chainHeight {
		return
	}

	fmt.Printf("  Scanning %d blocks...\n", chainHeight-walletHeight)

	// Snapshot blocks under a single chain read lock so we don't pay RWMutex
	// writer-preference overhead per height while the node is ingesting blocks.
	blocks := c.daemon.Chain().GetBlocksByHeightRange(walletHeight+1, chainHeight)

	scannedTo := walletHeight
	for _, block := range blocks {
		if block == nil {
			break
		}

		blockData := blockToScanData(block)
		found, spent := c.scanner.ScanBlock(blockData)

		h := block.Header.Height
		scannedTo = h
		if found > 0 || spent > 0 {
			fmt.Printf("    Block %d: +%d outputs, %d spent\n", h, found, spent)
		}
	}

	if scannedTo > walletHeight {
		c.wallet.SetSyncedHeight(scannedTo)
		fmt.Printf("  Wallet synced to height %d\n", scannedTo)
	}
}

func (c *CLI) cmdSeed() error {
	amber := "\033[38;2;255;170;0m"
	rst := "\033[0m"
	if c.noColor {
		amber = ""
		rst = ""
	}
	fmt.Printf("\n%s%s%s\n", amber, "# Seed", rst)
	fmt.Printf("  %sWARNING: Your recovery seed controls all funds.%s\n", amber, rst)
	fmt.Printf("  %sAnyone with this seed can steal your coins.%s\n", amber, rst)
	fmt.Printf("  %sNever share it. Never enter it online.%s\n", amber, rst)
	fmt.Print("\n  Show recovery seed? [y/N]: ")

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		return nil
	}

	mnemonic, err := c.wallet.Mnemonic()
	if err != nil {
		return fmt.Errorf("failed to read mnemonic: %w", err)
	}
	if mnemonic == "" {
		fmt.Println("  No recovery seed available (wallet may predate BIP39 support)")
		return nil
	}

	fmt.Println()
	words := strings.Fields(mnemonic)
	for i := 0; i < len(words); i += 4 {
		end := i + 4
		if end > len(words) {
			end = len(words)
		}
		row := ""
		for j := i; j < end; j++ {
			row += fmt.Sprintf("%2d.%-10s ", j+1, words[j])
		}
		fmt.Printf("  %s\n", strings.TrimRight(row, " "))
	}

	fmt.Println()
	fmt.Println("  Write these words down and store them safely.")
	fmt.Println("  Recover with: blocknet --recover")

	return nil
}

func (c *CLI) cmdImport() error {
	fmt.Printf("\n%s\n", c.sectionHead("Import"))
	fmt.Println("  1) 12-word recovery seed")
	fmt.Println("  2) spend-key/view-key (hex private keys)")
	fmt.Print("\n  Choose [1/2]: ")

	choiceLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read import type: %w", err)
	}
	choice := strings.ToLower(strings.TrimSpace(choiceLine))

	switch choice {
	case "1", "seed", "mnemonic", "12":
		return c.cmdImportFromMnemonic()
	case "2", "keys", "key", "spend", "view":
		return c.cmdImportFromKeys()
	default:
		return fmt.Errorf("invalid import type: choose 1 or 2")
	}
}

func (c *CLI) cmdImportFromMnemonic() error {
	fmt.Println("  Input the 12 words of your seed:")
	fmt.Print("\n> ")

	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read mnemonic: %w", err)
	}
	mnemonic := normalizeMnemonicInput(line)
	if !wallet.ValidateMnemonic(mnemonic) {
		return fmt.Errorf("invalid mnemonic phrase (expected 12 valid BIP39 words)")
	}

	base, walletPath, err := c.importWalletTargetPath()
	if err != nil {
		return err
	}

	password := c.wallet.EncryptionPasswordClone()
	defer wipeBytes(password)
	if len(password) == 0 {
		return fmt.Errorf("cannot import wallet: active wallet password unavailable")
	}

	if _, err := wallet.NewWalletFromMnemonic(walletPath, password, mnemonic, defaultWalletConfig()); err != nil {
		return fmt.Errorf("failed to create imported wallet: %w", err)
	}

	return printImportedWalletPath(base, walletPath)
}

func (c *CLI) cmdImportFromKeys() error {
	spendPriv, err := c.promptHex32("Input spend private key (64 hex chars): ")
	if err != nil {
		return fmt.Errorf("invalid spend private key: %w", err)
	}
	viewPriv, err := c.promptHex32("Input view private key (64 hex chars): ")
	if err != nil {
		return fmt.Errorf("invalid view private key: %w", err)
	}

	spendPub, err := ScalarToPubKey(spendPriv)
	if err != nil {
		return fmt.Errorf("failed to derive spend public key: %w", err)
	}
	viewPub, err := ScalarToPubKey(viewPriv)
	if err != nil {
		return fmt.Errorf("failed to derive view public key: %w", err)
	}

	base, walletPath, err := c.importWalletTargetPath()
	if err != nil {
		return err
	}

	password := c.wallet.EncryptionPasswordClone()
	defer wipeBytes(password)
	if len(password) == 0 {
		return fmt.Errorf("cannot import wallet: active wallet password unavailable")
	}

	keys := wallet.StealthKeys{
		SpendPrivKey: spendPriv,
		SpendPubKey:  spendPub,
		ViewPrivKey:  viewPriv,
		ViewPubKey:   viewPub,
	}
	if _, err := wallet.NewWalletFromStealthKeys(walletPath, password, keys, defaultWalletConfig()); err != nil {
		return fmt.Errorf("failed to create imported wallet: %w", err)
	}

	return printImportedWalletPath(base, walletPath)
}

func (c *CLI) importWalletTargetPath() (base string, walletPath string, err error) {
	fmt.Println("  Input the name of this wallet:")
	fmt.Print("\n> ")
	nameLine, err := c.reader.ReadString('\n')
	if err != nil {
		return "", "", fmt.Errorf("failed to read wallet name: %w", err)
	}
	base = filepath.Base(strings.TrimSpace(nameLine))
	if base == "" || base == "." || base == "/" {
		return "", "", fmt.Errorf("invalid wallet name")
	}
	if !strings.HasSuffix(base, ".wallet.md") {
		base += ".wallet.md"
	}

	walletPath = filepath.Join(filepath.Dir(c.walletFile), base)
	if _, err := os.Stat(walletPath); err == nil {
		return "", "", fmt.Errorf("wallet file already exists: %s", base)
	}
	return base, walletPath, nil
}

func (c *CLI) promptHex32(prompt string) ([32]byte, error) {
	var out [32]byte
	fmt.Print("  " + prompt)
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return out, err
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(line))
	if err != nil {
		return out, err
	}
	if len(decoded) != 32 {
		return out, fmt.Errorf("expected 64 hex chars")
	}
	copy(out[:], decoded)
	return out, nil
}

func printImportedWalletPath(base, walletPath string) error {
	resolvedPath, err := filepath.Abs(walletPath)
	if err != nil {
		resolvedPath = walletPath
	}

	fmt.Printf("\n  name: %s\n", base)
	fmt.Printf("  path: %s\n", resolvedPath)
	return nil
}

func normalizeMnemonicInput(input string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(input), ",", " ")
	return strings.Join(strings.Fields(normalized), " ")
}

func (c *CLI) viewWalletFilename() string {
	name := c.walletFile
	if strings.HasSuffix(name, ".wallet.dat") {
		return strings.TrimSuffix(name, ".wallet.dat") + ".view.wallet.dat"
	}
	ext := ""
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		ext = name[dot:]
		name = name[:dot]
	}
	return name + ".view" + ext
}

func (c *CLI) cmdViewKeys() error {
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("this is already a view-only wallet")
	}

	viewFile := c.viewWalletFilename()

	fmt.Printf("\n%s\n", c.sectionHead("View Keys"))
	fmt.Println("  This will create a view-only wallet that can monitor incoming")
	fmt.Println("  funds but CANNOT spend. Anyone with this file can see your")
	fmt.Println("  balance and transaction history.")
	fmt.Printf("\n  File: %s\n", viewFile)

	if fileExists(viewFile) {
		fmt.Print("\n  File already exists. Overwrite? [y/N]: ")
	} else {
		fmt.Print("\n  Create view-only wallet? [y/N]: ")
	}

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		return nil
	}

	password, err := c.promptNewPassword()
	if err != nil {
		return err
	}
	defer wipeBytes(password)

	keys := c.wallet.ExportViewOnlyKeys()
	_, err = wallet.NewViewOnlyWallet(viewFile, password, keys, defaultWalletConfig())
	if err != nil {
		return fmt.Errorf("failed to create view-only wallet: %w", err)
	}

	fmt.Printf("\n  View-only wallet saved to %s\n", viewFile)
	fmt.Println("  Load it with:")
	fmt.Printf("    blocknet --wallet %s\n", viewFile)

	return nil
}

func (c *CLI) cmdLock() {
	c.locked = true
	fmt.Printf("\n%s\n", c.sectionHead("Locked"))
}

func (c *CLI) cmdUnlock() error {
	if !c.locked {
		fmt.Printf("\n%s\n", c.sectionHead("Unlocked"))
		fmt.Println("  Already unlocked")
		return nil
	}

	password, err := c.promptPassword("Password: ")
	if err != nil {
		return err
	}

	// Verify password matches
	if !c.passwordHashSet {
		wipeBytes(password)
		return fmt.Errorf("unlock unavailable: password state not initialized")
	}
	hash := passwordHash(password)
	wipeBytes(password)
	if subtle.ConstantTimeCompare(hash[:], c.passwordHash[:]) != 1 {
		return fmt.Errorf("incorrect password")
	}

	c.locked = false
	fmt.Printf("\n%s\n", c.sectionHead("Unlocked"))
	return nil
}

func (c *CLI) cmdSave() error {
	if err := c.wallet.Save(); err != nil {
		return fmt.Errorf("failed to save wallet: %w", err)
	}
	fmt.Printf("\n%s\n", c.sectionHead("Saved"))
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

func (c *CLI) shutdown() error {
	// Stop API server first (removes cookie file)
	if c.api != nil {
		c.api.Stop()
	}

	fmt.Printf("\n%s\n", c.sectionHead("Shutdown"))
	if c.wallet != nil {
		fmt.Println("  Saving wallet...")
		if err := c.wallet.Save(); err != nil {
			fmt.Printf("  Warning: failed to save wallet: %v\n", err)
		}
	}

	fmt.Println("  Stopping daemon...")
	if err := c.daemon.Stop(); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	fmt.Println("  Done")
	return nil
}

// Password prompting with hidden input
func (c *CLI) promptPassword(prompt string) ([]byte, error) {
	fmt.Print(prompt)

	// Check if we're in a terminal
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		password, err := term.ReadPassword(fd)
		fmt.Println() // newline after hidden input
		return password, err
	}

	// Fallback for non-terminal (testing)
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(line)), nil
}

func (c *CLI) promptNewPassword() ([]byte, error) {
	password, err := c.promptPassword("  Enter new password: ")
	if err != nil {
		return nil, err
	}

	if len(password) < 3 {
		wipeBytes(password)
		return nil, fmt.Errorf("password must be at least 3 characters")
	}

	confirm, err := c.promptPassword("  Confirm password: ")
	if err != nil {
		wipeBytes(password)
		return nil, err
	}

	ph := passwordHash(password)
	ch := passwordHash(confirm)
	wipeBytes(confirm)
	if subtle.ConstantTimeCompare(ph[:], ch[:]) != 1 {
		wipeBytes(password)
		return nil, fmt.Errorf("passwords do not match")
	}

	return password, nil
}

func (c *CLI) sectionHead(label string) string {
	if c.noColor {
		return "# " + label
	}
	return "\033[38;2;170;255;0m#\033[0m " + label
}

func (c *CLI) errorHead(label string) string {
	if c.noColor {
		return "# " + label
	}
	return "\033[38;2;255;0;170m#\033[0m " + label
}

// Helpers
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func formatAmount(atomicUnits uint64) string {
	// 1 BNT = 100,000,000 atomic units (8 decimals)
	whole := atomicUnits / 100_000_000
	frac := atomicUnits % 100_000_000
	if frac == 0 {
		return fmt.Sprintf("%d BNT", whole)
	}
	// Trim trailing zeros
	fracStr := fmt.Sprintf("%08d", frac)
	fracStr = strings.TrimRight(fracStr, "0")
	return fmt.Sprintf("%d.%s BNT", whole, fracStr)
}

func parseAmount(s string) (uint64, error) {
	// Remove "BNT" suffix if present
	s = strings.TrimSuffix(strings.TrimSpace(s), "BNT")
	s = strings.TrimSpace(s)

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("invalid amount format")
	}

	// Parse whole part
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}

	const atomicPerBNT uint64 = 100_000_000
	if whole > (^uint64(0))/atomicPerBNT {
		return 0, fmt.Errorf("amount too large")
	}
	result := whole * atomicPerBNT

	// Parse fractional part if present
	if len(parts) == 2 {
		fracStr := parts[1]
		// Pad or truncate to 8 digits
		if len(fracStr) > 8 {
			fracStr = fracStr[:8]
		} else {
			fracStr = fracStr + strings.Repeat("0", 8-len(fracStr))
		}
		frac, err := strconv.ParseUint(fracStr, 10, 64)
		if err != nil {
			return 0, err
		}
		if result > (^uint64(0))-frac {
			return 0, fmt.Errorf("amount too large")
		}
		result += frac
	}

	return result, nil
}

// createTxBuilder creates a transaction builder with daemon integration
func (c *CLI) createTxBuilder() *wallet.Builder {
	cfg := wallet.TransferConfig{
		SelectRingMembers: func(realPubKey, realCommitment [32]byte) (keys, commitments [][32]byte, secretIndex int, err error) {
			ringData, err := c.daemon.Chain().SelectRingMembersWithCommitments(realPubKey, realCommitment)
			if err != nil {
				return nil, nil, 0, err
			}
			return ringData.Keys, ringData.Commitments, ringData.SecretIndex, nil
		},
		CreateCommitment: func(amount uint64, blinding [32]byte) [32]byte {
			commitment, _ := CreatePedersenCommitmentWithBlinding(amount, blinding)
			return commitment
		},
		CreateRangeProof: func(amount uint64, blinding [32]byte) ([]byte, error) {
			proof, err := CreateRangeProof(amount, blinding)
			if err != nil {
				return nil, err
			}
			return proof.Proof, nil
		},
		SignRingCT: func(ringKeys, ringCommitments [][32]byte, secretIndex int, privateKey, realBlinding, pseudoCommitment, pseudoBlinding [32]byte, message []byte) ([]byte, [32]byte, error) {
			sig, err := SignRingCT(ringKeys, ringCommitments, secretIndex, privateKey, realBlinding, pseudoCommitment, pseudoBlinding, message)
			if err != nil {
				return nil, [32]byte{}, err
			}
			return sig.Signature, sig.KeyImage, nil
		},
		GenerateBlinding: func() [32]byte {
			blinding, _ := GenerateBlinding()
			return blinding
		},
		ComputeTxID: func(txData []byte) ([32]byte, error) {
			tx, err := DeserializeTx(txData)
			if err != nil {
				return [32]byte{}, err
			}
			return tx.TxID()
		},
		DeriveStealthAddress: func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error) {
			output, err := DeriveStealthAddress(spendPub, viewPub)
			if err != nil {
				return txPriv, txPub, oneTimePub, err
			}
			return output.TxPrivKey, output.TxPubKey, output.OnetimePubKey, nil
		},
		DeriveSharedSecret: DeriveStealthSecretSender,
		ScalarToPoint:      ScalarToPubKey,
		PointAdd: func(p1, p2 [32]byte) ([32]byte, error) {
			return CommitmentAdd(p1, p2)
		},
		BlindingAdd: BlindingAdd,
		BlindingSub: BlindingSub,
		RingSize:    RingSize,
		MinFee:      1000, // 0.00001 BNT minimum
		FeePerByte:  10,   // 0.0000001 BNT per byte
	}

	return wallet.NewBuilder(c.wallet, cfg)
}

// autoScanBlocks subscribes to new blocks and scans them for wallet outputs
func (c *CLI) autoScanBlocks() {
	blockCh := c.daemon.SubscribeBlocks()

	for {
		select {
		case <-c.ctx.Done():
			return
		case block := <-blockCh:
			if block == nil {
				continue
			}

			c.mu.RLock()
			scanner := c.scanner
			w := c.wallet
			c.mu.RUnlock()

			if scanner == nil {
				continue
			}

			blockData := blockToScanData(block)
			found, spent := scanner.ScanBlock(blockData)
			if found > 0 || spent > 0 {
				if err := w.Save(); err != nil {
					fmt.Printf("  Warning: failed to persist wallet scan updates: %v\n", err)
				}
			}
		}
	}
}

// watchMinedBlocks prints explorer links for blocks we mined
func (c *CLI) watchMinedBlocks() {
	minedCh := c.daemon.SubscribeMinedBlocks()

	for {
		select {
		case <-c.ctx.Done():
			return
		case block := <-minedCh:
			if block == nil {
				continue
			}
			height := block.Header.Height
			url := fmt.Sprintf("https://explorer.blocknetcrypto.com/block/%d", height)
			fmt.Printf("\n%s\n", c.sectionHead(fmt.Sprintf("Mined block %d", height)))
			fmt.Printf("  %s\n", url)
		}
	}
}

// blockToScanData converts a Block to scanner-compatible format
func blockToScanData(block *Block) *wallet.BlockData {
	data := &wallet.BlockData{
		Height:       block.Header.Height,
		Transactions: make([]wallet.TxData, len(block.Transactions)),
	}

	for i, tx := range block.Transactions {
		txID, _ := tx.TxID()
		data.Transactions[i] = wallet.TxData{
			TxID:       txID,
			TxPubKey:   tx.TxPublicKey,
			IsCoinbase: tx.IsCoinbase(),
			Outputs:    make([]wallet.OutputData, len(tx.Outputs)),
		}

		for j, out := range tx.Outputs {
			data.Transactions[i].Outputs[j] = wallet.OutputData{
				Index:           j,
				PubKey:          out.PublicKey,
				Commitment:      out.Commitment,
				EncryptedAmount: out.EncryptedAmount,
				EncryptedMemo:   out.EncryptedMemo,
			}
		}

		for _, inp := range tx.Inputs {
			data.Transactions[i].KeyImages = append(data.Transactions[i].KeyImages, inp.KeyImage)
		}
	}

	return data
}

// formatDuration renders a duration as "Xh Ym" or "Ym" for UX ETA strings.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// sanitizeInput removes control characters from user input (fixes tmux copy-paste issues)
func sanitizeInput(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 32 && r < 127 {
			return r
		}
		return -1 // drop the rune
	}, s)
}

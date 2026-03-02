package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

	c.recoverWalletAfterChainReset()

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
			if latest, err := fetchLatestVersion(); err == nil {
				if urgency := versionUrgency(latest, Version); urgency != updateNone {
					c.printUpdateNotice(latest, urgency)
				}
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

func (c *CLI) recoverWalletAfterChainReset() {
	// Check if wallet is ahead of chain (chain was reset/reorged)
	if c.wallet == nil || c.daemon == nil {
		return
	}
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
	} else if chainHeight > 0 && walletHeight == chainHeight {
		// Conservative reorg recovery: if wallet and chain are at the same height,
		// rewind one block to clear potentially stale same-height fork state.
		removed := c.wallet.RewindToHeight(chainHeight - 1)
		if removed > 0 {
			fmt.Printf("  Chain reset: removed %d orphaned outputs, rewound to height %d\n", removed, chainHeight-1)
			if err := c.wallet.Save(); err != nil {
				fmt.Printf("  Warning: failed to persist rewound wallet: %v\n", err)
			}
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
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/blocknetprivacy/blocknet/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "blocknet-version-check")
	resp, err := client.Do(req)
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
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	latest := strings.TrimSpace(strings.TrimPrefix(payload.TagName, "v"))
	if latest == "" {
		return "", fmt.Errorf("tag_name not found in latest release payload")
	}
	return latest, nil
}

func parseVersionParts(v string) ([3]int, bool) {
	var parts [3]int
	core := strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if i := strings.IndexByte(core, '-'); i >= 0 {
		core = core[:i]
	}
	if i := strings.IndexByte(core, '+'); i >= 0 {
		core = core[:i]
	}
	segs := strings.SplitN(core, ".", 3)
	if len(segs) < 1 {
		return parts, false
	}
	for i, s := range segs {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

type updateUrgency int

const (
	updateNone updateUrgency = iota
	updatePatch
	updateMinor
	updateMajor
)

func versionUrgency(latest, current string) updateUrgency {
	l, okLatest := parseVersionParts(latest)
	c, okCurrent := parseVersionParts(current)
	if !okLatest || !okCurrent {
		return updateNone
	}
	for i := range l {
		if l[i] > c[i] {
			switch i {
			case 0:
				return updateMajor
			case 1:
				return updateMinor
			default:
				return updatePatch
			}
		}
		if l[i] < c[i] {
			return updateNone
		}
	}
	return updateNone
}

func (c *CLI) periodicVersionCheck() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if latest, err := fetchLatestVersion(); err == nil {
				if urgency := versionUrgency(latest, Version); urgency != updateNone {
					c.printUpdateNotice(latest, urgency)
				}
			}
		}
	}
}

func (c *CLI) printUpdateNotice(latest string, urgency updateUrgency) {
	accent := "\033[38;2;0;170;255m" // #0AF
	rst := "\033[0m"
	if c.noColor {
		accent = ""
		rst = ""
	}

	title := "Update available"
	message := "A newer release is available."
	switch urgency {
	case updatePatch:
		title = "Patch update available"
		message = "You're fine to keep running, but this includes fixes and polish worth pulling in."
		accent = "\033[38;2;0;170;255m" // #0AF
	case updateMinor:
		title = "Minor update available"
		message = "New features are available, and compatibility may drift soon. Plan to upgrade soon."
		accent = "\033[38;2;255;170;0m" // #FA0
	case updateMajor:
		title = "Major update available"
		message = "This is a significant release. Upgrading is strongly recommended."
		accent = "\033[38;2;255;0;170m" // #F0A
	}
	if c.noColor {
		accent = ""
	}

	fmt.Printf("\n%s# %s%s\n", accent, title, rst)
	fmt.Printf("  %sv%s -> v%s%s\n", accent, Version, latest, rst)
	fmt.Printf("  %s%s%s\n", accent, message, rst)
	fmt.Printf("  %shttps://github.com/blocknetprivacy/blocknet/releases/latest%s\n", accent, rst)
}

func (c *CLI) executeCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}

	if sendArgs, matched, err := parseSendArgsFromPaymentLink(line); matched {
		if err != nil {
			return err
		}
		parts = append([]string{"send"}, sendArgs...)
	}

	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	// Check if locked
	if c.locked && cmd != "unlock" && cmd != "quit" && cmd != "exit" {
		return fmt.Errorf("wallet is locked, use 'unlock' first")
	}

	switch cmd {
	case "help", "?":
		c.cmdHelp(args)
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
	case "outputs", "outs", "out":
		c.cmdOutputs(args)
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
	case "audit":
		c.cmdAuditKeyImages()
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

func parseSendArgsFromPaymentLink(line string) ([]string, bool, error) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nil, false, nil
	}

	lower := strings.ToLower(raw)
	isBlocknetURI := strings.HasPrefix(lower, "blocknet://") || strings.HasPrefix(lower, "blocknet:")
	isBntpayLink := strings.HasPrefix(lower, "bntpay.com/") ||
		strings.HasPrefix(lower, "www.bntpay.com/") ||
		strings.HasPrefix(lower, "https://bntpay.com/") ||
		strings.HasPrefix(lower, "http://bntpay.com/") ||
		strings.HasPrefix(lower, "https://www.bntpay.com/") ||
		strings.HasPrefix(lower, "http://www.bntpay.com/")
	if !isBlocknetURI && !isBntpayLink {
		return nil, false, nil
	}

	link := raw
	if isBntpayLink && !strings.Contains(lower, "://") {
		link = "https://" + raw
	}

	u, err := url.Parse(link)
	if err != nil {
		return nil, true, fmt.Errorf("invalid payment link: %w", err)
	}

	var address string
	switch strings.ToLower(u.Scheme) {
	case "blocknet":
		address = strings.TrimSpace(u.Host)
		if address == "" {
			address = strings.TrimSpace(u.Opaque)
		}
		if address == "" {
			address = strings.Trim(strings.TrimSpace(u.Path), "/")
		}
	case "http", "https":
		host := strings.ToLower(u.Hostname())
		if host != "bntpay.com" && host != "www.bntpay.com" {
			return nil, true, fmt.Errorf("invalid bntpay host: %s", u.Hostname())
		}
		address = strings.Trim(strings.TrimSpace(u.Path), "/")
	default:
		return nil, true, fmt.Errorf("unsupported payment link scheme: %s", u.Scheme)
	}

	if address == "" {
		return nil, true, fmt.Errorf("payment link missing address")
	}

	query := u.Query()
	amount := strings.TrimSpace(query.Get("amount"))
	if amount == "" {
		return nil, true, fmt.Errorf("payment link missing amount")
	}

	args := []string{address, amount}
	if memo := query.Get("memo"); memo != "" {
		args = append(args, memo)
	}
	return args, true, nil
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
	var whole uint64
	if parts[0] != "" {
		var err error
		whole, err = strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
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

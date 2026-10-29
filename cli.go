package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

type CLI struct {
	daemon          *Daemon
	wallet          *wallet.Wallet
	scanner         *wallet.Scanner
	ctx             context.Context
	cancel          context.CancelFunc
	passwordHash    [32]byte
	passwordHashSet bool
	api             *APIServer
	apiAddr         string
	dataDir         string
	walletFile      string
	mu              sync.RWMutex // protects wallet/scanner/passwordHash during hot-load
	// walletScanMu serialises every step that moves a wallet's synced tip
	// (live blocks and catch-up after load/import), so concurrent scanners
	// advance one shared cursor instead of rewinding each other.
	walletScanMu sync.Mutex
	// walletCatchUps counts running catch-up scans. While one runs, the live
	// block loop leaves scanning to it (it re-reads the chain tip every step),
	// so the wallet is not also re-encrypted every 50 blocks by the live loop.
	walletCatchUps atomic.Int32
	// walletSaveMu serialises background scan saves with unload, so no scan
	// writes a wallet file after its unload returns.
	walletSaveMu    sync.Mutex
	walletFileMu    sync.RWMutex
	settingsMu      sync.Mutex
	settings        Settings
	runningSettings Settings
}

// walletLoaded reports whether w is still the loaded wallet. Background scans
// stop once their wallet is unloaded or replaced.
func (c *CLI) walletLoaded(w *wallet.Wallet) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return w != nil && c.wallet == w
}

// scanWalletStep is syncWalletStep for the background scans: it moves w only
// while w is still loaded.
func (c *CLI) scanWalletStep(w *wallet.Wallet, sc *wallet.Scanner) bool {
	c.walletScanMu.Lock()
	defer c.walletScanMu.Unlock()
	return c.walletLoaded(w) && syncWalletStep(c.daemon.Chain(), c.daemon.Mempool(), w, sc)
}

// rewindScannedWallet serializes the initial reorg check with scanning and unload.
func (c *CLI) rewindScannedWallet(w *wallet.Wallet) int {
	c.walletScanMu.Lock()
	defer c.walletScanMu.Unlock()
	if !c.walletLoaded(w) {
		return 0
	}
	return rewindWalletToCanonicalTip(w, c.daemon.Chain())
}

// saveScannedWallet saves scan progress for w unless it was unloaded.
func (c *CLI) saveScannedWallet(w *wallet.Wallet) error {
	c.walletSaveMu.Lock()
	defer c.walletSaveMu.Unlock()
	if !c.walletLoaded(w) {
		return nil
	}
	return w.Save()
}

// waitForWalletScans returns once any scan step or save in flight finishes;
// after the wallet references are cleared, no later one starts.
func (c *CLI) waitForWalletScans() {
	c.walletScanMu.Lock()
	c.walletScanMu.Unlock()
	c.walletSaveMu.Lock()
	c.walletSaveMu.Unlock()
}

// CLIConfig holds CLI configuration
type CLIConfig struct {
	DataDir   string
	APIAddr   string
	SeedNodes []string
	// ResolveSeedNodes, when set, supplies the seeds just before P2P dialing
	// and replaces SeedNodes (see DaemonConfig.ResolveSeedNodes).
	ResolveSeedNodes func() []string
	Settings         Settings
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
		CheckStealthOutput: func(txPub, outputPub, viewPriv, spendPub [32]byte) bool {
			return CheckStealthOutput(spendPub, viewPriv, txPub, outputPub)
		},
		DeriveSpendKey: func(txPub, viewPriv, spendPriv [32]byte) ([32]byte, error) {
			return DeriveStealthSpendKey(txPub, viewPriv, spendPriv)
		},
		DeriveOutputSecret: func(txPub, viewPriv [32]byte) ([32]byte, error) {
			return DeriveStealthSecret(txPub, viewPriv)
		},
		DeriveOutputSecretIndexed: func(txPub, viewPriv [32]byte, outputIndex uint32) ([32]byte, error) {
			return DeriveStealthSecretIndexed(txPub, viewPriv, outputIndex)
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
		ScalarToPoint: ScalarToPubKey,
		PointAdd: func(p1, p2 [32]byte) ([32]byte, error) {
			return CommitmentAdd(p1, p2)
		},
		BlindingAdd:      BlindingAdd,
		ScanOutputsBatch: StealthScanBatch,
	}
}

// NewCLI creates and initializes the CLI
func NewCLI(cfg CLIConfig) (*CLI, error) {
	ctx, cancel := context.WithCancel(context.Background())

	st := cfg.Settings
	cli := &CLI{
		ctx:             ctx,
		cancel:          cancel,
		dataDir:         cfg.DataDir,
		walletFile:      st.WalletFile,
		settings:        st,
		runningSettings: st,
	}

	daemonCfg := DaemonConfig{
		DataDir:           cfg.DataDir,
		ListenAddrs:       []string{st.Listen},
		SeedNodes:         append(append([]string{}, cfg.SeedNodes...), st.ExtraPeers...),
		P2PWhitelistPeers: st.P2PWhitelist,
		P2PMaxInbound:     st.P2PMaxInbound,
		P2PMaxOutbound:    st.P2PMaxOutbound,
		SeedMode:          st.Seed,
		ExplorerAddr:      st.ExplorerAddr,
		SaveCheckpoints:   st.SaveCheckpoints,
		FullSync:          st.FullSync || params.IsTestnet,
		IdentityKeyPath:   seedIdentityKeyPath(st),
		ChainCacheCap:     st.ChainCacheCap,
	}
	if resolve := cfg.ResolveSeedNodes; resolve != nil {
		extraPeers := append([]string{}, st.ExtraPeers...)
		daemonCfg.ResolveSeedNodes = func() []string {
			return append(append([]string{}, resolve()...), extraPeers...)
		}
	}

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

	// Start the API server first, before the (potentially slow) P2P dial and
	// chain catch-up below. The chain DB is already open (NewDaemon), so
	// /status returns a valid height immediately; peers read as 0 and syncing
	// as false until the daemon starts. Binding the API up front means a node
	// that is far out of sync is reachable right away instead of appearing dead
	// while it connects — managers like bnt no longer time out and kill it.
	// Clients can still see that sync is incomplete (height/syncing fields), so
	// funds may not be fully visible until the chain catches up.
	logSinceProcessStart("daemon_ready")
	if c.api != nil {
		if err := c.api.Start(c.apiAddr); err != nil {
			return fmt.Errorf("failed to start API: %w", err)
		}
		logSinceProcessStart("api_listening")
		if isInsecureAPIBindAddress(c.apiAddr) {
			fmt.Printf("\n%s\n  API bind address %q is not loopback\n  Place behind trusted network boundaries or TLS\n", c.errorHead("Warning"), c.apiAddr)
		}
	}

	fmt.Println("  Connecting to network...")
	if err := c.daemon.Start(); err != nil {
		// Tear down the API we just brought up so we don't leave a stale cookie
		// or a half-open listener behind on a failed start.
		if c.api != nil {
			c.api.Stop()
		}
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	c.recoverWalletAfterChainReset()

	// Auto-scan new blocks for wallet
	go c.autoScanBlocks()

	// Watch for blocks we mined and print explorer links
	go c.watchMinedBlocks()

	c.printLogo()
	fmt.Printf("  %s\n  %s\n\n", c.errorHead("Network upgrade notice"), networkUpgradeNotice)
	height := c.daemon.Chain().Height()
	if notice := updateNotice(height); notice != "" {
		fmt.Printf("  %s\n  %s\n\n", c.errorHead("Update notice"), notice)
	}

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

func (c *CLI) recoverWalletAfterChainReset() {
	// Check if wallet is ahead of chain (chain was reset/reorged)
	if c.wallet == nil || c.daemon == nil {
		return
	}
	chainHeight := c.daemon.Chain().Height()
	originalWalletHeight := c.wallet.SyncedHeight()
	rewound := rewindWalletToCanonicalTip(c.wallet, c.daemon.Chain())
	if rewound > 0 {
		fmt.Printf("  Chain reset: removed %d orphaned outputs, rewound to height %d\n", rewound, c.wallet.SyncedHeight())
		if err := c.wallet.Save(); err != nil {
			fmt.Printf("  Warning: failed to persist rewound wallet: %v\n", err)
		}
	}

	walletHeight, walletHash := c.wallet.SyncedBlock()
	if chainHeight > 0 && originalWalletHeight == chainHeight && walletHeight == chainHeight && !walletSyncHashKnown(walletHash) {
		// Legacy wallets do not know which block hash they scanned at the tip.
		// Rewind one block so the next scan records canonical hash metadata.
		removed := c.wallet.RewindToHeight(chainHeight - 1)
		if removed > 0 {
			fmt.Printf("  Chain reset: removed %d orphaned outputs, rewound to height %d\n", removed, chainHeight-1)
			if err := c.wallet.Save(); err != nil {
				fmt.Printf("  Warning: failed to persist rewound wallet: %v\n", err)
			}
		}
	}

	issues := repairableWalletOutputIssues(auditWalletCanonicalOutputs(c.daemon.Chain(), c.wallet.AllOutputs()), false)
	removed := removeWalletOutputIssues(c.wallet, issues)
	if len(removed) == 0 {
		return
	}

	fmt.Printf("  Chain repair: removed %d noncanonical wallet outputs totaling %s\n",
		len(removed), formatAmount(sumOwnedOutputs(removed)))
	if err := c.wallet.Save(); err != nil {
		fmt.Printf("  Warning: failed to persist wallet output repair: %v\n", err)
	}
}

func (c *CLI) printLogo() {
	green := "\033[38;5;118m"
	if params.IsTestnet {
		green = "\033[38;2;255;0;170m"
	}
	reset := "\033[0m"

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

func (c *CLI) sectionHead(label string) string {
	return "\033[38;2;170;255;0m#\033[0m " + label
}

func (c *CLI) errorHead(label string) string {
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

// autoScanBlocks subscribes to new blocks and scans them for wallet outputs
func (c *CLI) autoScanBlocks() {
	blockCh := c.daemon.SubscribeBlocks()
	const saveBatchSize = 50
	unsaved := 0
	saveTimer := time.NewTimer(10 * time.Second)
	defer saveTimer.Stop()

	doSave := func() {
		if unsaved == 0 {
			return
		}
		c.mu.RLock()
		w := c.wallet
		c.mu.RUnlock()
		if w == nil {
			return
		}
		if err := c.saveScannedWallet(w); err != nil {
			fmt.Printf("  Warning: failed to persist wallet scan updates: %v\n", err)
		}
		unsaved = 0
		saveTimer.Reset(10 * time.Second)
	}

	for {
		select {
		case <-c.ctx.Done():
			doSave()
			return
		case <-saveTimer.C:
			doSave()
		case block := <-blockCh:
			if block == nil {
				continue
			}

			c.mu.RLock()
			scanner := c.scanner
			w := c.wallet
			c.mu.RUnlock()

			if scanner == nil || w == nil {
				continue
			}

			if c.walletCatchUps.Load() > 0 {
				continue
			}

			removed := c.rewindScannedWallet(w)
			if removed > 0 {
				unsaved++
			}

			for {
				if !c.scanWalletStep(w, scanner) {
					break
				}
				unsaved++
				if unsaved >= saveBatchSize {
					doSave()
				}
			}
		}
	}
}

// applyBlockToWallet scans a newly-connected block into the wallet, detecting
// reorgs. If the block does not build on the wallet's last synced tip, the
// wallet has been tracking a branch that was (at least partly) disconnected, so
// it may hold outputs from orphaned blocks — outputs that are no longer
// canonical and would be rejected as ring members ("not a canonical on-chain
// output") if a send selected them. In that case it reconciles against the
// canonical chain, dropping the stale outputs; otherwise it scans forward.
func applyBlockToWallet(chain *Chain, w *wallet.Wallet, sc *wallet.Scanner, block *Block) {
	if chain == nil || w == nil || sc == nil || block == nil {
		return
	}
	syncedHash := w.SyncedHash()
	extends := (syncedHash == [32]byte{} && block.Header.Height == w.SyncedHeight()+1) ||
		(syncedHash != [32]byte{} && block.Header.PrevHash == syncedHash)
	if extends {
		sc.ScanBlock(blockToScanData(block))
		w.SetSyncedTip(block.Header.Height, block.Hash())
		return
	}
	reconcileWalletReorg(chain, w, sc)
}

// reconcileWalletReorg rewinds the wallet to the finalized floor — reorgs deeper
// than MaxReorgDepth are rejected by consensus, so everything at or below the
// floor is immutable — then rescans the canonical chain forward to the tip. This
// drops owned outputs that lived only in disconnected blocks and re-discovers
// outputs on the new branch. Scanning is idempotent (AddOutput dedups, MarkSpent
// is idempotent), so an over-broad rescan is safe.
func reconcileWalletReorg(chain *Chain, w *wallet.Wallet, sc *wallet.Scanner) {
	if chain == nil || w == nil || sc == nil {
		return
	}
	tip := chain.Height()
	floor := uint64(0)
	if tip > MaxReorgDepth {
		floor = tip - MaxReorgDepth
	}
	if synced := w.SyncedHeight(); synced < floor {
		// The wallet is still behind the finalized floor; nothing above it can
		// have been reorged, so just resync forward from where it is.
		floor = synced
	}
	w.RewindToHeight(floor)
	for h := floor + 1; h <= tip; h++ {
		block := chain.GetBlockByHeight(h)
		if block == nil {
			return
		}
		sc.ScanBlock(blockToScanData(block))
		w.SetSyncedTip(h, block.Hash())
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
		Hash:         block.Hash(),
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

// sanitizeInput removes control characters from user input (fixes tmux copy-paste issues)
func sanitizeInput(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 32 && r < 127 {
			return r
		}
		return -1 // drop the rune
	}, s)
}

// deriveViewWalletFilename maps a wallet path to its view-only sibling:
// "main.wallet.dat" and "main.dat" -> "main.view.wallet.dat".
func deriveViewWalletFilename(walletFile string) string {
	name := strings.TrimSuffix(walletFile, ".wallet.dat")
	if name == walletFile {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name + ".view.wallet.dat"
}

func (c *CLI) walletFilePath() string {
	c.walletFileMu.RLock()
	defer c.walletFileMu.RUnlock()
	return c.walletFile
}

func (c *CLI) setWalletFile(path string) {
	c.walletFileMu.Lock()
	c.walletFile = path
	c.walletFileMu.Unlock()
}

func seedIdentityKeyPath(st Settings) string {
	if st.IdentityKeyPath != "" || !st.Seed {
		return st.IdentityKeyPath
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	network := "mainnet"
	if params.IsTestnet {
		network = "testnet"
	}
	return filepath.Join(configDir, "blocknet", network, "identity.key")
}

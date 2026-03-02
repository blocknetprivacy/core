package wallet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha3"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blocknet/protocol/params"

	"github.com/btcsuite/btcutil/base58"
	"golang.org/x/crypto/argon2"
)

// wipeBytes best-effort zeroes a byte slice.
// This is not a guarantee in Go (copies may exist), but it reduces exposure windows.
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func walletBackupDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, "blocknet", "mainnet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

const backupSuffix = ".BACKUP.dat"

// backupWalletFile copies the wallet file into the XDG config directory as two
// independent backups — one visible, one hidden. The filename includes a
// timestamp for human reference; existing backups for the same address are
// replaced so we never accumulate duplicates.
func backupWalletFile(walletFile string, address string) {
	dir, err := walletBackupDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(walletFile)
	if err != nil {
		return
	}

	suffix := "-" + address + backupSuffix
	// Remove any previous backups for this address (visible and hidden).
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			n := e.Name()
			bare := n
			if bare[0] == '.' {
				bare = bare[1:]
			}
			if strings.HasSuffix(bare, suffix) {
				os.Remove(filepath.Join(dir, n))
			}
		}
	}

	stamp := time.Now().Format("2006-01-02-150405")
	name := stamp + suffix
	os.WriteFile(filepath.Join(dir, name), data, 0o600)
	os.WriteFile(filepath.Join(dir, "."+name), data, 0o600)
}

// BackfillWalletBackups scans a directory for .dat wallet files that don't
// already have XDG backups and copies them as "unknown" address backups,
// using the file's modification time as the timestamp.
func BackfillWalletBackups(dirs ...string) {
	backupDir, err := walletBackupDir()
	if err != nil {
		return
	}
	// Collect existing backup filenames so we can skip files already backed up.
	existing, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}
	backedUpFiles := make(map[string]bool)
	for _, e := range existing {
		backedUpFiles[e.Name()] = true
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".dat") || strings.Contains(name, "BACKUP") {
				continue
			}
			src := filepath.Join(dir, name)
			data, err := os.ReadFile(src)
			if err != nil || len(data) == 0 {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			stamp := info.ModTime().Format("2006-01-02-150405")
			backupName := stamp + "-unknown" + backupSuffix

			// Check if a backup already exists for this exact file (by timestamp match).
			suffix := "-unknown" + backupSuffix
			alreadyExists := false
			for bName := range backedUpFiles {
				bare := bName
				if bare[0] == '.' {
					bare = bare[1:]
				}
				if strings.HasSuffix(bare, suffix) && strings.HasPrefix(bare, stamp) {
					alreadyExists = true
					break
				}
			}
			if alreadyExists {
				continue
			}

			os.WriteFile(filepath.Join(backupDir, backupName), data, 0o600)
			os.WriteFile(filepath.Join(backupDir, "."+backupName), data, 0o600)
			backedUpFiles[backupName] = true
			backedUpFiles["."+backupName] = true
		}
	}
}

// BackupEntry describes a wallet backup found in the XDG config directory.
type BackupEntry struct {
	Address   string // wallet address, or "unknown" for unidentified backups
	Timestamp string // YYYY-MM-DD from the backup filename
}

// ListBackups returns the wallet backups found in the XDG config directory.
func ListBackups() []BackupEntry {
	dir, err := walletBackupDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var result []BackupEntry
	for _, e := range entries {
		name := e.Name()
		if name[0] == '.' {
			name = name[1:]
		}
		if !strings.HasSuffix(name, backupSuffix) {
			continue
		}
		// Format: YYYY-MM-DD-HHmmSS-<address>.BACKUP.dat
		rest := strings.TrimSuffix(name, backupSuffix)
		if idx := strings.LastIndex(rest, "-"); idx >= 15 {
			addr := rest[idx+1:]
			stamp := rest[:10] // YYYY-MM-DD
			if addr != "" && !seen[addr] {
				seen[addr] = true
				result = append(result, BackupEntry{Address: addr, Timestamp: stamp})
			}
		}
	}
	return result
}

// RestoreBackup copies the XDG backup for the given address to destFile.
// Prefers the hidden copy (less likely to have been tampered with).
func RestoreBackup(address, destFile string) error {
	dir, err := walletBackupDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	suffix := "-" + address + backupSuffix
	// Two passes: hidden first, then visible.
	for _, hidden := range []bool{true, false} {
		for _, e := range entries {
			n := e.Name()
			isHidden := n[0] == '.'
			if isHidden != hidden {
				continue
			}
			bare := n
			if isHidden {
				bare = bare[1:]
			}
			if strings.HasSuffix(bare, suffix) {
				data, err := os.ReadFile(filepath.Join(dir, n))
				if err != nil {
					continue
				}
				return os.WriteFile(destFile, data, 0o600)
			}
		}
	}
	return fmt.Errorf("no backup found for %s", address)
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// StealthKeys contains the two keypairs needed for stealth addresses
type StealthKeys struct {
	SpendPrivKey [32]byte `json:"spend_priv"`
	SpendPubKey  [32]byte `json:"spend_pub"`
	ViewPrivKey  [32]byte `json:"view_priv"`
	ViewPubKey   [32]byte `json:"view_pub"`
}

// Address returns the public stealth address (base58 encoded spend+view pubkeys)
func (sk *StealthKeys) Address() string {
	// Address = base58(spend_pub || view_pub || checksum4)
	payload := make([]byte, 64)
	copy(payload[:32], sk.SpendPubKey[:])
	copy(payload[32:], sk.ViewPubKey[:])

	sum := addressChecksum(payload)
	combined := make([]byte, 0, 68)
	combined = append(combined, payload...)
	combined = append(combined, sum[:4]...)
	return base58.Encode(combined)
}

// OwnedOutput represents an output the wallet can spend
type OwnedOutput struct {
	TxID           [32]byte `json:"txid"`
	OutputIndex    int      `json:"output_index"`
	Amount         uint64   `json:"amount"`
	Blinding       [32]byte `json:"blinding"`
	OneTimePrivKey [32]byte `json:"one_time_priv"`
	OneTimePubKey  [32]byte `json:"one_time_pub"`
	Commitment     [32]byte `json:"commitment"`
	BlockHeight    uint64   `json:"block_height"`
	IsCoinbase     bool     `json:"is_coinbase"` // True if from mining reward
	Spent          bool     `json:"spent"`
	SpentHeight    uint64   `json:"spent_height,omitempty"`
	Memo           []byte   `json:"memo,omitempty"` // Decrypted memo payload
}

// SendRecord tracks outgoing transaction details
type SendRecord struct {
	TxID        [32]byte `json:"txid"`
	Timestamp   int64    `json:"timestamp"`
	Recipient   string   `json:"recipient"` // base58 address
	Amount      uint64   `json:"amount"`    // actual amount sent (not including fee)
	Fee         uint64   `json:"fee"`
	BlockHeight uint64   `json:"block_height"`   // when confirmed
	Memo        []byte   `json:"memo,omitempty"` // Memo used (plaintext)
}

// WalletData is the serializable wallet state
type WalletData struct {
	Version      uint32         `json:"version"`
	ViewOnly     bool           `json:"view_only"`          // True if this is a view-only wallet
	Mnemonic     string         `json:"mnemonic,omitempty"` // BIP39 12-word recovery phrase (empty for view-only)
	Keys         StealthKeys    `json:"keys"`
	Outputs      []*OwnedOutput `json:"outputs"`
	SendHistory  []*SendRecord  `json:"send_history,omitempty"` // Track outgoing transactions
	PendingCredits []*PendingCredit `json:"pending_credits,omitempty"` // UX-only pending credits (e.g. unconfirmed change)
	SyncedHeight uint64         `json:"synced_height"`
	CreatedAt    int64          `json:"created_at"`
}

// ViewOnlyKeys contains only the keys needed for a view-only wallet
type ViewOnlyKeys struct {
	SpendPubKey [32]byte `json:"spend_pub"`
	ViewPrivKey [32]byte `json:"view_priv"`
	ViewPubKey  [32]byte `json:"view_pub"`
}

// Wallet manages keys and tracks owned outputs
type Wallet struct {
	mu sync.RWMutex

	data     WalletData
	filename string
	password []byte // kept in memory for re-encryption on save

	// inputReservations tracks outputs reserved for pending spends.
	// Reservation is best-effort: it prevents concurrent builders from selecting the
	// same inputs, and expires automatically after a TTL.
	inputReservations map[reservedOutpoint]inputReservation
	nextLease         atomic.Uint64

	// Diagnostics counters (not persisted).
	memoDecryptFailures   atomic.Uint64
	memoDecryptLastHeight atomic.Uint64

	// Callbacks for crypto operations (set by main package)
	generateStealthKeys     func() (*StealthKeys, error)
	deriveStealthAddress    func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error)
	checkStealthOutput      func(txPub, outputPub, viewPriv, spendPub [32]byte) bool
	deriveSpendKey          func(txPub, viewPriv, spendPriv [32]byte) ([32]byte, error)
	deriveOutputSecret      func(txPub, viewPriv [32]byte) ([32]byte, error)
	generateKeypairFromSeed func(seed [32]byte) (priv, pub [32]byte, err error)
}

// PendingCredit tracks a credit we expect to receive but haven't yet scanned
// from a confirmed block (e.g. our own change output after broadcasting a tx).
// Persisted in the wallet file for UX continuity across restarts.
type PendingCredit struct {
	TxID    [32]byte `json:"txid"`
	Amount  uint64   `json:"amount"`
	AddedAt int64    `json:"added_at"`
}

type reservedOutpoint struct {
	TxID        [32]byte
	OutputIndex int
}

type inputReservation struct {
	lease     uint64
	expiresAt time.Time
}

// WalletConfig holds wallet configuration
type WalletConfig struct {
	GenerateStealthKeys  func() (*StealthKeys, error)
	DeriveStealthAddress func(spendPub, viewPub [32]byte) (txPriv, txPub, oneTimePub [32]byte, err error)
	CheckStealthOutput   func(txPub, outputPub, viewPriv, spendPub [32]byte) bool
	DeriveSpendKey       func(txPub, viewPriv, spendPriv [32]byte) ([32]byte, error)
	DeriveOutputSecret   func(txPub, viewPriv [32]byte) ([32]byte, error)

	// For deterministic key derivation from BIP39 seed
	GenerateKeypairFromSeed func(seed [32]byte) (priv, pub [32]byte, err error)
}

// NewWallet creates a new wallet with a fresh BIP39 mnemonic
func NewWallet(filename string, password []byte, cfg WalletConfig) (*Wallet, error) {
	// Generate new mnemonic
	mnemonic, err := GenerateMnemonic()
	if err != nil {
		return nil, fmt.Errorf("failed to generate mnemonic: %w", err)
	}

	return NewWalletFromMnemonic(filename, password, mnemonic, cfg)
}

// NewWalletFromMnemonic creates a wallet from an existing mnemonic (for recovery)
func NewWalletFromMnemonic(filename string, password []byte, mnemonic string, cfg WalletConfig) (*Wallet, error) {
	// Derive seed from mnemonic (no passphrase - password is for encryption only)
	seed, err := MnemonicToSeed(mnemonic, "")
	if err != nil {
		return nil, fmt.Errorf("invalid mnemonic: %w", err)
	}

	// Derive keys from seed
	keys, err := DeriveKeysFromSeed(seed, cfg.GenerateKeypairFromSeed)
	if err != nil {
		return nil, fmt.Errorf("failed to derive keys from seed: %w", err)
	}

	w := &Wallet{
		filename:                filename,
		password:                cloneBytes(password),
		inputReservations:       make(map[reservedOutpoint]inputReservation),
		generateStealthKeys:     cfg.GenerateStealthKeys,
		deriveStealthAddress:    cfg.DeriveStealthAddress,
		checkStealthOutput:      cfg.CheckStealthOutput,
		deriveSpendKey:          cfg.DeriveSpendKey,
		deriveOutputSecret:      cfg.DeriveOutputSecret,
		generateKeypairFromSeed: cfg.GenerateKeypairFromSeed,
	}

	w.data = WalletData{
		Version:      1,
		Mnemonic:     mnemonic,
		Keys:         *keys,
		Outputs:      make([]*OwnedOutput, 0),
		SyncedHeight: 0,
		CreatedAt:    unixNow(),
	}

	// Save immediately
	if err := w.Save(); err != nil {
		return nil, fmt.Errorf("failed to save new wallet: %w", err)
	}

	// Don't keep mnemonic resident in the long-lived wallet struct.
	// `Save()` preserves the on-disk mnemonic even when in-memory field is empty.
	w.data.Mnemonic = ""

	return w, nil
}

// NewWalletFromStealthKeys creates a full wallet from explicit spend/view keys.
// This path does not include a BIP39 mnemonic in the wallet file.
func NewWalletFromStealthKeys(filename string, password []byte, keys StealthKeys, cfg WalletConfig) (*Wallet, error) {
	w := &Wallet{
		filename:                filename,
		password:                cloneBytes(password),
		inputReservations:       make(map[reservedOutpoint]inputReservation),
		generateStealthKeys:     cfg.GenerateStealthKeys,
		deriveStealthAddress:    cfg.DeriveStealthAddress,
		checkStealthOutput:      cfg.CheckStealthOutput,
		deriveSpendKey:          cfg.DeriveSpendKey,
		deriveOutputSecret:      cfg.DeriveOutputSecret,
		generateKeypairFromSeed: cfg.GenerateKeypairFromSeed,
	}

	w.data = WalletData{
		Version:      1,
		Mnemonic:     "",
		Keys:         keys,
		Outputs:      make([]*OwnedOutput, 0),
		SyncedHeight: 0,
		CreatedAt:    unixNow(),
	}

	if err := w.Save(); err != nil {
		return nil, fmt.Errorf("failed to save imported wallet: %w", err)
	}

	return w, nil
}

// LoadWallet loads an existing encrypted wallet
func LoadWallet(filename string, password []byte, cfg WalletConfig) (*Wallet, error) {
	encrypted, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read wallet file: %w", err)
	}

	// Decrypt
	plaintext, err := decrypt(encrypted, password)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt wallet (wrong password?): %w", err)
	}
	defer wipeBytes(plaintext)

	var data WalletData
	if err := json.Unmarshal(plaintext, &data); err != nil {
		return nil, fmt.Errorf("failed to parse wallet data: %w", err)
	}

	// Avoid long-lived in-memory mnemonic retention; fetch on demand from disk.
	data.Mnemonic = ""

	return &Wallet{
		data:                    data,
		filename:                filename,
		password:                cloneBytes(password),
		inputReservations:       make(map[reservedOutpoint]inputReservation),
		generateStealthKeys:     cfg.GenerateStealthKeys,
		deriveStealthAddress:    cfg.DeriveStealthAddress,
		generateKeypairFromSeed: cfg.GenerateKeypairFromSeed,
		checkStealthOutput:      cfg.CheckStealthOutput,
		deriveSpendKey:          cfg.DeriveSpendKey,
		deriveOutputSecret:      cfg.DeriveOutputSecret,
	}, nil
}

// NewViewOnlyWallet creates a view-only wallet from exported keys
// View-only wallets can scan for incoming funds but cannot spend
func NewViewOnlyWallet(filename string, password []byte, keys ViewOnlyKeys, cfg WalletConfig) (*Wallet, error) {
	w := &Wallet{
		filename:             filename,
		password:             cloneBytes(password),
		inputReservations:    make(map[reservedOutpoint]inputReservation),
		deriveStealthAddress: cfg.DeriveStealthAddress,
		checkStealthOutput:   cfg.CheckStealthOutput,
		deriveOutputSecret:   cfg.DeriveOutputSecret,
	}

	// Create wallet data with view-only flag
	// SpendPrivKey is zeroed (we don't have it)
	w.data = WalletData{
		Version:  1,
		ViewOnly: true,
		Keys: StealthKeys{
			SpendPrivKey: [32]byte{}, // Zero - we don't have the spend key
			SpendPubKey:  keys.SpendPubKey,
			ViewPrivKey:  keys.ViewPrivKey,
			ViewPubKey:   keys.ViewPubKey,
		},
		Outputs:      make([]*OwnedOutput, 0),
		SyncedHeight: 0,
		CreatedAt:    unixNow(),
	}

	if err := w.Save(); err != nil {
		return nil, fmt.Errorf("failed to save view-only wallet: %w", err)
	}

	return w, nil
}

// LoadOrCreateWallet loads existing wallet or creates new one
func LoadOrCreateWallet(filename string, password []byte, cfg WalletConfig) (*Wallet, error) {
	if _, err := os.Stat(filename); errors.Is(err, os.ErrNotExist) {
		return NewWallet(filename, password, cfg)
	}
	return LoadWallet(filename, password, cfg)
}

// Save encrypts and writes wallet to disk
func (w *Wallet) Save() error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	dataToPersist := w.data
	// Mnemonic is intentionally not kept in the long-lived wallet struct; preserve
	// any on-disk mnemonic so future saves don't erase it.
	if !dataToPersist.ViewOnly && dataToPersist.Mnemonic == "" {
		if mnemonic, err := w.readMnemonicFromDisk(); err == nil && mnemonic != "" {
			dataToPersist.Mnemonic = mnemonic
		}
	}

	plaintext, err := json.MarshalIndent(dataToPersist, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal wallet: %w", err)
	}
	defer wipeBytes(plaintext)

	encrypted, err := encrypt(plaintext, w.password)
	if err != nil {
		return fmt.Errorf("failed to encrypt wallet: %w", err)
	}

	if err := os.WriteFile(w.filename, encrypted, 0600); err != nil {
		return fmt.Errorf("failed to write wallet file: %w", err)
	}

	backupWalletFile(w.filename, w.data.Keys.Address())
	return nil
}

// Address returns the wallet's public stealth address
func (w *Wallet) Address() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.Keys.Address()
}

// Mnemonic returns the BIP39 recovery phrase
func (w *Wallet) Mnemonic() (string, error) {
	w.mu.RLock()
	viewOnly := w.data.ViewOnly
	w.mu.RUnlock()
	if viewOnly {
		return "", nil
	}
	return w.readMnemonicFromDisk()
}

// IsViewOnly returns true if this is a view-only wallet
func (w *Wallet) IsViewOnly() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.ViewOnly
}

// EncryptionPasswordClone returns a best-effort clone of the wallet encryption
// password for internal operations that create additional wallet files.
func (w *Wallet) EncryptionPasswordClone() []byte {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return cloneBytes(w.password)
}

// ExportViewOnlyKeys exports the keys needed to create a view-only wallet
func (w *Wallet) ExportViewOnlyKeys() ViewOnlyKeys {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return ViewOnlyKeys{
		SpendPubKey: w.data.Keys.SpendPubKey,
		ViewPrivKey: w.data.Keys.ViewPrivKey,
		ViewPubKey:  w.data.Keys.ViewPubKey,
	}
}

// Keys returns the wallet's stealth keys
func (w *Wallet) Keys() StealthKeys {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.Keys
}

// SpendPubKey returns the spend public key
func (w *Wallet) SpendPubKey() [32]byte {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.Keys.SpendPubKey
}

// ViewPubKey returns the view public key
func (w *Wallet) ViewPubKey() [32]byte {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.Keys.ViewPubKey
}

// Maturity constants (must match block.go)
const (
	CoinbaseMaturity  = 60 // Mined coins locked for 60 blocks
	SafeConfirmations = 10 // Regular coins need 10 confirmations
	// EstimatedBlockInterval is a UX-only approximation used for CLI ETA displays.
	EstimatedBlockInterval = 5 * time.Minute
)

// IsOutputMature checks if an output is mature enough to spend
func IsOutputMature(out *OwnedOutput, currentHeight uint64) bool {
	if out.Spent {
		return false
	}

	confirmations := uint64(0)
	if currentHeight >= out.BlockHeight {
		confirmations = currentHeight - out.BlockHeight
	}

	if out.IsCoinbase {
		return confirmations >= CoinbaseMaturity
	}
	return confirmations >= SafeConfirmations
}

// Balance returns total unspent balance (regardless of maturity)
func (w *Wallet) Balance() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var total uint64
	for _, out := range w.data.Outputs {
		if !out.Spent {
			total += out.Amount
		}
	}
	return total
}

// SpendableBalance returns balance that can actually be spent now
func (w *Wallet) SpendableBalance(currentHeight uint64) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var total uint64
	for _, out := range w.data.Outputs {
		if IsOutputMature(out, currentHeight) {
			total += out.Amount
		}
	}
	return total
}

// PendingBalance returns balance that exists but can't be spent yet
func (w *Wallet) PendingBalance(currentHeight uint64) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var total uint64
	for _, out := range w.data.Outputs {
		if !out.Spent && !IsOutputMature(out, currentHeight) {
			total += out.Amount
		}
	}
	return total
}

// AllOutputs returns all outputs (spent and unspent)
func (w *Wallet) AllOutputs() []*OwnedOutput {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// Return snapshots, not internal pointers.
	outputs := make([]*OwnedOutput, 0, len(w.data.Outputs))
	for _, out := range w.data.Outputs {
		if out == nil {
			continue
		}
		c := *out
		if len(out.Memo) > 0 {
			c.Memo = append([]byte(nil), out.Memo...)
		} else {
			c.Memo = nil
		}
		outputs = append(outputs, &c)
	}
	return outputs
}

// SpendableOutputs returns all unspent outputs (regardless of maturity)
func (w *Wallet) SpendableOutputs() []*OwnedOutput {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var outputs []*OwnedOutput
	for _, out := range w.data.Outputs {
		if out != nil && !out.Spent {
			c := *out
			if len(out.Memo) > 0 {
				c.Memo = append([]byte(nil), out.Memo...)
			} else {
				c.Memo = nil
			}
			outputs = append(outputs, &c)
		}
	}
	return outputs
}

// MatureOutputs returns only outputs that are mature enough to spend
func (w *Wallet) MatureOutputs(currentHeight uint64) []*OwnedOutput {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var outputs []*OwnedOutput
	for _, out := range w.data.Outputs {
		if out != nil && IsOutputMature(out, currentHeight) {
			c := *out
			if len(out.Memo) > 0 {
				c.Memo = append([]byte(nil), out.Memo...)
			} else {
				c.Memo = nil
			}
			outputs = append(outputs, &c)
		}
	}
	return outputs
}

// AddOutput adds a newly discovered output.
// Deduplicates by (TxID, OutputIndex) so rescans or repeated block
// notifications don't inflate balances.
func (w *Wallet) AddOutput(out *OwnedOutput) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, existing := range w.data.Outputs {
		if existing.TxID == out.TxID && existing.OutputIndex == out.OutputIndex {
			return
		}
	}

	w.data.Outputs = append(w.data.Outputs, out)

	// If we were tracking an unconfirmed expected credit (e.g. change) and the
	// scanner just found an output from that tx in a confirmed block, drop it.
	if len(w.data.PendingCredits) > 0 {
		kept := w.data.PendingCredits[:0]
		for _, pc := range w.data.PendingCredits {
			if pc == nil || pc.TxID == out.TxID {
				continue
			}
			kept = append(kept, pc)
		}
		if len(kept) == 0 {
			w.data.PendingCredits = nil
		} else {
			w.data.PendingCredits = kept
		}
	}
}

// AddPendingCredit records an expected credit that hasn't been confirmed/scanned yet.
// This is used to surface "pending change" immediately after broadcasting a tx.
func (w *Wallet) AddPendingCredit(txID [32]byte, amount uint64) {
	if amount == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.data.PendingCredits == nil {
		w.data.PendingCredits = make([]*PendingCredit, 0, 1)
	}
	// Replace existing entry for the same txid (if any).
	for _, pc := range w.data.PendingCredits {
		if pc != nil && pc.TxID == txID {
			pc.Amount = amount
			pc.AddedAt = time.Now().Unix()
			return
		}
	}
	w.data.PendingCredits = append(w.data.PendingCredits, &PendingCredit{
		TxID:    txID,
		Amount:  amount,
		AddedAt: time.Now().Unix(),
	})
}

// PendingUnconfirmedBalance returns the total amount we expect to receive but
// haven't yet scanned from a confirmed block.
func (w *Wallet) PendingUnconfirmedBalance() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var total uint64
	for _, pc := range w.data.PendingCredits {
		if pc == nil {
			continue
		}
		total += pc.Amount
	}
	return total
}

// MarkSpent marks an output as spent by its one-time pubkey
func (w *Wallet) MarkSpent(oneTimePubKey [32]byte, height uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, out := range w.data.Outputs {
		if out.OneTimePubKey == oneTimePubKey && !out.Spent {
			out.Spent = true
			out.SpentHeight = height
			// Clear any pending reservation for this outpoint.
			if w.inputReservations != nil {
				delete(w.inputReservations, reservedOutpoint{TxID: out.TxID, OutputIndex: out.OutputIndex})
			}
			return true
		}
	}
	return false
}

// ReserveMatureInputs selects spendable mature outputs and reserves them under a lease.
// Callers should release the lease if the spend attempt is abandoned; otherwise the
// reservation expires after ttl.
func (w *Wallet) ReserveMatureInputs(currentHeight uint64, targetAmount uint64, ttl time.Duration) (lease uint64, inputs []*OwnedOutput, err error) {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}

	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.inputReservations == nil {
		w.inputReservations = make(map[reservedOutpoint]inputReservation)
	}

	// Drop expired reservations.
	for op, res := range w.inputReservations {
		if now.After(res.expiresAt) {
			delete(w.inputReservations, op)
		}
	}

	// Build candidate set from internal state while holding the lock.
	candidates := make([]*OwnedOutput, 0, len(w.data.Outputs))
	for _, out := range w.data.Outputs {
		if out == nil || out.Spent {
			continue
		}
		if !IsOutputMature(out, currentHeight) {
			continue
		}
		op := reservedOutpoint{TxID: out.TxID, OutputIndex: out.OutputIndex}
		if _, reserved := w.inputReservations[op]; reserved {
			continue
		}
		candidates = append(candidates, out)
	}

	selected, selErr := SelectInputs(candidates, targetAmount)
	if selErr != nil {
		return 0, nil, selErr
	}

	lease = w.nextLease.Add(1)
	expires := now.Add(ttl)

	// Reserve selected outputs (fail closed if any outpoint is already reserved).
	for _, out := range selected {
		op := reservedOutpoint{TxID: out.TxID, OutputIndex: out.OutputIndex}
		if existing, reserved := w.inputReservations[op]; reserved && now.Before(existing.expiresAt) {
			// Roll back partial reservations for this lease.
			for rop, res := range w.inputReservations {
				if res.lease == lease {
					delete(w.inputReservations, rop)
				}
			}
			return 0, nil, errors.New("selected output already reserved")
		}
		w.inputReservations[op] = inputReservation{lease: lease, expiresAt: expires}
	}

	// Return snapshots, not internal pointers.
	inputs = make([]*OwnedOutput, 0, len(selected))
	for _, out := range selected {
		c := *out
		if len(out.Memo) > 0 {
			c.Memo = append([]byte(nil), out.Memo...)
		} else {
			c.Memo = nil
		}
		inputs = append(inputs, &c)
	}

	return lease, inputs, nil
}

// ReserveAllMatureInputs reserves every mature, unspent, unreserved output.
func (w *Wallet) ReserveAllMatureInputs(currentHeight uint64, ttl time.Duration) (lease uint64, inputs []*OwnedOutput, err error) {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}

	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.inputReservations == nil {
		w.inputReservations = make(map[reservedOutpoint]inputReservation)
	}

	for op, res := range w.inputReservations {
		if now.After(res.expiresAt) {
			delete(w.inputReservations, op)
		}
	}

	var candidates []*OwnedOutput
	for _, out := range w.data.Outputs {
		if out == nil || out.Spent {
			continue
		}
		if !IsOutputMature(out, currentHeight) {
			continue
		}
		op := reservedOutpoint{TxID: out.TxID, OutputIndex: out.OutputIndex}
		if _, reserved := w.inputReservations[op]; reserved {
			continue
		}
		candidates = append(candidates, out)
	}

	if len(candidates) == 0 {
		return 0, nil, errors.New("no spendable outputs")
	}

	lease = w.nextLease.Add(1)
	expires := now.Add(ttl)

	for _, out := range candidates {
		op := reservedOutpoint{TxID: out.TxID, OutputIndex: out.OutputIndex}
		w.inputReservations[op] = inputReservation{lease: lease, expiresAt: expires}
	}

	inputs = make([]*OwnedOutput, 0, len(candidates))
	for _, out := range candidates {
		c := *out
		if len(out.Memo) > 0 {
			c.Memo = append([]byte(nil), out.Memo...)
		} else {
			c.Memo = nil
		}
		inputs = append(inputs, &c)
	}

	return lease, inputs, nil
}

// ReleaseInputLease releases all reservations held by a given lease id.
func (w *Wallet) ReleaseInputLease(lease uint64) {
	if lease == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for op, res := range w.inputReservations {
		if res.lease == lease {
			delete(w.inputReservations, op)
		}
	}
}

// SyncedHeight returns the last synced block height
func (w *Wallet) SyncedHeight() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.data.SyncedHeight
}

// SetSyncedHeight updates the sync height
func (w *Wallet) SetSyncedHeight(height uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data.SyncedHeight = height
}

// RewindToHeight removes outputs from blocks above the given height
// and resets synced height. Used when chain has been reset/reorged.
func (w *Wallet) RewindToHeight(height uint64) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	var kept []*OwnedOutput
	removed := 0
	for _, out := range w.data.Outputs {
		if out.BlockHeight <= height {
			// Also un-spend outputs whose spend was above the rewind point
			if out.Spent && out.SpentHeight > height {
				out.Spent = false
				out.SpentHeight = 0
			}
			kept = append(kept, out)
		} else {
			removed++
		}
	}
	w.data.Outputs = kept
	if w.data.SyncedHeight > height {
		w.data.SyncedHeight = height
	}
	return removed
}

// OutputCount returns total output count
func (w *Wallet) OutputCount() (total, unspent int) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	total = len(w.data.Outputs)
	for _, out := range w.data.Outputs {
		if !out.Spent {
			unspent++
		}
	}
	return
}

// RecordSend stores metadata about an outgoing transaction
func (w *Wallet) RecordSend(record *SendRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data.SendHistory = append(w.data.SendHistory, record)
}

// GetSendRecord retrieves send metadata by TxID, returns nil if not found
func (w *Wallet) GetSendRecord(txID [32]byte) *SendRecord {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for _, record := range w.data.SendHistory {
		if record.TxID == txID {
			return record
		}
	}
	return nil
}

// SendRecords returns a snapshot of the wallet's outgoing transaction records.
// Callers must treat the returned records as read-only.
func (w *Wallet) SendRecords() []*SendRecord {
	w.mu.RLock()
	defer w.mu.RUnlock()

	records := make([]*SendRecord, len(w.data.SendHistory))
	copy(records, w.data.SendHistory)
	return records
}

// ============================================================================
// Encryption helpers (Argon2id + AES-GCM)
// ============================================================================

type kdfParams struct {
	// Version is a monotonically increasing KDF "profile" version.
	// It is stored in the encrypted file header to support migration-aware decrypt.
	Version uint8

	Time    uint32 // iterations
	Memory  uint32 // KiB
	Threads uint8
}

const (
	walletEncMagicV1 = "BLKNTWLT" // 8 bytes

	walletEncFormatVersionV1 uint8 = 1

	walletEncSaltLen = 16
	walletEncKeyLen  = 32

	// Header = magic(8) + formatVer(1) + kdfVer(1) + time(4) + memKiB(4) + threads(1) + reserved(3)
	walletEncHeaderLenV1 = 8 + 1 + 1 + 4 + 4 + 1 + 3
)

var (
	// legacyKDFParams match the original hard-coded settings, used to decrypt old wallets.
	legacyKDFParams = kdfParams{
		Version: 0,
		Time:    3,
		Memory:  64 * 1024, // 64 MiB
		Threads: 4,
	}

	// defaultKDFParams are used for new encryptions (new wallets + on-save migrations).
	// Tuned upward for high-value wallet context.
	defaultKDFParams = kdfParams{
		Version: 1,
		Time:    3,
		Memory:  256 * 1024, // 256 MiB
		Threads: 4,
	}
)

func deriveKeyWithParams(password, salt []byte, p kdfParams) []byte {
	if p.Time == 0 {
		p.Time = legacyKDFParams.Time
	}
	if p.Memory == 0 {
		p.Memory = legacyKDFParams.Memory
	}
	if p.Threads == 0 {
		p.Threads = legacyKDFParams.Threads
	}
	return argon2.IDKey(password, salt, p.Time, p.Memory, p.Threads, walletEncKeyLen)
}

func encrypt(plaintext, password []byte) ([]byte, error) {
	// Generate random salt
	salt := make([]byte, walletEncSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}

	key := deriveKeyWithParams(password, salt, defaultKDFParams)
	defer wipeBytes(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Versioned format:
	// magic(8) || formatVer(1) || kdfVer(1) || time(4) || memKiB(4) || threads(1) || reserved(3) ||
	// salt(16) || nonce || ciphertext
	result := make([]byte, walletEncHeaderLenV1+walletEncSaltLen+gcm.NonceSize()+len(ciphertext))
	off := 0
	copy(result[off:off+8], []byte(walletEncMagicV1))
	off += 8
	result[off] = walletEncFormatVersionV1
	off++
	result[off] = defaultKDFParams.Version
	off++
	binary.BigEndian.PutUint32(result[off:off+4], defaultKDFParams.Time)
	off += 4
	binary.BigEndian.PutUint32(result[off:off+4], defaultKDFParams.Memory)
	off += 4
	result[off] = defaultKDFParams.Threads
	off++
	// reserved (3 bytes)
	off += 3
	copy(result[off:off+walletEncSaltLen], salt)
	off += walletEncSaltLen
	copy(result[off:off+gcm.NonceSize()], nonce)
	off += gcm.NonceSize()
	copy(result[off:], ciphertext)

	return result, nil
}

func decrypt(data, password []byte) ([]byte, error) {
	// New format starts with magic header; legacy format starts with salt.
	if len(data) >= walletEncHeaderLenV1+walletEncSaltLen {
		if string(data[:8]) == walletEncMagicV1 {
			formatVer := data[8]
			if formatVer != walletEncFormatVersionV1 {
				return nil, fmt.Errorf("unsupported wallet encryption format version: %d", formatVer)
			}

			kdfVer := data[9]
			_ = kdfVer // currently informational; we parse explicit params below.

			timeParam := binary.BigEndian.Uint32(data[10:14])
			memKiB := binary.BigEndian.Uint32(data[14:18])
			threads := data[18]

			off := walletEncHeaderLenV1
			if len(data) < off+walletEncSaltLen+12 {
				return nil, errors.New("ciphertext too short")
			}
			salt := data[off : off+walletEncSaltLen]
			off += walletEncSaltLen

			params := kdfParams{
				Version: kdfVer,
				Time:    timeParam,
				Memory:  memKiB,
				Threads: threads,
			}
			key := deriveKeyWithParams(password, salt, params)
			defer wipeBytes(key)

			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, err
			}
			gcm, err := cipher.NewGCM(block)
			if err != nil {
				return nil, err
			}

			nonceSize := gcm.NonceSize()
			if len(data) < off+nonceSize {
				return nil, errors.New("ciphertext too short")
			}
			nonce := data[off : off+nonceSize]
			ciphertext := data[off+nonceSize:]
			return gcm.Open(nil, nonce, ciphertext, nil)
		}
	}

	// Legacy format: salt (16) || nonce (12) || ciphertext; fixed legacy KDF params.
	if len(data) < walletEncSaltLen+12 {
		return nil, errors.New("ciphertext too short")
	}
	salt := data[:walletEncSaltLen]
	key := deriveKeyWithParams(password, salt, legacyKDFParams)
	defer wipeBytes(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < walletEncSaltLen+nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce := data[walletEncSaltLen : walletEncSaltLen+nonceSize]
	ciphertext := data[walletEncSaltLen+nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func unixNow() int64 {
	return time.Now().Unix()
}

func (w *Wallet) readMnemonicFromDisk() (string, error) {
	encrypted, err := os.ReadFile(w.filename)
	if err != nil {
		return "", err
	}

	plaintext, err := decrypt(encrypted, w.password)
	if err != nil {
		return "", err
	}
	defer wipeBytes(plaintext)

	var disk struct {
		ViewOnly  bool   `json:"view_only"`
		Mnemonic  string `json:"mnemonic,omitempty"`
		Version   uint32 `json:"version"`
		CreatedAt int64  `json:"created_at"`
	}
	if err := json.Unmarshal(plaintext, &disk); err != nil {
		return "", err
	}
	if disk.ViewOnly {
		return "", nil
	}
	return disk.Mnemonic, nil
}

// ParseAddress decodes a stealth address into spend and view pubkeys
func ParseAddress(address string) (spendPub, viewPub [32]byte, err error) {
	decoded := base58.Decode(address)

	switch len(decoded) {
	case 64:
		// Legacy (no checksum). Accepted for backward compatibility.
		copy(spendPub[:], decoded[:32])
		copy(viewPub[:], decoded[32:])
		return spendPub, viewPub, nil
	case 68:
		payload := decoded[:64]
		checksum := decoded[64:]
		sum := addressChecksum(payload)
		if checksum[0] != sum[0] || checksum[1] != sum[1] || checksum[2] != sum[2] || checksum[3] != sum[3] {
			return spendPub, viewPub, errors.New("invalid address checksum")
		}
		copy(spendPub[:], payload[:32])
		copy(viewPub[:], payload[32:])
		return spendPub, viewPub, nil
	default:
		return spendPub, viewPub, errors.New("invalid address length")
	}
}

func addressChecksum(payload []byte) [32]byte {
	const tag = "blocknet_stealth_address_checksum"
	b := make([]byte, 0, len(tag)+len(params.NetworkID)+len(payload))
	b = append(b, tag...)
	b = append(b, params.NetworkID...)
	b = append(b, payload...)
	return sha3.Sum256(b)
}

// Package p2p implements privacy-focused peer-to-peer networking
package p2p

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"blocknet/protocol/params"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// IdentityManager handles peer ID generation and rotation
type IdentityManager struct {
	mu sync.RWMutex

	currentKey  crypto.PrivKey
	currentID   peer.ID
	createdAt   time.Time
	rotationAge time.Duration

	// Path to persist identity (empty = ephemeral)
	persistPath string

	// Callback when identity rotates (node needs to restart connections)
	onRotate func(newKey crypto.PrivKey, newID peer.ID)
}

// IdentityConfig configures identity behavior
type IdentityConfig struct {
	// RotationInterval is how often to rotate identity (0 = never)
	// Only applies in ephemeral mode (no BLOCKNET_P2P_KEY env var, no XDG key)
	RotationInterval time.Duration

	// OnRotate is called when identity changes
	OnRotate func(newKey crypto.PrivKey, newID peer.ID)
}

// DefaultIdentityConfig returns sensible defaults
func DefaultIdentityConfig() IdentityConfig {
	return IdentityConfig{
		RotationInterval: 24 * time.Hour,
		OnRotate:         nil,
	}
}

// NewIdentityManager creates a new identity manager.
//
// Identity resolution order:
//  1. BLOCKNET_P2P_KEY env var → load or create key at that path, never rotate
//  2. XDG config dir (e.g. ~/.config/blocknet/mainnet/identity.key) → if exists, load it, never rotate
//  3. Otherwise → ephemeral identity with rotation
func NewIdentityManager(cfg IdentityConfig) (*IdentityManager, error) {
	var key crypto.PrivKey
	var id peer.ID
	var persistPath string
	var rotationAge time.Duration

	// 1. BLOCKNET_P2P_KEY env var — explicit persistent identity
	if envPath := os.Getenv("BLOCKNET_P2P_KEY"); envPath != "" {
		var err error
		key, id, err = loadIdentity(envPath)
		if err != nil {
			// File doesn't exist yet — generate and save
			key, id, err = generateIdentity()
			if err != nil {
				return nil, err
			}
			if err := saveIdentity(envPath, key); err != nil {
				return nil, fmt.Errorf("failed to save identity to BLOCKNET_P2P_KEY path %s: %w", envPath, err)
			}
			// generated and saved
		} else {
			// loaded from env
		}
		persistPath = envPath
		rotationAge = 0
	}

	// 2. XDG config dir — manually placed key file (skipped on testnet to
	//    avoid accidentally loading a mainnet identity).
	if key == nil && !params.IsTestnet {
		if xdgPath, err := defaultIdentityPath(); err == nil {
			if k, i, err := loadIdentity(xdgPath); err == nil {
				key = k
				id = i
				persistPath = xdgPath
				rotationAge = 0
				// loaded from XDG config
			}
		}
	}

	// 3. Ephemeral identity
	if key == nil {
		var err error
		key, id, err = generateIdentity()
		if err != nil {
			return nil, err
		}
		rotationAge = cfg.RotationInterval
		// log.Printf("Using ephemeral identity: %s (rotates every %s)", id.String()[:16]+"...", rotationAge)
	}

	return &IdentityManager{
		currentKey:  key,
		currentID:   id,
		createdAt:   time.Now(),
		rotationAge: rotationAge,
		persistPath: persistPath,
		onRotate:    cfg.OnRotate,
	}, nil
}

var overrideConfigDir string

// SetConfigDir overrides os.UserConfigDir() for identity key paths.
// Call before creating an IdentityManager. Intended for Android where
// the host app provides its own files directory.
func SetConfigDir(dir string) { overrideConfigDir = dir }

func resolveConfigDir() (string, error) {
	if overrideConfigDir != "" {
		return overrideConfigDir, nil
	}
	return os.UserConfigDir()
}

// defaultIdentityPath returns the XDG config path for the identity key
func defaultIdentityPath() (string, error) {
	configDir, err := resolveConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "blocknet", "mainnet", "identity.key"), nil
}

// loadIdentity loads an identity from disk
func loadIdentity(path string) (crypto.PrivKey, peer.ID, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	key, err := crypto.UnmarshalPrivateKey(data)
	if err != nil {
		return nil, "", err
	}

	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, "", err
	}

	return key, id, nil
}

// saveIdentity saves an identity to disk
func saveIdentity(path string, key crypto.PrivKey) error {
	data, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// generateIdentity creates a new Ed25519 keypair for peer identity
func generateIdentity() (crypto.PrivKey, peer.ID, error) {
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, "", err
	}

	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return nil, "", err
	}

	return priv, id, nil
}

// CurrentIdentity returns the current private key and peer ID
func (im *IdentityManager) CurrentIdentity() (crypto.PrivKey, peer.ID) {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.currentKey, im.currentID
}

// CurrentPeerID returns just the current peer ID
func (im *IdentityManager) CurrentPeerID() peer.ID {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.currentID
}

// Age returns how long the current identity has been active
func (im *IdentityManager) Age() time.Duration {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return time.Since(im.createdAt)
}

// ShouldRotate returns true if the identity is older than the rotation interval
func (im *IdentityManager) ShouldRotate() bool {
	if im.rotationAge == 0 {
		return false
	}
	im.mu.RLock()
	defer im.mu.RUnlock()
	return time.Since(im.createdAt) > im.rotationAge
}

// Rotate generates a new identity and notifies the callback
// Does nothing if rotation is disabled (persistent identity)
func (im *IdentityManager) Rotate() (peer.ID, error) {
	if im.rotationAge == 0 {
		return im.CurrentPeerID(), nil
	}

	newKey, newID, err := generateIdentity()
	if err != nil {
		return "", err
	}

	im.mu.Lock()
	// oldID := im.currentID
	im.currentKey = newKey
	im.currentID = newID
	im.createdAt = time.Now()
	callback := im.onRotate
	im.mu.Unlock()

	if callback != nil {
		callback(newKey, newID)
	}

	return newID, nil
}

// StartRotationLoop starts a background goroutine that periodically rotates identity
// Returns a stop function
func (im *IdentityManager) StartRotationLoop() func() {
	if im.rotationAge == 0 {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if im.ShouldRotate() {
					if _, err := im.Rotate(); err != nil {
						log.Printf("identity rotation failed: %v", err)
					}
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

// SetRotationCallback sets the callback for identity rotation
func (im *IdentityManager) SetRotationCallback(cb func(newKey crypto.PrivKey, newID peer.ID)) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.onRotate = cb
}

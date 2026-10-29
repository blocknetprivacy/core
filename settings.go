package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"blocknet/protocol/params"
)

const settingsFilename = "settings.json"

type Settings struct {
	PowCache        bool     `json:"pow_cache"`
	Listen          string   `json:"listen"`
	Seed            bool     `json:"seed"`
	FullSync        bool     `json:"full_sync"`
	SaveCheckpoints bool     `json:"save_checkpoints"`
	ExplorerAddr    string   `json:"explorer_addr"`
	WalletFile      string   `json:"wallet_file"`
	P2PMaxInbound   int      `json:"p2p_max_inbound"`
	P2PMaxOutbound  int      `json:"p2p_max_outbound"`
	P2PWhitelist    []string `json:"p2p_whitelist"`
	ExtraPeers      []string `json:"extra_peers"`
	IdentityKeyPath string   `json:"identity_key_path"`
	ChainCacheCap   int      `json:"chain_cache_cap"`
}

var restartSettings = []string{
	"pow_cache",
	"listen",
	"seed",
	"full_sync",
	"p2p_max_inbound",
	"p2p_max_outbound",
	"extra_peers",
	"identity_key_path",
	"chain_cache_cap",
}

func settingsPath(dataDir string) string {
	return filepath.Join(dataDir, settingsFilename)
}

func defaultSettings() Settings {
	s := Settings{
		PowCache:     true,
		Listen:       "/ip4/0.0.0.0/tcp/28080",
		WalletFile:   DefaultWalletFilename,
		P2PWhitelist: []string{},
		ExtraPeers:   []string{},
	}
	if params.IsTestnet {
		s.Listen = "/ip4/0.0.0.0/tcp/38080"
		s.WalletFile = TestnetWalletFilename
		s.FullSync = true
	}
	return s
}

func loadSettings(dataDir string) (Settings, bool, error) {
	s := defaultSettings()
	data, err := os.ReadFile(settingsPath(dataDir))
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, true, fmt.Errorf("%s: %w", settingsPath(dataDir), err)
	}
	if err := s.validate(); err != nil {
		return s, true, fmt.Errorf("%s: %w", settingsPath(dataDir), err)
	}
	return s, true, nil
}

func saveSettings(dataDir string, s Settings) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := settingsPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Settings) validate() error {
	if _, err := multiaddr.NewMultiaddr(s.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if s.ExplorerAddr != "" {
		if _, _, err := net.SplitHostPort(s.ExplorerAddr); err != nil {
			return fmt.Errorf("explorer_addr: %w", err)
		}
	}
	if strings.TrimSpace(s.WalletFile) == "" {
		return fmt.Errorf("wallet_file must not be empty")
	}
	s.WalletFile = filepath.Clean(s.WalletFile)
	if s.P2PMaxInbound < 0 || s.P2PMaxOutbound < 0 {
		return fmt.Errorf("p2p_max_inbound and p2p_max_outbound must be >= 0")
	}
	if s.ChainCacheCap < 0 {
		return fmt.Errorf("chain_cache_cap must be >= 0")
	}
	if s.P2PWhitelist == nil {
		s.P2PWhitelist = []string{}
	}
	for i, raw := range s.P2PWhitelist {
		id, err := peer.Decode(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("p2p_whitelist[%d]: %w", i, err)
		}
		s.P2PWhitelist[i] = id.String()
	}
	if s.ExtraPeers == nil {
		s.ExtraPeers = []string{}
	}
	for i, raw := range s.ExtraPeers {
		if _, err := peer.AddrInfoFromString(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("extra_peers[%d]: %w", i, err)
		}
		s.ExtraPeers[i] = strings.TrimSpace(raw)
	}
	return nil
}

func settingsJSONNames(a, b Settings) []string {
	var diff []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			diff = append(diff, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
		}
	}
	return diff
}

func pendingRestart(saved, running Settings) []string {
	pending := []string{}
	for _, name := range settingsJSONNames(saved, running) {
		for _, r := range restartSettings {
			if name == r {
				pending = append(pending, name)
			}
		}
	}
	return pending
}

package main

import (
	"encoding/json"
	"net/http"
	"slices"
)

type settingsResponse struct {
	Settings        Settings `json:"settings"`
	RestartRequired []string `json:"restart_required"`
}

func cloneSettings(s Settings) Settings {
	s.P2PWhitelist = slices.Clone(s.P2PWhitelist)
	s.ExtraPeers = slices.Clone(s.ExtraPeers)
	return s
}

func (s *APIServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if s.cli == nil {
		writeError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	s.cli.settingsMu.Lock()
	resp := settingsResponse{Settings: cloneSettings(s.cli.settings), RestartRequired: pendingRestart(s.cli.settings, s.cli.runningSettings)}
	s.cli.settingsMu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (s *APIServer) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	if s.cli == nil {
		writeError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	s.cli.settingsMu.Lock()
	defer s.cli.settingsMu.Unlock()

	cur := s.cli.settings
	next := cloneSettings(cur)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	if err := next.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if next.ExplorerAddr != cur.ExplorerAddr {
		if err := s.daemon.SetExplorerAddr(next.ExplorerAddr); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := saveSettings(s.dataDir, next); err != nil {
		if next.ExplorerAddr != cur.ExplorerAddr {
			_ = s.daemon.SetExplorerAddr(cur.ExplorerAddr)
		}
		writeInternal(w, r, http.StatusInternalServerError, "failed to save settings", err)
		return
	}

	if !slices.Equal(next.P2PWhitelist, cur.P2PWhitelist) {
		if node := s.daemon.Node(); node != nil {
			_ = node.SetBanWhitelist(next.P2PWhitelist)
		}
	}
	s.daemon.SetSaveCheckpoints(next.SaveCheckpoints)
	s.cli.setWalletFile(next.WalletFile)

	s.cli.settings = next
	writeJSON(w, http.StatusOK, settingsResponse{Settings: cloneSettings(next), RestartRequired: pendingRestart(next, s.cli.runningSettings)})
}

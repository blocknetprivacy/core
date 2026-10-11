package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkUpgradeNoticeInStatusAndExplorer(t *testing.T) {
	s, d, _ := newSettingsTestServer(t)

	rr := httptest.NewRecorder()
	s.handleStatus(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status struct {
		NetworkUpgrade networkUpgradeInfo `json:"network_upgrade"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.NetworkUpgrade != currentNetworkUpgrade() || !strings.Contains(status.NetworkUpgrade.Notice, "2028-02-16") {
		t.Fatalf("status network_upgrade = %+v", status.NetworkUpgrade)
	}

	e := NewExplorer(d)
	for _, path := range []string{"/", "/stats", "/block/0"} {
		rr := httptest.NewRecorder()
		e.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if !strings.Contains(rr.Body.String(), networkUpgradeNotice) {
			t.Fatalf("%s: explorer page has no upgrade notice (status %d)", path, rr.Code)
		}
	}
}

package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serveTestCheckpoints(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := checkpointsSource
	checkpointsSource = srv.URL
	t.Cleanup(func() { checkpointsSource = old })
}

func TestRefreshCheckpointsFile(t *testing.T) {
	a, b, c := strings.Repeat("A", 64), strings.Repeat("B", 64), strings.Repeat("C", 64)
	remote := "100:" + a + "\n200:" + b + "\n"

	cases := []struct {
		name     string
		local    *string
		status   int
		replaced bool
		wantErr  bool
	}{
		{"stale entry", ptr("100:" + a + "\n200:" + c + "\n"), http.StatusOK, true, false},
		{"missing entry", ptr("100:" + a + "\n"), http.StatusOK, true, false},
		{"same file", ptr(remote), http.StatusOK, false, false},
		{"local ahead", ptr(remote + "300:" + c + "\n"), http.StatusOK, false, false},
		{"no local file", nil, http.StatusOK, true, false},
		{"server error", ptr("100:" + a + "\n200:" + c + "\n"), http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serveTestCheckpoints(t, tc.status, remote)
			path := filepath.Join(t.TempDir(), checkpointsFilename)
			if tc.local != nil {
				if err := os.WriteFile(path, []byte(*tc.local), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res, err := refreshCheckpointsFile(context.Background(), path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if res.replaced != tc.replaced {
				t.Fatalf("replaced = %v, want %v", res.replaced, tc.replaced)
			}

			got, _ := os.ReadFile(path)
			backup, backupErr := os.ReadFile(path + ".bak")
			switch {
			case tc.replaced:
				if string(got) != remote {
					t.Fatalf("file not replaced: %q", got)
				}
				if res.maxHeight != 200 || len(res.checkpoints) != 2 {
					t.Fatalf("unexpected result %+v", res)
				}
				if tc.local != nil && string(backup) != *tc.local {
					t.Fatalf("backup does not hold the old file: %q", backup)
				}
				if tc.local == nil && backupErr == nil {
					t.Fatal("expected no backup when there was no local file")
				}
			case tc.local != nil:
				if string(got) != *tc.local {
					t.Fatalf("local file changed: %q", got)
				}
				if backupErr == nil {
					t.Fatal("expected no backup when nothing was replaced")
				}
			}
		})
	}
}

func TestDaemonRefreshCheckpointsLoadsIntoChain(t *testing.T) {
	good := strings.Repeat("A", 64)
	bad := strings.Repeat("B", 64)
	serveTestCheckpoints(t, http.StatusOK, "63800:"+good+"\n")

	chain, err := NewChain(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	t.Cleanup(func() { _ = chain.Close() })
	path := filepath.Join(t.TempDir(), checkpointsFilename)
	if err := os.WriteFile(path, []byte("63800:"+bad+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cps, _, maxH, err := loadCheckpointsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	chain.SetTrustedCheckpoints(cps, maxH)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{chain: chain, checkpointsFile: path, ctx: ctx}
	d.refreshCheckpoints()

	snap := chain.checkpointsSnap.Load()
	if snap == nil {
		t.Fatal("chain has no checkpoints")
	}
	got := (*snap)[63800]
	if !strings.EqualFold(hex.EncodeToString(got[:]), good) {
		t.Fatalf("chain checkpoint at 63800 = %x, want %s", got, good)
	}
}

func TestDaemonRefreshCheckpointsSkipsWhenSavingCheckpoints(t *testing.T) {
	serveTestCheckpoints(t, http.StatusOK, "63800:"+strings.Repeat("A", 64)+"\n")
	path := filepath.Join(t.TempDir(), checkpointsFilename)
	local := "63800:" + strings.Repeat("B", 64) + "\n"
	if err := os.WriteFile(path, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{checkpointsFile: path, ctx: ctx}
	d.saveCheckpoints.Store(true)
	d.refreshCheckpoints()

	if got, _ := os.ReadFile(path); string(got) != local {
		t.Fatalf("a node that writes its own checkpoints must keep its file, got %q", got)
	}
}

func ptr(s string) *string { return &s }

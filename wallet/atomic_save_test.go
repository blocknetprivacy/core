package wallet

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestAtomicWalletSaveReadersSeeCompleteFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix atomic rename/read semantics")
	}
	path := filepath.Join(t.TempDir(), "wallet.dat")
	old := bytes.Repeat([]byte{0x11}, 128*1024)
	next := bytes.Repeat([]byte{0x22}, 256*1024)
	if err := writeWalletAtomic(path, old); err != nil {
		t.Fatal(err)
	}
	// An open reader must retain the old snapshot when the pathname is replaced.
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil || (!bytes.Equal(got, old) && !bytes.Equal(got, next)) {
				t.Errorf("reader observed partial wallet: bytes=%d error=%v", len(got), err)
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		data := next
		if i%2 != 0 {
			data = old
		}
		if err := writeWalletAtomic(path, data); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	wg.Wait()
	heldBytes := make([]byte, len(old))
	if n, err := held.Read(heldBytes); err != nil || n != len(old) || !bytes.Equal(heldBytes, old) {
		t.Fatalf("old reader was modified: bytes=%d err=%v", n, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("wallet permissions: %o", info.Mode().Perm())
	}
	assertNoWalletTemps(t, filepath.Dir(path))
}

func TestAtomicWalletSaveFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "wallet.dat")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(destination, "existing")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeWalletAtomic(destination, []byte("new")); err == nil {
		t.Fatal("expected rename failure")
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "preserve" {
		t.Fatalf("destination changed: %q %v", got, err)
	}
	assertNoWalletTemps(t, dir)
}

func assertNoWalletTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".wallet-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v %v", matches, err)
	}
}

package wallet

import (
	"os"
	"path/filepath"
	"runtime"
)

// Replace a complete file instead of truncating the live wallet. On Unix, sync
// the containing directory too so the rename survives a machine crash.
func writeWalletAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".wallet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

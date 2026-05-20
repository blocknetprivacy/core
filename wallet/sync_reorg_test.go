package wallet

import "testing"

func TestWalletRewindRestoresSyncedBlockHash(t *testing.T) {
	w := &Wallet{}
	hash1 := [32]byte{0x01}
	hash2 := [32]byte{0x02}

	w.SetSyncedBlock(1, hash1)
	w.SetSyncedBlock(2, hash2)

	w.RewindToHeight(1)

	height, hash := w.SyncedBlock()
	if height != 1 {
		t.Fatalf("height=%d, want 1", height)
	}
	if hash != hash1 {
		t.Fatalf("hash=%x, want %x", hash[:], hash1[:])
	}
}

func TestWalletSetSyncedHeightClearsUnknownHash(t *testing.T) {
	w := &Wallet{}
	hash := [32]byte{0x01}

	w.SetSyncedBlock(1, hash)
	w.SetSyncedHeight(1)

	_, got := w.SyncedBlock()
	if got != ([32]byte{}) {
		t.Fatalf("hash=%x, want zero hash", got[:])
	}
}

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

func TestWalletSyncedBlockHistoryIsBounded(t *testing.T) {
	w := &Wallet{}
	for height := uint64(1); height <= syncedBlockHistoryLimit+20; height++ {
		w.SetSyncedBlock(height, [32]byte{byte(height), 0x01})
	}
	if got := len(w.data.SyncedBlocks); got != int(syncedBlockHistoryLimit) {
		t.Fatalf("history length=%d, want %d", got, syncedBlockHistoryLimit)
	}
	if got := w.data.SyncedBlocks[0].Height; got != 21 {
		t.Fatalf("oldest retained height=%d, want 21", got)
	}
	w.RewindToHeight(21)
	if height, hash := w.SyncedBlock(); height != 21 || hash != ([32]byte{21, 0x01}) {
		t.Fatalf("rewind lost retained ancestor: height=%d hash=%x", height, hash)
	}
	w.RewindToHeight(20)
	if _, hash := w.SyncedBlock(); hash != ([32]byte{}) {
		t.Fatalf("pruned ancestor must be unknown: hash=%x", hash)
	}
}

package main

import (
	"crypto/sha3"
	"sync"
	"testing"
)

func TestTxIDMatchesCanonicalSerializationUnderConcurrency(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	want := sha3.Sum256(tx.Serialize())

	const goroutines = 8
	got := make([][32]byte, goroutines)

	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := tx.TxID()
			if err != nil {
				t.Errorf("TxID failed: %v", err)
				return
			}
			got[i] = id
		}(i)
	}
	wg.Wait()

	for i, id := range got {
		if id != want {
			t.Fatalf("goroutine %d returned %x, want canonical serialization hash %x", i, id[:8], want[:8])
		}
	}
}

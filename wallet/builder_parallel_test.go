package wallet

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestBuilderForEachReturnsLowestIndexedError(t *testing.T) {
	for _, parallelism := range []int{0, 1, 4} {
		b := &Builder{config: TransferConfig{Parallelism: parallelism}}
		var calls atomic.Int32
		err := b.forEach(16, func(i int) error {
			calls.Add(1)
			if i == 3 || i == 9 {
				return fmt.Errorf("item %d", i)
			}
			return nil
		})
		if err == nil || err.Error() != "item 3" {
			t.Fatalf("parallelism %d: got %v, want item 3", parallelism, err)
		}
		if parallelism <= 1 && calls.Load() != 4 {
			t.Fatalf("one at a time must stop at the first error, ran %d", calls.Load())
		}
	}
}

func TestBuilderForEachRunsEveryItem(t *testing.T) {
	b := &Builder{config: TransferConfig{Parallelism: 8}}
	var seen [100]atomic.Int32
	if err := b.forEach(len(seen), func(i int) error { seen[i].Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		if seen[i].Load() != 1 {
			t.Fatalf("item %d ran %d times", i, seen[i].Load())
		}
	}
	if err := b.forEach(0, func(int) error { return errors.New("unexpected") }); err != nil {
		t.Fatal(err)
	}
}

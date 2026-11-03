package main

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// forEachParallel calls fn(i) for every i in [0, n) on up to GOMAXPROCS
// goroutines and returns once every call has finished. fn must be safe to call
// concurrently; it runs inline when there is only one item or one CPU.
func forEachParallel(n int, fn func(i int)) {
	workers := min(n, runtime.GOMAXPROCS(0))
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// firstError returns the lowest-indexed non-nil error, so a parallel check
// reports the same error a sequential one would.
func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// parallelHelpers bounds the goroutines forEachParallel adds across the whole
// process. Its callers can be many at once (one per peer stream), and the work
// is often a cgo call, which holds an OS thread; without a shared bound, threads
// would multiply with the number of callers.
var parallelHelpers = make(chan struct{}, runtime.GOMAXPROCS(0))

// forEachParallel calls fn(i) for every i in [0, n) and returns once every call
// has finished. The calling goroutine always works through the items, joined by
// up to GOMAXPROCS-1 helpers when the process-wide helper pool has room, so it
// never runs slower than a plain loop. fn must be safe to call concurrently.
func forEachParallel(n int, fn func(i int)) {
	var next atomic.Int64
	work := func() {
		for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
			fn(i)
		}
	}
	var wg sync.WaitGroup
helpers:
	for range min(n, runtime.GOMAXPROCS(0)) - 1 {
		select {
		case parallelHelpers <- struct{}{}:
			wg.Add(1)
			go func() {
				defer func() {
					<-parallelHelpers
					wg.Done()
				}()
				work()
			}()
		default:
			break helpers
		}
	}
	work()
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

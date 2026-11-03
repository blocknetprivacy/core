package main

import "sync"

// maxPrebuiltCoinbases bounds the reward addresses with a coinbase built ahead.
const maxPrebuiltCoinbases = 4

type coinbaseKey struct {
	spendPub, viewPub [32]byte
	height            uint64
}

type coinbasePrebuild struct {
	done     chan struct{}
	coinbase *CoinbaseResult
	err      error
}

// templateCoinbases builds the coinbase for the next height in the background
// once a template is served, so the first template after a new block doesn't
// wait for the coinbase range proof. Each prebuilt coinbase is used once.
type templateCoinbases struct {
	mu      sync.Mutex
	pending map[coinbaseKey]*coinbasePrebuild
}

// take returns the prebuilt coinbase for key, waiting for one still being
// built, or builds a new one.
func (tc *templateCoinbases) take(key coinbaseKey) (*CoinbaseResult, error) {
	tc.mu.Lock()
	p := tc.pending[key]
	delete(tc.pending, key)
	tc.mu.Unlock()
	if p != nil {
		<-p.done
		if p.err == nil {
			return p.coinbase, nil
		}
	}
	return CreateCoinbase(key.spendPub, key.viewPub, GetBlockReward(key.height), key.height)
}

// prebuild starts building the coinbase for key unless one is pending already.
// Entries more than one height away from key are dropped.
func (tc *templateCoinbases) prebuild(key coinbaseKey) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.pending == nil {
		tc.pending = make(map[coinbaseKey]*coinbasePrebuild)
	}
	if _, ok := tc.pending[key]; ok {
		return
	}
	for k := range tc.pending {
		if k.height+1 < key.height || k.height > key.height {
			delete(tc.pending, k)
		}
	}
	if len(tc.pending) >= maxPrebuiltCoinbases {
		return
	}
	p := &coinbasePrebuild{done: make(chan struct{})}
	tc.pending[key] = p
	go func() {
		defer close(p.done)
		p.coinbase, p.err = CreateCoinbase(key.spendPub, key.viewPub, GetBlockReward(key.height), key.height)
	}()
}

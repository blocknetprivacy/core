package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"blocknet/protocol/params"
)

// explorerMapBlocks is how many recent blocks the index chain map shows, one
// cell per block.
const explorerMapBlocks = 1000

// explorerMapCache holds the chain map JSON for one tip. It is rebuilt only
// when the tip hash changes, so index requests between blocks cost nothing.
type explorerMapCache struct {
	mu   sync.Mutex
	tip  [32]byte
	json template.JS
}

// chainMapJSON returns [[height, timestamp, txCount, difficulty, fees], ...]
// for the last explorerMapBlocks blocks, oldest first.
func (e *Explorer) chainMapJSON() template.JS {
	chain := e.daemon.chain
	height := chain.HeightFast()
	tip := chain.GetBlockByHeightFast(height)
	if tip == nil {
		return template.JS("[]")
	}
	tipHash := tip.Hash()

	e.mapCache.mu.Lock()
	defer e.mapCache.mu.Unlock()
	if e.mapCache.json != "" && e.mapCache.tip == tipHash {
		return e.mapCache.json
	}

	var start uint64
	if height+1 > explorerMapBlocks {
		start = height + 1 - explorerMapBlocks
	}
	rows := make([][5]uint64, 0, height-start+1)
	for h := start; h <= height; h++ {
		b := chain.GetBlockByHeightFast(h)
		if b == nil {
			continue
		}
		rows = append(rows, [5]uint64{h, uint64(b.Header.Timestamp), uint64(len(b.Transactions)), b.Header.Difficulty, blockFees(b)})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return template.JS("[]")
	}
	e.mapCache.tip = tipHash
	e.mapCache.json = template.JS(data)
	return e.mapCache.json
}

// blockFees sums the fees of every non-coinbase transaction in a block.
func blockFees(b *Block) uint64 {
	var fees uint64
	for i, tx := range b.Transactions {
		if i == 0 && b.Header.Height > 0 {
			continue
		}
		fees += tx.Fee
	}
	return fees
}

type explorerTxRow struct {
	Hash        string
	BlockHeight uint64
	Inputs      int
	Outputs     int
	Fee         float64
	Ts          int64
}

// latestTransfers returns up to limit non-coinbase transactions from the most
// recent blocks, newest first. It stops after a bounded number of blocks so a
// quiet chain cannot turn one index request into a full chain walk.
func (e *Explorer) latestTransfers(limit int) []explorerTxRow {
	chain := e.daemon.chain
	height := chain.HeightFast()
	var out []explorerTxRow
	for h, seen := height, 0; seen < 200 && len(out) < limit; h, seen = h-1, seen+1 {
		b := chain.GetBlockByHeightFast(h)
		if b != nil {
			for i := len(b.Transactions) - 1; i >= 0 && len(out) < limit; i-- {
				tx := b.Transactions[i]
				if len(tx.Inputs) == 0 {
					continue
				}
				id, _ := tx.TxID()
				out = append(out, explorerTxRow{
					Hash:        fmt.Sprintf("%x", id[:]),
					BlockHeight: h,
					Inputs:      len(tx.Inputs),
					Outputs:     len(tx.Outputs),
					Fee:         float64(tx.Fee) / 100_000_000,
					Ts:          b.Header.Timestamp,
				})
			}
		}
		if h == 0 {
			break
		}
	}
	return out
}

// handleBlockData serves the small JSON the index block panel needs when a
// cell is clicked: header fields and the transaction list.
// GET /blockdata/{height}
func (e *Explorer) handleBlockData(w http.ResponseWriter, r *http.Request) {
	height, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/blockdata/"), 10, 64)
	if err != nil {
		http.Error(w, "bad height", http.StatusBadRequest)
		return
	}
	chain := e.daemon.chain
	b := chain.GetBlockByHeightFast(height)
	if b == nil {
		http.Error(w, "block not found", http.StatusNotFound)
		return
	}
	hash := b.Hash()
	var blockTime int64
	if height > 0 {
		if prev := chain.GetBlockByHeightFast(height - 1); prev != nil {
			blockTime = b.Header.Timestamp - prev.Header.Timestamp
		}
	}
	txs := make([][5]any, 0, len(b.Transactions))
	ins, outs := 0, 0
	for i, tx := range b.Transactions {
		id, _ := tx.TxID()
		coinbase := i == 0 && height > 0
		txs = append(txs, [5]any{fmt.Sprintf("%x", id[:]), len(tx.Inputs), len(tx.Outputs), tx.Fee, coinbase})
		ins += len(tx.Inputs)
		outs += len(tx.Outputs)
	}
	chainHeight := chain.HeightFast()
	setImmutableCacheIfFinal(w, height, chainHeight)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"height":        height,
		"hash":          fmt.Sprintf("%x", hash[:]),
		"timestamp":     b.Header.Timestamp,
		"block_time":    blockTime,
		"difficulty":    b.Header.Difficulty,
		"nonce":         b.Header.Nonce,
		"reward":        GetBlockReward(height),
		"fees":          blockFees(b),
		"inputs":        ins,
		"outputs":       outs,
		"confirmations": chainHeight - height + 1,
		"txs":           txs,
	})
}

// renderNotFound shows the styled not-found page with a 404 status.
func renderNotFound(w http.ResponseWriter, what, why string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	explorerNotFoundPage.Execute(w, map[string]any{
		"IsTestnet": params.IsTestnet,
		"What":      what,
		"Why":       why,
	})
}

package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"sync"

	bolt "go.etcd.io/bbolt"
)

// canonicalOutputProbe asks where an output sits in the canonical chain.
type canonicalOutputProbe struct {
	TxID   [32]byte
	Height uint64
	Index  int
}

// canonicalOutputProbeResult describes the canonical block at Height and,
// when that block holds TxID, the output at Index.
type canonicalOutputProbeResult struct {
	BlockFound  bool
	BlockHash   [32]byte
	TxFound     bool
	OutputCount int
	PublicKey   [32]byte
	Commitment  [32]byte
}

// auditBlockOutputs decodes only output keys: the wallet audit needs nothing
// else, and skipping inputs, signatures and range proofs is several times
// faster than decoding a full Block.
type auditOutput struct {
	Commitment [32]byte `json:"commitment"`
	PublicKey  [32]byte `json:"public_key"`
}
type auditTransactionOutputs struct {
	Outputs []auditOutput `json:"outputs"`
}
type auditBlockOutputs struct {
	Transactions []auditTransactionOutputs `json:"transactions"`
}

// ProbeCanonicalOutputs answers probes from the height and transaction
// location indexes, decoding each referenced block once. The transaction's
// position comes from tx_locations_v1, which records the first transaction
// with that ID in the block, so results match scanning the block for it.
func (s *Storage) ProbeCanonicalOutputs(probes []canonicalOutputProbe) ([]canonicalOutputProbeResult, error) {
	results := make([]canonicalOutputProbeResult, len(probes))
	if len(probes) == 0 {
		return results, nil
	}
	order := make([]int, len(probes))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return probes[order[a]].Height < probes[order[b]].Height })

	// Split by height so each worker decodes disjoint blocks: every cut is
	// moved forward to the next height change.
	workers := min(runtime.GOMAXPROCS(0), 8)
	chunk := (len(order) + workers - 1) / workers
	cuts := []int{0}
	for cut := chunk; cut < len(order); cut += chunk {
		cut = max(cut, cuts[len(cuts)-1]+1)
		for cut < len(order) && probes[order[cut]].Height == probes[order[cut-1]].Height {
			cut++
		}
		if cut < len(order) {
			cuts = append(cuts, cut)
		}
	}
	cuts = append(cuts, len(order))

	var wg sync.WaitGroup
	errs := make([]error, len(cuts)-1)
	for w := 0; w+1 < len(cuts); w++ {
		wg.Add(1)
		go func(w int, part []int) {
			defer wg.Done()
			errs[w] = s.db.View(func(tx *bolt.Tx) error {
				return probeCanonicalOutputsLocked(tx, probes, part, results)
			})
		}(w, order[cuts[w]:cuts[w+1]])
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func probeCanonicalOutputsLocked(tx *bolt.Tx, probes []canonicalOutputProbe, order []int, results []canonicalOutputProbeResult) error {
	_, tipHeight, _, err := readTipMeta(tx.Bucket(bucketMeta))
	if err != nil {
		return err
	}
	heights := tx.Bucket(bucketHeights)
	blocks := tx.Bucket(bucketBlocks)
	locations := tx.Bucket(bucketTxLocations)

	var decodedHeight uint64
	var decoded *auditBlockOutputs
	for _, i := range order {
		probe := probes[i]
		result := &results[i]
		hashBytes := heights.Get(heightKey(probe.Height))
		if len(hashBytes) != 32 || probe.Height > tipHeight || blocks.Get(hashBytes) == nil {
			continue
		}
		result.BlockFound = true
		copy(result.BlockHash[:], hashBytes)

		var key [64]byte
		copy(key[:32], probe.TxID[:])
		copy(key[32:], hashBytes)
		location := locations.Get(key[:])
		if len(location) != 12 {
			continue
		}
		position := int(uint32(location[8])<<24 | uint32(location[9])<<16 | uint32(location[10])<<8 | uint32(location[11]))

		if decoded == nil || decodedHeight != probe.Height {
			decoded = &auditBlockOutputs{}
			data := blocks.Get(hashBytes)
			if len(data) > 0 && data[0] == blockFormatBinaryV1 {
				block, err := decodeBlock(data)
				if err != nil {
					return fmt.Errorf("decode block at height %d: %w", probe.Height, err)
				}
				for _, txn := range block.Transactions {
					entry := auditTransactionOutputs{}
					for _, out := range txn.Outputs {
						entry.Outputs = append(entry.Outputs, auditOutput{Commitment: out.Commitment, PublicKey: out.PublicKey})
					}
					decoded.Transactions = append(decoded.Transactions, entry)
				}
			} else if err := json.Unmarshal(data, decoded); err != nil {
				return fmt.Errorf("decode block at height %d: %w", probe.Height, err)
			}
			decodedHeight = probe.Height
		}
		if position >= len(decoded.Transactions) {
			return fmt.Errorf("transaction location outside block at height %d", probe.Height)
		}
		outputs := decoded.Transactions[position].Outputs
		result.TxFound = true
		result.OutputCount = len(outputs)
		if probe.Index >= 0 && probe.Index < len(outputs) {
			result.PublicKey = outputs[probe.Index].PublicKey
			result.Commitment = outputs[probe.Index].Commitment
		}
	}
	return nil
}

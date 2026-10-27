package main

import "blocknet/wallet"

func walletSyncHashKnown(hash [32]byte) bool {
	return hash != ([32]byte{})
}

func rewindWalletToCanonicalTip(w *wallet.Wallet, chain *Chain) int {
	if w == nil || chain == nil {
		return 0
	}

	removed := 0
	forkDetected := false
	for {
		chainHeight := chain.Height()
		walletHeight, walletHash := w.SyncedBlock()
		if walletHeight == 0 {
			return removed
		}
		if walletHeight > chainHeight {
			removed += w.RewindToHeight(chainHeight)
			continue
		}
		if !walletSyncHashKnown(walletHash) {
			if forkDetected {
				// Older wallets only persisted their tip hash. If history runs
				// out during a reorg, rescan from the finalized floor rather than
				// treating an unknown ancestor as proof of canonical membership.
				floor := uint64(0)
				if chainHeight > MaxReorgDepth {
					floor = chainHeight - MaxReorgDepth
				}
				if walletHeight > floor {
					removed += w.RewindToHeight(floor)
				}
			}
			return removed
		}

		block := chain.GetBlockByHeight(walletHeight)
		if block == nil {
			forkDetected = true
			removed += w.RewindToHeight(walletHeight - 1)
			continue
		}
		if block.Hash() == walletHash {
			return removed
		}
		forkDetected = true
		removed += w.RewindToHeight(walletHeight - 1)
	}
}

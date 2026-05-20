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
			return removed
		}

		block := chain.GetBlockByHeight(walletHeight)
		if block == nil {
			removed += w.RewindToHeight(walletHeight - 1)
			continue
		}
		if block.Hash() == walletHash {
			return removed
		}
		removed += w.RewindToHeight(walletHeight - 1)
	}
}

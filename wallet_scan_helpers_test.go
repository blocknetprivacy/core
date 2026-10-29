package main

import (
	"blocknet/protocol/params"
	"testing"
)

func walletScanAppendBlock(t *testing.T, chain *Chain, txs ...*Transaction) *Block {
	t.Helper()
	prev := chain.GetBlockByHeight(chain.Height())
	height := chain.Height() + 1
	block := &Block{Header: BlockHeader{Version: 1, Height: height, PrevHash: prev.Hash(), Timestamp: prev.Header.Timestamp + BlockIntervalSec, Difficulty: MinDifficulty}, Transactions: txs}
	var err error
	if block.Header.MerkleRoot, err = block.ComputeMerkleRoot(); err != nil {
		t.Fatal(err)
	}
	var spent [][32]byte
	for _, tx := range txs {
		for _, in := range tx.Inputs {
			spent = append(spent, in.KeyImage)
		}
	}
	if err := chain.Storage().CommitBlock(&BlockCommit{Block: block, Height: height, Hash: block.Hash(), Work: height + 1, IsMainTip: true, NewOutputs: blockOutputsForCommit(t, block), SpentKeyImgs: spent}); err != nil {
		t.Fatal(err)
	}
	chain.mu.Lock()
	chain.blocks[block.Hash()] = block
	chain.workAt[block.Hash()] = height + 1
	chain.byHeight[height] = block.Hash()
	chain.bestHash = block.Hash()
	chain.height = height
	chain.totalWork = height + 1
	chain.canonicalRingIndexDirty = true
	chain.publishTipLocked()
	chain.mu.Unlock()
	return block
}

func walletScanTestnet(t *testing.T) {
	t.Helper()
	testnet, network, chainID, indexed, memo := params.IsTestnet, params.NetworkID, params.ChainID, params.IndexedDerivationHeight, params.MemoBlockDomainSep
	base, pex, block, tx, sync, dan := params.P2PProtocolBase, params.ProtocolPEX, params.ProtocolBlock, params.ProtocolTx, params.ProtocolSync, params.ProtocolDandelion
	t.Cleanup(func() {
		params.IsTestnet, params.NetworkID, params.ChainID, params.IndexedDerivationHeight, params.MemoBlockDomainSep = testnet, network, chainID, indexed, memo
		params.P2PProtocolBase, params.ProtocolPEX, params.ProtocolBlock, params.ProtocolTx, params.ProtocolSync, params.ProtocolDandelion = base, pex, block, tx, sync, dan
	})
	params.InitTestnet()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

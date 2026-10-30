package main

import (
	"errors"

	"blocknet/protocol/params"
)

var errBlockFeesOverflow = errors.New("block fees overflow")

func upgradeActive(height, activation uint64) bool {
	return activation > 0 && height >= activation
}

func coinbaseClaimsFees(height uint64) bool {
	return upgradeActive(height, params.FaradayHeight)
}

func sumTxFees(txs []*Transaction) (uint64, error) {
	var total uint64
	for _, tx := range txs {
		if tx == nil {
			continue
		}
		if total+tx.Fee < total {
			return 0, errBlockFeesOverflow
		}
		total += tx.Fee
	}
	return total, nil
}

func coinbaseAmount(height uint64, fees uint64) (uint64, error) {
	reward := GetBlockReward(height)
	if !coinbaseClaimsFees(height) {
		return reward, nil
	}
	if reward+fees < reward {
		return 0, errBlockFeesOverflow
	}
	return reward + fees, nil
}

func createBlockCoinbase(spendPub, viewPub [32]byte, height uint64, txs []*Transaction) (*CoinbaseResult, error) {
	fees, err := sumTxFees(txs)
	if err != nil {
		return nil, err
	}
	amount, err := coinbaseAmount(height, fees)
	if err != nil {
		return nil, err
	}
	return CreateCoinbase(spendPub, viewPub, amount, height)
}

func coinbaseReward(b *Block) uint64 {
	amount, err := blockCoinbaseAmount(b)
	if err != nil {
		return GetBlockReward(b.Header.Height)
	}
	return amount
}

func blockCoinbaseAmount(b *Block) (uint64, error) {
	if b == nil || len(b.Transactions) == 0 {
		return GetBlockReward(0), nil
	}
	fees, err := sumTxFees(b.Transactions[1:])
	if err != nil {
		return 0, err
	}
	return coinbaseAmount(b.Header.Height, fees)
}

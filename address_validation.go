package main

import (
	"errors"

	"blocknet/wallet"
)

func parseValidatedAddress(address string) (spendPub, viewPub [32]byte, err error) {
	spendPub, viewPub, err = wallet.ParseAddress(address)
	if err != nil {
		return spendPub, viewPub, err
	}
	if !ValidateRistrettoPublicKey(spendPub[:]) {
		return spendPub, viewPub, errors.New("invalid address spend public key")
	}
	if !ValidateRistrettoPublicKey(viewPub[:]) {
		return spendPub, viewPub, errors.New("invalid address view public key")
	}
	return spendPub, viewPub, nil
}

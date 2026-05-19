package main

import (
	"strings"
	"testing"

	"blocknet/wallet"
)

const checksumValidInvalidRistrettoAddress = "S7YPHt98NDKrUNmFaHa9GQu4XJvRPkTR51bxdE4122UFxfB4cqdFP5R2pkJSrNTQGwmFVmKzKodu7F8XmHjTTx9PNx3i"

func TestParseValidatedAddressRejectsChecksumValidInvalidRistrettoKeys(t *testing.T) {
	_, _, err := parseValidatedAddress(checksumValidInvalidRistrettoAddress)
	if err == nil {
		t.Fatal("expected invalid Ristretto public key to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid address spend public key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseValidatedAddressAcceptsGeneratedStealthAddress(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}

	addr := (&wallet.StealthKeys{
		SpendPubKey: keys.SpendPubKey,
		ViewPubKey:  keys.ViewPubKey,
	}).Address()

	spendPub, viewPub, err := parseValidatedAddress(addr)
	if err != nil {
		t.Fatalf("parseValidatedAddress: %v", err)
	}
	if spendPub != keys.SpendPubKey || viewPub != keys.ViewPubKey {
		t.Fatal("parsed address public keys do not match generated keys")
	}
}

package main

import (
	"strings"
	"testing"
)

func TestExplorerSentProofKeyLabel(t *testing.T) {
	start := strings.Index(explorerTxTmpl, `<div data-p="s"`)
	end := strings.Index(explorerTxTmpl, `<div id="prove-result"`)
	if start < 0 || end <= start {
		t.Fatal("sent-proof form not found")
	}
	form := explorerTxTmpl[start:end]
	for _, want := range []string{
		"transaction private key (tx_key from prove)",
		`id="send-txpriv" placeholder="transaction private key (64 hex)"`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("sent-proof form missing %q", want)
		}
	}
	if strings.Contains(form, "view private key") {
		t.Fatal("sent-proof form must not request the wallet view private key")
	}
	// Receiver proof still needs the view key and must retain its warning.
	if !strings.Contains(explorerTxTmpl, "Your view private key is sent to this server.") {
		t.Fatal("receiver proof privacy warning missing")
	}
}

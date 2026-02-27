package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSendArgsFromPaymentLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantArgs  []string
		wantMatch bool
		wantErr   string
	}{
		{
			name:      "blocknet uri with memo",
			in:        "blocknet://Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F?amount=1.23&memo=abcd-1234-bcde-2345",
			wantArgs:  []string{"Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F", "1.23", "abcd-1234-bcde-2345"},
			wantMatch: true,
		},
		{
			name:      "blocknet uri without memo",
			in:        "blocknet://Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F?amount=1",
			wantArgs:  []string{"Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F", "1"},
			wantMatch: true,
		},
		{
			name:      "bntpay bare link",
			in:        "bntpay.com/Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F?amount=100&memo=invoice-42",
			wantArgs:  []string{"Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F", "100", "invoice-42"},
			wantMatch: true,
		},
		{
			name:      "bntpay handle link",
			in:        "https://bntpay.com/$rock?amount=100&memo=thank%20you",
			wantArgs:  []string{"$rock", "100", "thank you"},
			wantMatch: true,
		},
		{
			name:      "bntpay https link and decoded memo",
			in:        "https://bntpay.com/Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F?amount=0.5&memo=invoice%2042",
			wantArgs:  []string{"Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F", "0.5", "invoice 42"},
			wantMatch: true,
		},
		{
			name:      "payment link missing amount",
			in:        "blocknet://Y19BMGFbSdKRzHPoXG7Sreqf7x7u8x5GTpHFpzfkAYT58sdD6SFZnj9Q1GGHrJogaeV1pf9YrVMEWW2xncuHGy9eG2U4F?memo=x",
			wantMatch: true,
			wantErr:   "missing amount",
		},
		{
			name:      "non payment input",
			in:        "status",
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotArgs, matched, err := parseSendArgsFromPaymentLink(tt.in)
			if matched != tt.wantMatch {
				t.Fatalf("matched=%v want %v", matched, tt.wantMatch)
			}

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("args=%v want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}

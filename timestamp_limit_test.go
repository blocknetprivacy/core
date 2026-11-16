package main

import (
	"testing"
	"time"
)

func TestMaxBlockTimestampTightensAtFaraday(t *testing.T) {
	setFaradayHeight(t, 100)
	const now = int64(2_000_000_000)
	before := now + int64(TimestampFuturLimit/time.Second)
	after := now + int64(FaradayTimestampFutureLimit/time.Second)
	cases := []struct {
		name   string
		height uint64
		parent int64
		want   int64
	}{
		{"before Faraday", 99, now, before},
		{"before Faraday, parent ahead", 99, now + 3600, before},
		{"from Faraday", 100, now, after},
		{"from Faraday, parent within the limit", 101, after, after + 1},
		{"from Faraday, parent ahead", 101, now + 3600, now + 3601},
	}
	for _, tc := range cases {
		if got := maxBlockTimestamp(tc.height, tc.parent, now); got != tc.want {
			t.Errorf("%s: maxBlockTimestamp = %d, want %d", tc.name, got, tc.want)
		}
	}
	setFaradayHeight(t, 0)
	if got := maxBlockTimestamp(100, now, now); got != before {
		t.Errorf("Faraday disabled: maxBlockTimestamp = %d, want %d", got, before)
	}
}

func TestValidateTimestampWithMedianUsesTheFaradayLimit(t *testing.T) {
	setFaradayHeight(t, 100)
	now := time.Now().Unix()
	limit := int64(FaradayTimestampFutureLimit / time.Second)
	check := func(height uint64, timestamp, parent int64) error {
		return validateTimestampWithMedian(&BlockHeader{Height: height, Timestamp: timestamp}, parent-1, parent)
	}
	if err := check(99, now+limit+600, now); err != nil {
		t.Fatalf("pre-Faraday block within two hours rejected: %v", err)
	}
	if err := check(100, now+limit-60, now); err != nil {
		t.Fatalf("block within the Faraday limit rejected: %v", err)
	}
	if err := check(100, now+limit+60, now); err == nil {
		t.Fatal("block past the Faraday limit accepted")
	}
	// A parent already an hour ahead can still be extended by one second, but
	// no further.
	parent := now + 3600
	if err := check(100, parent+1, parent); err != nil {
		t.Fatalf("parent+1 on a parent ahead rejected: %v", err)
	}
	if err := check(100, parent+2, parent); err == nil {
		t.Fatal("block moving an ahead chain further ahead accepted")
	}
}

func TestCheapPrecheckAppliesTheFaradayLimit(t *testing.T) {
	chain, tip, _ := newPrecheckTestChain(t, 80)
	setFaradayHeight(t, 81)
	now := time.Now()
	if b := precheckCandidate(t, chain, tip, 81, now.Add(FaradayTimestampFutureLimit-time.Minute).Unix()); !chain.passesCheapPrecheck(b) {
		t.Fatal("block within the Faraday limit failed the cheap precheck")
	}
	if b := precheckCandidate(t, chain, tip, 81, now.Add(FaradayTimestampFutureLimit+time.Minute).Unix()); chain.passesCheapPrecheck(b) {
		t.Fatal("block past the Faraday limit passed the cheap precheck")
	}
}

func TestChainStampedAheadBeforeFaradayStillExtends(t *testing.T) {
	chain, tip, _ := newPrecheckTestChain(t, 80)
	// Height 81 is stamped an hour ahead under the pre-Faraday limit.
	ahead := precheckCandidate(t, chain, tip, 81, time.Now().Add(time.Hour).Unix())
	st := chain.Storage()
	_, _, tipWork, _ := st.GetTip()
	work, err := addCumulativeWork(tipWork, ahead.Header.Difficulty)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	aheadHash := ahead.Hash()
	if err := st.CommitBlock(&BlockCommit{Block: ahead, Height: 81, Hash: aheadHash, Work: work, IsMainTip: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	setFaradayHeight(t, 82)
	if b := precheckCandidate(t, chain, aheadHash, 82, ahead.Header.Timestamp+1); !chain.passesCheapPrecheck(b) {
		t.Fatal("parent+1 on a chain stamped ahead failed the cheap precheck")
	}
	if b := precheckCandidate(t, chain, aheadHash, 82, ahead.Header.Timestamp+60); chain.passesCheapPrecheck(b) {
		t.Fatal("a block moving the chain further ahead passed the cheap precheck")
	}
	// The template for the next block follows the same rule.
	if got := chain.templateTimestamp(aheadHash, time.Now().Unix()); got != ahead.Header.Timestamp+1 {
		t.Fatalf("template timestamp %d, want parent+1 %d", got, ahead.Header.Timestamp+1)
	}
}

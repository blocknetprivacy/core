package main

import "testing"

func TestUpdateNoticeAfterSupportWindow(t *testing.T) {
	cases := []struct {
		height uint64
		want   bool
	}{
		{0, false},
		{releaseHeight, false},
		{releaseHeight + versionSupportBlocks - 1, false},
		{releaseHeight + versionSupportBlocks, true},
		{releaseHeight + 2*versionSupportBlocks, true},
	}
	for _, tc := range cases {
		if got := updateNotice(tc.height) != ""; got != tc.want {
			t.Errorf("height %d: notice shown = %v, want %v", tc.height, got, tc.want)
		}
	}
}

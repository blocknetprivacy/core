package params

import "testing"

func TestPrefersBranch(t *testing.T) {
	const (
		work   = 10_000
		height = 100
		diff   = 1_000
	)
	cases := []struct {
		name      string
		newWork   uint64
		newHeight uint64
		want      bool
	}{
		{"equal work and height", work, height, false},
		{"less work and longer", work - 1, height + 1, false},
		{"more work and longer", work + 1, height + 1, true},
		{"same height, slightly heavier", work + diff/30, height, false},
		{"same height, exactly half a block heavier", work + diff/2, height, false},
		{"same height, more than half a block heavier", work + diff/2 + 1, height, true},
		{"shorter, slightly heavier", work + diff/4, height - 1, false},
		{"shorter, a block heavier", work + diff, height - 1, true},
	}
	for _, tc := range cases {
		if got := PrefersBranch(work, height, diff, tc.newWork, tc.newHeight); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if !PrefersBranch(work, height, 0, work+1, height) {
		t.Error("with no tip difficulty known, any extra work should win")
	}
}

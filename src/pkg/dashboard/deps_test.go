package dashboard

import "testing"

// TestNousBaselinePct guards hivecommons/hive#10026: baseline_pct must cap at
// 100 once snapshots pass NousBaselineTarget instead of growing unbounded
// (snapshots are never pruned, so the raw count keeps climbing).
func TestNousBaselinePct(t *testing.T) {
	cases := []struct {
		name  string
		count int
		want  float64
	}{
		{"negative snapshot count", -1, 0},
		{"zero snapshots", 0, 0},
		{"half of target", NousBaselineTarget / 2, 50},
		{"exactly at target", NousBaselineTarget, 100},
		{"one past target", NousBaselineTarget + 1, 100},
		{"far past target (the #10026 repro: 10519 snapshots)", 10519, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NousBaselinePct(tc.count); got != tc.want {
				t.Errorf("NousBaselinePct(%d) = %v, want %v", tc.count, got, tc.want)
			}
		})
	}
}

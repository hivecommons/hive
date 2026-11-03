package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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

func TestPruneNousSnapshots_KeepsNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{"1000.json", "2000.json", "3000.json", "4000.json"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := pruneNousSnapshots(dir, entries, 2); got != 2 {
		t.Fatalf("removed = %d, want 2", got)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 2 || left[0].Name() != "3000.json" || left[1].Name() != "4000.json" {
		t.Fatalf("remaining = %v, want newest two", left)
	}
	if got := pruneNousSnapshots(dir, left, 5); got != 0 {
		t.Fatalf("removed under limit = %d, want 0", got)
	}
}

func TestRefreshStatus_PrunesBeyondRetention(t *testing.T) {
	dir := t.TempDir()
	total := NousSnapshotRetention + 3
	for i := 0; i < total; i++ {
		name := filepath.Join(dir, fmt.Sprintf("%013d.json", 1000+i))
		if err := os.WriteFile(name, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ns := &NousState{SnapshotDir: dir, Phase: "collecting", Status: map[string]interface{}{}}
	ns.refreshStatus()
	if ns.Status["snapshots"] != NousSnapshotRetention {
		t.Errorf("snapshots = %v, want %d", ns.Status["snapshots"], NousSnapshotRetention)
	}
	if ns.Status["baseline_pct"] != float64(100) {
		t.Errorf("baseline_pct = %v, want 100", ns.Status["baseline_pct"])
	}
	left, _ := os.ReadDir(dir)
	if len(left) != NousSnapshotRetention {
		t.Errorf("files = %d, want %d", len(left), NousSnapshotRetention)
	}
}

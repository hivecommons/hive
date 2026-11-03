package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIssueBandVerifiedOpenCancelsLikelyDone11722(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		labels   []string
		want     string
		wantTick bool
	}{
		{"likely-done + verified-open + claimed is in-progress", []string{"hive/likely-done", "hive/verified-open", "claimed"}, "in-progress", false},
		{"likely-done + verified-open unclaimed is ready", []string{"hive/likely-done", "hive/verified-open"}, "ready", false},
		{"likely-done alone is done", []string{"hive/likely-done"}, "done", true},
		{"already-done + verified-open stays done", []string{"hive/already-done", "hive/verified-open"}, "done", true},
		{"covered-by-pr + verified-open stays done", []string{"hive/covered-by-pr", "Hive/Verified-Open"}, "done", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := IssueBand(github.Issue{Repo: "octo/demo", Number: 11722, Labels: tt.labels}, false, config.DashboardIssueBandsConfig{}, now)
			if info.Band != tt.want {
				t.Fatalf("band = %q, want %q (info=%+v)", info.Band, tt.want, info)
			}
			ticked := false
			for _, sig := range info.Signals {
				if sig.Glyph == "✓" {
					ticked = true
				}
			}
			if ticked != tt.wantTick {
				t.Fatalf("✓ signal = %v, want %v (signals=%+v)", ticked, tt.wantTick, info.Signals)
			}
		})
	}
}

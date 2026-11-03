package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIssueBandChipNamesWaitingLabel11505(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	cfg := config.DashboardIssueBandsConfig{WaitingLabels: []string{"blocked", "needs-decision", "needs-direction", "needs-spec", "2-discussing", "Epic", "needs-human", "needs-triage", "needs-reporter-confirmation", "custom-wait"}}
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"needs-decision", []string{"needs-decision"}, "decision"},
		{"needs-direction", []string{"needs-direction"}, "direction"},
		{"needs-spec", []string{"needs-spec"}, "spec"},
		{"needs-human", []string{"needs-human"}, "human"},
		{"needs-reporter-confirmation", []string{"needs-reporter-confirmation"}, "reporter"},
		{"blocked", []string{"blocked"}, "blocked"},
		{"2-discussing", []string{"2-discussing"}, "discussing"},
		{"Epic is case-insensitive", []string{"Epic"}, "epic"},
		{"needs-triage", []string{"needs-triage"}, "untriaged"},
		{"custom waiting label falls back to band short", []string{"custom-wait"}, ""},
		{"needs-human beats everything", []string{"needs-triage", "Epic", "2-discussing", "blocked", "needs-reporter-confirmation", "needs-spec", "needs-decision", "needs-direction", "needs-human"}, "human"},
		{"needs-direction beats needs-decision", []string{"needs-decision", "needs-direction"}, "direction"},
		{"needs-decision beats needs-spec", []string{"needs-spec", "needs-decision"}, "decision"},
		{"needs-spec beats reporter", []string{"needs-reporter-confirmation", "needs-spec"}, "spec"},
		{"reporter beats blocked", []string{"blocked", "needs-reporter-confirmation"}, "reporter"},
		{"blocked beats 2-discussing", []string{"2-discussing", "blocked"}, "blocked"},
		{"2-discussing beats Epic", []string{"Epic", "2-discussing"}, "discussing"},
		{"Epic beats needs-triage", []string{"needs-triage", "Epic"}, "epic"},
	}
	seen := map[string]string{}
	for _, entry := range issueLabelChips {
		if len(entry.chip) > 10 {
			t.Errorf("chip %q for %q exceeds 10 characters", entry.chip, entry.label)
		}
		if prev, ok := seen[entry.chip]; ok {
			t.Errorf("chip %q used for both %q and %q", entry.chip, prev, entry.label)
		}
		seen[entry.chip] = entry.label
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := IssueBand(github.Issue{Repo: "octo/demo", Number: 11505, Labels: tt.labels}, false, cfg, now)
			if info.Band != "waiting" {
				t.Fatalf("band = %q, want waiting", info.Band)
			}
			if info.Chip != tt.want {
				t.Fatalf("chip = %q, want %q", info.Chip, tt.want)
			}
		})
	}
}

func TestIssueBandChipOnlyForWaitingBand11505(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	info := IssueBand(github.Issue{Repo: "octo/demo", Number: 1, Labels: []string{"needs-human", "hive/already-done"}}, false, config.DashboardIssueBandsConfig{}, now)
	if info.Band != "done" || info.Chip != "" {
		t.Fatalf("done issue: band = %q chip = %q, want done with no chip", info.Band, info.Chip)
	}
	info = IssueBand(github.Issue{Repo: "octo/demo", Number: 2}, false, config.DashboardIssueBandsConfig{}, now)
	if info.Band != "ready" || info.Chip != "" {
		t.Fatalf("ready issue: band = %q chip = %q, want ready with no chip", info.Band, info.Chip)
	}
	if got := issueBandSpec("waiting", config.DashboardIssueBandsConfig{}); got.Label != "Parked" || got.Short != "parked" {
		t.Fatalf("waiting band spec changed: %+v", got)
	}
}

package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Band rules that agree with the labels the issue wears.
func TestIssueBandAgreesWithLabels(t *testing.T) {
	now := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		labels []string
		held   bool
		band   string
		chip   string
		signal string // a signal label that must be present ("" = none checked)
	}{
		// projectbluefin/utah#551: `hold` sat in ready/Unclaimed.
		{name: "held is parked", labels: []string{"kind/bug", "hold", "triage/accepted", "approved-direction"}, held: true, band: "waiting", chip: "hold"},
		{name: "held and claimed is parked", labels: []string{"hold", "hive/claimed-by-scanner"}, held: true, band: "waiting", chip: "hold"},
		{name: "held keeps its park chip", labels: []string{"hold", "needs-human"}, held: true, band: "waiting", chip: "human"},
		{name: "held but done stays done", labels: []string{"hold", "hive/already-done"}, held: true, band: "done"},
		// projectbluefin/common#1328: a parked RFC wearing a month-old likely-done.
		{name: "parked outranks likely-done", labels: []string{"needs-human", "needs-decision", "kind/feature", "hive/likely-done", "triage/accepted"}, band: "waiting"},
		{name: "likely-done alone is done", labels: []string{"hive/likely-done"}, band: "done"},
		{name: "parked does not outrank already-done", labels: []string{"needs-decision", "hive/already-done", "hive/likely-done"}, band: "done"},
		{name: "parked does not outrank covered-by-pr", labels: []string{"needs-decision", "hive/covered-by-pr", "hive/likely-done"}, band: "done"},
		// projectbluefin/utah#12, #1075: 040 read "already done" with an open PR.
		{name: "040 says an open PR covers it", labels: []string{"triage/accepted", stagelabels.CoveredByPR.Label()}, band: "done", signal: "covered by an open PR (stage/040) — check it merged before closing"},
		{name: "040 with already-done keeps already done", labels: []string{stagelabels.CoveredByPR.Label(), "hive/already-done"}, band: "done", signal: "already done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := IssueBand(github.Issue{Repo: "o/r", Number: 1, Labels: tt.labels}, tt.held, config.DashboardIssueBandsConfig{}, now)
			if info.Band != tt.band {
				t.Fatalf("band = %q, want %q (info=%+v)", info.Band, tt.band, info)
			}
			if tt.chip != "" && info.Chip != tt.chip {
				t.Fatalf("chip = %q, want %q", info.Chip, tt.chip)
			}
			if tt.signal != "" {
				found := false
				for _, sig := range info.Signals {
					if sig.Label == tt.signal {
						found = true
					}
				}
				if !found {
					t.Fatalf("signals %+v lack %q", info.Signals, tt.signal)
				}
			}
		})
	}
	if spec := issueBandSpec("waiting", config.DashboardIssueBandsConfig{}); !strings.Contains(spec.Rule, "or held") {
		t.Fatalf("waiting rule %q does not name holds", spec.Rule)
	}
}

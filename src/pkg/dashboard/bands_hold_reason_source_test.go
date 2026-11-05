package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hold_reason named a hold label PRs held only by their sentinel alert did
// not carry (projectbluefin/homebrew-tap#60, testsuite#1108), and called a
// holdguard hold the ACMM level gate (utah-packages#878).
func TestHeldReasonNamesOnlyWhatHoldsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  []string
		sources []string
		want    []string
		never   []string
	}{
		{name: "sentinel alert only", labels: []string{"sentinel-alert", "stage/072-returned-to-contributor"},
			want: []string{"until what holds it is cleared"}, never: []string{"hold label", "ACMM level gate"}},
		{name: "holdguard hold", labels: []string{"hold"}, sources: []string{github.HoldSourceHoldguard},
			want: []string{"label `hold`", "until the hold label is removed"}, never: []string{"ACMM level gate"}},
		{name: "level gate hold", labels: []string{"hold"}, sources: []string{github.HoldSourceLevelGate},
			want: []string{"label `hold`", "ACMM level gate"}},
		{name: "generic hold, sources unknown", labels: []string{"hold"},
			want: []string{"label `hold`", "ACMM level gate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := heldReason(tc.labels, true, "", tc.sources)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q: %s", w, got)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(got, n) {
					t.Errorf("must not say %q: %s", n, got)
				}
			}
		})
	}
}

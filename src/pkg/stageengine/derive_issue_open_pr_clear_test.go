package stageengine

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The issue sweep marks `has-open-pr` stale (IssueFacts.StaleOpenPRLabel)
// once its open-PR listing shows no PR links the issue; the deriver then
// asks the writer to clear it in the same write as the stage, next to any
// other clear. Without the mark, or on a closed issue, nothing is added.
func TestDeriveIssueClearsStaleOpenPRLabel(t *testing.T) {
	const label = "has-open-pr"
	cases := []struct {
		name  string
		mut   func(f *Facts)
		clear []string
	}{
		{"marked stale: cleared", func(f *Facts) {
			f.Labels = append(f.Labels, label)
			f.Issue.StaleOpenPRLabel = label
		}, []string{label}},
		{"worn but not marked: kept", func(f *Facts) { f.Labels = append(f.Labels, label) }, nil},
		{"beside a stale needs-triage: both cleared", func(f *Facts) {
			f.Labels = append(f.Labels, label, "needs-triage")
			f.Issue.Bucket = BucketInProgress
			f.Issue.TriageAwaitingLabel = "needs-triage"
			f.Issue.StaleOpenPRLabel = label
		}, []string{"needs-triage", label}},
		{"closed issue: nothing extra", func(f *Facts) {
			f.Open = false
			f.Issue.StaleOpenPRLabel = label
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveIssue(derivedIssueFacts(tc.mut))
			if !slices.Equal(d.Clear, tc.clear) {
				t.Fatalf("Clear = %v, want %v (stage %s)", d.Clear, tc.clear, d.Stage)
			}
			if d.Stage == stagelabels.None && len(d.Clear) != 0 {
				t.Fatalf("a None stage must not clear: %v", d.Clear)
			}
		})
	}
}

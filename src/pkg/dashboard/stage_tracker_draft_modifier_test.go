package dashboard

import (
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A draft PR reports the draft modifier whether the snapshot's draft bit or
// prow's do-not-merge/work-in-progress label says so, and that WIP label is
// the draft state on the tracker — not a maintainer hold — while a real hold
// label still leads a draft (hivecommons/hive#14572).
func TestStageTrackerModifiers_Draft(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		labels []string
		draft  bool
		want   []string
	}{
		{name: "draft bit only", labels: []string{"size/L", "author/clanker"}, draft: true, want: []string{modDraft}},
		{name: "prow WIP label only", labels: []string{"do-not-merge/work-in-progress", "size/L"}, want: []string{modDraft}},
		{name: "prow WIP label, any case", labels: []string{" Do-Not-Merge/Work-In-Progress "}, want: []string{modDraft}},
		{name: "draft bit and WIP label", labels: []string{"do-not-merge/work-in-progress"}, draft: true, want: []string{modDraft}},
		{name: "neither", labels: []string{"size/L", "author/clanker"}, want: nil},
		{name: "draft and hold: hold leads", labels: []string{"do-not-merge/hold"}, draft: true, want: []string{modHold, modDraft}},
		{name: "WIP and hold: hold leads", labels: []string{"do-not-merge/work-in-progress", "hold"}, want: []string{modHold, modDraft}},
		{name: "hold only is not a draft", labels: []string{"do-not-merge/hold"}, want: []string{modHold}},
		{name: "draft and blocked", labels: []string{"blocked"}, draft: true, want: []string{modBlocked, modDraft}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A PR before the merge gate: hold, blocked and draft are
			// actionable-now pills, so none of them is deferred.
			got, deferred := stageTrackerModifiers(tc.labels, "hv", stagelabels.Record{}, false, nil, now, tc.draft, "", "pr", stagelabels.PRInProgress, false)
			if len(deferred) != 0 {
				t.Fatalf("deferred = %v, want none", deferred)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("modifiers = %v, want %v", got, tc.want)
			}
		})
	}
}

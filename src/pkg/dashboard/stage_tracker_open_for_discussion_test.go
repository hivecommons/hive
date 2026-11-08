package dashboard

import (
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An unclaimed proposal / design issue at 018 reads "Open for discussion",
// not "Actionable" (hivecommons/hive#17150): it lights the Actionable
// column with its own word, the caption names the label that put it there
// and the way in (a claim), the needs-triage chip it may still wear is kept
// (018 is before the claim), and it does not count as waiting on a person.
func TestStageTracker_OpenForDiscussionStage(t *testing.T) {
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	store.Observe(stagelabels.Key("acme/console", 41), stagelabels.OpenForDiscussion, "open for discussion: proposal")
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 41, Title: "📝 proposal: shared cache", Author: "alice", AuthorIsHuman: true, Labels: []string{stagelabels.OpenForDiscussion.Label(), "proposal", "needs-triage"}},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 41)
	if it.Stage != trackerNumberOpenForDiscussion || it.StageLabel != "stage/018-open-for-discussion" || it.Mirrored {
		t.Fatalf("issue41 = %+v", it)
	}
	if col := stageTrackerItemColumn(&it); col != trackerColumnActionable {
		t.Fatalf("018 lights column %d, want Actionable (%d)", col, trackerColumnActionable)
	}
	if want := "Open for discussion (proposal); no agent work is queued — claim it to take it on"; it.Caption != want || it.Actor != "anyone" {
		t.Fatalf("caption=%q actor=%q, want %q / anyone", it.Caption, it.Actor, want)
	}
	if !hasMod(it, modNeedsTriage) || slices.Contains(it.StaleLabels, modNeedsTriage) {
		t.Fatalf("018 is before the claim: needs-triage stays a live chip, never stale (mods %v, stale %v)", it.Modifiers, it.StaleLabels)
	}
	if it.NeedsHuman {
		t.Fatalf("an open discussion is not parked on a person: %+v", it)
	}
	for _, st := range resp.Stages {
		if st.Number == trackerNumberOpenForDiscussion && st.Short != "Open for discussion" {
			t.Fatalf("stage 018 short = %q", st.Short)
		}
	}
}

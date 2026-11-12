package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// heldReviewFindingAsk is one in-scope finding as the lane verdict carries
// it (review.Aggregate.FindingAsks): a file:line and what to change.
const heldReviewFindingAsk = "`src/widget.go:42` return the error instead of dropping it"

// heldReviewEngine is a stage engine for a contributor PR a gate holds: the
// head is green, the verdict lookup answers what the review swarm recorded
// for the head, and the contributor nudge is wired. Nothing is cached for
// the head's mergeability, so only the verdict decides the route.
func heldReviewEngine(t *testing.T, apiURL string, clock *contributorLaneClock, verdict ContributorLaneVerdict, sink ContributorNudgeSink) (*Engine, *StageTracker, *stagelabels.Store) {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	tracker := NewStageTracker()
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	e := New(gatedConflictTransport{Client: client}, Options{
		Now: clock.now,
		StageLabelPolicy: func() StageLabelPolicy {
			return StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: 72 * time.Hour}
		},
		StageTracker:    tracker,
		StageStore:      store,
		CheckSummary:    greenChecks,
		ContributorSoak: NewContributorSoakTracker(),
		ContributorLanePolicy: func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}}
		},
		ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict { return verdict },
		ContributorNudge:       sink,
	})
	return e, tracker, store
}

// heldReviewCommented serves the review the swarm posted on PR 7: a
// COMMENTED review on the head, as every swarm review is. The findings are
// in the hive's verdict, never in a CHANGES_REQUESTED review state.
func heldReviewCommented(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7/reviews" {
		return false
	}
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"id": 1, "state": "COMMENTED", "commit_id": "aaa", "user": map[string]any{"login": testHiveAppBotLogin},
	}})
	return true
}

// A fork contributor's PR that a gate holds (an open sentinel alert, a hold
// label) is skipped by the contributor lane before it reads the reviewer
// verdict, so the lane observation's detail has none. The stage pass still
// sends the PR back to its author, because the human route reads the
// head's verdict itself. The nudge it then posts, and the fix-pr task the
// relay gets, must list the review's findings: the lead line alone tells
// the author that something is wrong but not what.
func TestHeldPRNudgeListsTheReviewFindings(t *testing.T) {
	verdict := ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: []string{heldReviewFindingAsk}}
	want := []string{swarmChangesAskLead, "review finding: " + heldReviewFindingAsk}
	cases := []struct {
		name   string
		label  string
		reason string
	}{
		{"sentinel alert", "sentinel-alert", contributorLaneReasonSentinelBlocked},
		{"hold label", "hold", "label:hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			api.statefulComments = true
			api.extra = heldReviewCommented
			clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
			sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "preempt", Online: true}}
			e, _, store := heldReviewEngine(t, api.url, clock, verdict, sink)

			// The lane's own diagnosis of this observation has no verdict
			// to read: the lead line is all it can say.
			if _, asks, _ := nudgeDiagnosis(stagelabels.LaneReasonChangesRequested, stageDetail{}); !slices.Equal(asks, want[:1]) {
				t.Fatalf("a detail with no verdict diagnosed %q, want the lead line alone", asks)
			}

			pr := nudgePR(7, "aaa", "role/contributor", "author/clanker", "stage/090-ready-for-human-merge", tc.label)
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, tc.reason, false, nil)
			e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec, ok := store.Get(stageKey("acme", "widget", 7))
			if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonChangesRequested {
				t.Fatalf("record = %+v ok=%v, want 072 for the review's requested changes", rec, ok)
			}
			if len(sink.nudges) != 1 {
				t.Fatalf("nudges = %+v, want one", sink.nudges)
			}
			if got := sink.nudges[0].ReviewAsks; !slices.Equal(got, want) {
				t.Fatalf("fix-pr task review asks = %q, want %q", got, want)
			}
			nudges := nudgeComments(api)
			if len(nudges) != 1 {
				t.Fatalf("nudge comments = %q, want one", nudges)
			}
			for _, part := range []string{
				"reason=reviewer-verdict head=aaa n=1 mode=preempt",
				"**Review asks:**\n- the review swarm requested changes\n- review finding: `src/widget.go:42` return the error instead of dropping it\n",
			} {
				if !strings.Contains(nudges[0], part) {
					t.Fatalf("nudge comment is missing %q:\n%s", part, nudges[0])
				}
			}
		})
	}
}

// A contributor PR no gate holds keeps the asks it has today: the lane read
// the verdict into the observation's detail, and the nudge lists the lead
// line and the verdict's findings from there.
func TestUnheldPRNudgeKeepsItsReviewAsks(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "preempt", Online: true}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: 72 * time.Hour})

	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa"), contributorLaneReasonChangesRequested, false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: []string{heldReviewFindingAsk}})
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{swarmChangesAskLead, "review finding: " + heldReviewFindingAsk}
	if len(sink.nudges) != 1 || !slices.Equal(sink.nudges[0].ReviewAsks, want) {
		t.Fatalf("nudges = %+v, want one with review asks %q", sink.nudges, want)
	}
	nudges := nudgeComments(api)
	if len(nudges) != 1 || !strings.Contains(nudges[0], "**Review asks:**\n- the review swarm requested changes\n- review finding: `src/widget.go:42` return the error instead of dropping it\n\nReply here or push a new head") {
		t.Fatalf("nudge comments = %q", nudges)
	}
}

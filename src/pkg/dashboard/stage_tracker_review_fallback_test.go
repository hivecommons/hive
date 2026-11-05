package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#16661: a Review track with no verdict and no reviewer in
// flight states what it waits on, derived from the PR's own lifecycle and
// its CI track — one case per evidence source, never "activity unknown".
func TestStageTrackerReviewFallbackStateDerivesAConcreteStatement(t *testing.T) {
	head := "f269349abcdef0123456789"
	ci := func(state string, passed, failed, total int) *StageTrackerActivity {
		return &StageTrackerActivity{Head: head, State: state, ChecksPassed: passed, ChecksFailed: failed, ChecksTotal: total}
	}
	cases := []struct {
		name    string
		in      stageTrackerReviewFallbackInputs
		state   string
		caption string
		source  string
	}{
		{"merged PR", stageTrackerReviewFallbackInputs{Head: head, Merged: true, Closed: true, Draft: true, CI: ci("running", 1, 0, 5)}, reviewStateClosed, "Review · n/a (merged)", reviewSourceDerived},
		{"closed PR", stageTrackerReviewFallbackInputs{Head: head, Closed: true, Draft: true, CI: ci("passing", 5, 0, 5)}, reviewStateClosed, "Review · n/a (closed)", reviewSourceDerived},
		{"draft outranks CI", stageTrackerReviewFallbackInputs{Head: head, Draft: true, CI: ci("running", 1, 0, 5)}, reviewStateNotRequested, "Review · draft, not requested", reviewSourceDerived},
		{"CI running", stageTrackerReviewFallbackInputs{Head: head, CI: ci("running", 3, 0, 11)}, reviewStateWaitingCI, "Review · waiting for CI", reviewSourceDerived},
		{"CI queued", stageTrackerReviewFallbackInputs{Head: head, CI: ci("queued", 0, 0, 11)}, reviewStateWaitingCI, "Review · waiting for CI", reviewSourceDerived},
		{"checks not all reported", stageTrackerReviewFallbackInputs{Head: head, CI: ci("missing", 7, 0, 11)}, reviewStateWaitingCI, "Review · waiting for CI", reviewSourceDerived},
		{"partial ring without a word", stageTrackerReviewFallbackInputs{Head: head, CI: ci("unknown", 13, 0, 19)}, reviewStateWaitingCI, "Review · waiting for CI", reviewSourceDerived},
		{"CI passed, hive reviewer lane", stageTrackerReviewFallbackInputs{Head: head, CI: ci("passing", 11, 0, 11), ReviewerLane: true}, reviewStateNotStarted, "Review · not started · hive reviewer next", reviewSourceDerived},
		{"CI failed, no reviewer lane", stageTrackerReviewFallbackInputs{Head: head, CI: ci("failing", 10, 1, 11)}, reviewStateNotStarted, "Review · not started · needs reviewer", reviewSourceDerived},
		{"CI skipped counts as done", stageTrackerReviewFallbackInputs{Head: head, CI: ci("skipped", 0, 0, 0), ReviewerLane: true}, reviewStateNotStarted, "Review · not started · hive reviewer next", reviewSourceDerived},
		{"full ring without a word counts as done", stageTrackerReviewFallbackInputs{Head: head, CI: ci("unknown", 19, 0, 19), ReviewerLane: true}, reviewStateNotStarted, "Review · not started · hive reviewer next", reviewSourceDerived},
		{"nothing fetched for the head", stageTrackerReviewFallbackInputs{Head: head, CI: ci("unknown", 0, 0, 0)}, "unknown", "Review · no data for head f269349 yet · refreshing", "unavailable"},
		{"CI from another head is no evidence", stageTrackerReviewFallbackInputs{Head: head, CI: &StageTrackerActivity{Head: "old", State: "passing"}}, "unknown", "Review · no data for head f269349 yet · refreshing", "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := trackerUnknownActivity(head, stageTrackerReviewNoDataCaption(head))
			stageTrackerReviewFallbackState(out, tc.in)
			if out.State != tc.state || out.Caption != tc.caption || out.Source != tc.source {
				t.Fatalf("got %s %q (%s), want %s %q (%s)", out.State, out.Caption, out.Source, tc.state, tc.caption, tc.source)
			}
			if out.Detail == "" || strings.Contains(out.Detail, "unknown") && tc.state != "unknown" {
				t.Fatalf("detail must explain the wait: %q", out.Detail)
			}
			if out.Active {
				t.Fatalf("a derived wait is not motion: %+v", out)
			}
		})
	}
	// A settled or in-flight track is left alone.
	for _, state := range []string{"approved", "changes_requested", "human_decision", "reviewing", "assigned", "unassigned", reviewStateUnverified} {
		out := &StageTrackerActivity{Head: head, State: state, Caption: "kept"}
		stageTrackerReviewFallbackState(out, stageTrackerReviewFallbackInputs{Head: head, Merged: true})
		if out.State != state || out.Caption != "kept" {
			t.Fatalf("%s was rewritten: %+v", state, out)
		}
	}
}

func TestStageTrackerReviewFallbackInputsReadThePR(t *testing.T) {
	now := time.Date(2026, 11, 4, 19, 0, 0, 0, time.UTC)
	it := &StageTrackerItem{Head: "abc", Modifiers: []string{modDraft}, CIActivity: &StageTrackerActivity{Head: "abc", State: "passing"}}
	in := stageTrackerReviewFallbackInputsFor(it, github.PullRequest{HeadSHA: "abc"}, true)
	if !in.Draft || in.Closed || in.Merged || in.CI != it.CIActivity || !in.ReviewerLane || in.Head != "abc" {
		t.Fatalf("draft modifier not read: %+v", in)
	}
	in = stageTrackerReviewFallbackInputsFor(&StageTrackerItem{Head: "abc"}, github.PullRequest{HeadSHA: "abc", MergedAt: now, ClosedAt: now, State: "closed"}, false)
	if !in.Merged || !in.Closed || in.Draft {
		t.Fatalf("merged PR not read: %+v", in)
	}
	in = stageTrackerReviewFallbackInputsFor(&StageTrackerItem{Head: "abc"}, github.PullRequest{HeadSHA: "abc", State: "closed"}, false)
	if in.Merged || !in.Closed {
		t.Fatalf("closed state not read: %+v", in)
	}
}

func TestStageTrackerCINoDataCaptionNamesTheHead(t *testing.T) {
	if got := stageTrackerCINoDataCaption("f269349abcdef"); got != "CI · no checks observed for head f269349 yet · refreshing" {
		t.Fatalf("caption = %q", got)
	}
	if got := stageTrackerCINoDataCaption(""); !strings.Contains(got, "head unknown") || strings.Contains(got, "CI unknown") {
		t.Fatalf("caption = %q", got)
	}
}

// hivecommons/hive#16617: one entry per perspective, a blocking verdict on
// the same perspective outranking another backend's approval, newest among
// equals; the first finding rides as the summary.
func TestTrackerReviewPerspectivesPreserveDissent(t *testing.T) {
	now := time.Date(2026, 11, 4, 19, 0, 0, 0, time.UTC)
	bundle := &evidence.Bundle{Verdicts: []evidence.Verdict{
		{Perspective: "security", Backend: "a", Model: "m", Verdict: "changes_requested", RecordedAt: now.Add(-2 * time.Minute), Findings: []evidence.Finding{{Summary: "token logged"}}},
		{Perspective: "security", Backend: "b", Model: "m", Verdict: "approve", RecordedAt: now},
		{Perspective: "quality", Backend: "a", Model: "m", Verdict: "approve", RecordedAt: now.Add(-3 * time.Minute)},
		{Perspective: "quality", Backend: "a", Model: "m", Verdict: "approve", RecordedAt: now},
		{Perspective: "", Backend: "a", Model: "m", Verdict: "approve", RecordedAt: now},
	}}
	got := trackerReviewPerspectives(bundle)
	if len(got) != 2 || got[0].Name != "quality" || got[0].Verdict != "approve" || got[0].Summary != "" ||
		got[1].Name != "security" || got[1].Verdict != "changes_requested" || got[1].Summary != "token logged" {
		t.Fatalf("perspectives = %+v", got)
	}
	out := &StageTrackerActivity{State: "changes_requested"}
	if d := stageTrackerReviewVerdictDetail(out, bundle); !strings.Contains(d, "Changes requested by the hive reviewer") || !strings.Contains(d, "1 perspective posted findings") {
		t.Fatalf("detail = %q", d)
	}
	out.State = "approved"
	if d := stageTrackerReviewVerdictDetail(out, bundle); !strings.Contains(d, "Approved by the hive reviewer on") {
		t.Fatalf("detail = %q", d)
	}
	if d := stageTrackerReviewVerdictDetail(out, nil); d != "" {
		t.Fatalf("no bundle, no detail: %q", d)
	}
}

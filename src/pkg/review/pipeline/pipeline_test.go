package pipeline

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

var (
	t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

const (
	testRepo = "acme/widgets"
	testURL  = "https://github.com/acme/widgets/pull/7"
	linkURL  = "https://github.com/acme/widgets/pull/7#pullrequestreview-1"
)

func basePR() ghpkg.ReviewQueueEntry {
	return ghpkg.ReviewQueueEntry{
		Repo: testRepo, Number: 7, Title: "fix: thing", Author: "alice", URL: testURL,
		HeadSHA: "head", CreatedAt: t0, CIState: ghpkg.ReviewQueueCIGreen, HiveAuthored: true,
	}
}

func withPR(mut func(*ghpkg.ReviewQueueEntry)) ghpkg.ReviewQueueEntry {
	pr := basePR()
	mut(&pr)
	return pr
}

func agg(verdict review.Verdict, head string, at time.Time) review.Aggregate {
	return review.Aggregate{Repo: testRepo, Number: 7, HeadSHA: head, Verdict: verdict, RecordedAt: at}
}

func TestDerive_Stages(t *testing.T) {
	approved := agg(review.VerdictApprove, "head", t1)
	approved.MergeEligible = true

	changes := agg(review.VerdictChangesRequested, "head", t1)
	changes.Reasons = []string{"security requested changes"}

	requiresHuman := agg(review.VerdictChangesRequested, "head", t1)
	requiresHuman.RequiresHuman = true

	capped := agg(review.VerdictChangesRequested, "head", t1)
	capped.FixAttempts = 3
	capped.MaxFixAttempts = 3

	cases := []struct {
		name       string
		in         Inputs
		stage      Stage
		since      time.Time
		action     string
		actionURL  string
		reasonPart string
	}{
		{
			name: "no evidence is unreviewed", in: Inputs{PR: basePR()},
			stage: StageUnreviewed, since: t0, action: ActionReview, reasonPart: "no review verdict",
		},
		{
			name:  "stale verdict on old head is unreviewed",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictChangesRequested, "old", t1)}},
			stage: StageUnreviewed, since: t0, action: ActionReview, reasonPart: "head moved",
		},
		{
			name:  "non-merge-eligible approve is unreviewed",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictApprove, "head", t1)}},
			stage: StageUnreviewed, since: t0, action: ActionReview, reasonPart: "not actionable",
		},
		{
			name: "pending review on head is reviewing",
			in: Inputs{PR: basePR(), Dispatch: review.DispatchState{Pending: []review.PendingReview{
				{Repo: testRepo, Number: 7, HeadSHA: "head", Perspective: "security", Dispatched: t2},
				{Repo: "widgets", Number: 7, HeadSHA: "head", Perspective: "quality", Dispatched: t1},
				{Repo: testRepo, Number: 7, HeadSHA: "old", Perspective: "docs", Dispatched: t0},
				{Repo: testRepo, Number: 8, HeadSHA: "head", Perspective: "docs", Dispatched: t0},
			}}},
			stage: StageReviewing, since: t1, action: ActionWait, reasonPart: "2 review perspective",
		},
		{
			name:  "changes_requested verdict",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{changes}, ReviewLink: &ghpkg.ReviewLink{URL: linkURL, HeadSHA: "head", State: "changes_requested", At: t0}},
			stage: StageChangesRequested, since: t1, action: ActionFix, actionURL: linkURL, reasonPart: "security requested changes",
		},
		{
			name:  "changes-requested hive review without verdict",
			in:    Inputs{PR: basePR(), ReviewLink: &ghpkg.ReviewLink{URL: linkURL, HeadSHA: "head", State: "CHANGES_REQUESTED", At: t2}},
			stage: StageChangesRequested, since: t2, action: ActionFix, actionURL: linkURL, reasonPart: "hive review",
		},
		{
			name:  "review link on old head is ignored",
			in:    Inputs{PR: basePR(), ReviewLink: &ghpkg.ReviewLink{URL: linkURL, HeadSHA: "old", State: "changes_requested", At: t2}},
			stage: StageUnreviewed, since: t0, action: ActionReview, reasonPart: "no review verdict",
		},
		{
			name: "pending fix is fixing",
			in: Inputs{PR: basePR(), Verdicts: []review.Aggregate{changes}, Dispatch: review.DispatchState{Fixes: []review.PendingFix{
				{Repo: testRepo, Number: 7, HeadSHA: "head", Agent: "fixer", Attempts: 1, Dispatched: t2},
			}}},
			stage: StageFixing, since: t2, action: ActionWait, reasonPart: "fix dispatched to fixer",
		},
		{
			name:  "fix cap reached moves changes_requested to human_hold",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{capped}},
			stage: StageHumanHold, since: t1, action: ActionHuman, reasonPart: "fix-cycle cap reached (3/3)",
		},
		{
			name: "fix cap reached moves fixing to human_hold",
			in: Inputs{PR: basePR(), FixCycleCap: 2, Dispatch: review.DispatchState{Fixes: []review.PendingFix{
				{Repo: testRepo, Number: 7, Attempts: 2, Dispatched: t2},
			}}},
			stage: StageHumanHold, since: t2, action: ActionHuman, reasonPart: "fix dispatched to an agent",
		},
		{
			name:  "hold label is human_hold",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"Hold"} }), Verdicts: []review.Aggregate{approved}},
			stage: StageHumanHold, action: ActionHuman, reasonPart: `label "Hold"`,
		},
		{
			name:  "needs-human label is human_hold",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"needs-human"} })},
			stage: StageHumanHold, action: ActionHuman, reasonPart: `label "needs-human"`,
		},
		{
			name:  "held flag is human_hold",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Held = true })},
			stage: StageHumanHold, action: ActionHuman, reasonPart: "held in the review queue",
		},
		{
			name:  "requires_human verdict",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{requiresHuman}},
			stage: StageHumanHold, since: t1, action: ActionHuman, reasonPart: "requires a human",
		},
		{
			name:  "requires_human verdict value",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictRequiresHuman, "head", t2)}},
			stage: StageHumanHold, since: t2, action: ActionHuman, reasonPart: "requires a human",
		},
		{
			name:  "reject verdict",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictReject, "head", t2)}},
			stage: StageHumanHold, since: t2, action: ActionHuman, reasonPart: "rejects",
		},
		{
			name: "dispatch human hold",
			in: Inputs{PR: basePR(), Dispatch: review.DispatchState{Human: []review.HumanReviewHold{
				{Repo: testRepo, Number: 7, HeadSHA: "head", UpdatedAt: t2},
			}}},
			stage: StageHumanHold, since: t2, action: ActionHuman, reasonPart: "no reason recorded",
		},
		{
			name:  "escalated entry",
			in:    Inputs{PR: basePR(), Escalation: &escalation.Entry{Escalated: true, LabelAppliedAt: t1}},
			stage: StageHumanHold, since: t1, action: ActionHuman, reasonPart: "escalated",
		},
		{
			name:  "merge-eligible approve with green CI",
			in:    Inputs{PR: basePR(), Verdicts: []review.Aggregate{approved}},
			stage: StageApproved, since: t1, action: ActionMerge, reasonPart: "merge-eligible approve",
		},
		{
			name:  "approved with red CI",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.CIState = ghpkg.ReviewQueueCIRed }), Verdicts: []review.Aggregate{approved}},
			stage: StageApproved, since: t1, action: ActionFixCI, reasonPart: "merge-eligible approve",
		},
		{
			name:  "lgtm label with pending CI",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"lgtm"}; p.CIState = ghpkg.ReviewQueueCIPending })},
			stage: StageApproved, action: ActionWaitCI, reasonPart: `label "lgtm"`,
		},
		{
			name:  "reviewer-passed SHA on head",
			in:    Inputs{PR: basePR(), Escalation: &escalation.Entry{ReviewerPassedSHA: "head", ReviewerPassedAt: t2}},
			stage: StageApproved, since: t2, action: ActionMerge, reasonPart: "reviewer passed",
		},
		{
			name:  "reviewer-passed SHA on old head is ignored",
			in:    Inputs{PR: basePR(), Escalation: &escalation.Entry{ReviewerPassedSHA: "old", ReviewerPassedAt: t2}},
			stage: StageUnreviewed, since: t0, action: ActionReview,
		},
		{
			name:  "approving hive review link",
			in:    Inputs{PR: basePR(), ReviewLink: &ghpkg.ReviewLink{URL: linkURL, HeadSHA: "head", State: "approved", At: t2}},
			stage: StageApproved, since: t2, action: ActionMerge, reasonPart: "hive review approved",
		},
		{
			name:  "merged by state",
			in:    Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"hold"} }), Merge: MergeState{State: "MERGED"}},
			stage: StageMerged, action: ActionNone, reasonPart: "merged",
		},
		{
			name:  "merged by timestamp beats closed",
			in:    Inputs{PR: basePR(), Merge: MergeState{State: MergeStateClosed, MergedAt: t2, ClosedAt: t2}},
			stage: StageMerged, since: t2, action: ActionNone, reasonPart: "merged",
		},
		{
			name:  "closed without merge",
			in:    Inputs{PR: basePR(), Merge: MergeState{State: MergeStateClosed, ClosedAt: t1}},
			stage: StageAbandoned, since: t1, action: ActionReopen, reasonPart: "closed",
		},
		{
			name:  "closed by timestamp",
			in:    Inputs{PR: basePR(), Merge: MergeState{State: MergeStateOpen, ClosedAt: t1}},
			stage: StageAbandoned, since: t1, action: ActionReopen, reasonPart: "closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Derive(tc.in)
			if card.Stage != tc.stage {
				t.Fatalf("stage = %s, want %s (reasons %q)", card.Stage, tc.stage, card.Reasons)
			}
			if !card.Since.Equal(tc.since) {
				t.Errorf("since = %v, want %v", card.Since, tc.since)
			}
			if card.NextAction.Kind != tc.action || card.NextAction.Label == "" {
				t.Errorf("next action = %+v, want kind %q", card.NextAction, tc.action)
			}
			wantURL := tc.actionURL
			if wantURL == "" {
				wantURL = testURL
			}
			if card.NextAction.URL != wantURL {
				t.Errorf("next action url = %q, want %q", card.NextAction.URL, wantURL)
			}
			if len(card.Reasons) == 0 {
				t.Fatalf("no reasons")
			}
			if tc.reasonPart != "" && !strings.Contains(strings.Join(card.Reasons, "|"), tc.reasonPart) {
				t.Errorf("reasons %q missing %q", card.Reasons, tc.reasonPart)
			}
			if card.Reviewers == nil || card.Reasons == nil {
				t.Errorf("reviewers/reasons must encode as arrays: %+v", card)
			}
			if card.Repo != testRepo || card.Number != 7 || card.URL != testURL || card.Title == "" || card.Author != "alice" || !card.HiveAuthored {
				t.Errorf("identity not copied: %+v", card)
			}
		})
	}
}

// TestDerive_Transitions walks one PR through the pipeline as evidence
// accumulates, asserting each rule hands off to the next.
func TestDerive_Transitions(t *testing.T) {
	in := Inputs{PR: basePR()}
	step := func(want Stage) {
		t.Helper()
		if got := Derive(in).Stage; got != want {
			t.Fatalf("stage = %s, want %s", got, want)
		}
	}
	step(StageUnreviewed)

	in.Dispatch.Pending = []review.PendingReview{{Repo: testRepo, Number: 7, HeadSHA: "head", Perspective: "security", Dispatched: t0}}
	step(StageReviewing)

	in.Dispatch.Pending = nil
	in.Verdicts = []review.Aggregate{agg(review.VerdictChangesRequested, "head", t1)}
	step(StageChangesRequested)

	in.Dispatch.Fixes = []review.PendingFix{{Repo: testRepo, Number: 7, HeadSHA: "head", Attempts: 1, Dispatched: t1}}
	step(StageFixing)

	// Fix pushed: head moves, the old verdict and fix go stale.
	in.PR.HeadSHA = "head2"
	step(StageUnreviewed)

	approved := agg(review.VerdictApprove, "head2", t2)
	approved.MergeEligible = true
	in.Verdicts = append(in.Verdicts, approved)
	step(StageApproved)

	in.PR.Labels = []string{"hold"}
	step(StageHumanHold)

	in.Merge = MergeState{State: MergeStateMerged, MergedAt: t2}
	step(StageMerged)
}

func TestDerive_ReviewersSeverityAndLoop(t *testing.T) {
	a := agg(review.VerdictChangesRequested, "head", t1)
	a.ReviewModel = "model-x"
	a.FixAttempts = 1
	a.Perspectives = map[review.Perspective]review.Verdict{"security": review.VerdictChangesRequested, "correctness": review.VerdictApprove}
	a.Findings = []review.PerspectiveFinding{
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityCritical}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityHigh}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityHigh}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityMedium}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityLow}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityInfo}},
		{Perspective: "security", Finding: outputschema.Finding{Severity: outputschema.SeverityCritical, ReviewScope: "out-of-scope"}},
	}
	older := agg(review.VerdictApprove, "head", t0)
	other := a
	other.Number = 99
	other.RecordedAt = t2
	in := Inputs{
		PR:       basePR(),
		Verdicts: []review.Aggregate{older, a, other},
		Dispatch: review.DispatchState{
			Pending: []review.PendingReview{
				{Repo: testRepo, Number: 7, HeadSHA: "head", Perspective: "security"},
				{Repo: testRepo, Number: 7, HeadSHA: "head", Perspective: "docs"},
			},
			FixAttempts: []review.FixAttempt{{Repo: testRepo, Number: 7, Attempts: 2}, {Repo: testRepo, Number: 8, Attempts: 5}},
		},
	}
	card := Derive(in)
	if card.Stage != StageChangesRequested {
		t.Fatalf("stage = %s", card.Stage)
	}
	wantReviewers := []Reviewer{{Perspective: "correctness", Model: "model-x"}, {Perspective: "security", Model: "model-x"}, {Perspective: "docs"}}
	if !reflect.DeepEqual(card.Reviewers, wantReviewers) {
		t.Fatalf("reviewers = %+v, want %+v", card.Reviewers, wantReviewers)
	}
	if want := (SeverityCounts{P0: 1, P1: 2, P2: 1, P3: 2}); card.Severity != want {
		t.Fatalf("severity = %+v, want %+v", card.Severity, want)
	}
	if card.LoopCount != 2 || card.LoopCap != review.DefaultFixCycleCap {
		t.Fatalf("loop = %d/%d", card.LoopCount, card.LoopCap)
	}

	in.Escalation = &escalation.Entry{ReEngagements: 4}
	in.FixCycleCap = 10
	if c := Derive(in); c.LoopCount != 4 || c.LoopCap != 10 || c.Stage != StageChangesRequested {
		t.Fatalf("escalation loop = %d/%d %s", c.LoopCount, c.LoopCap, c.Stage)
	}
}

func TestStage_TextAndJSON(t *testing.T) {
	want := []string{"unreviewed", "reviewing", "changes_requested", "fixing", "human_hold", "approved", "merged", "abandoned"}
	stages := Stages()
	if len(stages) != len(want) {
		t.Fatalf("Stages() = %v", stages)
	}
	for i, s := range stages {
		if s.String() != want[i] {
			t.Fatalf("String(%d) = %q, want %q", i, s, want[i])
		}
		raw, err := json.Marshal(s)
		if err != nil || string(raw) != `"`+want[i]+`"` {
			t.Fatalf("Marshal(%s) = %s, %v", s, raw, err)
		}
		var back Stage
		if err := json.Unmarshal(raw, &back); err != nil || back != s {
			t.Fatalf("Unmarshal(%s) = %v, %v", raw, back, err)
		}
		if p, err := ParseStage(" " + strings.ToUpper(want[i]) + " "); err != nil || p != s {
			t.Fatalf("ParseStage(%q) = %v, %v", want[i], p, err)
		}
	}
	if _, err := ParseStage("nope"); err == nil {
		t.Fatal("ParseStage accepted an unknown stage")
	}
	var s Stage
	if err := json.Unmarshal([]byte(`"nope"`), &s); err == nil {
		t.Fatal("Unmarshal accepted an unknown stage")
	}
	bad := Stage(42)
	if bad.String() != "stage(42)" {
		t.Fatalf("String(42) = %q", bad.String())
	}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("Marshal accepted an invalid stage")
	}
	if Stage(-1).String() != "stage(-1)" {
		t.Fatal("negative stage String")
	}
}

func TestCard_JSONShape(t *testing.T) {
	raw, err := json.Marshal(Derive(Inputs{PR: basePR()}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"stage", "since", "reviewers", "severity", "loop_count", "loop_cap", "next_action", "reasons"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("card JSON missing %q: %s", k, raw)
		}
	}
	if m["stage"] != "unreviewed" {
		t.Fatalf("stage = %v", m["stage"])
	}
}

func TestSameRepo(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"acme/widgets", "ACME/Widgets", true},
		{"acme/widgets", "widgets", true},
		{"widgets", "acme/widgets", true},
		{"acme/widgets", "other/widgets", false},
		{"widgets", "gadgets", false},
		{"", "widgets", false},
		{"acme/widgets", " ", false},
	}
	for _, tc := range cases {
		if got := SameRepo(tc.a, tc.b); got != tc.want {
			t.Errorf("SameRepo(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDerive_LoopWarning(t *testing.T) {
	changes := agg(review.VerdictChangesRequested, "head", t1)
	changes.Perspectives = map[review.Perspective]review.Verdict{"security": review.VerdictChangesRequested}
	base := func() Inputs {
		return Inputs{PR: basePR(), Verdicts: []review.Aggregate{changes}}
	}

	if w := Derive(base()).LoopWarning; w != nil {
		t.Fatalf("fresh PR warned: %+v", w)
	}

	in := base()
	in.FixCycleCap = 3
	in.Dispatch.FixAttempts = []review.FixAttempt{{Repo: testRepo, Number: 7, Attempts: 2}}
	w := Derive(in).LoopWarning
	if w == nil || w.Reviewer != "security" || !strings.Contains(w.Reason, "2 of 3") {
		t.Fatalf("cap-1 warning = %+v", w)
	}

	in = base()
	in.BotMaxAttempts = 2
	in.BotThreads = []BotThread{{Reviewer: "codex", Thread: "T1", Attempts: 1}, {Reviewer: "copilot", Thread: "T2", Attempts: 2}}
	w = Derive(in).LoopWarning
	if w == nil || w.Reviewer != "copilot" || w.Thread != "T2" {
		t.Fatalf("thread warning = %+v", w)
	}

	in.PR = withPR(func(pr *ghpkg.ReviewQueueEntry) { pr.Labels = []string{escalation.NeedsHumanLabel} })
	if w := Derive(in).LoopWarning; w != nil {
		t.Fatalf("needs-human PR warned: %+v", w)
	}
}

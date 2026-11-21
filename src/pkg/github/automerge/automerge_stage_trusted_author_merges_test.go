package automerge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The trusted-author lane writes its note once per evaluation, from the
// outcome: set when it merged the head it admitted (its review gate does not
// enforce), cleared when the gate enforces or a prefilter refuses the PR (a
// hold label), and left as it was when a lookup failed: the error says
// nothing new about the head (hivecommons/hive#19421).
func TestTrustedAuthorLaneNotesItMergesOverTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          string
		labels        []string
		reviewsStatus int
		want          bool
	}{
		{name: "shadow", mode: ReviewGateModeShadow, want: true},
		{name: "off", mode: ReviewGateModeOff, want: true},
		{name: "enforce", mode: ReviewGateModeEnforce},
		{name: "hold label", mode: ReviewGateModeShadow, labels: []string{"hold"}},
		{name: "review lookup error keeps the note", mode: ReviewGateModeEnforce, reviewsStatus: http.StatusInternalServerError, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{labels: tc.labels, reviewsStatus: tc.reviewsStatus}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger", ExcludeLabels: map[string]bool{"hold": true}, HoldLabel: "hold"}
			tracker := NewStageTracker()
			engine := newTrustedSweepEngine(api.URL, policy, Options{
				StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
				StageTracker:     tracker,
			})
			engine.SetReviewGate(&ReviewGate{
				Mode:    func() string { return tc.mode },
				Verdict: func(string, int, string) ReviewGateVerdict { return reviewGateNoBundle },
			})
			key := stageKey("acme", "widget", 7)
			// A word left from an earlier evaluation must not survive this one.
			tracker.trustedAuthorMerges = map[string]trustedAuthorMergesEntry{key: {head: "sha7", at: time.Now(), holdLabel: "hold"}}
			if _, _, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy); (err != nil) != (tc.reviewsStatus != 0) {
				t.Fatalf("err = %v", err)
			}
			hold, got := stageTrustedAuthorMerges(tracker, key, "sha7", time.Now())
			if got != tc.want || (got && hold != "hold") {
				t.Fatalf("note = %q, %v; want %v", hold, got, tc.want)
			}
		})
	}
}

// The note is per head and goes stale: a new head, a gate switched to
// enforce, an observe-only engine or a lane that stopped evaluating the PR
// past stageOwnerAbsenceGrace all read as no note.
func TestStageTrustedAuthorMergesClears(t *testing.T) {
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	tracker := NewStageTracker()
	e := New(nil, Options{Now: clock.now, StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }, StageTracker: tracker})
	key := stageKey("acme", "widget", 9)
	read := func(head string) bool {
		_, ok := stageTrustedAuthorMerges(tracker, key, head, clock.now())
		return ok
	}

	e.noteStageTrustedAuthorMerges("acme", "widget", 9, "sha1", "")
	if !read("sha1") {
		t.Fatal("no review gate (nothing enforces): the lane's note is missing")
	}
	if read("sha2") {
		t.Fatal("the note for sha1 reads for a new head")
	}

	e.SetReviewGate(&ReviewGate{RequireApproval: func() bool { return true }})
	e.noteStageTrustedAuthorMerges("acme", "widget", 9, "sha1", "")
	if read("sha1") {
		t.Fatal("review.require_approval raises the gate to enforce: the note must clear")
	}

	e.SetReviewGate(&ReviewGate{})
	e.observeOnly = true
	e.noteStageTrustedAuthorMerges("acme", "widget", 9, "sha1", "")
	if read("sha1") {
		t.Fatal("an observe-only pass merges nothing, so it must not note a merge")
	}

	e.observeOnly = false
	e.noteStageTrustedAuthorMerges("acme", "widget", 9, "sha1", "")
	if !read("sha1") {
		t.Fatal("shadow mode: the lane's note is missing")
	}
	clock.at = clock.at.Add(stageOwnerAbsenceGrace + time.Minute)
	if read("sha1") {
		t.Fatal("a note the lane has not renewed past the grace still reads")
	}
}

// The human route reads the lane's note for the observed head, whichever
// lane observed it, and the 085 comment says the PR will still merge; a new
// head without the note gets the comment it always had.
func TestHumanEvidenceTrustedAuthorMerges(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	verdict := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true,
		DecisionQuestion: "@clubanderson should the hive resolve its own review threads?", DecisionFinding: "the reviewer marked this change high-risk"}
	e, _ := humanStageEngine(t, api.url, clock, &verdict, 0)
	e.SetReviewGate(&ReviewGate{})
	tracker := e.stageTrackerOrShared()
	key := stageKey("acme", "widget", 9)
	obs := stageObservation{Lane: stageLaneTrusted, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha1",
		Reason: stageReasonVerdictRequiresHuman, AuthorKind: authorkind.Clanker}
	comment := func() string {
		ev := e.humanEvidence(context.Background(), tracker, key, obs, stagelabels.CauseDecision, clock.now())
		return stagelabels.DecisionComment(stagelabels.RouteHuman(ev))
	}

	if got := comment(); !strings.Contains(got, "will not merge this unattended") || strings.Contains(got, "will still merge") {
		t.Fatalf("before the lane noted anything the 085 comment changed:\n%s", got)
	}
	e.noteStageTrustedAuthorMerges("acme", "widget", 9, "sha1", "do-not-merge")
	got := comment()
	for _, want := range []string{"Needs a decision, but the hive will still merge this PR", "reviewer question: @clubanderson", "add the `do-not-merge` label"} {
		if !strings.Contains(got, want) {
			t.Fatalf("085 comment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "will not merge this unattended") {
		t.Fatalf("085 comment still promises no merge:\n%s", got)
	}

	obs.HeadSHA = "sha2"
	if got := comment(); strings.Contains(got, "will still merge") {
		t.Fatalf("the note for sha1 carried onto sha2:\n%s", got)
	}

	// GitHub reports sha1 dirty: the lane cannot merge it as it stands, so
	// the comment does not say it will.
	obs.HeadSHA = "sha1"
	obs.Detail.MergeableState, obs.Detail.MergeableStateHead = "dirty", "sha1"
	if got := comment(); strings.Contains(got, "will still merge") {
		t.Fatalf("a dirty head is said to merge:\n%s", got)
	}
}

// A person's own PR with the reviewer's question routes to 071, the author's
// to answer (hivecommons/hive#15202). When the trusted-author lane merges
// that head anyway, the author's note says so and names the label that stops
// it instead of "the hive will not merge it yet"; it is still posted once
// per head. A conflict on a PR with the same note keeps the held wording: it
// really does stop the merge.
func TestRequiresHumanOnTrustedPersonPRNoteSaysItStillMerges(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{
		Configured:        true,
		Verdict:           "requires_human",
		DecisionRequested: true,
		DecisionQuestion:  "should this groundwork land before its consumer?",
	}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	e.SetReviewGate(&ReviewGate{})
	ctx := context.Background()
	sweep := func(number int, reason string, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(number, labels...)
		pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
		e.noteStageTrustedAuthorMerges("acme", "widget", number, pr.GetHead().GetSHA(), "hold")
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageVerdict("acme", "widget", number, *verdict)
		e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sweep(31, contributorLaneReasonNoVerdict, "role/trusted", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 31); got != stagelabels.NeedsAuthor {
		t.Fatalf("stage=%v want 071", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], TrustedHuman071MarkerPrefix) {
		t.Fatalf("071 note = %q", api.comments)
	}
	note := api.comments[0]
	for _, want := range []string{
		"@clubanderson this PR is at `stage/071-needs-author` with a question from the reviewer for you, but the hive will still merge it",
		"To stop that until you have answered, add the `hold` label.",
		"the reviewer asked you: should this groundwork land before its consumer?",
		"Next: answer the question here",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("071 note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "will not merge it yet") {
		t.Fatalf("071 note still promises no merge:\n%s", note)
	}
	// Same head again: told once.
	sweep(31, contributorLaneReasonNoVerdict, "role/trusted", "author/human", "stage/071-needs-author")
	if len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("071 dedupe: comments=%q writes=%d", api.comments, api.writes())
	}

	sweep(32, "conflicting", "role/trusted", "author/human", "stage/050-review-pending")
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "will not merge it yet") || strings.Contains(api.comments[0], "will still merge") {
		t.Fatalf("conflict 071 note = %q", api.comments)
	}
}

// The same-head 085 re-park is built from the stage pass's verdict, not from
// Evidence: it carries the lane's note too, so it does not lead with "Needs
// a decision, not a merge" on a PR the lane merges. A hold label, a sentinel
// alert or no note keeps the re-park as it was.
func TestSameHeadDecisionRouteTrustedAuthorMerges(t *testing.T) {
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	tracker := NewStageTracker()
	e := New(nil, Options{Now: clock.now, StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }, StageTracker: tracker})
	key := stageKey("acme", "widget", 19421)
	obs := stageObservation{HeadSHA: "4902cf73db2c", Lane: stageLaneTrusted, Owner: "acme", Repo: "widget", Number: 19421}
	obs.Detail.VerdictKnown = true
	obs.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true,
		DecisionFinding: "the reviewer marked this change high-risk", DecisionQuestion: "@clubanderson should the hive resolve its own review threads?"}
	reparked := func(o stageObservation) stagelabels.Route {
		return e.withTrustedAuthorMerges(tracker, key, o, sameHeadDecisionRoute(o), clock.now())
	}

	if r := reparked(obs); r.TrustedAuthorMerges || !strings.Contains(stagelabels.DecisionComment(r), "Needs a decision, not a merge") {
		t.Fatalf("no note: re-park changed: %+v", r)
	}
	e.noteStageTrustedAuthorMerges("acme", "widget", 19421, obs.HeadSHA, "hold")
	r := reparked(obs)
	body := stagelabels.DecisionComment(r)
	for _, want := range []string{"Needs a decision, but the hive will still merge this PR:", "reviewer question: @clubanderson", "add the `hold` label."} {
		if !strings.Contains(body, want) {
			t.Fatalf("re-park comment missing %q:\n%s", want, body)
		}
	}
	if !r.TrustedAuthorMerges || strings.Contains(body, "Needs a decision, not a merge") {
		t.Fatalf("re-park route = %+v\n%s", r, body)
	}

	for name, labels := range map[string][]string{"hold": {"hold"}, "sentinel": {defaultSentinelLabel}, "blocked": {"blocked"}} {
		o := obs
		o.Labels = labels
		if r := reparked(o); r.TrustedAuthorMerges {
			t.Fatalf("%s: the re-park claims a merge the label stops: %+v", name, r)
		}
	}
	dirty := obs
	dirty.Detail.MergeableState, dirty.Detail.MergeableStateHead = "dirty", obs.HeadSHA
	if r := reparked(dirty); r.TrustedAuthorMerges {
		t.Fatalf("dirty head: the re-park claims a merge: %+v", r)
	}
	conflicting := obs
	conflicting.Reason = "conflicting"
	if r := reparked(conflicting); r.TrustedAuthorMerges {
		t.Fatalf("conflicting head: the re-park claims a merge: %+v", r)
	}
}

// settleStageTrustedAuthorMerges keeps "will still merge" only for an
// evaluation that merged or stopped on a wait that ends in the lane's merge.
// A stop the lane will not get past as the head stands (a conflict, an
// unsigned squash, a release hold, a fence, a never-reported required check,
// a known merge refusal …) clears it, and an error or a state the lane
// could not read this pass (forge state, current-base check) leaves it as
// it was.
func TestSettleStageTrustedAuthorMerges(t *testing.T) {
	for _, tc := range []struct {
		reason string
		err    error
		want   bool
	}{
		{reason: "", want: true},
		{reason: "check-pending", want: true},
		{reason: "required-check-pending:unit", want: true},
		{reason: "required-check-missing:unit", want: true},
		{reason: "pending: head pushed 30s ago (< min_head_age)", want: true},
		{reason: "behind", want: true},
		{reason: basePreparationReady, want: true},
		{reason: nativeQueueReason, want: true},
		{reason: "head-changed-since-eval", want: true},
		{reason: prstageMergeTrainAdmittedReason(), want: true},
		{reason: "conflicting"},
		{reason: autoMergeReasonSquashUnsigned},
		{reason: releaseInProgressStatusContext},
		{reason: requiredCheckNeverReportedPrefix + "unit"},
		{reason: "check-failing:unit"},
		{reason: nativeQueueReasonRejected},
		{reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeRequiresHuman},
		{reason: "linked-issue:hold:#7:hold"},
		{reason: "review-state-check", err: errors.New("boom"), want: true},
		{reason: autoMergeReasonForgeStateStale, want: true},
		{reason: autoMergeReasonForgeStateNotReady, want: true},
		{reason: autoMergeReasonCurrentBaseCheck, want: true},
	} {
		tracker := NewStageTracker()
		e := New(nil, Options{StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }, StageTracker: tracker})
		key := stageKey("acme", "widget", 7)
		tracker.trustedAuthorMerges = map[string]trustedAuthorMergesEntry{key: {head: "sha7", at: time.Now(), holdLabel: "hold"}}
		e.settleStageTrustedAuthorMerges("acme", "widget", 7, "sha7", "hold", tc.reason, tc.err)
		if _, got := stageTrustedAuthorMerges(tracker, key, "sha7", time.Now()); got != tc.want {
			t.Errorf("reason %q err %v: note = %v, want %v", tc.reason, tc.err, got, tc.want)
		}
	}
}

func prstageMergeTrainAdmittedReason() string {
	return prstage.MergeTrain{State: prstage.MergeTrainStateAdmitted, Front: 7, Size: 2}.Reason()
}

// trusted_authors switched off, a repository taken off its list, or
// auto-merge switched off for the repository: the lane no longer evaluates
// those PRs, so its notes go with it.
func TestTrustedAuthorSweepForgetsNotesItNoLongerOwns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy TrustedAuthorPolicy
	}{
		{name: "trusted_authors off", policy: TrustedAuthorPolicy{}},
		{name: "repository not listed", policy: TrustedAuthorPolicy{Enabled: true, RequireRole: "merger", Repos: map[string]bool{"acme/other": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			tracker := NewStageTracker()
			engine := newTrustedSweepEngine(api.URL, tc.policy, Options{
				StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
				StageTracker:     tracker,
			})
			key := stageKey("acme", "widget", 7)
			tracker.trustedAuthorMerges = map[string]trustedAuthorMergesEntry{key: {head: "sha7", at: time.Now(), holdLabel: "hold"}}
			if _, err := engine.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, ok := stageTrustedAuthorMerges(tracker, key, "sha7", time.Now()); ok {
				t.Fatal("the note outlived the lane")
			}
		})
	}
}

// The 085 comment follows the lane's note on one head: posted saying the PR
// will still merge, edited once to "will not merge" when the note goes, and
// edited once back when it returns. The causes explained on the head cannot
// tell the two wordings apart (hivecommons/hive#19421), and a pass with
// nothing changed writes nothing.
func TestDecisionCommentFollowsTrustedAuthorMergesOnOneHead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true,
		DecisionQuestion: "should the hive resolve its own review threads?", DecisionFinding: "the reviewer marked this change high-risk"}
	e, _ := humanStageEngine(t, api.url, clock, verdict, 0)
	e.SetReviewGate(&ReviewGate{})
	sweep := func(merges bool, labels ...string) {
		t.Helper()
		clock.at = clock.at.Add(time.Minute)
		pr := stagePR(41, append([]string{"role/trusted", "author/clanker"}, labels...)...)
		pr.User = &gh.User{Login: gh.Ptr("Danathar")}
		// A clanker PR (attribution trailer): the author/clanker label alone
		// does not make it one, and a person's own requires_human routes to
		// 071 instead (HumanAuthorOwesAnswer).
		pr.Body = gh.Ptr("Fixes the widget.\n\n-- hive: backend=pi model=gpt-6\n")
		head := ""
		if merges {
			head = pr.GetHead().GetSHA()
		}
		e.noteStageTrustedAuthorMerges("acme", "widget", 41, head, "hold")
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageVerdict("acme", "widget", 41, *verdict)
		e.noteStageRole("acme", "widget", 41, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	decisionComments := func() []string {
		var out []string
		for _, c := range api.comments {
			if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
				out = append(out, c)
			}
		}
		return out
	}
	step := func(name string, merges bool, wantEdits int, want string) {
		t.Helper()
		edits := len(api.commentEdits)
		sweep(merges, "stage/085-needs-decision")
		if got := decisionComments(); len(got) != 1 {
			t.Fatalf("%s: 085 comments posted = %d, want the one", name, len(got))
		}
		if n := len(api.commentEdits) - edits; n != wantEdits {
			t.Fatalf("%s: %d edits, want %d: %q", name, n, wantEdits, api.commentEdits[edits:])
		}
		if wantEdits > 0 && !strings.Contains(api.commentEdits[len(api.commentEdits)-1], want) {
			t.Fatalf("%s: edit missing %q:\n%s", name, want, api.commentEdits[len(api.commentEdits)-1])
		}
	}

	sweep(true, "stage/050-review-pending")
	if got := decisionComments(); len(got) != 1 || !strings.Contains(got[0], "Needs a decision, but the hive will still merge this PR") {
		t.Fatalf("085 comment = %q", got)
	}
	step("on again", true, 0, "")
	step("off", false, 1, "will not merge this unattended")
	step("off again", false, 0, "")
	step("on", true, 1, "Needs a decision, but the hive will still merge this PR")
	step("on again", true, 0, "")
}

// The trusted human's 071 note follows the lane's note on one head the same
// way: rewritten in place once per change, never posted again.
func TestTrustedHuman071NoteFollowsTrustedAuthorMergesOnOneHead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true,
		DecisionQuestion: "should this groundwork land before its consumer?"}
	e, _ := humanStageEngine(t, api.url, clock, verdict, 0)
	e.SetReviewGate(&ReviewGate{})
	sweep := func(merges bool, stageLabel string) {
		t.Helper()
		clock.at = clock.at.Add(time.Minute)
		pr := stagePR(51, "role/trusted", "author/human", stageLabel)
		pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
		head := ""
		if merges {
			head = pr.GetHead().GetSHA()
		}
		e.noteStageTrustedAuthorMerges("acme", "widget", 51, head, "hold")
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageVerdict("acme", "widget", 51, *verdict)
		e.noteStageRole("acme", "widget", 51, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	step := func(name string, merges bool, wantEdits int, want string) {
		t.Helper()
		edits := len(api.commentEdits)
		sweep(merges, "stage/071-needs-author")
		if got := humanNoteComments(api); len(got) != 1 {
			t.Fatalf("%s: 071 notes posted = %d, want the one", name, len(got))
		}
		if n := len(api.commentEdits) - edits; n != wantEdits {
			t.Fatalf("%s: %d edits, want %d: %q", name, n, wantEdits, api.commentEdits[edits:])
		}
		if wantEdits > 0 && !strings.Contains(api.commentEdits[len(api.commentEdits)-1], want) {
			t.Fatalf("%s: edit missing %q:\n%s", name, want, api.commentEdits[len(api.commentEdits)-1])
		}
	}

	sweep(true, "stage/050-review-pending")
	if got := humanNoteComments(api); len(got) != 1 || !strings.Contains(got[0], "but the hive will still merge it") {
		t.Fatalf("071 note = %q", got)
	}
	step("on again", true, 0, "")
	step("off", false, 1, "the hive will not merge it yet")
	step("off again", false, 0, "")
	step("on", true, 1, "but the hive will still merge it")
	step("on again", true, 0, "")
}

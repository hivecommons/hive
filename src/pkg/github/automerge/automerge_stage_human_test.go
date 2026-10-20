package automerge

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// humanStageEngine is newStageEngine plus a reviewer verdict for head "sha"
// (approve unless the test says otherwise), a 1h soak and a contributor
// tier below require_tiers.
func humanStageEngine(t *testing.T, apiURL string, clock *contributorLaneClock, verdict *ContributorLaneVerdict, soak time.Duration) (*Engine, *stagelabels.Store) {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	e := New(client, Options{
		Now:              clock.now,
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
		StageTracker:     NewStageTracker(),
		StageStore:       store,
		CheckSummary:     greenChecks,
		ContributorSoak:  NewContributorSoakTracker(),
		ContributorLanePolicy: func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, Soak: soak, RequireTiers: map[string]bool{"trusted": true}}
		},
		ContributorLaneVerdict: func(repo string, number int, head string) ContributorLaneVerdict {
			if !strings.HasPrefix(head, "sha") {
				return ContributorLaneVerdict{Configured: true}
			}
			return *verdict
		},
	})
	return e, store
}

func stageOf(t *testing.T, store *stagelabels.Store, number int) stagelabels.Stage {
	t.Helper()
	rec, ok := store.Get(stageKey("acme", "widget", number))
	if !ok {
		t.Fatalf("pr %d not tracked", number)
	}
	return rec.Stage
}

// TestHumanHandoffRoutesByEvidence drives one contributor PR whose only lane
// objection is trust through every head picture and checks where it lands
// (hivecommons/hive#12528).
func TestHumanHandoffRoutesByEvidence(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, time.Hour)
	ctx := context.Background()
	sweep := func(labels ...string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(9, labels...), "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), contributorLaneReasonClaimTier, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Approved + green + DCO yes, soak just started → 060, no comment.
	sweep()
	if got := stageOf(t, store, 9); got != stagelabels.Reviewed || len(api.comments) != 0 {
		t.Fatalf("soaking: stage=%v comments=%q", got, api.comments)
	}

	// Soak elapsed → 090 with the entry comment, once.
	clock.at = clock.at.Add(time.Hour)
	sweep("role/contributor", "author/human", "stage/060-reviewed")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("ready: stage=%v", got)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/090-ready-for-human-merge"}}) || !reflect.DeepEqual(api.removed, []string{"stage/060-reviewed"}) {
		t.Fatalf("ready writes added=%v removed=%v", api.added, api.removed)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.ReadyCommentMarker) ||
		!strings.Contains(api.comments[0], "verdict approved @sha, 3/3 checks green, DCO yes, soak 1h0m0s elapsed. Blocked only by `role/contributor` (below require_tiers).") {
		t.Fatalf("ready comment = %q", api.comments)
	}
	if strings.Contains(api.comments[0], "stage/090-needs-human") {
		t.Fatal("comment uses the old spelling")
	}
	sweep("role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("090 dedupe: comments=%q writes=%d", api.comments, api.writes())
	}

	// Verdict withdrawn for the head → 050, 090 label dropped, no regression.
	*verdict = ContributorLaneVerdict{Configured: true}
	sweep("role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if got := stageOf(t, store, 9); got != stagelabels.ReviewPending || !reflect.DeepEqual(api.removed, []string{"stage/090-ready-for-human-merge"}) {
		t.Fatalf("pending verdict: stage=%v removed=%v", got, api.removed)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); rec.Regressed() {
		t.Fatalf("090 → 050 regressed: %+v", rec)
	}

	// Changes requested → 071 (human author) with the verdict as the lane reason.
	*verdict = ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
	sweep("role/contributor", "author/human", "stage/050-review-pending")
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); rec.Stage != stagelabels.NeedsAuthor || rec.Reason != stagelabels.LaneReasonChangesRequested {
		t.Fatalf("changes requested: %+v", rec)
	}

	// Back to approve but a red check → 071 naming the check; DCO missing →
	// 071; conflicts → 071 (#12471: sentinel + conflicts is the author's
	// problem first).
	*verdict = ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 3, Green: 2, Red: []string{"lint"}, DCO: 1}, nil
	}
	clock.at = clock.at.Add(humanChecksTTL) // past the per-head check cache
	sweep("role/contributor", "author/human", "stage/071-needs-author")
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); rec.Stage != stagelabels.NeedsAuthor || rec.Reason != "check-failing:lint" {
		t.Fatalf("red check: %+v", rec)
	}
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 3, Green: 3, DCO: -1}, nil
	}
	clock.at = clock.at.Add(humanChecksTTL)
	sweep("role/contributor", "author/human", "stage/071-needs-author")
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); rec.Stage != stagelabels.NeedsAuthor || rec.Reason != stagelabels.LaneReasonDCOMissing {
		t.Fatalf("dco: %+v", rec)
	}
	e.checkSummary = greenChecks
	clock.at = clock.at.Add(humanChecksTTL)
	// needs-rebase is intercepted by classifyStage before routing; the
	// stage is 071 either way and the sentinel alert does not outrank it.
	sweep("role/contributor", "author/human", "stage/071-needs-author", "needs-rebase", "sentinel-alert")
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); rec.Stage != stagelabels.NeedsAuthor {
		t.Fatalf("conflict: %+v", rec)
	}

	// Sentinel alone (head otherwise mergeable) → 085, decision comment.
	clock.at = clock.at.Add(time.Hour)
	sweep("role/contributor", "author/human", "stage/071-needs-author", "sentinel-alert")
	if got := stageOf(t, store, 9); got != stagelabels.NeedsDecision {
		t.Fatalf("sentinel: stage=%v", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) || !strings.Contains(api.comments[0], "a sentinel alert is open on this PR") {
		t.Fatalf("085 comment = %q", api.comments)
	}
	// 085 → 090 when the alert clears is a flip, not a regression, and the
	// 090 comment is posted for this new visit.
	sweep("role/contributor", "author/human", "stage/085-needs-decision")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge || len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.ReadyCommentMarker) {
		t.Fatalf("alert cleared: stage=%v comments=%q", got, api.comments)
	}

	// Check digest unreadable → 060 (CI is a pending gate, never a pass).
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{}, errors.New("boom")
	}
	clock.at = clock.at.Add(humanChecksTTL)
	sweep("role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if got := stageOf(t, store, 9); got != stagelabels.Reviewed {
		t.Fatalf("unreadable checks: stage=%v", got)
	}
}

// TestHumanHandoffNewHeadWaits: a head nothing has reported on is 050 even
// with an approve verdict recorded for an older head.
func TestHumanHandoffNewHeadWaits(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{}, nil
	}
	pr := stagePR(10, "role/contributor", "author/human", "stage/090-ready-for-human-merge")
	pr.Head.SHA = ptrString("newhead")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoClaim, false, nil)
	e.noteStageRole("acme", "widget", 10, stagelabels.RoleUntrusted)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stageOf(t, store, 10); got != stagelabels.ReviewPending || len(api.comments) != 0 {
		t.Fatalf("new head: stage=%v comments=%q", got, api.comments)
	}
}

func ptrString(s string) *string { return &s }

// TestEnsureStageLabelsRenamesInPlace: a repo still carrying
// stage/090-needs-human gets it PATCHed to the new name (so PRs keep the
// label) instead of a second label being created.
func TestEnsureStageLabelsRenamesInPlace(t *testing.T) {
	api := &stageAPI{labels: map[string]string{"stage/090-needs-human": stagelabels.StageColor}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(11, "author/human", "stage/090-needs-human"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 11, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, old := api.labels["stage/090-needs-human"]; old {
		t.Fatalf("old label still present: %v", api.labels)
	}
	if api.labels["stage/090-ready-for-human-merge"] != stagelabels.StageColor || !reflect.DeepEqual(api.edits, []string{"stage/090-ready-for-human-merge=" + stagelabels.StageColor}) {
		t.Fatalf("rename edits=%v labels=%v", api.edits, api.labels)
	}
	for _, c := range api.creates {
		if c == "stage/090-ready-for-human-merge" {
			t.Fatal("renamed label was also created")
		}
	}
	// The PR itself: old spelling removed, 050 added (the verdict is pending).
	if !reflect.DeepEqual(api.added, [][]string{{"role/contributor", "stage/050-review-pending"}}) || !reflect.DeepEqual(api.removed, []string{"stage/090-needs-human"}) {
		t.Fatalf("pr writes added=%v removed=%v", api.added, api.removed)
	}

	// The withdrawn #12713 spelling of 072 is renamed back in place too.
	api3 := &stageAPI{labels: map[string]string{"stage/072-needs-author": stagelabels.StageColor}}
	srv3 := newStageAPI(t, api3)
	defer srv3.Close()
	e3, _, _ := newStageEngine(t, srv3.URL, clock)
	e3.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12, "author/human", "stage/072-needs-author"), contributorLaneReasonNoVerdict, false, nil)
	e3.noteStageRole("acme", "widget", 12, stagelabels.RoleContributor)
	if _, err := e3.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, old := api3.labels["stage/072-needs-author"]; old || api3.labels["stage/072-returned-to-contributor"] != stagelabels.StageColor || !slices.Contains(api3.edits, "stage/072-returned-to-contributor="+stagelabels.StageColor) {
		t.Fatalf("072 rename-back edits=%v labels=%v", api3.edits, api3.labels)
	}

	// A repo that already has both spellings: no PATCH, nothing created twice.
	api2 := &stageAPI{labels: map[string]string{"stage/090-needs-human": "123456", "stage/090-ready-for-human-merge": stagelabels.StageColor}}
	srv2 := newStageAPI(t, api2)
	defer srv2.Close()
	e2, _, _ := newStageEngine(t, srv2.URL, clock)
	e2.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12), contributorLaneReasonNoVerdict, false, nil)
	if _, err := e2.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api2.edits) != 0 || api2.labels["stage/090-needs-human"] != "123456" {
		t.Fatalf("both spellings: edits=%v labels=%v", api2.edits, api2.labels)
	}
}

func TestTrustAndDecisionCauseProse(t *testing.T) {
	if got := trustCause(contributorLaneReasonClaimTier, stagelabels.RoleNewcomer); got != "`role/newcomer` (below require_tiers)" {
		t.Fatalf("claim tier = %q", got)
	}
	if got := trustCause("untrusted-author-role", stagelabels.RoleUntrusted); got != "`role/untrusted` (no trusted-author role)" {
		t.Fatalf("role = %q", got)
	}
	if got := trustCause(contributorLaneReasonNoClaim, stagelabels.RoleNone); got != "the author's trust tier (no verified hive claim or registry tier)" {
		t.Fatalf("no claim = %q", got)
	}
	if decisionCause("excluded-label:hold") != "" || decisionCause(contributorLaneReasonSentinelBlocked) != "" || decisionCause("") != "" {
		t.Fatal("label-explained reasons must render empty")
	}
	if got := decisionCause(contributorLaneReasonWorkflowPath); !strings.Contains(got, "workflow") {
		t.Fatalf("workflow = %q", got)
	}
	if got := decisionCause(workflowApprovalRequiredPrefix + "ci/build"); !strings.Contains(got, "Hive posted `/ok-to-test`") {
		t.Fatalf("workflow approval = %q", got)
	}
	if got := decisionCause("something-new"); got != "the lane reported `something-new`" {
		t.Fatalf("fallback = %q", got)
	}
	checks, asks, _ := nudgeDiagnosisRouted("check-failing:lint", stageDetail{}, &stagelabels.Route{Reasons: []string{"red checks: lint, unit", "DCO sign-off missing", "merge conflict with the base branch", "verdict changes_requested @sha"}})
	if !reflect.DeepEqual(checks, []string{"lint", "unit"}) {
		t.Fatalf("routed checks = %v", checks)
	}
	if len(asks) != 3 || !strings.Contains(asks[0], "DCO") || !strings.Contains(asks[1], "rebase") || !strings.Contains(asks[2], "requested changes") {
		t.Fatalf("routed asks = %v", asks)
	}
	_, asks, _ = nudgeDiagnosisRouted(stagelabels.LaneReasonDCOMissing, stageDetail{}, nil)
	if len(asks) != 1 || !strings.Contains(asks[0], "DCO") {
		t.Fatalf("dco lane reason asks = %v", asks)
	}
}

// TestRequiresHumanLandsAt085WithEntryComment: a requires_human verdict the
// lane reports as contributor-lane:no-verdict routes through the human
// hand-off, so the PR gets stage/085-needs-decision AND the one-time
// <!-- hive:stage-needs-decision --> comment naming the verdict
// (hivecommons/hive#12560).
func TestRequiresHumanLandsAt085WithEntryComment(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()
	sweep := func(labels ...string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(13, labels...), contributorLaneReasonNoVerdict, false, nil)
		e.noteStageVerdict("acme", "widget", 13, *verdict)
		e.noteStageRole("acme", "widget", 13, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if got := humanVerdict(*verdict); got != stagelabels.VerdictRequiresHuman {
		t.Fatalf("humanVerdict = %v", got)
	}
	if decisionCause(stageReasonVerdictRequiresHuman) != "" {
		t.Fatal("verdict reason must not be rendered twice in the 085 comment")
	}

	sweep("role/contributor", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 13); got != stagelabels.NeedsDecision {
		t.Fatalf("stage=%v want 085", got)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/085-needs-decision"}}) || !reflect.DeepEqual(api.removed, []string{"stage/050-review-pending"}) {
		t.Fatalf("writes added=%v removed=%v", api.added, api.removed)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) ||
		!strings.Contains(api.comments[0], "the reviewer swarm returned `requires_human` @sha") ||
		strings.Contains(api.comments[0], "the lane reported") {
		t.Fatalf("085 entry comment = %q", api.comments)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 13)); rec.Reason != stageReasonVerdictRequiresHuman {
		t.Fatalf("recorded reason = %q", rec.Reason)
	}

	// Same state again: no second entry comment, no writes.
	sweep("role/contributor", "author/human", "stage/085-needs-decision")
	if len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("085 dedupe: comments=%q writes=%d", api.comments, api.writes())
	}
}

// TestEntryCommentOncePerStageAndHead: a same-head flap 085 -> 050 -> 085 does
// not repost the entry comment; a new head does (hivecommons/hive#12878).
func TestEntryCommentOncePerStageAndHead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()
	sweep := func(head string, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(13, labels...)
		pr.Head.SHA = gh.Ptr(head)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageVerdict("acme", "widget", 13, *verdict)
		e.noteStageRole("acme", "widget", 13, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	key := stageKey("acme", "widget", 13)

	sweep("sha", "role/contributor", "author/human", "stage/050-review-pending")
	if len(api.comments) != 1 {
		t.Fatalf("first entry comments = %q", api.comments)
	}
	store.Observe(key, stagelabels.ReviewPending, "review-requested")
	sweep("sha", "role/contributor", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 13); got != stagelabels.NeedsDecision {
		t.Fatalf("stage=%v want 085", got)
	}
	if len(api.comments) != 0 {
		t.Fatalf("same-head flap reposted: %q", api.comments)
	}
	if rec, _ := store.Get(key); !rec.EntryCommented {
		t.Fatalf("flag not set for the current visit: %+v", rec)
	}

	store.Observe(key, stagelabels.ReviewPending, "review-requested")
	sweep("sha2", "role/contributor", "author/human", "stage/050-review-pending")
	if len(api.comments) != 1 {
		t.Fatalf("new head comments = %q", api.comments)
	}
}

func TestDecisionRequestedLandsAt085WithQuestion(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{
		Configured:        true,
		Verdict:           "requires_human",
		DecisionRequested: true,
		DecisionQuestion:  "@clubanderson should decide whether this groundwork lands before its consumer.",
	}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()

	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(17, "role/contributor", "author/human", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageVerdict("acme", "widget", 17, *verdict)
	e.noteStageRole("acme", "widget", 17, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}

	if got := stageOf(t, store, 17); got != stagelabels.NeedsDecision {
		t.Fatalf("stage=%v want 085", got)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "reviewer question: @clubanderson should decide whether this groundwork lands before its consumer.") {
		t.Fatalf("085 entry comment = %q", api.comments)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 17)); rec.Reason != stageReasonDecisionRequested {
		t.Fatalf("recorded reason = %q", rec.Reason)
	}
}

func TestRequiresHumanLandsAt085WithSummary(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{
		Configured:     true,
		Verdict:        "requires_human",
		HumanSummaries: []string{"security requires human: Trust-surface file by location; a relay owner should confirm before merge."},
	}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()

	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(17, "author/contributor", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageVerdict("acme", "widget", 17, *verdict)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}

	if got := stageOf(t, store, 17); got != stagelabels.NeedsDecision {
		t.Fatalf("stage=%v want 085", got)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "reviewer summary: security requires human: Trust-surface file by location") {
		t.Fatalf("085 entry comment = %q", api.comments)
	}
}

func TestWorkflowApprovalSnippetRendersSelfContainedWorkflowForOwnAppLogin(t *testing.T) {
	e := &Engine{transport: stubTransport{}}
	got := e.workflowApprovalSnippet(workflowApprovalWorkflowMissingPrefix + "ci/build")
	for _, want := range []string{
		"<details>",
		"```yaml",
		"issue_comment:",
		`HIVE_APP_LOGINS: "` + testHiveAppBotLogin + `"`,
		"/actions/runs/${run_id}/approve",
		"actions: write",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("snippet missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "uses:") {
		t.Fatalf("snippet must be self-contained:\n%s", got)
	}
	if e.workflowApprovalSnippet(workflowApprovalRequiredPrefix+"ci/build") != "" || e.workflowApprovalSnippet("held") != "" {
		t.Fatal("snippet must only render for the missing-workflow reason")
	}
	var nilEngine *Engine
	if got := nilEngine.workflowApprovalSnippet(workflowApprovalWorkflowMissingPrefix + "x"); !strings.Contains(got, `HIVE_APP_LOGINS: "HIVE_APP_LOGIN[bot]"`) {
		t.Fatalf("nil engine should fall back to the placeholder login:\n%s", got)
	}
	if got := decisionCause(workflowApprovalWorkflowMissingPrefix + "ci/build"); !strings.Contains(got, "hivectl ok-to-test-template") || strings.Contains(got, "src/docs/templates") {
		t.Fatalf("missing-workflow cause = %q", got)
	}
}

// TestDecisionEntryRepostsWhenCauseChanges: a PR parked at 085 for a
// sentinel alert whose maintainer clears the alert, only for the lane's
// workflow-path fence to hold it, gets the new cause explained on the same
// head — once — instead of sitting under a stale "sentinel alert" comment.
func TestDecisionEntryRepostsWhenCauseChanges(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()
	sweep := func(reason string, labels ...string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(9, labels...), "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), reason, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
	if got := stageOf(t, store, 9); got != stagelabels.NeedsDecision {
		t.Fatalf("sentinel: stage=%v", got)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "a sentinel alert is open on this PR") {
		t.Fatalf("sentinel comment = %q", api.comments)
	}
	// Same cause again: no repost.
	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/085-needs-decision", "sentinel-alert")
	if len(api.comments) != 0 {
		t.Fatalf("same cause reposted: %q", api.comments)
	}
	// Alert cleared, workflow-path fence now the blocker: still 085, new cause explained.
	sweep(contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/085-needs-decision")
	if got := stageOf(t, store, 9); got != stagelabels.NeedsDecision {
		t.Fatalf("workflow path: stage=%v", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) ||
		!strings.Contains(api.comments[0], "edits CI workflow / action files") || strings.Contains(api.comments[0], "sentinel alert") {
		t.Fatalf("workflow-path comment = %q", api.comments)
	}
	rec, _ := store.Get(stageKey("acme", "widget", 9))
	if len(rec.EntryCommentedCauses) != 2 || !rec.EntryCommented || rec.EntryCommentedHead != "sha" {
		t.Fatalf("causes not recorded: %+v", rec)
	}
	// Flapping back to an already-explained cause stays quiet.
	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/085-needs-decision", "sentinel-alert")
	sweep(contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/085-needs-decision")
	if len(api.comments) != 0 {
		t.Fatalf("explained cause reposted: %q", api.comments)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); len(rec.EntryCommentedCauses) != 2 {
		t.Fatalf("cause list grew: %+v", rec)
	}
}

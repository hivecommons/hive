package automerge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
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
	if got := decisionCause(autoMergeReasonWorkflowPermission); !strings.Contains(got, "Workflows permission") || !strings.Contains(got, "A person must") || !strings.Contains(got, "if the installation lacks it") {
		t.Fatalf("workflow permission = %q", got)
	}
	// A fork head is out of the App's reach whatever it is granted: the cause
	// names who can update it and never asks for the workflows permission
	// (projectbluefin/utah#1001).
	if got := decisionCause(autoMergeReasonForkWorkflowUpdate); !strings.Contains(got, "in a fork") || !strings.Contains(got, "`gh pr update-branch`") || !strings.Contains(got, "Allow edits by maintainers") || strings.Contains(got, "Workflows: write") || strings.Contains(got, "permission") {
		t.Fatalf("fork workflow update = %q", got)
	}
	if got := decisionCause(workflowApprovalRequiredPrefix + "ci/build"); !strings.Contains(got, "Hive posted `/ok-to-test`") {
		t.Fatalf("workflow approval = %q", got)
	}
	if got := decisionCause("something-new"); got != "the lane reported `something-new`" {
		t.Fatalf("fallback = %q", got)
	}
	// The 085 comment names the rule GitHub refused the merge on.
	if got := decisionCause(autoMergeReasonMergeRefusedReview); !strings.Contains(got, "approving review") {
		t.Fatalf("review required = %q", got)
	}
	if got := decisionCause(autoMergeReasonMergeRefusedMethod); !strings.Contains(got, "merge method") || !strings.Contains(got, stagelabels.MergeCommitLabel) {
		t.Fatalf("method not allowed = %q", got)
	}
	// hivecommons/hive#13087: only the lane rules that allow no unattended
	// merge are manual-merge; a failed file read (fails closed) is not a rule.
	for _, r := range []string{contributorLaneReasonWorkflowPath, contributorLaneReasonTestsRemoved, autoMergeReasonHumanMergePath, autoMergeReasonMergeRefusedMethod} {
		if !manualMergeReason(r) {
			t.Fatalf("manualMergeReason(%q) = false", r)
		}
	}
	for _, r := range []string{autoMergeReasonHumanMergePathEvidence, stageReasonVerdictRequiresHuman, autoMergeReasonMergeRefusedReview, "held", "", "something-new"} {
		if manualMergeReason(r) {
			t.Fatalf("manualMergeReason(%q) = true", r)
		}
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
		// A clanker PR (attribution trailer): a person's requires_human
		// routes to 071 instead (hivecommons/hive#15202).
		pr := stagePR(13, labels...)
		pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
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

	sweep("role/contributor", "author/clanker", "stage/050-review-pending")
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
	// hivecommons/hive#13087: the contributor-lane comment names the release
	// that clears it — a trusted merger's approval, by review or reply.
	if !strings.Contains(api.comments[0], "**What to do:** a trusted merger approves this head") ||
		!strings.Contains(api.comments[0], "`/hive approve` reply posted after the verdict") ||
		strings.Contains(api.comments[0], "/hive decision") {
		t.Fatalf("085 entry comment does not name the contributor-lane action: %q", api.comments[0])
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 13)); rec.Reason != stageReasonVerdictRequiresHuman {
		t.Fatalf("recorded reason = %q", rec.Reason)
	}

	// Same state again: no second entry comment, no writes.
	sweep("role/contributor", "author/clanker", "stage/085-needs-decision")
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
		pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
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

	pr := stagePR(17, "role/contributor", "author/human", "stage/050-review-pending")
	pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
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

	pr := stagePR(17, "author/contributor", "stage/050-review-pending")
	pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
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

// TestRequiresHumanOnPersonPRRoutesToAuthor: a requires_human verdict on a
// PR a person pushed themselves is the author's question to answer, not a
// maintainer's decision (hivecommons/hive#15202). It lands at 071, the
// author is told what the reviewer asked whatever their role, and nothing
// ever queues adoption. A hive-authored PR has nobody to ask and keeps 085.
func TestRequiresHumanOnPersonPRRoutesToAuthor(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 20, 24, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{
		Configured:        true,
		Verdict:           "requires_human",
		DecisionRequested: true,
		DecisionQuestion:  "should this groundwork land before its consumer?",
	}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	ctx := context.Background()
	sweep := func(number int, role stagelabels.Role, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(number, labels...)
		pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
		if number == 23 {
			pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
		}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageVerdict("acme", "widget", number, *verdict)
		e.noteStageRole("acme", "widget", number, role)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sweep(21, stagelabels.RoleContributor, "role/contributor", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 21); got != stagelabels.NeedsAuthor {
		t.Fatalf("stage=%v want 071", got)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) {
		t.Fatalf("added=%v", api.added)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 21)); rec.Reason != stagelabels.LaneReasonReviewerQuestion {
		t.Fatalf("recorded reason = %q", rec.Reason)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], "<!-- hive:human-071-note") ||
		!strings.Contains(api.comments[0], "@clubanderson") ||
		!strings.Contains(api.comments[0], "the reviewer asked you: should this groundwork land before its consumer?") ||
		strings.Contains(api.comments[0], stagelabels.DecisionCommentMarker) {
		t.Fatalf("071 note = %q", api.comments)
	}
	// Same head again: told once.
	sweep(21, stagelabels.RoleContributor, "role/contributor", "author/human", "stage/071-needs-author")
	if len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("071 dedupe: comments=%q writes=%d", api.comments, api.writes())
	}

	// An owner (trusted role) gets the same route and note.
	sweep(22, stagelabels.RoleTrusted, "role/trusted", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 22); got != stagelabels.NeedsAuthor {
		t.Fatalf("owner stage=%v want 071", got)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "the reviewer asked you:") {
		t.Fatalf("owner 071 note = %q", api.comments)
	}

	// A hive-authored PR has no person to ask: still a decision.
	sweep(23, stagelabels.RoleTrusted, "role/trusted", "author/hive", "stage/050-review-pending")
	if got := stageOf(t, store, 23); got != stagelabels.NeedsDecision {
		t.Fatalf("hive-authored stage=%v want 085", got)
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

	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/human", "stage/050-review-pending", "sentinel-alert")
	if got := stageOf(t, store, 9); got != stagelabels.NeedsDecision {
		t.Fatalf("sentinel: stage=%v", got)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "a sentinel alert is open on this PR") {
		t.Fatalf("sentinel comment = %q", api.comments)
	}
	// Same cause again: no repost.
	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/human", "stage/085-needs-decision", "sentinel-alert")
	if len(api.comments) != 0 {
		t.Fatalf("same cause reposted: %q", api.comments)
	}
	// Alert cleared, workflow-path fence now the blocker: still 085, new cause explained.
	sweep(contributorLaneReasonWorkflowPath, "role/trusted", "author/human", "stage/085-needs-decision")
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
	sweep(contributorLaneReasonSentinelBlocked, "role/trusted", "author/human", "stage/085-needs-decision", "sentinel-alert")
	sweep(contributorLaneReasonWorkflowPath, "role/trusted", "author/human", "stage/085-needs-decision")
	if len(api.comments) != 0 {
		t.Fatalf("explained cause reposted: %q", api.comments)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 9)); len(rec.EntryCommentedCauses) != 2 {
		t.Fatalf("cause list grew: %+v", rec)
	}
}

// Admission skips carry no CI state in their reasons,
// so the human route's check digest decides whether the 050 head waits on
// the hive (hivecommons/hive#15009, #15164): running checks do, a settled head with
// no verdict does not.
func TestHumanHandoffAdmissionSkipPendingChecksWaitOnHive(t *testing.T) {
	for _, reason := range []string{
		contributorLaneReasonNoLinkedIssue, contributorLaneReasonNoClaim, contributorLaneReasonClaimUnverified,
		contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive, contributorLaneReasonClaimTier,
		contributorLaneReasonAuthorAssociation,
	} {
		t.Run(reason, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
			verdict := &ContributorLaneVerdict{Configured: true}
			e, store := humanStageEngine(t, api.url, clock, verdict, 0)
			e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
				return hgithub.CommitChecksSummary{Total: 3, Green: 1, Pending: 2, DCO: 1}, nil
			}
			sweep := func() {
				t.Helper()
				pr := stagePR(11, "role/contributor", "author/human")
				pr.Head.SHA = ptrString("sha11")
				e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
				e.noteStageRole("acme", "widget", 11, stagelabels.RoleContributor)
				if _, err := e.ApplyStageLabels(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			sweep()
			rec, _ := store.Get(stageKey("acme", "widget", 11))
			if rec.Stage != stagelabels.ReviewPending || !rec.WaitingOnHive {
				t.Fatalf("pending checks: stage=%v waiting_on_hive=%v", rec.Stage, rec.WaitingOnHive)
			}

			e.checkSummary = greenChecks
			clock.at = clock.at.Add(humanChecksTTL)
			sweep()
			rec, _ = store.Get(stageKey("acme", "widget", 11))
			if rec.Stage != stagelabels.ReviewPending || rec.WaitingOnHive {
				t.Fatalf("checks settled, no verdict: stage=%v waiting_on_hive=%v", rec.Stage, rec.WaitingOnHive)
			}

			// Once settled, dwelling without a verdict must not blame the reviewer.
			clock.at = clock.at.Add(2 * time.Hour)
			sweep()
			rec, _ = store.Get(stageKey("acme", "widget", 11))
			if rec.WaitingOnHive || rec.HiveHeldWarned || stagelabels.HiveHeld(rec, nil, clock.at) {
				t.Fatalf("admission skip blamed the hive after dwell: %+v", rec)
			}
		})
	}
}

// stageSupersededCloserSpy is supersededCloserSpy that still supplies the
// stage writer the label pass needs.
type stageSupersededCloserSpy struct {
	*supersededCloserSpy
	stage stageReconcilerTransport
}

func (s *stageSupersededCloserSpy) StageReconciler(store *stagelabels.Store, opts *stageengine.Options, logger *slog.Logger) stageengine.Reconciler {
	return s.stage.StageReconciler(store, opts, logger)
}

func newStageSupersededCloserSpy(t *testing.T, e *Engine, result hgithub.HiveSupersededClose) *stageSupersededCloserSpy {
	t.Helper()
	stage, ok := e.transport.(stageReconcilerTransport)
	if !ok {
		t.Fatal("engine transport provides no stage writer")
	}
	spy := &stageSupersededCloserSpy{supersededCloserSpy: &supersededCloserSpy{Transport: e.transport, result: result}, stage: stage}
	e.transport = spy
	return spy
}

// TestApplyStageLabelsHivePRSupersededClosesInsteadOfHumanRoute is
// hivecommons/hive#15208: the reviewer swarm answered requires_human on a
// hive-authored PR whose issue #15206 had just merged and closed. The pass
// asks the superseded rule before routing to a person; a close means no
// 085 label, no record, and the result counts it.
func TestApplyStageLabelsHivePRSupersededClosesInsteadOfHumanRoute(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	var prReads int
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/15208" {
			prReads++
			_, _ = io.WriteString(w, `{"number":15208,"state":"open","user":{"login":"`+testHiveAppBotLogin+`","type":"Bot"},"head":{"sha":"sha"},"base":{"ref":"v5"},"mergeable_state":"clean"}`)
			return true
		}
		return false
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 21, 2, 41, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{"intent-alignment requires human: Solves child (b) of #15188; conflicts with #15206 (owner PR for the same issue)"}}
	e.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return requiresHuman }
	spy := newStageSupersededCloserSpy(t, e, hgithub.HiveSupersededClose{Closed: true, Superseded: true, CloserPR: 15206, Issue: 15188, Verdict: "close", Reason: hgithub.SupersessionActionClosedDeclared})
	ctx := context.Background()
	observe := func() StageLabelResult {
		pr := stagePR(15208, "stage/050-review-pending", "role/trusted", "author/hive")
		pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "reviewer-verdict:requires_human", false, nil)
		e.noteStageRole("acme", "widget", 15208, stagelabels.RoleTrusted)
		e.noteStageVerdict("acme", "widget", 15208, requiresHuman)
		res, err := e.ApplyStageLabels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := observe()
	if res.ClosedSuperseded != 1 || len(spy.calls) != 1 || spy.calls[0] != 15208 || prReads != 1 {
		t.Fatalf("result=%+v calls=%v reads=%d, want one close", res, spy.calls, prReads)
	}
	if api.writes() != 0 {
		t.Fatalf("a closed PR gets no 085 label: added=%v removed=%v", api.added, api.removed)
	}
	if _, ok := store.Get(stageKey("acme", "widget", 15208)); ok {
		t.Fatal("closed PR must leave no stage record")
	}

	// The rule keeps the PR: the reviewer's ask routes to 085 as before,
	// and within the TTL the same head is not re-read.
	spy.result = hgithub.HiveSupersededClose{Verdict: "keep", Reason: "not-superseded"}
	api.reset()
	res = observe()
	if res.ClosedSuperseded != 0 || !reflect.DeepEqual(api.added, [][]string{{"stage/085-needs-decision"}}) {
		t.Fatalf("kept PR must reach 085: result=%+v added=%v", res, api.added)
	}
	if len(spy.calls) != 1 || prReads != 1 {
		t.Fatalf("same head within the TTL must not be re-read: calls=%v reads=%d", spy.calls, prReads)
	}
	clock.advance(supersededCheckTTL + time.Minute)
	observe()
	if len(spy.calls) != 2 {
		t.Fatalf("past the TTL the check runs again: calls=%v", spy.calls)
	}
}

// TestApplyStageLabelsHumanPRNeverAskedSupersededCloser: the short-circuit
// is for the hive's own PRs only; a person's PR at requires_human is routed
// to a person without a PR read.
func TestApplyStageLabelsHumanPRNeverAskedSupersededCloser(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 21, 2, 41, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	e.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return requiresHuman }
	spy := newStageSupersededCloserSpy(t, e, hgithub.HiveSupersededClose{Closed: true})
	pr := stagePR(337, "stage/050-review-pending", "role/contributor", "author/human")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonAuthorAssociation, false, nil)
	e.noteStageRole("acme", "widget", 337, stagelabels.RoleContributor)
	e.noteStageVerdict("acme", "widget", 337, requiresHuman)
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer's question on a person's own PR goes to the author
	// (071, hivecommons/hive#15202); the point here is that the closer is
	// never asked and no PR read happens.
	if len(spy.calls) != 0 || res.ClosedSuperseded != 0 || !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) {
		t.Fatalf("calls=%v result=%+v added=%v", spy.calls, res, api.added)
	}
}

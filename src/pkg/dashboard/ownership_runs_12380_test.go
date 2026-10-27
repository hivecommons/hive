package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Runs waiting on a person and approval-desk items parked for the operator
// reach the Your-turn inbox (hivecommons/hive#12380).

func humanRun12380(key, stage, reason string) Run {
	return Run{Key: key, Repo: "o/r", State: "active", Stage: stage, WaitingOn: RunWaitingOnHuman, WaitingReason: reason}
}

// repos12380 is one repository with a claimed, leased, stale issue under a
// run (#5), a second untouched issue (#6) and a hive PR (#9).
func repos12380() []FrontendRepo {
	old := ownershipNow11565.AddDate(0, 0, -30)
	return []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{
			github.Issue{Repo: "o/r", Number: 5, Labels: []string{"hive/claimed-by-scanner"}, Lane: "quality", CreatedAt: old, UpdatedAt: old},
			github.Issue{Repo: "o/r", Number: 6, CreatedAt: old, UpdatedAt: old},
		},
		OpenPrs: []any{github.PullRequest{Repo: "o/r", Number: 9, HiveAgent: "scanner", CreatedAt: ownershipNow11565.Add(-time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour)}},
	}}
}

func hub12380(t *testing.T, s *Server) *ContributeWSHub {
	t.Helper()
	hub := s.ContributeHub()
	if hub == nil {
		t.Fatal("the test server registers no contribute hub")
	}
	return hub
}

func env12380(waits *operatorWaits) ownershipEnv {
	env := ownershipEnv11565(4)
	env.leased = func(repo string, number int) bool { return number == 5 }
	env.operatorWaits = waits
	return env
}

func TestRunWaitStatesBecomeYourTurn12380(t *testing.T) {
	tests := []struct {
		name string
		run  Run
		code string
		text string
	}{
		{"plan checkpoint", humanRun12380("o/r#5", StagePlan, "checkpoint_enabled"), OwnershipReasonRunCheckpoint, "checkpoint"},
		{"spec checkpoint", humanRun12380("o/r#5", StageSpec, "checkpoint_enabled"), OwnershipReasonRunCheckpoint, "checkpoint"},
		{"implement checkpoint", humanRun12380("o/r#5", StageImplement, ""), OwnershipReasonRunCheckpoint, "checkpoint"},
		{"design review", humanRun12380("o/r#5", StagePlan, planning.PlanStateDesignReview), OwnershipReasonRunCheckpoint, "approve or reject"},
		{"interview", humanRun12380("o/r#5", StageSpec, worksource.RunWaitingReasonInterviewQuestions), OwnershipReasonRunCheckpoint, "interview"},
		{"stale plan", humanRun12380("o/r#5", StagePlan, planning.WaitingReasonStalePlan), OwnershipReasonRunStalePlan, "stale"},
		{"stage exhausted", humanRun12380("o/r#5", StageImplement, runWaitingReasonStageBudgetExhausted), OwnershipReasonRunStageExhausted, "reset the stage or abandon"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			waits := newOperatorWaits("o")
			waits.addRuns([]Run{tc.run})
			repos := repos12380()
			decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(waits))
			got := ownershipFor(repos[0].Ownership, ownershipKindIssue, 5)
			if got == nil || !got.YourTurn || got.ReasonCode != tc.code || got.Verb != OwnershipVerbReview {
				t.Fatalf("issue #5 = %+v, want your turn REVIEW/%s even though it is claimed and leased", got, tc.code)
			}
			if !strings.Contains(got.Reason, tc.text) || got.Link != ownershipLinkRuns || got.InboxRank == 0 {
				t.Fatalf("issue #5 = %+v, want a reason naming %q, the Runs link and an inbox rank", got, tc.text)
			}
		})
	}
}

func TestRunWaitClearsAndSkipsAgentWaits12380(t *testing.T) {
	// Without a run wait the claimed, leased, stale issue is not your turn:
	// the lease switches the safety net off.
	repos := repos12380()
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(nil))
	checkOwnership11565(t, *ownershipFor(repos[0].Ownership, ownershipKindIssue, 5), notYours11565)

	// A run waiting on an agent, a completed run, and a run on a work item with
	// no issue number add nothing.
	waits := newOperatorWaits("o")
	agentRun := humanRun12380("o/r#5", StagePlan, "")
	agentRun.WaitingOn = RunWaitingOnAgent
	done := humanRun12380("o/r#5", StagePlan, "")
	done.State = "completed"
	external := humanRun12380("o/r!LIN-7", StagePlan, "")
	waits.addRuns([]Run{agentRun, done, external})
	if len(waits.items) != 0 {
		t.Fatalf("waits = %v, want none", waits.items)
	}
	repos = repos12380()
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(waits))
	checkOwnership11565(t, *ownershipFor(repos[0].Ownership, ownershipKindIssue, 5), notYours11565)
}

func TestRunWaitOutranksSafetyNetButNotOtherYourTurn12380(t *testing.T) {
	waits := newOperatorWaits("o")
	waits.addRuns([]Run{humanRun12380("r#6", StagePlan, "")})
	if got := waits.apply(ownershipKindIssue, "o/r", 6, safetyNetOwnership(30)); got.ReasonCode != OwnershipReasonRunCheckpoint {
		t.Fatalf("a run wait must replace the safety net, got %+v", got)
	}
	if got := waits.apply(ownershipKindIssue, "O/R", 6, waitingElsewhere()); got.ReasonCode != OwnershipReasonRunCheckpoint {
		t.Fatalf("a run wait must replace a not-your-turn decision, got %+v", got)
	}
	sentinel := yourTurn(OwnershipReasonSentinelFlagged, "flagged")
	if got := waits.apply(ownershipKindIssue, "o/r", 6, sentinel); got != sentinel {
		t.Fatalf("a run wait must keep another your-turn decision, got %+v", got)
	}
	if got := waits.apply(ownershipKindPR, "o/r", 6, notYourTurn()); got.YourTurn {
		t.Fatalf("a run wait marks the issue, not a PR with the same number: %+v", got)
	}
	var none *operatorWaits
	if got := none.apply(ownershipKindIssue, "o/r", 6, notYourTurn()); got.YourTurn {
		t.Fatalf("nil waits must add nothing: %+v", got)
	}
}

// TestRunWaitNeedsNoEscalationSink12380: the inbox row comes from the run
// projection alone, so a hive with no escalation sink (no dispatcher, no
// notifier) still sees it.
func TestRunWaitNeedsNoEscalationSink12380(t *testing.T) {
	s, _ := apiServer(t)
	w := collectOperatorWaits(hub12380(t, s), "o")
	if w == nil {
		t.Fatal("a server-backed hub must yield a waits record")
	}
	w.addRuns([]Run{humanRun12380("o/r#5", StageImplement, runWaitingReasonStageBudgetExhausted)})
	repos := repos12380()
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(w))
	checkOwnership11565(t, *ownershipFor(repos[0].Ownership, ownershipKindIssue, 5), want11565{OwnershipVerbReview, OwnershipReasonRunStageExhausted})
	if collectOperatorWaits(nil, "o") != nil || collectOperatorWaits(NewContributeWSHub(nil, nil), "o") != nil {
		t.Fatal("no server means no waits")
	}
}

func TestApprovalDeskItemsBecomeYourTurn12380(t *testing.T) {
	s, inbox := approvalServer(t, 2)
	waits := collectOperatorWaits(hub12380(t, s), "hivecommons")
	repos := []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{
			github.PullRequest{Repo: "hivecommons/hive", Number: 1, HiveAgent: "scanner", CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565},
			github.PullRequest{Repo: "hivecommons/hive", Number: 2, HiveAgent: "scanner", CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565},
		},
	}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(waits))
	for _, n := range []int{1, 2} {
		got := ownershipFor(repos[0].Ownership, ownershipKindPR, n)
		if got == nil || !got.YourTurn || got.ReasonCode != OwnershipReasonApprovalPending || got.Link != ownershipLinkApprovals {
			t.Fatalf("PR #%d = %+v, want your turn approval_pending linking to Approvals", n, got)
		}
		if !strings.Contains(got.Reason, "Self-merge") || !strings.Contains(got.Reason, "needs-human") {
			t.Fatalf("PR #%d reason = %q, want the self-merge and the desk rule", n, got.Reason)
		}
	}

	// Resolving an item in the desk clears its row on the next refresh.
	var first string
	for _, p := range inbox.List() {
		if p.Request.Number == 1 {
			first = p.ID
		}
	}
	if _, err := inbox.Resolve(first, true, "owner", "ok"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	waits = collectOperatorWaits(hub12380(t, s), "hivecommons")
	repos[0].Ownership = nil
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env12380(waits))
	checkOwnership11565(t, *ownershipFor(repos[0].Ownership, ownershipKindPR, 1), notYours11565)
	checkOwnership11565(t, *ownershipFor(repos[0].Ownership, ownershipKindPR, 2), want11565{OwnershipVerbReview, OwnershipReasonApprovalPending})
}

func TestApprovalDeskDisabledAddsNoRows12380(t *testing.T) {
	s, _ := apiServer(t)
	waits := collectOperatorWaits(hub12380(t, s), "hivecommons")
	for key, o := range waits.items {
		if o.ReasonCode == OwnershipReasonApprovalPending {
			t.Fatalf("desk disabled, yet %s is an approval row", key)
		}
	}
}

func TestApprovalWaitKinds12380(t *testing.T) {
	waits := newOperatorWaits("o")
	waits.addApprovals([]toolapprove.PendingItem{
		{Request: toolapprove.Request{Kind: toolapprove.KindQueuedMerge, Repo: "r", Number: 3}},
		{Request: toolapprove.Request{Kind: toolapprove.KindPlanApproval, Repo: "o/r", Number: 4}},
		{Request: toolapprove.Request{Kind: toolapprove.KindAgentTool, Repo: "o/r", Number: 7}},
		{Request: toolapprove.Request{Kind: toolapprove.KindSelfMerge, Repo: "o/r"}},
	})
	if _, ok := waits.items[waits.key(ownershipKindPR, "o/r", 3)]; !ok {
		t.Errorf("a queued merge marks its PR")
	}
	if _, ok := waits.items[waits.key(ownershipKindIssue, "o/r", 4)]; !ok {
		t.Errorf("a plan approval marks its issue")
	}
	if len(waits.items) != 2 {
		t.Fatalf("waits = %v, want only the merge and the plan approval", waits.items)
	}
}

func TestYourTurnMenuLinksRunAndApprovalWaits12380(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
const base = { kind: 'issue', url: 'https://github.com/o/r/issues/5', isOwner: true, planningAllowed: true, hasPlan: false, canToggleHold: true, held: false };
const kinds = own => yourTurnMenuModel(Object.assign({}, base, { own })).entries.map(e => e.type + ':' + (e.label || e.text || e.id));
assert.deepEqual(kinds({ your_turn: true, verb: 'REVIEW', reason_code: 'run_checkpoint', reason: 'r', link: '#section-runs-section' }), ['link:Open in Runs', 'link:Open on GitHub']);
assert.deepEqual(kinds({ your_turn: true, verb: 'REVIEW', reason_code: 'approval_pending', reason: 'r', link: '#section-approvals-section' }), ['link:Open in Approvals', 'link:Open on GitHub']);
const m = yourTurnMenuModel(Object.assign({}, base, { own: { your_turn: true, verb: 'REVIEW', reason_code: 'run_stale_plan', reason: 'r', link: '#section-runs-section' } }));
const html = yourTurnMenuHTML(m, { repo: 'o/r', number: 5, url: base.url });
assert.match(html, /<a role="menuitem" tabindex="-1" class="yt-item" href="#section-runs-section">Open in Runs<\/a>/, 'the dashboard link opens in the same tab');
`, "yourTurnMenuModel", "yourTurnMenuHTML")
	runYourTurnNode(t, script)
}

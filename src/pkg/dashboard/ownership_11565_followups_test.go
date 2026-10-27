package dashboard

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// useOwnershipTempState points the your-turn local record and the escalation
// ledger at a temp dir (no ledger file) and returns the state path.
func useOwnershipTempState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldState, oldLedger := ownershipStatePath, ownershipEscalationLedgerPath
	ownershipStatePath = filepath.Join(dir, "ownership-state.json")
	ownershipEscalationLedgerPath = filepath.Join(dir, "missing-ledger.json")
	t.Cleanup(func() { ownershipStatePath, ownershipEscalationLedgerPath = oldState, oldLedger })
	return ownershipStatePath
}

func ownershipCfg11565(level int, agents map[string]config.AgentConfig, policies ...config.RepoPolicy) *config.Config {
	cfg := &config.Config{ACMMLevel: &level, AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: agents}}
	cfg.Project.Org = "o"
	cfg.Project.Repos = []string{"r", "x"}
	cfg.Project.RepoPolicies = policies
	return cfg
}

func agentIssueRepo11565(name string, number int) FrontendRepo {
	old := ownershipNow11565.Add(-time.Hour)
	return FrontendRepo{
		Name: name, Full: "o/" + name,
		ActionableIssues: []any{github.Issue{Repo: "o/" + name, Number: number, Labels: []string{"agent/scanner"}, CreatedAt: old, UpdatedAt: old}},
	}
}

func decision11565(t *testing.T, repo FrontendRepo, kind string, number int) Ownership {
	t.Helper()
	decision, ok := repo.Ownership[ownershipKey(kind, number)]
	if !ok {
		t.Fatalf("no decision for %s#%d", kind, number)
	}
	return decision
}

func TestOwnershipCoveredByOpenLinkedPR11565(t *testing.T) {
	env := ownershipEnv11565(5)
	covered := func(open bool) issueOwnershipInput {
		in := freshIssue11565("hive/covered-by-pr")
		in.openLinkedPR = open
		return in
	}
	checkOwnership11565(t, decideIssueOwnership(covered(true), env), notYours11565)
	checkOwnership11565(t, decideIssueOwnership(covered(false), env), want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone})

	// An open linked PR is rule 3: the safety net never covers the issue.
	stale := inLane11592(freshIssue11565(), "quality")
	stale.openLinkedPR = true
	stale.createdAt = ownershipNow11565.AddDate(0, 0, -60)
	stale.updatedAt = stale.createdAt
	checkOwnership11565(t, decideIssueOwnership(stale, env), notYours11565)
	stale.openLinkedPR = false
	checkOwnership11565(t, decideIssueOwnership(stale, env), want11565{OwnershipVerbReview, OwnershipReasonStaleSafetyNet})
	// Rule 3 is someone else's move, so the Overview equation counts it as
	// waiting elsewhere (part D1).
	stale.openLinkedPR = true
	if got := decideIssueOwnership(stale, env); !got.WaitingElsewhere {
		t.Fatalf("open linked PR: WaitingElsewhere = false, want true (%+v)", got)
	}
	stale.openLinkedPR = false

	tests := []struct {
		name  string
		issue github.Issue
		want  bool
	}{
		{"none", github.Issue{}, false},
		{"open link", github.Issue{LinkedPRs: []github.IssueLinkedPR{{Number: 4, State: "open"}}}, true},
		{"merged link", github.Issue{LinkedPRs: []github.IssueLinkedPR{{Number: 4, State: "merged", Merged: true}}}, false},
		{"open claim context", github.Issue{ClaimContext: &github.IssueClaimContext{PRNumber: 4, Decision: "covered_by_open_pr"}}, true},
		{"merged claim context", github.Issue{ClaimContext: &github.IssueClaimContext{PRNumber: 4, MergedPR: true, Decision: "merged_pr_needs_verification"}}, false},
	}
	for _, tt := range tests {
		if got := issueHasOpenLinkedPR(tt.issue); got != tt.want {
			t.Fatalf("%s: issueHasOpenLinkedPR = %v, want %v", tt.name, got, tt.want)
		}
	}

	// End to end: an actionable issue still labelled covered-by-pr whose PR
	// is open is not your turn.
	repos := []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{github.Issue{Repo: "o/r", Number: 3, Labels: []string{"hive/covered-by-pr"}, LinkedPRs: []github.IssueLinkedPR{{Number: 9, State: "open"}}, CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565}},
	}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	if got := decision11565(t, repos[0], ownershipKindIssue, 3); got.YourTurn {
		t.Fatalf("covered issue with an open linked PR is your turn: %+v", got)
	}
}

func TestNonActionableIssueRowOwnership11565(t *testing.T) {
	old := ownershipNow11565.Add(-time.Hour)
	repos := []FrontendRepo{{
		Name: "r", Full: "o/r",
		NonActionableIssues: []any{
			// F3: an agent-filed issue a person acknowledged keeps that
			// acknowledgement when a lane claims it.
			github.RepoWorkIssue{Repo: "o/r", Number: 1, Labels: []string{"agent/scanner"}, HumanAcknowledged: true, Bucket: repoWorkBucketFiltered, Reason: "Exempt label", CreatedAt: old, UpdatedAt: old},
			github.RepoWorkIssue{Repo: "o/r", Number: 2, Labels: []string{"agent/scanner"}, Bucket: repoWorkBucketFiltered, Reason: "Exempt label", CreatedAt: old, UpdatedAt: old},
			// F4: the ordered label check beats the recorded bucket.
			github.RepoWorkIssue{Repo: "o/r", Number: 3, Labels: []string{"needs-decision", "needs-human"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: old, UpdatedAt: old},
			// ...and the recorded bucket is the fallback when no label says.
			github.RepoWorkIssue{Repo: "o/r", Number: 4, Labels: []string{"renamed"}, Bucket: repoWorkBucketFiltered, Reason: "needs-spec", CreatedAt: old, UpdatedAt: old},
			// Renamed parking labels are honoured in the same order.
			github.RepoWorkIssue{Repo: "o/r", Number: 5, Labels: []string{"waiting-on-spec", "waiting-on-owner"}, Bucket: repoWorkBucketFiltered, Reason: "needs-spec", CreatedAt: old, UpdatedAt: old},
		},
	}}
	env := ownershipEnv11565(5)
	env.parking = ownershipParkingFor(config.HardSuppressLabelsConfig{NeedsHuman: []string{"needs-human", "waiting-on-owner"}, NeedsSpec: []string{"needs-spec", "waiting-on-spec"}})
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)

	if got := decision11565(t, repos[0], ownershipKindIssue, 1); got.YourTurn {
		t.Fatalf("acknowledged non-actionable agent issue is your turn: %+v", got)
	}
	if got := decision11565(t, repos[0], ownershipKindIssue, 2); got.ReasonCode != OwnershipReasonAgentFiledUnacknowledged {
		t.Fatalf("unacknowledged non-actionable agent issue = %+v", got)
	}
	for number, want := range map[int]want11565{
		3: {OwnershipVerbReview, OwnershipReasonParkedNeedsHuman},
		4: {OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec},
		5: {OwnershipVerbReview, OwnershipReasonParkedNeedsHuman},
	} {
		checkOwnership11565(t, decision11565(t, repos[0], ownershipKindIssue, number), want)
	}
}

func TestRecordEscalationFirstSeenPrunesOnlyEnumeratedRepos11565(t *testing.T) {
	useOwnershipTempState(t)
	pr := FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 8, HiveAgent: "scanner", Labels: []string{"needs-human"}}}
	seed := []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{pr}}}
	first := ownershipNow11565
	recordEscalationFirstSeen(seed, first)

	empty := func(mutate func(*FrontendRepo)) []FrontendRepo {
		repo := FrontendRepo{Name: "r", Full: "o/r", OpenPrs: []any{}}
		mutate(&repo)
		return []FrontendRepo{repo}
	}
	for name, repos := range map[string][]FrontendRepo{
		"paused repo":         empty(func(r *FrontendRepo) { r.Paused = true }),
		"no enumeration data": empty(func(r *FrontendRepo) { r.ownershipDataMissing = true }),
		"incomplete counts":   empty(func(r *FrontendRepo) { r.CountsIncomplete = true }),
	} {
		recordEscalationFirstSeen(repos, first.Add(time.Hour))
		if got := recordEscalationFirstSeen(seed, first.Add(2*time.Hour))("o/r", 8); !got.Equal(first) {
			t.Fatalf("%s pruned the first-seen entry: got %v", name, got)
		}
	}
	// An enumerated repo that no longer shows the PR does prune it.
	recordEscalationFirstSeen(empty(func(*FrontendRepo) {}), first.Add(3*time.Hour))
	if got := recordEscalationFirstSeen(empty(func(*FrontendRepo) {}), first.Add(4*time.Hour))("o/r", 8); !got.IsZero() {
		t.Fatalf("enumerated repo kept the first-seen entry: %v", got)
	}
}

func TestReviewerLaneAvailablePauseCadence11565(t *testing.T) {
	cfg := ownershipCfg11565(5, map[string]config.AgentConfig{"adjudicator": {Enabled: true, Role: "reviewer"}})
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"adjudicator": "30m"}},
		"busy": {Cadences: map[string]config.Cadence{"adjudicator": "pause"}},
		"off":  {Cadences: map[string]config.Cadence{"adjudicator": "off"}},
	}
	for mode, want := range map[string]bool{"idle": true, "busy": false, "off": false} {
		if got := reviewerLaneAvailable(cfg, nil, mode, "o/r"); got != want {
			t.Fatalf("mode %s: reviewerLaneAvailable = %v, want %v", mode, got, want)
		}
	}
}

func TestOwnershipConfigBackedLevels11565(t *testing.T) {
	useOwnershipTempState(t)
	override := func(v bool) *bool { return &v }
	agentIssueTurn := func(cfg *config.Config, name string) Ownership {
		t.Helper()
		repos := []FrontendRepo{agentIssueRepo11565(name, 1)}
		applyOwnershipDecisions(cfg, repos, nil, "idle", ownershipNow11565, nil)
		return decision11565(t, repos[0], ownershipKindIssue, 1)
	}

	// L5 -> L6 removes TRIAGE; back to L5 restores it.
	cfg := ownershipCfg11565(5, nil)
	checkOwnership11565(t, agentIssueTurn(cfg, "r"), want11565{OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged})
	l6 := 6
	cfg.ACMMLevel = &l6
	checkOwnership11565(t, agentIssueTurn(cfg, "r"), notYours11565)
	l5 := 5
	cfg.ACMMLevel = &l5
	checkOwnership11565(t, agentIssueTurn(cfg, "r"), want11565{OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged})

	// A repository's self-authorization-hold override restores TRIAGE for
	// that repository only.
	cfg = ownershipCfg11565(6, nil, config.RepoPolicy{Repo: "r", SelfAuthorizationHold: override(true)})
	checkOwnership11565(t, agentIssueTurn(cfg, "r"), want11565{OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged})
	checkOwnership11565(t, agentIssueTurn(cfg, "x"), notYours11565)

	// A repository level override alone changes nothing: the hold follows
	// the hive-wide level.
	cfg = ownershipCfg11565(6, nil, config.RepoPolicy{Repo: "r", ACMMLevel: &l5})
	checkOwnership11565(t, agentIssueTurn(cfg, "r"), notYours11565)
}

func TestOwnershipConfigBackedAdjudicator11565(t *testing.T) {
	useOwnershipTempState(t)
	old := ownershipNow11565.Add(-time.Minute)
	prRepo := func() []FrontendRepo {
		return []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 8, HiveAgent: "scanner", Labels: []string{"needs-human"}, CreatedAt: old, UpdatedAt: old}}}}}
	}
	decide := func(cfg *config.Config, statuses map[string]*crux.Process) Ownership {
		t.Helper()
		repos := prRepo()
		applyOwnershipDecisions(cfg, repos, statuses, "idle", ownershipNow11565, nil)
		return decision11565(t, repos[0], ownershipKindPR, 8)
	}
	agents := map[string]config.AgentConfig{"adjudicator": {Enabled: true, Role: "reviewer"}}
	// Inside the adjudicator wait: not your turn.
	checkOwnership11565(t, decide(ownershipCfg11565(5, agents), nil), notYours11565)
	// The adjudicator paused: reviewer_unavailable at once.
	checkOwnership11565(t, decide(ownershipCfg11565(5, agents), map[string]*crux.Process{"adjudicator": {Paused: true}}), want11565{OwnershipVerbReview, OwnershipReasonReviewerUnavailable})
	paused := map[string]config.AgentConfig{"adjudicator": {Enabled: true, Role: "reviewer", Paused: true}}
	checkOwnership11565(t, decide(ownershipCfg11565(5, paused), nil), want11565{OwnershipVerbReview, OwnershipReasonReviewerUnavailable})
}

func TestOwnershipEscalationFirstSeenOnlyAtL5Plus11565(t *testing.T) {
	statePath := useOwnershipTempState(t)
	repos := func() []FrontendRepo {
		return []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 8, HiveAgent: "scanner", Labels: []string{"needs-human"}}}}}}
	}
	applyOwnershipDecisions(ownershipCfg11565(4, nil), repos(), nil, "idle", ownershipNow11565, nil)
	if got := loadOwnershipState(statePath); len(got.EscalationFirstSeen) != 0 {
		t.Fatalf("first-seen recorded at L4: %v", got.EscalationFirstSeen)
	}
	applyOwnershipDecisions(ownershipCfg11565(5, nil), repos(), nil, "idle", ownershipNow11565, nil)
	if got := loadOwnershipState(statePath); len(got.EscalationFirstSeen) != 1 {
		t.Fatalf("first-seen not recorded at L5: %v", got.EscalationFirstSeen)
	}
}

func TestApplyRepoHoldDropsStoredOwnership11565(t *testing.T) {
	s := newTestServer()
	status := parityStatus9102(t)
	status.Repos[0].Ownership = map[string]Ownership{
		ownershipKey(ownershipKindPR, 11):    {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonLevelGateHold, Action: OwnershipActionReleaseHold},
		ownershipKey(ownershipKindIssue, 11): {YourTurn: true, Verb: OwnershipVerbClose, ReasonCode: OwnershipReasonVerifiedDone},
		ownershipKey(ownershipKindPR, 12):    {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonPersonPRNeedsHuman},
	}
	s.UpdateStatus(status)
	s.applyRepoHoldToStatus("octo/demo", 11, "pr", true, "hold", nil)

	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	got := s.status.Repos[0].Ownership
	if _, ok := got[ownershipKey(ownershipKindPR, 11)]; ok {
		t.Fatalf("moved PR kept its stored decision: %+v", got)
	}
	if _, ok := got[ownershipKey(ownershipKindIssue, 11)]; !ok {
		t.Fatalf("issue sharing the number lost its decision: %+v", got)
	}
	if _, ok := got[ownershipKey(ownershipKindPR, 12)]; !ok {
		t.Fatalf("unrelated PR lost its decision: %+v", got)
	}
}

// TestApplyRepoHoldLeavesPublishedOwnershipMapUntouched11565: status readers
// hold the published Ownership map without statusMu, so the hold move must
// replace it rather than delete from it. go test -race fails on an in-place
// delete.
func TestApplyRepoHoldLeavesPublishedOwnershipMapUntouched11565(t *testing.T) {
	s := newTestServer()
	status := parityStatus9102(t)
	status.Repos[0].Ownership = map[string]Ownership{
		ownershipKey(ownershipKindPR, 11): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonLevelGateHold},
		ownershipKey(ownershipKindPR, 12): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonPersonPRNeedsHuman},
	}
	s.UpdateStatus(status)
	published := status.Repos[0].Ownership
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			_ = ownershipFor(published, ownershipKindPR, 11)
		}
	}()
	s.applyRepoHoldToStatus("octo/demo", 11, "pr", true, "hold", nil)
	<-done
	if _, ok := published[ownershipKey(ownershipKindPR, 11)]; !ok {
		t.Fatalf("published map was mutated in place: %+v", published)
	}
}

// TestApplyRepoHoldNoRaceWithStatusEndpoint11565 overlaps the real /api/status
// handler with hold moves; go test -race fails if the snapshot is built
// outside statusMu.
func TestApplyRepoHoldNoRaceWithStatusEndpoint11565(t *testing.T) {
	s := newTestServer()
	status := parityStatus9102(t)
	status.Repos[0].Ownership = map[string]Ownership{
		ownershipKey(ownershipKindPR, 11): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonLevelGateHold},
		ownershipKey(ownershipKindPR, 12): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonPersonPRNeedsHuman},
	}
	s.UpdateStatus(status)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			rec := httptest.NewRecorder()
			s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		}
	}()
	for i := 0; i < 300; i++ {
		s.applyRepoHoldToStatus("octo/demo", 11, "pr", i%2 == 0, "hold", nil)
	}
	<-done
}

func workBreakdownStatus11675(t *testing.T) (*Server, *github.RepoWorkBreakdown) {
	t.Helper()
	s := newTestServer()
	status := parityStatus9102(t)
	status.Repos[0].WorkBreakdown = &github.RepoWorkBreakdown{}
	status.Repos[0].WorkBreakdown.PRs.Actionable = 5
	status.Repos[0].WorkBreakdown.PRs.Hold = 5
	status.Repos[0].Ownership = map[string]Ownership{
		ownershipKey(ownershipKindPR, 11): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonLevelGateHold},
		ownershipKey(ownershipKindPR, 12): {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonPersonPRNeedsHuman},
	}
	s.UpdateStatus(status)
	return s, status.Repos[0].WorkBreakdown
}

// TestApplyRepoHoldLeavesPublishedWorkBreakdownUntouched11675: the hold move
// must publish an adjusted copy of the work breakdown, not edit the shared one.
func TestApplyRepoHoldLeavesPublishedWorkBreakdownUntouched11675(t *testing.T) {
	s, published := workBreakdownStatus11675(t)
	s.applyRepoHoldToStatus("octo/demo", 11, "pr", true, "hold", nil)
	if published.PRs.Hold != 5 || published.PRs.Actionable != 5 {
		t.Fatalf("published work breakdown was mutated in place: %+v", published.PRs)
	}
	s.statusMu.RLock()
	got := s.status.Repos[0].WorkBreakdown
	s.statusMu.RUnlock()
	if got == published || got.PRs.Hold != 6 || got.PRs.Actionable != 4 {
		t.Fatalf("adjusted breakdown = %+v (same pointer: %v), want Hold 6 Actionable 4", got.PRs, got == published)
	}
}

// TestApplyRepoHoldNoRaceOnWorkBreakdown11675 overlaps /api/status encoding
// with hold moves that adjust a non-nil WorkBreakdown; go test -race fails on
// an in-place write.
func TestApplyRepoHoldNoRaceOnWorkBreakdown11675(t *testing.T) {
	s, _ := workBreakdownStatus11675(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			rec := httptest.NewRecorder()
			s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		}
	}()
	for i := 0; i < 300; i++ {
		s.applyRepoHoldToStatus("octo/demo", 11, "pr", i%2 == 0, "hold", nil)
	}
	<-done
}

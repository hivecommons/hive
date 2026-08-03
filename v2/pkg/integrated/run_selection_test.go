package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestPendingRetiredRepairVerificationEndsAfterFreshObservationOrResolution(t *testing.T) {
	retiredAt := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC)
	finding := &visualhive.FindingLifecycle{
		Status:     visualhive.StatusIssueOpen,
		LastSeenAt: retiredAt.Add(-time.Minute),
		LastRetiredRepair: &visualhive.RetiredRepair{
			RetiredAt: retiredAt,
		},
	}
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{"retired": finding}}
	if !pendingRetiredRepairVerification(state) {
		t.Fatal("fresh authoritative verification was not required after repair retirement")
	}
	finding.LastSeenAt = retiredAt.Add(time.Minute)
	if pendingRetiredRepairVerification(state) {
		t.Fatal("fresh present observation did not complete the bounded retirement verification cycle")
	}
	finding.LastSeenAt = retiredAt.Add(-time.Minute)
	finding.Status = visualhive.StatusResolved
	if pendingRetiredRepairVerification(state) {
		t.Fatal("authoritative resolution did not complete the bounded retirement verification cycle")
	}
}

func TestActiveRepairFindingSkipsHumanReviewOnlyIssue(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-baseline": {
			RepositoryFingerprint: "a-baseline", Status: visualhive.StatusIssueOpen,
			IssueNumber: 100, HumanReviewRequired: true,
		},
		"b-console": {
			RepositoryFingerprint: "b-console", Status: visualhive.StatusIssueOpen,
			IssueNumber: 101,
		},
	}}

	finding, ok := activeRepairFinding(state)
	if !ok || finding.RepositoryFingerprint != "b-console" {
		t.Fatalf("expected actionable finding, got ok=%t finding=%+v", ok, finding)
	}
}

func TestSelectedRepairKeyEnforcesRepositoryConcurrencyBeforeSortOrder(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-new-issue": {RepositoryFingerprint: "a-new-issue", Status: visualhive.StatusIssueOpen, IssueNumber: 1},
		"z-open-pr":   {RepositoryFingerprint: "z-open-pr", Status: visualhive.StatusPROpen, IssueNumber: 2, PRNumber: 3},
	}}
	if selected := selectedRepairKey(state); selected != "" {
		t.Fatalf("open PR must block a new repair regardless of fingerprint order, got %q", selected)
	}
	state.Findings["z-open-pr"].Status = visualhive.StatusRepairRunning
	if selected := selectedRepairKey(state); selected != "z-open-pr" {
		t.Fatalf("existing repair must resume before a new issue, got %q", selected)
	}
	state.Findings["z-open-pr"].HumanReviewRequired = true
	if selected := selectedRepairKey(state); selected != "" {
		t.Fatalf("manual review hold must block all repair dispatch, got %q", selected)
	}
}

func TestRepairPreparationAndPinnedCLIEnvironment(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "dashboard"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "dashboard", "package-lock.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "pyproject.toml"), []byte("[build-system]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := repairPreparationCommands(checkout)
	if len(commands) != 2 || commands[0].Name != "npm" || strings.Join(commands[0].Args, " ") != "--prefix dashboard ci" || commands[1].Name != "python" || strings.Join(commands[1].Args, " ") != "-m pip install ." {
		t.Fatalf("unexpected preparation commands: %+v", commands)
	}
	cli := filepath.Join(checkout, "visual-hive", "dist", "index.js")
	environment := repairValidationEnvironment(Config{VisualHiveArgs: []string{cli}})
	if environment["VISUAL_HIVE_CLI"] != cli {
		t.Fatalf("pinned CLI environment = %v", environment)
	}
}

func TestSourceChangesRemainLowRiskButTestOnlyChangesAreAutomatic(t *testing.T) {
	if risk := mergeRisk([]string{"dashboard/src/App.tsx"}); risk != automation.RiskLow {
		t.Fatalf("nested application source risk = %v, want low", risk)
	}
	if risk := mergeRisk([]string{"dashboard/src/App.test.tsx"}); risk != automation.RiskAutomatic {
		t.Fatalf("source-tree test-only risk = %v, want automatic", risk)
	}
	if risk := mergeRisk([]string{"tests/App.test.tsx"}); risk != automation.RiskAutomatic {
		t.Fatalf("dedicated test path risk = %v, want automatic", risk)
	}
	if risk := mergeRisk([]string{"dashboard/src/App.test.tsx", "dashboard/src/App.tsx"}); risk != automation.RiskLow {
		t.Fatalf("mixed test and application source risk = %v, want low", risk)
	}
}

func TestRepositoryTestFailureExpandsRepairPathsWithoutExpandingAutoMerge(t *testing.T) {
	configured := []string{"src/**", "**/*.test.*"}
	ordinary := repairPathsForFinding(configured, visualhive.FindingLifecycle{IssueKind: "mutation_survivor"})
	if len(ordinary) != len(configured) {
		t.Fatalf("ordinary finding inherited dependency repair authority: %v", ordinary)
	}
	repositoryFailure := repairPathsForFinding(configured, visualhive.FindingLifecycle{IssueKind: visualhive.RepositoryTestFailureKind})
	joined := strings.Join(repositoryFailure, "\n")
	for _, expected := range []string{"**/package.json", "**/package-lock.json", "**/pyproject.toml", "**/go.mod"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("repository-test repair paths lack %s: %v", expected, repositoryFailure)
		}
	}
	config := Config{AllowedAutoMergePaths: []string{"**/*.test.*"}}
	if strings.Contains(strings.Join(config.AllowedAutoMergePaths, "\n"), "package") {
		t.Fatal("per-finding dependency repair authority leaked into auto-merge paths")
	}
}

func TestRepositoryTestFailureIsSelectedBeforeOrdinaryOpenFinding(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-ordinary": {RepositoryFingerprint: "a-ordinary", IssueKind: "mutation_survivor", Status: visualhive.StatusIssueOpen, IssueNumber: 1},
		"z-blocking": {RepositoryFingerprint: "z-blocking", IssueKind: visualhive.RepositoryTestFailureKind, Status: visualhive.StatusIssueOpen, IssueNumber: 2},
	}}
	if selected := selectedRepairKey(state); selected != "z-blocking" {
		t.Fatalf("blocking repository-test failure was starved by fingerprint order: %q", selected)
	}
	if active, ok := activeRepairFinding(state); !ok || active.RepositoryFingerprint != "z-blocking" {
		t.Fatalf("orchestration selected a different open issue than the worker: ok=%t finding=%+v", ok, active)
	}
	state.Findings["a-ordinary"].Status = visualhive.StatusRepairRunning
	if selected := selectedRepairKey(state); selected != "a-ordinary" {
		t.Fatalf("existing active repair lost repository concurrency ownership: %q", selected)
	}
	if active, ok := activeRepairFinding(state); !ok || active.RepositoryFingerprint != "a-ordinary" {
		t.Fatalf("existing active repair was shadowed by another open issue: ok=%t finding=%+v", ok, active)
	}
}

func TestMergePolicyRecheckIsDeterministicBoundedAndObservationSafe(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"held-b": {
			RepositoryFingerprint: "held-b", Status: visualhive.StatusReady, IssueNumber: 3, PRNumber: 4,
			RepairCommitSHA: strings.Repeat("a", 40), HumanReviewRequired: true, ManualReviewKind: "merge_policy",
		},
		"held-a": {
			RepositoryFingerprint: "held-a", Status: visualhive.StatusReady, IssueNumber: 5, PRNumber: 6,
			RepairCommitSHA: strings.Repeat("b", 40), HumanReviewRequired: true, ManualReviewKind: "merge_policy",
		},
	}}
	held := mergePolicyHeldFindings(state)
	if len(held) != 2 || held[0].RepositoryFingerprint != "held-a" || held[1].RepositoryFingerprint != "held-b" {
		t.Fatalf("multiple policy holds were not returned deterministically: %+v", held)
	}

	config := Config{Repository: "owner/repo", DefaultBranch: "main", Automation: AutomationAutoMerge, ACMMLevel: 6, MaxRepairAttempts: 4,
		AllowedAutoMergePaths: []string{"**/*.test.*"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic}}
	policy := integratedPolicy(config)
	finding := held[0]
	gate := hivegithub.PullRequestGate{
		Number: finding.PRNumber, Open: true, HeadSHA: finding.RepairCommitSHA, BaseSHA: strings.Repeat("c", 40), BaseBranch: "main",
		MergeableKnown: true, Mergeable: true, ChangedFiles: []string{"src/metrics.test.ts"}, VisualHiveVerdictGreen: true,
		RequiredCheckNames: []string{"visual-hive"}, RequiredCheckStates: []string{"success"}, BranchProtectionEnabled: true,
	}
	if decision, allowed := mergePolicyRecheckDecision(config, policy, finding, gate); !allowed || !decision.Allowed {
		t.Fatalf("currently authorized exact test-only hold was not recheckable: %+v", decision)
	}
	gate.ChangedFiles = []string{"src/metrics.ts"}
	if decision, allowed := mergePolicyRecheckDecision(config, policy, finding, gate); allowed || decision.Allowed {
		t.Fatalf("still-denied source hold became recheckable: %+v", decision)
	}
	finding.ObservationHumanReviewRequired = true
	gate.ChangedFiles = []string{"src/metrics.test.ts"}
	if _, allowed := mergePolicyRecheckDecision(config, policy, finding, gate); allowed {
		t.Fatal("observation-level review authority was erased by policy recheck")
	}
}

func TestDispatchRetryIsBoundedToConcurrencyCancellation(t *testing.T) {
	if !retryCancelledDispatch("cancelled", 1, 3) || retryCancelledDispatch("failure", 1, 3) || retryCancelledDispatch("cancelled", 3, 3) {
		t.Fatal("dispatch retry classification must be cancellation-only and bounded")
	}
}

func TestRetryableRepairAttemptClassification(t *testing.T) {
	retryable := &repair.RetryableAttemptError{Cause: fmt.Errorf("validation failed")}
	if !repair.IsRetryableAttemptError(retryable) || repair.IsRetryableAttemptError(fmt.Errorf("policy denied")) {
		t.Fatal("repair retry classification must be explicit")
	}
}

func TestNeedsHostedRevisionEvidenceOnlyForFailedVisualExactHead(t *testing.T) {
	finding := visualhive.FindingLifecycle{
		Status: visualhive.StatusNeedsRevision, PRNumber: 19, RepairCommitSHA: "head", Branch: "hive/repair-a1",
		LastCheckRuns: []visualhive.CheckEvidence{{Name: "Private-safe lint", State: "success"}, {Name: "visual-hive", State: "failure"}},
	}
	if !needsHostedRevisionEvidence(finding) {
		t.Fatal("failed exact-head Visual Hive check should supply revision evidence")
	}
	finding.LastCheckRuns[1].State = "success"
	if needsHostedRevisionEvidence(finding) {
		t.Fatal("green Visual Hive check must not be treated as failed revision evidence")
	}
	finding.LastCheckRuns[1].State = "failure"
	finding.RepairCommitSHA = ""
	if needsHostedRevisionEvidence(finding) {
		t.Fatal("unbound PR evidence must never be fetched")
	}
}

func TestDurableRepairAttemptsUsesWorkerCheckpointForCurrentRecurrence(t *testing.T) {
	stateDir := t.TempDir()
	store, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(repair.Attempt{RepositoryFingerprint: "finding", Attempt: 4, AttemptCounted: true, Recurrence: 2, Stage: repair.StageNoChange}); err != nil {
		t.Fatal(err)
	}
	spent, err := durableRepairAttempts(stateDir, visualhive.FindingLifecycle{RepositoryFingerprint: "finding", RepairAttempts: 3, Recurrences: 2})
	if err != nil || spent != 4 {
		t.Fatalf("durable attempts = %d, err=%v, want 4", spent, err)
	}
	spent, err = durableRepairAttempts(stateDir, visualhive.FindingLifecycle{RepositoryFingerprint: "finding", RepairAttempts: 1, Recurrences: 3})
	if err != nil || spent != 1 {
		t.Fatalf("prior recurrence leaked into retry budget: attempts=%d err=%v", spent, err)
	}
}

func TestApprovedMergedBaselineProposalUsesReadyAndMergeAsApproval(t *testing.T) {
	gate := hivegithub.PullRequestGate{
		Merged: true, MergeSHA: "merge-sha", Draft: false, Hold: true,
		VisualHiveVerdictGreen: true, RequiredCheckStates: []string{"success"},
	}
	if !approvedMergedBaselineProposal(gate) {
		t.Fatal("a non-draft exact merge must be consumable even when GitHub retains Hive's hold label")
	}

	gate.Draft = true
	if approvedMergedBaselineProposal(gate) {
		t.Fatal("a draft merge must not count as reviewed baseline approval")
	}
	gate.Draft = false
	gate.HumanReviewRequired = true
	if approvedMergedBaselineProposal(gate) {
		t.Fatal("an unresolved repository review requirement must block baseline approval")
	}
	gate.HumanReviewRequired = false
	gate.RequiredCheckStates = []string{"pending"}
	if approvedMergedBaselineProposal(gate) {
		t.Fatal("non-green exact-head checks must block baseline approval")
	}
}

func TestMarkMergePolicyHoldPersistsReviewRequirement(t *testing.T) {
	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
				IssueNumber: 10, PRNumber: 12, RepairCommitSHA: "repair-sha",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	decision := automation.Decision{Action: automation.ActionMergePR, Reasons: []string{"visual-hive.config.yaml is outside the auto-merge allowlist"}}
	finding, _ := lifecycle.Finding("finding")
	if err := markMergePolicyHold(lifecycle, finding, decision); err != nil {
		t.Fatal(err)
	}

	reopened, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	held, exists := reopened.Finding("finding")
	if !exists || held.Status != visualhive.StatusReady || !held.HumanReviewRequired || held.ManualReviewKind != "merge_policy" || !strings.Contains(held.ManualReviewReason, "outside the auto-merge allowlist") {
		t.Fatalf("merge policy hold was not durable: %+v", held)
	}
}

func TestReconcileExternallyMergedRepairRejectsMergeWithoutHiveIntent(t *testing.T) {
	deleted := 0
	server := newMergedRepairServer(t, &deleted)
	defer server.Close()

	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"earlier-open-issue": {
				Repository: "owner/repo", RepositoryFingerprint: "earlier-open-issue", Status: visualhive.StatusIssueOpen, IssueNumber: 9,
			},
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
				IssueNumber: 10, PRNumber: 12, RepairCommitSHA: "repair-sha", Branch: "hive/repair-proof",
				HumanReviewRequired: true, ManualReviewKind: "merge_policy", ManualReviewReason: "review required",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	policy := automation.Policy{ACMMLevel: 6, Mode: automation.ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 5}
	reconciled, err := reconcileExternallyMergedRepair(context.Background(), dir, Config{Repository: "owner/repo", RepositoryID: "123"}, lifecycle, client, policy)
	if err == nil || !strings.Contains(err.Error(), "without a durable Hive merge intent") {
		t.Fatalf("external merge was not rejected: reconciled=%t err=%v", reconciled, err)
	}
	finding, exists := lifecycle.Finding("finding")
	if reconciled || !exists || finding.Status != visualhive.StatusReady || finding.MergeSHA != "" || !finding.HumanReviewRequired || deleted != 0 {
		t.Fatalf("rejected external merge changed lifecycle: reconciled=%t deleted=%d finding=%+v", reconciled, deleted, finding)
	}
}

func TestReconcileOpenRepairDuplicatesClosesOnlySupersededExactPR(t *testing.T) {
	closed, deleted := 0, 0
	marker := "<!-- hive-repair: finding -->"
	oldHead, currentHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls" && request.URL.Query().Get("state") == "open":
			_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":11,"html_url":"https://example.test/pull/11","body":%q,"head":{"ref":"hive/repair-old","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","repo":{"id":123,"full_name":"owner/repo"}}},{"number":12,"html_url":"https://example.test/pull/12","body":%q,"head":{"ref":"hive/repair-current","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","repo":{"id":123,"full_name":"owner/repo"}}}]`, marker, oldHead, marker, currentHead))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/11":
			state := "open"
			if closed > 0 {
				state = "closed"
			}
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"number":11,"state":%q,"body":%q,"head":{"ref":"hive/repair-old","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","repo":{"id":123,"full_name":"owner/repo"}}}`, state, marker, oldHead))
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/11":
			closed++
			_, _ = io.WriteString(writer, `{"number":11,"state":"closed"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-old":
			_, _ = fmt.Fprintf(writer, `{"ref":"refs/heads/hive/repair-old","object":{"sha":%q,"type":"commit"}}`, oldHead)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-current":
			_, _ = fmt.Fprintf(writer, `{"ref":"refs/heads/hive/repair-current","object":{"sha":%q,"type":"commit"}}`, currentHead)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-old":
			deleted++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
				IssueNumber: 10, PRNumber: 12, PRURL: "https://example.test/pull/12", RepairCommitSHA: currentHead,
				Branch: "hive/repair-current", RepairAttempts: 2, OwningAgentHint: "quality",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	policy := automation.Policy{ACMMLevel: 6, Mode: automation.ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 5}
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileOpenRepairDuplicates(context.Background(), store, Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main"}, lifecycle, client, policy); err != nil {
		t.Fatal(err)
	}
	if closed != 1 || deleted != 1 {
		t.Fatalf("superseded repair was not closed and deleted exactly once: closed=%d deleted=%d", closed, deleted)
	}
	finding, _ := lifecycle.Finding("finding")
	if finding.PRNumber != 12 || finding.Branch != "hive/repair-current" || finding.RepairCommitSHA != currentHead {
		t.Fatalf("durable current repair was changed: %+v", finding)
	}
}

func TestReconcileApprovedBaselineBranchDeletesExactReviewedRef(t *testing.T) {
	deleted := 0
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/matching-refs/heads/hive/baseline-":
			_, _ = io.WriteString(writer, `[]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/baseline-reviewed":
			_, _ = io.WriteString(writer, `{"ref":"refs/heads/hive/baseline-reviewed","object":{"sha":"reviewed-head","type":"commit"}}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/baseline-reviewed":
			deleted++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	lifecycleState := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusIssueClosed,
				IssueNumber: 10, RepairAttempts: 2, OwningAgentHint: "quality",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(lifecycleState)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repairStore.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "finding", Attempt: 2,
		BaselineReview: &repair.BaselineReview{
			Status: repair.BaselineReviewApproved, ProposalBranch: "hive/baseline-reviewed",
			ProposalCommitSHA: "reviewed-head", ProposalPRNumber: 29,
		},
	}); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	policy := automation.Policy{ACMMLevel: 6, Mode: automation.ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 5}
	if err := reconcileApprovedBaselineBranches(context.Background(), stateDir, Config{Repository: "owner/repo"}, lifecycle, client, policy); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("approved baseline branch deletion count = %d, want 1", deleted)
	}
}

func TestReconcileExternallyMergedRepairDoesNotImplicitlyMigrateLegacyClosedState(t *testing.T) {
	deleted := 0
	server := newMergedRepairServer(t, &deleted)
	defer server.Close()
	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusIssueClosed,
				IssueNumber: 10, PRNumber: 12, RepairCommitSHA: "repair-sha", Branch: "hive/repair-proof",
				HumanReviewRequired: true, ManualReviewKind: "merge_policy", ManualReviewReason: "legacy hold",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	policy := automation.Policy{ACMMLevel: 6, Mode: automation.ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 5}
	reconciled, err := reconcileExternallyMergedRepair(context.Background(), dir, Config{Repository: "owner/repo", RepositoryID: "123"}, lifecycle, client, policy)
	if err != nil {
		t.Fatal(err)
	}
	finding, exists := lifecycle.Finding("finding")
	if reconciled || !exists || finding.Status != visualhive.StatusIssueClosed || finding.MergeSHA != "" || deleted != 0 {
		t.Fatalf("legacy closed merge bypassed explicit migration: reconciled=%t deleted=%d finding=%+v", reconciled, deleted, finding)
	}
}

func newMergedRepairServer(t *testing.T, deleted *int) *httptest.Server {
	t.Helper()
	return newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/12":
			_, _ = io.WriteString(writer, `{"number":12,"changed_files":1,"state":"closed","merged":true,"merge_commit_sha":"merge-sha","head":{"sha":"repair-sha","ref":"hive/repair-proof","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-before"},"labels":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/12/files":
			_, _ = io.WriteString(writer, `[{"filename":"index.html"}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/repair-sha/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":120,"name":"visual-hive","app":{"id":42},"check_suite":{"id":121},"head_sha":"repair-sha","status":"completed","conclusion":"success"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/repair-sha/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[{"id":122,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","event":"pull_request","head_branch":"hive/repair-proof","head_sha":"repair-sha","status":"completed","conclusion":"success","check_suite_id":121,"repository":{"full_name":"owner/repo"},"head_repository":{"full_name":"owner/repo"},"pull_requests":[{"number":12,"head":{"sha":"repair-sha","ref":"hive/repair-proof"},"base":{"sha":"base-before","ref":"main"}}]}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			_, _ = io.WriteString(writer, `{"ref":"refs/heads/hive/repair-proof","object":{"sha":"repair-sha","type":"commit"}}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-proof":
			(*deleted)++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestVerifyPostMergeTargetAcceptsOnlyNonConflictingDescendant(t *testing.T) {
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		filename := ".hive/integrated.json"
		if strings.Contains(request.URL.Path, "head-conflict") {
			filename = "test/run-with-env.test.mjs"
		}
		_, _ = fmt.Fprintf(writer, `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"merge_base_commit":{"sha":"merge-sha"},"files":[{"filename":%q}]}`, filename)
	}))
	defer server.Close()

	stateDir := t.TempDir()
	store, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(repair.Attempt{RepositoryFingerprint: "finding", CommitSHA: "repair-sha", PRNumber: 12, ChangedFiles: []string{"test/run-with-env.test.mjs"}}); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	finding := visualhive.FindingLifecycle{RepositoryFingerprint: "finding", RepairCommitSHA: "repair-sha", MergeSHA: "merge-sha", PRNumber: 12}
	options, err := verifyPostMergeTarget(context.Background(), stateDir, Config{Repository: "owner/repo"}, finding, "head-safe", client)
	if err != nil || options.VerifiedMergeAncestorFingerprint != "finding" || options.VerifiedMergeAncestorSHA != "merge-sha" {
		t.Fatalf("safe descendant was not verified: options=%+v err=%v", options, err)
	}
	if _, err := verifyPostMergeTarget(context.Background(), stateDir, Config{Repository: "owner/repo"}, finding, "head-conflict", client); err == nil {
		t.Fatal("descendant that changed the repair file must be rejected")
	}
}

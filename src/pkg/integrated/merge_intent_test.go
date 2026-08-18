package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/visualhive"
)

func TestMergeIntentDurablyBindsExactRepositoryPullRequestAndDiff(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent := exactTestMergeIntent(nil)
	if err := store.SaveMergeIntent(intent); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.LoadMergeIntent()
	if err != nil || !exists {
		t.Fatalf("load merge intent: exists=%t err=%v", exists, err)
	}
	if err := validateMergeIntent(loaded, intent.Repository, intent.RepositoryID, intent.DiffDigest, intent.Gate); err != nil {
		t.Fatalf("exact durable merge intent was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*MergeIntent, *hivegithub.PullRequestGate, *string, *string, *string){
		"repository": func(_ *MergeIntent, _ *hivegithub.PullRequestGate, repository, _, _ *string) {
			*repository = "owner/other"
		},
		"repository_id": func(_ *MergeIntent, _ *hivegithub.PullRequestGate, _, repositoryID, _ *string) { *repositoryID = "456" },
		"pr":            func(_ *MergeIntent, gate *hivegithub.PullRequestGate, _, _, _ *string) { gate.Number++ },
		"head": func(_ *MergeIntent, gate *hivegithub.PullRequestGate, _, _, _ *string) {
			gate.HeadSHA = strings.Repeat("d", 40)
		},
		"base": func(_ *MergeIntent, gate *hivegithub.PullRequestGate, _, _, _ *string) {
			gate.BaseSHA = strings.Repeat("e", 40)
		},
		"diff": func(_ *MergeIntent, _ *hivegithub.PullRequestGate, _, _, digest *string) {
			*digest = strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, gate := loaded, loaded.Gate
			repository, repositoryID, digest := intent.Repository, intent.RepositoryID, intent.DiffDigest
			mutate(&candidate, &gate, &repository, &repositoryID, &digest)
			if err := validateMergeIntent(candidate, repository, repositoryID, digest, gate); err == nil {
				t.Fatalf("merge intent accepted changed %s", name)
			}
		})
	}

	corrupt := loaded
	corrupt.Gate.HeadSHA = strings.Repeat("f", 40)
	data, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "merge-intent.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadMergeIntent(); err == nil {
		t.Fatal("durable merge intent with a gate/head mismatch was accepted")
	}
}

func TestPrepareMergeIntentInvalidatesStaleDurableIntent(t *testing.T) {
	rawDiff := "diff --git a/tests/fix.test.ts b/tests/fix.test.ts\n-old\n+new\n"
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.URL.Path == "/user":
			_, _ = io.WriteString(writer, `{"login":"accountable-user"}`)
		case request.URL.Path == "/repos/owner/repo/pulls/12" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, rawDiff)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Config{Repository: "owner/repo", RepositoryID: "123"}); err != nil {
		t.Fatal(err)
	}
	stale := exactTestMergeIntent(nil)
	stale.PRNumber, stale.Gate.Number = 11, 11
	stale.HeadSHA, stale.Gate.HeadSHA = strings.Repeat("d", 40), strings.Repeat("d", 40)
	stale.Request.ExpectedHeadSHA, stale.Request.TestedHeadSHA = stale.HeadSHA, stale.HeadSHA
	stale.Authorization, _ = mergeAuthorizationDigest(stale)
	if err := store.SaveMergeIntent(stale); err != nil {
		t.Fatal(err)
	}

	currentGate := exactTestMergeGate()
	currentGate.ChangedFiles = []string{"tests/fix.test.ts"}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Automation: AutomationAutoMerge, ACMMLevel: 6, MaxRepairAttempts: 4, AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic}}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "finding", PRNumber: 12, RepairCommitSHA: currentGate.HeadSHA,
	}
	policy := integratedPolicy(config)
	decision := policy.Authorize(mergeActionRequest(config, finding, currentGate))
	prepared, err := prepareMergeIntent(context.Background(), stateDir, config, visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "finding", PRNumber: 12, RepairCommitSHA: currentGate.HeadSHA,
	}, currentGate, nil, policy, decision, client)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := digestTestDiff(rawDiff)
	if prepared.PRNumber != 12 || prepared.HeadSHA != currentGate.HeadSHA || prepared.DiffDigest != wantDigest {
		t.Fatalf("stale merge intent was not replaced by exact current intent: %+v", prepared)
	}
	loaded, exists, err := store.LoadMergeIntent()
	if err != nil || !exists || loaded.PRNumber != prepared.PRNumber || loaded.HeadSHA != prepared.HeadSHA || loaded.BaseSHA != prepared.BaseSHA || loaded.DiffDigest != prepared.DiffDigest || !loaded.AuthorizedAt.Equal(prepared.AuthorizedAt) {
		t.Fatalf("replacement merge intent was not durable: exists=%t loaded=%+v err=%v", exists, loaded, err)
	}
	audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), `"action":"invalidate_merge_intent"`) || !strings.Contains(string(audit), `"action":"authorize_merge_intent"`) {
		t.Fatalf("intent replacement audit is incomplete: %q err=%v", audit, err)
	}
}

func TestFinalGateDriftAfterDurableAuthorizationInvalidatesIntentWithoutMerge(t *testing.T) {
	stateDir := t.TempDir()
	config := exactTestMergeConfig()
	config.StateDir = stateDir
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	finding := &visualhive.FindingLifecycle{
		Repository: config.Repository, RepositoryID: config.RepositoryID, RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
		PRNumber: 12, Branch: "hive/repair-proof", RepairCommitSHA: strings.Repeat("a", 40), RepairAttempts: 1, OwningAgentHint: "quality",
	}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lifecycleData, err := json.Marshal(visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{"finding": finding},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), lifecycleData, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}

	normalPullReads, mergeRequests := 0, 0
	rawDiff := "diff --git a/tests/fix.test.ts b/tests/fix.test.ts\n-old\n+new\n"
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
		switch {
		case request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.URL.Path == "/user":
			_, _ = io.WriteString(writer, `{"login":"accountable-user"}`)
		case request.URL.Path == "/repos/owner/repo/pulls/12" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, rawDiff)
		case request.URL.Path == "/repos/owner/repo/pulls/12":
			normalPullReads++
			labels := "[]"
			if normalPullReads >= 2 {
				labels = `[{"name":"hold"}]`
			}
			_, _ = fmt.Fprintf(writer, `{"number":12,"changed_files":1,"state":"open","draft":false,"mergeable":true,"mergeable_state":"clean","head":{"sha":%q,"ref":"hive/repair-proof","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q},"labels":%s}`, head, base, labels)
		case request.URL.Path == "/repos/owner/repo/pulls/12/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/fix.test.ts"}]`)
		case request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case request.URL.Path == "/repos/owner/repo/rules/branches/main":
			_, _ = io.WriteString(writer, `[]`)
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs":
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"check_runs":[{"id":120,"name":"visual-hive","app":{"id":42},"check_suite":{"id":121},"head_sha":%q,"status":"completed","conclusion":"success"}]}`, head)
		case request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":122,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","event":"pull_request","head_branch":"hive/repair-proof","head_sha":%q,"status":"completed","conclusion":"success","check_suite_id":121,"repository":{"full_name":"owner/repo"},"head_repository":{"full_name":"owner/repo"},"pull_requests":[{"number":12,"head":{"sha":%q,"ref":"hive/repair-proof"},"base":{"sha":%q,"ref":"main"}}]}]}`, head, head, base)
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/12/merge":
			mergeRequests++
			_, _ = io.WriteString(writer, `{"merged":true,"sha":"must-not-merge"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	expectedGate := exactTestMergeGate()
	expectedGate.ChangedFiles = []string{"tests/fix.test.ts"}
	_, err = mergeReadyFindingAtBoundary(context.Background(), stateDir, config, *finding, lifecycle, expectedGate, nil, client)
	var finalGateErr *hivegithub.FinalMergeGateError
	if err == nil || !errors.As(err, &finalGateErr) || mergeRequests != 0 {
		t.Fatalf("hold drift after intent persistence reached merge: requests=%d err=%v", mergeRequests, err)
	}
	if _, exists, loadErr := store.LoadMergeIntent(); loadErr != nil || exists {
		t.Fatalf("obsolete merge intent survived conclusive pre-mutation drift: exists=%t err=%v", exists, loadErr)
	}
	audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "invalidate_merge_intent_at_final_gate") {
		t.Fatalf("final gate invalidation was not durably audited: %q err=%v", audit, err)
	}
}

func TestFinalLiveBoundaryRejectsObservationHoldAddedAfterApproval(t *testing.T) {
	stateDir := t.TempDir()
	config := exactTestMergeConfig()
	config.StateDir = stateDir
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	approval := exactTestMergeApproval(strings.Repeat("c", 64))
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	expectedFinding := visualhive.FindingLifecycle{
		Repository: config.Repository, RepositoryID: config.RepositoryID, RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
		IssueNumber: 11, PRNumber: approval.PRNumber, Branch: "hive/repair-proof", RepairCommitSHA: approval.HeadSHA,
		HumanReviewRequired: true, ManualReviewKind: "merge_policy", RepairAttempts: 1,
	}
	liveFinding := expectedFinding
	liveFinding.ObservationHumanReviewRequired = true
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{"finding": &liveFinding},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	gate := exactTestMergeGate()
	gate.ChangedFiles = []string{"visual-hive.config.yaml"}
	_, _, _, err = authorizeLiveMergeIntent(context.Background(), stateDir, config, expectedFinding, lifecycle, gate, approval.DiffDigest, &approval, nil)
	if err == nil || !strings.Contains(err.Error(), "observation-level human review") {
		t.Fatalf("preexisting merge approval overrode newly observed review authority: %v", err)
	}
	if _, exists, loadErr := store.LoadMergeIntent(); loadErr != nil || exists {
		t.Fatalf("observation-held finding persisted merge authority: exists=%t err=%v", exists, loadErr)
	}
}

func TestReconcileApprovedMergeIntentRecoversCrashAndConsumesAuthority(t *testing.T) {
	stateDir := t.TempDir()
	deleted := 0
	rawDiff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+new\n"
	server := newExactMergedIntentServer(t, rawDiff, &deleted)
	defer server.Close()

	approval := exactTestMergeApproval(digestTestDiff(rawDiff))
	intent := exactTestMergeIntent(&approval)
	intent.DiffDigest, intent.Approval.DiffDigest = approval.DiffDigest, approval.DiffDigest
	store, lifecycle := saveExactMergeRecoveryState(t, stateDir, visualhive.StatusReady, "", true, intent, approval)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := exactTestMergeConfig()

	reconciled, err := reconcileExternallyMergedRepair(context.Background(), stateDir, config, lifecycle, client, integratedPolicy(config))
	if err != nil {
		t.Fatal(err)
	}
	finding, exists := lifecycle.Finding("finding")
	if !reconciled || !exists || finding.Status != visualhive.StatusMerged || finding.MergeSHA != exactTestMergeSHA() || finding.HumanReviewRequired || finding.ManualReviewKind != "" {
		t.Fatalf("exact approved intent was not recovered into merged lifecycle: reconciled=%t finding=%+v", reconciled, finding)
	}
	assertMergeAuthorityConsumed(t, store)
	if deleted != 1 {
		t.Fatalf("merged repair branch deletion count = %d, want 1", deleted)
	}
}

func TestReconcileMergeIntentConsumesOrphansAfterLifecycleWasMarkedMerged(t *testing.T) {
	stateDir := t.TempDir()
	deleted := 0
	rawDiff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+new\n"
	server := newExactMergedIntentServer(t, rawDiff, &deleted)
	defer server.Close()

	approval := exactTestMergeApproval(digestTestDiff(rawDiff))
	intent := exactTestMergeIntent(&approval)
	intent.DiffDigest, intent.Approval.DiffDigest = approval.DiffDigest, approval.DiffDigest
	store, lifecycle := saveExactMergeRecoveryState(t, stateDir, visualhive.StatusMerged, exactTestMergeSHA(), false, intent, approval)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := exactTestMergeConfig()

	reconciled, err := reconcileExternallyMergedRepair(context.Background(), stateDir, config, lifecycle, client, integratedPolicy(config))
	if err != nil {
		t.Fatal(err)
	}
	finding, exists := lifecycle.Finding("finding")
	if reconciled || !exists || finding.Status != visualhive.StatusMerged || finding.MergeSHA != exactTestMergeSHA() {
		t.Fatalf("already-merged lifecycle was not preserved: reconciled=%t finding=%+v", reconciled, finding)
	}
	assertMergeAuthorityConsumed(t, store)
	if deleted != 1 {
		t.Fatalf("merged repair branch deletion count = %d, want 1", deleted)
	}
}

func TestMergeIntentRecoveryRejectsDifferentMergeActor(t *testing.T) {
	stateDir := t.TempDir()
	deleted := 0
	rawDiff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+new\n"
	server := newMergedIntentServer(t, rawDiff, &deleted, "external-user", strings.Repeat("b", 40), true, "success")
	defer server.Close()
	approval := exactTestMergeApproval(digestTestDiff(rawDiff))
	intent := exactTestMergeIntent(&approval)
	intent.DiffDigest, intent.Approval.DiffDigest = approval.DiffDigest, approval.DiffDigest
	intent.Authorization, _ = mergeAuthorizationDigest(intent)
	_, lifecycle := saveExactMergeRecoveryState(t, stateDir, visualhive.StatusReady, "", true, intent, approval)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := reconcileExternallyMergedRepair(context.Background(), stateDir, exactTestMergeConfig(), lifecycle, client, integratedPolicy(exactTestMergeConfig())); err == nil || !strings.Contains(err.Error(), "does not match Hive intent writer") {
		t.Fatalf("external merge actor consumed Hive intent: %v", err)
	}
	if deleted != 0 {
		t.Fatal("actor-mismatched recovery deleted the repair branch")
	}
}

func TestMergeIntentRecoveryUsesPersistedAuthorizationNotMutableCurrentPolicy(t *testing.T) {
	stateDir := t.TempDir()
	deleted := 0
	rawDiff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+new\n"
	server := newMergedIntentServer(t, rawDiff, &deleted, "accountable-user", strings.Repeat("d", 40), false, "failure")
	defer server.Close()
	approval := exactTestMergeApproval(digestTestDiff(rawDiff))
	intent := exactTestMergeIntent(&approval)
	intent.DiffDigest, intent.Approval.DiffDigest = approval.DiffDigest, approval.DiffDigest
	intent.Authorization, _ = mergeAuthorizationDigest(intent)
	_, lifecycle := saveExactMergeRecoveryState(t, stateDir, visualhive.StatusReady, "", true, intent, approval)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	current := exactTestMergeConfig()
	current.Automation, current.ACMMLevel, current.AllowedAutoMergePaths = AutomationAdvisory, 2, nil
	reconciled, err := reconcileExternallyMergedRepair(context.Background(), stateDir, current, lifecycle, client, integratedPolicy(current))
	if err != nil || !reconciled {
		t.Fatalf("authorized completed merge was stranded by mutable post-merge state: reconciled=%t err=%v", reconciled, err)
	}
}

func TestApproveMergeRejectsLiveRepositoryIdentityMismatch(t *testing.T) {
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/owner/repo" {
			_, _ = io.WriteString(writer, `{"id":456,"full_name":"owner/repo"}`)
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()

	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := exactTestMergeConfig()
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err = ApproveMerge(context.Background(), ApproveMergeOptions{
		StateDir: stateDir, PRNumber: 12, ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBaseSHA: strings.Repeat("b", 40),
		ExpectedDiff: strings.Repeat("c", 64), Reason: "reviewed exact config repair", GitHub: client,
	})
	if err == nil || !strings.Contains(err.Error(), "live repository identity does not match") {
		t.Fatalf("live repository ID mismatch was not rejected: %v", err)
	}
	if _, exists, loadErr := store.LoadMergeApproval(); loadErr != nil || exists {
		t.Fatalf("repository mismatch persisted an approval: exists=%t err=%v", exists, loadErr)
	}
}

func TestApprovedFindingSelectionWinsOverUnrelatedOpenIssue(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	approval := exactTestMergeApproval(strings.Repeat("c", 64))
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-unrelated": {
			Repository: "owner/repo", RepositoryFingerprint: "a-unrelated", Status: visualhive.StatusIssueOpen, IssueNumber: 4,
		},
		"z-approved": {
			Repository: "owner/repo", RepositoryFingerprint: "z-approved", Status: visualhive.StatusReady, IssueNumber: 8,
			PRNumber: approval.PRNumber, RepairCommitSHA: approval.HeadSHA, HumanReviewRequired: true, ManualReviewKind: "merge_policy",
		},
	}}
	finding, exists, err := repairFindingForOrchestration(stateDir, state)
	if err != nil || !exists || finding.RepositoryFingerprint != "z-approved" {
		t.Fatalf("approval did not select its held finding ahead of unrelated work: exists=%t finding=%+v err=%v", exists, finding, err)
	}
}

func exactTestMergeConfig() Config {
	return Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Automation: AutomationAutoMerge, ACMMLevel: 6,
		MaxRepairAttempts: 4, AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
	}
}

func exactTestMergeGate() hivegithub.PullRequestGate {
	return hivegithub.PullRequestGate{
		Number: 12, Open: true, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseBranch: "main",
		MergeableKnown: true, Mergeable: true, ChangedFiles: []string{"visual-hive.config.yaml"},
		VisualHiveVerdictGreen: true, VisualHiveProvenanceVerified: true, VisualHiveCheckState: "success",
		VisualHiveCheckRunID: 120, VisualHiveWorkflowRunID: 122, VisualHiveWorkflowPath: ".github/workflows/visual-hive-pr.yml", VisualHiveWorkflowEvent: "pull_request",
		RequiredCheckStates: []string{"success"}, RequiredCheckNames: []string{"visual-hive"},
		BranchProtectionEnabled: true, BranchProtectionConfigured: true, BranchProtectionStrict: true, BranchProtectionAdminEnforced: true,
	}
}

func exactTestMergeApproval(diffDigest string) MergeApproval {
	gate := exactTestMergeGate()
	return MergeApproval{
		SchemaVersion: MergeApprovalSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: gate.Number,
		HeadSHA: gate.HeadSHA, BaseSHA: gate.BaseSHA, BaseBranch: gate.BaseBranch, DiffDigest: diffDigest, Actor: "accountable-user",
		Reason: "reviewed exact config repair", ApprovedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func exactTestMergeIntent(approval *MergeApproval) MergeIntent {
	gate := exactTestMergeGate()
	if approval == nil {
		gate.ChangedFiles = []string{"tests/fix.test.ts"}
	}
	digest := strings.Repeat("c", 64)
	if approval != nil {
		digest = approval.DiffDigest
	}
	config := exactTestMergeConfig()
	finding := visualhive.FindingLifecycle{Repository: config.Repository, RepositoryFingerprint: "finding", PRNumber: gate.Number, RepairCommitSHA: gate.HeadSHA, RepairAttempts: 1, OwningAgentHint: "quality"}
	policy := integratedPolicy(config)
	request := mergeActionRequest(config, finding, gate)
	decision := policy.Authorize(request)
	if approval != nil && !decision.Allowed {
		if approved, eligible := authorizePathApprovedMerge(policy, request, decision); eligible {
			decision = approved
		}
	}
	intent := MergeIntent{
		SchemaVersion: MergeIntentSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: gate.Number,
		HeadSHA: gate.HeadSHA, BaseSHA: gate.BaseSHA, BaseBranch: gate.BaseBranch, DiffDigest: digest, Gate: gate, Approval: approval,
		WriterActor: "accountable-user", Policy: policy, Request: request, Decision: decision,
		AuthorizedAt: time.Now().UTC().Truncate(time.Second),
	}
	intent.Authorization, _ = mergeAuthorizationDigest(intent)
	return intent
}

func saveExactMergeRecoveryState(t *testing.T, stateDir string, status visualhive.LifecycleStatus, mergeSHA string, held bool, intent MergeIntent, approval MergeApproval) (*Store, *visualhive.LifecycleStore) {
	t.Helper()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMergeIntent(intent); err != nil {
		t.Fatal(err)
	}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	finding := &visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "finding", Status: status,
		IssueNumber: 8, PRNumber: 12, Branch: "hive/repair-proof", RepairCommitSHA: intent.HeadSHA, MergeSHA: mergeSHA,
		RepairAttempts: 1, OwningAgentHint: "quality", HumanReviewRequired: held,
	}
	if held {
		finding.ManualReviewKind, finding.ManualReviewReason = "merge_policy", "review exact config repair"
	}
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{"finding": finding},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	return store, lifecycle
}

func newExactMergedIntentServer(t *testing.T, rawDiff string, deleted *int) *httptest.Server {
	return newMergedIntentServer(t, rawDiff, deleted, "accountable-user", strings.Repeat("b", 40), true, "success")
}

func newMergedIntentServer(t *testing.T, rawDiff string, deleted *int, mergedBy, liveBase string, strict bool, checkState string) *httptest.Server {
	t.Helper()
	head := strings.Repeat("a", 40)
	mergeSHA := exactTestMergeSHA()
	return newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.URL.Path == "/repos/owner/repo/pulls/12" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, rawDiff)
		case request.URL.Path == "/repos/owner/repo/pulls/12":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"number":12,"changed_files":1,"state":"closed","merged":true,"merged_by":{"login":%q},"merge_commit_sha":%q,"draft":false,"mergeable":true,"head":{"sha":%q,"ref":"hive/repair-proof","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q},"labels":[]}`, mergedBy, mergeSHA, head, liveBase))
		case request.URL.Path == "/repos/owner/repo/pulls/12/files":
			_, _ = io.WriteString(writer, `[{"filename":"visual-hive.config.yaml"}]`)
		case request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"required_status_checks":{"strict":%t,"checks":[{"context":"visual-hive","app_id":42}]},"required_pull_request_reviews":{"required_approving_review_count":0}}`, strict))
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":120,"name":"visual-hive","app":{"id":42},"check_suite":{"id":121},"head_sha":%q,"status":"completed","conclusion":%q}]}`, head, checkState))
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":122,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","event":"pull_request","head_branch":"hive/repair-proof","head_sha":%q,"status":"completed","conclusion":%q,"check_suite_id":121,"repository":{"full_name":"owner/repo"},"head_repository":{"full_name":"owner/repo"},"pull_requests":[{"number":12,"head":{"sha":%q,"ref":"hive/repair-proof"},"base":{"sha":%q,"ref":"main"}}]}]}`, head, checkState, head, liveBase))
		case request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"ref":"refs/heads/hive/repair-proof","object":{"sha":%q,"type":"commit"}}`, head))
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-proof":
			(*deleted)++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
}

func exactTestMergeSHA() string { return strings.Repeat("c", 40) }

func assertMergeAuthorityConsumed(t *testing.T, store *Store) {
	t.Helper()
	if _, exists, err := store.LoadMergeIntent(); err != nil || exists {
		t.Fatalf("merge intent was not consumed: exists=%t err=%v", exists, err)
	}
	if _, exists, err := store.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("merge approval was not consumed: exists=%t err=%v", exists, err)
	}
	audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "consume_merge_intent_recovered") || !strings.Contains(string(audit), "consume_merge_approval") {
		t.Fatalf("authority consumption audit is incomplete: %q err=%v", audit, err)
	}
}

func digestTestDiff(diff string) string {
	digest := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(digest[:])
}

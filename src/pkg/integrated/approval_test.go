package integrated

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/visualhive"
)

func TestMergeApprovalRecordBindsRepositoryBaseHeadAndDiff(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	approval := MergeApproval{
		SchemaVersion: MergeApprovalSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: 7,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseBranch: "main", DiffDigest: strings.Repeat("c", 64),
		Actor: "accountable-user", Reason: "reviewed exact config repair", ApprovedAt: time.Now().UTC(),
	}
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.LoadMergeApproval()
	if err != nil || !exists {
		t.Fatalf("load approval: exists=%t err=%v", exists, err)
	}
	gate := hivegithub.PullRequestGate{Number: 7, HeadSHA: approval.HeadSHA, BaseSHA: approval.BaseSHA, BaseBranch: "main", ChangedFiles: []string{"visual-hive.config.yaml"}}
	if err := ValidateMergeApproval(loaded, "owner/repo", "123", approval.DiffDigest, gate); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*hivegithub.PullRequestGate, *MergeApproval, *string){
		"head":    func(g *hivegithub.PullRequestGate, _ *MergeApproval, _ *string) { g.HeadSHA = strings.Repeat("d", 40) },
		"base":    func(g *hivegithub.PullRequestGate, _ *MergeApproval, _ *string) { g.BaseSHA = strings.Repeat("d", 40) },
		"pr":      func(g *hivegithub.PullRequestGate, _ *MergeApproval, _ *string) { g.Number++ },
		"repo":    func(_ *hivegithub.PullRequestGate, a *MergeApproval, _ *string) { a.Repository = "other/repo" },
		"repo_id": func(_ *hivegithub.PullRequestGate, a *MergeApproval, _ *string) { a.RepositoryID = "456" },
		"diff": func(_ *hivegithub.PullRequestGate, _ *MergeApproval, digest *string) {
			*digest = strings.Repeat("e", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedGate, changedApproval, digest := gate, loaded, approval.DiffDigest
			mutate(&changedGate, &changedApproval, &digest)
			if err := ValidateMergeApproval(changedApproval, "owner/repo", "123", digest, changedGate); err == nil {
				t.Fatal("changed approval identity was accepted")
			}
		})
	}
}

func TestPathApprovalCannotOverrideAnyOtherMergeGate(t *testing.T) {
	config := Config{
		Repository: "owner/repo", DefaultBranch: "main", Automation: AutomationAutoMerge, ACMMLevel: 6, MaxRepairAttempts: 4,
		AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
	}
	finding := visualhive.FindingLifecycle{OwningAgentHint: "quality", RepairAttempts: 1, RepairCommitSHA: strings.Repeat("a", 40)}
	baseGate := hivegithub.PullRequestGate{
		Number: 7, Open: true, HeadSHA: finding.RepairCommitSHA, BaseSHA: strings.Repeat("b", 40), BaseBranch: "main", MergeableKnown: true, Mergeable: true,
		ChangedFiles: []string{"visual-hive.config.yaml"}, VisualHiveVerdictGreen: true, RequiredCheckNames: []string{"visual-hive"}, RequiredCheckStates: []string{"success"}, BranchProtectionEnabled: true,
	}
	policy := integratedPolicy(config)
	request := mergeActionRequest(config, finding, baseGate)
	denied := policy.Authorize(request)
	approved, eligible := authorizePathApprovedMerge(policy, request, denied)
	if !eligible || !approved.Allowed {
		t.Fatalf("path-only denial was not approvable: denied=%+v approved=%+v", denied, approved)
	}

	cases := map[string]func(*hivegithub.PullRequestGate, *Config, *visualhive.FindingLifecycle){
		"unknown_mergeability": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.MergeableKnown = false
		},
		"unmergeable": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) { g.Mergeable = false },
		"red_visual": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.VisualHiveVerdictGreen = false
		},
		"red_check": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.RequiredCheckStates = []string{"failure"}
		},
		"missing_checks": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.RequiredCheckStates = nil
		},
		"unprotected": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.BranchProtectionEnabled = false
		},
		"hold": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) { g.Hold = true },
		"review": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.HumanReviewRequired = true
		},
		"baseline": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.BaselineChanged = true
		},
		"workflow": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.WorkflowChanged = true
		},
		"security": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.SecuritySensitive = true
		},
		"deployment": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.DeploymentChanged = true
		},
		"risk": func(g *hivegithub.PullRequestGate, _ *Config, _ *visualhive.FindingLifecycle) {
			g.ChangedFiles = []string{"src/app.ts"}
		},
		"paused": func(_ *hivegithub.PullRequestGate, c *Config, _ *visualhive.FindingLifecycle) { c.Paused = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			gate, changedConfig, changedFinding := baseGate, config, finding
			mutate(&gate, &changedConfig, &changedFinding)
			changedPolicy := integratedPolicy(changedConfig)
			changedRequest := mergeActionRequest(changedConfig, changedFinding, gate)
			decision := changedPolicy.Authorize(changedRequest)
			if override, eligible := authorizePathApprovedMerge(changedPolicy, changedRequest, decision); eligible && override.Allowed {
				t.Fatalf("approval bypassed %s: denied=%+v override=%+v", name, decision, override)
			}
		})
	}
}

func TestRepositoryTestRepairAlwaysRequiresExactPathApproval(t *testing.T) {
	config := exactTestMergeConfig()
	config.AllowedAutoMergePaths = []string{"**/*.test.*", "**/*_test.go"}
	policy := integratedPolicy(config)
	finding := visualhive.FindingLifecycle{
		IssueKind: visualhive.RepositoryTestFailureKind, RepairAttempts: 1,
		RepairCommitSHA: strings.Repeat("a", 40), OwningAgentHint: "quality",
	}
	gate := exactTestMergeGate()
	gate.HeadSHA = finding.RepairCommitSHA
	gate.ChangedFiles = []string{"src/security-audit.test.ts"}
	request := mergeActionRequest(config, finding, gate)
	if ordinary := policy.Authorize(request); !ordinary.Allowed {
		t.Fatalf("fixture must prove the global test-only auto-merge policy would allow this change: %+v", ordinary)
	}
	perFinding := mergePolicyForFinding(policy, finding)
	denied := perFinding.Authorize(request)
	if denied.Allowed || len(denied.Reasons) != 1 || !strings.Contains(denied.Reasons[0], "outside the auto-merge allowlist") {
		t.Fatalf("repository-test repair did not receive an exact path-policy hold: %+v", denied)
	}
	approved, eligible := authorizePathApprovedMerge(perFinding, request, denied)
	if !eligible || !approved.Allowed {
		t.Fatalf("exact-head approval could not override only the forced path hold: eligible=%t decision=%+v", eligible, approved)
	}
}

func TestHeldApprovedFindingIsReachableAndAmbiguityFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	store, _ := NewStore(filepath.Join(stateDir, "integrated"))
	approval := MergeApproval{
		SchemaVersion: MergeApprovalSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: 7,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseBranch: "main", DiffDigest: strings.Repeat("c", 64),
		Actor: "accountable-user", Reason: "reviewed", ApprovedAt: time.Now().UTC(),
	}
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"finding": {RepositoryFingerprint: "finding", IssueNumber: 6, PRNumber: 7, RepairCommitSHA: approval.HeadSHA, Status: visualhive.StatusReady, HumanReviewRequired: true, ManualReviewKind: "merge_policy"},
	}}
	finding, ok, err := repairFindingForOrchestration(stateDir, state)
	if err != nil || !ok || finding.RepositoryFingerprint != "finding" {
		t.Fatalf("held approval was not reachable: ok=%t finding=%+v err=%v", ok, finding, err)
	}
	copy := *state.Findings["finding"]
	copy.RepositoryFingerprint = "duplicate"
	state.Findings["duplicate"] = &copy
	if _, _, err := repairFindingForOrchestration(stateDir, state); err == nil {
		t.Fatal("ambiguous approval mapping was accepted")
	}
}

func TestObservationReviewInvalidatesPreexistingApprovalBeforeSelection(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	approval := MergeApproval{
		SchemaVersion: MergeApprovalSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: 7,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseBranch: "main", DiffDigest: strings.Repeat("c", 64),
		Actor: "accountable-user", Reason: "reviewed before new evidence", ApprovedAt: time.Now().UTC(),
	}
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{
		"held": {
			Repository: "owner/repo", RepositoryFingerprint: "held", Status: visualhive.StatusReady, IssueNumber: 6,
			PRNumber: 7, RepairCommitSHA: approval.HeadSHA, HumanReviewRequired: true, ManualReviewKind: "merge_policy",
			ObservationHumanReviewRequired: true,
		},
		"next": {Repository: "owner/repo", RepositoryFingerprint: "next", Status: visualhive.StatusIssueOpen, IssueNumber: 8},
	}, ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{}}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
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
	if _, _, err := repairFindingForOrchestration(stateDir, lifecycle.Snapshot()); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("preexisting approval selected a newly observation-held finding: %v", err)
	}
	if err := reconcileStaleMergeApproval(context.Background(), stateDir, Config{Repository: "owner/repo", RepositoryID: "123"}, lifecycle, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("observation-invalidated approval remained active: exists=%t err=%v", exists, err)
	}
	selected, exists, err := repairFindingForOrchestration(stateDir, lifecycle.Snapshot())
	if err != nil || exists {
		t.Fatalf("invalidated approval released the repository-wide one-active-PR slot while the held PR still exists: selected=%+v exists=%t err=%v", selected, exists, err)
	}
}

func TestApproveMergeBindsAuthenticatedActorAndRawDiff(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	rawDiff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+new\n"
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.URL.Path == "/user":
			_, _ = io.WriteString(writer, `{"login":"authenticated-reviewer","id":9001,"type":"User"}`)
		case request.URL.Path == "/repos/owner/repo/pulls/7" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, rawDiff)
		case request.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = io.WriteString(writer, `{"number":7,"changed_files":1,"state":"open","draft":false,"mergeable":true,"head":{"sha":"`+head+`","ref":"hive/repair-proof","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"`+base+`"},"labels":[]}`)
		case request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"visual-hive.config.yaml"}]`)
		case request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42},{"context":"unit","app_id":42}]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":1,"name":"visual-hive","app":{"id":42},"check_suite":{"id":701},"head_sha":"`+head+`","status":"completed","conclusion":"success"},{"id":2,"name":"unit","app":{"id":42},"head_sha":"`+head+`","status":"completed","conclusion":"success"}]}`)
		case request.URL.Path == "/repos/owner/repo/commits/"+head+"/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.URL.Path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[{"id":702,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","event":"pull_request","head_branch":"hive/repair-proof","head_sha":"`+head+`","status":"completed","conclusion":"success","check_suite_id":701,"repository":{"full_name":"owner/repo"},"head_repository":{"full_name":"owner/repo"},"pull_requests":[{"number":7,"head":{"sha":"`+head+`","ref":"hive/repair-proof"},"base":{"sha":"`+base+`","ref":"main"}}]}]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	store, _ := NewStore(filepath.Join(stateDir, "integrated"))
	if err := store.Save(Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Automation: AutomationAutoMerge, ACMMLevel: 6,
		MaxRepairAttempts: 4, AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
	}); err != nil {
		t.Fatal(err)
	}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lifecycleState := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{"finding": {
			Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusReady, IssueNumber: 6,
			PRNumber: 7, RepairCommitSHA: head, RepairAttempts: 1, HumanReviewRequired: true, ManualReviewKind: "merge_policy",
		}}, ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, _ := json.Marshal(lifecycleState)
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	lifecycleState.Findings["finding"].ObservationHumanReviewRequired = true
	data, _ = json.Marshal(lifecycleState)
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveMerge(context.Background(), ApproveMergeOptions{
		StateDir: stateDir, PRNumber: 7, ExpectedHeadSHA: head, PlanOnly: true, GitHub: client,
	}); err == nil || !strings.Contains(err.Error(), "not a ready Hive repair") {
		t.Fatalf("observation-level authority was offered a merge-policy approval plan: %v", err)
	}
	lifecycleState.Findings["finding"].ObservationHumanReviewRequired = false
	data, _ = json.Marshal(lifecycleState)
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := ApproveMerge(context.Background(), ApproveMergeOptions{
		StateDir: stateDir, PRNumber: 7, ExpectedHeadSHA: head, PlanOnly: true, GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Planned || plan.DiffDigest == "" || plan.Gate.BaseSHA != base {
		t.Fatalf("approval plan was not exact: %+v", plan)
	}
	if _, exists, err := store.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("read-only approval plan persisted authority: exists=%t err=%v", exists, err)
	}
	if _, err := ApproveMerge(context.Background(), ApproveMergeOptions{
		StateDir: stateDir, PRNumber: 7, ExpectedHeadSHA: head, ExpectedBaseSHA: base,
		ExpectedDiff: strings.Repeat("f", 64), Reason: "stale review", GitHub: client,
	}); err == nil {
		t.Fatal("approval apply accepted a diff digest other than the reviewed plan")
	}
	result, err := ApproveMerge(context.Background(), ApproveMergeOptions{
		StateDir: stateDir, PRNumber: 7, ExpectedHeadSHA: head, ExpectedBaseSHA: base,
		ExpectedDiff: plan.DiffDigest, Reason: "reviewed exact config repair", GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval.Actor != "authenticated-reviewer" || result.Approval.RepositoryID != "123" || result.Approval.DiffDigest == "" {
		t.Fatalf("approval identity was not bound: %+v", result.Approval)
	}
	loaded, exists, err := store.LoadMergeApproval()
	if err != nil || !exists || loaded.DiffDigest != result.Approval.DiffDigest {
		t.Fatalf("approval was not persisted: exists=%t approval=%+v err=%v", exists, loaded, err)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "authenticated-reviewer") || !strings.Contains(string(audit), result.Approval.DiffDigest) {
		t.Fatalf("approval audit is incomplete: %q err=%v", audit, err)
	}
	revoked, err := RevokeMergeApproval(context.Background(), RevokeMergeApprovalOptions{StateDir: stateDir, Reason: "operator cancelled approval", GitHub: client})
	if err != nil || !revoked.Revoked || revoked.Actor != "authenticated-reviewer" {
		t.Fatalf("approval revocation failed: %+v err=%v", revoked, err)
	}
	if _, exists, err := store.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("revoked approval remained active: exists=%t err=%v", exists, err)
	}
}

func TestStaleApprovalIsInvalidatedBeforeFindingSelection(t *testing.T) {
	oldHead, newHead, base := strings.Repeat("a", 40), strings.Repeat("d", 40), strings.Repeat("b", 40)
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.URL.Path == "/repos/owner/repo/pulls/7" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n-old\n+changed\n")
		case request.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = io.WriteString(writer, `{"number":7,"changed_files":1,"state":"open","draft":false,"mergeable":true,"head":{"sha":"`+newHead+`"},"base":{"ref":"main","sha":"`+base+`"},"labels":[]}`)
		case request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"visual-hive.config.yaml"}]`)
		case request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case request.URL.Path == "/repos/owner/repo/commits/"+newHead+"/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"id":1,"name":"visual-hive","app":{"id":42},"status":"completed","conclusion":"success"}]}`)
		case request.URL.Path == "/repos/owner/repo/commits/"+newHead+"/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	stateDir := t.TempDir()
	store, _ := NewStore(filepath.Join(stateDir, "integrated"))
	approval := MergeApproval{SchemaVersion: MergeApprovalSchema, Repository: "owner/repo", RepositoryID: "123", PRNumber: 7, HeadSHA: oldHead, BaseSHA: base, BaseBranch: "main", DiffDigest: strings.Repeat("c", 64), Actor: "reviewer", Reason: "reviewed", ApprovedAt: time.Now().UTC()}
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{
		"held": {Repository: "owner/repo", RepositoryFingerprint: "held", Status: visualhive.StatusReady, IssueNumber: 6, PRNumber: 7, RepairCommitSHA: oldHead, HumanReviewRequired: true, ManualReviewKind: "merge_policy"},
		"next": {Repository: "owner/repo", RepositoryFingerprint: "next", Status: visualhive.StatusIssueOpen, IssueNumber: 8},
	}, ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := reconcileStaleMergeApproval(context.Background(), stateDir, Config{Repository: "owner/repo", RepositoryID: "123"}, lifecycle, client); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("stale approval remained active: exists=%t err=%v", exists, err)
	}
	selected, exists, err := repairFindingForOrchestration(stateDir, lifecycle.Snapshot())
	if err != nil || exists {
		t.Fatalf("stale approval invalidation released the one-active-PR slot while its held repair PR still exists: selected=%+v exists=%t err=%v", selected, exists, err)
	}
}

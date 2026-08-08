package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

const acceptanceRepairDiff = "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed by governed Visual Hive proposal\n"

// TestNormalVisualWorkVerticalAcceptance is intentionally a local, no-merge
// proof. Every opaque evidence capability is minted by the production GitHub
// verifier from untrusted HTTP/ZIP responses; the test never constructs a
// verified packet or PR receipt directly.
func TestNormalVisualWorkVerticalAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	repository, remote, baseSHA := seedAcceptanceRepository(t)
	stateDir := filepath.Join(t.TempDir(), "hive-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	producerCommit := strings.Repeat("a", 40)
	firstFixture := buildAcceptanceSourceFixture(t, acceptanceSourceFixtureOptions{
		BundleID: "acceptance-work", RunID: 42, RunAttempt: 2, BundleArtifactID: 99, SourceArtifactID: 98,
		BaseSHA: baseSHA, ProducerCommit: producerCommit, IncludeFinding: true, Authoritative: true,
	})
	secondFixture := buildAcceptanceSourceFixture(t, acceptanceSourceFixtureOptions{
		BundleID: "acceptance-after-close", RunID: 43, RunAttempt: 1, BundleArtifactID: 199, SourceArtifactID: 198,
		BaseSHA: baseSHA, ProducerCommit: producerCommit, IncludeFinding: false, Authoritative: false,
	})
	api := newAcceptanceGitHubAPI(t, remote, baseSHA, firstFixture, secondFixture)
	defer api.Close()
	githubClient := hivegithub.NewClientForTest(api.URL(), "owner", []string{"repo"}, logger)

	installed := integrated.Config{
		SchemaVersion: integrated.ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: integrated.CoverageStandard, Automation: integrated.AutomationRepairPR, Provider: "codex",
		ProviderCommand: "acceptance-contained-codex", ProviderArgs: []string{"--sealed"}, ExecutionMode: integrated.ExecutionLocal,
		RunIntervalSeconds: 60, ACMMLevel: 5, MaxActiveIssues: 2, MaxRepairAttempts: 2,
		VisualHive: true, VisualHiveRef: producerCommit, TestCommands: [][]string{{"git", "diff", "--check"}},
		AllowedRepairPaths: []string{"src/**"}, CheckoutDir: repository, StateDir: stateDir,
	}

	policyRoot := filepath.Join(t.TempDir(), "policies")
	policyDir := filepath.Join(policyRoot, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(policyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policyDir, "quality.md"), []byte("ACCEPTANCE_QUALITY_POLICY\nproject=${PROJECT_NAME}\nrepos=${AUTHORIZED_REPOS}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	includeRepositories := true
	agentConfig := config.AgentConfig{
		Enabled: true, Role: "quality", Backend: "copilot", Model: "persistent-parent-model", LaunchCmd: "never-launch-persistent-parent",
		KickTemplate: "quality.md", IncludeRepos: &includeRepositories, CavemanMode: "full",
		Tools:       &config.ToolsConfig{Preset: "acceptance", Rules: []config.ToolRule{{Pattern: "git diff --check", Action: "allow", Reason: "verified validation only"}}},
		Connections: []config.ConnectionConfig{{Name: "project-wiki", Type: "knowledge", URI: "https://inert.invalid/wiki"}},
	}
	normalConfig := &config.Config{
		HiveID: "acceptance-hive", Project: config.ProjectConfig{Org: "owner", Name: "acceptance", PrimaryRepo: "repo", Repos: []string{"repo"}},
		Agents: map[string]config.AgentConfig{"quality": agentConfig}, Policies: config.PoliciesConfig{LocalDir: policyRoot},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"quality": "1m"}}}},
	}

	primer := newAcceptanceKnowledgePrimer(t, logger)
	sched := scheduler.New(normalConfig, logger)
	sched.SetPrimer(primer)
	t.Setenv("HIVE_WORK_DIR", filepath.Join(t.TempDir(), "ordinary-manager"))
	manager := agent.NewManager(normalConfig.Agents, logger, agent.ProjectContext{Org: "owner", Repos: []string{"repo"}, ACMMLevel: 5, PRsAllowed: true, PolicyDir: policyRoot})
	manager.SetACMMLevel(5)
	executor := &acceptanceContainedExecutor{identity: agent.SpecialistChildExecutorIdentity{
		Backend: "codex", ProviderSHA256: strings.Repeat("d", 64), Model: "acceptance-codex-model", ConfigurationSHA256: strings.Repeat("e", 64),
	}}

	ownershipConfigurations := 0
	normalOwnership, err := claimNormalVisualWorkOwnership(stateDir, manager, func(candidate *agent.Manager) (bool, error) {
		if candidate != manager {
			return false, errors.New("normal ownership did not receive the ordinary Manager")
		}
		ownershipConfigurations++
		if configureErr := candidate.ConfigureSpecialistChildDispatcher(agent.SpecialistChildDispatcherOptions{
			RepairStateRoot: filepath.Join(stateDir, "repair"), Executor: executor,
		}); configureErr != nil {
			return false, configureErr
		}
		return true, nil
	})
	if err != nil || normalOwnership == nil || ownershipConfigurations != 1 {
		t.Fatalf("claim and configure normal runtime ownership: lease=%v configurations=%d err=%v", normalOwnership, ownershipConfigurations, err)
	}
	defer releaseDaemonLease(normalOwnership)
	legacyFactories := 0
	legacy, legacyErr := claimIntegratedDaemonRuntime(stateDir, installed, func(string, integrated.Config) (*integratedSpecialistRuntime, error) {
		legacyFactories++
		return &integratedSpecialistRuntime{}, nil
	})
	if !errors.Is(legacyErr, errDaemonLeaseHeld) || legacy != nil || legacyFactories != 0 {
		t.Fatalf("normal ownership admitted a duplicate legacy manager: runtime=%v factories=%d err=%v", legacy, legacyFactories, legacyErr)
	}

	quality, err := beads.NewStore(filepath.Join(stateDir, "beads", "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(stateDir, "visual-hive"))
	if err != nil {
		t.Fatal(err)
	}
	gov := governor.New(normalConfig.Governor, normalConfig.Agents, logger)
	issues := &acceptanceIssueClient{}
	controller, err := visualcontroller.New(gov, lifecycle, map[string]*beads.Store{"quality": quality}, manager, issues, installed, 5)
	if err != nil {
		t.Fatal(err)
	}
	controller.SetRuntimeConfigLoader(func() (integrated.Config, int, error) { return installed, manager.GetACMMLevel(), nil })
	intake := &acceptanceIntake{controller: controller}
	source := &acceptanceArtifactSource{
		client: githubClient, installed: installed, fixtures: []acceptanceSourceFixture{firstFixture, secondFixture}, destination: filepath.Join(stateDir, "visual-hive", "artifacts"),
		loseFirstConsumeResponse: true,
	}
	transportCalls := 0
	repairer := &normalVisualRepairer{
		scheduler: sched, manager: manager, controller: controller, lifecycle: lifecycle, github: githubClient,
		providerCommand: installed.ProviderCommand, providerArgs: append([]string(nil), installed.ProviderArgs...), expectedRemoteURL: remote,
		gitTransportToken: func(context.Context) (string, error) {
			transportCalls++
			return "normal-controller-private-token", nil
		},
		loadConfig: func() (integrated.Config, int, error) { return installed, manager.GetACMMLevel(), nil },
	}
	verifier := &normalVisualPullRequestVerifier{
		github: githubClient, lifecycle: lifecycle,
		loadConfig: func() (integrated.Config, int, error) { return installed, manager.GetACMMLevel(), nil },
		// The vertical fixture exercises the successful v3 bundle path. Its
		// dedicated GitHub server proves the full bundle separately, so bind
		// the new preinspection seam to the same successful exact-head gate.
		inspectGate: func(context.Context, string, int) (hivegithub.PullRequestGate, error) {
			return hivegithub.PullRequestGate{VisualHiveCheckState: "success"}, nil
		},
	}
	newService := func(verdict normalservice.PullRequestVerdictVerifier) *normalservice.Service {
		service, serviceErr := normalservice.New(normalservice.Options{
			StateDir: filepath.Join(stateDir, "visual-hive"), PollInterval: time.Second, LeaseRetry: time.Millisecond,
			AcquireLease:  func() (func(), error) { return integrated.AcquireNormalVisualWorkLease(stateDir) },
			ShouldQuiesce: func() (bool, error) { return false, nil },
			Source:        source, Intake: intake, Repairer: repairer, Verdict: verdict, PullRequestState: verifier, Logger: logger,
		})
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return service
	}

	// First epoch deliberately has no final verifier. It must stop after one
	// governed Worker PR, leaving enough durable identity for restart.
	if err := newService(nil).RunCycle(ctx); !errors.Is(err, normalservice.ErrFinalVerdictPending) {
		t.Fatalf("first cycle should wait at exact-head verdict: %v", err)
	}
	first := api.Snapshot()
	checks, starts, childRequest := executor.Snapshot()
	if transportCalls != 0 || executor.SawControllerGitToken() {
		t.Fatalf("normal Git transport scope drifted: controller resolutions=%d child_received_token=%t", transportCalls, executor.SawControllerGitToken())
	}
	if source.FetchCalls() != 1 || intake.Imports() != 1 || len(gov.AdmissionHistory()) != 1 || issues.Creates() != 1 ||
		checks != 2 || starts != 1 || first.PRCreates != 1 || first.PREdits != 0 || first.PRHeadSHA == "" || first.PRBranch == "" ||
		countAcceptanceRepairBranches(t, remote) != 1 || source.ConsumeCalls() != 0 || intake.Completions() != 0 {
		t.Fatalf("first vertical side effects drifted: fetch=%d import=%d admissions=%d issues=%d checks=%d starts=%d api=%+v branches=%d consume=%d complete=%d",
			source.FetchCalls(), intake.Imports(), len(gov.AdmissionHistory()), issues.Creates(), checks, starts, first, countAcceptanceRepairBranches(t, remote), source.ConsumeCalls(), intake.Completions())
	}
	containedOrder, _, decodeErr := decodeAcceptanceContainedPrompt(childRequest.Prompt)
	if decodeErr != nil {
		t.Fatalf("decode controller-bound contained request: %v", decodeErr)
	}
	for _, marker := range []string{
		"ACCEPTANCE_QUALITY_POLICY", "Project identity snapshot", "Project: acceptance", "owner/repo", "ACCEPTANCE_PRIMER_FACT",
		"value is broken",
	} {
		if !strings.Contains(containedOrder.TaskPrompt, marker) {
			t.Fatalf("contained governed prompt omitted %q", marker)
		}
	}
	governedInput, err := decodeAcceptanceGovernedInput(containedOrder.TaskPrompt)
	if err != nil || governedInput.Capability.ConfiguredRoleConnectionCount != 1 || governedInput.Capability.KnowledgeMode != "read-only-primer-snapshot" ||
		governedInput.Capability.ToolPolicySHA256 == "" || governedInput.ProjectContextSnapshot == "" || governedInput.KnowledgePrimerSnapshot == "" {
		t.Fatalf("contained governed prompt lost role tools/connections, project, or primer snapshot: input=%+v err=%v", governedInput, err)
	}
	if childRequest.Root == "" || strings.HasPrefix(strings.ToLower(filepath.Clean(childRequest.Root)), strings.ToLower(filepath.Clean(repository))+string(filepath.Separator)) {
		t.Fatalf("proposal child received the repository checkout: root=%q repository=%q", childRequest.Root, repository)
	}
	if value := strings.TrimSpace(acceptanceGit(t, remote, "show", first.PRBranch+":src/value.txt")); value != "fixed by governed Visual Hive proposal" {
		t.Fatalf("Worker did not push the governed patch to the local bare remote: %q", value)
	}
	if finding, ok := lifecycle.Finding(first.RepositoryFingerprint); !ok || finding.Status != visualhive.StatusPROpen || finding.PRNumber != 7 || finding.RepairCommitSHA != first.PRHeadSHA {
		t.Fatalf("Worker PR was not durably owned by the existing lifecycle: found=%t finding=%+v", ok, finding)
	}

	// The final evidence fixture is built only after the real Worker commit is
	// known. It remains untrusted bytes until the production PR-v3 verifier
	// independently binds the live API, run, App, artifacts, source, and MAC.
	api.SetPullRequestFixture(buildAcceptancePullRequestFixture(t, acceptancePullRequestFixtureOptions{
		BaseSHA: baseSHA, HeadSHA: first.PRHeadSHA, HeadBranch: first.PRBranch,
	}))
	consumeLoss := newService(verifier).RunCycle(ctx)
	if consumeLoss == nil || !strings.Contains(consumeLoss.Error(), "injected consume response loss") {
		t.Fatalf("sealed verdict did not reach the consume response-loss boundary: %v", consumeLoss)
	}
	afterVerdict := api.Snapshot()
	checks, starts, _ = executor.Snapshot()
	if afterVerdict.PRCreates != 1 || afterVerdict.PRV3BundleDownloads != 1 || afterVerdict.PRV3SourceDownloads != 1 ||
		checks != 2 || starts != 1 || source.FetchCalls() != 1 || intake.Imports() != 1 || intake.Completions() != 1 ||
		source.ConsumeCalls() != 1 || source.ConsumeEffects() != 1 || countAcceptanceRepairBranches(t, remote) != 1 {
		t.Fatalf("verdict restart duplicated work or missed the single consume effect: api=%+v checks=%d starts=%d fetch=%d import=%d complete=%d consumeCalls=%d consumeEffects=%d branches=%d",
			afterVerdict, checks, starts, source.FetchCalls(), intake.Imports(), intake.Completions(), source.ConsumeCalls(), source.ConsumeEffects(), countAcceptanceRepairBranches(t, remote))
	}
	ready, ok := lifecycle.Finding(first.RepositoryFingerprint)
	if !ok || ready.Status != visualhive.StatusReady || ready.LastPullRequestCheckReceipt == nil || ready.PRNumber != 7 || ready.IssueNumber <= 0 || ready.PRURL == "" {
		t.Fatalf("real sealed check-only receipt did not produce one Ready, still-open lifecycle: found=%t finding=%+v", ok, ready)
	}
	if ready.Status == visualhive.StatusResolved || ready.Status == visualhive.StatusIssueClosed {
		t.Fatalf("PR check evidence improperly resolved the issue lifecycle: %+v", ready)
	}

	// A second restart must replay only the already-applied consume. The source
	// records one logical effect even though the first response was lost.
	if err := newService(verifier).RunCycle(ctx); err != nil {
		t.Fatalf("consume response-loss replay: %v", err)
	}
	if source.FetchCalls() != 1 || source.ConsumeCalls() != 2 || source.ConsumeEffects() != 1 || intake.Completions() != 1 || api.Snapshot().PRCreates != 1 {
		t.Fatalf("consume replay duplicated a durable effect: fetch=%d calls=%d effects=%d complete=%d api=%+v",
			source.FetchCalls(), source.ConsumeCalls(), source.ConsumeEffects(), intake.Completions(), api.Snapshot())
	}

	// The exact live open PR is a repository-wide serialization gate. Packet 2
	// cannot even be fetched while it remains open.
	if err := newService(verifier).RunCycle(ctx); !errors.Is(err, normalservice.ErrOpenPullRequest) {
		t.Fatalf("open exact Worker PR did not block packet 2: %v", err)
	}
	if source.FetchCalls() != 1 || intake.Imports() != 1 || api.Snapshot().PRCreates != 1 {
		t.Fatalf("open PR gate leaked a later packet or PR: fetch=%d import=%d api=%+v", source.FetchCalls(), intake.Imports(), api.Snapshot())
	}

	// A read-only exact state change to closed releases the next fetch. Packet 2
	// is deliberately non-authoritative and empty, so it produces no repair,
	// baseline, merge, or lifecycle-resolution authority.
	api.SetPullRequestState("closed", false)
	if err := newService(verifier).RunCycle(ctx); !errors.Is(err, normalservice.ErrNoDispatch) {
		t.Fatalf("exact closed PR did not release the next non-repair packet: %v", err)
	}
	finalAPI := api.Snapshot()
	checks, starts, _ = executor.Snapshot()
	if source.FetchCalls() != 2 || intake.Imports() != 2 || len(gov.AdmissionHistory()) != 1 || checks != 2 || starts != 1 ||
		finalAPI.PRCreates != 1 || finalAPI.PREdits != 0 || finalAPI.Merges != 0 || countAcceptanceRepairBranches(t, remote) != 1 ||
		source.ConsumeEffects() != 2 || issues.Creates() != 1 {
		t.Fatalf("closed release changed the one-proposal acceptance invariant: fetch=%d import=%d admissions=%d checks=%d starts=%d api=%+v branches=%d consumes=%d issues=%d",
			source.FetchCalls(), intake.Imports(), len(gov.AdmissionHistory()), checks, starts, finalAPI, countAcceptanceRepairBranches(t, remote), source.ConsumeEffects(), issues.Creates())
	}
	status, err := manager.GetStatus("quality")
	if err != nil || status.PID != 0 || status.HasLaunched || status.StartedAt != nil {
		t.Fatalf("ordinary Manager launched a persistent parent agent: status=%+v err=%v", status, err)
	}
	if legacyFactories != 0 {
		t.Fatalf("legacy specialist factory ran despite normal ownership: %d", legacyFactories)
	}
}

type acceptanceArtifactSource struct {
	mu                       sync.Mutex
	client                   *hivegithub.Client
	installed                integrated.Config
	fixtures                 []acceptanceSourceFixture
	destination              string
	fetchCalls               int
	consumeCalls             int
	consumeEffects           int
	consumed                 map[int64]bool
	loseFirstConsumeResponse bool
}

func (source *acceptanceArtifactSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	source.mu.Lock()
	index := source.fetchCalls
	if index >= len(source.fixtures) {
		source.mu.Unlock()
		return integrated.NormalVisualWork{}, errors.New("acceptance source has no later packet")
	}
	fixture := source.fixtures[index]
	source.fetchCalls++
	source.mu.Unlock()
	_, verified, err := source.client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
		Repository: source.installed.Repository, WorkflowRunID: fixture.RunID, ArtifactID: fixture.BundleArtifactID,
		SourceArtifactID: fixture.SourceArtifactID, FetchSourceArtifact: true,
		DestinationDir: filepath.Join(source.destination, fmt.Sprintf("run-%d", fixture.RunID)), TargetRef: source.installed.DefaultBranch, MaxACMM: source.installed.ACMMLevel,
		ExpectedProducerGitCommit: source.installed.VisualHiveRef, ExpectedWorkflowName: acceptanceSourceWorkflowName,
		ExpectedWorkflowPath: acceptanceSourceWorkflowPath, ExpectedEvent: "workflow_dispatch",
	})
	if err != nil {
		return integrated.NormalVisualWork{}, err
	}
	return integrated.NormalVisualWork{Config: source.installed, Artifact: verified, Workflow: integrated.WorkflowRunEvidence{
		CorrelationID: acceptanceSHA256([]byte(fmt.Sprintf("acceptance-workflow-%d", fixture.RunID))), RunID: fixture.RunID,
		RunURL: fmt.Sprintf("https://github.test/owner/repo/actions/runs/%d", fixture.RunID), HeadSHA: fixture.BaseSHA, Conclusion: "success",
		EvidenceArtifact: fixture.SourceArtifactID, BundleArtifact: fixture.BundleArtifactID,
	}}, nil
}

func (source *acceptanceArtifactSource) Consume(workflow integrated.WorkflowRunEvidence, allowAlreadyConsumed bool) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.consumeCalls++
	if source.consumed == nil {
		source.consumed = map[int64]bool{}
	}
	if source.consumed[workflow.RunID] {
		if !allowAlreadyConsumed {
			return errors.New("acceptance source rejected a consumed workflow without replay authority")
		}
		return nil
	}
	source.consumed[workflow.RunID] = true
	source.consumeEffects++
	if source.loseFirstConsumeResponse {
		source.loseFirstConsumeResponse = false
		return errors.New("injected consume response loss after durable source effect")
	}
	return nil
}

func (source *acceptanceArtifactSource) FetchCalls() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.fetchCalls
}

func (source *acceptanceArtifactSource) ConsumeCalls() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.consumeCalls
}

func (source *acceptanceArtifactSource) ConsumeEffects() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.consumeEffects
}

type acceptanceIntake struct {
	mu          sync.Mutex
	controller  *visualcontroller.Controller
	imports     int
	completions int
}

func (intake *acceptanceIntake) Import(ctx context.Context, artifact hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	intake.mu.Lock()
	intake.imports++
	intake.mu.Unlock()
	return intake.controller.Import(ctx, artifact)
}

func (intake *acceptanceIntake) RevalidateSpecialistBoundary(source, order, digest string) (visualcontroller.DispatchEnvelope, error) {
	return intake.controller.RevalidateSpecialistBoundary(source, order, digest)
}

func (intake *acceptanceIntake) DeferUnselectedSpecialistDispatches(ctx context.Context, selected string, deferred []string, correlation string) error {
	return intake.controller.DeferUnselectedSpecialistDispatches(ctx, selected, deferred, correlation)
}

func (intake *acceptanceIntake) CompleteSpecialistPullRequest(source string, completion visualcontroller.SpecialistPullRequestCompletion) error {
	err := intake.controller.CompleteSpecialistPullRequest(source, completion)
	if err == nil {
		intake.mu.Lock()
		intake.completions++
		intake.mu.Unlock()
	}
	return err
}

func (intake *acceptanceIntake) Imports() int {
	intake.mu.Lock()
	defer intake.mu.Unlock()
	return intake.imports
}

func (intake *acceptanceIntake) Completions() int {
	intake.mu.Lock()
	defer intake.mu.Unlock()
	return intake.completions
}

type acceptanceIssueClient struct {
	mu      sync.Mutex
	creates int
}

func (*acceptanceIssueClient) ResolveLifecycleIssueWriter(context.Context, string, string, int) (string, int64, error) {
	return "hive-acceptance", 9001, nil
}

func (client *acceptanceIssueClient) UpsertLifecycleIssueOwned(_ context.Context, repository, _ string, writer string, writerID int64, _ string, _ string, _ []string) (int, string, bool, error) {
	if repository != "owner/repo" || writer != "hive-acceptance" || writerID != 9001 {
		return 0, "", false, errors.New("acceptance issue writer identity drifted")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.creates++
	if client.creates != 1 {
		return 0, "", false, errors.New("acceptance issue was duplicated")
	}
	return 3, "https://github.test/owner/repo/issues/3", true, nil
}

func (*acceptanceIssueClient) UpdateLifecycleIssueOwned(context.Context, string, int, string, string, int64, string, string, string, []string) (int, string, error) {
	return 0, "", errors.New("acceptance proof forbids lifecycle issue updates")
}

func (*acceptanceIssueClient) MigrateLifecycleIssueMarkerOwned(context.Context, string, int, string, string, string, int64, string, string, string, []string) (int, string, error) {
	return 0, "", errors.New("acceptance proof forbids lifecycle marker migration")
}

func (client *acceptanceIssueClient) Creates() int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.creates
}

type acceptanceContainedExecutor struct {
	mu                    sync.Mutex
	identity              agent.SpecialistChildExecutorIdentity
	checks                int
	starts                int
	request               agent.SpecialistChildExecutionRequest
	sawControllerGitToken bool
}

func (executor *acceptanceContainedExecutor) Check(context.Context) (agent.SpecialistChildExecutorIdentity, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.checks++
	identity := executor.identity
	identity.AuthorizationID = fmt.Sprintf("acceptance-auth-%064x", executor.checks)
	return identity, nil
}

func (executor *acceptanceContainedExecutor) Start(ctx context.Context, request agent.SpecialistChildExecutionRequest) (agent.SpecialistChildProcess, error) {
	order, lease, err := decodeAcceptanceContainedPrompt(request.Prompt)
	if err != nil {
		return nil, err
	}
	result := acceptanceSpecialistModelResult{
		SchemaVersion: "hive.specialist-model-result.v1", WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: agent.SpecialistCompletionProposed, Summary: "one governed contained proposal", UnifiedDiff: acceptanceRepairDiff,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	executor.mu.Lock()
	executor.starts++
	executor.request = request
	executor.sawControllerGitToken = executor.sawControllerGitToken || gittransport.HasControllerToken(ctx)
	pid := 31000 + executor.starts
	executor.mu.Unlock()
	return acceptanceContainedProcess{pid: pid, provider: request.ProviderSHA256, result: agent.SpecialistChildExecutionResult{
		Response: encoded, TurnID: request.AuthorizationID, CompletedAt: time.Now().UTC(),
	}}, nil
}

func (executor *acceptanceContainedExecutor) Snapshot() (int, int, agent.SpecialistChildExecutionRequest) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.checks, executor.starts, executor.request
}

type acceptanceContainedProcess struct {
	pid      int
	provider string
	result   agent.SpecialistChildExecutionResult
}

func (process acceptanceContainedProcess) PID() int               { return process.pid }
func (process acceptanceContainedProcess) ProviderSHA256() string { return process.provider }
func (process acceptanceContainedProcess) Wait() (agent.SpecialistChildExecutionResult, error) {
	return process.result, nil
}

func (executor *acceptanceContainedExecutor) SawControllerGitToken() bool {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.sawControllerGitToken
}

func (process acceptanceContainedProcess) ForceReap(context.Context) error {
	return nil
}

type acceptanceSpecialistModelResult struct {
	SchemaVersion    string                           `json:"schema_version"`
	WorkOrderID      string                           `json:"work_order_id"`
	RequestSHA256    string                           `json:"request_sha256"`
	LeaseSHA256      string                           `json:"lease_sha256"`
	SessionID        string                           `json:"session_id"`
	Specialist       agent.SpecialistRole             `json:"specialist"`
	BaseSHA          string                           `json:"base_sha"`
	BaseTreeSHA      string                           `json:"base_tree_sha"`
	TaskPromptSHA256 string                           `json:"task_prompt_sha256"`
	Status           agent.SpecialistCompletionStatus `json:"status"`
	Summary          string                           `json:"summary"`
	UnifiedDiff      string                           `json:"unified_diff"`
}

func decodeAcceptanceContainedPrompt(prompt string) (agent.SpecialistWorkOrder, agent.SpecialistLease, error) {
	const orderMarker = "CONTROLLER_BOUND_WORK_ORDER_JSON\n"
	const leaseMarker = "\nCONTROLLER_BOUND_LEASE_JSON\n"
	const finalMarker = "\n\nFINAL_CAPABILITY_WRAPPER_V1"
	orderStart := strings.LastIndex(prompt, orderMarker)
	if orderStart < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("contained prompt omitted controller-bound work order")
	}
	orderStart += len(orderMarker)
	leaseOffset := strings.Index(prompt[orderStart:], leaseMarker)
	if leaseOffset < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("contained prompt omitted controller-bound lease")
	}
	leaseStart := orderStart + leaseOffset + len(leaseMarker)
	leaseEnd := strings.Index(prompt[leaseStart:], finalMarker)
	if leaseEnd < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("contained prompt omitted final capability wrapper")
	}
	var order agent.SpecialistWorkOrder
	if err := json.Unmarshal([]byte(prompt[orderStart:orderStart+leaseOffset]), &order); err != nil {
		return order, agent.SpecialistLease{}, err
	}
	var lease agent.SpecialistLease
	if err := json.Unmarshal([]byte(prompt[leaseStart:leaseStart+leaseEnd]), &lease); err != nil {
		return order, lease, err
	}
	return order, lease, nil
}

type acceptanceGovernedPromptInput struct {
	ProjectContextSnapshot  string `json:"project_context_snapshot"`
	KnowledgePrimerSnapshot string `json:"knowledge_primer_snapshot"`
	Capability              struct {
		ConfiguredRoleConnectionCount int    `json:"configured_role_connection_count"`
		KnowledgeMode                 string `json:"knowledge_mode"`
		ToolPolicySHA256              string `json:"tool_policy_sha256"`
	} `json:"capability"`
}

func decodeAcceptanceGovernedInput(prompt string) (acceptanceGovernedPromptInput, error) {
	const startMarker = "CANONICAL LOSSLESS INPUT JSON\n"
	const endMarker = "\n\nEXACT TYPED OBSERVATION/FINDING JSON"
	start := strings.Index(prompt, startMarker)
	if start < 0 {
		return acceptanceGovernedPromptInput{}, errors.New("governed prompt omitted canonical input marker")
	}
	start += len(startMarker)
	end := strings.Index(prompt[start:], endMarker)
	if end < 0 {
		return acceptanceGovernedPromptInput{}, errors.New("governed prompt omitted canonical input terminator")
	}
	var input acceptanceGovernedPromptInput
	if err := json.Unmarshal([]byte(prompt[start:start+end]), &input); err != nil {
		return input, err
	}
	return input, nil
}

func newAcceptanceKnowledgePrimer(t *testing.T, logger *slog.Logger) *knowledge.Primer {
	t.Helper()
	server := newAcceptanceKnowledgeServer(t)
	t.Cleanup(server.Close)
	return knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: server.URL, Shared: true}}, knowledge.PrimerConfig{MaxFacts: 5}, logger)
}

func countAcceptanceRepairBranches(t *testing.T, remote string) int {
	t.Helper()
	output := acceptanceGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads")
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "refs/heads/hive/repair-") {
			count++
		}
	}
	return count
}

func acceptanceSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

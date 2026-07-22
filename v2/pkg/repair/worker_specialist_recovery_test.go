package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestWorkerRecoversPreUpgradeStageModelRunningOrderWithoutRecomposition(t *testing.T) {
	repository, remote := seedGitRepository(t)
	baseSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	baseTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	state, err := NewStore(filepath.Join(t.TempDir(), "repair-state"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	anchor := clock
	roleWorkDir := t.TempDir()
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(roleWorkDir, "mailbox"), agent.SpecialistMailboxOptions{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	identity := agent.SpecialistSessionIdentity{
		Specialist: agent.SpecialistQuality, AgentName: "quality", AgentID: "quality", Backend: "codex",
		SessionID: "hive-quality-recovery", TmuxSession: "hive-quality-recovery", WorkDir: roleWorkDir,
		ProviderSHA256: strings.Repeat("e", 64),
	}
	fingerprint := strings.Repeat("9", 64)
	providerConfig := SpecialistProviderConfig{
		Mailbox: mailbox, Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		RecurrenceKey: fingerprint + ":r0", Attempt: 1,
		ExpectedBaseSHA: baseSHA, ExpectedBaseTreeSHA: baseTree,
		Evidence: agent.SpecialistEvidenceIdentity{
			BundleSchemaVersion: "visual-hive.hive-bundle.v3", BundleSHA256: strings.Repeat("b", 64),
			ManifestSHA256: strings.Repeat("d", 64), ArtifactIndexSHA256: strings.Repeat("e", 64),
			VerificationReceiptSHA256: strings.Repeat("c", 64), RepositoryID: "123", WorkflowRunID: 41, WorkflowRunAttempt: 1,
			WorkflowRunHeadSHA: baseSHA, WorkflowName: "Hive Visual Hive Production", WorkflowRunName: "Hive Visual Hive Production [test]",
			WorkflowPath: ".github/workflows/hive-visual-hive.yml", WorkflowEvent: "workflow_dispatch", HeadBranch: "main",
			ArtifactID: 73, SourceArtifactID: 74, ArtifactName: "visual-hive-evidence", ArtifactSHA256: strings.Repeat("d", 64),
		},
		Specialist: agent.SpecialistQuality, RouteReason: "test adequacy routes to quality",
		AllowedPaths: []string{"src/**"}, Validation: []string{"git diff --check"},
		Deadline: anchor.Add(25 * time.Minute), PollInterval: 5 * time.Millisecond, Now: func() time.Time { return clock },
	}
	currentEvidence := providerConfig.Evidence
	currentEvidence.Variant = visualhive.SpecialistEvidenceVariantVisualHiveV3
	evidenceReceipt := visualhive.SpecialistEvidenceReceipt{
		RepositoryID: currentEvidence.RepositoryID, WorkflowRunID: "41", WorkflowRunAttempt: "1",
		ArtifactID: "73", SourceArtifactID: "74", ArtifactName: currentEvidence.ArtifactName,
		CommitSHA: currentEvidence.WorkflowRunHeadSHA, HeadBranch: currentEvidence.HeadBranch, Event: currentEvidence.WorkflowEvent,
		WorkflowName: currentEvidence.WorkflowName, WorkflowRunName: currentEvidence.WorkflowRunName, WorkflowPath: currentEvidence.WorkflowPath,
		BundleSchema: currentEvidence.BundleSchemaVersion, BundleSHA256: currentEvidence.BundleSHA256,
		ManifestSHA256: currentEvidence.ManifestSHA256, ArtifactIndexSHA: currentEvidence.ArtifactIndexSHA256,
	}
	currentEvidence.VerificationReceipt = &evidenceReceipt
	currentEvidence.VerificationReceiptSHA256, _ = evidenceReceipt.SHA256()
	providerConfig.Evidence.ManifestSHA256 = ""
	providerConfig.Evidence.ArtifactIndexSHA256 = ""
	providerConfig.Evidence.RepositoryID = ""
	providerConfig.Evidence.WorkflowName, providerConfig.Evidence.WorkflowRunName, providerConfig.Evidence.WorkflowPath = "", "", ""
	providerConfig.Evidence.WorkflowEvent, providerConfig.Evidence.HeadBranch, providerConfig.Evidence.SourceArtifactID = "", "", 0
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	workerConfig := Config{
		RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote, Agent: "quality",
		Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
		AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
		AttemptStartedAt: anchor, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Fingerprint: "specialist-recovery",
		Status: visualhive.StatusIssueOpen, Title: "Repair the value", Body: "The verified value remains broken.",
		IssueKind: "test_adequacy", Severity: "medium", OwningAgentHint: "quality",
		AffectedContracts: []string{"src/value.txt"}, ValidationCommand: "git diff --check",
		IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}

	firstDispatcher := &fakeSpecialistDispatcher{identity: identity}
	providerConfig.Dispatcher = firstDispatcher
	firstProvider, err := NewSpecialistProvider(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDispatcher.dispatch = func(agent.SpecialistDispatchRequest) { cancelFirst() }
	firstWorker := &Worker{Config: workerConfig, Provider: firstProvider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	_, firstErr := firstWorker.Run(firstCtx, finding)
	if !errors.Is(firstErr, ErrSpecialistWorkPending) {
		t.Fatalf("interrupted first run = %v, want durable pending work", firstErr)
	}
	attempt, ok := state.Get(fingerprint)
	if !ok || attempt.Stage != StageModelRunning || !validSpecialistWorkOrderID(attempt.ModelInvocationID) || attempt.AttemptCounted {
		t.Fatalf("interrupted specialist checkpoint = %+v", attempt)
	}
	if len(firstDispatcher.dispatchCalls) != 1 || firstDispatcher.dispatchCalls[0].TaskID != attempt.ModelInvocationID {
		t.Fatalf("first dispatch = %+v, checkpoint=%s", firstDispatcher.dispatchCalls, attempt.ModelInvocationID)
	}
	preUpgradeOrder, err := mailbox.LoadWorkOrder(attempt.ModelInvocationID)
	if err != nil {
		t.Fatal(err)
	}
	preUpgradePaths, _ := mailbox.Paths(preUpgradeOrder.ID)
	preUpgradeBytes, err := os.ReadFile(preUpgradePaths.Order)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"\"variant\"", "\"manifest_sha256\"", "\"verification_receipt\""} {
		if strings.Contains(string(preUpgradeBytes), forbidden) {
			t.Fatalf("pre-upgrade running order contains new evidence field %s: %s", forbidden, preUpgradeBytes)
		}
	}
	writeSpecialistOutput(t, mailbox, attempt.ModelInvocationID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered one exact proposal")

	secondDispatcher := &fakeSpecialistDispatcher{identity: identity, releaseErr: agent.ErrSpecialistTaskLeaseAbsent}
	providerConfig.Dispatcher = secondDispatcher
	providerConfig.Evidence = currentEvidence
	secondProvider, err := NewSpecialistProvider(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondWorker := &Worker{Config: workerConfig, Provider: secondProvider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Minute)
	result, err := secondWorker.Run(secondCtx, finding)
	cancelSecond()
	if err != nil {
		t.Fatal(err)
	}
	if result.PRNumber != 17 || pulls.calls != 1 || secondDispatcher.checkCalls != 0 || secondDispatcher.inspectCalls != 1 || len(secondDispatcher.dispatchCalls) != 0 {
		t.Fatalf("recovery result=%+v pulls=%d check/inspect/dispatch=%d/%d/%d", result, pulls.calls, secondDispatcher.checkCalls, secondDispatcher.inspectCalls, len(secondDispatcher.dispatchCalls))
	}
	attempt, _ = state.Get(fingerprint)
	if attempt.Stage != StagePROpen || !attempt.AttemptCounted || attempt.Attempt != 1 || attempt.ModelInvocationID != firstDispatcher.dispatchCalls[0].TaskID {
		t.Fatalf("completed recovery checkpoint = %+v", attempt)
	}
	order, err := mailbox.LoadWorkOrder(attempt.ModelInvocationID)
	if err != nil {
		t.Fatal(err)
	}
	lease, receipt, proposal, err := mailbox.LoadCompletion(order)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := mailbox.Paths(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := os.ReadDir(filepath.Dir(paths.OrderDirectory))
	if err != nil {
		t.Fatal(err)
	}
	remoteRepairRefs := func() []string {
		var refs []string
		for _, ref := range strings.Fields(gitOutput(t, remote, "for-each-ref", "--format=%(refname)")) {
			if strings.HasPrefix(ref, "refs/heads/hive/repair") {
				refs = append(refs, ref)
			}
		}
		return refs
	}
	repairRefs := remoteRepairRefs()
	if len(orders) != 1 || orders[0].Name() != order.ID || !orders[0].IsDir() {
		t.Fatalf("durable specialist work orders = %+v, want one exact order %s", orders, order.ID)
	}
	if order.RequestSHA256 == "" || lease.LeaseSHA256 == "" || receipt.ReceiptSHA256 == "" || receipt.UnifiedDiffSHA256 == "" {
		t.Fatalf("specialist evidence was not content-bound: order=%+v lease=%+v receipt=%+v", order, lease, receipt)
	}
	if lease.SessionID != identity.SessionID || receipt.SessionID != identity.SessionID || receipt.WorkOrderID != order.ID || receipt.LeaseSHA256 != lease.LeaseSHA256 {
		t.Fatalf("specialist session/receipt identity drifted: identity=%+v order=%+v lease=%+v receipt=%+v", identity, order, lease, receipt)
	}
	if string(proposal) != specialistProviderTestDiff {
		t.Fatalf("durable specialist proposal = %q, want exact proposed diff", proposal)
	}
	if len(repairRefs) != 1 || repairRefs[0] != "refs/heads/"+result.Branch {
		t.Fatalf("remote repair refs = %v, want only refs/heads/%s", repairRefs, result.Branch)
	}
	if lifecycle.prOpens != 1 || lifecycle.pr != result.PRNumber || lifecycle.sha != result.CommitSHA {
		t.Fatalf("lifecycle PR-open transitions = %d pr=%d sha=%s, want one transition for result %+v", lifecycle.prOpens, lifecycle.pr, lifecycle.sha, result)
	}

	orderID := order.ID
	requestSHA := order.RequestSHA256
	leaseSHA := lease.LeaseSHA256
	sessionID := lease.SessionID
	receiptSHA := receipt.ReceiptSHA256
	proposalSHA := receipt.UnifiedDiffSHA256
	proposalBytes := string(proposal)
	finding.Status = visualhive.StatusPROpen
	finding.RepairAttempts = 1
	finding.Branch = result.Branch
	finding.RepairCommitSHA = result.CommitSHA
	finding.PRNumber = result.PRNumber
	finding.PRURL = result.PRURL
	third, err := secondWorker.Run(context.Background(), finding)
	if err != nil || !third.Resumed || pulls.calls != 1 || lifecycle.prOpens != 1 || secondDispatcher.inspectCalls != 1 || len(secondDispatcher.dispatchCalls) != 0 {
		t.Fatalf("idempotent rerun = %+v, %v pulls=%d lifecycle_pr_opens=%d inspect=%d dispatch=%d", third, err, pulls.calls, lifecycle.prOpens, secondDispatcher.inspectCalls, len(secondDispatcher.dispatchCalls))
	}
	replayedAttempt, ok := state.Get(fingerprint)
	if !ok || replayedAttempt.Stage != StagePROpen || replayedAttempt.ModelInvocationID != orderID || replayedAttempt.Attempt != 1 || !replayedAttempt.AttemptCounted {
		t.Fatalf("idempotent rerun changed the repair checkpoint: %+v", replayedAttempt)
	}
	replayedOrder, err := mailbox.LoadWorkOrder(orderID)
	if err != nil {
		t.Fatal(err)
	}
	replayedLease, replayedReceipt, replayedProposal, err := mailbox.LoadCompletion(replayedOrder)
	if err != nil {
		t.Fatal(err)
	}
	replayedOrders, err := os.ReadDir(filepath.Dir(paths.OrderDirectory))
	if err != nil {
		t.Fatal(err)
	}
	replayedRefs := remoteRepairRefs()
	if len(replayedOrders) != 1 || replayedOrders[0].Name() != orderID || !replayedOrders[0].IsDir() {
		t.Fatalf("idempotent rerun changed specialist work-order cardinality: %+v", replayedOrders)
	}
	if replayedOrder.ID != orderID || replayedOrder.RequestSHA256 != requestSHA || replayedLease.LeaseSHA256 != leaseSHA || replayedLease.SessionID != sessionID || replayedReceipt.ReceiptSHA256 != receiptSHA || replayedReceipt.UnifiedDiffSHA256 != proposalSHA || string(replayedProposal) != proposalBytes {
		t.Fatalf("idempotent rerun changed durable specialist evidence: order=%+v lease=%+v receipt=%+v", replayedOrder, replayedLease, replayedReceipt)
	}
	if len(replayedRefs) != 1 || replayedRefs[0] != "refs/heads/"+result.Branch {
		t.Fatalf("idempotent rerun changed remote repair refs: %v", replayedRefs)
	}
}

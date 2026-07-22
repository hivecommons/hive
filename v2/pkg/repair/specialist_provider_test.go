package repair

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
)

const specialistProviderTestDiff = "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"

type managerBackedSpecialistExecutor struct {
	mu       sync.Mutex
	checks   int
	starts   int
	identity agent.SpecialistChildExecutorIdentity
}

func (executor *managerBackedSpecialistExecutor) Check(context.Context) (agent.SpecialistChildExecutorIdentity, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.checks++
	identity := executor.identity
	identity.AuthorizationID = fmt.Sprintf("codex-auth-%064x", executor.checks)
	return identity, nil
}

func (executor *managerBackedSpecialistExecutor) Start(_ context.Context, request agent.SpecialistChildExecutionRequest) (agent.SpecialistChildProcess, error) {
	executor.mu.Lock()
	executor.starts++
	pid := 9700 + executor.starts
	executor.mu.Unlock()
	order, lease, err := decodeManagerBackedSpecialistPrompt(request.Prompt)
	if err != nil {
		return nil, err
	}
	result := specialistModelResult{
		SchemaVersion: specialistModelResultSchema, WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: agent.SpecialistCompletionProposed, Summary: "Manager-owned governed proposal", UnifiedDiff: specialistProviderTestDiff,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &managerBackedSpecialistProcess{
		pid: pid, providerSHA256: request.ProviderSHA256,
		result: agent.SpecialistChildExecutionResult{Response: encoded, TurnID: request.AuthorizationID, CompletedAt: time.Now().UTC()},
	}, nil
}

func (executor *managerBackedSpecialistExecutor) counts() (int, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.checks, executor.starts
}

type managerBackedSpecialistProcess struct {
	pid            int
	providerSHA256 string
	result         agent.SpecialistChildExecutionResult
}

func (process *managerBackedSpecialistProcess) PID() int               { return process.pid }
func (process *managerBackedSpecialistProcess) ProviderSHA256() string { return process.providerSHA256 }
func (process *managerBackedSpecialistProcess) Wait() (agent.SpecialistChildExecutionResult, error) {
	return process.result, nil
}

func (process *managerBackedSpecialistProcess) ForceReap(context.Context) error {
	return nil
}

func decodeManagerBackedSpecialistPrompt(prompt string) (agent.SpecialistWorkOrder, agent.SpecialistLease, error) {
	const orderMarker = "CONTROLLER_BOUND_WORK_ORDER_JSON\n"
	const leaseMarker = "\nCONTROLLER_BOUND_LEASE_JSON\n"
	const finalMarker = "\n\nFINAL_CAPABILITY_WRAPPER_V1"
	orderStart := strings.LastIndex(prompt, orderMarker)
	if orderStart < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("missing controller-bound work order")
	}
	orderStart += len(orderMarker)
	leaseAt := strings.Index(prompt[orderStart:], leaseMarker)
	if leaseAt < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("missing controller-bound lease")
	}
	leaseStart := orderStart + leaseAt + len(leaseMarker)
	leaseEnd := strings.Index(prompt[leaseStart:], finalMarker)
	if leaseEnd < 0 {
		return agent.SpecialistWorkOrder{}, agent.SpecialistLease{}, errors.New("missing final capability wrapper")
	}
	var order agent.SpecialistWorkOrder
	if err := json.Unmarshal([]byte(prompt[orderStart:orderStart+leaseAt]), &order); err != nil {
		return order, agent.SpecialistLease{}, err
	}
	var lease agent.SpecialistLease
	if err := json.Unmarshal([]byte(prompt[leaseStart:leaseStart+leaseEnd]), &lease); err != nil {
		return order, lease, err
	}
	return order, lease, nil
}

type failFirstReleaseDispatcher struct {
	inner SpecialistDispatcher
	mu    sync.Mutex
	fail  bool
}

func (dispatcher *failFirstReleaseDispatcher) CheckSpecialistRole(ctx context.Context, role agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	return dispatcher.inner.CheckSpecialistRole(ctx, role)
}
func (dispatcher *failFirstReleaseDispatcher) InspectSpecialistRole(ctx context.Context, role agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	return dispatcher.inner.InspectSpecialistRole(ctx, role)
}
func (dispatcher *failFirstReleaseDispatcher) DispatchSpecialistTask(ctx context.Context, request agent.SpecialistDispatchRequest) (agent.SpecialistDispatchResult, error) {
	return dispatcher.inner.DispatchSpecialistTask(ctx, request)
}
func (dispatcher *failFirstReleaseDispatcher) ObserveSpecialistTaskResponse(ctx context.Context, request agent.SpecialistTaskResponseRequest) (agent.SpecialistTaskResponse, error) {
	return dispatcher.inner.ObserveSpecialistTaskResponse(ctx, request)
}
func (dispatcher *failFirstReleaseDispatcher) VerifySpecialistTaskCompletion(ctx context.Context, request agent.SpecialistTaskCompletionVerificationRequest) (agent.SpecialistTaskResponse, error) {
	return dispatcher.inner.VerifySpecialistTaskCompletion(ctx, request)
}
func (dispatcher *failFirstReleaseDispatcher) ReleaseSpecialistTask(role agent.SpecialistRole, taskID string) error {
	dispatcher.mu.Lock()
	if !dispatcher.fail {
		dispatcher.fail = true
		dispatcher.mu.Unlock()
		return errors.New("simulated crash after receipt persistence before child consumption")
	}
	dispatcher.mu.Unlock()
	return dispatcher.inner.ReleaseSpecialistTask(role, taskID)
}

type fakeSpecialistDispatcher struct {
	identity      agent.SpecialistSessionIdentity
	checkErr      error
	inspectErr    error
	dispatchErr   error
	observeErr    error
	verifyErr     error
	releaseErr    error
	dispatch      func(agent.SpecialistDispatchRequest)
	dispatchReply func(agent.SpecialistDispatchRequest) agent.SpecialistDispatchResult
	observeReply  func(agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse
	verifyReply   func(agent.SpecialistTaskCompletionVerificationRequest) agent.SpecialistTaskResponse
	checkCalls    int
	inspectCalls  int
	dispatchCalls []agent.SpecialistDispatchRequest
	observeCalls  []agent.SpecialistTaskResponseRequest
	verifyCalls   []agent.SpecialistTaskCompletionVerificationRequest
	releaseCalls  []agent.SpecialistDispatchRequest
	lastResponse  agent.SpecialistTaskResponse
}

func (f *fakeSpecialistDispatcher) CheckSpecialistRole(_ context.Context, _ agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	f.checkCalls++
	return f.identity, f.checkErr
}

func (f *fakeSpecialistDispatcher) InspectSpecialistRole(_ context.Context, _ agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	f.inspectCalls++
	return f.identity, f.inspectErr
}

func (f *fakeSpecialistDispatcher) DispatchSpecialistTask(_ context.Context, request agent.SpecialistDispatchRequest) (agent.SpecialistDispatchResult, error) {
	f.dispatchCalls = append(f.dispatchCalls, request)
	if f.dispatch != nil {
		f.dispatch(request)
	}
	if f.dispatchReply != nil {
		return f.dispatchReply(request), f.dispatchErr
	}
	return agent.SpecialistDispatchResult{SpecialistSessionIdentity: f.identity}, f.dispatchErr
}

func (f *fakeSpecialistDispatcher) ObserveSpecialistTaskResponse(_ context.Context, request agent.SpecialistTaskResponseRequest) (agent.SpecialistTaskResponse, error) {
	f.observeCalls = append(f.observeCalls, request)
	if f.observeReply != nil {
		response := f.observeReply(request)
		f.lastResponse = response
		return response, f.observeErr
	}
	if f.observeErr != nil {
		return agent.SpecialistTaskResponse{}, f.observeErr
	}
	return agent.SpecialistTaskResponse{}, agent.ErrSpecialistTaskResponsePending
}

func (f *fakeSpecialistDispatcher) VerifySpecialistTaskCompletion(_ context.Context, request agent.SpecialistTaskCompletionVerificationRequest) (agent.SpecialistTaskResponse, error) {
	f.verifyCalls = append(f.verifyCalls, request)
	if f.verifyErr != nil {
		return agent.SpecialistTaskResponse{}, f.verifyErr
	}
	if f.verifyReply != nil {
		return f.verifyReply(request), nil
	}
	if f.lastResponse.SchemaVersion == "" {
		return agent.SpecialistTaskResponse{}, errors.New("fake dispatcher has no Manager-owned child evidence")
	}
	return f.lastResponse, nil
}

func (f *fakeSpecialistDispatcher) ReleaseSpecialistTask(role agent.SpecialistRole, taskID string) error {
	f.releaseCalls = append(f.releaseCalls, agent.SpecialistDispatchRequest{TaskID: taskID, Specialist: role})
	return f.releaseErr
}

type specialistProviderFixture struct {
	provider   *SpecialistProvider
	dispatcher *fakeSpecialistDispatcher
	mailbox    *agent.SpecialistMailbox
	worktree   string
	clock      *time.Time
	baseSHA    string
	baseTree   string
}

func TestSpecialistProviderReleasesOnlyDefinitelyUndeliveredLease(t *testing.T) {
	t.Run("typed non-delivery permits immediate exact retry", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		prompt := "bounded retry prompt"
		fixture.dispatcher.dispatchErr = fmt.Errorf("%w: kick failed before delivery", agent.ErrSpecialistTaskNotDelivered)
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || runErr.Launched {
			t.Fatalf("definite non-delivery result = %#v, %v", runErr, err)
		}
		pending, listErr := fixture.mailbox.ListPending()
		if listErr != nil || len(pending) != 1 {
			t.Fatalf("pending order after non-delivery = %+v, %v", pending, listErr)
		}
		if _, leaseErr := fixture.mailbox.LoadLease(pending[0]); !errors.Is(leaseErr, os.ErrNotExist) {
			t.Fatalf("definitely undelivered lease was retained: %v", leaseErr)
		}
		fixture.dispatcher.dispatchErr = nil
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			writeSpecialistOutput(t, fixture.mailbox, request.TaskID, agent.SpecialistCompletionBlocked, nil, "bounded retry completed")
		}
		_, err = fixture.provider.Run(context.Background(), fixture.worktree, prompt)
		if err == nil || !strings.Contains(err.Error(), "bounded retry completed") {
			t.Fatalf("exact retry did not consume the same blocked order: %v", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 2 || fixture.dispatcher.dispatchCalls[0].TaskID != fixture.dispatcher.dispatchCalls[1].TaskID {
			t.Fatalf("retry dispatch identities = %+v", fixture.dispatcher.dispatchCalls)
		}
	})

	t.Run("unclassified error preserves ambiguous lease", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatchErr = errors.New("third-party dispatcher returned an unknown error")
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "ambiguous dispatch prompt")
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || !runErr.Launched {
			t.Fatalf("unclassified dispatch result = %#v, %v", runErr, err)
		}
		pending, listErr := fixture.mailbox.ListPending()
		if listErr != nil || len(pending) != 1 {
			t.Fatalf("ambiguous pending order = %+v, %v", pending, listErr)
		}
		if _, leaseErr := fixture.mailbox.LoadLease(pending[0]); leaseErr != nil {
			t.Fatalf("ambiguous dispatch lease was removed: %v", leaseErr)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 {
			t.Fatalf("ambiguous dispatcher call count = %d", len(fixture.dispatcher.dispatchCalls))
		}
	})
}

func TestReleaseDispatchedSpecialistFinalizesVerifiedReusedResult(t *testing.T) {
	dispatcher := &fakeSpecialistDispatcher{}
	order := agent.SpecialistWorkOrder{ID: "swo-" + strings.Repeat("a", 64), SpecialistWorkOrderRequest: agent.SpecialistWorkOrderRequest{
		Kind: agent.SpecialistWorkOrderKindGovernedVisualHiveProposal, Specialist: agent.SpecialistQuality,
	}}
	if err := releaseDispatchedSpecialist(dispatcher, order, true, false); err != nil || len(dispatcher.releaseCalls) != 0 {
		t.Fatalf("unverified reused result authorized release: calls=%d err=%v", len(dispatcher.releaseCalls), err)
	}
	if err := releaseDispatchedSpecialist(dispatcher, order, true, true); err != nil || len(dispatcher.releaseCalls) != 1 {
		t.Fatalf("verified reused result did not finalize: calls=%d err=%v", len(dispatcher.releaseCalls), err)
	}
	dispatcher.releaseErr = agent.ErrSpecialistTaskLeaseAbsent
	if err := releaseDispatchedSpecialist(dispatcher, order, true, true); err != nil {
		t.Fatalf("idempotent verified reuse rejected already-consumed task: %v", err)
	}
}

func TestSpecialistProviderRecoversExactPreparedAndLeasedOrdersWithoutDuplicates(t *testing.T) {
	t.Run("crash after prepare before lease dispatches exact order once", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "prepared crash window")
		if err != nil {
			t.Fatal(err)
		}
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			writeSpecialistOutput(t, fixture.mailbox, request.TaskID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered prepared order")
		}
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, modelPatchBegin) {
			t.Fatalf("prepared recovery = %#v, %v", result, err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.dispatchCalls[0].TaskID != order.ID {
			t.Fatalf("prepared recovery dispatches = %+v", fixture.dispatcher.dispatchCalls)
		}
	})

	t.Run("live lease is observed and brokered without check or dispatch", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "leased crash window")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
			t.Fatal(err)
		}
		writeSpecialistOutput(t, fixture.mailbox, order.ID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "broker recovered completion")
		fixture.dispatcher.releaseErr = agent.ErrSpecialistTaskLeaseAbsent
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, modelPatchBegin) {
			t.Fatalf("leased recovery = %#v, %v", result, err)
		}
		if fixture.dispatcher.checkCalls != 0 || fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("leased recovery check/inspect/dispatch = %d/%d/%d", fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("expired ambiguous lease never redispatches", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "expired lease")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
			t.Fatal(err)
		}
		*fixture.clock = order.Deadline.Add(time.Second)
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || !runErr.Launched || !strings.Contains(err.Error(), "expired ambiguous lease") {
			t.Fatalf("expired lease result = %#v, %v", runErr, err)
		}
		if fixture.dispatcher.checkCalls != 0 || fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("expired lease reached a specialist: %d/%d/%d", fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("expired lease recovers an in-deadline Codex completion", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "expired completed lease")
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline)
		if err != nil {
			t.Fatal(err)
		}
		fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
			return structuredSpecialistTaskResponseAt(t, fixture, request, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered completed response", lease.Deadline.Add(-time.Second))
		}
		fixture.dispatcher.releaseErr = agent.ErrSpecialistTaskLeaseAbsent
		*fixture.clock = lease.Deadline.Add(time.Second)
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, "+fixed") {
			t.Fatalf("expired completion recovery = %#v, %v", result, err)
		}
		if fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 || len(fixture.dispatcher.observeCalls) != 1 {
			t.Fatalf("expired completion inspect/dispatch/observe = %d/%d/%d", fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls))
		}
	})
}

func TestSpecialistProviderProposalIsBrokeredAndCompletedReplayDoesNotRedispatch(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	diff := []byte("diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n")
	fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
		return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionProposed, diff, "fixed the exact value")
	}

	if err := fixture.provider.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	prompt := "  exact bounded task prompt\n"
	result, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Summary != "fixed the exact value" {
		t.Fatalf("Run() summary = %q", result.Summary)
	}
	wantOutput := modelPatchBegin + "\n" + strings.TrimSuffix(string(diff), "\n") + "\n" + modelPatchEnd
	if result.Output != wantOutput {
		t.Fatalf("Run() output = %q, want %q", result.Output, wantOutput)
	}
	if len(fixture.dispatcher.dispatchCalls) != 1 || len(fixture.dispatcher.observeCalls) != 1 || len(fixture.dispatcher.releaseCalls) != 1 {
		t.Fatalf("dispatch/observe/release calls = %d/%d/%d, want 1/1/1", len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls), len(fixture.dispatcher.releaseCalls))
	}
	request := fixture.dispatcher.dispatchCalls[0]
	if request.Specialist != agent.SpecialistQuality || request.TaskID == "" {
		t.Fatalf("dispatch request = %#v", request)
	}
	for _, required := range []string{"Do not call tools", "Do not read or write files", "Hive alone owns", "CANONICAL_WORK_ORDER_JSON", "CANONICAL_LEASE_JSON", "hive.specialist-model-result.v1", `"task_prompt":"  exact bounded task prompt\n"`, "HIVE_SPECIALIST_DISPATCH_READY_"} {
		if !strings.Contains(request.Message, required) {
			t.Fatalf("dispatch message is missing %q: %s", required, request.Message)
		}
	}
	if marker := agent.SpecialistDispatchReadyMarker(request.TaskID); marker == "" || !strings.HasSuffix(strings.TrimSpace(request.Message), marker) {
		t.Fatalf("dispatch message does not end in its task-specific readiness marker: %q", request.Message)
	}
	for _, forbidden := range []string{"completion.status", "proposal.diff", "summary.txt", "receipt.json", "hive specialist complete"} {
		if strings.Contains(request.Message, forbidden) {
			t.Fatalf("dispatch message contains forbidden filesystem/helper instruction %q: %s", forbidden, request.Message)
		}
	}
	order, err := fixture.mailbox.LoadWorkOrder(request.TaskID)
	if err != nil {
		t.Fatalf("LoadWorkOrder() error = %v", err)
	}
	if order.Repository != "daviddiaz0317/proof" || order.RepositoryFingerprint != strings.Repeat("a", 64) || order.RecurrenceKey != "finding:visual-1" || order.Attempt != 2 {
		t.Fatalf("work order lifecycle identity = %#v", order.SpecialistWorkOrderRequest)
	}
	if order.BaseSHA != fixture.baseSHA || order.BaseTreeSHA != fixture.baseTree || order.TaskPrompt != prompt {
		t.Fatalf("work order checkout/prompt identity = base %s tree %s prompt %q", order.BaseSHA, order.BaseTreeSHA, order.TaskPrompt)
	}
	if order.Evidence.WorkflowRunID != 41 || order.Evidence.ArtifactID != 73 || order.RouteReason != "test adequacy routes to quality" {
		t.Fatalf("work order evidence/route identity = %#v", order.SpecialistWorkOrderRequest)
	}
	lease, receipt, storedDiff, err := fixture.mailbox.LoadCompletion(order)
	if err != nil {
		t.Fatalf("LoadCompletion() error = %v", err)
	}
	if lease.SessionID != fixture.dispatcher.identity.SessionID || receipt.Status != agent.SpecialistCompletionProposed || !strings.Contains(string(storedDiff), "+fixed") {
		t.Fatalf("completion = lease %#v receipt %#v diff %q", lease, receipt, storedDiff)
	}

	// A completed content-identical order is consumed without even checking the
	// manager. This proves a crash/retry cannot duplicate specialist work.
	fixture.dispatcher.checkErr = errors.New("manager deliberately unavailable during replay")
	replayed, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatalf("replayed Run() error = %v", err)
	}
	if replayed.Output != result.Output || len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.checkCalls != 2 {
		t.Fatalf("replay output/calls = %q dispatch=%d checks=%d", replayed.Output, len(fixture.dispatcher.dispatchCalls), fixture.dispatcher.checkCalls)
	}
}

func TestGovernedSchedulerCompositionReservesOnceAndLeasedRecoveryDoesNotRecompose(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	configureGovernedSpecialistFixture(&fixture)
	builderCalls, reservationCalls, launchGuardCalls := 0, 0, 0
	var reserved agent.SpecialistWorkOrder
	config := fixture.provider.config
	// Keep the controller binding deliberately non-canonical. The mailbox sorts
	// the same path set before hashing and persistence; replay must compare that
	// canonical identity without creating or reserving a second work order.
	config.AllowedPaths = []string{"tests/**", "src/**"}
	config.GovernedBuilder = func(_ context.Context, worktree, baseSHA, baseTreeSHA, prompt string, readiness agent.SpecialistSessionIdentity) (agent.SpecialistWorkOrderRequest, error) {
		builderCalls++
		if worktree != fixture.worktree || baseSHA != fixture.baseSHA || baseTreeSHA != fixture.baseTree ||
			readiness.SessionID != fixture.dispatcher.identity.SessionID || readiness.ExecutorAuthorizationID != fixture.dispatcher.identity.ExecutorAuthorizationID {
			return agent.SpecialistWorkOrderRequest{}, errors.New("builder did not receive the exact Worker/readiness bindings")
		}
		request := fixture.provider.workOrderRequest(baseSHA, baseTreeSHA, prompt)
		request.AllowedPaths = append([]string(nil), config.AllowedPaths...)
		request.ExternalRef = "visual-hive://owner/repo/" + strings.Repeat("a", 64)
		request.PacketSHA256 = strings.Repeat("1", 64)
		request.FindingSHA256 = strings.Repeat("2", 64)
		request.SourceContextSHA256 = strings.Repeat("3", 64)
		request.SourceContextBindingSHA256 = strings.Repeat("4", 64)
		request.AuthorityClass = "sealed-source-context-proposal-v1"
		request.PolicySHA256 = strings.Repeat("5", 64)
		request.KnowledgeSHA256 = strings.Repeat("6", 64)
		request.ToolPolicySHA256 = strings.Repeat("7", 64)
		request.CapabilitySHA256 = strings.Repeat("8", 64)
		request.RoleConfigSnapshot = &agent.SpecialistRoleConfigSnapshot{
			SchemaVersion: agent.SpecialistRoleConfigSnapshotSchema, Backend: "copilot", Model: "persistent-role-model",
			ToolRulesSHA256: strings.Repeat("9", 64), ConnectionsSHA256: strings.Repeat("a", 64), FullConfigSHA256: request.ToolPolicySHA256,
		}
		roleSnapshotJSON, err := json.Marshal(*request.RoleConfigSnapshot)
		if err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		roleSnapshotDigest := sha256.Sum256(roleSnapshotJSON)
		request.RoleConfigSnapshotSHA256 = hex.EncodeToString(roleSnapshotDigest[:])
		request.ReproductionMode = agent.SpecialistReproductionModeNone
		request.AffectedContracts = []string{"contract/value"}
		request.KnowledgeKeywords = []string{"test_adequacy", "quality", "contract-value"}
		return request, nil
	}
	config.ReserveWorkOrder = func(order agent.SpecialistWorkOrder) error {
		reservationCalls++
		reserved = order
		return nil
	}
	config.LaunchGuard = func(agent.SpecialistWorkOrder) error {
		launchGuardCalls++
		return errors.New("current runtime pause blocks a fresh launch")
	}
	provider, err := NewSpecialistProvider(config)
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider = provider
	if err := fixture.provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "normal Scheduler governed prompt")
	if err != nil {
		t.Fatal(err)
	}
	if builderCalls != 1 || reservationCalls != 1 || reserved.ID != order.ID || reserved.RequestSHA256 != order.RequestSHA256 || order.ID != "swo-"+order.RequestSHA256 {
		t.Fatalf("composition/reservation = builder %d reserve %d reserved=%+v order=%+v", builderCalls, reservationCalls, reserved, order)
	}
	if got := strings.Join(order.AllowedPaths, ","); got != "src/**,tests/**" {
		t.Fatalf("persisted governed paths = %q, want canonical mailbox order", got)
	}
	drifted := order.SpecialistWorkOrderRequest
	drifted.AllowedPaths = append(append([]string(nil), order.AllowedPaths...), "README.md")
	if err := fixture.provider.validateBuiltGovernedRequest(drifted, fixture.baseSHA, fixture.baseTree); err == nil {
		t.Fatal("governed replay accepted an expanded repair scope")
	}
	_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
	var runErr *ProviderRunError
	if err == nil || !errors.As(err, &runErr) || runErr.Launched || !strings.Contains(err.Error(), "current runtime pause") {
		t.Fatalf("fresh guarded launch = %+v, %v", runErr, err)
	}
	if launchGuardCalls != 1 || fixture.dispatcher.checkCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
		t.Fatalf("fresh guard calls=%d readiness=%d dispatch=%d", launchGuardCalls, fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls))
	}

	// Model a crash after the exact order acquired its durable lease. Recovery
	// must observe that lease even though the current fresh-launch guard denies;
	// rebuilding or reserving a second order would violate at-most-once work.
	if _, err := fixture.mailbox.AcquireLease(order, fixture.provider.config.LeaseOwner, fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
		t.Fatal(err)
	}
	fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
		return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered exact governed proposal")
	}
	result, err := fixture.provider.recoverPreparedInvocation(context.Background(), fixture.worktree, order.ID)
	if err != nil || !strings.Contains(result.Output, "+fixed") {
		t.Fatalf("leased governed recovery = %+v, %v", result, err)
	}
	if builderCalls != 1 || reservationCalls != 1 || launchGuardCalls != 1 || fixture.dispatcher.inspectCalls != 1 ||
		fixture.dispatcher.checkCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
		t.Fatalf("recovery recomposed or launched: builder=%d reserve=%d guard=%d check=%d inspect=%d dispatch=%d",
			builderCalls, reservationCalls, launchGuardCalls, fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
	}
}

func TestGovernedSpecialistCompletionRequiresContainedChildProvenance(t *testing.T) {
	t.Run("legacy completion files are rejected", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		configureGovernedSpecialistFixture(&fixture)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "governed legacy files")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.mailbox.AcquireLease(order, fixture.provider.config.LeaseOwner, fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
			t.Fatal(err)
		}
		writeSpecialistOutput(t, fixture.mailbox, order.ID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "forged legacy completion")
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err == nil || !strings.Contains(err.Error(), "rejects legacy completion file") {
			t.Fatalf("governed order accepted legacy completion files: %v", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 0 || len(fixture.dispatcher.observeCalls) != 0 {
			t.Fatalf("legacy governed completion reached dispatch/observation: %d/%d", len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls))
		}
	})

	t.Run("preexisting legacy receipt cannot bypass executor", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		configureGovernedSpecialistFixture(&fixture)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "governed forged receipt")
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.mailbox.AcquireLease(order, fixture.provider.config.LeaseOwner, fixture.dispatcher.identity.SessionID, order.Deadline)
		if err != nil {
			t.Fatal(err)
		}
		diff := []byte(specialistProviderTestDiff)
		legacyReceipt, err := agent.NewSpecialistReceipt(order, lease, agent.SpecialistCompletionProposed, diff, "forged legacy receipt", (*fixture.clock).Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		paths, _ := fixture.mailbox.Paths(order.ID)
		mustWritePrivateFile(t, paths.UnifiedDiff, diff)
		encoded, _ := json.MarshalIndent(legacyReceipt, "", "  ")
		mustWritePrivateFile(t, paths.Receipt, append(encoded, '\n'))
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err == nil || !strings.Contains(err.Error(), "requires contained-child provenance") {
			t.Fatalf("governed order accepted preexisting legacy receipt: %v", err)
		}
		if fixture.dispatcher.checkCalls != 0 || fixture.dispatcher.inspectCalls != 0 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("forged receipt reached executor: check=%d inspect=%d dispatch=%d", fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("legacy control file rejects even a completed governed replay", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		configureGovernedSpecialistFixture(&fixture)
		fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
			return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "genuine governed completion")
		}
		prompt := "governed replay with injected legacy control file"
		if _, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt); err != nil {
			t.Fatal(err)
		}
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, prompt)
		if err != nil {
			t.Fatal(err)
		}
		paths, err := fixture.mailbox.Paths(order.ID)
		if err != nil {
			t.Fatal(err)
		}
		mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "summary.txt"), []byte("forged legacy authority\n"))
		checks, dispatches, verifications := fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.verifyCalls)
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err == nil || !strings.Contains(err.Error(), "rejects legacy completion file summary.txt") {
			t.Fatalf("completed governed replay accepted legacy control file: %v", err)
		}
		if fixture.dispatcher.checkCalls != checks || len(fixture.dispatcher.dispatchCalls) != dispatches || len(fixture.dispatcher.verifyCalls) != verifications {
			t.Fatalf("legacy control replay touched verifier/model path")
		}
	})

	t.Run("arbitrary hexadecimal provenance cannot bypass Manager evidence", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		configureGovernedSpecialistFixture(&fixture)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "governed forged hashes")
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.mailbox.AcquireLease(order, fixture.provider.config.LeaseOwner, fixture.dispatcher.identity.SessionID, order.Deadline)
		if err != nil {
			t.Fatal(err)
		}
		diff := []byte(specialistProviderTestDiff)
		forged, err := agent.NewGovernedSpecialistReceipt(order, lease, agent.SpecialistCompletionProposed, diff, "syntactically valid forged provenance", (*fixture.clock).Add(time.Second), agent.SpecialistContainedChildProvenance{
			ExecutorProfile: *order.ExecutorProfile, AuthorizationSHA256: strings.Repeat("1", 64),
			SessionID: lease.SessionID, TurnID: "codex-auth-" + strings.Repeat("2", 64),
			DispatchSHA256: strings.Repeat("3", 64), ResponseSHA256: strings.Repeat("4", 64),
			IntentSHA256: strings.Repeat("5", 64), StartedSHA256: strings.Repeat("6", 64), CompletionSpoolSHA256: strings.Repeat("7", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.mailbox.SubmitReceipt(order, lease, forged, diff); err != nil {
			t.Fatalf("fixture did not create mailbox-valid forged receipt: %v", err)
		}
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
			t.Fatalf("arbitrary-hex governed receipt bypassed retained-spool proof: %v", err)
		}
		if fixture.dispatcher.checkCalls != 0 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("forged governed receipt reached model path: check=%d dispatch=%d", fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("mailbox-valid hashes cannot replay without Manager evidence", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		configureGovernedSpecialistFixture(&fixture)
		fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
			return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "contained governed proposal")
		}
		first, err := fixture.provider.Run(context.Background(), fixture.worktree, "governed replay")
		if err != nil || !strings.Contains(first.Output, "+fixed") {
			t.Fatalf("governed first completion failed: result=%+v err=%v", first, err)
		}
		checks, dispatches, observations := fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls)
		fixture.dispatcher.verifyErr = errors.New("no retained Manager-owned spool")
		_, err = fixture.provider.Run(context.Background(), fixture.worktree, "governed replay")
		if err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
			t.Fatalf("syntactically valid mailbox receipt bypassed the authority verifier: %v", err)
		}
		if fixture.dispatcher.checkCalls != checks || len(fixture.dispatcher.dispatchCalls) != dispatches || len(fixture.dispatcher.observeCalls) != observations {
			t.Fatalf("failed replay verifier touched model path: check=%d/%d dispatch=%d/%d observe=%d/%d", fixture.dispatcher.checkCalls, checks, len(fixture.dispatcher.dispatchCalls), dispatches, len(fixture.dispatcher.observeCalls), observations)
		}
	})
}

func TestGovernedProviderRealManagerFinalizesReceiptBeforeConsumeCrash(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	executor := newManagerBackedSpecialistExecutor()
	state := filepath.Join(t.TempDir(), "repair-state")
	manager := newManagerBackedSpecialistManager(t, state, executor)
	configureManagerBackedSpecialistProvider(&fixture, manager, executor.identity)
	fixture.provider.config.Dispatcher = &failFirstReleaseDispatcher{inner: manager}
	prompt := "real Manager crash after durable governed receipt"

	_, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err == nil || !strings.Contains(err.Error(), "simulated crash after receipt persistence") {
		t.Fatalf("release crash window was not injected after receipt persistence: %v", err)
	}
	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.mailbox.LoadCompletion(order); err != nil {
		t.Fatalf("governed receipt was not durable before release crash: %v", err)
	}
	childRoot := filepath.Join(state, "specialist-children", order.ID)
	if _, err := os.Stat(filepath.Join(childRoot, "complete.json")); err != nil {
		t.Fatalf("Manager completion spool is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(childRoot, "consumed.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release crash unexpectedly consumed the child: %v", err)
	}
	if _, starts := executor.counts(); starts != 1 {
		t.Fatalf("initial governed request launched %d models", starts)
	}

	recoveryExecutor := newManagerBackedSpecialistExecutor()
	restarted := newManagerBackedSpecialistManager(t, state, recoveryExecutor)
	fixture.provider.config.Dispatcher = restarted
	replayed, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err != nil || !strings.Contains(replayed.Output, "+fixed") {
		t.Fatalf("receipt-before-release replay failed: result=%+v err=%v", replayed, err)
	}
	if checks, starts := recoveryExecutor.counts(); checks != 0 || starts != 0 {
		t.Fatalf("verified receipt replay touched model path: checks=%d starts=%d", checks, starts)
	}
	if _, err := os.Stat(filepath.Join(childRoot, "consumed.json")); err != nil {
		t.Fatalf("replay did not persist exact consumed marker: %v", err)
	}
	if _, err := restarted.CheckSpecialistRole(context.Background(), agent.SpecialistQuality); err != nil {
		t.Fatalf("consumed replay left the role WIP: %v", err)
	}
	if checks, starts := recoveryExecutor.counts(); checks != 1 || starts != 0 {
		t.Fatalf("next task readiness counts = checks %d starts %d", checks, starts)
	}
}

func TestGovernedProviderRealManagerRecoversOnlyExactPartialDiff(t *testing.T) {
	for _, test := range []struct {
		name      string
		diff      []byte
		wantError string
	}{
		{name: "exact", diff: []byte(specialistProviderTestDiff)},
		{name: "mismatch", diff: []byte(strings.Replace(specialistProviderTestDiff, "+fixed", "+forged", 1)), wantError: "different content"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSpecialistProviderFixture(t)
			executor := newManagerBackedSpecialistExecutor()
			state := filepath.Join(t.TempDir(), "repair-state")
			manager := newManagerBackedSpecialistManager(t, state, executor)
			configureManagerBackedSpecialistProvider(&fixture, manager, executor.identity)
			order, _, response := dispatchManagerBackedSpecialistChild(t, fixture, manager, "governed partial diff "+test.name)
			if response.CompletionSpoolSHA256 == "" {
				t.Fatal("real Manager did not produce content-bound completion spool")
			}
			paths, err := fixture.mailbox.Paths(order.ID)
			if err != nil {
				t.Fatal(err)
			}
			mustWritePrivateFile(t, paths.UnifiedDiff, test.diff)
			if _, err := os.Stat(paths.Receipt); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial-write setup unexpectedly has a receipt: %v", err)
			}

			recoveryExecutor := newManagerBackedSpecialistExecutor()
			restarted := newManagerBackedSpecialistManager(t, state, recoveryExecutor)
			fixture.provider.config.Dispatcher = restarted
			result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
			if test.wantError == "" {
				if err != nil || !strings.Contains(result.Output, "+fixed") {
					t.Fatalf("exact partial diff was not reconstructed: result=%+v err=%v", result, err)
				}
				if _, _, storedDiff, err := fixture.mailbox.LoadCompletion(order); err != nil || string(storedDiff) != specialistProviderTestDiff {
					t.Fatalf("reconstructed receipt/diff mismatch: diff=%q err=%v", storedDiff, err)
				}
				if _, err := os.Stat(filepath.Join(state, "specialist-children", order.ID, "consumed.json")); err != nil {
					t.Fatalf("exact partial recovery did not consume child: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("mismatched partial diff was accepted: result=%+v err=%v", result, err)
				}
				if _, statErr := os.Stat(paths.Receipt); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("mismatch persisted an authoritative receipt: %v", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(state, "specialist-children", order.ID, "consumed.json")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("mismatch consumed retained child evidence: %v", statErr)
				}
			}
			if checks, starts := recoveryExecutor.counts(); checks != 0 || starts != 0 {
				t.Fatalf("partial-diff recovery touched model path: checks=%d starts=%d", checks, starts)
			}
		})
	}
}

func TestSpecialistProviderBlockedCompletionReturnsNoPatch(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
		return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionBlocked, nil, "verified context is insufficient")
	}

	result, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
	if err == nil {
		t.Fatal("Run() error = nil, want blocked result")
	}
	var runErr *ProviderRunError
	var blocked *SpecialistBlockedError
	if !errors.As(err, &runErr) || !runErr.Launched || !errors.As(err, &blocked) {
		t.Fatalf("Run() error = %T %v, want launched SpecialistBlockedError", err, err)
	}
	if result.Output != "" || result.Summary != "verified context is insufficient" {
		t.Fatalf("blocked result = %#v", result)
	}
	if len(fixture.dispatcher.releaseCalls) != 1 {
		t.Fatalf("release calls = %d, want 1", len(fixture.dispatcher.releaseCalls))
	}
}

func TestSpecialistModelResultIsStrictAndContentBound(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "strict model result")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	valid := specialistModelResult{
		SchemaVersion: specialistModelResultSchema, WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: agent.SpecialistCompletionProposed, Summary: "bounded proposal", UnifiedDiff: specialistProviderTestDiff,
	}
	encoded, _ := json.Marshal(valid)
	if _, diff, err := parseSpecialistModelResult(string(encoded), order, lease); err != nil || !strings.Contains(string(diff), "+fixed") {
		t.Fatalf("valid result = %q, %v", diff, err)
	}

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "surrounding prose", value: "result: " + string(encoded), want: "decode strict"},
		{name: "second document", value: string(encoded) + " {}", want: "exactly one"},
		{name: "unknown field", value: strings.TrimSuffix(string(encoded), "}") + `,"authority":"merge"}`, want: "unknown field"},
		{name: "wrong request", value: strings.Replace(string(encoded), order.RequestSHA256, strings.Repeat("f", 64), 1), want: "identity"},
		{name: "blocked with diff", value: strings.Replace(string(encoded), `"status":"proposed"`, `"status":"blocked"`, 1), want: "empty unified_diff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseSpecialistModelResult(test.value, order, lease); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSpecialistProviderFailsClosedOnStaleCheckoutAndWrongRoleOrSession(t *testing.T) {
	t.Run("stale checkout", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.provider.config.ExpectedBaseSHA = strings.Repeat("f", 40)
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "checkout is stale") {
			t.Fatalf("Run() error = %v, want stale checkout", err)
		}
		if fixture.dispatcher.checkCalls != 0 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("stale checkout reached specialist: checks=%d dispatch=%d", fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("wrong role", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.identity.Specialist = agent.SpecialistScanner
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "role mismatch") {
			t.Fatalf("Run() error = %v, want role mismatch", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("wrong role dispatched %d tasks", len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("wrong dispatch session", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatchReply = func(agent.SpecialistDispatchRequest) agent.SpecialistDispatchResult {
			identity := fixture.dispatcher.identity
			identity.SessionID = "hive-quality-restarted"
			return agent.SpecialistDispatchResult{SpecialistSessionIdentity: identity}
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "does not match the durable work order lease") {
			t.Fatalf("Run() error = %v, want session mismatch", err)
		}
		var runErr *ProviderRunError
		if !errors.As(err, &runErr) || !runErr.Launched || len(fixture.dispatcher.releaseCalls) != 1 {
			t.Fatalf("session mismatch error/release = %#v/%d", runErr, len(fixture.dispatcher.releaseCalls))
		}
	})
}

func TestSpecialistProviderRejectsGovernedExecutorProfileMismatchBeforeLeaseOrLaunch(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	fixture.provider.config.WorkOrderKind = agent.SpecialistWorkOrderKindGovernedVisualHiveProposal
	fixture.provider.config.ExecutorProfile = &agent.SpecialistProposalExecutorProfile{
		Backend: "codex", ProviderSHA256: strings.Repeat("f", 64), Model: "codex-proof-model",
		ConfigurationSHA256: strings.Repeat("a", 64), ContainmentProfile: agent.SpecialistContainmentProfileV1,
		BackendParityClaimed: false,
	}
	fixture.dispatcher.identity.Backend = "copilot"
	fixture.dispatcher.identity.ConfiguredBackend = "copilot"
	fixture.dispatcher.identity.ExecutorBackend = "codex"
	fixture.dispatcher.identity.ExecutorModel = "codex-proof-model"
	fixture.dispatcher.identity.ExecutorConfigSHA256 = strings.Repeat("a", 64)
	fixture.dispatcher.identity.ExecutorAuthorizationID = "codex-auth-" + strings.Repeat("b", 64)
	fixture.dispatcher.identity.ContainmentProfile = agent.SpecialistContainmentProfileV1
	fixture.dispatcher.identity.BackendParityClaimed = false
	fixture.dispatcher.identity.TmuxSession = ""
	fixture.dispatcher.identity.TmuxSocket = ""

	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "governed executor mismatch")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
	var runErr *ProviderRunError
	if err == nil || !errors.As(err, &runErr) || runErr.Launched || !strings.Contains(err.Error(), "exact executor profile") {
		t.Fatalf("profile mismatch was not rejected before launch: %#v %v", runErr, err)
	}
	if fixture.dispatcher.checkCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
		t.Fatalf("profile mismatch reached dispatch: checks=%d dispatches=%d", fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls))
	}
	if _, leaseErr := fixture.mailbox.LoadLease(order); !errors.Is(leaseErr, os.ErrNotExist) {
		t.Fatalf("profile mismatch acquired a mailbox lease: %v", leaseErr)
	}
}

func TestSpecialistProviderRejectsTamperedLeaseAndOutput(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			paths, err := fixture.mailbox.Paths(request.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := os.ReadFile(paths.Lease)
			if err != nil {
				t.Fatal(err)
			}
			var lease map[string]any
			if err := json.Unmarshal(encoded, &lease); err != nil {
				t.Fatal(err)
			}
			lease["session_id"] = "tampered-session"
			encoded, _ = json.MarshalIndent(lease, "", "  ")
			if err := os.WriteFile(paths.Lease, append(encoded, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || (!strings.Contains(err.Error(), "digest mismatch") && !strings.Contains(err.Error(), "stale or mismatched")) {
			t.Fatalf("Run() error = %v, want tampered lease rejection", err)
		}
		if len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("tampered live lease was released: %d", len(fixture.dispatcher.releaseCalls))
		}
	})

	t.Run("completion status", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			paths, _ := fixture.mailbox.Paths(request.TaskID)
			mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "summary.txt"), []byte("attempted tamper"))
			mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "completion.status"), []byte("resolved\n"))
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "exactly proposed or blocked") {
			t.Fatalf("Run() error = %v, want invalid status rejection", err)
		}
	})
}

func TestSpecialistProviderTimeoutCancelAndPendingReplayDoNotDuplicateDispatch(t *testing.T) {
	t.Run("timeout and pending replay", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		// Leave enough time for Windows Git process startup; the timeout is meant
		// to occur only after the specialist task has been dispatched.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := fixture.provider.Run(ctx, fixture.worktree, "bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("Run() error = %v, want durable pending work", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("first run dispatch/release = %d/%d", len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.releaseCalls))
		}
		replayCtx, replayCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer replayCancel()
		_, err = fixture.provider.Run(replayCtx, fixture.worktree, "bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("pending replay error = %v", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.inspectCalls != 1 {
			t.Fatalf("pending replay dispatch/inspect = %d/%d", len(fixture.dispatcher.dispatchCalls), fixture.dispatcher.inspectCalls)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		fixture.dispatcher.dispatch = func(agent.SpecialistDispatchRequest) { cancel() }
		_, err := fixture.provider.Run(ctx, fixture.worktree, "different bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("Run() error = %v, want durable pending work", err)
		}
		if len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("canceled live task was released: %d", len(fixture.dispatcher.releaseCalls))
		}
	})
}

func TestSpecialistProviderFailsClosedWhenMailboxIsOutsideRoleWorkDir(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	fixture.dispatcher.identity.WorkDir = t.TempDir()
	_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
	if err == nil || !strings.Contains(err.Error(), "mailbox must be contained") {
		t.Fatalf("Run() error = %v, want mailbox containment rejection", err)
	}
	if len(fixture.dispatcher.dispatchCalls) != 0 {
		t.Fatalf("outside mailbox dispatched %d tasks", len(fixture.dispatcher.dispatchCalls))
	}
}

func newSpecialistProviderFixture(t *testing.T) specialistProviderFixture {
	t.Helper()
	worktree, baseSHA, baseTree := makeSpecialistTestRepository(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := &now
	roleWorkDir := t.TempDir()
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(roleWorkDir, ".hive-specialist-mailbox"), agent.SpecialistMailboxOptions{Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatalf("NewSpecialistMailbox() error = %v", err)
	}
	dispatcher := &fakeSpecialistDispatcher{identity: agent.SpecialistSessionIdentity{
		Specialist:     agent.SpecialistQuality,
		AgentName:      "quality",
		AgentID:        "quality",
		Backend:        "codex",
		SessionID:      "hive-quality-test",
		TmuxSession:    "hive-quality-test",
		WorkDir:        roleWorkDir,
		ProviderSHA256: strings.Repeat("e", 64),
	}}
	provider, err := NewSpecialistProvider(SpecialistProviderConfig{
		Dispatcher:            dispatcher,
		Mailbox:               mailbox,
		Repository:            "DavidDiaz0317/Proof",
		RepositoryFingerprint: strings.Repeat("a", 64),
		RecurrenceKey:         "finding:visual-1",
		Attempt:               2,
		ExpectedBaseSHA:       baseSHA,
		ExpectedBaseTreeSHA:   baseTree,
		Evidence: agent.SpecialistEvidenceIdentity{
			BundleSchemaVersion:       "visual-hive.hive-bundle.v3",
			BundleSHA256:              strings.Repeat("b", 64),
			ManifestSHA256:            strings.Repeat("d", 64),
			ArtifactIndexSHA256:       strings.Repeat("e", 64),
			VerificationReceiptSHA256: strings.Repeat("c", 64),
			RepositoryID:              "123",
			WorkflowRunID:             41,
			WorkflowRunAttempt:        3,
			WorkflowRunHeadSHA:        baseSHA,
			WorkflowName:              "Hive Visual Hive Production",
			WorkflowRunName:           "Hive Visual Hive Production [test]",
			WorkflowPath:              ".github/workflows/hive-visual-hive.yml",
			WorkflowEvent:             "workflow_dispatch",
			HeadBranch:                "main",
			ArtifactID:                73,
			SourceArtifactID:          74,
			ArtifactName:              "visual-hive-evidence",
			ArtifactSHA256:            strings.Repeat("d", 64),
		},
		Specialist:   agent.SpecialistQuality,
		RouteReason:  "test adequacy routes to quality",
		AllowedPaths: []string{"src/**"},
		Validation:   []string{"npm test"},
		Deadline:     now.Add(10 * time.Minute),
		PollInterval: 5 * time.Millisecond,
		Now:          func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatalf("NewSpecialistProvider() error = %v", err)
	}
	return specialistProviderFixture{provider: provider, dispatcher: dispatcher, mailbox: mailbox, worktree: worktree, clock: clock, baseSHA: baseSHA, baseTree: baseTree}
}

func configureGovernedSpecialistFixture(fixture *specialistProviderFixture) {
	profile := &agent.SpecialistProposalExecutorProfile{
		Backend: "codex", ProviderSHA256: fixture.dispatcher.identity.ProviderSHA256, Model: "codex-proof-model",
		ConfigurationSHA256: strings.Repeat("a", 64), ContainmentProfile: agent.SpecialistContainmentProfileV1,
		BackendParityClaimed: false,
	}
	fixture.provider.config.WorkOrderKind = agent.SpecialistWorkOrderKindGovernedVisualHiveProposal
	fixture.provider.config.ExecutorProfile = profile
	identity := &fixture.dispatcher.identity
	identity.Backend = "copilot"
	identity.ConfiguredBackend = "copilot"
	identity.ExecutorBackend = profile.Backend
	identity.ExecutorModel = profile.Model
	identity.ExecutorConfigSHA256 = profile.ConfigurationSHA256
	identity.ExecutorAuthorizationID = "codex-auth-" + strings.Repeat("b", 64)
	identity.ContainmentProfile = profile.ContainmentProfile
	identity.BackendParityClaimed = false
	identity.TmuxSession = ""
	identity.TmuxSocket = ""
}

func newManagerBackedSpecialistExecutor() *managerBackedSpecialistExecutor {
	return &managerBackedSpecialistExecutor{identity: agent.SpecialistChildExecutorIdentity{
		Backend: "codex", ProviderSHA256: strings.Repeat("e", 64), Model: "codex-proof-model",
		ConfigurationSHA256: strings.Repeat("a", 64),
	}}
}

func newManagerBackedSpecialistManager(t *testing.T, state string, executor agent.SpecialistChildExecutor) *agent.Manager {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := agent.NewManager(map[string]config.AgentConfig{
		"quality": {
			Enabled: true, Backend: "copilot", Model: "persistent-copilot-model", Role: "quality",
			LaunchCmd: "inert-persistent-launch", CavemanMode: "full",
			Connections: []config.ConnectionConfig{{Name: "inert-parent-api", Type: "api", URI: "https://parent.invalid"}},
		},
	}, logger, agent.ProjectContext{Org: "fork-only"})
	if err := manager.ConfigureSpecialistChildDispatcher(agent.SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	return manager
}

func configureManagerBackedSpecialistProvider(fixture *specialistProviderFixture, dispatcher SpecialistDispatcher, identity agent.SpecialistChildExecutorIdentity) {
	fixture.provider.config.Dispatcher = dispatcher
	fixture.provider.config.WorkOrderKind = agent.SpecialistWorkOrderKindGovernedVisualHiveProposal
	fixture.provider.config.ExecutorProfile = &agent.SpecialistProposalExecutorProfile{
		Backend: "codex", ProviderSHA256: identity.ProviderSHA256, Model: identity.Model,
		ConfigurationSHA256: identity.ConfigurationSHA256, ContainmentProfile: agent.SpecialistContainmentProfileV1,
		BackendParityClaimed: false,
	}
}

func dispatchManagerBackedSpecialistChild(t *testing.T, fixture specialistProviderFixture, manager *agent.Manager, prompt string) (agent.SpecialistWorkOrder, agent.SpecialistLease, agent.SpecialistTaskResponse) {
	t.Helper()
	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := manager.CheckSpecialistRole(context.Background(), order.Specialist)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.mailbox.AcquireLease(order, fixture.provider.config.LeaseOwner, identity.SessionID, order.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	message, err := specialistDispatchMessage(order, lease)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := manager.DispatchSpecialistTask(context.Background(), agent.SpecialistDispatchRequest{
		TaskID: order.ID, Specialist: order.Specialist, Message: message,
		Order: order, Lease: lease, RequestSHA256: order.RequestSHA256,
		SessionID: identity.SessionID, AgentName: identity.AgentName, AgentID: identity.AgentID,
		ConfiguredBackend: identity.ConfiguredBackend, ProviderSHA256: identity.ProviderSHA256,
		ExecutorBackend: identity.ExecutorBackend, BackendParityClaimed: identity.BackendParityClaimed,
		ContainmentProfile: identity.ContainmentProfile, ExecutorModel: identity.ExecutorModel,
		ExecutorConfigSHA256: identity.ExecutorConfigSHA256, ExecutorAuthorizationID: identity.ExecutorAuthorizationID,
	})
	if err != nil || !dispatch.Started {
		t.Fatalf("real Manager child dispatch failed: result=%+v err=%v", dispatch, err)
	}
	response, err := manager.ObserveSpecialistTaskResponse(context.Background(), agent.SpecialistTaskResponseRequest{
		TaskID: order.ID, Specialist: order.Specialist, SessionID: lease.SessionID, Message: message,
	})
	if err != nil {
		t.Fatal(err)
	}
	return order, lease, response
}

func makeSpecialistTestRepository(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	runSpecialistTestGit(t, root, "init", "--initial-branch=main")
	runSpecialistTestGit(t, root, "config", "user.name", "Hive Test")
	runSpecialistTestGit(t, root, "config", "user.email", "hive@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "value.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSpecialistTestGit(t, root, "add", "--", "src/value.txt")
	runSpecialistTestGit(t, root, "commit", "-m", "initial")
	baseSHA := strings.TrimSpace(runSpecialistTestGit(t, root, "rev-parse", "HEAD"))
	baseTree := strings.TrimSpace(runSpecialistTestGit(t, root, "rev-parse", "HEAD^{tree}"))
	return root, baseSHA, baseTree
}

func runSpecialistTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeSpecialistOutput(t *testing.T, mailbox *agent.SpecialistMailbox, orderID string, status agent.SpecialistCompletionStatus, diff []byte, summary string) {
	t.Helper()
	paths, err := mailbox.Paths(orderID)
	if err != nil {
		t.Fatal(err)
	}
	mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "summary.txt"), []byte(summary+"\n"))
	if status == agent.SpecialistCompletionProposed {
		mustWritePrivateFile(t, paths.UnifiedDiff, diff)
	}
	mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "completion.status"), []byte(string(status)+"\n"))
}

func mustWritePrivateFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func structuredSpecialistTaskResponse(t *testing.T, fixture specialistProviderFixture, request agent.SpecialistTaskResponseRequest, status agent.SpecialistCompletionStatus, diff []byte, summary string) agent.SpecialistTaskResponse {
	return structuredSpecialistTaskResponseAt(t, fixture, request, status, diff, summary, fixture.clock.UTC())
}

func structuredSpecialistTaskResponseAt(t *testing.T, fixture specialistProviderFixture, request agent.SpecialistTaskResponseRequest, status agent.SpecialistCompletionStatus, diff []byte, summary string, completedAt time.Time) agent.SpecialistTaskResponse {
	t.Helper()
	order, err := fixture.mailbox.LoadWorkOrder(request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.mailbox.LoadLease(order)
	if err != nil {
		t.Fatal(err)
	}
	result := specialistModelResult{
		SchemaVersion: specialistModelResultSchema, WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: status, Summary: summary, UnifiedDiff: string(diff),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	dispatchDigest := sha256.Sum256([]byte(request.Message))
	responseDigest := sha256.Sum256(encoded)
	response := agent.SpecialistTaskResponse{
		SchemaVersion: agent.SpecialistTaskResponseSchema, WorkOrderID: order.ID, Specialist: order.Specialist,
		SessionID: lease.SessionID, TurnID: "turn-specialist-test", ProviderSHA256: fixture.dispatcher.identity.ProviderSHA256,
		DispatchSHA256: hex.EncodeToString(dispatchDigest[:]), ResponseSHA256: hex.EncodeToString(responseDigest[:]),
		Response: string(encoded), CompletedAt: completedAt.UTC(),
	}
	if order.Kind == agent.SpecialistWorkOrderKindGovernedVisualHiveProposal && order.ExecutorProfile != nil {
		response.TurnID = fixture.dispatcher.identity.ExecutorAuthorizationID
		authorizationDigest := sha256.Sum256([]byte(response.TurnID))
		response.AuthorizationSHA256 = hex.EncodeToString(authorizationDigest[:])
		response.ExecutorBackend = order.ExecutorProfile.Backend
		response.ExecutorModel = order.ExecutorProfile.Model
		response.ExecutorConfigSHA256 = order.ExecutorProfile.ConfigurationSHA256
		response.ContainmentProfile = order.ExecutorProfile.ContainmentProfile
		response.IntentSHA256 = strings.Repeat("1", 64)
		response.StartedSHA256 = strings.Repeat("2", 64)
		response.CompletionSpoolSHA256 = strings.Repeat("3", 64)
	}
	return response
}

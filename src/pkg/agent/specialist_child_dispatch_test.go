package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

type fakeSpecialistChildExecutor struct {
	mu       sync.Mutex
	identity SpecialistChildExecutorIdentity
	checkErr error
	process  *fakeSpecialistChildProcess
	starts   int
	request  SpecialistChildExecutionRequest
	started  chan struct{}
}

func (executor *fakeSpecialistChildExecutor) Check(context.Context) (SpecialistChildExecutorIdentity, error) {
	if executor.checkErr != nil {
		return SpecialistChildExecutorIdentity{}, executor.checkErr
	}
	return executor.identity, nil
}

func (executor *fakeSpecialistChildExecutor) Start(ctx context.Context, request SpecialistChildExecutionRequest) (SpecialistChildProcess, error) {
	executor.mu.Lock()
	executor.starts++
	executor.request = request
	process := executor.process
	started := executor.started
	executor.mu.Unlock()
	if process != nil {
		process.setLaunch(ctx, request.AuthorizationID)
	}
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if process == nil {
		return nil, errors.New("fake child process is unavailable")
	}
	return process, nil
}

func (executor *fakeSpecialistChildExecutor) snapshot() (int, SpecialistChildExecutionRequest) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.starts, executor.request
}

type fakeSpecialistChildCompletion struct {
	result SpecialistChildExecutionResult
	err    error
}

type fakeSpecialistChildProcess struct {
	pid      int
	provider string
	done     chan fakeSpecialistChildCompletion
	mu       sync.RWMutex
	ctx      context.Context
	turnID   string
}

type hungForceReapSpecialistProcess struct {
	release chan struct{}
	once    sync.Once
	forced  atomic.Bool
}

type failedReapSpecialistProcess struct {
	err    error
	forced *atomic.Bool
}

func (process failedReapSpecialistProcess) PID() int { return 9402 }
func (process failedReapSpecialistProcess) ProviderSHA256() string {
	return strings.Repeat("b", 64)
}
func (process failedReapSpecialistProcess) Wait() (SpecialistChildExecutionResult, error) {
	return SpecialistChildExecutionResult{}, errors.New("provider failed")
}
func (process failedReapSpecialistProcess) ForceReap(context.Context) error {
	if process.forced != nil {
		process.forced.Store(true)
	}
	return process.err
}

func (process *hungForceReapSpecialistProcess) PID() int { return 9401 }
func (process *hungForceReapSpecialistProcess) ProviderSHA256() string {
	return strings.Repeat("a", 64)
}

func TestSpecialistChildShutdownAttemptsEveryExactReapAfterFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	firstForced, secondForced := &atomic.Bool{}, &atomic.Bool{}
	firstErr := errors.New("first containment failure")
	children := map[SpecialistRole]*specialistChildSession{
		SpecialistQuality: {
			taskID: "swo-" + strings.Repeat("c", 64), role: SpecialistQuality, launched: true, done: done, cancel: func() {},
			process: failedReapSpecialistProcess{err: firstErr, forced: firstForced},
		},
		SpecialistCIMaintainer: {
			taskID: "swo-" + strings.Repeat("d", 64), role: SpecialistCIMaintainer, launched: true, done: done, cancel: func() {},
			process: failedReapSpecialistProcess{forced: secondForced},
		},
	}
	err := (&Manager{specialistChildren: children}).ShutdownSpecialistChildren(context.Background())
	if !errors.Is(err, firstErr) || !firstForced.Load() || !secondForced.Load() {
		t.Fatalf("shutdown did not attempt every exact child reap: err=%v first=%t second=%t", err, firstForced.Load(), secondForced.Load())
	}
}

func TestSpecialistChildShutdownCoversProcessThatAppearsDuringLaunch(t *testing.T) {
	process := &hungForceReapSpecialistProcess{release: make(chan struct{})}
	child := &specialistChildSession{
		taskID: "swo-" + strings.Repeat("e", 64), role: SpecialistQuality,
		launchDone: make(chan struct{}), cancel: func() {}, done: make(chan struct{}),
	}
	manager := &Manager{specialistChildren: map[SpecialistRole]*specialistChildSession{SpecialistQuality: child}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		child.mu.Lock()
		child.launched, child.process = true, process
		child.mu.Unlock()
		child.launchOnce.Do(func() { close(child.launchDone) })
		_, _ = process.Wait()
		child.doneOnce.Do(func() { close(child.done) })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := manager.ShutdownSpecialistChildren(ctx); err != nil {
		t.Fatalf("shutdown missed process appearing during Start: %v", err)
	}
	if !process.forced.Load() {
		t.Fatal("shutdown did not force-reap process that appeared during Start")
	}
}
func (process *hungForceReapSpecialistProcess) Wait() (SpecialistChildExecutionResult, error) {
	<-process.release
	return SpecialistChildExecutionResult{}, context.Canceled
}
func (process *hungForceReapSpecialistProcess) ForceReap(context.Context) error {
	process.forced.Store(true)
	process.once.Do(func() { close(process.release) })
	return nil
}

func newFakeSpecialistChildProcess(pid int, provider string) *fakeSpecialistChildProcess {
	return &fakeSpecialistChildProcess{pid: pid, provider: provider, done: make(chan fakeSpecialistChildCompletion, 1)}
}

func (process *fakeSpecialistChildProcess) PID() int               { return process.pid }
func (process *fakeSpecialistChildProcess) ProviderSHA256() string { return process.provider }
func (process *fakeSpecialistChildProcess) Wait() (SpecialistChildExecutionResult, error) {
	process.mu.RLock()
	ctx := process.ctx
	process.mu.RUnlock()
	if ctx == nil {
		return SpecialistChildExecutionResult{}, errors.New("fake specialist child has no launch context")
	}
	select {
	case completion := <-process.done:
		return completion.result, completion.err
	case <-ctx.Done():
		return SpecialistChildExecutionResult{}, ctx.Err()
	}
}

func (process *fakeSpecialistChildProcess) complete(response string) {
	process.mu.RLock()
	turnID := process.turnID
	process.mu.RUnlock()
	process.completeWithTurn(response, turnID)
}

func (process *fakeSpecialistChildProcess) completeWithTurn(response, turnID string) {
	process.done <- fakeSpecialistChildCompletion{result: SpecialistChildExecutionResult{
		Response: []byte(response), TurnID: turnID, CompletedAt: time.Now().UTC(),
	}}
}

func (process *fakeSpecialistChildProcess) fail(err error) {
	process.done <- fakeSpecialistChildCompletion{err: err}
}

func (process *fakeSpecialistChildProcess) ForceReap(context.Context) error {
	process.mu.RLock()
	ctx := process.ctx
	process.mu.RUnlock()
	if ctx == nil {
		return errors.New("fake specialist child has no launch context")
	}
	<-ctx.Done()
	return nil
}

func (process *fakeSpecialistChildProcess) setLaunch(ctx context.Context, turnID string) {
	process.mu.Lock()
	defer process.mu.Unlock()
	process.ctx = ctx
	process.turnID = turnID
}

func TestSpecialistChildShutdownForceReapsHungExactChildBeforeSuccess(t *testing.T) {
	process := &hungForceReapSpecialistProcess{release: make(chan struct{})}
	child := &specialistChildSession{
		taskID: "swo-" + strings.Repeat("a", 64), role: SpecialistQuality,
		process: process, launched: true, cancel: func() {}, done: make(chan struct{}),
	}
	manager := &Manager{specialistChildren: map[SpecialistRole]*specialistChildSession{SpecialistQuality: child}}
	go func() {
		_, _ = process.Wait()
		child.doneOnce.Do(func() { close(child.done) })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := manager.ShutdownSpecialistChildren(ctx); err != nil {
		t.Fatalf("force-reap shutdown failed: %v", err)
	}
	if !process.forced.Load() {
		t.Fatal("shutdown did not force-reap the hung exact specialist child")
	}
	select {
	case <-child.done:
	default:
		t.Fatal("shutdown returned before hung specialist child completion was verified")
	}
}

func TestSpecialistChildShutdownDoesNotTreatClosedDoneAsReapProof(t *testing.T) {
	done := make(chan struct{})
	close(done)
	containmentErr := errors.New("process-tree containment was not proved")
	child := &specialistChildSession{
		taskID: "swo-" + strings.Repeat("b", 64), role: SpecialistQuality,
		process: failedReapSpecialistProcess{err: containmentErr}, launched: true,
		cancel: func() {}, done: done,
	}
	manager := &Manager{specialistChildren: map[SpecialistRole]*specialistChildSession{SpecialistQuality: child}}
	err := manager.ShutdownSpecialistChildren(context.Background())
	if !errors.Is(err, containmentErr) {
		t.Fatalf("shutdown released ownership without exact reap proof: %v", err)
	}
}

func testSpecialistChildIdentity(backend string) SpecialistChildExecutorIdentity {
	return SpecialistChildExecutorIdentity{
		Backend: backend, ProviderSHA256: strings.Repeat("a", 64), Model: "codex-proof-model",
		ConfigurationSHA256: strings.Repeat("b", 64), AuthorizationID: "codex-auth-" + strings.Repeat("c", 64),
	}
}

func configureTestSpecialistChild(t *testing.T, manager *Manager, executor SpecialistChildExecutor) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "repair-state")
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestResetSpecialistChildDispatcherRequiresNoActiveChildrenAndAllowsRebind(t *testing.T) {
	manager := &Manager{specialistChildren: make(map[SpecialistRole]*specialistChildSession)}
	first := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex")}
	firstState := configureTestSpecialistChild(t, manager, first)
	manager.specialistChildren[SpecialistQuality] = &specialistChildSession{}
	if err := manager.ResetSpecialistChildDispatcher(firstState); err == nil || !strings.Contains(err.Error(), "children are active") {
		t.Fatalf("active reset error=%v", err)
	}
	delete(manager.specialistChildren, SpecialistQuality)
	manager.specialistsDown = true
	if err := manager.ResetSpecialistChildDispatcher(filepath.Join(t.TempDir(), "different")); err == nil || !strings.Contains(err.Error(), "different repair state root") {
		t.Fatalf("mismatched reset error=%v", err)
	}
	if err := manager.ResetSpecialistChildDispatcher(firstState); err != nil {
		t.Fatal(err)
	}
	if manager.specialistChildExecutor != nil || manager.specialistChildRoot != "" || manager.specialistsDown {
		t.Fatal("specialist dispatcher reset left runtime bindings behind")
	}
	second := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex")}
	configureTestSpecialistChild(t, manager, second)
	if manager.specialistChildExecutor != second || manager.specialistChildRoot == "" {
		t.Fatal("specialist dispatcher did not accept managed rebind")
	}
}

func testSpecialistChildDispatchRequest(t *testing.T, identity SpecialistSessionIdentity, kind string) SpecialistDispatchRequest {
	profile := &SpecialistProposalExecutorProfile{
		Backend: "codex", ProviderSHA256: identity.ProviderSHA256, Model: identity.ExecutorModel,
		ConfigurationSHA256: identity.ExecutorConfigSHA256, ContainmentProfile: identity.ContainmentProfile,
		BackendParityClaimed: false,
	}
	return testSpecialistChildDispatchRequestWithProfile(t, identity, kind, profile)
}

func testSpecialistChildDispatchRequestWithProfile(t *testing.T, identity SpecialistSessionIdentity, kind string, profile *SpecialistProposalExecutorProfile) SpecialistDispatchRequest {
	t.Helper()
	now := time.Now().UTC()
	mailbox, err := NewSpecialistMailbox(filepath.Join(t.TempDir(), "mailbox"), SpecialistMailboxOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	workOrderRequest := testSpecialistRequest(now, "governed-child:"+string(identity.Specialist), now.Add(5*time.Minute))
	if kind == SpecialistWorkOrderKindGovernedVisualHiveProposal {
		workOrderRequest = testGovernedSpecialistRequest(now, "governed-child:"+string(identity.Specialist), now.Add(5*time.Minute))
		workOrderRequest.ExecutorProfile = profile
		workOrderRequest.ExecutorProfileSHA256 = specialistExecutorProfileDigest(*profile)
	} else {
		workOrderRequest.Kind = kind
	}
	order, err := mailbox.Prepare(workOrderRequest)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "test-controller", identity.SessionID, order.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	message := "ROLE_POLICY: preserve expert review. PROJECT_CONTEXT: fork-only proof. READ_ONLY_KNOWLEDGE: verified evidence. OPERATIONAL_INJECTION: use GitHub and parent tools."
	return SpecialistDispatchRequest{
		TaskID: order.ID, Specialist: order.Specialist, Message: message,
		Order: order, Lease: lease, RequestSHA256: order.RequestSHA256,
		SessionID: identity.SessionID, AgentName: identity.AgentName, AgentID: identity.AgentID,
		ConfiguredBackend: identity.ConfiguredBackend, ExecutorBackend: identity.ExecutorBackend,
		BackendParityClaimed: identity.BackendParityClaimed, ContainmentProfile: identity.ContainmentProfile,
		ProviderSHA256: identity.ProviderSHA256, ExecutorModel: identity.ExecutorModel,
		ExecutorConfigSHA256: identity.ExecutorConfigSHA256, ExecutorAuthorizationID: identity.ExecutorAuthorizationID,
	}
}

func testSpecialistChildVerificationRequest(request SpecialistDispatchRequest, response SpecialistTaskResponse) SpecialistTaskCompletionVerificationRequest {
	return SpecialistTaskCompletionVerificationRequest{
		TaskID: request.TaskID, Specialist: request.Specialist, RequestSHA256: request.RequestSHA256,
		SessionID: request.SessionID, Message: request.Message,
		Provenance: SpecialistContainedChildProvenance{
			ExecutorProfile: *request.Order.ExecutorProfile, AuthorizationSHA256: response.AuthorizationSHA256,
			SessionID: response.SessionID, TurnID: response.TurnID, DispatchSHA256: response.DispatchSHA256,
			ResponseSHA256: response.ResponseSHA256, IntentSHA256: response.IntentSHA256,
			StartedSHA256: response.StartedSHA256, CompletionSpoolSHA256: response.CompletionSpoolSHA256,
		},
	}
}

func overwriteSpecialistChildTestDocument(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installPassingTmuxStub(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	name := "tmux"
	content := "#!/bin/sh\nprintf 'Copilot\\n> Enter to send\\n'\n"
	if runtime.GOOS == "windows" {
		name = "tmux.cmd"
		content = "@echo off\r\necho Copilot\r\necho ^> Enter to send\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSpecialistChildPreservesCopilotParentAndAllowsConcurrentNormalKick(t *testing.T) {
	installPassingTmuxStub(t)
	configBefore := config.AgentConfig{
		Enabled: true, Backend: "copilot", Model: "persistent-copilot-model", Mode: "ISSUES_PRS_MERGE", Role: "quality",
		LaunchCmd: "parent-launch-command-must-remain-inert", CavemanMode: "full",
		Tools:       &config.ToolsConfig{Preset: "full"},
		Connections: []config.ConnectionConfig{{Name: "parent-live-api", Type: "api", URI: "https://parent.invalid", Auth: &config.ConnectionAuth{Type: "env", EnvVar: "PARENT_LIVE_TOKEN"}}},
	}
	manager := newManager(map[string]config.AgentConfig{"quality": configBefore}, testSpecialistLogger(), ProjectContext{Org: "fork-only"}, t.TempDir())
	parent := manager.agents["quality"]
	// This test exercises persistent-role isolation, not the production
	// per-agent UID boundary. Keep tmux entirely behind the local stub.
	parent.UID = 0
	startedAt := time.Now().Add(-time.Hour).UTC()
	parent.State, parent.PID, parent.StartedAt = StateRunning, 4242, &startedAt
	parent.tmuxSession, parent.tmuxSocket, parent.launchGen = "persistent-quality-pane", "persistent-socket", 17
	parent.BackendOverride, parent.ModelOverride = "claude", "persistent-override-model"
	var parentCancelCalls atomic.Int32
	parent.cancel = func() { parentCancelCalls.Add(1) }

	process := newFakeSpecialistChildProcess(9001, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	state := configureTestSpecialistChild(t, manager, executor)
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ConfiguredBackend != "copilot" || identity.ExecutorBackend != "codex" || identity.BackendParityClaimed || identity.ExecutorModel == configBefore.Model {
		t.Fatalf("configured/executor identities were not kept separate: %+v", identity)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)

	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
		dispatchDone <- dispatchErr
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("contained child did not start")
	}
	if err := manager.SendKick("quality", "ordinary persistent-role kick"); err != nil {
		t.Fatalf("normal kick was blocked by separate child WIP: %v", err)
	}
	process.complete(`{"schema_version":"test.proposal.v1","status":"blocked","summary":"bounded"}`)
	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("contained child did not finish")
	}

	starts, childRequest := executor.snapshot()
	if starts != 1 || process.PID() == parent.PID {
		t.Fatalf("child process identity was not distinct: starts=%d request=%+v", starts, childRequest)
	}
	if !strings.HasPrefix(childRequest.Root, filepath.Join(state, "specialist-children")) || strings.Contains(strings.ToLower(childRequest.Root), ".git") {
		t.Fatalf("child did not use a neutral controller root: %q", childRequest.Root)
	}
	for _, forbidden := range []string{configBefore.LaunchCmd, "parent-live-api", "PARENT_LIVE_TOKEN"} {
		if strings.Contains(childRequest.Prompt, forbidden) {
			t.Fatalf("parent runtime configuration leaked into child prompt: %q", forbidden)
		}
	}
	if strings.Contains(childRequest.Prompt, `"caveman_mode":"`+configBefore.CavemanMode+`"`) {
		t.Fatalf("parent caveman runtime configuration leaked into child prompt: %q", configBefore.CavemanMode)
	}
	if !strings.Contains(childRequest.Prompt, "ROLE_POLICY") || !strings.Contains(childRequest.Prompt, "FINAL_CAPABILITY_WRAPPER_V1") ||
		strings.LastIndex(childRequest.Prompt, "FINAL_CAPABILITY_WRAPPER_V1") < strings.Index(childRequest.Prompt, "OPERATIONAL_INJECTION") {
		t.Fatalf("expertise context was not preserved and finally demoted: %s", childRequest.Prompt)
	}
	if parent.PID != 4242 || parent.tmuxSession != "persistent-quality-pane" || parent.tmuxSocket != "persistent-socket" || parent.launchGen != 17 ||
		parent.BackendOverride != "claude" || parent.ModelOverride != "persistent-override-model" || parent.State != StateRunning || parent.StartedAt != &startedAt ||
		!reflect.DeepEqual(parent.Config, configBefore) || parentCancelCalls.Load() != 0 {
		t.Fatalf("persistent parent identity/configuration changed: %+v", parent)
	}
	if len(manager.specialistLeases) != 0 || manager.specialistChildren[SpecialistQuality] == nil {
		t.Fatalf("child state was not isolated from persistent leases: leases=%v children=%v", manager.specialistLeases, manager.specialistChildren)
	}
	parent.cancel()
	if parentCancelCalls.Load() != 1 {
		t.Fatal("parent cancellation identity was replaced")
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, request.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestSpecialistChildAcceptsAllExistingRoleMetadataAsInertExpertise(t *testing.T) {
	configs := make(map[string]config.AgentConfig, len(specialistRoleAllowlist))
	for role := range specialistRoleAllowlist {
		backend := "copilot"
		if role == SpecialistSecurity || role == SpecialistArchitect {
			backend = "claude"
		}
		configs[string(role)] = config.AgentConfig{
			Enabled: true, Backend: backend, Model: "parent-" + string(role), Role: string(role), Description: "policy-" + string(role),
			LaunchCmd: "persistent-launch-" + string(role), CavemanMode: "lite", Tools: &config.ToolsConfig{Preset: "full"},
			Connections: []config.ConnectionConfig{{Name: "parent-connection-" + string(role), Type: "mcp", URI: "local"}},
		}
	}
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{PolicyDir: "read-only-policy"}, t.TempDir())
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex")}
	configureTestSpecialistChild(t, manager, executor)
	for role := range specialistRoleAllowlist {
		before := configs[string(role)]
		identity, err := manager.CheckSpecialistRole(context.Background(), role)
		if err != nil {
			t.Fatalf("%s inert parent metadata blocked child: %v", role, err)
		}
		if identity.ConfiguredBackend != before.Backend || identity.ExecutorBackend != "codex" || identity.BackendParityClaimed || !reflect.DeepEqual(manager.agents[string(role)].Config, before) {
			t.Fatalf("%s parent/executor audit identity drift: identity=%+v config=%+v", role, identity, manager.agents[string(role)].Config)
		}
		message := "ROLE_POLICY_" + string(role) + " PROJECT_CONTEXT READ_ONLY_KNOWLEDGE OPERATIONAL_INJECTION"
		prompt, err := specialistChildCapabilityPrompt(SpecialistDispatchRequest{Message: message})
		if err != nil || !strings.Contains(prompt, message) || strings.LastIndex(prompt, "FINAL_CAPABILITY_WRAPPER_V1") < strings.Index(prompt, "OPERATIONAL_INJECTION") {
			t.Fatalf("%s expertise was not preserved under the final capability wrapper: %v", role, err)
		}
	}
	if starts, _ := executor.snapshot(); starts != 0 {
		t.Fatalf("readiness launched %d child processes", starts)
	}
}

func TestSpecialistChildRejectsNonCodexExecutorAndLegacyOrderBeforeStart(t *testing.T) {
	manager := newManager(map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "copilot", Model: "persistent-model", Role: "quality"},
	}, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	nonCodex := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("claude")}
	configureTestSpecialistChild(t, manager, nonCodex)
	if _, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality); err == nil || !strings.Contains(err.Error(), "sealed Codex identity") {
		t.Fatalf("non-Codex proposal executor was accepted: %v", err)
	}
	if starts, _ := nonCodex.snapshot(); starts != 0 {
		t.Fatalf("non-Codex executor reached launch %d times", starts)
	}

	manager = newManager(map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "claude", Model: "persistent-model", Role: "quality"},
	}, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9002, strings.Repeat("a", 64))
	codex := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process}
	configureTestSpecialistChild(t, manager, codex)
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	legacy := testSpecialistChildDispatchRequest(t, identity, "")
	if _, err := manager.DispatchSpecialistTask(context.Background(), legacy); err == nil || !strings.Contains(err.Error(), "exact immutable work order") {
		t.Fatalf("legacy work order entered contained child path: %v", err)
	}
	if starts, _ := codex.snapshot(); starts != 0 {
		t.Fatalf("legacy work order reached launch %d times", starts)
	}

	baseProfile := SpecialistProposalExecutorProfile{
		Backend: "codex", ProviderSHA256: identity.ProviderSHA256, Model: identity.ExecutorModel,
		ConfigurationSHA256: identity.ExecutorConfigSHA256, ContainmentProfile: SpecialistContainmentProfileV1,
	}
	mismatches := map[string]func(*SpecialistProposalExecutorProfile){
		"backend":  func(profile *SpecialistProposalExecutorProfile) { profile.Backend = "claude" },
		"provider": func(profile *SpecialistProposalExecutorProfile) { profile.ProviderSHA256 = strings.Repeat("d", 64) },
		"model":    func(profile *SpecialistProposalExecutorProfile) { profile.Model = "other-codex-model" },
		"config": func(profile *SpecialistProposalExecutorProfile) {
			profile.ConfigurationSHA256 = strings.Repeat("e", 64)
		},
		"containment": func(profile *SpecialistProposalExecutorProfile) { profile.ContainmentProfile = "degraded" },
		"parity claim": func(profile *SpecialistProposalExecutorProfile) {
			profile.BackendParityClaimed = true
		},
	}
	for name, mutate := range mismatches {
		t.Run("profile_"+name, func(t *testing.T) {
			profile := baseProfile
			mutate(&profile)
			mismatch := testSpecialistChildDispatchRequestWithProfile(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal, &profile)
			if _, err := manager.DispatchSpecialistTask(context.Background(), mismatch); err == nil || !strings.Contains(err.Error(), "governed work-order profile") {
				t.Fatalf("governed executor profile mismatch reached launch: %v", err)
			}
		})
	}
	if starts, _ := codex.snapshot(); starts != 0 {
		t.Fatalf("profile mismatch launched %d model processes", starts)
	}
}

func TestSpecialistChildSingleLaunchAndCompleteSpoolRecovery(t *testing.T) {
	configs := map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "copilot", Model: "persistent-model", Role: "quality"},
	}
	state := filepath.Join(t.TempDir(), "repair-state")
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9101, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
			results <- dispatchErr
		}()
	}
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("single child invocation did not start")
	}
	process.complete(`{"schema_version":"test.proposal.v1","status":"blocked","summary":"recovered"}`)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("duplicate waiter did not finish")
		}
	}
	if starts, _ := executor.snapshot(); starts != 1 {
		t.Fatalf("same task launched %d model processes", starts)
	}
	if err := manager.ShutdownSpecialistChildren(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "specialist-children", request.TaskID, "complete.json")); err != nil {
		t.Fatalf("shutdown deleted the unconsumed completion spool: %v", err)
	}

	restarted := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	recoveryExecutor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: newFakeSpecialistChildProcess(9102, strings.Repeat("a", 64))}
	if err := restarted.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: recoveryExecutor}); err != nil {
		t.Fatal(err)
	}
	recoveredIdentity, err := restarted.InspectSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredIdentity.SessionID != identity.SessionID || recoveredIdentity.ProviderSHA256 != identity.ProviderSHA256 {
		t.Fatalf("recovered spool identity drift: got=%+v want=%+v", recoveredIdentity, identity)
	}
	response, err := restarted.ObserveSpecialistTaskResponse(context.Background(), SpecialistTaskResponseRequest{
		TaskID: request.TaskID, Specialist: request.Specialist, SessionID: request.SessionID, Message: request.Message,
	})
	if err != nil || !strings.Contains(response.Response, "recovered") {
		t.Fatalf("complete spool recovery failed: response=%+v err=%v", response, err)
	}
	if _, err := restarted.CheckSpecialistRole(context.Background(), SpecialistQuality); err == nil || !strings.Contains(err.Error(), "durable child invocation intent") {
		t.Fatalf("complete spool allowed a second readiness authorization: %v", err)
	}
	if starts, _ := recoveryExecutor.snapshot(); starts != 0 {
		t.Fatalf("complete spool recovery launched %d additional model processes", starts)
	}
	if err := restarted.ReleaseSpecialistTask(SpecialistQuality, request.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestSpecialistChildPostStartFailureAndCallerCancelNeverRedispatch(t *testing.T) {
	configs := map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "claude", Model: "persistent-model", Role: "quality"},
	}
	state := filepath.Join(t.TempDir(), "repair-state")
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9201, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	caller, cancelCaller := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, dispatchErr := manager.DispatchSpecialistTask(caller, request)
		first <- dispatchErr
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child invocation did not start")
	}
	cancelCaller()
	if err := <-first; err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
		t.Fatalf("caller cancellation was not classified as ambiguous: %v", err)
	}
	process.fail(errors.New("simulated controller-visible child failure"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = manager.ObserveSpecialistTaskResponse(context.Background(), SpecialistTaskResponseRequest{
			TaskID: request.TaskID, Specialist: request.Specialist, SessionID: request.SessionID,
			Message: request.Message,
		})
		if err != nil && !errors.Is(err, ErrSpecialistTaskResponsePending) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed child did not become observable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(err.Error(), "simulated controller-visible child failure") {
		t.Fatalf("unexpected child failure: %v", err)
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, request.TaskID); err == nil || !strings.Contains(err.Error(), "successful content-bound completion") {
		t.Fatalf("failed child evidence was releasable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "specialist-children", request.TaskID, "intent.json")); err != nil {
		t.Fatalf("failed release deleted ambiguous intent: %v", err)
	}

	restarted := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	recoveryExecutor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: newFakeSpecialistChildProcess(9202, strings.Repeat("a", 64))}
	if err := restarted.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: recoveryExecutor}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.InspectSpecialistRole(context.Background(), SpecialistQuality); err == nil || !strings.Contains(err.Error(), "incomplete or conflicting completion spool") {
		t.Fatalf("incomplete post-start spool was not failed closed: %v", err)
	}
	if _, err := restarted.DispatchSpecialistTask(context.Background(), request); err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
		t.Fatalf("post-start failure was redispatched: %v", err)
	}
	if starts, _ := recoveryExecutor.snapshot(); starts != 0 {
		t.Fatalf("ambiguous invocation launched %d replacement model processes", starts)
	}
}

func TestSpecialistChildShutdownRetainsLaunchedAmbiguousIntentAndNeverRedispatches(t *testing.T) {
	configs := map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "copilot", Model: "persistent-model", Role: "quality"},
	}
	state := filepath.Join(t.TempDir(), "repair-state")
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9301, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
		dispatchDone <- dispatchErr
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not launch before shutdown")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := manager.ShutdownSpecialistChildren(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dispatchDone:
		if err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
			t.Fatalf("shutdown launch was not ambiguous: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not reap the exact child")
	}
	childRoot := filepath.Join(state, "specialist-children", request.TaskID)
	for _, name := range []string{"intent.json", "started.json"} {
		if _, err := os.Stat(filepath.Join(childRoot, name)); err != nil {
			t.Fatalf("shutdown deleted %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(childRoot, "complete.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled child fabricated a complete spool: %v", err)
	}

	restarted := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	recoveryExecutor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: newFakeSpecialistChildProcess(9302, strings.Repeat("a", 64))}
	if err := restarted.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: recoveryExecutor}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.InspectSpecialistRole(context.Background(), SpecialistQuality); err == nil || !strings.Contains(err.Error(), "incomplete or conflicting completion spool") {
		t.Fatalf("shutdown intent was not retained as ambiguous: %v", err)
	}
	if _, err := restarted.DispatchSpecialistTask(context.Background(), request); err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
		t.Fatalf("retained shutdown intent was redispatched: %v", err)
	}
	if starts, _ := recoveryExecutor.snapshot(); starts != 0 {
		t.Fatalf("retained shutdown intent launched %d replacement models", starts)
	}
}

func TestSpecialistChildCompletionVerifierBindsExactRetainedEvidenceAcrossRestart(t *testing.T) {
	configs := map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "copilot", Model: "persistent-model", Role: "quality"},
	}
	state := filepath.Join(t.TempDir(), "repair-state")
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9401, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	dispatched := make(chan error, 1)
	go func() {
		_, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
		dispatched <- dispatchErr
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("contained child did not start")
	}
	process.complete(`{"schema_version":"test.proposal.v1","status":"blocked","summary":"verified retained evidence"}`)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	response, err := manager.ObserveSpecialistTaskResponse(context.Background(), SpecialistTaskResponseRequest{
		TaskID: request.TaskID, Specialist: request.Specialist, SessionID: request.SessionID, Message: request.Message,
	})
	if err != nil {
		t.Fatal(err)
	}
	verification := testSpecialistChildVerificationRequest(request, response)
	verified, err := manager.VerifySpecialistTaskCompletion(context.Background(), verification)
	if err != nil || !equalJSON(verified, response) {
		t.Fatalf("genuine Manager evidence did not verify: response=%+v err=%v", verified, err)
	}

	wrongTurn := verification
	wrongTurn.Provenance.TurnID = "codex-auth-" + strings.Repeat("d", 64)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), wrongTurn); err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
		t.Fatalf("mutated turn provenance verified: %v", err)
	}
	wrongStarted := verification
	wrongStarted.Provenance.StartedSHA256 = strings.Repeat("e", 64)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), wrongStarted); err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
		t.Fatalf("mutated started provenance verified: %v", err)
	}
	wrongProfile := verification
	wrongProfile.Provenance.ExecutorProfile.ConfigurationSHA256 = strings.Repeat("f", 64)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), wrongProfile); err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
		t.Fatalf("mutated executor profile verified: %v", err)
	}
	wrongResponse := verification
	wrongResponse.Provenance.ResponseSHA256 = strings.Repeat("d", 64)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), wrongResponse); err == nil || !strings.Contains(err.Error(), "Manager-owned child evidence") {
		t.Fatalf("mutated response digest verified: %v", err)
	}

	childRoot := filepath.Join(state, "specialist-children", request.TaskID)
	intentPath := filepath.Join(childRoot, "intent.json")
	originalIntent, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	var tamperedIntent specialistChildIntent
	if err := json.Unmarshal(originalIntent, &tamperedIntent); err != nil {
		t.Fatal(err)
	}
	tamperedIntent.MessageSHA256 = strings.Repeat("0", 64)
	overwriteSpecialistChildTestDocument(t, intentPath, tamperedIntent)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), verification); err == nil || !strings.Contains(err.Error(), "retained specialist child intent") {
		t.Fatalf("tampered intent.json verified: %v", err)
	}
	if err := os.WriteFile(intentPath, originalIntent, 0o600); err != nil {
		t.Fatal(err)
	}

	startedPath := filepath.Join(childRoot, "started.json")
	originalStarted, err := os.ReadFile(startedPath)
	if err != nil {
		t.Fatal(err)
	}
	var started specialistChildStarted
	if err := json.Unmarshal(originalStarted, &started); err != nil {
		t.Fatal(err)
	}
	started.PID++
	overwriteSpecialistChildTestDocument(t, startedPath, started)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), verification); err == nil || !strings.Contains(err.Error(), "retained specialist child start") {
		t.Fatalf("tampered started.json verified: %v", err)
	}
	if err := os.WriteFile(startedPath, originalStarted, 0o600); err != nil {
		t.Fatal(err)
	}

	spoolPath := filepath.Join(childRoot, "complete.json")
	originalSpool, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	var spool specialistChildSpool
	if err := json.Unmarshal(originalSpool, &spool); err != nil {
		t.Fatal(err)
	}
	spool.Response.TurnID = "codex-auth-" + strings.Repeat("f", 64)
	spool.SpoolSHA256 = ""
	spool.SpoolSHA256, err = digestJSON(spool)
	if err != nil {
		t.Fatal(err)
	}
	overwriteSpecialistChildTestDocument(t, spoolPath, spool)
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), verification); err == nil || !strings.Contains(err.Error(), "retained specialist child completion") {
		t.Fatalf("tampered complete.json verified: %v", err)
	}
	if err := os.WriteFile(spoolPath, originalSpool, 0o600); err != nil {
		t.Fatal(err)
	}
	removedSpoolPath := spoolPath + ".removed"
	if err := os.Rename(spoolPath, removedSpoolPath); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), verification); err == nil || !strings.Contains(err.Error(), "retained specialist child completion") {
		t.Fatalf("removed complete.json verified: %v", err)
	}
	if err := os.Rename(removedSpoolPath, spoolPath); err != nil {
		t.Fatal(err)
	}

	if err := manager.ReleaseSpecialistTask(SpecialistQuality, request.TaskID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSpecialistChildIntent(childRoot)
	if err != nil {
		t.Fatal(err)
	}
	started, err = loadSpecialistChildStarted(childRoot, intent)
	if err != nil {
		t.Fatal(err)
	}
	spool, err = loadSpecialistChildSpool(childRoot, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadSpecialistChildConsumed(childRoot, intent, started, spool); err != nil {
		t.Fatalf("consumed marker was not content-bound: %v", err)
	}

	restarted := newManager(configs, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	recoveryExecutor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: newFakeSpecialistChildProcess(9402, strings.Repeat("a", 64))}
	if err := restarted.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: state, Executor: recoveryExecutor}); err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.VerifySpecialistTaskCompletion(context.Background(), verification); err != nil || !equalJSON(got, response) {
		t.Fatalf("restart could not verify retained consumed evidence: got=%+v err=%v", got, err)
	}
	if starts, _ := recoveryExecutor.snapshot(); starts != 0 {
		t.Fatalf("retained-spool verification launched %d models", starts)
	}
	if _, err := restarted.CheckSpecialistRole(context.Background(), SpecialistQuality); err != nil {
		t.Fatalf("consumed evidence kept role WIP after restart: %v", err)
	}
}

func TestSpecialistChildRejectsEmptyOrWrongAttestedTurn(t *testing.T) {
	for _, test := range []struct {
		name string
		turn func(SpecialistSessionIdentity) string
	}{
		{name: "empty", turn: func(SpecialistSessionIdentity) string { return "" }},
		{name: "wrong", turn: func(SpecialistSessionIdentity) string { return "codex-auth-" + strings.Repeat("d", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newManager(map[string]config.AgentConfig{
				"quality": {Enabled: true, Backend: "copilot", Role: "quality"},
			}, testSpecialistLogger(), ProjectContext{}, t.TempDir())
			process := newFakeSpecialistChildProcess(9501, strings.Repeat("a", 64))
			executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
			state := configureTestSpecialistChild(t, manager, executor)
			identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
			if err != nil {
				t.Fatal(err)
			}
			request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
			done := make(chan error, 1)
			go func() {
				_, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
				done <- dispatchErr
			}()
			select {
			case <-executor.started:
			case <-time.After(5 * time.Second):
				t.Fatal("contained child did not start")
			}
			process.completeWithTurn(`{"schema_version":"test.proposal.v1","status":"blocked","summary":"bad turn"}`, test.turn(identity))
			if err := <-done; err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) || !strings.Contains(err.Error(), "attested execution turn") {
				t.Fatalf("invalid turn was accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(state, "specialist-children", request.TaskID, "complete.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid turn persisted a completion spool: %v", err)
			}
		})
	}
}

func TestSpecialistChildCanceledOriginalAndReusedWaiterConsumeOnce(t *testing.T) {
	manager := newManager(map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "claude", Model: "persistent-model", Role: "quality"},
	}, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	process := newFakeSpecialistChildProcess(9601, strings.Repeat("a", 64))
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex"), process: process, started: make(chan struct{})}
	state := configureTestSpecialistChild(t, manager, executor)
	identity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistChildDispatchRequest(t, identity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, dispatchErr := manager.DispatchSpecialistTask(firstCtx, request)
		firstDone <- dispatchErr
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("contained child did not start")
	}
	cancelFirst()
	if err := <-firstDone; err == nil || !errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
		t.Fatalf("original canceled waiter was not ambiguous: %v", err)
	}

	reusedDone := make(chan struct {
		result SpecialistDispatchResult
		err    error
	}, 1)
	go func() {
		result, dispatchErr := manager.DispatchSpecialistTask(context.Background(), request)
		reusedDone <- struct {
			result SpecialistDispatchResult
			err    error
		}{result: result, err: dispatchErr}
	}()
	process.complete(`{"schema_version":"test.proposal.v1","status":"blocked","summary":"reused waiter consumed"}`)
	reused := <-reusedDone
	if reused.err != nil || !reused.result.Reused {
		t.Fatalf("second waiter did not reuse exact child: result=%+v err=%v", reused.result, reused.err)
	}
	response, err := manager.ObserveSpecialistTaskResponse(context.Background(), SpecialistTaskResponseRequest{
		TaskID: request.TaskID, Specialist: request.Specialist, SessionID: request.SessionID, Message: request.Message,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifySpecialistTaskCompletion(context.Background(), testSpecialistChildVerificationRequest(request, response)); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, request.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "specialist-children", request.TaskID, "consumed.json")); err != nil {
		t.Fatalf("reused successful waiter did not persist consumption: %v", err)
	}
	newIdentity, err := manager.CheckSpecialistRole(context.Background(), SpecialistQuality)
	if err != nil {
		t.Fatalf("consumed reused child kept the role WIP: %v", err)
	}
	newRequest := testSpecialistChildDispatchRequest(t, newIdentity, SpecialistWorkOrderKindGovernedVisualHiveProposal)
	if newRequest.TaskID == request.TaskID {
		t.Fatal("next readiness authorization recomposed the consumed task")
	}
	if starts, _ := executor.snapshot(); starts != 1 {
		t.Fatalf("reused wait path launched %d models", starts)
	}
}

func TestSpecialistChildRejectsLinkedRepairStateBeforeExecutorCheck(t *testing.T) {
	actual := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked-repair-state")
	if err := os.Symlink(actual, linked); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	manager := newManager(map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "copilot", Role: "quality"},
	}, testSpecialistLogger(), ProjectContext{}, t.TempDir())
	executor := &fakeSpecialistChildExecutor{identity: testSpecialistChildIdentity("codex")}
	if err := manager.ConfigureSpecialistChildDispatcher(SpecialistChildDispatcherOptions{RepairStateRoot: linked, Executor: executor}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "link") {
		t.Fatalf("linked repair state was accepted: %v", err)
	}
	if starts, _ := executor.snapshot(); starts != 0 {
		t.Fatalf("linked repair state reached %d child starts", starts)
	}
}

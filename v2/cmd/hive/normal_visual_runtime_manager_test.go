package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

type fakeNormalVisualRuntime struct {
	binding   normalVisualRuntimeBinding
	startErr  error
	starts    atomic.Int32
	triggers  atomic.Int32
	stops     atomic.Int32
	releases  atomic.Int32
	readiness atomic.Int32
}

func (runtime *fakeNormalVisualRuntime) Binding() normalVisualRuntimeBinding {
	return runtime.binding
}

func (runtime *fakeNormalVisualRuntime) Start(context.Context) error {
	runtime.starts.Add(1)
	return runtime.startErr
}

func (runtime *fakeNormalVisualRuntime) Trigger(context.Context) error {
	runtime.triggers.Add(1)
	return nil
}

func (runtime *fakeNormalVisualRuntime) Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	return visualcontroller.Result{}, nil
}

func (runtime *fakeNormalVisualRuntime) Stop(context.Context) error {
	runtime.stops.Add(1)
	return nil
}

func (runtime *fakeNormalVisualRuntime) ClearDashboardReadiness() error {
	runtime.readiness.Add(1)
	return nil
}

func (runtime *fakeNormalVisualRuntime) ReleaseOnProcessExit() {
	runtime.releases.Add(1)
}

func (runtime *fakeNormalVisualRuntime) BeadDirs() []string {
	return []string{"/data/beads/quality", "/data/beads/security"}
}

type testNormalVisualContract struct {
	mu     sync.Mutex
	config integrated.Config
	exists bool
	err    error
}

func (contract *testNormalVisualContract) load(*config.Config) (integrated.Config, bool, error) {
	contract.mu.Lock()
	defer contract.mu.Unlock()
	return contract.config, contract.exists, contract.err
}

func (contract *testNormalVisualContract) set(config integrated.Config, exists bool, err error) {
	contract.mu.Lock()
	contract.config, contract.exists, contract.err = config, exists, err
	contract.mu.Unlock()
}

func testInstalledNormalVisualContract(t *testing.T, stateDir string) integrated.Config {
	t.Helper()
	checkoutDir := t.TempDir()
	return integrated.Config{
		Repository: "owner/repository", RepositoryID: "123", DefaultBranch: "main",
		Automation: integrated.AutomationRepairPR, ExecutionMode: integrated.ExecutionLocal,
		ProviderCommand: "codex", ProviderArgs: []string{"exec"},
		RunIntervalSeconds: 60, VisualHive: true, VisualHiveRef: strings.Repeat("a", 40),
		VisualHiveCommand: "node", VisualHiveArgs: []string{"visual-hive.js"},
		StateDir: stateDir, CheckoutDir: checkoutDir,
	}
}

func newTestNormalVisualRuntimeManager(
	t *testing.T,
	contract *testNormalVisualContract,
	factory func(normalVisualRuntimeBinding) *fakeNormalVisualRuntime,
) (*normalVisualRuntimeManager, *atomic.Int32, *[]*fakeNormalVisualRuntime) {
	t.Helper()
	var factoryCalls atomic.Int32
	var instancesMu sync.Mutex
	instances := []*fakeNormalVisualRuntime{}
	manager := &normalVisualRuntimeManager{
		process: context.Background(), normal: &config.Config{}, load: contract.load,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), beadDirs: []string{"/data/beads/ordinary"},
	}
	manager.factory = func(installed integrated.Config) (normalVisualRuntimeInstance, error) {
		factoryCalls.Add(1)
		binding, err := normalVisualBinding(installed)
		if err != nil {
			return nil, err
		}
		instance := factory(binding)
		instancesMu.Lock()
		instances = append(instances, instance)
		instancesMu.Unlock()
		return instance, nil
	}
	return manager, &factoryCalls, &instances
}

func TestNormalVisualRuntimeManagerHotActivatesExactlyOnce(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{}
	manager, factoryCalls, instances := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || active {
		t.Fatalf("dormant ensure active=%t err=%v", active, err)
	}
	contract.set(testInstalledNormalVisualContract(t, stateDir), true, nil)

	const callers = 32
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			active, err := manager.Ensure(context.Background())
			if err != nil {
				errs <- err
			} else if !active {
				errs <- errors.New("runtime did not become active")
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if factoryCalls.Load() != 1 || len(*instances) != 1 || (*instances)[0].starts.Load() != 1 {
		t.Fatalf("factory=%d instances=%d starts=%d", factoryCalls.Load(), len(*instances), (*instances)[0].starts.Load())
	}
	if err := manager.Trigger(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (*instances)[0].triggers.Load() != 1 {
		t.Fatalf("triggers=%d", (*instances)[0].triggers.Load())
	}
}

func TestNormalVisualRuntimeManagerFailsClosedOnBindingDrift(t *testing.T) {
	stateDir := t.TempDir()
	installed := testInstalledNormalVisualContract(t, stateDir)
	contract := &testNormalVisualContract{config: installed, exists: true}
	manager, factoryCalls, instances := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || !active {
		t.Fatalf("initial ensure active=%t err=%v", active, err)
	}
	installed.VisualHiveRef = strings.Repeat("b", 40)
	contract.set(installed, true, nil)
	if active, err := manager.Ensure(context.Background()); err == nil || active || !strings.Contains(err.Error(), "managed stop/rebind") {
		t.Fatalf("drift ensure active=%t err=%v", active, err)
	}
	if factoryCalls.Load() != 1 || len(*instances) != 1 {
		t.Fatalf("factory=%d instances=%d", factoryCalls.Load(), len(*instances))
	}
}

func TestNormalVisualRuntimeBindingIgnoresLifecycleOnlyChanges(t *testing.T) {
	installed := testInstalledNormalVisualContract(t, t.TempDir())
	first, err := normalVisualBinding(installed)
	if err != nil {
		t.Fatal(err)
	}
	installed.Paused = true
	installed.UpdatedAt = time.Now().UTC()
	installed.SetupPRNumber = 42
	second, err := normalVisualBinding(installed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("lifecycle-only state changed runtime binding: first=%s second=%s", first.Digest, second.Digest)
	}
	installed.TestCommands = [][]string{{"go", "test", "./..."}}
	third, err := normalVisualBinding(installed)
	if err != nil {
		t.Fatal(err)
	}
	if third.Digest == second.Digest {
		t.Fatal("static runtime command change did not change the binding")
	}
}

func TestNormalVisualRuntimeManagerStopAllowsManagedReinstall(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	manager, factoryCalls, instances := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || !active {
		t.Fatalf("initial ensure active=%t err=%v", active, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, err := manager.ResumeReconciliation(context.Background()); err != nil || !active {
		t.Fatalf("reinstall ensure active=%t err=%v", active, err)
	}
	if factoryCalls.Load() != 2 || len(*instances) != 2 ||
		(*instances)[0].stops.Load() != 1 || (*instances)[1].starts.Load() != 1 {
		t.Fatalf("factory=%d instances=%d first_stops=%d second_starts=%d",
			factoryCalls.Load(), len(*instances), (*instances)[0].stops.Load(), (*instances)[1].starts.Load())
	}
}

func TestNormalVisualRuntimeManagerStopSuppressesObserverUntilManagedResume(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	manager, factoryCalls, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || !active {
		t.Fatalf("initial ensure active=%t err=%v", active, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go manager.Observe(ctx, time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	cancel()
	if factoryCalls.Load() != 1 {
		t.Fatalf("observer restarted a lifecycle-suppressed runtime: factory=%d", factoryCalls.Load())
	}
	if active, err := manager.ResumeReconciliation(context.Background()); err != nil || !active {
		t.Fatalf("managed resume active=%t err=%v", active, err)
	}
	if factoryCalls.Load() != 2 {
		t.Fatalf("managed resume factory=%d", factoryCalls.Load())
	}
}

func TestNormalVisualRuntimeManagerActivationFailureLeavesNoActiveRuntime(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	manager, factoryCalls, instances := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding, startErr: errors.New("readiness unavailable")}
	})
	if active, err := manager.Ensure(context.Background()); err == nil || active {
		t.Fatalf("failed ensure active=%t err=%v", active, err)
	}
	if manager.active != nil || factoryCalls.Load() != 1 || (*instances)[0].releases.Load() != 1 {
		t.Fatalf("active=%v factory=%d releases=%d", manager.active, factoryCalls.Load(), (*instances)[0].releases.Load())
	}
}

func TestNormalVisualRuntimeManagerObserverActivatesLateContract(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{}
	manager, factoryCalls, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.Observe(ctx, time.Millisecond)
	contract.set(testInstalledNormalVisualContract(t, stateDir), true, nil)
	deadline := time.Now().Add(2 * time.Second)
	for factoryCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if factoryCalls.Load() != 1 {
		t.Fatalf("observer factory calls=%d", factoryCalls.Load())
	}
}

func TestDashboardTriggerLazilyReconcilesNormalVisualRuntime(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	manager, factoryCalls, instances := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	original := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(manager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(original) })
	if err := triggerDashboardVisualCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if factoryCalls.Load() != 1 || len(*instances) != 1 ||
		(*instances)[0].starts.Load() != 1 || (*instances)[0].triggers.Load() != 1 {
		t.Fatalf("factory=%d instances=%d starts=%d triggers=%d",
			factoryCalls.Load(), len(*instances), (*instances)[0].starts.Load(), (*instances)[0].triggers.Load())
	}
}

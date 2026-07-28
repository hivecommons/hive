package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

var (
	dashboardNormalVisualRuntime         atomic.Pointer[normalVisualRuntimeManager]
	normalVisualRuntimeReconcileInterval = 5 * time.Second
)

type normalVisualRuntimeBinding struct {
	Digest     string
	Repository string
	StateDir   string
}

type normalVisualRuntimeInstance interface {
	Binding() normalVisualRuntimeBinding
	Start(context.Context) error
	Trigger(context.Context) error
	Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error)
	Stop(context.Context) error
	ClearDashboardReadiness() error
	ReleaseOnProcessExit()
	BeadDirs() []string
}

type normalVisualRuntimeFactory func(integrated.Config) (normalVisualRuntimeInstance, error)

// normalVisualRuntimeManager is the single in-process owner of the additive
// Visual Hive controller. It observes only Hive's authoritative installed
// contract and never creates a second ordinary Manager or scheduler.
type normalVisualRuntimeManager struct {
	mu         sync.Mutex
	process    context.Context
	normal     *config.Config
	load       func(*config.Config) (integrated.Config, bool, error)
	factory    normalVisualRuntimeFactory
	logger     *slog.Logger
	active     normalVisualRuntimeInstance
	beadDirs   []string
	suppressed bool
}

type normalVisualRuntimeDependencies struct {
	ProcessContext    context.Context
	NormalConfig      *config.Config
	Governor          *governor.Governor
	BeadStores        map[string]*beads.Store
	LifecycleBeadDirs map[string]string
	BeadsRoot         string
	HiveID            string
	AgentManager      *agent.Manager
	GitHub            *hivegithub.Client
	Scheduler         *scheduler.Scheduler
	GitTransportToken func(context.Context) (string, error)
	Dashboard         *dashboard.Server
	Logger            *slog.Logger
}

func newNormalVisualRuntimeManager(deps normalVisualRuntimeDependencies) (*normalVisualRuntimeManager, error) {
	if deps.ProcessContext == nil || deps.NormalConfig == nil || deps.Governor == nil ||
		deps.AgentManager == nil || deps.GitHub == nil || deps.Scheduler == nil ||
		deps.GitTransportToken == nil || deps.Dashboard == nil {
		return nil, errors.New("normal Visual Hive runtime manager dependencies are incomplete")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	baseDirs := sortedNormalVisualBeadDirs(deps.LifecycleBeadDirs)
	manager := &normalVisualRuntimeManager{
		process:  deps.ProcessContext,
		normal:   deps.NormalConfig,
		load:     loadCurrentVisualWorkContract,
		logger:   deps.Logger,
		beadDirs: baseDirs,
	}
	manager.factory = func(installed integrated.Config) (normalVisualRuntimeInstance, error) {
		return buildNormalVisualRuntime(deps, installed)
	}
	return manager, nil
}

// Ensure activates the exact authoritative contract once. Repeated calls for
// the same binding are no-ops; a changed live binding fails closed and must use
// the supported stop/rebind lifecycle.
func (manager *normalVisualRuntimeManager) Ensure(_ context.Context) (bool, error) {
	if manager == nil {
		return false, errors.New("normal Visual Hive runtime manager is unavailable")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.suppressed {
		return false, nil
	}

	installed, exists, err := manager.load(manager.normal)
	if err != nil {
		return false, err
	}
	if !exists {
		if manager.active != nil {
			return false, errors.New("authoritative Visual Hive contract disappeared while its runtime is active; use supported uninstall")
		}
		return false, nil
	}
	binding, err := normalVisualBinding(installed)
	if err != nil {
		return false, err
	}
	if manager.active != nil {
		current := manager.active.Binding()
		if current.Digest == binding.Digest {
			return true, nil
		}
		return false, fmt.Errorf(
			"installed Visual Hive contract changed from %s to %s while the runtime is active; use the managed stop/rebind lifecycle",
			current.Repository, binding.Repository,
		)
	}

	instance, err := manager.factory(installed)
	if err != nil {
		return false, err
	}
	if instance == nil {
		return false, errors.New("normal Visual Hive runtime factory returned no instance")
	}
	if instance.Binding().Digest != binding.Digest {
		instance.ReleaseOnProcessExit()
		return false, errors.New("normal Visual Hive runtime factory returned a mismatched contract binding")
	}
	if err := instance.Start(manager.process); err != nil {
		instance.ReleaseOnProcessExit()
		return false, err
	}
	manager.active = instance
	manager.beadDirs = instance.BeadDirs()
	manager.logger.Info("normal Visual Hive runtime reconciled in-process",
		"repository", binding.Repository,
		"binding", binding.Digest,
	)
	return true, nil
}

func (manager *normalVisualRuntimeManager) Trigger(ctx context.Context) error {
	active, err := manager.Ensure(ctx)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	manager.mu.Lock()
	instance := manager.active
	manager.mu.Unlock()
	if instance == nil {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return instance.Trigger(ctx)
}

func (manager *normalVisualRuntimeManager) Import(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	active, err := manager.Ensure(ctx)
	if err != nil {
		return visualcontroller.Result{}, err
	}
	if !active {
		return visualcontroller.Result{}, errors.New("normal Visual Hive work service is not configured")
	}
	manager.mu.Lock()
	instance := manager.active
	manager.mu.Unlock()
	if instance == nil {
		return visualcontroller.Result{}, errors.New("normal Visual Hive work service is not configured")
	}
	return instance.Import(ctx, source)
}

func (manager *normalVisualRuntimeManager) Stop(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.suppressed = true
	if manager.active == nil {
		return nil
	}
	if err := manager.active.Stop(ctx); err != nil {
		return err
	}
	manager.active = nil
	return nil
}

// ResumeReconciliation is used only after an exact managed setup/cancel or a
// failed uninstall mutation. It prevents the background observer from
// recreating a runtime while uninstall is between its stop and durable CLI
// transaction.
func (manager *normalVisualRuntimeManager) ResumeReconciliation(ctx context.Context) (bool, error) {
	if manager == nil {
		return false, errors.New("normal Visual Hive runtime manager is unavailable")
	}
	manager.mu.Lock()
	manager.suppressed = false
	manager.mu.Unlock()
	return manager.Ensure(ctx)
}

func (manager *normalVisualRuntimeManager) ClearDashboardReadiness() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active == nil {
		return nil
	}
	return manager.active.ClearDashboardReadiness()
}

func (manager *normalVisualRuntimeManager) ReleaseOnProcessExit() {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil {
		manager.active.ReleaseOnProcessExit()
		manager.active = nil
	}
}

func (manager *normalVisualRuntimeManager) BeadDirs() []string {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.beadDirs...)
}

func (manager *normalVisualRuntimeManager) Observe(ctx context.Context, interval time.Duration) {
	if manager == nil {
		return
	}
	if interval <= 0 {
		interval = normalVisualRuntimeReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastFailure := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := manager.Ensure(ctx)
			failure := ""
			if err != nil {
				sum := sha256.Sum256([]byte(err.Error()))
				failure = hex.EncodeToString(sum[:])
			}
			if failure == lastFailure {
				continue
			}
			lastFailure = failure
			if err != nil {
				manager.logger.Warn("normal Visual Hive runtime reconciliation is pending", "error", err)
			} else {
				manager.logger.Info("normal Visual Hive runtime reconciliation recovered")
			}
		}
	}
}

type concreteNormalVisualRuntime struct {
	mu         sync.Mutex
	binding    normalVisualRuntimeBinding
	controller *visualcontroller.Controller
	runner     *normalservice.Service
	health     *normalVisualServiceHealthReporter
	readiness  *normalVisualDashboardGate
	ownership  *os.File
	manager    *agent.Manager
	logger     *slog.Logger
	beadDirs   []string
	stopper    *normalVisualRuntimeStopper
	started    bool
}

func buildNormalVisualRuntime(deps normalVisualRuntimeDependencies, installed integrated.Config) (normalVisualRuntimeInstance, error) {
	binding, err := normalVisualBinding(installed)
	if err != nil {
		return nil, err
	}
	stores := cloneNormalVisualBeadStores(deps.BeadStores)
	dirs := cloneNormalVisualBeadDirs(deps.LifecycleBeadDirs)
	initialized, err := ensureVisualSpecialistBeadStores(stores, dirs, deps.BeadsRoot, deps.HiveID)
	if err != nil {
		return nil, fmt.Errorf("initialize fixed Visual Hive specialist bead stores: %w", err)
	}
	for _, role := range initialized {
		deps.Logger.Info("Visual Hive specialist beads store initialized", "agent", role, "count", stores[role].Count())
	}
	lifecycle, err := visualLifecycleForInstalledContract(installed)
	if err != nil {
		return nil, fmt.Errorf("initialize normal Visual Hive lifecycle: %w", err)
	}
	controller, err := visualcontroller.New(
		deps.Governor, lifecycle, stores, deps.AgentManager, deps.GitHub,
		installed, inferACMMLevel(deps.NormalConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize normal Visual Hive intake: %w", err)
	}
	controller.SetAuditSink(dashboardVisualWorkAudit{server: deps.Dashboard})
	controller.SetRuntimeConfigLoader(func() (integrated.Config, int, error) {
		current, exists, loadErr := loadAuthoritativeVisualWorkContract()
		if loadErr != nil {
			return integrated.Config{}, 0, loadErr
		}
		if !exists {
			return integrated.Config{}, 0, errors.New("authoritative installed Visual Hive contract is unavailable")
		}
		return current, deps.AgentManager.GetACMMLevel(), nil
	})
	instance := &concreteNormalVisualRuntime{
		binding:    binding,
		controller: controller,
		manager:    deps.AgentManager,
		logger:     deps.Logger,
		beadDirs:   sortedNormalVisualBeadDirs(dirs),
	}
	if configuredRuntimeOwnerIntent(installed) != runtimeOwnerNormalHive {
		deps.Logger.Info("normal Visual Hive intake does not own repair polling for the current installed config",
			"repository", installed.Repository,
			"runtime_owner", configuredRuntimeOwnerIntent(installed),
		)
		return instance, nil
	}

	readiness, err := newNormalVisualDashboardGate(deps.Dashboard, installed.StateDir, installed.Repository)
	if err != nil {
		return nil, err
	}
	ownership, err := claimNormalVisualWorkOwnership(installed.StateDir, deps.AgentManager, func(ordinaryManager *agent.Manager) (bool, error) {
		runner, health, configureErr := configureNormalVisualWorkRunner(
			installed, controller, lifecycle, deps.Scheduler, ordinaryManager,
			deps.GitHub, deps.GitTransportToken, readiness.Ready, deps.Logger,
		)
		if configureErr != nil || runner == nil {
			return false, configureErr
		}
		instance.runner = runner
		instance.health = health
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if ownership == nil {
		return nil, errors.New("normal Visual Hive runtime did not acquire ordinary-Manager ownership")
	}
	instance.readiness = readiness
	instance.ownership = ownership
	return instance, nil
}

func (runtime *concreteNormalVisualRuntime) Binding() normalVisualRuntimeBinding {
	if runtime == nil {
		return normalVisualRuntimeBinding{}
	}
	return runtime.binding
}

func (runtime *concreteNormalVisualRuntime) Start(parent context.Context) error {
	if runtime == nil {
		return errors.New("normal Visual Hive runtime is unavailable")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.started {
		return nil
	}
	if runtime.runner == nil {
		runtime.started = true
		return nil
	}
	if parent == nil {
		return errors.New("normal Visual Hive runtime requires the ordinary Hive process context")
	}
	if runtime.readiness == nil || runtime.health == nil || runtime.ownership == nil {
		return errors.New("normal Visual Hive runtime start bindings are incomplete")
	}
	if err := runtime.readiness.Activate(time.Now().UTC()); err != nil {
		runtime.cleanupUnstarted()
		return fmt.Errorf("activate normal Visual Hive dashboard readiness: %w", err)
	}
	if err := runtime.health.Initialize(); err != nil {
		runtime.cleanupUnstarted()
		return fmt.Errorf("initialize normal Visual Hive health: %w", err)
	}
	runnerContext, cancelRunner := context.WithCancel(parent)
	runnerDone := make(chan struct{})
	runtime.stopper = &normalVisualRuntimeStopper{
		cancel:  cancelRunner,
		done:    runnerDone,
		manager: runtime.manager,
		resetSpecialists: func() error {
			return runtime.manager.ResetSpecialistChildDispatcher(filepath.Join(runtime.binding.StateDir, "repair"))
		},
		logger: runtime.logger,
		releaseOwnership: func() {
			if runtime.readiness != nil {
				if err := runtime.readiness.Stop(); err != nil {
					runtime.logger.Warn("remove normal Visual Hive dashboard readiness during uninstall", "error", err)
				}
			}
			releaseDaemonLease(runtime.ownership)
			runtime.ownership = nil
		},
	}
	runner := runtime.runner
	go func() {
		defer close(runnerDone)
		runner.Run(runnerContext)
	}()
	runtime.started = true
	return nil
}

func (runtime *concreteNormalVisualRuntime) Trigger(ctx context.Context) error {
	runtime.mu.Lock()
	runner, started := runtime.runner, runtime.started
	runtime.mu.Unlock()
	if !started || runner == nil {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return runner.Trigger(ctx)
}

func (runtime *concreteNormalVisualRuntime) Import(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	if runtime == nil || runtime.controller == nil {
		return visualcontroller.Result{}, errors.New("normal Visual Hive work service is not configured")
	}
	return runtime.controller.Import(ctx, source)
}

func (runtime *concreteNormalVisualRuntime) Stop(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	runtime.mu.Lock()
	stopper, runner := runtime.stopper, runtime.runner
	runtime.mu.Unlock()
	if runner != nil {
		if stopper == nil {
			return errors.New("normal Visual Hive runtime stop bindings are incomplete")
		}
		if err := stopper.Stop(ctx); err != nil {
			return err
		}
	}
	runtime.mu.Lock()
	runtime.started = false
	runtime.stopper = nil
	runtime.runner = nil
	runtime.controller = nil
	runtime.mu.Unlock()
	return nil
}

func (runtime *concreteNormalVisualRuntime) ClearDashboardReadiness() error {
	if runtime == nil || runtime.readiness == nil {
		return nil
	}
	return runtime.readiness.Stop()
}

func (runtime *concreteNormalVisualRuntime) ReleaseOnProcessExit() {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.stopper != nil && runtime.stopper.cancel != nil {
		runtime.stopper.cancel()
	}
	if runtime.readiness != nil {
		if err := runtime.readiness.Stop(); err != nil && runtime.logger != nil {
			runtime.logger.Warn("remove normal Visual Hive dashboard readiness on process exit", "error", err)
		}
	}
	releaseDaemonLease(runtime.ownership)
	runtime.ownership = nil
	runtime.started = false
}

func (runtime *concreteNormalVisualRuntime) BeadDirs() []string {
	if runtime == nil {
		return nil
	}
	return append([]string(nil), runtime.beadDirs...)
}

func (runtime *concreteNormalVisualRuntime) cleanupUnstarted() {
	if runtime.readiness != nil {
		_ = runtime.readiness.Stop()
	}
	if runtime.manager != nil {
		_ = runtime.manager.ResetSpecialistChildDispatcher(filepath.Join(runtime.binding.StateDir, "repair"))
	}
	releaseDaemonLease(runtime.ownership)
	runtime.ownership = nil
}

func normalVisualBinding(installed integrated.Config) (normalVisualRuntimeBinding, error) {
	stateDir, err := canonicalNormalVisualPath(installed.StateDir)
	if err != nil {
		return normalVisualRuntimeBinding{}, fmt.Errorf("bind normal Visual Hive state directory: %w", err)
	}
	checkoutDir, err := canonicalNormalVisualPath(installed.CheckoutDir)
	if err != nil {
		return normalVisualRuntimeBinding{}, fmt.Errorf("bind normal Visual Hive checkout directory: %w", err)
	}
	repository := strings.ToLower(strings.TrimSpace(installed.Repository))
	if repository == "" {
		return normalVisualRuntimeBinding{}, errors.New("bind normal Visual Hive runtime: repository is empty")
	}
	binding := struct {
		Repository         string                   `json:"repository"`
		RepositoryID       string                   `json:"repository_id"`
		DefaultBranch      string                   `json:"default_branch"`
		StateDir           string                   `json:"state_dir"`
		CheckoutDir        string                   `json:"checkout_dir"`
		RuntimeOwner       runtimeOwnerIntent       `json:"runtime_owner"`
		Coverage           integrated.Coverage      `json:"coverage"`
		Automation         integrated.Automation    `json:"automation"`
		ExecutionMode      integrated.ExecutionMode `json:"execution_mode"`
		ACMMLevel          int                      `json:"acmm_level"`
		ProviderCommand    string                   `json:"provider_command"`
		ProviderArgs       []string                 `json:"provider_args"`
		RunIntervalSeconds int64                    `json:"run_interval_seconds"`
		VisualHiveRef      string                   `json:"visual_hive_ref"`
		VisualHiveCommand  string                   `json:"visual_hive_command"`
		VisualHiveArgs     []string                 `json:"visual_hive_args"`
		VisualConfigDigest string                   `json:"visual_config_digest"`
		TestCommands       [][]string               `json:"test_commands"`
		AllowedRepairPaths []string                 `json:"allowed_repair_paths"`
	}{
		Repository: repository, RepositoryID: strings.TrimSpace(installed.RepositoryID),
		DefaultBranch: strings.TrimSpace(installed.DefaultBranch), StateDir: stateDir, CheckoutDir: checkoutDir,
		RuntimeOwner: configuredRuntimeOwnerIntent(installed), Coverage: installed.Coverage,
		Automation: installed.Automation, ExecutionMode: installed.ExecutionMode, ACMMLevel: installed.ACMMLevel,
		ProviderCommand: strings.TrimSpace(installed.ProviderCommand), ProviderArgs: append([]string(nil), installed.ProviderArgs...),
		RunIntervalSeconds: installed.RunIntervalSeconds, VisualHiveRef: strings.TrimSpace(installed.VisualHiveRef),
		VisualHiveCommand: strings.TrimSpace(installed.VisualHiveCommand), VisualHiveArgs: append([]string(nil), installed.VisualHiveArgs...),
		VisualConfigDigest: strings.TrimSpace(installed.VisualHiveConfigDigest),
		TestCommands:       cloneNormalVisualCommands(installed.TestCommands),
		AllowedRepairPaths: append([]string(nil), installed.AllowedRepairPaths...),
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return normalVisualRuntimeBinding{}, err
	}
	sum := sha256.Sum256(data)
	return normalVisualRuntimeBinding{
		Digest: hex.EncodeToString(sum[:]), Repository: repository, StateDir: stateDir,
	}, nil
}

func cloneNormalVisualCommands(source [][]string) [][]string {
	result := make([][]string, len(source))
	for index, command := range source {
		result[index] = append([]string(nil), command...)
	}
	return result
}

func canonicalNormalVisualPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
}

func cloneNormalVisualBeadStores(source map[string]*beads.Store) map[string]*beads.Store {
	result := make(map[string]*beads.Store, len(source))
	for role, store := range source {
		result[role] = store
	}
	return result
}

func cloneNormalVisualBeadDirs(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for role, dir := range source {
		result[role] = dir
	}
	return result
}

func sortedNormalVisualBeadDirs(source map[string]string) []string {
	dirs := make([]string, 0, len(source))
	for _, dir := range source {
		if trimmed := strings.TrimSpace(dir); trimmed != "" {
			dirs = append(dirs, trimmed)
		}
	}
	sort.Strings(dirs)
	return dirs
}

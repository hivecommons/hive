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
	PlanRepairRetirement(context.Context) (normalservice.RepairRetirementPlan, error)
	RetireRepair(context.Context, *normalservice.RepairRetirementPlan) error
	Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error)
	Stop(context.Context) error
	ClearDashboardReadiness() error
	ReleaseOnProcessExit()
	BeadDirs() []string
}

type normalVisualRuntimeFactory func(integrated.Config, liveGitHubRuntimeSnapshot) (normalVisualRuntimeInstance, error)

// normalVisualRuntimeManager is the single in-process owner of the additive
// Visual Hive controller. It observes only Hive's authoritative installed
// contract and never creates a second ordinary Manager or scheduler.
type normalVisualRuntimeManager struct {
	mu                   sync.Mutex
	process              context.Context
	normal               *config.Config
	load                 func(*config.Config) (integrated.Config, bool, error)
	factory              normalVisualRuntimeFactory
	githubRuntime        func() (liveGitHubRuntimeSnapshot, bool)
	logger               *slog.Logger
	active               normalVisualRuntimeInstance
	activeGitHubBinding  string
	activeGitHubRevision uint64
	beadDirs             []string
	suppressed           bool
	rebindRequired       bool
	lastActivationError  string
	lastReconciledAt     time.Time
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
	Scheduler         *scheduler.Scheduler
	GitHubRuntime     func() (liveGitHubRuntimeSnapshot, bool)
	Dashboard         *dashboard.Server
	Logger            *slog.Logger
}

func newNormalVisualRuntimeManager(deps normalVisualRuntimeDependencies) (*normalVisualRuntimeManager, error) {
	if deps.ProcessContext == nil || deps.NormalConfig == nil || deps.Governor == nil ||
		deps.AgentManager == nil || deps.Scheduler == nil || deps.GitHubRuntime == nil || deps.Dashboard == nil {
		return nil, errors.New("normal Visual Hive runtime manager dependencies are incomplete")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	baseDirs := sortedNormalVisualBeadDirs(deps.LifecycleBeadDirs)
	manager := &normalVisualRuntimeManager{
		process:       deps.ProcessContext,
		normal:        deps.NormalConfig,
		load:          loadCurrentVisualWorkContract,
		githubRuntime: deps.GitHubRuntime,
		logger:        deps.Logger,
		beadDirs:      baseDirs,
	}
	manager.factory = func(installed integrated.Config, githubRuntime liveGitHubRuntimeSnapshot) (normalVisualRuntimeInstance, error) {
		return buildNormalVisualRuntime(deps, installed, githubRuntime)
	}
	return manager, nil
}

// Ensure activates the exact authoritative contract once. Repeated calls for
// the same binding are no-ops; a changed live binding fails closed and must use
// the supported stop/rebind lifecycle.
func (manager *normalVisualRuntimeManager) Ensure(_ context.Context) (active bool, err error) {
	if manager == nil {
		return false, errors.New("normal Visual Hive runtime manager is unavailable")
	}
	defer func() { manager.recordActivationResult(active, err) }()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.suppressed {
		return false, nil
	}
	if manager.rebindRequired {
		return false, errors.New("Visual Hive runtime is held after contract or writer drift; complete the supported managed rebind lifecycle")
	}

	installed, exists, err := manager.load(manager.normal)
	if err != nil {
		return false, err
	}
	if !exists {
		if manager.active != nil {
			if stopErr := manager.stopActiveLocked(manager.process); stopErr != nil {
				return false, fmt.Errorf("stop Visual Hive after its authoritative contract disappeared: %w", stopErr)
			}
			manager.rebindRequired = true
			return false, errors.New("authoritative Visual Hive contract disappeared while its runtime was active; use supported uninstall or managed rebind")
		}
		return false, nil
	}
	binding, err := normalVisualBinding(installed)
	if err != nil {
		return false, err
	}
	githubRuntime, available := manager.githubRuntime()
	if !available {
		if manager.active != nil {
			if err := manager.active.Stop(manager.process); err != nil {
				return false, fmt.Errorf("stop Visual Hive after GitHub runtime loss: %w", err)
			}
			manager.active = nil
			manager.activeGitHubBinding = ""
			manager.activeGitHubRevision = 0
		}
		return false, errors.New("Visual Hive contract is installed, but the live GitHub runtime is unavailable")
	}
	if githubRuntime.Mode == "app" {
		if err := githubRuntime.App.RequireVisualHivePermissions(); err != nil {
			if manager.active != nil {
				if stopErr := manager.active.Stop(manager.process); stopErr != nil {
					return false, fmt.Errorf("stop Visual Hive after GitHub App permission loss: %w", stopErr)
				}
				manager.active = nil
				manager.activeGitHubBinding = ""
				manager.activeGitHubRevision = 0
			}
			return false, fmt.Errorf("Visual Hive GitHub App runtime is not authorized: %w", err)
		}
	}
	if err := validateInstalledGitHubRuntimeBinding(installed, githubRuntime); err != nil {
		if manager.active != nil {
			if stopErr := manager.stopActiveLocked(manager.process); stopErr != nil {
				return false, fmt.Errorf("stop Visual Hive after installed GitHub writer drift: %w", stopErr)
			}
			manager.rebindRequired = true
		}
		return false, err
	}
	if !strings.EqualFold(githubRuntime.Repository, installed.Repository) || fmt.Sprintf("%d", githubRuntime.RepositoryID) != strings.TrimSpace(installed.RepositoryID) {
		if manager.active != nil {
			if stopErr := manager.stopActiveLocked(manager.process); stopErr != nil {
				return false, fmt.Errorf("stop Visual Hive after repository binding drift: %w", stopErr)
			}
			manager.rebindRequired = true
		}
		return false, errors.New("live GitHub runtime is bound to a different repository; use managed rebind")
	}
	if manager.active != nil {
		current := manager.active.Binding()
		if current.Digest != binding.Digest {
			if stopErr := manager.stopActiveLocked(manager.process); stopErr != nil {
				return false, fmt.Errorf("stop Visual Hive after installed contract drift: %w", stopErr)
			}
			manager.rebindRequired = true
			return false, fmt.Errorf(
				"installed Visual Hive contract changed from %s to %s while the runtime is active; use the managed stop/rebind lifecycle",
				current.Repository, binding.Repository,
			)
		}
		if manager.activeGitHubBinding != githubRuntime.BindingDigest {
			if stopErr := manager.stopActiveLocked(manager.process); stopErr != nil {
				return false, fmt.Errorf("stop Visual Hive after live GitHub binding drift: %w", stopErr)
			}
			manager.rebindRequired = true
			return false, errors.New("live GitHub App, installation, repository, or writer identity changed while Visual Hive is active; use managed rebind")
		}
		if manager.activeGitHubRevision == githubRuntime.Revision {
			return true, nil
		}
		if err := manager.active.Stop(manager.process); err != nil {
			return false, fmt.Errorf("rotate live GitHub runtime: %w", err)
		}
		manager.active = nil
	}

	instance, err := manager.factory(installed, githubRuntime)
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
	manager.activeGitHubBinding = githubRuntime.BindingDigest
	manager.activeGitHubRevision = githubRuntime.Revision
	manager.beadDirs = instance.BeadDirs()
	manager.logger.Info("normal Visual Hive runtime reconciled in-process",
		"repository", binding.Repository,
		"binding", binding.Digest,
	)
	return true, nil
}

// validateInstalledGitHubRuntimeBinding prevents a pod replacement from
// silently adopting a different App, installation, or writer. In-memory
// activeGitHubBinding excludes hot swaps during one process; this durable
// contract comparison provides the same exclusion after a restart.
func validateInstalledGitHubRuntimeBinding(installed integrated.Config, githubRuntime liveGitHubRuntimeSnapshot) error {
	mode := strings.ToLower(strings.TrimSpace(githubRuntime.Mode))
	switch mode {
	case "app":
		if installed.SetupAuthorizationAppID <= 0 || installed.SetupAuthorizationInstallationID <= 0 ||
			installed.SetupAuthorizationWriterID <= 0 || strings.TrimSpace(installed.SetupAuthorizationWriterLogin) == "" ||
			!strings.EqualFold(installed.SetupAuthorizationWriterType, "Bot") ||
			strings.TrimSpace(installed.SetupAuthorizationAppBindingDigest) == "" {
			return errors.New("installed contract has no exact GitHub App writer binding; use managed upgrade/rebind")
		}
		if installed.SetupAuthorizationAppID != githubRuntime.App.AppID ||
			installed.SetupAuthorizationInstallationID != githubRuntime.App.InstallationID ||
			installed.SetupAuthorizationWriterID != githubRuntime.Writer.ID ||
			!strings.EqualFold(installed.SetupAuthorizationWriterLogin, githubRuntime.Writer.Login) ||
			!strings.EqualFold(installed.SetupAuthorizationWriterType, githubRuntime.Writer.Type) ||
			!strings.EqualFold(installed.SetupAuthorizationAppBindingDigest, githubRuntime.App.BindingDigest) {
			return errors.New("live GitHub App, installation, or writer does not match the installed contract; use managed rebind")
		}
	case "pat":
		if installed.SetupAuthorizationAppID != 0 || installed.SetupAuthorizationInstallationID != 0 {
			return errors.New("installed App-backed contract cannot run with a PAT; use managed rebind")
		}
		expectedID := installed.SetupAuthorizationWriterID
		expectedLogin := strings.TrimSpace(installed.SetupAuthorizationWriterLogin)
		if expectedID <= 0 {
			expectedID = installed.SetupAuthorizationActorID
			expectedLogin = strings.TrimSpace(installed.SetupAuthorizationActorLogin)
		}
		if expectedID <= 0 || githubRuntime.Writer.ID != expectedID || !strings.EqualFold(githubRuntime.Writer.Type, "User") ||
			(expectedLogin != "" && !strings.EqualFold(expectedLogin, githubRuntime.Writer.Login)) {
			return errors.New("live GitHub PAT writer does not match the installed human writer")
		}
	default:
		return fmt.Errorf("live GitHub runtime mode %q is invalid", githubRuntime.Mode)
	}
	return nil
}

func (manager *normalVisualRuntimeManager) recordActivationResult(active bool, err error) {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.lastReconciledAt = time.Now().UTC()
	manager.lastActivationError = ""
	if err != nil {
		digest := sha256.Sum256([]byte(err.Error()))
		manager.lastActivationError = hex.EncodeToString(digest[:])
	}
}

func (manager *normalVisualRuntimeManager) RuntimeStatus() map[string]any {
	status := map[string]any{"state": "unavailable", "active": false}
	if manager == nil {
		return status
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	status["state"] = "dormant"
	status["suppressed"] = manager.suppressed
	if manager.suppressed {
		status["state"] = "suppressed"
	}
	if !manager.lastReconciledAt.IsZero() {
		status["last_reconciled_at"] = manager.lastReconciledAt
	}
	if manager.lastActivationError != "" {
		status["activation_error_reference"] = manager.lastActivationError
		status["state"] = "pending"
	}
	if manager.rebindRequired {
		status["state"] = "rebind_required"
		status["rebind_required"] = true
	}
	if manager.active != nil {
		binding := manager.active.Binding()
		status["state"] = "ready"
		status["active"] = true
		status["contract_binding_digest"] = binding.Digest
		status["repository"] = binding.Repository
		status["state_dir"] = binding.StateDir
		status["github_runtime_binding_digest"] = manager.activeGitHubBinding
		status["github_runtime_revision"] = manager.activeGitHubRevision
	}
	return status
}

// ActiveBinding reports the one in-process Visual Hive runtime without
// attempting reconciliation. Hosted preflight uses this read-only view to
// prove that setup will not overlap an already-owned controller.
func (manager *normalVisualRuntimeManager) ActiveBinding() (normalVisualRuntimeBinding, bool) {
	if manager == nil {
		return normalVisualRuntimeBinding{}, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active == nil {
		return normalVisualRuntimeBinding{}, false
	}
	return manager.active.Binding(), true
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

func (manager *normalVisualRuntimeManager) PlanRepairRetirement(ctx context.Context) (normalservice.RepairRetirementPlan, error) {
	active, err := manager.Ensure(ctx)
	if err != nil {
		return normalservice.RepairRetirementPlan{}, err
	}
	if !active {
		return normalservice.RepairRetirementPlan{}, errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	manager.mu.Lock()
	instance := manager.active
	manager.mu.Unlock()
	if instance == nil {
		return normalservice.RepairRetirementPlan{}, errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return instance.PlanRepairRetirement(ctx)
}

func (manager *normalVisualRuntimeManager) RetireRepair(ctx context.Context, expected *normalservice.RepairRetirementPlan) error {
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
	return instance.RetireRepair(ctx, expected)
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
	return manager.stopActiveLocked(ctx)
}

func (manager *normalVisualRuntimeManager) stopActiveLocked(ctx context.Context) error {
	if manager.active == nil {
		return nil
	}
	if err := manager.active.Stop(ctx); err != nil {
		return err
	}
	manager.active = nil
	manager.activeGitHubBinding = ""
	manager.activeGitHubRevision = 0
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
	manager.rebindRequired = false
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
		manager.activeGitHubBinding = ""
		manager.activeGitHubRevision = 0
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

func buildNormalVisualRuntime(deps normalVisualRuntimeDependencies, installed integrated.Config, githubRuntime liveGitHubRuntimeSnapshot) (normalVisualRuntimeInstance, error) {
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
		deps.Governor, lifecycle, stores, deps.AgentManager, githubRuntime.Client,
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
			githubRuntime.Client, githubRuntime.Token, readiness.Ready, deps.Logger,
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

func (runtime *concreteNormalVisualRuntime) PlanRepairRetirement(ctx context.Context) (normalservice.RepairRetirementPlan, error) {
	runtime.mu.Lock()
	runner, started := runtime.runner, runtime.started
	runtime.mu.Unlock()
	if !started || runner == nil {
		return normalservice.RepairRetirementPlan{}, errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return runner.PlanRepairRetirement(ctx)
}

func (runtime *concreteNormalVisualRuntime) RetireRepair(ctx context.Context, expected *normalservice.RepairRetirementPlan) error {
	runtime.mu.Lock()
	runner, started := runtime.runner, runtime.started
	runtime.mu.Unlock()
	if !started || runner == nil {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return runner.RetireRepair(ctx, expected)
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
		SetupActorID       int64                    `json:"setup_actor_id"`
		SetupActorLogin    string                   `json:"setup_actor_login"`
		SetupWriterID      int64                    `json:"setup_writer_id"`
		SetupWriterLogin   string                   `json:"setup_writer_login"`
		SetupWriterType    string                   `json:"setup_writer_type"`
		SetupAppID         int64                    `json:"setup_app_id"`
		SetupInstallation  int64                    `json:"setup_installation_id"`
		SetupAppBinding    string                   `json:"setup_app_binding"`
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
		SetupActorID:       installed.SetupAuthorizationActorID,
		SetupActorLogin:    strings.ToLower(strings.TrimSpace(installed.SetupAuthorizationActorLogin)),
		SetupWriterID:      installed.SetupAuthorizationWriterID,
		SetupWriterLogin:   strings.ToLower(strings.TrimSpace(installed.SetupAuthorizationWriterLogin)),
		SetupWriterType:    strings.ToLower(strings.TrimSpace(installed.SetupAuthorizationWriterType)),
		SetupAppID:         installed.SetupAuthorizationAppID,
		SetupInstallation:  installed.SetupAuthorizationInstallationID,
		SetupAppBinding:    strings.ToLower(strings.TrimSpace(installed.SetupAuthorizationAppBindingDigest)),
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

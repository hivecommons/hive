package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const daemonStatusSchema = "hive.integrated-daemon.v2"
const daemonLeaseSchema = "hive.integrated-daemon-lease.v2"
const normalVisualDaemonLeaseSchema = "hive.normal-visual-daemon-lease.v1"

var errDaemonLeaseHeld = errors.New("hive scheduler lease is already held")

type integratedDaemonLease struct {
	SchemaVersion    string    `json:"schema_version"`
	PID              int       `json:"pid"`
	Executable       string    `json:"executable"`
	HiveCommit       string    `json:"hive_commit"`
	ExecutableSHA256 string    `json:"executable_sha256"`
	AcquiredAt       time.Time `json:"acquired_at"`
}

// normalVisualDaemonLease is an observable ownership proof for the ordinary
// Hive/dashboard process. Its distinct schema can never be mistaken for a
// legacy scheduler identity or targeted by legacy stop operations.
type normalVisualDaemonLease struct {
	SchemaVersion    string    `json:"schema_version"`
	PID              int       `json:"pid"`
	StateDir         string    `json:"state_dir"`
	Executable       string    `json:"executable"`
	HiveCommit       string    `json:"hive_commit"`
	ExecutableSHA256 string    `json:"executable_sha256"`
	AcquiredAt       time.Time `json:"acquired_at"`
}

type integratedDaemonStatus struct {
	SchemaVersion    string               `json:"schema_version"`
	PID              int                  `json:"pid"`
	Running          bool                 `json:"running"`
	RuntimeRunning   bool                 `json:"runtime_running"`
	Repository       string               `json:"repository,omitempty"`
	StateDir         string               `json:"state_dir"`
	Executable       string               `json:"executable,omitempty"`
	HiveCommit       string               `json:"hive_commit,omitempty"`
	ExecutableSHA256 string               `json:"executable_sha256,omitempty"`
	StartedAt        time.Time            `json:"started_at,omitempty"`
	StoppedAt        time.Time            `json:"stopped_at,omitempty"`
	IntervalSeconds  int64                `json:"interval_seconds"`
	LastAttemptAt    time.Time            `json:"last_attempt_at,omitempty"`
	LastSuccessAt    time.Time            `json:"last_success_at,omitempty"`
	LastRunURL       string               `json:"last_run_url,omitempty"`
	LastError        string               `json:"last_error,omitempty"`
	NextRunAt        time.Time            `json:"next_run_at,omitempty"`
	Service          *daemonServiceStatus `json:"service,omitempty"`
}

var daemonStatusMu sync.Mutex

func runIntegratedStart(args []string) int {
	flags := flag.NewFlagSet("hive start", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	interval := flags.Duration("interval", 15*time.Minute, "production scan interval")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key used only when resuming an ambiguous dispatch")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return dispatchHostedOperation(*stateDir, "resume", *requestID, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	status, err := ensureIntegratedDaemonStarted(*stateDir, *interval)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start Hive:", err)
		return 1
	}
	if err := clearDeferredSchedulerStart(*stateDir); err != nil {
		fmt.Fprintln(os.Stderr, "Hive scheduler started, but clearing deferred start state failed:", err)
		return 1
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("Hive scheduler is running for %s (pid=%d, interval=%s).\n", status.Repository, status.PID, time.Duration(status.IntervalSeconds)*time.Second)
	return 0
}

func ensureIntegratedDaemonStarted(stateDir string, interval time.Duration) (integratedDaemonStatus, error) {
	if interval < time.Minute || interval > 24*time.Hour {
		return integratedDaemonStatus{}, fmt.Errorf("scan interval must be between 1 minute and 24 hours")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	config, err := store.Load()
	if err != nil {
		return integratedDaemonStatus{}, fmt.Errorf("complete hive setup before start: %w", err)
	}
	if err := preflightLegacySchedulerStart(stateDir, config); err != nil {
		return integratedDaemonStatus{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	runtimeStatus := readIntegratedDaemonRuntimeStatus(stateDir)
	if spec, exists, readErr := readDaemonServiceSpec(stateDir); readErr == nil && exists && sameDaemonExecutableIdentity(spec.Executable, spec.HiveCommit, spec.ExecutableSHA256, executable, commit, digest) && spec.IntervalSeconds == int64(interval.Seconds()) {
		service := inspectDaemonService(stateDir, runtimeStatus.Running)
		if runtimeStatus.Running && sameDaemonExecutableIdentity(runtimeStatus.Executable, runtimeStatus.HiveCommit, runtimeStatus.ExecutableSHA256, executable, commit, digest) && daemonServiceReady(service) {
			runtimeStatus.RuntimeRunning = true
			runtimeStatus.Service = service
			return runtimeStatus, nil
		}
	}
	// Disable/remove any old registration before terminating a legacy or stale
	// lease owner. Otherwise the service manager can race the upgrade by
	// restarting the old executable while the new definition is installed.
	if err := uninstallDaemonService(stateDir); err != nil {
		return integratedDaemonStatus{}, err
	}
	if _, err := terminateIntegratedDaemonRuntime(stateDir); err != nil {
		return integratedDaemonStatus{}, err
	}
	spec, err := activateDaemonService(stateDir, executable, interval)
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	logPath := filepath.Join(stateDir, "integrated", "daemon.log")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status := readIntegratedDaemonRuntimeStatus(stateDir)
		if status.Running {
			status.RuntimeRunning = true
			status.Service = inspectDaemonService(stateDir, true)
			if !daemonServiceReady(status.Service) {
				_ = uninstallDaemonService(stateDir)
				_, _ = terminateIntegratedDaemonRuntime(stateDir)
				return integratedDaemonStatus{}, fmt.Errorf("scheduler runtime started without an exact persistent service binding: %+v", status.Service)
			}
			store.Audit(integrated.AuditEntry{Action: "daemon_start", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d interval=%s", status.PID, time.Duration(status.IntervalSeconds)*time.Second)})
			return status, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	service := inspectDaemonService(stateDir, false)
	cleanupErr := uninstallDaemonService(stateDir)
	_, _ = terminateIntegratedDaemonRuntime(stateDir)
	if cleanupErr != nil {
		return integratedDaemonStatus{}, fmt.Errorf("scheduler service %s did not claim persistent state; inspect %s (service=%+v); cleanup failed: %w", spec.Name, logPath, service, cleanupErr)
	}
	return integratedDaemonStatus{}, fmt.Errorf("scheduler service %s did not claim persistent state; inspect %s (service=%+v)", spec.Name, logPath, service)
}

func runIntegratedStop(args []string) int {
	flags := flag.NewFlagSet("hive stop", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key used only when resuming an ambiguous dispatch")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return dispatchHostedOperation(*stateDir, "pause", *requestID, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	status, err := stopIntegratedDaemon(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stop Hive:", err)
		return 1
	}
	if err := clearDeferredSchedulerStart(*stateDir); err != nil {
		fmt.Fprintln(os.Stderr, "Hive scheduler stopped, but canceling deferred start failed:", err)
		return 1
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Println("Hive scheduler is stopped; persistent lifecycle state is preserved.")
	return 0
}

func stopIntegratedDaemon(stateDir string) (integratedDaemonStatus, error) {
	status := readIntegratedDaemonRuntimeStatus(stateDir)
	if err := uninstallDaemonService(stateDir); err != nil {
		status.Service = inspectDaemonService(stateDir, status.Running)
		return status, err
	}
	status, err := terminateIntegratedDaemonRuntime(stateDir)
	if err != nil {
		status.Service = inspectDaemonService(stateDir, status.Running)
		return status, err
	}
	status.Service = inspectDaemonService(stateDir, false)
	if store, err := integrated.NewStore(filepath.Join(stateDir, "integrated")); err == nil {
		if config, loadErr := store.Load(); loadErr == nil {
			store.Audit(integrated.AuditEntry{Action: "daemon_stop", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", status.PID)})
		}
	}
	return status, nil
}

func terminateIntegratedDaemonRuntime(stateDir string) (integratedDaemonStatus, error) {
	status := readIntegratedDaemonRuntimeStatus(stateDir)
	if pid, targeted := integratedDaemonTerminationTarget(status); targeted {
		if err := terminateProcess(pid); err != nil && processIsAlive(pid) {
			return status, err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && processIsAlive(pid) {
			time.Sleep(100 * time.Millisecond)
		}
		if processIsAlive(pid) {
			return status, fmt.Errorf("scheduler did not exit within 10 seconds")
		}
	}
	status = readIntegratedDaemonRuntimeStatus(stateDir)
	status.Running = false
	if status.StoppedAt.IsZero() {
		status.StoppedAt = time.Now().UTC()
	}
	_ = writeIntegratedDaemonStatus(stateDir, status)
	return status, nil
}

func integratedDaemonTerminationTarget(status integratedDaemonStatus) (int, bool) {
	if !status.Running || status.PID <= 0 {
		return 0, false
	}
	return status.PID, true
}

func runIntegratedDaemon(args []string) int {
	flags := flag.NewFlagSet("hive daemon", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	interval := flags.Duration("interval", 15*time.Minute, "production scan interval")
	runTimeout := flags.Duration("run-timeout", 45*time.Minute, "maximum production run duration")
	logPath := flags.String("log-file", "", "append scheduler output to this file")
	expectedCommit := flags.String("expected-hive-commit", "", "immutable Hive commit bound by the persistent service")
	expectedDigest := flags.String("expected-executable-sha256", "", "Hive executable SHA-256 bound by the persistent service")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	executable, executableErr := os.Executable()
	commit, digest, identityErr := currentDaemonExecutableIdentity(executable)
	if executableErr != nil || identityErr != nil || !sameDaemonExecutableIdentity(executable, commit, digest, executable, strings.ToLower(strings.TrimSpace(*expectedCommit)), strings.ToLower(strings.TrimSpace(*expectedDigest))) {
		fmt.Fprintln(os.Stderr, "persistent scheduler executable identity does not match its immutable service binding")
		return 1
	}
	if strings.TrimSpace(*logPath) != "" {
		logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open scheduler log:", err)
			return 1
		}
		defer logFile.Close()
		priorStdout, priorStderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = logFile, logFile
		defer func() { os.Stdout, os.Stderr = priorStdout, priorStderr }()
	}
	if *interval < time.Minute || *interval > 24*time.Hour {
		fmt.Fprintln(os.Stderr, "scan interval must be between 1 minute and 24 hours")
		return 2
	}
	stateDirAbs, err := filepath.Abs(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store, err := integrated.NewStore(filepath.Join(stateDirAbs, "integrated"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if config.ExecutionMode == integrated.ExecutionHosted {
		fmt.Fprintln(os.Stderr, "hosted installations are owned by the repository controller and cannot start a local daemon")
		return 1
	}
	if err := preflightLegacySchedulerStart(stateDirAbs, config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runtimeState, err := claimIntegratedDaemonRuntime(stateDirAbs, config, newIntegratedSpecialistRuntime)
	if errors.Is(err, errDaemonLeaseHeld) {
		// Concurrent and repeated starts are idempotent. The caller that spawned
		// this process will observe the scheduler or normal Hive runtime which
		// won the lease. The specialist factory has not run at this point.
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runtimeClosed := false
	defer func() {
		if !runtimeClosed {
			_ = runtimeState.Close()
		}
	}()
	started := time.Now().UTC()
	status := integratedDaemonStatus{SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: config.Repository, StateDir: stateDirAbs, Executable: executable, HiveCommit: commit, ExecutableSHA256: digest, StartedAt: started, IntervalSeconds: int64(interval.Seconds())}
	if err := writeIntegratedDaemonStatus(stateDirAbs, status); err != nil {
		fmt.Fprintln(os.Stderr, "write scheduler status:", err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: "daemon_claim", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", os.Getpid())})
	defer func() {
		status.Running = false
		status.StoppedAt = time.Now().UTC()
		status.NextRunAt = time.Time{}
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		status.LastAttemptAt = time.Now().UTC()
		status.LastError = ""
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
		cycleCtx, cancel := context.WithTimeout(ctx, *runTimeout+time.Minute)
		result, cycleErr := runtimeState.RunCycle(cycleCtx, stateDirAbs, *runTimeout, runIntegratedDaemonCycle)
		cancel()
		now := time.Now().UTC()
		wait := *interval
		if cycleErr != nil {
			if errors.Is(cycleErr, integrated.ErrRunInProgress) {
				fmt.Printf("%s scheduler run skipped because another production run owns the repository lease\n", now.Format(time.RFC3339))
			} else {
				status.LastError = sanitizeDaemonError(cycleErr)
				if wait > time.Minute {
					wait = time.Minute
				}
				fmt.Fprintf(os.Stderr, "%s scheduler run failed: %s\n", now.Format(time.RFC3339), status.LastError)
			}
		} else {
			status.LastSuccessAt = now
			status.LastRunURL = result.Workflow.RunURL
			fmt.Printf("%s scheduler run succeeded: %s\n", now.Format(time.RFC3339), result.Workflow.RunURL)
		}
		status.NextRunAt = now.Add(wait)
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if err := runtimeState.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "shutdown persistent Hive specialists:", err)
				return 1
			}
			runtimeClosed = true
			return 0
		case <-timer.C:
		}
	}
}

type integratedDaemonSpecialistFactory func(string, integrated.Config) (*integratedSpecialistRuntime, error)

type integratedDaemonCycleRunner func(context.Context, string, time.Duration, *integratedSpecialistRuntime) (integrated.RunResult, error)

// claimedIntegratedDaemonRuntime binds the legacy specialist runtime and its
// cycles to the scheduler ownership lease. The normal Hive process claims the
// same lease before enabling its ordinary-Manager service, so these two paths
// cannot construct managers or run cycles concurrently.
type claimedIntegratedDaemonRuntime struct {
	lease       *os.File
	specialists *integratedSpecialistRuntime
}

func claimIntegratedDaemonRuntime(stateDir string, config integrated.Config, factory integratedDaemonSpecialistFactory) (*claimedIntegratedDaemonRuntime, error) {
	if factory == nil {
		return nil, errors.New("integrated daemon specialist factory is required")
	}
	lease, err := claimDaemonLease(stateDir)
	if err != nil {
		return nil, err
	}
	specialists, err := factory(stateDir, config)
	if err != nil {
		releaseDaemonLease(lease)
		return nil, fmt.Errorf("initialize persistent Hive specialists: %w", err)
	}
	return &claimedIntegratedDaemonRuntime{lease: lease, specialists: specialists}, nil
}

func (runtimeState *claimedIntegratedDaemonRuntime) RunCycle(ctx context.Context, stateDir string, timeout time.Duration, runner integratedDaemonCycleRunner) (integrated.RunResult, error) {
	if runtimeState == nil || runtimeState.lease == nil {
		return integrated.RunResult{}, errors.New("integrated daemon cycle requires exclusive runtime ownership")
	}
	if runner == nil {
		return integrated.RunResult{}, errors.New("integrated daemon cycle runner is required")
	}
	return runner(ctx, stateDir, timeout, runtimeState.specialists)
}

func (runtimeState *claimedIntegratedDaemonRuntime) Close() error {
	if runtimeState == nil {
		return nil
	}
	var err error
	if runtimeState.specialists != nil {
		err = runtimeState.specialists.Close()
		runtimeState.specialists = nil
	}
	releaseDaemonLease(runtimeState.lease)
	runtimeState.lease = nil
	return err
}

// runClaimedIntegratedOneShot acquires the same scheduler ownership used by
// the legacy daemon before constructing its specialist Manager. This keeps the
// supported `hive run` entry point from briefly creating a second Manager while
// the normal Hive process owns Visual Hive work, even though RunOnce would
// later reject on the separate production-run lease.
func runClaimedIntegratedOneShot(
	ctx context.Context,
	stateDir string,
	timeout time.Duration,
	config integrated.Config,
	factory integratedDaemonSpecialistFactory,
	runner integratedDaemonCycleRunner,
) (result integrated.RunResult, err error) {
	if runner == nil {
		return result, errors.New("integrated one-shot cycle runner is required")
	}
	runtimeState, err := claimIntegratedDaemonRuntime(stateDir, config, factory)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := runtimeState.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("shutdown persistent Hive specialists: %w", closeErr))
		}
	}()
	return runtimeState.RunCycle(ctx, stateDir, timeout, runner)
}

func runIntegratedDaemonCycle(ctx context.Context, stateDir string, timeout time.Duration, specialists *integratedSpecialistRuntime) (integrated.RunResult, error) {
	token := resolveGitHubToken("HIVE_GITHUB_TOKEN")
	if token == "" {
		return integrated.RunResult{}, fmt.Errorf("GitHub authorization is unavailable; run gh auth login")
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integrated.RunResult{}, err
	}
	config, err := store.Load()
	if err != nil {
		return integrated.RunResult{}, err
	}
	if config.Paused {
		return integrated.RunResult{}, fmt.Errorf("repository automation is paused")
	}
	if ok, message := validateVisualHiveLauncher(config); !ok {
		return integrated.RunResult{}, fmt.Errorf("Visual Hive runtime is not ready: %s", message)
	}
	if specialists != nil {
		if err := specialists.matches(stateDir, config); err != nil {
			return integrated.RunResult{}, err
		}
	} else {
		providerCtx, providerCancel := context.WithTimeout(ctx, 45*time.Second)
		providerErr := integratedSetupCodexProvider(config.ProviderCommand, config.ProviderArgs).Health(providerCtx)
		providerCancel()
		if providerErr != nil {
			return integrated.RunResult{}, providerErr
		}
	}
	for _, check := range liveRepositoryChecks(ctx, client, config) {
		if check.OK {
			continue
		}
		if check.Name == "branch_protection" && config.Automation == integrated.AutomationAutoMerge {
			pending, pendingErr := daemonCanRunProtectionActivation(ctx, client, config)
			if pendingErr != nil {
				return integrated.RunResult{}, fmt.Errorf("readiness check %s failed: %w", check.Name, pendingErr)
			}
			if pending {
				// RunOnce is the only writer allowed to activate protection. Doctor
				// remains strict, but the scheduler must be able to enter that exact
				// two-phase transition after the managed setup PR is installed.
				continue
			}
		}
		return integrated.RunResult{}, fmt.Errorf("readiness check %s failed: %s", check.Name, check.Message)
	}
	options := integrated.RunOptions{StateDir: stateDir, Timeout: timeout, GitHub: client, GitTransportToken: token}
	if specialists != nil {
		options.Specialists = specialists.Manager
		options.SpecialistWorkDir = specialists.WorkDir
	}
	return integrated.RunOnce(ctx, options)
}

func daemonCanRunProtectionActivation(ctx context.Context, client *hivegithub.Client, config integrated.Config) (bool, error) {
	if client == nil || config.Automation != integrated.AutomationAutoMerge {
		return false, nil
	}
	protection, err := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
	if err != nil {
		return false, fmt.Errorf("inspect pending protection activation: %w", err)
	}
	expectedAppID, err := client.ExpectedGitHubAppID(ctx, "github-actions")
	if err != nil {
		return false, fmt.Errorf("resolve pending protection activation producer: %w", err)
	}
	activation, exists, err := integrated.ReadProtectionActivation(config.StateDir)
	if err != nil {
		return false, fmt.Errorf("read pending protection activation state: %w", err)
	}
	activationDurable := exists && integrated.ProtectionActivationMatchesConfig(activation, config) && activation.CheckAppID == expectedAppID
	return daemonProtectionActivationRunnable(config.Automation, protection, expectedAppID, activationDurable), nil
}

func daemonProtectionActivationRunnable(automation integrated.Automation, protection hivegithub.BranchProtectionSummary, expectedAppID int64, activationDurable bool) bool {
	if automation != integrated.AutomationAutoMerge || expectedAppID <= 0 {
		return false
	}
	if !protection.Enabled {
		// No repository-owned policy exists, so RunOnce may perform the trusted
		// scan and create Hive's conservative minimum protection.
		return true
	}
	if !protection.Strict || !protection.AdminEnforced {
		return false
	}
	visualRequired := false
	for _, check := range protection.RequiredCheckIdentities {
		if strings.EqualFold(strings.TrimSpace(check.Context), "visual-hive") && check.AppID == expectedAppID {
			visualRequired = true
			break
		}
	}
	// Existing policy is never modified. It is safe to enter RunOnce only when
	// that policy is already exact and the remaining work is the trusted-run
	// activation checkpoint (including a new digest after an upgrade).
	return visualRequired && !activationDurable
}

func claimDaemonLease(stateDir string) (*os.File, error) {
	return claimDaemonOwnershipLease(stateDir, true)
}

// claimNormalVisualDaemonLease uses the scheduler's authoritative OS lock and
// writes a distinct ordinary-runtime identity. Legacy status/stop parsing does
// not accept that schema, while normal status can prove the dashboard-owned
// Visual service is live without launching a second scheduler.
func claimNormalVisualDaemonLease(stateDir string) (*os.File, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	file, err := claimDaemonOwnershipLease(stateDir, false)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	lease := normalVisualDaemonLease{
		SchemaVersion: normalVisualDaemonLeaseSchema, PID: os.Getpid(), StateDir: stateDir,
		Executable: executable, HiveCommit: commit, ExecutableSHA256: digest, AcquiredAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err == nil {
		err = file.Truncate(0)
	}
	if err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		releaseDaemonLease(file)
		return nil, fmt.Errorf("persist ordinary Hive Visual service ownership: %w", err)
	}
	return file, nil
}

func claimDaemonOwnershipLease(stateDir string, persistDaemonIdentity bool) (*os.File, error) {
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "daemon.lease")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Hive scheduler lease: %w", err)
	}
	locked, err := tryLockDaemonLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("claim Hive scheduler lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, errDaemonLeaseHeld
	}
	if !persistDaemonIdentity {
		if err := file.Truncate(0); err == nil {
			_, err = file.Seek(0, 0)
		}
		if err == nil {
			err = file.Sync()
		}
		if err != nil {
			releaseDaemonLease(file)
			return nil, fmt.Errorf("clear legacy scheduler identity for normal Hive ownership: %w", err)
		}
		return file, nil
	}
	executable, err := os.Executable()
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	lease := integratedDaemonLease{SchemaVersion: daemonLeaseSchema, PID: os.Getpid(), Executable: executable, HiveCommit: commit, ExecutableSHA256: digest, AcquiredAt: time.Now().UTC()}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	if err := file.Truncate(0); err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		releaseDaemonLease(file)
		return nil, fmt.Errorf("persist Hive scheduler lease: %w", err)
	}
	// These files are from the pre-lease implementation. They are never
	// consulted after the lease is held and are safe to clean up here.
	_ = os.Remove(filepath.Join(dir, "daemon.pid"))
	_ = os.Remove(filepath.Join(dir, "daemon.starting"))
	return file, nil
}

func releaseDaemonLease(file *os.File) {
	if file == nil {
		return
	}
	_ = unlockDaemonLease(file)
	_ = file.Close()
}

func readIntegratedDaemonLease(stateDir string) (integratedDaemonLease, bool) {
	path := filepath.Join(stateDir, "integrated", "daemon.lease")
	for attempt := 0; attempt < 5; attempt++ {
		data, err := os.ReadFile(path)
		if err != nil {
			return integratedDaemonLease{}, false
		}
		var lease integratedDaemonLease
		if json.Unmarshal(data, &lease) == nil && lease.SchemaVersion == daemonLeaseSchema && lease.PID > 0 && lease.Executable != "" && validDaemonHexIdentity(lease.HiveCommit, 40) && validDaemonHexIdentity(lease.ExecutableSHA256, 64) {
			file, openErr := os.OpenFile(path, os.O_RDWR, 0o600)
			if openErr != nil {
				return integratedDaemonLease{}, false
			}
			locked, lockErr := tryReadDaemonLease(file)
			if locked {
				_ = unlockDaemonLease(file)
			}
			_ = file.Close()
			if lockErr != nil || locked {
				return lease, false
			}
			// The record is immutable while its owner holds the lock. Confirm
			// that ownership did not change between the read and lock probe.
			current, readErr := os.ReadFile(path)
			if readErr == nil && string(current) == string(data) {
				return lease, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return integratedDaemonLease{}, false
}

func readNormalVisualDaemonLease(stateDir string) (normalVisualDaemonLease, bool) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return normalVisualDaemonLease{}, false
	}
	path := filepath.Join(stateDir, "integrated", "daemon.lease")
	for attempt := 0; attempt < 5; attempt++ {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return normalVisualDaemonLease{}, false
		}
		var lease normalVisualDaemonLease
		valid := json.Unmarshal(data, &lease) == nil && lease.SchemaVersion == normalVisualDaemonLeaseSchema &&
			lease.PID > 0 && !lease.AcquiredAt.IsZero() && lease.Executable != "" && sameSpecialistRuntimePath(lease.StateDir, stateDir) &&
			validDaemonHexIdentity(lease.HiveCommit, 40) && validDaemonHexIdentity(lease.ExecutableSHA256, 64)
		if valid {
			file, openErr := os.OpenFile(path, os.O_RDWR, 0o600)
			if openErr != nil {
				return normalVisualDaemonLease{}, false
			}
			locked, lockErr := tryReadDaemonLease(file)
			if locked {
				_ = unlockDaemonLease(file)
			}
			_ = file.Close()
			if lockErr != nil || locked {
				return lease, false
			}
			current, currentErr := os.ReadFile(path)
			// The held OS lock is the ownership proof. Process-table metadata is
			// diagnostic only and may drift during an atomic executable upgrade;
			// never permit a second service owner merely because it drifted.
			if currentErr == nil && string(current) == string(data) {
				return lease, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return normalVisualDaemonLease{}, false
}

func normalVisualDaemonObserved(stateDir string) (normalVisualDaemonLease, bool, bool) {
	lease, running := readNormalVisualDaemonLease(stateDir)
	return lease, lease.SchemaVersion == normalVisualDaemonLeaseSchema, running
}

func readIntegratedDaemonStatus(stateDir string) integratedDaemonStatus {
	status := readIntegratedDaemonRuntimeStatus(stateDir)
	status.RuntimeRunning = status.Running
	status.Service = inspectDaemonService(stateDir, status.Running)
	if status.Service.Managed {
		// Running represents the operator's persistent desired state. The
		// runtime-specific bit remains available while the OS supervisor is in
		// its bounded restart delay or a damaged registration awaits repair.
		status.Running = true
	}
	return status
}

func readIntegratedDaemonRuntimeStatus(stateDir string) integratedDaemonStatus {
	stateDir, _ = filepath.Abs(stateDir)
	leasePath := filepath.Join(stateDir, "integrated", "daemon.lease")
	_, leaseErr := os.Stat(leasePath)
	leasePresent := leaseErr == nil || !errors.Is(leaseErr, os.ErrNotExist)
	lease, leaseHeld := integratedDaemonLease{}, false
	if leaseErr == nil {
		lease, leaseHeld = readIntegratedDaemonLease(stateDir)
	}
	leaseStatus := func(lastError string) integratedDaemonStatus {
		status := integratedDaemonStatus{SchemaVersion: daemonStatusSchema, StateDir: stateDir, LastError: lastError}
		if leaseHeld && processIsAlive(lease.PID) {
			status.PID = lease.PID
			status.Executable = lease.Executable
			status.HiveCommit = lease.HiveCommit
			status.ExecutableSHA256 = lease.ExecutableSHA256
			status.StartedAt = lease.AcquiredAt
			status.Running = true
		}
		return status
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "integrated", "daemon.json"))
	if err != nil {
		return leaseStatus("")
	}
	var status integratedDaemonStatus
	if json.Unmarshal(data, &status) != nil || status.SchemaVersion != daemonStatusSchema {
		return leaseStatus("daemon status is invalid")
	}
	if leasePresent {
		// The live OS lock is the ownership proof. The executable on disk may
		// legitimately have been atomically replaced by an installer while the
		// old daemon keeps running from a deleted/backup inode.
		status.Running = leaseHeld && lease.PID == status.PID && processIsAlive(status.PID) && sameDaemonExecutableIdentity(status.Executable, status.HiveCommit, status.ExecutableSHA256, lease.Executable, lease.HiveCommit, lease.ExecutableSHA256)
	} else {
		// Legacy status without a lease needs the stronger executable check to
		// avoid treating a reused PID as Hive.
		status.Running = status.PID > 0 && status.Executable != "" && validDaemonHexIdentity(status.HiveCommit, 40) && validDaemonHexIdentity(status.ExecutableSHA256, 64) && processIsAlive(status.PID) && processMatchesExecutable(status.PID, status.Executable)
	}
	return status
}

func sameDaemonExecutableIdentity(leftPath, leftCommit, leftDigest, rightPath, rightCommit, rightDigest string) bool {
	pathsEqual := filepath.Clean(leftPath) == filepath.Clean(rightPath)
	if runtime.GOOS == "windows" {
		pathsEqual = strings.EqualFold(filepath.Clean(leftPath), filepath.Clean(rightPath))
	}
	return pathsEqual && strings.EqualFold(strings.TrimSpace(leftCommit), strings.TrimSpace(rightCommit)) && strings.EqualFold(strings.TrimSpace(leftDigest), strings.TrimSpace(rightDigest))
}

func writeIntegratedDaemonStatus(stateDir string, status integratedDaemonStatus) error {
	daemonStatusMu.Lock()
	defer daemonStatusMu.Unlock()
	status.SchemaVersion = daemonStatusSchema
	status.Service = nil
	status.RuntimeRunning = false
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(dir, fmt.Sprintf(".daemon-%d.tmp", os.Getpid()))
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dir, "daemon.json"))
}

func daemonEnvironment(environment []string) []string {
	denied := map[string]bool{"HIVE_GITHUB_TOKEN": true, "GH_TOKEN": true, "GITHUB_TOKEN": true}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !denied[strings.ToUpper(name)] {
			result = append(result, entry)
		}
	}
	return result
}

func sanitizeDaemonError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(err.Error(), "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

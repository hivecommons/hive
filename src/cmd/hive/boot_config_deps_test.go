package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/tracing"
)

// bootConfig drives every process-level effect of the boot's first phase
// through bootConfigDeps (#7571, step 2). These tests substitute fakes for
// argv, env, exit, the singleton flock, the upgrade marker, config load,
// tracing and signals, and assert on the phase's DECISIONS: which fast path
// fired, what was logged, which cleanups were registered, and what state the
// later phases inherit on b.

// exitCalled is the sentinel a fake exit panics with so the phase stops where
// os.Exit would have. runBootConfig recovers it.
type exitCalled struct{ code int }

// bootConfigFake is a fully-wired bootConfigDeps whose effects are recorded
// instead of performed. Tests override individual fields.
type bootConfigFake struct {
	deps bootConfigDeps
	env  map[string]string

	stdout bytes.Buffer
	log    bytes.Buffer

	lockPath      string
	lockReleased  bool
	markerCleared bool
	hubRun        bool
	hubConfigPath string
	signalsWired  bool
	traceShutdown bool
	reconciledSHA string
	parsedDefault string
}

func newBootConfigFake(t *testing.T) *bootConfigFake {
	t.Helper()
	f := &bootConfigFake{env: map[string]string{}}
	f.deps = bootConfigDeps{
		args:   []string{"hive"},
		stdout: &f.stdout,
		stderr: io.Discard,
		getenv: func(k string) string { return f.env[k] },
		exit:   func(code int) { panic(exitCalled{code}) },
		parseFlags: func(def string) string {
			f.parsedDefault = def
			return def
		},
		releaseChannel:          func() string { return "" },
		selfImage:               func() string { return "" },
		reconcileUpgradeOutcome: func(sha string, _ *slog.Logger) { f.reconciledSHA = sha },
		acquireLock: func(path string) (func(), error) {
			f.lockPath = path
			return func() { f.lockReleased = true }, nil
		},
		readUpgradeMarker:  func() ([]byte, error) { return nil, os.ErrNotExist },
		clearUpgradeMarker: func() error { f.markerCleared = true; return nil },
		runHub: func(_ *slog.Logger, configPath string) {
			f.hubRun, f.hubConfigPath = true, configPath
		},
		loadConfig: func(string) (*config.Config, error) {
			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Repos = []string{"acme/widgets"}
			cfg.Agents = map[string]config.AgentConfig{}
			return cfg, nil
		},
		// Every logger the phase installs is captured, including the stdout
		// bootstrap logger's replacement, so assertions see the whole boot.
		fileLogger: func(*config.Config) *slog.Logger {
			return slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		},
		hiveID: func(*slog.Logger) string { return "hive-test-id" },
		stat:   func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		initTracing: func(context.Context, tracing.Config) (func(context.Context) error, error) {
			return func(context.Context) error { f.traceShutdown = true; return nil }, nil
		},
		loadReachState: func(string, *slog.Logger) error { return nil },
		notifySignals:  func(chan<- os.Signal) { f.signalsWired = true },
	}
	// The bootstrap logger writes JSON to deps.stdout; route it into the
	// same capture buffer so pre-config log lines are assertable too.
	f.deps.stdout = io.MultiWriter(&f.stdout, &f.log)
	// bootConfig installs its loggers as the process default; restore the
	// test's afterwards so one test's capture does not leak into the next.
	saved := slog.Default()
	t.Cleanup(func() { slog.SetDefault(saved) })
	return f
}

// runBootConfig runs the phase and reports whether it returned, its return
// value, and the exit code if a fake exit fired instead.
func runBootConfig(t *testing.T, b *boot, deps bootConfigDeps) (returned, ok bool, exitCode int) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			e, isExit := r.(exitCalled)
			if !isExit {
				panic(r)
			}
			returned, exitCode = false, e.code
		}
	}()
	return true, b.bootConfigWith(deps), -1
}

func TestBootConfigWith_VersionFastPathWritesToStdoutOnly(t *testing.T) {
	f := newBootConfigFake(t)
	f.deps.args = []string{"hive", "--version"}
	b := &boot{}

	returned, ok, _ := runBootConfig(t, b, f.deps)
	if !returned || ok {
		t.Fatalf("returned=%v ok=%v, want returned=true ok=false", returned, ok)
	}
	if !strings.HasPrefix(f.stdout.String(), "hive ") {
		t.Fatalf("stdout = %q, want the version line", f.stdout.String())
	}
	if f.parsedDefault != "" || f.lockPath != "" || f.reconciledSHA != "" {
		t.Fatalf("fast path did work it must skip: flags=%q lock=%q reconcile=%q", f.parsedDefault, f.lockPath, f.reconciledSHA)
	}
}

func TestBootConfigWith_ValidateExitsWithConfigCheckStatus(t *testing.T) {
	for _, verb := range []string{"validate", "--config-check"} {
		t.Run(verb, func(t *testing.T) {
			f := newBootConfigFake(t)
			f.deps.args = []string{"hive", verb, "--config", "/nonexistent/hive.yaml"}

			returned, _, code := runBootConfig(t, &boot{}, f.deps)
			if returned {
				t.Fatal("bootConfig returned; validate must exit with the check's status")
			}
			if code == 0 {
				t.Fatalf("exit code = 0 for a missing config, want non-zero")
			}
			if f.parsedDefault != "" {
				t.Fatal("validate parsed the process flag set; it takes its own")
			}
		})
	}
}

func TestBootConfigWith_DuplicateProcessExits(t *testing.T) {
	f := newBootConfigFake(t)
	f.deps.acquireLock = func(path string) (func(), error) {
		f.lockPath = path
		return nil, errors.New("flock: resource temporarily unavailable")
	}
	b := &boot{}

	returned, _, code := runBootConfig(t, b, f.deps)
	if returned || code != duplicateProcessExitCode {
		t.Fatalf("returned=%v code=%d, want exit(%d)", returned, code, duplicateProcessExitCode)
	}
	if f.lockPath == "" {
		t.Fatal("lock was never attempted")
	}
	if !strings.Contains(f.log.String(), "another hive process is already running") {
		t.Fatalf("missing duplicate-process error in log:\n%s", f.log.String())
	}
	if len(b.cleanup.fns) != 0 {
		t.Fatalf("registered %d cleanups after a failed lock, want 0", len(b.cleanup.fns))
	}
}

func TestBootConfigWith_SingletonLockCanBeDisabledByEnv(t *testing.T) {
	f := newBootConfigFake(t)
	f.env[singletonLockEnv] = singletonLockDisable
	f.deps.acquireLock = func(string) (func(), error) {
		t.Fatal("acquireLock called with the singleton disabled")
		return nil, nil
	}

	if returned, ok, _ := runBootConfig(t, &boot{}, f.deps); !returned || !ok {
		t.Fatalf("returned=%v ok=%v, want a normal boot", returned, ok)
	}
}

func TestBootConfigWith_UpgradeMarkerVerdicts(t *testing.T) {
	saved := gitShort
	gitShort = "abc1234"
	t.Cleanup(func() { gitShort = saved })

	cases := []struct {
		name        string
		marker      string
		wantCleared bool
		wantLog     string
	}{
		{
			name:        "landed",
			marker:      `{"current_sha":"old0000","target_sha":"abc1234"}`,
			wantCleared: true,
			wantLog:     "upgrade landed, cleared marker",
		},
		{
			name:        "did-not-land",
			marker:      `{"current_sha":"abc1234","target_sha":"fff9999","attempts":2}`,
			wantCleared: false,
			wantLog:     "previous self-upgrade attempt did not land",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootConfigFake(t)
			f.deps.readUpgradeMarker = func() ([]byte, error) { return []byte(tc.marker), nil }

			if returned, ok, _ := runBootConfig(t, &boot{}, f.deps); !returned || !ok {
				t.Fatalf("returned=%v ok=%v, want a normal boot", returned, ok)
			}
			if f.markerCleared != tc.wantCleared {
				t.Fatalf("marker cleared = %v, want %v", f.markerCleared, tc.wantCleared)
			}
			if !strings.Contains(f.log.String(), tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, f.log.String())
			}
			if f.reconciledSHA != "abc1234" {
				t.Fatalf("reconcileUpgradeOutcome ran with %q, want the running SHA", f.reconciledSHA)
			}
		})
	}
}

func TestBootConfigWith_HubModeRunsHubAndReturnsFalse(t *testing.T) {
	f := newBootConfigFake(t)
	f.env["HIVE_MODE"] = "hub"
	f.env[hiveConfigEnv] = "/etc/hive/hub.yaml"
	f.deps.loadConfig = func(string) (*config.Config, error) {
		t.Fatal("hub mode loaded the spoke config")
		return nil, nil
	}
	b := &boot{}

	returned, ok, _ := runBootConfig(t, b, f.deps)
	if !returned || ok {
		t.Fatalf("returned=%v ok=%v, want returned=true ok=false", returned, ok)
	}
	if !f.hubRun || f.hubConfigPath != "/etc/hive/hub.yaml" {
		t.Fatalf("hubRun=%v path=%q, want the hub started on the HIVE_CONFIG path", f.hubRun, f.hubConfigPath)
	}
	if b.cfg != nil || b.ctx != nil {
		t.Fatal("hub mode populated spoke boot state")
	}
}

func TestBootConfigWith_ConfigLoadFailureExits1(t *testing.T) {
	f := newBootConfigFake(t)
	f.deps.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("yaml: bad") }

	returned, _, code := runBootConfig(t, &boot{}, f.deps)
	if returned || code != 1 {
		t.Fatalf("returned=%v code=%d, want exit(1)", returned, code)
	}
	if !strings.Contains(f.log.String(), "failed to load config") {
		t.Fatalf("missing load error in log:\n%s", f.log.String())
	}
}

func TestBootConfigWith_HappyPathPopulatesBootAndRegistersCleanups(t *testing.T) {
	f := newBootConfigFake(t)
	f.env[hiveConfigEnv] = "/etc/hive/hive.yaml"
	f.deps.stat = func(path string) (os.FileInfo, error) {
		if path == config.RuntimeConfigFile {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	b := &boot{}

	returned, ok, _ := runBootConfig(t, b, f.deps)
	if !returned || !ok {
		t.Fatalf("returned=%v ok=%v, want true/true", returned, ok)
	}
	if b.cfg == nil || b.logger == nil || b.ctx == nil || b.startTime.IsZero() {
		t.Fatalf("boot state incomplete: cfg=%v logger=%v ctx=%v start=%v", b.cfg, b.logger, b.ctx, b.startTime)
	}
	if b.configPath != "/etc/hive/hive.yaml" || f.parsedDefault != "/etc/hive/hive.yaml" {
		t.Fatalf("configPath=%q parsedDefault=%q, want HIVE_CONFIG as the -config default", b.configPath, f.parsedDefault)
	}
	if b.cfg.HiveID != "hive-test-id" {
		t.Fatalf("cfg.HiveID = %q, want the identity from deps.hiveID", b.cfg.HiveID)
	}
	if !f.signalsWired {
		t.Fatal("signal handler was not installed")
	}
	if b.repoTargetMisconfigured == nil || b.repoTargetIssueMessage == nil {
		t.Fatal("repo-target closures not set")
	}
	if b.repoTargetMisconfigured() {
		t.Fatalf("repoTargetMisconfigured() = true for a valid config: %q", b.repoTargetIssueMessage())
	}
	for _, want := range []string{"hive starting", "persisted runtime config present", "loaded removed-agents tombstone"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("log missing %q", want)
		}
	}
	if strings.Contains(f.log.String(), "config path disagreement") {
		t.Error("flagged a disagreement when HIVE_CONFIG matches the loaded path")
	}

	// Cleanups: lock release, ctx cancel and trace shutdown, run LIFO by main.
	if len(b.cleanup.fns) != 3 {
		t.Fatalf("registered %d cleanups, want 3 (lock, cancel, tracing)", len(b.cleanup.fns))
	}
	b.cleanup.run()
	if !f.lockReleased || !f.traceShutdown {
		t.Fatalf("cleanup ran: lockReleased=%v traceShutdown=%v, want both", f.lockReleased, f.traceShutdown)
	}
	if b.ctx.Err() == nil {
		t.Fatal("boot ctx not cancelled by cleanup")
	}
}

func TestBootConfigWith_ConfigPathDisagreementAndTracingFailureAreWarnings(t *testing.T) {
	f := newBootConfigFake(t)
	f.env[hiveConfigEnv] = "/data/hive.yaml.runtime"
	f.deps.parseFlags = func(string) string { return "/etc/hive/hive.yaml" }
	// tracing.Init hands back a no-op shutdown alongside its error; the fake
	// mirrors that contract so the registered cleanup stays callable.
	f.deps.initTracing = func(context.Context, tracing.Config) (func(context.Context) error, error) {
		return func(context.Context) error { return nil }, errors.New("otlp: dial refused")
	}
	f.deps.loadReachState = func(string, *slog.Logger) error { return errors.New("no reach state") }
	f.deps.loadConfig = func(string) (*config.Config, error) {
		cfg := &config.Config{}
		cfg.Agents = map[string]config.AgentConfig{}
		cfg.Project.Org = "acme"
		cfg.Project.Repos = []string{"other/repo"} // not under the org
		return cfg, nil
	}
	b := &boot{}

	if returned, ok, _ := runBootConfig(t, b, f.deps); !returned || !ok {
		t.Fatalf("returned=%v ok=%v; none of these faults may stop the boot", returned, ok)
	}
	for _, want := range []string{
		"config path disagreement",
		"tracing init failed; continuing without tracing",
		"reach state load failed",
		"repo target misconfigured",
	} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("log missing %q", want)
		}
	}
	if !b.repoTargetMisconfigured() || b.repoTargetIssueMessage() == "" {
		t.Fatal("repo-target closures do not report the misconfiguration")
	}
	// A failed tracing init still registers its (no-op) shutdown slot: the
	// cleanup count must not depend on which optional subsystems came up.
	if len(b.cleanup.fns) != 3 {
		t.Fatalf("registered %d cleanups, want 3", len(b.cleanup.fns))
	}
}

func TestDefaultBootConfigDeps_IsFullyWired(t *testing.T) {
	d := defaultBootConfigDeps()
	if d.args == nil || d.stdout == nil || d.stderr == nil || d.getenv == nil || d.exit == nil ||
		d.parseFlags == nil || d.releaseChannel == nil || d.selfImage == nil ||
		d.reconcileUpgradeOutcome == nil || d.acquireLock == nil || d.readUpgradeMarker == nil ||
		d.clearUpgradeMarker == nil || d.runHub == nil || d.loadConfig == nil || d.fileLogger == nil ||
		d.hiveID == nil || d.stat == nil || d.initTracing == nil || d.loadReachState == nil ||
		d.notifySignals == nil {
		t.Fatal("defaultBootConfigDeps left a seam nil; production boot would panic")
	}
}

// The default seams that are safe to run on a developer host are exercised
// once so a typo in the adapter closure (wrong path constant, swapped
// argument) fails here rather than on a spoke's first boot.
func TestDefaultBootConfigDeps_AdaptersDelegate(t *testing.T) {
	d := defaultBootConfigDeps()
	dir := t.TempDir()

	release, err := d.acquireLock(dir + "/hive.lock")
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	if _, err := d.acquireLock(dir + "/hive.lock"); err == nil {
		t.Fatal("second acquireLock on the same path succeeded; the singleton is not exclusive")
	}
	release()

	cfg := &config.Config{}
	cfg.Governor.Logging.Dir = dir + "/logs"
	cfg.Governor.Logging.Level = "info"
	if d.fileLogger(cfg) == nil {
		t.Fatal("fileLogger returned nil")
	}
	if _, err := d.stat(dir + "/missing"); !os.IsNotExist(err) {
		t.Fatalf("stat(missing) err = %v, want not-exist", err)
	}
	shutdown, err := d.initTracing(context.Background(), tracing.Config{Enabled: false})
	if err != nil || shutdown == nil {
		t.Fatalf("initTracing(disabled): shutdown nil=%v err=%v, want a no-op shutdown", shutdown == nil, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
	if got := d.getenv("HIVE_BOOT_CONFIG_DEPS_TEST_UNSET"); got != "" {
		t.Fatalf("getenv(unset) = %q", got)
	}
	if d.releaseChannel() != "" && d.selfImage() == "" {
		t.Fatal("releaseChannel reported a channel with no self image")
	}
}

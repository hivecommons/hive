package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// bootDashboardAPIFake records what bootDashboardAPIWith asked for. The
// per-tick bodies are captured, not run, so a test drives them directly.
type bootDashboardAPIFake struct {
	deps bootDashboardAPIDeps
	log  bytes.Buffer

	registeredOn *dashboard.Server
	registered   *dashboard.Dependencies
	forgeFn      func() dashboard.ForgeAppInventory
	discoverTick func()
	healTick     func()
	inception    *dashboard.InceptionWatcher
	inceptionRun int
}

func newBootDashboardAPIFake() *bootDashboardAPIFake {
	f := &bootDashboardAPIFake{}
	f.deps = bootDashboardAPIDeps{
		registerAPI: func(srv *dashboard.Server, d *dashboard.Dependencies) {
			f.registeredOn, f.registered = srv, d
		},
		setForgeAppInventory:  func(_ *dashboard.Server, fn func() dashboard.ForgeAppInventory) { f.forgeFn = fn },
		startInstallDiscovery: func(_ context.Context, tick func()) { f.discoverTick = tick },
		startSelfHeal:         func(_ context.Context, tick func()) { f.healTick = tick },
		startInceptionWatcher: func(_ context.Context, w *dashboard.InceptionWatcher) {
			f.inception = w
			f.inceptionRun++
		},
	}
	return f
}

func bootDashboardAPIConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-dashboard-api-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widgets", "gadgets"}
	cfg.GitHub.InstallationID = 4242
	cfg.GitHub.AppID = 7
	cfg.GitHub.KeyFile = "/nonexistent/app.pem"
	cfg.Agents = map[string]config.AgentConfig{}
	return cfg
}

func newBootDashboardAPIBoot(t *testing.T, f *bootDashboardAPIFake, cfg *config.Config) *boot {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &boot{
		ctx:            ctx,
		cfg:            cfg,
		logger:         logger,
		gov:            governor.New(cfg.Governor, cfg.Agents, logger),
		sched:          scheduler.New(cfg, logger),
		agentMgr:       agent.NewManager(cfg.Agents, logger, agent.ProjectContext{}),
		dashSrv:        dashboard.NewServer(0, logger),
		advisoryIssues: map[string]int{},
		appAuthFailure: "no App credentials in test",
	}
	b.refreshDashboard = func() {}
	return b
}

func TestBootDashboardAPIWith_RegistersDependenciesFromTheBootStruct(t *testing.T) {
	f := newBootDashboardAPIFake()
	cfg := bootDashboardAPIConfig()
	b := newBootDashboardAPIBoot(t, f, cfg)

	b.bootDashboardAPIWith(f.deps)

	if f.registeredOn != b.dashSrv || f.registered == nil {
		t.Fatal("API not registered on the boot's dashboard server")
	}
	d := f.registered
	if d.Config != cfg || d.Governor != b.gov || d.Scheduler != b.sched || d.AgentMgr != b.agentMgr || d.Logger != b.logger || d.Ctx != b.ctx {
		t.Fatal("dashboard.Dependencies does not carry the boot struct's collaborators")
	}
	if d.GHClient != nil || d.GHAppAuth != nil {
		t.Fatal("an App-less boot must hand the dashboard a nil client and auth, not a stale one")
	}
	if d.RefreshFunc == nil || d.PersistFunc == nil || d.ReInitFunc == nil || d.EnumerateFunc == nil ||
		d.RescanReposFunc == nil || d.AdvisoryResetFunc == nil || d.ReinitGitHubFunc == nil ||
		d.IssueClaimed == nil || d.HookFire == nil || d.ResolveAppKeyFileFunc == nil {
		t.Fatal("a dashboard callback was left nil — the handler behind it would panic on first click")
	}

	// The Set-ID handler resolves the key file the same way boot does (#2459):
	// the hub-delivered per-app-id key wins over an empty configured path.
	t.Setenv("GH_APP_KEY_FILE", "/env/app.pem")
	if got, want := d.ResolveAppKeyFileFunc("", 99), resolveAppKeyFile("", "/env/app.pem", 99); got != want {
		t.Fatalf("ResolveAppKeyFileFunc = %q, want %q (boot's resolveAppKeyFile)", got, want)
	}
}

func TestBootDashboardAPIWith_AppliesClassifiedAppStateOnlyWhenRequired(t *testing.T) {
	t.Run("required: state and diagnosis reach the banner", func(t *testing.T) {
		f := newBootDashboardAPIFake()
		b := newBootDashboardAPIBoot(t, f, bootDashboardAPIConfig())
		b.githubAppRequired = true
		b.githubAppState = github.AppStateInsufficientPerms
		b.githubAppDiag = "missing contents:write"

		b.bootDashboardAPIWith(f.deps)

		if !b.dashSrv.IsGitHubAppRequired() {
			t.Fatal("banner not raised although boot classified the App as required")
		}
		if got := b.dashSrv.GetGitHubAppPermIssue(); got != "missing contents:write" {
			t.Fatalf("perm issue = %q, want boot's diagnosis", got)
		}
	})
	t.Run("not required: banner stays down", func(t *testing.T) {
		f := newBootDashboardAPIFake()
		b := newBootDashboardAPIBoot(t, f, bootDashboardAPIConfig())
		b.githubAppDiag = "stale diagnosis that must not leak"

		b.bootDashboardAPIWith(f.deps)

		if b.dashSrv.IsGitHubAppRequired() || b.dashSrv.GetGitHubAppPermIssue() != "" {
			t.Fatal("banner raised or diagnosis applied on a healthy boot")
		}
	})
}

func TestBootDashboardAPIWith_RecheckReportsMissingCredentials(t *testing.T) {
	f := newBootDashboardAPIFake()
	b := newBootDashboardAPIBoot(t, f, bootDashboardAPIConfig())
	b.githubAppRequired = true

	b.bootDashboardAPIWith(f.deps)

	// The Re-check button on a credential-less hive must fail closed and say
	// why, rather than the generic "not accessible".
	if b.dashSrv.RecheckGitHubApp() {
		t.Fatal("recheck reported success with no GitHub client")
	}
	if !b.dashSrv.IsGitHubAppRequired() {
		t.Fatal("recheck cleared the banner without a client to verify with")
	}
	if !strings.Contains(f.log.String(), "running without GitHub credentials") || !strings.Contains(f.log.String(), "no App credentials in test") {
		t.Fatalf("recheck did not surface the boot-time auth failure:\n%s", f.log.String())
	}
}

func TestBootDashboardAPIWith_NoReposInstallsNoRecheckOrSelfHeal(t *testing.T) {
	f := newBootDashboardAPIFake()
	cfg := bootDashboardAPIConfig()
	cfg.Project.Repos = nil
	cfg.Project.PrimaryRepo = ""
	b := newBootDashboardAPIBoot(t, f, cfg)

	b.bootDashboardAPIWith(f.deps)

	if b.dashSrv.RecheckGitHubApp() {
		t.Fatal("recheck callback installed with no repo to check against")
	}
	if f.healTick != nil {
		t.Fatal("self-heal loop started with no primary repo")
	}
	if f.discoverTick == nil {
		t.Fatal("installation-ID discovery must run regardless of repos — it is how a late-approved install is adopted")
	}
}

func TestBootDashboardAPIWith_TickBodiesAreCheapWhenNothingToDo(t *testing.T) {
	f := newBootDashboardAPIFake()
	cfg := bootDashboardAPIConfig()
	b := newBootDashboardAPIBoot(t, f, cfg)

	b.bootDashboardAPIWith(f.deps)

	if f.discoverTick == nil || f.healTick == nil {
		t.Fatal("discovery and self-heal loops not started")
	}
	// installation_id already set: the discovery tick returns before any
	// GitHub call (there is no App auth here, so a call would fail loudly).
	f.discoverTick()
	// Banner not showing: the self-heal tick is a no-op that makes no calls.
	f.healTick()
	if strings.Contains(f.log.String(), "self-heal") || strings.Contains(f.log.String(), "discover") {
		t.Fatalf("idle ticks did work:\n%s", f.log.String())
	}
	if b.dashSrv.IsGitHubAppRequired() {
		t.Fatal("idle self-heal tick raised the banner")
	}
}

func TestBootDashboardAPIWith_ForgeInventoryResolvesTheActiveKey(t *testing.T) {
	f := newBootDashboardAPIFake()
	cfg := bootDashboardAPIConfig()
	b := newBootDashboardAPIBoot(t, f, cfg)
	t.Setenv("GH_APP_KEY_FILE", "")

	b.bootDashboardAPIWith(f.deps)

	if f.forgeFn == nil {
		t.Fatal("Forge App inventory provider not installed")
	}
	inv := f.forgeFn()
	if want := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID); inv.ActiveKeyFile != want {
		t.Fatalf("ActiveKeyFile = %q, want %q", inv.ActiveKeyFile, want)
	}
	if inv.HeldKeys == nil {
		t.Fatal("HeldKeys must be an empty slice, not nil, so the tab renders an empty list rather than null")
	}
}

func TestBootDashboardAPIWith_InceptionWatcherNeedsTheBrainstormStore(t *testing.T) {
	f := newBootDashboardAPIFake()
	b := newBootDashboardAPIBoot(t, f, bootDashboardAPIConfig())

	b.bootDashboardAPIWith(f.deps)

	if f.inceptionRun != 0 {
		t.Fatal("inception watcher started without a brainstorm bead store to watch")
	}
}

func TestRunTickLoop_FiresOnScheduleAndStopsOnCancel(t *testing.T) {
	const interval = 5 * time.Millisecond
	fired := make(chan struct{}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTickLoop(ctx, interval, true, func() { fired <- struct{}{} })
	}()

	timeout := time.After(2 * time.Second)
	for got := 0; got < 3; got++ {
		select {
		case <-fired:
		case <-timeout:
			t.Fatalf("loop fired %d times in 2s, want the immediate run plus ticks", got)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTickLoop did not return after ctx cancel")
	}

	// Without runFirst nothing happens until the first tick.
	lazy := 0
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	runTickLoop(ctx2, time.Hour, false, func() { lazy++ })
	if lazy != 0 {
		t.Fatalf("runFirst=false ran the body %d times before any tick", lazy)
	}
}

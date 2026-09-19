package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
)

// bootWatchersFake records what bootWatchersWith asked for and captures the
// hive.yaml reload callback so a test can run one reload by hand.
type bootWatchersFake struct {
	deps bootWatchersDeps
	log  bytes.Buffer

	watchedPath    string
	onReload       func(*config.Config)
	watcher        *config.Watcher
	started        *config.Watcher
	startedAgents  []string
	tokenRefreshes int
	hosts          []string
}

func newBootWatchersFake() *bootWatchersFake {
	f := &bootWatchersFake{}
	f.deps = bootWatchersDeps{
		newConfigWatcher: func(path string, onReload func(*config.Config), logger *slog.Logger) *config.Watcher {
			f.watchedPath, f.onReload = path, onReload
			f.watcher = config.NewWatcher(path, onReload, logger)
			return f.watcher
		},
		startConfigWatcher: func(_ context.Context, w *config.Watcher) { f.started = w },
		startAgent: func(_ context.Context, _ *agent.Manager, name string, _ *slog.Logger) {
			f.startedAgents = append(f.startedAgents, name)
		},
		refreshAgentTokens: func(context.Context, *agent.Manager) { f.tokenRefreshes++ },
		registerGitHubHost: func(host string) { f.hosts = append(f.hosts, host) },
	}
	return f
}

func bootWatchersConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-watchers-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widgets"}
	lvl := 3
	cfg.ACMMLevel = &lvl
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	cfg.RemovedAgents = []string{"retired"}
	return cfg
}

func newBootWatchersBoot(t *testing.T, f *bootWatchersFake, cfg *config.Config) (*boot, *int) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	refreshes := 0
	return &boot{
		ctx:              ctx,
		cfg:              cfg,
		logger:           logger,
		configPath:       filepath.Join(t.TempDir(), "hive.yaml"),
		gov:              governor.New(cfg.Governor, cfg.EnabledAgents(), logger),
		agentMgr:         agent.NewManager(cfg.EnabledAgents(), logger, agent.ProjectContext{}),
		dashSrv:          dashboard.NewServer(0, logger),
		refreshDashboard: func() { refreshes++ },
	}, &refreshes
}

func TestBootWatchersWith_WatchesTheLoadedConfigPathAndAllowlistsGitHub(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	b, _ := newBootWatchersBoot(t, f, cfg)

	b.bootWatchersWith(f.deps)

	if f.watchedPath != b.configPath {
		t.Fatalf("watching %q, want the path bootConfig loaded (%q)", f.watchedPath, b.configPath)
	}
	if f.started == nil || f.started != f.watcher {
		t.Fatal("the config watcher that was built is not the one that was started")
	}
	sort.Strings(f.hosts)
	if strings.Join(f.hosts, ",") != "api.github.com,github.com" {
		t.Fatalf("proxy hosts = %v, want github.com's API and web hosts", f.hosts)
	}
}

func TestBootWatchersWith_RegistersGHEHostsWithTheProxy(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	cfg.GitHub.APIURL = "https://ghe.example.test/api/v3"
	b, _ := newBootWatchersBoot(t, f, cfg)

	b.bootWatchersWith(f.deps)

	for _, h := range f.hosts {
		if h != "ghe.example.test" {
			t.Fatalf("proxy hosts = %v, want only the GHE host for a GHE-configured hive", f.hosts)
		}
	}
	if len(f.hosts) == 0 {
		t.Fatal("GHE host never registered — mode enforcement would not apply to it")
	}
}

func TestBootWatchersWith_ReloadPreservesRuntimeStateAndSwapsTheRest(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	b, refreshes := newBootWatchersBoot(t, f, cfg)
	b.bootWatchersWith(f.deps)
	if f.onReload == nil {
		t.Fatal("reload callback not installed")
	}

	// What a re-read of hive.yaml looks like: no HiveID (runtime-only), a
	// stale ACMM level, a tombstone of its own, a new repo, a new enabled
	// agent, a new on-demand agent, and the retired agent resurrected.
	staleLevel := 1
	newCfg := &config.Config{}
	newCfg.Project.Org = "acme"
	newCfg.Project.Repos = []string{"widgets", "gadgets"}
	newCfg.ACMMLevel = &staleLevel
	newCfg.RemovedAgents = []string{"also-retired"}
	newCfg.Agents = map[string]config.AgentConfig{
		"scanner":  {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
		"reviewer": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
		"oncall":   {Enabled: true, Backend: "copilot", Model: "gpt-5.4", OnDemand: true},
		"retired":  {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}

	f.onReload(newCfg)

	if cfg.HiveID != "boot-watchers-test" {
		t.Fatalf("HiveID = %q after reload, want the runtime value preserved", cfg.HiveID)
	}
	if cfg.ACMMLevel == nil || *cfg.ACMMLevel != 3 {
		t.Fatalf("ACMMLevel = %v after reload, want the manager's authoritative 3, not the file's stale 1", cfg.ACMMLevel)
	}
	if !sameStringSlice(cfg.Project.Repos, []string{"widgets", "gadgets"}) {
		t.Fatalf("repos = %v, want the reloaded list", cfg.Project.Repos)
	}
	// Tombstones are the union of both sides and the pruned agent stays gone
	// (#2439).
	if !cfg.IsAgentRemoved("retired") || !cfg.IsAgentRemoved("also-retired") {
		t.Fatalf("tombstones = %v, want union of live and reloaded", cfg.RemovedAgents)
	}
	if _, resurrected := cfg.Agents["retired"]; resurrected {
		t.Fatal("a tombstoned agent came back through the reload")
	}
	// Only the added, enabled, not-on-demand agent is launched.
	if strings.Join(f.startedAgents, ",") != "reviewer" {
		t.Fatalf("started %v after reload, want just reviewer (oncall is on-demand, scanner pre-existed, retired is tombstoned)", f.startedAgents)
	}
	if f.tokenRefreshes != 0 {
		t.Fatal("App auth rebuilt although the GitHub identity did not change")
	}
	if *refreshes != 1 {
		t.Fatalf("dashboard refreshed %d times after reload, want 1", *refreshes)
	}
	if !strings.Contains(f.log.String(), "reload: preserved removed-agents") {
		t.Fatal("tombstone-preservation debug line missing — it is the #2439 diagnostic")
	}
}

func TestBootWatchersWith_ReloadWithChangedIdentityRebuildsAuthOrFailsLoudly(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	b, _ := newBootWatchersBoot(t, f, cfg)
	b.bootWatchersWith(f.deps)

	// The identity changed, so a rebuild is attempted; the key file resolves
	// to a path that does not exist here, so it must fail with an ERROR line,
	// keep the previous client, and not deliver tokens for an auth that was
	// never built.
	newCfg := *cfg
	newCfg.Agents = map[string]config.AgentConfig{}
	newCfg.GitHub.AppID = 99
	newCfg.GitHub.InstallationID = 1234
	t.Setenv("GH_APP_KEY_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	f.onReload(&newCfg)

	if !strings.Contains(f.log.String(), "github app auth rebuild after config reload failed") {
		t.Fatalf("identity change did not attempt (and report) an auth rebuild:\n%s", f.log.String())
	}
	if f.tokenRefreshes != 0 || b.ghClient != nil || b.appAuth != nil {
		t.Fatal("a failed rebuild must leave the client/auth untouched and refresh no tokens")
	}
	if cfg.GitHub.AppID != 99 {
		t.Fatal("reloaded GitHub identity not swapped into the live config")
	}
}

func TestBootWatchersWith_ReloadWithChangedIdentityRebuildsAuth(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	b, _ := newBootWatchersBoot(t, f, cfg)
	b.bootWatchersWith(f.deps)

	keyFile := filepath.Join(t.TempDir(), "app.pem")
	writeTestAppKey(t, keyFile)
	t.Setenv("GH_APP_KEY_FILE", keyFile)
	newCfg := *cfg
	newCfg.Agents = map[string]config.AgentConfig{}
	newCfg.GitHub.AppID = 99
	newCfg.GitHub.InstallationID = 1234
	f.onReload(&newCfg)

	if b.ghClient == nil || b.appAuth == nil {
		t.Fatalf("client/auth not rebuilt after the identity changed:\n%s", f.log.String())
	}
	if f.tokenRefreshes != 1 {
		t.Fatalf("token refreshes = %d after rebuild, want 1 (#4072: immediate delivery)", f.tokenRefreshes)
	}
	if !strings.Contains(f.log.String(), "github app auth rebuilt after config reload") {
		t.Fatalf("rebuild not logged:\n%s", f.log.String())
	}

	// A second reload with the same identity must not rebuild again.
	same := newCfg
	f.onReload(&same)
	if f.tokenRefreshes != 1 {
		t.Fatal("unchanged identity triggered another rebuild")
	}
}

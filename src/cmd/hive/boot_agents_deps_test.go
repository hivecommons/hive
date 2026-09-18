package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// bootAgents builds the agent manager for real and hands every long-lived
// effect to bootAgentsDeps (#7571, step 2). These tests record those effects
// and assert on the phase's decisions: what is unconditional, what sits
// behind the usable-App gate, and what the manager was wired with.

type bootAgentsFake struct {
	deps bootAgentsDeps
	log  bytes.Buffer

	loopsMgr        *agent.Manager
	dirsPrepared    bool
	auditStarted    bool
	relays          *requestRelays
	relayClient     *github.Client
	sweepClient     *github.Client
	sweepMax        int
	sweepAllowed    bool
	sweepLevel      *int
	minterCalls     int
	permWatcherRuns int
}

func newBootAgentsFake() *bootAgentsFake {
	f := &bootAgentsFake{}
	f.deps = bootAgentsDeps{
		startAgentLoops:       func(_ context.Context, mgr *agent.Manager) { f.loopsMgr = mgr },
		prepareRequestDirs:    func(*slog.Logger) { f.dirsPrepared = true },
		startTokenAccessAudit: func(context.Context, *slog.Logger) { f.auditStarted = true },
		startRequestRelays: func(_ context.Context, c *github.Client, r requestRelays) {
			f.relayClient, f.relays = c, &r
		},
		startSelfAuthoredSweep: func(_ context.Context, c *github.Client, max int, allowed bool, level *int) {
			f.sweepClient, f.sweepMax, f.sweepAllowed, f.sweepLevel = c, max, allowed, level
		},
		buildMinter: func(*config.Config, *slog.Logger) (agent.AgentMintIssuer, error) {
			f.minterCalls++
			return nil, errors.New("no signing key in test")
		},
		startPermissionsWatcher: func(*slog.Logger) { f.permWatcherRuns++ },
	}
	return f
}

func newBootAgentsBoot(t *testing.T, f *bootAgentsFake, cfg *config.Config) *boot {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &boot{
		ctx:    ctx,
		cfg:    cfg,
		logger: logger,
		gov:    governor.New(cfg.Governor, cfg.EnabledAgents(), logger),
	}
}

func bootAgentsConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-agents-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widgets"}
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	cfg.Governor.Gateways = []config.GatewayConfig{{Name: "corp-litellm", Kind: "litellm", Endpoint: "http://gw.internal"}}
	return cfg
}

func TestBootAgentsWith_NoAppKeepsUnconditionalWiringAndSkipsRelays(t *testing.T) {
	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	b := newBootAgentsBoot(t, f, cfg)

	b.bootAgentsWith(f.deps)

	if b.agentMgr == nil {
		t.Fatal("agent manager not constructed")
	}
	if f.loopsMgr != b.agentMgr {
		t.Fatal("agent loops not started on the manager the phase published")
	}
	if !f.dirsPrepared || !f.auditStarted || f.permWatcherRuns != 1 {
		t.Fatalf("unconditional effects: dirs=%v audit=%v perm=%d, want all", f.dirsPrepared, f.auditStarted, f.permWatcherRuns)
	}
	if f.relays != nil || f.sweepClient != nil {
		t.Fatal("request relays / self-authored sweep started with no App client")
	}
	if f.minterCalls != 0 {
		t.Fatal("minter built with mint.enabled=false")
	}
	hooked := false
	for _, h := range b.preShutdownHooks.hooks {
		hooked = hooked || h.name == "archive-kick-logs"
	}
	if !hooked {
		t.Fatal("archive-kick-logs shutdown hook not registered")
	}
	// The gateway-backend predicate must be installed before any override
	// replay: a gateway-named backend is accepted, an unknown one is not.
	if err := b.agentMgr.SetBackendOverride("scanner", "corp-litellm"); err != nil {
		t.Fatalf("gateway-named backend override rejected: %v (predicate not wired, #3961)", err)
	}
	if err := b.agentMgr.SetBackendOverride("scanner", "no-such-gateway"); err == nil {
		t.Fatal("unknown backend override accepted")
	}
	if !strings.Contains(f.log.String(), "no bob api key configured") {
		t.Fatalf("bob key posture not logged:\n%s", f.log.String())
	}
}

func TestBootAgentsWith_UsableAppArmsRelaysAfterClientIsConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	cfg.GitHub.AppID = 4242
	cfg.GitHub.InstallationID = 99
	cfg.AutoMerge.MaxMerges = 3
	cfg.AutoMerge.RequiredChecks = []string{"build", "test"}
	b := newBootAgentsBoot(t, f, cfg)
	b.ghClient = github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, b.logger)

	b.bootAgentsWith(f.deps)

	if f.relays == nil || f.relayClient != b.ghClient {
		t.Fatal("request relays not started on the App client")
	}
	if f.relays.prOpen == nil || f.relays.issueOpen == nil || f.relays.review == nil || f.relays.merge == nil || f.relays.holdLabel == nil {
		t.Fatalf("a relay was armed without its authorizer: %+v", *f.relays)
	}
	// The PR relay's authorizer is the manager's own gate: an unknown agent
	// is refused, so a forged request file cannot open a PR.
	if err := f.relays.prOpen("ghost", 0); err == nil {
		t.Fatal("PR relay authorizer accepted an unknown agent")
	}
	if f.sweepClient != b.ghClient || f.sweepMax != 3 || f.sweepLevel != cfg.ACMMLevel {
		t.Fatalf("self-authored sweep: client ok=%v max=%d level=%v, want the App client, 3, cfg.ACMMLevel", f.sweepClient == b.ghClient, f.sweepMax, f.sweepLevel)
	}
	if f.sweepAllowed != cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel) {
		t.Fatalf("sweep acmmAllowed = %v, want the config's verdict", f.sweepAllowed)
	}
}

func TestBootAgentsWith_AppConfiguredButNotUsableKeepsRelaysOff(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	cfg.GitHub.AppID = 4242 // no InstallationID: HasUsableApp() is false
	b := newBootAgentsBoot(t, f, cfg)
	b.ghClient = github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, b.logger)

	b.bootAgentsWith(f.deps)

	if f.relays != nil || f.sweepClient != nil {
		t.Fatal("relays armed without an installation to author as")
	}
	if !f.dirsPrepared {
		t.Fatal("request dirs must exist even when the App is not yet usable, or agent requests are discarded")
	}
}

func TestBootAgentsWith_MintFailureIsWarnOnly(t *testing.T) {
	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	cfg.Mint.Enabled = true
	b := newBootAgentsBoot(t, f, cfg)

	b.bootAgentsWith(f.deps)

	if f.minterCalls != 1 {
		t.Fatalf("buildMinter called %d times, want 1", f.minterCalls)
	}
	if !strings.Contains(f.log.String(), "minter setup failed; agents keep App token only") {
		t.Fatalf("mint failure not logged as a warning:\n%s", f.log.String())
	}
	if b.agentMgr == nil {
		t.Fatal("mint failure aborted the phase")
	}
}

func TestBootAgentsWith_MintSuccessAttachesIssuer(t *testing.T) {
	f := newBootAgentsFake()
	f.deps.buildMinter = func(*config.Config, *slog.Logger) (agent.AgentMintIssuer, error) {
		f.minterCalls++
		return fakeMintIssuer{}, nil
	}
	cfg := bootAgentsConfig()
	cfg.Mint.Enabled = true
	cfg.Mint.Issuer = "https://mint.test"
	b := newBootAgentsBoot(t, f, cfg)

	b.bootAgentsWith(f.deps)

	if !strings.Contains(f.log.String(), "mint credential enabled") {
		t.Fatalf("mint success not logged:\n%s", f.log.String())
	}
}

type fakeMintIssuer struct{}

func (fakeMintIssuer) Enabled() bool                                 { return true }
func (fakeMintIssuer) MintAgentToken(string, string) (string, error) { return "tok", nil }

func TestDefaultBootAgentsDeps_IsFullyWired(t *testing.T) {
	d := defaultBootAgentsDeps()
	if d.startAgentLoops == nil || d.prepareRequestDirs == nil || d.startTokenAccessAudit == nil ||
		d.startRequestRelays == nil || d.startSelfAuthoredSweep == nil || d.buildMinter == nil ||
		d.startPermissionsWatcher == nil {
		t.Fatal("defaultBootAgentsDeps left a seam nil; production boot would panic")
	}
	// buildMinter's adapter must not smuggle a typed-nil pointer into the
	// interface on failure — SetAgentMint would then see a non-nil issuer.
	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	if issuer, err := d.buildMinter(cfg, restoreTestLogger()); err == nil || issuer != nil {
		t.Fatalf("buildMinter(no key) = (%v, %v), want (nil, error)", issuer, err)
	}
}

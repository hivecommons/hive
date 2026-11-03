package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cruxtest"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/worksource"
)

type bootDashboardFake struct {
	deps bootDashboardDeps

	port          int
	authToken     string
	srv           *dashboard.Server
	sessionsPath  string
	lifecyclePath string
	sessionsSrv   *dashboard.Server
	lifecycleSrv  *dashboard.Server
	slaPath       string
	slaSrv        *dashboard.Server
}

func newBootDashboardFake() *bootDashboardFake {
	f := &bootDashboardFake{}
	f.deps = bootDashboardDeps{
		newServer: func(port int, authToken string, logger *slog.Logger) *dashboard.Server {
			f.port, f.authToken = port, authToken
			f.srv = dashboard.NewServer(0, logger)
			return f.srv
		},
		enableSessionPersistence:    func(s *dashboard.Server, p string) { f.sessionsSrv, f.sessionsPath = s, p },
		enableLifecyclePersistence:  func(s *dashboard.Server, p string) { f.lifecycleSrv, f.lifecyclePath = s, p },
		enableSLAHistoryPersistence: func(s *dashboard.Server, p string) { f.slaSrv, f.slaPath = s, p },
	}
	return f
}

func bootDashboardConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-dashboard-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets"}
	cfg.Dashboard.Port = 8443
	cfg.Dashboard.AuthToken = "tok"
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	return cfg
}

func newBootDashboardBoot(t *testing.T, cfg *config.Config) *boot {
	t.Helper()
	b, _ := newDepsTestBoot(t, cfg)
	b.sched = scheduler.New(cfg, b.logger)
	b.agentMgr = cruxtest.NewRuntime(cfg.Agents, b.logger, crux.ProjectContext{})
	b.ghClient = fakeGitHubClient(t)
	b.beadStores = map[string]*beads.Store{}
	return b
}

func newTestBeadStore(t *testing.T) *beads.Store {
	t.Helper()
	st, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

type cmdStaticWorkSource struct {
	sourceType string
	issues     []worksource.Issue
}

func (s cmdStaticWorkSource) SourceType() string { return s.sourceType }
func (s cmdStaticWorkSource) ListIssues(context.Context) ([]worksource.Issue, error) {
	return append([]worksource.Issue(nil), s.issues...), nil
}

func TestBootDashboardWithConstructsAndPersistsOnPVC(t *testing.T) {
	cfg := bootDashboardConfig()
	b := newBootDashboardBoot(t, cfg)
	b.pendingTokenSeed = []dashboard.TokenSparklineEntry{{Timestamp: 1, Input: 10, Output: 5}}
	f := newBootDashboardFake()

	b.bootDashboardWith(f.deps)

	if f.port != 8443 || f.authToken != "tok" {
		t.Fatalf("server built with %d/%q", f.port, f.authToken)
	}
	if b.dashSrv != f.srv {
		t.Fatal("constructed server not handed off")
	}
	if f.sessionsSrv != f.srv || f.sessionsPath != dashboardSessionsPath || !strings.HasPrefix(f.sessionsPath, "/data/") {
		t.Fatalf("session persistence = %q on %p", f.sessionsPath, f.sessionsSrv)
	}
	if f.lifecycleSrv != f.srv || f.lifecyclePath != lifecycleTimelinePath || !strings.HasPrefix(f.lifecyclePath, "/data/") {
		t.Fatalf("lifecycle persistence = %q on %p", f.lifecyclePath, f.lifecycleSrv)
	}
	if f.slaSrv != f.srv || f.slaPath != slaHistoryPath || !strings.HasPrefix(f.slaPath, "/data/") {
		t.Fatalf("sla history persistence = %q on %p", f.slaPath, f.slaSrv)
	}
	if got := f.srv.TokenSparklineHistory(); len(got) != 1 || got[0].Input != 10 {
		t.Fatalf("token seed not applied: %+v", got)
	}
	audit, advisory, classifier := b.sched.IoscanHooksForTest()
	if audit == nil || advisory == nil {
		t.Fatal("scheduler ioscan hooks not wired")
	}
	if classifier {
		t.Fatal("classifier must not be installed when ioscan classifier is disabled")
	}
	// Shutdown hooks registered but not run: closing must be safe afterwards.
	b.cleanup.run()
	b.preShutdownHooks.run()
}

func TestBootDashboardWithAuditHookReachesDashboardLog(t *testing.T) {
	b := newBootDashboardBoot(t, bootDashboardConfig())
	f := newBootDashboardFake()

	b.bootDashboardWith(f.deps)

	audit, _, _ := b.sched.IoscanHooksForTest()
	audit("ioscan_blocked", "injection in title", "scanner")
	recent := f.srv.GetAudit().Recent(1)
	if len(recent) != 1 || recent[0].Action != "ioscan_blocked" || recent[0].Agent != "scanner" || recent[0].User != "scanner" {
		t.Fatalf("audit ring = %+v", recent)
	}
}

func TestBootDashboardWithWiresRunStageWorkSourceAccessor(t *testing.T) {
	worksource.SetRunStageAccessor(nil)
	t.Cleanup(func() { worksource.SetRunStageAccessor(nil) })

	cfg := bootDashboardConfig()
	cfg.Runs.Spektacular.Enabled = true
	cfg.Governor.WorkSource.RunStages = true
	b := newBootDashboardBoot(t, cfg)
	f := newBootDashboardFake()
	b.bootDashboardWith(f.deps)
	ws, err := worksource.AppendAdditive(cmdStaticWorkSource{sourceType: "github"}, cfg.Governor.WorkSource)
	if err != nil {
		t.Fatalf("AppendAdditive: %v", err)
	}
	if issues, err := ws.ListIssues(context.Background()); err == nil || !strings.Contains(err.Error(), "run lease registry unavailable") || len(issues) != 0 {
		t.Fatalf("wired accessor ListIssues = %+v, %v; want live dashboard accessor error before API registration", issues, err)
	}
}

func TestBootDashboardWithAdvisoryHookFallsBackAcrossStores(t *testing.T) {
	cases := []struct {
		name   string
		stores []string
		agent  string
		want   string
	}{
		{"own store", []string{"scanner", "fixer"}, "fixer", "fixer"},
		{"scanner fallback", []string{"scanner", "supervisor"}, "ghost", "scanner"},
		{"supervisor fallback", []string{"supervisor", "other"}, "ghost", "supervisor"},
		{"any store", []string{"other"}, "ghost", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBootDashboardBoot(t, bootDashboardConfig())
			for _, name := range tc.stores {
				b.beadStores[name] = newTestBeadStore(t)
			}
			b.bootDashboardWith(newBootDashboardFake().deps)

			_, advisory, _ := b.sched.IoscanHooksForTest()
			advisory("suspicious issue", "score 0.91", tc.agent)

			got := b.beadStores[tc.want].List(beads.ListFilter{})
			if len(got) != 1 || got[0].Title != "suspicious issue" || got[0].Type != beads.TypeAdvisory {
				t.Fatalf("store %q = %+v", tc.want, got)
			}
			if got[0].Metadata["ioscan_classifier"] != "score 0.91" {
				t.Fatalf("metadata = %v", got[0].Metadata)
			}
			for _, name := range tc.stores {
				if name != tc.want && len(b.beadStores[name].List(beads.ListFilter{})) != 0 {
					t.Fatalf("advisory leaked into store %q", name)
				}
			}
		})
	}
}

func TestBootDashboardWithAdvisoryHookNoStoresIsNoop(t *testing.T) {
	b := newBootDashboardBoot(t, bootDashboardConfig())
	b.bootDashboardWith(newBootDashboardFake().deps)
	_, advisory, _ := b.sched.IoscanHooksForTest()
	advisory("suspicious issue", "score 0.91", "ghost") // must not panic
}

func TestBootDashboardWithClassifier(t *testing.T) {
	// The fixture's agent runs on copilot, which boot would use as the
	// classifier CLI when no endpoint is configured (#14499); these cases
	// are about a hive with no usable backend at all, so they run on codex.
	noToollessAgents := func(cfg *config.Config) {
		t.Setenv(config.LiteLLMEndpointEnv, "")
		for name, a := range cfg.Agents {
			a.Backend = "codex"
			cfg.Agents[name] = a
		}
	}
	t.Run("no endpoint logs and skips", func(t *testing.T) {
		cfg := bootDashboardConfig()
		on := true
		cfg.Ioscan.Enabled = &on
		cfg.Ioscan.Classifier.Enabled = &on
		noToollessAgents(cfg)
		b := newBootDashboardBoot(t, cfg)
		// bootDashboardWith starts backfillRecentMergedPRs in a goroutine that
		// logs through b.logger, so the sink must be goroutine-safe (#13168).
		var log syncLogBuffer
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.sched = scheduler.New(cfg, b.logger)

		b.bootDashboardWith(newBootDashboardFake().deps)

		if _, _, installed := b.sched.IoscanHooksForTest(); installed {
			t.Fatal("classifier must not install without an endpoint")
		}
		if !strings.Contains(log.String(), "ioscan classifier enabled but not running") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
	t.Run("L5 default on, no endpoint installs fail-closed classifier", func(t *testing.T) {
		cfg := bootDashboardConfig()
		on := true
		cfg.Ioscan.Enabled = &on
		level := 5
		cfg.ACMMLevel = &level
		noToollessAgents(cfg)
		b := newBootDashboardBoot(t, cfg)
		var log syncLogBuffer
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.sched = scheduler.New(cfg, b.logger)

		b.bootDashboardWith(newBootDashboardFake().deps)

		if _, _, installed := b.sched.IoscanHooksForTest(); !installed {
			t.Fatal("L5 requires the classifier; an unavailable classifier must be installed so scans fail closed")
		}
		if !strings.Contains(log.String(), "required at this ACMM level but not running") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
	t.Run("L5, no endpoint, copilot agents installs the copilot CLI classifier", func(t *testing.T) {
		cfg := bootDashboardConfig()
		on := true
		cfg.Ioscan.Enabled = &on
		level := 5
		cfg.ACMMLevel = &level
		t.Setenv(config.LiteLLMEndpointEnv, "")
		b := newBootDashboardBoot(t, cfg)
		var log syncLogBuffer
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.sched = scheduler.New(cfg, b.logger)

		b.bootDashboardWith(newBootDashboardFake().deps)

		if _, _, installed := b.sched.IoscanHooksForTest(); !installed {
			t.Fatal("copilot agents with no endpoint must install the copilot CLI classifier")
		}
		if !strings.Contains(log.String(), "backend=copilot") || !strings.Contains(log.String(), "backend_from_agents=true") || strings.Contains(log.String(), "not running") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
	t.Run("L5 with classifier disabled explicitly skips", func(t *testing.T) {
		cfg := bootDashboardConfig()
		on, off := true, false
		cfg.Ioscan.Enabled = &on
		cfg.Ioscan.Classifier.Enabled = &off
		level := 5
		cfg.ACMMLevel = &level
		b := newBootDashboardBoot(t, cfg)
		var log syncLogBuffer
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.sched = scheduler.New(cfg, b.logger)

		b.bootDashboardWith(newBootDashboardFake().deps)

		if _, _, installed := b.sched.IoscanHooksForTest(); installed {
			t.Fatal("explicit classifier.enabled: false must win at L5")
		}
	})
	t.Run("endpoint installs cached classifier", func(t *testing.T) {
		cfg := bootDashboardConfig()
		on := true
		cfg.Ioscan.Enabled = &on
		cfg.Ioscan.Classifier.Enabled = &on
		cfg.Ioscan.Classifier.Model = "judge-small"
		cfg.Governor.Trajectory.Endpoint = "http://127.0.0.1:1/v1"
		b := newBootDashboardBoot(t, cfg)
		// bootDashboardWith starts backfillRecentMergedPRs in a goroutine that
		// logs through b.logger, so the sink must be goroutine-safe (#13168).
		var log syncLogBuffer
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.sched = scheduler.New(cfg, b.logger)

		b.bootDashboardWith(newBootDashboardFake().deps)

		if _, _, installed := b.sched.IoscanHooksForTest(); !installed {
			t.Fatal("classifier not installed")
		}
		if !strings.Contains(log.String(), "model=judge-small") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
}

func TestBootDashboardWithSandboxAuditCreatesRejectionBead(t *testing.T) {
	b := newBootDashboardBoot(t, bootDashboardConfig())
	b.beadStores["scanner"] = newTestBeadStore(t)
	f := newBootDashboardFake()

	b.bootDashboardWith(f.deps)

	b.agentMgr.(cruxtest.Runtime).SandboxAuditForTest("scanner", "sandbox_broker_rejected", "push to main refused")
	b.agentMgr.(cruxtest.Runtime).SandboxAuditForTest("scanner", "sandbox_failed", "oom")

	if recent := f.srv.GetAudit().Recent(2); len(recent) != 2 {
		t.Fatalf("audit ring = %+v", recent)
	}
	got := b.beadStores["scanner"].List(beads.ListFilter{})
	if len(got) != 1 || got[0].Metadata["sandbox_broker_rejection"] != "push to main refused" {
		t.Fatalf("beads = %+v", got)
	}
}

func TestBootDashboardWithNilGitHubClientSkipsAttributionSink(t *testing.T) {
	b := newBootDashboardBoot(t, bootDashboardConfig())
	b.ghClient = nil
	b.bootDashboardWith(newBootDashboardFake().deps) // must not panic
	if b.dashSrv == nil {
		t.Fatal("server not handed off")
	}
}

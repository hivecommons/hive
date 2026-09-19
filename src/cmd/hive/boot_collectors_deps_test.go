package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/tokens"
)

type bootCollectorsFake struct {
	deps bootCollectorsDeps

	tokenCollector *tokens.Collector
	tokenStop      <-chan struct{}
	started        []string
	persisted      map[string]string
	lookups        []string
	login          string
	lookupErr      error
	contributeSrv  *dashboard.Server
	actionable     []byte
	actionableErr  error
}

func newBootCollectorsFake() *bootCollectorsFake {
	f := &bootCollectorsFake{persisted: map[string]string{}, actionableErr: os.ErrNotExist}
	f.deps = bootCollectorsDeps{
		startTokenCollector: func(c *tokens.Collector, stop <-chan struct{}) { f.tokenCollector, f.tokenStop = c, stop },
		startCollector: func(_ context.Context, name string, c ctxCollector) {
			if c == nil {
				panic("nil collector " + name)
			}
			f.started = append(f.started, name)
		},
		enablePersistence: func(name string, _ pvcPersister, path string) { f.persisted[name] = path },
		lookupTokenLogin: func(token, apiURL string) (string, error) {
			f.lookups = append(f.lookups, token+"@"+apiURL)
			return f.login, f.lookupErr
		},
		startContributeMetrics: func(_ context.Context, srv *dashboard.Server) { f.contributeSrv = srv },
		readLastActionable:     func() ([]byte, error) { return f.actionable, f.actionableErr },
	}
	return f
}

func bootCollectorsConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.HiveID = "boot-collectors-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets"}
	cfg.Project.AIAuthor = "acme-bot"
	cfg.Data.MetricsDir = t.TempDir()
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	return cfg
}

func newBootCollectorsBoot(t *testing.T, cfg *config.Config) *boot {
	t.Helper()
	b, _ := newDepsTestBoot(t, cfg)
	b.gov = governor.New(cfg.Governor, cfg.EnabledAgents(), b.logger)
	b.agentMgr = agent.NewManager(cfg.Agents, b.logger, agent.ProjectContext{})
	b.ghClient = fakeGitHubClient(t)
	b.dashSrv = dashboard.NewServer(0, b.logger)
	b.beadStores = map[string]*beads.Store{}
	return b
}

func TestBootCollectorsWithStartsEveryCollectorAndPersistsOnPVC(t *testing.T) {
	cfg := bootCollectorsConfig(t)
	b := newBootCollectorsBoot(t, cfg)
	f := newBootCollectorsFake()

	b.bootCollectorsWith(f.deps)

	if f.tokenCollector == nil || f.tokenCollector != b.tokenCollector || f.tokenStop == nil {
		t.Fatal("token collector not started with a stop channel")
	}
	sort.Strings(f.started)
	if got := strings.Join(f.started, ","); got != "activity,fleet-stats,metrics,repo-cost" {
		t.Fatalf("started = %q", got)
	}
	for name, want := range map[string]string{
		"fleet-stats": fleetStatsPersistPath,
		"activity":    activityPersistPath,
		"repo-cost":   repoCostPersistPath,
	} {
		if got := f.persisted[name]; got != want || !strings.HasPrefix(got, "/data/") {
			t.Fatalf("persistence[%s] = %q, want %q", name, got, want)
		}
	}
	if f.contributeSrv != b.dashSrv {
		t.Fatal("contribute metrics not started on the boot's dashboard server")
	}
	if len(f.lookups) != 0 {
		t.Fatalf("configured ai_author must skip the token lookup, got %v", f.lookups)
	}
	if b.metricsCollector == nil || b.fleetStatsCollector == nil || b.activityCollector == nil || b.repoCostCollector == nil || b.refreshDashboard == nil {
		t.Fatal("collectors not handed off to boot")
	}
	// The token collector's stop channel is closed by the cleanup stack.
	b.cleanup.run()
	select {
	case <-f.tokenStop:
	default:
		t.Fatal("cleanup did not close the token collector stop channel")
	}
}

func TestBootCollectorsWithFleetStatsIdentityFallsBackToToken(t *testing.T) {
	cases := []struct {
		name      string
		login     string
		err       error
		wantLog   string
		wantCalls int
	}{
		{"lookup succeeds", "acme-bot-token", nil, "using bot token identity", 1},
		{"lookup fails", "", errors.New("401"), "bot identity lookup failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootCollectorsConfig(t)
			cfg.Project.AIAuthor = ""
			cfg.GitHub.Token = "ghp_test"
			b := newBootCollectorsBoot(t, cfg)
			var log strings.Builder
			b.logger = slog.New(slog.NewTextHandler(&log, nil))
			f := newBootCollectorsFake()
			f.login, f.lookupErr = tc.login, tc.err

			b.bootCollectorsWith(f.deps)

			if len(f.lookups) != tc.wantCalls || !strings.HasPrefix(f.lookups[0], "ghp_test@") {
				t.Fatalf("lookups = %v", f.lookups)
			}
			if !strings.Contains(log.String(), tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, log.String())
			}
		})
	}
}

func TestBootCollectorsWithNoAuthorLogsDisabled(t *testing.T) {
	cfg := bootCollectorsConfig(t)
	cfg.Project.AIAuthor = ""
	b := newBootCollectorsBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootCollectorsFake()

	b.bootCollectorsWith(f.deps)

	if len(f.lookups) != 0 {
		t.Fatalf("no token means no lookup, got %v", f.lookups)
	}
	if !strings.Contains(log.String(), "fleet stats collector disabled") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestBootCollectorsWithRestoresCachedActionable(t *testing.T) {
	cfg := bootCollectorsConfig(t)
	b := newBootCollectorsBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootCollectorsFake()
	cached := github.ActionableResult{GeneratedAt: time.Now().Add(-time.Minute)}
	cached.Issues.Count = 7
	cached.Issues.SLAViolations = 2
	cached.PRs.Count = 3
	cached.Hold.Total = 1
	f.actionable, _ = json.Marshal(cached)
	f.actionableErr = nil

	spark0 := len(b.dashSrv.TokenSparklineHistory())
	b.bootCollectorsWith(f.deps)

	if got := b.lastActionable.Load(); got == nil || got.Issues.Count != 7 {
		t.Fatalf("lastActionable = %+v", got)
	}
	st := b.gov.GetState()
	if st.QueueIssues != 7 || st.QueuePRs != 3 {
		t.Fatalf("governor queue not seeded: %+v", st)
	}
	// Every published status appends one token sparkline sample.
	if spark1 := len(b.dashSrv.TokenSparklineHistory()); spark1 != spark0+1 {
		t.Fatalf("refreshDashboard did not publish exactly one status: sparkline %d -> %d", spark0, spark1)
	}
	if !strings.Contains(log.String(), "restored cached actionable data") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestBootCollectorsWithIgnoresMissingOrCorruptCache(t *testing.T) {
	for name, mutate := range map[string]func(*bootCollectorsFake){
		"missing": func(f *bootCollectorsFake) { f.actionableErr = os.ErrNotExist },
		"corrupt": func(f *bootCollectorsFake) { f.actionable, f.actionableErr = []byte("{nope"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			b := newBootCollectorsBoot(t, bootCollectorsConfig(t))
			f := newBootCollectorsFake()
			mutate(f)

			b.bootCollectorsWith(f.deps)

			if b.lastActionable.Load() != nil {
				t.Fatal("lastActionable must stay nil")
			}
			if st := b.gov.GetState(); st.QueueIssues != 0 || st.QueuePRs != 0 {
				t.Fatalf("governor queue seeded from bad cache: %+v", st)
			}
		})
	}
}

func TestBootCollectorsWithRefreshDashboardPublishesStatus(t *testing.T) {
	b := newBootCollectorsBoot(t, bootCollectorsConfig(t))
	b.bootCollectorsWith(newBootCollectorsFake().deps)

	spark0 := len(b.dashSrv.TokenSparklineHistory())
	b.refreshDashboard()
	if spark1 := len(b.dashSrv.TokenSparklineHistory()); spark1 != spark0+1 {
		t.Fatalf("refreshDashboard did not publish: sparkline %d -> %d", spark0, spark1)
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// newEvalCycleAPI is a tolerant fake of the GitHub REST surface runEvalCycle
// touches on a quiet org: every list endpoint is empty, every object endpoint
// is a minimal document, and nothing ever errors. It records the paths hit so
// a test can assert the cycle actually enumerated rather than bailed early.
func newEvalCycleAPI(t *testing.T, hits *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1700000000},"search":{"limit":30,"remaining":30,"reset":1700000000}}}`))
		case strings.HasPrefix(r.URL.Path, "/search/"):
			_, _ = w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
		case strings.HasSuffix(r.URL.Path, "/repos/testorg/widget"):
			_, _ = w.Write([]byte(`{"name":"widget","full_name":"testorg/widget","default_branch":"main","owner":{"login":"testorg"}}`))
		case strings.Contains(r.URL.Path, "/issues") || strings.Contains(r.URL.Path, "/pulls") ||
			strings.Contains(r.URL.Path, "/commits") || strings.Contains(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// evalCycleFixture wires the real governor, scheduler, agent manager and
// dashboard server around a fake GitHub API, with zero agents so no kick can
// reach a tmux pane. Every optional collaborator is nil, which runEvalCycle
// must tolerate — production starts with several of them absent too.
type evalCycleFixture struct {
	cfg      *config.Config
	gh       *github.Client
	gov      *governor.Governor
	sched    *scheduler.Scheduler
	agentMgr *agent.Manager
	dashSrv  *dashboard.Server
	last     *atomic.Pointer[github.ActionableResult]
	hits     []string
}

func newEvalCycleFixture(t *testing.T) *evalCycleFixture {
	t.Helper()
	f := &evalCycleFixture{last: new(atomic.Pointer[github.ActionableResult])}
	api := newEvalCycleAPI(t, &f.hits)
	logger := restoreTestLogger()

	f.cfg = &config.Config{}
	f.cfg.HiveID = "eval-cycle-test"
	f.cfg.Project.Org = "testorg"
	f.cfg.Project.Repos = []string{"widget"}
	f.cfg.Agents = map[string]config.AgentConfig{}

	f.gh = github.NewClientForTest(api.URL, "testorg", []string{"widget"}, logger)
	f.gov = governor.New(f.cfg.Governor, f.cfg.Agents, logger)
	f.sched = scheduler.New(f.cfg, logger)
	f.agentMgr = agent.NewManager(f.cfg.Agents, logger, agent.ProjectContext{})
	f.dashSrv = dashboard.NewServer(0, logger)
	return f
}

func (f *evalCycleFixture) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runEvalCycle(ctx, f.cfg, f.gh, f.gov, f.sched, f.agentMgr, f.dashSrv,
		nil, // notifier
		map[string]*beads.Store{},
		nil, // tokenCollector
		nil, // metricsCollector
		nil, // nousState
		f.last,
		nil, // advisoryStore
		map[string]int{},
		nil, // restartedAgents
		restoreTestLogger())
}

// status reads back what the cycle published through the dashboard's own
// GET /api/status handler, so the assertion covers the same path the UI uses.
func (f *evalCycleFixture) status(t *testing.T) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	f.dashSrv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status body is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestRunEvalCycle_QuietOrgEnumeratesAndPublishesStatus(t *testing.T) {
	f := newEvalCycleFixture(t)

	f.run(t)

	if len(f.hits) == 0 {
		t.Fatal("runEvalCycle made no GitHub calls: it bailed before enumeration")
	}
	if got := f.last.Load(); got == nil {
		t.Fatal("lastActionable was not stamped after a successful enumeration")
	}
	st := f.status(t)
	if st["status"] == "initializing" {
		t.Fatalf("dashboard status was never published: %v", st)
	}
	if _, ok := st["statusSeq"]; !ok {
		t.Fatalf("published status lacks statusSeq: %v", st)
	}
}

func TestRunEvalCycle_SecondCycleAdvancesStatusSeq(t *testing.T) {
	f := newEvalCycleFixture(t)

	f.run(t)
	first := f.status(t)["statusSeq"].(float64)
	f.run(t)
	second := f.status(t)["statusSeq"].(float64)

	if second <= first {
		t.Fatalf("statusSeq did not advance across cycles: %v -> %v", first, second)
	}
}

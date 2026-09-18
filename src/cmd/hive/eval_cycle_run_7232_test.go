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
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`{"id":101,"body":"posted"}`))
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

	beadStores     map[string]*beads.Store
	advisoryIssues map[string]int
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
	f.beadStores = map[string]*beads.Store{}
	f.advisoryIssues = map[string]int{}

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
		f.beadStores,
		nil, // tokenCollector
		nil, // metricsCollector
		nil, // nousState
		f.last,
		nil, // advisoryStore
		f.advisoryIssues,
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

// TestRunEvalCycle_PostsAdvisoryDigestAndHealsAppAuthFinding drives the
// digest path end to end: a bead store holding one App-permission finding and
// a pre-resolved pinned advisory issue make the cycle build, pin, render and
// POST the digest; the successful App write is then the proof that retires the
// permission finding (#2575) and clears the App banner.
func TestRunEvalCycle_PostsAdvisoryDigestAndHealsAppAuthFinding(t *testing.T) {
	f := newEvalCycleFixture(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finding, err := store.Create("Insufficient repo permissions for the GitHub App", beads.TypeAdvisory, beads.PriorityHigh, "auditor", "")
	if err != nil {
		t.Fatal(err)
	}
	f.beadStores["auditor"] = store
	f.advisoryIssues["widget"] = 4

	f.run(t)

	var posted bool
	for _, h := range f.hits {
		if h == "POST /repos/testorg/widget/issues/4/comments" {
			posted = true
		}
	}
	if !posted {
		t.Fatalf("digest was never posted to the pinned issue; hits=%v", f.hits)
	}
	healed, err := store.Get(finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if healed.Status != beads.StatusClosed && healed.Status != beads.StatusDone {
		t.Fatalf("App-auth finding must be retired by a successful App digest post; status=%s", healed.Status)
	}
	st := f.status(t)
	if v, _ := st["githubAppRequired"].(bool); v {
		t.Fatalf("a successful App write must clear the App-required banner: %v", st["githubAppRequired"])
	}
}

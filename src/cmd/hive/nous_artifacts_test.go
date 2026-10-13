package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/nousartifacts"
	"github.com/hivecommons/hive/pkg/nousengine"
)

// fakeArtifactTicker is a hand-driven nousArtifactTicker so no test sleeps.
type fakeArtifactTicker struct {
	ch      chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func (f *fakeArtifactTicker) C() <-chan time.Time { return f.ch }
func (f *fakeArtifactTicker) Stop()               { f.once.Do(func() { close(f.stopped) }) }

// artifactGitHub is a minimal GitHub double for the whole mirror path:
// repo lookup, branch refs, GraphQL commit and PR create/dedupe.
type artifactGitHub struct {
	mu       sync.Mutex
	refs     map[string]string
	commits  int
	prs      int
	graphql  []map[string]any
	prBodies []map[string]any
}

func newArtifactGitHub(t *testing.T) (*artifactGitHub, *httptest.Server) {
	t.Helper()
	g := &artifactGitHub{refs: map[string]string{"main": "base0000"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/repos/acme/lab"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "lab", "private": true, "default_branch": "main"})
		case r.Method == http.MethodGet && strings.Contains(p, "/git/ref/heads/"):
			branch := p[strings.Index(p, "/git/ref/heads/")+len("/git/ref/heads/"):]
			sha, ok := g.refs[branch]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha, "type": "commit"}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/refs"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ref, _ := body["ref"].(string)
			sha, _ := body["sha"].(string)
			g.refs[strings.TrimPrefix(ref, "refs/heads/")] = sha
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/graphql"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.graphql = append(g.graphql, body)
			g.commits++
			_, _ = io.WriteString(w, `{"data":{"createCommitOnBranch":{"commit":{"oid":"c0ffee0000000000000000000000000000000000"}}}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/pulls"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/pulls"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.prBodies = append(g.prBodies, body)
			g.prs++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": g.prs, "html_url": "https://example.test/pr/1", "user": map[string]any{"login": "hive[bot]"}})
		default:
			t.Logf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func alwaysBudget() (bool, string) { return true, "" }

func writeFinishedCampaign(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * nousartifacts.QuietPeriod)
	for name, body := range map[string]string{
		"ledger.json":               `[{"action":"x"}]`,
		nousengine.MetaFindingsFile: `{"findings":[]}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestStartNousArtifactSyncDisabledPublishesOnly(t *testing.T) {
	state := &dashboard.NousState{Status: map[string]interface{}{"existing": 1}}
	s := startNousArtifactSync(context.Background(), state, config.ResolvedNousArtifactRepo{}, t.TempDir(), nil, nil, nil, nil, nil)
	if s != nil || state.ArtifactSync != nil {
		t.Fatal("disabled mirror must not build a syncer or expose the seam")
	}
	st, ok := state.Status[nousStatusArtifactRepoKey].(nousartifacts.Status)
	if !ok || st.Enabled || state.Status["existing"] != 1 {
		t.Fatalf("status = %+v", state.Status)
	}
}

func TestStartNousArtifactSyncWithoutGitHubClient(t *testing.T) {
	state := &dashboard.NousState{}
	resolved := config.ResolvedNousArtifactRepo{Enabled: true, Repo: "acme/lab", SyncInterval: time.Minute}
	if s := startNousArtifactSync(context.Background(), state, resolved, t.TempDir(), nil, nil, nil, nil, quietLogger()); s != nil {
		t.Fatal("no client → no syncer")
	}
	st := state.Status[nousStatusArtifactRepoKey].(nousartifacts.Status)
	if st.Enabled || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	publishNousArtifactStatus(nil, nousartifacts.Status{}) // nil-safe
}

func TestStartNousArtifactSyncEndToEnd(t *testing.T) {
	gh, srv := newArtifactGitHub(t)
	client := github.NewClientForTest(srv.URL, "acme", []string{"lab"}, quietLogger())
	root := t.TempDir()
	writeFinishedCampaign(t, root, "run-1")

	state := &dashboard.NousState{}
	var auditMu sync.Mutex
	var audits []string
	audit := func(user, action, detail, agent, repo string, target int) {
		auditMu.Lock()
		defer auditMu.Unlock()
		audits = append(audits, user+":"+action+":"+repo)
	}
	tick := &fakeArtifactTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolved := config.ResolvedNousArtifactRepo{Enabled: true, Repo: "acme/lab", Path: "campaigns", SyncInterval: time.Minute}
	s := startNousArtifactSync(ctx, state, resolved, root, client, alwaysBudget, audit, func(time.Duration) nousArtifactTicker { return tick }, quietLogger())
	if s == nil || state.ArtifactSync == nil {
		t.Fatal("enabled mirror must return a syncer and expose the seam")
	}

	// The goroutine's first pass pushes run-1; wait on status, not sleep.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := s.Status()
		if st.LastSyncAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror never synced: %+v", st)
		}
		runtime.Gosched()
	}
	gh.mu.Lock()
	commits, prs := gh.commits, gh.prs
	gh.mu.Unlock()
	if commits != 1 || prs != 1 {
		t.Fatalf("commits=%d prs=%d", commits, prs)
	}
	gh.mu.Lock()
	_, branchCreated := gh.refs[nousartifacts.BranchFor("run-1")]
	gh.mu.Unlock()
	if !branchCreated {
		t.Fatal("campaign branch not created")
	}
	auditMu.Lock()
	gotAudit := strings.Join(audits, ",")
	auditMu.Unlock()
	if !strings.Contains(gotAudit, nousartifacts.AuditUser+":"+nousartifacts.AuditActionPush+":acme/lab") {
		t.Fatalf("audit = %q", gotAudit)
	}

	// Owner "Sync now": nothing changed → unchanged, no new commit.
	res, err := state.ArtifactSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := res.(nousartifacts.Result); !ok || r.Unchanged != 1 || len(r.Pushed) != 0 {
		t.Fatalf("sync now = %+v", res)
	}

	// Status is mirrored under /api/nous/status.artifact_repo.
	state.Mu.Lock()
	st, ok := state.Status[nousStatusArtifactRepoKey].(nousartifacts.Status)
	state.Mu.Unlock()
	if !ok || !st.Enabled || st.Repo != "acme/lab" || st.LastSyncAt == nil {
		t.Fatalf("published status = %+v", st)
	}

	// A tick runs another (unchanged) pass; cancel stops the ticker.
	tick.ch <- time.Now()
	cancel()
	select {
	case <-tick.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("ticker not stopped after ctx cancel")
	}
}

func TestNousArtifactSyncSeamReportsSkipAsError(t *testing.T) {
	_, srv := newArtifactGitHub(t)
	client := github.NewClientForTest(srv.URL, "acme", []string{"lab"}, quietLogger())
	state := &dashboard.NousState{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // goroutine exits immediately
	tick := &fakeArtifactTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	resolved := config.ResolvedNousArtifactRepo{Enabled: true, Repo: "acme/lab", SyncInterval: time.Minute}
	s := startNousArtifactSync(ctx, state, resolved, t.TempDir(), client, func() (bool, string) { return false, "api budget mode conserve" }, nil, func(time.Duration) nousArtifactTicker { return tick }, quietLogger())
	if s == nil {
		t.Fatal("expected a syncer")
	}
	if _, err := state.ArtifactSync(context.Background()); err == nil || !strings.Contains(err.Error(), "conserve") {
		t.Fatalf("err = %v", err)
	}
}

func TestNousArtifactBudgetGate(t *testing.T) {
	ok, reason := nousArtifactBudgetGate(nil)()
	if !ok || reason != "" {
		t.Fatalf("nil client defaults to normal: %v %q", ok, reason)
	}
	if ticker := newRealArtifactTicker(time.Hour); ticker == nil {
		t.Fatal("nil ticker")
	} else {
		ticker.Stop()
	}
}

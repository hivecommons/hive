package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	prGateTestWait = 5 * time.Second
	prGateTestHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prGateTestRepo = "example-org/example-repo"
)

// fakePRGateGitHub answers the two calls the gate makes: re-reading the PR and
// writing a commit status. It records the statuses it was asked to write.
type fakePRGateGitHub struct {
	mu     sync.Mutex
	title  string
	reads  int
	posts  []map[string]string
	paths  []string
	posted chan struct{}
}

func (f *fakePRGateGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.ToLower(r.URL.Path)
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/4242"):
		f.mu.Lock()
		f.reads++
		title := f.title
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 4242, "state": "open", "draft": false, "title": title,
			"head": map[string]any{"sha": prGateTestHead},
		})
	case r.Method == http.MethodPost && strings.Contains(path, "/statuses/"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posts = append(f.posts, body)
		f.paths = append(f.paths, path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
		f.posted <- struct{}{}
	default:
		http.NotFound(w, r)
	}
}

func prGateTestServer(t *testing.T, cfg config.PRGateConfig) (*Server, *fakePRGateGitHub) {
	t.Helper()
	fake := &fakePRGateGitHub{title: "✨ feat: add thing", posted: make(chan struct{}, 8)}
	ghSrv := httptest.NewServer(fake)
	t.Cleanup(ghSrv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(0, logger)
	s.deps = &Dependencies{
		Config:   &config.Config{AutoMerge: config.AutoMergeConfig{PRGate: cfg}},
		GHClient: github.NewClientForTest(ghSrv.URL, "example-org", []string{"example-repo"}, logger),
		Ctx:      context.Background(),
	}
	return s, fake
}

func readPRGateFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr_gate/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPRGateWebhookModeOffDoesNothing(t *testing.T) {
	cases := map[string]config.PRGateConfig{
		"unset":                     {},
		"explicit off":              {Mode: "off", Repos: []string{prGateTestRepo}},
		"shadow, repo not in scope": {Mode: "shadow", Repos: []string{"example-org/other"}},
		"shadow, no repos":          {Mode: "shadow"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			s, fake := prGateTestServer(t, cfg)
			if got := s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json")); got != "" {
				t.Fatalf("outcome %q, want none", got)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.reads != 0 || len(fake.posts) != 0 {
				t.Fatalf("GitHub touched: reads=%d posts=%v", fake.reads, fake.posts)
			}
		})
	}
}

func TestPRGateWebhookShadowPostsOnlyTheShadowStatusOnTheEventHead(t *testing.T) {
	s, fake := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{prGateTestRepo}})
	if got := s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json")); got != "scheduled" {
		t.Fatalf("outcome %q, want scheduled", got)
	}
	select {
	case <-fake.posted:
	case <-time.After(prGateTestWait):
		t.Fatal("no status was posted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.posts) != 1 || fake.posts[0]["context"] != "verify PR contents (hive)" || fake.posts[0]["state"] != "success" {
		t.Fatalf("posts = %v, want only the shadow status", fake.posts)
	}
	if !strings.HasSuffix(fake.paths[0], "/statuses/"+prGateTestHead) {
		t.Errorf("status went to %s, want the event head %s", fake.paths[0], prGateTestHead)
	}
}

func TestPRGateWebhookComparesWorkflowCheckRun(t *testing.T) {
	s, fake := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{prGateTestRepo}})
	s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json"))
	select {
	case <-fake.posted:
	case <-time.After(prGateTestWait):
		t.Fatal("no status was posted")
	}
	if got := s.observePRGateEvent("check_run", readPRGateFixture(t, "check_run_completed.json")); got != "compared" {
		t.Fatalf("outcome %q, want compared", got)
	}
	st := s.prGateInstance().Snapshot()[prGateTestRepo]
	if st.Agreements != 1 || st.Disagreements != 0 {
		t.Fatalf("stats = %+v, want one agreement", st)
	}
}

func TestPRGateWebhookIgnoresOtherEvents(t *testing.T) {
	s, _ := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{prGateTestRepo}})
	for _, event := range []string{"push", "issues", "status", "pull_request_review"} {
		if got := s.observePRGateEvent(event, readPRGateFixture(t, "pull_request_opened.json")); got != "" {
			t.Errorf("event %s produced %q", event, got)
		}
	}
	closed := []byte(`{"action":"closed","number":1,"pull_request":{"number":1,"head":{"sha":"x"}},"repository":{"full_name":"example-org/example-repo"}}`)
	if got := s.observePRGateEvent("pull_request", closed); got != "" {
		t.Errorf("closed action produced %q", got)
	}
}

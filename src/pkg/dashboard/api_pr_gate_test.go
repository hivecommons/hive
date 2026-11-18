package dashboard

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/prgate"
)

const prGateTestWait = 5 * time.Second

type fakePRGateAPI struct {
	mu      sync.Mutex
	subject prgate.Subject
	posts   []string
	reads   int
	posted  chan struct{}
}

func (f *fakePRGateAPI) ReadSubject(context.Context, string, int) (prgate.Subject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.subject, nil
}

func (f *fakePRGateAPI) PostStatus(_ context.Context, _ string, _ int, _, statusContext, _, _ string) error {
	f.mu.Lock()
	f.posts = append(f.posts, statusContext)
	f.mu.Unlock()
	f.posted <- struct{}{}
	return nil
}

func prGateTestServer(t *testing.T, cfg config.PRGateConfig, api *fakePRGateAPI) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(0, logger)
	s.deps = &Dependencies{Config: &config.Config{AutoMerge: config.AutoMergeConfig{PRGate: cfg}}}
	s.prGateOnce.Do(func() {
		s.prGate = prgate.New(api, s.prGateModeOf, logger)
	})
	return s
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
	for name, cfg := range map[string]config.PRGateConfig{
		"unset":                     {},
		"explicit off":              {Mode: "off", Repos: []string{"example-org/example-repo"}},
		"shadow, repo not in scope": {Mode: "shadow", Repos: []string{"example-org/other"}},
		"shadow, no repos":          {Mode: "shadow"},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakePRGateAPI{posted: make(chan struct{}, 8)}
			s := prGateTestServer(t, cfg, api)
			if got := s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json")); got != "" {
				t.Fatalf("outcome %q, want none", got)
			}
			if api.reads != 0 || len(api.posts) != 0 {
				t.Fatalf("API touched: reads=%d posts=%v", api.reads, api.posts)
			}
		})
	}
}

func TestPRGateWebhookShadowPostsOnlyTheShadowStatus(t *testing.T) {
	api := &fakePRGateAPI{
		subject: prgate.Subject{HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Title: "✨ feat: add thing", State: "open"},
		posted:  make(chan struct{}, 8),
	}
	s := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{"example-org/example-repo"}}, api)
	if got := s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json")); got != "scheduled" {
		t.Fatalf("outcome %q, want scheduled", got)
	}
	select {
	case <-api.posted:
	case <-time.After(prGateTestWait):
		t.Fatal("no status was posted")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.posts) != 1 || api.posts[0] != prgate.ContextVerifyShadow {
		t.Fatalf("posts = %v, want only %q", api.posts, prgate.ContextVerifyShadow)
	}
}

func TestPRGateWebhookComparesWorkflowCheckRun(t *testing.T) {
	api := &fakePRGateAPI{
		subject: prgate.Subject{HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Title: "✨ feat: add thing", State: "open"},
		posted:  make(chan struct{}, 8),
	}
	s := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{"example-org/example-repo"}}, api)
	s.observePRGateEvent("pull_request", readPRGateFixture(t, "pull_request_opened.json"))
	<-api.posted
	if got := s.observePRGateEvent("check_run", readPRGateFixture(t, "check_run_completed.json")); got != "compared" {
		t.Fatalf("outcome %q, want compared", got)
	}
	st := s.prGate.Snapshot()["example-org/example-repo"]
	if st.Agreements != 1 || st.Disagreements != 0 {
		t.Fatalf("stats = %+v, want one agreement", st)
	}
}

func TestPRGateWebhookIgnoresOtherEvents(t *testing.T) {
	api := &fakePRGateAPI{posted: make(chan struct{}, 8)}
	s := prGateTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{"example-org/example-repo"}}, api)
	for _, event := range []string{"push", "issues", "status", "pull_request_review"} {
		if got := s.observePRGateEvent(event, readPRGateFixture(t, "pull_request_opened.json")); got != "" {
			t.Errorf("event %s produced %q", event, got)
		}
	}
	if got := s.observePRGateEvent("pull_request", []byte(`{"action":"closed","number":1,"pull_request":{"number":1,"head":{"sha":"x"}},"repository":{"full_name":"example-org/example-repo"}}`)); got != "" {
		t.Errorf("closed action produced %q", got)
	}
}

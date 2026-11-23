package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// backstopGitHub serves the backstop's list and status reads and hands the
// gate's own re-read and status write to fakePRGateGitHub.
type backstopGitHub struct {
	*fakePRGateGitHub
	mu       sync.Mutex
	requests []string
}

func (b *backstopGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.ToLower(r.URL.Path)
	b.mu.Lock()
	b.requests = append(b.requests, r.Method+" "+path)
	b.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls"):
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number": 4242, "state": "open", "draft": false, "title": "✨ feat: add thing",
			"head": map[string]any{"sha": prGateTestHead},
		}})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/commits/"+prGateTestHead+"/status"):
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "statuses": []any{}})
	default:
		b.fakePRGateGitHub.ServeHTTP(w, r)
	}
}

func backstopTestServer(t *testing.T, cfg config.PRGateConfig) (*Server, *backstopGitHub) {
	t.Helper()
	fake := &backstopGitHub{fakePRGateGitHub: &fakePRGateGitHub{title: "✨ feat: add thing", posted: make(chan struct{}, 8)}}
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

func TestPRGateBackstopModeOffMakesNoRequest(t *testing.T) {
	for name, cfg := range map[string]config.PRGateConfig{
		"unset":                               {},
		"explicit off":                        {Mode: "off", Repos: []string{prGateTestRepo}},
		"shadow, no repository":               {Mode: "shadow"},
		"shadow, another repository in scope": {Mode: "shadow", Repos: []string{"example-org/other"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := backstopTestServer(t, cfg)
			s.sweepPRGate(context.Background())
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for _, r := range fake.requests {
				if strings.Contains(r, "example-org/example-repo") {
					t.Errorf("request to the out-of-scope repository: %s", r)
				}
			}
			if name != "shadow, another repository in scope" && len(fake.requests) != 0 {
				t.Errorf("requests = %v, want none", fake.requests)
			}
		})
	}
}

// A head the webhook path never saw gets the shadow status from the sweep,
// on the exact listed head.
func TestPRGateBackstopPostsOnAMissedHead(t *testing.T) {
	s, fake := backstopTestServer(t, config.PRGateConfig{Mode: "shadow", Repos: []string{prGateTestRepo}})
	s.sweepPRGate(context.Background())
	fake.fakePRGateGitHub.mu.Lock()
	defer fake.fakePRGateGitHub.mu.Unlock()
	if len(fake.posts) != 1 || fake.posts[0]["context"] != "verify PR contents (hive)" {
		t.Fatalf("posts = %v, want the shadow status", fake.posts)
	}
	if !strings.HasSuffix(fake.paths[0], "/statuses/"+prGateTestHead) {
		t.Errorf("status went to %s", fake.paths[0])
	}
}

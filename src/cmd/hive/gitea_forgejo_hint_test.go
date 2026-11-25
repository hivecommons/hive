package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// versionServer serves /api/v1/version with status and body, recording each
// request's Authorization header.
type versionServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	auths  []string
}

func newVersionServer(t *testing.T, version string) *versionServer {
	t.Helper()
	s := &versionServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		status := s.status
		s.mu.Unlock()
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *versionServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

func (s *versionServer) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

// A gitea config pointed at Forgejo is reported once per instance; a real
// Gitea instance is not reported; the gitea token goes only to its block's
// host.
func TestGiteaForgejoHint(t *testing.T) {
	t.Setenv("HIVE_TEST_GITEA_TOKEN", "secret")
	gitea := newVersionServer(t, "1.22.3")
	forgejo := newVersionServer(t, "11.0.1+gitea-1.22.0")

	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Forge = config.ForgeGitea
	cfg.Project.Repos = []string{"app", "tea", "app"}
	cfg.Gitea.URL, cfg.Gitea.TokenEnv = gitea.URL, "HIVE_TEST_GITEA_TOKEN"
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "tea", Forge: config.ForgeGitea, ForgeURL: forgejo.URL}}

	var logs bytes.Buffer
	h := newGiteaForgejoHint(slog.New(slog.NewTextHandler(&logs, nil)))
	h.check(context.Background(), cfg)
	h.check(context.Background(), cfg)

	if got := strings.Count(logs.String(), "reports a Forgejo version"); got != 1 {
		t.Fatalf("hint logged %d times, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), forgejo.URL) || strings.Contains(logs.String(), gitea.URL) {
		t.Fatalf("hint must name only the Forgejo instance:\n%s", logs.String())
	}
	if got := gitea.requests(); len(got) != 1 || got[0] != "token secret" {
		t.Fatalf("gitea block host: requests %q, want one carrying the token", got)
	}
	if got := forgejo.requests(); len(got) != 1 || got[0] != "" {
		t.Fatalf("forge_url on another host: requests %q, want one without the token", got)
	}
}

// A probe that fails is retried on the next check rather than recorded.
func TestGiteaForgejoHintRetriesFailedProbe(t *testing.T) {
	srv := newVersionServer(t, "7.0.0+gitea-1.22.0")
	srv.setStatus(http.StatusBadGateway)

	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitea
	cfg.Gitea.URL = srv.URL

	var logs bytes.Buffer
	h := newGiteaForgejoHint(slog.New(slog.NewTextHandler(&logs, nil)))
	h.check(context.Background(), cfg)
	if strings.Contains(logs.String(), "reports a Forgejo version") {
		t.Fatalf("no hint expected from a failed probe:\n%s", logs.String())
	}
	srv.setStatus(http.StatusOK)
	h.check(context.Background(), cfg)
	h.check(context.Background(), cfg)
	if got := len(srv.requests()); got != 2 {
		t.Fatalf("requests = %d, want 2 (failed probe retried once, then answered)", got)
	}
	if got := strings.Count(logs.String(), "reports a Forgejo version"); got != 1 {
		t.Fatalf("hint logged %d times, want 1:\n%s", got, logs.String())
	}
}

// Only gitea repos are probed: a Forgejo or GitHub hive makes no request.
func TestGiteaProbeTargetsSkipsOtherKinds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeForgejo
	cfg.Project.Repos = []string{"app"}
	cfg.Forgejo.URL = "https://codeberg.org"
	cfg.Gitea.URL = "https://gitea.example.com"
	if got := giteaProbeTargets(cfg); len(got) != 0 {
		t.Fatalf("targets = %+v, want none", got)
	}
	if got := giteaProbeTargets(&config.Config{}); len(got) != 0 {
		t.Fatalf("github hive targets = %+v, want none", got)
	}
}

package hub

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hivegithub "github.com/hivecommons/hive/pkg/github"
)

// githubPRSource adapts the hub's GitHub client to reach.PRSource; the
// mapping must carry number/title/merge-commit/merged-at/files through
// unchanged and propagate fetch errors instead of half-built PRInfo.
func TestGitHubPRSourceMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/kubestellar/hive/pulls/9942", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9942,"title":"reach mapping","merged_at":"2026-08-10T12:00:00Z","merge_commit_sha":"beef9942"}`)
	})
	mux.HandleFunc("GET /repos/kubestellar/hive/pulls/9942/files", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"filename":"src/pkg/hub/saas.go"},{"filename":"docs/reach.md"}]`)
	})
	mux.HandleFunc("GET /repos/kubestellar/hive/pulls", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"number":9942,"title":"reach mapping","merged_at":"2026-08-10T12:00:00Z","merge_commit_sha":"beef9942"},
			{"number":9941,"title":"closed unmerged"}
		]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := hivegithub.NewClientForTest(server.URL, "kubestellar", []string{"hive"}, slog.Default())
	src := NewGitHubPRSource(client, "v4")

	info, err := src.MergedPR(context.Background(), 9942)
	if err != nil {
		t.Fatalf("MergedPR: %v", err)
	}
	if info.Number != 9942 || info.Title != "reach mapping" || info.MergeCommit != "beef9942" {
		t.Errorf("PRInfo = %+v, want the fake's facts", info)
	}
	if !info.MergedAt.Equal(time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("MergedAt = %v", info.MergedAt)
	}
	if len(info.Files) != 2 || info.Files[0] != "src/pkg/hub/saas.go" {
		t.Errorf("Files = %v", info.Files)
	}

	infos, err := src.RecentMergedPRs(context.Background(), 5)
	if err != nil {
		t.Fatalf("RecentMergedPRs: %v", err)
	}
	if len(infos) != 1 || infos[0].Number != 9942 || len(infos[0].Files) != 2 {
		t.Errorf("RecentMergedPRs = %+v, want just merged #9942 with its files", infos)
	}
}

func TestGitHubPRSourceErrorPropagation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	src := NewGitHubPRSource(
		hivegithub.NewClientForTest(server.URL, "kubestellar", []string{"hive"}, slog.Default()), "v4")

	if _, err := src.MergedPR(context.Background(), 9943); err == nil {
		t.Error("MergedPR must propagate the fetch error")
	}
	if _, err := src.RecentMergedPRs(context.Background(), 3); err == nil {
		t.Error("RecentMergedPRs must propagate the fetch error")
	}
}

// The lite repo-access HTTP client must block redirects into private address
// space and cap redirect chains — it fetches operator-supplied hosts.
func TestLiteRepoAccessHTTPClientRedirectPolicy(t *testing.T) {
	c := liteRepoAccessHTTPClient()
	if c.Timeout <= 0 {
		t.Error("client must carry a finite timeout")
	}

	req := httptest.NewRequest("GET", "http://127.0.0.1/priv", nil)
	if err := c.CheckRedirect(req, make([]*http.Request, 1)); err == nil {
		t.Error("redirect to a private address must be blocked")
	}
	pub := httptest.NewRequest("GET", "https://api.github.com/x", nil)
	if err := c.CheckRedirect(pub, make([]*http.Request, 10)); err == nil {
		t.Error("redirect chains past 10 hops must be stopped")
	}
}

// verifyGitHubRepoAccess must refuse a private/internal github_host before
// making any request (SSRF gate).
func TestVerifyGitHubRepoAccessRejectsPrivateHost(t *testing.T) {
	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "10.0.0.5", "o", "r")
	if ok || err == nil {
		t.Errorf("private host must be rejected, got ok=%v err=%v", ok, err)
	}
}

func TestNewDibsPublicChecker(t *testing.T) {
	c := newDibsPublicChecker(slog.Default())
	if c == nil || c.apiBase != "https://api.github.com" || c.client == nil || c.now == nil {
		t.Errorf("checker not fully initialized: %+v", c)
	}
	if cap(c.sem) != dibsPublicCheckParallel {
		t.Errorf("sem cap = %d, want %d", cap(c.sem), dibsPublicCheckParallel)
	}
}

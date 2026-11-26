package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// hivecommons/hive#19633: the boot-side halves of PUT /api/github-token.

func TestApplyRotatedGitHubToken_AdoptsPATClientAndRefreshesAgents(t *testing.T) {
	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	cfg.GitHub.Token = "ghp_new_token"
	b, _ := newBootWatchersBoot(t, f, cfg)
	if b.ghClient != nil {
		t.Fatal("precondition: no client before the rotation")
	}

	refreshes := 0
	b.applyRotatedGitHubTokenWith("ghp_new_token", func(context.Context, crux.Runtime) { refreshes++ })

	if b.ghClient == nil {
		t.Fatal("the rotated PAT client was not adopted")
	}
	if b.currentGitHubClient() != b.ghClient {
		t.Fatal("the provider still hands out the old client; consumers would keep the revoked token")
	}
	if refreshes != 1 {
		t.Fatalf("agent token refreshes = %d, want 1", refreshes)
	}
	if strings.Contains(f.log.String(), "ghp_new_token") {
		t.Fatal("token value leaked into the log")
	}
}

func TestVerifyRotatedGitHubToken_UsesConfiguredAPIAndRepos(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "token ghp_new_token" && r.Header.Get("Authorization") != "Bearer ghp_new_token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"rotator"}`))
		case r.URL.Path == "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"full_name":"acme/widgets"}`))
		case r.URL.Path == "/rate_limit":
			w.Header().Set("X-OAuth-Scopes", "repo, read:org, workflow")
			_, _ = w.Write([]byte(`{"resources":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	f := newBootWatchersFake()
	cfg := bootWatchersConfig()
	cfg.GitHub.APIURL = srv.URL
	b, _ := newBootWatchersBoot(t, f, cfg)

	login, err := b.verifyRotatedGitHubToken(context.Background(), "ghp_new_token")
	if err != nil {
		t.Fatalf("verify: %v (paths %v)", err, seen())
	}
	if login != "rotator" {
		t.Fatalf("login = %q, want rotator", login)
	}
	if !rotationPathSeen(seen(), "/repos/acme/widgets") {
		t.Fatalf("project repo was not checked for read access: %v", seen())
	}

	if _, err := b.verifyRotatedGitHubToken(context.Background(), "ghp_revoked"); err == nil {
		t.Fatal("a token GitHub refuses was accepted")
	}

	cfg.GitHub.AppID = 123456
	if _, err := b.verifyRotatedGitHubToken(context.Background(), "ghp_new_token"); err == nil {
		t.Fatal("an App-mode hive accepted a PAT rotation")
	}
}

func rotationPathSeen(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

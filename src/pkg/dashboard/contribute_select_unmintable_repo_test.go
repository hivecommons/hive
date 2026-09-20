package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// newRepoRejectingAppAuth mints for every repo except badRepo, which GitHub
// answers with the 422 a renamed / removed repository produces (#7869).
func newRepoRejectingAppAuth(t *testing.T, badRepo string) (*ghpkg.AppAuth, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		for _, repo := range req.Repositories {
			if repo == badRepo {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation.","errors":[]}`)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"tok-%s","expires_at":%q}`, req.Repositories, time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := ghpkg.NewAppAuthWithCache(1, 2, keyFile, filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth, &mints
}

func twoRepoStatus(s *Server) {
	issue := func(repo string, n int) any {
		return map[string]any{
			"number": float64(n), "title": "Actionable issue", "author": "someone-else",
			"url": fmt.Sprintf("https://github.com/myorg/%s/issues/%d", repo, n),
		}
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "review", Full: "myorg/review", ActionableIssues: []any{issue("review", 638)}},
		{Name: "testsuite", Full: "myorg/testsuite", ActionableIssues: []any{issue("testsuite", 866)}},
	}}
	s.statusMu.Unlock()
}

// #7869: a 422 on the token mint for ONE repo (renamed / removed from the
// installation) must not fail the whole selection — the hub excludes that repo
// and hands out the next candidate in the same call.
func TestSelectTask_RepoScopedMintFailureFallsThroughToNextRepo(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	auth, mints := newRepoRejectingAppAuth(t, "review")
	s.deps.GHAppAuth = auth

	conn := lockScopeConn(hub, "danathar", "c-dan")
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign after skipping the unmintable repo, got %+v", msg)
	}
	if msg.Repo != "myorg/testsuite" || msg.Number != 866 {
		t.Fatalf("expected the next repo's item, got %s#%d", msg.Repo, msg.Number)
	}
	if got := mints.Load(); got != 2 {
		t.Fatalf("expected exactly 2 mint attempts (bad repo, then good), got %d", got)
	}
	if !hub.repoUnmintable("myorg/review", time.Now()) {
		t.Fatal("the rejected repo should be excluded for the cooldown")
	}
	if hub.repoUnmintable("myorg/review", time.Now().Add(mintFailureRepoCooldown+time.Second)) {
		t.Fatal("the exclusion must lapse after mintFailureRepoCooldown")
	}
}

// #7869: on the next pass the excluded repo is skipped BEFORE any GitHub
// round-trip, so a fleet retrying every 30 s does not re-discover the failure.
func TestSelectTask_UnmintableRepoSkippedWithoutMint(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	auth, mints := newRepoRejectingAppAuth(t, "review")
	s.deps.GHAppAuth = auth
	hub.markRepoUnmintable("myorg/review", time.Now())

	msg := hub.selectTask(lockScopeConn(hub, "danathar", "c-dan"))
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/testsuite" {
		t.Fatalf("expected testsuite assignment, got %+v", msg)
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("excluded repo must not be minted for; got %d mint attempts", got)
	}
}

// #7869: when every candidate lives in an unmintable repo the reason names the
// repo list, not the credential, and the claim is rolled back.
func TestSelectTask_AllReposUnmintableReportsRepoUnmintable(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	auth, _ := newRepoRejectingAppAuth(t, "repo1")
	s.deps.GHAppAuth = auth

	conn := lockScopeConn(hub, "carol", "c-carol")
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableRepoUnmintable {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableRepoUnmintable, msg)
	}
	conn.mu.Lock()
	task := conn.currentTask
	conn.mu.Unlock()
	if task != nil {
		t.Fatalf("claim must be rolled back after a repo-scoped mint failure: %+v", task)
	}
}

// A non-repo failure (5xx) keeps the #2436 token_mint_failed contract and does
// not exclude the repo — the next pass must retry it.
func TestSelectTask_NonRepoMintFailureDoesNotExcludeRepo(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newFailingAppAuth(t)

	msg := hub.selectTask(lockScopeConn(hub, "carol", "c-carol"))
	if msg == nil || msg.Reason != taskUnavailableTokenMintFailed {
		t.Fatalf("expected %s, got %+v", taskUnavailableTokenMintFailed, msg)
	}
	if hub.repoUnmintable("myorg/repo1", time.Now()) {
		t.Fatal("a 5xx mint failure is not repo-scoped and must not exclude the repo")
	}
}

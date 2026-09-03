package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// oneActionableIssue installs a status with a single actionable issue that any
// tier can be assigned (author is someone else, so no #2390 own-work skew).
func oneActionableIssue(s *Server) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(101),
						"title":  "Actionable issue",
						"url":    "https://github.com/myorg/repo1/issues/101",
						"author": "someone-else",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()
}

// TestTrustControls_DisabledTierRefused (#2436 finding 2): a contributor whose
// TrustTier is in hub.disabled_tiers is refused with a tier_disabled negative-ack
// rather than silently handed work.
func TestTrustControls_DisabledTierRefused(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}

	// Fail-before shape: without disabled_tiers the tier gets assigned work.
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected a task_assign before disabling the tier, got %+v", msg)
	}
	// Clear the connection's held task and re-provide a fresh actionable issue so
	// the next select isn't skipped as an already-active issue.
	conn.currentTask = nil
	oneActionableIssue(s)

	// Now disable the newcomer tier: selection must refuse with tier_disabled.
	s.deps.Config.Hub.DisabledTiers = []string{"newcomer"}
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected an explicit negative-ack for a disabled tier, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableTierDisabled {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableTierDisabled, msg.Type, msg.Reason)
	}
}

// TestTrustControls_MaxConcurrentEnforced (#2436 finding 3): a second concurrent
// assignment to the SAME identity beyond tier_limits.max_concurrent is refused
// with a concurrency_limit negative-ack. Uses the live-connection holds as the
// source of truth.
func TestTrustControls_MaxConcurrentEnforced(t *testing.T) {
	hub, s := covK2Hub(t)

	// newcomer defaults to MaxConcurrent: 1 (config.go). Make it explicit so the
	// test does not depend on defaulting having run.
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer": {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
	}

	// Two live connections for the SAME identity (same ContributorID), simulating
	// one identity opening several connections. The first already holds a task.
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer"},
		currentTask: &WSTaskAssign{TaskID: "t-held", Repo: "myorg/other", Number: 5},
		lastPong:    time.Now(),
	}
	second := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-held"] = held
	hub.connections["conn-second"] = second
	hub.mu.Unlock()

	oneActionableIssue(s)

	// The identity already holds 1 == MaxConcurrent, so the second connection is
	// refused rather than given a second concurrent write-capable assignment.
	msg := hub.selectTask(second)
	if msg == nil {
		t.Fatalf("expected a concurrency-limit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableConcurrencyLimit {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableConcurrencyLimit, msg.Type, msg.Reason)
	}

	// Fail-before shape: with a higher limit the same second connection is served.
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer": {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 2},
	}
	oneActionableIssue(s)
	if msg := hub.selectTask(second); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("with MaxConcurrent=2 the second assignment should be served, got %+v", msg)
	}
}

// newFailingAppAuth builds a real *ghpkg.AppAuth whose JWT generation succeeds
// (valid RSA key) but whose CreateInstallationToken call hits a server that
// always 500s, so ScopedToken — and therefore mintScopedToken — returns an
// error. This exercises the #2436 finding 1 mint-failure path deterministically
// and offline.
func newFailingAppAuth(t *testing.T) *ghpkg.AppAuth {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := ghpkg.NewAppAuthWithCache(1, 2, keyFile,
		filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth
}

// TestTrustControls_MintFailureNegativeAck (#2436 finding 1): when the scoped
// token mint fails, selectTask sends an explicit token_mint_failed negative-ack
// instead of returning a silent nil that strands the contributor.
func TestTrustControls_MintFailureNegativeAck(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// Force the mint to fail via a real AppAuth whose token endpoint 500s.
	s.deps.GHAppAuth = newFailingAppAuth(t)

	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected an explicit token-mint negative-ack, got nil (silent strand)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableTokenMintFailed {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableTokenMintFailed, msg.Type, msg.Reason)
	}
	// The contributor was NOT left holding a phantom task.
	conn.mu.Lock()
	ct := conn.currentTask
	conn.mu.Unlock()
	if ct != nil {
		t.Fatalf("mint failure must not leave a currentTask assigned, got %+v", ct)
	}
}

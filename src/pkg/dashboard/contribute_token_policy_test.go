package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContributorTokenExpiryBoundary(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	policy := contributorTokenPolicy{maxAge: 90 * 24 * time.Hour}
	p := &ContributorProfile{RegistrationToken: "hash"}
	policy.issue(p, now)
	for _, tc := range []struct {
		name    string
		at      time.Time
		expired bool
	}{
		{"before", p.TokenExpiresAt.Add(-time.Nanosecond), false},
		{"exact", p.TokenExpiresAt, true},
		{"after", p.TokenExpiresAt.Add(time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.expired(p, tc.at); got != tc.expired {
				t.Fatalf("expired=%v want %v", got, tc.expired)
			}
		})
	}
	if policy.rotationDue(p, now.Add(policy.maxAge*4/5-time.Nanosecond)) {
		t.Fatal("warning too early")
	}
	if !policy.rotationDue(p, now.Add(policy.maxAge*4/5)) {
		t.Fatal("missing 80 percent warning")
	}
	policy.idleMax = time.Hour
	if policy.expired(p, now.Add(time.Hour-time.Nanosecond)) {
		t.Fatal("idle expiry too early")
	}
	if !policy.expired(p, now.Add(time.Hour)) {
		t.Fatal("idle expiry boundary accepted")
	}
	policy.idleMax = 0
	policy.maxAge = time.Hour
	if !policy.expired(p, now.Add(time.Hour)) {
		t.Fatal("policy reduction not enforced")
	}
}

func TestContributorTokenMigrationPersistsOnce(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	p := &ContributorProfile{GitHubUsername: "legacy", ContributorID: "c-legacy", RegistrationToken: sha256Hex("legacy-token"), RegisteredAt: "2020-01-01T00:00:00Z"}
	if err := saveContributorProfile(p); err != nil {
		t.Fatal(err)
	}
	policy := contributorTokenPolicy{maxAge: 90 * 24 * time.Hour}
	now := time.Now().UTC()
	if err := policy.migrate(p, now); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadContributorProfile(p.GitHubUsername)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TokenIssuedAt.Equal(now) || !reloaded.TokenExpiresAt.Equal(now.Add(policy.maxAge)) {
		t.Fatal("migration did not use migration time")
	}
	if err := policy.migrate(reloaded, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, err := loadContributorProfile(p.GitHubUsername)
	if err != nil {
		t.Fatal(err)
	}
	if !again.TokenIssuedAt.Equal(now) {
		t.Fatal("restart extended token lifetime")
	}
}

func TestContributorTokenExpiredHTTPAndRecovery(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	p, token := createContributorProfile("expired", contributorTokenPolicy{maxAge: -time.Hour})
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/token", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "reissue-token") || !strings.Contains(w.Body.String(), "hivectl contribute login") {
		t.Fatalf("expired response: %d %s", w.Code, w.Body.String())
	}
	fresh := reissueContributorToken(p)
	if fresh == "" {
		t.Fatal("reissue failed")
	}
	if got := profileForToken(s, fresh); got == nil {
		t.Fatal("fresh token rejected")
	}
	if got := profileForToken(s, token); got != nil {
		t.Fatal("old token survived reissue")
	}
}

func TestContributorTokenRevokeAllAndWriteGate(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	a, ta := createContributorProfile("alice")
	_, tb := createContributorProfile("bob")
	stale := *a
	for _, role := range []string{"", "read", "owner"} {
		req := httptest.NewRequest(http.MethodPost, "/api/contributors/tokens/revoke-all", nil)
		req.Header.Set("X-Hive-Role", role)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if role != "owner" {
			if w.Code != http.StatusForbidden {
				t.Fatalf("role %q: %d", role, w.Code)
			}
			p, err := loadContributorProfile("alice")
			if err != nil || p.RegistrationToken != sha256Hex(ta) {
				t.Fatal("unauthorized revoke modified token")
			}
		} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked":2`) {
			t.Fatalf("bulk revoke: %d %s", w.Code, w.Body.String())
		}
	}
	// Even the legacy zero-version save exemption must not resurrect a token.
	stale.Version = 0
	stale.TasksCompleted++
	if err := saveContributorProfile(&stale); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{ta, tb} {
		if profileForToken(s, token) != nil {
			t.Fatal("revoked token accepted")
		}
	}
	current, err := loadContributorProfile("alice")
	if err != nil {
		t.Fatal(err)
	}
	if current.TrustTier != "newcomer" {
		t.Fatal("token revocation changed trust")
	}
	if fresh := reissueContributorToken(current); fresh == "" || profileForToken(s, fresh) == nil {
		t.Fatal("GitHub reissue cannot recover from bulk revocation")
	}
}

func TestContributorTokenSingleRevokePreservesOtherTokens(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	_, ta := createContributorProfile("alice")
	_, tb := createContributorProfile("bob")
	req := httptest.NewRequest(http.MethodPost, "/api/contributors/alice/token/revoke", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke one: %d %s", w.Code, w.Body.String())
	}
	if profileForToken(s, ta) != nil {
		t.Fatal("revoked token accepted")
	}
	if profileForToken(s, tb) == nil {
		t.Fatal("unrelated credential revoked")
	}
}

func TestContributorTokenSessionBoundToHandshake(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	p, token := createContributorProfile("session")
	c := &ContributorConnection{profile: p, registrationTokenHash: sha256Hex(token)}
	h := &ContributeWSHub{}
	if !h.registrationTokenSessionValid(c) {
		t.Fatal("fresh session rejected")
	}
	fresh := reissueContributorToken(p)
	if fresh == "" {
		t.Fatal("reissue failed")
	}
	// The live profile may refresh after rotation. That must not silently replace
	// the session's proof of possession with the newly-issued credential.
	current, err := loadContributorProfile("session")
	if err != nil {
		t.Fatal(err)
	}
	c.profile = current
	if h.registrationTokenSessionValid(c) {
		t.Fatal("old session adopted rotated credential")
	}
	c.registrationTokenHash = sha256Hex(fresh)
	if !h.registrationTokenSessionValid(c) {
		t.Fatal("new session rejected")
	}
	current.TokenExpiresAt = time.Now().Add(-time.Second)
	if err := saveContributorProfileTokenCAS(current, false, true); err != nil {
		t.Fatal(err)
	}
	if h.registrationTokenSessionValid(c) {
		t.Fatal("connected session survived expiry")
	}
}

func TestContributorTokenPolicyEnvironment(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTE_TOKEN_MAX_AGE", "48h")
	t.Setenv("HIVE_CONTRIBUTE_TOKEN_IDLE_MAX", "2h")
	p := defaultContributorTokenPolicy()
	if p.maxAge != 48*time.Hour || p.idleMax != 2*time.Hour {
		t.Fatalf("wrong env policy: %+v", p)
	}
	t.Setenv("HIVE_CONTRIBUTE_TOKEN_MAX_AGE", "0")
	if defaultContributorTokenPolicy().maxAge != 90*24*time.Hour {
		t.Fatal("zero disabled mandatory expiry")
	}
}

// A last-used touch is bookkeeping: version drift from a concurrent writer must
// never turn a valid token into an authentication failure (#13811 review).
func TestContributorTokenLastUsedTouchToleratesVersionDrift(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	policy := contributorTokenPolicy{maxAge: 90 * 24 * time.Hour}
	p, token := createContributorProfile("drift", policy)
	stale, err := loadContributorProfile("drift")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour)
	stale.TokenLastUsedAt = old
	if err := saveContributorProfileTokenCAS(stale, false, true); err != nil {
		t.Fatal(err)
	}
	// An unrelated writer advances the version past every in-flight copy.
	bumped, err := loadContributorProfile("drift")
	if err != nil {
		t.Fatal(err)
	}
	bumped.TasksCompleted++
	if err := saveContributorProfileCAS(bumped, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	policy.touchLastUsed(p, sha256Hex(token), now)
	cur, err := loadContributorProfile("drift")
	if err != nil {
		t.Fatal(err)
	}
	if !cur.TokenLastUsedAt.Equal(now) || cur.TasksCompleted != bumped.TasksCompleted {
		t.Fatalf("touch lost data: last_used=%v tasks=%d", cur.TokenLastUsedAt, cur.TasksCompleted)
	}
	if p.Version == cur.Version {
		t.Fatal("drifted caller copy was promoted to the current version")
	}

	for i := 0; i < 2; i++ {
		got, reason := authenticateContributorToken(token, policy)
		if got == nil {
			t.Fatalf("auth %d after version drift: %s", i, reason)
		}
		next, err := loadContributorProfile("drift")
		if err != nil {
			t.Fatal(err)
		}
		next.TasksCompleted++
		if err := saveContributorProfileCAS(next, false); err != nil {
			t.Fatal(err)
		}
		cur = next
	}

	// The touch never writes once the token it authenticated has been replaced.
	if fresh := reissueContributorToken(cur, policy); fresh == "" {
		t.Fatal("reissue failed")
	}
	before, err := loadContributorProfile("drift")
	if err != nil {
		t.Fatal(err)
	}
	policy.touchLastUsed(p, sha256Hex(token), now.Add(time.Hour))
	after, err := loadContributorProfile("drift")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !after.TokenLastUsedAt.Equal(before.TokenLastUsedAt) {
		t.Fatal("touch wrote to a profile whose token was rotated")
	}
}

func TestContributorTokenConcurrentAuthenticationNeverFails(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	policy := contributorTokenPolicy{maxAge: 90 * 24 * time.Hour, idleMax: time.Hour}
	_, token := createContributorProfile("busy", policy)
	p, err := loadContributorProfile("busy")
	if err != nil {
		t.Fatal(err)
	}
	p.TokenLastUsedAt = time.Now().UTC().Add(-30 * time.Minute)
	if err := saveContributorProfileTokenCAS(p, false, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, reason := authenticateContributorToken(token, policy); got == nil {
				failures <- reason
			}
		}()
	}
	wg.Wait()
	close(failures)
	for reason := range failures {
		t.Errorf("valid token rejected under concurrency: %s", reason)
	}
	cur, err := loadContributorProfile("busy")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(cur.TokenLastUsedAt) > time.Minute {
		t.Fatalf("last-used not recorded: %v", cur.TokenLastUsedAt)
	}
}

func profileForToken(s *Server, token string) *ContributorProfile {
	p, _ := authenticateContributorToken(token, s.contributorTokenPolicy())
	return p
}

// A revoked contributor still authenticates to a profile so each caller can
// audit the refusal itself; the token-status endpoint must keep refusing it.
func TestContributorTokenRevokedProfileReturnedForCallerAudit(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	p, token := createContributorProfile("carol")
	p.TrustTier = "revoked"
	if err := saveContributorProfile(p); err != nil {
		t.Fatal(err)
	}
	got, reason := authenticateContributorToken(token, s.contributorTokenPolicy())
	if got == nil || got.TrustTier != "revoked" || reason != "" {
		t.Fatalf("revoked profile not returned to caller: %v %q", got, reason)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/token", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "revoked") {
		t.Fatalf("token status for revoked contributor: %d %s", w.Code, w.Body.String())
	}
}

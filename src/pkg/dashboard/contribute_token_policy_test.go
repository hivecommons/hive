package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if got := contributorProfileFromRegistrationToken(fresh); got == nil {
		t.Fatal("fresh token rejected")
	}
	if got := contributorProfileFromRegistrationToken(token); got != nil {
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
		if contributorProfileFromRegistrationToken(token) != nil {
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
	if fresh := reissueContributorToken(current); fresh == "" || contributorProfileFromRegistrationToken(fresh) == nil {
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
	if contributorProfileFromRegistrationToken(ta) != nil {
		t.Fatal("revoked token accepted")
	}
	if contributorProfileFromRegistrationToken(tb) == nil {
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

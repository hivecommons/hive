package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func covMiscServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// --- Pure helpers: SetGitBranch / upstreamBranch / stringField ---

func TestCovG_SetGitBranchAndUpstream(t *testing.T) {
	orig := versionBranch
	defer func() { versionBranch = orig }()

	SetGitBranch("feature-x")
	if versionBranch != "feature-x" {
		t.Fatalf("SetGitBranch did not set versionBranch")
	}
	if upstreamBranch() != "feature-x" {
		t.Fatalf("upstreamBranch should echo a known branch")
	}

	SetGitBranch("unknown")
	if upstreamBranch() != defaultUpstreamBranch {
		t.Fatalf("upstreamBranch should fall back to default for 'unknown'")
	}

	SetGitBranch("")
	if upstreamBranch() != defaultUpstreamBranch {
		t.Fatalf("upstreamBranch should fall back to default for empty")
	}
}

func TestCovG_StringField(t *testing.T) {
	m := map[string]interface{}{"a": "hello", "n": 42}
	if got := stringField(m, "a"); got != "hello" {
		t.Fatalf("stringField a = %q", got)
	}
	// Present but not a string → "".
	if got := stringField(m, "n"); got != "" {
		t.Fatalf("stringField n (non-string) = %q, want empty", got)
	}
	// Missing key → "".
	if got := stringField(m, "missing"); got != "" {
		t.Fatalf("stringField missing = %q, want empty", got)
	}
}

// --- buildSnapshot: node is absent / /data unwritable in tests, so this
// exercises the MkdirAll + exec error path (all errors are logged, not fatal). ---

func TestCovG_BuildSnapshot(t *testing.T) {
	s := covMiscServer(t)
	out := t.TempDir() + "/snap.html"
	// Runs `node build-snapshot.mjs ...`; node/script absent → the Warn branch.
	s.buildSnapshot(out, "light")
}

// --- SSO handoff ---

func TestCovG_HandleSSO(t *testing.T) {
	s := covMiscServer(t)
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv("HIVE_SSO_PUBLIC_KEY", "")
	t.Setenv("HIVE_SSO_PUBLIC_KEY_PREV", "")

	// No shared secret → terminal error page, NOT a redirect. Redirecting to
	// "/" here is what produced the infinite browser bounce.
	rec := httptest.NewRecorder()
	s.handleSSO(rec, httptest.NewRequest(http.MethodGet, "/sso", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-secret: expected 503, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("no-secret must not redirect, got Location %q", loc)
	}

	// Secret set but no token → 400.
	t.Setenv("HIVE_HUB_SECRET", "shhh")
	rec = httptest.NewRecorder()
	s.handleSSO(rec, httptest.NewRequest(http.MethodGet, "/sso", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-token: expected 400, got %d", rec.Code)
	}

	// Secret + bogus token → 401 (verification fails, bounces to login page).
	rec = httptest.NewRecorder()
	s.handleSSO(rec, httptest.NewRequest(http.MethodGet, "/sso?token=not-a-valid-token", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token: expected 401, got %d", rec.Code)
	}
}

// --- gh-user-auth session + logout ---

func TestCovG_HandleGHUserAuthSession(t *testing.T) {
	s := covMiscServer(t)
	rec := httptest.NewRecorder()
	s.handleGHUserAuthSession(rec, httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/session", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", rec.Code)
	}
}

func TestCovG_HandleGHUserAuthLogout(t *testing.T) {
	s := covMiscServer(t)

	// Logout with no session cookie is rejected before it can delete credentials.
	rec := httptest.NewRecorder()
	s.handleGHUserAuthLogout(rec, httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/logout", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout no-session: expected 401, got %d", rec.Code)
	}

	// Logout WITH a live session cookie exercises the lookup + delete branch.
	sid := s.createUserSession("alice", "owner")
	req := httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec = httptest.NewRecorder()
	s.handleGHUserAuthLogout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout with session: expected 200, got %d", rec.Code)
	}
	if s.lookupSession(sid) != nil {
		t.Fatalf("session should be deleted after logout")
	}
}

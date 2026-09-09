package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Tests for handleContributeReissueToken (api_contribute.go) — the
// authenticated token-rotation endpoint. Existing coverage only pinned the
// two 401 refusals (missing/invalid GitHub token); the branches this file
// pins are the ones that matter for account security:
//
//   - the SUCCESS path actually ROTATES: the response carries a new plaintext
//     token, the on-disk profile stores only its sha256 (never the plaintext),
//     TokenPlain is cleared, and the previous token's hash no longer matches;
//   - the legacy "token <t>" Authorization scheme is accepted alongside
//     "Bearer <t>";
//   - an authenticated but UNREGISTERED identity gets 404 (no profile is
//     conjured into existence);
//   - a REVOKED contributor gets 403 and — crucially — their stored
//     registration token is NOT rotated, so revocation cannot be laundered
//     into a fresh credential;
//   - an unrecognized Authorization scheme is treated as no token → 401.

// reissueEnv points contributor-profile storage at a temp dir and returns a
// server whose GitHub /user endpoint resolves to login (userMock semantics:
// login=="" → 401). Distinct GitHub tokens must be used per request because
// validateGitHubToken caches token→username process-wide (ghTokenCache).
func reissueEnv(t *testing.T, login string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	return v1Server(t, login), dir
}

func postReissue(s *Server, authHeader string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/reissue-token", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

// writeReissueProfile persists a minimal contributor profile directly into the
// temp contributors dir, bypassing saveContributorProfile so the test controls
// the exact starting bytes (including the stored token hash).
func writeReissueProfile(t *testing.T, dir string, p *ContributorProfile) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.GitHubUsername+".json"), b, 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

func readReissueProfile(t *testing.T, dir, username string) *ContributorProfile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, username+".json"))
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	var p ContributorProfile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	return &p
}

func TestReissueToken_BearerSuccessRotatesStoredHash(t *testing.T) {
	s, dir := reissueEnv(t, "rotator")

	oldPlain := "old-registration-token"
	writeReissueProfile(t, dir, &ContributorProfile{
		GitHubUsername:    "rotator",
		ContributorID:     "c-rot-1",
		RegistrationToken: sha256Hex(oldPlain),
		TokenPlain:        oldPlain,
		TrustTier:         "contributor",
	})

	rec := postReissue(s, "Bearer gh-reissue-bearer-ok")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	newPlain := resp["registration_token"]
	if newPlain == "" {
		t.Fatal("response missing registration_token")
	}
	if newPlain == oldPlain {
		t.Fatal("reissue returned the OLD token — no rotation happened")
	}
	if resp["contributor_id"] != "c-rot-1" {
		t.Fatalf("contributor_id = %q, want c-rot-1", resp["contributor_id"])
	}

	got := readReissueProfile(t, dir, "rotator")
	if got.RegistrationToken != sha256Hex(newPlain) {
		t.Fatalf("stored RegistrationToken = %q, want sha256 of the returned token", got.RegistrationToken)
	}
	if got.RegistrationToken == sha256Hex(oldPlain) {
		t.Fatal("old token hash still valid after reissue — previous credential was not invalidated")
	}
	if got.TokenPlain != "" {
		t.Fatalf("TokenPlain = %q after reissue, want empty — plaintext must not persist", got.TokenPlain)
	}
	if got.RegistrationToken == newPlain {
		t.Fatal("profile stores the PLAINTEXT token — must store only its sha256")
	}
}

func TestReissueToken_LegacyTokenSchemeAccepted(t *testing.T) {
	s, dir := reissueEnv(t, "legacy-scheme")

	writeReissueProfile(t, dir, &ContributorProfile{
		GitHubUsername:    "legacy-scheme",
		ContributorID:     "c-leg-1",
		RegistrationToken: sha256Hex("whatever"),
		TrustTier:         "contributor",
	})

	rec := postReissue(s, "token gh-reissue-legacy-ok")
	if rec.Code != http.StatusOK {
		t.Fatalf(`"token <t>" scheme: expected 200, got %d: %s`, rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["registration_token"] == "" {
		t.Fatal("legacy scheme succeeded without returning a new token")
	}
}

func TestReissueToken_UnregisteredIdentity404(t *testing.T) {
	s, dir := reissueEnv(t, "ghost")

	rec := postReissue(s, "Bearer gh-reissue-ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	// 404 must not conjure a profile into existence.
	if _, err := os.Stat(filepath.Join(dir, "ghost.json")); !os.IsNotExist(err) {
		t.Fatalf("profile file created for unregistered identity (stat err=%v)", err)
	}
}

func TestReissueToken_RevokedIdentity403AndNoRotation(t *testing.T) {
	s, dir := reissueEnv(t, "banned")

	storedHash := sha256Hex("revoked-old-token")
	writeReissueProfile(t, dir, &ContributorProfile{
		GitHubUsername:    "banned",
		ContributorID:     "c-ban-1",
		RegistrationToken: storedHash,
		TrustTier:         "revoked",
	})

	rec := postReissue(s, "Bearer gh-reissue-banned")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	got := readReissueProfile(t, dir, "banned")
	if got.RegistrationToken != storedHash {
		t.Fatal("revoked contributor's stored token changed — 403 path must not rotate")
	}
	if got.TrustTier != "revoked" {
		t.Fatalf("TrustTier = %q after refusal, want revoked", got.TrustTier)
	}
}

func TestReissueToken_UnknownAuthScheme401(t *testing.T) {
	s, dir := reissueEnv(t, "schemer")

	storedHash := sha256Hex("keep-me")
	writeReissueProfile(t, dir, &ContributorProfile{
		GitHubUsername:    "schemer",
		ContributorID:     "c-sch-1",
		RegistrationToken: storedHash,
		TrustTier:         "contributor",
	})

	// "Basic ..." is neither Bearer nor token — must be treated as no
	// credential at all, not passed to GitHub as-is.
	rec := postReissue(s, "Basic Z2gtcmVpc3N1ZQ==")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown scheme: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := readReissueProfile(t, dir, "schemer"); got.RegistrationToken != storedHash {
		t.Fatal("401 path rotated the stored token")
	}
}

package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// oci_fss.go — create/delete error branches (conn error, bad parse)
// ============================================================

func TestOCICreateConnError(t *testing.T) {
	injectOCIConfig(t, testOCIConfig(t))
	old := ociFSSEndpointFmt
	ociFSSEndpointFmt = "http://127.0.0.1:1%.0s" // closed port -> client.Do fails
	defer func() { ociFSSEndpointFmt = old }()

	if _, err := createOCIFileSystem("fs", slog.Default()); err == nil {
		t.Error("expected conn error creating FS")
	}
	if _, err := createOCIExport("fs", "/p", slog.Default()); err == nil {
		t.Error("expected conn error creating export")
	}
	if err := deleteOCIExport("e", slog.Default()); err == nil {
		t.Error("expected conn error deleting export")
	}
	if err := deleteOCIFileSystem("f", slog.Default()); err == nil {
		t.Error("expected conn error deleting FS")
	}
}

func TestOCICreateBadResponseParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if _, err := createOCIFileSystem("fs", slog.Default()); err == nil {
		t.Error("expected parse error on bad FS response")
	}
	if _, err := createOCIExport("fs", "/p", slog.Default()); err == nil {
		t.Error("expected parse error on bad export response")
	}
}

func TestCreateOCIFileSystemMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`)) // no id
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if _, err := createOCIFileSystem("fs", slog.Default()); err == nil {
		t.Error("expected error when FS response has no ID")
	}
}

// ============================================================
// reading_list.go — successful fetch populates the cache (source=medium)
// ============================================================

func TestReadingListMediumSource(t *testing.T) {
	resetRLCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"@type":"SocialMediaPosting","headline":"H","datePublished":"2026-05-01T00:00:00Z","mainEntityOfPage":"https://x/1","author":{"name":"A"}}`))
	}))
	defer srv.Close()
	old := readingListURLVar
	readingListURLVar = srv.URL
	defer func() { readingListURLVar = old }()

	s := &HubServer{logger: slog.Default()}
	arts, _, source := s.readingList()
	if source != readingListSourceMedium {
		t.Errorf("expected medium source after good fetch, got %q", source)
	}
	if len(arts) != 1 {
		t.Errorf("expected 1 article, got %d", len(arts))
	}
	resetRLCache(t)
}

// ============================================================
// sso.go — remaining verify branches (wrong secret, empty username)
// ============================================================

func TestVerifySSOTokenBranches(t *testing.T) {
	seed := SSOSigningSeedFromMaster("shared")
	pub := ssoPublicKeyFromSeed(seed)
	otherPub := ssoPublicKeyFromSeed(SSOSigningSeedFromMaster("other"))
	now := time.Now()
	tok := MintSSOToken(seed, "alice", "owner", "hiveA", now)
	if tok == "" {
		t.Fatal("mint failed")
	}

	// Wrong key -> bad signature.
	if _, _, err := VerifySSOToken(otherPub, tok, "hiveA", now); err == nil {
		t.Error("expected bad signature with the wrong public key")
	}
	// Empty key -> no verification key error.
	if _, _, err := VerifySSOToken("", tok, "hiveA", now); err == nil {
		t.Error("expected error with empty public key")
	}
	// Malformed token.
	if _, _, err := VerifySSOToken(pub, "no-dot", "hiveA", now); err == nil {
		t.Error("expected malformed token error")
	}
	// Wrong hive.
	if _, _, err := VerifySSOToken(pub, tok, "hiveB", now); err == nil {
		t.Error("expected wrong-hive error")
	}
	// Expired.
	if _, _, err := VerifySSOToken(pub, tok, "hiveA", now.Add(10*time.Minute)); err == nil {
		t.Error("expected expired error")
	}
	// Not yet valid.
	if _, _, err := VerifySSOToken(pub, tok, "hiveA", now.Add(-10*time.Minute)); err == nil {
		t.Error("expected not-yet-valid error")
	}
	// Valid round trip.
	u, role, err := VerifySSOToken(pub, tok, "hiveA", now)
	if err != nil || u != "alice" || role != "owner" {
		t.Errorf("valid verify failed: u=%q role=%q err=%v", u, role, err)
	}

	// Mint refuses empty inputs.
	if MintSSOToken("", "a", "r", "h", now) != "" {
		t.Error("mint should refuse empty seed")
	}
	if MintSSOToken(seed, "", "r", "h", now) != "" {
		t.Error("mint should refuse empty username")
	}
	if MintSSOToken(seed, "a", "r", "", now) != "" {
		t.Error("mint should refuse empty hive")
	}
}

// ============================================================
// oauth.go — handleAuthUser + handleLogout
// ============================================================

func TestHandleAuthUserAndLogout(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	// No cookie -> not authenticated.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	s.handleAuthUser(rec, req)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"authenticated":true`) {
		t.Error("expected unauthenticated without cookie")
	}

	// With a cookie for an existing user.
	mkUser(t, "octocat")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req.AddCookie(testAuthCookie("octocat"))
	s.handleAuthUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("auth user status = %d, want 200", rec.Code)
	}

	// Logout clears the cookie.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	s.handleLogout(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("logout status = %d, want 200", rec.Code)
	}
}

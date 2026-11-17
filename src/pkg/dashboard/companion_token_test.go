package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func serverWithCompanionTokens(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	old := companionTokenPath
	companionTokenPath = filepath.Join(t.TempDir(), "companion-tokens.json")
	t.Cleanup(func() { companionTokenPath = old })
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)
	s.SetMeetingsService(&fakeCompanionService{})
	return s, deps
}

var companionTokenRE = regexp.MustCompile(`hct_[0-9a-f]+`)

func companionTokenPagePost(t *testing.T, s *Server, path, user, role string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", role)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func mintCompanionTokenAs(t *testing.T, s *Server, user string) string {
	t.Helper()
	rec := companionTokenPagePost(t, s, companionTokenPagePath, user, "read-write")
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body.String())
	}
	token := companionTokenRE.FindString(rec.Body.String())
	if token == "" {
		t.Fatalf("no token on the page: %s", rec.Body.String())
	}
	return token
}

func TestCompanionTokenPageMintsShowsAndRevokes(t *testing.T) {
	s, _ := serverWithCompanionTokens(t)
	get := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, companionTokenPagePath, nil)
	req.Header.Set("X-Hive-User", "ana")
	req.Header.Set("X-Hive-Role", "read-write")
	s.mux.ServeHTTP(get, req)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "no companion token") {
		t.Fatalf("page = %d %s", get.Code, get.Body.String())
	}

	token := mintCompanionTokenAs(t, s, "ana")
	if !strings.HasPrefix(token, companionTokenPrefix) {
		t.Fatalf("token = %q", token)
	}
	raw, err := os.ReadFile(companionTokenPath)
	if err != nil || strings.Contains(string(raw), token) || !strings.Contains(string(raw), sha256Hex(token)) {
		t.Fatalf("stored = %s (%v)", raw, err)
	}
	if info, err := os.Stat(companionTokenPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v (%v)", info, err)
	}
	again := httptest.NewRecorder()
	s.mux.ServeHTTP(again, req)
	if body := again.Body.String(); strings.Contains(body, token) || !strings.Contains(body, "You have a token") {
		t.Fatalf("page after mint shows the token again or no status: %s", body)
	}

	if rec := companionTokenPagePost(t, s, companionTokenPagePath+"/revoke", "ana", "read-write"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "was revoked") {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := companionTokenStatus("ana"); ok {
		t.Fatal("token still stored after revoke")
	}
	if rec := companionTokenPagePost(t, s, companionTokenPagePath+"/revoke", "ana", "read-write"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "was revoked") {
		t.Fatalf("second revoke = %d", rec.Code)
	}

	if rec := companionTokenPagePost(t, s, companionTokenPagePath, "rita", "read"); rec.Code != http.StatusForbidden {
		t.Fatalf("read role mint = %d", rec.Code)
	}
	cross := httptest.NewRecorder()
	creq := httptest.NewRequest(http.MethodPost, companionTokenPagePath, nil)
	creq.Header.Set("X-Hive-User", "ana")
	creq.Header.Set("X-Hive-Role", "read-write")
	creq.Header.Set("Sec-Fetch-Site", "cross-site")
	s.mux.ServeHTTP(cross, creq)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross-site mint = %d", cross.Code)
	}
}

func TestCompanionTokenReachesOnlyTheCompanionRoutes(t *testing.T) {
	s, _ := serverWithCompanionTokens(t)
	token := mintCompanionTokenAs(t, s, "ana")
	h := s.Handler()
	do := func(method, path string, header string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if header == "authorization" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if header != "" {
			req.Header.Set(companionTokenHeader, header)
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(http.MethodGet, "/api/companion/info", token); code != http.StatusOK {
		t.Fatalf("info with token = %d", code)
	}
	if code := do(http.MethodPost, "/api/companion/meetings/mtg-c1/end", token); code != http.StatusOK {
		t.Fatalf("end with token = %d", code)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, companionTokenPagePath},
		{http.MethodPost, companionTokenPagePath},
		{http.MethodGet, "/api/meetings"},
		{http.MethodGet, "/api/status"},
		{http.MethodPost, "/api/pause"},
		{http.MethodGet, "/api/companion/meetingsx"},
	} {
		if code := do(tc.method, tc.path, token); code != http.StatusUnauthorized {
			t.Errorf("%s %s with companion token = %d, want 401", tc.method, tc.path, code)
		}
	}
	if code := do(http.MethodGet, "/api/companion/info", "authorization"); code != http.StatusUnauthorized {
		t.Errorf("token in Authorization = %d, want 401", code)
	}
	for _, bad := range []string{"hct_nope", "hwt_" + strings.TrimPrefix(token, companionTokenPrefix), "hct_"} {
		if code := do(http.MethodGet, "/api/companion/info", bad); code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", bad, code)
		}
	}
	if _, err := revokeCompanionToken("ana"); err != nil {
		t.Fatal(err)
	}
	if code := do(http.MethodGet, "/api/companion/info", token); code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", code)
	}
}

func TestCompanionTokenActsAtTheLiveRole(t *testing.T) {
	s, deps := serverWithCompanionTokens(t)
	token := mintCompanionTokenAs(t, s, "ana")
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/companion/info", nil)
		r.Header.Set(companionTokenHeader, token)
		return r
	}
	r := req()
	if !s.companionTokenAuthenticates(r) || r.Header.Get("X-Hive-User") != "ana" || r.Header.Get("X-Hive-Role") != "read-write" {
		t.Fatalf("identity = %q %q", r.Header.Get("X-Hive-User"), r.Header.Get("X-Hive-Role"))
	}
	deps.Config.Dashboard.AuthorizedUsers = []string{"octo", "ana:owner"}
	r = req()
	if !s.companionTokenAuthenticates(r) || r.Header.Get("X-Hive-Role") != "owner" || r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		t.Fatalf("upgraded identity = %q verified=%q", r.Header.Get("X-Hive-Role"), r.Header.Get(ownerRoleVerifiedHeader))
	}
	deps.Config.Dashboard.AuthorizedUsers = []string{"octo", "ana:read"}
	if s.companionTokenAuthenticates(req()) {
		t.Fatal("a user downgraded to read must not stream")
	}
	deps.Config.Dashboard.AuthorizedUsers = []string{"octo"}
	deps.Config.Dashboard.HubProxied = false
	if s.companionTokenAuthenticates(req()) {
		t.Fatal("a user removed from a direct-route allowlist must not stream")
	}
}

func TestCompanionTokenStoreEdges(t *testing.T) {
	old := companionTokenPath
	companionTokenPath = filepath.Join(t.TempDir(), "sub", "companion-tokens.json")
	t.Cleanup(func() { companionTokenPath = old })
	if _, _, ok := companionTokenOwner("hct_missing", time.Now()); ok {
		t.Fatal("no file must not authenticate")
	}
	tok, err := mintCompanionToken("ana", "read-write", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u, role, ok := companionTokenOwner(tok, time.Now()); !ok || u != "ana" || role != "read-write" {
		t.Fatalf("owner = %q %q %v", u, role, ok)
	}
	if rec, _ := companionTokenStatus("ana"); rec.LastUsedAt == "" {
		t.Fatal("last use not recorded")
	}
	if err := os.WriteFile(companionTokenPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := companionTokenOwner(tok, time.Now()); ok {
		t.Fatal("a corrupt store must fail closed")
	}
	if _, err := mintCompanionToken("ana", "read-write", time.Now()); err == nil {
		t.Fatal("minting over a corrupt store should fail")
	}
	if _, err := revokeCompanionToken("ana"); err == nil {
		t.Fatal("revoking from a corrupt store should fail")
	}
	if _, ok := companionTokenStatus("ana"); ok {
		t.Fatal("corrupt store has no status")
	}
	if err := os.WriteFile(companionTokenPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := loadCompanionTokens(); err != nil || f.Tokens == nil {
		t.Fatalf("empty store = %+v %v", f, err)
	}
	if !companionTokenAdmits("/api/companion/meetings") || companionTokenAdmits("/api/companion/token") {
		t.Fatal("admitted paths")
	}
}

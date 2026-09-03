package commands

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	tuiclient "github.com/hivecommons/hive/pkg/tui/client"
)

// TestMain pins this package's environment for the same reason
// pkg/tui/poll_test.go pins its variables: attachSession reads the developer's
// real HIVE_DASHBOARD_COOKIE and their real session cache, so without these a
// developer who has run `hivectl login` for a live hive would hand that
// session to every fixture server in this package — and the tests asserting
// on the Cookie header would pass against their credential rather than the
// one the test planted.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hivectl-commands-config-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic(err)
	}
	if err := os.Setenv(hivectl.CookieEnv, ""); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// isolatedStore redirects the session cache to a per-test directory and
// returns the store commands in this test will actually use.
func isolatedStore(t *testing.T) *hivectl.SessionStore {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := hivectl.DefaultSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// deviceFlowServer is a fixture hive whose /start and /poll complete a login
// on the first poll, minting sessionCookie. No GitHub, no browser, no network
// beyond the loopback httptest listener — per the epic's testing convention.
func deviceFlowServer(t *testing.T, sessionCookie string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/gh-user-auth/start":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":0}`))
		case "/api/gh-user-auth/poll":
			http.SetCookie(w, &http.Cookie{Name: "hive_session", Value: sessionCookie, MaxAge: 3600})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"complete","username":"octocat"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

// TestLoginCachesSessionAndNeverPrintsIt is the command's core contract, both
// halves of it: the operator sees the code, the URL and who they are — and the
// credential itself appears NOWHERE in the output, because a terminal
// scrollback (or a CI log of one) must never hold a dashboard login.
func TestLoginCachesSessionAndNeverPrintsIt(t *testing.T) {
	store := isolatedStore(t)
	server := deviceFlowServer(t, "s3ss10nvalue")
	defer server.Close()

	stdout, stderr, err := execute(t, server, "", "login")
	if err != nil {
		t.Fatalf("login = %v (stderr %q)", err, stderr)
	}

	for _, want := range []string{"WDJB-MJHT", "https://github.com/login/device", "octocat", store.Path()} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
	for name, output := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(output, "s3ss10nvalue") {
			t.Errorf("%s leaks the session credential:\n%s", name, output)
		}
	}

	sess, err := store.Load(server.URL)
	if err != nil || sess == nil {
		t.Fatalf("Load() after login = (%+v, %v), want the cached session", sess, err)
	}
	if sess.Cookie != "hive_session=s3ss10nvalue" {
		t.Errorf("cached cookie = %q, want %q", sess.Cookie, "hive_session=s3ss10nvalue")
	}
	if sess.Username != "octocat" {
		t.Errorf("cached username = %q, want octocat", sess.Username)
	}
}

// TestLoginDeniedSurfacesServerReason pins the allowlist rejection path end to
// end: the server's own explanation reaches the operator, and nothing is
// cached — a rejected login must not leave a credential-shaped file behind.
func TestLoginDeniedSurfacesServerReason(t *testing.T) {
	store := isolatedStore(t)
	const denial = "your GitHub account (mallory) is not authorized to access this hive. Contact the hive owner to request access."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/gh-user-auth/start":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":0}`))
		case "/api/gh-user-auth/poll":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"error","error":"` + denial + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "login")
	if err == nil || !strings.Contains(err.Error(), denial) {
		t.Fatalf("login = %v, want the server's denial verbatim", err)
	}
	if sess, err := store.Load(server.URL); err != nil || sess != nil {
		t.Fatalf("Load() after denied login = (%+v, %v), want (nil, nil)", sess, err)
	}
}

// TestSubcommandUsesCachedSession proves the point of caching at all: after a
// login, an ordinary subcommand presents the session with nothing exported.
func TestSubcommandUsesCachedSession(t *testing.T) {
	store := isolatedStore(t)
	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"IDLE"}`))
	}))
	defer server.Close()

	if err := store.Save(server.URL, hivectl.Session{
		Cookie: "hive_session=cached", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := execute(t, server, "", "system", "status"); err != nil {
		t.Fatalf("system status = %v", err)
	}
	if gotCookie != "hive_session=cached" {
		t.Errorf("Cookie = %q, want the cached session", gotCookie)
	}
}

// TestEnvCookieBeatsCache pins precedence: an explicitly exported
// HIVE_DASHBOARD_COOKIE always wins over the cache, so an operator's
// deliberate credential can never be silently overridden by an old login.
func TestEnvCookieBeatsCache(t *testing.T) {
	store := isolatedStore(t)
	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"IDLE"}`))
	}))
	defer server.Close()

	if err := store.Save(server.URL, hivectl.Session{
		Cookie: "hive_session=cached", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hivectl.CookieEnv, "hive_session=env-wins")

	if _, _, err := execute(t, server, "", "system", "status"); err != nil {
		t.Fatalf("system status = %v", err)
	}
	if gotCookie != "hive_session=env-wins" {
		t.Errorf("Cookie = %q, want the exported value to win over the cache", gotCookie)
	}
}

// TestRejectedCachedSessionNamesLogin is the acceptance criterion for the
// stale-credential path: a cached session the server no longer accepts — via
// expiry or revocation — must produce advice naming `hivectl login`, not a
// bare 401 that reads like a wrong token.
func TestRejectedCachedSessionNamesLogin(t *testing.T) {
	store := isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := store.Save(server.URL, hivectl.Session{
		Cookie: "hive_session=stale", ObtainedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := execute(t, server, "", "system", "status")
	if err == nil || !strings.Contains(err.Error(), "hivectl login") {
		t.Fatalf("system status = %v, want a 401 whose message names 'hivectl login'", err)
	}
}

// TestLogoutClearsCacheAndHitsEndpoint covers logout's two halves: the
// server-side session is ended through the existing endpoint, presenting the
// cached cookie so the server clears THAT session, and the local credential is
// removed.
func TestLogoutClearsCacheAndHitsEndpoint(t *testing.T) {
	store := isolatedStore(t)
	var gotPath, gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCookie = r.URL.Path, r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"logged_out"}`))
	}))
	defer server.Close()

	if err := store.Save(server.URL, hivectl.Session{
		Cookie: "hive_session=abc", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execute(t, server, "", "logout")
	if err != nil {
		t.Fatalf("logout = %v", err)
	}
	if gotPath != "/api/gh-user-auth/logout" {
		t.Errorf("path = %q, want /api/gh-user-auth/logout", gotPath)
	}
	if gotCookie != "hive_session=abc" {
		t.Errorf("Cookie = %q, want the session being ended", gotCookie)
	}
	if !strings.Contains(stdout, "Logged out") {
		t.Errorf("stdout = %q, want a logout confirmation", stdout)
	}
	if sess, err := store.Load(server.URL); err != nil || sess != nil {
		t.Fatalf("Load() after logout = (%+v, %v), want (nil, nil)", sess, err)
	}
}

// TestTUIExportsCachedSession pins the login→TUI handoff: the cache entry for
// the URL the TUI will dial is exported as HIVE_DASHBOARD_COOKIE — the TUI's
// own session lane — before the TUI starts. The cache key crosses the
// 127.0.0.1/localhost spelling gap deliberately: `hivectl login`'s --server
// default and the TUI's HIVE_DASHBOARD_URL default spell the same loopback
// dashboard differently, and a login under one must be found under the other.
func TestTUIExportsCachedSession(t *testing.T) {
	store := isolatedStore(t)
	t.Setenv(tuiclient.BaseURLEnv, "http://localhost:39999")
	t.Setenv(hivectl.CookieEnv, "")

	if err := store.Save("http://127.0.0.1:39999", hivectl.Session{
		Cookie: "hive_session=for-tui", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	exportCachedSessionForTUI()
	if got := os.Getenv(hivectl.CookieEnv); got != "hive_session=for-tui" {
		t.Errorf("%s = %q, want the cached session exported for the TUI", hivectl.CookieEnv, got)
	}
}

// TestTUIExportRespectsExplicitEnv pins precedence on the TUI path too: an
// operator's exported HIVE_DASHBOARD_COOKIE is never overwritten by the cache.
func TestTUIExportRespectsExplicitEnv(t *testing.T) {
	store := isolatedStore(t)
	t.Setenv(tuiclient.BaseURLEnv, "http://localhost:39999")
	t.Setenv(hivectl.CookieEnv, "hive_session=explicit")

	if err := store.Save("http://127.0.0.1:39999", hivectl.Session{
		Cookie: "hive_session=cached", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	exportCachedSessionForTUI()
	if got := os.Getenv(hivectl.CookieEnv); got != "hive_session=explicit" {
		t.Errorf("%s = %q, want the operator's exported value left alone", hivectl.CookieEnv, got)
	}
}

// TestLoginAndLogoutAreRegistered mirrors TestTUICommandIsRegistered's
// rationale: registration is one AddCommand line and exactly what a conflict
// resolution drops silently.
func TestLoginAndLogoutAreRegistered(t *testing.T) {
	_, _, root := newTestRoot()
	found := map[string]bool{}
	for _, command := range root.Commands() {
		found[command.Name()] = true
	}
	for _, name := range []string{"login", "logout"} {
		if !found[name] {
			t.Errorf("no `%s` subcommand registered on the hivectl root", name)
		}
	}
}

// TestLogoutClearsCacheWhenServerRefuses pins logout's failure policy: the
// local credential is removed even when the hive cannot confirm — it is the
// half only this command can clear, and the server-side session expires on its
// own — and the non-confirmation is REPORTED, not swallowed.
func TestLogoutClearsCacheWhenServerRefuses(t *testing.T) {
	store := isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"failed to remove persisted GitHub credentials"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := store.Save(server.URL, hivectl.Session{
		Cookie: "hive_session=abc", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execute(t, server, "", "logout")
	if err != nil {
		t.Fatalf("logout = %v, want success with the server failure reported in the output", err)
	}
	if !strings.Contains(stdout, "could not confirm") {
		t.Errorf("stdout = %q, want the unconfirmed-logout note", stdout)
	}
	if sess, err := store.Load(server.URL); err != nil || sess != nil {
		t.Fatalf("Load() after logout = (%+v, %v), want the cache cleared regardless", sess, err)
	}
}

// TestLoginReportsCacheWriteFailure pins the half-succeeded login: the device
// flow completed but the session could not be written. Saying nothing would
// leave the operator believing they are logged in until the next command 401s
// — the error must say the login worked and the CACHE failed.
func TestLoginReportsCacheWriteFailure(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	// Block the config subdirectory with a regular file so Save cannot create it.
	if err := os.WriteFile(configDir+"/hive", []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := deviceFlowServer(t, "s3ss10n")
	defer server.Close()

	_, _, err := execute(t, server, "", "login")
	if err == nil || !strings.Contains(err.Error(), "could not be cached") {
		t.Fatalf("login = %v, want an error saying the session could not be cached", err)
	}
	if strings.Contains(err.Error(), "s3ss10n") {
		t.Errorf("error %q leaks the session credential", err)
	}
}

// TestLogoutWithNothingCached pins the empty case as a calm no-op, not an
// error: logging out of a hive you never logged in to is not a failure.
func TestLogoutWithNothingCached(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s — with no credential there is no session to end", r.URL.Path)
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "logout")
	if err != nil {
		t.Fatalf("logout = %v", err)
	}
	if !strings.Contains(stdout, "No session to log out") {
		t.Errorf("stdout = %q, want the no-op explanation", stdout)
	}
}

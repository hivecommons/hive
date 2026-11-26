package hub

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// GitHub login link (#19992): a link is written only after the person proves
// both sides, the OIDC session (who started it) and GitHub OAuth (which login).

const (
	linkTestOIDCUser  = "custom:alice-sub"
	linkTestOtherUser = "custom:mallory-sub"
	linkTestOwner     = "custom:owner-sub"
	linkTestHiveID    = "hive-link-test"
)

func linkTestHub(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv(hubAdminsEnv, linkTestOwner)
	pendingGitHubLinksMu.Lock()
	pendingGitHubLinks = map[string]pendingGitHubLink{}
	pendingGitHubLinksMu.Unlock()
	for _, id := range []string{linkTestOIDCUser, linkTestOtherUser, linkTestOwner} {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: id, CanonicalID: id, Provider: "custom", Hives: map[string]string{}}); err != nil {
			t.Fatalf("save user %s: %v", id, err)
		}
	}
	return &HubServer{logger: slog.Default(), hubSecret: testHubSecret, keyGenerations: legacyGenerationSet(testHubSecret)}
}

// fakeGitHub serves the token and /user endpoints, answering login for /user.
func fakeGitHub(t *testing.T, login string) {
	t.Helper()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"gho_link"}`))
	}))
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"` + login + `"}`))
	}))
	t.Cleanup(tok.Close)
	t.Cleanup(usr.Close)
	oldTok, oldUsr := defaultGHTokenURL, defaultGHUserURL
	defaultGHTokenURL, defaultGHUserURL = tok.URL, usr.URL
	t.Cleanup(func() { defaultGHTokenURL, defaultGHUserURL = oldTok, oldUsr })
}

// startLink POSTs the start endpoint as user and returns the state nonce the
// hub handed this browser.
func startLink(t *testing.T, s *HubServer, user, expectedLogin string) (nonce string, rec *httptest.ResponseRecorder) {
	t.Helper()
	form := url.Values{}
	if expectedLogin != "" {
		form.Set("login", expectedLogin)
	}
	req := httptest.NewRequest(http.MethodPost, githubLinkStartPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://"+hubCanonicalHost())
	req.AddCookie(testAuthCookie(user))
	rec = httptest.NewRecorder()
	s.requireAuth(s.handleGitHubLinkStart)(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == githubLinkStateCookieName {
			nonce = c.Value
		}
	}
	return nonce, rec
}

// callback drives the shared OAuth callback for a link state in a browser
// that holds nonce and is signed in as user.
func callback(t *testing.T, s *HubServer, user, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	state := url.QueryEscape(nonce + oauthStateSeparator + githubLinkStateProvider + oauthStateSeparator)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: githubLinkStateCookieName, Value: nonce})
	if user != "" {
		req.AddCookie(testAuthCookie(user))
	}
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)
	return rec
}

func TestGitHubLink_ProvenLinkIsWrittenAndCarriedInSSO(t *testing.T) {
	s := linkTestHub(t)
	fakeGitHub(t, "alice-gh")

	nonce, rec := startLink(t, s, linkTestOIDCUser, "alice-gh")
	if rec.Code != http.StatusSeeOther || nonce == "" {
		t.Fatalf("start = %d (nonce %q), body %s", rec.Code, nonce, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape(githubLinkStateProvider)) {
		t.Fatalf("authorize URL does not carry a link state: %s", loc)
	}
	rec = callback(t, s, linkTestOIDCUser, nonce)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != githubLinkDoneRedirect {
		t.Fatalf("callback = %d %q, body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	u := loadSaaSUser(linkTestOIDCUser)
	if u.LinkedGitHubLogin != "alice-gh" || u.LinkedGitHubProvenAt.IsZero() {
		t.Fatalf("link not written: %+v", u)
	}
	// The link step must not sign the browser in as the GitHub account.
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			t.Fatal("a link callback must not mint a session")
		}
	}
	if loadSaaSUser("alice-gh") != nil {
		t.Fatal("a link callback must not create a GitHub user record")
	}
	// The SSO handoff carries the proven link to the spoke.
	link := ssoGitHubLink(linkTestOIDCUser)
	if link.Login != "alice-gh" || link.LinkedAt.IsZero() {
		t.Fatalf("ssoGitHubLink = %+v", link)
	}
	tok := spoke.MintSSOTokenWithGitHubLink(s.ssoSigningSeed(), linkTestOIDCUser, "owner", linkTestHiveID, link, time.Now())
	id, err := spoke.VerifySSOTokenIdentity(ssoPublicKeyFromSeed(s.ssoSigningSeed()), tok, linkTestHiveID, time.Now())
	if err != nil || id.GitHubLink.Login != "alice-gh" || id.Username != linkTestOIDCUser {
		t.Fatalf("token identity = %+v, err %v", id, err)
	}
	// The same state cannot be replayed.
	if rec := callback(t, s, linkTestOIDCUser, nonce); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", rec.Code)
	}
}

// An owner (hub admin) cannot assert a link for someone else.
func TestGitHubLink_OwnerAssertedLinkIsRefused(t *testing.T) {
	s := linkTestHub(t)
	for _, body := range []string{
		`{"username":"` + linkTestOIDCUser + `","login":"alice-gh"}`,
		`{"username":"` + linkTestOIDCUser + `","action":"link","login":"alice-gh"}`,
		`{"username":"` + linkTestOIDCUser + `","action":"unlink","login":"alice-gh"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, githubLinkAdminPath, bytes.NewBufferString(body))
		req.Header.Set("Origin", "https://"+hubCanonicalHost())
		req.AddCookie(testAuthCookie(linkTestOwner))
		rec := httptest.NewRecorder()
		s.requireAdmin(s.handleGitHubLinkAdmin)(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("owner-asserted link %s = %d, want 403", body, rec.Code)
		}
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" {
		t.Fatalf("owner assertion wrote a link: %q", u.LinkedGitHubLogin)
	}
	// An unproven value (as if written by anything but the OAuth step) is
	// never forwarded to a spoke.
	u := loadSaaSUser(linkTestOIDCUser)
	u.LinkedGitHubLogin = "alice-gh"
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	if link := ssoGitHubLink(linkTestOIDCUser); link.Login != "" {
		t.Fatalf("unproven link forwarded: %+v", link)
	}
}

// The GitHub account OAuth returns must be the one the person asked to link.
func TestGitHubLink_MismatchedOAuthIdentityIsRefused(t *testing.T) {
	s := linkTestHub(t)
	fakeGitHub(t, "someone-else")
	nonce, _ := startLink(t, s, linkTestOIDCUser, "alice-gh")
	rec := callback(t, s, linkTestOIDCUser, nonce)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched GitHub account = %d, want 403", rec.Code)
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" {
		t.Fatalf("mismatch wrote a link: %q", u.LinkedGitHubLogin)
	}
}

// Linking someone else: the callback must run in the session that started
// the link. Mallory completing GitHub OAuth in a browser signed in as Alice
// (or the reverse) is refused.
func TestGitHubLink_DifferentSessionAtCallbackIsRefused(t *testing.T) {
	s := linkTestHub(t)
	fakeGitHub(t, "mallory-gh")
	nonce, _ := startLink(t, s, linkTestOtherUser, "mallory-gh")
	rec := callback(t, s, linkTestOIDCUser, nonce)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback in another session = %d, want 403", rec.Code)
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" {
		t.Fatal("victim got linked to the attacker's GitHub login")
	}
	if u := loadSaaSUser(linkTestOtherUser); u.LinkedGitHubLogin != "" {
		t.Fatal("starter got linked although the callback ran elsewhere")
	}
	// No session at all is refused too.
	nonce, _ = startLink(t, s, linkTestOtherUser, "mallory-gh")
	if rec := callback(t, s, "", nonce); rec.Code != http.StatusForbidden {
		t.Fatalf("callback with no session = %d, want 403", rec.Code)
	}
	// A state the hub never issued is refused.
	if rec := callback(t, s, linkTestOtherUser, strings.Repeat("ab", oauthStateNonceBytes)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d, want 400", rec.Code)
	}
}

// One GitHub login backs at most one linked identity.
func TestGitHubLink_LoginAlreadyLinkedElsewhereIsRefused(t *testing.T) {
	s := linkTestHub(t)
	other := loadSaaSUser(linkTestOtherUser)
	other.LinkedGitHubLogin, other.LinkedGitHubProvenAt = "alice-gh", time.Now()
	if err := saveSaaSUser(other); err != nil {
		t.Fatal(err)
	}
	fakeGitHub(t, "alice-gh")
	nonce, _ := startLink(t, s, linkTestOIDCUser, "alice-gh")
	if rec := callback(t, s, linkTestOIDCUser, nonce); rec.Code != http.StatusConflict {
		t.Fatalf("already linked elsewhere = %d, want 409", rec.Code)
	}
}

func TestGitHubLink_StartRefusals(t *testing.T) {
	s := linkTestHub(t)
	mkUser(t, "octocat")
	if _, rec := startLink(t, s, linkTestOIDCUser, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing login = %d, want 400", rec.Code)
	}
	if _, rec := startLink(t, s, "octocat", "octocat"); rec.Code != http.StatusBadRequest {
		t.Fatalf("GitHub sign-in start = %d, want 400", rec.Code)
	}
	if _, rec := startLink(t, s, linkTestOIDCUser, "bad login!"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid login = %d, want 400", rec.Code)
	}
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "")
	if _, rec := startLink(t, s, linkTestOIDCUser, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no GitHub OAuth = %d, want 503", rec.Code)
	}
	// Cross-site POST is refused before anything starts.
	req := httptest.NewRequest(http.MethodPost, githubLinkStartPath, nil)
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(testAuthCookie(linkTestOIDCUser))
	rec := httptest.NewRecorder()
	s.requireAuth(s.handleGitHubLinkStart)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site start = %d, want 403", rec.Code)
	}
}

func TestGitHubLink_Unlink(t *testing.T) {
	s := linkTestHub(t)
	set := func() {
		u := loadSaaSUser(linkTestOIDCUser)
		u.LinkedGitHubLogin, u.LinkedGitHubProvenAt = "alice-gh", time.Now()
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
	}
	// Self unlink.
	set()
	req := httptest.NewRequest(http.MethodPost, githubLinkUnlinkPath, nil)
	req.Header.Set("Origin", "https://"+hubCanonicalHost())
	req.AddCookie(testAuthCookie(linkTestOIDCUser))
	rec := httptest.NewRecorder()
	s.requireAuth(s.handleGitHubLinkUnlink)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("self unlink = %d", rec.Code)
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" || !u.LinkedGitHubProvenAt.IsZero() {
		t.Fatalf("self unlink left %+v", u)
	}
	if link := ssoGitHubLink(linkTestOIDCUser); link.Login != "" {
		t.Fatal("unlinked login still forwarded")
	}
	// Owner unlink.
	set()
	req = httptest.NewRequest(http.MethodPost, githubLinkAdminPath,
		bytes.NewBufferString(`{"username":"`+linkTestOIDCUser+`","action":"unlink"}`))
	req.Header.Set("Origin", "https://"+hubCanonicalHost())
	req.AddCookie(testAuthCookie(linkTestOwner))
	rec = httptest.NewRecorder()
	s.requireAdmin(s.handleGitHubLinkAdmin)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner unlink = %d %s", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" {
		t.Fatalf("owner unlink left %q", u.LinkedGitHubLogin)
	}
	// A non-admin cannot use the admin endpoint.
	req = httptest.NewRequest(http.MethodPost, githubLinkAdminPath,
		bytes.NewBufferString(`{"username":"`+linkTestOIDCUser+`","action":"unlink"}`))
	req.Header.Set("Origin", "https://"+hubCanonicalHost())
	req.AddCookie(testAuthCookie(linkTestOtherUser))
	rec = httptest.NewRecorder()
	s.requireAdmin(s.handleGitHubLinkAdmin)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin unlink = %d, want 403", rec.Code)
	}
	// Unknown user.
	req = httptest.NewRequest(http.MethodPost, githubLinkAdminPath,
		bytes.NewBufferString(`{"username":"custom:nobody","action":"unlink"}`))
	req.Header.Set("Origin", "https://"+hubCanonicalHost())
	req.AddCookie(testAuthCookie(linkTestOwner))
	rec = httptest.NewRecorder()
	s.requireAdmin(s.handleGitHubLinkAdmin)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user unlink = %d, want 404", rec.Code)
	}
}

func TestGitHubLink_Status(t *testing.T) {
	s := linkTestHub(t)
	u := loadSaaSUser(linkTestOIDCUser)
	u.LinkedGitHubLogin, u.LinkedGitHubProvenAt = "alice-gh", time.Now()
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, githubLinkStatusPath, nil)
	req.AddCookie(testAuthCookie(linkTestOIDCUser))
	rec := httptest.NewRecorder()
	s.requireAuth(s.handleGitHubLinkStatus)(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"github_login":"alice-gh"`) || !strings.Contains(rec.Body.String(), `"linkable":true`) {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPendingGitHubLinks_ExpireAndCap(t *testing.T) {
	pendingGitHubLinksMu.Lock()
	pendingGitHubLinks = map[string]pendingGitHubLink{}
	pendingGitHubLinksMu.Unlock()
	now := time.Now()
	if !putPendingGitHubLink("old", pendingGitHubLink{CanonicalID: linkTestOIDCUser, Expires: now.Add(-time.Second)}, now.Add(-time.Minute)) {
		t.Fatal("put failed")
	}
	if _, ok := takePendingGitHubLink("old", now); ok {
		t.Fatal("expired pending link must not be returned")
	}
	pendingGitHubLinksMu.Lock()
	for i := 0; i < maxPendingGitHubLinks; i++ {
		pendingGitHubLinks["n"+strconv.Itoa(i)] = pendingGitHubLink{Expires: now.Add(time.Hour)}
	}
	pendingGitHubLinksMu.Unlock()
	if putPendingGitHubLink("new", pendingGitHubLink{Expires: now.Add(time.Hour)}, now) {
		t.Fatal("a full pending map must refuse new links")
	}
	pendingGitHubLinksMu.Lock()
	pendingGitHubLinks = map[string]pendingGitHubLink{}
	pendingGitHubLinksMu.Unlock()
}

// A link state is checked only against the host-only link cookie. The
// sign-in state cookie (which a sibling tenant on the registrable domain
// could plant) does not satisfy it, and a link state never falls through to
// the GitHub sign-in path.
func TestGitHubLink_SignInStateCookieDoesNotSatisfyLinkState(t *testing.T) {
	s := linkTestHub(t)
	fakeGitHub(t, "alice-gh")
	nonce, _ := startLink(t, s, linkTestOIDCUser, "alice-gh")
	state := url.QueryEscape(nonce + oauthStateSeparator + githubLinkStateProvider + oauthStateSeparator)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	req.AddCookie(testAuthCookie(linkTestOIDCUser))
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("link state with only the sign-in cookie = %d, want 400", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			t.Fatal("a link state must never mint a GitHub session")
		}
	}
	if u := loadSaaSUser(linkTestOIDCUser); u.LinkedGitHubLogin != "" {
		t.Fatal("link written without the link cookie")
	}
	if loadSaaSUser("alice-gh") != nil {
		t.Fatal("link state fell through to the GitHub sign-in path")
	}
}

// An admin viewing as a linked user does not carry that user's link into an
// SSO handoff; the user's own handoff does.
func TestGitHubLink_ImpersonatingAdminGetsNoLink(t *testing.T) {
	s := linkTestHub(t)
	u := loadSaaSUser(linkTestOIDCUser)
	u.LinkedGitHubLogin, u.LinkedGitHubProvenAt = "alice-gh", time.Now()
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	own := httptest.NewRequest(http.MethodGet, "/api/saas/hives/h/open", nil)
	own.AddCookie(testAuthCookie(linkTestOIDCUser))
	if link := s.ssoGitHubLinkForRequest(own, linkTestOIDCUser); link.Login != "alice-gh" {
		t.Fatalf("own handoff link = %+v", link)
	}
	imp := mintImpersonateCookieValueForGeneration(s.keyGenerations, canonicalizeLegacy(linkTestOwner), linkTestOIDCUser, time.Now())
	if imp == "" {
		t.Fatal("could not mint impersonation cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/h/open", nil)
	req.AddCookie(testAuthCookie(linkTestOwner))
	req.AddCookie(&http.Cookie{Name: impersonateCookieName, Value: imp})
	if eff, _, imping := s.resolveIdentity(req); !imping || !canonicalEqual(eff, linkTestOIDCUser) {
		t.Fatalf("test setup: not impersonating (%q, %v)", eff, imping)
	}
	if link := s.ssoGitHubLinkForRequest(req, linkTestOIDCUser); link.Login != "" {
		t.Fatalf("impersonating admin got the victim's link: %+v", link)
	}
}

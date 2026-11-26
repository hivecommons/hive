package hub

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// GitHub login link for a non-GitHub sign-in (hivecommons/hive#19992).
//
// A person who signs in to the hub through an OpenID Connect provider may link
// the GitHub login they control, so a spoke can show them the contributor
// profile that login owns. The link is proven, never asserted:
//
//   - the person starts it from their own hub session (POST, CSRF-checked,
//     refused while an admin is impersonating);
//   - the hub sends them through GitHub OAuth with a state nonce bound both to
//     this browser (a host-only __Host- cookie a sibling tenant cannot plant)
//     and to the identity that started it (pendingGitHubLinks);
//   - on the callback the signed-in identity must still be the one that started
//     it, and the login GitHub returns must match the one they asked for, if any.
//
// Only then is SaaSUser.LinkedGitHubLogin written, with LinkedGitHubProvenAt.
// Nobody else can create a link: there is no endpoint that takes a login to
// store, and the admin endpoint below refuses anything but an unlink. An owner
// or the person can remove a link. Link, unlink and refusals are audited.
//
// The hub forwards a link to a spoke only when LinkedGitHubProvenAt is set (see
// ssoGitHubLink), so a value written by anything other than this flow is never
// trusted downstream.

const (
	// githubLinkStateCookieName holds a started link's state nonce. Separate
	// from the sign-in state cookie and host-only by its __Host- prefix.
	githubLinkStateCookieName = "__Host-hive_github_link"

	// githubLinkStateProvider marks a GitHub OAuth state as a link step rather
	// than a sign-in. It is not a login provider name, so the sign-in path can
	// never complete a link state as a GitHub login.
	githubLinkStateProvider = "github-link"

	// githubLinkStartPath starts a link (POST) from the signed-in session.
	githubLinkStartPath = "/api/auth/github-link/start"
	// githubLinkUnlinkPath removes the caller's own link (POST).
	githubLinkUnlinkPath = "/api/auth/github-link/unlink"
	// githubLinkStatusPath reports the caller's own link (GET).
	githubLinkStatusPath = "/api/auth/github-link"
	// githubLinkAdminPath lets a hub admin remove another user's link (POST).
	githubLinkAdminPath = "/api/admin/users/github-link"

	// githubLinkDoneRedirect is where a completed link lands.
	githubLinkDoneRedirect = "/dashboard?github_link=linked"

	// maxPendingGitHubLinks bounds the in-memory pending map so a flood of
	// started-but-abandoned links cannot grow it without limit.
	maxPendingGitHubLinks = 10000

	// githubLinkActionUnlink is the only action the admin endpoint accepts.
	githubLinkActionUnlink = "unlink"
)

// pendingGitHubLink is a started link waiting for its GitHub callback.
type pendingGitHubLink struct {
	CanonicalID   string
	ExpectedLogin string
	Expires       time.Time
}

// githubLinkWriteMu serializes the "linked elsewhere" check with the save, so
// two hub users proving the same GitHub account cannot both link it.
var githubLinkWriteMu sync.Mutex

var (
	pendingGitHubLinksMu sync.Mutex
	pendingGitHubLinks   = map[string]pendingGitHubLink{}
)

// putPendingGitHubLink records a started link under its state nonce, reaping
// expired entries first and refusing when the map is full.
func putPendingGitHubLink(nonce string, p pendingGitHubLink, now time.Time) bool {
	pendingGitHubLinksMu.Lock()
	defer pendingGitHubLinksMu.Unlock()
	for k, v := range pendingGitHubLinks {
		if now.After(v.Expires) {
			delete(pendingGitHubLinks, k)
		}
	}
	if len(pendingGitHubLinks) >= maxPendingGitHubLinks {
		return false
	}
	pendingGitHubLinks[nonce] = p
	return true
}

// takePendingGitHubLink removes and returns the started link for nonce. It is
// single-use: a second callback with the same state finds nothing.
func takePendingGitHubLink(nonce string, now time.Time) (pendingGitHubLink, bool) {
	pendingGitHubLinksMu.Lock()
	defer pendingGitHubLinksMu.Unlock()
	p, ok := pendingGitHubLinks[nonce]
	delete(pendingGitHubLinks, nonce)
	if !ok || now.After(p.Expires) {
		return pendingGitHubLink{}, false
	}
	return p, true
}

// registerGitHubLinkRoutes wires the link endpoints. Called from registerOAuth
// once a login provider is configured.
func (s *HubServer) registerGitHubLinkRoutes() {
	s.mux.HandleFunc("GET "+githubLinkStatusPath, s.requireAuth(s.handleGitHubLinkStatus))
	s.mux.HandleFunc("POST "+githubLinkStartPath, s.requireAuth(s.handleGitHubLinkStart))
	s.mux.HandleFunc("POST "+githubLinkUnlinkPath, s.requireAuth(s.handleGitHubLinkUnlink))
	s.mux.HandleFunc("POST "+githubLinkAdminPath, s.requireAdmin(s.handleGitHubLinkAdmin))
}

// isNonGitHubIdentity reports whether id is a signed-in identity from a
// provider other than GitHub. Only those can link a GitHub login: a GitHub
// sign-in already is one.
func isNonGitHubIdentity(id string) bool {
	provider, _, ok := parseCanonical(canonicalizeLegacy(id))
	return ok && provider != legacyProvider
}

// handleGitHubLinkStatus serves GET /api/auth/github-link: the caller's own
// link, if any.
func (s *HubServer) handleGitHubLinkStatus(w http.ResponseWriter, r *http.Request) {
	u := loadSaaSUser(s.getAuthUser(r))
	out := map[string]any{"linkable": false}
	if u != nil {
		out["linkable"] = isNonGitHubIdentity(userCanonicalID(u))
		if login := provenLinkedGitHubLogin(u); login != "" {
			out["github_login"] = login
			out["linked_at"] = u.LinkedGitHubProvenAt.UTC().Format(time.RFC3339)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleGitHubLinkStart serves POST /api/auth/github-link/start. The form or
// JSON field "login" is optional: when set, the callback refuses any other
// GitHub account.
func (s *HubServer) handleGitHubLinkStart(w http.ResponseWriter, r *http.Request) {
	// requireAuth already refused a write while impersonating; read the REAL
	// identity anyway so nothing here can act for an impersonation target.
	caller := s.getRealAuthUser(r)
	if caller == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}
	if !isNonGitHubIdentity(caller) {
		http.Error(w, `{"error":"you signed in with GitHub; there is nothing to link"}`, http.StatusBadRequest)
		return
	}
	clientID := os.Getenv("HIVE_HUB_OAUTH_CLIENT_ID")
	if strings.TrimSpace(clientID) == "" {
		http.Error(w, `{"error":"GitHub sign-in is not configured on this hub"}`, http.StatusServiceUnavailable)
		return
	}
	// The login is required, so GitHub silently signing the browser in as
	// some other account it already holds can never link that account.
	expected := strings.TrimSpace(r.FormValue("login"))
	if expected == "" || !isValidName(expected) {
		http.Error(w, `{"error":"a valid GitHub login to link is required"}`, http.StatusBadRequest)
		return
	}
	nonce, err := oauthStateNonce()
	if err != nil {
		s.logger.Warn("github link: cannot mint state nonce", "error", err)
		http.Error(w, `{"error":"link unavailable"}`, http.StatusInternalServerError)
		return
	}
	now := time.Now()
	if !putPendingGitHubLink(nonce, pendingGitHubLink{CanonicalID: canonicalizeLegacy(caller), ExpectedLogin: expected, Expires: now.Add(oauthStateTTL)}, now) {
		http.Error(w, `{"error":"too many links in progress; try again shortly"}`, http.StatusServiceUnavailable)
		return
	}
	setGitHubLinkStateCookie(w, nonce, int(oauthStateTTL.Seconds()))
	s.logger.Info("audit: github link started", "user", caller, "expected_login", expected)
	state := url.QueryEscape(nonce + oauthStateSeparator + githubLinkStateProvider + oauthStateSeparator)
	authURL := fmt.Sprintf("%s?client_id=%s&scope=&redirect_uri=%s&state=%s",
		defaultGHAuthorizeURL, url.QueryEscape(clientID), oauthRedirectURI(), state)
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// isGitHubLinkState reports whether the callback's state names a link step
// (nonce:github-link:) at any decode depth the sign-in path tolerates.
func isGitHubLinkState(r *http.Request) bool {
	for _, cand := range callbackStateCandidates(r) {
		if _, rest, found := strings.Cut(cand, oauthStateSeparator); found {
			if provider, _, _ := strings.Cut(rest, oauthStateSeparator); provider == githubLinkStateProvider {
				return true
			}
		}
	}
	return false
}

// githubLinkStateNonce returns the state nonce of a link step when it matches
// this browser's host-only link cookie (constant-time compare).
func githubLinkStateNonce(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(githubLinkStateCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	for _, cand := range callbackStateCandidates(r) {
		nonce, rest, found := strings.Cut(cand, oauthStateSeparator)
		if !found || nonce == "" {
			continue
		}
		if provider, _, _ := strings.Cut(rest, oauthStateSeparator); provider != githubLinkStateProvider {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(nonce), []byte(cookie.Value)) == 1 {
			return nonce, true
		}
	}
	return "", false
}

// handleGitHubLinkCallback verifies a link step's state against the link
// cookie, makes it single-use, and completes the link.
func (s *HubServer) handleGitHubLinkCallback(w http.ResponseWriter, r *http.Request, code string) {
	nonce, ok := githubLinkStateNonce(r)
	clearGitHubLinkStateCookie(w)
	if !ok {
		s.logger.Warn("audit: github link refused", "reason", "state does not match this browser's link cookie")
		http.Error(w, "invalid link state; start the link again", http.StatusBadRequest)
		return
	}
	s.completeGitHubLink(w, r, nonce, code)
}

// setGitHubLinkStateCookie binds a started link to this browser. The
// __Host- prefix makes the browser refuse the cookie from any other host,
// so a sibling tenant on the hub's registrable domain cannot plant a state
// nonce in someone else's browser.
func setGitHubLinkStateCookie(w http.ResponseWriter, nonce string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     githubLinkStateCookieName,
		Value:    nonce,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearGitHubLinkStateCookie(w http.ResponseWriter) {
	setGitHubLinkStateCookie(w, "", -1)
}

// completeGitHubLink finishes a link step on the shared OAuth callback. The
// caller has verified the state nonce against this browser's link cookie.
func (s *HubServer) completeGitHubLink(w http.ResponseWriter, r *http.Request, nonce, code string) {
	pending, ok := takePendingGitHubLink(nonce, time.Now())
	if !ok {
		s.logger.Warn("audit: github link refused", "reason", "no pending link for this state")
		http.Error(w, "this link step expired or was already used; start it again", http.StatusBadRequest)
		return
	}
	// The OIDC session is the other half of the proof: the identity signed in
	// now must be the one that started the link.
	caller := s.getRealAuthUser(r)
	if caller == "" || !canonicalEqual(caller, pending.CanonicalID) {
		s.logger.Warn("audit: github link refused", "reason", "signed-in identity does not match the one that started the link",
			"started_by", pending.CanonicalID, "signed_in", caller)
		http.Error(w, "the signed-in account changed during the link; sign in again and restart it", http.StatusForbidden)
		return
	}
	login, _, _, ok := s.exchangeGitHubLogin(w, code)
	if !ok {
		return // exchangeGitHubLogin wrote the error
	}
	if !strings.EqualFold(login, pending.ExpectedLogin) {
		s.logger.Warn("audit: github link refused", "reason", "GitHub account does not match the requested login",
			"user", caller, "requested", pending.ExpectedLogin, "github", login)
		http.Error(w, "GitHub signed you in as a different account than the one you asked to link", http.StatusForbidden)
		return
	}
	u := loadSaaSUser(caller)
	if u == nil {
		s.logger.Warn("audit: github link refused", "reason", "unknown user", "user", caller)
		http.Error(w, "unknown user; sign in again", http.StatusUnauthorized)
		return
	}
	if u.Blocked {
		s.logger.Warn("audit: github link refused", "reason", "account blocked", "user", caller)
		http.Error(w, "account blocked", http.StatusForbidden)
		return
	}
	githubLinkWriteMu.Lock()
	defer githubLinkWriteMu.Unlock()
	if holder := githubLoginLinkedElsewhere(login, u); holder != "" {
		s.logger.Warn("audit: github link refused", "reason", "login already linked to another hub user",
			"user", caller, "github", login, "holder", holder)
		http.Error(w, "that GitHub login is already linked to another account", http.StatusConflict)
		return
	}
	before := provenLinkedGitHubLogin(u)
	u.LinkedGitHubLogin = login
	u.LinkedGitHubProvenAt = time.Now().UTC()
	if err := saveSaaSUser(u); err != nil {
		s.logger.Error("github link: save failed", "user", caller, "error", err)
		http.Error(w, "could not save the link", http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: github login linked", "user", caller, "github", login, "before", before)
	http.Redirect(w, r, githubLinkDoneRedirect, http.StatusSeeOther)
}

// githubLoginLinkedElsewhere returns the hub user, other than self, that
// already holds login as a proven link. One GitHub login backs at most one
// linked identity.
func githubLoginLinkedElsewhere(login string, self *SaaSUser) string {
	for _, other := range listAllSaaSUsers() {
		if self != nil && canonicalEqual(userCanonicalID(&other), userCanonicalID(self)) {
			continue
		}
		if l := provenLinkedGitHubLogin(&other); l != "" && strings.EqualFold(l, login) {
			return userCanonicalID(&other)
		}
	}
	return ""
}

// provenLinkedGitHubLogin is the user's linked GitHub login when this flow
// proved it, else "".
func provenLinkedGitHubLogin(u *SaaSUser) string {
	if u == nil || u.LinkedGitHubProvenAt.IsZero() {
		return ""
	}
	return strings.TrimSpace(u.LinkedGitHubLogin)
}

// clearGitHubLink removes u's link and saves. Reports the login removed.
func clearGitHubLink(u *SaaSUser) (string, error) {
	before := strings.TrimSpace(u.LinkedGitHubLogin)
	u.LinkedGitHubLogin = ""
	u.LinkedGitHubProvenAt = time.Time{}
	return before, saveSaaSUser(u)
}

// handleGitHubLinkUnlink serves POST /api/auth/github-link/unlink: the caller
// removes their own link.
func (s *HubServer) handleGitHubLinkUnlink(w http.ResponseWriter, r *http.Request) {
	caller := s.getRealAuthUser(r)
	u := loadSaaSUser(caller)
	if u == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}
	before, err := clearGitHubLink(u)
	if err != nil {
		s.logger.Error("github unlink: save failed", "user", caller, "error", err)
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: github login unlinked", "user", caller, "github", before, "by", caller)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "removed": before})
}

// handleGitHubLinkAdmin serves POST /api/admin/users/github-link with
// {"username": "<identity>", "action": "unlink"}. An admin may remove a link;
// any request that would create or change one is refused and audited.
func (s *HubServer) handleGitHubLinkAdmin(w http.ResponseWriter, r *http.Request) {
	admin := s.getRealAuthUser(r)
	var body struct {
		Username string `json:"username"`
		Action   string `json:"action"`
		Login    string `json:"login"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGitHubLinkBodyBytes)).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Login) != "" || body.Action != githubLinkActionUnlink {
		s.logger.Warn("audit: github link refused", "reason", "an owner cannot create a link",
			"by", admin, "target", body.Username, "login", body.Login, "action", body.Action)
		http.Error(w, `{"error":"a GitHub link is created only by the person completing GitHub sign-in; an owner can only unlink"}`, http.StatusForbidden)
		return
	}
	u := loadSaaSUser(strings.TrimSpace(body.Username))
	if u == nil {
		http.Error(w, `{"error":"unknown user"}`, http.StatusNotFound)
		return
	}
	before, err := clearGitHubLink(u)
	if err != nil {
		s.logger.Error("github unlink (admin): save failed", "user", body.Username, "error", err)
		http.Error(w, `{"error":"could not save"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: github login unlinked", "user", body.Username, "github", before, "by", admin)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "removed": before})
}

// maxGitHubLinkBodyBytes bounds the admin request body.
const maxGitHubLinkBodyBytes = 4096

// ssoGitHubLink is the proven GitHub login link an SSO handoff for username
// carries to a spoke, or the zero value. Only a non-GitHub identity with a
// link this flow proved qualifies (#19992).
func ssoGitHubLink(username string) spoke.SSOGitHubLink {
	if !isNonGitHubIdentity(username) {
		return spoke.SSOGitHubLink{}
	}
	u := loadSaaSUser(username)
	login := provenLinkedGitHubLogin(u)
	if login == "" || u.Blocked {
		return spoke.SSOGitHubLink{}
	}
	return spoke.SSOGitHubLink{Login: login, LinkedAt: u.LinkedGitHubProvenAt}
}

// ssoGitHubLinkForRequest is ssoGitHubLink for the request's SSO handoff. An
// admin viewing as another user ("View as user") never carries that user's
// link to a spoke.
func (s *HubServer) ssoGitHubLinkForRequest(r *http.Request, username string) spoke.SSOGitHubLink {
	if _, _, impersonating := s.resolveIdentity(r); impersonating {
		return spoke.SSOGitHubLink{}
	}
	return ssoGitHubLink(username)
}

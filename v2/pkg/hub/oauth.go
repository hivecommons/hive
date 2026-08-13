package hub

import (
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	oauthTimeout     = 10 * time.Second
	cookieMaxAgeDays = 7 // login session cookie lifetime
)

// These GitHub.com OAuth/API endpoints are vars (not consts) so tests can point
// the token-exchange and user-fetch flows at a local httptest server; the hub
// never reassigns them in production.
var (
	// defaultGHAuthorizeURL is the GitHub.com OAuth authorization endpoint.
	defaultGHAuthorizeURL = "https://github.com/login/oauth/authorize"
	// defaultGHTokenURL is the GitHub.com OAuth token exchange endpoint.
	defaultGHTokenURL = "https://github.com/login/oauth/access_token"
	// defaultGHUserURL is the GitHub.com API user endpoint.
	defaultGHUserURL = "https://api.github.com/user"
)

func (s *HubServer) registerOAuth() {
	clientID := os.Getenv("HIVE_HUB_OAUTH_CLIENT_ID")
	if clientID == "" {
		s.logger.Info("hub OAuth disabled (no HIVE_HUB_OAUTH_CLIENT_ID)")
		return
	}
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /api/auth/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("GET /api/auth/user", s.handleAuthUser)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.logger.Info("hub OAuth enabled", "client_id", clientID)
}

// linkPreviewUserAgents are the crawlers that fetch a URL purely to build a
// preview card. They never carry a session, so every authenticated hub link
// (/login, and anything that redirects there like /api/saas/hives/{id}/open)
// bounced them to GitHub's OAuth page — and they scraped GITHUB's Open Graph
// tags. Shared Hive links unfurled as "GitHub — Build software better,
// together" with the GitHub logo.
//
// Matched case-insensitively as substrings of the User-Agent.
var linkPreviewUserAgents = []string{
	"slackbot",         // Slack
	"twitterbot",       // X/Twitter
	"facebookexternal", // Facebook / Messenger / WhatsApp
	"linkedinbot",      // LinkedIn
	"discordbot",       // Discord
	"telegrambot",      // Telegram
	"whatsapp",         // WhatsApp (older UA)
	"skypeuripreview",  // Skype
	"embedly",          // generic embed service
	"redditbot",        // Reddit
	"mattermost",       // Mattermost
	"googlebot",        // search snippet
	"bingbot",
}

// isLinkPreviewCrawler reports whether this request is a preview bot rather than
// a person. Deliberately conservative: a false positive only means a crawler
// sees a preview card instead of a redirect, while a false negative just
// restores the old (wrong) unfurl. It must never affect real sign-in traffic.
func isLinkPreviewCrawler(r *http.Request) bool {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		return false
	}
	for _, bot := range linkPreviewUserAgents {
		if strings.Contains(ua, bot) {
			return true
		}
	}
	return false
}

// hubPublicURL is the canonical public origin used to build absolute Open Graph
// URLs. Unfurlers require absolute image URLs — a relative path is ignored — and
// they fetch the image without a session, so this must be the externally
// reachable host.
const hubPublicURL = "https://hive.kubestellar.io"

// linkPreviewMaxAge is how long an unfurler may cache the preview HTML. Short,
// because the copy may be reworded on any deploy; the image is cached far longer.
const linkPreviewMaxAge = 5 * time.Minute

// writeLinkPreview serves Hive's own Open Graph card. Status 200 with no
// redirect, so the crawler stops here instead of following through to GitHub.
//
// The meta tags are kept at the very top of <head>: Slackbot reads only the
// first 32KB of a response, so anything below that is invisible to it.
func writeLinkPreview(w http.ResponseWriter) {
	const previewHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8">
<title>Hive — AI agents that maintain your repo</title>
<meta name="description" content="Hive — the AI maintainer you own outright. AI agents triage issues, write fixes, patch CVEs, and merge on green behind six autonomy levels.">
<meta property="og:site_name" content="Hive">
<meta property="og:title" content="Hive — AI agents that maintain your repo">
<meta property="og:description" content="Put your repo on autopilot. Hive runs a fleet of AI agents on your backlog behind six autonomy levels — test coverage earns the confidence to raise a level, and you (the admin) choose when to raise it.">
<meta property="og:type" content="website">
<meta property="og:url" content="` + hubPublicURL + `">
<meta property="og:image" content="` + hubPublicURL + `/og-card.png">
<meta property="og:image:type" content="image/png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:image:alt" content="Hive — AI agents that maintain your repo">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Hive — AI agents that maintain your repo">
<meta name="twitter:description" content="Put your repo on autopilot. AI agents on your backlog behind six autonomy levels.">
<meta name="twitter:image" content="` + hubPublicURL + `/og-card.png">
</head><body>
<h1>Hive</h1>
<p>AI agents that maintain your repo. <a href="` + hubPublicURL + `">Sign in to continue.</a></p>
</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Preview cards are identical for every link and change only on deploy.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(linkPreviewMaxAge.Seconds())))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(previewHTML))
}

// ogCardPath is the pre-rendered preview image inside staticFS. It is a real
// PNG, not an SVG: no major unfurler (Slack, X/Twitter, Facebook, LinkedIn,
// Discord) renders an SVG og:image — they show a blank placeholder instead — so
// an SVG card would leave the preview image broken. It is 1200x630, the size
// every platform crops from, and ~93KB, comfortably under Slack's 1MB limit
// above which images are silently dropped.
const ogCardPath = "static/og-card.png"

// ogCardMaxAge is how long unfurlers and CDNs may cache the preview image. The
// card only changes on deploy, and crawlers re-fetch it for every shared link,
// so a long TTL costs nothing.
const ogCardMaxAge = 24 * time.Hour

// handleOGCard serves the Open Graph preview image. Registered outside
// registerOAuth so it stays reachable even when OAuth is unconfigured —
// otherwise the image 404s and the card renders blank.
func (s *HubServer) handleOGCard(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile(ogCardPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ogCardMaxAge.Seconds())))
	w.Write(data)
}

func (s *HubServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Serve Hive's own card to preview crawlers instead of redirecting them into
	// GitHub's OAuth page, whose Open Graph tags they would otherwise scrape.
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	clientID := os.Getenv("HIVE_HUB_OAUTH_CLIENT_ID")
	redirect := r.URL.Query().Get("redirect")
	if redirect == "" {
		redirect = r.URL.Query().Get("rd")
	}
	if redirect != "" && (!strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//")) {
		// Redirect trust intentionally still spans sibling tenants: hosted hives
		// bounce here via their ingress auth-signin and must be returned home.
		// This is NOT the CSRF boundary — see isSameOriginAsHub (audit F4).
		if !isTrustedRedirectTarget(redirect) {
			redirect = "/dashboard"
		}
	}
	// SECURITY (audit F11, CWE-352): bind this login to THIS browser.
	//
	// `state` used to be nothing but the redirect target, so it proved only that
	// the callback carried a URL — never that this browser had actually STARTED
	// a login. An attacker could complete an OAuth flow against their own GitHub
	// account and hand the victim the resulting callback URL, logging the victim
	// into the ATTACKER's account (login CSRF / session fixation); anything the
	// victim then did landed in the attacker's account.
	//
	// Mint an unguessable nonce, set it in a short-lived host-scoped cookie, and
	// carry it in state. The callback requires the two to match, so a state the
	// victim's browser did not mint cannot be replayed against them. The
	// redirect target rides along after the separator and is still validated on
	// the way out — the open-redirect half was already handled and is unchanged.
	nonce, err := oauthStateNonce()
	if err != nil {
		s.logger.Warn("OAuth: cannot mint login state nonce", "error", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    nonce,
		Path:     "/",
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		// Lax, not Strict: the callback arrives as a top-level GET redirect
		// FROM github.com, and Strict would withhold the cookie on exactly that
		// navigation and break every login.
		SameSite: http.SameSiteLaxMode,
	})
	state := url.QueryEscape(nonce + oauthStateSeparator + redirect)
	// An EMPTY scope is deliberate, not an omission. The hub only needs to know
	// WHO is logging in: the callback reads GET /user for the login name and
	// signs it into the session cookie. GitHub serves /user's public profile —
	// including "login" — for a token with no scopes at all, so identity works
	// unscoped.
	//
	// The hub used to ask for read:user because the request wizard listed the
	// user's repositories for them to pick from. That listing is gone: it could
	// only ever see public github.com, and it could never generalize to GitLab
	// or Gitea. Requesters now type a repository URL instead, which needs no
	// permission on the requester's account whatsoever.
	//
	// Do NOT restore a scope here without also restoring a feature that needs
	// it — asking for access the product does not use is a consent prompt users
	// are right to refuse.
	authURL := fmt.Sprintf("%s?client_id=%s&scope=&redirect_uri=%s&state=%s",
		defaultGHAuthorizeURL, clientID, "https://hive.kubestellar.io/api/auth/callback", state)
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

func (s *HubServer) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	// SECURITY (audit F11, CWE-352): verify the login started in THIS browser
	// BEFORE exchanging the code or minting any session. A callback whose state
	// does not match this browser's cookie is a login the victim never began —
	// most likely an attacker replaying their own completed flow to log the
	// victim into the attacker's account.
	//
	// Checked first, so a forged callback never reaches the token exchange:
	// exchanging burns a real one-time code and touches the SaaS user record,
	// neither of which should happen for a request we are about to reject.
	if !s.verifyOAuthStateNonce(r) {
		s.clearOAuthStateCookie(w)
		s.logger.Warn("OAuth: rejected callback with missing or mismatched state nonce")
		http.Error(w, "invalid login state — please start the login again", http.StatusBadRequest)
		return
	}
	// Single-use: clear it now so a captured callback URL cannot be replayed.
	s.clearOAuthStateCookie(w)

	clientID := os.Getenv("HIVE_HUB_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("HIVE_HUB_OAUTH_CLIENT_SECRET")

	tokenReq, _ := http.NewRequest("POST", defaultGHTokenURL, nil)
	q := tokenReq.URL.Query()
	q.Set("client_id", clientID)
	q.Set("client_secret", clientSecret)
	q.Set("code", code)
	tokenReq.URL.RawQuery = q.Encode()
	tokenReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: oauthTimeout}
	resp, err := client.Do(tokenReq)
	if err != nil {
		s.logger.Warn("OAuth token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	const maxOAuthResponseBytes = 1 << 16
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		s.logger.Warn("OAuth: failed to parse token response", "error", err)
		http.Error(w, "invalid token response", http.StatusBadGateway)
		return
	}

	if tokenResp.AccessToken == "" {
		s.logger.Warn("OAuth: no access token in response")
		http.Error(w, "no access token", http.StatusBadGateway)
		return
	}

	userReq, _ := http.NewRequest("GET", defaultGHUserURL, nil)
	userReq.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	userResp, err := client.Do(userReq)
	if err != nil {
		http.Error(w, "user fetch failed", http.StatusBadGateway)
		return
	}
	defer userResp.Body.Close()

	userBody, _ := io.ReadAll(io.LimitReader(userResp.Body, maxOAuthResponseBytes))
	var user struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(userBody, &user); err != nil {
		s.logger.Warn("OAuth: failed to parse user response", "error", err)
		http.Error(w, "invalid user response", http.StatusBadGateway)
		return
	}

	if user.Login == "" || !isValidName(user.Login) {
		s.logger.Warn("OAuth: invalid or empty login", "login", user.Login)
		http.Error(w, "invalid user login", http.StatusBadGateway)
		return
	}

	s.logger.Info("audit: hub OAuth login", "user", user.Login)

	// Set the session cookie to a signed, tamper-evident value so it cannot be
	// forged. If the hub has no secret to sign with, fail the login rather than
	// emit an unsigned (trusted-by-default) cookie.
	// N2: mint ASYMMETRICALLY. The hub holds the Ed25519 private seed; spokes
	// receive only the public key, so a spoke operator can verify this cookie but
	// can no longer forge one (notably an admin cookie).
	cookieValue := mintHubUserCookieValueV2(s.sessionSigningSeed(), user.Login)
	if cookieValue == "" {
		s.logger.Warn("OAuth: cannot mint signed session cookie", "user", user.Login)
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return
	}
	// AUDIT F4, DELIBERATELY NOT CHANGED — read before "fixing" this.
	//
	// The audit asks for a host-only `__Host-` session cookie. That is correct
	// in the abstract and NOT safely applicable here, because this cookie is
	// load-bearing across a trust boundary: it is minted by the hub (Go) and
	// verified INDEPENDENTLY by every spoke's Node proxy
	// (v2/proxy/server.js — verifyHubUserCookieEither, and the WebSocket
	// terminal path). The proxy can only verify a cookie the browser actually
	// sends it, and the browser only sends this one to <id>.hive.kubestellar.io
	// BECAUSE of the Domain attribute below. Dropping Domain (which __Host-
	// additionally forbids outright) would stop the cookie reaching any spoke
	// and log every hosted tenant out of their own dashboard and terminal —
	// a fleet-wide outage across ~62 hives, and precisely the flag-day auth
	// change that caused incident #2773.
	//
	// The verify-both/mint-new pattern used for the N2 Ed25519 cutover
	// (hub_cookie.go) does NOT rescue this. That pattern works when both
	// formats travel the same path and only the VERIFIER must learn a new
	// shape. Here the change is to the cookie's delivery scope: a host-only
	// cookie is never transmitted to the spoke at all, so there is no request
	// in which a spoke could verify it, no matter what it accepts. Making this
	// safe needs a real design change (e.g. a distinct spoke-scoped session
	// cookie minted per tenant at SSO handoff), not a rollout trick.
	//
	// What this PR does instead is remove the sibling's ability to USE the
	// cookie it receives: an untrusted tenant can no longer author an accepted
	// mutation (isSameOriginAsHub) nor read a credentialed CORS response. The
	// cookie still travels to siblings — a real, ACCEPTED residual risk that a
	// hostile tenant can read it only if some other bug lets them (it stays
	// HttpOnly + Secure), which is why the spoke-scoped-session redesign is
	// tracked as follow-up rather than closed.
	cookie := &http.Cookie{
		Name:     "hive_hub_user",
		Value:    cookieValue,
		Path:     "/",
		Domain:   ".hive.kubestellar.io",
		MaxAge:   86400 * cookieMaxAgeDays,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, cookie)

	saasUser := ensureSaaSUser(user.Login)
	// A completed OAuth callback IS a login — count it here and nowhere else.
	// ensureSaaSUser already refreshed LastLogin; the count is the engagement
	// signal the admin Users card reads. Persist unconditionally below so a login
	// is recorded even when there is no token to encrypt (the token branch used to
	// be the only save path, so a token-encrypt failure silently dropped the whole
	// record update).
	saasUser.LoginCount++
	if encrypted, err := encryptToken(tokenResp.AccessToken); err == nil {
		saasUser.EncryptedToken = encrypted
	}
	saveSaaSUser(saasUser)

	redirect := "/dashboard"
	if decoded, err := url.QueryUnescape(r.URL.Query().Get("state")); err == nil && decoded != "" {
		// The nonce was already verified above; take only the redirect half.
		if _, target, ok := strings.Cut(decoded, oauthStateSeparator); ok {
			decoded = target
		}
		if decoded != "" && ((strings.HasPrefix(decoded, "/") && !strings.HasPrefix(decoded, "//")) || isTrustedRedirectTarget(decoded)) {
			redirect = decoded
		}
	}
	http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
}

func (s *HubServer) handleAuthUser(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}
	// Trust the carried username only when its signature verifies; a legacy
	// unsigned or forged cookie reports unauthenticated, prompting a re-login.
	// N2: accept v2 (Ed25519) or, during rollout only, the legacy HMAC format.
	// F10: also enforces a v3 cookie's signed expiry and revocation state.
	username, ok := s.verifyHubUserCookie(cookie.Value)
	if !ok || loadSaaSUser(username) == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}
	isAdmin := isHubAdmin(username)
	// Fold impersonation status into the auth payload the dashboard already
	// fetches, so the "Viewing as … read-only" banner renders without a second
	// round-trip. When an admin is impersonating, report the effective identity
	// as the target (that is what every per-user view is rendering as) but keep
	// hub_admin FALSE — during impersonation the admin is deliberately a normal
	// user, so admin-only affordances hide via the existing role checks.
	payload := map[string]any{
		"authenticated": true,
		"login":         username,
		"avatar_url":    fmt.Sprintf("https://github.com/%s.png", username),
		"hub_admin":     isAdmin,
	}
	if grant, ok := s.activeImpersonationGrant(r); ok {
		payload["login"] = grant.Target
		payload["avatar_url"] = fmt.Sprintf("https://github.com/%s.png", grant.Target)
		payload["hub_admin"] = false
		payload["impersonating"] = true
		payload["viewing_as"] = grant.Target
		payload["real_user"] = username
	}
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HubServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	// AUDIT F10: deleting the browser's copy is not logging out. Anyone who
	// captured the cookie value keeps a working session until its own expiry.
	// Record the session ID server-side so the verifier rejects it from now on.
	//
	// This is a no-op today because minting still emits v2, which carries no
	// session ID — that is the point of this PR being verifier-only. It becomes
	// load-bearing the moment minting flips to v3, with no further change to
	// this handler.
	if c, err := r.Cookie("hive_hub_user"); err == nil && c.Value != "" {
		s.revokeHubSessionCookie(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hive_hub_user",
		Value:    "",
		Path:     "/",
		Domain:   ".hive.kubestellar.io",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

const (
	// oauthStateCookieName holds the per-login nonce that binds an OAuth
	// callback to the browser that started it (audit F11). Host-scoped
	// deliberately: unlike hive_hub_user it carries no Domain, so sibling
	// tenants never receive it.
	oauthStateCookieName = "hive_oauth_state"

	// oauthStateSeparator splits the nonce from the redirect target inside the
	// state parameter. ":" cannot appear in a nonce (hex) and any ":" in the
	// redirect lands in the second half, which strings.Cut keeps intact.
	oauthStateSeparator = ":"

	// oauthStateTTL bounds how long a started login may sit unfinished. Long
	// enough to authorize on GitHub (including a fresh GitHub login), short
	// enough that a captured state is not indefinitely useful.
	oauthStateTTL = 15 * time.Minute

	// oauthStateNonceBytes is the entropy behind the nonce. 32 bytes is far
	// beyond guessing and matches the other random values minted in this package.
	oauthStateNonceBytes = 32
)

// oauthStateNonce mints an unguessable login nonce.
func oauthStateNonce() (string, error) {
	buf := make([]byte, oauthStateNonceBytes)
	if _, err := io.ReadFull(cryptoRand.Reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// verifyOAuthStateNonce reports whether the callback's state carries the same
// nonce this browser was issued at login.
//
// Fails CLOSED: a missing cookie, a missing or malformed state, or any mismatch
// is a rejection. Compared in constant time — the nonce is a secret, and a
// length-or-content leak would let an attacker narrow it by timing.
func (s *HubServer) verifyOAuthStateNonce(r *http.Request) bool {
	cookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	decoded, err := url.QueryUnescape(r.URL.Query().Get("state"))
	if err != nil || decoded == "" {
		return false
	}
	got, _, ok := strings.Cut(decoded, oauthStateSeparator)
	if !ok || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(cookie.Value)) == 1
}

// clearOAuthStateCookie expires the login nonce, making it single-use.
func (s *HubServer) clearOAuthStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

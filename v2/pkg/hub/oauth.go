package hub

import (
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

const (
	oauthTimeout     = 10 * time.Second
	cookieMaxAgeDays = 7 // login session cookie lifetime

	// oauthRedirectURI is the single OAuth/OIDC callback registered on every
	// provider's side. All providers share one callback path; the state parameter
	// carries which provider to complete against.
	oauthRedirectURI = "https://hive.kubestellar.io/api/auth/callback"

	// oidcNonceCookieName holds the per-login OIDC replay nonce. Host-scoped like
	// the CSRF state cookie; the id_token must echo it back or the callback fails.
	oidcNonceCookieName = "hive_oidc_nonce"
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

	// Build the human-login provider set: GitHub (when its client id is set) plus
	// any configured OIDC providers (Google/IBMid/Red Hat — enabled iff their
	// <PREFIX>_CLIENT_ID env is present). The GitHub endpoints come from the
	// existing test-overridable vars so the auth registry shares the hub's seam.
	s.authProviders = auth.BuildRegistry(clientID, defaultGHAuthorizeURL, defaultGHTokenURL)

	// Nothing to serve if no provider at all is configured. Historically the hub
	// keyed entirely on GitHub; now it stays "OAuth disabled" only when NEITHER
	// GitHub nor any OIDC provider is set.
	if s.authProviders.Count() == 0 {
		s.logger.Info("hub OAuth disabled (no login provider configured)")
		return
	}
	s.mux.HandleFunc("GET /login", s.handleLogin)
	// Per-provider entry point. GitHub keeps its existing behavior; an OIDC
	// provider ("google"/"ibmid"/"redhat") starts the OIDC authorize.
	s.mux.HandleFunc("GET /login/{provider}", s.handleProviderLogin)
	s.mux.HandleFunc("GET /api/auth/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("GET /api/auth/user", s.handleAuthUser)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	names := make([]string, 0, s.authProviders.Count())
	for _, p := range s.authProviders.Providers() {
		names = append(names, p.Name)
	}
	s.logger.Info("hub login enabled", "providers", strings.Join(names, ","), "github_client_id", clientID)
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

// handleLogin is the login entry point. With exactly one provider enabled
// (today: GitHub) it redirects straight into that provider, preserving the
// pre-multi-provider UX byte-for-byte. With more than one enabled it renders a
// small provider picker; each button links to /login/{provider} carrying the
// redirect target through.
func (s *HubServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Serve Hive's own card to preview crawlers instead of redirecting them into
	// a provider's OAuth page, whose Open Graph tags they would otherwise scrape.
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	redirect := s.loginRedirectTarget(r)
	providers := s.authProviders.Providers()
	if len(providers) == 0 {
		// Registry not built (a HubServer constructed without registerOAuth — the
		// unit-test path) but GitHub is configured: behave as single-GitHub so the
		// pre-multi-provider UX is preserved with no registry.
		if gh := s.resolveProvider(legacyProvider); gh != nil {
			s.startProviderLogin(w, r, gh, redirect)
			return
		}
		http.Error(w, "login unavailable", http.StatusServiceUnavailable)
		return
	}
	if len(providers) == 1 {
		// Single provider: go straight in — no picker, unchanged UX.
		s.startProviderLogin(w, r, providers[0], redirect)
		return
	}
	s.writeProviderPicker(w, providers, redirect)
}

// handleProviderLogin starts login for a specific provider named in the path.
func (s *HubServer) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	name := r.PathValue("provider")
	p := s.authProviders.Get(name)
	if p == nil {
		http.Error(w, "unknown login provider", http.StatusNotFound)
		return
	}
	s.startProviderLogin(w, r, p, s.loginRedirectTarget(r))
}

// loginRedirectTarget extracts and validates the post-login redirect from the
// request (?redirect= / ?rd=), defaulting to /dashboard for anything untrusted.
func (s *HubServer) loginRedirectTarget(r *http.Request) string {
	redirect := r.URL.Query().Get("redirect")
	if redirect == "" {
		redirect = r.URL.Query().Get("rd")
	}
	if redirect != "" && (!strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//")) {
		if !isTrustedRedirectTarget(redirect) {
			redirect = "/dashboard"
		}
	}
	return redirect
}

// startProviderLogin mints the CSRF state nonce (and, for OIDC, the replay nonce)
// and redirects the browser into the provider's authorize endpoint. GitHub keeps
// its exact original request shape; OIDC providers get an OIDC nonce too.
func (s *HubServer) startProviderLogin(w http.ResponseWriter, r *http.Request, p *auth.Provider, redirect string) {
	// SECURITY (audit F11, CWE-352): bind this login to THIS browser.
	//
	// `state` used to be nothing but the redirect target, so it proved only that
	// the callback carried a URL — never that this browser had actually STARTED
	// a login. An attacker could complete an OAuth flow against their own account
	// and hand the victim the resulting callback URL, logging the victim into the
	// ATTACKER's account (login CSRF / session fixation).
	//
	// Mint an unguessable nonce, set it in a short-lived host-scoped cookie, and
	// carry it in state. The callback requires the two to match, so a state the
	// victim's browser did not mint cannot be replayed against them. The provider
	// name and redirect target ride along after the separators and are validated
	// on the way out.
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
		// Lax, not Strict: the callback arrives as a top-level GET redirect FROM
		// the provider, and Strict would withhold the cookie on exactly that
		// navigation and break every login.
		SameSite: http.SameSiteLaxMode,
	})

	// state = nonce : provider : redirect. The provider name is carried so the
	// callback knows which provider to complete against without a second cookie.
	state := url.QueryEscape(nonce + oauthStateSeparator + p.Name + oauthStateSeparator + redirect)

	if !p.IsOIDC {
		// GitHub: identical request shape to the pre-multi-provider hub. An EMPTY
		// scope is deliberate — the hub only needs WHO is logging in, and GitHub
		// serves /user's public profile (including "login") unscoped. Do NOT add a
		// scope without a feature that needs it.
		authURL := fmt.Sprintf("%s?client_id=%s&scope=&redirect_uri=%s&state=%s",
			p.AuthorizeURL, p.ClientID, oauthRedirectURI, state)
		http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
		return
	}

	// OIDC: mint a replay nonce, cookie it, and pass it in the authorize request;
	// the callback requires the id_token to echo it back.
	oidcNonce, err := oauthStateNonce()
	if err != nil {
		s.logger.Warn("OIDC: cannot mint nonce", "provider", p.Name, "error", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcNonceCookieName,
		Value:    oidcNonce,
		Path:     "/",
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	authURL, err := p.AuthCodeURL(oauthRedirectURI, state, oidcNonce)
	if err != nil {
		s.logger.Warn("OIDC: cannot build authorize URL", "provider", p.Name, "error", err)
		http.Error(w, "login unavailable — provider not reachable", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// providerGlyph returns a tiny inline SVG/emoji-free label mark per provider for
// the picker. Kept as plain text marks (no remote assets) so the picker page is
// fully self-contained and needs no external fetch.
func providerGlyph(name string) string {
	switch name {
	case "github":
		return "&#xf09b;" // rendered as text fallback; see picker CSS note
	case "google":
		return "G"
	case "ibmid":
		return "IBM"
	case "redhat":
		return "&#x1f452;"
	default:
		return "&#x2022;"
	}
}

// writeProviderPicker renders the multi-provider sign-in page. Shown only when
// more than one provider is enabled; a single-provider hub never reaches here.
// The redirect target is preserved by threading it through each button's link.
func (s *HubServer) writeProviderPicker(w http.ResponseWriter, providers []*auth.Provider, redirect string) {
	rd := ""
	if redirect != "" {
		rd = "?redirect=" + url.QueryEscape(redirect)
	}
	var buttons strings.Builder
	for _, p := range providers {
		// Each button is a plain link to /login/{provider}; startProviderLogin
		// mints the nonces there. Provider name/display are from our closed set,
		// not user input, so they are safe to embed.
		buttons.WriteString(fmt.Sprintf(
			`<a class="prov prov-%s" href="/login/%s%s"><span class="glyph">%s</span><span>Continue with %s</span></a>`+"\n",
			html.EscapeString(p.Name), html.EscapeString(p.Name), rd, providerGlyph(p.Name), html.EscapeString(p.DisplayName)))
	}
	page := `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in — Hive</title>
<style>
:root{color-scheme:light dark}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0d1117;color:#e6edf3}
.card{width:min(360px,92vw);padding:32px 28px;border:1px solid #30363d;border-radius:14px;background:#161b22;text-align:center}
h1{font-size:20px;margin:0 0 4px}
p.sub{color:#8b949e;font-size:13px;margin:0 0 24px}
.prov{display:flex;align-items:center;gap:12px;justify-content:center;width:100%;box-sizing:border-box;
padding:11px 14px;margin:8px 0;border:1px solid #30363d;border-radius:9px;background:#21262d;color:#e6edf3;
text-decoration:none;font-size:14px;font-weight:600;transition:background .12s,border-color .12s}
.prov:hover{background:#30363d;border-color:#8b949e}
.glyph{display:inline-flex;align-items:center;justify-content:center;width:22px;height:22px;
border-radius:5px;background:#0d1117;font-size:11px;font-weight:700}
.foot{margin-top:20px;color:#6e7681;font-size:11px}
</style></head><body>
<div class="card">
<h1>Sign in to Hive</h1>
<p class="sub">Choose how you'd like to continue.</p>
` + buttons.String() + `<div class="foot">Your login provider controls access only. Hive's GitHub work runs through its own app.</div>
</div></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(page))
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

	// state = nonce : provider : redirect. The provider name tells us which flow
	// to complete; it defaults to github so a stale two-part state (from a login
	// begun on the pre-multi-provider hub, mid-deploy) still completes as GitHub.
	providerName, redirect := s.parseCallbackState(r)
	p := s.resolveProvider(providerName)
	if p == nil {
		s.logger.Warn("OAuth: callback for unconfigured provider", "provider", providerName)
		http.Error(w, "unknown login provider", http.StatusBadRequest)
		return
	}

	// Resolve the login into a canonical identity (+ optional avatar/email and, for
	// GitHub, the user access token to store). Each branch fully validates its own
	// response; a failure returns an HTTP error and no session is minted.
	var (
		canonicalID string
		avatarURL   string
		email       string
		ghToken     string // GitHub user access token, if any (stored encrypted)
	)
	if p.IsOIDC {
		claims, err := p.Exchange(r.Context(), code, oauthRedirectURI, s.oidcNonceFromCookie(r))
		s.clearOIDCNonceCookie(w)
		if err != nil {
			s.logger.Warn("OIDC: callback verification failed", "provider", p.Name, "error", err)
			http.Error(w, "login failed — could not verify your identity", http.StatusBadGateway)
			return
		}
		id, err := makeCanonical(p.Name, claims.Subject)
		if err != nil {
			s.logger.Warn("OIDC: subject not usable as identity", "provider", p.Name, "error", err)
			http.Error(w, "login failed — invalid identity", http.StatusBadGateway)
			return
		}
		canonicalID = id
		avatarURL = claims.AvatarURL
		email = claims.Email
		s.logger.Info("audit: hub OIDC login", "provider", p.Name, "user", canonicalID)
	} else {
		login, avatar, token, ok := s.exchangeGitHubLogin(w, code)
		if !ok {
			return // exchangeGitHubLogin already wrote the error
		}
		// GitHub primary identity is the bare login (the shim reads it as
		// github:<login>); keep it bare so legacy files/grants/cookies are
		// byte-identical to the pre-multi-provider hub.
		canonicalID = login
		avatarURL = avatar
		ghToken = token
		s.logger.Info("audit: hub OAuth login", "user", login)
	}

	// From here the two paths converge: mint the session cookies over the
	// canonical id (the signing machinery signs an opaque string, so this is
	// provider-agnostic) and persist the user record.
	if !s.mintSessionCookies(w, canonicalID) {
		return // mintSessionCookies wrote the error
	}

	saasUser := ensureSaaSUser(canonicalID)
	// Stamp the provider fields so the Users-table badge and dual-read storage
	// have an explicit canonical identity (Phase 1d fields). For a legacy GitHub
	// user these are derivable, but writing them makes the record self-describing.
	provider, _, _ := parseCanonical(canonicalizeLegacy(canonicalID))
	saasUser.CanonicalID = canonicalizeLegacy(canonicalID)
	saasUser.Provider = provider
	if avatarURL != "" {
		saasUser.AvatarURL = avatarURL
	}
	if email != "" {
		saasUser.Email = email
	}
	// A completed callback IS a login — count it here and nowhere else.
	saasUser.LoginCount++
	if ghToken != "" {
		if encrypted, err := encryptToken(ghToken); err == nil {
			saasUser.EncryptedToken = encrypted
		}
	}
	saveSaaSUser(saasUser)

	if redirect == "" {
		redirect = "/dashboard"
	}
	http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
}

// resolveProvider returns the auth.Provider to complete a callback against. It
// reads the built registry, but falls back to synthesizing a GitHub provider
// from the current env/endpoint vars when the registry is absent (a HubServer
// constructed without registerOAuth — the unit-test path) or does not carry
// GitHub. This keeps the GitHub callback working exactly as before regardless of
// whether the registry was built, while OIDC always requires the registry.
func (s *HubServer) resolveProvider(name string) *auth.Provider {
	if p := s.authProviders.Get(name); p != nil {
		return p
	}
	// GitHub is always resolvable by synthesizing it from the current env/endpoint
	// vars: the registry may not carry it (a HubServer built without registerOAuth
	// — the unit-test path — or one where the GitHub client id was unset). An empty
	// client id is a production-config concern, not a handler concern; the pre-
	// multi-provider hub also redirected to GitHub's authorize with whatever client
	// id was in env. OIDC providers, by contrast, MUST come from the built registry.
	if name == legacyProvider || name == "" {
		return &auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			IsOIDC:       false,
			AuthorizeURL: defaultGHAuthorizeURL,
			TokenURL:     defaultGHTokenURL,
			ClientID:     os.Getenv("HIVE_HUB_OAUTH_CLIENT_ID"),
			Scopes:       []string{""},
		}
	}
	return nil
}

// parseCallbackState extracts the provider name and validated redirect target
// from the (already nonce-verified) state parameter. State is
// "nonce:provider:redirect". A two-part legacy state (nonce:redirect, from a
// login begun before this deploy) yields provider "github" and treats the second
// half as the redirect.
func (s *HubServer) parseCallbackState(r *http.Request) (provider, redirect string) {
	provider = "github"
	redirect = "/dashboard"
	decoded, err := url.QueryUnescape(r.URL.Query().Get("state"))
	if err != nil || decoded == "" {
		return provider, redirect
	}
	// Strip the already-verified nonce.
	_, rest, ok := strings.Cut(decoded, oauthStateSeparator)
	if !ok {
		return provider, redirect
	}
	// rest is "provider:redirect" (new) or just "redirect" (legacy two-part).
	maybeProvider, maybeRedirect, ok := strings.Cut(rest, oauthStateSeparator)
	if ok && s.authProviders.Get(maybeProvider) != nil {
		provider = maybeProvider
		rest = maybeRedirect
	}
	if rest != "" && ((strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "//")) || isTrustedRedirectTarget(rest)) {
		redirect = rest
	}
	return provider, redirect
}

// exchangeGitHubLogin runs the GitHub OAuth code→token→/user flow and returns the
// login, avatar, and user access token. On any failure it writes the HTTP error
// and returns ok=false. This is the original callback logic, factored out so the
// dispatcher can share the surrounding cookie/persistence code with OIDC.
func (s *HubServer) exchangeGitHubLogin(w http.ResponseWriter, code string) (login, avatar, token string, ok bool) {
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
		return "", "", "", false
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
		return "", "", "", false
	}
	if tokenResp.AccessToken == "" {
		s.logger.Warn("OAuth: no access token in response")
		http.Error(w, "no access token", http.StatusBadGateway)
		return "", "", "", false
	}

	userReq, _ := http.NewRequest("GET", defaultGHUserURL, nil)
	userReq.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	userResp, err := client.Do(userReq)
	if err != nil {
		http.Error(w, "user fetch failed", http.StatusBadGateway)
		return "", "", "", false
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
		return "", "", "", false
	}
	if user.Login == "" || !isValidName(user.Login) {
		s.logger.Warn("OAuth: invalid or empty login", "login", user.Login)
		http.Error(w, "invalid user login", http.StatusBadGateway)
		return "", "", "", false
	}
	return user.Login, user.AvatarURL, tokenResp.AccessToken, true
}

// mintSessionCookies sets both the HMAC (hive_hub_user) and Ed25519
// (hive_hub_user_v2) session cookies over the given canonical identity. Returns
// false (after writing an HTTP error) if the hub has no secret to sign with — an
// unsigned cookie would be trusted by default and must never be emitted.
func (s *HubServer) mintSessionCookies(w http.ResponseWriter, canonicalID string) bool {
	cookieValue := mintHubUserCookieValue(s.sessionCookieKey(), canonicalID)
	if cookieValue == "" {
		s.logger.Warn("OAuth: cannot mint signed session cookie", "user", canonicalID)
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hive_hub_user",
		Value:    cookieValue,
		Path:     "/",
		Domain:   ".hive.kubestellar.io",
		MaxAge:   86400 * cookieMaxAgeDays,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	// N2 (CWE-321/798): ALSO emit an Ed25519-signed cookie. hive_hub_user stays
	// HMAC because that is the only format v2 spokes verify; v4 accepts either.
	// This second cookie is what actually closes N2 for spokes that can read it: a
	// spoke holding only the PUBLIC key can verify but not mint one, so it can no
	// longer forge a hub-admin cookie. Two cookies rather than one hybrid value:
	// each verifier signs the whole prefix, so no single concatenation satisfies
	// both.
	if v2Value := mintHubUserCookieValueV2(s.sessionSigningSeed(), canonicalID); v2Value != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     hubUserCookieV2Name,
			Value:    v2Value,
			Path:     "/",
			Domain:   ".hive.kubestellar.io",
			MaxAge:   86400 * cookieMaxAgeDays,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return true
}

// oidcNonceFromCookie returns the OIDC replay nonce this browser was issued at
// login start, or "" if absent. The id_token must echo it back.
func (s *HubServer) oidcNonceFromCookie(r *http.Request) string {
	c, err := r.Cookie(oidcNonceCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
}

// clearOIDCNonceCookie expires the OIDC nonce, making it single-use.
func (s *HubServer) clearOIDCNonceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcNonceCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// displayIdentity returns the human-facing login label and avatar URL for a
// canonical (or legacy bare) identity. For a GitHub user this is the bare login
// and the derived github.com/<login>.png avatar — byte-identical to the
// pre-multi-provider behavior. For an OIDC user it prefers the STORED avatar
// (Google/IBMid provide a picture) and a friendly display label (email, else
// display name, else the canonical id), never the opaque provider:sub.
func (s *HubServer) displayIdentity(identity string) (login, avatarURL string) {
	provider, subject, ok := parseCanonical(canonicalizeLegacy(identity))
	if !ok {
		return identity, ""
	}
	if provider == legacyProvider {
		// GitHub: unchanged. subject is the bare login.
		return subject, fmt.Sprintf("https://github.com/%s.png", subject)
	}
	// Non-GitHub: use the stored record for a good label + avatar.
	login = identity
	if u := loadSaaSUser(canonicalizeLegacy(identity)); u != nil {
		if u.Email != "" {
			login = u.Email
		}
		avatarURL = u.AvatarURL
	}
	return login, avatarURL
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
	username, ok := s.verifyHubUserDual(cookie.Value)
	if !ok || loadSaaSUser(username) == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false}`))
		return
	}
	isAdmin := isHubAdmin(username)
	displayLogin, avatar := s.displayIdentity(username)
	// Fold impersonation status into the auth payload the dashboard already
	// fetches, so the "Viewing as … read-only" banner renders without a second
	// round-trip. When an admin is impersonating, report the effective identity
	// as the target (that is what every per-user view is rendering as) but keep
	// hub_admin FALSE — during impersonation the admin is deliberately a normal
	// user, so admin-only affordances hide via the existing role checks.
	payload := map[string]any{
		"authenticated": true,
		"login":         displayLogin,
		"avatar_url":    avatar,
		"hub_admin":     isAdmin,
	}
	if grant, ok := s.activeImpersonationGrant(r); ok {
		targetLogin, targetAvatar := s.displayIdentity(grant.Target)
		payload["login"] = targetLogin
		payload["avatar_url"] = targetAvatar
		payload["hub_admin"] = false
		payload["impersonating"] = true
		payload["viewing_as"] = targetLogin
		payload["real_user"] = displayLogin
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

	// N2: clear the Ed25519 cookie too, or a logout leaves a still-valid
	// credential in the browser — the session-invalidation hole in miniature.
	http.SetCookie(w, &http.Cookie{
		Name:     hubUserCookieV2Name,
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

	// oauthStateNonceBytes is the entropy behind the nonce.
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

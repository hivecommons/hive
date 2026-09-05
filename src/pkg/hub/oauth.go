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

	"github.com/hivecommons/hive/pkg/auth"
)

const (
	oauthTimeout     = 10 * time.Second
	cookieMaxAgeDays = 7 // login session cookie lifetime

	// cookieSessionTTL is the SIGNED lifetime baked into a v3 session cookie
	// (audit F10). Derived from cookieMaxAgeDays so the enforced expiry and the
	// browser's MaxAge hint can never drift apart — the two must agree, or a
	// session either dies early in the browser or outlives its signature.
	//
	// This is the value that actually bounds a stolen cookie: MaxAge is advisory
	// and an attacker replaying a captured value ignores it entirely, whereas exp
	// is inside the signature and checked by every verifier.
	cookieSessionTTL = time.Duration(cookieMaxAgeDays) * 24 * time.Hour

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
	// any configured OIDC providers (Google/IBMid/Red Hat/Microsoft/custom —
	// enabled iff their <PREFIX>_CLIENT_ID env is present). The GitHub endpoints
	// come from the existing test-overridable vars so the auth registry shares the
	// hub's seam.
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
	// provider ("google"/"ibmid"/"redhat"/…) starts the OIDC authorize.
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

// linkPreviewMaxAge is how long an unfurler may cache the preview HTML. Short,
// because the copy may be reworded on any deploy; the image is cached far longer.
const linkPreviewMaxAge = 5 * time.Minute

// writeLinkPreview serves Hive's own Open Graph card. Status 200 with no
// redirect, so the crawler stops here instead of following through to GitHub.
//
// The meta tags are kept at the very top of <head>: Slackbot reads only the
// first 32KB of a response, so anything below that is invisible to it.
func writeLinkPreview(w http.ResponseWriter) {
	publicURL := hubPublicURL()
	previewHTML := `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8">
<title>Hive — AI agents that maintain your repo</title>
<meta name="description" content="Hive — the AI maintainer you own outright. AI agents triage issues, write fixes, patch CVEs, and merge on green behind six autonomy levels.">
<meta property="og:site_name" content="Hive">
<meta property="og:title" content="Hive — AI agents that maintain your repo">
<meta property="og:description" content="Put your repo on autopilot. Hive runs a fleet of AI agents on your backlog behind six autonomy levels — test coverage earns the confidence to raise a level, and you (the admin) choose when to raise it.">
<meta property="og:type" content="website">
<meta property="og:url" content="` + publicURL + `">
<meta property="og:image" content="` + publicURL + `/og-card.png">
<meta property="og:image:type" content="image/png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:image:alt" content="Hive — AI agents that maintain your repo">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Hive — AI agents that maintain your repo">
<meta name="twitter:description" content="Put your repo on autopilot. AI agents on your backlog behind six autonomy levels.">
<meta name="twitter:image" content="` + publicURL + `/og-card.png">
</head><body>
<h1>Hive</h1>
<p>AI agents that maintain your repo. <a href="` + publicURL + `">Sign in to continue.</a></p>
</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Preview cards are identical for every link and change only on deploy.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(linkPreviewMaxAge.Seconds())))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(previewHTML))
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
	_, _ = w.Write(data)
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
	// A signed-in user bounced here with a redirect target (a spoke's nginx
	// auth-signin, or any trusted ?redirect=) needs no provider round-trip —
	// the session cookie already proves who they are. Send them straight back
	// to the validated target instead of rendering the picker or re-entering
	// OAuth. Plain /login with no redirect still shows the picker so switching
	// accounts stays possible. The target is already validated by
	// loginRedirectTarget (isTrustedRedirectTarget), so this introduces no new
	// open-redirect surface.
	if redirect != "" {
		for _, value := range hubSessionCookieValues(r) {
			if u, ok := s.verifyHubUserCookie(value); ok && loadSaaSUser(u) != nil {
				http.Redirect(w, r, redirect, http.StatusSeeOther)
				return
			}
		}
	}
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
		// Redirect trust intentionally still spans sibling tenants: hosted hives
		// bounce here via their ingress auth-signin and must be returned home.
		// This is NOT the CSRF boundary — see isSameOriginAsHub (audit F4).
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
			p.AuthorizeURL, p.ClientID, oauthRedirectURI(), state)
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
	authURL, err := p.AuthCodeURL(oauthRedirectURI(), state, oidcNonce)
	if err != nil {
		s.logger.Warn("OIDC: cannot build authorize URL", "provider", p.Name, "error", err)
		http.Error(w, "login unavailable — provider not reachable", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// ibmLogoSVG is the official IBM 8-bar wordmark (the striped "IBM" letters), in
// IBM Blue. Used for the IBMid provider in the login picker. viewBox is wide
// (~2.4:1); callers give IBMid a wider glyph slot so it isn't squashed.
const ibmLogoSVG = `<svg viewBox="0 0 512 205" fill="#1F70C1" aria-hidden="true"><path d="M99.55552,190.060579 L99.55552,204.282819 L0,204.282819 L0,190.060579 L99.55552,190.060579 Z M255.1384,190.059939 C245.151671,199.241068 232.070596,204.31949 218.50496,204.282019 L218.50496,204.282019 L113.77792,204.141379 L113.77792,190.059939 Z M403.1664,190.059779 L398.2,204.282179 L393.2784,190.059779 L403.1664,190.059779 Z M355.55584,190.060579 L355.55584,204.282819 L284.44464,204.282819 L284.44464,190.060579 L355.55584,190.060579 Z M512,190.060579 L512,204.282819 L440.8888,204.282819 L440.8888,190.060579 L512,190.060579 Z M271.24672,162.908899 C270.026362,167.89787 268.099708,172.686973 265.52512,177.131139 L265.52512,177.131139 L113.77792,177.131139 L113.77792,162.908899 Z M412.6976,162.909379 L407.7056,177.131779 L388.7392,177.131779 L383.7472,162.909379 L412.6976,162.909379 Z M355.55584,162.908899 L355.55584,177.131139 L284.44464,177.131139 L284.44464,162.908899 L355.55584,162.908899 Z M512,162.908899 L512,177.131139 L440.8888,177.131139 L440.8888,162.908899 L512,162.908899 Z M99.55552,162.908899 L99.55552,177.131139 L0,177.131139 L0,162.908899 L99.55552,162.908899 Z M71.11104,135.757379 L71.11104,149.979779 L28.44432,149.979779 L28.44432,135.757379 L71.11104,135.757379 Z M184.88896,135.757379 L184.88896,149.979779 L142.22224,149.979779 L142.22224,135.757379 L184.88896,135.757379 Z M270.90576,135.757379 C272.166041,140.393192 272.805755,145.175711 272.80816,149.979779 L272.80816,149.979779 L224.96976,149.979779 L224.96976,135.757379 Z M422.2304,135.757379 L417.2368,149.979779 L379.208,149.979779 L374.2144,135.757379 L422.2304,135.757379 Z M355.55568,135.757379 L355.55568,149.979779 L312.88896,149.979779 L312.88896,135.757379 L355.55568,135.757379 Z M483.55552,135.757379 L483.55552,149.979779 L440.8888,149.979779 L440.8888,135.757379 L483.55552,135.757379 Z M71.11104,108.606019 L71.11104,122.828259 L28.44432,122.828259 L28.44432,108.606019 L71.11104,108.606019 Z M355.55568,108.606019 L355.55568,122.828259 L312.88896,122.828259 L312.88896,108.606019 L355.55568,108.606019 Z M483.55552,108.606019 L483.55552,122.828259 L440.8888,122.828259 L440.8888,108.606019 L483.55552,108.606019 Z M253.64576,108.605379 C258.382421,112.634795 262.394807,117.444874 265.50928,122.827459 L265.50928,122.827459 L142.22176,122.827459 L142.22176,108.605379 Z M431.7616,108.605379 L426.7696,122.827779 L369.6752,122.827779 L364.6832,108.605379 L431.7616,108.605379 Z M394.224,81.4549786 L398.2224,92.9509786 L402.2192,81.4549786 L483.5552,81.4549786 L483.5552,95.6773786 L440.8896,95.6773786 L440.8896,82.6085786 L436.3008,95.6773786 L360.144,95.6773786 L355.5552,82.6069786 L355.5552,95.6773786 L312.8896,95.6773786 L312.8896,81.4549786 L394.224,81.4549786 Z M142.22224,81.4543386 L265.51024,81.4551386 C262.395586,86.8377816 258.383042,91.6479099 253.64624,95.6773786 L253.64624,95.6773786 L142.22224,95.6773786 L142.22224,81.4543386 Z M71.11104,81.4543386 L71.11104,95.6765786 L28.44432,95.6765786 L28.44432,81.4543386 L71.11104,81.4543386 Z M71.11104,54.3029786 L71.11104,68.5252186 L28.44432,68.5252186 L28.44432,54.3029786 L71.11104,54.3029786 Z M184.88896,54.3029786 L184.88896,68.5252186 L142.22224,68.5252186 L142.22224,54.3029786 L184.88896,54.3029786 Z M272.80816,54.3031386 C272.805733,59.1071522 272.166019,63.8896155 270.90576,68.5253786 L270.90576,68.5253786 L224.96976,68.5253786 L224.96976,54.3031386 Z M384.7824,54.3029786 L389.728,68.5253786 L312.8896,68.5253786 L312.8896,54.3029786 L384.7824,54.3029786 Z M483.5552,54.3029786 L483.5552,68.5253786 L406.7168,68.5253786 L411.6624,54.3029786 L483.5552,54.3029786 Z M99.55552,27.1514586 L99.55552,41.3736986 L0,41.3736986 L0,27.1514586 L99.55552,27.1514586 Z M265.52512,27.1514586 C268.099627,31.5955505 270.026276,36.3845354 271.24672,41.3733786 L271.24672,41.3733786 L113.77792,41.3733786 L113.77792,27.1514586 Z M512,27.1509786 L512,41.3733786 L416.1584,41.3733786 L421.104,27.1509786 L512,27.1509786 Z M375.3408,27.1509786 L380.2864,41.3733786 L284.4448,41.3733786 L284.4448,27.1509786 L375.3408,27.1509786 Z M99.55552,9.85716419e-05 L99.55552,14.2223386 L0,14.2223386 L0,9.85716419e-05 L99.55552,9.85716419e-05 Z M218.50496,4.91529226e-05 C232.066886,-0.0182214039 245.141087,5.05759937 255.13792,14.2221786 L255.13792,14.2221786 L113.77792,14.2221786 L113.77792,4.91529226e-05 Z M512,0.000578571642 L512,14.2229786 L425.6,14.2229786 L430.5456,0.000578571642 L512,0.000578571642 Z M365.8992,0.000578571642 L370.8448,14.2229786 L284.4448,14.2229786 L284.4448,0.000578571642 L365.8992,0.000578571642 Z"/></svg>`

// providerGlyph returns a small inline SVG (or text) mark per provider for the
// picker. Inline SVG rather than an icon font: a Font Awesome codepoint would
// render as a tofu/striped box because no icon font is loaded, and the picker is
// deliberately self-contained (no external fetch). The GitHub mark uses
// currentColor so it inherits the button's foreground in any theme; Google and
// Microsoft use their official multi-color marks; IBMid is the striped wordmark.
func providerGlyph(name string) string {
	switch name {
	case "github":
		return `<svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>`
	case "google":
		return `<svg viewBox="0 0 18 18" aria-hidden="true"><path fill="#4285F4" d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62z"/><path fill="#34A853" d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.33A9 9 0 0 0 9 18z"/><path fill="#FBBC05" d="M3.97 10.72a5.4 5.4 0 0 1 0-3.44V4.95H.96a9 9 0 0 0 0 8.1l3.01-2.33z"/><path fill="#EA4335" d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.58C13.46.89 11.42 0 9 0A9 9 0 0 0 .96 4.95l3.01 2.33C4.68 5.16 6.66 3.58 9 3.58z"/></svg>`
	case "ibmid":
		// Official IBM 8-bar wordmark (the striped "IBM" letterform), in IBM Blue.
		// Wide aspect (~2.4:1); the .glyph CSS gives IBMid extra width so the
		// letters are not squashed.
		return ibmLogoSVG
	case "redhat":
		return `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M16.35 14.4c1.6 0 3.9-.33 3.9-2.24a1.8 1.8 0 0 0-.04-.44l-.95-4.12c-.22-.9-.41-1.31-2-2.12-1.24-.63-3.94-1.67-4.74-1.67-.74 0-.96.96-1.85.94-.86-.02-1.5-.74-2.3-.74-.77 0-1.27.52-1.66 1.6 0 0-1.08 3.05-1.22 3.49a.83.83 0 0 0-.03.25c0 1.2 4.71 5.32 10.88 5.32M20.47 12.94c.22 1.05.22 1.16.22 1.3 0 1.8-2.02 2.79-4.67 2.79-6 0-11.25-3.51-11.25-5.83 0-.32.07-.63.18-.93C2.94 10.36 1 10.63 1 12.34c0 2.8 6.63 6.26 11.87 6.26 4.02 0 5.03-1.82 5.03-3.25 0-1.13-.97-2.4-2.43-2.41"/></svg>`
	case "microsoft":
		// Microsoft's four-square logo (official brand colors).
		return `<svg viewBox="0 0 23 23" aria-hidden="true"><path fill="#F25022" d="M0 0h11v11H0z"/><path fill="#7FBA00" d="M12 0h11v11H12z"/><path fill="#00A4EF" d="M0 12h11v11H0z"/><path fill="#FFB900" d="M12 12h11v11H12z"/></svg>`
	case "custom":
		// Generic OIDC provider (operator-configured): a neutral key mark, since
		// we can't ship a third party's brand logo we don't know in advance.
		return `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M14 6a5 5 0 1 0-4.9 6L7 14v2H5v2H2v-3l6.1-6.1A5 5 0 0 0 14 6zm-4-1a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3z"/></svg>`
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
.card{width:min(440px,94vw);padding:44px 40px;border:1px solid #30363d;border-radius:18px;background:#161b22;text-align:center}
h1{font-size:26px;margin:0 0 6px;letter-spacing:-.01em}
p.sub{color:#8b949e;font-size:15px;margin:0 0 32px}
.prov{display:flex;align-items:center;gap:16px;width:100%;box-sizing:border-box;
padding:16px 22px;margin:12px 0;border:1px solid #30363d;border-radius:12px;background:#21262d;color:#e6edf3;
text-decoration:none;font-size:16px;font-weight:600;transition:background .12s,border-color .12s,transform .05s}
.prov:hover{background:#30363d;border-color:#8b949e}
.prov:active{transform:translateY(1px)}
.glyph{display:inline-flex;align-items:center;justify-content:center;min-width:32px;height:32px;flex:none}
.glyph svg{display:block;height:24px;width:auto;max-width:48px}
.foot{margin-top:28px;color:#6e7681;font-size:12px;line-height:1.5}
</style></head><body>
<div class="card">
<h1>Sign in to Hive</h1>
<p class="sub">Choose how you'd like to continue.</p>
` + buttons.String() + `<div class="foot">Your login provider controls access only. Hive's GitHub work runs through its own app.</div>
</div></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
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
		displayName string // provider-asserted human name (OIDC name claim/userinfo)
		ghToken     string // GitHub user access token, if any (stored encrypted)
	)
	if p.IsOIDC {
		claims, err := p.Exchange(r.Context(), code, oauthRedirectURI(), s.oidcNonceFromCookie(r))
		s.clearOIDCNonceCookie(w)
		if err != nil {
			// Server-side diagnostics only: log which step failed (discovery /
			// token_exchange / id_token_verify) and whether this browser still
			// carried its nonce cookie. The user sees only the generic message.
			s.logger.Warn("OIDC: callback verification failed",
				"provider", p.Name,
				"step", auth.FailedStep(err),
				"nonce_cookie_present", s.oidcNonceFromCookie(r) != "",
				"error", err)
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
		displayName = claims.Name
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

	// From here the two paths converge: mint the session cookie over the
	// canonical id (the Ed25519 signing machinery signs an opaque string, so
	// this is provider-agnostic) and persist the user record.
	if !s.mintSessionCookies(w, r, canonicalID) {
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
	// Backfill-by-login: every completed login UPSERTS the display claims onto
	// the existing record, so provider:sub users created before enrichment
	// shipped get their name on their next sign-in — no migration. Only
	// non-empty values overwrite, so a provider that stops sending a claim
	// never blanks a previously-stored one.
	if displayName != "" {
		saasUser.DisplayName = displayName
	}
	// Best-effort country inference, source 2 of 2 (see user_country.go). Reads
	// only the Accept-Language header this request already carried — no IP
	// geolocation, no third-party lookup, nothing sent off-box — and fills the
	// field ONLY when the record has none, so an explicit pick in the wizard is
	// never overwritten by a browser's language setting. Cannot fail: with no
	// region subtag it stores nothing and no flag renders.
	applyInferredCountry(saasUser, r)
	// A completed callback IS a login — count it here and nowhere else.
	// ensureSaaSUser already refreshed LastLogin; the count is the engagement
	// signal the admin Users card reads. Persist unconditionally below so a login
	// is recorded even when there is no token to encrypt (the token branch used to
	// be the only save path, so a token-encrypt failure silently dropped the whole
	// record update).
	saasUser.LoginCount++
	if ghToken != "" {
		if encrypted, err := encryptToken(ghToken); err == nil {
			saasUser.EncryptedToken = encrypted
		}
	}
	if err := saveSaaSUser(saasUser); err != nil {
		s.logger.Error("oauth callback: save failed", "user", saasUser.GitHubUsername, "error", err)
	}

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
//
// Uses the SAME cookie-matched decoding as verifyOAuthStateNonce, so the
// provider/redirect are always read from the exact state string whose nonce was
// verified — never from a differently-decoded sibling.
func (s *HubServer) parseCallbackState(r *http.Request) (provider, redirect string) {
	provider = "github"
	redirect = "/dashboard"
	decoded, _, ok, _ := s.matchedCallbackState(r)
	if !ok || decoded == "" {
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
	defer func() { _ = resp.Body.Close() }()

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
	defer func() { _ = userResp.Body.Close() }()

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

// mintSessionCookies sets the hub session cookie over the given canonical
// identity. Returns false (after writing an HTTP error) if the hub has no
// secret to sign with — an unsigned cookie would be trusted by default and
// must never be emitted.
//
// The value is a signed, tamper-evident token so it cannot be forged.
// N2: minted ASYMMETRICALLY. The hub holds the Ed25519 private seed; spokes
// receive only the public key, so a spoke operator can verify this cookie but
// can no longer forge one (notably an admin cookie). The signer signs an opaque
// string, so a canonical "provider:subject" identity rides through with no
// format change.
//
// AUDIT F10: production now mints V3, not V2.
//
// V2 carries only the username. Its lifetime lives entirely in the cookie's
// MaxAge attribute — a CLIENT-side hint the browser is free to ignore and an
// attacker who captured the value simply discards — and it has no session ID, so
// there is nothing for logout to revoke. Concretely: a stolen V2 cookie is valid
// forever, and /api/auth/logout was a no-op against it.
//
// V3 moves both facts inside the signature: a signed `exp` the verifier enforces
// regardless of what the client claims, and a random `sid` the hub records at
// logout so the verifier rejects it on demand (hub_session_revocation.go).
//
// WHY IT IS SAFE TO FLIP NOW. The V3 minting function has carried a comment
// saying nothing calls it in production, deliberately, because a spoke that
// cannot parse the shape breaks /terminal fleet-wide — incident #2773, the
// v2-hub/v4-spoke split. That condition no longer holds: the fleet is v4, and
// the spoke-side verifier in src/proxy/server.js tries the modern lanes first,
// so a spoke accepts a V3 cookie today. This is
// therefore the verify-both/mint-new cutover the N2 change already rehearsed,
// with the verifier side shipped well ahead of the minter — not a flag day.
//
// The TTL matches the cookie's MaxAge, so the signed expiry and the browser hint
// agree and nothing changes about how long a session lasts. The difference is
// only that the signed one is now the ENFORCED one.
//
// Note the residual on spokes: the proxy enforces signature and signed expiry
// but has no revocation store, so a revoked session can still reach a spoke
// until its expiry. Closing that requires the spoke to ask the hub on the
// terminal path; it is tracked separately and called out in hub_session_revocation.go.
func (s *HubServer) mintSessionCookies(w http.ResponseWriter, r *http.Request, canonicalID string) bool {
	// ROTATION (master-key-rotation.md, follow-on PR #1): mint under the CURRENT
	// generation and stamp its ID into the signed claims, so the verifier can
	// select one key instead of trying each and so telemetry can see which key
	// a live session is on. Minting is current-ONLY — dual acceptance is a
	// property of the verifier alone, and a minter that also used a previous
	// generation would mean a rotation never converged.
	//
	// On a hub that has never rotated this is the same seed and the same bytes
	// as before, minus a `g:1` claim the Node proxy ignores. keyGenerations is
	// nil only in hand-built test servers, which stay on the unmarked mint.
	var cookieValue string
	if s.keyGenerations != nil {
		cookieValue, _ = mintHubUserCookieValueV3ForGeneration(
			s.keyGenerations, canonicalID, time.Now(), cookieSessionTTL)
	} else {
		cookieValue, _ = mintHubUserCookieValueV3(
			s.sessionSigningSeed(), canonicalID, time.Now(), cookieSessionTTL)
	}
	if cookieValue == "" {
		s.logger.Warn("OAuth: cannot mint signed session cookie", "user", canonicalID)
		http.Error(w, "session unavailable", http.StatusInternalServerError)
		return false
	}
	setSessionCookies(w, r, cookieValue)
	return true
}

// setSessionCookies issues the Set-Cookie pair for an already-minted session
// value: the live hive_hub_user cookie scoped to sessionCookieDomain(r.Host),
// preceded by an expiry of the legacy .hive.kubestellar.io-scoped copy.
//
// Factored out of mintSessionCookies (#4193) so the SAME cookies can be
// re-issued outside the login callback: a session minted BEFORE the #4171
// domain widening still rides a host-only or .hive.kubestellar.io-scoped
// cookie the browser never sends to dibs.kubestellar.io, and only a fresh
// Set-Cookie can rescope it. handleAuthUser re-issues the verified value on
// every authenticated dashboard load for exactly that reason — no new crypto,
// just the delivery scope.
func setSessionCookies(w http.ResponseWriter, r *http.Request, cookieValue string) {
	// AUDIT F4, DELIBERATELY NOT CHANGED — read before "fixing" this.
	//
	// The audit asks for a host-only `__Host-` session cookie. That is correct
	// in the abstract and NOT safely applicable here, because this cookie is
	// load-bearing across a trust boundary: it is minted by the hub (Go) and
	// verified INDEPENDENTLY by every spoke's Node proxy on the WebSocket
	// terminal path (src/proxy/server.js). The proxy can only verify a cookie
	// the browser actually sends it, and the browser only sends this one to <id>.hive.kubestellar.io
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
	//
	// #4171 WIDENS the Domain one level, from .hive.kubestellar.io to the hub's
	// registrable domain (derived, not hard-coded — sessionCookieDomain in
	// saas.go), so first-party sibling products (dibs) receive the cookie and can
	// SSO against /api/saas/whoami. This does not change the F4 analysis above:
	// every host that received the cookie before (all *.hive.<domain> spoke
	// dashboards — deeper subdomains of the new parent scope) still receives it,
	// and the hosts ADDED are exclusively hive-operated first-party services
	// under that same domain, not tenant-controlled origins. The tenant-side risk
	// profile is therefore unchanged, and the same isSameOriginAsHub containment
	// applies. Local/dev hosts get a host-only cookie instead (a browser rejects
	// a non-covering Domain).
	//
	// SameSite=Lax is correct only while the sibling shares the hub's registrable
	// domain — same-SITE, so the browser attaches the cookie without needing
	// SameSite=None. That is a precondition of the bridge, not a property of the
	// hostnames: a sibling on a DIFFERENT registrable domain is both cross-site
	// and outside the cookie's scope, and is signed out. Moving the hub across
	// domains therefore strands every sibling left behind, which is why the old
	// sibling host redirects rather than dual-serves (#5925).
	domain := sessionCookieDomain(r.Host)
	// Rollout hygiene (#4171): a pre-widening session cookie scoped
	// Domain=.hive.kubestellar.io is a SEPARATE jar entry from the one minted
	// below, and the browser would send both under the same name. Expire the
	// legacy-scoped copy in the same response so a fresh login converges to
	// exactly one session cookie. (Requests that still carry both in the
	// interim are handled by hubSessionCookieValues, which tries every copy.)
	// Emitted BEFORE the real cookie so anything scanning Set-Cookie in order
	// finds the live session last.
	for _, clearDomain := range legacySessionCookieDomains(domain) {
		http.SetCookie(w, &http.Cookie{
			Name:     "hive_hub_user",
			Value:    "",
			Path:     "/",
			Domain:   clearDomain,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	cookie := &http.Cookie{
		Name:     "hive_hub_user",
		Value:    cookieValue,
		Path:     "/",
		Domain:   domain,
		MaxAge:   86400 * cookieMaxAgeDays,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, cookie)
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
// pre-multi-provider behavior. For an OIDC user it prefers the STORED
// provider-asserted display name, then email, then the canonical id — and the
// stored avatar (Google/Microsoft provide a picture) — never nothing.
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
		switch {
		case u.DisplayName != "":
			login = u.DisplayName
		case u.Email != "":
			login = u.Email
		}
		avatarURL = u.AvatarURL
	}
	return login, avatarURL
}

// identityLabeler returns a per-request memoized resolver from a canonical (or
// legacy bare) identity to its human display label — displayIdentity's login
// half, cached so a list that repeats the same few owners does one lookup per
// DISTINCT identity instead of one stored-user read per row. Empty in, empty
// out. NOT safe for concurrent use; intended for a single handler invocation.
func (s *HubServer) identityLabeler() func(string) string {
	cache := map[string]string{}
	return func(identity string) string {
		if identity == "" {
			return ""
		}
		if l, ok := cache[identity]; ok {
			return l
		}
		l, _ := s.displayIdentity(identity)
		cache[identity] = l
		return l
	}
}

func (s *HubServer) handleAuthUser(w http.ResponseWriter, r *http.Request) {
	// Trust a carried username only when its signature verifies; a legacy
	// unsigned or forged cookie reports unauthenticated, prompting a re-login.
	// N2: accept v2 (Ed25519) or, during rollout only, the legacy HMAC format.
	// F10: also enforces a v3 cookie's signed expiry and revocation state.
	// #4171: every hive_hub_user copy is tried (the domain-widening rollout can
	// briefly leave two in the jar), matching getRealAuthUser.
	var username, sessionValue string
	for _, value := range hubSessionCookieValues(r) {
		if u, ok := s.verifyHubUserCookie(value); ok && loadSaaSUser(u) != nil {
			username = u
			sessionValue = value
			break
		}
	}
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated":false}`))
		return
	}
	// #4193: re-scope the session cookie on every authenticated dashboard load.
	// A session minted BEFORE the #4171 domain widening still rides a cookie
	// scoped host-only or to .hive.kubestellar.io, which the browser never
	// sends to dibs.kubestellar.io — SSO silently fails for exactly the users
	// who were already signed in when the widening shipped, until they happen
	// to log out and back in. Re-issuing the SAME verified value with
	// Domain=sessionCookieDomain(r.Host) (plus the legacy-scope expiry) here
	// converges those sessions without new crypto or a forced re-login.
	//
	// This endpoint and not every API call: the dashboard fetches
	// /api/auth/user exactly once on load, so the Set-Cookie pair rides one
	// cheap request per page view instead of spamming every poll. Re-setting
	// an already-wide cookie is a no-op for the jar (same name/domain/path),
	// and the refreshed MaxAge is only the browser-side hint — the session's
	// real lifetime is the SIGNED expiry inside the value, which this does not
	// touch. Dev/localhost hosts keep their host-only cookie via
	// sessionCookieDomain, same as the login callback.
	setSessionCookies(w, r, sessionValue)
	isAdmin := isHubAdmin(username)
	displayLogin, avatar := s.displayIdentity(username)
	// The viewer's own country, for the flag beside their nav avatar. Sent as
	// the alpha-2 CODE, not the glyph, so the dashboard derives the emoji the
	// same way every other render site does — and so an empty value is
	// unambiguously "we do not know" rather than an empty-looking glyph.
	// Absent from the payload entirely when unset (omitted below).
	var country string
	if u := loadSaaSUser(username); u != nil {
		country = normalizeCountryCode(u.Country)
	}
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
	// Only ship the key when there is a country to ship. An absent key and an
	// empty string both render no flag, but omitting it keeps the payload for
	// the (currently vast) majority of users byte-identical to before.
	if country != "" {
		payload["country"] = country
	}
	if grant, ok := s.activeImpersonationGrant(r); ok {
		targetLogin, targetAvatar := s.displayIdentity(grant.Target)
		payload["login"] = targetLogin
		payload["avatar_url"] = targetAvatar
		payload["hub_admin"] = false
		// Every per-user view renders AS the target, so the flag must follow the
		// target too — otherwise the admin's own flag would sit beside the
		// impersonated user's face. Delete rather than blank it when the target
		// has no country, so no stale flag survives the swap.
		delete(payload, "country")
		if tu := loadSaaSUser(grant.Target); tu != nil {
			if tc := normalizeCountryCode(tu.Country); tc != "" {
				payload["country"] = tc
			}
		}
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
	_, _ = w.Write(data)
}

func (s *HubServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	// AUDIT F10: deleting the browser's copy is not logging out. Anyone who
	// captured the cookie value keeps a working session until its own expiry.
	// Record the session ID server-side so the verifier rejects it from now on.
	// Now load-bearing: minting emits v3 (see mintSessionCookies).
	//
	// AUDIT F15: this route is deliberately unauthenticated — logout must work
	// even when the session is already broken, and requiring auth to log out is
	// its own denial of service. But it used to pass the RAW cookie value
	// straight to revokeHubSessionCookie, and revocation writes to a persisted
	// store. An anonymous attacker could POST arbitrary attacker-chosen values
	// and grow that store without bound: every entry lands on the PVC, is
	// rewritten in full on each revocation, and is loaded on every hub start. The
	// entries were also stored until the *cookie's own claimed* expiry, which is
	// attacker-supplied, so a forged far-future exp pinned an entry for as long
	// as the attacker liked.
	//
	// The fix is to verify BEFORE revoking. verifyHubUserCookie checks the
	// Ed25519 signature, so a session ID can only enter the store if the hub
	// itself minted the cookie — the store is now bounded by sessions the hub
	// actually issued, not by requests an attacker can send. A caller presenting
	// a real cookie is not an attacker; they are the owner of that session,
	// logging out.
	//
	// The browser's copy is cleared unconditionally regardless, below: a client
	// with a corrupt or expired cookie must still be able to clear it.
	// #4171: revoke EVERY verifiable copy — the domain-widening rollout can
	// leave two hive_hub_user cookies (legacy .hive.kubestellar.io scope and
	// the new parent scope) carrying different session IDs, and logging out
	// must kill both sessions, not just whichever the jar sent first.
	for _, value := range hubSessionCookieValues(r) {
		if _, ok := s.verifyHubUserCookie(value); ok {
			s.revokeHubSessionCookie(value)
		} else {
			// Not an error path worth surfacing to the client — an expired or
			// already-revoked cookie logging out is entirely normal — but it is
			// worth counting, because a spike is what an enumeration attempt
			// against this route looks like.
			s.logger.Debug("logout: unverifiable session cookie, nothing to revoke")
		}
	}
	// Clear the browser's copy under the SAME Domain minting used — a deletion
	// only removes the jar entry whose domain matches. Also clear the legacy
	// .hive.kubestellar.io-scoped entry (#4171 rollout: a pre-widening session
	// is a separate jar entry that a parent-scoped deletion cannot remove).
	liveDomain := sessionCookieDomain(r.Host)
	clearDomains := append([]string{liveDomain}, legacySessionCookieDomains(liveDomain)...)
	for _, domain := range clearDomains {
		http.SetCookie(w, &http.Cookie{
			Name:     "hive_hub_user",
			Value:    "",
			Path:     "/",
			Domain:   domain,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
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
//
// Logs (server-side only) WHY a rejection happened — no_cookie / no_state /
// nonce_mismatch — and whether a non-standard decode depth matched, so a
// provider that changes its state round-trip encoding is diagnosable from one
// log line instead of a generic warn.
func (s *HubServer) verifyOAuthStateNonce(r *http.Request) bool {
	_, depth, ok, reason := s.matchedCallbackState(r)
	if !ok {
		s.logger.Warn("OAuth: state nonce rejected", "reason", reason)
		return false
	}
	if depth != stateDecodeDepthStandard {
		// Not a failure — the nonce DID match — but the provider round-tripped
		// state at an unexpected encoding depth. Worth a log line: this is the
		// canary for an IdP that starts re-encoding or pre-decoding state.
		s.logger.Warn("OAuth: state matched at non-standard decode depth", "decode_depth", depth)
	}
	return true
}

// State decode depths, named for the matchedCallbackState log line. The hub
// QueryEscapes state once and url.Values.Encode() escapes it again on the OIDC
// authorize hop, so after Go's automatic query decode the STANDARD shape needs
// exactly one more unescape (depth 1). Depth 0 is a provider that returned the
// state pre-decoded (or the GitHub path, whose manual URL interpolation escapes
// only once — a no-op second unescape). Depth 2 tolerates a provider that
// re-encoded the state an extra time on the return trip.
const (
	stateDecodeDepthRaw      = 0
	stateDecodeDepthStandard = 1
	stateDecodeDepthExtra    = 2
)

// callbackStateCandidates returns plausible decodings of the callback's state
// parameter, indexed by decode depth (see the depth constants). r.URL.Query()
// already applied one decode; index 0 is that raw value, index 1 the standard
// one-more-unescape shape, index 2 an extra tolerance pass. Candidates that
// fail to unescape are simply absent — selection is gated on the cookie nonce
// match, so a bogus candidate can never be chosen over a legitimate one.
func callbackStateCandidates(r *http.Request) []string {
	raw := r.URL.Query().Get("state")
	if raw == "" {
		return nil
	}
	out := []string{raw}
	d1, err := url.QueryUnescape(raw)
	if err != nil {
		return out
	}
	out = append(out, d1)
	if d2, err := url.QueryUnescape(d1); err == nil {
		out = append(out, d2)
	}
	return out
}

// matchedCallbackState returns the decoded state whose leading nonce segment
// matches this browser's state cookie, plus the decode depth it matched at.
// The nonce comparison is constant-time per candidate; candidate strings derive
// only from public URL structure, so trying up to three shapes leaks nothing.
// On failure, reason is one of "no_cookie", "no_state", "nonce_mismatch".
func (s *HubServer) matchedCallbackState(r *http.Request) (state string, depth int, ok bool, reason string) {
	cookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || cookie.Value == "" {
		return "", 0, false, "no_cookie"
	}
	cands := callbackStateCandidates(r)
	if len(cands) == 0 {
		return "", 0, false, "no_state"
	}
	// Standard depth first so, when several depths decode identically (a nonce
	// is hex and unescapes to itself), the reported depth is the expected one.
	order := []int{stateDecodeDepthStandard, stateDecodeDepthRaw, stateDecodeDepthExtra}
	for _, i := range order {
		if i >= len(cands) {
			continue
		}
		nonce, _, found := strings.Cut(cands[i], oauthStateSeparator)
		if !found || nonce == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(nonce), []byte(cookie.Value)) == 1 {
			return cands[i], i, true, ""
		}
	}
	return "", 0, false, "nonce_mismatch"
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

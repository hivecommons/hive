package dashboard

import (
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

func (s *Server) handleGHAuth(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.GitHub
	authType := "token"
	if cfg.HasApp() {
		authType = "app"
	}
	jsonResponse(w, map[string]interface{}{
		"ok":              true,
		"type":            authType,
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
	})
}

var userTokenPath = "/data/gh-user-token"

func (s *Server) handleGHUserAuthStatus(w http.ResponseWriter, r *http.Request) {
	// The login status must reflect THIS request's user, not the single
	// persisted token. On a direct-route spoke, resolving from the per-user
	// session is the only correct answer — otherwise every visitor would see
	// the last-authenticated user's identity (the reported vulnerability).
	// The role is the LIVE allowlist role (session_live_role.go), not the one
	// frozen into the session at login, so the UI's idea of the user's
	// capabilities always matches what the gated endpoints will enforce; a
	// revoked session reports logged-out rather than a ghost identity.
	if sess := s.sessionFromRequest(r); sess != nil {
		if live, ok := s.liveSessionRole(sess); ok {
			jsonResponse(w, map[string]interface{}{"logged_in": true, "username": sess.Username, "role": live})
			return
		}
	}
	// Hub-proxied path: nginx injects the per-user X-Hive-User/X-Hive-Role, so
	// report THAT user rather than the single shared persisted token (which
	// would show every proxied visitor the owner's identity).
	if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		jsonResponse(w, map[string]interface{}{"logged_in": true, "username": hubUser, "role": r.Header.Get("X-Hive-Role")})
		return
	}
	if s.directRouteAuthzEnabled() || s.hubProxied() || s.authToken != "" {
		// No valid session on a direct-route spoke → not logged in for this
		// request, regardless of any persisted owner token on disk. Same on a
		// hub-proxied spoke: nginx injects X-Hive-User for every signed-in
		// visitor, so its absence means anonymous, and the persisted owner
		// token must not stand in for them (see resolveViewerUsername).
		// Same again behind a dashboard auth token (#7394): this endpoint is
		// public, so an anonymous caller reaches it on a token-protected spoke
		// and must read as logged-out, not as the owner.
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	tokenData, err := os.ReadFile(userTokenPath)
	if err != nil || len(strings.TrimSpace(string(tokenData))) == 0 {
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	token := strings.TrimSpace(string(tokenData))
	user, err := github.ValidateTokenCached(token, s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		jsonResponse(w, map[string]interface{}{"logged_in": false, "error": "token expired or revoked"})
		return
	}
	jsonResponse(w, map[string]interface{}{"logged_in": true, "username": user.Login, "avatar_url": user.AvatarURL})
}

func (s *Server) handleGHUserAuthStart(w http.ResponseWriter, r *http.Request) {
	// Always resolves to a github.com client ID (configured or the public
	// default) — login is github.com even on GHE hives, so this never fails for
	// a blank oauth_client_id and never uses a GHE client.
	clientID := s.deps.Config.GitHub.OAuthClientIDResolved()

	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	state, err := github.StartDeviceFlow(clientID, s.deps.Config.GitHub.OAuthBaseURL(), s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Bind the flow to THIS caller. Both start and poll are public
	// (isPublicPath), and the session cookie is minted on the POLL response —
	// so without a client-held secret, any unauthenticated poller could race
	// the legitimate operator and walk away with their freshly approved
	// session. flow_id is that secret: crypto-random, returned only to the
	// caller who started the flow, and required (constant-time) on every poll.
	// The GitHub device_code stays server-side as before.
	flowID, err := newDeviceFlowID()
	if err != nil {
		jsonError(w, "failed to start device flow", http.StatusInternalServerError)
		return
	}
	s.deviceFlowState = state
	s.deviceFlowID = flowID
	s.auditFromRequest(r, "gh_auth_start", "", "")
	jsonResponse(w, map[string]interface{}{
		"user_code":        state.UserCode,
		"verification_uri": state.VerificationURI,
		"expires_in":       state.ExpiresIn,
		"interval":         state.Interval,
		"flow_id":          flowID,
	})
}

// newDeviceFlowID mints the opaque per-flow secret handed to the client that
// starts a device flow. 128 bits of crypto randomness, hex-encoded.
func newDeviceFlowID() (string, error) {
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Server) handleGHUserAuthPoll(w http.ResponseWriter, r *http.Request) {
	// Read the caller's flow binding BEFORE taking the lock — decodeBody does
	// network I/O and must not serialize behind another poll's GitHub call.
	var pollReq struct {
		FlowID string `json:"flow_id"`
	}
	_ = decodeBody(r, &pollReq) // absent/invalid body leaves FlowID empty; enforced below

	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	if s.deviceFlowState == nil {
		jsonError(w, "no device flow in progress — call /api/gh-user-auth/start first", http.StatusBadRequest)
		return
	}
	// Enforce the client binding whenever this flow was minted with one (every
	// flow started through handleGHUserAuthStart is). A poll that cannot prove
	// it started the flow gets nothing — in particular it must never be the
	// request the session cookie is set on. Constant-time compare: flow_id is
	// a secret. State stays intact so the legitimate holder's polls proceed.
	if s.deviceFlowID != "" &&
		subtle.ConstantTimeCompare([]byte(pollReq.FlowID), []byte(s.deviceFlowID)) != 1 {
		jsonError(w, "no device flow in progress — call /api/gh-user-auth/start first", http.StatusBadRequest)
		return
	}

	clientID := s.deps.Config.GitHub.OAuthClientIDResolved()
	token, status, scope, err := github.PollDeviceFlowWithScope(clientID, s.deviceFlowState.DeviceCode, s.deps.Config.GitHub.OAuthBaseURL(), s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		s.deviceFlowState = nil
		s.deviceFlowID = ""
		jsonResponse(w, map[string]interface{}{"status": "error", "error": err.Error()})
		return
	}
	if status == "authorization_pending" {
		jsonResponse(w, map[string]interface{}{"status": "pending"})
		return
	}
	if status == "slow_down" {
		jsonResponse(w, map[string]interface{}{"status": "slow_down"})
		return
	}

	// Resolve the GitHub identity BEFORE persisting anything. An unauthorized
	// user's token must never be written to disk or wired in as the hive's user
	// client — otherwise a rejected login would still leak its token into the
	// shared client and become the hive's identity.
	user, err := github.ValidateTokenCached(token, s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil || user == nil || user.Login == "" {
		s.deviceFlowState = nil
		s.deviceFlowID = ""
		// Audit the failed login so the owner can see attempts that never got
		// far enough to resolve a GitHub identity. Actor is "unknown" because we
		// could not verify who they are.
		s.audit.Log("unknown", "login_error", auditDetail("reason", "could not verify GitHub identity"), "")
		jsonResponse(w, map[string]interface{}{"status": "error", "error": "could not verify GitHub identity"})
		return
	}
	s.deviceFlowState = nil
	s.deviceFlowID = ""
	username := user.Login
	avatarURL := user.AvatarURL

	// Per-hive authorization: on a direct-route spoke, only GitHub users on the
	// configured allowlist may obtain a session. The hub-proxied path is gated
	// upstream by nginx and leaves the allowlist empty, so it is unaffected.
	role := config.RoleOwner
	if s.directRouteAuthzEnabled() {
		resolvedRole, ok := s.deps.Config.Dashboard.AuthorizedRole(username)
		if !ok {
			// Do NOT persist the token, do NOT set a session, do NOT log the
			// user in. Reject before any state is written.
			s.deps.Logger.Warn("device-flow login rejected: user not authorized for this hive", "username", username)
			// Audit the denied login with the attempted GitHub username as the
			// actor so the owner can see WHO tried and was rejected.
			s.audit.Log(username, "login_denied", auditDetail("reason", "not authorized for this hive"), "")
			// Return a terminal {status:"error"} the login page understands, not a
			// bare jsonError — the device-flow poll loop only stops on status
			// "complete" or "error", so a plain {error} would leave it spinning
			// "Waiting for authorization…" forever after a denial.
			jsonResponse(w, map[string]interface{}{
				"status": "error",
				"error":  "your GitHub account (" + username + ") is not authorized to access this hive. Contact the hive owner to request access.",
			})
			return
		}
		role = resolvedRole
	}

	// The login token stays bound to THIS per-user session. It is used only when
	// GitHub granted public_repo/repo so feedback issues can be created as the
	// submitter; it is never installed as the hive's shared GitHub identity.

	s.deps.Logger.Info("GitHub user authenticated via device flow", "username", username, "role", role)

	// Issue a per-user session (opaque random id → username+role) instead of a
	// single shared cookie. Each authenticated user gets their own session so
	// requests resolve to the user that owns their cookie — never a shared one.
	//
	// Always mint the session. This used to be gated on s.authToken != "", which
	// meant a spoke with no dashboard token logged "authenticated via device
	// flow", returned {status:"complete"}, and set NO cookie at all — so "/"
	// rejected the request and the login page bounced forever (same failure
	// handleSSO fixed). The session store is the authority on identity here and
	// does not depend on a shared token existing.
	sid := s.createUserSessionWithToken(username, role, token, scope)
	if sid == "" {
		jsonResponse(w, map[string]interface{}{"status": "error", "error": "failed to create session"})
		return
	}
	setSessionCookie(w, r, sid)
	// Mint the short-lived per-hive terminal assertion cookie the Node proxy
	// verifies as its PRIMARY per-hive gate (finding C3 follow-up). Same
	// {user,hive,role} just authorized for this session, bound to THIS hive
	// with an expiry. No-op on non-hosted hives.
	s.setTerminalAssertionCookie(w, r, username, role)

	// Audit the successful login with the authenticated GitHub username as the
	// actor (not the request's X-Hive-User, which has no session yet at this
	// point) so the owner can see WHO logged in.
	s.audit.Log(username, "login", auditDetail("method", "github device flow", "role", role), "")
	jsonResponse(w, map[string]interface{}{"status": "complete", "username": username, "avatar_url": avatarURL})
}

// handleSSO exchanges a hub-minted, HMAC-signed handoff token for a local
// per-user session, so a user already authenticated on the hub can open this
// direct-route spoke without a second GitHub device-flow login. It fails closed:
// the token must verify against the shared HIVE_HUB_SECRET, be scoped to THIS
// hive, be unexpired, AND carry a username that is in this spoke's
// authorized_users allowlist. On success it mints the same kind of session the
// device flow does and redirects to the dashboard root.
func (s *Server) handleSSO(w http.ResponseWriter, r *http.Request) {
	// Loop breaker. An authentication failure must never present as an infinite
	// redirect: if this navigation has already been through the handoff
	// maxSSOHops times, something between here and "/" is bouncing us and no
	// further hop will help. Stop and say so.
	hop, _ := strconv.Atoi(r.URL.Query().Get(ssoHopParam))
	if hop >= maxSSOHops {
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff loop detected", "hops", hop)
		}
		writeSSOError(w, r, http.StatusLoopDetected, ssoErrLoopDetected,
			"Signing in to this hive redirected in a circle, so it was stopped before your browser gave up.",
			"Sign in directly with GitHub below. If that also fails, the hive's access settings likely disagree with the hub's — ask the hive operator to check that your account is on this hive's authorized-users list.")
		return
	}

	// SSO verification uses the hub's Ed25519 PUBLIC key (C2 follow-up: SSO is
	// asymmetric). A hub-hosted spoke is injected HIVE_SSO_PUBLIC_KEY and never the
	// master or any signing seed, so a spoke operator who reads its pod env cannot
	// mint SSO-as-any-owner tokens — only the hub, holding the private seed, can
	// sign. SpokeSSOPublicKey falls back to deriving the public key from
	// HIVE_HUB_SECRET for self-hosted/legacy spokes that still hold the master, so
	// verification succeeds against the hub-minted token either way.
	// ROTATION (master-key-rotation.md follow-on #6): a spoke may hold TWO hub
	// public keys during a master rotation — the current generation's and the
	// outgoing one's — because the hub starts minting under the new generation
	// immediately while the reconcile lane takes ~6h to walk the fleet. Trying
	// only the primary key would 401 every SSO handoff into a not-yet-reconciled
	// spoke for those hours. SpokeSSOPublicKeys returns current-then-previous,
	// and returns exactly ONE key — byte-identical to SpokeSSOPublicKey — when
	// HIVE_SSO_PUBLIC_KEY_PREV is absent, which is its state on every spoke
	// today and on every spoke that has never seen a rotation.
	pubKeys := spoke.SpokeSSOPublicKeys()
	if len(pubKeys) == 0 {
		// No verification key → SSO cannot be verified. Terminate with an
		// explanation. Redirecting to "/" here is what produced the historical
		// infinite bounce: "/" is auth-gated, sends the user back to the hub
		// login, the hub sees a valid session and hands off to /sso again.
		writeSSOError(w, r, http.StatusServiceUnavailable, ssoErrNoSecret,
			"This hive has no hub SSO verification key configured, so single sign-on from the hub cannot be verified.",
			"Ask the hive operator to set HIVE_HUB_SECRET (or HIVE_SSO_PUBLIC_KEY) on this hive. In the meantime you can sign in directly with GitHub using the button below.")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		writeSSOError(w, r, http.StatusBadRequest, ssoErrMissingToken,
			"This single sign-on link is missing its handoff token.",
			"Open the hive from the hub dashboard rather than pasting the /sso URL directly.")
		return
	}

	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}

	username, tokenRole, _, err := spoke.VerifySSOTokenAcrossKeys(pubKeys, token, hiveID, time.Now())
	if err != nil {
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff rejected", "error", err.Error())
		}
		// Don't leak which check failed, but DO terminate here with an
		// explanation rather than serving anything that re-enters the handoff.
		writeSSOError(w, r, http.StatusUnauthorized, ssoErrBadToken,
			"The single sign-on handoff token was rejected — it is expired, malformed, or was issued for a different hive.",
			"Go back to the hub dashboard and open this hive again to get a fresh link. Handoff links are short-lived by design.")
		return
	}

	// The token proves the hub authenticated this user, but authorization is
	// still LOCAL to the spoke: the user must be in THIS hive's allowlist. This
	// keeps the spoke the authority on who may enter, even via SSO — a valid
	// hub token for a user not on the allowlist is refused. The role comes from
	// the allowlist (authoritative), not the token, so the hub can never
	// escalate a user's role on the spoke. liveAllowlistRole is the same shared
	// rule authenticate re-applies on EVERY subsequent request
	// (session_live_role.go), so the role minted here can never outlive a later
	// Manage Access change.
	role, authorized := s.liveAllowlistRole(username, tokenRole)
	if !authorized {
		// Allowlist is enforced and this user isn't on it → deny.
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff: user not authorized for this hive", "username", username)
		}
		writeSSOError(w, r, http.StatusForbidden, ssoErrNotAuthorized,
			"You are signed in to the hub, but this hive's own authorized-users list does not include your account.",
			"Ask the hive owner to grant you access to this hive, then open it again from the hub dashboard.")
		return
	}
	if role == "" {
		role = config.RoleRead
	}

	// Always mint the session. This used to be gated on s.authToken != "", which
	// meant a spoke with no dashboard token logged "authenticated via SSO",
	// redirected to "/", and set NO cookie at all — so "/" rejected the request
	// and the browser bounced forever. The session store is the authority on
	// identity here and does not depend on a shared token existing.
	sid := s.createUserSession(username, role)
	if sid == "" {
		writeSSOError(w, r, http.StatusInternalServerError, ssoErrSessionFailed,
			"The hive could not create a login session for you.",
			"This is a problem on the hive itself. Retry in a moment; if it persists, ask the hive operator to check the dashboard logs.")
		return
	}
	setSessionCookie(w, r, sid)
	// Mint the short-lived per-hive terminal assertion cookie the Node proxy
	// verifies as its PRIMARY per-hive gate (finding C3 follow-up). The role here
	// is the spoke's authoritative allowlist role (resolved above), bound to THIS
	// hive with an expiry. No-op on non-hosted hives.
	s.setTerminalAssertionCookie(w, r, username, role)
	s.audit.Log(username, "login", auditDetail("method", "hub sso handoff", "role", role), "")
	if s.deps != nil && s.deps.Logger != nil {
		s.deps.Logger.Info("user authenticated via hub SSO handoff", "username", username, "role", role)
	}
	// Land on the dashboard root, carrying a hop counter. If something upstream
	// bounces us back into /sso anyway, the counter lets the NEXT handoff detect
	// the cycle and stop with an error instead of spinning (see the ssoHopParam
	// check at the top of this handler).
	http.Redirect(w, r, "/?"+ssoHopParam+"="+strconv.Itoa(hop+1), http.StatusSeeOther)
}

// SSO handoff failure identifiers, surfaced on the terminal error page so a
// user can quote one to an operator and an operator can grep for it.
const (
	ssoErrNoSecret      = "SSO_NO_HUB_SECRET"
	ssoErrMissingToken  = "SSO_MISSING_TOKEN"
	ssoErrBadToken      = "SSO_TOKEN_REJECTED"
	ssoErrNotAuthorized = "SSO_NOT_AUTHORIZED"
	ssoErrSessionFailed = "SSO_SESSION_FAILED"
	ssoErrLoopDetected  = "SSO_REDIRECT_LOOP"
)

// ssoHopParam is the query parameter carrying how many times this browser has
// been through the SSO handoff for this navigation. The hub's handoff link
// carries no hop, so a normal first visit is hop 0; each success redirect
// increments it. Anything that bounces the browser back into /sso preserves the
// query string, so a genuine cycle counts upward and trips maxSSOHops.
const ssoHopParam = "sso_hop"

// maxSSOHops is how many SSO handoffs are allowed for one navigation before we
// declare a redirect loop and stop. A healthy handoff takes exactly one hop; a
// small allowance absorbs a legitimate re-handoff (e.g. a racing session
// expiry) without letting a true cycle run away. Browsers give up around 20
// redirects with an opaque error, so we must terminate well before that to be
// the one that explains what happened.
const maxSSOHops = 3

// writeSSOError terminates an SSO handoff with a self-contained HTML page that
// says what failed and what to do about it. It exists because the alternative —
// redirecting a failed handoff back toward login — is what produces an infinite
// browser bounce, the single worst failure mode here: it tells the user nothing
// and is indistinguishable from an outage. Any non-success exit from handleSSO
// must come through this function.
//
// It also clears any stale session cookie, so a half-established session cannot
// keep re-triggering the same failure on reload.
func writeSSOError(w http.ResponseWriter, r *http.Request, status int, code, what, action string) {
	clearSessionCookie(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A failed handoff must never be cached; a cached 401/403 would make the
	// hive look permanently broken even after access is granted.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, ssoErrorPage, html.EscapeString(code), html.EscapeString(what), html.EscapeString(action))
}

// ssoErrorPage is the terminal error shell for a failed SSO handoff. Format
// verbs in order: %[1]s error code, %[2]s what happened, %[3]s what to do. It
// deliberately offers "/" (which serves the device-flow login page when
// unauthenticated) as an escape hatch and a link back to the hub, so the user
// is never stranded.
const ssoErrorPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign-in failed — Hive</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh;padding:24px}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:40px;max-width:560px}
.bee{font-size:2.5rem;margin-bottom:12px}
h1{font-size:1.5rem;margin-bottom:16px}
p{color:#8b949e;line-height:1.6;margin-bottom:16px}
.action{color:#e6edf3}
code{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:2px 8px;font-size:0.8rem;color:#f0883e}
.btn{display:inline-block;padding:10px 20px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:16px 8px 0 0}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
</style></head>
<div class="card">
<div class="bee">&#128029;</div>
<h1>Sign-in didn't complete</h1>
<p>%[2]s</p>
<p class="action">%[3]s</p>
<p>Error code: <code>%[1]s</code></p>
<a class="btn btn-primary" href="/">Sign in with GitHub</a>
<a class="btn btn-secondary" href="https://hive.hivecommons.dev/dashboard">Back to the hub</a>
</div>
</html>`

func (s *Server) handleGHUserAuthLogout(w http.ResponseWriter, r *http.Request) {
	// Clear only THIS request's session so logging out affects one user, not
	// everyone. Removing the disk token only makes sense when the logging-out
	// user is the one whose token is persisted (the owner/last-authenticated
	// user); an anonymous POST must never wipe the owner's persisted token.
	var loggedOut, loggedOutRole string
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		clearSessionCookie(w)
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	sess := s.lookupSession(c.Value)
	if sess == nil {
		s.deleteSession(c.Value)
		clearSessionCookie(w)
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	loggedOut = sess.Username
	loggedOutRole = sess.Role
	s.deleteSession(c.Value)

	// Only clear the persisted GitHub token when the logging-out user is the
	// owner (read-write). Use the role bound to the session at login time — not
	// a fresh config lookup — so a later allowlist change can't leave a
	// logging-out owner's own token stranded on disk. Viewer logouts leave the
	// hive's user client intact.
	if loggedOutRole == config.RoleOwner {
		var removedToken string
		if tokenData, err := os.ReadFile(userTokenPath); err == nil {
			removedToken = strings.TrimSpace(string(tokenData))
		}
		if err := os.Remove(userTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.deps.Logger.Error("GitHub user token removal failed", "error", err)
			jsonError(w, "failed to remove persisted GitHub credentials", http.StatusInternalServerError)
			return
		}
		github.InvalidateTokenIdentity(removedToken)
	}
	clearSessionCookie(w)
	s.auditFromRequest(r, "gh_auth_logout", "", "")
	s.deps.Logger.Info("GitHub user logged out", "username", loggedOut)
	jsonResponse(w, map[string]interface{}{"status": "logged_out"})
}

func (s *Server) handleGHUserAuthSession(w http.ResponseWriter, r *http.Request) {
	// This endpoint only lands the user on the dashboard after the device flow
	// completed. The per-user session cookie was already set by the poll
	// handler; we never mint a session here (doing so from a shared secret or a
	// disk token file would grant any caller a valid session).
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleGHRateLimits(w http.ResponseWriter, r *http.Request) {
	if s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	limits, err := s.deps.GHClient.RateLimits(s.deps.Ctx)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, limits)
}

// maskSecret replaces the interior of a secret string with bullet characters,
// revealing only the last 4 characters (matching old hive behavior).
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	const visibleSuffix = 4
	if len(s) <= visibleSuffix {
		return strings.Repeat("•", len(s))
	}
	masked := strings.Repeat("•", len(s)-visibleSuffix)
	return masked + s[len(s)-visibleSuffix:]
}

func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	// On a direct-route spoke the shared token must never be handed to a
	// browser: identity there is per-user (device-flow sessions), and the
	// shared token no longer grants API access on that path (see authenticate).
	// Exposing it publicly here would leak the internal server-to-server secret
	// (used as X-Hive-Internal by the local proxy) to any visitor.
	// The same applies on a hub-proxied spoke: nginx injects per-user identity,
	// so the token is server-to-server only there too — and reporting
	// configured=true made the SPA prompt hosted operators for a token they
	// were never given (and don't need; their hub login already authorizes).
	if s.directRouteAuthzEnabled() || s.hubProxied() {
		jsonError(w, "not available on this hive", http.StatusNotFound)
		return
	}
	// SECURITY: never return the token VALUE. On a self-hosted (non-direct-route)
	// hive the dashboard token IS the API credential, so handing it to any
	// same-origin caller made "authentication" meaningless (an unauthenticated
	// visitor could GET it and then mutate). This endpoint now only reports
	// WHETHER a token is configured; a browser that needs to authenticate an
	// operator obtains the token by having the operator paste it (see the SPA's
	// token prompt), never by reading it back from the server.
	token := s.authToken
	if token == "" {
		token = os.Getenv("HIVE_DASHBOARD_TOKEN")
	}
	okResponse(w, map[string]string{"configured": strconv.FormatBool(token != "")})
}

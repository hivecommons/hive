// Authentication and session plumbing for the SaaS surface:
// require-auth wrappers, spoke upgrade proof verification, CSRF and
// origin checks, session cookie domains, identity resolution, GitHub
// token validation, spoke proxy auth, and the whoami/auth-check
// endpoints.
package hub

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

func (s *HubServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		username := s.getAuthUser(r)
		if username == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
			return
		}
		user := loadSaaSUser(username)
		if user == nil {
			ensureSaaSUser(username)
			user = loadSaaSUser(username)
		}
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown user — please log in again"}`))
			return
		}
		if user.Blocked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"account blocked"}`))
			return
		}
		next(w, r)
	}
}

// requireAuthOrSpokeUpgrade accepts the normal hub session for hub-dashboard
// clicks and, for a hosted spoke dashboard, the spoke's server-to-server proof
// plus the already-authenticated operator identity injected by that spoke.
func (s *HubServer) requireAuthOrSpokeUpgrade(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		username := s.getAuthUser(r)
		if username == "" {
			spokeUser, reason := s.trustedSpokeUpgradeUser(r, r.PathValue("id"))
			if spokeUser != "" {
				next(w, r)
				return
			}
			// Honest-error standard (#4446): every rejection on the spoke lane
			// names WHICH credential failed and what to do about it, because the
			// spoke dashboard relays this body verbatim into the operator's
			// toast — a bare "not authenticated" told a logged-in owner nothing.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
			return
		}
		user := loadSaaSUser(username)
		if user == nil {
			ensureSaaSUser(username)
			user = loadSaaSUser(username)
		}
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown user — please log in again"}`))
			return
		}
		if user.Blocked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"account blocked"}`))
			return
		}
		next(w, r)
	}
}

// trustedSpokeUpgradeUser authenticates the spoke-relayed upgrade lane. It
// returns the user to attribute the upgrade to and an empty reason on success,
// or ("", reason) on failure, where reason is an operator-facing explanation of
// exactly which credential failed (the spoke shows it verbatim in a toast).
//
// The proof (X-Hive-Proxy-Auth = the hive's own dashboard token) is the
// load-bearing credential: possession of that per-hive secret already grants
// owner on the spoke dashboard itself, so a proof-verified request is the
// spoke's authenticated operator by construction. X-Hive-User is attribution,
// not authentication — gateway-fronted spokes (the Node auth proxy on :3001)
// authenticate the operator with the shared token, strip per-user identity
// headers, and therefore relay NO X-Hive-User even for a legitimate logged-in
// owner. Rejecting that shape was the "Upgrade failed: not authenticated" bug:
// a proof-verified request with no user identity is now attributed to the
// hive's registered owner instead of being turned away.
func (s *HubServer) trustedSpokeUpgradeUser(r *http.Request, hiveID string) (string, string) {
	username := r.Header.Get("X-Hive-User")
	proof := r.Header.Get(proxyAuthHeader)
	if hiveID == "" {
		return "", "not authenticated — upgrade request named no hive"
	}
	if username == "" && proof == "" {
		// Nothing to verify at all. Old spoke builds (pre proof-forwarding)
		// relay the upgrade click with no credentials whatsoever; tell the
		// operator how to upgrade past that build instead of a dead end.
		return "", "not authenticated — this upgrade request reached the hub with no hub session and no spoke credentials; if it came from a spoke dashboard, that spoke build is too old to relay its upgrade credentials — trigger this hive's upgrade from the hub dashboard (or enable auto-upgrade), after which the spoke button will work"
	}
	if r.Header.Get("X-Hive-Role") != saasRoleOwner {
		return "", "not authenticated — spoke upgrade requests must carry the owner role"
	}
	if proof == "" {
		return "", "not authenticated — spoke upgrade proof missing: the spoke sent no dashboard-token proof (X-Hive-Proxy-Auth); set DASHBOARD_AUTH_TOKEN on the spoke to its hive-secrets/dashboard-token value"
	}
	switch s.verifySpokeUpgradeProof(hiveID, proof) {
	case spokeProofOK:
		// verified — fall through to attribution below
	case spokeProofUnverifiable:
		return "", "not authenticated — the hub has no stored dashboard-token record for this hive and could not read its hive-secrets/dashboard-token secret (the hive's cluster is unreachable from the hub, e.g. pull-only); a spoke on a current build reports its token over the authenticated heartbeat — trigger this hive's upgrade from the hub dashboard once, and the spoke's Upgrade button will verify against the stored record from then on"
	default: // spokeProofMismatch
		return "", "not authenticated — spoke upgrade proof rejected: the spoke's DASHBOARD_AUTH_TOKEN does not match this hive's dashboard-token secret; re-sync the spoke's token"
	}
	if username == "" {
		// Proof verified but no per-user identity (shared-token gateway
		// topology): attribute the upgrade to the hive's registered owner.
		if h := loadSaaSHive(hiveID); h != nil {
			username = h.Owner
		}
		if username == "" {
			return "", "not authenticated — spoke upgrade request carried no user identity and this hive has no registered owner to attribute it to"
		}
	}
	user := loadSaaSUser(username)
	if user == nil {
		return "", fmt.Sprintf("not authenticated — the hub has no record of user %q; log in to the hub once, then retry", username)
	}
	if user.Blocked {
		return "", "not authenticated — this account is blocked on the hub"
	}
	return username, ""
}

// spokeProofVerdict is the outcome of verifying a spoke's dashboard-token
// upgrade proof. The three-way split exists for the honest-error chain:
// "your token is wrong" and "the hub cannot check any token" demand different
// operator actions and must never share one message.
type spokeProofVerdict int

const (
	spokeProofOK spokeProofVerdict = iota
	// spokeProofMismatch: at least one reference credential was available and
	// the presented proof matched none of them.
	spokeProofMismatch
	// spokeProofUnverifiable: the hub has NO reference to check against — no
	// stored DashboardTokenHash record and no readable secret (pull-only or
	// otherwise unreachable cluster).
	spokeProofUnverifiable
)

// verifySpokeUpgradeProof checks a spoke-relayed upgrade proof against, in
// order:
//
//  1. The hub's OWN stored record (SaaSHive.DashboardTokenHash — written at
//     provisioning when the hub mints the token, refreshed from the spoke's
//     authenticated heartbeat). This needs no cluster access at all, which is
//     the point: hosted spokes on pull-only clusters (e.g. fmaas) are reached
//     only by their outbound heartbeat, and requiring a live kubectl secret
//     read there made every proof unverifiable by design.
//  2. A live read of the hive's hive-secrets/dashboard-token secret
//     (spokeProxyAuthToken, cached) — the pre-existing lane, kept as fallback
//     for hives that predate the stored record on clusters the hub CAN reach.
//     A successful live read that matches also backfills the stored record, so
//     the next verification (and a later loss of cluster access) no longer
//     depends on the cluster. A rotation the stored record missed is adopted
//     the same way: stale hash, live read matches, record refreshed.
func (s *HubServer) verifySpokeUpgradeProof(hiveID, proof string) spokeProofVerdict {
	verifiable := false
	if h := loadSaaSHive(hiveID); h != nil && h.DashboardTokenHash != "" {
		verifiable = true
		if secureCompareHub(HashDashboardToken(proof), h.DashboardTokenHash) {
			return spokeProofOK
		}
	}
	if expected := s.spokeProxyAuthToken(hiveID); expected != "" {
		verifiable = true
		if secureCompareHub(proof, expected) {
			if h := loadSaaSHive(hiveID); h != nil {
				if hash := HashDashboardToken(expected); h.DashboardTokenHash != hash {
					h.DashboardTokenHash = hash
					_ = saveSaaSHive(h)
				}
			}
			return spokeProofOK
		}
	}
	if !verifiable {
		return spokeProofUnverifiable
	}
	return spokeProofMismatch
}

// adoptSpokeDashboardTokenHash folds a heartbeat-reported dashboard-token hash
// into the hive's stored record (see HeartbeatPayload.DashboardTokenHash for
// why the spoke reports it, and verifySpokeUpgradeProof for what reads it).
// Callers must have authenticated the beat's per-hive bearer first. An empty
// or malformed value changes nothing: old spokes and token-less spokes report
// nothing, and the hub must keep whatever record it already has.
func (s *HubServer) adoptSpokeDashboardTokenHash(payload *HeartbeatPayload) {
	reported := payload.DashboardTokenHash
	if reported == "" || !isHexSHA256(reported) {
		return
	}
	if !strings.HasPrefix(payload.HiveID, "hosted-") && !strings.HasPrefix(payload.HiveID, "saas-") {
		return
	}
	h := loadSaaSHive(payload.HiveID)
	if h == nil || h.DashboardTokenHash == reported {
		return
	}
	h.DashboardTokenHash = reported
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to store heartbeat-reported dashboard token hash", "hive_id", payload.HiveID, "error", err)
	}
}

// isHexSHA256 reports whether s is a well-formed lowercase-or-uppercase hex
// SHA-256 digest — 64 hex characters, the only shape adoptSpokeDashboardTokenHash
// will persist.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isCSRFSafe reports whether a request may be allowed to MUTATE state.
//
// AUDIT F4 (replay half). This used to end with
//
//	return strings.Contains(ct, "application/json")
//
// which meant a mutation carrying NEITHER Origin NOR Referer was treated as
// safe as long as it said `Content-Type: application/json`. That fails OPEN, and
// the Content-Type is not a defence: it is fully attacker-controlled, and — the
// part that actually matters — Origin is absent in exactly the cases CSRF cares
// about. A browser omits Origin on plenty of navigations and redirect-issued
// requests, and Referer is routinely stripped by
// `Referrer-Policy: no-referrer`, privacy extensions, and corporate proxies. So
// "no headers" was never a reliable signal of "not a browser"; it was a signal
// of "we cannot tell", and the old code resolved that ambiguity in the
// attacker's favour on every requireAuth and requireAdmin write.
//
// It now fails CLOSED: a mutation must positively demonstrate it is either
// same-origin or not-a-browser. There are exactly two ways to do that.
//
//  1. Origin (preferred) or Referer matches the hub. Browsers attach Origin to
//     every cross-origin mutation and cannot forge it from script, which is what
//     makes it load-bearing.
//
//  2. The request authenticates with `Authorization: Bearer …` and sends NO
//     session cookie. This is the explicit non-browser lane, and it is safe for
//     a structural reason rather than a stylistic one: CSRF is an AMBIENT
//     credential attack. A cross-site request rides the cookie the browser
//     attaches automatically; it cannot attach an Authorization header, because
//     setting one from script requires CORS permission the hub never grants. A
//     request whose ONLY credential is a bearer token therefore cannot be
//     cross-site forged — the attacker would need the token itself, at which
//     point CSRF is irrelevant.
//
//     The "no session cookie" half is not optional. If a request carrying the
//     ambient cookie could opt out of the CSRF check merely by ALSO presenting a
//     header, then an attacker who can get any header set (or a stale token
//     lying in a client) re-opens the hole. Cookie present ⇒ ambient credential
//     ⇒ Origin must be proven.
//
// See getRealAuthUser: Bearer is already a first-class authentication path here,
// so this is not a new trust surface, only an explicit statement of which lane a
// caller is using.
func isCSRFSafe(r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
		return true
	}
	// Origin first: it is present on cross-origin browser mutations and cannot
	// be spoofed by page script.
	if origin := r.Header.Get("Origin"); origin != "" {
		return isSameOriginAsHub(origin)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return isSameOriginAsHub(referer)
	}
	// No origin information. The ONLY remaining way to be safe is to prove this
	// is not an ambient-credential browser request.
	return isNonBrowserAPIRequest(r)
}

// isNonBrowserAPIRequest reports whether a request authenticates purely with a
// bearer token and carries no ambient session cookie — the one shape that is
// structurally immune to CSRF. See isCSRFSafe for why both halves are required.
func isNonBrowserAPIRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	// Any hub session cookie means an ambient credential is in play, and the
	// request must prove its origin like any other browser mutation. Checked by
	// presence, not by validity: an INVALID cookie still means a browser sent
	// one, and letting a bogus cookie value re-enable the bearer bypass would be
	// a trivially attacker-satisfiable condition.
	if c, err := r.Cookie("hive_hub_user"); err == nil && c.Value != "" {
		return false
	}
	return true
}

const (
	defaultHubPublicURL          = "https://hive.kubestellar.io"
	defaultHubCanonicalHost      = "hive.kubestellar.io"
	defaultHubSpokeDomain        = "hive.kubestellar.io"
	defaultLegacyHubCookieDomain = ".hive.kubestellar.io"
)

// hubPublicURL is the canonical public origin used to build absolute URLs.
func hubPublicURL() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultHubPublicURL
}

// oauthRedirectURI is the single OAuth/OIDC callback registered on every
// provider's side. All providers share one callback path; the state parameter
// carries which provider to complete against.
func oauthRedirectURI() string {
	return hubPublicURL() + "/api/auth/callback"
}

// hubCanonicalHost is the ONE host that serves the hub's own dashboard and API.
// Every legitimate browser mutation against the hub is issued by a document
// loaded from this host — tenant spokes are separate origins that talk to the
// hub through their own proxy, never by scripting a cross-origin POST at it.
func hubCanonicalHost() string {
	u, err := url.Parse(hubPublicURL())
	if err != nil || u.Hostname() == "" {
		return defaultHubCanonicalHost
	}
	return strings.ToLower(u.Hostname())
}

// hubSpokeDomain is the shared parent domain the hosted tenants live under
// (<id>.hive.kubestellar.io by default). It is a REDIRECT-trust boundary only
// — see isTrustedRedirectTarget — and deliberately NOT a CSRF or CORS boundary.
func hubSpokeDomain() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_SPOKE_DOMAIN")); v != "" {
		return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(v), "."), ".")
	}
	return defaultHubSpokeDomain
}

func hubDomainSuffix() string {
	return "." + hubSpokeDomain()
}

func legacyHubCookieDomain() string {
	v := strings.TrimSpace(os.Getenv("HIVE_HUB_LEGACY_COOKIE_DOMAIN"))
	if v == "" {
		return ""
	}
	return "." + strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(v), "."), ".")
}

func legacySessionCookieDomains(liveDomain string) []string {
	var domains []string
	if liveDomain != "" && liveDomain != defaultLegacyHubCookieDomain {
		domains = append(domains, defaultLegacyHubCookieDomain)
	}
	if legacy := legacyHubCookieDomain(); legacy != "" && legacy != liveDomain && legacy != defaultLegacyHubCookieDomain {
		domains = append(domains, legacy)
	}
	return domains
}

// sessionCookieParentDomain is the registrable parent domain the hub session
// cookie is scoped to, so first-party sibling products on other kubestellar.io
// subdomains (dibs.kubestellar.io) receive it and can SSO against the hub via
// GET /api/saas/whoami. Derived from hubCanonicalHost (its parent domain)
// rather than spelled out so the two can never disagree.
func sessionCookieParentDomain() string {
	if parent, err := publicsuffix.EffectiveTLDPlusOne(hubCanonicalHost()); err == nil {
		return parent
	}
	if _, parent, ok := strings.Cut(hubCanonicalHost(), "."); ok {
		return parent
	}
	return hubCanonicalHost()
}

// sessionCookieDomain returns the Domain attribute the hub session cookie
// (hive_hub_user) must carry for a request served on host, or "" for a
// host-only cookie.
//
// Production (any host under kubestellar.io, including the hub itself) gets
// Domain=.kubestellar.io so that BOTH consumers of the cookie receive it:
//   - every hosted spoke's Node proxy on <id>.hive.kubestellar.io, which
//     independently verifies it for the tenant dashboard and terminal (the
//     original reason the cookie carried Domain=.hive.kubestellar.io); and
//   - sibling first-party products such as dibs.kubestellar.io, which read it
//     and call back to /api/saas/whoami (#4171).
//
// Local/dev hosts (localhost, 127.0.0.1, anything not under kubestellar.io)
// get a host-only cookie: a browser rejects a Set-Cookie whose Domain does not
// cover the request host, so widening there would break local login outright.
func sessionCookieDomain(host string) string {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	parent := sessionCookieParentDomain()
	if h == parent || strings.HasSuffix(h, "."+parent) {
		return "." + parent
	}
	return ""
}

// hubSessionCookieValues returns every hive_hub_user value on the request, in
// jar order. During the .kubestellar.io domain-widening rollout (#4171) a
// browser may briefly hold TWO copies of the cookie — the legacy
// .hive.kubestellar.io-scoped one and the new parent-scoped one — and it sends
// both under the same name. Callers must try each candidate rather than
// trusting whichever copy the jar happens to order first, or a stale legacy
// cookie would shadow a fresh session (and vice versa) until re-login.
func hubSessionCookieValues(r *http.Request) []string {
	var vals []string
	for _, c := range r.Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			vals = append(vals, c.Value)
		}
	}
	return vals
}

// isSameOriginAsHub reports whether raw names the hub's own origin, i.e. the
// only origin permitted to drive a state-changing request or to receive a
// credentialed CORS response.
//
// SECURITY (audit F4): this used to be isTrustedOrigin, which accepted EVERY
// suffix match of .hive.kubestellar.io. Because the hub session cookie is
// scoped Domain=.hive.kubestellar.io, the browser attaches it to requests
// issued from any sibling tenant — so a hostile hive operator, serving script
// from their own <id>.hive.kubestellar.io dashboard, could POST at the hub with
// the victim admin's ambient cookie and have the CSRF gate wave it through. The
// audit demonstrated exactly this by flipping another tenant's visibility from
// a sibling Origin. Suffix-matching a domain whose subdomains are handed out to
// untrusted third parties is not an origin check at all.
//
// localhost/127.0.0.1 stay trusted for local development, where the hub is
// served from those hosts and there is no multi-tenant sibling to speak of.
func isSameOriginAsHub(raw string) bool {
	host, ok := originHost(raw)
	if !ok {
		return false
	}
	return host == hubCanonicalHost() || host == "localhost" || host == "127.0.0.1"
}

// isTrustedRedirectTarget reports whether raw is a URL the hub may bounce a
// browser BACK to after login.
//
// This one MUST keep accepting sibling tenants, and that is not an oversight:
// every hosted hive's ingress carries
//
//	auth-signin: https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri
//
// (see saas_provision.go), so the ordinary "open my hive" flow arrives at the
// hub with redirect=https://<id>.hive.kubestellar.io/... and must be allowed to
// return there. Narrowing this to the exact hub origin would break sign-in for
// all hosted tenants.
//
// Sending a browser to a sibling is a far weaker capability than accepting a
// mutation FROM one: the tenant already controls that host and can navigate the
// user there unaided. The dangerous half — trusting a sibling to author a
// request — is what isSameOriginAsHub now refuses.
func isTrustedRedirectTarget(raw string) bool {
	host, ok := originHost(raw)
	if !ok {
		return false
	}
	return host == hubCanonicalHost() ||
		strings.HasSuffix(host, hubDomainSuffix()) ||
		host == "localhost" ||
		host == "127.0.0.1"
}

// originHost extracts the hostname from raw, rejecting values that do not parse.
// Shared by both trust predicates so they can never disagree about how a URL is
// decomposed.
func originHost(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return u.Hostname(), true
}

// getRealAuthUser resolves the REAL authenticated user from the signed session
// cookie or a Bearer token, with NO impersonation applied. This is the identity
// the admin actually logged in as. Impersonation is layered on top of this by
// resolveIdentity — never inside it — so the write-block and the admin gate can
// always reason about who is really driving the request.
func (s *HubServer) getRealAuthUser(r *http.Request) string {
	// Every hive_hub_user copy is tried, not just the first (see
	// hubSessionCookieValues — the domain-widening rollout can leave two).
	for _, value := range hubSessionCookieValues(r) {
		// The cookie value is only trusted when its HMAC signature verifies
		// against the hub secret. A legacy unsigned cookie or a forged value
		// fails here and is treated as logged out, so the user re-authenticates
		// through the normal login flow (which re-mints a signed cookie).
		// N2: accept v2 (Ed25519) or, during rollout only, the legacy HMAC format.
		// F10: verifyHubUserCookie additionally enforces a v3 cookie's SIGNED
		// expiry and its revocation state, which MaxAge alone never did.
		if username, ok := s.verifyHubUserCookie(value); ok {
			if loadSaaSUser(username) != nil {
				return username
			}
		}
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if username := s.validateGitHubToken(token); username != "" {
			return username
		}
	}

	return ""
}

// resolveIdentity is the single decision point for admin "View as user"
// impersonation. It returns the EFFECTIVE identity every per-user view should
// render as, the REAL admin login driving the request, and whether an
// impersonation grant is currently active.
//
// Impersonation is honored ONLY when every one of these holds (fail closed on
// any miss — the request is then simply the real user, not impersonating):
//
//   - a hive_hub_impersonate cookie is present AND its HMAC verifies AND it has
//     not expired (verifyImpersonateCookieValue);
//   - the grant's Admin field equals the REAL signed session user;
//   - that real user is a hub admin (isHubAdmin — only an admin may impersonate
//     — a stolen cookie replayed on a non-admin session is ignored);
//   - the target resolves to a real registered user on disk.
//
// The effective identity switches to the target ONLY for GET/HEAD requests.
// For any mutating method the effective identity stays the admin so no write is
// ever attributed to the target; the write itself is separately refused 403 by
// requireAuth/requireAdmin. This split means impersonation can never elevate:
// the target is always a normal user, and writes never run under it.
func (s *HubServer) resolveIdentity(r *http.Request) (effective, realUser string, impersonating bool) {
	realUser = s.getRealAuthUser(r)
	if realUser == "" || !isHubAdmin(realUser) {
		return realUser, realUser, false
	}
	cookie, err := r.Cookie(impersonateCookieName)
	if err != nil || cookie.Value == "" {
		return realUser, realUser, false
	}
	grant, _, ok := verifyImpersonateCookieValueWithGenerations(s.currentGenerations(), cookie.Value, time.Now())
	if !ok {
		return realUser, realUser, false
	}
	// The cookie must name THIS real admin as its actor. Anything else — a
	// cookie minted for a different admin, or one lifted onto the wrong
	// session — is ignored rather than trusted.
	if grant.Admin != realUser {
		return realUser, realUser, false
	}
	if loadSaaSUser(grant.Target) == nil {
		return realUser, realUser, false
	}
	// A valid, active grant exists. Report impersonating=true regardless of
	// method (so writes can be blocked), but only SWITCH the effective identity
	// for read requests.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return grant.Target, realUser, true
	}
	return realUser, realUser, true
}

// getAuthUser returns the EFFECTIVE identity for the request: the impersonated
// target on a GET/HEAD while a valid admin grant is active, otherwise the real
// authenticated user. Every per-user handler calls this, so making it
// impersonation-aware is what renders all per-user views as the target with no
// per-handler change.
func (s *HubServer) getAuthUser(r *http.Request) string {
	effective, _, _ := s.resolveIdentity(r)
	return effective
}

var (
	ghTokenCacheMu sync.RWMutex
	ghTokenCache   = map[string]ghTokenCacheEntry{}
)

const ghTokenCacheTTL = 5 * time.Minute

type ghTokenCacheEntry struct {
	username  string
	expiresAt time.Time
}

func (s *HubServer) validateGitHubToken(token string) string {
	if token == "" {
		return ""
	}

	ghTokenCacheMu.RLock()
	if entry, ok := ghTokenCache[token]; ok && time.Now().Before(entry.expiresAt) {
		ghTokenCacheMu.RUnlock()
		return entry.username
	}
	ghTokenCacheMu.RUnlock()

	client := &http.Client{Timeout: 10 * time.Second}
	// Hub always validates tokens against github.com (the hub is a SaaS service).
	req, err := http.NewRequest("GET", defaultGHUserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		return ""
	}

	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: user.Login, expiresAt: time.Now().Add(ghTokenCacheTTL)}
	ghTokenCacheMu.Unlock()

	return user.Login
}

func (s *HubServer) handleUserToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HiveID   string `json:"hive_id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HiveID == "" || body.Username == "" {
		http.Error(w, `{"error":"hive_id and username required"}`, http.StatusBadRequest)
		return
	}

	requester := s.getAuthUser(r)
	if requester != body.Username && !isHubAdmin(requester) {
		http.Error(w, `{"error":"can only retrieve your own token"}`, http.StatusForbidden)
		return
	}

	user := loadSaaSUser(body.Username)
	if user == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}

	if _, ok := user.Hives[body.HiveID]; !ok {
		http.Error(w, `{"error":"user has no access to this hive"}`, http.StatusForbidden)
		return
	}

	if user.EncryptedToken == "" {
		http.Error(w, `{"error":"no token stored for this user"}`, http.StatusNotFound)
		return
	}

	token, err := decryptToken(user.EncryptedToken)
	if err != nil {
		s.logger.Warn("failed to decrypt user token", "user", body.Username, "error", err)
		http.Error(w, `{"error":"token decryption failed"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: user token issued", "user", body.Username, "hive", body.HiveID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

var publicPaths = []string{"/snapshot", "/leaderboard", "/contribute", "/api/leaderboard", "/api/contribute", ssoHandoffPath}

// ssoHandoffPath is the spoke's SSO handoff endpoint. It MUST bypass the hub's
// nginx auth_request gate.
//
// Why: the auth-check subrequest authorizes a user against THIS hive's grant
// list (user.Hives[hiveID]). The whole point of the handoff is to admit a user
// who is authenticated on the hub but has no hub-side grant row for the hive —
// the spoke's own authorized_users allowlist is the authority. Gating /sso on
// the hub check therefore 401s exactly the requests the handoff exists to
// serve, and nginx turns that 401 into an auth-signin redirect back to the hub
// login, which (the user already having a valid hub cookie) immediately
// redirects to /sso again — an infinite bounce the browser eventually aborts.
//
// This does NOT weaken authentication: the signed, hive-scoped, short-lived
// HMAC token in the query IS the credential, and dashboard.handleSSO verifies
// it against HIVE_HUB_SECRET plus the spoke's own allowlist before minting any
// session. It is the same reasoning that already makes /sso a public path on
// the spoke half (see dashboard.isPublicPath).
const ssoHandoffPath = "/sso"

// proxyAuthHeader is the response header the hub sets on a successful
// auth-check subrequest to PROVE to the spoke that the request was
// authenticated by the hub's nginx auth-proxy (not spoofed by a client that
// merely supplied X-Hive-User/X-Hive-Role directly). nginx is configured to
// copy this header onto the upstream request via the auth-response-headers
// annotation (see saas_provision.go). Its value is the spoke's own dashboard
// token, so the spoke can constant-time-compare it against its authToken.
//
// CONTRACT (spoke half, separate v3 PR must verify EXACTLY this):
//   - Header name: "X-Hive-Proxy-Auth"
//   - Value: the hive's dashboard token — the raw value stored in the spoke's
//     "hive-secrets" k8s secret under key "dashboard-token", i.e. the same
//     string the spoke reads from DASHBOARD_AUTH_TOKEN into its authToken.
//   - Set ONLY on the authenticated success path; NEVER on public-path,
//     unfurl-bot, unauthenticated (401), or no-access (403) responses.
const proxyAuthHeader = "X-Hive-Proxy-Auth"

// spokeProxyAuthCacheTTL bounds how long a hive's dashboard token is memoized
// so the per-request auth-check subrequest avoids a kubectl exec on every call
// while still picking up a re-provisioned token within a bounded window.
const spokeProxyAuthCacheTTL = 5 * time.Minute

// spokeProxyAuthEntry is a cached dashboard token with its expiry.
type spokeProxyAuthEntry struct {
	token   string
	expires time.Time
}

// spokeProxyAuthToken returns the given hive's dashboard token — the shared
// secret the spoke holds as its authToken (DASHBOARD_AUTH_TOKEN / the
// "dashboard-token" key of the "hive-secrets" k8s secret). It memoizes the
// value for spokeProxyAuthCacheTTL so the hot auth-check path does not exec
// kubectl on every proxied request. Returns "" if the token cannot be
// resolved (e.g. the hive isn't in the registry or its cluster is unreachable);
// callers must then omit the proof header rather than send an empty one.
func (s *HubServer) spokeProxyAuthToken(hiveID string) string {
	now := time.Now()

	s.spokeProxyAuthMu.Lock()
	if entry, ok := s.spokeProxyAuthCache[hiveID]; ok && now.Before(entry.expires) {
		tok := entry.token
		s.spokeProxyAuthMu.Unlock()
		return tok
	}
	s.spokeProxyAuthMu.Unlock()

	// Resolve the registry entry (ID + ClusterID) that loadSpokeAuthToken needs.
	var hive *RegistryEntry
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			h := s.registry.Hives[i]
			hive = &h
			break
		}
	}
	s.mu.RUnlock()
	if hive == nil {
		return ""
	}

	token := s.loadSpokeAuthToken(hive)
	if token == "" {
		return ""
	}

	s.spokeProxyAuthMu.Lock()
	s.spokeProxyAuthCache[hiveID] = spokeProxyAuthEntry{token: token, expires: now.Add(spokeProxyAuthCacheTTL)}
	s.spokeProxyAuthMu.Unlock()

	return token
}

// handleSaaSWhoami resolves the hub session for a sibling first-party product
// (#4171). Dibs (dibs.kubestellar.io) has no login of its own: it forwards the
// browser's hive_hub_user cookie here server-to-server and expects
//
//	200 {"username","display_name","email","avatar_url"}
//
// for a valid session, or 401 JSON otherwise.
//
// username is the STABLE identity key: the bare GitHub login for GitHub users
// (byte-identical to what every pre-multi-provider consumer keys on), or the
// hub's canonical "provider:sub" form for OIDC users — never a display name,
// never an email (emails are reassignable; subs are not). display_name/email/
// avatar_url come from the enriched SaaSUser record (DisplayName et al are
// refreshed on every completed login).
//
// Deliberately no CORS headers: the caller is a server, not a browser, and
// adding credentialed CORS here would hand the session identity to scripts.
// Cache-Control: no-store because the answer is per-session and revocable.
func (s *HubServer) handleSaaSWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	// Bare login for GitHub identities, canonical provider:sub for the rest —
	// exactly the key SaaSUser records are stored and looked up under.
	stableKey := username
	if provider, subject, ok := parseCanonical(canonicalizeLegacy(username)); ok && provider == legacyProvider {
		stableKey = subject
	}
	displayLogin, avatar := s.displayIdentity(username)
	displayName := user.DisplayName
	if displayName == "" {
		// GitHub users have no provider-asserted name claim stored; the display
		// login (their bare GitHub login) is the established fallback.
		displayName = displayLogin
	}
	data, err := json.Marshal(map[string]string{
		"username":     stableKey,
		"display_name": displayName,
		"email":        user.Email,
		"avatar_url":   avatar,
	})
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func (s *HubServer) handleSaaSAuthCheck(w http.ResponseWriter, r *http.Request) {
	hiveID := r.URL.Query().Get("hive")
	if hiveID == "" {
		http.Error(w, "missing hive param", http.StatusBadRequest)
		return
	}

	originalURI := r.Header.Get("X-Original-URI")
	if originalURI == "" {
		if origURL := r.Header.Get("X-Original-URL"); origURL != "" {
			if u, err := url.Parse(origURL); err == nil {
				originalURI = u.Path
			}
		}
	}
	if originalURI == "" {
		originalURI = r.URL.Query().Get("uri")
	}
	for _, p := range publicPaths {
		if strings.HasPrefix(originalURI, p) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if isUnfurlBot(r.Header.Get("User-Agent")) {
		w.WriteHeader(http.StatusOK)
		return
	}

	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, "no access", http.StatusForbidden)
		return
	}

	role, ok := user.Hives[hiveID]
	// The spoke enforces owner-gated actions (requireOwnerRole, e.g.
	// POST /api/self-upgrade) from X-Hive-Role alone, so a stale/demoted
	// stored role would lock the hive's TRUE owner out of their own spoke.
	// Elevate the canonical owner — scoped to their OWN hive — before
	// forwarding the role (#4081).
	if role != "owner" && s.userOwnsHive(username, hiveID) {
		role = "owner"
		ok = true
	}
	if !ok {
		http.Error(w, "no access to this hive", http.StatusForbidden)
		return
	}

	w.Header().Set("X-Hive-User", username)
	w.Header().Set("X-Hive-Role", role)
	// Prove to the spoke that this request really passed through the hub's
	// auth-proxy: set X-Hive-Proxy-Auth to the hive's own dashboard token so
	// the spoke can constant-time-compare it against its authToken. Only set on
	// this authenticated success path; a client hitting the spoke directly
	// cannot forge it because it never learns the token. If the token can't be
	// resolved, omit the header (backward-compatible: the spoke half must fail
	// open only until it is deployed — see the v3 spoke PR).
	if proxyAuth := s.spokeProxyAuthToken(hiveID); proxyAuth != "" {
		w.Header().Set(proxyAuthHeader, proxyAuth)
	}
	w.WriteHeader(http.StatusOK)
}

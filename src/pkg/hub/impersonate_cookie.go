package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"
)

// Admin read-only "View as user" impersonation.
//
// A separate, short-lived, signed cookie (`hive_hub_impersonate`) lets the hub
// admin — and ONLY the hub admin — see the hub dashboard exactly as another
// registered user sees it, without ever gaining that user's privileges. The
// impersonation is READ-ONLY: while it is active the server rejects every
// mutating request with 403 (see requireAuth / requireAdmin), so the worst an
// impersonating admin can do is look.
//
// Security properties this cookie enforces, in order of importance:
//
//  1. It is tamper-evident. The value is bound to the hub secret with an
//     HMAC-SHA256 using the SAME construction as the session cookie
//     (hub_cookie.go). A forged, edited, or unsigned value fails verification
//     and is treated as "not impersonating" — never as an escalation.
//  2. It carries the REAL admin login it was minted for. resolveIdentity only
//     honors the cookie when that admin field equals BOTH the real signed
//     session user AND hubAdminUsername. A user cannot lift the admin's
//     impersonation cookie onto their own session to borrow the admin's view,
//     and the admin cannot mint one that names someone else as the actor.
//  3. It expires. The payload carries an absolute Unix expiry (impersonateTTL
//     from mint time); a stale cookie is ignored. This bounds how long a
//     forgotten "View as" session can linger.
//
// Wire format (all in the clear except the trailing signature, exactly like the
// session cookie — none of these fields are secret; the signature is what
// proves the hub minted them):
//
//	value = <admin>|<target>|<expUnix>.<base64url(HMAC-SHA256(admin|target|expUnix, hubSecret))>
//
// The admin and target are GitHub logins, which loadSaaSUser already validates
// to exclude "/" "\\" and ".." — and usernames never contain "|" or "." — so
// the delimiters cannot be smuggled.

// impersonateTTL bounds how long a single "View as" session stays valid before
// the admin must re-enter it. Deliberately short: impersonation is an
// intentional, supervised action, not a durable mode, and a short window limits
// the blast radius of a forgotten session or a leaked cookie.
const impersonateTTL = 30 * time.Minute

// impersonateCookieName is the browser cookie that carries an active
// impersonation grant. It is intentionally distinct from the session cookie
// (hive_hub_user) so the two are minted, verified, and cleared independently:
// exiting impersonation never disturbs the real login.
const impersonateCookieName = "hive_hub_impersonate"

// impersonateSep separates the payload fields. It is a character that cannot
// appear in a GitHub username, so field boundaries are unambiguous.
const impersonateSep = "|"

// impersonationGrant is the decoded, verified payload of an impersonation
// cookie. It is only ever produced by verifyImpersonateCookieValue, which
// returns ok=false unless the signature verified.
type impersonationGrant struct {
	Admin  string // the real admin login the cookie was minted for
	Target string // the user being viewed
	Exp    int64  // absolute expiry, Unix seconds
}

// impersonatePayload is the signed portion of the cookie: the three fields
// joined by impersonateSep. Keeping the join in one place guarantees mint and
// verify hash byte-for-byte identical input.
func impersonatePayload(admin, target string, exp int64) string {
	return admin + impersonateSep + target + impersonateSep + strconv.FormatInt(exp, 10)
}

// impersonateSign returns the URL-safe base64 HMAC-SHA256 of payload under
// secret, reusing the session cookie's encoding (hubCookieB64) so both cookies
// share one crypto scheme.
func impersonateSign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hubCookieB64.EncodeToString(mac.Sum(nil))
}

// mintImpersonateCookieValueForGeneration mints an impersonation cookie under
// the CURRENT generation and stamps it with that generation's marker.
//
// This is the pilot adoption of the master-secret rotation mechanism
// (hub_generations.go), chosen because the impersonation cookie is the
// lowest-risk verifier on the platform: it is hub-only (no spoke, no Node
// proxy, no Deployment env), it lives 30 minutes, and it grants nothing — an
// impersonating admin is read-only, so a failure here degrades to "not
// impersonating" rather than to any loss of access.
//
// The cookie is one of the artifacts that CAN carry a marker: it has payload
// room and the hub controls both mint and verify. So the verifier can SELECT a
// generation instead of trial-verifying, and the audit trail records which key
// was used. Returns "" when there is no minting generation, preserving the
// fail-closed contract of the underlying mint.
func mintImpersonateCookieValueForGeneration(gs *generationSet, admin, target string, now time.Time) string {
	g, ok := gs.currentGeneration()
	if !ok {
		return ""
	}
	inner := mintImpersonateCookieValue(deriveDomainKey(g.Secret, infoImpersonateKey), admin, target, now)
	if inner == "" {
		return ""
	}
	return formatGenerationMarker(g.ID) + inner
}

// verifyImpersonateCookieValueWithGenerations verifies an impersonation cookie
// against every generation the set still accepts, and reports WHICH one
// accepted.
//
// Dual acceptance is the point: during a rotation a cookie minted under the
// outgoing generation must keep working until it expires on its own, or every
// rotation logs the admin out mid-session. The window is finite by
// construction — acceptableGenerations drops any previous generation past its
// VerifyUntil — so this cannot become the unbounded verify-both lane that F1/F2
// took five audits to remove.
//
// An UNMARKED cookie (every one minted before this change) is tried against
// each acceptable generation in turn, so deploying this is a non-event for
// cookies already in browsers.
//
// Returns the accepting generation ID alongside the grant. Callers that do not
// care may ignore it; it exists so "is anything still using the old key?" is a
// question the telemetry can answer without reading the code.
func verifyImpersonateCookieValueWithGenerations(gs *generationSet, value string, now time.Time) (impersonationGrant, int, bool) {
	if value == "" {
		return impersonationGrant{}, 0, false
	}
	return verifyWithGenerations(gs, value, now,
		func(secret, artifact string) (impersonationGrant, bool) {
			return verifyImpersonateCookieValue(deriveDomainKey(secret, infoImpersonateKey), artifact, now)
		})
}

// mintImpersonateCookieValue returns a signed impersonation cookie value that
// records admin viewing target, expiring impersonateTTL from now. It returns ""
// when no secret is configured or either login is empty — callers must treat
// that as "cannot start impersonation" rather than emitting an unsigned value.
func mintImpersonateCookieValue(secret, admin, target string, now time.Time) string {
	if secret == "" || admin == "" || target == "" {
		return ""
	}
	exp := now.Add(impersonateTTL).Unix()
	payload := impersonatePayload(admin, target, exp)
	return payload + "." + impersonateSign(secret, payload)
}

// verifyImpersonateCookieValue parses a cookie value, recomputes its HMAC over
// the carried payload, and constant-time-compares it against the presented
// signature. It returns (grant, true) ONLY when the signature verifies AND the
// grant has not expired (checked against now). A forged, edited, unsigned,
// malformed, or expired value returns (zero, false) so the caller treats the
// request as NOT impersonating — the cookie can only ever be ignored, never
// trusted-by-default.
func verifyImpersonateCookieValue(secret, value string, now time.Time) (impersonationGrant, bool) {
	var zero impersonationGrant
	if secret == "" || value == "" {
		return zero, false
	}
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return zero, false
	}
	payload, sig := value[:idx], value[idx+1:]
	expected := impersonateSign(secret, payload)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return zero, false
	}
	parts := strings.Split(payload, impersonateSep)
	if len(parts) != 3 {
		return zero, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return zero, false
	}
	if now.Unix() >= exp {
		return zero, false
	}
	if parts[0] == "" || parts[1] == "" {
		return zero, false
	}
	return impersonationGrant{Admin: parts[0], Target: parts[1], Exp: exp}, true
}

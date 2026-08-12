package dashboard

import (
	"net/http"
)

// Terminal-assertion renewal (audit F4 residual — spoke-scoped session, step 1).
//
// # THE PROBLEM THIS EXISTS TO SHRINK
//
// The hub session cookie `hive_hub_user` is scoped Domain=.hive.kubestellar.io,
// so the browser delivers a valid hub session credential to EVERY sibling
// tenant's server on every request. #3499 stopped a sibling from USING it (it is
// no longer a same-origin author for CSRF/CORS purposes), but a hostile tenant
// backend still RECEIVES it in plaintext at the HTTP layer. The only way to stop
// that is to drop the Domain attribute — and the only thing still forcing the
// Domain to exist is the spoke-side Node proxy, which verifies `hive_hub_user`
// to gate /terminal.
//
// So the residual reduces to one question: can the spoke authorize /terminal
// using ONLY credentials scoped to the spoke's own host?
//
// # WHAT ALREADY EXISTS
//
// Almost all of it. The proxy's PRIMARY gate (authorizeTerminal) is already the
// spoke-local `hive_terminal_assertion` cookie: host-only, Path=/terminal,
// HMAC-signed with a spoke-local key, carrying {user, role, hive_id, exp}. It is
// a genuinely spoke-scoped credential and it is already minted at both entry
// points — device-flow login and hub SSO handoff (setTerminalAssertionCookie).
//
// # WHY THE HUB COOKIE IS STILL LOAD-BEARING — THE ACTUAL BLOCKER
//
// A lifetime mismatch, not a missing mechanism:
//
//   - hive_session (spoke-local, host-only) lives 30 days.
//   - hive_terminal_assertion lives 15 minutes, and was minted ONLY at login.
//
// So 15 minutes after logging in, a legitimate user's assertion expires. From
// then on authorizeTerminal finds no usable assertion and falls through to the
// static-allowlist branch, which is keyed on the verified `hive_hub_user`
// username. That fallback — reached for the overwhelming majority of real
// /terminal requests, since sessions outlive assertions 2880:1 — is precisely
// what makes the domain-scoped hub cookie load-bearing on the spoke.
//
// Nothing renewed the assertion, because there was no way to: renewal needs an
// authenticated caller, and the only always-present authenticated identity the
// proxy could see was the hub-wide cookie.
//
// # WHAT THIS HANDLER DOES
//
// It closes that loop using the spoke's OWN session. handleRenewTerminalAssertion
// re-mints the assertion from the caller's `hive_session` — a host-only,
// spoke-local cookie that the hub never sees and no sibling ever receives — so a
// live spoke session can keep a live terminal grant without the hub cookie
// participating at all.
//
// This does NOT by itself remove the Domain from hive_hub_user, and deliberately
// so: see the PR body. It is the verifier-side capability that a later,
// separately-sequenced PR needs in order to make that removal safe. Shipping the
// capability first, and flipping the minting later, is the verify-both/mint-new
// ordering that incident #2773 exists to enforce.
//
// SECURITY PROPERTIES (all fail-closed):
//
//   - Authentication is the spoke-local session ONLY. This handler never reads
//     hive_hub_user, so it cannot be driven by a credential a sibling tenant
//     received. No session → 401, and no cookie is written.
//   - Role is re-resolved from THIS hive's authorized-users allowlist on every
//     renewal, not copied from the old assertion or the session. A user whose
//     access was revoked or downgraded hub-side cannot renew their way past it;
//     the renewed assertion carries the CURRENT role, and the proxy's role check
//     (TERMINAL_ROLES) then denies a downgraded user. This is what keeps
//     revocation effective — a self-renewing grant that froze its own role at
//     login would be strictly worse than the 15-minute expiry it replaces.
//   - It mints only the same {user, role, hive, exp} assertion the login path
//     already mints, with the same key, TTL, Path and flags. It introduces no
//     new token format, no new key, and nothing that crosses the hub/spoke
//     boundary — so there is no cross-language crypto to pin and no hub-side
//     change required for it to be safe.
//   - It cannot escalate: role comes from the allowlist, and a user absent from
//     an enforced allowlist gets no assertion at all.
const renewTerminalAssertionPath = "/api/terminal/assertion/renew"

// handleRenewTerminalAssertion re-mints the caller's per-hive terminal assertion
// cookie from their spoke-local session. See the file comment for why this
// exists and what it deliberately does not do.
func (s *Server) handleRenewTerminalAssertion(w http.ResponseWriter, r *http.Request) {
	// Authenticate from the SPOKE-LOCAL session only. Deliberately not
	// sessionFromRequest+hub cookie fallback: the entire point is that this path
	// works without the hub-wide cookie, so reading it here would defeat the
	// exercise and silently re-create the dependency.
	sess := s.sessionFromRequest(r)
	if sess == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Re-resolve the role from this hive's allowlist so a hub-side revocation or
	// downgrade takes effect at renewal rather than being frozen at login.
	role := sess.Role
	if s.deps != nil && s.deps.Config != nil {
		if allowRole, ok := s.deps.Config.Dashboard.AuthorizedRole(sess.Username); ok {
			role = allowRole
		} else if s.deps.Config.Dashboard.IsDirectRouteAuthzEnabled() {
			// Allowlist is enforced and this user is no longer on it. Deny, and
			// write no cookie — the existing assertion then simply expires.
			http.Error(w, `{"error":"not authorized for this hive"}`, http.StatusForbidden)
			return
		}
	}

	// Mint with the same helper the login and SSO paths use, so the assertion's
	// shape, key, TTL, Path and cookie flags cannot drift between mint sites.
	s.setTerminalAssertionCookie(w, r, sess.Username, role)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNoContent)
}

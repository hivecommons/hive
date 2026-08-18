package hub

import (
	"time"
)

// Dual-generation acceptance for the HUB SESSION COOKIE — follow-on PR #1 of
// the master-key rotation design (src/docs/design/master-key-rotation.md).
//
// WHY THE SESSION COOKIE IS THE ONE THAT MATTERS. It is the longest-lived
// artifact bound to a generation (cookieMaxAgeDays == 7, which is exactly why
// defaultVerifyWindow is 7 days). Without dual acceptance, the instant an
// operator rotates the master every browser session on the platform is
// invalidated at once — the single most visible symptom of the flag day this
// whole mechanism exists to remove.
//
// WHERE THE MARKER LIVES, AND WHY IT DIFFERS PER FORMAT. The session cookie
// exists in two live shapes and they do NOT have the same amount of room:
//
//	v3  base64url(JSON{u,iat,exp,sid[,g]}) .v3. base64url(sig)
//	v2  <username> .v2. base64url(sig)
//
//   - v3 CAN carry a marker, as the `g` claim (hub_cookie.go hubCookieClaims).
//     It rides INSIDE the signature, so it is tamper-evident, and the Node proxy
//     ignores JSON fields it does not recognize, so adding it needs no spoke
//     roll. This is the design doc's "field in payload" row.
//
//   - v2 CANNOT. Its username sits in the clear at the head of the value with no
//     envelope around it, and prefixing the VALUE with "g<N>." — the impersonation
//     cookie's scheme — is not available to either shape here, because the same
//     value is parsed independently by src/proxy/server.js. A prefix would sit in
//     front of the username (v2) or in front of the base64url body (v3) and break
//     the proxy's parse for every hosted tenant. Making the rotation mechanism
//     require a fleet-wide proxy roll would defeat its entire purpose.
//
// So v2 gets bounded TRIAL verification instead — the same treatment the
// heartbeat bearer gets, and bounded by the same maxLiveGenerations == 2. Two
// Ed25519 verifications worst case, on a path that already does one.
//
// MINTING IS ALWAYS CURRENT-ONLY. Dual acceptance is a property of the VERIFIER.
// If minting also used a previous generation the rotation would never converge,
// which is the failure mode that makes a compat lane permanent.

// mintHubUserCookieValueV3ForGeneration mints a v3 session cookie under the
// CURRENT generation and stamps that generation into the signed claims.
//
// Returns ("", "") when the set has no minting generation, preserving the
// fail-closed contract of the underlying mint: no key configured must mean
// "cannot establish a session", never "emit an unsigned cookie".
func mintHubUserCookieValueV3ForGeneration(gs *generationSet, username string, now time.Time, ttl time.Duration) (value, sid string) {
	g, ok := gs.currentGeneration()
	if !ok {
		return "", ""
	}
	return mintHubUserCookieValueV3Gen(
		deriveDomainKey(g.Secret, infoSessionEd25519Seed), username, now, ttl, g.ID)
}

// sessionSeedForGeneration returns the Ed25519 signing SEED for one generation.
// Private key material — hub-only, never injected into a spoke.
func sessionSeedForGeneration(g keyGeneration) string {
	return deriveDomainKey(g.Secret, infoSessionEd25519Seed)
}

// sessionPublicKeyForGeneration returns the hex Ed25519 PUBLIC key a verifier
// checks one generation's session cookies against.
func sessionPublicKeyForGeneration(g keyGeneration) string {
	return ssoPublicKeyFromSeed(sessionSeedForGeneration(g))
}

// verifyHubUserCookieAcrossGenerations verifies a session cookie against every
// generation the set still accepts, and reports WHICH one accepted.
//
// Resolution, in order:
//
//  1. If the cookie is v3 AND carries a `g` claim naming a generation the set
//     still accepts, verify against THAT generation only. One Ed25519 check, and
//     the accepting generation is recorded rather than inferred.
//  2. Otherwise — unmarked (every cookie in a browser today), v2, or a `g`
//     naming a generation we no longer accept — try each acceptable generation
//     in current-first order.
//
// Case 2 is what makes deploying this a non-event: every already-issued cookie
// lacks a marker, and an unmarked cookie must VERIFY against the current
// generation, not be rejected for want of a field that did not exist when it was
// minted.
//
// A marker naming a generation we do not accept must never make verification
// FAIL where trial verification would have succeeded. The `g` claim is inside
// the signature so it cannot be forged on a valid cookie, but it CAN name an
// expired generation on a cookie that is otherwise fine — in which case falling
// through is correct, and the expiry still binds because the expired generation
// is not in the acceptable set to be tried.
//
// Every lane below is the existing verifyHubUserCookieEitherAt, called once per
// generation with that generation's derived material. Nothing new verifies:
// this adds a second KEY, not a second FORMAT and not a second LANE. In
// particular the F1 legacy symmetric lane stays deleted — legacySecret is passed
// as "" on every attempt, so there is no path by which a rotation could
// resurrect it.
//
// Returns (username, generationID, true) on success.
func verifyHubUserCookieAcrossGenerations(gs *generationSet, value string, now time.Time, revoked hubSessionRevokedFunc) (string, int, bool) {
	if value == "" {
		return "", 0, false
	}
	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) == 0 {
		return "", 0, false
	}

	attempt := func(g keyGeneration) (string, bool) {
		// legacySecret is deliberately "" — see AUDIT F1 in
		// verifyHubUserCookieEitherAt. The lane is gone and must stay gone; a
		// rotation adds generations, never lanes.
		return verifyHubUserCookieEitherAt(sessionPublicKeyForGeneration(g), "", value, now, revoked)
	}

	if claimed := hubCookieClaimedGeneration(value); claimed > 0 {
		for _, g := range acceptable {
			if g.ID != claimed {
				continue
			}
			if u, ok := attempt(g); ok {
				return u, g.ID, true
			}
			// The cookie NAMED this generation and this generation's key says
			// no. Do not fall through: the marker is inside the signature, so a
			// cookie that carries it and fails under it is not a cookie some
			// other generation signed — it is forged, tampered, expired, or
			// revoked. Trying the others could only re-admit a value whose own
			// signed claim already failed.
			return "", 0, false
		}
		// Claimed a generation we no longer accept (expired or unknown). Fall
		// through to trial verification: the marker is an optimization, and an
		// artifact must never be rejected merely for naming a key we retired.
		// If it really was signed by that retired key, every remaining attempt
		// fails anyway — which IS the finiteness guarantee, enforced by the key
		// rather than by the marker.
	}

	for _, g := range acceptable {
		if u, ok := attempt(g); ok {
			return u, g.ID, true
		}
	}
	return "", 0, false
}

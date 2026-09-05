package spoke

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Short-lived signed terminal assertion (finding C3 follow-up).
//
// This is the principled upgrade over the static per-hive username allowlist
// that finding C3 (#2756) shipped: instead of "is this hub user statically
// listed on this hive", the Node proxy in front of ttyd verifies a FRESH,
// EXPIRING, role-carrying grant the spoke minted for THIS user on THIS hive at
// session time, binding {user, hive_id, role, expiry}.
//
// WHY IT DOES NOT SHARE THE SSO SIGNING PATH (C2 coordination):
// The SSO handoff token is ASYMMETRIC (Ed25519, C2 follow-up #2761): ONLY the
// hub may mint it and a spoke holds the PUBLIC key to verify — because an SSO
// token asserts hub-wide identity and a spoke operator must not be able to mint
// one. The terminal assertion is a fundamentally DIFFERENT trust shape: it is
// SYMMETRIC and SPOKE-LOCAL — the spoke both MINTS it (right after it
// establishes a per-user session) and the proxy VERIFIES it, both on the SAME
// spoke, with a key the spoke legitimately holds. That is a correct symmetric
// use, so the terminal assertion has its OWN dedicated HMAC signer
// (terminalSign) and its OWN derived sub-key, entirely independent of the SSO
// signing primitive. It deliberately does NOT call the SSO signer (which #2761
// removes when SSO goes Ed25519), so this code survives that cutover unchanged.
//
// It reuses only the pure DATA-SHAPE helpers from sso.go — the ssoClaims struct,
// the ssoB64 URL-safe base64, and the ssoClockSkew tolerance — none of which is
// signing material.

const (
	// terminalAssertionVersion namespaces the signed payload so a verifier accepts
	// ONLY terminal assertions and can never be tricked into treating an SSO
	// handoff token (or any other domain's token) as a terminal grant. Distinct
	// version string from ssoTokenVersion.
	terminalAssertionVersion = "hive-terminal-v1"

	// terminalAssertionTTL bounds how long a freshly-minted terminal assertion is
	// valid. Longer than the SSO handoff's TTL because it is not consumed-once on a
	// redirect — it rides in a cookie the proxy re-checks on the terminal page load
	// AND the websocket upgrade — but still short so a leaked cookie is a small
	// window, and the user must re-establish a session to renew it. 15 minutes is
	// the upper bound the C3 follow-up called for (e.g. 5–15 min).
	terminalAssertionTTL = 15 * time.Minute

	// infoTerminalKey is the domain-separation label for the terminal assertion's
	// symmetric signing sub-key. It mirrors the C2 (#2758/#2761) deriveDomainKey
	// convention — HMAC-SHA256(master, info) rendered as hex — so when C2's
	// hub_keys.go lands the two derivations are FUNCTIONALLY IDENTICAL and this can
	// be folded onto deriveDomainKey(master, infoTerminalKey) in a later cleanup.
	// It is a DISTINCT label from the heartbeat/session/sso sub-keys, so the
	// terminal key can only ever sign/verify terminal assertions.
	infoTerminalKey = "hive-terminal-v1"

	// EnvTerminalKey is the dedicated spoke-side env var carrying the PER-HIVE
	// terminal signing key (provisionTerminalKey). It is the preferred lane and,
	// measured on the live fleet, the one every spoke actually uses. When unset,
	// TerminalSigningKey SELF-DERIVES the same per-hive value from the master
	// plus HIVE_ID — it never falls back to a fleet-uniform key. See
	// TerminalSigningKey for why both former fallbacks were deleted (audit N3).
	EnvTerminalKey = "HIVE_TERMINAL_KEY"

	// envHubSecret is the raw master, still present on every spoke today. It is
	// read ONLY as the input to the per-hive self-derivation lane, never as a
	// terminal key in its own right.
	envHubSecret = "HIVE_HUB_SECRET"
)

// terminalSign returns the URL-safe base64 HMAC-SHA256 of body under key. This is
// the terminal assertion's OWN signer — identical construction to the pre-C2
// ssoSign, but deliberately separate so the terminal path does not depend on the
// SSO signing primitive (which #2761 replaces with Ed25519).
func terminalSign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return ssoB64.EncodeToString(mac.Sum(nil))
}

// deriveTerminalKeyFrom is DELETED (audit N3).
//
// It returned HMAC-SHA256(master, infoTerminalKey) with NO hive ID. Because the
// master is fleet-uniform — measured live: present on 65/65 spokes with exactly
// ONE distinct value — that expression derived a single terminal key for the
// entire fleet, so an assertion minted on any spoke verified on every other.
// Domain separation does not help when the keying input is shared by every
// tenant; only binding the hive ID does.
//
// The replacement is derivePerHiveKey(master, infoTerminalKey, hiveID), used
// inline by TerminalSigningKey's self-derive lane. Nothing may reintroduce a
// hiveID-less terminal derivation: a helper that exists is a helper that gets
// called, which is how this lane survived the original N3 fix.

// TerminalSigningKey resolves the symmetric key the spoke mints — and the proxy
// verifies — terminal assertions with.
//
// Resolution order, most-to-least specific. EVERY lane is PER-HIVE:
//
//  1. HIVE_TERMINAL_KEY — the hub-injected per-hive sub-key
//     (provisionTerminalKey: HMAC(master, infoTerminalKey || 0x00 || hiveID)).
//     This is the normal hosted path; measured on the live fleet it is present
//     and 65-distinct on every spoke.
//  2. Self-derived per-hive key, from HIVE_HUB_SECRET + HIVE_ID. This mirrors
//     SpokeHeartbeatKey's lane 2 (audit F2) and exists for the same reason: the
//     per-hive key is a pure function of two things the spoke ALREADY HOLDS, so
//     a spoke can become identity-bound with no hub action and no re-provision.
//
// !! AUDIT N3 MUST NOT REGRESS: there is deliberately NO lane that resolves to a
// FLEET-UNIFORM value. !!
//
// Two such lanes used to sit here and both are DELETED by this change:
//
//   - HIVE_SESSION_KEY. Measured live: present on 65/65 spokes and byte-IDENTICAL
//     across all of them. An assertion minted on spoke A verified on spoke B, so
//     any tenant operator could forge a shell grant for an arbitrary user on an
//     arbitrary hive. N3 closed this in PROVISIONING (by injecting
//     HIVE_TERMINAL_KEY so lane 1 wins), but the lane itself was left in the
//     resolver — so it was one absent env var away from being live again, which
//     is exactly the state of a re-provisioned or manifest-reapplied spoke (see
//     perhive_env_reconcile.go's header: the fleet's posture is held by an
//     out-of-band patch no controller maintained).
//   - deriveTerminalKeyFrom(HIVE_HUB_SECRET), i.e. deriveDomainKey with NO hiveID.
//     The master is fleet-uniform (measured: 65/65 spokes, 1 distinct value), so
//     this derived exactly ONE key for the entire fleet. It was the same forgery
//     lane wearing domain separation: separating the DOMAIN does nothing when the
//     input is shared by every tenant.
//
// Lane 2 replaces the second of those in place: same inputs, same no-hub-action
// property, but binding the hive ID makes the result unforgeable across tenants.
// derivePerHiveKey returns "" for an empty hive ID rather than silently falling
// back to a shared key, so a spoke that cannot identify itself mints nothing.
//
// Returns "" when nothing resolves, preserving fail-closed behavior (no key → no
// assertion minted → the proxy falls back to the #2756 static allowlist, which
// is a degradation in convenience, not in safety).
//
// ROTATION (master-key-rotation.md, follow-on PR #5). There is deliberately NO
// trial verification here, and none is possible: a spoke holds ONE master and
// ONE injected key, never a generation set — the hub never provisions generation
// material to a spoke. Terminal assertions are also minted AND verified on the
// same spoke from this same resolver, so minter and verifier hold an identical
// value at every instant and dual acceptance has nothing to reconcile. Rotation
// converges here through the reconcile lane re-patching HIVE_TERMINAL_KEY, at
// the cost of invalidating in-flight assertions — which self-heal within their
// 15-minute TTL. See the design doc's PR #5 note for why that is the whole job.
//
// The Node proxy's TERMINAL_SIGNING_KEY mirrors this order EXACTLY
// (src/proxy/server.js) — the two MUST stay in lockstep, and PR #6 carries the
// matching proxy change.
func TerminalSigningKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvTerminalKey)); v != "" {
		return v
	}
	// Lane 2: self-derive the PER-HIVE key from the master the spoke already
	// holds plus its own identity. Never deriveTerminalKeyFrom(master) — that is
	// fleet-uniform and is the N3 forgery lane.
	return derivePerHiveKey(
		strings.TrimSpace(os.Getenv(envHubSecret)),
		infoTerminalKey,
		strings.TrimSpace(os.Getenv(EnvHiveID)),
	)
}

// MintTerminalAssertion creates a short-lived, HMAC-signed assertion binding
// {username, role, hiveID, expiry} for opening a terminal on a SINGLE hive, using
// the resolved terminal signing key. Reuses the ssoClaims shape but with
// terminalAssertionVersion and the dedicated terminalSign. Returns "" if the key
// is empty or identity is missing.
func MintTerminalAssertion(key, username, role, hiveID string, now time.Time) string {
	if key == "" || username == "" || hiveID == "" {
		return ""
	}
	claims := ssoClaims{
		Version:  terminalAssertionVersion,
		Username: username,
		Role:     role,
		HiveID:   hiveID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(terminalAssertionTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	body := ssoB64.EncodeToString(payload)
	return body + "." + terminalSign(key, body)
}

// VerifyTerminalAssertion validates a terminal assertion against key and the
// verifier's own hiveID, returning the carried username and role. It fails closed
// on any mismatch: bad signature, wrong version (an SSO handoff token is NOT a
// terminal grant), expired/not-yet-valid, or a hiveID that is not THIS hive (an
// assertion minted for hive A can never open a terminal on hive B).
//
// This is the Go reference the Node proxy's verifyTerminalAssertion mirrors
// EXACTLY; keep the two in lockstep. `now` is the verifier's clock.
func VerifyTerminalAssertion(key, token, expectedHiveID string, now time.Time) (username, role string, err error) {
	if key == "" {
		return "", "", fmt.Errorf("terminal-assertion: no signing key configured")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("terminal-assertion: malformed token")
	}
	body, sig := parts[0], parts[1]

	// Constant-time signature check BEFORE trusting any payload bytes.
	expected := terminalSign(key, body)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", "", fmt.Errorf("terminal-assertion: bad signature")
	}

	raw, err := ssoB64.DecodeString(body)
	if err != nil {
		return "", "", fmt.Errorf("terminal-assertion: undecodable payload")
	}
	var claims ssoClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", "", fmt.Errorf("terminal-assertion: unparseable claims")
	}
	if claims.Version != terminalAssertionVersion {
		return "", "", fmt.Errorf("terminal-assertion: unexpected token version")
	}
	if claims.HiveID != expectedHiveID {
		return "", "", fmt.Errorf("terminal-assertion: assertion is for a different hive")
	}
	if claims.Username == "" {
		return "", "", fmt.Errorf("terminal-assertion: empty username")
	}
	nowUnix := now.Unix()
	skew := int64(ssoClockSkew / time.Second)
	if claims.IssuedAt > nowUnix+skew {
		return "", "", fmt.Errorf("terminal-assertion: not yet valid")
	}
	if claims.Expiry < nowUnix-skew {
		return "", "", fmt.Errorf("terminal-assertion: expired")
	}
	return claims.Username, claims.Role, nil
}

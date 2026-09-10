// Package terminalassert holds the SPOKE-LOCAL terminal-assertion primitives:
// per-hive signing-key resolution, minting, and verification of the short-lived
// signed grant that opens a terminal on a single hive.
//
// It is a LEAF package (stdlib-only) extracted from pkg/hub so that the spoke
// dashboard (pkg/dashboard), which both mints and verifies these assertions
// locally, no longer has to import the hub server package to reach its own
// spoke-local security primitive. pkg/hub retains thin aliases for its
// provisioning and test call sites; both sides now consume this leaf.
package terminalassert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
// The claims struct, URL-safe base64, and clock-skew tolerance below are this
// package's own copies of the pure DATA-SHAPE conventions pkg/hub's sso.go
// established — none of which is signing material. The wire shape (JSON tags
// v/u/r/h/iat/exp) MUST stay identical to sso.go's ssoClaims so existing
// assertions and the Node proxy's verifier keep working unchanged.

const (
	// terminalAssertionVersion namespaces the signed payload so a verifier accepts
	// ONLY terminal assertions and can never be tricked into treating an SSO
	// handoff token (or any other domain's token) as a terminal grant. Distinct
	// version string from the SSO token version.
	terminalAssertionVersion = "hive-terminal-v1"

	// terminalAssertionTTL bounds how long a freshly-minted terminal assertion is
	// valid. Longer than the SSO handoff's TTL because it is not consumed-once on a
	// redirect — it rides in a cookie the proxy re-checks on the terminal page load
	// AND the websocket upgrade — but still short so a leaked cookie is a small
	// window, and the user must re-establish a session to renew it. 15 minutes is
	// the upper bound the C3 follow-up called for (e.g. 5–15 min).
	terminalAssertionTTL = 15 * time.Minute

	// InfoKey is the domain-separation label for the terminal assertion's
	// symmetric signing sub-key. It mirrors the C2 (#2758/#2761) deriveDomainKey
	// convention — HMAC-SHA256(master, info) rendered as hex. It is a DISTINCT
	// label from the heartbeat/session/sso sub-keys, so the terminal key can only
	// ever sign/verify terminal assertions. pkg/hub's provisioning path
	// (provisionTerminalKey) uses this same label via its infoTerminalKey alias,
	// which is what keeps the hub-injected lane and the spoke self-derive lane
	// byte-identical.
	InfoKey = "hive-terminal-v1"

	// EnvTerminalKey is the dedicated spoke-side env var carrying the PER-HIVE
	// terminal signing key (provisionTerminalKey). It is the preferred lane and,
	// measured on the live fleet, the one every spoke actually uses. When unset,
	// SigningKey SELF-DERIVES the same per-hive value from the master plus
	// HIVE_ID — it never falls back to a fleet-uniform key. See SigningKey for
	// why both former fallbacks were deleted (audit N3).
	EnvTerminalKey = "HIVE_TERMINAL_KEY"

	// envHubSecret is the raw master, still present on every spoke today. It is
	// read ONLY as the input to the per-hive self-derivation lane, never as a
	// terminal key in its own right.
	envHubSecret = "HIVE_HUB_SECRET"

	// envHiveID mirrors pkg/hub's EnvHiveID: the spoke's own identity, the second
	// input to the per-hive self-derivation lane.
	envHiveID = "HIVE_ID"
)

// claims is the signed payload of a terminal assertion. The JSON tags are the
// shared hive token wire shape (identical to pkg/hub's ssoClaims) and MUST NOT
// change: the Node proxy's verifier parses these exact keys.
type claims struct {
	Version  string `json:"v"`
	Username string `json:"u"`
	Role     string `json:"r"`
	HiveID   string `json:"h"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
}

// b64 encodes without padding so the token is URL-safe (same convention as
// pkg/hub's ssoB64).
var b64 = base64.RawURLEncoding

// clockSkew tolerates minor clock drift between minter and verifier (same value
// as pkg/hub's ssoClockSkew).
const clockSkew = 30 * time.Second

// terminalSign returns the URL-safe base64 HMAC-SHA256 of body under key. This is
// the terminal assertion's OWN signer — identical construction to the pre-C2
// ssoSign, but deliberately separate so the terminal path does not depend on the
// SSO signing primitive (which #2761 replaces with Ed25519).
func terminalSign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return b64.EncodeToString(mac.Sum(nil))
}

// DerivePerHiveKey derives the per-hive, per-domain symmetric sub-key:
// hex(HMAC-SHA256(master, info || 0x00 || hiveID)).
//
// This is the SINGLE implementation of the per-hive derivation — moved here
// from pkg/hub's hub_keys.go, which now delegates. It must stay single-sourced:
// the hub PROVISIONS HIVE_TERMINAL_KEY with it and the spoke SELF-DERIVES the
// same value with it (SigningKey lane 2), so any drift between two copies would
// silently split the two lanes.
//
// Mixing the hive ID into the derivation makes the key per-spoke while keeping
// every property a fleet-wide scheme had: deterministic in the existing master
// (no new secret to store, rotate, or escrow), reproducible by the hub on
// demand, and stable across re-provisioning.
//
// The 0x00 separator matters. Plain concatenation is ambiguous — ("hive-terminal",
// "a|b") and ("hive-terminal|a", "b") would hash identically — and hive IDs are
// operator-influenced, so an ambiguous encoding is a real (if narrow) collision
// lane. A NUL byte cannot appear in either input: info strings are compile-time
// constants and hive IDs are validated by the hub's isValidName.
//
// Returns "" for an empty master OR an empty hiveID: a keyless or identity-less
// caller must fail closed rather than silently sharing one key again.
func DerivePerHiveKey(master, info, hiveID string) string {
	if master == "" || hiveID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	mac.Write([]byte{0})
	mac.Write([]byte(hiveID))
	return hex.EncodeToString(mac.Sum(nil))
}

// SigningKey resolves the symmetric key the spoke mints — and the proxy
// verifies — terminal assertions with.
//
// Resolution order, most-to-least specific. EVERY lane is PER-HIVE:
//
//  1. HIVE_TERMINAL_KEY — the hub-injected per-hive sub-key
//     (provisionTerminalKey: HMAC(master, InfoKey || 0x00 || hiveID)).
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
// Two such lanes used to sit here and both are DELETED by that audit:
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
// DerivePerHiveKey returns "" for an empty hive ID rather than silently falling
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
func SigningKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvTerminalKey)); v != "" {
		return v
	}
	// Lane 2: self-derive the PER-HIVE key from the master the spoke already
	// holds plus its own identity. Never a hiveID-less derivation — that is
	// fleet-uniform and is the N3 forgery lane.
	return DerivePerHiveKey(
		strings.TrimSpace(os.Getenv(envHubSecret)),
		InfoKey,
		strings.TrimSpace(os.Getenv(envHiveID)),
	)
}

// Mint creates a short-lived, HMAC-signed assertion binding
// {username, role, hiveID, expiry} for opening a terminal on a SINGLE hive, using
// the resolved terminal signing key. Reuses the shared claims wire shape but with
// terminalAssertionVersion and the dedicated terminalSign. Returns "" if the key
// is empty or identity is missing.
func Mint(key, username, role, hiveID string, now time.Time) string {
	if key == "" || username == "" || hiveID == "" {
		return ""
	}
	c := claims{
		Version:  terminalAssertionVersion,
		Username: username,
		Role:     role,
		HiveID:   hiveID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(terminalAssertionTTL).Unix(),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	body := b64.EncodeToString(payload)
	return body + "." + terminalSign(key, body)
}

// Verify validates a terminal assertion against key and the verifier's own
// hiveID, returning the carried username and role. It fails closed on any
// mismatch: bad signature, wrong version (an SSO handoff token is NOT a terminal
// grant), expired/not-yet-valid, or a hiveID that is not THIS hive (an assertion
// minted for hive A can never open a terminal on hive B).
//
// This is the Go reference the Node proxy's verifyTerminalAssertion mirrors
// EXACTLY; keep the two in lockstep. `now` is the verifier's clock.
func Verify(key, token, expectedHiveID string, now time.Time) (username, role string, err error) {
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

	raw, err := b64.DecodeString(body)
	if err != nil {
		return "", "", fmt.Errorf("terminal-assertion: undecodable payload")
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", "", fmt.Errorf("terminal-assertion: unparseable claims")
	}
	if c.Version != terminalAssertionVersion {
		return "", "", fmt.Errorf("terminal-assertion: unexpected token version")
	}
	if c.HiveID != expectedHiveID {
		return "", "", fmt.Errorf("terminal-assertion: assertion is for a different hive")
	}
	if c.Username == "" {
		return "", "", fmt.Errorf("terminal-assertion: empty username")
	}
	nowUnix := now.Unix()
	skew := int64(clockSkew / time.Second)
	if c.IssuedAt > nowUnix+skew {
		return "", "", fmt.Errorf("terminal-assertion: not yet valid")
	}
	if c.Expiry < nowUnix-skew {
		return "", "", fmt.Errorf("terminal-assertion: expired")
	}
	return c.Username, c.Role, nil
}

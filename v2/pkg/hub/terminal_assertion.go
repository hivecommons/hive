package hub

import (
	"crypto/hmac"
	"crypto/sha256"
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

	// EnvTerminalKey is the dedicated spoke-side env var a least-privilege
	// provisioning path may inject (mirrors C2's EnvSessionKey / EnvHeartbeatKey).
	// When unset, terminalSigningKey falls back to the session sub-key the spoke
	// already holds, then to deriving from HIVE_HUB_SECRET — see terminalSigningKey.
	EnvTerminalKey = "HIVE_TERMINAL_KEY"

	// envSessionKey / envHubSecret are the fallbacks terminalSigningKey resolves
	// against. Named as string literals here (not imported from hub_keys.go, which
	// lives only on the C2 branch) so this file compiles on its own base and after
	// the C2 merge alike.
	envSessionKey = "HIVE_SESSION_KEY"
	envHubSecret  = "HIVE_HUB_SECRET"
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

// deriveTerminalKeyFrom returns the domain-separated terminal sub-key of master,
// as lowercase hex — HMAC-SHA256(master, infoTerminalKey). Same formula as C2's
// deriveDomainKey, kept local (under a distinct name) so it neither depends on
// nor collides with hub_keys.go's deriveDomainKey when both land. Returns "" for
// an empty master so an unset secret can never derive a usable key (fail closed).
func deriveTerminalKeyFrom(master string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(infoTerminalKey))
	return hex.EncodeToString(mac.Sum(nil))
}

// TerminalSigningKey resolves the symmetric key the spoke mints — and the proxy
// verifies — terminal assertions with. Resolution order (most-to-least specific),
// matching the spoke-side least-privilege story and surviving the C2 cutover:
//
//  1. HIVE_TERMINAL_KEY — a dedicated derived sub-key, if a future provisioning
//     path injects one (most privilege-minimal).
//  2. HIVE_SESSION_KEY — the session sub-key a C2-provisioned spoke ALREADY holds
//     (the spoke both mints and verifies terminal grants, so reusing the
//     spoke-local session key is a legitimate symmetric use). This is the normal
//     post-#2761 hosted path: the spoke no longer receives the master.
//  3. deriveTerminalKeyFrom(HIVE_HUB_SECRET) — self-hosted / pre-#2761 hosted
//     spokes still hold the master; derive the terminal sub-key from it.
//
// Returns "" only when none is configured, preserving fail-closed behavior (no
// key → no assertion minted → proxy falls back to the #2756 static allowlist).
// The Node proxy's terminalSigningKey() mirrors this order EXACTLY.
func TerminalSigningKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvTerminalKey)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envSessionKey)); v != "" {
		return v
	}
	return deriveTerminalKeyFrom(strings.TrimSpace(os.Getenv(envHubSecret)))
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

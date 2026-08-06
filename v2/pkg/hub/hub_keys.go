package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Domain-separated key derivation for the hub master secret (CWE-321/798, C2).
//
// Historically ONE symmetric key, HIVE_HUB_SECRET, signed EVERYTHING: the
// heartbeat bearer, the session cookie, the SSO handoff token, and the
// impersonation cookie — and that same master was injected in plaintext into
// every spoke Deployment. Any spoke operator could read it from the pod env and
// forge admin session cookies, mint SSO-as-any-owner tokens, or emit
// impersonation grants. The signing material for browser/admin trust lived on
// machines whose operators are NOT trusted with that authority.
//
// The fix keeps a single master secret but derives a DISTINCT sub-key per trust
// domain. A holder of one domain's sub-key learns nothing about the master or
// the other domains' keys (HMAC is a PRF), so a sub-key can only ever sign/verify
// within its own domain. Crucially this lets the hub hand a spoke ONLY the
// sub-keys it actually needs (heartbeat + session + sso verification) while the
// impersonation key — and the master itself — never leave the hub.
//
// Derivation is HMAC-SHA256(master, info) rendered as lowercase hex. This is a
// standard HKDF-Expand-style single-block expansion: deterministic, no extra
// dependency (x/crypto/hkdf is not a direct module dependency), and more than
// sufficient for domain separation since each info string is a fixed, unique
// label. The output is hex so every derived key is a safe ASCII value for an env
// var, a Bearer token, and an HMAC key on both the Go and Node sides.

const (
	// Domain-separator info strings. Each is versioned ("-v1") so the format can
	// evolve without a verifier ever silently accepting a foreign-domain key.
	// These constants are the contract shared with the spoke (which receives the
	// already-derived keys) and with the Node proxy (v2/proxy/server.js), so they
	// must NOT change without a coordinated rollout.
	infoHeartbeatKey   = "hive-heartbeat-v1"
	infoSessionKey     = "hive-session-v1"
	infoSSOKey         = "hive-sso-v1"
	infoImpersonateKey = "hive-impersonate-v1"
)

// deriveDomainKey returns a domain-separated sub-key of master for the given
// info label, as lowercase hex. Returns "" for an empty master so callers keep
// their existing "no secret configured → feature disabled / fail closed"
// behavior unchanged (an empty master must never derive to a usable key).
func deriveDomainKey(master, info string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	return hex.EncodeToString(mac.Sum(nil))
}

// The four per-domain accessors below are the ONLY way hub code should obtain
// signing material. Nothing outside the heartbeat verify path should ever touch
// s.hubSecret (the master) directly again.

func (s *HubServer) heartbeatKey() string {
	return deriveDomainKey(s.hubSecret, infoHeartbeatKey)
}

func (s *HubServer) sessionKey() string {
	return deriveDomainKey(s.hubSecret, infoSessionKey)
}

func (s *HubServer) ssoKey() string {
	return deriveDomainKey(s.hubSecret, infoSSOKey)
}

func (s *HubServer) impersonateKey() string {
	return deriveDomainKey(s.hubSecret, infoImpersonateKey)
}

// Dedicated spoke-side env vars. A hub-hosted spoke is provisioned with ONLY the
// derived sub-keys it needs (heartbeat + session + sso); it never receives the
// master HIVE_HUB_SECRET. Impersonation is a hub-only capability, so no spoke key
// exists for it — a spoke simply cannot mint or verify an impersonation grant.
const (
	EnvHeartbeatKey = "HIVE_HEARTBEAT_KEY"
	EnvSessionKey   = "HIVE_SESSION_KEY"
	EnvSSOKey       = "HIVE_SSO_KEY"
)

// spokeDomainKey resolves a domain sub-key for spoke-side code. It prefers the
// dedicated per-domain env var (the modern, least-privilege provisioning path).
// If that is empty it falls back to deriving the sub-key from HIVE_HUB_SECRET —
// the backward-compatible path for (a) self-hosted hives whose operator legitimately
// holds the master and points heartbeats at their own hub, and (b) any spoke still
// rolling on an OLD Deployment that injected the master before this change. In the
// fallback the spoke still gets the CORRECT domain-separated key, so hub verification
// (which now expects the derived key) succeeds either way. Returns "" only when
// neither source is configured, preserving each caller's fail-closed behavior.
func spokeDomainKey(envVar, info string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	return deriveDomainKey(strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")), info)
}

// SpokeHeartbeatKey returns the bearer a spoke presents on /api/heartbeat and
// /api/task-status. Exported for use from the dashboard/heartbeat call sites.
func SpokeHeartbeatKey() string { return spokeDomainKey(EnvHeartbeatKey, infoHeartbeatKey) }

// SpokeSessionKey returns the key a spoke uses to VERIFY the hub-minted
// `hive_hub_user` session/terminal cookie. It signs nothing on the spoke.
func SpokeSessionKey() string { return spokeDomainKey(EnvSessionKey, infoSessionKey) }

// SpokeSSOKey returns the key a spoke uses to VERIFY a hub-minted SSO handoff
// token. It signs nothing on the spoke.
func SpokeSSOKey() string { return spokeDomainKey(EnvSSOKey, infoSSOKey) }

// provisionMasterSecret resolves the hub's MASTER secret at provision time, the
// same way NewHubServer does: prefer HIVE_HUB_SECRET, else the persisted
// /data/saas/hub-secret.key. Used ONLY to derive the per-domain sub-keys that get
// injected into a spoke — the master itself is never placed in the Deployment.
func provisionMasterSecret() string {
	if s := strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")); s != "" {
		return s
	}
	if data, err := os.ReadFile("/data/saas/hub-secret.key"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

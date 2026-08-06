package hub

import (
	"crypto/ed25519"
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

	// infoSSOEd25519Seed derives the SSO signing SEED for the C2 follow-up: SSO is
	// now ASYMMETRIC. deriveDomainKey(master, infoSSOEd25519Seed) yields a 32-byte
	// (hex) value used verbatim as the Ed25519 seed (ed25519.SeedSize == 32, and a
	// SHA-256 HMAC is exactly 32 bytes), so the hub's SSO keypair is deterministic
	// in the existing master — no new secret, no rotation. Distinct label from the
	// legacy symmetric infoSSOKey so the two can never derive to the same bytes.
	infoSSOEd25519Seed = "hive-sso-ed25519-v1"
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

// ssoSigningSeed returns the hex-encoded 32-byte Ed25519 SEED the hub signs SSO
// handoff tokens with (C2 follow-up: SSO is asymmetric). This is PRIVATE-key
// material and must NEVER be injected into a spoke — spokes get only the public
// key (SpokeSSOPublicKey / provisionSSOPublicKey). Returns "" for an empty master
// so minting stays fail-closed.
func (s *HubServer) ssoSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSSOEd25519Seed)
}

func (s *HubServer) impersonateKey() string {
	return deriveDomainKey(s.hubSecret, infoImpersonateKey)
}

// ssoPublicKeyFromSeed expands a hex Ed25519 seed into the hex-encoded 32-byte
// public key. Returns "" if seedHex is not a valid 32-byte seed. Used by both the
// hub-side public-key accessor and provisioning so the spoke and hub agree on the
// exact public key derived from one master.
func ssoPublicKeyFromSeed(seedHex string) string {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}

// Dedicated spoke-side env vars. A hub-hosted spoke is provisioned with ONLY the
// derived sub-keys it needs (heartbeat + session + sso); it never receives the
// master HIVE_HUB_SECRET. Impersonation is a hub-only capability, so no spoke key
// exists for it — a spoke simply cannot mint or verify an impersonation grant.
const (
	EnvHeartbeatKey = "HIVE_HEARTBEAT_KEY"
	EnvSessionKey   = "HIVE_SESSION_KEY"
	// EnvSSOPublicKey carries the Ed25519 PUBLIC key a spoke verifies SSO handoff
	// tokens with (C2 follow-up). It replaces the old symmetric HIVE_SSO_KEY: a
	// spoke holding only the public key can verify hub-minted tokens but CANNOT mint
	// any. EnvSSOKeyLegacy is still read for one release so spokes rolling on an old
	// Deployment (symmetric HIVE_SSO_KEY, pre-cutover hub) keep working until the hub
	// re-provisions them with the public key.
	EnvSSOPublicKey = "HIVE_SSO_PUBLIC_KEY"
	EnvSSOKeyLegacy = "HIVE_SSO_KEY"
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

// SpokeSSOPublicKey returns the Ed25519 PUBLIC key a spoke uses to VERIFY a
// hub-minted SSO handoff token (C2 follow-up: SSO is asymmetric). It signs nothing
// on the spoke and — unlike the old symmetric key — cannot be used to mint a token
// even by a hostile spoke operator who reads it from the pod env.
//
// Resolution order:
//  1. HIVE_SSO_PUBLIC_KEY — the modern, least-privilege injection (public key only).
//  2. Derive from HIVE_HUB_SECRET — for self-hosted hives whose operator legitimately
//     holds the master and points SSO at their own hub, and for any spoke rolling on
//     an OLD Deployment that injected the master before the C2 change. The spoke
//     derives the SAME public key the hub signs against, so verification succeeds.
//
// It deliberately does NOT fall back to the legacy symmetric HIVE_SSO_KEY: that key
// is not an Ed25519 public key, so it can neither verify a -v2 token nor be turned
// into the correct public key without the master. A spoke that still holds only the
// legacy symmetric key will get "" here and fail SSO closed until the hub
// re-provisions it with HIVE_SSO_PUBLIC_KEY (the mint side moved to -v2 in lockstep).
func SpokeSSOPublicKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvSSOPublicKey)); v != "" {
		return v
	}
	return ssoPublicKeyFromSeed(SSOSigningSeedFromMaster(strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET"))))
}

// SSOSigningSeedFromMaster derives the hex Ed25519 signing SEED from a master
// secret. This is PRIVATE-key material: it can mint SSO tokens, so it must only be
// used where the caller legitimately holds the master (the hub itself, or a
// self-hosted operator's own tooling/tests). It intentionally requires the master
// — a spoke, which holds only the public key, can never produce this seed. Returns
// "" for an empty master (fail-closed).
func SSOSigningSeedFromMaster(master string) string {
	return deriveDomainKey(master, infoSSOEd25519Seed)
}

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

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

	// infoInviteKey derives the PER-HIVE HMAC key that signs contributor invite
	// tokens (dashboard.inviteSigningSecret). Invite tokens carry attribution, not
	// access, so this is the lowest-value key in the set — but it was the LAST
	// spoke-side consumer of the raw master HIVE_HUB_SECRET, which is why it gets
	// its own derived lane. Per-hive rather than fleet-wide because an invite
	// minted on one tenant's hive has no business verifying on another's.
	infoInviteKey = "hive-invite-v1"

	// infoSSOEd25519Seed derives the SSO signing SEED for the C2 follow-up: SSO is
	// now ASYMMETRIC. deriveDomainKey(master, infoSSOEd25519Seed) yields a 32-byte
	// (hex) value used verbatim as the Ed25519 seed (ed25519.SeedSize == 32, and a
	// SHA-256 HMAC is exactly 32 bytes), so the hub's SSO keypair is deterministic
	// in the existing master — no new secret, no rotation. Distinct label from the
	// legacy symmetric infoSSOKey so the two can never derive to the same bytes.
	infoSSOEd25519Seed = "hive-sso-ed25519-v1"

	// infoSessionEd25519Seed derives the Ed25519 signing SEED for the hub SESSION
	// cookie (audit N2). Same construction as infoSSOEd25519Seed and for the same
	// reason: a spoke must be able to VERIFY a hub-minted value without being able
	// to MINT one.
	//
	// The symmetric infoSessionKey above cannot give that. It is provisioned into
	// every spoke so the spoke's proxy can check `hive_hub_user`, but an HMAC key
	// that verifies also signs — so any spoke operator could read it from their pod
	// env and mint `clubanderson.<sig>`, which requireAdmin accepts on ~21 admin
	// routes. Distinct label from infoSessionKey so the two can never collide.
	infoSessionEd25519Seed = "hive-session-ed25519-v1"
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

// derivePerHiveKey returns a sub-key bound to BOTH a trust domain and a single
// hive: HMAC-SHA256(master, info || 0x00 || hiveID).
//
// SECURITY (audit N1/N3, CWE-321/798). deriveDomainKey above takes a FIXED label,
// so every spoke in the fleet derives byte-identical keys from the one master.
// Possession therefore proves "some provisioned spoke" and never "this spoke" —
// which is the whole of the hosted kill chain: spoke A's key verifies on spoke B,
// so one hostile tenant can forge terminal grants for any other tenant and beat
// heartbeats as any victim.
//
// Mixing the hive ID into the derivation makes the key per-spoke while keeping
// every property the fleet-wide scheme had: deterministic in the existing master
// (no new secret to store, rotate, or escrow), reproducible by the hub on demand,
// and stable across re-provisioning.
//
// The 0x00 separator matters. Plain concatenation is ambiguous — ("hive-terminal",
// "a|b") and ("hive-terminal|a", "b") would hash identically — and hive IDs are
// operator-influenced, so an ambiguous encoding is a real (if narrow) collision
// lane. A NUL byte cannot appear in either input: info strings are compile-time
// constants and hive IDs are validated by isValidName.
//
// Returns "" for an empty master OR an empty hiveID: a keyless or identity-less
// caller must fail closed rather than silently sharing one key again.
func derivePerHiveKey(master, info, hiveID string) string {
	if master == "" || hiveID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	mac.Write([]byte{0})
	mac.Write([]byte(hiveID))
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

// sessionSigningSeed returns the hex-encoded 32-byte Ed25519 SEED the hub signs
// session cookies with (audit N2). PRIVATE key material: it must NEVER be
// injected into a spoke — spokes receive only sessionPublicKey().
func (s *HubServer) sessionSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSessionEd25519Seed)
}

// sessionPublicKey returns the hex Ed25519 PUBLIC key a spoke verifies hub
// session cookies with. Safe to place in a Deployment: it verifies, it cannot
// sign.
func (s *HubServer) sessionPublicKey() string {
	return ssoPublicKeyFromSeed(s.sessionSigningSeed())
}

// provisionSessionPublicKey is the provisioning-time mirror of
// sessionPublicKey, resolved against the CURRENT generation's secret
// (provisionCurrentSecret). Before any rotation exists that is byte-identical
// to provisionMasterSecret().
func provisionSessionPublicKey() string {
	return ssoPublicKeyFromSeed(deriveDomainKey(provisionCurrentSecret(), infoSessionEd25519Seed))
}

// provisionTerminalKey returns the PER-HIVE terminal signing key injected into a
// spoke as HIVE_TERMINAL_KEY (audit N3).
//
// The terminal assertion is minted on the spoke (dashboard/session.go) and
// verified on that same spoke (proxy/server.js), so a symmetric key is the right
// shape here — unlike the hub session cookie, no other party needs to verify it.
// What was wrong was that TerminalSigningKey() fell through to HIVE_SESSION_KEY,
// which is fleet-uniform: an assertion minted with spoke A's key verified on
// spoke B, so any spoke could forge a shell grant for any user on any tenant.
//
// Binding the key to the hive ID closes that without changing the mint/verify
// code on either side — TerminalSigningKey() and the proxy's mirror already
// prefer HIVE_TERMINAL_KEY; provisioning simply never set it.
func provisionTerminalKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoTerminalKey, hiveID)
}

// provisionHeartbeatKey returns the PER-HIVE heartbeat bearer injected into a
// spoke as HIVE_HEARTBEAT_KEY (audit N1).
//
// The spoke presents this to the hub, so unlike the terminal key it crosses the
// trust boundary — but it is still symmetric, because the hub can re-derive it
// on demand from the master plus the claimed hive ID. That is precisely what
// makes the identity binding possible: with a fleet-shared bearer the hub had no
// way to check that the caller was the hive it claimed to be, which is why
// handleHeartbeat trusted body-supplied hive_id and three key-delivery lanes
// became IDOR.
func provisionHeartbeatKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoHeartbeatKey, hiveID)
}

// provisionInviteKey returns the PER-HIVE contributor-invite signing key injected
// into a spoke as HIVE_INVITE_KEY.
//
// Mirrors provisionTerminalKey exactly: the token is both minted and verified on
// the same spoke (dashboard/api_contribute.go), so a symmetric key is the right
// shape, and binding it to the hive ID means an invite link from one tenant is
// meaningless on another.
//
// The point of this var is removing the last spoke-side READER of the raw master:
// inviteSigningSecret() used HIVE_HUB_SECRET itself as the HMAC key, so the invite
// lane was the one place a spoke still needed the master to function. With this
// injected it does not.
func provisionInviteKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoInviteKey, hiveID)
}

// heartbeatKeyFor returns the per-hive heartbeat bearer the hub EXPECTS from
// hiveID. Mirror of provisionHeartbeatKey, resolved against the running hub's
// own master rather than the provisioning-time lookup.
func (s *HubServer) heartbeatKeyFor(hiveID string) string {
	return derivePerHiveKey(s.hubSecret, infoHeartbeatKey, hiveID)
}

// verifyHeartbeatBearer authenticates a heartbeat and BINDS it to the claimed
// hive (audit N1/F2).
//
// The ONLY accepted credential is the per-hive bearer for exactly this hiveID:
// HMAC(master, infoHeartbeatKey || 0x00 || hiveID). Because the hub re-derives
// it from the hive ID the caller claims, presenting it proves the caller is THAT
// hive — the claimed identity is self-authenticating.
//
// A fleet-wide lane used to be accepted alongside it: deriveDomainKey(master,
// infoHeartbeatKey), a pure function of the single hub master and therefore
// stamped identically into every spoke. Possession proved "some provisioned
// spoke" and never "THIS hive", and handleHeartbeat trusts the body-supplied
// hive_id, so any spoke could beat as any victim and be handed the victim's key
// material. That lane is DELETED (F2). It was retained only to avoid a flag-day
// cutover; the precondition for removal has since been met — every spoke either
// holds an injected per-hive bearer or self-derives one from the master plus its
// own HIVE_ID (SpokeHeartbeatKey), which needs no hub-side re-provisioning.
//
// Fails closed: an empty bearer, or an empty hiveID (which makes the per-hive
// derivation return ""), authenticates nothing. The comparison is constant-time.
func (s *HubServer) verifyHeartbeatBearer(presented, hiveID string) bool {
	if presented == "" {
		return false
	}
	perHive := s.heartbeatKeyFor(hiveID)
	return perHive != "" && secureCompareHub(presented, perHive)
}

// heartbeatBearerIsPerHive reports whether the presented bearer is the
// identity-bound one. Retained after the F2 deletion because it still backs the
// live GET /api/saas/admin/auth-rollout telemetry (noteHeartbeatAuthPath →
// AuthRolloutStatus), which now serves a different purpose: rather than gating
// the deletion, it lets an operator SEE which hives are authenticating and
// confirm none regressed. Post-F2 a bearer that is not per-hive no longer
// verifies at all, so callers observe this only for bearers that already passed
// verifyHeartbeatBearer.
func (s *HubServer) heartbeatBearerIsPerHive(presented, hiveID string) bool {
	perHive := s.heartbeatKeyFor(hiveID)
	return perHive != "" && secureCompareHub(presented, perHive)
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
	// EnvInviteKey carries the per-hive contributor-invite signing key. The spoke
	// both mints and verifies invite tokens with it, so it is symmetric; it exists
	// so inviteSigningSecret() no longer has to read the raw master.
	EnvInviteKey = "HIVE_INVITE_KEY"
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

// EnvHiveID is the spoke's own hive identity, injected into every hosted
// Deployment and read by loadOrGenerateHiveID as the authoritative source. It is
// the second input SpokeHeartbeatKey needs to SELF-DERIVE the per-hive bearer.
const EnvHiveID = "HIVE_ID"

// SpokeHeartbeatKey returns the bearer a spoke presents on /api/heartbeat and
// /api/task-status. Exported for use from the dashboard/heartbeat call sites.
//
// Resolution order, most to least identity-bound (audit F2):
//
//  1. HIVE_HEARTBEAT_KEY — the hub-injected value. Since the N1 provisioning
//     change this IS the per-hive bearer, so a spoke provisioned at any point
//     after that change is already identity-bound with no code involved.
//  2. Self-derived per-hive bearer, from HIVE_HUB_SECRET + HIVE_ID. This is the
//     lane that makes an IN-PLACE cutover possible without re-provisioning.
//  3. Fleet-wide derivation from HIVE_HUB_SECRET alone — the legacy value, kept
//     only for a spoke that genuinely cannot identify itself (no HIVE_ID).
//
// Lane 2 is the F2 fix. The per-hive bearer is HMAC(master, info || 0x00 ||
// hiveID) — a pure function of two things the spoke ALREADY HOLDS. Any spoke
// with both the master and its own hive ID can compute the identity-bound bearer
// itself, with no hub action, no new secret, and no re-provision. Measured on the
// live fleet, 62 of 65 spokes are already on lane 1 and the remaining 3 have
// HIVE_HEARTBEAT_KEY absent but HIVE_HUB_SECRET and HIVE_ID both present — so
// they move from the fleet-wide bearer to the per-hive one the moment they roll
// this code, which is what lets the hub's legacy lane be deleted afterwards.
//
// A spoke deriving lane 2 is strictly SAFER than one deriving lane 3: it presents
// a credential that only authenticates as itself. This does not weaken the
// self-hosted case either — a single-tenant operator's hub re-derives the same
// per-hive value from the same master, so verification succeeds unchanged.
//
// Lane 3 remains only for the degenerate "master but no identity" configuration.
// derivePerHiveKey returns "" for an empty hive ID rather than silently sharing
// one key again, so that case is detected rather than assumed away.
func SpokeHeartbeatKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvHeartbeatKey)); v != "" {
		return v
	}
	master := strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET"))
	if hiveID := strings.TrimSpace(os.Getenv(EnvHiveID)); hiveID != "" {
		if perHive := derivePerHiveKey(master, infoHeartbeatKey, hiveID); perHive != "" {
			return perHive
		}
	}
	return deriveDomainKey(master, infoHeartbeatKey)
}

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

// provisionGenerationSet resolves the generation set that provision-time and
// reconcile-time derivation share.
//
// WHY THIS EXISTS AS ONE FUNCTION. saas_provision.go's template data map and
// perhive_env_reconcile.go's desiredPerHiveEnv must derive the five per-hive
// env vars byte-identically — desiredPerHiveEnv's own doc comment says so, and
// the consequence of divergence is not subtle: the sweep would see "drift" on
// every freshly provisioned hive, patch it, and roll its pod every cycle
// forever. Making them both resolve their master through THIS function means a
// rotation cannot move one side without the other.
//
// TODAY THIS IS EXACTLY legacyGenerationSet(provisionMasterSecret()). There is
// no persisted generations file yet and no endpoint that can create a second
// generation (that is follow-on PR #4), so currentSecret() is always the same
// string provisionMasterSecret() returns and every derived value is
// byte-identical to what this code produced before generations existed. This
// change is a READ-PATH change in effect: it re-expresses where the master
// comes from without changing which master it is.
//
// Returns nil for an empty master, and currentSecret() on a nil set returns ""
// — so the fail-closed contract every caller already relies on (deriveDomainKey
// and derivePerHiveKey both return "" for an empty master) is preserved through
// the nil case rather than needing a new one.
func provisionGenerationSet() *generationSet {
	if gs := provisionGenerationsOverride; gs != nil {
		return gs
	}
	return legacyGenerationSet(provisionMasterSecret())
}

// provisionGenerationsOverride replaces the resolved generation set.
//
// It exists so a test can drive the ROTATED case, which is otherwise
// unreachable: there is no persisted generations file and no endpoint that can
// create a second generation until follow-on PR #4, so without this seam
// "derives from the current generation" and "derives from the raw master" are
// indistinguishable and no test could tell them apart. That is precisely the
// kind of test-that-passes-for-the-wrong-reason this seam prevents.
//
// nil in production, set only via withProvisionGenerations in tests. Read
// without a lock because it is only ever written before the code under test
// runs, from that test's own goroutine — the same discipline as
// HubServer.keyGenerations, which is set once in NewHubServer.
var provisionGenerationsOverride *generationSet

// provisionCurrentSecret is the MINTING master at provision/reconcile time: the
// secret of the current generation. Every spoke-bound derivation goes through
// this rather than provisionMasterSecret() directly, so that after a rotation
// newly provisioned and newly reconciled spokes both receive material from the
// new current generation while the hub's dual acceptance keeps the not-yet-
// converged spokes authenticating against the demoted previous one.
func provisionCurrentSecret() string {
	return provisionGenerationSet().currentSecret()
}

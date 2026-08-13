package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Domain-separated key derivation for the hub master secret — v2 hub-side
// backport of the v4 C2 fix (#2758) plus the Ed25519 SSO follow-up (#2771),
// carried onto v2 so this hub can authenticate a v4 spoke that has moved to
// derived keys.
//
// SCOPE OF THIS BACKPORT (deliberately hub-only): the v2 hub keeps signing the
// legacy raw-secret heartbeat and symmetric-HMAC SSO for the 33 old hives, and
// ADDITIONALLY derives the domain-separated keys a v4 spoke self-derives from
// the SAME master HIVE_HUB_SECRET. It ports ONLY what the hub needs to verify a
// v4 heartbeat bearer and mint a v4-verifiable SSO token: the four info labels,
// deriveDomainKey, ssoPublicKeyFromSeed, SSOSigningSeedFromMaster, and the two
// hub accessors heartbeatKey / ssoSigningSeed. The spoke-side Spoke*/spokeDomainKey/
// Env* provisioning symbols from v4 are intentionally NOT ported — no spoke change
// is needed here and the v2 hub never provisions derived keys into a Deployment.
//
// Derivation is HMAC-SHA256(master, info) rendered as lowercase hex (an
// HKDF-Expand-style single-block expansion): deterministic, no new dependency,
// and byte-identical to what the v4 spoke computes, so the two agree on every
// derived key given one shared master.

const (
	// Domain-separator info strings. Each is versioned ("-v1") so the format can
	// evolve without a verifier ever silently accepting a foreign-domain key.
	// These constants are the contract shared with the v4 spoke (which self-derives
	// the same keys from HIVE_HUB_SECRET), so they must match v4 verbatim.
	infoHeartbeatKey = "hive-heartbeat-v1"
	infoSessionKey   = "hive-session-v1"
	infoSSOKey       = "hive-sso-v1"

	// infoSSOEd25519Seed derives the SSO signing SEED for the asymmetric SSO
	// cutover: deriveDomainKey(master, infoSSOEd25519Seed) yields a 32-byte (hex)
	// value used verbatim as the Ed25519 seed (ed25519.SeedSize == 32, and a
	// SHA-256 HMAC is exactly 32 bytes), so the hub's SSO keypair is deterministic
	// in the existing master — no new secret, no rotation. Distinct label from the
	// legacy symmetric infoSSOKey so the two can never derive to the same bytes.
	infoSSOEd25519Seed = "hive-sso-ed25519-v1"

	// infoSessionEd25519Seed derives the Ed25519 signing SEED for the hub SESSION
	// cookie (audit N2). Same construction as infoSSOEd25519Seed and for the same
	// reason: a spoke must be able to VERIFY a hub-minted value without being
	// able to MINT one. The symmetric infoSessionKey cannot give that — an HMAC
	// key that verifies also signs, so any spoke holding it could mint an admin
	// cookie. Distinct label so the two can never collide.
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
// SECURITY (audit N1/N3, CWE-321/798). deriveDomainKey above takes a FIXED
// label, so every spoke in the fleet derives byte-identical keys from the one
// master. Possession therefore proves "some provisioned spoke" and never "this
// spoke" — which is the whole of the hosted kill chain: spoke A's key verifies
// on spoke B, so one hostile tenant can forge terminal grants for another and
// beat heartbeats as any victim.
//
// Mixing the hive ID in keeps every property the fleet-wide scheme had:
// deterministic in the existing master (no new secret to store or rotate),
// re-derivable by the hub on demand, and stable across re-provisioning.
//
// The 0x00 separator matters. Plain concatenation is ambiguous —
// ("hive-terminal", "a|b") and ("hive-terminal|a", "b") would hash identically —
// and hive IDs are operator-influenced. A NUL can appear in neither input: info
// strings are compile-time constants and hive IDs are validated by isValidName.
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

// heartbeatKeyFor returns the per-hive heartbeat bearer the hub EXPECTS from
// hiveID (audit N1).
func (s *HubServer) heartbeatKeyFor(hiveID string) string {
	return derivePerHiveKey(s.hubSecret, infoHeartbeatKey, hiveID)
}

// heartbeatBearerIsPerHive reports whether the presented bearer is the modern,
// identity-bound one. Rollout telemetry only — see issue #3234.
func (s *HubServer) heartbeatBearerIsPerHive(presented, hiveID string) bool {
	perHive := s.heartbeatKeyFor(hiveID)
	return perHive != "" && secureCompareHub(presented, perHive)
}

// heartbeatKey returns the derived heartbeat bearer a v4 spoke presents on
// /api/heartbeat and /api/task-status. The v2 hub accepts this IN ADDITION to
// the legacy raw s.hubSecret (see heartbeatBearerOK), never instead of it.
func (s *HubServer) heartbeatKey() string {
	return deriveDomainKey(s.hubSecret, infoHeartbeatKey)
}

// sessionCookieKey returns the derived session key the hub signs the
// hive_hub_user cookie with after the C2 session-key cutover. A v4 spoke's Node
// proxy verifies the cookie against deriveDomainKey(master, infoSessionKey), so
// the hub must mint with the SAME derived key (not the raw master) for the
// spoke's standalone /terminal to accept the session. Returns "" for an empty
// master, so mintHubUserCookieValue stays fail-closed (no unsigned cookie).
func (s *HubServer) sessionCookieKey() string {
	return deriveDomainKey(s.hubSecret, infoSessionKey)
}

// sessionSigningSeed returns the hex Ed25519 SEED the hub signs session cookies
// with (audit N2). PRIVATE material: never inject it into a spoke.
func (s *HubServer) sessionSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSessionEd25519Seed)
}

// sessionPublicKey returns the hex Ed25519 PUBLIC key a spoke verifies hub
// session cookies with. Safe to place in a Deployment: it verifies, never signs.
func (s *HubServer) sessionPublicKey() string {
	return ssoPublicKeyFromSeed(s.sessionSigningSeed())
}

// verifyHubUserDual verifies the hive_hub_user cookie against the derived
// session key (how cookies are minted after the C2 session-key cutover) OR
// the raw master (legacy cookies still in browsers). Additive: never rejects
// a value the single-key check would have accepted.
func (s *HubServer) verifyHubUserDual(value string) (string, bool) {
	// N2 (CWE-321/798): accept the ASYMMETRIC value first. Only the hub can mint
	// it — a spoke holding just the public key can verify but not forge — so it
	// is the strongest of the three and should win when present.
	// F10: a v3 cookie is the strongest form — same Ed25519 asymmetry as v2, PLUS
	// a signed expiry and a revocable session ID. Try it first so a v3 value is
	// never silently downgraded to a weaker lane.
	//
	// revoked is nil here: this branch has no revocation store yet, and nil means
	// "nothing is revoked", which is the correct behaviour while nothing mints v3.
	// Wiring a real store is part of the mint flip, not of shipping the verifier.
	if u, ok := verifyHubUserCookieValueV3(s.sessionPublicKey(), value, time.Now(), nil); ok {
		return u, true
	}
	if u, ok := verifyHubUserCookieValueV2(s.sessionPublicKey(), value); ok {
		return u, true
	}
	// The two symmetric paths below are ROLLOUT ONLY. Both are forgeable by any
	// spoke that holds the corresponding key, which on v2 is every spoke (the
	// raw master is injected into each Deployment). They stay until the fleet is
	// re-provisioned and existing cookies age out; #3234 tracks the removal.
	if u, ok := verifyHubUserCookieValue(s.sessionCookieKey(), value); ok {
		return u, true
	}
	return verifyHubUserCookieValue(s.hubSecret, value)
}

// ssoSigningSeed returns the hex-encoded 32-byte Ed25519 SEED the hub signs v4
// SSO handoff tokens with. This is PRIVATE-key material (it can mint tokens) and
// must never leave the hub. Returns "" for an empty master so minting stays
// fail-closed.
func (s *HubServer) ssoSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSSOEd25519Seed)
}

// ssoPublicKeyFromSeed expands a hex Ed25519 seed into the hex-encoded 32-byte
// public key. Returns "" if seedHex is not a valid 32-byte seed. Kept for parity
// with v4 and for tests that assert the hub and a v4 spoke derive the same
// public key from one master.
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

// SSOSigningSeedFromMaster derives the hex Ed25519 signing SEED from a master
// secret. This is PRIVATE-key material: it can mint SSO tokens, so it must only
// be used where the caller legitimately holds the master (the hub itself). It
// intentionally requires the master. Returns "" for an empty master (fail-closed).
func SSOSigningSeedFromMaster(master string) string {
	return deriveDomainKey(master, infoSSOEd25519Seed)
}

// ssoEd25519HiveIDs is the transitional allowlist of hive IDs known to run a v4
// image, i.e. spokes that verify SSO with an Ed25519 PUBLIC key and therefore
// CANNOT verify the legacy symmetric token. It is seeded with the auto-upgraded
// console hive; add IDs here as more hives cut over to v4.
//
// TRANSITIONAL: this exists only until the whole fleet is v4. The real signal is
// the heartbeat-reported version/commit (see hiveWantsEd25519SSO), which lets a
// hive opt in automatically once it reports a v4 build. Until every hive reports
// a classifiable version, this positive-only allowlist keeps the DEFAULT as the
// legacy symmetric mint (safe for the 33 old hives) and switches to Ed25519 ONLY
// for a hive we have positively identified as v4.
var ssoEd25519HiveIDs = map[string]bool{
	"hosted-kubestellar-console-4vkt": true,
	// Spokes IMAGE-FLIPPED to v4 rather than provisioned as v4. They run v4 code
	// (so they verify Ed25519 only) but the hub has no registry entry for them
	// yet — hiveWantsEd25519SSO returns early on entry == nil, so the rolling-tag
	// check below never runs and they fell back to the legacy symmetric mint.
	// Listing them here restores SSO immediately; each drops out of this map on
	// its own once it heartbeats and the v4-latest tag check classifies it.
	"hosted-available-akswec2-260810-ig9w":               true,
	"hosted-available-akswec2-260810-n0sv":               true,
	"hosted-available-akswec2-260810-wbkh":               true,
	"hosted-available-oke-01-placeholder-bb95":           true,
	"hosted-available-oke-11-placeholder-r05x":           true,
	"hosted-available-oke-12-placeholder-2soo":           true,
	"hosted-available-oke-14-placeholder-jwgm":           true,
	"hosted-available-oke-15-placeholder-iv1r":           true,
	"hosted-available-oke-260812-0nzc-available-ok-ba44": true,
	"hosted-available-oke-260812-756i-available-ok-7ids": true,
	"hosted-available-oke-260812-8yiz-available-ok-go05": true,
	"hosted-available-oke-260812-ixya-available-ok-3do3": true,
	"hosted-available-oke-260812-jlem-available-ok-nnpj": true,
	"hosted-available-vllmd-09":                          true,
	"hosted-available-vllmd-15":                          true,
	"hosted-available-vllmd-260731-fbo1":                 true,
	"hosted-available-vllmd-260731-fikn":                 true,
	"hosted-available-vllmd-260731-gzlo":                 true,
	"hosted-available-vllmd-260731-iia2":                 true,
	"hosted-available-vllmd-260731-jnip":                 true,
	"hosted-available-vllmd-260731-nq4a":                 true,
	"hosted-available-vllmd-260731-os6j":                 true,
	"hosted-available-vllmd-260731-sl58":                 true,
	"hosted-available-vllmd-260731-vx5z":                 true,
	"hosted-available-vllmd-260806-1bo3":                 true,
	"hosted-available-vllmd-260806-1cyh":                 true,
	"hosted-available-vllmd-260806-2d19":                 true,
	"hosted-available-vllmd-260806-3yl8":                 true,
	"hosted-available-vllmd-260806-50rc":                 true,
	"hosted-available-vllmd-260806-5q6l":                 true,
	"hosted-available-vllmd-260806-6vax":                 true,
	"hosted-available-vllmd-260806-8ejh":                 true,
	"hosted-available-vllmd-260806-8xkt":                 true,
	"hosted-available-vllmd-260806-ajoe":                 true,
	"hosted-open-source-osscar-appli-rc57":               true,
}

// hiveWantsEd25519SSO reports whether the hub should mint an ASYMMETRIC (Ed25519)
// SSO token for this hive instead of the legacy symmetric one. It returns true
// ONLY on positive identification of a v4 spoke — it fails safe to false (legacy
// symmetric) for every hive it cannot classify, so the 33 old hives are never
// broken. Positive signals, in order:
//
//  1. The explicit transitional allowlist (ssoEd25519HiveIDs) — currently the
//     auto-upgraded console hive.
//  2. A heartbeat-reported version/commit that indicates a v4 image. `entry` is
//     the hive's RegistryEntry (nil if the hub has no record yet, which stays
//     legacy). We treat the tracked v4 target commit 12c34e5 as the marker; the
//     hub records the short git hash the spoke reports (RegistryEntry.GitHash),
//     and the running image tag (RegistryEntry.ImageRef) is checked too so a hive
//     pinned to a v4 SHA/tag is caught even before a commit match.
//
// This is transitional: once every hive reports a classifiable v4 version the
// allowlist can be retired and this becomes purely version-driven.
func hiveWantsEd25519SSO(hiveID string, entry *RegistryEntry) bool {
	if ssoEd25519HiveIDs[hiveID] {
		return true
	}
	if entry == nil {
		return false
	}
	// ssoEd25519TargetCommit is the v4 all-fixes target the hub tracks. A hive
	// whose reported commit (or image tag) contains it is running a v4 build that
	// verifies SSO with Ed25519. Short-hash-safe: RegistryEntry.GitHash is stored
	// as a short SHA, and the tag may embed the same prefix.
	const ssoEd25519TargetCommit = "12c34e5"
	gh := strings.ToLower(strings.TrimSpace(entry.GitHash))
	// Match either direction so a stored short SHA and the (possibly longer)
	// target commit still compare equal on their common prefix.
	if gh != "" && (strings.HasPrefix(ssoEd25519TargetCommit, gh) || strings.HasPrefix(gh, ssoEd25519TargetCommit)) {
		return true
	}
	img := strings.ToLower(entry.ImageRef)
	if strings.Contains(img, ssoEd25519TargetCommit) {
		return true
	}
	// ssoEd25519ImageTag is the ROLLING v4 tag. The commit check above pins one
	// frozen build (12c34e5), so every v4 spoke that has since rolled forward —
	// which is all of them — stopped matching and silently fell back to the
	// legacy symmetric mint. A v4 spoke CANNOT verify that token, so the handoff
	// died with "sso: bad signature" and the user was bounced to the hub login.
	// Matching the tag itself is what actually tracks "this hive runs v4", and it
	// keeps working across rolls instead of expiring at the next image build.
	//
	// Still positive-only: an unclassifiable hive stays on the legacy symmetric
	// mint, so the v2 fleet is untouched.
	const ssoEd25519ImageTag = "v4-latest"
	return strings.Contains(img, ssoEd25519ImageTag)
}

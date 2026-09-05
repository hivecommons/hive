package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	infoHeartbeatKey   = "hive-heartbeat-v1"
	infoSessionKey     = "hive-session-v1"
	infoSSOKey         = "hive-sso-v1"
	infoImpersonateKey = "hive-impersonate-v1"

	infoInviteKey = "hive-invite-v1"

	infoSSOEd25519Seed = "hive-sso-ed25519-v1"

	infoSessionEd25519Seed = "hive-session-ed25519-v1"
)

func deriveDomainKey(master, info string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	return hex.EncodeToString(mac.Sum(nil))
}

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

func (s *HubServer) heartbeatKey() string {
	return deriveDomainKey(s.hubSecret, infoHeartbeatKey)
}

func (s *HubServer) sessionKey() string {
	return deriveDomainKey(s.hubSecret, infoSessionKey)
}

func (s *HubServer) ssoSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSSOEd25519Seed)
}

func (s *HubServer) impersonateKey() string {
	return deriveDomainKey(s.hubSecret, infoImpersonateKey)
}

func (s *HubServer) sessionSigningSeed() string {
	return deriveDomainKey(s.hubSecret, infoSessionEd25519Seed)
}

func (s *HubServer) sessionPublicKey() string {
	return ssoPublicKeyFromSeed(s.sessionSigningSeed())
}

func provisionSessionPublicKey() string {
	return ssoPublicKeyFromSeed(deriveDomainKey(provisionCurrentSecret(), infoSessionEd25519Seed))
}

func provisionTerminalKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoTerminalKey, hiveID)
}

func provisionHeartbeatKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoHeartbeatKey, hiveID)
}

func provisionInviteKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoInviteKey, hiveID)
}

func (s *HubServer) heartbeatKeyFor(hiveID string) string {
	return derivePerHiveKey(s.hubSecret, infoHeartbeatKey, hiveID)
}

func (s *HubServer) verifyHeartbeatBearer(presented, hiveID string) bool {
	_, ok := s.verifyHeartbeatBearerGeneration(presented, hiveID)
	return ok
}

// setHubSecret replaces the hub's master AND the generation set derived from it,
// keeping the two in lockstep.
//
// These are two representations of ONE fact, and once any verifier reads the
// generation set (this PR makes the heartbeat the second such verifier, after
// the impersonation cookie), assigning s.hubSecret alone leaves the hub deriving
// keys from a master it no longer believes it has. That drift is silent: the
// build is fine, the types are fine, and the only symptom is a 401.
//
// Setting a fresh master necessarily DISCARDS any previous generations — there
// is no basis on which to keep verifying against generations of a master that is
// being replaced wholesale rather than rotated. A real rotation goes through
// generationSet.rotate, which is what preserves the outgoing generation.
func (s *HubServer) setHubSecret(secret string) {
	if s == nil {
		return
	}
	s.hubSecret = secret
	s.keyGenerations = legacyGenerationSet(secret)
}

// verifyHeartbeatBearerGeneration is verifyHeartbeatBearer with the accepting
// master generation reported, so an operator can see whether any spoke is still
// authenticating with pre-rotation material rather than having to infer it.
//
// keyGenerations is nil only in hand-built test servers. Those fall back to the
// single-master path so this stays a pure addition of a second KEY rather than
// a change to what authenticates.
func (s *HubServer) verifyHeartbeatBearerGeneration(presented, hiveID string) (int, bool) {
	if s == nil || presented == "" {
		return 0, false
	}
	if s.keyGenerations != nil {
		return verifyHeartbeatBearerAcrossGenerations(s.keyGenerations, presented, hiveID, time.Now())
	}
	perHive := s.heartbeatKeyFor(hiveID)
	if perHive != "" && secureCompareHub(presented, perHive) {
		return legacyGenerationID, true
	}
	return 0, false
}

// verifyHeartbeatBearerAcrossGenerations is BOUNDED TRIAL VERIFICATION of the
// heartbeat bearer against every live master generation.
//
// WHY TRIAL AND NOT A MARKER. The bearer IS the derived key, presented raw in
// the Authorization header — there is no envelope to put a "g<N>." in. Its
// format is also a contract with already-deployed spoke Deployments
// (HIVE_HEARTBEAT_KEY) and with SpokeHeartbeatKey()'s self-derive lane, so
// changing the format would make the rotation mechanism itself require the
// fleet-wide flag day it exists to avoid. Trial verification is what the design
// prescribes for exactly this class of artifact.
//
// THE BOUND IS THE WHOLE ARGUMENT. maxLiveGenerations == 2, so the worst case is
// two HMAC computations where there was one — strictly less than the rest of a
// heartbeat already costs, and an attacker cannot use it as an asymmetric-cost
// lever because the count is capped by the loader rather than by anything the
// caller supplies.
//
// !! AUDIT F2 (Critical, open across five audits) MUST NOT REGRESS. !!
//
// F2 was closed by DELETING the fleet-wide lane — deriveDomainKey(master,
// infoHeartbeatKey), a value stamped byte-identically into every spoke, whose
// possession proved "some provisioned spoke" and never "THIS hive". Because
// handleHeartbeat trusts the body-supplied hive_id, that lane let any spoke beat
// as any victim and be handed the victim's key material.
//
// This function adds a second GENERATION, not a second LANE. Every candidate
// below is derivePerHiveKey(g.Secret, infoHeartbeatKey, hiveID) — identity-bound
// under EVERY generation, and derivePerHiveKey returns "" for an empty hiveID so
// an identity-less caller authenticates nothing no matter how many generations
// exist. There is deliberately no code path here that derives without hiveID; if
// one ever appears, F2 is re-opened.
//
// TIMING. Every attempt uses secureCompareHub (subtle.ConstantTimeCompare), and
// the loop deliberately does NOT return early on a match: it ORs the results and
// evaluates every acceptable generation regardless. Early return would leak,
// through response latency, WHICH generation accepted a bearer — which is a
// (small) oracle telling an attacker whether a given spoke has converged onto
// the new key, and therefore which spokes are still holding material derived
// from a master the operator is trying to retire. Two HMACs is cheap enough that
// there is no reason to buy anything with that leak.
//
// Returns the accepting generation ID, which is 0 when nothing accepted.
func verifyHeartbeatBearerAcrossGenerations(gs *generationSet, presented, hiveID string, now time.Time) (int, bool) {
	if presented == "" || hiveID == "" {
		return 0, false
	}
	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) == 0 {
		return 0, false
	}
	matched := 0
	for _, g := range acceptable {
		// F2: identity-bound under EVERY generation. Never deriveDomainKey.
		perHive := derivePerHiveKey(g.Secret, infoHeartbeatKey, hiveID)
		if perHive == "" {
			continue
		}
		// The compare is on its OWN line, unconditionally, so it runs for every
		// acceptable generation. Folding it into the `if` below as
		// `secureCompareHub(...) && matched == 0` would work today only because
		// Go evaluates the left operand first — a reordering during some future
		// tidy-up would silently reintroduce the early-exit timing leak. Keep
		// them separate.
		ok := secureCompareHub(presented, perHive)
		if ok && matched == 0 {
			matched = g.ID
		}
	}
	return matched, matched != 0
}

// heartbeatBearerIsPerHive reports whether the presented bearer is the
// identity-bound one. Retained after the F2 deletion because it still backs the
// live GET /api/saas/admin/auth-rollout telemetry (noteHeartbeatAuthPath →
// AuthRolloutStatus), which now serves a different purpose: rather than gating
// the deletion, it lets an operator SEE which hives are authenticating and
// confirm none regressed. Post-F2 a bearer that is not per-hive no longer
// verifies at all, so callers observe this only for bearers that already passed
// verifyHeartbeatBearer.
//
// ROTATION: this must consider every live generation for the same reason the
// verifier does. A spoke that has not yet been reconciled onto the new master
// presents a bearer derived from the PREVIOUS generation — still per-hive, still
// identity-bound, still accepted. Checking only the current generation would
// report it as "not per-hive" and make the auth-rollout surface show a fleet-wide
// F2 regression that has not happened, during exactly the window an operator is
// watching that surface most closely.
func (s *HubServer) heartbeatBearerIsPerHive(presented, hiveID string) bool {
	if s == nil {
		return false
	}
	if s.keyGenerations != nil {
		_, ok := verifyHeartbeatBearerAcrossGenerations(s.keyGenerations, presented, hiveID, time.Now())
		return ok
	}
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

// SpokeInviteKey returns the PER-HIVE key a spoke both mints and verifies
// contributor invite tokens with (dashboard.inviteSigningSecret).
//
// Resolution order mirrors TerminalSigningKey exactly, because the two keys have
// exactly the same shape — symmetric, spoke-local, minted and verified by the
// same process:
//
//  1. HIVE_INVITE_KEY — the hub-injected per-hive key (provisionInviteKey).
//  2. Self-derived per-hive key from HIVE_HUB_SECRET + HIVE_ID.
//
// Returns "" when neither resolves, so the caller keeps its existing fail-closed
// fallback (a persisted per-instance random secret) rather than signing with an
// empty key.
//
// WHY THIS EXISTS. inviteSigningSecret's lane 2 used the RAW MASTER as the HMAC
// key — not a key derived from it, the master itself. Measured on the live fleet
// that is the lane in use on 65/65 spokes, because HIVE_INVITE_KEY is emitted by
// the provisioning template but is NOT carried by the reconcile sweep, so no live
// spoke has ever received it. Two consequences, both fixed by routing through
// here:
//
//   - The master is fleet-uniform, so every spoke signed invites with the SAME
//     key: an invite minted on one tenant verified on every other, defeating the
//     per-hive binding provisionInviteKey was introduced to provide.
//   - Using the master directly as an HMAC key gives the invite lane no domain
//     separation at all — a signing oracle over attacker-influenced input keyed
//     by the master that also protects heartbeats, sessions and SSO.
//
// ROTATION (master-key-rotation.md PR #5): as with the terminal key there is no
// trial verification and none is possible — a spoke holds one master, never a
// generation set. Rotation re-keys invites once; in-flight invite links become
// invalid, which the invite flow already tolerates (verifyInviteToken returning
// "" means "no attribution", never an error).
func SpokeInviteKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvInviteKey)); v != "" {
		return v
	}
	return derivePerHiveKey(
		strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")),
		infoInviteKey,
		strings.TrimSpace(os.Getenv(EnvHiveID)),
	)
}

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
// hubSecretPath (/data/saas/hub-secret.key in production). Used ONLY to derive
// the per-domain sub-keys that get injected into a spoke — the master itself is
// never placed in the Deployment.
func provisionMasterSecret() string {
	if s := strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")); s != "" {
		return s
	}
	if data, err := os.ReadFile(hubSecretPath); err == nil {
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
// AUDIT 8 / F19 — THIS FUNCTION USED TO BE INERT.
//
// It previously read:
//
//	if gs := provisionGenerationsOverride; gs != nil { return gs }
//	return legacyGenerationSet(provisionMasterSecret())
//
// and provisionGenerationsOverride was written by NOTHING but a test helper. So
// in production it unconditionally returned generation 1 built from the raw
// /data/saas/hub-secret.key — a file rotateMasterSecret never rewrites. The
// hub's MINTING and VERIFYING side used the real persisted set
// (s.keyGenerations); this, the entire SPOKE-BOUND derivation side, used a set
// nothing could ever advance. The two were disjoint, so a rotation changed what
// the hub minted but never what the fleet held: the sweep computed "desired"
// from generation 1, saw no drift against what spokes already had, and patched
// nothing. Every one of the seven rotation PRs was dead on arrival.
//
// THE FIX is the package-level pointer below, installed by NewHubServer from
// the same load that populates s.keyGenerations. Now one set feeds both sides
// and a rotation moves them together.
//
// THREE STATES, and the third is the F20 handoff:
//
//   - INSTALLED (liveGenerations non-nil): return the hub's live set. After a
//     rotation this is the new current generation, which is the whole point.
//
//   - NOT INSTALLED (liveGenerations nil, liveGenerationsUntrusted false): no
//     HubServer has been constructed in this process. This is the state of
//     every unit test that derives keys without building a hub, and of any
//     provisioning helper invoked before NewHubServer runs. Fall back to
//     legacyGenerationSet(provisionMasterSecret()) — byte-identical to the
//     pre-F19 behaviour, which is what keeps this a no-op on an un-rotated hub.
//
//   - UNTRUSTED (liveGenerationsUntrusted true): NewHubServer could not
//     establish which generation is current (F20's generationsUntrusted). REFUSE
//     — return nil, do not re-derive our own notion from the raw master file.
//     Falling back here would be the exact silent un-rotation F20 fixed on the
//     minting side, reintroduced on the derivation side: the master file holds
//     SUPERSEDED generation-1 material once a rotation has happened, and writing
//     keys derived from it onto live Deployments would push the whole fleet
//     BACKWARDS onto a retired generation. nil propagates as "" through
//     currentSecret(), which desiredPerHiveEnv's empty-master guard already
//     turns into "skip this hive, patch nothing".
//
// Returns nil for an empty master, and currentSecret() on a nil set returns ""
// — so the fail-closed contract every caller already relies on (deriveDomainKey
// and derivePerHiveKey both return "" for an empty master) is preserved through
// the nil case rather than needing a new one.
func provisionGenerationSet() *generationSet {
	// The test seam still wins, so the ROTATED case stays drivable without
	// standing up a HubServer. See provisionGenerationsOverride.
	if gs := provisionGenerationsOverride; gs != nil {
		return gs
	}
	liveGenerationsMu.RLock()
	gs, untrusted := liveGenerations, liveGenerationsUntrusted
	liveGenerationsMu.RUnlock()
	if untrusted {
		// FAIL CLOSED. Never re-derive from the raw master here — see above.
		return nil
	}
	if gs != nil && generationSetDescendsFrom(gs, provisionMasterSecret()) {
		return gs
	}
	return legacyGenerationSet(provisionMasterSecret())
}

// generationSetDescendsFrom reports whether gs is a set this process's ambient
// master could legitimately have produced — i.e. whether SOME generation in it
// holds that master.
//
// WHY THE INSTALLED SET IS NOT UNCONDITIONALLY AUTHORITATIVE. liveGenerations is
// process-global, but provisionMasterSecret() is re-read from the environment
// (HIVE_HUB_SECRET) or the file on every call. Those can disagree, and when they
// do the installed set is STALE — it descends from a master this process is no
// longer configured with. Deriving from a stale set would hand spokes keys that
// verify against nothing.
//
// The invariant that makes the check sound: rotation only ever ADDS a
// generation and demotes the outgoing one (generationSet.rotate carries the
// previous current forward), so on a correctly-installed hub the ambient master
// is still present in the set as either the current or the previous generation.
// A set that contains it is therefore a legitimate descendant; one that does not
// belongs to a different master entirely.
//
// This is what keeps the un-rotated fallback honest and is load-bearing for
// TestPerHiveEnvGenerationIsAReadPathChangeToday and friends: a test that builds
// a HubServer pins the global to that hub's master, and a LATER test using a
// different master must not silently derive from the earlier one. In production
// it covers the same shape — a second NewHubServer, or an operator changing
// HIVE_HUB_SECRET without the generations file following.
//
// Returns false for a nil set or an empty master so both fall through to the
// legacy path, which is itself nil-for-empty. Comparison is a plain string
// equality on secrets already held in memory by this process; it is not a
// verification of attacker-supplied input, so constant time is not required.
func generationSetDescendsFrom(gs *generationSet, master string) bool {
	if gs == nil || master == "" {
		return false
	}
	for _, g := range gs.Generations {
		if g.Secret == master {
			return true
		}
	}
	return false
}

// liveGenerations is the hub's live generation set as seen by the spoke-bound
// derivation path, installed by NewHubServer and replaced by rotateMasterSecret.
//
// WHY A PACKAGE-LEVEL POINTER AND NOT A RECEIVER. Every caller in the
// derivation chain — desiredPerHiveEnv, provisionHeartbeatKey,
// provisionTerminalKey, provisionInviteKey, provisionSessionPublicKey, and
// saas_provision.go's template map — is a free function, and several are called
// from template expansion that has no HubServer in scope. Threading a receiver
// through all of them is the larger refactor; the audit explicitly sanctions
// either shape. This is the smaller change and it makes the two sides share one
// object rather than two copies that can drift.
//
// A generation set is only ever REPLACED, never mutated (see
// currentGenerations), so a reader that takes this pointer holds a
// self-consistent snapshot for as long as it needs it.
var (
	liveGenerationsMu sync.RWMutex
	liveGenerations   *generationSet
	// liveGenerationsUntrusted latches the F20 generationsUntrusted outcome.
	//
	// It is a SEPARATE flag rather than a sentinel value of liveGenerations
	// because nil has to keep meaning "no hub in this process" for the unit
	// tests that never build one. Collapsing "untrusted" into "nil" would make
	// every such test take the refuse path and derive nothing.
	liveGenerationsUntrusted bool
)

// setLiveGenerations installs the set the spoke-bound derivation path reads.
//
// Called by NewHubServer with the loaded set, and by rotateMasterSecret with
// the newly rotated one — the latter is what makes a rotation actually reach
// the fleet, which is the F19 fix. untrusted latches the fail-closed state and
// is only ever set from the generationsUntrusted branch.
func setLiveGenerations(gs *generationSet, untrusted bool) {
	liveGenerationsMu.Lock()
	defer liveGenerationsMu.Unlock()
	liveGenerations = gs
	liveGenerationsUntrusted = untrusted
}

// provisionGenerationsOverride replaces the resolved generation set.
//
// RETAINED DELIBERATELY after F19 wired provisionGenerationSet() to the live
// set. It is still the only way to drive the derivation path from an explicit
// set without constructing a whole HubServer, and the tests that use it
// (perhive_env_generation_test.go) are the ones that distinguish "derives from
// the CURRENT generation" from "derives from the raw master" — the exact
// test-that-passes-for-the-wrong-reason this seam prevents. Removing it would
// delete that coverage, so it stays.
//
// nil in production, set only via withProvisionGenerations in tests. Read
// without a lock because it is only ever written before the code under test
// runs, from that test's own goroutine.
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

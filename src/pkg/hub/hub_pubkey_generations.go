package hub

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strings"
	"time"
)

// SSO / SESSION Ed25519 PUBLIC KEY plurality — follow-on PR #6 of the
// master-key rotation design (src/docs/design/master-key-rotation.md).
//
// WHY THIS ONE IS SEQUENCED LAST, AND WHY IT IS A DIFFERENT SHAPE FROM #1-#5.
// Every earlier follow-on made a HUB-side verifier accept two generations. The
// hub holds the whole generation set, so "accept both" was a local change: read
// one more entry out of a struct the hub already has.
//
// This one is not that. The SSO handoff token and the hub session cookie are
// MINTED ON THE HUB and VERIFIED ON THE SPOKE — by Go in
// pkg/dashboard/api.go (SSO) and, independently, by Node in src/proxy/server.js
// (session cookie). The verifying party does not hold the generation set, does
// not hold any master, and by design holds no private material at all. It holds
// ONE hex Ed25519 public key that the hub put in its Deployment env.
//
// So the failure this PR exists to prevent is not "the hub rejects an old
// artifact". It is the mirror image: the hub, the instant it rotates, starts
// minting under generation N, and ~65 spokes are still holding generation N-1's
// PUBLIC key. Those spokes cannot verify anything the hub now mints. Every
// hosted SSO handoff 401s and every hosted terminal session fails its cookie
// check — not for the 30 minutes an impersonation cookie lives, but for the
// ~6 hours the rate-limited reconcile lane takes to walk the fleet at 3 patches
// per 15-minute cycle. That is the flag day, relocated from the hub to the
// fleet.
//
// THE FIX IS THEREFORE PLURALITY ON THE SPOKE, NOT SELECTION ON THE HUB. A
// spoke must hold BOTH live generations' public keys and try each. Which puts
// this squarely in the design doc's "CANNOT carry a marker" bucket —
//
//	| SSO/session PUBLIC keys | A hex Ed25519 public key has a fixed 32-byte form. |
//
// — so the mechanism is BOUNDED TRIAL VERIFICATION against the live
// generations, current-then-previous, bounded by maxLiveGenerations == 2. Worst
// case is two Ed25519 verifications on a path that already does one.
//
// ORDERING — THE PROPERTY THE WHOLE PR TURNS ON.
//
// The VERIFIER must be able to accept two public keys BEFORE the hub ever hands
// out a second one. A spoke whose proxy understands only one key, handed a
// second, is not merely un-improved — depending on the env contract it can be
// actively broken (see EnvSSOPublicKeyPrevious below for the delimited-list
// analysis that ruled that encoding out). So this PR ships ONLY the verifier
// plurality and the provisioning of the second var. It does not, and cannot,
// cause a rotation: rotation is triggered by the admin endpoint from #4, and
// until an operator calls it there is exactly ONE generation.
//
// WITH ONE GENERATION THIS IS A NO-OP, BY CONSTRUCTION. previousPublicKeys
// returns an empty slice whenever acceptableGenerations yields only the current
// generation, which is the state of every hub in the fleet today and the state
// of every hub that has never been rotated. An empty previous list means:
//   - desiredPerHiveEnv emits NO second var, so perHiveEnvDrift sees no drift,
//     so NO spoke is patched and NO pod rolls;
//   - the Go and Node verifiers evaluate a zero-length candidate list after the
//     primary key and behave byte-identically to today.
//
// The second var appears on a spoke for the first time only after a rotation,
// and disappears again — via the same drift-and-patch lane — once the previous
// generation expires out of acceptableGenerations. It is self-clearing, which
// is what stops it becoming the unversioned permanent compat lane that F1/F2
// were.

const (
	// EnvSSOPublicKeyPrevious carries the PREVIOUS generation's Ed25519 public
	// key for SSO handoff verification, alongside (never instead of)
	// EnvSSOPublicKey.
	//
	// WHY A SECOND VAR AND NOT A DELIMITED LIST IN THE EXISTING VAR. A list was
	// the tempting option — it touches no provisioning surface and no reconcile
	// list. It is wrong, and the reason is measurable rather than aesthetic.
	//
	// The value in HIVE_SSO_PUBLIC_KEY is read by TWO independently written
	// verifiers, and during the ~6h reconcile walk some spokes are running the
	// OLD image. Feed "<hex>,<hex>" to each of them:
	//
	//   Node  Buffer.from(v,'hex')      -> 32 bytes. Silently truncates at the
	//                                      first non-hex byte, yielding key #1.
	//                                      Keeps working, by accident.
	//   Go    hex.DecodeString(v)       -> error "invalid byte: U+002C ','".
	//                                      SpokeSSOPublicKey / VerifySSOToken
	//                                      fail closed. SSO DEAD on that spoke.
	//
	// The two languages disagree about what a delimited list means, and the
	// disagreement is exactly "does hosted SSO still work on an un-rolled
	// spoke". A list would therefore break SSO on every spoke the reconcile
	// lane has patched but that has not yet rolled the new image — which is the
	// entire fleet for the first cycle, and a shrinking slice of it for six
	// hours. Backward compatibility here is mandatory, so the encoding that has
	// a defined meaning to old readers wins: an ADDITIONAL var, which old code
	// does not read at all and is consequently unable to misread.
	//
	// A separate var is also the only encoding that lets a spoke's key set
	// SHRINK cleanly. Retirement removes a var, which both readers already
	// handle (absent == empty == no candidate); shrinking a list would require
	// old readers to re-parse a value they are truncating anyway.
	EnvSSOPublicKeyPrevious = "HIVE_SSO_PUBLIC_KEY_PREV"

	// envSessionPublicKeyPrevious is the session-cookie mirror of
	// EnvSSOPublicKeyPrevious. Unexported for the same reason
	// envSessionPublicKey is: only the Node proxy reads it, so there is no Go
	// caller to export it for, and naming it here keeps the reconcile check and
	// the reconcile patch from ever disagreeing about the spelling.
	envSessionPublicKeyPrevious = "HIVE_SESSION_PUBLIC_KEY_PREV"
)

// errSSONoVerificationKey and errSSORejected are the two outcomes
// VerifySSOTokenAcrossKeys can report.
//
// They are deliberately COARSE. VerifySSOToken distinguishes bad-signature from
// wrong-version from expired from wrong-hive, which is useful in a hub-side log
// but must not become richer here: with several keys tried, a per-key error
// would tell an unauthenticated caller WHICH generation rejected its token and
// therefore whether this spoke has converged. The caller in pkg/dashboard
// already collapses the detailed error into one 401 for the user and logs the
// detail; this returns only "no key configured" (an operator-facing
// configuration fault, already surfaced as a distinct 503) versus "rejected".
var (
	errSSONoVerificationKey = ssoError("sso: no verification key configured")
	errSSORejected          = ssoError("sso: handoff token rejected")
)

// ssoError is a constant-friendly error type, so the two sentinels above can be
// compared with errors.Is by identity without a package-level init.
type ssoError string

func (e ssoError) Error() string { return string(e) }

// ssoPublicKeyForGeneration returns the hex Ed25519 PUBLIC key a verifier checks
// one generation's SSO handoff tokens against. Mirror of
// sessionPublicKeyForGeneration (hub_cookie_generations.go) for the SSO domain.
//
// PRIVATE MATERIAL NEVER LEAVES: the seed produced here is expanded and
// DISCARDED inside ssoPublicKeyFromSeed, which returns only the public half.
// infoSSOEd25519Seed and infoSessionEd25519Seed derive SEEDS — hub-only signing
// material — and hub_keys.go's ssoSigningSeed/sessionSigningSeed doc comments
// state that those must never be injected into a spoke. Nothing in this file
// returns a seed to a caller, and nothing in this file is reachable from a
// provisioning path except through ssoPublicKeyFromSeed.
func ssoPublicKeyForGeneration(g keyGeneration) string {
	return ssoPublicKeyFromSeed(deriveDomainKey(g.Secret, infoSSOEd25519Seed))
}

// previousPublicKeys returns the PUBLIC keys of every acceptable generation
// EXCEPT the current one, in most-recent-first order, for the domain named by
// `info`.
//
// Empty for a set with one generation — which is every hub today. That empty
// return is what makes this whole PR a no-op before the first rotation: the
// callers below all treat an empty list as "emit nothing / try nothing".
//
// Expiry binds here and nowhere else: acceptableGenerations excludes a previous
// generation whose VerifyUntil has passed (and treats a MISSING VerifyUntil as
// already expired, so a hand-edited generations file fails closed). Once the
// window shuts, this returns empty again and the reconcile lane removes the var
// from the fleet — the acceptance window closes on a wall clock, with no
// operator action, which is the finiteness property the design demands.
func previousPublicKeys(gs *generationSet, info string, now time.Time) []string {
	if gs == nil {
		return nil
	}
	acceptable := gs.acceptableGenerations(now)
	out := make([]string, 0, len(acceptable))
	for _, g := range acceptable {
		if g.ID == gs.Current {
			continue
		}
		pub := ssoPublicKeyFromSeed(deriveDomainKey(g.Secret, info))
		if pub == "" {
			// A generation whose seed does not expand yields no candidate rather
			// than an empty-string one. An empty key must never reach a
			// Deployment: the spoke-side resolvers treat an empty var exactly
			// like an absent one, so writing "" would look converged while
			// verifying nothing.
			continue
		}
		out = append(out, pub)
	}
	return out
}

// provisionSSOPublicKeyPrevious and provisionSessionPublicKeyPrevious are the
// provisioning/reconcile-time values for the two _PREV vars.
//
// They return "" when there is no previous generation. The callers
// (desiredPerHiveEnv) must then OMIT the var entirely rather than set it empty —
// see desiredPerHiveEnv's own fail-safe comment for why an empty var is strictly
// worse than an absent one.
//
// Only the FIRST previous generation is offered even though previousPublicKeys
// returns a slice. maxLiveGenerations == 2 means the slice can never hold more
// than one entry today; taking [0] rather than joining keeps the env contract
// single-valued per var, which is the property that made a second var the right
// encoding in the first place. If maxLiveGenerations were ever raised, this
// would need a third var — deliberately a visible code change rather than a
// silently-lengthening string.
// SCOPE NOTE — the provisioning TEMPLATE is deliberately not changed.
//
// saas_provision.go's Deployment YAML emits the five mandatory vars from a
// text/template with unconditional `- name: X / value: "{{.Y}}"` blocks. The
// two _PREV vars are EMPTY in production and empty on every un-rotated hub, so
// adding them there would mean either emitting `value: ""` on all 65 spokes —
// the exact empty-var outcome desiredPerHiveEnv's fail-safe exists to prevent —
// or introducing conditional blocks into a YAML template whose whitespace and
// quoting are load-bearing, to cover a case that is unreachable today.
//
// The reconcile lane covers it instead, and covers it correctly: a hive
// provisioned during a rotation window is read by the very next sweep, drifts
// on the missing _PREV, and is patched within one 15-minute cycle. The cost is
// bounded — a freshly provisioned spoke can fail SSO for up to one cycle if the
// hub happens to be mid-rotation — and it is strictly smaller than the cost of
// a template edit that ships an empty var to the whole fleet immediately.
func provisionSSOPublicKeyPrevious() string {
	return firstOrEmpty(previousPublicKeys(provisionGenerationSet(), infoSSOEd25519Seed, time.Now()))
}

func provisionSessionPublicKeyPrevious() string {
	return firstOrEmpty(previousPublicKeys(provisionGenerationSet(), infoSessionEd25519Seed, time.Now()))
}

func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// SpokeSSOPublicKeys returns EVERY Ed25519 public key this spoke should try when
// verifying a hub-minted SSO handoff token, in current-then-previous order.
//
// This is the spoke-side plural form of SpokeSSOPublicKey, which is retained
// unchanged and still returns the single primary key. Retaining it matters: it
// is the value the /sso handler reports on in its "no verification key
// configured" branch, and callers that legitimately want "the one key the hub
// is minting with" should not be silently handed a list.
//
// BACKWARD COMPATIBILITY, WHICH IS THE POINT. HIVE_SSO_PUBLIC_KEY_PREV is
// absent on all 65 spokes today and stays absent until an operator rotates. An
// absent var yields a one-element result that is byte-identical to what
// SpokeSSOPublicKey returns, so a spoke running this code with today's env
// verifies exactly what it verifies now, in exactly one Ed25519 operation. The
// legacy single-value configuration is not a special case here; it is the
// zero-previous case of the general one.
//
// The master-derived fallback (self-hosted spokes holding HIVE_HUB_SECRET) is
// inherited from SpokeSSOPublicKey and deliberately NOT extended to the
// previous generation: a spoke cannot know the hub's generation set, and a
// self-hosted operator rotating their own master is rotating the thing the
// fallback derives from, so there is nothing to derive a previous key from.
//
// FAIL CLOSED. Empty and malformed entries are dropped by validation, not
// passed through. A malformed _PREV must not be able to affect the primary key
// — see verifySSOTokenAcrossKeys.
func SpokeSSOPublicKeys() []string {
	return appendDistinctPublicKey(
		validPublicKeys(SpokeSSOPublicKey()),
		strings.TrimSpace(os.Getenv(EnvSSOPublicKeyPrevious)),
	)
}

// validPublicKeys filters a candidate list down to well-formed hex Ed25519
// public keys.
//
// A key that is empty, not hex, or not exactly ed25519.PublicKeySize bytes is
// DROPPED rather than passed to the verifier. Dropping matters more than it
// looks: crypto/ed25519.Verify PANICS on a public key of the wrong length, so
// handing an unvalidated string to a verify loop turns a malformed env var into
// a crash of the spoke's dashboard rather than a failed verification. Node's
// crypto.createPublicKey throws for the same input, which server.js already
// catches — the two sides are made to agree here on the stricter behaviour.
func validPublicKeys(keys ...string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		out = append(out, k)
	}
	return out
}

// appendDistinctPublicKey adds `candidate` to `keys` when it is well-formed and
// not already present.
//
// The de-duplication is not tidiness. Before the first rotation, and again
// briefly after a var is written but before the generation actually differs, the
// previous key can equal the current one. Appending it anyway would double the
// verification work on the hottest hosted path for no benefit, and would make
// "how many keys is this spoke trying" a misleading number on the readiness
// surface. The comparison is a plain string compare on PUBLIC, non-secret
// values, so constant-time is not required here — and deliberately so: see
// verifySSOTokenAcrossKeys for where constant-time DOES bind.
func appendDistinctPublicKey(keys []string, candidate string) []string {
	valid := validPublicKeys(candidate)
	if len(valid) == 0 {
		return keys
	}
	for _, existing := range keys {
		if existing == valid[0] {
			return keys
		}
	}
	return append(keys, valid[0])
}

// VerifySSOTokenAcrossKeys is bounded trial verification of an SSO handoff token
// against every public key a spoke holds, current-then-previous.
//
// WHY THE SPOKE TRIES BOTH RATHER THAN THE HUB PICKING ONE. The hub cannot pick.
// It mints under the current generation and has no way to know, at mint time,
// whether the spoke the user is being handed off to has been reconciled yet —
// the reconcile lane walks the fleet over ~6h and the hub does not gate the SSO
// button on convergence. Minting under the PREVIOUS generation instead would be
// worse in both directions: it would break already-converged spokes, and it
// would mean the rotation never converges, which is precisely how a compat lane
// becomes permanent.
//
// TIMING / NON-DISCLOSURE. The loop does NOT return early. It evaluates every
// candidate key and records the first index that verified, so the number of
// Ed25519 operations performed is a function of how many keys the spoke holds
// and never of WHICH key matched. Returning on first success would leak, through
// response latency, whether this spoke has been reconciled onto the new
// generation — a (small) oracle mapping the fleet's convergence state, offered
// on an unauthenticated endpoint, during exactly the window an operator is
// mid-rotation. This mirrors verifyHeartbeatBearerAcrossGenerations, which
// declines the same early exit for the same reason and documents why the compare
// must stay on its own line.
//
// The signature check itself is ed25519.Verify, which is constant-time in the
// signature and safe on attacker-controlled input; nothing here compares
// secret-derived material with ==, because nothing here is secret — these are
// public keys. What is protected is the CONVERGENCE STATE, and it is protected
// by uniform work rather than by comparison discipline.
//
// FAIL CLOSED, AND A MALFORMED SECOND KEY CANNOT DISABLE THE FIRST. `keys` is
// expected to have come from SpokeSSOPublicKeys / validPublicKeys, which drop
// malformed entries; this function additionally treats an individual failed
// verification as just that — a failed candidate — rather than an error that
// aborts the loop. So a garbage HIVE_SSO_PUBLIC_KEY_PREV costs one wasted
// candidate and changes nothing about whether the primary key verifies. An
// empty list verifies nothing at all.
//
// Returns (username, role, index of the accepting key, nil) on success.
func VerifySSOTokenAcrossKeys(keys []string, token, expectedHiveID string, now time.Time) (username, role string, keyIndex int, err error) {
	valid := validPublicKeys(keys...)
	if len(valid) == 0 {
		return "", "", -1, errSSONoVerificationKey
	}
	matchedIndex := -1
	var matchedUser, matchedRole string
	for i, k := range valid {
		u, r, verr := VerifySSOToken(k, token, expectedHiveID, now)
		// Recorded unconditionally, and the loop deliberately continues. See the
		// TIMING note above: an early return here would make the spoke's
		// convergence state observable as latency.
		if verr == nil && matchedIndex == -1 {
			matchedIndex, matchedUser, matchedRole = i, u, r
		}
	}
	if matchedIndex == -1 {
		return "", "", -1, errSSORejected
	}
	return matchedUser, matchedRole, matchedIndex, nil
}

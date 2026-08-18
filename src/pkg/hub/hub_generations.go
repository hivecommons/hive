package hub

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Master-secret GENERATIONS — the mechanism that makes rotation possible.
//
// THE PROBLEM. Every piece of signing material on the platform is a pure
// function of ONE value, the hub master secret, with no marker recording which
// master produced it (hub_keys.go: heartbeat, session, session-Ed25519, SSO,
// impersonate, terminal, invite). The "-v1" suffixes on the info labels version
// the DOMAIN, not the key. So changing the master changes all seven derived
// values at once, and no verifier can accept both the outgoing and the incoming
// form. A rotation is therefore a fleet-wide flag day: every browser session
// invalidated, every heartbeat 401'd, every SSO handoff unverifiable, across ~66
// hosted spokes, simultaneously. Which is why the master has never been rotated
// and, as the code stands, cannot be.
//
// THE SHAPE OF THE FIX. The hub holds an ORDERED SET of generations rather than
// a single secret. Exactly one is CURRENT — the only one that MINTS — and zero
// or more are PREVIOUS, accepted for VERIFY only and each carrying an explicit
// expiry. Rotation promotes a new current and demotes the outgoing one, so
// artifacts minted before the rotation keep verifying while artifacts minted
// after it use the new material. Convergence of spoke-held material then happens
// through the existing rate-limited reconcile lane (perhive_env_reconcile.go),
// not through a re-provision.
//
// WHY THIS IS NOT THE "VERIFY-BOTH" LANE THAT TOOK FIVE AUDITS TO REMOVE. The
// F1/F2 lanes were verify-both lanes too, and they were a real vulnerability for
// years. What made them so was not that they accepted two credentials — it was
// that they were UNVERSIONED and PERMANENT. Nothing in the code named which
// alternative was the legacy one, and nothing said when it stopped being
// accepted, so the only mechanism that could ever end them was an audit finding.
// Both properties are fixed here deliberately:
//
//   - VERSIONED. A previous generation is a numbered entry in a list, not an
//     unnamed `if` branch. "Which key accepted this request" is a value the code
//     returns (see verifyWithGenerations) and the telemetry can count, so the
//     question "is anything still using the old key?" has an answer that does not
//     require reading the code.
//   - FINITE. Every previous generation carries VerifyUntil. An expired
//     generation is not accepted, full stop — the window closes on a wall clock
//     whether or not anyone remembers to close it. This is the property the F1/F2
//     lanes lacked entirely, and it is why this one cannot rot into permanence
//     the way those did.
//
// DERIVATION IS UNCHANGED. Each generation's secret feeds deriveDomainKey and
// derivePerHiveKey exactly as the single master does today. No domain's key
// FORMAT changes, which is what lets a rotation happen without the Node proxy or
// any spoke needing to understand generations at all: a spoke handed a
// HIVE_SESSION_KEY derived from generation 3 just has a different string in its
// env than it had before, and every existing code path treats it identically.

// keyGeneration is one master secret plus the metadata that says what may be
// done with it and until when.
type keyGeneration struct {
	// ID names this generation. Small monotonically increasing integer. NOT
	// secret — it appears in cookie values and log lines, which is fine: it
	// names a key, it is not a key.
	ID int `json:"id"`
	// Secret is the master for this generation. Never logged, never returned by
	// any handler, never placed in a Deployment.
	Secret string `json:"secret"`
	// Created records when this generation was minted, for operator forensics.
	Created time.Time `json:"created,omitempty"`
	// VerifyUntil is the hard expiry of this generation's VERIFY acceptance. It
	// is meaningful only for PREVIOUS generations; the current generation is
	// always accepted and carries the zero value.
	//
	// This field is the whole reason this design does not become the permanent
	// compat lane it replaces. See the file comment.
	VerifyUntil time.Time `json:"verify_until,omitempty"`
}

const (
	// legacyGenerationID is the generation number assigned to the pre-rotation
	// master — the raw contents of /data/saas/hub-secret.key. Every hub in the
	// fleet is on this generation today.
	//
	// It starts at 1 rather than 0 so that "generation 0" can never be confused
	// with the zero value of an int: an artifact whose parsed marker is 0 is
	// malformed, not "generation zero".
	legacyGenerationID = 1

	// maxLiveGenerations caps how many generations may be live (current plus
	// previous) at once.
	//
	// This bound is what makes trial verification acceptable for the artifacts
	// that CANNOT carry a generation marker — the heartbeat bearer above all,
	// which is a bare derived string presented raw in an Authorization header
	// with no envelope to put a marker in. Verifying such an artifact means
	// deriving and comparing once per live generation, so an unbounded set would
	// turn every heartbeat into unbounded HMAC work and hand an attacker a cheap
	// asymmetric cost. At 2, the worst case is two HMAC computations where there
	// was one — less than the rest of a heartbeat already costs.
	//
	// 2 is also the smallest value that permits rotation at all (you need the
	// incoming and the outgoing one simultaneously), so it is deliberately the
	// minimum that works rather than a round number. Raising it would allow
	// stacking rotations before the previous has converged, which is a
	// misfeature: it lets an operator start a second rotation while the first is
	// still mid-flight and lose track of which spokes are on which key.
	maxLiveGenerations = 2

	// defaultVerifyWindow is how long a demoted generation stays acceptable for
	// VERIFY after a rotation.
	//
	// Set to match cookieMaxAgeDays (7 days), because the longest-lived artifact
	// bound to a generation is a session cookie: any shorter and a rotation logs
	// out users whose cookies were minted just before it, which is the flag-day
	// symptom this whole mechanism exists to avoid. Any longer and the old key
	// stays live past the point where it protects anything, which is the F1/F2
	// failure mode.
	defaultVerifyWindow = 7 * 24 * time.Hour
)

// generationSet is the hub's ordered set of master generations.
//
// Invariants, all established by newGenerationSet and relied on by every method:
//   - exactly one generation is current
//   - the current generation is first in Generations
//   - previous generations follow, most recent first
//   - len(Generations) <= maxLiveGenerations
type generationSet struct {
	// Current is the ID of the generation that MINTS. Exactly one.
	Current int `json:"current"`
	// Generations holds the live set, current first.
	Generations []keyGeneration `json:"generations"`
}

// newGenerationSet builds a validated set from an unordered slice, establishing
// every invariant above. Returns nil if no generation matches currentID — a set
// with no minting key is not a degraded set, it is an unusable one, and callers
// must fall back to the legacy single-master path rather than run with it.
func newGenerationSet(currentID int, gens []keyGeneration) *generationSet {
	var current *keyGeneration
	var previous []keyGeneration
	for i := range gens {
		g := gens[i]
		if strings.TrimSpace(g.Secret) == "" {
			// A generation with no secret derives every key to "" (see
			// deriveDomainKey's fail-closed contract). Admitting one would make
			// verification silently compare against empty keys, so drop it.
			continue
		}
		if g.ID == currentID {
			c := g
			current = &c
			continue
		}
		previous = append(previous, g)
	}
	if current == nil {
		return nil
	}
	// Most recent previous generation first, so verification tries the
	// likeliest match before the older ones.
	sort.Slice(previous, func(i, j int) bool { return previous[i].ID > previous[j].ID })

	ordered := make([]keyGeneration, 0, maxLiveGenerations)
	ordered = append(ordered, *current)
	for _, p := range previous {
		if len(ordered) >= maxLiveGenerations {
			break
		}
		ordered = append(ordered, p)
	}
	return &generationSet{Current: current.ID, Generations: ordered}
}

// legacyGenerationSet wraps a single pre-rotation master as generation 1.
//
// This is the state of EVERY hub in the fleet today, and the reason there is no
// migration step: a hub that has never rotated has exactly one generation, so
// dual acceptance degenerates to single acceptance and every derived key is
// byte-identical to what the same master produces now. "Never rotated" and
// "rotated back down to one generation" are therefore the same state, which
// means rollback needs no special handling either.
func legacyGenerationSet(master string) *generationSet {
	if strings.TrimSpace(master) == "" {
		return nil
	}
	return newGenerationSet(legacyGenerationID, []keyGeneration{
		{ID: legacyGenerationID, Secret: master},
	})
}

// currentGeneration returns the generation that MINTS. Returns (zero, false)
// for a nil or empty set so minting stays fail-closed: no key configured must
// mean "cannot mint", never "mint unsigned".
func (gs *generationSet) currentGeneration() (keyGeneration, bool) {
	if gs == nil || len(gs.Generations) == 0 {
		return keyGeneration{}, false
	}
	return gs.Generations[0], true
}

// currentSecret returns the minting master, or "" when there is none. The ""
// flows into deriveDomainKey, which returns "" for an empty master, so an
// unconfigured hub fails closed through the existing contract rather than
// needing a new one at every call site.
func (gs *generationSet) currentSecret() string {
	g, ok := gs.currentGeneration()
	if !ok {
		return ""
	}
	return g.Secret
}

// acceptableGenerations returns the generations a VERIFIER may consider at
// time `now`, current first.
//
// This is where the finiteness promise is actually kept. A previous generation
// whose VerifyUntil has passed is EXCLUDED — not warned about, not logged and
// accepted anyway, but excluded — so the acceptance window closes on a wall
// clock with no operator action. The current generation has no VerifyUntil and
// is always included.
func (gs *generationSet) acceptableGenerations(now time.Time) []keyGeneration {
	if gs == nil {
		return nil
	}
	out := make([]keyGeneration, 0, len(gs.Generations))
	for _, g := range gs.Generations {
		if g.ID == gs.Current {
			out = append(out, g)
			continue
		}
		if g.VerifyUntil.IsZero() || !now.Before(g.VerifyUntil) {
			// A previous generation with no expiry is exactly the unbounded
			// compat lane this design exists to prevent, so treat a missing
			// expiry as ALREADY EXPIRED rather than as "never expires". A
			// malformed or hand-edited generations file therefore fails closed.
			continue
		}
		out = append(out, g)
	}
	return out
}

// rotate returns a NEW set with newSecret as the incoming current generation
// and the outgoing current demoted to verify-only until now+window.
//
// Pure: it does not mutate the receiver, so a caller that fails to persist the
// result has not already changed the hub's in-memory state. Returns nil for an
// empty secret — rotating TO no key would silently disable minting.
func (gs *generationSet) rotate(newSecret string, now time.Time, window time.Duration) *generationSet {
	if strings.TrimSpace(newSecret) == "" {
		return nil
	}
	if window <= 0 {
		window = defaultVerifyWindow
	}
	nextID := legacyGenerationID
	var carried []keyGeneration
	if gs != nil {
		for _, g := range gs.Generations {
			if g.ID >= nextID {
				nextID = g.ID + 1
			}
		}
		// Only the OUTGOING CURRENT is carried forward. Older previous
		// generations are dropped rather than aged out, which is what keeps the
		// set within maxLiveGenerations and, more importantly, means a second
		// rotation cannot quietly extend the life of a key from two rotations
		// ago.
		if cur, ok := gs.currentGeneration(); ok {
			cur.VerifyUntil = now.Add(window)
			carried = append(carried, cur)
		}
	}
	gens := append([]keyGeneration{{ID: nextID, Secret: newSecret, Created: now}}, carried...)
	return newGenerationSet(nextID, gens)
}

// Generation markers on minted artifacts.
//
// An artifact that can carry a marker lets a verifier SELECT the one generation
// to check rather than trying each in turn: cheaper, and it makes "which key
// verified this" observable instead of inferred. The marker is a prefix
// "g<N>." on the artifact value.
//
// Not every artifact can carry one. Cookies and SSO tokens have payload room;
// a heartbeat bearer is a bare HMAC-derived string presented raw in an
// Authorization header, and the terminal/invite keys are symmetric values read
// from env by both Go and Node — none of those has an envelope, and giving one
// to them would change a contract with already-deployed spokes, making the
// rotation mechanism itself require the flag day it exists to avoid. Those
// artifacts use bounded trial verification instead, which maxLiveGenerations
// keeps to at most two attempts. See src/docs/design/master-key-rotation.md for
// the per-artifact table.

// generationMarkerPrefix introduces a generation marker. Chosen as a letter
// rather than a bare digit so an unmarked legacy value can never be mistaken
// for a marked one: legacy artifacts begin with a username or a base64 payload,
// and the parser requires BOTH this prefix and a well-formed integer.
const generationMarkerPrefix = "g"

// formatGenerationMarker renders the prefix a minted artifact carries.
func formatGenerationMarker(id int) string {
	return generationMarkerPrefix + strconv.Itoa(id) + "."
}

// splitGenerationMarker separates a leading generation marker from the rest of
// an artifact value.
//
// Returns (id, remainder, true) when a well-formed marker is present, and
// (0, value, false) when it is not — which is the UNMARKED LEGACY case and is
// explicitly not an error. Every artifact minted before this change lacks a
// marker, so a verifier must be able to fall back to trying each acceptable
// generation rather than rejecting it. That fallback is what makes deploying
// this change itself a non-event.
//
// The parse is strict: a value like "gfoo.bar" or "g.bar" or "g-1.bar" is NOT a
// marker, because treating a malformed marker as a valid one would let a
// crafted value steer generation selection.
func splitGenerationMarker(value string) (int, string, bool) {
	if !strings.HasPrefix(value, generationMarkerPrefix) {
		return 0, value, false
	}
	rest := value[len(generationMarkerPrefix):]
	dot := strings.Index(rest, ".")
	if dot <= 0 {
		return 0, value, false
	}
	id, err := strconv.Atoi(rest[:dot])
	if err != nil || id <= 0 {
		return 0, value, false
	}
	return id, rest[dot+1:], true
}

// verifyWithGenerations is the shared dual-acceptance driver.
//
// It resolves which generations to try — the marked one if the artifact names a
// generation the set still accepts, otherwise every acceptable generation in
// current-first order — and calls `attempt` with each generation's secret and
// the artifact stripped of its marker. It returns the ID of the generation that
// accepted, so callers can report which key verified rather than merely that
// something did. That return value is the versioning promise made observable:
// it is what the readiness surface counts.
//
// A marker naming a generation that is NOT acceptable (expired, or unknown)
// falls through to trying every acceptable generation rather than failing
// outright. The marker is an optimization hint carried on an unauthenticated
// value, so it must never be able to make verification FAIL — otherwise anyone
// could invalidate a valid artifact by editing its prefix. It can only ever
// make verification cheaper.
func verifyWithGenerations[T any](
	gs *generationSet,
	value string,
	now time.Time,
	attempt func(secret, artifact string) (T, bool),
) (T, int, bool) {
	var zero T
	acceptable := gs.acceptableGenerations(now)
	if len(acceptable) == 0 {
		return zero, 0, false
	}
	markedID, stripped, marked := splitGenerationMarker(value)
	if marked {
		for _, g := range acceptable {
			if g.ID == markedID {
				out, ok := attempt(g.Secret, stripped)
				if ok {
					return out, g.ID, true
				}
				// The marker named a live generation and the signature did not
				// verify under it. Do NOT fall through to the other
				// generations: the artifact asserted which key signed it, that
				// key says no, and trying the others would only serve to
				// re-admit a value whose own claim already failed.
				return zero, 0, false
			}
		}
		// Marker named a generation we do not accept (expired or unknown). Fall
		// through and try what we do accept, against the STRIPPED value.
		value = stripped
	}
	for _, g := range acceptable {
		if out, ok := attempt(g.Secret, value); ok {
			return out, g.ID, true
		}
	}
	return zero, 0, false
}

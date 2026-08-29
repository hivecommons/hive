package hub

import (
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Persistence and the operator-facing rotate mechanism for master-secret
// generations (follow-on PR #4 of src/docs/design/master-key-rotation.md).
//
// The foundation (hub_generations.go) can already REPRESENT a rotation:
// generationSet.rotate is pure, maxLiveGenerations bounds the set, and every
// previous generation carries VerifyUntil. What it cannot do is CREATE one, or
// remember one. Both gaps are closed here, and they are closed together
// deliberately: an endpoint that rotates without persisting would produce a
// rotation that survives until the next hub roll — which happens several times
// a day — and a hub that came back holding only generation 1 would reject every
// artifact minted since the rotation while quietly re-minting on the old key.
// That is strictly worse than never having rotated.
//
// WHY A SEPARATE FILE FROM hub-secret.key. /data/saas/hub-secret.key stays
// exactly as it is — 64 hex bytes, raw, never rewritten by this code. It is
// generation 1's secret and nothing else, so a hub that rolls BACK to
// pre-rotation code finds precisely the file it expects and keeps working on
// generation 1. Rewriting it in place would make rollback a data-loss event.
// The generations live alongside it in hub-generations.json, and when that file
// is absent — the state of every hub in the fleet today — the loader synthesizes
// legacyGenerationSet from hub-secret.key. So there is no migration step and no
// flag day: "never rotated" and "rotated back down to one generation" are the
// same on-disk state.

// hubGenerationsPath is where the generation set is persisted.
//
// A SIBLING of /data/saas/hub-secret.key on the hub PVC, which is the volume
// that survives a pod roll — and the hub rolls several times a day, so anything
// held only in memory is effectively not held at all. Verified live: the hub's
// HIVE_HUB_SECRET env is UNSET and hub-secret.key exists on the PVC at 64
// bytes, so the file is the live source of the master and this file sits in the
// same durable place.
//
// A var, not a const, so tests can redirect it — the same convention as
// alertAcksPath, journeyStatePath, and revokedSessionsPath.
var hubGenerationsPath = "/data/saas/hub-generations.json"

// hubGenerationsFileMode is 0600, NOT the 0644 the other /data/saas JSON files
// use. Those hold acks, banners, and journey state; this one holds MASTER
// SECRETS in plaintext, so it matches hub-secret.key's own 0600 rather than its
// neighbours' mode.
const hubGenerationsFileMode = 0o600

// hubGenerationsQuarantineSuffix preserves an unparseable file for inspection
// instead of overwriting it.
//
// The recovery differs sharply from the alert-acks precedent, and the
// difference is the point. A corrupt acks file is discarded and the system
// starts fresh, because losing an ack merely un-silences an alert — the safe
// direction. A corrupt GENERATIONS file must NOT be discarded and replaced with
// a fresh rotation: that would mint new material the fleet has never seen while
// forgetting the generation the fleet is actually on. The bad bytes are left on
// disk under this suffix so an operator can look.
//
// AUDIT 8 / F20. What this file used to do next was fall back to the legacy
// single-generation set, on the argument that hub-secret.key is authoritative
// for generation 1 so "unreadable" and "never rotated" resolve identically.
// That argument is true before the first rotation and FALSE after it: once a
// rotation has happened, generation 1 is SUPERSEDED material carrying a
// VerifyUntil, and re-installing it as the current minting generation is a
// silent un-rotation. The file's own presence is the discriminator — see
// generationsLoadOutcome.
const hubGenerationsQuarantineSuffix = ".corrupt"

// generationsReadAttempts is how many times a non-ENOENT read is retried before
// the loader gives up and fails closed.
//
// The hazard this fix closes is a TRANSIENT fault — a PVC remount, an NFS blip,
// an EIO — so the first thing to do about it is simply to try again. Failing
// closed is the correct end state but it is not a cheap one: it takes the hub
// out of minting, so it should be reached only after the transient explanation
// has been ruled out. Three attempts over ~300ms costs nothing on the happy
// path (where the first read succeeds) and converts the overwhelmingly likely
// fault — a blip lasting milliseconds — back into a normal startup.
const generationsReadAttempts = 3

// generationsReadRetryDelay is the pause between read attempts. Deliberately
// short: this runs on the hub's startup path, and a hub that takes seconds
// longer to come up is its own availability problem.
const generationsReadRetryDelay = 100 * time.Millisecond

// hubGenerationsFileMu serialises writers, for the reason alertAcksFileMu
// documents: two concurrent saves writing the same tmp path interleave their
// bytes and rename non-JSON into place. Here the consequence is worse than a
// lost ack — an unparseable generations file means the hub falls back to
// generation 1 on its next restart.
var hubGenerationsFileMu sync.Mutex

// persistedGenerations is the on-disk shape. It is exactly generationSet's
// JSON, named separately so the file format is a deliberate, reviewable
// artifact rather than an incidental consequence of a struct definition.
type persistedGenerations struct {
	Current     int             `json:"current"`
	Generations []keyGeneration `json:"generations"`
	// RotatedAt records the most recent rotation, for operator forensics and —
	// more importantly — for the double-rotation guard. Never used to decide
	// whether a key is ACCEPTABLE; that is VerifyUntil's job alone.
	RotatedAt time.Time `json:"rotated_at,omitempty"`
}

// masterSecretBytes is the length of a generated master, matching the 32 bytes
// NewHubServer generates for hub-secret.key (rendered as 64 hex chars).
const masterSecretBytes = 32

// generateMasterSecret returns a fresh hex master secret from crypto/rand.
//
// Returns an error rather than a short read: a partially-filled buffer would
// produce a low-entropy master that every subsequent derivation inherits, and
// silently rotating TO a weak key is worse than refusing to rotate at all.
func generateMasterSecret() (string, error) {
	b := make([]byte, masterSecretBytes)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// saveGenerations writes the set atomically (tmp + rename) at 0600.
//
// Returns an error rather than only logging, unlike its neighbours. The
// rotate handler MUST fail loudly if this fails: a rotation that is applied in
// memory but not on disk is a rotation the hub forgets on its next roll, and
// the operator would have no signal that the fleet is now converging onto a key
// the hub is about to lose.
func saveGenerations(gs *generationSet, rotatedAt time.Time) error {
	if gs == nil || len(gs.Generations) == 0 {
		return errors.New("refusing to persist an empty generation set")
	}
	hubGenerationsFileMu.Lock()
	defer hubGenerationsFileMu.Unlock()

	data, err := json.MarshalIndent(persistedGenerations{
		Current:     gs.Current,
		Generations: gs.Generations,
		RotatedAt:   rotatedAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(hubGenerationsPath), 0o755); err != nil {
		return err
	}
	tmpPath := hubGenerationsPath + ".tmp"
	// WriteFile's mode applies only on create, so an existing tmp file left by
	// a crashed write could keep a laxer mode. Remove it first so 0600 is
	// guaranteed for a file that contains master secrets.
	_ = os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, data, hubGenerationsFileMode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, hubGenerationsPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// generationsLoadOutcome names what the loader was able to establish about the
// hub's rotation state. It exists because the two degraded cases that used to
// be collapsed into one return value are opposites (audit 8, F20).
type generationsLoadOutcome int

const (
	// generationsLoaded: the file was read and parsed and yielded a usable set.
	generationsLoaded generationsLoadOutcome = iota
	// generationsNeverRotated: the file is ABSENT (ENOENT). This is the state
	// of every hub in the fleet today and it is a POSITIVE fact, not an
	// absence of information: the file is created only by saveGenerations, so
	// its non-existence proves no rotation has ever been persisted. The legacy
	// single-generation set built from hub-secret.key is therefore exactly
	// right, and generation 1 is the CURRENT generation rather than a
	// superseded one.
	generationsNeverRotated
	// generationsUntrusted: the file exists (or its existence could not even
	// be determined) but its contents could not be established — an
	// unretryable read error, or bytes that do not parse, or a parsed set with
	// no minting generation.
	//
	// THIS IS THE CASE THAT MUST NOT FALL BACK TO LEGACY. The hub knows a
	// rotation may have happened and cannot tell which generation is current.
	// Installing legacy here re-mints on superseded material, drops the
	// post-rotation generation out of the accepted set entirely, and — because
	// RotatedAt goes with it — clears the anti-stranding cooldown so the next
	// rotation can strand a fleet that is still converging. Every one of those
	// is a widening or a reversion, which is precisely what the design
	// forbids.
	generationsUntrusted
)

// loadGenerations reads the persisted set.
//
// THE LOAD-BEARING DISTINCTION IS "ABSENT" vs "UNREADABLE", and it is the whole
// of this function. Both used to return the legacy set. They must not:
//
//   - Missing file (ENOENT): normal, and the state of every hub in the fleet
//     today. Returns the legacy single-generation set with
//     generationsNeverRotated. Correct because hub-secret.key is authoritative
//     for generation 1 and the generations file is only ever created by
//     saveGenerations, so "no file" IS "no rotation".
//
//   - Unretryable read error (EACCES, EIO, a stat that itself fails): the file
//     may exist and may record a rotation. Returns (nil, zero,
//     generationsUntrusted). NO legacy fallback — that would be a silent
//     un-rotation to superseded material.
//
//   - Unparseable bytes: quarantined for inspection, then
//     generationsUntrusted. A truncated write of a ROTATED file parses as
//     nothing; treating it as "empty" would be indistinguishable from treating
//     it as "never rotated", which is the same downgrade by another route.
//
//   - Parseable but with no generation matching Current: newGenerationSet
//     returns nil. A set with no minting key is unusable, not degraded — and,
//     critically, it is evidence a rotation WAS written, so it is untrusted
//     rather than absent.
//
// This is the same philosophy as VerifyUntil.IsZero() meaning ALREADY EXPIRED
// rather than "never expires": when the file cannot be trusted, the hub must
// never quietly WIDEN what it accepts or REVERT what it mints.
//
// Note what is deliberately NOT validated here: an expired VerifyUntil. An
// expired previous generation is loaded into the set and then excluded by
// acceptableGenerations at every VERIFY. Filtering at load time instead would
// make expiry depend on when the hub last restarted — a generation would stay
// live across a long-running process and vanish on a roll. The wall clock must
// be the only thing that closes the window.
//
// Returns the set AND the persisted RotatedAt. The timestamp must survive a
// restart or the double-rotation cooldown would reset on every hub roll —
// several times a day — which would make the guard trivially bypassable by
// waiting for the next deploy rather than for the fleet to converge.
func loadGenerations(master string, logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}) (*generationSet, time.Time, generationsLoadOutcome) {
	data, err := readGenerationsFile()
	if err != nil {
		if os.IsNotExist(err) {
			// The one legitimate fallback. No rotation has ever been
			// persisted, so generation 1 is genuinely current.
			return legacyGenerationSet(master), time.Time{}, generationsNeverRotated
		}
		if logger != nil {
			logger.Warn("hub generations file could not be read after retries — REFUSING to fall back to the single-generation master, "+
				"because a rotation may have been recorded in it and falling back would silently re-mint on superseded key material",
				"path", hubGenerationsPath, "attempts", generationsReadAttempts, "error", err)
		}
		return nil, time.Time{}, generationsUntrusted
	}

	var p persistedGenerations
	if err := json.Unmarshal(data, &p); err != nil {
		if logger != nil {
			logger.Warn("hub generations file is not parseable — quarantining and REFUSING to fall back to the single-generation master; NOT rotating",
				"path", hubGenerationsPath, "error", err)
		}
		_ = os.Rename(hubGenerationsPath, hubGenerationsPath+hubGenerationsQuarantineSuffix)
		return nil, time.Time{}, generationsUntrusted
	}

	gs := newGenerationSet(p.Current, p.Generations)
	if gs == nil {
		if logger != nil {
			// Never log the file's contents — it holds master secrets. Counts
			// and the current ID only.
			logger.Warn("hub generations file has no usable current generation — REFUSING to fall back to the single-generation master",
				"path", hubGenerationsPath, "current", p.Current, "entries", len(p.Generations))
		}
		return nil, time.Time{}, generationsUntrusted
	}
	if logger != nil {
		logger.Info("loaded hub master generations",
			"current", gs.Current, "live_generations", len(gs.Generations))
	}
	return gs, p.RotatedAt, generationsLoaded
}

// afterGenerationsReadAttempt, if non-nil, is called with the attempt index
// (0-based) immediately after each read attempt in readGenerationsFile
// completes, whether it succeeded or failed. It exists purely as a test
// seam — see TestF20_ReadIsRetriedBeforeFailingClosed — so a test can learn
// exactly when attempt 0 has failed and synchronize a fault-clearing action
// to land deterministically between attempts, instead of racing that action's
// own timer against generationsReadRetryDelay. Left nil in production, where
// calling it is a no-op check on every attempt.
var afterGenerationsReadAttempt func(attempt int)

// readGenerationsFile reads the generations file, retrying a non-ENOENT error a
// bounded number of times.
//
// WHY RETRY AT ALL. The fault this whole path guards against is transient by
// construction — a PVC remount, an NFS blip, a device-mapper hiccup. Failing
// closed is the right END state but a costly one (the hub stops minting), so it
// must not be reached on a fault that would have cleared on the next syscall.
// A missing file is returned IMMEDIATELY without retry: ENOENT is not a fault,
// it is the fleet's normal steady state, and retrying it would add 300ms to
// every hub start for nothing.
//
// Returns the raw error so the caller can still test it with os.IsNotExist.
func readGenerationsFile() ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < generationsReadAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(generationsReadRetryDelay)
		}
		data, err := os.ReadFile(hubGenerationsPath)
		if afterGenerationsReadAttempt != nil {
			afterGenerationsReadAttempt(attempt)
		}
		if err == nil {
			return data, nil
		}
		lastErr = err
		if os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// Rotation.

// errRotationTooSoon is returned when a second rotation would strand spokes
// that are still converging onto the first.
var errRotationTooSoon = errors.New("previous rotation has not converged")

// errGenerationsUntrusted is returned when a rotation is attempted on a hub
// whose generation state could not be established at startup (audit 8, F20).
var errGenerationsUntrusted = errors.New("hub generation state is untrusted")

// errRotationWouldStrandSpokes is returned when the last reconcile sweep did
// not observe the entire fleet (audit 8, F19/F21). Distinct from
// errRotationTooSoon because the remedy is different: the cooldown clears on its
// own with time, this one does not clear until the fleet becomes observable.
var errRotationWouldStrandSpokes = errors.New("rotation would strand unobserved spokes")

// fleetObservationSummary is the evidence-completeness slice of the reconcile
// sweep's state — the only part the rotation interlock needs.
type fleetObservationSummary struct {
	Considered          int
	Unreachable         int
	UnreachableClusters []string
	// FullyObserved mirrors PerHiveEnvStatus.FleetFullyObserved EXACTLY, and is
	// asserted equal to it by TestRotationInterlockMatchesFleetFullyObserved.
	// Two expressions of one predicate is one more than ideal; the test exists
	// so they cannot drift, which is the same discipline the four VerifyUntil
	// readers are held to.
	FullyObserved bool
}

// fleetObservation reports what the last per-hive env sweep managed to observe.
//
// It reads the same four fields PerHiveEnvSnapshot does, under the same
// perHiveEnvMu, but deliberately does NOT call PerHiveEnvSnapshot: that function
// also reads s.keyGenerations, and rotateMasterSecret calls this while holding
// keyGenerationsMu for write. Snapshotting only the observation counters keeps
// the interlock free of any second lock order and avoids an unsynchronised read
// of the very field the caller is about to replace.
func (s *HubServer) fleetObservation() fleetObservationSummary {
	out := fleetObservationSummary{}
	if s == nil {
		// A nil hub has observed nothing, and "nothing observed" is not
		// "everything observed" — fail closed, same as a zero-hive sweep.
		return out
	}
	s.perHiveEnvMu.RLock()
	defer s.perHiveEnvMu.RUnlock()
	out.Considered = s.perHiveEnvConsidered
	out.Unreachable = s.perHiveEnvUnreachable
	out.UnreachableClusters = append([]string(nil), s.perHiveEnvUnreachableClusters...)
	sort.Strings(out.UnreachableClusters)
	out.FullyObserved = out.Considered > 0 && out.Unreachable == 0
	return out
}

// unreachableClusterSuffix renders cluster IDs for an operator-facing message.
// Cluster IDs are non-secret registry identifiers (e.g. "heartbeat-only-cluster"); no key
// material, hive name, or env value passes through here.
func unreachableClusterSuffix(clusters []string) string {
	if len(clusters) == 0 {
		return ""
	}
	return ", unreachable clusters: " + strings.Join(clusters, ",")
}

// rotationCooldown is how long after a rotation a SECOND rotation is refused
// without an explicit force.
//
// WHY REFUSE RATHER THAN QUEUE OR SILENTLY ALLOW. maxLiveGenerations is 2, and
// deliberately so (see its comment). rotate() carries forward ONLY the outgoing
// current, so rotating twice in quick succession DROPS the generation from two
// rotations ago — and that dropped generation is exactly the one most of the
// fleet is still on, because the reconcile lane walks spokes at 3 patches per
// 15-minute cycle. A second rotation an hour into the first would leave ~54 of
// 66 spokes holding material the hub no longer accepts: they would 401 on every
// heartbeat until the sweep reached them, hours later. That is the fleet-wide
// flag day this entire design exists to prevent, reintroduced through the front
// door.
//
// The cooldown is sized to the CONVERGENCE time, not to the verify window. 66
// spokes at 3 per 15-minute cycle is ~5.5 hours, so 8 hours covers a full sweep
// with margin for cycles lost to unreachable clusters. It is deliberately much
// shorter than defaultVerifyWindow (7 days): waiting for the previous
// generation to EXPIRE before allowing another rotation would make emergency
// re-rotation impossible for a week, and the actual hazard is unconverged
// spokes, not the old key still being accepted.
const rotationCooldown = 8 * time.Hour

// rotationDecision is the pure, testable answer to "may this rotation proceed?"
// — separated from the handler so the guard has no HTTP in it and the handler
// has no policy in it.
type rotationDecision struct {
	// Allowed is whether the rotation may proceed.
	Allowed bool
	// Reason explains a refusal, for the operator. Never contains key material.
	Reason string
	// RetryAfter is how long until the cooldown lapses. Zero when allowed.
	RetryAfter time.Duration
}

// evaluateRotation decides whether a rotation may proceed.
//
// THE CHOICE MADE HERE: REFUSE, with an explicit force flag as the override,
// rather than allowing it and warning. A warning on a response body is not a
// control — the operator most likely to double-rotate is one who did not
// realise the first rotation was still in flight, which is precisely the
// operator who will not read it. Requiring `force` makes stranding the fleet a
// thing you have to TYPE, and the endpoint tells you what you would be doing.
//
// force is honoured because there is a real case for it: if the new generation
// itself is compromised (a leaked rotate response, a bad deploy), rotating
// again immediately is the correct emergency action even at the cost of
// stranding spokes for a few hours. A guard with no override would turn an
// 8-hour cooldown into an 8-hour window of known-compromised material.
func evaluateRotation(lastRotation time.Time, now time.Time, force bool) rotationDecision {
	if lastRotation.IsZero() {
		// Never rotated. Nothing can be stranded.
		return rotationDecision{Allowed: true}
	}
	elapsed := now.Sub(lastRotation)
	if elapsed >= rotationCooldown {
		return rotationDecision{Allowed: true}
	}
	// A negative elapsed means lastRotation is in the FUTURE — a clock skew or
	// a hand-edited file. Treat it as "inside the cooldown" rather than
	// computing a nonsense RetryAfter: fail closed.
	remaining := rotationCooldown - elapsed
	if remaining > rotationCooldown {
		remaining = rotationCooldown
	}
	if force {
		return rotationDecision{Allowed: true, Reason: "forced within cooldown", RetryAfter: remaining}
	}
	return rotationDecision{
		Allowed: false,
		Reason: "a rotation is still converging; with at most two live generations a second rotation " +
			"would drop the generation most spokes are still on and 401 them until the reconcile sweep " +
			"catches up. Wait for the cooldown, or pass force=true if the current generation is itself compromised.",
		RetryAfter: remaining,
	}
}

// currentGenerations returns an immutable snapshot of the hub's generation set.
//
// The set is only ever REPLACED, never mutated, so the returned pointer stays
// valid and self-consistent for as long as the caller holds it — a verifier
// that reads just before a rotation verifies against the pre-rotation set,
// which is exactly right because the outgoing generation remains acceptable.
func (s *HubServer) currentGenerations() *generationSet {
	if s == nil {
		return nil
	}
	s.keyGenerationsMu.RLock()
	defer s.keyGenerationsMu.RUnlock()
	return s.keyGenerations
}

// lastRotationAt reports when the current generation was minted, or the zero
// time on a hub that has never rotated.
func (s *HubServer) lastRotationAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.keyGenerationsMu.RLock()
	defer s.keyGenerationsMu.RUnlock()
	return s.lastKeyRotation
}

// rotateMasterSecret performs a rotation: generate, demote, persist, install.
//
// ORDER MATTERS AND IS DELIBERATE. The new set is persisted BEFORE it is
// installed in memory. If the write fails the hub keeps running on the old set
// and the caller gets an error — a hub that rotated in memory only would mint
// on a key it forgets at its next roll (several times a day), and every
// artifact minted in between would become unverifiable. Persist-then-install
// makes a failed rotation a no-op rather than a time bomb.
//
// Returns the NEW set on success. Never returns, logs, or otherwise exposes any
// secret material — the caller gets generation IDs and timestamps only.
func (s *HubServer) rotateMasterSecret(now time.Time, force bool) (*generationSet, rotationDecision, error) {
	if s == nil {
		return nil, rotationDecision{}, errors.New("no hub server")
	}
	// Serialise the whole rotation, not just the field write. Two concurrent
	// POSTs that both read the set, both built a rotation from it, and both
	// persisted would race: the second write would carry forward the FIRST
	// rotation's outgoing generation, silently discarding the first rotation's
	// new current — which is live material by then. Holding the write lock
	// across evaluate-generate-persist-install makes the second POST observe
	// the first one's lastKeyRotation and be refused by the cooldown, which is
	// the idempotency guarantee a double-submit needs.
	s.keyGenerationsMu.Lock()
	defer s.keyGenerationsMu.Unlock()

	// AUDIT 8 / F20. A nil set here means the loader could not establish what
	// this hub's current generation is (generationsUntrusted). Rotating from
	// that state is the worst available move: rotate() on a nil receiver
	// carries nothing forward, so it would mint a brand-new generation 1 and
	// declare the fleet's actual current generation to have never existed —
	// stranding every spoke with no verify window at all. Refuse, loudly,
	// regardless of force. force exists to override the CONVERGENCE cooldown,
	// not to override not knowing what the state is.
	if s.keyGenerations == nil {
		return nil, rotationDecision{
			Allowed: false,
			Reason: "the hub's generation state is untrusted — the generations file could not be read or parsed at startup. " +
				"Rotating now would discard the generation the fleet is actually on. Resolve the file on the hub PVC and restart the hub.",
		}, errGenerationsUntrusted
	}

	// AUDIT 8 / F19 + F21. THE STRANDING INTERLOCK.
	//
	// F19 wires the derivation path to the live set, which means a rotation now
	// actually reaches the fleet. F21 measured that 44 of the 70 hosted spokes
	// sit on pull_only clusters (the heartbeat-only cluster, a spoke cluster) that the reconcile sweep
	// structurally cannot read or patch — by design, and that is not changing
	// until the Option D wrapped-master delivery path lands. Those two facts
	// together are dangerous in a way neither is alone: before F19 a rotation
	// was a silent no-op, and after F19 without this gate it would converge the
	// 26 reachable spokes, leave 44 on the outgoing generation, and strand them
	// when retirement fires unconditionally at verify_until seven days later.
	// The audit's phrase for it is "converts a silent no-op into a 7-day-delayed
	// outage", and it is the reason F21 was named a precondition of F19.
	//
	// So: refuse to rotate unless the last sweep OBSERVED THE ENTIRE FLEET.
	// FleetFullyObserved is (ConsideredHives > 0 && UnreachableHives == 0), and
	// a pull-only cluster increments UnreachableHives for every hive on it, so
	// this is false on the production hub today and will stay false until every
	// spoke is reachable by whatever delivery mechanism ships. That is the
	// intended effect: the wiring lands now, and the ability to fire it unlocks
	// only when the thing that makes it safe exists.
	//
	// WHY HERE AND NOT IN THE HANDLER. This is the single choke point every
	// rotation passes through; a gate in handleRotateMasterKey could be bypassed
	// by any future internal caller. It sits BEFORE evaluateRotation so an
	// operator sees the stranding reason rather than a cooldown message.
	//
	// WHY NOT force. force overrides the convergence COOLDOWN — a "the current
	// generation is compromised, accept the cost" switch. This is not that: with
	// 44 spokes structurally unreachable the cost is not "some spokes lag", it
	// is "44 spokes hard-fail heartbeat in 7 days with no path to converge
	// them". There is no in-band way to accept that responsibly, so force does
	// not reach it. An operator who genuinely must rotate a compromised master
	// ahead of Option D has to make the fleet observable first.
	//
	// NOTE the deliberate asymmetry with retirement, which is unconditional and
	// must stay that way (hub_generations_retire.go:42-60): gating RETIREMENT on
	// convergence would let one unreachable spoke pin a superseded secret live
	// forever. Gating the START of a rotation strands nobody — nothing has been
	// demoted yet — so the two gates point in opposite directions correctly.
	if obs := s.fleetObservation(); !obs.FullyObserved {
		return nil, rotationDecision{
			Allowed: false,
			Reason: fmt.Sprintf("refusing to rotate: the last reconcile sweep did not observe the whole fleet "+
				"(%d hives considered, %d unreachable%s). Rotating now would converge only the reachable spokes and "+
				"strand the rest when the previous generation retires at verify_until. Make every spoke reachable "+
				"by the convergence lane first; force does not override this.",
				obs.Considered, obs.Unreachable, unreachableClusterSuffix(obs.UnreachableClusters)),
		}, errRotationWouldStrandSpokes
	}

	decision := evaluateRotation(s.lastKeyRotation, now, force)
	if !decision.Allowed {
		return nil, decision, errRotationTooSoon
	}

	secret, err := generateMasterSecret()
	if err != nil {
		return nil, decision, err
	}
	next := s.keyGenerations.rotate(secret, now, defaultVerifyWindow)
	if next == nil {
		// rotate returns nil only for an empty new secret, which
		// generateMasterSecret cannot produce — but rotating to no key would
		// silently disable minting, so refuse rather than install it.
		return nil, decision, errors.New("rotation produced no usable generation set")
	}
	if err := saveGenerations(next, now); err != nil {
		return nil, decision, err
	}
	s.keyGenerations = next
	s.lastKeyRotation = now
	// AUDIT 8 / F19. Install into the package-level pointer the SPOKE-BOUND
	// derivation path reads, in the same critical section that installs the
	// minting set. This single line is what makes a rotation observable to
	// desiredPerHiveEnv and therefore to the reconcile sweep; without it the
	// sweep keeps computing "desired" from the pre-rotation generation, sees no
	// drift, and patches nothing — the inertness this finding is about.
	//
	// Deliberately inside the keyGenerationsMu critical section so the two can
	// never disagree, even transiently. setLiveGenerations takes a DIFFERENT
	// mutex (liveGenerationsMu) and nothing that holds liveGenerationsMu ever
	// reaches for keyGenerationsMu, so this cannot deadlock.
	setLiveGenerations(next, false)
	return next, decision, nil
}

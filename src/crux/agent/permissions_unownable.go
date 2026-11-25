package agent

import (
	"log/slog"
	"os"
	"sync"
)

// unownableHint tells the operator who can actually repair the ownership the
// watcher is reporting.
const unownableHint = "chown needs root (CAP_CHOWN); the watcher runs unprivileged after the entrypoint's privilege drop, so these entries are repaired by the entrypoint's root phase or out of band, never by retrying here"

// cannotChown reports whether this process certainly cannot chown an entry
// owned by ownerUID, so attempting it would only ever return EPERM.
//
// chown(2) lets an unprivileged caller change only the GROUP of an inode it
// OWNS (to a group it belongs to); changing the uid, or anything about an
// inode owned by someone else, needs CAP_CHOWN. The hive process drops to dev
// (uid 1001) at the entrypoint, so every entry another uid owns — a root seed,
// or a sandbox kick's workspace under /data/agents/sandbox/<slug>-<nonce>
// written by the Job pod's uid (#18120) — is unrepairable by construction, and
// a surge of kicks mints thousands of such paths between two ticks. Deciding
// that here, before the syscall, is what keeps the failure from being logged
// once per path (4087 WARN lines in ten minutes on hive-tall-frog).
func cannotChown(ownerUID uint32) bool {
	euid := os.Geteuid()
	return euid != 0 && uint32(euid) != ownerUID
}

// unownableGrowthFactor is how much the skipped count must grow over the
// largest count already reported at WARN before the summary warns again. A
// surge of sandbox kicks grows a standing unfixable tree a little on almost
// every tick, so warning on any growth re-warned every tick (10 WARNs in ten
// minutes on marinatedconcrete, count=9447, hivecommons/hive#19791); doubling
// bounds the WARNs to log2(count) for the life of the condition.
const unownableGrowthFactor = 2

// unownableClearTicks is how many consecutive ticks must skip nothing before
// the condition counts as cleared and the high-water mark resets. Sandbox
// workspaces come and go with their kicks, so a single empty tick between two
// waves is not a recovery; requiring a run of them keeps that flicker from
// re-arming a WARN for the next wave.
const unownableClearTicks = 3

// unownableTally counts, per watcher tick, the entries fixEntry skipped
// because cannotChown held, and reports them as ONE summary line per tick
// instead of one line per path. The summary is WARN only when the count at
// least doubles the high-water mark since the condition last cleared, and
// DEBUG otherwise, so a standing unfixable tree costs one WARN when it
// appears, one more each time it doubles, and nothing at WARN in steady state.
type unownableTally struct {
	mu         sync.Mutex
	count      int    // entries skipped this tick
	sample     string // first skipped path this tick, for the operator's grep
	highWater  int    // largest count already reported at WARN; 0 once clear
	clearTicks int    // consecutive ticks that skipped nothing
}

// skipped records one entry the watcher could not own this tick.
func (u *unownableTally) skipped(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.count == 0 {
		u.sample = path
	}
	u.count++
}

// flush ends the tick: emits the summary and resets the per-tick counters. It
// returns whether the summary went out at WARN (tests).
func (u *unownableTally) flush(logger *slog.Logger) bool {
	u.mu.Lock()
	count, sample := u.count, u.sample
	u.count, u.sample = 0, ""
	if count == 0 {
		// Condition cleared once it stays clear for a run of ticks: the
		// next appearance after that is a new finding.
		u.clearTicks++
		if u.clearTicks >= unownableClearTicks {
			u.highWater = 0
		}
		u.mu.Unlock()
		return false
	}
	u.clearTicks = 0
	warn := u.highWater == 0 || count >= u.highWater*unownableGrowthFactor
	if warn {
		u.highWater = count
	}
	u.mu.Unlock()

	attrs := []any{
		"count", count,
		"sample_path", sample,
		"watcher_uid", os.Geteuid(),
		"hint", unownableHint,
	}
	if warn {
		logger.Warn("permissions watcher: entries owned by another uid need a chown this process cannot perform; skipped without retrying",
			append(attrs, "note", "one summary per tick; logged at debug until the count doubles or the condition clears")...)
		return true
	}
	logger.Debug("permissions watcher: entries owned by another uid skipped (unchanged)", attrs...)
	return false
}

// permUnownable is the watcher's shared tally. Package-level for the same
// reason as permWarnDedupe: fixEntry keeps its (path, fi, logger) signature.
var permUnownable = &unownableTally{}

// resetPermUnownable restores pristine tally state. Tests only.
func resetPermUnownable() { permUnownable = &unownableTally{} }

package automerge

import (
	"sync"
	"time"
)

// sweepMemory is what the lanes remember across sweeps about a PR head:
// the merge-forward tried on it, an update-branch or merge-forward GitHub
// refused for lack of the workflows permission, a merge GitHub refused
// (automerge_merge_refusal.go), and the holds already logged.
//
// It is shared by every engine built over the same GitHub client, not kept
// on one engine. The hive runs the queued, trusted-author and contributor
// lanes every minute on a one-shot engine (SweepTrustedAuthorAutoMerges,
// SweepContributorLaneAutoMerges, cmd/hive runAutoMergeSweepIfDue), next to
// the long-lived self-authored loop, so a mark on the engine was empty
// again on the next tick: projectbluefin fsdk-containers#508 sent the same
// refused update-branch to GitHub every ~75 s, and the "logged once per
// head" holds were logged every sweep.
//
// An engine over a transport that does not share state (the sweep's test
// fakes, a nil transport) keeps its own memory, as before. A read-only
// evaluation (IntegrationEligibility) gets its own too, so a probe that
// cannot act never marks a head as tried.
type sweepMemory struct {
	// onceMu guards once and holdsLogged: key -> when it was last used.
	onceMu      sync.Mutex
	once        map[string]time.Time
	holdsLogged map[string]time.Time
	// mergeRefusalsMu guards mergeRefusals (automerge_merge_refusal.go).
	mergeRefusalsMu sync.Mutex
	mergeRefusals   map[string]mergeRefusal
}

// sweepMemoryStateName is the name the memory is kept under on the client
// (hgithub.Client.SharedState).
const sweepMemoryStateName = "automerge.sweep-memory"

// sweepMemoryIdle is how long a once-per-head mark nothing has used stays:
// past it the PR merged, closed or moved to another head long ago.
const sweepMemoryIdle = 24 * time.Hour

// sweepMemoryPruneAt is the entry count at which idle marks are dropped on
// the next insert.
const sweepMemoryPruneAt = 1024

func newSweepMemory() *sweepMemory { return &sweepMemory{} }

// sharedSweepMemory returns the memory the transport keeps for every engine
// built over it, or nil when it keeps none.
func sharedSweepMemory(transport Transport) *sweepMemory {
	provider, ok := transport.(interface {
		SharedState(name string, build func() any) any
	})
	if !ok {
		return nil
	}
	m, _ := provider.SharedState(sweepMemoryStateName, func() any { return newSweepMemory() }).(*sweepMemory)
	return m
}

// memory returns the engine's sweep memory, building a private one for an
// engine not made by New.
func (c *Engine) memory() *sweepMemory {
	c.sweepMemoryOnce.Do(func() {
		if c.sweepMemory == nil {
			c.sweepMemory = newSweepMemory()
		}
	})
	return c.sweepMemory
}

// markOnce records key and reports whether this is its first use.
func (m *sweepMemory) markOnce(key string, now time.Time) bool {
	m.onceMu.Lock()
	defer m.onceMu.Unlock()
	if _, ok := m.once[key]; ok {
		m.once[key] = now
		return false
	}
	m.once = sweepMemoryInsert(m.once, key, now)
	return true
}

// hasOnce reports whether key was recorded.
func (m *sweepMemory) hasOnce(key string, now time.Time) bool {
	m.onceMu.Lock()
	defer m.onceMu.Unlock()
	if _, ok := m.once[key]; !ok {
		return false
	}
	m.once[key] = now
	return true
}

// markHoldLogged records a logged hold and reports whether this is its
// first report.
func (m *sweepMemory) markHoldLogged(key string, now time.Time) bool {
	m.onceMu.Lock()
	defer m.onceMu.Unlock()
	if _, ok := m.holdsLogged[key]; ok {
		m.holdsLogged[key] = now
		return false
	}
	m.holdsLogged = sweepMemoryInsert(m.holdsLogged, key, now)
	return true
}

// sweepMemoryInsert adds key to set, first dropping marks idle past
// sweepMemoryIdle once the set is large. The caller holds the lock.
func sweepMemoryInsert(set map[string]time.Time, key string, now time.Time) map[string]time.Time {
	if set == nil {
		set = make(map[string]time.Time)
	}
	if len(set) >= sweepMemoryPruneAt {
		for k, at := range set {
			if now.Sub(at) >= sweepMemoryIdle {
				delete(set, k)
			}
		}
	}
	set[key] = now
	return set
}

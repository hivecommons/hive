package github

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Base red across passes (projectbluefin/common#1684, chairlift#811).
//
// enrichBaseBranchRed re-reads the base tip on every pass, and a pass that
// missed the base's red sent the PR back to its author: the tip had just
// moved and its run of the check was still queued or in progress, or the
// listing failed. On common#1684 one head moved between 060 and 072 ten
// times in an afternoon while the same E2E check stayed red on main, and
// every 072 queued a fix for the author that the next pass withdrew.
//
// The client therefore remembers, per base, the checks it last saw red. A
// check leaves the set only when a read sees the tip's run of it complete
// with a non-red conclusion; a run still queued or in progress, a tip with
// no run of it yet, or a failed listing keeps it. A check not seen red for
// baseRedMemoryTTL is dropped, so a base nobody reads again cannot pin a
// stale red.
const baseRedMemoryTTL = 2 * time.Hour

// baseCheckState is one check's latest run on a base tip.
type baseCheckState int

const (
	// baseCheckRunning: queued or in progress, so it says nothing yet.
	baseCheckRunning baseCheckState = iota
	baseCheckRed
	baseCheckGreen
)

// baseRedMemory is the per-base red set. The zero value is ready to use.
type baseRedMemory struct {
	mu sync.Mutex
	// bases maps baseRedMemoryKey → check name → when it was last seen red.
	bases map[string]map[string]time.Time
	now   func() time.Time
}

func baseRedMemoryKey(owner, repo, ref string) string {
	return strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(repo) + "@" + strings.TrimSpace(ref))
}

func (m *baseRedMemory) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// observe folds one successful read of a base tip into the memory and
// returns the base's red set: the checks red on the tip now, plus the
// remembered ones the tip has not yet shown green.
func (m *baseRedMemory) observe(key string, states map[string]baseCheckState) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	if m.bases == nil {
		m.bases = map[string]map[string]time.Time{}
	}
	checks := m.bases[key]
	if checks == nil {
		checks = map[string]time.Time{}
		m.bases[key] = checks
	}
	for name, st := range states {
		switch st {
		case baseCheckRed:
			checks[name] = now
		case baseCheckGreen:
			delete(checks, name)
		}
	}
	return m.liveLocked(key, now)
}

// remembered is the base's red set for a pass whose read failed, or nil.
func (m *baseRedMemory) remembered(key string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveLocked(key, m.clock())
}

// liveLocked drops the base's checks past the TTL and returns the rest.
func (m *baseRedMemory) liveLocked(key string, now time.Time) map[string]bool {
	checks := m.bases[key]
	var red map[string]bool
	for name, at := range checks {
		if now.Sub(at) >= baseRedMemoryTTL {
			delete(checks, name)
			continue
		}
		if red == nil {
			red = map[string]bool{}
		}
		red[name] = true
	}
	if len(checks) == 0 {
		delete(m.bases, key)
	}
	return red
}

// snapshot is every remembered base's red checks, sorted, keyed by
// baseRedMemoryKey.
func (m *baseRedMemory) snapshot() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	out := map[string][]string{}
	for key := range m.bases {
		var names []string
		for name := range m.liveLocked(key, now) {
			names = append(names, name)
		}
		if len(names) > 0 {
			sort.Strings(names)
			out[key] = names
		}
	}
	return out
}

// BaseRedSnapshot returns the checks this client last saw red on each base
// branch tip and has not seen turn green since, keyed by "owner/repo@base"
// (lower case). The stage reconciler uses it to keep a PR head the
// CI-enriched snapshot has not read yet out of the repair stages when its
// only red checks are the base's (BaseRedRememberedKey).
func (c *Client) BaseRedSnapshot() map[string][]string {
	if c == nil {
		return nil
	}
	return c.baseRed.snapshot()
}

// BaseRedRememberedKey is the BaseRedSnapshot key for repo ("owner/name")
// and base ref.
func BaseRedRememberedKey(repo, baseRef string) string {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok {
		return ""
	}
	return baseRedMemoryKey(owner, name, baseRef)
}

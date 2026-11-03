package automerge

import (
	"strings"
	"sync"
	"time"
)

// MergeQueueBases remembers base branches whose ruleset refused a direct
// merge as merge-queue-only (hivecommons/hive#14068). The hive merges over
// REST and cannot enqueue, so no sweep can merge a PR into such a base. The
// sweeps read it to skip the merge call; the merge-eligibility classifier
// reads it so the dashboard does not call such a PR merge-eligible.
//
// An entry expires after mergeQueueBaseRecheck, and the next merge attempt
// on that base finds out again, so removing the queue rule is picked up
// without a restart. The memory is in-process only: after a restart a base
// is unknown until a sweep tries to merge into it once.
//
// The current-base gate also reads it (HoldBaseRefresh), over the longer
// mergeQueueBaseRefreshHold, so the hive stops merging the base into a PR it
// can never merge directly (hivecommons/hive#13634, #14068).
type MergeQueueBases struct {
	mu   sync.Mutex
	seen map[string]mergeQueueBase
}

// mergeQueueBase is one remembered base. refused is the last time GitHub
// refused a direct merge into it as merge-queue-only; held is the last time
// the current-base gate kept a behind head off it instead of updating it.
type mergeQueueBase struct {
	refused time.Time
	held    time.Time
}

// NewMergeQueueBases returns an empty memory.
func NewMergeQueueBases() *MergeQueueBases {
	return &MergeQueueBases{seen: make(map[string]mergeQueueBase)}
}

func mergeQueueBaseKey(repo, base string) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strings.TrimSpace(base)
}

// Note records that base of repo ("owner/name") refused a direct merge at now.
func (m *MergeQueueBases) Note(repo, base string, now time.Time) {
	if m == nil || strings.TrimSpace(base) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]mergeQueueBase)
	}
	key := mergeQueueBaseKey(repo, base)
	entry := m.seen[key]
	entry.refused = now
	m.seen[key] = entry
}

// Required reports whether base of repo ("owner/name") refused a direct merge
// as merge-queue-only within mergeQueueBaseRecheck of now, or the current-base
// gate held a behind head on it within mergeQueueBaseRecheck (HoldBaseRefresh).
func (m *MergeQueueBases) Required(repo, base string, now time.Time) bool {
	if m == nil || strings.TrimSpace(base) == "" {
		return false
	}
	key := mergeQueueBaseKey(repo, base)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.seen[key]
	if !ok {
		return false
	}
	if now.Sub(entry.refused) >= mergeQueueBaseRefreshHold {
		delete(m.seen, key)
		return false
	}
	last := entry.refused
	if entry.held.After(last) {
		last = entry.held
	}
	return now.Sub(last) < mergeQueueBaseRecheck
}

// HoldBaseRefresh reports whether base of repo ("owner/name") refused a direct
// merge as merge-queue-only within mergeQueueBaseRefreshHold of now, so the
// current-base gate must not merge the base into a behind head
// (hivecommons/hive#13634, #14068). The hive cannot enqueue, the merge queue
// tests base plus PR itself, and each refresh only moved the head off its
// review (projectbluefin/finpilot#599: three refreshes in a day, each
// followed by a wait for a fresh review, never a merge).
//
// A true answer also marks the base held, which keeps Required true for
// mergeQueueBaseRecheck: the sweep then holds the behind head at
// merge-queue-required instead of re-probing the base with a direct merge of
// a head that does not contain it (if the queue rule had been removed, that
// probe would merge a stale head past auto_merge.require_current_base). The
// direct-merge probe still runs on a head that is current once no behind
// head was held for mergeQueueBaseRecheck; a hold never extends refused, so
// with no new refusal the gate goes back to updating after
// mergeQueueBaseRefreshHold.
func (m *MergeQueueBases) HoldBaseRefresh(repo, base string, now time.Time) bool {
	if m == nil || strings.TrimSpace(base) == "" {
		return false
	}
	key := mergeQueueBaseKey(repo, base)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.seen[key]
	if !ok {
		return false
	}
	if now.Sub(entry.refused) >= mergeQueueBaseRefreshHold {
		delete(m.seen, key)
		return false
	}
	entry.held = now
	m.seen[key] = entry
	return true
}

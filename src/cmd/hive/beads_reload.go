package main

import (
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/beads"
)

// beadReloadWarner suppresses repeats of an identical per-agent bead-store
// reload failure. The eval-cycle reload runs forever, so a store the hub
// cannot read (e.g. a beads.json owned 0600 by a per-agent uid, #5505)
// produced the SAME warn line every cycle indefinitely — retry spam that
// buried the one occurrence carrying the actionable repair hint. The first
// failure per agent logs at WARN; identical repeats demote to DEBUG; a
// changed error text warns again (new information); success clears the entry
// so a future regression warns at WARN again. Mirrors the warnDeduper pattern
// in pkg/agent/permissions_watcher.go.
type beadReloadWarner struct {
	mu   sync.Mutex
	seen map[string]string // agent -> last error text
}

// shouldWarn records the failure and reports whether it is new (first failure
// for this agent, or the error text changed since last time).
func (w *beadReloadWarner) shouldWarn(agent, errText string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = make(map[string]string)
	}
	if prev, ok := w.seen[agent]; ok && prev == errText {
		return false
	}
	w.seen[agent] = errText
	return true
}

// clear forgets an agent's recorded failure so the next one warns at WARN
// again. Called on a successful reload — the condition changed, so a future
// failure is a new finding, not a repeat.
func (w *beadReloadWarner) clear(agent string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, agent)
}

// beadReloadWarns is the shared dedupe state for reloadBeadStores. Package
// level because the eval cycle is re-entered as a plain function call.
var beadReloadWarns = &beadReloadWarner{}

// reloadBeadStores re-reads every agent's bead store from disk (agents write
// beads via the bd CLI, so in-memory stores go stale between eval cycles),
// logging each distinct failure once at WARN and identical repeats at DEBUG.
func reloadBeadStores(beadStores map[string]*beads.Store, logger *slog.Logger) {
	for name, store := range beadStores {
		err := store.Reload()
		if err == nil {
			beadReloadWarns.clear(name)
			continue
		}
		if beadReloadWarns.shouldWarn(name, err.Error()) {
			logger.Warn("failed to reload beads from disk",
				"agent", name,
				"error", err,
				"note", "identical repeats logged at debug until this changes",
			)
		} else {
			logger.Debug("failed to reload beads from disk", "agent", name, "error", err)
		}
	}
}

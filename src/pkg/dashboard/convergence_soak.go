package dashboard

import (
	"net/http"
	"sync"
)

// ── #4263: fixed-commit soak telemetry ─────────────────────────────────────────
//
// The maintainer's comparison requirement: an owner must be able to hold the
// Hive commit, models, prompts/policies, and workload constant while changing
// ONLY the convergence rollout mode, and later compare what each treatment
// did. That comparison needs durable per-eval-cycle facts attributed to the
// running commit and the captured (mode, generation) pair — which is exactly
// one bounded row per enrolled pass, never an unbounded subject-timeseries
// store (subject-level snapshots stay ephemeral in the #4246 diagnostics).
//
// Pattern: budget-window-history.json (#4298) — a small leaf-locked ring,
// seeded on startup, snapshotted newest-first, persisted atomically by the
// main persist loop on the same cadence as the other series.

// convergenceSoakMaxEntries caps the ring. At the default 60s eval cadence,
// 2880 entries is two days of continuous soak per treatment leg — enough to
// compare off/shadow/enforce runs back to back — while staying a few hundred
// KB on disk. A named constant so retention is a deliberate number.
const convergenceSoakMaxEntries = 2880

// convergenceSoakEnrolledPath names the one currently enrolled guard. Later
// guard enrollments add their own value; the field exists so records from
// different enrolled paths can never be conflated.
const convergenceSoakEnrolledPath = "internal_kick_dispatch"

// ConvergenceSoakEntry is one enrolled evaluation pass, as the soak endpoint
// serves it and the persist loop stores it.
//
// Timestamps are epoch milliseconds, matching every other history series the
// dashboard persists. Commit and Mode/Generation are recorded PER ROW rather
// than read live, for the same reason budget history stores its limit: a later
// upgrade or mode flip must not rewrite what past rows ran under.
type ConvergenceSoakEntry struct {
	// Timestamp is when the pass completed its convergence projection.
	Timestamp int64 `json:"timestamp"`
	// Commit is the ldflags-baked short SHA of the RUNNING binary — the
	// fixed-commit attribution anchor (pattern: pkg/tracing reach counters).
	Commit string `json:"commit"`
	// Mode and Generation are the pair captured at the start of the pass; every
	// candidate in the pass was judged under them.
	Mode       string `json:"mode"`
	Generation uint64 `json:"generation"`
	// EnrolledPath names the guard this row measured (convergenceSoakEnrolledPath).
	EnrolledPath string `json:"enrolled_path"`
	// RawIssues / Admitted / Blocked / Unknown count the pass's candidate
	// dispositions: the raw enumerated population, the admitted projection, and
	// the withheld findings split by established-blocked vs irreducibly-unknown.
	RawIssues int `json:"raw_issues"`
	Admitted  int `json:"admitted"`
	Blocked   int `json:"blocked"`
	Unknown   int `json:"unknown"`
	// PartialLedger records whether the sweep read less than the full bead
	// ledger (the #3904 partial-coverage compromise was in force).
	PartialLedger bool `json:"partial_ledger"`
	// WouldDiffer is true when enforcement would have changed (or, in enforce,
	// DID change) the dispatched population — the shadow-vs-baseline fact the
	// comparison exists to collect.
	WouldDiffer bool `json:"would_differ"`
	// Enforced records whether the withheld candidates were actually removed
	// from the dispatched population (mode "enforce") or only reported.
	Enforced bool `json:"enforced"`
	// DecisionLatencyMs is how long the pass's observation + evaluation took,
	// for cross-treatment overhead comparison.
	DecisionLatencyMs int64 `json:"decision_latency_ms"`
}

// convergenceSoakTracker is the bounded ring. Leaf-locked: nothing here calls
// back into Server, so the mutex is taken and released inside one method.
type convergenceSoakTracker struct {
	mu sync.Mutex
	// entries holds passes oldest-first.
	entries []ConvergenceSoakEntry
}

func (t *convergenceSoakTracker) record(e ConvergenceSoakEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, e)
	if len(t.entries) > convergenceSoakMaxEntries {
		t.entries = t.entries[len(t.entries)-convergenceSoakMaxEntries:]
	}
}

// snapshot returns a copy of the recorded passes, newest FIRST — the order an
// operator reads them in, and the order the API serves.
func (t *convergenceSoakTracker) snapshot() []ConvergenceSoakEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ConvergenceSoakEntry, 0, len(t.entries))
	for i := len(t.entries) - 1; i >= 0; i-- {
		out = append(out, t.entries[i])
	}
	return out
}

// seed restores persisted telemetry on startup. Entries arrive newest-first
// (the order snapshot emits) and are stored oldest-first internally, so a
// save/load round trip preserves order. Over-long input is truncated to the
// newest entries rather than rejected. Unlike reach counters, rows from OTHER
// commits are deliberately KEPT: the whole point of the soak file is comparing
// treatments across a fixed commit — and detecting when the commit was NOT
// fixed — so history must survive both restarts and upgrades, attributed row
// by row.
func (t *convergenceSoakTracker) seed(entries []ConvergenceSoakEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = t.entries[:0]
	for i := len(entries) - 1; i >= 0; i-- {
		t.entries = append(t.entries, entries[i])
	}
	if len(t.entries) > convergenceSoakMaxEntries {
		t.entries = t.entries[len(t.entries)-convergenceSoakMaxEntries:]
	}
}

// convergenceSoak lazily constructs the tracker so a zero-value Server works
// in tests without a constructor change (the budgetWindows pattern).
func (s *Server) convergenceSoak() *convergenceSoakTracker {
	s.convergenceSoakOnce.Do(func() {
		s.convergenceSoakTrk = &convergenceSoakTracker{}
	})
	return s.convergenceSoakTrk
}

// RecordConvergenceSoak appends one enrolled pass. The running commit is
// stamped here so every caller gets consistent attribution; a missing enrolled
// path gets the current one. Timestamp must be set by the caller (it knows
// when the pass ran).
func (s *Server) RecordConvergenceSoak(e ConvergenceSoakEntry) {
	if e.Commit == "" {
		e.Commit = versionShort
	}
	if e.EnrolledPath == "" {
		e.EnrolledPath = convergenceSoakEnrolledPath
	}
	s.convergenceSoak().record(e)
}

// ConvergenceSoakHistory returns the recorded passes, newest first. Empty on a
// hive that has never run with the toggle on — ordinary, never an error.
func (s *Server) ConvergenceSoakHistory() []ConvergenceSoakEntry {
	return s.convergenceSoak().snapshot()
}

// SeedConvergenceSoak restores persisted telemetry on startup.
func (s *Server) SeedConvergenceSoak(entries []ConvergenceSoakEntry) {
	s.convergenceSoak().seed(entries)
}

// SetMutationStats wires the live mutation-boundary counters into convergence
// status responses. Nil leaves the response at its zero/default shape.
func (s *Server) SetMutationStats(stats func() interface{}) {
	if s == nil {
		return
	}
	s.mutationStats = stats
}

// handleConvergenceSoak serves GET /api/convergence/soak — the longitudinal
// read/export path. OWNER-ONLY, matching the settings surface that controls
// the mode: the soak comparison is an operator concern and the rows carry
// repository work identifiers.
func (s *Server) handleConvergenceSoak(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	mode, generation := s.ConvergenceModeGeneration()
	mutationStats := interface{}(nil)
	if s.mutationStats != nil {
		mutationStats = s.mutationStats()
	}
	jsonResponse(w, map[string]interface{}{
		"commit":        versionShort,
		"mode":          mode,
		"generation":    generation,
		"enrolled_path": convergenceSoakEnrolledPath,
		"max_entries":   convergenceSoakMaxEntries,
		"mutation":      mutationStats,
		"entries":       s.ConvergenceSoakHistory(),
	})
}

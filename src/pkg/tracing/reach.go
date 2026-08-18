// Component reach counters (#3993, phase 2a of #3973).
//
// Every span started through StartSpan increments an in-process counter keyed
// by (component, running commit). This is design decision D2 of #3973: the
// counters are DELIBERATELY independent of the OTel exporter. The `otel:`
// block is opt-in per spoke and most of the fleet exports nowhere (including
// all pull-only spokes), so a span-backend query would report ~0 reach for
// most hives and call merged code "unused" — the same trap the phase-1
// anchoring rule exists to prevent, one level up. Counting in-process means
// every spoke reports reach over the heartbeat (D1) with zero new network
// dependencies; spans remain the deep-inspection layer where backends exist.
//
// Hot-path cost: recording a span is a read-locked map lookup plus a handful
// of atomic adds — no allocation, no write lock, no time formatting. The write
// lock is taken only the first time a component name is seen (bounded at
// MaxReachComponents times per process lifetime).
package tracing

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// MaxReachComponents caps how many distinct components the reach registry
	// tracks. The set rides every heartbeat and is persisted to
	// /data/reach-state.json, so it must be bounded: span names are code-owned
	// today (~a dozen components), but a future call site interpolating input
	// into a span name would otherwise grow the heartbeat and the hub's
	// per-hive storage without limit. 64 is several times the realistic
	// component count (one per top-level package that emits spans) while
	// keeping the worst-case payload small. Beyond the cap spans are counted
	// in OverflowSpans and the truncation is LOGGED once per component —
	// never silent (#3973 resolved open question OQ-1). The hub clips
	// hostile/oversized reports to this same constant on receive.
	MaxReachComponents = 64

	// reachWindowSeconds is the length of the rolling activity bucket, in
	// seconds: one hour. Phase 2c computes error-rate deltas per deploy window
	// from consecutive heartbeat reports, and the cumulative-since-boot
	// counters alone cannot expose "what happened recently" without the hub
	// diffing beats — a 1h tumbling bucket gives 2c a recent-activity signal
	// that survives missed beats. Seconds because the counters store unix
	// seconds in atomics.
	reachWindowSeconds = int64(60 * 60)

	// maxOverflowLoggedComponents bounds the once-per-component truncation log
	// set. The set exists so overflow is never silent, but it must not itself
	// become an unbounded map fed by hostile span names; past this many
	// distinct overflowed names one final aggregate warning is logged and
	// further new names only increment the overflow counters.
	maxOverflowLoggedComponents = 256
)

// ReachWindow is the rolling 1h activity bucket inside a ReachEntry. Start is
// when the current bucket began (RFC3339 UTC); the counts cover spans since
// then. A bucket older than 1h is reset on the next recorded span, so a stale
// Start means the component has been idle since Start+1h.
type ReachWindow struct {
	Start      string `json:"start"`
	SpansTotal int64  `json:"spans_total"`
	SpansError int64  `json:"spans_error"`
}

// ReachEntry is one (component, running commit) reach counter as it appears on
// the heartbeat, in /data/reach-state.json, and in the hub's per-hive storage.
//
// CONTRACT (#3993 → #3994): the JSON keys component / commit / spans_total /
// spans_error / first_seen / last_seen are phase 2b's read interface and are
// fixed by the #3973 epic — do not rename or omit them. Timestamps are RFC3339
// UTC. The counts are cumulative since boot (or since the persisted state was
// loaded); Window1h is the additive rolling bucket for 2c.
type ReachEntry struct {
	Component  string       `json:"component"`
	Commit     string       `json:"commit"`
	SpansTotal int64        `json:"spans_total"`
	SpansError int64        `json:"spans_error"`
	FirstSeen  string       `json:"first_seen"`
	LastSeen   string       `json:"last_seen"`
	Window1h   *ReachWindow `json:"window_1h,omitempty"`
}

// ReachReport is the full reach snapshot: what the spoke persists, what rides
// the heartbeat as component_reach, and what the hub stores per hive. Entries
// are sorted by component name so reports are deterministic and diffable.
// OverflowSpans counts spans whose component was refused by the
// MaxReachComponents cap; OverflowComponents counts the distinct refused
// component names observed (a floor once the logging set saturates).
type ReachReport struct {
	Entries            []ReachEntry `json:"entries"`
	OverflowSpans      int64        `json:"overflow_spans,omitempty"`
	OverflowComponents int          `json:"overflow_components,omitempty"`
}

// reachCounters is the per-component hot-path state. All fields are atomics so
// concurrent StartSpan/End calls never contend on a lock; winMu is taken only
// on the rare window rollover (at most once per component per hour).
type reachCounters struct {
	firstSeen atomic.Int64 // unix seconds; 0 = never seen (set once via CAS)
	lastSeen  atomic.Int64 // unix seconds
	total     atomic.Int64
	errors    atomic.Int64
	winStart  atomic.Int64 // unix seconds; start of the current 1h bucket
	winTotal  atomic.Int64
	winErrors atomic.Int64
	winMu     sync.Mutex // serializes bucket resets only
}

// rollWindow resets the 1h bucket when it has expired. The fast path is a
// single atomic load; the mutex is only for the rollover itself. A counter
// racing the reset can land on either side of the zeroing — an error of at
// most a few spans once per hour, which is noise at reach granularity and the
// deliberate price of a lock-free increment path.
func (c *reachCounters) rollWindow(now int64) {
	ws := c.winStart.Load()
	if ws != 0 && now-ws < reachWindowSeconds {
		return
	}
	c.winMu.Lock()
	defer c.winMu.Unlock()
	ws = c.winStart.Load()
	if ws == 0 || now-ws >= reachWindowSeconds {
		c.winStart.Store(now)
		c.winTotal.Store(0)
		c.winErrors.Store(0)
	}
}

// reachState is the process-global registry. There is exactly one running
// commit per process (the ldflags-baked SHA), so the in-memory key is the
// component alone; the commit is attached when snapshotting. Keying the
// PERSISTED entries by commit is what makes a new binary start fresh counters
// naturally — LoadReachState resumes only entries whose commit matches.
type reachState struct {
	mu         sync.RWMutex
	commit     string
	logger     *slog.Logger
	components map[string]*reachCounters
	// overflowLogged holds component names whose truncation has already been
	// logged, so the "never silent" guarantee costs one log line per name, not
	// one per span. Bounded by maxOverflowLoggedComponents.
	overflowLogged       map[string]struct{}
	overflowLogSaturated bool
	overflowSpans        atomic.Int64
}

var globalReach = newReachState()

func newReachState() *reachState {
	return &reachState{
		components:     make(map[string]*reachCounters),
		overflowLogged: make(map[string]struct{}),
	}
}

func (r *reachState) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// counters returns the counter block for component, creating it under the
// write lock on first sight. Returns nil when the MaxReachComponents cap
// refuses a new component — the caller must count the span as overflow.
func (r *reachState) counters(component string) *reachCounters {
	r.mu.RLock()
	c := r.components[component]
	r.mu.RUnlock()
	if c != nil {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.components[component]; c != nil {
		return c
	}
	if len(r.components) >= MaxReachComponents {
		r.noteOverflowLocked(component)
		return nil
	}
	c = &reachCounters{}
	r.components[component] = c
	return c
}

// noteOverflowLocked logs a refused component's truncation exactly once per
// name (never silent — #3993), with the logging set itself bounded so hostile
// span names cannot grow it without limit. Caller holds r.mu.
func (r *reachState) noteOverflowLocked(component string) {
	if _, seen := r.overflowLogged[component]; seen {
		return
	}
	if len(r.overflowLogged) >= maxOverflowLoggedComponents {
		if !r.overflowLogSaturated {
			r.overflowLogSaturated = true
			r.log().Warn("reach: overflow component log set saturated — further new components are counted but no longer named (#3993)",
				"logged_names", len(r.overflowLogged),
				"cap", MaxReachComponents,
			)
		}
		return
	}
	r.overflowLogged[component] = struct{}{}
	r.log().Warn("reach: component cap reached — spans for this component are counted only in overflow_spans (#3993)",
		"component", component,
		"cap", MaxReachComponents,
	)
}

// recordReachStart counts a span start for component. Called from StartSpan on
// every span regardless of exporter configuration (D2).
func recordReachStart(component string) {
	c := globalReach.counters(component)
	if c == nil {
		globalReach.overflowSpans.Add(1)
		return
	}
	now := time.Now().Unix()
	c.firstSeen.CompareAndSwap(0, now)
	c.lastSeen.Store(now)
	c.rollWindow(now)
	c.total.Add(1)
	c.winTotal.Add(1)
}

// recordReachError counts a span that ended with error status. Read-only
// lookup: a component refused by the cap was already counted as overflow at
// start, so a miss here is deliberately dropped rather than re-noted.
func recordReachError(component string) {
	globalReach.mu.RLock()
	c := globalReach.components[component]
	globalReach.mu.RUnlock()
	if c == nil {
		return
	}
	now := time.Now().Unix()
	c.lastSeen.Store(now)
	c.rollWindow(now)
	c.errors.Add(1)
	c.winErrors.Add(1)
}

// formatReachTime renders a unix-seconds counter timestamp as RFC3339 UTC, the
// format everything else on the heartbeat uses. Zero (never seen) is "".
func formatReachTime(unixSec int64) string {
	if unixSec == 0 {
		return ""
	}
	return time.Unix(unixSec, 0).UTC().Format(time.RFC3339)
}

// ReachSnapshot returns the current reach report, or nil when nothing has been
// recorded — nil keeps component_reach off the heartbeat entirely (omitempty)
// for a spoke that has emitted no spans yet.
func ReachSnapshot() *ReachReport {
	r := globalReach
	r.mu.RLock()
	defer r.mu.RUnlock()
	overflowSpans := r.overflowSpans.Load()
	if len(r.components) == 0 && overflowSpans == 0 {
		return nil
	}
	report := &ReachReport{
		Entries:            make([]ReachEntry, 0, len(r.components)),
		OverflowSpans:      overflowSpans,
		OverflowComponents: len(r.overflowLogged),
	}
	for name, c := range r.components {
		entry := ReachEntry{
			Component:  name,
			Commit:     r.commit,
			SpansTotal: c.total.Load(),
			SpansError: c.errors.Load(),
			FirstSeen:  formatReachTime(c.firstSeen.Load()),
			LastSeen:   formatReachTime(c.lastSeen.Load()),
		}
		if ws := c.winStart.Load(); ws != 0 {
			entry.Window1h = &ReachWindow{
				Start:      formatReachTime(ws),
				SpansTotal: c.winTotal.Load(),
				SpansError: c.winErrors.Load(),
			}
		}
		report.Entries = append(report.Entries, entry)
	}
	sort.Slice(report.Entries, func(i, j int) bool {
		return report.Entries[i].Component < report.Entries[j].Component
	})
	return report
}

// LoadReachState installs the running commit + logger on the reach registry
// and resumes counters from a previous run of the SAME commit persisted at
// path (its own file, /data/reach-state.json — deliberately not the main
// state file, per #3973 resolved OQ-2). Entries persisted by a different
// commit are dropped: the counters are keyed by commit, so a freshly upgraded
// binary starts fresh keys naturally and never inherits reach it did not earn.
// A missing file is a normal first boot, not an error.
func LoadReachState(path, commit string, logger *slog.Logger) error {
	r := globalReach
	r.mu.Lock()
	r.commit = commit
	r.logger = logger
	r.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read reach state: %w", err)
	}
	var report ReachReport
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("parse reach state: %w", err)
	}

	now := time.Now().Unix()
	resumed, dropped := 0, 0
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range report.Entries {
		if e.Component == "" || e.Commit != commit {
			dropped++
			continue
		}
		if len(r.components) >= MaxReachComponents {
			// A hand-edited or corrupt file must not bypass the cap.
			r.noteOverflowLocked(e.Component)
			continue
		}
		c := &reachCounters{}
		c.total.Store(e.SpansTotal)
		c.errors.Store(e.SpansError)
		if t, err := time.Parse(time.RFC3339, e.FirstSeen); err == nil {
			c.firstSeen.Store(t.Unix())
		}
		if t, err := time.Parse(time.RFC3339, e.LastSeen); err == nil {
			c.lastSeen.Store(t.Unix())
		}
		// Resume the rolling bucket only while it is still current; an expired
		// bucket restarts on the next span rather than resurrecting stale
		// counts into a fresh hour.
		if e.Window1h != nil {
			if t, err := time.Parse(time.RFC3339, e.Window1h.Start); err == nil && now-t.Unix() < reachWindowSeconds {
				c.winStart.Store(t.Unix())
				c.winTotal.Store(e.Window1h.SpansTotal)
				c.winErrors.Store(e.Window1h.SpansError)
			}
		}
		r.components[e.Component] = c
		resumed++
	}
	// Overflow is not commit-keyed in the file, so it is only trustworthy when
	// the file demonstrably belongs to THIS commit (something resumed). A file
	// written by a previous binary starts the new one at zero overflow, the
	// same way its component counters start fresh.
	if resumed > 0 && report.OverflowSpans > 0 {
		r.overflowSpans.Store(report.OverflowSpans)
	}
	if resumed > 0 || dropped > 0 {
		r.log().Info("reach state loaded (#3993)",
			"path", path, "commit", commit, "resumed", resumed, "dropped_other_commit", dropped)
	}
	return nil
}

// SaveReachState writes the current reach report to path atomically
// (tmp + rename, matching snapshot.SaveState). Called on the spoke's existing
// state-save cadence — every eval tick and at shutdown — never on the span
// hot path. Nothing recorded yet writes nothing (and is not an error).
func SaveReachState(path string) error {
	report := ReachSnapshot()
	if report == nil {
		return nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal reach state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write reach state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename reach state: %w", err)
	}
	return nil
}

// reachSpan wraps the span returned by StartSpan so the reach error counter
// (#3993) can observe "ended with error status" without a SpanProcessor — a
// processor only runs when the SDK provider is installed (exporter
// configured), and D2 requires counting without one. One small allocation per
// span, on the span object itself (which is already heap-bound as an
// interface); the counter increments stay atomic and lock-free.
//
// Status precedence follows the OTel spec: Ok is final and cannot be
// downgraded back to Error by a later SetStatus.
type reachSpan struct {
	trace.Span
	component string
	status    atomic.Int32 // last effective codes.Code
	ended     atomic.Bool
}

func (s *reachSpan) SetStatus(code codes.Code, description string) {
	switch code {
	case codes.Ok:
		s.status.Store(int32(codes.Ok))
	case codes.Error:
		if codes.Code(s.status.Load()) != codes.Ok {
			s.status.Store(int32(codes.Error))
		}
	}
	s.Span.SetStatus(code, description)
}

// End counts the error exactly once even if a defer and an explicit call both
// End the span, then delegates.
func (s *reachSpan) End(options ...trace.SpanEndOption) {
	if s.ended.CompareAndSwap(false, true) && codes.Code(s.status.Load()) == codes.Error {
		recordReachError(s.component)
	}
	s.Span.End(options...)
}

// resetReachForTest replaces the global registry, giving tests a clean slate
// with a chosen commit and logger. Test-only; the production registry is
// installed once by LoadReachState at boot.
func resetReachForTest(commit string, logger *slog.Logger) {
	fresh := newReachState()
	fresh.commit = commit
	fresh.logger = logger
	globalReach = fresh
}

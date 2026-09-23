package persona

import (
	"fmt"
	"strings"
	"time"
)

// Persona learning (hivecommons/hive#8363) turns a small set of explicit,
// auditable behaviour signals into PROPOSED persona changes. Nothing in this
// file mutates depth or summary length on its own: a suggestion is only
// applied when the user accepts it, and the evidence that produced it is kept
// on the record so the user can always see why a setting moved.
//
// This package is deliberately stdlib-only. The learning code path has no
// import of ACMM, agent mode, or policy packages, and the import-boundary
// conformance test in learning_test.go keeps it that way: a persona change
// can never change what an agent may do.

// Signal names. Each one is an explicit, countable action the user took in
// the chat spine; no transcript text is ever stored.
const (
	// SignalExpanded: the user asked for the technical rendering after a
	// summary (`!runs <key> more`).
	SignalExpanded = "expanded"
	// SignalSkipped: the user acted on a summary (approve/reject) without
	// expanding it.
	SignalSkipped = "skipped"
	// SignalReAsked: the user asked for the same summary again without
	// expanding it, a follow-up the technical rendering would have answered.
	SignalReAsked = "re-asked"
)

// Suggestion keys name the persona field a suggestion proposes to move.
const (
	SuggestionKeyDepth         = "depth"
	SuggestionKeySummaryLength = "summary_length"
)

// Defaults for the adjustment rule. Operators override them through
// persona.learning.threshold and persona.learning.window_days.
const (
	// DefaultLearningThreshold is the number of same-direction signals inside
	// one window that produces a suggestion.
	DefaultLearningThreshold = 5
	// DefaultLearningWindowDays bounds the evidence window; counters reset when
	// the window rolls over.
	DefaultLearningWindowDays = 7

	hoursPerDay = 24
)

// LearningConfig is the operator configuration for persona learning. It is
// default off; an absent block is zero behaviour change.
type LearningConfig struct {
	Enabled    bool
	Threshold  int
	WindowDays int
}

// EffectiveThreshold returns the configured threshold or the default.
func (c LearningConfig) EffectiveThreshold() int {
	if c.Threshold <= 0 {
		return DefaultLearningThreshold
	}
	return c.Threshold
}

// EffectiveWindowDays returns the configured window in days or the default.
func (c LearningConfig) EffectiveWindowDays() int {
	if c.WindowDays <= 0 {
		return DefaultLearningWindowDays
	}
	return c.WindowDays
}

// EffectiveWindow returns the evidence window as a duration.
func (c LearningConfig) EffectiveWindow() time.Duration {
	return time.Duration(c.EffectiveWindowDays()) * hoursPerDay * time.Hour
}

// Signals are the per-user, per-window counters. They are stored on the
// persona record as counts, never as transcripts.
type Signals struct {
	WindowStart time.Time `json:"window_start,omitempty"`
	Expanded    int       `json:"expanded,omitempty"`
	Skipped     int       `json:"skipped,omitempty"`
	ReAsked     int       `json:"re_asked,omitempty"`
}

// Total returns the number of signals recorded in the current window.
func (s Signals) Total() int {
	return s.Expanded + s.Skipped + s.ReAsked
}

// Suggestion is a proposed one-step change to one persona field. It is
// inert until the user accepts it.
type Suggestion struct {
	Key        string    `json:"key"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Evidence   string    `json:"evidence"`
	ProposedAt time.Time `json:"proposed_at"`
}

// Adjustment records the last accepted suggestion so `!persona show` can
// explain why a value moved and offer an undo.
type Adjustment struct {
	Key       string    `json:"key"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Evidence  string    `json:"evidence"`
	AppliedAt time.Time `json:"applied_at"`
}

// Learning is the learning state carried on a persona record.
type Learning struct {
	Signals        Signals      `json:"signals"`
	Suggestions    []Suggestion `json:"suggestions,omitempty"`
	LastAdjustment *Adjustment  `json:"last_adjustment,omitempty"`
}

func (l *Learning) clone() *Learning {
	if l == nil {
		return nil
	}
	out := &Learning{Signals: l.Signals}
	if len(l.Suggestions) > 0 {
		out.Suggestions = append([]Suggestion(nil), l.Suggestions...)
	}
	if l.LastAdjustment != nil {
		adj := *l.LastAdjustment
		out.LastAdjustment = &adj
	}
	return out
}

func (r Record) learning() *Learning {
	if r.Learning == nil {
		return &Learning{}
	}
	return r.Learning.clone()
}

// Suggestions returns the pending suggestions in proposal order.
func (r Record) Suggestions() []Suggestion {
	if r.Learning == nil {
		return nil
	}
	return append([]Suggestion(nil), r.Learning.Suggestions...)
}

// RecordSignal counts one signal for the record at now and, when the
// same-direction count reaches the configured threshold inside the window,
// attaches a one-step suggestion. It never changes depth or summary length.
//
// A disabled configuration or a pinned record returns the record unchanged,
// so a user with learning off accumulates nothing and is never suggested to.
func (r Record) RecordSignal(signal string, now time.Time, cfg LearningConfig) (Record, error) {
	if !cfg.Enabled || r.Pinned {
		return r, nil
	}
	r = r.Normalize()
	state := r.learning()
	window := cfg.EffectiveWindow()
	if state.Signals.WindowStart.IsZero() || now.Sub(state.Signals.WindowStart) >= window {
		state.Signals = Signals{WindowStart: now}
	}
	switch signal {
	case SignalExpanded:
		state.Signals.Expanded++
	case SignalSkipped:
		state.Signals.Skipped++
	case SignalReAsked:
		state.Signals.ReAsked++
	default:
		return r, fmt.Errorf("unknown persona signal %q", signal)
	}

	threshold := cfg.EffectiveThreshold()
	days := cfg.EffectiveWindowDays()
	if more := state.Signals.Expanded + state.Signals.ReAsked; more >= threshold {
		evidence := formatEvidence(state.Signals.Expanded, "expansion", state.Signals.ReAsked, "re-ask", days)
		state.propose(r.stepUp(), evidence, now)
		// One step per window: the evidence is spent whether or not the record
		// had headroom, so a second week of expansions cannot stack.
		state.Signals = Signals{WindowStart: now}
	} else if state.Signals.Skipped >= threshold {
		evidence := formatEvidence(state.Signals.Skipped, "skip", 0, "", days)
		state.propose(r.stepDown(), evidence, now)
		state.Signals = Signals{WindowStart: now}
	}
	r.Learning = state
	return r, nil
}

// propose attaches the suggestion unless it has no target (the record is at
// the top or bottom of the ladder) or one is already pending for that key.
func (l *Learning) propose(s Suggestion, evidence string, now time.Time) {
	if s.Key == "" {
		return
	}
	for _, pending := range l.Suggestions {
		if pending.Key == s.Key {
			return
		}
	}
	s.Evidence = evidence
	s.ProposedAt = now
	l.Suggestions = append(l.Suggestions, s)
}

// stepUp returns the next "more detail" step, or an empty suggestion at the
// top: depth outcomes -> technical first, then summary length one notch.
func (r Record) stepUp() Suggestion {
	if r.Depth != DepthTechnical {
		return Suggestion{Key: SuggestionKeyDepth, From: r.Depth, To: DepthTechnical}
	}
	switch r.SummaryLength {
	case SummaryShort:
		return Suggestion{Key: SuggestionKeySummaryLength, From: SummaryShort, To: SummaryStandard}
	case SummaryStandard:
		return Suggestion{Key: SuggestionKeySummaryLength, From: SummaryStandard, To: SummaryDetailed}
	}
	return Suggestion{}
}

// stepDown returns the next "less detail" step, or an empty suggestion at
// the bottom: depth technical -> outcomes first, then summary length one notch.
func (r Record) stepDown() Suggestion {
	if r.Depth == DepthTechnical {
		return Suggestion{Key: SuggestionKeyDepth, From: DepthTechnical, To: DepthOutcomes}
	}
	switch r.SummaryLength {
	case SummaryDetailed:
		return Suggestion{Key: SuggestionKeySummaryLength, From: SummaryDetailed, To: SummaryStandard}
	case SummaryStandard:
		return Suggestion{Key: SuggestionKeySummaryLength, From: SummaryStandard, To: SummaryShort}
	}
	return Suggestion{}
}

// formatEvidence renders the non-zero counts behind a suggestion, for example
// "5 expansions in 7 days" or "3 expansions and 2 re-asks in 7 days". A zero
// count is omitted rather than rendered as "0 expansions".
func formatEvidence(primary int, primaryNoun string, secondary int, secondaryNoun string, days int) string {
	var parts []string
	if primary > 0 {
		parts = append(parts, plural(primary, primaryNoun))
	}
	if secondary > 0 {
		parts = append(parts, plural(secondary, secondaryNoun))
	}
	return fmt.Sprintf("%s in %d days", strings.Join(parts, " and "), days)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// AcceptSuggestion applies the 1-based nth pending suggestion, records it as
// the last adjustment, and clears the remaining suggestions and counters.
func (r Record) AcceptSuggestion(n int, now time.Time) (Record, Adjustment, error) {
	pending := r.Suggestions()
	if n < 1 || n > len(pending) {
		return r, Adjustment{}, fmt.Errorf("no persona suggestion #%d (%d pending)", n, len(pending))
	}
	chosen := pending[n-1]
	updated, err := r.Set(chosen.Key, chosen.To)
	if err != nil {
		return r, Adjustment{}, err
	}
	adj := Adjustment{Key: chosen.Key, From: chosen.From, To: chosen.To, Evidence: chosen.Evidence, AppliedAt: now}
	state := updated.learning()
	state.Suggestions = nil
	state.Signals = Signals{WindowStart: now}
	state.LastAdjustment = &adj
	updated.Learning = state
	return updated, adj, nil
}

// RejectSuggestions discards every pending suggestion and the counters that
// produced them. Depth and summary length are untouched.
func (r Record) RejectSuggestions(now time.Time) Record {
	if r.Learning == nil {
		return r
	}
	state := r.learning()
	state.Suggestions = nil
	state.Signals = Signals{WindowStart: now}
	r.Learning = state
	return r
}

// UndoAdjustment reverts the last accepted adjustment and pins the record so
// learning stops until the user unpins it.
func (r Record) UndoAdjustment(now time.Time) (Record, Adjustment, error) {
	if r.Learning == nil || r.Learning.LastAdjustment == nil {
		return r, Adjustment{}, fmt.Errorf("no persona adjustment to undo")
	}
	last := *r.Learning.LastAdjustment
	reverted, err := r.Set(last.Key, last.From)
	if err != nil {
		return r, Adjustment{}, err
	}
	state := reverted.learning()
	state.LastAdjustment = nil
	state.Suggestions = nil
	state.Signals = Signals{WindowStart: now}
	reverted.Learning = state
	reverted.Pinned = true
	return reverted, last, nil
}

// SetPinned pins or unpins the record. Pinning also drops pending
// suggestions; unpinning resumes signal collection from an empty window.
func (r Record) SetPinned(pinned bool, now time.Time) Record {
	r.Pinned = pinned
	if r.Learning != nil {
		state := r.learning()
		state.Suggestions = nil
		state.Signals = Signals{WindowStart: now}
		r.Learning = state
	}
	return r
}

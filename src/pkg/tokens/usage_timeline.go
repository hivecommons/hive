package tokens

import "sort"

// Backend identifiers carried on SessionSummary.Backend. They name the tool that
// produced a session, which is what decides whether that session's tokens can be
// placed on a timeline at all.
const (
	// BackendClaude is the Claude Code CLI. Its session JSONL records
	// per-assistant-message usage WITH a per-entry timestamp, so it is the only
	// backend that currently yields a UsageEvent timeline.
	BackendClaude = "claude"
	// BackendCopilot is the Copilot CLI. Its native events.jsonl accrues all
	// token usage in a single lump at session.shutdown, while MITM live capture
	// writes timestamped per-completion usage under the same backend.
	BackendCopilot = "copilot"
	// BackendBob is the bobshell backend. Its messages carry per-message usage
	// but the parser does not currently read per-message timestamps, and
	// sessions without explicit usage fall back to character estimates.
	BackendBob = "bob"
	// BackendInference is the MITM/inference sink path (bare-mode agents).
	BackendInference = "inference"
)

// maxUsageEventsPerSession bounds the retained per-message timeline for one
// session. A long agent run can emit many thousands of assistant messages, and
// the summary is persisted to /data/token-summary.json and shipped in API
// payloads, so an unbounded timeline is a real memory and payload hazard.
//
// The bound is enforced by COALESCING adjacent events into coarser time
// buckets, never by dropping them: total tokens are preserved exactly, only
// message-level resolution degrades. SessionSummary.UsageCoalesced reports how
// many raw events were folded so a consumer can see the timeline is coarse
// rather than assuming full fidelity. 2000 events keeps a worst-case session
// timeline near ~100KB while giving an interval join far finer grain than the
// minutes-to-hours spacing of audit events it is joined against.
const maxUsageEventsPerSession = 2000

// usageTimeline accumulates per-message usage during a scan and enforces the
// per-session bound by coalescing. Zero value is ready to use.
type usageTimeline struct {
	events    []UsageEvent
	coalesced int
}

// add records one per-message usage event. Events with no tokens at all are
// dropped — they carry no cost and would only dilute the timeline — but every
// event with any token count is retained, including ones with an unknown
// (zero) timestamp.
func (t *usageTimeline) add(e UsageEvent) {
	if e.Total() == 0 {
		return
	}
	if e.Coalesced == 0 {
		e.Coalesced = 1
	}
	t.events = append(t.events, e)
	if len(t.events) > maxUsageEventsPerSession*2 {
		t.compact()
	}
}

// compact halves the timeline by merging adjacent pairs of events. The merged
// event keeps the EARLIER timestamp and the model of the larger contributor, so
// an interval join places the merged tokens no later than the earliest message
// they came from. Token counts are summed, never discarded.
func (t *usageTimeline) compact() {
	t.sortEvents()
	merged := make([]UsageEvent, 0, len(t.events)/2+1)
	for i := 0; i < len(t.events); i += 2 {
		a := t.events[i]
		if i+1 >= len(t.events) {
			merged = append(merged, a)
			break
		}
		b := t.events[i+1]
		// The earlier timestamp wins; a zero (unknown) timestamp must not
		// win, since it would claim the merged tokens happened at the epoch.
		ts := a.TimestampMs
		if ts == 0 || (b.TimestampMs != 0 && b.TimestampMs < ts) {
			ts = b.TimestampMs
		}
		model := a.Model
		if b.Total() > a.Total() && b.Model != "" {
			model = b.Model
		}
		t.coalesced += a.Coalesced + b.Coalesced - 1
		merged = append(merged, UsageEvent{
			TimestampMs: ts,
			Model:       model,
			Coalesced:   a.Coalesced + b.Coalesced,
			Input:       a.Input + b.Input,
			Output:      a.Output + b.Output,
			CacheRead:   a.CacheRead + b.CacheRead,
			CacheCreate: a.CacheCreate + b.CacheCreate,
		})
	}
	t.events = merged
}

// sortEvents orders the timeline by timestamp ascending. Events with an unknown
// (zero) timestamp sort LAST, not first: sorting them to the front would make
// them look like the session's opening messages and hand their tokens to
// whatever interval starts the session.
func (t *usageTimeline) sortEvents() {
	sort.SliceStable(t.events, func(i, j int) bool {
		a, b := t.events[i].TimestampMs, t.events[j].TimestampMs
		if a == 0 || b == 0 {
			return b == 0 && a != 0
		}
		return a < b
	})
}

// finish returns the bounded, time-ordered timeline and the number of raw
// events that were coalesced into it.
func (t *usageTimeline) finish() ([]UsageEvent, int) {
	for len(t.events) > maxUsageEventsPerSession {
		t.compact()
	}
	t.sortEvents()
	if len(t.events) == 0 {
		return nil, 0
	}
	return t.events, t.coalesced
}

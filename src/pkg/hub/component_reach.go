package hub

// Hub receipt of spoke component-reach reports (#3993, phase 2a of #3973).
//
// The heartbeat's component_reach field is SPOKE INPUT parsed on the hub, so
// everything in it is bounded and sanitized before it touches the registry:
// entry count is clipped to tracing.MaxReachComponents (a hostile spoke must
// not grow hub memory or the persisted registry without limit), strings go
// through the identifier sanitizer with explicit length caps, counts are
// clamped non-negative, and timestamps must parse as RFC3339 or are dropped.
// The stored report is NEVER trusted for anything but storage here — no joins,
// no ancestry, no endpoint, no UI; that is #3994's job.

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/tracing"
)

const (
	// maxReachComponentRunes bounds a stored component name. Component names
	// are span-name prefixes ("governor", "agent", …) — the same identifier
	// class isValidName caps at 100 — so anything longer is garbage or abuse.
	maxReachComponentRunes = 100

	// maxReachCommitRunes bounds a stored commit id. A full git SHA is 40 hex
	// chars; nothing legitimate is longer.
	maxReachCommitRunes = 40

	// maxReachSpanCount bounds any single reach counter. 100 billion spans
	// mirrors the TotalTokens24h clamp: far beyond anything a real spoke can
	// emit between boots, small enough that a hostile value cannot overflow
	// downstream arithmetic.
	maxReachSpanCount = 100_000_000_000

	// maxReachOverflowComponents bounds the reported count of DISTINCT
	// refused component names. The spoke's own once-per-name overflow log set
	// is capped at 256; a report claiming more than a few times that is not
	// describing a real registry.
	maxReachOverflowComponents = 1024
)

// truncateReachRunes caps s at n runes. Rune-based like sanitizeField, so a
// multi-byte sequence can never be split into invalid UTF-8.
func truncateReachRunes(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// sanitizeReachTime accepts a spoke-reported timestamp only if it parses as
// RFC3339, re-rendered in canonical UTC form. Anything else becomes "" —
// which every reader must treat as "unknown", never as an epoch.
func sanitizeReachTime(s string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// sanitizeComponentReach validates, clips, and copies a spoke-reported reach
// report for storage on the registry entry. nil in (old spoke / nothing
// recorded yet) is nil out — "no data", never an error. The returned report
// never aliases the caller's payload. Entries past tracing.MaxReachComponents
// are dropped and surfaced in OverflowComponents so the clip is visible in the
// stored data rather than silent.
func sanitizeComponentReach(in *tracing.ReachReport) *tracing.ReachReport {
	if in == nil {
		return nil
	}
	out := &tracing.ReachReport{
		OverflowSpans:      clampInt64(in.OverflowSpans, 0, maxReachSpanCount),
		OverflowComponents: clampInt(in.OverflowComponents, 0, maxReachOverflowComponents),
	}
	entries := in.Entries
	if len(entries) > tracing.MaxReachComponents {
		// The clip itself is reach data the spoke claimed and the hub refused:
		// fold the refused count into OverflowComponents so a hostile or buggy
		// oversized report is visibly truncated, never silently shrunk.
		out.OverflowComponents = clampInt(out.OverflowComponents+len(entries)-tracing.MaxReachComponents,
			0, maxReachOverflowComponents)
		entries = entries[:tracing.MaxReachComponents]
	}
	out.Entries = make([]tracing.ReachEntry, 0, len(entries))
	for _, e := range entries {
		component := truncateReachRunes(sanitizeHeartbeatField(e.Component), maxReachComponentRunes)
		if component == "" {
			// A name that sanitizes to nothing identifies nothing — drop it.
			continue
		}
		entry := tracing.ReachEntry{
			Component:  component,
			Commit:     truncateReachRunes(sanitizeHeartbeatField(e.Commit), maxReachCommitRunes),
			SpansTotal: clampInt64(e.SpansTotal, 0, maxReachSpanCount),
			SpansError: clampInt64(e.SpansError, 0, maxReachSpanCount),
			FirstSeen:  sanitizeReachTime(e.FirstSeen),
			LastSeen:   sanitizeReachTime(e.LastSeen),
		}
		if e.Window1h != nil {
			entry.Window1h = &tracing.ReachWindow{
				Start:      sanitizeReachTime(e.Window1h.Start),
				SpansTotal: clampInt64(e.Window1h.SpansTotal, 0, maxReachSpanCount),
				SpansError: clampInt64(e.Window1h.SpansError, 0, maxReachSpanCount),
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	if len(out.Entries) == 0 && out.OverflowSpans == 0 && out.OverflowComponents == 0 {
		// An empty report stores as nil so "reported nothing" and "never
		// reported" read the same downstream — and the carry-forward on the
		// heartbeat path keeps the previous real report instead.
		return nil
	}
	return out
}

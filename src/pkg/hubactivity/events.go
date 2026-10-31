package hubactivity

import (
	"slices"
	"time"
)

// Activity events (RFC hivecommons/hive#15861 §A/§B, child #15964). The
// digest says where every item is now; events say how items moved since the
// hub last acknowledged them — stage hops, opens and merges — so the hub
// board can animate movement and show merge toasts. Both ends bound them:
// the spoke buffers at most MaxEvents, dropping the oldest and disclosing
// the count, and the hub keeps a fleet-wide in-memory ring.

const (
	// MaxEvents caps the events one activity block carries (and the spoke
	// buffers between acknowledgements). The oldest are dropped first.
	MaxEvents = 200
)

// Event types.
const (
	EventOpened = "opened"
	EventStage  = "stage"
	EventMerged = "merged"
)

var allowedEventTypes = []string{EventOpened, EventStage, EventMerged}

// Event is one stage hop, open or merge. Seq is the spoke's monotonic
// sequence number; the hub acknowledges the highest Seq it holds and the
// spoke trims its buffer up to it.
type Event struct {
	Seq    uint64    `json:"seq"`
	Type   string    `json:"type"`
	Repo   string    `json:"repo"`
	Number int       `json:"n"`
	Kind   string    `json:"kind,omitempty"`
	Title  string    `json:"title,omitempty"`
	From   string    `json:"from,omitempty"`
	Stage  string    `json:"stage,omitempty"`
	At     time.Time `json:"at"`
	// Private marks a private-repository item; such events exist only
	// when the spoke shares private items.
	Private bool `json:"private,omitempty"`
}

// Merge is one merged pull request the spoke's tracker reports.
type Merge struct {
	Repo     string
	Number   int
	MergedAt time.Time
	Private  bool
}

// Snapshot is what a spoke's tracker gives the activity publisher: the
// open items and the recently merged pull requests.
type Snapshot struct {
	Items  []Item
	Merged []Merge
}

// NormalizeEvent bounds and validates one event; ok is false for an event
// that must be dropped (unknown type, repo not owner/name, no number or no
// sequence number).
func NormalizeEvent(e Event) (Event, bool) {
	out := e
	out.Type = oneOf(cleanToken(e.Type), allowedEventTypes)
	out.Repo = cleanToken(e.Repo)
	if out.Type == "" || e.Seq == 0 || !repoPattern.MatchString(out.Repo) || e.Number <= 0 || e.Number > maxCount {
		return Event{}, false
	}
	out.Kind = oneOf(cleanToken(e.Kind), allowedKinds)
	out.Title = cleanText(e.Title, MaxTitleRunes)
	out.From = slug(e.From)
	out.Stage = slug(e.Stage)
	out.At = e.At.UTC()
	return out, true
}

// SanitizeEvents normalizes events received from a spoke, drops malformed
// and duplicate (by Seq) rows, orders them by Seq and keeps at most
// MaxEvents, dropping the oldest. dropped is how many the cap removed. The
// input is not modified.
func SanitizeEvents(events []Event) (out []Event, dropped int) {
	seen := map[uint64]bool{}
	for _, e := range events {
		n, ok := NormalizeEvent(e)
		if !ok || seen[n.Seq] {
			continue
		}
		seen[n.Seq] = true
		out = append(out, n)
	}
	slices.SortStableFunc(out, func(a, b Event) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	if len(out) > MaxEvents {
		dropped = len(out) - MaxEvents
		out = slices.Clone(out[dropped:])
	}
	return out, dropped
}

// ClampCount bounds a spoke-claimed count the way Sanitize bounds every
// integer in the digest.
func ClampCount(n int) int {
	return clampCount(n)
}

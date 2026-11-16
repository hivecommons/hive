// Package inbox turns voice-recorder items that knowledge connectors already
// ingest as facts (Plaud, Wispr Flow, ...) into a source-agnostic triage list
// with routes to spek RFCs, dibs ideas, or "knowledge only".
package inbox

import (
	"context"
	"errors"
	"time"
)

// Route statuses for one (item, destination) pair.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusFiled   = "filed"
	StatusFailed  = "failed"
)

// Destination names. Knowledge is implicit: items are Knowledge facts.
const (
	DestSpekRFC = "spek_rfc"
	DestDibs    = "dibs"
	DestDismiss = "dismiss"
)

// Item is one incoming recording/dictation as the Inbox lists it.
type Item struct {
	// Source is the connector type (plaud, wisprflow, ...).
	Source string `json:"source"`
	// Connector is the knowledge.connectors[] entry name that synced it.
	Connector string `json:"connector"`
	// ID is the upstream id (Plaud file id, ...). (Source, ID) is the key.
	ID      string `json:"id"`
	Title   string `json:"title"`
	Excerpt string `json:"excerpt,omitempty"`
	// Summary is the recording's summary/memo text (bounded to
	// SummaryRunes); the full text is read through Fetch.
	Summary string `json:"summary,omitempty"`
	// HasTranscript reports that Fetch returns a transcript.
	HasTranscript bool `json:"has_transcript,omitempty"`
	// Pending reports that the source has produced neither a summary nor a
	// transcript yet (for Plaud: the recording was never transcribed).
	Pending    bool      `json:"pending,omitempty"`
	RecordedAt time.Time `json:"recorded_at,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	// FactSlug / FactPath / Layer locate the Knowledge fact backing the item.
	FactSlug string `json:"fact_slug"`
	FactPath string `json:"fact_path,omitempty"`
	Layer    string `json:"layer,omitempty"`
	// Routes holds per-destination state; absent means never routed.
	Routes map[string]*RouteState `json:"routes,omitempty"`
	// Dismissed marks "knowledge only": the item stays a fact, no further route.
	Dismissed bool `json:"dismissed,omitempty"`
	// Processed is true once any route filed or the item was dismissed.
	Processed bool `json:"processed"`
	// Transcription is the machine-transcription state, when one ran.
	Transcription *TranscriptionState `json:"transcription,omitempty"`
	// Transcribable marks a pending item with audio that may be sent for
	// machine transcription now (transcription enabled and consented).
	Transcribable bool `json:"transcribable,omitempty"`
	// TranscribeUnavailable is why a pending item with audio cannot be
	// machine-transcribed right now: one of the TranscribeBlocked* codes.
	// Empty when transcribable, busy, or not applicable. Never carries a key.
	TranscribeUnavailable string `json:"transcribe_unavailable,omitempty"`
}

// Key returns the stable "source/id" key used by the state store.
func (it Item) Key() string { return Key(it.Source, it.ID) }

// Key joins a source and item id.
func Key(source, id string) string { return source + "/" + id }

// RouteState is the lifecycle of one item on one destination.
type RouteState struct {
	Status string `json:"status"`
	// Repo is the owner/repo a spek_rfc route targeted.
	Repo string `json:"repo,omitempty"`
	// URL is the RFC issue or dibs idea once filed.
	URL string `json:"url,omitempty"`
	// Children are the RFC's child issue URLs.
	Children []string `json:"children,omitempty"`
	// Note says who took the work on a delegated route (the agent name).
	Note      string    `json:"note,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Content is the full item body a destination needs.
type Content struct {
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Transcript string `json:"transcript"`
	// TranscriptLabel is set when Transcript is a machine transcript made on
	// this hive: "Transcript (machine: deepgram/nova-3)".
	TranscriptLabel string `json:"transcript_label,omitempty"`
	// Highlights lists moments flagged during the recording, when any.
	Highlights string `json:"highlights,omitempty"`
	// Markdown is the whole fact body when Summary/Transcript are not split.
	Markdown string `json:"markdown"`
	// Pending is true when the source has no text for the item yet.
	Pending bool `json:"pending"`
	// Provenance is a stable reference such as plaud://recordings/<id>.
	Provenance string `json:"provenance"`
}

// AudioLink is a fresh, short-lived link to an item's audio. It is minted on
// demand and never persisted: not in facts, not in the route state.
type AudioLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AudioSource is optionally implemented by sources whose items have audio.
type AudioSource interface {
	AudioLink(ctx context.Context, it Item) (AudioLink, error)
}

// ErrNoAudio is returned when an item's source offers no audio.
var ErrNoAudio = errors.New("no audio is available for this item")

// SummaryRunes bounds Item.Summary in listings.
const SummaryRunes = 2000

// Text returns transcript+summary, falling back to the raw markdown.
func (c Content) Text() string {
	if c.Transcript == "" && c.Summary == "" {
		return c.Markdown
	}
	out := c.Transcript
	if out != "" && c.TranscriptLabel != "" {
		out = c.TranscriptLabel + ":\n" + out
	}
	if c.Summary != "" {
		if out != "" {
			out += "\n\n"
		}
		out += "Summary:\n" + c.Summary
	}
	return out
}

// Source lists and fetches items of one type. The fact-backed source covers
// every connector type; vendor-specific sources can replace it later.
type Source interface {
	Type() string
	List(ctx context.Context) ([]Item, error)
	Fetch(ctx context.Context, it Item) (Content, error)
}

// HandOffSource is optionally implemented by a source that must learn when
// one of its items has left the inbox: filed or delegated to a destination,
// or kept as knowledge only. The hive's own meetings use it to erase the
// transcript upstream once it has been handed off (hivecommons/hive#18293).
type HandOffSource interface {
	HandedOff(ctx context.Context, it Item, dest string) error
}

// Request is one routing request.
type Request struct {
	Destination string
	// Repo is the owner/repo for spek_rfc; empty uses the configured default.
	Repo string
}

// Result is what a destination filed.
type Result struct {
	URL      string
	Children []string
	// Note is set instead of URL when the work was delegated to an agent.
	Note string
}

// Destination files an item somewhere outside Knowledge.
type Destination interface {
	Name() string
	Deliver(ctx context.Context, it Item, c Content, req Request) (Result, error)
}

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
	// DestSession records an item sent into an Inception (spek) session; it
	// is a route marker only, never a Route destination.
	DestSession = "session"
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
	// FactSourceLayer identifies the connected vault for dashboard fact links.
	FactSourceLayer string `json:"fact_source_layer,omitempty"`
	// FactState is what the backing fact holds: "ready" (the source's own
	// text), "pending" (metadata only, no text yet) or "machine" (this
	// hive's machine transcript). Empty when no fact exists yet.
	FactState string `json:"fact_state,omitempty"`
	// Routes holds per-destination state; absent means never routed.
	Routes map[string]*RouteState `json:"routes,omitempty"`
	// ShareIntents are shares waiting for a machine transcript (or failed
	// because it did not finish), keyed by destination. See ShareIntent.
	ShareIntents map[string]*ShareIntent `json:"share_intents,omitempty"`
	// ShareHistory is the append-only list of shares of this item, oldest
	// first: what, who, when, status and the target.
	ShareHistory []*ShareRecord `json:"share_history,omitempty"`
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
	// AutoTranscribe marks a pending item with audio that the hive
	// transcribes without a click (see AutoTranscribe): its own recordings,
	// dropped and mailed-in audio, and auto-mode sources. The Inbox shows it
	// as waiting for transcription rather than asking for 🎧 Transcribe.
	AutoTranscribe bool `json:"auto_transcribe,omitempty"`
	// Origin says where an item someone added came from: "upload ·
	// notes.md" for a dropped file, "link · example.org" for a URL.
	Origin string `json:"origin,omitempty"`
	// AddedBy is the dashboard user who added the item (uploads and links).
	AddedBy string `json:"added_by,omitempty"`
	// UploadKind is the detected type of an upload (markdown, json, ...).
	UploadKind string `json:"upload_kind,omitempty"`
	// RecordedBy is the dashboard user who made a hive recording.
	RecordedBy string `json:"recorded_by,omitempty"`
	// AudioExpired reports that retention deleted the raw audio of a hive
	// recording (the transcript and fact stay); there is nothing to play.
	AudioExpired bool `json:"audio_expired,omitempty"`
	// AudioRetentionDays is how long a hive recording's audio is kept.
	AudioRetentionDays int `json:"audio_retention_days,omitempty"`
	// AudioExpiresAt is when retention deletes audio this hive stores
	// itself (a hive recording, a dropped audio file); zero when the audio
	// lives elsewhere, is already gone, or is kept without a limit.
	AudioExpiresAt time.Time `json:"audio_expires_at,omitzero"`
	// AwaitingApproval marks an item from outside the hive's allowlist (the
	// email source, #18395): it cannot be routed, shared or transcribed until
	// the owner approves it.
	AwaitingApproval bool `json:"awaiting_approval,omitempty"`
	// Sender is the From address of an emailed item.
	Sender string `json:"sender,omitempty"`
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
	Note string `json:"note,omitempty"`
	// Session is the Inception (spek) session id a DestSession route sent the
	// item into (Note holds "session/<id>" plus its name).
	Session   string    `json:"session,omitempty"`
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
	// machineFact marks content read from a fact whose text is this hive's
	// machine transcript (written back by writeMachineFact).
	machineFact bool
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
	// Actor is who asked (a dashboard user, or AutoRouteActor).
	Actor string
	// ShareID is the share-history record this route continues; empty makes
	// Route append a new record.
	ShareID string
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

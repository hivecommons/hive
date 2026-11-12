// Package inbox turns voice-recorder items that knowledge connectors already
// ingest as facts (Plaud, Wispr Flow, ...) into a source-agnostic triage list
// with routes to spek RFCs, dibs ideas, or "knowledge only".
package inbox

import (
	"context"
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
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Excerpt    string    `json:"excerpt,omitempty"`
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
	Children  []string  `json:"children,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Content is the full item body a destination needs.
type Content struct {
	Title      string
	Summary    string
	Transcript string
	// Markdown is the whole fact body when Summary/Transcript are not split.
	Markdown string
	// Provenance is a stable reference such as plaud://recordings/<id>.
	Provenance string
}

// Text returns transcript+summary, falling back to the raw markdown.
func (c Content) Text() string {
	if c.Transcript == "" && c.Summary == "" {
		return c.Markdown
	}
	out := c.Transcript
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

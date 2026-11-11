package bundle

import (
	"encoding/json"
	"time"
)

// IndexEntry is one row of the marketplace catalogue index served by the hub
// at GET /api/marketplace/workflows and proxied by spokes. Entries are
// immutable once published: name@version plus Hash identify a bundle.
type IndexEntry struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Hash        string    `json:"hash"`
	Tier        string    `json:"tier"`
	Description string    `json:"description,omitempty"`
	Source      string    `json:"source,omitempty"`
	States      int       `json:"states"`
	Columns     int       `json:"columns"`
	PublishedAt time.Time `json:"publishedAt"`
	PublishedBy string    `json:"publishedBy,omitempty"`
	// SourceSHA is the commit the model was ingested from, for ingested tiers.
	SourceSHA string `json:"sourceSHA,omitempty"`
	// NewerIngestAvailable is set on curated entries when a later
	// ingested-unreviewed version of the same name exists.
	NewerIngestAvailable bool `json:"newerIngestAvailable"`
	// Reserved for later phases; always present (empty objects) so clients
	// can bind to them now.
	Adoption   json.RawMessage `json:"adoption"`
	Ratings    json.RawMessage `json:"ratings"`
	Benchmarks json.RawMessage `json:"benchmarks"`
}

// Key is `name@version`.
func (e IndexEntry) Key() string { return e.Name + "@" + e.Version }

// EmptyObject is the JSON value of the reserved fields.
var EmptyObject = json.RawMessage(`{}`)

// Index is the catalogue index document.
type Index struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	Entries     []IndexEntry `json:"entries"`
}

// EntryFor summarises a verified bundle as a catalogue row.
func EntryFor(b *Bundle, publishedAt time.Time, publishedBy, sourceSHA string) IndexEntry {
	e := IndexEntry{
		PublishedAt: publishedAt.UTC(),
		PublishedBy: publishedBy,
		SourceSHA:   sourceSHA,
		Adoption:    EmptyObject,
		Ratings:     EmptyObject,
		Benchmarks:  EmptyObject,
	}
	if b == nil || b.Model == nil {
		return e
	}
	e.Name = b.Model.Metadata.Name
	e.Version = b.Model.Spec.Meta.Version
	e.Hash = b.Hash
	e.Tier = b.Tier
	e.Description = b.Model.Spec.Meta.Description
	e.Source = b.Model.Spec.Meta.Author
	if b.Model.Spec.Scope.Repository != "" {
		e.Source = b.Model.Spec.Scope.Repository
	}
	e.States = len(b.Model.Spec.States)
	if b.Profile != nil {
		e.Columns = len(b.Profile.Columns)
	}
	return e
}

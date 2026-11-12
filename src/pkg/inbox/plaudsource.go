package inbox

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// PlaudType is the Source type for Plaud recordings.
const PlaudType = connector.TypePlaud

// PlaudSource reads recordings straight from the Plaud connector. Item.ID is
// the Plaud file id (the `attr_plaud_file_id` of the fact the same sync
// writes), so route state carries over between this source and FactSource.
type PlaudSource struct {
	// Name is the knowledge.connectors[] entry name.
	Name   string
	Reader connector.PlaudReader
	// Ready reports whether the connector is signed in; nil means always.
	Ready func(ctx context.Context) bool
}

// Type implements Source.
func (p *PlaudSource) Type() string { return PlaudType }

// Available reports whether the connector can be read right now.
func (p *PlaudSource) Available(ctx context.Context) bool {
	return p.Reader != nil && (p.Ready == nil || p.Ready(ctx))
}

// List implements Source: newest first.
func (p *PlaudSource) List(ctx context.Context) ([]Item, error) {
	recs, err := p.Reader.ListRecordings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(recs))
	for _, r := range recs {
		out = append(out, Item{
			Source:     PlaudType,
			Connector:  p.Name,
			ID:         r.ID,
			Title:      r.Title,
			RecordedAt: r.RecordedAt,
			DurationMS: r.DurationMS,
			Tags:       r.Tags,
		})
	}
	sortItems(out)
	return out, nil
}

// Fetch implements Source.
func (p *PlaudSource) Fetch(ctx context.Context, it Item) (Content, error) {
	c, err := p.Reader.ReadRecording(ctx, it.ID)
	if err != nil {
		return Content{}, err
	}
	var md strings.Builder
	md.WriteString("# " + it.Title + "\n")
	if c.Summary != "" {
		md.WriteString("\n## Summary\n\n" + c.Summary + "\n")
	}
	if c.Transcript != "" {
		md.WriteString("\n## Transcript\n\n" + c.Transcript + "\n")
	}
	return Content{
		Title:      it.Title,
		Summary:    c.Summary,
		Transcript: c.Transcript,
		Markdown:   strings.TrimSpace(md.String()),
		Provenance: PlaudType + "://recordings/" + it.ID,
	}, nil
}

// PreferDirect lists through Direct while it is available and healthy and
// falls back to Facts otherwise, so the Inbox keeps working before Plaud is
// signed in or when the MCP server is unreachable.
type PreferDirect struct {
	Direct *PlaudSource
	Facts  Source
}

// Type implements Source.
func (s *PreferDirect) Type() string { return s.Facts.Type() }

// List implements Source.
func (s *PreferDirect) List(ctx context.Context) ([]Item, error) {
	if s.Direct.Available(ctx) {
		if items, err := s.Direct.List(ctx); err == nil {
			return items, nil
		}
	}
	return s.Facts.List(ctx)
}

// Fetch implements Source: fact-backed items (FactPath set) read their fact,
// direct items read Plaud.
func (s *PreferDirect) Fetch(ctx context.Context, it Item) (Content, error) {
	if it.FactPath != "" {
		return s.Facts.Fetch(ctx, it)
	}
	return s.Direct.Fetch(ctx, it)
}

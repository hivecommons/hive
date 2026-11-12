package inbox

import (
	"context"
	"strings"
	"sync"
	"time"

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

// directListTTL bounds how long a direct Plaud listing is reused before it
// is refreshed in the background. Listing spawns the Plaud MCP process, so
// the dashboard poll must never pay for it.
const directListTTL = 5 * time.Minute

// directListTimeout bounds one background direct listing.
const directListTimeout = 2 * time.Minute

// PreferDirect lists through Direct while it is available and healthy and
// falls back to Facts otherwise, so the Inbox keeps working before Plaud is
// signed in or when the MCP server is unreachable.
//
// The direct listing is cached for TTL and refreshed in the background:
// List returns whatever is cached (or just facts while nothing is) and
// never blocks on the Plaud process. Direct items are merged with the facts
// of the same id so they keep FactSlug/FactPath/Layer/Excerpt.
type PreferDirect struct {
	Direct *PlaudSource
	Facts  Source
	// TTL overrides directListTTL; zero means the default.
	TTL time.Duration

	mu         sync.Mutex
	direct     []Item
	fetched    time.Time
	refreshing bool
}

// Type implements Source.
func (s *PreferDirect) Type() string { return s.Facts.Type() }

// List implements Source.
func (s *PreferDirect) List(ctx context.Context) ([]Item, error) {
	facts, err := s.Facts.List(ctx)
	if !s.Direct.Available(ctx) {
		return facts, err
	}
	direct, stale := s.cached()
	if stale {
		s.refreshAsync()
	}
	if direct == nil {
		return facts, err
	}
	return mergeDirect(direct, facts), err
}

// cached returns the cached direct listing (nil when none or when the last
// refresh failed) and whether it is due for a refresh.
func (s *PreferDirect) cached() (items []Item, stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.TTL
	if ttl <= 0 {
		ttl = directListTTL
	}
	return s.direct, !s.refreshing && time.Since(s.fetched) >= ttl
}

// refreshAsync starts one background Refresh unless one is already running.
func (s *PreferDirect) refreshAsync() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), directListTimeout)
		defer cancel()
		_ = s.Refresh(ctx)
	}()
}

// Refresh lists Direct now and caches the result. A failure clears the
// cache so List falls back to Facts, and is not retried before TTL.
func (s *PreferDirect) Refresh(ctx context.Context) error {
	items, err := s.Direct.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = false
	s.fetched = time.Now()
	if err != nil {
		s.direct = nil
		return err
	}
	if items == nil {
		items = []Item{}
	}
	s.direct = items
	return nil
}

// mergeDirect overlays the fact fields onto direct items with the same id
// and appends facts Plaud no longer lists (outside its since_days window).
func mergeDirect(direct, facts []Item) []Item {
	byID := make(map[string]Item, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	out := make([]Item, 0, len(direct)+len(facts))
	for _, it := range direct {
		if f, ok := byID[it.ID]; ok {
			it.FactSlug, it.FactPath, it.Layer, it.Excerpt = f.FactSlug, f.FactPath, f.Layer, f.Excerpt
			if it.Title == "" {
				it.Title = f.Title
			}
			delete(byID, it.ID)
		}
		out = append(out, it)
	}
	for _, f := range facts {
		if _, ok := byID[f.ID]; ok {
			out = append(out, f)
		}
	}
	sortItems(out)
	return out
}

// Fetch implements Source: fact-backed items (FactPath set) read their fact,
// direct items read Plaud.
func (s *PreferDirect) Fetch(ctx context.Context, it Item) (Content, error) {
	if it.FactPath != "" {
		return s.Facts.Fetch(ctx, it)
	}
	return s.Direct.Fetch(ctx, it)
}

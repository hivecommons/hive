package connector

import (
	"context"
	"sort"
	"strings"
	"time"
)

// PlaudRecording is one recording as the Plaud MCP server lists it. ID is
// the Plaud file id, the same value written to the `plaud_file_id` fact attr.
type PlaudRecording struct {
	ID         string
	Title      string
	RecordedAt time.Time
	DurationMS int64
	Tags       []string
}

// PlaudContent is the rendered note and transcript of one recording.
type PlaudContent struct {
	Summary    string
	Transcript string
}

// PlaudReader reads recordings straight from Plaud without going through
// the facts a sync writes. The plaud connector implements it.
type PlaudReader interface {
	// ListRecordings lists recent recordings, newest first.
	ListRecordings(ctx context.Context) ([]PlaudRecording, error)
	// ReadRecording returns the note and transcript of one recording.
	ReadRecording(ctx context.Context, id string) (PlaudContent, error)
}

var _ PlaudReader = (*plaudConnector)(nil)

// ListRecordings lists recordings within the connector's since_days window,
// capped at max_files, newest first.
func (c *plaudConnector) ListRecordings(ctx context.Context) ([]PlaudRecording, error) {
	client, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	since := c.now().Add(-time.Duration(c.scopeInt("since_days", plaudDefaultSinceDays)) * 24 * time.Hour)
	files, err := c.listFiles(ctx, client, since, c.scopeInt("max_files", plaudDefaultMaxFiles))
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, t := range strings.Split(c.cfg.ScopeValue("tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	out := make([]PlaudRecording, 0, len(files))
	for _, f := range files {
		rec := PlaudRecording{ID: f.ID, Title: strings.TrimSpace(f.Name), RecordedAt: f.StartAt.UTC(), DurationMS: f.DurationMS, Tags: tags}
		if f.StartAt.IsZero() {
			rec.RecordedAt = f.CreatedAt.UTC()
		}
		if rec.Title == "" {
			rec.Title = "Plaud recording " + f.ID
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RecordedAt.After(out[j].RecordedAt) })
	return out, nil
}

// ReadRecording fetches the note and transcript of one recording.
func (c *plaudConnector) ReadRecording(ctx context.Context, id string) (PlaudContent, error) {
	client, err := c.dial(ctx)
	if err != nil {
		return PlaudContent{}, err
	}
	defer func() { _ = client.Close() }()
	note, transcript, err := c.fetchNoteAndTranscript(ctx, client, id)
	if err != nil {
		return PlaudContent{}, err
	}
	return PlaudContent{Summary: strings.TrimSpace(note), Transcript: strings.TrimSpace(transcript)}, nil
}

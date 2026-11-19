package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

type fakeMeetings struct {
	recs    []connector.MeetingRecording
	content connector.MeetingContent
	link    connector.MediaLink
	linkErr error
	reads   []string
}

func (f *fakeMeetings) ListMeetings(context.Context) ([]connector.MeetingRecording, error) {
	return f.recs, nil
}

func (f *fakeMeetings) ReadMeeting(_ context.Context, id string) (connector.MeetingContent, error) {
	f.reads = append(f.reads, id)
	return f.content, nil
}

func (f *fakeMeetings) MediaLink(context.Context, string) (connector.MediaLink, error) {
	return f.link, f.linkErr
}

const zoomFact = `---
title: Weekly sync
type: reference
layer: personal
status: active
source: zoom
connector: calls
source_id: uuid-1
attr_zoom_recording_id: uuid-1
attr_recorded_at: 2026-11-10T08:00:00Z
attr_duration_ms: 1800000
synthesized: 2026-11-10T08:00:00Z
---

# Weekly sync

## Summary (Zoom)

Ship it.

## Transcript

[00:01] Andy: Hello
`

func TestMeetingSourceListFetchAndAudio(t *testing.T) {
	older := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	fm := &fakeMeetings{
		recs: []connector.MeetingRecording{
			{ID: "uuid-1", Topic: "Weekly sync", StartAt: older, DurationMS: 1800000},
			{ID: "uuid-2", Topic: "Design review", StartAt: newer, Tags: []string{"meetings"}},
		},
		content: connector.MeetingContent{Summary: "Ship it.", Transcript: "[00:01] Andy: Hello", Participants: []string{"Andy", "Pat"}},
		link:    connector.MediaLink{URL: "https://zoom.us/rec/download/x?access_token=t", ExpiresAt: newer},
	}
	src := &MeetingSource{SourceType: ZoomType, Name: "calls", Reader: fm}
	if src.Type() != "zoom" {
		t.Fatalf("type = %q", src.Type())
	}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].ID != "uuid-2" || items[0].Source != "zoom" || items[0].Connector != "calls" || items[0].Key() != "zoom/uuid-2" || items[0].Tags[0] != "meetings" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[1].DurationMS != 1800000 || !items[0].RecordedAt.Equal(newer) {
		t.Fatalf("mapping lost: %+v", items)
	}
	c, err := src.Fetch(context.Background(), items[0])
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "Design review" || c.Summary != "Ship it." || c.Transcript != "[00:01] Andy: Hello" || c.Provenance != "zoom://recordings/uuid-2" || !strings.Contains(c.Markdown, "**Participants:** Andy, Pat") {
		t.Fatalf("content = %+v", c)
	}
	link, err := src.AudioLink(context.Background(), items[0])
	if err != nil || link.URL != fm.link.URL || !link.ExpiresAt.Equal(newer) {
		t.Fatalf("audio = %+v, %v", link, err)
	}
	fm.linkErr = connector.ErrMeetingNoMedia
	if _, err := src.AudioLink(context.Background(), items[0]); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("no media must map to ErrNoAudio: %v", err)
	}

	pending := &MeetingSource{SourceType: TeamsType, Name: "mtg", Reader: &fakeMeetings{content: connector.MeetingContent{Pending: true}}}
	pc, err := pending.Fetch(context.Background(), Item{Source: TeamsType, ID: "rec-1", Title: "Standup"})
	if err != nil || !pc.Pending || !strings.Contains(pc.Markdown, connector.MeetingPendingText) || pc.Provenance != "teams://recordings/rec-1" {
		t.Fatalf("pending content = %+v, %v", pc, err)
	}
}

// A MeetingSource is a DirectSource: PreferDirect merges its live listing
// with the zoom facts of the same id and reads audio only through it.
func TestPreferDirectWithMeetingSource(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "zoom-calls-uuid-1.md", zoomFact)
	facts := &FactSource{ConnectorsDir: dir, SourceType: ZoomType}
	fm := &fakeMeetings{
		recs:    []connector.MeetingRecording{{ID: "uuid-1", Topic: "Weekly sync", StartAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}, {ID: "uuid-9", Topic: "New", StartAt: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}},
		content: connector.MeetingContent{Transcript: "live"},
		link:    connector.MediaLink{URL: "https://zoom.us/rec/download/y?access_token=t"},
	}
	ready := false
	direct := &MeetingSource{SourceType: ZoomType, Name: "calls", Reader: fm, Ready: func(context.Context) bool { return ready }}
	src := &PreferDirect{Direct: direct, Facts: facts, TTL: time.Hour}
	ctx := context.Background()
	if src.Type() != "zoom" {
		t.Fatalf("type = %q", src.Type())
	}
	items, err := src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "uuid-1" || items[0].Summary != "Ship it." {
		t.Fatalf("not connected should use facts: %+v %v", items, err)
	}
	if _, err := src.AudioLink(ctx, items[0]); !errors.Is(err, ErrNoAudio) || !strings.Contains(err.Error(), "zoom is not connected") {
		t.Fatalf("audio while disconnected: %v", err)
	}
	ready = true
	if err := src.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	items, err = src.List(ctx)
	if err != nil || len(items) != 2 || items[0].ID != "uuid-9" || items[1].ID != "uuid-1" || items[1].FactPath == "" {
		t.Fatalf("connected should merge live with facts: %+v %v", items, err)
	}
	if c, err := src.Fetch(ctx, items[1]); err != nil || c.Summary != "Ship it." || len(fm.reads) != 0 {
		t.Fatalf("fact-backed item must read the fact: %+v %v reads=%v", c, err, fm.reads)
	}
	if c, err := src.Fetch(ctx, items[0]); err != nil || c.Transcript != "live" {
		t.Fatalf("live item must read the platform: %+v %v", c, err)
	}
	if link, err := src.AudioLink(ctx, items[1]); err != nil || link.URL != fm.link.URL {
		t.Fatalf("audio = %+v, %v", link, err)
	}
}

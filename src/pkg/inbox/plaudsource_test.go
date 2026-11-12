package inbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

type fakePlaud struct {
	recs    []connector.PlaudRecording
	content connector.PlaudContent
	listErr error
	reads   []string
}

func (f *fakePlaud) ListRecordings(context.Context) ([]connector.PlaudRecording, error) {
	return f.recs, f.listErr
}

func (f *fakePlaud) ReadRecording(_ context.Context, id string) (connector.PlaudContent, error) {
	f.reads = append(f.reads, id)
	return f.content, nil
}

func TestPlaudSourceListAndFetch(t *testing.T) {
	older := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	fp := &fakePlaud{
		recs: []connector.PlaudRecording{
			{ID: "rec-1", Title: "Old", RecordedAt: older, DurationMS: 1000},
			{ID: "rec-2", Title: "New", RecordedAt: newer, Tags: []string{"voice"}},
		},
		content: connector.PlaudContent{Summary: "sum", Transcript: "words"},
	}
	src := &PlaudSource{Name: "my-plaud", Reader: fp}
	if src.Type() != "plaud" {
		t.Fatalf("type = %q", src.Type())
	}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].ID != "rec-2" || items[0].Connector != "my-plaud" || items[0].Source != "plaud" || items[0].Key() != "plaud/rec-2" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[1].DurationMS != 1000 || !items[0].RecordedAt.Equal(newer) {
		t.Fatalf("mapping lost: %+v", items)
	}
	c, err := src.Fetch(context.Background(), items[0])
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "New" || c.Summary != "sum" || c.Transcript != "words" || c.Provenance != "plaud://recordings/rec-2" {
		t.Fatalf("content = %+v", c)
	}
	if len(fp.reads) != 1 || fp.reads[0] != "rec-2" {
		t.Fatalf("reads = %v", fp.reads)
	}
}

func TestPreferDirectSelection(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-1.md", plaudFact)
	facts := &FactSource{ConnectorsDir: dir, SourceType: "plaud"}
	fp := &fakePlaud{recs: []connector.PlaudRecording{{ID: "direct-1", Title: "Direct"}}}
	ready := false
	direct := &PlaudSource{Name: "plaud", Reader: fp, Ready: func(context.Context) bool { return ready }}
	src := &PreferDirect{Direct: direct, Facts: facts}
	ctx := context.Background()

	items, err := src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "rec-1" {
		t.Fatalf("not signed in should use facts: %v %v", items, err)
	}
	if _, err := src.Fetch(ctx, items[0]); err != nil || len(fp.reads) != 0 {
		t.Fatalf("fact item must not hit plaud: err=%v reads=%v", err, fp.reads)
	}

	ready = true
	items, err = src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "direct-1" {
		t.Fatalf("signed in should use plaud: %v %v", items, err)
	}
	if _, err := src.Fetch(ctx, items[0]); err != nil || len(fp.reads) != 1 {
		t.Fatalf("direct item should read plaud: err=%v reads=%v", err, fp.reads)
	}

	fp.listErr = errors.New("mcp down")
	items, err = src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "rec-1" {
		t.Fatalf("plaud failure should fall back to facts: %v %v", items, err)
	}
}

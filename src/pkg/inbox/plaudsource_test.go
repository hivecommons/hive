package inbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
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
	fp := &fakePlaud{recs: []connector.PlaudRecording{{ID: "direct-1", Title: "Direct", RecordedAt: time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)}}}
	ready := false
	direct := &PlaudSource{Name: "plaud", Reader: fp, Ready: func(context.Context) bool { return ready }}
	src := &PreferDirect{Direct: direct, Facts: facts, TTL: time.Hour}
	ctx := context.Background()

	items, err := src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "rec-1" {
		t.Fatalf("not signed in should use facts: %v %v", items, err)
	}
	if _, err := src.Fetch(ctx, items[0]); err != nil || len(fp.reads) != 0 {
		t.Fatalf("fact item must not hit plaud: err=%v reads=%v", err, fp.reads)
	}

	ready = true
	if err := src.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	items, err = src.List(ctx)
	if err != nil || len(items) != 2 || items[0].ID != "direct-1" || items[1].ID != "rec-1" {
		t.Fatalf("signed in should merge plaud with facts: %v %v", items, err)
	}
	if items[1].FactPath == "" || items[1].Excerpt == "" {
		t.Fatalf("fact-only item lost fact fields: %+v", items[1])
	}
	if _, err := src.Fetch(ctx, items[0]); err != nil || len(fp.reads) != 1 {
		t.Fatalf("direct item should read plaud: err=%v reads=%v", err, fp.reads)
	}

	fp.listErr = errors.New("mcp down")
	if err := src.Refresh(ctx); err == nil {
		t.Fatal("refresh should report the plaud error")
	}
	items, err = src.List(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "rec-1" {
		t.Fatalf("plaud failure should fall back to facts: %v %v", items, err)
	}
}

// List must never pay for the Plaud process: the first signed-in call
// returns facts and kicks off one background refresh.
func TestPreferDirectListDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-1.md", plaudFact)
	facts := &FactSource{ConnectorsDir: dir, SourceType: "plaud"}
	fp := &countingPlaud{recs: []connector.PlaudRecording{{ID: "rec-1", Title: "Direct title"}}, gate: make(chan struct{})}
	src := &PreferDirect{Direct: &PlaudSource{Name: "plaud", Reader: fp}, Facts: facts, TTL: time.Hour}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		items, err := src.List(ctx)
		if err != nil || len(items) != 1 || items[0].Title != "RFC: Voice inbox" {
			t.Fatalf("list %d while plaud is slow should serve facts: %v %v", i, items, err)
		}
	}
	fp.release()
	items := testutil.EventuallyValue(t, 5*time.Second, func() ([]Item, bool) {
		got, _ := src.List(ctx)
		return got, len(got) == 1 && got[0].Title == "Direct title"
	}, "background refresh never landed")
	if n := fp.calls.Load(); n != 1 {
		t.Fatalf("ListRecordings calls = %d, want 1 (single-flight, cached for TTL)", n)
	}
	it := items[0]
	if it.FactSlug != "plaud-plaud-rec-1" || it.Layer != "personal" || it.FactPath == "" || it.Excerpt == "" {
		t.Fatalf("direct item lost fact fields: %+v", it)
	}
}

type countingPlaud struct {
	recs  []connector.PlaudRecording
	calls atomic.Int32
	gate  chan struct{}
	once  sync.Once
}

func (c *countingPlaud) release() {
	c.once.Do(func() {
		if c.gate != nil {
			close(c.gate)
		}
	})
}

func (c *countingPlaud) ListRecordings(context.Context) ([]connector.PlaudRecording, error) {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	return c.recs, nil
}

func (c *countingPlaud) ReadRecording(context.Context, string) (connector.PlaudContent, error) {
	return connector.PlaudContent{}, nil
}

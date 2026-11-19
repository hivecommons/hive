package inbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// A fact written for a recording Plaud never transcribed: the connector's
// metadata bullets and pending note, no Summary/Transcript section.
const pendingFact = `---
title: nate v2 - workflow
type: reference
layer: personal
status: active
source: plaud
connector: plaud
source_id: rec-pending
attr_plaud_file_id: rec-pending
attr_plaud_content: pending
attr_recorded_at: 2026-11-11T19:24:28Z
attr_duration_ms: 307100
---

# nate v2 - workflow

- **Recorded:** 2026-11-11T19:24:28Z
- **Duration:** 5m7s

_Plaud has not transcribed this recording yet._

## Highlights

- 05:03
`

func TestFactSourceSummaryTranscriptAndPending(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-1.md", plaudFact)
	writeFact(t, dir, "personal", "plaud-plaud-rec-pending.md", pendingFact)
	src := &FactSource{ConnectorsDir: dir, SourceType: "plaud"}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	ready := byID["rec-1"]
	if ready.Summary == "" || !ready.HasTranscript || ready.Pending {
		t.Fatalf("transcribed fact = %+v", ready)
	}
	pending := byID["rec-pending"]
	if pending.Summary != "" || pending.HasTranscript || !pending.Pending || pending.DurationMS != 307100 {
		t.Fatalf("pending fact = %+v", pending)
	}
	// The excerpt is prose: never the metadata bullets the UI shows itself.
	if strings.Contains(pending.Excerpt, "Duration") || !strings.Contains(pending.Excerpt, "has not transcribed") {
		t.Fatalf("excerpt = %q", pending.Excerpt)
	}
	c, err := src.Fetch(context.Background(), pending)
	if err != nil || !c.Pending || c.Highlights != "- 05:03" || c.Summary != "" {
		t.Fatalf("pending content = %+v, %v", c, err)
	}
	if got := truncateRunes(strings.Repeat("é", SummaryRunes+1), SummaryRunes); !strings.HasSuffix(got, "…") || len([]rune(got)) != SummaryRunes+1 {
		t.Fatalf("truncate = %d runes", len([]rune(got)))
	}
}

type fakePlaudAudio struct {
	fakePlaud
	links int
	err   error
}

func (f *fakePlaudAudio) AudioLink(_ context.Context, id string) (connector.PlaudAudioLink, error) {
	f.links++
	if f.err != nil {
		return connector.PlaudAudioLink{}, f.err
	}
	return connector.PlaudAudioLink{URL: "https://cdn.plaud.example/" + id + ".mp3?sig=1", ExpiresAt: time.Date(2026, 11, 12, 19, 0, 0, 0, time.UTC)}, nil
}

func TestPlaudSourceAudioAndPendingFetch(t *testing.T) {
	ctx := context.Background()
	plain := &PlaudSource{Name: "plaud", Reader: &fakePlaud{}}
	if _, err := plain.AudioLink(ctx, Item{ID: "rec-1"}); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("reader without audio = %v, want ErrNoAudio", err)
	}
	fp := &fakePlaudAudio{fakePlaud: fakePlaud{content: connector.PlaudContent{Pending: true, Highlights: "- 05:03"}}}
	ready := false
	direct := &PlaudSource{Name: "plaud", Reader: fp, Ready: func(context.Context) bool { return ready }}
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-pending.md", pendingFact)
	src := &PreferDirect{Direct: direct, Facts: &FactSource{ConnectorsDir: dir, SourceType: "plaud"}, TTL: time.Hour}

	if _, err := src.AudioLink(ctx, Item{Source: "plaud", ID: "rec-pending"}); !errors.Is(err, ErrNoAudio) || fp.links != 0 {
		t.Fatalf("audio while disconnected = %v (links %d)", err, fp.links)
	}
	ready = true
	link, err := src.AudioLink(ctx, Item{Source: "plaud", ID: "rec-pending"})
	if err != nil || link.URL != "https://cdn.plaud.example/rec-pending.mp3?sig=1" || link.ExpiresAt.IsZero() {
		t.Fatalf("link = %+v, %v", link, err)
	}

	// A pending fact is read live while Plaud is reachable, so text shows up
	// as soon as Plaud has it; a transcribed fact is still served from disk.
	items, err := src.Facts.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %v, %v", items, err)
	}
	c, err := src.Fetch(ctx, items[0])
	if err != nil || !c.Pending || len(fp.reads) != 1 || !strings.Contains(c.Markdown, connector.PlaudPendingText) {
		t.Fatalf("pending fetch = %+v err=%v reads=%v", c, err, fp.reads)
	}
	fp.content = connector.PlaudContent{Summary: "now summarized", Transcript: "words"}
	c, err = src.Fetch(ctx, items[0])
	if err != nil || c.Pending || c.Summary != "now summarized" || !strings.Contains(c.Markdown, "## Transcript") {
		t.Fatalf("fetch after transcription = %+v, %v", c, err)
	}
	notPending := items[0]
	notPending.Pending = false
	if _, err := src.Fetch(ctx, notPending); err != nil || len(fp.reads) != 2 {
		t.Fatalf("transcribed fact must read from disk: err=%v reads=%v", err, fp.reads)
	}
}

func TestServiceContentAndAudioLink(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-pending.md", pendingFact)
	fp := &fakePlaudAudio{fakePlaud: fakePlaud{content: connector.PlaudContent{Pending: true}}}
	direct := &PlaudSource{Name: "plaud", Reader: fp}
	src := &PreferDirect{Direct: direct, Facts: &FactSource{ConnectorsDir: dir, SourceType: "plaud"}, TTL: time.Hour}
	svc := NewService(config.InboxConfig{}, NewStore(filepath.Join(dir, "inbox.json")), []Source{src}, nil, nil)
	ctx := context.Background()
	it, found, err := svc.Find(ctx, "plaud", "rec-pending")
	if err != nil || !found {
		t.Fatalf("find = %v %v", found, err)
	}
	if _, err := svc.Content(ctx, it); err != nil {
		t.Fatal(err)
	}
	link, err := svc.AudioLink(ctx, it)
	if err != nil || link.URL == "" {
		t.Fatalf("link = %+v, %v", link, err)
	}
	if _, err := svc.AudioLink(ctx, Item{Source: "wisprflow", ID: "x"}); err == nil {
		t.Fatal("unknown source must error")
	}
	// Nothing about the link reaches the state store or the vault.
	items, _ := svc.Items(ctx)
	for _, it := range items {
		for _, r := range it.Routes {
			if strings.Contains(r.URL, "cdn.plaud.example") {
				t.Fatalf("audio link persisted in route state: %+v", r)
			}
		}
	}
}

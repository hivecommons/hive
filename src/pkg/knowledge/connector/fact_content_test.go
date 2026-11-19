package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syncPlaudInto runs one Plaud sync and writes every page with w, the way
// the syncer does.
func syncPlaudInto(t *testing.T, pc *plaudConnector, w *FactWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := pc.cfg
	if _, err := pc.Sync(ctx, "", func(p Page) error { _, _, err := w.Write(cfg, p); return err }); err != nil {
		t.Fatal(err)
	}
}

// An audio-only recording (Plaud has no summary or transcript) becomes a
// pending fact: metadata, the pending note and the highlights, no text.
func TestPlaudAudioOnlyFactShape(t *testing.T) {
	pc := newTestPlaud(t, newPlaudTestServer(), nil)
	w := testWriter(t)
	syncPlaudInto(t, pc, w)
	data, err := os.ReadFile(filepath.Join(w.Dir, Slug(TypePlaud, "plaud", "rec-2")+".md"))
	if err != nil {
		t.Fatal(err)
	}
	fact := string(data)
	fm, body := splitFactFrontMatter(fact)
	if fm["attr_plaud_content"] != FactContentPending || FactContentState(fm) != FactContentPending || fm["attr_plaud_file_id"] != "rec-2" || fm["attr_duration_ms"] != "61000" || fm["title"] != "Drive home" || fm["source_id"] != "rec-2" {
		t.Fatalf("audio-only front matter = %v", fm)
	}
	for _, want := range []string{"# Drive home", "- **Recorded:** 2026-11-11T18:00:00Z", "- **Duration:** 1m1s", "_" + PlaudPendingText + "_", "## Highlights", "- 05:03"} {
		if !strings.Contains(body, want) {
			t.Errorf("audio-only body missing %q:\n%s", want, fact)
		}
	}
	if strings.Contains(body, "## Transcript") || strings.Contains(body, "## Summary") {
		t.Fatalf("audio-only fact has a text section:\n%s", fact)
	}
}

// machineVersion turns a pending Plaud fact into what the Inbox write-back
// leaves: content machine, a machine transcript section, same metadata.
func machineVersion(fact string) string {
	fact = strings.Replace(fact, "attr_plaud_content: pending", "attr_plaud_content: machine\nattr_transcript_provider: whisper/base.en", 1)
	return strings.Replace(fact, "_"+PlaudPendingText+"_", "## Transcript (machine: whisper/base.en)\n\n[00:01] pick up milk", 1)
}

func TestFactWriterKeepsMachineFactUntilSourceText(t *testing.T) {
	pc := newTestPlaud(t, newPlaudTestServer(), nil)
	w := testWriter(t)
	syncPlaudInto(t, pc, w)
	path := filepath.Join(w.Dir, Slug(TypePlaud, "plaud", "rec-2")+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	machine := machineVersion(string(data))
	if err := os.WriteFile(path, []byte(machine), 0o644); err != nil {
		t.Fatal(err)
	}

	// The next sync still finds no text upstream: the machine fact stays.
	syncPlaudInto(t, pc, w)
	if got, _ := os.ReadFile(path); string(got) != machine {
		t.Fatalf("pending sync clobbered the machine fact:\n%s", got)
	}

	// A machine fact keeps being re-read so source text can replace it.
	fm, body := splitFactFrontMatter(machine)
	if !plaudFactNeedsRefetch(fm, body) {
		t.Fatal("machine Plaud fact must be refetched")
	}
	if !meetingFactNeedsRefetch(map[string]string{"attr_meeting_content": FactContentMachine}) || meetingFactNeedsRefetch(map[string]string{"attr_meeting_content": FactContentReady}) {
		t.Fatal("meeting refetch must cover machine facts and skip ready ones")
	}

	// Source text arrives: it replaces the machine transcript.
	cfg := pc.cfg
	ready := Page{ID: "rec-2", Title: "Drive home", Markdown: "# Drive home\n\n## Transcript\n\n[00:00] Andy: real words\n", Attrs: map[string]string{"plaud_file_id": "rec-2", plaudContentAttr: FactContentReady}}
	if _, changed, err := w.Write(cfg, ready); err != nil || !changed {
		t.Fatalf("ready write = %v %v", changed, err)
	}
	got, _ := os.ReadFile(path)
	fm, _ = splitFactFrontMatter(string(got))
	if fm["attr_plaud_content"] != FactContentReady || fm["attr_transcript_provider"] != "" || !strings.Contains(string(got), "real words") || strings.Contains(string(got), "pick up milk") {
		t.Fatalf("upstream fact = %s", got)
	}

	// An archived page still tombstones a machine fact.
	if err := os.WriteFile(path, []byte(machine), 0o644); err != nil {
		t.Fatal(err)
	}
	archived := Page{ID: "rec-2", Title: "Drive home", Archived: true, Attrs: map[string]string{plaudContentAttr: FactContentPending}}
	if _, changed, err := w.Write(cfg, archived); err != nil || !changed {
		t.Fatalf("archived write = %v %v", changed, err)
	}
}

func TestFactContentState(t *testing.T) {
	if got := FactContentState(map[string]string{"attr_meeting_content": "machine", "title": "x"}); got != FactContentMachine {
		t.Fatalf("state = %q", got)
	}
	if got := FactContentKey(map[string]string{"title": "x"}); got != "" {
		t.Fatalf("key = %q", got)
	}
	if !pageContentPending(map[string]string{"zoom_recording_id": "1", "meeting_content": "pending"}) || pageContentPending(map[string]string{"meeting_content": "ready"}) {
		t.Fatal("pageContentPending")
	}
}

package inbox

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// audioFactSource is a fact-backed source whose items have audio, the way
// PreferDirect serves Plaud: list and read the facts, mint links upstream.
type audioFactSource struct {
	*FactSource
	link string
}

func (s *audioFactSource) AudioLink(context.Context, Item) (AudioLink, error) {
	return AudioLink{URL: s.link, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// audioOnlyFact is what the Plaud connector writes for a recording Plaud
// never transcribed (see TestPlaudAudioOnlyFactShape for the real render).
func audioOnlyFact() string {
	return "---\ntitle: Drive home\ntype: reference\nlayer: personal\nstatus: active\ntags: [connector, plaud, plaud]\n" +
		"source: plaud\nconnector: plaud\nsource_id: rec-2\nsource_path: Plaud\nattr_duration_ms: 61000\n" +
		"attr_plaud_content: pending\nattr_plaud_file_id: rec-2\nsynthesized: 2026-11-11T18:00:00Z\nsynced_at: 2026-11-11T20:00:00Z\n---\n\n" +
		"# Drive home\n\n- **Recorded:** 2026-11-11T18:00:00Z\n- **Duration:** 1m1s\n\n_" + connector.PlaudPendingText + "_\n\n" +
		"## Highlights\n\nMoments flagged with the device button:\n\n- 05:03\n"
}

type factEnv struct {
	svc   *Service
	store *TranscriptStore
	path  string
	now   time.Time
}

func newFactEnv(t *testing.T, writeFact bool) *factEnv {
	t.Helper()
	srv := audioServer(t, http.StatusOK, txAudio)
	dir := t.TempDir()
	layer := filepath.Join(dir, "personal")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &factEnv{path: filepath.Join(layer, "plaud-plaud-rec-2.md"), now: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	if writeFact {
		e.writeFact(t, audioOnlyFact())
	}
	src := &audioFactSource{FactSource: &FactSource{ConnectorsDir: dir, SourceType: "plaud"}, link: srv.URL + "/rec-2.mp3?sig=secret"}
	e.svc = NewService(config.InboxConfig{Transcription: activeTx()}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), []Source{src}, nil, nil)
	e.svc.Run = func(fn func()) { fn() }
	e.svc.Now = func() time.Time { return e.now }
	e.store = NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	e.svc.EnableTranscription(e.store)
	fake := &transcribe.Fake{Result: transcribe.Result{
		Segments: []transcribe.Segment{{Start: time.Second, Text: "pick up milk"}},
		Language: "en", Duration: 61 * time.Second,
	}}
	e.svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	e.svc.TranscribeBackoff = func(int) time.Duration { return 0 }
	return e
}

func (e *factEnv) writeFact(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(e.path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *factEnv) readFact(t *testing.T) (map[string]string, string) {
	t.Helper()
	data, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	return splitFrontMatter(string(data))
}

func (e *factEnv) item(t *testing.T) Item {
	t.Helper()
	it, ok, err := e.svc.Find(context.Background(), "plaud", "rec-2")
	if !ok || err != nil {
		t.Fatalf("find = %v %v", ok, err)
	}
	return it
}

func TestAudioOnlyFactIsPendingBeforeTranscription(t *testing.T) {
	e := newFactEnv(t, true)
	it := e.item(t)
	if !it.Pending || it.HasTranscript || it.FactState != connector.FactContentPending || it.FactSlug != "plaud-plaud-rec-2" {
		t.Fatalf("audio-only item = %+v", it)
	}
	c, err := e.svc.Content(context.Background(), it)
	if err != nil || !c.Pending || c.Transcript != "" || c.Summary != "" || c.Highlights == "" {
		t.Fatalf("audio-only content = %+v %v", c, err)
	}
}

func TestTranscriptionWritesMachineFact(t *testing.T) {
	e := newFactEnv(t, true)
	ctx := context.Background()
	it := e.item(t)
	if _, err := e.svc.Transcribe(ctx, it, TranscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	rec, ok := e.store.Get("plaud", "rec-2")
	if !ok || rec.Status != TranscriptionDone {
		t.Fatalf("record = %+v", rec)
	}

	fm, body := e.readFact(t)
	if fm["attr_plaud_content"] != connector.FactContentMachine || fm["attr_transcript_provider"] != rec.Provider+"/"+rec.Model || fm["attr_transcript_language"] != "en" {
		t.Fatalf("machine fact front matter = %v", fm)
	}
	// Source attrs and provenance are kept.
	if fm["source"] != "plaud" || fm["source_id"] != "rec-2" || fm["attr_plaud_file_id"] != "rec-2" || fm["attr_duration_ms"] != "61000" || fm["title"] != "Drive home" {
		t.Fatalf("source attrs lost: %v", fm)
	}
	if strings.Contains(body, connector.PlaudPendingText) {
		t.Fatalf("pending note kept:\n%s", body)
	}
	for _, want := range []string{"- **Duration:** 1m1s", "## " + rec.Label(), "<" + UntrustedTag + ` source="plaud" id="rec-2"`, "[00:01] pick up milk", "</" + UntrustedTag, "## Highlights", "- 05:03"} {
		if !strings.Contains(body, want) {
			t.Errorf("machine fact body missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "## "+rec.Label()) > strings.Index(body, "## Highlights") {
		t.Fatalf("transcript must come before highlights:\n%s", body)
	}

	// The item reads as transcribed and in knowledge; the Inbox shows the
	// stored text unwrapped, under its machine label.
	it = e.item(t)
	if it.Pending || !it.HasTranscript || it.FactState != connector.FactContentMachine {
		t.Fatalf("item after write-back = %+v", it)
	}
	c, err := e.svc.Content(ctx, it)
	if err != nil || c.Transcript != rec.Text || c.TranscriptLabel != rec.Label() {
		t.Fatalf("content = %+v %v", c, err)
	}
	// A second listing does not rewrite the fact.
	before, _ := os.ReadFile(e.path)
	_ = e.item(t)
	after, _ := os.ReadFile(e.path)
	if string(before) != string(after) {
		t.Fatal("listing rewrote a machine fact")
	}
}

func TestMachineTranscriptReachesFactWrittenLater(t *testing.T) {
	e := newFactEnv(t, false)
	// The transcript finished before the sync wrote the fact (or before
	// write-back existed).
	if err := e.store.Put(TranscriptRecord{Source: "plaud", ID: "rec-2", Status: TranscriptionDone, Provider: "whisper", Model: "base.en", Text: "[00:01] pick up milk", UpdatedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	e.writeFact(t, audioOnlyFact())
	it := e.item(t)
	if it.FactState != connector.FactContentMachine {
		t.Fatalf("item = %+v", it)
	}
	fm, body := e.readFact(t)
	if fm["attr_plaud_content"] != connector.FactContentMachine || fm["attr_transcript_provider"] != "whisper/base.en" || !strings.Contains(body, "pick up milk") {
		t.Fatalf("backfilled fact = %v\n%s", fm, body)
	}
}

func TestUpstreamTextIsNeverOverwrittenByMachineFact(t *testing.T) {
	e := newFactEnv(t, false)
	if err := e.store.Put(TranscriptRecord{Source: "plaud", ID: "rec-2", Status: TranscriptionDone, Provider: "whisper", Model: "base.en", Text: "[00:01] pick up milk", UpdatedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	ready := strings.Replace(audioOnlyFact(), "attr_plaud_content: pending", "attr_plaud_content: ready", 1)
	ready = strings.Replace(ready, "_"+connector.PlaudPendingText+"_", "## Transcript\n\n[00:00] Andy: real words", 1)
	e.writeFact(t, ready)
	it := e.item(t)
	if it.FactState != connector.FactContentReady {
		t.Fatalf("item = %+v", it)
	}
	if data, _ := os.ReadFile(e.path); string(data) != ready {
		t.Fatalf("upstream fact rewritten:\n%s", data)
	}
	c, err := e.svc.Content(context.Background(), it)
	if err != nil || c.Transcript != "[00:00] Andy: real words" || c.TranscriptLabel != "" {
		t.Fatalf("content = %+v %v", c, err)
	}
}

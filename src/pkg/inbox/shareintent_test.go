package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// rfcDest records the content an RFC route delivered.
type rfcDest struct {
	mu  sync.Mutex
	got []Content
	req []Request
}

func (d *rfcDest) Name() string { return DestSpekRFC }
func (d *rfcDest) Deliver(_ context.Context, _ Item, c Content, r Request) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got, d.req = append(d.got, c), append(d.req, r)
	return Result{URL: "https://github.com/o/r/issues/9"}, nil
}

func (d *rfcDest) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.got) }

// withKeyEnv makes the default provider's API key present.
func withKeyEnv(string) string { return "test-key" }

func intentFor(dest string) ShareIntent {
	return ShareIntent{Destination: dest, Actor: "rw@example.com", Repo: "o/r"}
}

// An audio-only item is transcribed first, then the RFC runs with the
// transcript; the intent is gone afterwards.
func TestShareWhenReadyTranscribesThenRoutes(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	e.svc.Getenv = withKeyEnv
	out, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts())
	if err != nil || !out.Waiting {
		t.Fatalf("ShareWhenReady = %+v, %v", out, err)
	}
	e.svc.Wait()
	if dest.count() != 1 || !strings.Contains(dest.got[0].Text(), "hello hive") || dest.req[0].Repo != "o/r" {
		t.Fatalf("RFC delivered = %+v %+v", dest.got, dest.req)
	}
	if got := e.svc.Store.Intents("plaud/rec-1"); len(got) != 0 {
		t.Fatalf("intent left behind: %+v", got)
	}
}

// Knowledge-only waits for the transcript the same way.
func TestShareWhenReadyKnowledgeAfterTranscript(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, activeTx(), srv.URL)
	e.svc.Getenv = withKeyEnv
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestDismiss), e.opts()); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if it := e.item(t); !it.Dismissed {
		t.Fatalf("item not kept as knowledge after transcription: %+v", it)
	}
}

// Transcription failure fails the intent with the reason; nothing is sent.
func TestShareIntentFailsWithReasonAndSendsNothing(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	e.svc.Getenv = withKeyEnv
	boom := errors.New("provider exploded")
	e.fake.Errs = []error{boom, boom, boom, boom, boom, boom}
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts()); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	in := e.svc.Store.Intents("plaud/rec-1")[DestSpekRFC]
	if in == nil || in.Status != IntentFailed || !strings.Contains(in.Error, "provider exploded") {
		t.Fatalf("intent = %+v", in)
	}
	if dest.count() != 0 {
		t.Fatal("a failed transcription must not send the share")
	}
	if it := e.item(t); it.ShareIntents[DestSpekRFC] == nil || it.Dismissed {
		t.Fatalf("item = %+v", it)
	}
}

// Unavailable transcription refuses audio-only shares; DIBS and items with
// text are unaffected.
func TestShareWhenReadyUnavailableAndTextItems(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, dest)
	_, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts())
	if !errors.Is(err, ErrTranscribeUnavailable) || !strings.Contains(err.Error(), TranscribeBlockedDisabled) {
		t.Fatalf("err = %v", err)
	}
	if len(e.svc.Store.Intents("plaud/rec-1")) != 0 || dest.count() != 0 {
		t.Fatal("nothing may be stored or sent when transcription is unavailable")
	}
	withText := e.item(t)
	withText.Pending = false
	out, err := e.svc.ShareWhenReady(context.Background(), withText, intentFor(DestSpekRFC), e.opts())
	e.svc.Wait()
	if err != nil || out.Waiting || dest.count() != 1 {
		t.Fatalf("item with text: %+v %v delivered=%d", out, err, dest.count())
	}
}

// DIBS is not a transcribe-first destination: it routes directly.
func TestRouteDibsIsUnaffectedByTranscription(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dibs := &contentDest{}
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, dibs)
	if _, err := e.svc.Route(context.Background(), e.item(t), Request{Destination: DestDibs}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if len(e.svc.Store.Intents("plaud/rec-1")) != 0 {
		t.Fatal("DIBS must not create a share intent")
	}
}

// A restart with a persisted intent resumes: the transcript is made again
// and the share runs. The intent file holds no item text.
func TestShareIntentSurvivesRestart(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	e.svc.Getenv = withKeyEnv
	var deferred []func()
	e.svc.Run = func(fn func()) { deferred = append(deferred, fn) }
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts()); err != nil {
		t.Fatal(err)
	}
	if dest.count() != 0 {
		t.Fatal("share must wait for the transcript")
	}
	raw, err := os.ReadFile(e.svc.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Standup", "not transcribed", "hello hive", "secret-signature"} {
		if strings.Contains(string(raw), text) {
			t.Fatalf("intent state stores item text or a secret (%q): %s", text, raw)
		}
	}
	if info, _ := os.Stat(e.svc.Store.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v", info.Mode().Perm())
	}
	// "Restart": a fresh service over the same files; the queued job is gone.
	svc2 := NewService(config.InboxConfig{Transcription: activeTx()}, NewStore(e.svc.Store.Path), []Source{e.src}, []Destination{dest}, nil)
	svc2.Run = func(fn func()) { fn() }
	svc2.Getenv = withKeyEnv
	svc2.Now = func() time.Time { return e.now }
	svc2.EnableTranscription(NewTranscriptStore(e.dir))
	svc2.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return e.fake, nil }
	svc2.TranscribeBackoff = func(int) time.Duration { return 0 }
	svc2.ResumeIntents(context.Background())
	svc2.Wait()
	if dest.count() != 1 || !strings.Contains(dest.got[0].Text(), "hello hive") {
		t.Fatalf("restart did not resume the share: %+v", dest.got)
	}
	if len(svc2.Store.Intents("plaud/rec-1")) != 0 {
		t.Fatal("intent should be cleared after the resumed share")
	}
}

// Other destinations (the spek session) register through the hook.
func TestRegisterIntentRunnerHook(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, activeTx(), srv.URL)
	e.svc.Getenv = withKeyEnv
	var got ShareIntent
	e.svc.RegisterIntentRunner("spek", func(_ context.Context, it Item, in ShareIntent) error {
		got = in
		if it.Pending {
			t.Error("runner must see the item as having text")
		}
		return nil
	})
	in := ShareIntent{Destination: "spek", Actor: "rw", Params: map[string]string{"session": "new"}}
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), in, e.opts()); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if got.Destination != "spek" || got.Params["session"] != "new" || got.Actor != "rw" {
		t.Fatalf("runner got %+v", got)
	}
}

// An old state file (no intents) still loads.
func TestStoreLoadsOldStateWithoutIntents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	old, _ := json.Marshal(map[string]any{"plaud/a": map[string]any{"dismissed": true}})
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	items := []Item{{Source: "plaud", ID: "a"}}
	NewStore(path).Apply(items)
	if !items[0].Dismissed || items[0].ShareIntents != nil {
		t.Fatalf("items = %+v", items)
	}
}

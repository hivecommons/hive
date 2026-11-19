package inbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mailin/wire"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// autoFake wires a fake transcriber into svc and returns it.
func autoFake(svc *Service, errs ...error) *transcribe.Fake {
	fake := &transcribe.Fake{Result: transcribe.Result{Segments: []transcribe.Segment{{Text: "auto text"}}, Duration: time.Second}, Errs: errs}
	svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	svc.Getenv = func(string) string { return "" }
	svc.TranscribeBackoff = func(int) time.Duration { return 0 }
	return fake
}

func TestAutoTranscribeUploadInManualModeAndIdempotent(t *testing.T) {
	// Manual mode: dropped audio is the hive's own and needs no click.
	e := newUploadEnv(t, config.InboxConfig{Transcription: activeTx()})
	e.svc.Run = func(fn func()) { fn() }
	store := NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	e.svc.EnableTranscription(store)
	fake := autoFake(e.svc)
	it, err := addMedia(t, e, "memo.ogg", oggHead, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := store.Get(UploadType, it.ID); !ok || rec.Status != TranscriptionDone || fake.Calls() != 1 {
		t.Fatalf("upload not auto-transcribed: %+v %v calls %d", rec, ok, fake.Calls())
	}
	// Done: a second hook (sweep, a repeated create) starts nothing.
	if _, ok := e.svc.AutoTranscribe(context.Background(), it, TranscribeOptions{}); ok || fake.Calls() != 1 {
		t.Fatalf("a transcribed upload was started again: calls %d", fake.Calls())
	}
	if n, _ := e.svc.TranscribeSweep(context.Background(), TranscribeOptions{}); n != 0 || fake.Calls() != 1 {
		t.Fatalf("sweep re-queued a done upload: %d", n)
	}
}

func TestAutoTranscribeNoDoubleStartWhileInFlight(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{Transcription: activeTx()})
	var pending []func()
	e.svc.Run = func(fn func()) { pending = append(pending, fn) }
	e.svc.EnableTranscription(NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts")))
	fake := autoFake(e.svc)
	it, err := addMedia(t, e, "memo.ogg", oggHead, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("creation queued %d runs, want 1", len(pending))
	}
	// Queued and not yet run: every other hook is a no-op.
	if st, ok := e.svc.AutoTranscribe(context.Background(), it, TranscribeOptions{}); ok || st.Status != TranscriptionQueued {
		t.Fatalf("second auto start = %+v %v", st, ok)
	}
	if n, _ := e.svc.TranscribeSweep(context.Background(), TranscribeOptions{}); n != 0 {
		t.Fatalf("sweep queued %d while in flight", n)
	}
	if len(pending) != 1 {
		t.Fatalf("runs = %d, want 1", len(pending))
	}
	items, _ := e.svc.Items(context.Background())
	if len(items) != 1 || items[0].Transcription == nil || items[0].Transcription.Status != TranscriptionQueued || items[0].Transcribable || !items[0].AutoTranscribe {
		t.Fatalf("in-flight item = %+v", items)
	}
	pending[0]()
	e.svc.Wait()
	if fake.Calls() != 1 {
		t.Fatalf("calls = %d", fake.Calls())
	}
}

func TestAutoTranscribeRecordingOnFinalizeOnce(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	fake := withTranscription(t, svc, activeTx())
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	svc.Wait()
	if fake.Calls() != 1 {
		t.Fatalf("finalize calls = %d, want 1", fake.Calls())
	}
	got, _ := src.Store.Get(m.ID)
	svc.autoTranscribeRecording(src, got, RecordingOptions{})
	svc.Wait()
	if fake.Calls() != 1 {
		t.Fatal("a finished recording was transcribed twice")
	}
}

func TestAutoTranscribeMailedInAudioAfterApprovalOnly(t *testing.T) {
	e, _ := newEmailSource(t)
	svc := NewService(config.InboxConfig{Transcription: activeTx()}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), nil, nil, nil)
	svc.Run = func(fn func()) { fn() }
	store := NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	svc.EnableTranscription(store)
	fake := autoFake(svc)
	svc.EnableEmail(e)
	ctx := context.Background()

	trusted, _ := e.Add(emailDelivery("t1", att("memo.m4a", wire.AttachAudio, "audio/mp4", emailAudio)), true)
	if _, ok := svc.AutoTranscribe(ctx, trusted[0], TranscribeOptions{}); !ok {
		t.Fatal("trusted mailed-in audio not auto-transcribed")
	}
	if rec, _ := store.Get(EmailType, trusted[0].ID); rec.Status != TranscriptionDone || rec.Text == "" {
		t.Fatalf("record = %+v", rec)
	}
	untrusted, _ := e.Add(emailDelivery("u1", att("memo.m4a", wire.AttachAudio, "audio/mp4", emailAudio)), false)
	if _, ok := svc.AutoTranscribe(ctx, untrusted[0], TranscribeOptions{}); ok || fake.Calls() != 1 {
		t.Fatalf("unapproved audio was transcribed: calls %d", fake.Calls())
	}
	approved, err := e.Approve(untrusted[0].ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.AutoTranscribe(ctx, approved, TranscribeOptions{}); !ok || fake.Calls() != 2 {
		t.Fatalf("approved audio not auto-transcribed: calls %d", fake.Calls())
	}
	// Markdown has nothing to transcribe.
	md, _ := e.Add(emailDelivery("m1", att("n.md", wire.AttachMarkdown, "text/markdown", emailMD)), true)
	if _, ok := svc.AutoTranscribe(ctx, md[0], TranscribeOptions{}); ok {
		t.Fatal("markdown was queued")
	}
}

func TestAutoTranscribeSkipsWhenInactive(t *testing.T) {
	for name, tx := range map[string]config.InboxTranscription{
		"disabled":   {ConsentAcknowledged: true},
		"no consent": {Enabled: true},
	} {
		e := newUploadEnv(t, config.InboxConfig{Transcription: tx})
		e.svc.Run = func(fn func()) { fn() }
		e.svc.EnableTranscription(NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts")))
		fake := autoFake(e.svc)
		if _, err := addMedia(t, e, "memo.ogg", oggHead, []byte("body")); err != nil {
			t.Fatal(err)
		}
		if fake.Calls() != 0 {
			t.Fatalf("%s: transcribed anyway", name)
		}
	}
}

func TestAutoTranscribeFailureRecordedAndRetriedWithBackoff(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{Transcription: activeTx()})
	e.svc.Run = func(fn func()) { fn() }
	store := NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	e.svc.EnableTranscription(store)
	e.svc.Now = func() time.Time { return e.now }
	fake := autoFake(e.svc, errors.New("provider said no"))
	it, err := addMedia(t, e, "memo.ogg", oggHead, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(UploadType, it.ID)
	if !ok || rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "provider said no") || rec.Attempts != 1 {
		t.Fatalf("failure not recorded: %+v %v", rec, ok)
	}
	items, _ := e.svc.Items(context.Background())
	if len(items) != 1 || items[0].Transcription == nil || items[0].Transcription.Status != TranscriptionFailed || !items[0].Transcribable {
		t.Fatalf("failed item must show its state and stay retryable: %+v", items)
	}
	// Not due yet: no immediate retry loop.
	if _, ok := e.svc.AutoTranscribe(context.Background(), items[0], TranscribeOptions{}); ok || fake.Calls() != 1 {
		t.Fatalf("failed run retried before its backoff: calls %d", fake.Calls())
	}
	// After the backoff the sweep retries it.
	e.now = e.now.Add(2 * autoRetryBase)
	if n, _ := e.svc.TranscribeSweep(context.Background(), TranscribeOptions{}); n != 1 || fake.Calls() != 2 {
		t.Fatalf("retry after backoff: queued %d calls %d", n, fake.Calls())
	}
	if rec, _ := store.Get(UploadType, it.ID); rec.Status != TranscriptionDone || rec.Attempts != 2 {
		t.Fatalf("retried record = %+v", rec)
	}
}

func TestRecoverTranscriptionsResumesInterruptedRunsAtBoot(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	// Recorded while transcription was off: nothing ran.
	fake := withTranscription(t, svc, config.InboxTranscription{})
	running := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	queued := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	looping := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	untouched := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	store := svc.transcriptStore()
	// What the previous process left on disk when it died.
	for _, rec := range []TranscriptRecord{
		{Source: RecordingType, ID: running.ID, Status: TranscriptionRunning, Attempts: 1},
		{Source: RecordingType, ID: queued.ID, Status: TranscriptionQueued, Attempts: 1},
		{Source: RecordingType, ID: looping.ID, Status: TranscriptionRunning, Attempts: TranscriptionMaxAttempts},
	} {
		if err := store.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	// "Restart": enable transcription, recover, then the boot sweep.
	withTranscription(t, svc, activeTx())
	svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	n, err := svc.RecoverTranscriptions()
	if err != nil || n != 3 {
		t.Fatalf("recovered %d, %v; want 3", n, err)
	}
	if rec, _ := store.Get(RecordingType, running.ID); rec.Status != TranscriptionFailed || !strings.HasPrefix(rec.Reason, ReasonInterrupted) || rec.Attempts != 1 {
		t.Fatalf("interrupted running = %+v", rec)
	}
	if rec, _ := store.Get(RecordingType, queued.ID); rec.Attempts != 0 {
		t.Fatalf("a run that never started kept its attempt: %+v", rec)
	}
	queuedN, err := svc.TranscribeSweep(context.Background(), TranscribeOptions{})
	svc.Wait()
	if err != nil || queuedN != 3 {
		t.Fatalf("boot sweep queued %d, %v; want running, queued and the untranscribed one", queuedN, err)
	}
	for id, attempts := range map[string]int{running.ID: 2, queued.ID: 1, untouched.ID: 1} {
		if rec, _ := store.Get(RecordingType, id); rec.Status != TranscriptionDone || rec.Attempts != attempts {
			t.Fatalf("%s after boot = %+v, want done after %d attempts", id, rec, attempts)
		}
	}
	// A recording that keeps crashing the hive stops at the attempt bound.
	if rec, _ := store.Get(RecordingType, looping.ID); rec.Status != TranscriptionFailed || rec.Attempts != TranscriptionMaxAttempts {
		t.Fatalf("crash-looping recording = %+v", rec)
	}
	// Recovery never touches a job this process is running.
	var held []func()
	svc.Run = func(fn func()) { held = append(held, fn) }
	live := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	if rec, _ := store.Get(RecordingType, live.ID); len(held) != 1 || rec.Status != TranscriptionQueued {
		t.Fatalf("new recording not queued: %+v, runs %d", rec, len(held))
	}
	if n, _ := svc.RecoverTranscriptions(); n != 0 {
		t.Fatalf("recovered a live job: %d", n)
	}
	held[0]()
	svc.Wait()
}

func TestTranscribeConcurrencySlots(t *testing.T) {
	const slots = 2
	svc := NewService(config.InboxConfig{}, NewStore(""), nil, nil, nil)
	svc.TranscribeConcurrency = slots
	jobs := make([]*txJob, slots+1)
	for i := range jobs {
		jobs[i] = svc.txEnqueue(Item{Source: "plaud", ID: string(rune('a' + i))}, "", "", transcribe.Speed{})
	}
	ready := func(j *txJob) bool {
		select {
		case <-j.ready:
			return true
		default:
			return false
		}
	}
	if !ready(jobs[0]) || !ready(jobs[1]) || ready(jobs[2]) {
		t.Fatal("the first two jobs must run, the third wait")
	}
	var st TranscriptionState
	if !svc.txLive("plaud", "c", &st) || st.QueuePosition != 1 {
		t.Fatalf("waiting job position = %d, want 1", st.QueuePosition)
	}
	svc.txDequeue(jobs[1])
	if !ready(jobs[2]) {
		t.Fatal("a freed slot did not wake the waiting job")
	}

	for value, want := range map[string]int{"": DefaultTranscribeConcurrency, "abc": DefaultTranscribeConcurrency, "0": DefaultTranscribeConcurrency, "3": 3, "1000": maxTranscribeConcurrency} {
		s := NewService(config.InboxConfig{}, NewStore(""), nil, nil, nil)
		s.Getenv = func(k string) string {
			if k == TranscribeConcurrencyEnv {
				return value
			}
			return ""
		}
		if got := s.transcribeConcurrency(); got != want {
			t.Fatalf("%s=%q: %d, want %d", TranscribeConcurrencyEnv, value, got, want)
		}
	}
}

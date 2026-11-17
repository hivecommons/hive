package inbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

const txAudio = "AUDIO"

// txSource is a pending-recording source with on-demand audio links.
type txSource struct {
	items    []Item
	content  Content
	link     string
	linkErr  error
	fetchErr error
}

func (s *txSource) Type() string                         { return "plaud" }
func (s *txSource) List(context.Context) ([]Item, error) { return s.items, nil }
func (s *txSource) Fetch(context.Context, Item) (Content, error) {
	return s.content, s.fetchErr
}
func (s *txSource) AudioLink(context.Context, Item) (AudioLink, error) {
	if s.linkErr != nil {
		return AudioLink{}, s.linkErr
	}
	return AudioLink{URL: s.link, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// contentDest records the content a route delivered.
type contentDest struct{ got Content }

func (d *contentDest) Name() string { return DestDibs }
func (d *contentDest) Deliver(_ context.Context, _ Item, c Content, _ Request) (Result, error) {
	d.got = c
	return Result{URL: "https://dibs.example/i/1"}, nil
}

func audioServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag-1"`)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type txEnv struct {
	svc    *Service
	src    *txSource
	store  *TranscriptStore
	fake   *transcribe.Fake
	audits []TranscribeAudit
	dir    string
	now    time.Time
}

func (e *txEnv) opts() TranscribeOptions {
	return TranscribeOptions{Actor: "owner@example.com", Audit: func(a TranscribeAudit) { e.audits = append(e.audits, a) }}
}

func activeTx() config.InboxTranscription {
	return config.InboxTranscription{Enabled: true, ConsentAcknowledged: true}
}

func newTxEnv(t *testing.T, tx config.InboxTranscription, audioURL string, dests ...Destination) *txEnv {
	t.Helper()
	e := &txEnv{dir: filepath.Join(t.TempDir(), "connector-state", "transcripts"), now: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	e.src = &txSource{
		items:   []Item{{Source: "plaud", ID: "rec-1", Title: "Standup", Pending: true, DurationMS: 90000}},
		content: Content{Title: "Standup", Pending: true, Markdown: "not transcribed", Provenance: "plaud://recordings/rec-1"},
		link:    audioURL + "/rec-1.mp3?sig=secret-signature",
	}
	e.svc = NewService(config.InboxConfig{Transcription: tx}, NewStore(filepath.Join(t.TempDir(), "inbox.json")), []Source{e.src}, dests, nil)
	e.svc.Run = func(fn func()) { fn() }
	e.svc.Now = func() time.Time { return e.now }
	e.store = NewTranscriptStore(e.dir)
	e.svc.EnableTranscription(e.store)
	e.fake = &transcribe.Fake{Result: transcribe.Result{
		Segments: []transcribe.Segment{{Start: time.Second, Speaker: "Speaker 1", Text: "hello hive"}},
		Language: "en", Duration: 90 * time.Second,
	}}
	e.svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return e.fake, nil }
	e.svc.TranscribeBackoff = func(int) time.Duration { return 0 }
	return e
}

func (e *txEnv) item(t *testing.T) Item {
	t.Helper()
	it, ok, err := e.svc.Find(context.Background(), "plaud", "rec-1")
	if !ok || err != nil {
		t.Fatalf("find = %v %v", ok, err)
	}
	return it
}

func TestTranscribeManualRunStoresTranscriptAndContentFallsBack(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &contentDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	ctx := context.Background()

	it := e.item(t)
	if !it.Transcribable || it.Transcription != nil {
		t.Fatalf("pending item with audio must be transcribable: %+v", it)
	}
	st, err := e.svc.Transcribe(ctx, it, e.opts())
	if err != nil || st.Status != TranscriptionQueued || st.Attempts != 1 {
		t.Fatalf("transcribe = %+v %v", st, err)
	}
	rec, ok := e.store.Get("plaud", "rec-1")
	if !ok || rec.Status != TranscriptionDone || rec.Text != "[00:01] Speaker 1: hello hive" || rec.Language != "en" || rec.AudioSeconds != 90 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.AudioSize != int64(len(txAudio)) || rec.AudioETag != "etag-1" || e.fake.Bytes() != int64(len(txAudio)) {
		t.Fatalf("audio identity = %d %q, streamed %d", rec.AudioSize, rec.AudioETag, e.fake.Bytes())
	}
	path := e.store.path("plaud", "rec-1")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("transcript file mode = %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret-signature") || strings.Contains(string(data), srv.URL) {
		t.Fatalf("transcript file leaks the audio link: %s", data)
	}

	// Audit: one entry with the run's facts, never the link.
	if len(e.audits) != 1 {
		t.Fatalf("audits = %+v", e.audits)
	}
	a := e.audits[0]
	if a.Actor != "owner@example.com" || a.Outcome != TranscriptionDone || a.Provider != "deepgram" || a.Model != "nova-3" || a.AudioSeconds != 90 || a.ItemID != "rec-1" {
		t.Fatalf("audit = %+v", a)
	}
	fields := strings.Join(a.Fields(), " ")
	if !strings.Contains(fields, "outcome done") || !strings.Contains(fields, "audio_seconds 90") || !strings.Contains(fields, "estimated_usd 0.006") || strings.Contains(fields, "sig=") {
		t.Fatalf("audit fields = %q", fields)
	}
	if u := e.store.Usage(e.now); u.MonthSeconds != 90 || u.TodayItems != 1 {
		t.Fatalf("usage = %+v", u)
	}

	// The listing: no longer pending, carries the state, no button.
	it = e.item(t)
	if it.Pending || !it.HasTranscript || it.Transcribable || it.Transcription == nil || it.Transcription.Status != TranscriptionDone {
		t.Fatalf("item after transcription = %+v", it)
	}
	c, err := e.svc.Content(ctx, it)
	if err != nil || c.Pending || c.Transcript != rec.Text || c.TranscriptLabel != "Transcript (machine: deepgram/nova-3)" {
		t.Fatalf("content = %+v %v", c, err)
	}
	if !strings.HasPrefix(c.Text(), "Transcript (machine: deepgram/nova-3):\n[00:01]") {
		t.Fatalf("text = %q", c.Text())
	}
	// Routing reads the same fallback content.
	if _, err := e.svc.Route(ctx, it, Request{Destination: DestDibs}); err != nil {
		t.Fatal(err)
	}
	if dest.got.Transcript != rec.Text || dest.got.TranscriptLabel == "" {
		t.Fatalf("delivered content = %+v", dest.got)
	}
	if _, err := e.svc.Transcribe(ctx, it, e.opts()); !errors.Is(err, ErrAlreadyTranscribed) {
		t.Fatalf("second transcribe = %v", err)
	}

	// The source going offline still serves the machine transcript.
	e.src.fetchErr = errors.New("plaud offline")
	if c, err := e.svc.Content(ctx, it); err != nil || c.Transcript != rec.Text || c.Provenance != "plaud://recordings/rec-1" {
		t.Fatalf("offline content = %+v %v", c, err)
	}
	e.src.fetchErr = nil

	// An upstream transcript, once it appears, wins.
	e.src.content = Content{Title: "Standup", Summary: "plaud summary", Transcript: "[00:00] Andy: real words"}
	e.src.items[0].Pending, e.src.items[0].HasTranscript = false, true
	c, err = e.svc.Content(ctx, e.item(t))
	if err != nil || c.Transcript != "[00:00] Andy: real words" || c.TranscriptLabel != "" {
		t.Fatalf("upstream content = %+v %v", c, err)
	}
}

func TestTranscribeGates(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	ctx := context.Background()

	off := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	if _, err := off.svc.Transcribe(ctx, off.item(t), off.opts()); !errors.Is(err, ErrTranscriptionDisabled) {
		t.Fatalf("disabled = %v", err)
	}
	if off.item(t).Transcribable {
		t.Fatal("disabled transcription must not offer the button")
	}
	noConsent := newTxEnv(t, config.InboxTranscription{Enabled: true}, srv.URL)
	if _, err := noConsent.svc.Transcribe(ctx, noConsent.item(t), noConsent.opts()); !errors.Is(err, ErrTranscriptionConsent) {
		t.Fatalf("no consent = %v", err)
	}
	if noConsent.item(t).Transcribable {
		t.Fatal("unconsented transcription must not offer the button")
	}
	noStore := NewService(config.InboxConfig{Transcription: activeTx()}, NewStore(""), nil, nil, nil)
	if _, err := noStore.Transcribe(ctx, Item{Source: "plaud", ID: "x", Pending: true}, TranscribeOptions{}); !errors.Is(err, ErrTranscriptionDisabled) {
		t.Fatalf("no store = %v", err)
	}

	e := newTxEnv(t, activeTx(), srv.URL)
	it := e.item(t)
	it.Pending = false
	if _, err := e.svc.Transcribe(ctx, it, e.opts()); !errors.Is(err, ErrNotPending) {
		t.Fatalf("not pending = %v", err)
	}
	if err := e.store.Put(TranscriptRecord{Source: "plaud", ID: "rec-1", Status: TranscriptionRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); !errors.Is(err, ErrTranscriptionBusy) {
		t.Fatalf("busy = %v", err)
	}
	if e.item(t).Transcribable {
		t.Fatal("a running item must not offer the button")
	}
	if e.fake.Calls() != 0 || len(e.audits) != 0 {
		t.Fatal("gated requests must not upload or audit")
	}
}

func TestTranscribeBudgetAndLimitSkips(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	ctx := context.Background()
	cases := []struct {
		name   string
		tx     config.InboxTranscription
		setup  func(e *txEnv)
		prefix string
	}{
		{"too long", config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MaxDurationMinutes: 1}, nil, SkipTooLong},
		{"daily cap", config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, DailyMaxItems: 2}, func(e *txEnv) { _ = e.store.AddUsage(e.now, 0, 2) }, SkipBudget},
		{"monthly budget", config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MonthlyBudgetMinutes: 10}, func(e *txEnv) { _ = e.store.AddUsage(e.now, 9*60, 0) }, SkipBudget},
		{"unsupported", activeTx(), func(e *txEnv) {
			e.svc.SetSources([]Source{&fakeSource{items: e.src.items}})
		}, SkipUnsupported},
	}
	for _, tc := range cases {
		e := newTxEnv(t, tc.tx, srv.URL)
		if tc.setup != nil {
			tc.setup(e)
		}
		it := e.item(t)
		st, err := e.svc.Transcribe(ctx, it, e.opts())
		if err != nil || st.Status != TranscriptionSkipped || !strings.HasPrefix(st.Reason, tc.prefix+":") {
			t.Fatalf("%s: state = %+v err = %v", tc.name, st, err)
		}
		if e.fake.Calls() != 0 {
			t.Fatalf("%s: a skipped item must not be uploaded", tc.name)
		}
		if len(e.audits) != 1 || e.audits[0].Outcome != TranscriptionSkipped || e.audits[0].Reason != st.Reason {
			t.Fatalf("%s: audits = %+v", tc.name, e.audits)
		}
		got := e.item(t)
		if got.Transcription == nil || got.Transcription.Status != TranscriptionSkipped {
			t.Fatalf("%s: listing = %+v", tc.name, got)
		}
	}
	// A budget skip can be retried once budget frees up; attempts accumulate.
	e := newTxEnv(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, DailyMaxItems: 1}, srv.URL)
	_ = e.store.AddUsage(e.now, 0, 1)
	if st, _ := e.svc.Transcribe(ctx, e.item(t), e.opts()); st.Status != TranscriptionSkipped {
		t.Fatalf("capped = %+v", st)
	}
	if !e.item(t).Transcribable {
		t.Fatal("a skipped item may be retried")
	}
	e.now = e.now.Add(24 * time.Hour)
	if st, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); err != nil || st.Attempts != 2 {
		t.Fatalf("next day = %+v %v", st, err)
	}
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionDone {
		t.Fatalf("record = %+v", rec)
	}
}

func TestTranscribeOversizeAudio(t *testing.T) {
	ctx := context.Background()
	// Content-Length over the cap: refused before any byte is uploaded.
	srv := audioServer(t, http.StatusOK, "0123456789")
	e := newTxEnv(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MaxBytes: 4}, srv.URL)
	if _, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.store.Get("plaud", "rec-1")
	if rec.Status != TranscriptionSkipped || !strings.HasPrefix(rec.Reason, SkipTooLarge+":") || e.fake.Bytes() != 0 {
		t.Fatalf("record = %+v streamed=%d", rec, e.fake.Bytes())
	}
	if len(e.audits) != 1 || e.audits[0].AudioSeconds != 0 {
		t.Fatalf("an unsent upload bills nothing: %+v", e.audits)
	}
	// Chunked (no Content-Length): the limit reader cuts the stream.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "0123")
			w.(http.Flusher).Flush()
		}
	}))
	defer chunked.Close()
	e = newTxEnv(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MaxBytes: 5}, chunked.URL)
	if _, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionSkipped || !strings.HasPrefix(rec.Reason, SkipTooLarge) {
		t.Fatalf("chunked record = %+v", rec)
	}
}

func TestTranscribeFailures(t *testing.T) {
	ctx := context.Background()
	ok := audioServer(t, http.StatusOK, txAudio)
	missing := audioServer(t, http.StatusNotFound, "")
	flaky := audioServer(t, http.StatusServiceUnavailable, "")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	run := func(t *testing.T, audioURL string, setup func(e *txEnv)) (*txEnv, TranscriptRecord) {
		t.Helper()
		e := newTxEnv(t, activeTx(), audioURL)
		if setup != nil {
			setup(e)
		}
		if _, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); err != nil {
			t.Fatal(err)
		}
		rec, _ := e.store.Get("plaud", "rec-1")
		if strings.Contains(rec.Reason, "secret-signature") {
			t.Fatalf("reason leaks the audio link: %q", rec.Reason)
		}
		if len(e.audits) != 1 || e.audits[0].Outcome != rec.Status {
			t.Fatalf("audits = %+v", e.audits)
		}
		return e, rec
	}

	_, rec := run(t, ok.URL, func(e *txEnv) { e.fake.Errs = []error{errors.New("provider exploded")} })
	if rec.Status != TranscriptionFailed || rec.Reason != "provider exploded" {
		t.Fatalf("provider error = %+v", rec)
	}
	e, rec := run(t, ok.URL, func(e *txEnv) {
		e.fake.Errs = []error{&transcribe.RetryableError{Err: errors.New("429")}, &transcribe.RetryableError{Err: errors.New("429")}}
	})
	if rec.Status != TranscriptionDone || e.fake.Calls() != 3 {
		t.Fatalf("retried run = %+v calls=%d", rec, e.fake.Calls())
	}
	e, rec = run(t, ok.URL, func(e *txEnv) {
		e.fake.Errs = []error{&transcribe.RetryableError{Err: errors.New("busy")}, &transcribe.RetryableError{Err: errors.New("busy")}, &transcribe.RetryableError{Err: errors.New("busy")}}
	})
	if rec.Status != TranscriptionFailed || e.fake.Calls() != TranscriptionMaxAttempts {
		t.Fatalf("exhausted run = %+v calls=%d", rec, e.fake.Calls())
	}
	if !e.item(t).Transcribable {
		t.Fatal("a failed item may be retried by hand")
	}
	_, rec = run(t, missing.URL, nil)
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "HTTP 404") || !strings.Contains(rec.Reason, strings.TrimPrefix(missing.URL, "http://")) {
		t.Fatalf("404 = %+v", rec)
	}
	_, rec = run(t, flaky.URL, nil)
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "HTTP 503") {
		t.Fatalf("503 = %+v", rec)
	}
	_, rec = run(t, deadURL, nil)
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "audio download from") {
		t.Fatalf("transport = %+v", rec)
	}
	_, rec = run(t, ok.URL, func(e *txEnv) { e.src.link = "http://[::1" })
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "invalid link") || !strings.Contains(rec.Reason, "unknown host") {
		t.Fatalf("bad link = %+v", rec)
	}
	_, rec = run(t, ok.URL, func(e *txEnv) { e.src.linkErr = ErrNoAudio })
	if rec.Status != TranscriptionSkipped || !strings.HasPrefix(rec.Reason, SkipUnsupported) {
		t.Fatalf("no audio = %+v", rec)
	}
	e, rec = run(t, ok.URL, func(e *txEnv) { e.fake.Result.Segments = nil })
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "no speech") {
		t.Fatalf("empty = %+v", rec)
	}
	if u := e.store.Usage(e.now); u.MonthSeconds != 90 {
		t.Fatalf("an empty transcript is still billed by the provider: %+v", u)
	}
	_, rec = run(t, ok.URL, func(e *txEnv) {
		e.svc.NewTranscriber = nil
		e.svc.Getenv = func(string) string { return "" }
	})
	if rec.Status != TranscriptionFailed || !strings.Contains(rec.Reason, "$DEEPGRAM_API_KEY") {
		t.Fatalf("no key = %+v", rec)
	}
}

func TestTranscribeBackgroundRun(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, activeTx(), srv.URL)
	e.svc.Run = nil
	if _, err := e.svc.Transcribe(context.Background(), e.item(t), TranscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionDone {
		t.Fatalf("background record = %+v", rec)
	}
}

func TestDefaultTranscriberAndSettings(t *testing.T) {
	env := map[string]string{"DG": "dg-key"}
	getenv := func(k string) string { return env[k] }
	tr, err := DefaultTranscriber(config.InboxTranscription{APIKeyEnv: "DG", Model: "nova-2"}, getenv)
	dg, ok := tr.(*transcribe.Deepgram)
	if err != nil || !ok || dg.APIKey != "dg-key" || dg.KeyEnv != "DG" || dg.Model != "nova-2" || !dg.Diarize || dg.Language != "auto" {
		t.Fatalf("deepgram = %+v %v", tr, err)
	}
	if _, err := DefaultTranscriber(config.InboxTranscription{Provider: "whisper"}, getenv); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if _, err := DefaultTranscriber(config.InboxTranscription{}, getenv); !errors.Is(err, transcribe.ErrNoKey) {
		t.Fatalf("missing key = %v", err)
	}

	e := newTxEnv(t, activeTx(), "http://audio.invalid")
	e.svc.Getenv = getenv
	st := e.svc.Settings().Transcription
	if !st.Enabled || st.KeyPresent || st.APIKeyEnv != "DEEPGRAM_API_KEY" || st.Model != "nova-3" || st.Mode != "manual" || st.MaxBytes != 524288000 || st.MonthlyBudgetMinutes != 600 || st.Sources == nil {
		t.Fatalf("settings = %+v", st)
	}
	env["DEEPGRAM_API_KEY"] = "set"
	_ = e.store.AddUsage(e.now, 150, 2)
	st = e.svc.TranscriptionSettings()
	if !st.KeyPresent || st.MonthMinutesUsed != 2.5 || st.TodayItems != 2 {
		t.Fatalf("settings with usage = %+v", st)
	}
	t.Setenv("DEEPGRAM_API_KEY", "")
	plain := NewService(config.InboxConfig{}, NewStore(""), nil, nil, nil)
	if plain.TranscriptionSettings().KeyPresent {
		t.Fatal("os.Getenv fallback: DEEPGRAM_API_KEY is not set in tests")
	}
}

func TestTranscriptStore(t *testing.T) {
	dir := t.TempDir()
	s := NewTranscriptStore(filepath.Join(dir, "transcripts"))
	if _, ok := s.Get("plaud", "missing"); ok {
		t.Fatal("missing record found")
	}
	for _, id := range []string{"../../escape", ".hidden", "a/b", ""} {
		p := s.path("plaud", id)
		if !strings.HasPrefix(p, filepath.Join(dir, "transcripts", "plaud")+string(os.PathSeparator)) {
			t.Fatalf("id %q escapes the store: %s", id, p)
		}
	}
	if safeName("a/b") == safeName("a_b") {
		t.Fatal("rewritten names must not collide")
	}
	if err := s.Put(TranscriptRecord{Source: "plaud", ID: "a/b", Status: TranscriptionDone, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if rec, ok := s.Get("plaud", "a/b"); !ok || rec.Text != "x" {
		t.Fatalf("round trip = %+v", rec)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < usageKeepDays+5; i++ {
		if err := s.AddUsage(start.AddDate(0, 0, i*10), 60, 1); err != nil {
			t.Fatal(err)
		}
	}
	l := s.ledger()
	if len(l.Days) != usageKeepDays || len(l.Months) > usageKeepMonths {
		t.Fatalf("ledger not pruned: %d days, %d months", len(l.Days), len(l.Months))
	}
	// A store whose directory is a file cannot write.
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	bad := NewTranscriptStore(blocker)
	if err := bad.Put(TranscriptRecord{Source: "plaud", ID: "x"}); err == nil {
		t.Fatal("put into a file path must fail")
	}
	if err := bad.AddUsage(start, 1, 1); err == nil {
		t.Fatal("usage into a file path must fail")
	}
}

func TestTranscriptionHelpers(t *testing.T) {
	if linkHost("::bad") != "unknown host" || linkHost("https://cdn.example/x?sig=1") != "cdn.example" {
		t.Fatal("linkHost")
	}
	if mimeFromURL("https://cdn.example/a.M4A?x=1") != "audio/mp4" || mimeFromURL("https://cdn.example/a") != "" || mimeFromURL("::bad") != "" {
		t.Fatal("mimeFromURL")
	}
	if redactLink("x", "") != "x" || redactLink("get https://l/?s=1 failed", "https://l/?s=1") != "get [audio link] failed" {
		t.Fatal("redactLink")
	}
	fields := TranscribeAudit{Source: "plaud", ItemID: "r", Outcome: "skipped", Reason: "budget: x"}.Fields()
	if fields[len(fields)-2] != "reason" || fields[len(fields)-1] != "budget: x" {
		t.Fatalf("fields = %v", fields)
	}
	if MachineTranscriptLabel("deepgram", "nova-3") != "Transcript (machine: deepgram/nova-3)" {
		t.Fatal("label")
	}
	// No machine transcript: content passes through untouched, errors too.
	svc := NewService(config.InboxConfig{}, NewStore(""), nil, nil, nil)
	svc.EnableTranscription(NewTranscriptStore(t.TempDir()))
	boom := errors.New("boom")
	if _, err := svc.withMachineTranscript(Item{Source: "plaud", ID: "x"}, Content{}, boom); !errors.Is(err, boom) {
		t.Fatalf("passthrough err = %v", err)
	}
}

func TestTranscribeUnavailableReason(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	withKey := func(string) string { return "dg-secret-value" }
	cases := []struct {
		name   string
		tx     config.InboxTranscription
		getenv func(string) string
		want   string
	}{
		{"disabled", config.InboxTranscription{}, withKey, TranscribeBlockedDisabled},
		{"no key", activeTx(), func(string) string { return "" }, TranscribeBlockedNoKey},
		{"no consent", config.InboxTranscription{Enabled: true}, withKey, TranscribeBlockedConsent},
		{"available", activeTx(), withKey, ""},
	}
	for _, c := range cases {
		e := newTxEnv(t, c.tx, srv.URL)
		e.svc.Getenv = c.getenv
		it := e.item(t)
		if it.TranscribeUnavailable != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, it.TranscribeUnavailable, c.want)
		}
		if strings.Contains(it.TranscribeUnavailable, "dg-secret") {
			t.Errorf("%s: reason leaks the key", c.name)
		}
	}

	budget := newTxEnv(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MonthlyBudgetMinutes: 1}, srv.URL)
	budget.svc.Getenv = withKey
	if err := budget.store.AddUsage(budget.now, 120, 1); err != nil {
		t.Fatal(err)
	}
	if got := budget.item(t).TranscribeUnavailable; got != TranscribeBlockedBudget {
		t.Errorf("budget: reason = %q", got)
	}

	busy := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	if err := busy.store.Put(TranscriptRecord{Source: "plaud", ID: "rec-1", Status: TranscriptionRunning}); err != nil {
		t.Fatal(err)
	}
	if got := busy.item(t).TranscribeUnavailable; got != "" {
		t.Errorf("busy item must carry no reason, got %q", got)
	}
}

package inbox

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

func autoTx(sources ...string) config.InboxTranscription {
	return config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, Mode: config.InboxTranscriptionModeAuto, Sources: sources}
}

func TestTranscribeSweepOnlyAutoModeOptedInSources(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	ctx := context.Background()
	for name, tx := range map[string]config.InboxTranscription{
		"manual mode":    {Enabled: true, ConsentAcknowledged: true, Sources: []string{"plaud"}},
		"not opted in":   autoTx("zoom"),
		"disabled":       {Mode: config.InboxTranscriptionModeAuto, Sources: []string{"plaud"}},
		"no consent yet": {Enabled: true, Mode: config.InboxTranscriptionModeAuto, Sources: []string{"plaud"}},
	} {
		e := newTxEnv(t, tx, srv.URL)
		if n, err := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 || err != nil || e.fake.Calls() != 0 {
			t.Fatalf("%s: queued %d err %v calls %d", name, n, err, e.fake.Calls())
		}
	}
	noStore := NewService(config.InboxConfig{Transcription: autoTx("plaud")}, NewStore(""), nil, nil, nil)
	if n, _ := noStore.TranscribeSweep(ctx, TranscribeOptions{}); n != 0 {
		t.Fatal("no store, no sweep")
	}

	e := newTxEnv(t, autoTx("plaud"), srv.URL)
	n, err := e.svc.TranscribeSweep(ctx, TranscribeOptions{Actor: AutoTranscribeActor, Audit: e.opts().Audit})
	if n != 1 || err != nil || e.fake.Calls() != 1 {
		t.Fatalf("auto sweep queued %d err %v calls %d", n, err, e.fake.Calls())
	}
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionDone {
		t.Fatalf("record = %+v", rec)
	}
	if len(e.audits) != 1 || e.audits[0].Actor != AutoTranscribeActor {
		t.Fatalf("audits = %+v", e.audits)
	}
	// Dedupe: a second sweep finds nothing to do.
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 || e.fake.Calls() != 1 {
		t.Fatalf("second sweep queued %d calls %d", n, e.fake.Calls())
	}
	// A connector name counts as a source name too.
	e = newTxEnv(t, autoTx("my-plaud"), srv.URL)
	e.src.items[0].Connector = "my-plaud"
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 1 {
		t.Fatalf("connector-name opt-in queued %d", n)
	}
}

func TestTranscribeSweepRespectsBudgetWithoutRecordingSkips(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	ctx := context.Background()
	e := newTxEnv(t, autoTx("plaud"), srv.URL)
	e.fake.Result.Duration = time.Minute
	e.src.items = []Item{
		{Source: "plaud", ID: "a", Pending: true, DurationMS: 60000, RecordedAt: e.now.Add(-time.Minute)},
		{Source: "plaud", ID: "a", Pending: true, DurationMS: 60000, RecordedAt: e.now.Add(-time.Minute)},
		{Source: "plaud", ID: "b", Pending: true, DurationMS: 60000, RecordedAt: e.now.Add(-2 * time.Minute)},
		{Source: "plaud", ID: "c", Pending: true, DurationMS: 60000, RecordedAt: e.now.Add(-3 * time.Minute)},
	}
	tx := autoTx("plaud")
	tx.DailyMaxItems = 2
	e.svc.UpdateConfig(config.InboxConfig{Transcription: tx})
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 2 || e.fake.Calls() != 2 {
		t.Fatalf("daily cap: queued %d calls %d (duplicate listing must be deduped)", n, e.fake.Calls())
	}
	if _, ok := e.store.Get("plaud", "c"); ok {
		t.Fatal("the sweep must leave capped items untouched, not record a skip")
	}
	// Monthly minutes: 2 of 3 used and each item needs 1, so one more fits.
	tx.DailyMaxItems = 0
	tx.MonthlyBudgetMinutes = 3
	e.svc.UpdateConfig(config.InboxConfig{Transcription: tx})
	e.now = e.now.Add(24 * time.Hour)
	e.src.items = append(e.src.items, Item{Source: "plaud", ID: "d", Pending: true, DurationMS: 60000})
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 1 {
		t.Fatalf("monthly budget: queued %d", n)
	}
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 {
		t.Fatalf("over budget: queued %d", n)
	}
}

func TestTranscribeSweepBackoffAndMaxAttempts(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	ctx := context.Background()
	e := newTxEnv(t, autoTx("plaud"), srv.URL)
	boom := errors.New("bad audio")
	e.fake.Errs = []error{boom, boom, boom, boom}
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 1 {
		t.Fatalf("first run queued %d", n)
	}
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 {
		t.Fatal("a failed item waits for its backoff")
	}
	e.now = e.now.Add(autoRetryBase)
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 1 {
		t.Fatal("retry after one hour")
	}
	e.now = e.now.Add(autoRetryBase)
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 {
		t.Fatal("second retry waits two hours")
	}
	e.now = e.now.Add(autoRetryBase)
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 1 {
		t.Fatal("second retry after two hours")
	}
	e.now = e.now.Add(24 * time.Hour)
	if n, _ := e.svc.TranscribeSweep(ctx, e.opts()); n != 0 {
		t.Fatalf("max %d attempts", TranscriptionMaxAttempts)
	}
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Attempts != TranscriptionMaxAttempts || rec.Status != TranscriptionFailed {
		t.Fatalf("record = %+v", rec)
	}
	// The owner can still retry by hand.
	e.fake.Errs = nil
	if _, err := e.svc.Transcribe(ctx, e.item(t), e.opts()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionDone {
		t.Fatalf("manual retry = %+v", rec)
	}
}

func TestAutoDue(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		st   *TranscriptionState
		want bool
	}{
		{"never ran", nil, true},
		{"empty", &TranscriptionState{}, true},
		{"done", &TranscriptionState{Status: TranscriptionDone, Attempts: 1}, false},
		{"running", &TranscriptionState{Status: TranscriptionRunning, Attempts: 1}, false},
		{"budget same day", &TranscriptionState{Status: TranscriptionSkipped, Reason: SkipBudget + ": x", Attempts: 1, UpdatedAt: now.Add(-time.Hour)}, false},
		{"budget next day", &TranscriptionState{Status: TranscriptionSkipped, Reason: SkipBudget + ": x", Attempts: 1, UpdatedAt: now.Add(-24 * time.Hour)}, true},
		{"too long", &TranscriptionState{Status: TranscriptionSkipped, Reason: SkipTooLong + ": x", Attempts: 1, UpdatedAt: now.Add(-48 * time.Hour)}, false},
		{"failed early", &TranscriptionState{Status: TranscriptionFailed, Attempts: 1, UpdatedAt: now.Add(-time.Minute)}, false},
		{"failed due", &TranscriptionState{Status: TranscriptionFailed, Attempts: 1, UpdatedAt: now.Add(-time.Hour)}, true},
		{"attempts used", &TranscriptionState{Status: TranscriptionFailed, Attempts: TranscriptionMaxAttempts, UpdatedAt: now.Add(-48 * time.Hour)}, false},
	}
	for _, tc := range cases {
		if got := autoDue(tc.st, now); got != tc.want {
			t.Errorf("%s: autoDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTranscriptionEgressHosts(t *testing.T) {
	if got := TranscriptionEgressHosts(config.InboxTranscription{}, "deepgram"); len(got) != 0 || got == nil {
		t.Fatalf("disabled egress = %#v", got)
	}
	if got := TranscriptionEgressHosts(activeTx(), "deepgram"); len(got) != 1 || got[0] != transcribe.DeepgramHost {
		t.Fatalf("enabled egress = %v", got)
	}
	e := newTxEnv(t, activeTx(), "http://audio.invalid")
	if st := e.svc.TranscriptionSettings(); len(st.EgressHosts) != 1 || st.EgressHosts[0] != "api.deepgram.com" {
		t.Fatalf("settings egress = %v", st.EgressHosts)
	}
}

func TestTranscribeAuditDetail(t *testing.T) {
	got := TranscribeAudit{Source: "plaud", ItemID: "r", Provider: "deepgram", Model: "nova-3", AudioSeconds: 61, Outcome: "done"}.Detail()
	want := "source=plaud, item=r, provider=deepgram, model=nova-3, audio_seconds=61, estimated_usd=0.0000, outcome=done"
	if got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
}

package inbox

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestRefFromResultPerDestination(t *testing.T) {
	if r := refFromResult(Item{}, DestSpekRFC, Result{URL: "https://github.com/o/r/pull/77"}); r == nil || r.Number != 77 {
		t.Fatalf("rfc ref = %+v", r)
	}
	if r := refFromResult(Item{}, DestSpekRFC, Result{}); r != nil {
		t.Fatalf("rfc ref without URL = %+v", r)
	}
	if r := refFromResult(Item{}, DestDibs, Result{}); r != nil {
		t.Fatalf("dibs ref without URL = %+v", r)
	}
	if r := refFromResult(Item{FactSlug: "plaud-x", Layer: "personal"}, DestDismiss, Result{}); r == nil || r.ID != "plaud-x" || r.Name != "personal" {
		t.Fatalf("knowledge ref = %+v", r)
	}
	if r := refFromResult(Item{}, DestDismiss, Result{}); r != nil {
		t.Fatalf("knowledge ref without fact = %+v", r)
	}
	if r := refFromResult(Item{}, "other", Result{URL: "x"}); r != nil {
		t.Fatalf("unknown destination ref = %+v", r)
	}
	if shareDestFor(DestSpekRFC) != ShareDestRFC || shareDestFor(DestDismiss) != ShareDestKnowledge || shareDestFor(DestDibs) != ShareDestDibs {
		t.Fatal("shareDestFor mapping")
	}
}

func TestClipReasonAndHistoryCap(t *testing.T) {
	long := strings.Repeat("é", maxShareReasonRunes+50)
	if got := clipReason("  " + long + "  "); len([]rune(got)) != maxShareReasonRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipReason = %d runes", len([]rune(got)))
	}
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	for i := 0; i < maxShareRecords+5; i++ {
		if _, err := e.svc.RecordShare("plaud/rec-1", ShareRecord{Destination: ShareDestDibs, Actor: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.svc.Store.History("plaud/rec-1")); n != maxShareRecords {
		t.Fatalf("history length = %d, want the cap %d", n, maxShareRecords)
	}
	if h := e.svc.Store.History("nope/none"); h != nil {
		t.Fatalf("unknown key history = %+v", h)
	}
}

func TestShareHistoryHelpersIgnoreUnknownRecords(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	if err := e.svc.FinishShare("nope/none", "sh-0", ShareDone, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FinishShare("plaud/rec-1", "missing", ShareDone, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.FinishLatestShare("nope/none", ShareDestRFC, func(*ShareRecord) {}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.FailIntent("nope/none", DestSpekRFC, "x"); err != nil {
		t.Fatal(err)
	}
	if keys := e.svc.Store.PendingRFCKeys(); len(keys) != 0 {
		t.Fatalf("pending = %v", keys)
	}
	// Clearing the knowledge mark records nothing; setting it does.
	if err := e.svc.DismissBy("andy", "plaud", "rec-1", false); err != nil {
		t.Fatal(err)
	}
	if len(e.svc.Store.History("plaud/rec-1")) != 0 {
		t.Fatal("undo must not record a share")
	}
}

func TestRecordRFCPublishedRejectsUnparsableOutput(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	if err := e.svc.RecordRFCPublished("plaud", "rec-1", "no links here"); err == nil {
		t.Fatal("expected an error for output without an RFC URL")
	}
}

// With no finder installed, or nothing in progress, the check is a no-op.
func TestCheckRFCPublicationsNoFinder(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	if f, x := e.svc.CheckRFCPublications(context.Background()); f != 0 || x != 0 {
		t.Fatalf("no finder: %d %d", f, x)
	}
	e.svc.SetRFCFinder(func(context.Context, string, string) (string, error) { return "", nil })
	if f, x := e.svc.CheckRFCPublications(context.Background()); f != 0 || x != 0 {
		t.Fatalf("nothing pending: %d %d", f, x)
	}
	if e.svc.rfcPublishWait() != DefaultRFCPublishWait {
		t.Fatal("default wait")
	}
}

// A finished transcript found on resume runs the waiting share.
func TestResumeIntentsRunsShareForFinishedTranscript(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	e.svc.Getenv = withKeyEnv
	var deferred []func()
	e.svc.Run = func(fn func()) { deferred = append(deferred, fn) }
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts()); err != nil {
		t.Fatal(err)
	}
	// The transcript completes but the process dies before finishIntents.
	e.svc.Run = func(fn func()) { fn() }
	rec, _ := e.store.Get("plaud", "rec-1")
	rec.Status, rec.Text = TranscriptionDone, "hello hive"
	if err := e.store.Put(rec); err != nil {
		t.Fatal(err)
	}
	// No Wait: the queued transcription job above never runs in this test.
	e.svc.ResumeIntents(context.Background())
	if dest.count() != 1 {
		t.Fatalf("resume did not run the share (delivered %d)", dest.count())
	}
	if h := history(t, e); len(h) != 1 || h[0].Status != ShareDone {
		t.Fatalf("history = %+v", h)
	}
}

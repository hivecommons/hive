package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// delegDest hands the RFC to an agent (a Note, no URL yet).
type delegDest struct{}

func (delegDest) Name() string { return DestSpekRFC }
func (delegDest) Deliver(context.Context, Item, Content, Request) (Result, error) {
	return Result{Note: "handed to agent builder to draft with spek and publish to o/r"}, nil
}

type failDest struct{ name string }

func (d failDest) Name() string { return d.name }
func (failDest) Deliver(context.Context, Item, Content, Request) (Result, error) {
	return Result{}, errors.New("dibs down")
}

func history(t *testing.T, e *txEnv) []*ShareRecord {
	t.Helper()
	return e.svc.Store.History("plaud/rec-1")
}

// A delegated RFC stays in progress until the publish output names the RFC;
// then the record is done and carries the issue URL and number.
func TestShareHistoryRFCTargetFilledAfterPublish(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, delegDest{})
	it := e.item(t)
	it.Pending = false
	if _, err := e.svc.Route(context.Background(), it, Request{Destination: DestSpekRFC, Repo: "o/r", Actor: "andy"}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	h := history(t, e)
	if len(h) != 1 || h[0].Destination != ShareDestRFC || h[0].Actor != "andy" || h[0].Status != ShareInProgress || h[0].Ref != nil || h[0].FinishedAt != nil {
		t.Fatalf("after delegate: %+v", h)
	}
	if err := e.svc.RecordRFCPublished("plaud", "rec-1", "RFC https://github.com/o/r/issues/18412 child https://github.com/o/r/issues/18413"); err != nil {
		t.Fatal(err)
	}
	h = history(t, e)
	if h[0].Status != ShareDone || h[0].Ref == nil || h[0].Ref.Number != 18412 || h[0].Ref.URL != "https://github.com/o/r/issues/18412" || h[0].FinishedAt == nil {
		t.Fatalf("after publish: %+v %+v", h[0], h[0].Ref)
	}
	if rt, _ := e.svc.Store.Route("plaud/rec-1", DestSpekRFC); rt.Status != StatusFiled || rt.URL == "" {
		t.Fatalf("route = %+v", rt)
	}
}

// DIBS, failures, Knowledge-only and auto-routing each append a record with
// the right actor, status and target. History is append-only: a retry adds.
func TestShareHistoryPaths(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dibs := &contentDest{}
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, dibs)
	ctx := context.Background()
	it := e.item(t)
	it.Pending = false
	if _, err := e.svc.Route(ctx, it, Request{Destination: DestDibs, Actor: "rw@example.com"}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if err := e.svc.DismissBy("andy", "plaud", "rec-1", true); err != nil {
		t.Fatal(err)
	}
	h := history(t, e)
	if len(h) != 2 || h[0].Destination != ShareDestDibs || h[0].Status != ShareDone || h[0].Ref == nil || h[0].Ref.ID != "1" || h[0].Ref.URL != "https://dibs.example/i/1" || h[0].Actor != "rw@example.com" {
		t.Fatalf("dibs record: %+v", h)
	}
	if h[1].Destination != ShareDestKnowledge || h[1].Actor != "andy" || h[1].Status != ShareDone || h[1].FinishedAt == nil {
		t.Fatalf("knowledge record: %+v", h[1])
	}
	// Failure keeps the reason and appends (never overwrites) on retry.
	f := newTxEnv(t, config.InboxTranscription{}, srv.URL, failDest{name: DestDibs})
	fit := f.item(t)
	fit.Pending = false
	for i := 0; i < 2; i++ {
		if _, err := f.svc.Route(ctx, fit, Request{Destination: DestDibs, Actor: "andy"}); err != nil {
			t.Fatal(err)
		}
		f.svc.Wait()
	}
	fh := f.svc.Store.History("plaud/rec-1")
	if len(fh) != 2 || fh[0].Status != ShareFailed || fh[0].Reason != "dibs down" || fh[1].Status != ShareFailed {
		t.Fatalf("failed records: %+v", fh)
	}
}

// Auto-routing rules record the inbox-auto actor.
func TestShareHistoryAutoRouteActor(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, &contentDest{})
	e.svc.UpdateConfig(config.InboxConfig{Rules: []config.InboxRule{{TitlePrefix: "Standup", Destination: DestDibs}}})
	if _, err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	h := history(t, e)
	if len(h) != 1 || h[0].Actor != AutoRouteActor || h[0].Destination != ShareDestDibs {
		t.Fatalf("auto record: %+v", h)
	}
}

// A transcribe-first share is recorded as transcribing, then done; nothing
// of the item text lands in the persisted history.
func TestShareHistoryTranscribeThenShare(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	dest := &rfcDest{}
	e := newTxEnv(t, activeTx(), srv.URL, dest)
	e.svc.Getenv = withKeyEnv
	var deferred []func()
	e.svc.Run = func(fn func()) { deferred = append(deferred, fn) }
	if _, err := e.svc.ShareWhenReady(context.Background(), e.item(t), intentFor(DestSpekRFC), e.opts()); err != nil {
		t.Fatal(err)
	}
	if h := history(t, e); len(h) != 1 || h[0].Status != ShareTranscribing || h[0].Actor != "rw@example.com" {
		t.Fatalf("while transcribing: %+v", h)
	}
	e.svc.Run = func(fn func()) { fn() }
	for len(deferred) > 0 {
		fn := deferred[0]
		deferred = deferred[1:]
		fn()
	}
	e.svc.Wait()
	h := history(t, e)
	if len(h) != 1 || h[0].Status != ShareDone || h[0].Ref == nil || h[0].Ref.Number != 9 {
		t.Fatalf("after transcription: %+v %+v", h, h[0].Ref)
	}
	raw, _ := os.ReadFile(e.svc.Store.Path)
	var doc map[string]struct {
		History []json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	hist, _ := json.Marshal(doc["plaud/rec-1"].History)
	for _, text := range []string{"Standup", "hello hive", "not transcribed", "secret-signature"} {
		if strings.Contains(string(hist), text) {
			t.Fatalf("history stores item text or a secret (%q): %s", text, hist)
		}
	}
}

// A failed transcription marks the record failed with the reason.
func TestShareHistoryTranscriptionFailure(t *testing.T) {
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
	h := history(t, e)
	if len(h) != 1 || h[0].Status != ShareFailed || h[0].Reason == "" || dest.count() != 0 {
		t.Fatalf("records: %+v", h)
	}
}

// RecordShare/FinishShare are the helper for other destinations (spek).
func TestRecordShareHelper(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL)
	id, err := e.svc.RecordShare("plaud/rec-1", ShareRecord{Destination: ShareDestSpek, Actor: "rw"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FinishShare("plaud/rec-1", id, ShareDone, "", &ShareRef{ID: "sess-1", Name: "nate-v2"}); err != nil {
		t.Fatal(err)
	}
	h := history(t, e)
	if len(h) != 1 || h[0].Status != ShareDone || h[0].Ref.Name != "nate-v2" || h[0].StartedAt.IsZero() || h[0].FinishedAt == nil {
		t.Fatalf("records: %+v", h)
	}
	if it := e.item(t); len(it.ShareHistory) != 1 {
		t.Fatalf("item history = %+v", it.ShareHistory)
	}
}

func delegatedRFC(t *testing.T) (*txEnv, string) {
	t.Helper()
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, config.InboxTranscription{}, srv.URL, delegDest{})
	it := e.item(t)
	it.Pending = false
	if _, err := e.svc.Route(context.Background(), it, Request{Destination: DestSpekRFC, Repo: "o/r", Actor: "andy"}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	return e, "plaud://recordings/rec-1"
}

// The poll finds the published RFC by the item's provenance reference and
// completes the share with its URL and number.
func TestCheckRFCPublicationsFindsPublishedRFC(t *testing.T) {
	e, marker := delegatedRFC(t)
	calls := 0
	e.svc.SetRFCFinder(func(_ context.Context, repo, m string) (string, error) {
		calls++
		if repo != "o/r" || m != marker {
			t.Errorf("finder called with %q %q", repo, m)
		}
		if calls == 1 {
			return "", nil // not published yet
		}
		return "https://github.com/o/r/issues/18412", nil
	})
	if found, expired := e.svc.CheckRFCPublications(context.Background()); found != 0 || expired != 0 {
		t.Fatalf("first check = %d %d", found, expired)
	}
	if h := history(t, e); h[0].Status != ShareInProgress {
		t.Fatalf("still in progress expected: %+v", h[0])
	}
	if found, _ := e.svc.CheckRFCPublications(context.Background()); found != 1 {
		t.Fatalf("second check found %d", found)
	}
	h := history(t, e)
	if h[0].Status != ShareDone || h[0].Ref == nil || h[0].Ref.Number != 18412 || h[0].Ref.URL != "https://github.com/o/r/issues/18412" {
		t.Fatalf("record = %+v %+v", h[0], h[0].Ref)
	}
	if rt, _ := e.svc.Store.Route("plaud/rec-1", DestSpekRFC); rt.Status != StatusFiled || rt.URL == "" {
		t.Fatalf("route = %+v", rt)
	}
	if found, _ := e.svc.CheckRFCPublications(context.Background()); found != 0 || calls != 2 {
		t.Fatalf("a finished share must not be polled again (calls=%d)", calls)
	}
}

// Nothing appears within the wait: the record and the route fail with the
// reason; before that the share stays in progress.
func TestCheckRFCPublicationsTimesOut(t *testing.T) {
	e, _ := delegatedRFC(t)
	e.svc.RFCPublishWait = 2 * time.Hour
	e.svc.SetRFCFinder(func(context.Context, string, string) (string, error) { return "", nil })
	e.now = e.now.Add(time.Hour)
	if _, expired := e.svc.CheckRFCPublications(context.Background()); expired != 0 {
		t.Fatal("expired too early")
	}
	e.now = e.now.Add(90 * time.Minute)
	if _, expired := e.svc.CheckRFCPublications(context.Background()); expired != 1 {
		t.Fatal("expected one expired share")
	}
	h := history(t, e)
	if h[0].Status != ShareFailed || !strings.Contains(h[0].Reason, "o/r") || h[0].FinishedAt == nil {
		t.Fatalf("record = %+v", h[0])
	}
	if rt, _ := e.svc.Store.Route("plaud/rec-1", DestSpekRFC); rt.Status != StatusFailed || rt.Error == "" {
		t.Fatalf("route = %+v", rt)
	}
}

// A finder error is not a verdict: the share stays in progress.
func TestCheckRFCPublicationsFinderErrorKeepsWaiting(t *testing.T) {
	e, _ := delegatedRFC(t)
	e.svc.SetRFCFinder(func(context.Context, string, string) (string, error) { return "", errors.New("rate limited") })
	e.svc.CheckRFCPublications(context.Background())
	if h := history(t, e); h[0].Status != ShareInProgress {
		t.Fatalf("record = %+v", h[0])
	}
}

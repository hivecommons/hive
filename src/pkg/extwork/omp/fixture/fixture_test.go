package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	workflowDir = "../testdata/omp-fixture"
	settle      = 5 * time.Second
	// negativeWait bounds a check that something does NOT happen.
	negativeWait = 50 * time.Millisecond
)

func hub(t *testing.T) (*omp.Broker, *omp.Listener) {
	t.Helper()
	b := omp.NewBroker()
	l, err := omp.Listen("", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return b, l
}

func peerFor(t *testing.T, b *omp.Broker, identity string) *omp.Peer {
	t.Helper()
	return testutil.EventuallyValue(t, settle, func() (*omp.Peer, bool) {
		return b.Peer(identity)
	}, "peer %s never attached", identity)
}

func bundle(t *testing.T, workKey, hint string) []byte {
	t.Helper()
	adm := omp.BundleAdmission{WorkKey: workKey, AssignmentID: "task-" + workKey, Generation: 3, Stage: "implement", ContractRevision: "c1", InputRevision: "7d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60"}
	p := map[string]any{"admission": adm, "summary": "fixture bundle", "repo": "hivecommons/omp-fixture"}
	if hint != "" {
		p["hints"] = map[string]string{HintResultClass: hint}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func waitState(t *testing.T, p *omp.Peer, key extwork.ExecutionKey, want extwork.State) extwork.Observation {
	t.Helper()
	return testutil.EventuallyValue(t, settle, func() (extwork.Observation, bool) {
		obs, _ := p.Observe(key)
		return obs, obs.State == want
	}, "state %s not reached for %s", want, key)
}

// waitPending blocks until the workbench has parked at least one offer.
func waitPending(t *testing.T, w *Workbench) []string {
	t.Helper()
	return testutil.EventuallyValue(t, settle, func() ([]string, bool) {
		keys := w.Pending()
		return keys, len(keys) > 0
	}, "no offer was parked")
}

func TestLoadWorkflowErrors(t *testing.T) {
	if _, err := LoadWorkflow(t.TempDir()); err == nil {
		t.Fatal("missing workflow accepted")
	}
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, WorkflowFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("{")
	if _, err := LoadWorkflow(dir); err == nil {
		t.Fatal("unparseable workflow accepted")
	}
	write(`{"engine":"flue","version":"1","input_revision":"x","stages":[{"name":"a"}]}`)
	if _, err := LoadWorkflow(dir); err == nil {
		t.Fatal("wrong engine accepted")
	}
	write(`{"engine":"omp","version":"","input_revision":"x","stages":[{"name":"a"}]}`)
	if _, err := LoadWorkflow(dir); err == nil {
		t.Fatal("empty version accepted")
	}
	write(`{"engine":"omp","version":"1","input_revision":"x","stages":[{"name":"a","artifacts":[{"path":"r","source":"nope"}]}]}`)
	if _, err := LoadWorkflow(dir); err == nil || !strings.Contains(err.Error(), "missing artifact source") {
		t.Fatalf("missing source accepted: %v", err)
	}
	wf, err := LoadWorkflow(workflowDir)
	if err != nil || len(wf.Stages) != 2 || wf.Engine != EngineName {
		t.Fatalf("real workflow = %+v %v", wf, err)
	}
}

func TestStartErrors(t *testing.T) {
	if _, err := Start(Options{}); err == nil {
		t.Fatal("missing options accepted")
	}
	if _, err := Start(Options{HubURL: "ws://127.0.0.1:1", Identity: "x", WorkflowDir: t.TempDir()}); err == nil {
		t.Fatal("bad workflow dir accepted")
	}
	if _, err := Start(Options{HubURL: "ws://127.0.0.1:1", Identity: "x", WorkflowDir: workflowDir}); err == nil || !strings.Contains(err.Error(), "dial hub") {
		t.Fatalf("unreachable hub = %v", err)
	}
	// A hub that is not the relay listener answers hello with something
	// other than hello_ok (here: nothing, the connection closes).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := Start(Options{HubURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Identity: "x", WorkflowDir: workflowDir}); err == nil {
		t.Fatal("non-relay hub accepted")
	}
	_, l := hub(t)
	if _, err := Start(Options{HubURL: l.URL(), Identity: "x", WorkflowDir: workflowDir, Capabilities: []string{"run-stage"}}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("refused hello = %v", err)
	}
	if _, err := Start(Options{HubURL: l.URL(), Identity: "x", WorkflowDir: workflowDir, ControlListen: "300.0.0.1:1"}); err == nil {
		t.Fatal("bad control listen accepted")
	}
}

func TestWorkbenchLifecycle(t *testing.T) {
	b, l := hub(t)
	w, err := Start(Options{HubURL: l.URL(), Identity: "wb", WorkflowDir: workflowDir, Incarnation: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Incarnation() != "s1" || !strings.HasPrefix(w.ControlAddr(), "http://127.0.0.1:") {
		t.Fatalf("workbench = %s %s", w.Incarnation(), w.ControlAddr())
	}
	p := peerFor(t, b, "wb")
	ctx := context.Background()
	payload := bundle(t, "w1", "")
	key := extwork.ExecutionKey("key-1")

	// Auto policy: a summary with the marker declines, otherwise accepts.
	if d, reason, err := p.Offer(ctx, extwork.Offer{ExecutionKey: "declined", Summary: "x " + DefaultDeclineMarker}, 1); d != extwork.OfferDeclined || err != nil || !strings.Contains(reason, "policy") {
		t.Fatalf("declined offer = %s %q %v", d, reason, err)
	}
	if d, _, err := p.Offer(ctx, extwork.Offer{ExecutionKey: key, Summary: "ok"}, 1); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("accepted offer = %s %v", d, err)
	}
	st := w.Stats()
	if st.Offers != 2 || st.Accepts != 1 || st.Declines != 1 || !st.Connected {
		t.Fatalf("stats = %+v", st)
	}
	// Start, dedup, conflict.
	res, err := p.Start(ctx, key, 1, "implement", payload)
	if err != nil || res.RemoteRunID == "" || res.Deduplicated {
		t.Fatalf("Start = %+v %v", res, err)
	}
	res2, err := p.Start(ctx, key, 1, "implement", payload)
	if err != nil || !res2.Deduplicated || res2.RemoteRunID != res.RemoteRunID {
		t.Fatalf("dedup Start = %+v %v", res2, err)
	}
	if _, err := p.Start(ctx, key, 1, "implement", []byte("other")); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	if st := w.Stats(); st.Runs != 1 || st.Starts != 2 {
		t.Fatalf("stats after starts = %+v", st)
	}
	waitState(t, p, key, extwork.StateAccepted)
	// Bad bundle is refused by the workbench.
	if d, _, _ := p.Offer(ctx, extwork.Offer{ExecutionKey: "bad", Summary: "ok"}, 1); d != extwork.OfferAccepted {
		t.Fatal("bad offer not accepted")
	}
	if _, err := p.Start(ctx, "bad", 1, "implement", []byte("{not json")); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("bad bundle = %v", err)
	}
	// Ticks: review, report, then receipt + terminal; waiting in between.
	w.Tick()
	if obs := waitState(t, p, key, extwork.StateRunning); obs.Stage != "review" {
		t.Fatalf("stage = %+v", obs)
	}
	if !w.SetWaiting(res.RemoteRunID, true) || w.SetWaiting("nope", true) {
		t.Fatal("SetWaiting")
	}
	waitState(t, p, key, extwork.StateWaiting)
	w.Tick() // a waiting run does not advance
	if !w.SetWaiting(res.RemoteRunID, false) {
		t.Fatal("resume")
	}
	waitState(t, p, key, extwork.StateRunning)
	w.Tick()
	w.Tick()
	obs := waitState(t, p, key, extwork.StateTerminal)
	if obs.Receipt == nil || obs.Receipt.Path != omp.ReceiptArtifact || obs.Receipt.Size == 0 {
		t.Fatalf("terminal = %+v", obs)
	}
	rc, err := p.OpenArtifact(key, omp.ReceiptArtifact)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	receipt, err := extwork.ParseReceipt(raw)
	if err != nil || receipt.WorkKey != "w1" || receipt.Generation != 3 || receipt.RemoteIncarnation != "s1" || len(receipt.Artifacts) != 1 {
		t.Fatalf("receipt = %+v %v", receipt, err)
	}
	if w.SetWaiting(res.RemoteRunID, true) {
		t.Fatal("a terminal run accepted waiting")
	}
	// Cancel after terminal: acknowledged, not stopped.
	facts, err := p.Cancel(ctx, key)
	if err != nil || !facts.Acknowledged || facts.Stopped || facts.Detail != "already terminal" {
		t.Fatalf("cancel terminal = %+v %v", facts, err)
	}
	// Result-class hints steer the receipt.
	for _, hint := range []string{"no_change", "blocked", "failed"} {
		hkey := extwork.ExecutionKey("key-" + hint)
		if d, _, _ := p.Offer(ctx, extwork.Offer{ExecutionKey: hkey, Summary: "ok"}, 1); d != extwork.OfferAccepted {
			t.Fatal("hint offer not accepted")
		}
		if _, err := p.Start(ctx, hkey, 1, "implement", bundle(t, hint, hint)); err != nil {
			t.Fatal(err)
		}
		w.Tick()
		w.Tick()
		w.Tick()
		hobs := waitState(t, p, hkey, extwork.StateTerminal)
		hrc, err := p.OpenArtifact(hkey, omp.ReceiptArtifact)
		if err != nil || hobs.Receipt == nil {
			t.Fatal(err)
		}
		hraw, _ := io.ReadAll(hrc)
		_ = hrc.Close()
		hr, err := extwork.ParseReceipt(hraw)
		if err != nil || string(hr.ResultClass) != hint || len(hr.Artifacts) != 0 {
			t.Fatalf("%s receipt = %+v %v", hint, hr, err)
		}
	}
	if st := w.Stats(); st.Receipts != 4 {
		t.Fatalf("receipts = %d", st.Receipts)
	}
	// Disconnect: the peer is gone, stats say so, control still answers.
	w.Disconnect()
	<-p.Gone()
	if st := w.Stats(); st.Connected {
		t.Fatal("still connected after Disconnect")
	}
	select {
	case <-w.Done():
	case <-time.After(settle):
		t.Fatal("read loop did not end")
	}
}

func TestCancelStopsAndIgnores(t *testing.T) {
	b, l := hub(t)
	stops, err := Start(Options{HubURL: l.URL(), Identity: "stops", WorkflowDir: workflowDir})
	if err != nil {
		t.Fatal(err)
	}
	defer stops.Close()
	ignores, err := Start(Options{HubURL: l.URL(), Identity: "ignores", WorkflowDir: workflowDir, IgnoreCancel: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ignores.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		w       *Workbench
		id      string
		stopped bool
	}{{stops, "stops", true}, {ignores, "ignores", false}} {
		p := peerFor(t, b, tc.id)
		key := extwork.ExecutionKey("k")
		if _, err := p.Cancel(ctx, key); !errors.Is(err, extwork.ErrNotFound) {
			t.Fatalf("cancel unknown key = %v", err)
		}
		if d, _, _ := p.Offer(ctx, extwork.Offer{ExecutionKey: key, Summary: "ok"}, 1); d != extwork.OfferAccepted {
			t.Fatal("offer")
		}
		if _, err := p.Start(ctx, key, 1, "implement", bundle(t, tc.id, "")); err != nil {
			t.Fatal(err)
		}
		tc.w.Tick()
		waitState(t, p, key, extwork.StateRunning)
		facts, err := p.Cancel(ctx, key)
		if err != nil || !facts.Acknowledged || facts.Stopped != tc.stopped {
			t.Fatalf("%s cancel = %+v %v", tc.id, facts, err)
		}
		if tc.stopped {
			if obs := waitState(t, p, key, extwork.StateTerminal); obs.Receipt != nil {
				t.Fatalf("stopped run has a receipt: %+v", obs)
			}
		} else {
			tc.w.Tick()
			tc.w.Tick()
			if obs := waitState(t, p, key, extwork.StateTerminal); obs.Receipt == nil {
				t.Fatalf("ignoring run has no receipt: %+v", obs)
			}
		}
		if st := tc.w.Stats(); st.Cancels != 1 {
			t.Fatalf("%s cancels = %d", tc.id, st.Cancels)
		}
	}
	// A cancel for a key the workbench never ran is acknowledged as such.
	// (Reached through the hub side: the peer refuses it before sending, so
	// exercise the workbench branch directly.)
	stops.handleCancel(omp.Message{Type: omp.MsgCancel, ExecutionKey: "never"})
	if st := stops.Stats(); st.Cancels != 2 {
		t.Fatalf("cancels = %d", st.Cancels)
	}
}

func TestInteractiveAndUnsolicitedStart(t *testing.T) {
	b, l := hub(t)
	w, err := Start(Options{HubURL: l.URL(), Identity: "inter", WorkflowDir: workflowDir, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	p := peerFor(t, b, "inter")
	ctx := context.Background()
	key := extwork.ExecutionKey("parked")
	done := make(chan extwork.OfferDecision, 1)
	go func() {
		d, _, _ := p.Offer(ctx, extwork.Offer{ExecutionKey: key, Summary: "park me"}, 1)
		done <- d
	}()
	if keys := waitPending(t, w); len(keys) != 1 || keys[0] != string(key) {
		t.Fatalf("pending = %v", keys)
	}
	if w.Answer("other", true, "") {
		t.Fatal("answered a key that was not pending")
	}
	// A bundle pushed for a key the workbench has not accepted is counted
	// and refused, never executed.
	w.handleStart(omp.Message{Type: omp.MsgStart, ExecutionKey: string(key), Payload: []byte("{}")})
	if st := w.Stats(); st.UnsolicitedStarts != 1 || st.PayloadBytesBeforeAccept != 2 || st.Runs != 0 {
		t.Fatalf("unsolicited start stats = %+v", st)
	}
	if !w.Answer(string(key), false, "operator said no") {
		t.Fatal("answer not delivered")
	}
	if d := <-done; d != extwork.OfferDeclined {
		t.Fatalf("decision = %s", d)
	}
	if len(w.Pending()) != 0 {
		t.Fatal("answered offer still pending")
	}
}

func TestControlSurface(t *testing.T) {
	b, l := hub(t)
	w, err := Start(Options{HubURL: l.URL(), Identity: "ctl", WorkflowDir: workflowDir, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	p := peerFor(t, b, "ctl")
	base := w.ControlAddr()
	get := func(path string) (int, []byte) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	post := func(path string) (int, []byte) {
		resp, err := http.Post(base+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	if code, raw := get("/control/stats"); code != http.StatusOK || !bytes.Contains(raw, []byte(`"connected":true`)) {
		t.Fatalf("stats = %d %s", code, raw)
	}
	if code, _ := get("/control/tick"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET tick = %d", code)
	}
	if code, _ := get("/control/disconnect"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET disconnect = %d", code)
	}
	if code, _ := post("/control/answer/x?decision=maybe"); code != http.StatusBadRequest {
		t.Fatalf("bad decision = %d", code)
	}
	if code, _ := post("/control/answer/x?decision=accept"); code != http.StatusNotFound {
		t.Fatalf("answer without offer = %d", code)
	}
	if code, _ := post("/control/wait/nope"); code != http.StatusNotFound {
		t.Fatalf("wait unknown = %d", code)
	}
	if code, _ := get("/control/pending"); code != http.StatusOK {
		t.Fatalf("pending = %d", code)
	}
	key := extwork.ExecutionKey("via-control")
	go func() { _, _, _ = p.Offer(context.Background(), extwork.Offer{ExecutionKey: key, Summary: "s"}, 1) }()
	waitPending(t, w)
	if code, _ := post("/control/answer/" + string(key) + "?decision=accept"); code != http.StatusOK {
		t.Fatalf("answer = %d", code)
	}
	if _, err := p.Start(context.Background(), key, 1, "implement", bundle(t, "ctl", "")); err != nil {
		t.Fatal(err)
	}
	if code, _ := post("/control/tick"); code != http.StatusOK {
		t.Fatalf("tick = %d", code)
	}
	obs := waitState(t, p, key, extwork.StateRunning)
	if code, _ := post("/control/wait/" + obs.RemoteRunID); code != http.StatusOK {
		t.Fatalf("wait = %d", code)
	}
	waitState(t, p, key, extwork.StateWaiting)
	if code, _ := post("/control/resume/" + obs.RemoteRunID); code != http.StatusOK {
		t.Fatalf("resume = %d", code)
	}
	if code, _ := post("/control/disconnect"); code != http.StatusOK {
		t.Fatalf("disconnect = %d", code)
	}
	<-p.Gone()
}

func TestBuildReceiptValidates(t *testing.T) {
	wf, err := LoadWorkflow(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	adm := omp.BundleAdmission{WorkKey: "w", AssignmentID: "t", Generation: 1, Stage: "implement", ContractRevision: "c", InputRevision: wf.InputRevision}
	for _, class := range []outputschema.StageReceiptResultClass{outputschema.ReceiptResultCompleted, outputschema.ReceiptResultNoChange, outputschema.ReceiptResultBlocked, outputschema.ReceiptResultFailed} {
		raw, err := BuildReceipt(wf, adm, "repo", "key", "run", "inc", class, 0, 3)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := extwork.ParseReceipt(raw)
		if err != nil || receipt.ResultClass != class || receipt.EndedAt != "2026-09-23T00:03:00Z" {
			t.Fatalf("%s: %+v %v", class, receipt, err)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a reading test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRun(t *testing.T) {
	var out, errOut syncBuffer
	if code := Run(context.Background(), []string{"-bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("bad flag exit = %d", code)
	}
	if code := Run(context.Background(), nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "required") {
		t.Fatalf("missing flags exit = %d %s", code, errOut.String())
	}
	if code := Run(context.Background(), []string{"-hub", "ws://127.0.0.1:1", "-identity", "x", "-workflow", workflowDir}, &out, &errOut); code != 1 {
		t.Fatalf("unreachable hub exit = %d", code)
	}
	b, l := hub(t)
	// Without the capability the hub refuses and Run exits 1.
	if code := Run(context.Background(), []string{"-hub", l.URL(), "-identity", "nocap", "-workflow", workflowDir, "-no-capability"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "refused") {
		t.Fatalf("no-capability exit = %d %s", code, errOut.String())
	}
	// With it, Run prints the control address and exits cleanly on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"-hub", l.URL(), "-identity", "runner", "-workflow", workflowDir, "-incarnation", "r1", "-interactive", "-ignore-cancel", "-decline-marker", "[no]"}, &out, &errOut)
	}()
	testutil.Eventually(t, settle, func() bool { return strings.Contains(out.String(), AddrLinePrefix) }, "Run never printed its address")
	if !strings.Contains(out.String(), AddrLinePrefix+"http://127.0.0.1:") {
		t.Fatalf("no address line in %q", out.String())
	}
	p := peerFor(t, b, "runner")
	if p.Incarnation() != "r1" {
		t.Fatalf("incarnation = %s", p.Incarnation())
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, errOut.String())
		}
	case <-time.After(settle):
		t.Fatal("Run did not return after cancel")
	}
	// The hub dropping the link keeps Run alive until its context ends.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan int, 1)
	go func() {
		done2 <- Run(ctx2, []string{"-hub", l.URL(), "-identity", "dropped", "-workflow", workflowDir}, &out, &errOut)
	}()
	p2 := peerFor(t, b, "dropped")
	_ = p2.Close()
	select {
	case <-done2:
		t.Fatal("Run returned before its context ended")
	case <-time.After(negativeWait):
	}
	cancel2()
	select {
	case code := <-done2:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(settle):
		t.Fatal("Run did not return after cancel")
	}
}

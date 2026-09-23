package omp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
	"github.com/hivecommons/hive/pkg/extwork/omp/fixture"
)

// The conformance rows of #8201 that apply to an interactive host (see the
// "OMP as second host" section of src/docs/design/external-workflow-admission.md)
// are exercised here against the REAL fixture process: a second OS process
// speaking the relay-channel protocol as an OMP workbench would, attached to
// a loopback listener that stands in for the hub's contributor WebSocket.

const (
	fixtureStartTimeout = 10 * time.Second
	settleTimeout       = 5 * time.Second
	stageCount          = 2
	testAckTimeout      = 5 * time.Second
)

type hub struct {
	broker   *omp.Broker
	listener *omp.Listener
}

func startHub(t *testing.T) *hub {
	t.Helper()
	b := omp.NewBroker()
	l, err := omp.Listen("", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &hub{broker: b, listener: l}
}

func (h *hub) adapter(t *testing.T, version string) *omp.Adapter {
	t.Helper()
	if version == "" {
		version = workflowVersion
	}
	a, err := omp.New(omp.Config{Peers: h.broker, WorkflowVersion: version, OfferTimeout: testAckTimeout, AckTimeout: testAckTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// waitPeer blocks until the broker holds (or, with want false, no longer
// holds) a peer for identity.
func (h *hub) waitPeer(t *testing.T, identity string, want bool) {
	t.Helper()
	testutil.Eventually(t, settleTimeout, func() bool {
		_, ok := h.broker.Peer(identity)
		return ok == want
	}, "peer %s attached=%v not reached", identity, want)
}

type wbOpts struct {
	identity     string
	incarnation  string
	interactive  bool
	ignoreCancel bool
	noCapability bool
}

// wbProc is a running workbench child process.
type wbProc struct {
	t        *testing.T
	cmd      *exec.Cmd
	ctrl     string
	identity string
	done     chan struct{}
	stderr   *strings.Builder
}

func startWorkbench(t *testing.T, h *hub, o wbOpts) *wbProc {
	t.Helper()
	if o.identity == "" {
		o.identity = identityA
	}
	args := []string{"-test.run=^$", "--", "-hub", h.listener.URL(), "-identity", o.identity, "-workflow", workflowDir}
	if o.incarnation != "" {
		args = append(args, "-incarnation", o.incarnation)
	}
	if o.interactive {
		args = append(args, "-interactive")
	}
	if o.ignoreCancel {
		args = append(args, "-ignore-cancel")
	}
	if o.noCapability {
		args = append(args, "-no-capability")
	}
	cmd := exec.Command(os.Args[0], args...)
	// A scrubbed environment: no GITHUB_TOKEN, no dashboard token. The
	// workbench must find nothing it could use to act outside the lease.
	cmd.Env = []string{childEnv + "=1", "PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + os.Getenv("TMPDIR")}
	cmd.WaitDelay = fixtureStartTimeout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &wbProc{t: t, cmd: cmd, identity: o.identity, done: make(chan struct{}), stderr: &strings.Builder{}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	addrCh := make(chan string, 1)
	go func() {
		defer close(p.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, fixture.AddrLinePrefix) {
				addrCh <- strings.TrimPrefix(line, fixture.AddrLinePrefix)
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	t.Cleanup(p.kill)
	if o.noCapability {
		// The workbench must be refused at hello and exit non-zero.
		if err := cmd.Wait(); err == nil {
			t.Fatal("a workbench without ext-exec/omp attached")
		}
		return p
	}
	select {
	case p.ctrl = <-addrCh:
	case <-time.After(fixtureStartTimeout):
		_ = cmd.Process.Kill()
		t.Fatalf("workbench did not print its control address; stderr: %s", p.stderr.String())
	}
	h.waitPeer(t, o.identity, true)
	return p
}

func (p *wbProc) kill() {
	if p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	<-p.done
}

func (p *wbProc) control(method, path string) []byte {
	p.t.Helper()
	req, err := http.NewRequest(method, p.ctrl+path, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	return raw
}

func (p *wbProc) tick()       { p.control(http.MethodPost, "/control/tick") }
func (p *wbProc) disconnect() { p.control(http.MethodPost, "/control/disconnect") }
func (p *wbProc) answer(key extwork.ExecutionKey, decision string) {
	p.control(http.MethodPost, "/control/answer/"+string(key)+"?decision="+decision)
}

func (p *wbProc) stats() fixture.Stats {
	var st fixture.Stats
	if err := json.Unmarshal(p.control(http.MethodGet, "/control/stats"), &st); err != nil {
		p.t.Fatal(err)
	}
	return st
}

func (p *wbProc) pending() []string {
	var keys []string
	if err := json.Unmarshal(p.control(http.MethodGet, "/control/pending"), &keys); err != nil {
		p.t.Fatal(err)
	}
	return keys
}

func (p *wbProc) waitStats(cond func(fixture.Stats) bool) fixture.Stats {
	p.t.Helper()
	return testutil.EventuallyValue(p.t, settleTimeout, func() (fixture.Stats, bool) {
		st := p.stats()
		return st, cond(st)
	}, "workbench stats never satisfied the condition")
}

func newBinding(t *testing.T, a extwork.Adapter, store extwork.AdmissionStore, mode string, opts ...extwork.Option) (*extwork.Binding, *extwork.MemorySink) {
	t.Helper()
	sink := &extwork.MemorySink{}
	if store == nil {
		store = extwork.NewFileStore(t.TempDir())
	}
	return extwork.New(a, store, extwork.NewFileStore(t.TempDir()), sink, mode, opts...), sink
}

// waitObserve polls the binding until the adapter reports want, recording
// progress events on every state change along the way.
func waitObserve(t *testing.T, b *extwork.Binding, adm extwork.Admission, want extwork.State) extwork.Observation {
	t.Helper()
	return testutil.EventuallyValue(t, settleTimeout, func() (extwork.Observation, bool) {
		obs, err := b.Observe(context.Background(), adm, "")
		return obs, err == nil && obs.State == want
	}, "state %s not reached", want)
}

// runToTerminal ticks the workbench through every stage and returns the
// terminal observation, which must carry the receipt reference.
func runToTerminal(t *testing.T, p *wbProc, b *extwork.Binding, adm extwork.Admission) extwork.Observation {
	t.Helper()
	for range stageCount {
		p.tick()
		waitObserve(t, b, adm, extwork.StateRunning)
	}
	p.tick()
	obs := waitObserve(t, b, adm, extwork.StateTerminal)
	if obs.Receipt == nil {
		t.Fatalf("terminal without receipt: %+v", obs)
	}
	return obs
}

func hasAction(sink *extwork.MemorySink, action string) bool {
	for _, a := range sink.Actions() {
		if a == action {
			return true
		}
	}
	return false
}

func progressStates(sink *extwork.MemorySink) []extwork.State {
	var states []extwork.State
	for _, ev := range sink.Events() {
		if ev.Action == extwork.EventProgress {
			states = append(states, ev.State)
		}
	}
	return states
}

// Accept before context: an interactive workbench parks the offer; while it
// is parked nothing beyond the summary leaves Hive, a Dispatch attempt is
// refused without a frame, and only an explicit accept lets the bundle
// through. The decision is audited before the start.
func TestConformanceAcceptBeforeContext(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{interactive: true})
	a := h.adapter(t, "")
	ctx := context.Background()
	b, sink := newBinding(t, a, nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#6899", "task-6899", 3, "implement", extwork.ModeReportOnly, nil)
	key := adm.ExecutionKey()

	type offerResult struct {
		d   extwork.OfferDecision
		err error
	}
	offered := make(chan offerResult, 1)
	go func() {
		d, err := b.Offer(ctx, a.Host(identityA, adm), adm, "summary only")
		offered <- offerResult{d, err}
	}()
	st := wb.waitStats(func(s fixture.Stats) bool { return s.Offers == 1 })
	if st.Starts != 0 || st.Accepts != 0 {
		t.Fatalf("workbench saw work before answering: %+v", st)
	}
	if keys := wb.pending(); len(keys) != 1 || keys[0] != string(key) {
		t.Fatalf("pending offers = %v", keys)
	}
	// A dispatch while the offer is parked is refused Hive-side; the
	// workbench never sees a bundle.
	if _, err := b.Dispatch(ctx, adm, payload); !errors.Is(err, omp.ErrNotAccepted) {
		t.Fatalf("dispatch before accept = %v", err)
	}
	if st := wb.stats(); st.Starts != 0 || st.UnsolicitedStarts != 0 || st.PayloadBytesBeforeAccept != 0 {
		t.Fatalf("context reached the workbench before acceptance: %+v", st)
	}
	wb.answer(key, "accept")
	select {
	case r := <-offered:
		if r.d != extwork.OfferAccepted || r.err != nil {
			t.Fatalf("Offer = %s %v", r.d, r.err)
		}
	case <-time.After(settleTimeout):
		t.Fatal("offer never resolved after the workbench accepted")
	}
	res, err := b.Dispatch(ctx, adm, payload)
	if err != nil || !res.Started || res.Run.RemoteRunID == "" {
		t.Fatalf("Dispatch = %+v %v", res, err)
	}
	st = wb.waitStats(func(s fixture.Stats) bool { return s.Runs == 1 })
	if st.UnsolicitedStarts != 0 || st.GitHubTokenPresent || st.DashboardTokenPresent {
		t.Fatalf("workbench stats = %+v", st)
	}
	actions := sink.Actions()
	accepted, started := -1, -1
	for i, act := range actions {
		switch act {
		case extwork.EventOfferAccepted:
			accepted = i
		case extwork.EventStarted:
			started = i
		}
	}
	if accepted < 0 || started < 0 || accepted > started {
		t.Fatalf("audit order = %v", actions)
	}
	pinned, ok, err := b.Admission(adm.AssignmentID)
	if err != nil || !ok || pinned.EngineIncarnation != res.Run.RemoteIncarnation || pinned.RemoteRunID != res.Run.RemoteRunID {
		t.Fatalf("admission not pinned to the workbench session: %+v %v %v", pinned, ok, err)
	}
}

// Decline leaves the lease unassigned and audited: a declined offer persists
// nothing, starts nothing, and is recorded with the workbench's reason.
// Unrelated ready work on the same workbench still runs.
func TestConformanceDeclineLeavesLeaseUnassigned(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{})
	a := h.adapter(t, "")
	ctx := context.Background()
	store := extwork.NewFileStore(t.TempDir())
	b, sink := newBinding(t, a, store, extwork.ModeReportOnly)

	held, _ := admissionFor(t, "github:hivecommons/hive#201", "task-201", 1, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, held), held, "held "+fixture.DefaultDeclineMarker); d != extwork.OfferDeclined || !errors.Is(err, extwork.ErrDeclined) {
		t.Fatalf("held Offer = %s %v", d, err)
	}
	if _, ok, _ := store.Load(held.AssignmentID); ok {
		t.Fatal("a declined offer persisted an admission")
	}
	st := wb.waitStats(func(s fixture.Stats) bool { return s.Declines == 1 })
	if st.Starts != 0 || st.Accepts != 0 {
		t.Fatalf("declined offer reached execution: %+v", st)
	}
	var declined *extwork.ProgressEvent
	for _, ev := range sink.Events() {
		if ev.Action == extwork.EventOfferDeclined {
			ev := ev
			declined = &ev
		}
	}
	if declined == nil || declined.AssignmentID != held.AssignmentID {
		t.Fatalf("decline not audited: %+v", declined)
	}
	if reason, ok := declined.Fields["reason"].(string); !ok || !strings.Contains(reason, "workbench policy") {
		t.Fatalf("decline audited without the workbench reason: %+v", declined.Fields)
	}
	ready, readyPayload := admissionFor(t, "github:hivecommons/hive#202", "task-202", 1, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, ready), ready, "ready summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("ready Offer = %s %v", d, err)
	}
	if _, err := b.Dispatch(ctx, ready, readyPayload); err != nil {
		t.Fatal(err)
	}
	if st := wb.waitStats(func(s fixture.Stats) bool { return s.Runs == 1 }); st.Offers != 2 {
		t.Fatalf("workbench stats = %+v", st)
	}
}

// Progress events recorded: every state the workbench reports (accepted,
// running per stage, waiting, terminal) lands on the audit as a progress
// event carrying the stage, and the terminal receipt is bound to the lease
// and generation. Replay rehydrates it without a second run.
func TestConformanceProgressEventsAndReceiptBinding(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{incarnation: "session-1"})
	a := h.adapter(t, "")
	ctx := context.Background()
	b, sink := newBinding(t, a, nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#301", "task-301", 4, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, adm), adm, "summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	res, err := b.Dispatch(ctx, adm, payload)
	if err != nil || res.Run.RemoteIncarnation != "session-1" {
		t.Fatalf("Dispatch = %+v %v", res, err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	waitObserve(t, b, pinned, extwork.StateAccepted)
	wb.tick()
	obs := waitObserve(t, b, pinned, extwork.StateRunning)
	if obs.Stage != "review" {
		t.Fatalf("first stage = %+v", obs)
	}
	wb.control(http.MethodPost, "/control/wait/"+res.Run.RemoteRunID)
	waitObserve(t, b, pinned, extwork.StateWaiting)
	wb.control(http.MethodPost, "/control/resume/"+res.Run.RemoteRunID)
	waitObserve(t, b, pinned, extwork.StateRunning)
	wb.tick()
	// running(review) -> running(report) is not a state change, so the
	// audit stays quiet; the stage is still visible on Observe.
	obs = testutil.EventuallyValue(t, settleTimeout, func() (extwork.Observation, bool) {
		o, _ := b.Observe(ctx, pinned, "")
		return o, o.Stage == "report"
	}, "second stage not observed")
	if obs.State != extwork.StateRunning {
		t.Fatalf("second stage = %+v", obs)
	}
	wb.tick()
	obs = waitObserve(t, b, pinned, extwork.StateTerminal)
	if obs.Receipt == nil || obs.Receipt.Path != omp.ReceiptArtifact {
		t.Fatalf("terminal = %+v", obs)
	}
	want := []extwork.State{extwork.StateAccepted, extwork.StateRunning, extwork.StateWaiting, extwork.StateRunning, extwork.StateTerminal}
	got := progressStates(sink)
	if len(got) != len(want) {
		t.Fatalf("progress states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("progress states = %v, want %v", got, want)
		}
	}
	for _, ev := range sink.Events() {
		if ev.Action != extwork.EventProgress {
			continue
		}
		stage, _ := ev.Fields["stage"].(string)
		if stage == "" {
			t.Fatalf("progress event without a stage: %+v", ev)
		}
		if (ev.State == extwork.StateRunning || ev.State == extwork.StateWaiting) && stage != "review" && stage != "report" {
			t.Fatalf("running/waiting event carries stage %q: %+v", stage, ev)
		}
	}

	// Receipt bound to lease and generation.
	receipt, err := b.FetchReceipt(ctx, pinned, "", *obs.Receipt)
	if err != nil {
		t.Fatalf("FetchReceipt: %v", err)
	}
	if receipt.Generation != adm.Generation || receipt.AssignmentID != adm.AssignmentID || receipt.WorkKey != adm.WorkKey || receipt.RemoteRunID != res.Run.RemoteRunID || receipt.RemoteIncarnation != "session-1" || len(receipt.Artifacts) != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	otherGen := pinned
	otherGen.Generation++
	if err := extwork.BindReceipt(otherGen, receipt); !errors.Is(err, extwork.ErrReceiptUnbound) {
		t.Fatalf("receipt bound to another generation: %v", err)
	}
	otherTask := pinned
	otherTask.AssignmentID = "task-999"
	if err := extwork.BindReceipt(otherTask, receipt); !errors.Is(err, extwork.ErrReceiptUnbound) {
		t.Fatalf("receipt bound to another lease: %v", err)
	}
	if d := b.Decide(pinned, receipt, nil, true); d.Verdict != extwork.VerdictAccepted {
		t.Fatalf("Decide = %+v", d)
	}
	starts := wb.stats().Starts
	replayed, ok, err := b.Replay(pinned)
	if err != nil || !ok || replayed.OutputDigest != receipt.OutputDigest {
		t.Fatalf("Replay = %+v %v %v", replayed, ok, err)
	}
	if wb.stats().Starts != starts {
		t.Fatal("replay reached the workbench")
	}
	// The receipt is evidence, not authority: a tampered reference is
	// refused before parsing, and nothing is stored for it.
	bad := *obs.Receipt
	bad.Digest = strings.Repeat("0", 64)
	if _, err := b.FetchReceipt(ctx, pinned, "", bad); !errors.Is(err, extwork.ErrReceiptDigest) {
		t.Fatalf("wrong digest = %v", err)
	}
	if !hasAction(sink, extwork.EventReceiptRefused) || !hasAction(sink, extwork.EventReceiptVerified) {
		t.Fatalf("receipt events = %v", sink.Actions())
	}
}

// Disconnect mid-stage: the workbench goes away while a stage runs. The run
// becomes unknown, never stopped or terminal; cancel cannot claim stopped;
// recovery is uncertain and starts nothing; and a workbench that reconnects
// under a new session is never mistaken for the old one. Reclaiming the
// lease is the hub's own expiry, not a state this adapter invents.
func TestConformanceDisconnectMidStage(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{incarnation: "session-a"})
	a := h.adapter(t, "")
	ctx := context.Background()
	store := extwork.NewFileStore(t.TempDir())
	b, sink := newBinding(t, a, store, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#501", "task-501", 1, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, adm), adm, "summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	wb.tick()
	waitObserve(t, b, pinned, extwork.StateRunning)
	startsBefore := wb.stats().Starts

	wb.disconnect()
	h.waitPeer(t, identityA, false)
	obs, err := b.Observe(ctx, pinned, "")
	if !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown {
		t.Fatalf("observe after disconnect = %+v %v", obs, err)
	}
	if states := progressStates(sink); states[len(states)-1] != extwork.StateUnknown {
		t.Fatalf("audit must end in unknown, got %v", states)
	}
	facts, err := b.Cancel(ctx, pinned, "")
	if err == nil || facts.Stopped {
		t.Fatalf("cancel after disconnect = %+v %v (stopped must never be claimed)", facts, err)
	}
	rec, err := b.Recover(ctx, adm, payload)
	if !errors.Is(err, extwork.ErrUncertain) || !rec.Uncertain || rec.Started || rec.Adopted {
		t.Fatalf("recover after disconnect = %+v %v", rec, err)
	}
	if st := wb.stats(); st.Starts != startsBefore {
		t.Fatalf("recovery reached the disconnected workbench: %+v", st)
	}
	// A new session under the same identity is a different instance: the
	// pinned incarnation mismatches, nothing is adopted, nothing starts.
	wb2 := startWorkbench(t, h, wbOpts{incarnation: "session-b"})
	if _, err := b.Observe(ctx, pinned, ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("observe on the new session = %v", err)
	}
	rec, err = b.Recover(ctx, adm, payload)
	if !errors.Is(err, extwork.ErrUncertain) || !errors.Is(err, extwork.ErrIncarnationMismatch) || rec.Started || rec.Adopted {
		t.Fatalf("recover on the new session = %+v %v", rec, err)
	}
	if st := wb2.stats(); st.Starts != 0 || st.Offers != 0 {
		t.Fatalf("new session received work it never accepted: %+v", st)
	}
	// Positive control: a fresh generation is a new execution key and can
	// be offered to the new session normally.
	retry, retryPayload := admissionFor(t, "github:hivecommons/hive#501", "task-501", 2, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, retry), retry, "retry"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("retry Offer = %s %v", d, err)
	}
	if res, err := b.Dispatch(ctx, retry, retryPayload); err != nil || res.Run.RemoteIncarnation != "session-b" {
		t.Fatalf("retry Dispatch = %+v %v", res, err)
	}
}

// Write-capable stage refused: because OMP is unconfined (T3), a stage that
// could write is refused before any frame reaches the workbench, at the
// offer and at dispatch, while a report-only stage on the same workbench is
// admitted (positive control).
func TestConformanceWriteCapableStageRefused(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{})
	a := h.adapter(t, "")
	ctx := context.Background()
	b, sink := newBinding(t, a, nil, extwork.ModeReportOnly)
	for stage := range omp.WriteCapableStages {
		adm, payload := admissionFor(t, "github:hivecommons/hive#7", "task-7-"+stage, 1, stage, extwork.ModeReportOnly, nil)
		if d, err := b.Offer(ctx, a.Host(identityA, adm), adm, "summary"); d != extwork.OfferDeclined || !errors.Is(err, omp.ErrWriteCapableStage) {
			t.Fatalf("stage %s Offer = %s %v", stage, d, err)
		}
		if _, err := b.Dispatch(ctx, adm, payload); !errors.Is(err, omp.ErrWriteCapableStage) || !errors.Is(err, extwork.ErrRefused) {
			t.Fatalf("stage %s Dispatch = %v", stage, err)
		}
	}
	if st := wb.stats(); st.Offers != 0 || st.Starts != 0 {
		t.Fatalf("a write-capable stage reached the workbench: %+v", st)
	}
	if !hasAction(sink, extwork.EventOfferDeclined) {
		t.Fatalf("refusals not audited: %v", sink.Actions())
	}
	ok, okPayload := admissionFor(t, "github:hivecommons/hive#7", "task-7-implement", 1, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, ok), ok, "summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("positive control Offer = %s %v", d, err)
	}
	if _, err := b.Dispatch(ctx, ok, okPayload); err != nil {
		t.Fatalf("positive control Dispatch: %v", err)
	}
	if st := wb.waitStats(func(s fixture.Stats) bool { return s.Runs == 1 }); st.Offers != 1 {
		t.Fatalf("positive control stats = %+v", st)
	}
}

// Disabled or shadow performs nothing: off refuses; shadow persists and
// audits the admission but sends no frame to the workbench and starts no
// run. Proven by the workbench's own counters, not a flag name.
func TestConformanceShadowAndOffNeverStart(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{})
	a := h.adapter(t, "")
	ctx := context.Background()
	adm, payload := admissionFor(t, "github:hivecommons/hive#1601", "task-1601", 1, "implement", extwork.ModeShadow, nil)

	off, _ := newBinding(t, a, nil, extwork.ModeOff)
	if _, err := off.Dispatch(ctx, adm, payload); !errors.Is(err, extwork.ErrDisabled) {
		t.Fatalf("off Dispatch = %v", err)
	}
	if _, err := off.Offer(ctx, a.Host(identityA, adm), adm, "s"); !errors.Is(err, extwork.ErrDisabled) {
		t.Fatalf("off Offer = %v", err)
	}
	shadow, sink := newBinding(t, a, nil, extwork.ModeShadow)
	if d, err := shadow.Offer(ctx, a.Host(identityA, adm), adm, "s"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("shadow Offer = %s %v", d, err)
	}
	res, err := shadow.Dispatch(ctx, adm, payload)
	if err != nil || !res.Shadow || res.Started {
		t.Fatalf("shadow Dispatch = %+v %v", res, err)
	}
	if _, err := shadow.Observe(ctx, adm, ""); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("shadow Observe = %v", err)
	}
	if rec, err := shadow.Recover(ctx, adm, payload); err != nil || rec.Started || rec.Adopted {
		t.Fatalf("shadow Recover = %+v %v", rec, err)
	}
	// Shadow sends no frame at all, so the workbench counters are read
	// synchronously; the positive control below proves they can move.
	if st := wb.stats(); st.Offers != 0 || st.Starts != 0 || st.Runs != 0 || st.Cancels != 0 {
		t.Fatalf("shadow/off mode touched the workbench: %+v", st)
	}
	if !hasAction(sink, extwork.EventShadowObserved) || hasAction(sink, extwork.EventStarted) {
		t.Fatalf("shadow events = %v", sink.Actions())
	}
	// Positive control: the same workbench and admission DO start in
	// report-only mode, so the counters above were capable of moving.
	liveAdm, livePayload := admissionFor(t, "github:hivecommons/hive#1601", "task-1601", 1, "implement", extwork.ModeReportOnly, nil)
	live, _ := newBinding(t, a, nil, extwork.ModeReportOnly)
	if d, err := live.Offer(ctx, a.Host(identityA, liveAdm), liveAdm, "s"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("live Offer = %s %v", d, err)
	}
	if _, err := live.Dispatch(ctx, liveAdm, livePayload); err != nil {
		t.Fatal(err)
	}
	if st := wb.waitStats(func(s fixture.Stats) bool { return s.Runs == 1 }); st.Offers != 1 {
		t.Fatalf("positive control stats = %+v", st)
	}
}

// Same key and payload twice yields one run on the workbench (the binding
// and the workbench both deduplicate); a changed payload under the same key
// conflicts; and a replacement adapter (hub restart, empty in-memory map)
// recovers by adopting the workbench's own run through the keyed start.
func TestConformanceDedupConflictAndRecover(t *testing.T) {
	h := startHub(t)
	wb := startWorkbench(t, h, wbOpts{})
	a := h.adapter(t, "")
	ctx := context.Background()
	store := extwork.NewFileStore(t.TempDir())
	b, _ := newBinding(t, a, store, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#4", "task-4", 1, "implement", extwork.ModeReportOnly, nil)
	if d, err := b.Offer(ctx, a.Host(identityA, adm), adm, "summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	res, err := b.Dispatch(ctx, adm, payload)
	if err != nil || res.Run.Deduplicated {
		t.Fatalf("first Dispatch = %+v %v", res, err)
	}
	res2, err := b.Dispatch(ctx, adm, payload)
	if err != nil || !res2.Run.Deduplicated || res2.Run.RemoteRunID != res.Run.RemoteRunID {
		t.Fatalf("second Dispatch = %+v %v", res2, err)
	}
	if st := wb.waitStats(func(s fixture.Stats) bool { return s.Starts == 2 }); st.Runs != 1 {
		t.Fatalf("workbench runs = %d, want 1", st.Runs)
	}
	changed, changedPayload := admissionFor(t, "github:hivecommons/hive#4", "task-4", 1, "implement", extwork.ModeReportOnly, map[string]string{"extra": "context"})
	if changed.ExecutionKey() != adm.ExecutionKey() {
		t.Fatal("test setup: changed payload must share the execution key")
	}
	if _, err := b.Dispatch(ctx, changed, changedPayload); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("changed payload = %v", err)
	}
	// The adapter refuses the conflict too, before any frame is sent.
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: changed, Payload: changedPayload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("adapter conflict = %v", err)
	}
	if st := wb.stats(); st.Starts != 2 || st.Runs != 1 {
		t.Fatalf("conflict reached the workbench: %+v", st)
	}
	// Replacement adapter over the same durable admission: it knows no
	// keys, so Observe is not-found and Recover issues the keyed start,
	// which the workbench answers as its existing run.
	replacement, _ := newBinding(t, h.adapter(t, ""), store, extwork.ModeReportOnly)
	rec, err := replacement.Recover(ctx, adm, payload)
	if err != nil || !rec.Started || !rec.Run.Deduplicated || rec.Run.RemoteRunID != res.Run.RemoteRunID {
		t.Fatalf("Recover = %+v %v", rec, err)
	}
	if st := wb.waitStats(func(s fixture.Stats) bool { return s.Starts == 3 }); st.Runs != 1 {
		t.Fatalf("recovery created a second run: %+v", st)
	}
	pinned, _, _ := replacement.Admission(adm.AssignmentID)
	runToTerminal(t, wb, replacement, pinned)
}

// Cancel facts: a workbench that stops reports stopped and the run ends
// without a receipt; one that ignores the request stays visibly running,
// finishes anyway, and its late success is rejected once authority is no
// longer current while the execution fact is kept.
func TestConformanceCancelFacts(t *testing.T) {
	h := startHub(t)
	ctx := context.Background()

	wb := startWorkbench(t, h, wbOpts{identity: "wb-stops"})
	a := h.adapter(t, "")
	b, _ := newBinding(t, a, nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#801", "task-801", 1, "implement", extwork.ModeReportOnly, nil)
	adm.Authority.Identity = "wb-stops"
	adm.RequestDigest = extwork.RequestDigest(payload)
	if d, err := b.Offer(ctx, a.Host("wb-stops", adm), adm, "s"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	wb.tick()
	waitObserve(t, b, pinned, extwork.StateRunning)
	facts, err := b.Cancel(ctx, pinned, "")
	if err != nil || !facts.Requested || !facts.Acknowledged || !facts.Stopped {
		t.Fatalf("Cancel = %+v %v", facts, err)
	}
	obs := waitObserve(t, b, pinned, extwork.StateTerminal)
	if obs.Receipt != nil {
		t.Fatalf("stopped run published a receipt: %+v", obs)
	}
	if d := b.Decide(pinned, nil, nil, true); d.Verdict != extwork.VerdictUncertain {
		t.Fatalf("Decide without receipt = %+v", d)
	}
	if st := wb.stats(); st.Cancels != 1 {
		t.Fatalf("cancels = %d", st.Cancels)
	}

	ignoring := startWorkbench(t, h, wbOpts{identity: "wb-ignores", ignoreCancel: true})
	b2, sink := newBinding(t, a, nil, extwork.ModeReportOnly)
	adm2, payload2 := admissionFor(t, "github:hivecommons/hive#802", "task-802", 1, "implement", extwork.ModeReportOnly, nil)
	adm2.Authority.Identity = "wb-ignores"
	if d, err := b2.Offer(ctx, a.Host("wb-ignores", adm2), adm2, "s"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	if _, err := b2.Dispatch(ctx, adm2, payload2); err != nil {
		t.Fatal(err)
	}
	pinned2, _, _ := b2.Admission(adm2.AssignmentID)
	ignoring.tick()
	waitObserve(t, b2, pinned2, extwork.StateRunning)
	facts, err = b2.Cancel(ctx, pinned2, "")
	if err != nil || !facts.Requested || !facts.Acknowledged || facts.Stopped {
		t.Fatalf("ignored Cancel = %+v %v (stopped must stay false)", facts, err)
	}
	if obs, err := b2.Observe(ctx, pinned2, ""); err != nil || obs.State != extwork.StateRunning {
		t.Fatalf("after ignored cancel = %+v %v", obs, err)
	}
	ignoring.tick()
	ignoring.tick()
	obs = waitObserve(t, b2, pinned2, extwork.StateTerminal)
	receipt, err := b2.FetchReceipt(ctx, pinned2, "", *obs.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if d := b2.Decide(pinned2, receipt, nil, false); d.Verdict != extwork.VerdictRejected || !d.ExecutionFact {
		t.Fatalf("late success after revocation = %+v", d)
	}
	if !hasAction(sink, extwork.EventCancelRequested) {
		t.Fatalf("cancel facts not recorded: %v", sink.Actions())
	}
}

// Peer capability gate against the real process: a workbench that connects
// without ext-exec/omp is refused at attachment and exits; it is never
// registered as a peer of any kind.
func TestConformancePeerWithoutCapabilityRefused(t *testing.T) {
	h := startHub(t)
	p := startWorkbench(t, h, wbOpts{identity: "no-cap", noCapability: true})
	if !strings.Contains(p.stderr.String(), "refused") {
		t.Fatalf("stderr = %q, want a refusal", p.stderr.String())
	}
	if _, ok := h.broker.Peer("no-cap"); ok || len(h.broker.Identities()) != 0 {
		t.Fatal("a workbench without the capability was attached")
	}
	// Positive control: the same process image with the capability attaches.
	startWorkbench(t, h, wbOpts{identity: "with-cap"})
	if ids := h.broker.Identities(); len(ids) != 1 || ids[0] != "with-cap" {
		t.Fatalf("identities = %v", ids)
	}
}

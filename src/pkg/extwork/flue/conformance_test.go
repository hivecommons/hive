package flue_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/flue"
	"github.com/hivecommons/hive/pkg/extwork/flue/fixture"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// The conformance rows of #8201 that Gate 1 must pass (see the Gate 1 split in
// src/docs/design/external-workflow-admission.md) are exercised here against
// the REAL fixture process: a second OS process speaking Flue's native surface,
// not a mock of the adapter interface.

const (
	fixtureStartTimeout = 10 * time.Second
	readyPollInterval   = 20 * time.Millisecond
	stageCount          = 3
)

// fixtureProc is a running fixture child process.
type fixtureProc struct {
	t           *testing.T
	cmd         *exec.Cmd
	addr        string
	stateDir    string
	incarnation string
	proxy       string
	ignoreAbort bool
	done        chan struct{}
}

type fixtureOpts struct {
	stateDir    string
	incarnation string
	ignoreAbort bool
	proxy       string
	// listen pins the host:port. A restart reuses the previous address, because
	// the adapter is configured with a fixed endpoint and must never follow a
	// moving one; a recreated engine is recognised by its incarnation, not by
	// where it listens.
	listen string
}

func startFixture(t *testing.T, o fixtureOpts) *fixtureProc {
	t.Helper()
	if o.stateDir == "" {
		o.stateDir = t.TempDir()
	}
	args := []string{"-test.run=^$", "--", "-workflow", workflowDir, "-state", o.stateDir}
	if o.incarnation != "" {
		args = append(args, "-incarnation", o.incarnation)
	}
	if o.ignoreAbort {
		args = append(args, "-ignore-abort")
	}
	if o.proxy != "" {
		args = append(args, "-egress-proxy", o.proxy)
	}
	if o.listen != "" {
		args = append(args, "-listen", o.listen)
	}
	cmd := exec.Command(os.Args[0], args...)
	// A scrubbed environment: no GITHUB_TOKEN, no dashboard token, no proxy
	// variables. The fixture must find nothing it could use to act outside
	// the lease.
	cmd.Env = []string{childEnv + "=1", "PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + os.Getenv("TMPDIR")}
	cmd.WaitDelay = fixtureStartTimeout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &fixtureProc{t: t, cmd: cmd, stateDir: o.stateDir, incarnation: o.incarnation, proxy: o.proxy, ignoreAbort: o.ignoreAbort, done: make(chan struct{})}
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
	select {
	case p.addr = <-addrCh:
	case <-time.After(fixtureStartTimeout):
		_ = cmd.Process.Kill()
		t.Fatal("fixture process did not print its address")
	}
	t.Cleanup(p.kill)
	p.waitReady()
	return p
}

// waitReady blocks until the fixture answers its info endpoint, so a test
// never observes a process that has printed its address but is not yet
// accepting connections (relevant after a restart on a reused port).
func (p *fixtureProc) waitReady() {
	p.t.Helper()
	deadline := time.Now().Add(fixtureStartTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(readyPollInterval)
	}
	p.t.Fatalf("fixture at %s did not become ready", p.addr)
}

// kill simulates the engine going away (an outage or a hard restart).
func (p *fixtureProc) kill() {
	if p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	<-p.done
}

// restart brings the engine back on its own state dir: the same incarnation
// resumes; a different one is a recreated instance.
func (p *fixtureProc) restart(incarnation string) *fixtureProc {
	p.kill()
	return startFixture(p.t, fixtureOpts{stateDir: p.stateDir, incarnation: incarnation, ignoreAbort: p.ignoreAbort, proxy: p.proxy, listen: strings.TrimPrefix(p.addr, "http://")})
}

func (p *fixtureProc) control(method, path string) []byte {
	p.t.Helper()
	req, err := http.NewRequest(method, p.addr+path, nil)
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

func (p *fixtureProc) tick() { p.control(http.MethodPost, "/control/tick") }

func (p *fixtureProc) stats() fixture.Stats {
	var st fixture.Stats
	if err := json.Unmarshal(p.control(http.MethodGet, "/control/stats"), &st); err != nil {
		p.t.Fatal(err)
	}
	return st
}

func (p *fixtureProc) adapter(version string) *flue.Adapter {
	p.t.Helper()
	if version == "" {
		version = workflowVersion
	}
	a, err := flue.New(flue.Config{Endpoint: p.addr, WorkflowVersion: version})
	if err != nil {
		p.t.Fatal(err)
	}
	return a
}

type denyProxy struct {
	mu   sync.Mutex
	seen []string
}

func (d *denyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.seen = append(d.seen, r.Method+" "+r.Host)
	d.mu.Unlock()
	w.WriteHeader(http.StatusForbidden)
}

func (d *denyProxy) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func newBinding(t *testing.T, a extwork.Adapter, store extwork.AdmissionStore, mode string, opts ...extwork.Option) (*extwork.Binding, *extwork.MemorySink) {
	t.Helper()
	sink := &extwork.MemorySink{}
	if store == nil {
		store = extwork.NewFileStore(t.TempDir())
	}
	return extwork.New(a, store, extwork.NewFileStore(t.TempDir()), sink, mode, opts...), sink
}

// runToTerminal ticks the fixture through every stage and returns the final
// observation, which must carry the receipt reference.
func runToTerminal(t *testing.T, p *fixtureProc, b *extwork.Binding, adm extwork.Admission) extwork.Observation {
	t.Helper()
	ctx := context.Background()
	var obs extwork.Observation
	for range stageCount {
		p.tick()
		var err error
		obs, err = b.Observe(ctx, adm, "")
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	if obs.State != extwork.StateTerminal || obs.Receipt == nil {
		t.Fatalf("not terminal after %d ticks: %+v", stageCount, obs)
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

// Row 1: admitted task and valid immutable report; an unrelated admitted
// task also succeeds. Rows 4 and 15 ride along: a second dispatch of the same
// key and payload deduplicates, and the settled result replays without a
// second run.
func TestConformanceAdmittedTaskAndReplay(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	a := p.adapter("")
	ctx := context.Background()
	b, sink := newBinding(t, a, nil, extwork.ModeReportOnly)

	adm, payload := admissionFor(t, "github:hivecommons/hive#101", "task-101", 1, nil)
	if d, err := b.Offer(ctx, nil, adm, "summary only"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("Offer = %s %v", d, err)
	}
	res, err := b.Dispatch(ctx, adm, payload)
	if err != nil || !res.Started || res.Run.Deduplicated {
		t.Fatalf("Dispatch = %+v %v", res, err)
	}
	pinned, ok, err := b.Admission(adm.AssignmentID)
	if err != nil || !ok || pinned.EngineIncarnation == "" || pinned.EngineIncarnation != res.Run.RemoteIncarnation {
		t.Fatalf("admission not pinned to the engine incarnation: %+v %v %v", pinned, ok, err)
	}
	// Row 4: same key, same payload, twice: one native run.
	res2, err := b.Dispatch(ctx, adm, payload)
	if err != nil || !res2.Run.Deduplicated || res2.Run.RemoteRunID != res.Run.RemoteRunID {
		t.Fatalf("second Dispatch = %+v %v", res2, err)
	}
	if st := p.stats(); st.Runs != 1 {
		t.Fatalf("native runs = %d, want 1", st.Runs)
	}
	obs := runToTerminal(t, p, b, pinned)
	receipt, err := b.FetchReceipt(ctx, pinned, "", *obs.Receipt)
	if err != nil {
		t.Fatalf("FetchReceipt: %v", err)
	}
	if receipt.RemoteRunID != res.Run.RemoteRunID || receipt.RemoteIncarnation != res.Run.RemoteIncarnation || len(receipt.Artifacts) != 2 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if d := b.Decide(pinned, receipt, nil, true); d.Verdict != extwork.VerdictAccepted {
		t.Fatalf("Decide = %+v", d)
	}
	// Row 15: replay without a second run.
	dispatches := p.stats().Dispatches
	replayed, ok, err := b.Replay(pinned)
	if err != nil || !ok || replayed.OutputDigest != receipt.OutputDigest {
		t.Fatalf("Replay = %+v %v %v", replayed, ok, err)
	}
	if p.stats().Dispatches != dispatches {
		t.Fatal("replay dispatched again")
	}
	// An unrelated admitted task also succeeds and gets its own native run.
	other, otherPayload := admissionFor(t, "github:hivecommons/hive#102", "task-102", 1, nil)
	if _, err := b.Dispatch(ctx, other, otherPayload); err != nil {
		t.Fatal(err)
	}
	if st := p.stats(); st.Runs != 2 {
		t.Fatalf("native runs = %d, want 2", st.Runs)
	}
	for _, want := range []string{extwork.EventOfferAccepted, extwork.EventAdmissionPersisted, extwork.EventStarted, extwork.EventProgress, extwork.EventReceiptVerified, extwork.EventDecision} {
		if !hasAction(sink, want) {
			t.Errorf("missing progress event %s in %v", want, sink.Actions())
		}
	}
}

// Row 2: a held task starts nothing, and unrelated ready work still runs.
// Holding is the host declining the offer before any context is delivered.
func TestConformanceHeldTaskStartsNothing(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	ctx := context.Background()
	b, sink := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	host := extwork.NewInteractiveHost()

	held, heldPayload := admissionFor(t, "github:hivecommons/hive#201", "task-201", 1, nil)
	host.Answer(held.ExecutionKey(), extwork.OfferDeclined, "held by operator")
	if d, err := b.Offer(ctx, host, held, "held summary"); d != extwork.OfferDeclined || !errors.Is(err, extwork.ErrDeclined) {
		t.Fatalf("held Offer = %s %v", d, err)
	}
	if p.stats().Dispatches != 0 || len(heldPayload) == 0 {
		t.Fatal("a declined offer reached the engine")
	}
	ready, readyPayload := admissionFor(t, "github:hivecommons/hive#202", "task-202", 1, nil)
	host.Answer(ready.ExecutionKey(), extwork.OfferAccepted, "ok")
	if d, err := b.Offer(ctx, host, ready, "ready summary"); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("ready Offer = %s %v", d, err)
	}
	if _, err := b.Dispatch(ctx, ready, readyPayload); err != nil {
		t.Fatal(err)
	}
	if st := p.stats(); st.Runs != 1 {
		t.Fatalf("native runs = %d, want 1 (only the ready task)", st.Runs)
	}
	if !hasAction(sink, extwork.EventOfferDeclined) || !hasAction(sink, extwork.EventOfferAccepted) {
		t.Fatalf("offer decisions not recorded: %v", sink.Actions())
	}
}

// Row 3: missing capability or unsupported version is refused, never
// downgraded, and nothing reaches the engine.
func TestConformanceCapabilityAndVersionRefused(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	ctx := context.Background()
	adm, payload := admissionFor(t, "github:hivecommons/hive#301", "task-301", 1, nil)

	b, _ := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	noCap := adm
	noCap.Authority.Capability = "run-stage"
	if _, err := b.Dispatch(ctx, noCap, payload); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("missing capability = %v", err)
	}
	pinnedElsewhere, _ := newBinding(t, p.adapter("flue-fixture/2.0.0"), nil, extwork.ModeReportOnly)
	if _, err := pinnedElsewhere.Dispatch(ctx, adm, payload); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("unsupported version = %v", err)
	}
	if st := p.stats(); st.Dispatches != 0 || st.Runs != 0 {
		t.Fatalf("refused requests reached the engine: %+v", st)
	}
	// Positive control: the same request with the capability is admitted.
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatalf("positive control: %v", err)
	}
}

// Row 5: same key with a changed payload conflicts; a recreated engine
// instance is never adopted.
func TestConformanceConflictAndRecreatedEngine(t *testing.T) {
	p := startFixture(t, fixtureOpts{incarnation: "gen-a"})
	ctx := context.Background()
	store := extwork.NewFileStore(t.TempDir())
	b, _ := newBinding(t, p.adapter(""), store, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#501", "task-501", 1, nil)
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	changed, changedPayload := admissionFor(t, "github:hivecommons/hive#501", "task-501", 1, map[string]string{"extra": "context"})
	if changed.ExecutionKey() != adm.ExecutionKey() {
		t.Fatal("test setup: changed payload must share the execution key")
	}
	if _, err := b.Dispatch(ctx, changed, changedPayload); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("changed payload = %v", err)
	}
	if stored, _, _ := store.Load(adm.AssignmentID); stored.RequestDigest != adm.RequestDigest || stored.RemoteRunID == "" {
		t.Fatalf("a conflicting dispatch touched the durable admission: %+v", stored)
	}
	// The engine refuses natively too: same idempotency key, different
	// payload is submission_conflict at admission.
	if _, err := p.adapter("").Start(ctx, extwork.StartRequest{Admission: changed, Payload: changedPayload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("native conflict = %v", err)
	}
	if p.stats().Runs != 1 {
		t.Fatal("conflict created a second run")
	}

	// Delete and recreate the engine: a different instance, empty state. The
	// durable record knows the native run was accepted, so the run is lost
	// with the old instance: uncertain, nothing adopted, nothing started.
	p = p.restart("gen-b")
	addr := p.addr
	b2, sink := newBinding(t, p.adapter(""), store, extwork.ModeReportOnly)
	rec, err := b2.Recover(ctx, adm, payload)
	if !errors.Is(err, extwork.ErrUncertain) || !errors.Is(err, extwork.ErrIncarnationMismatch) || rec.Adopted || rec.Started {
		t.Fatalf("recover on recreated engine = %+v %v", rec, err)
	}
	if st := p.stats(); st.Dispatches != 0 {
		t.Fatalf("recreated engine received %d dispatches", st.Dispatches)
	}
	// The other half of the table: the process died after the engine accepted
	// but before the record learned the run id, and the engine was recreated.
	// Nothing is outstanding on the live instance, so the admission is
	// re-pinned to it and started there exactly once.
	crash := errors.New("simulated process death")
	lostStore := extwork.NewFileStore(t.TempDir())
	lost, _ := newBinding(t, p.adapter(""), lostStore, extwork.ModeReportOnly, extwork.WithHooks(extwork.Hooks{AfterStart: func() error { return crash }}))
	lostAdm, lostPayload := admissionFor(t, "github:hivecommons/hive#502", "task-502", 1, nil)
	if _, err := lost.Dispatch(ctx, lostAdm, lostPayload); !errors.Is(err, crash) {
		t.Fatalf("crash not surfaced: %v", err)
	}
	if stored, _, _ := lostStore.Load(lostAdm.AssignmentID); stored.RemoteRunID != "" || stored.EngineIncarnation != "gen-b" {
		t.Fatalf("record after crash = %+v", stored)
	}
	p = p.restart("gen-c")
	if p.addr != addr {
		t.Fatalf("recreated engine moved from %s to %s; the adapter's endpoint is fixed configuration", addr, p.addr)
	}
	after, _ := newBinding(t, p.adapter(""), lostStore, extwork.ModeReportOnly)
	rec, err = after.Recover(ctx, lostAdm, lostPayload)
	if err != nil || !rec.Started || rec.Adopted || rec.Uncertain || rec.Run.RemoteIncarnation != "gen-c" {
		t.Fatalf("recover on recreated engine without recorded run = %+v %v", rec, err)
	}
	if st := p.stats(); st.Runs != 1 || st.Dispatches != 1 {
		t.Fatalf("live engine after re-pin: %+v", st)
	}
	if stored, _, _ := lostStore.Load(lostAdm.AssignmentID); stored.EngineIncarnation != "gen-c" || stored.RemoteRunID != rec.Run.RemoteRunID {
		t.Fatalf("record not re-pinned: %+v", stored)
	}
	if !hasAction(sink, extwork.EventRecovered) {
		t.Fatalf("uncertainty not recorded: %v", sink.Actions())
	}
	pinned, ok, err := b2.Admission(adm.AssignmentID)
	if err != nil || !ok || pinned.EngineIncarnation != "gen-a" {
		t.Fatalf("pinned admission = %+v %v %v", pinned, ok, err)
	}
	// b2 still talks to the configured endpoint, now served by gen-c: the
	// pinned gen-a incarnation mismatches, so nothing is adopted.
	if _, err := b2.Observe(ctx, pinned, ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("observe on recreated engine = %v", err)
	}
}

// Row 6: process death before start, after remote accept, and after receipt
// persistence each recover without a blind duplicate dispatch. Row 7 rides
// along: the recovering binding is a different owner over the same durable
// admission and adopts the same logical run.
func TestConformanceCrashWindows(t *testing.T) {
	p := startFixture(t, fixtureOpts{incarnation: "stable"})
	ctx := context.Background()
	crash := errors.New("simulated process death")

	// Window 1: durable admission, no start yet.
	store1 := extwork.NewFileStore(t.TempDir())
	b1, _ := newBinding(t, p.adapter(""), store1, extwork.ModeReportOnly, extwork.WithHooks(extwork.Hooks{AfterPersist: func() error { return crash }}))
	adm1, payload1 := admissionFor(t, "github:hivecommons/hive#601", "task-601", 1, nil)
	if _, err := b1.Dispatch(ctx, adm1, payload1); !errors.Is(err, crash) {
		t.Fatalf("window 1 dispatch = %v", err)
	}
	r1, _ := newBinding(t, p.adapter(""), store1, extwork.ModeReportOnly)
	rec, err := r1.Recover(ctx, adm1, payload1)
	if err != nil || !rec.Started || rec.Adopted {
		t.Fatalf("window 1 recover = %+v %v", rec, err)
	}
	if st := p.stats(); st.Runs != 1 {
		t.Fatalf("window 1 runs = %d", st.Runs)
	}

	// Window 2: remote accepted, process died before recording anything else.
	store2 := extwork.NewFileStore(t.TempDir())
	b2, _ := newBinding(t, p.adapter(""), store2, extwork.ModeReportOnly, extwork.WithHooks(extwork.Hooks{AfterStart: func() error { return crash }}))
	adm2, payload2 := admissionFor(t, "github:hivecommons/hive#602", "task-602", 1, nil)
	if _, err := b2.Dispatch(ctx, adm2, payload2); !errors.Is(err, crash) {
		t.Fatalf("window 2 dispatch = %v", err)
	}
	replacement, _ := newBinding(t, p.adapter(""), store2, extwork.ModeReportOnly)
	rec, err = replacement.Recover(ctx, adm2, payload2)
	if err != nil || !rec.Adopted || rec.Started || !rec.Run.Deduplicated {
		t.Fatalf("window 2 recover = %+v %v", rec, err)
	}
	if st := p.stats(); st.Runs != 2 {
		t.Fatalf("window 2 runs = %d, want 2 (no duplicate)", st.Runs)
	}

	// Window 3: receipt persisted, then death; recovery replays and never
	// touches the engine.
	pinned2, _, _ := replacement.Admission(adm2.AssignmentID)
	obs := runToTerminal(t, p, replacement, pinned2)
	if _, err := replacement.FetchReceipt(ctx, pinned2, "", *obs.Receipt); err != nil {
		t.Fatal(err)
	}
	p.kill()
	rec, err = replacement.Recover(ctx, adm2, payload2)
	if err != nil || !rec.Adopted || rec.Observation.State != extwork.StateTerminal {
		t.Fatalf("window 3 recover = %+v %v", rec, err)
	}
	if replayed, ok, err := replacement.Replay(pinned2); err != nil || !ok || replayed.RemoteRunID == "" {
		t.Fatalf("window 3 replay = %+v %v %v", replayed, ok, err)
	}

	// Missing durable admission: no authority is issued at all.
	fresh, _ := newBinding(t, p.adapter(""), extwork.NewFileStore(t.TempDir()), extwork.ModeReportOnly)
	if _, err := fresh.Recover(ctx, adm1, payload1); !errors.Is(err, extwork.ErrNoDurableAdmission) {
		t.Fatalf("recover without durable admission = %v", err)
	}
}

// Row 8: cancel delivered but the workload ignores it, and success races
// cancellation. No false stopped state; late output cannot resurrect revoked
// authority.
func TestConformanceCancelIgnoredAndLateSuccess(t *testing.T) {
	p := startFixture(t, fixtureOpts{ignoreAbort: true})
	ctx := context.Background()
	b, sink := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#801", "task-801", 1, nil)
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	p.tick()
	facts, err := b.Cancel(ctx, pinned, "")
	if err != nil || !facts.Requested || !facts.Acknowledged || facts.Stopped {
		t.Fatalf("Cancel = %+v %v (stopped must stay false when the workload ignores abort)", facts, err)
	}
	obs, err := b.Observe(ctx, pinned, "")
	if err != nil || obs.State != extwork.StateRunning {
		t.Fatalf("after ignored cancel state = %+v %v", obs, err)
	}
	// The workload finishes anyway; the receipt verifies, but authority was
	// revoked, so the candidate is rejected while the execution fact stays.
	obs = runToTerminal(t, p, b, pinned)
	receipt, err := b.FetchReceipt(ctx, pinned, "", *obs.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	d := b.Decide(pinned, receipt, nil, false)
	if d.Verdict != extwork.VerdictRejected || !d.ExecutionFact {
		t.Fatalf("late success after revocation = %+v", d)
	}
	if !hasAction(sink, extwork.EventCancelRequested) {
		t.Fatalf("cancel facts not recorded: %v", sink.Actions())
	}
	if p.stats().AbortRequests != 1 {
		t.Fatalf("abort requests = %d", p.stats().AbortRequests)
	}
}

// Row 9: poll outage; the authoritative native state wins once the engine is
// back. Row 13: a run waiting on a human is visible as waiting.
func TestConformanceOutageAndWaiting(t *testing.T) {
	p := startFixture(t, fixtureOpts{incarnation: "durable"})
	ctx := context.Background()
	store := extwork.NewFileStore(t.TempDir())
	b, sink := newBinding(t, p.adapter(""), store, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#901", "task-901", 1, nil)
	res, err := b.Dispatch(ctx, adm, payload)
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	p.tick()
	p.control(http.MethodPost, "/control/wait/"+res.Run.RemoteRunID)
	obs, err := b.Observe(ctx, pinned, "")
	if err != nil || obs.State != extwork.StateWaiting {
		t.Fatalf("waiting = %+v %v", obs, err)
	}
	p.control(http.MethodPost, "/control/resume/"+res.Run.RemoteRunID)

	// Outage: the engine is gone; observation is unknown, not failed.
	p.kill()
	obs, err = b.Observe(ctx, pinned, "")
	if !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown {
		t.Fatalf("outage observe = %+v %v", obs, err)
	}
	rec, err := b.Recover(ctx, adm, payload)
	if !errors.Is(err, extwork.ErrUncertain) || !rec.Uncertain {
		t.Fatalf("outage recover = %+v %v", rec, err)
	}
	// Same incarnation resumes from its own state: the run is still there and
	// the local unknown is replaced by the authoritative running state.
	p = p.restart("durable")
	b2, _ := newBinding(t, p.adapter(""), store, extwork.ModeReportOnly)
	rec, err = b2.Recover(ctx, adm, payload)
	if err != nil || !rec.Adopted || rec.Run.RemoteRunID != res.Run.RemoteRunID || rec.Observation.State != extwork.StateRunning {
		t.Fatalf("post-outage recover = %+v %v", rec, err)
	}
	if p.stats().Runs != 1 {
		t.Fatal("outage recovery created a second run")
	}
	states := []extwork.State{}
	for _, ev := range sink.Events() {
		if ev.Action == extwork.EventProgress {
			states = append(states, ev.State)
		}
	}
	if len(states) < 2 || states[len(states)-1] != extwork.StateUnknown {
		t.Fatalf("progress events must show waiting then unknown, got %v", states)
	}
}

// Row 10: artifact missing, truncated, malicious path, wrong digest, or an
// unavailable store: nothing is accepted. Row 11: a receipt cannot authorise
// a different subject. Row 12: engine says success but the verifier rejects.
func TestConformanceArtifactAndVerifierRefusals(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	ctx := context.Background()
	b, sink := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#1001", "task-1001", 1, nil)
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	obs := runToTerminal(t, p, b, pinned)
	ref := *obs.Receipt

	cases := map[string]struct {
		ref  extwork.ReceiptRef
		want error
	}{
		"missing":        {extwork.ReceiptRef{Path: "nope.json", Digest: ref.Digest, Size: ref.Size}, extwork.ErrReceiptMissing},
		"truncated":      {extwork.ReceiptRef{Path: ref.Path, Digest: ref.Digest, Size: ref.Size + 1}, extwork.ErrReceiptTruncated},
		"malicious path": {extwork.ReceiptRef{Path: "../../etc/passwd", Digest: ref.Digest, Size: ref.Size}, extwork.ErrReceiptPath},
		"wrong digest":   {extwork.ReceiptRef{Path: ref.Path, Digest: strings.Repeat("0", 64), Size: ref.Size}, extwork.ErrReceiptDigest},
		"oversized":      {extwork.ReceiptRef{Path: ref.Path, Digest: ref.Digest, Size: extwork.DefaultMaxReceiptBytes + 1}, extwork.ErrReceiptTooLarge},
	}
	for name, tc := range cases {
		if _, err := b.FetchReceipt(ctx, pinned, "", tc.ref); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if _, ok, _ := b.Replay(pinned); ok {
			t.Errorf("%s: a refused receipt was stored", name)
		}
	}
	// Row 11: another subject has another execution key, so the engine holds
	// nothing for it and the fetch is refused; and even the honest bytes,
	// once fetched, cannot be bound to that other subject.
	other := pinned
	other.InputRevision = strings.Repeat("a", 40)
	if _, err := b.FetchReceipt(ctx, other, "", ref); !errors.Is(err, extwork.ErrReceiptRefused) {
		t.Fatalf("other subject fetch = %v", err)
	}
	if _, ok, _ := b.Replay(other); ok {
		t.Fatal("a refused fetch for another subject was stored")
	}
	// Positive control: the honest reference verifies.
	receipt, err := b.FetchReceipt(ctx, pinned, "", ref)
	if err != nil {
		t.Fatalf("honest receipt refused: %v", err)
	}
	if err := extwork.BindReceipt(other, receipt); !errors.Is(err, extwork.ErrReceiptUnbound) {
		t.Fatalf("honest receipt bound to another subject: %v", err)
	}
	// Row 12: the verifier rejects; the execution fact is retained.
	reject := func(extwork.Admission, *outputschema.StageReceipt) error { return errors.New("patch does not apply") }
	if d := b.Decide(pinned, receipt, reject, true); d.Verdict != extwork.VerdictRejected || !d.ExecutionFact {
		t.Fatalf("verifier reject = %+v", d)
	}
	if !hasAction(sink, extwork.EventReceiptRefused) || !hasAction(sink, extwork.EventReceiptVerified) {
		t.Fatalf("receipt events = %v", sink.Actions())
	}
	// Unavailable store: a receipt that cannot be persisted is not accepted.
	blocked := extwork.NewFileStore("/dev/null/not-a-dir")
	nb := extwork.New(p.adapter(""), extwork.NewMemoryStore(), blocked, nil, extwork.ModeReportOnly)
	if _, err := nb.FetchReceipt(ctx, pinned, "", ref); err == nil {
		t.Fatal("receipt accepted although the store is unavailable")
	}
	// Result classes are their own verdicts, not success.
	for hint, want := range map[string]extwork.Verdict{"no_change": extwork.VerdictNoChange, "blocked": extwork.VerdictBlocked, "failed": extwork.VerdictRejected} {
		hadm, hpayload := admissionFor(t, "github:hivecommons/hive#1001-"+hint, "task-1001-"+hint, 1, map[string]string{fixture.HintResultClass: hint})
		if _, err := b.Dispatch(ctx, hadm, hpayload); err != nil {
			t.Fatal(err)
		}
		hp, _, _ := b.Admission(hadm.AssignmentID)
		hobs := runToTerminal(t, p, b, hp)
		hr, err := b.FetchReceipt(ctx, hp, "", *hobs.Receipt)
		if err != nil {
			t.Fatalf("%s receipt: %v", hint, err)
		}
		if d := b.Decide(hp, hr, nil, true); d.Verdict != want {
			t.Errorf("%s verdict = %s, want %s", hint, d.Verdict, want)
		}
	}
}

// Row 14: the store refuses the write; no unpersisted authority becomes
// active and nothing reaches the engine.
func TestConformanceStoreWriteFails(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	ctx := context.Background()
	store := extwork.NewMemoryStore()
	store.FailPersist = extwork.ErrStoreUnavailable
	b, _ := newBinding(t, p.adapter(""), store, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#1401", "task-1401", 1, nil)
	if _, err := b.Dispatch(ctx, adm, payload); !errors.Is(err, extwork.ErrStoreUnavailable) {
		t.Fatalf("Dispatch = %v", err)
	}
	if st := p.stats(); st.Dispatches != 0 {
		t.Fatalf("engine received %d dispatches after a failed persist", st.Dispatches)
	}
	if _, err := b.Recover(ctx, adm, payload); !errors.Is(err, extwork.ErrNoDurableAdmission) {
		t.Fatalf("Recover = %v", err)
	}
}

// Row 16: disabled or shadow-only performs no external starts. The property
// is proven by the engine's own counters, not by a flag name.
func TestConformanceShadowAndOffNeverStart(t *testing.T) {
	p := startFixture(t, fixtureOpts{})
	ctx := context.Background()
	adm, payload := admissionFor(t, "github:hivecommons/hive#1601", "task-1601", 1, nil)

	off, _ := newBinding(t, p.adapter(""), nil, extwork.ModeOff)
	if _, err := off.Dispatch(ctx, adm, payload); !errors.Is(err, extwork.ErrDisabled) {
		t.Fatalf("off Dispatch = %v", err)
	}
	shadowStore := extwork.NewFileStore(t.TempDir())
	shadow, sink := newBinding(t, p.adapter(""), shadowStore, extwork.ModeShadow)
	if d, err := shadow.Offer(ctx, nil, adm, "s"); d != extwork.OfferAccepted || err != nil {
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
	if st := p.stats(); st.Dispatches != 0 || st.Runs != 0 || st.AbortRequests != 0 {
		t.Fatalf("shadow/off mode touched the engine: %+v", st)
	}
	if !hasAction(sink, extwork.EventShadowObserved) || hasAction(sink, extwork.EventStarted) {
		t.Fatalf("shadow events = %v", sink.Actions())
	}
	// Positive control: the same engine and admission DO start in report-only
	// mode, so the counters above were capable of moving.
	live, _ := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	if _, err := live.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	if st := p.stats(); st.Runs != 1 {
		t.Fatalf("positive control runs = %d", st.Runs)
	}
}

// Step 8: the side-effect inventory is enforced. The fixture's instrument
// stage attempts one outbound POST (a preview publish) and one GitHub write
// (a comment). Both must be refused by the egress boundary, the fixture must
// hold no GitHub token, and every class (b) artifact must appear in the
// receipt.
func TestConformanceSideEffectsBlockedAndArtifactsListed(t *testing.T) {
	proxy := &denyProxy{}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	p := startFixture(t, fixtureOpts{proxy: ps.URL})
	ctx := context.Background()
	b, _ := newBinding(t, p.adapter(""), nil, extwork.ModeReportOnly)
	adm, payload := admissionFor(t, "github:hivecommons/hive#8", "task-8", 1, nil)
	if _, err := b.Dispatch(ctx, adm, payload); err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := b.Admission(adm.AssignmentID)
	obs := runToTerminal(t, p, b, pinned)
	receipt, err := b.FetchReceipt(ctx, pinned, "", *obs.Receipt)
	if err != nil {
		t.Fatal(err)
	}

	st := p.stats()
	if st.GitHubTokenPresent {
		t.Fatal("fixture process saw a GITHUB_TOKEN")
	}
	if len(st.EffectAttempts) != 2 {
		t.Fatalf("effect attempts = %+v, want the outbound POST and the GitHub write", st.EffectAttempts)
	}
	kinds := map[string]bool{}
	for _, a := range st.EffectAttempts {
		kinds[a.Kind] = true
		if a.Outcome != fixture.EffectRefused {
			t.Errorf("class (a) effect %s to %s was %s, want refused", a.Kind, a.Target, a.Outcome)
		}
	}
	if !kinds[fixture.EffectOutboundPost] || !kinds[fixture.EffectGitHubWrite] {
		t.Fatalf("attempt kinds = %v", kinds)
	}
	seen := proxy.requests()
	if len(seen) != 2 {
		t.Fatalf("egress boundary saw %v, want exactly two refused CONNECTs", seen)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, http.MethodConnect+" ") {
			t.Errorf("effect reached the boundary as %q, not a refused CONNECT", s)
		}
	}
	// Class (b): every artifact the workflow declares is in the receipt.
	wf, err := fixture.LoadWorkflow(workflowDir)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, stg := range wf.Stages {
		for _, art := range stg.Artifacts {
			declared[art.Path] = true
		}
	}
	listed := map[string]bool{}
	for _, art := range receipt.Artifacts {
		listed[art.Path] = true
	}
	for path := range declared {
		if !listed[path] {
			t.Errorf("class (b) artifact %s missing from receipt %v", path, receipt.Artifacts)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("receipt lists %v, workflow declares %v", listed, declared)
	}
}

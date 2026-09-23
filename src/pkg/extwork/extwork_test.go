package extwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	testInputRevision = "0123456789abcdef0123456789abcdef01234567"
	testEngine        = "fake"
	testVersion       = "fake-1"
)

func testAdmission() Admission {
	payload := testPayload()
	return Admission{
		WorkKey:          "github:hivecommons/hive#1",
		AssignmentID:     "task-1",
		Generation:       7,
		Stage:            "implement",
		ContractRevision: "contract-1",
		Engine:           testEngine,
		WorkflowVersion:  testVersion,
		InputRevision:    testInputRevision,
		RequestDigest:    RequestDigest(payload),
		Authority:        AuthorityBinding{Identity: "relay-a", Tier: "T1", Capability: "ext-exec/fake", Mode: ModeReportOnly},
	}
}

func testPayload() []byte { return []byte(`{"summary":"bounded bundle"}`) }

// receiptJSON builds a valid stage_receipt AgentReport bound to adm.
func receiptJSON(t *testing.T, adm Admission, class outputschema.StageReceiptResultClass, artifacts []outputschema.Artifact, mutate func(*outputschema.StageReceipt)) []byte {
	t.Helper()
	if artifacts == nil {
		artifacts = []outputschema.Artifact{}
	}
	parts := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		parts = append(parts, strings.Join([]string{a.Repo, a.Path, a.Description}, "\x00"))
	}
	sort.Strings(parts)
	receipt := &outputschema.StageReceipt{
		SchemaVersion:    outputschema.StageReceiptSchemaVersion,
		WorkKey:          adm.WorkKey,
		AssignmentID:     adm.AssignmentID,
		Generation:       adm.Generation,
		Stage:            adm.Stage,
		ContractRevision: adm.ContractRevision,
		ExecutionKey:     string(adm.ExecutionKey()),
		Engine:           &outputschema.StageReceiptEngine{Name: adm.Engine, Version: adm.WorkflowVersion},
		RemoteRunID:      "run-1",
		InputRevision:    adm.InputRevision,
		OutputDigest:     effects.StableDigest(parts...),
		ResultClass:      class,
		StartedAt:        "2026-09-22T00:00:00Z",
		EndedAt:          "2026-09-22T00:01:00Z",
		Provenance:       &proof.Provenance{Query: "fake:run-1"},
		Artifacts:        artifacts,
	}
	if mutate != nil {
		mutate(receipt)
	}
	report := outputschema.AgentReport{
		Lane: "fake", Kind: outputschema.KindStageReceipt,
		Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
		Summary: "fixture receipt", Receipt: receipt,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func completedArtifacts() []outputschema.Artifact {
	return []outputschema.Artifact{{Repo: "hivecommons/hive", Path: "report.md", Description: "report"}}
}

func refFor(raw []byte, path string) ReceiptRef {
	return ReceiptRef{Path: path, Digest: RequestDigest(raw), Size: int64(len(raw))}
}

// fakeAdapter is an in-process engine with keyed admission semantics.
type fakeAdapter struct {
	starts     int
	runs       map[ExecutionKey]fakeRun
	artifacts  map[string][]byte
	observeErr error
	startErr   error
	cancel     CancelFacts
	cancelErr  error
	state      State
	receipt    *ReceiptRef
	incarn     string
	pinErr     error
	openErr    error
}

type fakeRun struct {
	digest string
	id     string
}

func newFake() *fakeAdapter {
	return &fakeAdapter{runs: map[ExecutionKey]fakeRun{}, artifacts: map[string][]byte{}, state: StateRunning, incarn: "inc-1"}
}

func (f *fakeAdapter) Engine() string { return testEngine }

func (f *fakeAdapter) Incarnation(context.Context) (string, error) {
	if f.pinErr != nil {
		return "", f.pinErr
	}
	return f.incarn, nil
}

func (f *fakeAdapter) Start(_ context.Context, req StartRequest) (StartResult, error) {
	if f.startErr != nil {
		return StartResult{}, f.startErr
	}
	key := req.Admission.ExecutionKey()
	digest := RequestDigest(req.Payload)
	if run, ok := f.runs[key]; ok {
		if run.digest != digest {
			return StartResult{}, ErrConflict
		}
		return StartResult{RemoteRunID: run.id, RemoteIncarnation: f.incarn, Deduplicated: true}, nil
	}
	f.starts++
	f.runs[key] = fakeRun{digest: digest, id: "run-1"}
	return StartResult{RemoteRunID: "run-1", RemoteIncarnation: f.incarn}, nil
}

func (f *fakeAdapter) Observe(_ context.Context, key ExecutionKey, incarnation string) (Observation, error) {
	if f.observeErr != nil {
		return Observation{}, f.observeErr
	}
	if incarnation != "" && incarnation != f.incarn {
		return Observation{}, ErrIncarnationMismatch
	}
	run, ok := f.runs[key]
	if !ok {
		return Observation{}, ErrNotFound
	}
	return Observation{State: f.state, RemoteRunID: run.id, RemoteIncarnation: f.incarn, Stage: "report", Receipt: f.receipt}, nil
}

func (f *fakeAdapter) Cancel(context.Context, ExecutionKey, string) (CancelFacts, error) {
	return f.cancel, f.cancelErr
}

func (f *fakeAdapter) OpenArtifact(_ context.Context, _ ExecutionKey, _ string, path string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	raw, ok := f.artifacts[path]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func TestValidStateAndMode(t *testing.T) {
	for _, s := range []State{StateAccepted, StateRunning, StateWaiting, StateTerminal, StateUnknown} {
		if !ValidState(s) {
			t.Errorf("%s should be valid", s)
		}
	}
	if ValidState("done") {
		t.Error("done must not be a valid state")
	}
	for _, m := range []string{ModeOff, ModeShadow, ModeReportOnly} {
		if !ValidMode(m) {
			t.Errorf("%s should be valid", m)
		}
	}
	if ValidMode("enforce") {
		t.Error("enforce must not be a valid mode")
	}
}

func TestAdmissionValidateAndKey(t *testing.T) {
	adm := testAdmission()
	if err := adm.Validate(); err != nil {
		t.Fatalf("valid admission rejected: %v", err)
	}
	cases := map[string]func(*Admission){
		"work_key":             func(a *Admission) { a.WorkKey = "" },
		"assignment_id":        func(a *Admission) { a.AssignmentID = " " },
		"stage":                func(a *Admission) { a.Stage = "" },
		"contract_revision":    func(a *Admission) { a.ContractRevision = "" },
		"engine":               func(a *Admission) { a.Engine = "" },
		"workflow_version":     func(a *Admission) { a.WorkflowVersion = "" },
		"input_revision":       func(a *Admission) { a.InputRevision = "" },
		"request_digest":       func(a *Admission) { a.RequestDigest = "" },
		"authority.identity":   func(a *Admission) { a.Authority.Identity = "" },
		"authority.capability": func(a *Admission) { a.Authority.Capability = "" },
		"generation":           func(a *Admission) { a.Generation = 0 },
		"authority.mode":       func(a *Admission) { a.Authority.Mode = "enforce" },
	}
	for name, mutate := range cases {
		bad := adm
		mutate(&bad)
		err := bad.Validate()
		if !errors.Is(err, ErrInvalidAdmission) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Key ignores the request digest and the authority, and moves with every identity.
	same := adm
	same.RequestDigest = "other"
	same.Authority.Identity = "relay-b"
	if same.ExecutionKey() != adm.ExecutionKey() {
		t.Error("execution key must not depend on payload digest or authority")
	}
	moved := adm
	moved.Generation++
	if moved.ExecutionKey() == adm.ExecutionKey() {
		t.Error("execution key must change with generation")
	}
	if RequestDigest([]byte("a")) == RequestDigest([]byte("b")) {
		t.Error("digest collision")
	}
}

func TestMemoryStore(t *testing.T) {
	m := NewMemoryStore()
	adm := testAdmission()
	if _, ok, _ := m.Load(adm.AssignmentID); ok {
		t.Fatal("empty store reported an admission")
	}
	if err := m.Persist(adm); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.Load(adm.AssignmentID)
	if err != nil || !ok || got.ExecutionKey() != adm.ExecutionKey() {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := m.LoadReceipt("x"); ok {
		t.Fatal("empty receipt store reported a receipt")
	}
	if err := m.SaveReceipt("x", []byte("r")); err != nil {
		t.Fatal(err)
	}
	if raw, ok, _ := m.LoadReceipt("x"); !ok || string(raw) != "r" {
		t.Fatalf("LoadReceipt = %q %v", raw, ok)
	}
	m.FailPersist = ErrStoreUnavailable
	if err := m.Persist(adm); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Persist with FailPersist = %v", err)
	}
}

func TestCleanArtifactPath(t *testing.T) {
	good := []string{"receipt.json", "out/report.md", "a/b/c.txt"}
	for _, p := range good {
		if _, err := CleanArtifactPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	bad := []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "./a", "a\\b", "a\x00b", "http://x/y", strings.Repeat("a", outputschema.MaxArtifactPathLength+1)}
	for _, p := range bad {
		if _, err := CleanArtifactPath(p); !errors.Is(err, ErrReceiptPath) {
			t.Errorf("%q accepted (err=%v)", p, err)
		}
	}
}

func TestReadVerified(t *testing.T) {
	raw := []byte("hello")
	ref := refFor(raw, "r")
	if got, err := readVerified(bytes.NewReader(raw), ref, FetchLimits{}); err != nil || string(got) != "hello" {
		t.Fatalf("readVerified = %q %v", got, err)
	}
	if _, err := readVerified(bytes.NewReader(raw), ReceiptRef{Size: 0, Digest: ref.Digest}, FetchLimits{}); !errors.Is(err, ErrReceiptTooLarge) {
		t.Errorf("zero size: %v", err)
	}
	if _, err := readVerified(bytes.NewReader(raw), ReceiptRef{Size: 5, Digest: ref.Digest}, FetchLimits{MaxBytes: 3}); !errors.Is(err, ErrReceiptTooLarge) {
		t.Errorf("over limit by declared size: %v", err)
	}
	if _, err := readVerified(bytes.NewReader([]byte("hello world")), ReceiptRef{Size: 3, Digest: ref.Digest}, FetchLimits{MaxBytes: 4}); !errors.Is(err, ErrReceiptTooLarge) {
		t.Errorf("over limit by actual bytes: %v", err)
	}
	if _, err := readVerified(bytes.NewReader([]byte("hel")), ref, FetchLimits{}); !errors.Is(err, ErrReceiptTruncated) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := readVerified(bytes.NewReader([]byte("jello")), ref, FetchLimits{}); !errors.Is(err, ErrReceiptDigest) {
		t.Errorf("wrong digest: %v", err)
	}
	if _, err := readVerified(errReader{}, ref, FetchLimits{}); !errors.Is(err, ErrTransport) {
		t.Errorf("read error: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestParseAndBindReceipt(t *testing.T) {
	adm := testAdmission()
	raw := receiptJSON(t, adm, outputschema.ReceiptResultCompleted, completedArtifacts(), nil)
	receipt, err := ParseReceipt(raw)
	if err != nil {
		t.Fatalf("ParseReceipt: %v", err)
	}
	if err := BindReceipt(adm, receipt); err != nil {
		t.Fatalf("BindReceipt: %v", err)
	}
	if _, err := ParseReceipt([]byte("{")); !errors.Is(err, ErrReceiptSchema) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := ParseReceipt([]byte(`{"lane":"x","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"s"}`)); !errors.Is(err, ErrReceiptSchema) {
		t.Errorf("wrong kind: %v", err)
	}
	if err := BindReceipt(adm, nil); !errors.Is(err, ErrReceiptUnbound) {
		t.Errorf("nil receipt: %v", err)
	}
	mutations := map[string]func(*outputschema.StageReceipt){
		"work_key":          func(r *outputschema.StageReceipt) { r.WorkKey = "other" },
		"assignment_id":     func(r *outputschema.StageReceipt) { r.AssignmentID = "other" },
		"generation":        func(r *outputschema.StageReceipt) { r.Generation = 99 },
		"stage":             func(r *outputschema.StageReceipt) { r.Stage = "plan" },
		"contract_revision": func(r *outputschema.StageReceipt) { r.ContractRevision = "v0" },
		"execution_key":     func(r *outputschema.StageReceipt) { r.ExecutionKey = "deadbeef" },
		"engine": func(r *outputschema.StageReceipt) {
			r.Engine = &outputschema.StageReceiptEngine{Name: "other", Version: testVersion}
		},
		"engine.version": func(r *outputschema.StageReceipt) {
			r.Engine = &outputschema.StageReceiptEngine{Name: testEngine, Version: "v9"}
		},
		"input_revision": func(r *outputschema.StageReceipt) { r.InputRevision = strings.Repeat("f", 40) },
	}
	for name, mutate := range mutations {
		r, err := ParseReceipt(receiptJSON(t, adm, outputschema.ReceiptResultCompleted, completedArtifacts(), mutate))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if err := BindReceipt(adm, r); !errors.Is(err, ErrReceiptUnbound) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: BindReceipt = %v", name, err)
		}
	}
}

func TestFetchReceiptRefusals(t *testing.T) {
	adm := testAdmission()
	f := newFake()
	raw := receiptJSON(t, adm, outputschema.ReceiptResultCompleted, completedArtifacts(), nil)
	f.artifacts["receipt.json"] = raw
	ctx := context.Background()
	if _, _, err := FetchReceipt(ctx, f, adm, "", ReceiptRef{Path: "../receipt.json", Digest: "x", Size: 1}, FetchLimits{}); !errors.Is(err, ErrReceiptPath) {
		t.Errorf("malicious path: %v", err)
	}
	if _, _, err := FetchReceipt(ctx, f, adm, "", ReceiptRef{Path: "receipt.json", Digest: "x", Size: DefaultMaxReceiptBytes + 1}, FetchLimits{}); !errors.Is(err, ErrReceiptTooLarge) {
		t.Errorf("declared too large: %v", err)
	}
	if _, _, err := FetchReceipt(ctx, f, adm, "", refFor(raw, "missing.json"), FetchLimits{}); !errors.Is(err, ErrReceiptMissing) {
		t.Errorf("missing: %v", err)
	}
	wrong := refFor(raw, "receipt.json")
	wrong.Digest = strings.Repeat("0", 64)
	if _, _, err := FetchReceipt(ctx, f, adm, "", wrong, FetchLimits{}); !errors.Is(err, ErrReceiptDigest) {
		t.Errorf("wrong digest: %v", err)
	}
	short := refFor(raw, "receipt.json")
	short.Size--
	if _, _, err := FetchReceipt(ctx, f, adm, "", short, FetchLimits{}); !errors.Is(err, ErrReceiptTruncated) {
		t.Errorf("truncated: %v", err)
	}
	f.artifacts["bad.json"] = []byte(`{"lane":"x"}`)
	if _, _, err := FetchReceipt(ctx, f, adm, "", refFor(f.artifacts["bad.json"], "bad.json"), FetchLimits{}); !errors.Is(err, ErrReceiptSchema) {
		t.Errorf("schema: %v", err)
	}
	other := adm
	other.ContractRevision = "contract-2"
	if _, _, err := FetchReceipt(ctx, f, other, "", refFor(raw, "receipt.json"), FetchLimits{}); !errors.Is(err, ErrReceiptUnbound) {
		t.Errorf("unbound: %v", err)
	}
	f.openErr = ErrTransport
	if _, _, err := FetchReceipt(ctx, f, adm, "", refFor(raw, "receipt.json"), FetchLimits{}); !errors.Is(err, ErrTransport) {
		t.Errorf("open transport error: %v", err)
	}
	f.openErr = nil
	receipt, got, err := FetchReceipt(ctx, f, adm, "", refFor(raw, "receipt.json"), FetchLimits{})
	if err != nil || receipt.RemoteRunID != "run-1" || !bytes.Equal(got, raw) {
		t.Fatalf("FetchReceipt = %+v %v", receipt, err)
	}
}

func TestDecide(t *testing.T) {
	adm := testAdmission()
	parse := func(class outputschema.StageReceiptResultClass, arts []outputschema.Artifact) *outputschema.StageReceipt {
		r, err := ParseReceipt(receiptJSON(t, adm, class, arts, nil))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if d := Decide(adm, nil, nil, true); d.Verdict != VerdictUncertain || d.ExecutionFact {
		t.Errorf("nil receipt: %+v", d)
	}
	completed := parse(outputschema.ReceiptResultCompleted, completedArtifacts())
	if d := Decide(adm, completed, nil, true); d.Verdict != VerdictAccepted || !d.ExecutionFact {
		t.Errorf("completed: %+v", d)
	}
	if d := Decide(adm, completed, nil, false); d.Verdict != VerdictRejected || !d.ExecutionFact {
		t.Errorf("revoked authority: %+v", d)
	}
	reject := func(Admission, *outputschema.StageReceipt) error { return errors.New("verifier says no") }
	if d := Decide(adm, completed, reject, true); d.Verdict != VerdictRejected || !strings.Contains(d.Reason, "verifier says no") || !d.ExecutionFact {
		t.Errorf("predicate reject: %+v", d)
	}
	other := adm
	other.Stage = "plan"
	if d := Decide(other, completed, nil, true); d.Verdict != VerdictRejected || !d.ExecutionFact {
		t.Errorf("unbound: %+v", d)
	}
	if d := Decide(adm, parse(outputschema.ReceiptResultNoChange, nil), nil, true); d.Verdict != VerdictNoChange {
		t.Errorf("no_change: %+v", d)
	}
	if d := Decide(adm, parse(outputschema.ReceiptResultBlocked, nil), nil, true); d.Verdict != VerdictBlocked {
		t.Errorf("blocked: %+v", d)
	}
	if d := Decide(adm, parse(outputschema.ReceiptResultFailed, nil), nil, true); d.Verdict != VerdictRejected {
		t.Errorf("failed: %+v", d)
	}
	if d := Decide(adm, parse(outputschema.ReceiptResultUnknown, nil), nil, true); d.Verdict != VerdictUncertain {
		t.Errorf("unknown: %+v", d)
	}
	weird := parse(outputschema.ReceiptResultCompleted, completedArtifacts())
	weird.ResultClass = "partial"
	if d := Decide(adm, weird, nil, true); d.Verdict != VerdictRejected {
		t.Errorf("unknown class: %+v", d)
	}
}

func TestHosts(t *testing.T) {
	ctx := context.Background()
	if d, _, err := AutoAcceptHost.Decide(ctx, Offer{}); d != OfferAccepted || err != nil {
		t.Fatalf("auto accept = %s %v", d, err)
	}
	h := NewInteractiveHost()
	offer := Offer{ExecutionKey: "k1"}
	// Answer before Decide is honoured.
	h.Answer("k1", OfferDeclined, "busy")
	if d, reason, err := h.Decide(ctx, offer); d != OfferDeclined || reason != "busy" || err != nil {
		t.Fatalf("pre-answered = %s %q %v", d, reason, err)
	}
	// Decide before Answer parks the offer and reports it pending.
	done := make(chan OfferDecision, 1)
	go func() {
		d, _, _ := h.Decide(ctx, Offer{ExecutionKey: "k2"})
		done <- d
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(h.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.Pending(); len(got) != 1 || got[0] != "k2" {
		t.Fatalf("Pending = %v", got)
	}
	h.Answer("k2", OfferAccepted, "ok")
	h.Answer("k2", OfferDeclined, "dropped second answer")
	if d := <-done; d != OfferAccepted {
		t.Fatalf("answered decide = %s", d)
	}
	// Context end declines without an answer.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if d, _, err := h.Decide(cctx, Offer{ExecutionKey: "k3"}); d != OfferDeclined || !errors.Is(err, ErrOfferPending) {
		t.Fatalf("cancelled decide = %s %v", d, err)
	}
	sink := &MemorySink{}
	sink.Record(ProgressEvent{Action: "a"})
	if got := sink.Actions(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("Actions = %v", got)
	}
	var called bool
	ProgressSinkFunc(func(ProgressEvent) { called = true }).Record(ProgressEvent{})
	if !called {
		t.Fatal("ProgressSinkFunc not called")
	}
}

func TestBindingOffer(t *testing.T) {
	adm := testAdmission()
	sink := &MemorySink{}
	off := New(newFake(), NewMemoryStore(), nil, sink, "bogus")
	if off.Mode() != ModeOff {
		t.Fatalf("unknown mode resolved to %q, want off", off.Mode())
	}
	if _, err := off.Offer(context.Background(), nil, adm, "s"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off Offer = %v", err)
	}
	b := New(newFake(), NewMemoryStore(), nil, sink, ModeReportOnly)
	bad := adm
	bad.WorkKey = ""
	if _, err := b.Offer(context.Background(), nil, bad, "s"); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("invalid Offer = %v", err)
	}
	if d, err := b.Offer(context.Background(), nil, adm, "s"); d != OfferAccepted || err != nil {
		t.Fatalf("auto Offer = %s %v", d, err)
	}
	var seen Offer
	decline := HostFunc(func(_ context.Context, o Offer) (OfferDecision, string, error) {
		seen = o
		return OfferDeclined, "no capacity", nil
	})
	if d, err := b.Offer(context.Background(), decline, adm, "only the summary"); d != OfferDeclined || !errors.Is(err, ErrDeclined) {
		t.Fatalf("declined Offer = %s %v", d, err)
	}
	if seen.Summary != "only the summary" || seen.ExecutionKey != adm.ExecutionKey() || seen.AssignmentID != adm.AssignmentID {
		t.Fatalf("offer carried %+v", seen)
	}
	failing := HostFunc(func(context.Context, Offer) (OfferDecision, string, error) { return "", "", errors.New("link down") })
	if _, err := b.Offer(context.Background(), failing, adm, "s"); !errors.Is(err, ErrDeclined) {
		t.Fatalf("erroring host = %v", err)
	}
	want := []string{EventOfferAccepted, EventOfferDeclined, EventOfferDeclined}
	if got := sink.Actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", got)
	}
}

func TestBindingDispatchAndShadow(t *testing.T) {
	adm := testAdmission()
	ctx := context.Background()
	f := newFake()
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeOff).Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off Dispatch = %v", err)
	}
	sink := &MemorySink{}
	shadow := New(f, NewMemoryStore(), nil, sink, ModeShadow)
	res, err := shadow.Dispatch(ctx, adm, testPayload())
	if err != nil || !res.Shadow || res.Started {
		t.Fatalf("shadow Dispatch = %+v %v", res, err)
	}
	if f.starts != 0 {
		t.Fatalf("shadow mode performed %d external starts", f.starts)
	}
	if _, err := shadow.Recover(ctx, adm, testPayload()); err != nil || f.starts != 0 {
		t.Fatalf("shadow Recover started work: %d %v", f.starts, err)
	}
	if got := sink.Actions(); !contains(got, EventShadowObserved) || contains(got, EventStarted) {
		t.Fatalf("shadow events = %v", got)
	}

	store := NewMemoryStore()
	b := New(f, store, nil, sink, ModeReportOnly)
	bad := adm
	bad.Generation = 0
	if _, err := b.Dispatch(ctx, bad, testPayload()); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("invalid Dispatch = %v", err)
	}
	if _, err := b.Dispatch(ctx, adm, []byte("tampered")); !errors.Is(err, ErrPayloadDigest) {
		t.Fatalf("digest mismatch = %v", err)
	}
	store.FailPersist = ErrStoreUnavailable
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("persist failure = %v", err)
	}
	if f.starts != 0 {
		t.Fatal("a failed persist must not dispatch")
	}
	store.FailPersist = nil
	f.pinErr = ErrTransport
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrTransport) || f.starts != 0 {
		t.Fatalf("pin failure = %v starts=%d", err, f.starts)
	}
	f.pinErr = nil
	res, err = b.Dispatch(ctx, adm, testPayload())
	if err != nil || !res.Started || res.Run.RemoteRunID != "run-1" || res.Run.Deduplicated {
		t.Fatalf("Dispatch = %+v %v", res, err)
	}
	if pinned, ok, _ := b.Admission(adm.AssignmentID); !ok || pinned.EngineIncarnation != "inc-1" {
		t.Fatalf("persisted admission did not pin the incarnation: %+v %v", pinned, ok)
	}
	res, err = b.Dispatch(ctx, adm, testPayload())
	if err != nil || !res.Run.Deduplicated || f.starts != 1 {
		t.Fatalf("second Dispatch = %+v %v starts=%d", res, err, f.starts)
	}
	changed := adm
	changed.RequestDigest = RequestDigest([]byte("different"))
	if _, err := b.Dispatch(ctx, changed, []byte("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload = %v", err)
	}
	if stored, _, _ := store.Load(adm.AssignmentID); stored.RequestDigest != adm.RequestDigest || stored.RemoteRunID != "run-1" {
		t.Fatalf("a conflicting dispatch touched the durable admission: %+v", stored)
	}
	// The engine's own conflict is still mapped when the record is absent.
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeReportOnly).Dispatch(ctx, changed, []byte("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("engine conflict = %v", err)
	}
	// A new generation is a new execution identity and may replace the record.
	next := adm
	next.Generation++
	if res, err := b.Dispatch(ctx, next, testPayload()); err != nil || !res.Started {
		t.Fatalf("next generation = %+v %v", res, err)
	}
	if _, err := New(f, &failingLoadStore{}, nil, nil, ModeReportOnly).Dispatch(ctx, adm, testPayload()); err == nil || !strings.Contains(err.Error(), "load admission") {
		t.Fatalf("load failure on dispatch = %v", err)
	}
	f.startErr = ErrRefused
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused = %v", err)
	}
}

func TestBindingHooksAndRecover(t *testing.T) {
	adm := testAdmission()
	ctx := context.Background()
	crash := errors.New("process died")

	// Window 1: died after persist, before start.
	f := newFake()
	store := NewMemoryStore()
	b := New(f, store, nil, nil, ModeReportOnly, WithHooks(Hooks{AfterPersist: func() error { return crash }}))
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, crash) {
		t.Fatalf("crash not surfaced: %v", err)
	}
	sink := &MemorySink{}
	b2 := New(f, store, nil, sink, ModeReportOnly)
	rec, err := b2.Recover(ctx, adm, testPayload())
	if err != nil || !rec.Started || rec.Adopted || f.starts != 1 {
		t.Fatalf("recover before start = %+v %v starts=%d", rec, err, f.starts)
	}

	// Window 2: died after remote accept, before anything else was persisted.
	f = newFake()
	store = NewMemoryStore()
	b = New(f, store, nil, nil, ModeReportOnly, WithHooks(Hooks{AfterStart: func() error { return crash }}))
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, crash) {
		t.Fatalf("crash not surfaced: %v", err)
	}
	rec, err = New(f, store, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload())
	if err != nil || !rec.Adopted || rec.Started || f.starts != 1 || rec.Run.RemoteRunID != "run-1" {
		t.Fatalf("recover after accept = %+v %v starts=%d", rec, err, f.starts)
	}

	// Window 3: died after the receipt was persisted; replay, no start.
	receipts := NewMemoryStore()
	raw := receiptJSON(t, adm, outputschema.ReceiptResultCompleted, completedArtifacts(), nil)
	_ = receipts.SaveReceipt(adm.AssignmentID, raw)
	f = newFake()
	rec, err = New(f, store, receipts, nil, ModeReportOnly).Recover(ctx, adm, testPayload())
	if err != nil || !rec.Adopted || rec.Observation.State != StateTerminal || f.starts != 0 {
		t.Fatalf("recover after receipt = %+v %v starts=%d", rec, err, f.starts)
	}

	// No durable admission: no authority is minted.
	f = newFake()
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload()); !errors.Is(err, ErrNoDurableAdmission) || f.starts != 0 {
		t.Fatalf("recover without admission = %v starts=%d", err, f.starts)
	}
	other := adm
	other.RequestDigest = "changed"
	if _, err := New(f, store, nil, nil, ModeReportOnly).Recover(ctx, other, testPayload()); !errors.Is(err, ErrNoDurableAdmission) {
		t.Fatalf("recover with changed digest = %v", err)
	}
	if _, err := New(f, store, nil, nil, ModeReportOnly).Recover(ctx, adm, []byte("tampered")); !errors.Is(err, ErrPayloadDigest) {
		t.Fatalf("recover with tampered payload = %v", err)
	}
	// Recreated engine, record says a native run was accepted: never adopt
	// what the new instance holds, never start again; uncertain.
	recreated := newFake()
	recStore := NewMemoryStore()
	if _, err := New(recreated, recStore, nil, nil, ModeReportOnly).Dispatch(ctx, adm, testPayload()); err != nil {
		t.Fatal(err)
	}
	if stored, _, _ := recStore.Load(adm.AssignmentID); stored.RemoteRunID != "run-1" || stored.EngineIncarnation != "inc-1" {
		t.Fatalf("record after start = %+v", stored)
	}
	recreated.incarn = "inc-2"
	recreated.runs = map[ExecutionKey]fakeRun{}
	rec, err = New(recreated, recStore, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload())
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, ErrIncarnationMismatch) || !rec.Uncertain || rec.Adopted || rec.Started || recreated.starts != 1 {
		t.Fatalf("recreated engine, recorded run: recover = %+v %v starts=%d", rec, err, recreated.starts)
	}
	// Recreated engine, record says no native run was recorded (death after
	// remote accept, before the record was updated): nothing is outstanding on
	// the live instance, so re-pin to it and start there.
	unstarted := newFake()
	unStore := NewMemoryStore()
	if _, err := New(unstarted, unStore, nil, nil, ModeReportOnly, WithHooks(Hooks{AfterStart: func() error { return crash }})).Dispatch(ctx, adm, testPayload()); !errors.Is(err, crash) {
		t.Fatalf("crash not surfaced: %v", err)
	}
	if stored, _, _ := unStore.Load(adm.AssignmentID); stored.RemoteRunID != "" {
		t.Fatalf("record must not know the run yet: %+v", stored)
	}
	unstarted.incarn = "inc-2"
	unstarted.runs = map[ExecutionKey]fakeRun{}
	rec, err = New(unstarted, unStore, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload())
	if err != nil || !rec.Started || rec.Adopted || rec.Uncertain || unstarted.starts != 2 || rec.Run.RemoteIncarnation != "inc-2" {
		t.Fatalf("recreated engine, no recorded run: recover = %+v %v starts=%d", rec, err, unstarted.starts)
	}
	if stored, _, _ := unStore.Load(adm.AssignmentID); stored.EngineIncarnation != "inc-2" || stored.RemoteRunID != "run-1" {
		t.Fatalf("record not re-pinned to the live engine: %+v", stored)
	}
	// Same situation in shadow mode starts nothing.
	shadowFake := newFake()
	shadowStore := NewMemoryStore()
	_ = shadowStore.Persist(func() Admission { a := adm; a.EngineIncarnation = "inc-0"; return a }())
	if rec, err := New(shadowFake, shadowStore, nil, nil, ModeShadow).Recover(ctx, adm, testPayload()); err != nil || rec.Started || shadowFake.starts != 0 {
		t.Fatalf("shadow recreated recover = %+v %v starts=%d", rec, err, shadowFake.starts)
	}
	// Tampered payload and a pin failure on the live engine both refuse.
	tampered := newFake()
	tamperedStore := NewMemoryStore()
	_ = tamperedStore.Persist(func() Admission { a := adm; a.EngineIncarnation = "inc-0"; return a }())
	if _, err := New(tampered, tamperedStore, nil, nil, ModeReportOnly).Recover(ctx, adm, []byte("tampered")); !errors.Is(err, ErrPayloadDigest) {
		t.Fatalf("recreated recover with tampered payload = %v", err)
	}
	tampered.pinErr = ErrTransport
	if _, err := New(tampered, tamperedStore, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload()); !errors.Is(err, ErrTransport) || tampered.starts != 0 {
		t.Fatalf("recreated recover with pin failure = %v starts=%d", err, tampered.starts)
	}
	tampered.pinErr = nil
	tamperedStore.FailPersist = ErrStoreUnavailable
	if _, err := New(tampered, tamperedStore, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload()); !errors.Is(err, ErrStoreUnavailable) || tampered.starts != 0 {
		t.Fatalf("recreated recover with re-pin persist failure = %v starts=%d", err, tampered.starts)
	}
	// Transport failure: uncertain, nothing started.
	f.observeErr = ErrTransport
	rec, err = New(f, store, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload())
	if !errors.Is(err, ErrUncertain) || !rec.Uncertain || f.starts != 0 {
		t.Fatalf("uncertain recover = %+v %v starts=%d", rec, err, f.starts)
	}
	if _, err := New(f, store, nil, nil, ModeOff).Recover(ctx, adm, testPayload()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off recover = %v", err)
	}
	failStore := &failingLoadStore{}
	if _, err := New(f, failStore, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload()); err == nil || !strings.Contains(err.Error(), "load admission") {
		t.Fatalf("load failure = %v", err)
	}
	f.startErr = ErrRefused
	f.observeErr = nil
	if _, err := New(f, store, nil, nil, ModeReportOnly).Recover(ctx, adm, testPayload()); !errors.Is(err, ErrRefused) {
		t.Fatalf("recover start refused = %v", err)
	}
}

type failingLoadStore struct{}

func (failingLoadStore) Persist(Admission) error { return nil }
func (failingLoadStore) Load(string) (Admission, bool, error) {
	return Admission{}, false, errors.New("disk gone")
}

func TestBindingObserveCancelReceiptReplay(t *testing.T) {
	adm := testAdmission()
	ctx := context.Background()
	f := newFake()
	sink := &MemorySink{}
	fixed := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	b := New(f, NewMemoryStore(), nil, sink, ModeReportOnly, WithClock(func() time.Time { return fixed }), WithLimits(FetchLimits{MaxBytes: DefaultMaxReceiptBytes}))
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeOff).Observe(ctx, adm, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off Observe = %v", err)
	}
	if _, err := b.Observe(ctx, adm, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("observe before start = %v", err)
	}
	if _, err := b.Dispatch(ctx, adm, testPayload()); err != nil {
		t.Fatal(err)
	}
	obs, err := b.Observe(ctx, adm, "inc-1")
	if err != nil || obs.State != StateRunning || !obs.ObservedAt.Equal(fixed) {
		t.Fatalf("Observe = %+v %v", obs, err)
	}
	before := len(sink.Events())
	if _, err := b.Observe(ctx, adm, "inc-1"); err != nil || len(sink.Events()) != before {
		t.Fatalf("unchanged state must not record a new progress event (%d -> %d)", before, len(sink.Events()))
	}
	if _, err := b.Observe(ctx, adm, "inc-2"); !errors.Is(err, ErrIncarnationMismatch) {
		t.Fatalf("recreated engine = %v", err)
	}
	f.state = "exotic"
	obs, _ = b.Observe(ctx, adm, "inc-1")
	if obs.State != StateUnknown {
		t.Fatalf("invalid state must map to unknown, got %s", obs.State)
	}
	f.state = StateWaiting
	obs, _ = b.Observe(ctx, adm, "inc-1")
	if obs.State != StateWaiting {
		t.Fatalf("waiting = %s", obs.State)
	}
	f.observeErr = ErrTransport
	obs, err = b.Observe(ctx, adm, "inc-1")
	if !errors.Is(err, ErrTransport) || obs.State != StateUnknown {
		t.Fatalf("transport = %+v %v", obs, err)
	}
	f.observeErr = nil

	// Cancel facts pass through untouched.
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeOff).Cancel(ctx, adm, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off Cancel = %v", err)
	}
	f.cancel = CancelFacts{Requested: true, Acknowledged: true, Stopped: false, Detail: "workload ignored abort"}
	facts, err := b.Cancel(ctx, adm, "inc-1")
	if err != nil || facts.Stopped || !facts.Requested {
		t.Fatalf("Cancel = %+v %v", facts, err)
	}
	f.cancelErr = ErrTransport
	if _, err := b.Cancel(ctx, adm, "inc-1"); !errors.Is(err, ErrTransport) {
		t.Fatalf("Cancel transport = %v", err)
	}

	// Receipt: refusal is recorded; success is stored and replayable.
	if _, err := New(f, NewMemoryStore(), nil, nil, ModeOff).FetchReceipt(ctx, adm, "", ReceiptRef{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off FetchReceipt = %v", err)
	}
	if _, ok, err := b.Replay(adm); ok || err != nil {
		t.Fatalf("replay before receipt = %v %v", ok, err)
	}
	raw := receiptJSON(t, adm, outputschema.ReceiptResultCompleted, completedArtifacts(), nil)
	f.artifacts["receipt.json"] = raw
	if _, err := b.FetchReceipt(ctx, adm, "inc-1", ReceiptRef{Path: "receipt.json", Digest: "bad", Size: int64(len(raw))}); !errors.Is(err, ErrReceiptDigest) {
		t.Fatalf("bad digest = %v", err)
	}
	receipt, err := b.FetchReceipt(ctx, adm, "inc-1", refFor(raw, "receipt.json"))
	if err != nil || receipt.ResultClass != outputschema.ReceiptResultCompleted {
		t.Fatalf("FetchReceipt = %+v %v", receipt, err)
	}
	replayed, ok, err := b.Replay(adm)
	if err != nil || !ok || replayed.ExecutionKey != string(adm.ExecutionKey()) {
		t.Fatalf("Replay = %+v %v %v", replayed, ok, err)
	}
	if d := b.Decide(adm, replayed, nil, true); d.Verdict != VerdictAccepted {
		t.Fatalf("Decide = %+v", d)
	}
	other := adm
	other.Stage = "plan"
	if _, ok, err := b.Replay(other); !ok || !errors.Is(err, ErrReceiptUnbound) {
		t.Fatalf("replay for another subject = %v %v", ok, err)
	}
	corrupt := NewMemoryStore()
	_ = corrupt.SaveReceipt(adm.AssignmentID, []byte("{"))
	if _, ok, err := New(f, NewMemoryStore(), corrupt, nil, ModeReportOnly).Replay(adm); !ok || !errors.Is(err, ErrReceiptSchema) {
		t.Fatalf("corrupt replay = %v %v", ok, err)
	}
	saveFail := New(f, NewMemoryStore(), failingReceiptStore{}, nil, ModeReportOnly)
	if _, err := saveFail.FetchReceipt(ctx, adm, "inc-1", refFor(raw, "receipt.json")); err == nil || !strings.Contains(err.Error(), "persist receipt") {
		t.Fatalf("save failure = %v", err)
	}
	actions := sink.Actions()
	for _, want := range []string{EventAdmissionPersisted, EventStarted, EventProgress, EventCancelRequested, EventReceiptRefused, EventReceiptVerified, EventDecision} {
		if !contains(actions, want) {
			t.Errorf("missing event %s in %v", want, actions)
		}
	}
	if len(sink.Events()) > 0 && sink.Events()[0].ExecutionKey != adm.ExecutionKey() {
		t.Error("events must carry the execution key")
	}
}

type failingReceiptStore struct{}

func (failingReceiptStore) SaveReceipt(string, []byte) error         { return errors.New("disk full") }
func (failingReceiptStore) LoadReceipt(string) ([]byte, bool, error) { return nil, false, nil }

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if r.Linked("fake") || len(r.Engines()) != 0 {
		t.Fatal("empty registry reports engines")
	}
	if _, err := r.Open("fake", nil); !errors.Is(err, ErrEngineNotLinked) {
		t.Fatalf("Open unlinked = %v", err)
	}
	r.Register("fake", func(map[string]string) (Adapter, error) { return newFake(), nil })
	if !r.Linked("fake") || strings.Join(r.Engines(), ",") != "fake" {
		t.Fatalf("Linked/Engines = %v %v", r.Linked("fake"), r.Engines())
	}
	if a, err := r.Open("fake", nil); err != nil || a.Engine() != testEngine {
		t.Fatalf("Open = %v %v", a, err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	r.Register("fake", nil)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// gatedAdapter refuses every admission whose stage is "publish" before any
// offer or persist, the seam an unconfined host uses.
type gatedAdapter struct {
	*fakeAdapter
	offers int
}

var errGateRefused = errors.New("gate: stage may not be bound to this host")

func (g *gatedAdapter) Admit(adm Admission) error {
	if adm.Stage == "publish" {
		return errGateRefused
	}
	return nil
}

func TestBindingGateRefusesBeforeOfferAndPersist(t *testing.T) {
	ctx := context.Background()
	g := &gatedAdapter{fakeAdapter: newFake()}
	store := NewMemoryStore()
	sink := &MemorySink{}
	b := New(g, store, nil, sink, ModeReportOnly)
	adm := testAdmission()
	adm.Stage = "publish"
	host := HostFunc(func(context.Context, Offer) (OfferDecision, string, error) {
		g.offers++
		return OfferAccepted, "should never be asked", nil
	})
	if d, err := b.Offer(ctx, host, adm, "s"); d != OfferDeclined || !errors.Is(err, errGateRefused) || errors.Is(err, ErrDeclined) {
		t.Fatalf("gated Offer = %s %v (want the gate error itself, not a host decline)", d, err)
	}
	if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, errGateRefused) {
		t.Fatalf("gated Dispatch = %v", err)
	}
	if g.offers != 0 || g.starts != 0 {
		t.Fatalf("gate did not run first: offers=%d starts=%d", g.offers, g.starts)
	}
	if _, ok, _ := store.Load(adm.AssignmentID); ok {
		t.Fatal("a gated admission was persisted")
	}
	if !contains(sink.Actions(), EventOfferDeclined) {
		t.Fatalf("gate refusal not audited: %v", sink.Actions())
	}
	// Positive control: the default stage passes the gate and is offered
	// and started.
	ok := testAdmission()
	if d, err := b.Offer(ctx, host, ok, "s"); d != OfferAccepted || err != nil || g.offers != 1 {
		t.Fatalf("positive control Offer = %s %v offers=%d", d, err, g.offers)
	}
	if _, err := b.Dispatch(ctx, ok, testPayload()); err != nil || g.starts != 1 {
		t.Fatalf("positive control Dispatch = %v starts=%d", err, g.starts)
	}
}

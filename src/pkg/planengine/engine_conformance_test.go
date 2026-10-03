package planengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/planning"
)

// Conformance suite for ADR-0021 AC-4(a), (b) and (c): the lease invariants
// an engine may never observe, every row of the failure-mapping table, and a
// second registered engine surfacing on the receipt and the plan import. The
// fake engine records every call it receives, so "this never reached the
// engine" is asserted positively instead of by the absence of an effect.

const (
	conformanceEngineName   = "second-planner"
	conformanceContractRev  = "second-planner-status/v1"
	conformanceEngineVer    = "second-planner-0.1.0"
	conformanceBaselineName = "spektacular"
)

// engineCall is one engine call as the observer made it: the verb and the
// arguments the Engine interface allows, nothing else.
type engineCall struct {
	Verb     string
	Dir      string
	Kind     string
	Artifact string
}

// conformanceEngine is an in-memory Engine. Status answers from a scripted
// sequence whose last entry repeats, and every call is recorded.
type conformanceEngine struct {
	mu         sync.Mutex
	name       string
	contract   string
	version    string
	statuses   []answer
	idx        int
	resolveTo  string
	resolveErr error
	plan       *Plan
	planErr    error
	specBody   string
	specErr    error
	calls      []engineCall
}

func newConformanceEngine(name string) *conformanceEngine {
	return &conformanceEngine{name: name, contract: name + "-status/v1", version: name + "-test"}
}

func (e *conformanceEngine) Name() string             { return e.name }
func (e *conformanceEngine) ContractRevision() string { return e.contract }
func (e *conformanceEngine) EngineVersion() string    { return e.version }

func (e *conformanceEngine) Probe(context.Context) (ProbeResult, error) {
	e.record(engineCall{Verb: "Probe"})
	return ProbeResult{Present: true, Version: e.version, Binary: e.name}, nil
}

func (e *conformanceEngine) Status(_ context.Context, dir, kind, artifact string) (ArtifactStatus, error) {
	e.record(engineCall{Verb: "Status", Dir: dir, Kind: kind, Artifact: artifact})
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.statuses) == 0 {
		return ArtifactStatus{}, errors.New("no scripted status")
	}
	i := e.idx
	if i >= len(e.statuses) {
		i = len(e.statuses) - 1
	}
	e.idx++
	return e.statuses[i].status, e.statuses[i].err
}

func (e *conformanceEngine) ResolveArtifact(_ context.Context, dir, kind, slug string) (string, error) {
	e.record(engineCall{Verb: "ResolveArtifact", Dir: dir, Kind: kind, Artifact: slug})
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resolveErr != nil {
		return "", e.resolveErr
	}
	return e.resolveTo, nil
}

func (e *conformanceEngine) ExportPlan(_ context.Context, dir, artifact string) (Plan, error) {
	e.record(engineCall{Verb: "ExportPlan", Dir: dir, Kind: KindPlan, Artifact: artifact})
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.planErr != nil {
		return Plan{}, e.planErr
	}
	if e.plan == nil {
		return Plan{}, errors.New("no scripted plan")
	}
	return *e.plan, nil
}

func (e *conformanceEngine) ReadSpec(_ context.Context, dir, artifact string) (string, error) {
	e.record(engineCall{Verb: "ReadSpec", Dir: dir, Kind: KindSpec, Artifact: artifact})
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.specBody, e.specErr
}

func (e *conformanceEngine) record(call engineCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
}

func (e *conformanceEngine) recorded() []engineCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]engineCall(nil), e.calls...)
}

func (e *conformanceEngine) callCount() int { return len(e.recorded()) }

func (e *conformanceEngine) callsFor(verb string) int {
	n := 0
	for _, call := range e.recorded() {
		if call.Verb == verb {
			n++
		}
	}
	return n
}

// conformanceRegistry is an in-memory lease registry that records everything
// the observer hands it, with the same generation rules the dashboard
// applies: an advance mints a generation and moves the lease to the next
// stage.
type conformanceRegistry struct {
	mu         sync.Mutex
	stage      Stage
	advanceErr error
	advances   []Stage
	statuses   []ArtifactStatus
	receipts   []outputschema.StageReceipt
	plans      []*Plan
	refusals   []string
	progress   []map[string]string
}

func newConformanceRegistry(stage string) *conformanceRegistry {
	return &conformanceRegistry{stage: Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: stage, Identity: testIdentity,
		TaskID: testTaskID, Key: testRepo + "!" + testRunKey, Repo: testRepo,
		WorkDir: testWorkDir, Gen: 1,
	}}
}

func (f *conformanceRegistry) ActiveStages(time.Time) ([]Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []Stage{f.stage}, nil
}

func (f *conformanceRegistry) Advance(_ context.Context, st Stage, status ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.advanceErr != nil {
		return f.advanceErr
	}
	f.advances = append(f.advances, st)
	f.statuses = append(f.statuses, status)
	f.receipts = append(f.receipts, receipt)
	f.plans = append(f.plans, plan)
	f.stage.Gen++
	f.stage.Stage = nextStage(st.Stage)
	return nil
}

func (f *conformanceRegistry) Refuse(_ Stage, reason string, _ *ArtifactStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals = append(f.refusals, reason)
}

func (f *conformanceRegistry) RecordProgress(_ Stage, attrs map[string]string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(attrs))
	for k, v := range attrs {
		cp[k] = v
	}
	f.progress = append(f.progress, cp)
}

func (f *conformanceRegistry) refusalReasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refusals...)
}

func conformanceRunner(reg Registry, engine Engine) *Runner {
	return &Runner{
		Engine:   engine,
		Poll:     testPoll,
		Registry: reg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// --- AC-4(a): lease invariants -------------------------------------------

// A stage the runner must not poll never reaches the engine at all: the
// engine's call log is empty, not merely free of side effects.
func TestConformance_LeaseInvariantsNeverReachTheEngine(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(st *Stage)
		want        TickResult
		wantRefusal string
	}{
		{
			name:   "unclaimed admission lease waits for a relay",
			mutate: func(st *Stage) { st.Identity = "hive-triage"; st.WorkDir = ""; st.Unclaimed = true },
			want:   TickResult{Unclaimed: 1},
		},
		{
			name:   "relay-held lease is reported over the websocket",
			mutate: func(st *Stage) { st.RelayHeld = true },
			want:   TickResult{RelayHeld: 1},
		},
		{
			name:        "empty workdir is refused as missing_workdir",
			mutate:      func(st *Stage) { st.WorkDir = "" },
			want:        TickResult{Refused: 1},
			wantRefusal: RefuseMissingWorkDir,
		},
		{
			name:   "blank workdir is refused before the engine is called",
			mutate: func(st *Stage) { st.WorkDir = "   " },
			want:   TickResult{Refused: 1}, wantRefusal: RefuseMissingWorkDir,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newConformanceRegistry(StageSpec)
			tc.mutate(&reg.stage)
			eng := newConformanceEngine(conformanceBaselineName)
			eng.statuses = []answer{found(KindSpec, testRunKey, DocumentFinal)}

			if res := conformanceRunner(reg, eng).Tick(context.Background(), t0); res != tc.want {
				t.Fatalf("tick = %+v, want %+v", res, tc.want)
			}
			if calls := eng.recorded(); len(calls) != 0 {
				t.Fatalf("the engine was called for a stage it may not see: %+v", calls)
			}
			var wantRefusals []string
			if tc.wantRefusal != "" {
				wantRefusals = []string{tc.wantRefusal}
			}
			if got := reg.refusalReasons(); !reflect.DeepEqual(got, wantRefusals) {
				t.Fatalf("refusals = %v, want %v", got, wantRefusals)
			}
			if len(reg.advances) != 0 {
				t.Fatalf("a stage that may not be polled advanced: %+v", reg.advances)
			}
		})
	}
}

// The engine is handed the stage's work dir, the artifact kind and the
// artifact name, and nothing else: no identity, task ID, generation or
// registry handle, by the interface's shape and by what it is actually
// called with.
func TestConformance_EngineNeverSeesTheLease(t *testing.T) {
	engineType := reflect.TypeOf((*Engine)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	stringType := reflect.TypeOf("")
	for i := 0; i < engineType.NumMethod(); i++ {
		method := engineType.Method(i)
		for j := 0; j < method.Type.NumIn(); j++ {
			if in := method.Type.In(j); in != ctxType && in != stringType {
				t.Fatalf("Engine.%s takes a %s; an engine may only be handed a context and strings", method.Name, in)
			}
		}
	}

	for _, stage := range []string{StageSpec, StagePlan} {
		t.Run(stage, func(t *testing.T) {
			reg := newConformanceRegistry(stage)
			eng := newConformanceEngine(conformanceBaselineName)
			eng.statuses = []answer{found(kindOf(stage), testRunKey, DocumentFinal)}
			eng.plan = testPlan()
			eng.specBody = "# spec body"
			leaseGen := strconv.FormatUint(reg.stage.Gen, 10)

			if res := conformanceRunner(reg, eng).Tick(context.Background(), t0); res.Advanced != 1 {
				t.Fatalf("tick = %+v", res)
			}
			calls := eng.recorded()
			if len(calls) == 0 {
				t.Fatal("the engine was never called")
			}
			forbidden := []string{testIdentity, testTaskID, reg.stage.Key, leaseGen}
			for _, call := range calls {
				if call.Dir != testWorkDir {
					t.Fatalf("%s was called in %q, want the stage work dir", call.Verb, call.Dir)
				}
				if call.Artifact != testRunKey {
					t.Fatalf("%s asked for %q, want the lease artifact", call.Verb, call.Artifact)
				}
				for _, secret := range forbidden {
					if call.Kind == secret || call.Artifact == secret || call.Dir == secret {
						t.Fatalf("%s was handed lease state %q: %+v", call.Verb, secret, call)
					}
				}
			}
		})
	}
}

// kindOf is the artifact kind a stage's document has.
func kindOf(stage string) string {
	kind, _ := kindForStage(stage)
	return kind
}

// --- AC-4(b): the failure-mapping table -----------------------------------

type failureRow struct {
	name   string
	stage  string
	setup  func(reg *conformanceRegistry, eng *conformanceEngine)
	ticks  int
	want   TickResult
	refuse []string
	// parked: the stage is terminal, so a later tick neither polls nor
	// refuses again. Otherwise the failure is transient and the next poll
	// calls the engine again.
	parked bool
	check  func(t *testing.T, reg *conformanceRegistry, eng *conformanceEngine)
}

func TestConformance_FailureMappingTable(t *testing.T) {
	rows := []failureRow{
		{
			name:  "status not_found before any observation resolves and is retried",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{missing(KindSpec, testRunKey)}
				eng.resolveErr = notFound(KindSpec, testRunKey)
			},
			ticks: 1,
			want:  TickResult{Polled: 1, Errors: 1},
			check: func(t *testing.T, _ *conformanceRegistry, eng *conformanceEngine) {
				if eng.callsFor("ResolveArtifact") == 0 {
					t.Fatal("a missing artifact was not re-resolved before being counted as an error")
				}
			},
		},
		{
			name:  "status not_found after an observation parks the lease",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindSpec, testRunKey, DocumentDraft), missing(KindSpec, testRunKey)}
			},
			ticks:  2,
			want:   TickResult{Polled: 2, Refused: 1},
			refuse: []string{RefuseReplacedDocument},
			parked: true,
		},
		{
			name:  "document_status stale refuses the advance",
			stage: StagePlan,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindPlan, testRunKey, DocumentStale)}
				eng.plan = testPlan()
			},
			ticks:  1,
			want:   TickResult{Polled: 1, Refused: 1},
			refuse: []string{RefuseStalePlan},
			parked: true,
			check: func(t *testing.T, reg *conformanceRegistry, eng *conformanceEngine) {
				if eng.callsFor("ExportPlan") != 0 {
					t.Fatal("a stale plan was exported")
				}
				if len(reg.plans) != 0 {
					t.Fatalf("a stale plan was imported: %+v", reg.plans)
				}
			},
		},
		{
			name:  "final then draft under the same name is a stale plan",
			stage: StagePlan,
			setup: func(reg *conformanceRegistry, eng *conformanceEngine) {
				reg.advanceErr = errors.New("persist failed")
				eng.statuses = []answer{
					found(KindPlan, testRunKey, DocumentFinal),
					found(KindPlan, testRunKey, DocumentDraft),
				}
				eng.plan = testPlan()
			},
			ticks:  2,
			want:   TickResult{Polled: 2, Errors: 1, Refused: 1},
			refuse: []string{RefuseStalePlan},
			parked: true,
		},
		{
			name:  "superseded document parks the lease for a reset",
			stage: StagePlan,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindPlan, testRunKey, DocumentSuperseded)}
			},
			ticks:  1,
			want:   TickResult{Polled: 1, Refused: 1},
			refuse: []string{RefuseReplacedDocument},
			parked: true,
		},
		{
			name:  "archived document parks the lease for a reset",
			stage: StagePlan,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindPlan, testRunKey, DocumentArchived)}
			},
			ticks:  1,
			want:   TickResult{Polled: 1, Refused: 1},
			refuse: []string{RefuseArchivedDocument},
			parked: true,
		},
		{
			name:  "draft records progress and waits",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindSpec, testRunKey, DocumentDraft)}
			},
			ticks: 2,
			want:  TickResult{Polled: 2},
			check: func(t *testing.T, reg *conformanceRegistry, _ *conformanceEngine) {
				if len(reg.progress) == 0 || reg.progress[0][AttrDocumentStatus] != string(DocumentDraft) {
					t.Fatalf("draft progress not recorded: %+v", reg.progress)
				}
				if len(reg.advances) != 0 || len(reg.refusals) != 0 {
					t.Fatal("a draft document advanced or was refused")
				}
			},
		},
		{
			name:  "final spec is read, receipted and advanced",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindSpec, testRunKey, DocumentFinal)}
				eng.specBody = "# spec body"
			},
			ticks: 1,
			want:  TickResult{Polled: 1, Advanced: 1},
			check: func(t *testing.T, reg *conformanceRegistry, eng *conformanceEngine) {
				if eng.callsFor("ReadSpec") != 1 {
					t.Fatalf("ReadSpec calls = %d, want 1", eng.callsFor("ReadSpec"))
				}
				if len(reg.statuses) != 1 || reg.statuses[0].Body != "# spec body" {
					t.Fatalf("the spec body did not reach the advance: %+v", reg.statuses)
				}
				if len(reg.receipts) != 1 {
					t.Fatalf("receipts = %d, want 1", len(reg.receipts))
				}
				validateReceipt(t, reg.receipts[0])
				if reg.plans[0] != nil {
					t.Fatal("a spec advance carried a plan import")
				}
			},
		},
		{
			name:  "final plan whose export fails is refused, not retried",
			stage: StagePlan,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{found(KindPlan, testRunKey, DocumentFinal)}
				eng.planErr = errors.New("exit status 2")
			},
			ticks:  1,
			want:   TickResult{Polled: 1, Refused: 1},
			refuse: []string{RefusePlanImportFailed},
			parked: true,
		},
		{
			name:  "final plan the registry cannot import is refused, not retried",
			stage: StagePlan,
			setup: func(reg *conformanceRegistry, eng *conformanceEngine) {
				reg.advanceErr = &PlanImportError{RunKey: testRunKey, Artifact: testRunKey, Err: errors.New("no bead store configured")}
				eng.statuses = []answer{found(KindPlan, testRunKey, DocumentFinal)}
				eng.plan = testPlan()
			},
			ticks:  1,
			want:   TickResult{Polled: 1, Refused: 1},
			refuse: []string{RefusePlanImportFailed},
			parked: true,
		},
		{
			name:  "verb error is counted and retried",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{{err: &VerbError{Kind: KindSpec, Name: testRunKey, Code: "internal_error", Message: "boom"}}}
			},
			ticks: 1,
			want:  TickResult{Polled: 1, Errors: 1},
		},
		{
			name:  "contract error is counted and retried",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{{err: &ContractError{Kind: KindSpec, Name: testRunKey, Reason: "unknown document_status"}}}
			},
			ticks: 1,
			want:  TickResult{Polled: 1, Errors: 1},
		},
		{
			name:  "context deadline is counted and retried",
			stage: StageSpec,
			setup: func(_ *conformanceRegistry, eng *conformanceEngine) {
				eng.statuses = []answer{{err: context.DeadlineExceeded}}
			},
			ticks: 1,
			want:  TickResult{Polled: 1, Errors: 1},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { runFailureRow(t, row) })
	}
}

func runFailureRow(t *testing.T, row failureRow) {
	t.Helper()
	reg := newConformanceRegistry(row.stage)
	eng := newConformanceEngine(conformanceBaselineName)
	if row.setup != nil {
		row.setup(reg, eng)
	}
	r := conformanceRunner(reg, eng)

	now := t0
	var got TickResult
	for i := 0; i < row.ticks; i++ {
		res := r.Tick(context.Background(), now)
		got.Polled += res.Polled
		got.Advanced += res.Advanced
		got.Refused += res.Refused
		got.Errors += res.Errors
		now = now.Add(testPoll)
	}
	if got != row.want {
		t.Fatalf("after %d ticks: %+v, want %+v", row.ticks, got, row.want)
	}
	if reasons := reg.refusalReasons(); !reflect.DeepEqual(reasons, row.refuse) {
		t.Fatalf("refusals = %v, want %v", reasons, row.refuse)
	}

	before := eng.callCount()
	extra := r.Tick(context.Background(), now)
	if row.parked {
		if extra.Polled != 0 || extra.Refused != 0 || eng.callCount() != before {
			t.Fatalf("a parked stage was polled again: %+v (engine calls %d -> %d)", extra, before, eng.callCount())
		}
	} else if eng.callCount() == before {
		t.Fatal("a transient failure was not retried on the next poll")
	}
	if row.check != nil {
		row.check(t, reg, eng)
	}
}

// --- AC-4(c): a second registered engine ----------------------------------

// A second engine registered under its own name drives the same observer: it
// is built through the registry, its name and contract revision are stamped
// on the receipt (the name the import records as the plan's source, #10357),
// and the plan it exports is imported as a DRAFT Hive plan.
func TestConformance_SecondEngineSurfacesOnTheReceiptAndTheImport(t *testing.T) {
	Register(conformanceEngineName, func(config.RunsConfig, *slog.Logger) (Engine, error) {
		eng := newConformanceEngine(conformanceEngineName)
		eng.contract = conformanceContractRev
		eng.version = conformanceEngineVer
		eng.statuses = []answer{found(KindPlan, testRunKey, DocumentFinal)}
		eng.plan = testPlan()
		return eng, nil
	})
	build, ok := Lookup(conformanceEngineName)
	if !ok {
		t.Fatalf("Lookup(%q) = false; registered engines are %v", conformanceEngineName, Names())
	}
	engine, err := build(config.RunsConfig{}, slog.Default())
	if err != nil || engine == nil {
		t.Fatalf("building %q: %v", conformanceEngineName, err)
	}

	reg := &fakeLeaseRegistry{stage: StagePlan, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true}
	r := &Runner{
		Engine:   engine,
		Poll:     testPoll,
		Registry: NewLeaseRegistryAdapter(reg, engine),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("second-engine tick = %+v", res)
	}
	if len(reg.receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(reg.receipts))
	}
	var receipt outputschema.StageReceipt
	if err := json.Unmarshal(reg.receipts[0], &receipt); err != nil {
		t.Fatalf("decoding receipt: %v", err)
	}
	validateReceipt(t, receipt)
	if receipt.Engine == nil || receipt.Engine.Name != conformanceEngineName || receipt.Engine.Version != conformanceEngineVer {
		t.Fatalf("receipt engine = %+v", receipt.Engine)
	}
	if receipt.ContractRevision != conformanceContractRev {
		t.Fatalf("contract revision = %q, want %q", receipt.ContractRevision, conformanceContractRev)
	}
	if receipt.Provenance == nil || receipt.Provenance.Query != conformanceEngineName+" plan status "+testRunKey {
		t.Fatalf("provenance = %+v", receipt.Provenance)
	}

	// The baseline engine's receipt for the same lease differs only in engine
	// identity: the lease facts the receipt pins are engine-neutral.
	baseline := BuildReceipt(&scriptedEngine{}, Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: StagePlan,
		Identity: testIdentity, TaskID: testTaskID, Repo: testRepo, WorkDir: testWorkDir, Gen: 1,
	}, doc(KindPlan, testRunKey, DocumentFinal), t0)
	if baseline.Engine.Name == receipt.Engine.Name || baseline.ContractRevision == receipt.ContractRevision {
		t.Fatalf("the two engines share an identity: %+v vs %+v", baseline.Engine, receipt.Engine)
	}
	if baseline.WorkKey != receipt.WorkKey || baseline.ExecutionKey != receipt.ExecutionKey || baseline.InputRevision != receipt.InputRevision {
		t.Fatalf("engine identity moved an engine-neutral receipt field:\n %+v\n %+v", baseline, receipt)
	}

	// The exported plan reached the import verbatim, and lands as a DRAFT
	// plan sourced from the engine the receipt names, so implement stays
	// gated until ApprovePlan and no model redecomposes it.
	if len(reg.plans) != 1 || reg.plans[0] != RenderTaskList(*testPlan()) {
		t.Fatalf("imported task list = %q", reg.plans)
	}
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("bead store: %v", err)
	}
	epic, err := store.Create("hcl encoding helpers", beads.TypeEpic, beads.PriorityMedium, "architect", testRunKey)
	if err != nil {
		t.Fatalf("epic: %v", err)
	}
	if err := store.SetMetadata(epic.ID, planning.MetaSource, receipt.Engine.Name); err != nil {
		t.Fatalf("source metadata: %v", err)
	}
	if epic, err = store.Get(epic.ID); err != nil {
		t.Fatalf("reloading epic: %v", err)
	}
	result, err := planning.DecomposeFromOutput(store, epic, reg.plans[0], planning.Options{AutoApprove: false})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if len(result.Children) != len(testPlan().Tasks) {
		t.Fatalf("children = %d, want %d", len(result.Children), len(testPlan().Tasks))
	}
	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("reloading epic: %v", err)
	}
	if got.Meta(planning.MetaSource) != conformanceEngineName {
		t.Fatalf("plan source = %q, want %q", got.Meta(planning.MetaSource), conformanceEngineName)
	}
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("plan_status = %q, want draft until ApprovePlan", got.Meta(planning.MetaPlanStatus))
	}
}

package planengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/planning"
)

const (
	testRunKey   = "20260922132517-hcl-encoding-helpers"
	testRepo     = "myorg/repo1"
	testTaskID   = "task-8303"
	testIdentity = "c-8303"
	testPoll     = time.Minute
	testLeaseTTL = 10 * time.Minute
	testWorkDir  = "."
)

var t0 = time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)

// doc is the status document the baseline engine reports: the #8301 shape
// with the frontmatter dates Spektacular prints (midnight UTC, closed only
// once the document is final).
func doc(kind, name string, status DocumentStatus) ArtifactStatus {
	out := ArtifactStatus{
		Kind:           kind,
		Name:           name,
		ArtifactID:     name,
		DocumentStatus: status,
		CurrentStep:    "authoring",
		CompletedSteps: []string{"interview"},
		CreatedAt:      time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 9, 22, 13, 30, 0, 0, time.UTC),
	}
	if status == DocumentFinal {
		out.ClosedAt = time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	}
	return out
}

func notFound(kind, name string) error {
	return &NotFoundError{Kind: kind, Name: name, Message: "artifact " + name + " was not found"}
}

// answer is one scripted Status outcome.
type answer struct {
	status ArtifactStatus
	err    error
}

func found(kind, name string, status DocumentStatus) answer {
	return answer{status: doc(kind, name, status)}
}

func missing(kind, name string) answer { return answer{err: notFound(kind, name)} }

// scriptedEngine answers Status from a fixed sequence (the last entry
// repeats) or, when byName is set, from the documents that "exist"; it
// records every call so a test can assert what the observer asked for.
type scriptedEngine struct {
	mu         sync.Mutex
	answers    []answer
	byName     map[string]ArtifactStatus
	idx        int
	plan       *Plan
	planErr    error
	specBody   string
	specErr    error
	resolveTo  string
	resolveErr error

	kinds     []string
	polled    []string
	dirs      []string
	exported  []string
	specReads []string
}

func (e *scriptedEngine) Name() string             { return "spektacular" }
func (e *scriptedEngine) ContractRevision() string { return "spektacular-status/v1" }
func (e *scriptedEngine) EngineVersion() string    { return "spektacular-pr-45" }

func (e *scriptedEngine) Probe(context.Context) (ProbeResult, error) {
	return ProbeResult{Present: true, Version: "0.23.1", Binary: "spektacular"}, nil
}

func (e *scriptedEngine) Status(_ context.Context, dir, kind, artifact string) (ArtifactStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.kinds = append(e.kinds, kind)
	e.polled = append(e.polled, artifact)
	e.dirs = append(e.dirs, dir)
	if e.byName != nil {
		status, ok := e.byName[artifact]
		if !ok {
			return ArtifactStatus{}, notFound(kind, artifact)
		}
		return status, nil
	}
	if len(e.answers) == 0 {
		return ArtifactStatus{}, errors.New("no scripted status")
	}
	i := e.idx
	if i >= len(e.answers) {
		i = len(e.answers) - 1
	}
	e.idx++
	return e.answers[i].status, e.answers[i].err
}

func (e *scriptedEngine) ResolveArtifact(_ context.Context, _, kind, slug string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resolveErr != nil {
		return "", e.resolveErr
	}
	if e.resolveTo == "" {
		return "", notFound(kind, slug)
	}
	return e.resolveTo, nil
}

func (e *scriptedEngine) ExportPlan(_ context.Context, _, artifact string) (Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exported = append(e.exported, artifact)
	if e.planErr != nil {
		return Plan{}, e.planErr
	}
	if e.plan == nil {
		return Plan{}, errors.New("no scripted plan")
	}
	return *e.plan, nil
}

func (e *scriptedEngine) ReadSpec(_ context.Context, _, artifact string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specReads = append(e.specReads, artifact)
	return e.specBody, e.specErr
}

func (e *scriptedEngine) statusCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.polled)
}

func (e *scriptedEngine) lastKind() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.kinds) == 0 {
		return ""
	}
	return e.kinds[len(e.kinds)-1]
}

// testPlan is the three-task plan a final plan document exports.
func testPlan() *Plan {
	return &Plan{Kind: KindPlan, Name: testRunKey, Tasks: []PlanTask{
		{Ref: "T1", Title: "Add encoding helpers", Repo: "hivecommons/hive", Execution: "agent_suitable"},
		{Ref: "T2", Title: "Wire helpers into the parser", DependsOn: []string{"T1"}},
		{Ref: "T3", Title: "Sign off on the public API", DependsOn: []string{"T2"}, Execution: "human_required"},
	}}
}

// fakeRegistry is an in-memory lease registry with the same generation rules
// the dashboard applies.
type fakeRegistry struct {
	mu         sync.Mutex
	stage      Stage
	present    bool
	gen        uint64
	listErr    error
	advanceErr error
	advances   []Stage
	receipts   []outputschema.StageReceipt
	plans      []*Plan
	refusals   []string
	progress   []map[string]string
}

func newFakeRegistry(stage string) *fakeRegistry {
	return &fakeRegistry{present: true, gen: 1, stage: Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: stage, Identity: testIdentity,
		TaskID: testTaskID, Repo: testRepo, WorkDir: testWorkDir, Gen: 1,
	}}
}

func (f *fakeRegistry) ActiveStages(time.Time) ([]Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !f.present {
		return nil, nil
	}
	return []Stage{f.stage}, nil
}

func (f *fakeRegistry) Advance(_ context.Context, st Stage, _ ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.advanceErr != nil {
		return f.advanceErr
	}
	f.advances = append(f.advances, st)
	f.receipts = append(f.receipts, receipt)
	f.plans = append(f.plans, plan)
	f.gen++
	f.stage.Gen = f.gen
	f.stage.Stage = nextStage(st.Stage)
	return nil
}

func (f *fakeRegistry) Refuse(_ Stage, reason string, _ *ArtifactStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals = append(f.refusals, reason)
}

func (f *fakeRegistry) RecordProgress(_ Stage, attrs map[string]string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(attrs))
	for k, v := range attrs {
		cp[k] = v
	}
	f.progress = append(f.progress, cp)
}

func newRunner(reg Registry, engine Engine) *Runner {
	return &Runner{
		Engine:   engine,
		Poll:     testPoll,
		Registry: reg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// --- Tick: the poll loop --------------------------------------------------

func TestTick_DraftThenFinalAdvancesOnceAndWritesOneReceipt(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{
		found(KindSpec, testRunKey, DocumentDraft),
		found(KindSpec, testRunKey, DocumentDraft),
		found(KindSpec, testRunKey, DocumentFinal),
	}, specBody: "# spec body"}
	r := newRunner(reg, eng)

	now := t0
	var total TickResult
	for i := 0; i < 3; i++ {
		res := r.Tick(context.Background(), now)
		total.Advanced += res.Advanced
		total.Polled += res.Polled
		now = now.Add(testPoll)
	}
	if total.Advanced != 1 || total.Polled != 3 {
		t.Fatalf("after draft, draft, final: %+v", total)
	}
	if len(reg.advances) != 1 || reg.advances[0].Stage != StageSpec || len(reg.receipts) != 1 {
		t.Fatalf("advances = %+v receipts = %d", reg.advances, len(reg.receipts))
	}
	if reg.stage.Stage != StagePlan || reg.stage.Gen != 2 {
		t.Fatalf("registry stage after advance = %+v", reg.stage)
	}
	if reg.plans[0] != nil {
		t.Fatal("a spec advance must not carry a plan import")
	}
	receipt := reg.receipts[0]
	validateReceipt(t, receipt)
	if receipt.Stage != StageSpec || receipt.Generation != 1 || receipt.WorkKey != testRepo+"!"+testRunKey || receipt.AssignmentID != testTaskID {
		t.Fatalf("receipt = %+v", receipt)
	}
	if len(reg.refusals) != 0 {
		t.Fatalf("unexpected refusals: %v", reg.refusals)
	}
	if len(reg.progress) == 0 || reg.progress[0][AttrDocumentStatus] != string(DocumentDraft) || reg.progress[0][AttrCurrentStep] != "authoring" {
		t.Fatalf("status progress not recorded: %+v", reg.progress)
	}
	// A final spec carries the artifact body to the checkpoint summary.
	if len(eng.specReads) != 1 || reg.advances[0].Stage != StageSpec {
		t.Fatalf("spec reads = %v", eng.specReads)
	}

	// The next tick polls the NEW stage (plan) under the new generation, and the
	// old spec stage is never advanced twice.
	eng.mu.Lock()
	eng.answers = append(eng.answers, found(KindPlan, testRunKey, DocumentDraft))
	eng.idx = len(eng.answers) - 1
	eng.mu.Unlock()
	res := r.Tick(context.Background(), now)
	if res.Advanced != 0 || res.Polled != 1 || len(reg.advances) != 1 {
		t.Fatalf("plan-stage tick = %+v advances=%d", res, len(reg.advances))
	}
	if eng.lastKind() != KindPlan {
		t.Fatalf("next poll asked for %q, want plan", eng.lastKind())
	}
}

func TestTick_SpecReadFailureStillAdvances(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{
		answers: []answer{found(KindSpec, testRunKey, DocumentFinal)},
		specErr: errors.New("file read failed"),
	}
	if res := newRunner(reg, eng).Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
}

func TestTick_PlanFinalImportsStructuredPlanAsDraft(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	eng := &scriptedEngine{answers: []answer{found(KindPlan, testRunKey, DocumentFinal)}, plan: testPlan()}
	r := newRunner(reg, eng)

	if res := r.Tick(context.Background(), t0); res.Advanced != 1 {
		t.Fatalf("plan final tick = %+v", res)
	}
	if len(reg.plans) != 1 || reg.plans[0] == nil || len(reg.plans[0].Tasks) != 3 {
		t.Fatalf("plan import = %+v", reg.plans)
	}
	if reg.stage.Stage != StageImplement {
		t.Fatalf("stage after plan final = %q, want implement", reg.stage.Stage)
	}
	// The implement stage has no planning document: the runner never polls it.
	before := eng.statusCalls()
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 0 || eng.statusCalls() != before {
		t.Fatalf("implement stage was polled: %+v", res)
	}

	// The imported plan lands as a DRAFT (AutoApprove false): implement stays
	// gated until ApprovePlan, and no model is asked to redecompose.
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("bead store: %v", err)
	}
	epic, err := store.Create("hcl encoding helpers", beads.TypeEpic, beads.PriorityMedium, "architect", testRunKey)
	if err != nil {
		t.Fatalf("epic: %v", err)
	}
	result, err := planning.DecomposeFromOutput(store, epic, RenderTaskList(*reg.plans[0]), planning.Options{AutoApprove: false})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if len(result.Children) != 3 {
		t.Fatalf("children = %d", len(result.Children))
	}
	first, err := store.Get(result.Children[0].ID)
	if err != nil {
		t.Fatalf("first child: %v", err)
	}
	if first.Meta(planning.MetaPlanRepo) != "hivecommons/hive" {
		t.Fatalf("plan_repo = %q, want hivecommons/hive", first.Meta(planning.MetaPlanRepo))
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("plan_status = %q, want draft until ApprovePlan", got.Meta(planning.MetaPlanStatus))
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	got, _ = store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatal("ApprovePlan did not approve")
	}
}

func TestTick_ResolvesArtifactAfterNotFound(t *testing.T) {
	const resolved = "20260925163042-kubestellar-console-23725"
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = "kubestellar-console-23725"
	eng := &scriptedEngine{
		byName:    map[string]ArtifactStatus{resolved: doc(KindSpec, resolved, DocumentFinal)},
		resolveTo: resolved,
	}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if len(eng.polled) != 2 || eng.polled[1] != resolved {
		t.Fatalf("polled = %v, want the slug then the resolved id", eng.polled)
	}
	// The resolution is reported on the run timeline.
	var sawResolved bool
	for _, p := range reg.progress {
		if p[AttrReason] == "artifact_resolved" && p[AttrArtifact] == resolved {
			sawResolved = true
		}
	}
	if !sawResolved {
		t.Fatalf("artifact resolution not recorded: %+v", reg.progress)
	}
}

func TestTick_ReresolvesArtifactAfterGenerationChange(t *testing.T) {
	// Generation 1 resolves an older document that never goes final; the retry
	// generation leaves a newer document in the same work dir. The runner must
	// re-resolve rather than keep polling the old id.
	const oldID = "20260925160000-kubestellar-console-23725"
	const newID = "20260925163042-kubestellar-console-23725"
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = "kubestellar-console-23725"
	eng := &scriptedEngine{
		byName:    map[string]ArtifactStatus{oldID: doc(KindSpec, oldID, DocumentDraft)},
		resolveTo: oldID,
	}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Advanced != 0 || res.Errors != 0 {
		t.Fatalf("gen1 tick = %+v", res)
	}
	eng.mu.Lock()
	eng.byName[newID] = doc(KindSpec, newID, DocumentFinal)
	eng.resolveTo = newID
	eng.mu.Unlock()
	reg.mu.Lock()
	reg.stage.Gen = 2
	reg.mu.Unlock()
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("gen2 tick = %+v (polled %v)", res, eng.polled)
	}
	if last := eng.polled[len(eng.polled)-1]; last != newID {
		t.Fatalf("gen2 polled %q, want %q (polled %v)", last, newID, eng.polled)
	}
}

func TestTick_SupersededOrArchivedDocumentParksLease(t *testing.T) {
	cases := map[DocumentStatus]string{
		DocumentSuperseded: RefuseReplacedDocument,
		DocumentArchived:   RefuseArchivedDocument,
	}
	for status, reason := range cases {
		t.Run(string(status), func(t *testing.T) {
			reg := newFakeRegistry(StagePlan)
			eng := &scriptedEngine{answers: []answer{found(KindPlan, testRunKey, status)}, plan: testPlan()}
			r := newRunner(reg, eng)
			if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Errors != 0 || res.Advanced != 0 {
				t.Fatalf("tick = %+v", res)
			}
			if len(reg.refusals) != 1 || reg.refusals[0] != reason {
				t.Fatalf("refusals = %v, want [%s]", reg.refusals, reason)
			}
			if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 0 || eng.statusCalls() != 1 {
				t.Fatalf("parked tick = %+v status calls = %d", res, eng.statusCalls())
			}
		})
	}
}

func TestTick_PlanFinalWithFailedExportRefusesAndParks(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	eng := &scriptedEngine{answers: []answer{found(KindPlan, testRunKey, DocumentFinal)}, planErr: errors.New("exit status 2")}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Advanced != 0 || res.Refused != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if len(reg.advances) != 0 {
		t.Fatal("advanced without a plan export")
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefusePlanImportFailed {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	// Parked: later polls neither call the engine again nor re-refuse.
	calls := eng.statusCalls()
	if res := r.Tick(context.Background(), t0.Add(3*testPoll)); res.Polled != 0 || res.Refused != 0 || eng.statusCalls() != calls {
		t.Fatalf("parked tick = %+v calls %d -> %d", res, calls, eng.statusCalls())
	}
	// A new generation (operator reset or executor retry) looks again.
	reg.stage.Gen = 2
	eng.mu.Lock()
	eng.planErr = nil
	eng.plan = testPlan()
	eng.mu.Unlock()
	if res := r.Tick(context.Background(), t0.Add(4*testPoll)); res.Advanced != 1 {
		t.Fatalf("post-reset tick = %+v", res)
	}
}

func TestTick_PlanImportRejectedByRegistryRefuses(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	reg.advanceErr = &PlanImportError{RunKey: testRunKey, Artifact: testRunKey, Err: errors.New("no bead store configured for plan import")}
	eng := &scriptedEngine{answers: []answer{found(KindPlan, testRunKey, DocumentFinal)}, plan: testPlan()}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Errors != 0 || res.Advanced != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefusePlanImportFailed {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 0 {
		t.Fatalf("parked tick = %+v", res)
	}
}

func TestTick_StalePlanStatusRefusesImplementAdvance(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	eng := &scriptedEngine{answers: []answer{found(KindPlan, testRunKey, DocumentStale)}, plan: testPlan()}
	r := newRunner(reg, eng)

	if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Advanced != 0 || res.Errors != 0 {
		t.Fatalf("stale tick = %+v", res)
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefuseStalePlan {
		t.Fatalf("refusals = %+v", reg.refusals)
	}
	if len(reg.advances) != 0 || len(reg.plans) != 0 {
		t.Fatalf("stale plan advanced/imported: advances=%d plans=%d", len(reg.advances), len(reg.plans))
	}
}

func TestTick_FinalThenDraftRefusesStalePlan(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	reg.advanceErr = errors.New("persist failed")
	eng := &scriptedEngine{answers: []answer{
		found(KindPlan, testRunKey, DocumentFinal),
		found(KindPlan, testRunKey, DocumentDraft),
	}, plan: testPlan()}
	r := newRunner(reg, eng)

	// final observed, but the registry could not persist the advance.
	if res := r.Tick(context.Background(), t0); res.Errors != 1 || res.Advanced != 0 {
		t.Fatalf("final tick = %+v", res)
	}
	reg.advanceErr = nil
	// Same name flips back to draft: a stale plan. Refuse, never advance.
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Refused != 1 || res.Advanced != 0 {
		t.Fatalf("draft-after-final tick = %+v", res)
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefuseStalePlan {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	// Refused stages are parked: not polled, even much later, until the
	// generation changes.
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL*3)); res.Polled != 0 || res.Refused != 0 {
		t.Fatalf("parked tick = %+v", res)
	}
	if len(reg.advances) != 0 {
		t.Fatal("stale plan advanced")
	}
	// An operator reset (new generation) lets the runner look again.
	reg.stage.Gen = 9
	eng.mu.Lock()
	eng.answers = []answer{found(KindPlan, testRunKey, DocumentFinal)}
	eng.idx = 0
	eng.mu.Unlock()
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL*3+testPoll)); res.Advanced != 1 {
		t.Fatalf("post-reset tick = %+v", res)
	}
}

func TestTick_VanishedAfterObservationRefusesRebind(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{
		found(KindSpec, testRunKey, DocumentDraft),
		missing(KindSpec, testRunKey),
	}}
	r := newRunner(reg, eng)
	r.Tick(context.Background(), t0)
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Refused != 1 {
		t.Fatalf("vanished tick = %+v", res)
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefuseReplacedDocument {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	if len(reg.advances) != 0 || reg.stage.Gen != 1 {
		t.Fatal("a replaced document must not advance or mint a generation on its own")
	}
}

func TestTick_MissingFromTheStartIsAnError(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{missing(KindSpec, testRunKey)}}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Errors != 1 || res.Refused != 0 {
		t.Fatalf("missing tick = %+v", res)
	}
	if len(reg.refusals) != 0 || len(reg.advances) != 0 {
		t.Fatalf("refusals=%v advances=%d", reg.refusals, len(reg.advances))
	}
}

func TestTick_PollIntervalAndHousekeeping(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, eng)
	r.Tick(context.Background(), t0)
	if res := r.Tick(context.Background(), t0.Add(testPoll/2)); res.Polled != 0 {
		t.Fatalf("polled inside the interval: %+v", res)
	}
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 1 {
		t.Fatalf("not polled after the interval: %+v", res)
	}
	// A stage that disappears from the registry is forgotten.
	reg.present = false
	r.Tick(context.Background(), t0.Add(2*testPoll))
	if len(r.stages) != 0 {
		t.Fatalf("stale runner state kept: %d", len(r.stages))
	}
	// Registry errors are counted, not fatal.
	reg.listErr = errors.New("registry down")
	if res := r.Tick(context.Background(), t0.Add(3*testPoll)); res.Errors != 1 {
		t.Fatalf("registry error tick = %+v", res)
	}
	// Nil receivers, an engine-less and a registry-less runner are inert.
	var nilRunner *Runner
	if res := nilRunner.Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("nil runner tick = %+v", res)
	}
	if res := (&Runner{}).Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("registry-less tick = %+v", res)
	}
	if res := (&Runner{Registry: reg}).Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("engine-less tick = %+v", res)
	}
	if (&Runner{}).logger() == nil {
		t.Fatal("logger() returned nil")
	}
	if next := nextStage(StageImplement); next != "" {
		t.Fatalf("nextStage(implement) = %q", next)
	}
	if _, ok := kindForStage(StageImplement); ok {
		t.Fatal("the implement stage has no document kind")
	}
}

func TestTick_MissingWorkDirRefusesBeforeTheEngineIsCalled(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	reg.stage.WorkDir = ""
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, eng)
	if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Polled != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if eng.statusCalls() != 0 {
		t.Fatal("a stage without a workdir reached the engine")
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefuseMissingWorkDir {
		t.Fatalf("refusals = %v", reg.refusals)
	}
}

func TestTick_RelayHeldStageIsLeftToTheWebsocket(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	reg.stage.RelayHeld = true
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, eng)
	res := r.Tick(context.Background(), t0)
	if res.RelayHeld != 1 || res.Polled != 0 || res.Refused != 0 {
		t.Fatalf("relay-held tick = %+v", res)
	}
	if eng.statusCalls() != 0 {
		t.Fatal("a relay-held stage reached the engine")
	}
}

func TestRun_TicksUntilCancelled(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, eng)
	r.Poll = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, func() time.Time { return t0 })
	}()
	deadline := time.After(2 * time.Second)
	for {
		reg.mu.Lock()
		n := len(reg.advances)
		reg.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never advanced the stage")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	// A zero Poll falls back to a positive ticker interval.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	(&Runner{Registry: reg, Engine: eng}).Run(ctx2, nil)
}

// --- Unclaimed admission leases -------------------------------------------

// An admission lease (owned by hive-triage, no relay yet) has no checkout by
// construction. The runner must wait for a claim, not park it with
// missing_workdir; once a relay claims the same generation and a checkout
// exists, the stage is polled like any other.
func TestTick_UnclaimedAdmissionWaitsForClaim(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	reg.stage.Identity = "hive-triage"
	reg.stage.WorkDir = ""
	reg.stage.Unclaimed = true
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, eng)

	res := r.Tick(context.Background(), t0)
	if res.Unclaimed != 1 || res.Refused != 0 || res.Polled != 0 {
		t.Fatalf("unclaimed tick = %+v, want Unclaimed=1 Refused=0 Polled=0", res)
	}
	if len(reg.refusals) != 0 {
		t.Fatalf("unclaimed admission was refused: %v", reg.refusals)
	}

	// A relay claims the lease: same run key, stage and generation, new owner
	// and a real checkout.
	reg.mu.Lock()
	reg.stage.Identity = testIdentity
	reg.stage.WorkDir = testWorkDir
	reg.stage.Unclaimed = false
	reg.mu.Unlock()

	res = r.Tick(context.Background(), t0.Add(testPoll))
	if res.Polled != 1 || res.Unclaimed != 0 || res.Refused != 0 {
		t.Fatalf("claimed tick = %+v, want Polled=1", res)
	}
}

// A refusal recorded against one owner's checkout must not stick to the
// stage when the same generation changes hands to a relay that has one.
func TestTick_OwnerChangeReArmsRefusedStage(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	reg.stage.WorkDir = ""
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, eng)

	res := r.Tick(context.Background(), t0)
	if res.Refused != 1 || len(reg.refusals) != 1 || reg.refusals[0] != RefuseMissingWorkDir {
		t.Fatalf("first tick = %+v refusals=%v", res, reg.refusals)
	}
	// Same owner, still no checkout: refusal is terminal, not repeated.
	res = r.Tick(context.Background(), t0.Add(testPoll))
	if res.Refused != 0 || res.Polled != 0 {
		t.Fatalf("second tick = %+v, want nothing", res)
	}

	reg.mu.Lock()
	reg.stage.Identity = "other-relay"
	reg.stage.WorkDir = testWorkDir
	reg.mu.Unlock()

	res = r.Tick(context.Background(), t0.Add(2*testPoll))
	if res.Polled != 1 || res.Refused != 0 {
		t.Fatalf("re-armed tick = %+v, want Polled=1", res)
	}
}

func TestTick_CanceledContextSkipsPolling(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	eng := &scriptedEngine{answers: []answer{found(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, eng)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Tick(ctx, t0)
	if eng.statusCalls() != 0 {
		t.Fatal("canceled tick still polled a stage")
	}
}

func TestStageKeyIsRunKeyAndStage(t *testing.T) {
	if stageKey(Stage{RunKey: testRunKey, Stage: StageSpec}) == stageKey(Stage{RunKey: testRunKey, Stage: StagePlan}) {
		t.Fatal("two stages of one run share a stage key")
	}
	if stageKey(Stage{RunKey: testRunKey, Stage: StageSpec}) != stageKey(Stage{RunKey: testRunKey, Stage: StageSpec, Gen: 7}) {
		t.Fatal("the stage key must not depend on the generation")
	}
}

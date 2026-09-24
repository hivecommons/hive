package spektacular

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/escalate"
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
)

var t0 = time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)

// scriptedExec answers status calls from a fixed sequence (the last entry
// repeats) and plan export calls from exportJSON; it records every args slice.
type scriptedExec struct {
	mu         sync.Mutex
	statuses   []string
	exportJSON string
	exportErr  error
	files      map[string]string
	calls      [][]string
	idx        int
}

func (s *scriptedExec) exec(_ context.Context, args []string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string(nil), args...))
	if len(args) >= 2 && args[1] == verbExport {
		if s.exportErr != nil {
			if s.exportJSON != "" {
				return []byte(s.exportJSON), s.exportErr
			}
			return nil, s.exportErr
		}
		return []byte(s.exportJSON), nil
	}
	if len(args) >= 4 && args[0] == KindPlan && args[1] == verbFile && args[2] == verbRead {
		if s.files != nil {
			if out, ok := s.files[args[3]]; ok {
				return []byte(out), nil
			}
		}
		return []byte(`{"error":true,"code":"not_found","message":"file ` + args[3] + ` not found","resource":"` + args[3] + `"}`), errors.New("exit status 1")
	}
	if len(s.statuses) == 0 {
		return nil, errors.New("no scripted status")
	}
	i := s.idx
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	s.idx++
	entry := s.statuses[i]
	if strings.HasPrefix(entry, "ERR:") {
		return []byte(strings.TrimPrefix(entry, "ERR:")), errors.New("exit status 2")
	}
	return []byte(entry), nil
}

func (s *scriptedExec) statusCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if len(c) >= 2 && c[1] == verbStatus {
			n++
		}
	}
	return n
}

// statusJSON is the exact per-artifact status shape jumppad-labs/spektacular#45
// prints: an `error:false` envelope, frontmatter dates as RFC3339 midnight
// UTC with closed_at "" while open, and the (usually empty) spec / plan
// cross-references.
func statusJSON(kind, name string, status DocumentStatus) string {
	closed := `""`
	if status == DocumentFinal {
		closed = `"2026-09-22T00:00:00Z"`
	}
	plan := ""
	if kind == KindPlan {
		plan = name
	}
	return fmt.Sprintf(`{"error":false,"kind":%q,"name":%q,"artifact_id":%q,"document_status":%q,"current_step":"authoring","completed_steps":["interview"],"created_at":"2026-09-21T00:00:00Z","updated_at":"2026-09-22T13:30:00Z","closed_at":%s,"spec":"","plan":%q}`, kind, name, name, status, closed, plan)
}

// notFoundJSON is the #45 error envelope for a missing artifact.
const notFoundJSON = `ERR:{"error":true,"code":"artifact_not_found","message":"plan artifact \"x\" was not found","resource":"x","next_action":"run ` + "`spektacular plan file list`" + ` to see available plans"}`

const exportJSON = `{"kind":"plan","name":"` + testRunKey + `","tasks":[{"ref":"T1","title":"Add encoding helpers","repo":"hivecommons/hive","execution":"agent_suitable"},{"ref":"T2","title":"Wire helpers into the parser","depends_on":["T1"]},{"ref":"T3","title":"Sign off on the public API","depends_on":["T2"],"execution":"human_required"}]}`

// fakeRegistry is an in-memory lease registry with the same generation and
// expiry rules the dashboard applies.
type fakeRegistry struct {
	mu         sync.Mutex
	stage      Stage
	present    bool
	gen        uint64
	listErr    error
	advanceErr error
	retryErr   error
	advances   []Stage
	receipts   []outputschema.StageReceipt
	plans      []*Plan
	retries    []Stage
	refusals   []string
}

func newFakeRegistry(stage string) *fakeRegistry {
	return &fakeRegistry{present: true, gen: 1, stage: Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: stage, Identity: testIdentity,
		TaskID: testTaskID, Repo: testRepo, Gen: 1, ExpiresAt: t0.Add(testLeaseTTL),
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

func (f *fakeRegistry) Advance(_ context.Context, st Stage, _ ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, now time.Time) error {
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
	f.stage.ExpiresAt = now.Add(testLeaseTTL)
	return nil
}

func (f *fakeRegistry) Retry(_ context.Context, st Stage, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retryErr != nil {
		return f.retryErr
	}
	f.retries = append(f.retries, st)
	f.gen++
	f.stage.Gen = f.gen
	f.stage.ExpiresAt = now.Add(testLeaseTTL)
	return nil
}

func (f *fakeRegistry) Refuse(_ Stage, reason string, _ *ArtifactStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals = append(f.refusals, reason)
}

type escalations struct {
	mu     sync.Mutex
	events []escalate.Event
}

func (e *escalations) record(ev escalate.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func newRunner(reg Registry, ex *scriptedExec, esc *escalations) *Runner {
	return &Runner{
		Exec:     ex.exec,
		Poll:     testPoll,
		Registry: reg,
		Escalate: esc.record,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func validateReceipt(t *testing.T, r outputschema.StageReceipt) {
	t.Helper()
	raw, err := json.Marshal(outputschema.AgentReport{
		Lane: "runs", Kind: outputschema.KindStageReceipt, Summary: "stage receipt",
		Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
		Receipt: &r,
	})
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if _, err := outputschema.Validate(raw); err != nil {
		t.Fatalf("receipt does not satisfy outputschema: %v\n%s", err, raw)
	}
}

// --- Status: contract parsing --------------------------------------------

func TestStatus_ParsesContract(t *testing.T) {
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}}
	r := &Runner{Exec: ex.exec}
	st, err := r.Status(context.Background(), KindPlan, testRunKey)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Kind != KindPlan || st.Name != testRunKey || st.ArtifactID != testRunKey || st.JoinKey() != testRunKey || !st.Final() || st.CurrentStep != "authoring" ||
		len(st.CompletedSteps) != 1 || st.CreatedAt.IsZero() || st.UpdatedAt.IsZero() || st.ClosedAt.IsZero() ||
		st.Spec != "" || st.Plan != testRunKey {
		t.Fatalf("parsed status = %+v", st)
	}
	// The CLI has no --json flag: the invocation is exactly `<kind> status <name>`.
	want := []string{KindPlan, verbStatus, testRunKey}
	if got := ex.calls[0]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if st.CreatedAt.Hour() != 0 || st.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at should be the midnight-UTC frontmatter date, got %s", st.CreatedAt)
	}
}

func TestStatus_ArtifactIDIsJoinKeyWithNameFallback(t *testing.T) {
	withID := `{"error":false,"kind":"plan","name":"000057_old","artifact_id":"20260922132517-a1b2c3d4-new","document_status":"final","current_step":"finished","completed_steps":[],"created_at":"","closed_at":"","spec":"","plan":""}`
	st, err := (&Runner{Exec: (&scriptedExec{statuses: []string{withID}}).exec}).Status(context.Background(), KindPlan, "000057_old")
	if err != nil {
		t.Fatalf("Status with artifact_id: %v", err)
	}
	if st.JoinKey() != "20260922132517-a1b2c3d4-new" {
		t.Fatalf("JoinKey = %q", st.JoinKey())
	}
	legacy := `{"error":false,"kind":"plan","name":"000057_old","document_status":"final","current_step":"finished","completed_steps":[],"created_at":"","closed_at":"","spec":"","plan":""}`
	st, err = (&Runner{Exec: (&scriptedExec{statuses: []string{legacy}}).exec}).Status(context.Background(), KindPlan, "000057_old")
	if err != nil {
		t.Fatalf("legacy status: %v", err)
	}
	if st.JoinKey() != "000057_old" {
		t.Fatalf("legacy JoinKey = %q", st.JoinKey())
	}
}

func TestStatus_ToleratesAbsentUpdatedAt(t *testing.T) {
	// Spektacular may drop updated_at when no workflow state matches the
	// artifact, emit it as "" or null, and emits closed_at "" while open.
	// None of those may fail the parse or change the progress decision.
	base := `{"error":false,"kind":"spec","name":%q,"document_status":"final","current_step":"finished","completed_steps":["interview","authoring"],"created_at":"2026-09-21T00:00:00Z","closed_at":"2026-09-22T00:00:00Z","spec":"","plan":""%s}`
	for name, tail := range map[string]string{
		"absent": ``,
		"null":   `,"updated_at":null`,
		"empty":  `,"updated_at":""`,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &scriptedExec{statuses: []string{fmt.Sprintf(base, testRunKey, tail)}}
			st, err := (&Runner{Exec: ex.exec}).Status(context.Background(), KindSpec, testRunKey)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !st.UpdatedAt.IsZero() || !st.Final() || st.CurrentStep != "finished" || len(st.CompletedSteps) != 2 {
				t.Fatalf("status = %+v", st)
			}
		})
	}
	// An open document reports closed_at "" and created_at may be "" when the
	// frontmatter carries no date; both decode to the zero time.
	open := fmt.Sprintf(`{"error":false,"kind":"spec","name":%q,"document_status":"draft","current_step":"authoring","completed_steps":[],"created_at":"","updated_at":"2026-09-22T13:30:00Z","closed_at":"","spec":"","plan":""}`, testRunKey)
	st, err := (&Runner{Exec: (&scriptedExec{statuses: []string{open}}).exec}).Status(context.Background(), KindSpec, testRunKey)
	if err != nil || !st.CreatedAt.IsZero() || !st.ClosedAt.IsZero() || st.Final() {
		t.Fatalf("open status = %+v err=%v", st, err)
	}
	// A timestamp that is present but malformed is still a contract error.
	bad := fmt.Sprintf(`{"error":false,"kind":"spec","name":%q,"document_status":"draft","current_step":"authoring","completed_steps":[],"created_at":"yesterday","closed_at":""}`, testRunKey)
	var ce *ContractError
	if _, err := (&Runner{Exec: (&scriptedExec{statuses: []string{bad}}).exec}).Status(context.Background(), KindSpec, testRunKey); !errors.As(err, &ce) {
		t.Fatalf("malformed timestamp err = %v (%T)", err, err)
	}
	var ft flexTime
	if err := ft.UnmarshalJSON([]byte(`42`)); err == nil {
		t.Fatal("numeric timestamp accepted")
	}
}

func TestArtifactKey(t *testing.T) {
	// The bare artifact name is the only stable join key across stages
	// (spektacular#45, #46); every file-address spelling reduces to it.
	const bare = "000057_git-commit"
	for _, in := range []string{bare, bare + ".md", bare + "/plan.md", bare + "/PLAN.MD", " " + bare + ".MD ", bare + ".markdown"} {
		if got := ArtifactKey(in); got != bare {
			t.Fatalf("ArtifactKey(%q) = %q, want %q", in, got, bare)
		}
	}
	// A dot inside a name is not an extension, and a path that does not end
	// in a markdown document (an issue-style key) is not an artifact address.
	for _, keep := range []string{"000058_v1.2-upgrade", "myorg/repo1#42", "other/repo!x:nope", bare + "/plan"} {
		if got := ArtifactKey(keep); got != keep {
			t.Fatalf("ArtifactKey(%q) = %q, want it unchanged", keep, got)
		}
	}
	if got := ArtifactKey("  "); got != "" {
		t.Fatalf("ArtifactKey(blank) = %q", got)
	}
}

func TestStatus_JoinsByBareNameAcrossSpellings(t *testing.T) {
	// The lease may still carry a file address; the CLI is always asked for
	// the bare name, and a status answered under the bare name matches a
	// request made with the extension.
	const bare = "000057_git-commit"
	for _, spelling := range []string{bare + ".md", bare + "/plan.md", bare} {
		ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, bare, DocumentFinal)}, exportJSON: strings.ReplaceAll(exportJSON, testRunKey, bare)}
		r := &Runner{Exec: ex.exec}
		st, err := r.Status(context.Background(), KindPlan, spelling)
		if err != nil || st.Name != bare {
			t.Fatalf("Status(%q): %+v %v", spelling, st, err)
		}
		plan, err := r.ExportPlan(context.Background(), spelling)
		if err != nil || plan.Name != bare {
			t.Fatalf("ExportPlan(%q): %+v %v", spelling, plan, err)
		}
		for _, call := range ex.calls {
			if call[2] != bare {
				t.Fatalf("CLI asked for %q, want bare %q (call %v)", call[2], bare, call)
			}
		}
	}
	// Two leases spelled differently key the same stage state in the runner.
	if stageKey(Stage{RunKey: ArtifactKey(bare + ".md"), Stage: StageSpec}) != stageKey(Stage{RunKey: ArtifactKey(bare), Stage: StageSpec}) {
		t.Fatal("stage keys differ across spellings")
	}
}

func TestStatus_MissingDocumentIsTyped(t *testing.T) {
	for name, body := range map[string]string{
		"artifact_not_found":  notFoundJSON,
		"file verb not_found": `ERR:{"error":true,"code":"not_found","message":"file \"x\" not found"}`,
		"legacy string error": `ERR:{"error":"plan not found","code":"not_found"}`,
		"message only":        `ERR:{"error":"no such spec: not found"}`,
		"zero exit envelope":  `{"error":true,"code":"artifact_not_found","message":"gone"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &scriptedExec{statuses: []string{body}}
			_, err := (&Runner{Exec: ex.exec}).Status(context.Background(), KindSpec, testRunKey)
			var nf *NotFoundError
			if !errors.As(err, &nf) || nf.Kind != KindSpec || nf.Name != testRunKey || nf.Error() == "" {
				t.Fatalf("err = %v (%T), want *NotFoundError", err, err)
			}
		})
	}
}

func TestStatus_OtherErrorsAreTyped(t *testing.T) {
	cases := map[string]struct {
		body string
		want any
	}{
		"verb error":      {`ERR:{"error":true,"code":"backend","message":"store unreachable"}`, new(*VerbError)},
		"legacy verb err": {`ERR:{"error":"store unreachable","code":"backend"}`, new(*VerbError)},
		"zero exit error": {`{"error":true,"code":"backend","message":"store unreachable"}`, new(*VerbError)},
		"non-json exit":   {`ERR:panic: boom`, new(*ContractError)},
		"empty exit":      {`ERR:`, new(*ContractError)},
		"bad json":        {`{not json`, new(*ContractError)},
		"kind mismatch":   {statusJSON(KindPlan, testRunKey, DocumentDraft), new(*ContractError)},
		"name mismatch":   {statusJSON(KindSpec, "other-name", DocumentDraft), new(*ContractError)},
		"unknown status":  {statusJSON(KindSpec, testRunKey, DocumentStatus("archived")), new(*ContractError)},
		"exec plain fail": {`ERR:{"nope":true}`, new(*ContractError)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ex := &scriptedExec{statuses: []string{tc.body}}
			_, err := (&Runner{Exec: ex.exec}).Status(context.Background(), KindSpec, testRunKey)
			if err == nil {
				t.Fatal("expected an error")
			}
			switch want := tc.want.(type) {
			case **VerbError:
				if !errors.As(err, want) || (*want).Code != "backend" || (*want).Message != "store unreachable" || (*want).Error() == "" {
					t.Fatalf("err = %v (%T), want *VerbError", err, err)
				}
			case **ContractError:
				if !errors.As(err, want) || (*want).Error() == "" {
					t.Fatalf("err = %v (%T), want *ContractError", err, err)
				}
			}
		})
	}
	r := &Runner{Exec: (&scriptedExec{}).exec}
	if _, err := r.Status(context.Background(), "epic", testRunKey); err == nil {
		t.Fatal("unsupported kind accepted")
	}
	if _, err := r.Status(context.Background(), KindSpec, "  "); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := (&Runner{}).Status(context.Background(), KindSpec, testRunKey); err == nil {
		t.Fatal("runner without Exec did not error")
	}
	var ce *ContractError
	wrapped := &ContractError{Kind: KindSpec, Name: testRunKey, Reason: "x", Err: errors.New("inner")}
	if !errors.As(fmt.Errorf("outer: %w", wrapped), &ce) || ce.Unwrap() == nil || !strings.Contains(ce.Error(), "inner") {
		t.Fatal("ContractError does not unwrap")
	}
}

func TestExportPlanAndRenderTaskList(t *testing.T) {
	ex := &scriptedExec{exportJSON: exportJSON}
	r := &Runner{Exec: ex.exec}
	plan, err := r.ExportPlan(context.Background(), testRunKey)
	if err != nil {
		t.Fatalf("ExportPlan: %v", err)
	}
	if len(plan.Tasks) != 3 || plan.Name != testRunKey {
		t.Fatalf("plan = %+v", plan)
	}
	tasks := agentparse.ParseTaskList(agentparse.SplitLines(RenderTaskList(plan)))
	if len(tasks) != 3 {
		t.Fatalf("rendered task list parsed into %d tasks: %q", len(tasks), RenderTaskList(plan))
	}
	if tasks[1].Ref != "T2" || len(tasks[1].DependsOn) != 1 || tasks[1].DependsOn[0] != "T1" || tasks[1].Execution != agentparse.ExecutionAgentSuitable {
		t.Fatalf("T2 = %+v, want depends on T1 and agent_suitable default", tasks[1])
	}
	if tasks[2].Execution != agentparse.ExecutionHumanRequired {
		t.Fatalf("T3 execution = %q", tasks[2].Execution)
	}
	// A task without a ref is numbered so dependencies still resolve.
	rendered := RenderTaskList(Plan{Tasks: []PlanTask{{Title: "Untagged"}}})
	if !strings.Contains(rendered, "[T1] Untagged") {
		t.Fatalf("rendered = %q", rendered)
	}

	for name, ex := range map[string]*scriptedExec{
		"bad json":   {exportJSON: `{`},
		"wrong name": {exportJSON: `{"kind":"plan","name":"other","tasks":[{"title":"x"}]}`},
		"no tasks":   {exportJSON: `{"kind":"plan","name":"` + testRunKey + `","tasks":[]}`},
		"exec error": {exportErr: errors.New("exit status 2")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Runner{Exec: ex.exec}).ExportPlan(context.Background(), testRunKey); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := r.ExportPlan(context.Background(), ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestPlanFallbackParsers(t *testing.T) {
	jsonPlan, err := parsePlanJSON(testRunKey, []byte(`{"tasks":[{"id":"T1","title":"Add API","repo":"hivecommons/hive"},{"id":"T2","title":"Wire runner","depends_on":["T1"]}]}`), "tasks.json")
	if err != nil {
		t.Fatalf("parsePlanJSON: %v", err)
	}
	if jsonPlan.Name != testRunKey || jsonPlan.Tasks[0].Ref != "" || jsonPlan.Tasks[0].ID != "T1" || jsonPlan.Tasks[0].Repo != "hivecommons/hive" {
		t.Fatalf("json plan = %+v", jsonPlan)
	}
	rendered := RenderTaskList(jsonPlan)
	if !strings.Contains(rendered, "[T1] Add API") || !strings.Contains(rendered, "[T2] Wire runner (depends: T1)") {
		t.Fatalf("rendered json-id plan = %q", rendered)
	}

	md := []byte(`---
document_status: final
---

## Tasks

- [T1] Add API (repo: hivecommons/hive) [agent_suitable]
2. [T2] Wire runner (depends: T1) [human_required]
- [ ] Write docs (depends: T1, T2)
`)
	plan, err := ParsePlanMarkdown(testRunKey+"/plan.md", md)
	if err != nil {
		t.Fatalf("ParsePlanMarkdown: %v", err)
	}
	if plan.Name != testRunKey || len(plan.Tasks) != 3 {
		t.Fatalf("markdown plan = %+v", plan)
	}
	if plan.Tasks[0].Ref != "T1" || plan.Tasks[0].Repo != "hivecommons/hive" || plan.Tasks[0].Execution != "agent_suitable" {
		t.Fatalf("T1 = %+v", plan.Tasks[0])
	}
	if plan.Tasks[1].Execution != "human_required" || len(plan.Tasks[1].DependsOn) != 1 || plan.Tasks[1].DependsOn[0] != "T1" {
		t.Fatalf("T2 = %+v", plan.Tasks[1])
	}
	if plan.Tasks[2].Ref != "T3" || len(plan.Tasks[2].DependsOn) != 2 {
		t.Fatalf("T3 = %+v", plan.Tasks[2])
	}
	if _, err := ParsePlanMarkdown(testRunKey, []byte("# prose only")); err == nil {
		t.Fatal("prose-only plan.md parsed as tasks")
	}
}

// --- Tick: the poll loop --------------------------------------------------

func TestTick_DraftThenFinalAdvancesOnceAndWritesOneReceipt(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{
		statusJSON(KindSpec, testRunKey, DocumentDraft),
		statusJSON(KindSpec, testRunKey, DocumentDraft),
		statusJSON(KindSpec, testRunKey, DocumentFinal),
	}}
	esc := &escalations{}
	r := newRunner(reg, ex, esc)

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
	if len(esc.events) != 0 || len(reg.retries) != 0 || len(reg.refusals) != 0 {
		t.Fatalf("unexpected side effects: esc=%d retries=%d refusals=%v", len(esc.events), len(reg.retries), reg.refusals)
	}

	// The next tick polls the NEW stage (plan) under the new generation, and the
	// old spec stage is never advanced twice.
	ex.statuses = append(ex.statuses, statusJSON(KindPlan, testRunKey, DocumentDraft))
	ex.idx = len(ex.statuses) - 1
	res := r.Tick(context.Background(), now)
	if res.Advanced != 0 || res.Polled != 1 || len(reg.advances) != 1 {
		t.Fatalf("plan-stage tick = %+v advances=%d", res, len(reg.advances))
	}
	last := ex.calls[len(ex.calls)-1]
	if last[0] != KindPlan {
		t.Fatalf("next poll asked for %q, want plan", last[0])
	}
}

func TestTick_NeverFinalRetriesOnceThenEscalatesWithNoThirdGeneration(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentDraft)}}
	esc := &escalations{}
	r := newRunner(reg, ex, esc)

	if res := r.Tick(context.Background(), t0); res.Retried != 0 || res.Escalated != 0 {
		t.Fatalf("first tick before expiry = %+v", res)
	}
	// First expiry: reclaim mints ONE retry generation.
	now := t0.Add(testLeaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Retried != 1 || res.Escalated != 0 {
		t.Fatalf("first expiry = %+v", res)
	}
	if reg.stage.Gen != 2 || len(reg.retries) != 1 {
		t.Fatalf("after first expiry gen=%d retries=%d", reg.stage.Gen, len(reg.retries))
	}
	// Second expiry: budget (2) exhausted, escalation raised, no third generation.
	now = now.Add(testLeaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Retried != 0 || res.Escalated != 1 {
		t.Fatalf("second expiry = %+v", res)
	}
	if len(esc.events) != 1 {
		t.Fatalf("escalations = %d, want 1", len(esc.events))
	}
	ev := esc.events[0]
	if ev.Severity != escalate.SeverityDecision || ev.RunKey != testRunKey || ev.Stage != StagePlan || ev.Gen != 2 || ev.Attempts != 2 || !ev.At.Equal(now) {
		t.Fatalf("escalation = %+v", ev)
	}
	// Any later tick is inert: no third generation, no second escalation.
	now = now.Add(testLeaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Polled != 0 || res.Retried != 0 || res.Escalated != 0 {
		t.Fatalf("post-escalation tick = %+v", res)
	}
	if reg.stage.Gen != 2 || len(reg.retries) != 1 || len(esc.events) != 1 || len(reg.advances) != 0 {
		t.Fatalf("third generation minted: gen=%d retries=%d esc=%d", reg.stage.Gen, len(reg.retries), len(esc.events))
	}
}

func TestTick_PlanFinalImportsStructuredPlanAsDraft(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportJSON: exportJSON}
	esc := &escalations{}
	r := newRunner(reg, ex, esc)

	if res := r.Tick(context.Background(), t0); res.Advanced != 1 {
		t.Fatalf("plan final tick = %+v", res)
	}
	if len(reg.plans) != 1 || reg.plans[0] == nil || len(reg.plans[0].Tasks) != 3 {
		t.Fatalf("plan import = %+v", reg.plans)
	}
	if reg.stage.Stage != StageImplement {
		t.Fatalf("stage after plan final = %q, want implement", reg.stage.Stage)
	}
	// The implement stage has no Spektacular document: the runner never polls it.
	before := ex.statusCalls()
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 0 || ex.statusCalls() != before {
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

func TestTick_PlanFinalFallsBackWhenExportVerbIsMissing(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{
		statuses:   []string{statusJSON(KindPlan, testRunKey, DocumentFinal)},
		exportJSON: `{"error":true,"code":"unknown_subcommand","message":"unknown subcommand \"export\" for \"spektacular plan\""}`,
		exportErr:  errors.New("exit status 1"),
		files: map[string]string{
			testRunKey + "/tasks.json": `{"name":"` + testRunKey + `","tasks":[{"id":"T1","title":"Read task JSON","repo":"hivecommons/hive"},{"id":"T2","title":"Advance plan","depends_on":["T1"]}]}`,
		},
	}
	r := newRunner(reg, ex, &escalations{})
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("plan final fallback tick = %+v", res)
	}
	if reg.stage.Stage != StageImplement {
		t.Fatalf("stage after fallback = %q, want implement", reg.stage.Stage)
	}
	if len(reg.plans) != 1 || len(reg.plans[0].Tasks) != 2 || reg.plans[0].Tasks[0].ID != "T1" || reg.plans[0].Tasks[0].Repo != "hivecommons/hive" {
		t.Fatalf("fallback plan = %+v", reg.plans)
	}
	var sawExport, sawTasks bool
	for _, call := range ex.calls {
		if strings.Join(call, " ") == "plan export "+testRunKey+" --format json" {
			sawExport = true
		}
		if strings.Join(call, " ") == "plan file read "+testRunKey+"/tasks.json" {
			sawTasks = true
		}
	}
	if !sawExport || !sawTasks {
		t.Fatalf("calls = %+v, want export then tasks.json fallback", ex.calls)
	}
}

func TestTick_PlanFinalFallsBackToPlanMarkdown(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{
		statuses:   []string{statusJSON(KindPlan, testRunKey, DocumentFinal)},
		exportJSON: `{"error":true,"code":"unknown_subcommand","message":"unknown subcommand \"export\" for \"spektacular plan\""}`,
		exportErr:  errors.New("exit status 1"),
		files: map[string]string{
			testRunKey + "/plan.md": "- [T1] Add markdown fallback [agent_suitable]\n- [T2] Advance implement (depends: T1)\n",
		},
	}
	r := newRunner(reg, ex, &escalations{})
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("plan.md fallback tick = %+v", res)
	}
	if len(reg.plans) != 1 || len(reg.plans[0].Tasks) != 2 || reg.plans[0].Tasks[1].DependsOn[0] != "T1" {
		t.Fatalf("markdown fallback plan = %+v", reg.plans)
	}
}

func TestTick_PlanFinalWithFailedExportDoesNotAdvance(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportErr: errors.New("exit status 2")}
	r := newRunner(reg, ex, &escalations{})
	if res := r.Tick(context.Background(), t0); res.Advanced != 0 || res.Errors != 1 {
		t.Fatalf("tick = %+v", res)
	}
	if len(reg.advances) != 0 {
		t.Fatal("advanced without a plan export")
	}
}

func TestTick_StalePlanStatusRefusesImplementAdvance(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentStale)}, exportJSON: exportJSON}
	r := newRunner(reg, ex, &escalations{})

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
	ex := &scriptedExec{statuses: []string{
		statusJSON(KindPlan, testRunKey, DocumentFinal),
		statusJSON(KindPlan, testRunKey, DocumentDraft),
	}, exportJSON: exportJSON}
	r := newRunner(reg, ex, &escalations{})

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
	// Refused stages are parked: not polled, not retried, not escalated even
	// past expiry, until the generation changes.
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL*3)); res.Polled != 0 || res.Retried != 0 || res.Escalated != 0 {
		t.Fatalf("parked tick = %+v", res)
	}
	if len(reg.advances) != 0 {
		t.Fatal("stale plan advanced")
	}
	// An operator reset (new generation) lets the runner look again.
	reg.stage.Gen = 9
	reg.stage.ExpiresAt = t0.Add(testLeaseTTL * 4)
	ex.statuses = []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}
	ex.idx = 0
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL*3+testPoll)); res.Advanced != 1 {
		t.Fatalf("post-reset tick = %+v", res)
	}
}

func TestTick_VanishedAfterObservationRefusesRebind(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft), notFoundJSON}}
	esc := &escalations{}
	r := newRunner(reg, ex, esc)
	r.Tick(context.Background(), t0)
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Refused != 1 {
		t.Fatalf("vanished tick = %+v", res)
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefuseReplacedDocument {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	if len(reg.advances) != 0 || len(reg.retries) != 0 || len(esc.events) != 0 {
		t.Fatal("a replaced document must not advance, retry, or escalate on its own")
	}
}

func TestTick_MissingFromTheStartIsAnErrorAndStillExpires(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{notFoundJSON}}
	esc := &escalations{}
	r := &Runner{Exec: ex.exec, Poll: testPoll, MaxRetries: 1, Registry: reg, Escalate: esc.record, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Errors != 1 || res.Refused != 0 {
		t.Fatalf("missing tick = %+v", res)
	}
	// Budget of 1: the first expiry escalates straight away.
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL+time.Second)); res.Escalated != 1 || res.Retried != 0 {
		t.Fatalf("expiry tick = %+v", res)
	}
	if len(esc.events) != 1 || len(reg.refusals) != 0 {
		t.Fatalf("esc=%d refusals=%v", len(esc.events), reg.refusals)
	}
}

func TestTick_PollIntervalAndHousekeeping(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, ex, &escalations{})
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
	// Retry failures are counted and leave the budget untouched for next time.
	reg2 := newFakeRegistry(StageSpec)
	reg2.retryErr = errors.New("persist failed")
	r2 := newRunner(reg2, &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}, &escalations{})
	if res := r2.Tick(context.Background(), t0.Add(testLeaseTTL+time.Second)); res.Errors != 1 || res.Retried != 0 {
		t.Fatalf("retry failure tick = %+v", res)
	}
	// Nil receivers and a runner without a registry are inert.
	var nilRunner *Runner
	if res := nilRunner.Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("nil runner tick = %+v", res)
	}
	if res := (&Runner{}).Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("registry-less tick = %+v", res)
	}
	if (&Runner{}).maxRetries() != DefaultMaxRetries || (&Runner{MaxRetries: 4}).maxRetries() != 4 {
		t.Fatal("maxRetries default/override wrong")
	}
	if (&Runner{}).logger() == nil {
		t.Fatal("logger() returned nil")
	}
	if next := nextStage(StageImplement); next != "" {
		t.Fatalf("nextStage(implement) = %q", next)
	}
}

func TestRun_TicksUntilCancelled(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, ex, &escalations{})
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
	(&Runner{Registry: reg}).Run(ctx2, nil)
}

// --- Receipt ---------------------------------------------------------------

func TestBuildReceipt_FallsBackWhenStatusLacksTimes(t *testing.T) {
	st := Stage{RunKey: testRunKey, Artifact: testRunKey, Stage: StageSpec, TaskID: testTaskID, Gen: 3}
	receipt := BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal}, t0)
	validateReceipt(t, receipt)
	if receipt.WorkKey != testRunKey || receipt.StartedAt != t0.Format(time.RFC3339Nano) || receipt.EndedAt != receipt.StartedAt {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Artifacts[0].Repo != testRunKey {
		t.Fatalf("artifact repo fell back to %q", receipt.Artifacts[0].Repo)
	}
	// updated_at is a file mtime whenever no workflow state matches the
	// artifact, so it never stands in for closed_at: without closed_at the
	// receipt ends at the advance instant.
	updated := t0.Add(time.Hour)
	now := t0.Add(2 * time.Hour)
	receipt = BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, UpdatedAt: updated}, now)
	if receipt.EndedAt != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("EndedAt = %s, want the advance instant, never updated_at", receipt.EndedAt)
	}
	// Two observations of the same final document that differ only in
	// updated_at (a checkout or reformat moved the mtime) are the same input.
	a := BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, UpdatedAt: updated, ClosedAt: t0.Add(time.Hour)}, now)
	b := BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, ClosedAt: t0.Add(time.Hour)}, now)
	if a.InputRevision != b.InputRevision {
		t.Fatalf("InputRevision moved with updated_at: %s vs %s", a.InputRevision, b.InputRevision)
	}
	withID := BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: "000057_old", ArtifactID: "20260922132517-a1b2c3d4-new", DocumentStatus: DocumentFinal, CreatedAt: t0, ClosedAt: t0.Add(time.Hour)}, now)
	if withID.Artifacts[0].Path != "spec/20260922132517-a1b2c3d4-new" || withID.InputRevision == a.InputRevision {
		t.Fatalf("artifact_id was not used as receipt join key: %+v", withID)
	}
	if !strings.Contains(a.Provenance.Query, "spektacular spec status "+testRunKey) || strings.Contains(a.Provenance.Query, "--json") {
		t.Fatalf("provenance query = %q", a.Provenance.Query)
	}
}

// TestUpdatedAtNeverDecides pins the contract answer from spektacular#45:
// updated_at is a file mtime whenever no workflow state matches the artifact,
// so no progress or staleness decision in this package may read it. Only
// runner.go (the parser) may mention the field.
func TestUpdatedAtNeverDecides(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "runner.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "UpdatedAt") {
			t.Fatalf("%s reads updated_at; progress is document_status/current_step/completed_steps and staleness is the lease clock", name)
		}
	}
}

// --- Fixture: the scripted CLI through BinaryExec -------------------------

func fixtureExec(t *testing.T, scenario string) ExecFunc {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "spektacular-fake", "spektacular"))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "calls")
	inner := BinaryExec(script)
	return func(ctx context.Context, args []string) ([]byte, error) {
		t.Setenv("SPEK_FAKE_SCENARIO", scenario)
		t.Setenv("SPEK_FAKE_STATE", state)
		return inner(ctx, args)
	}
}

func TestFixture_DraftFinalThroughBinaryExec(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	r := &Runner{Exec: fixtureExec(t, "draft-final"), Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var advanced int
	for i := 0; i < 3; i++ {
		advanced += r.Tick(context.Background(), t0.Add(time.Duration(i)*testPoll)).Advanced
	}
	if advanced != 1 || reg.stage.Stage != StagePlan {
		t.Fatalf("fixture draft-final: advanced=%d stage=%q", advanced, reg.stage.Stage)
	}
	// The plan export assumption round-trips through the real CLI boundary too.
	plan, err := r.ExportPlan(context.Background(), testRunKey)
	if err != nil || len(plan.Tasks) != 3 {
		t.Fatalf("fixture export: %v %+v", err, plan)
	}
}

func TestFixture_MissingIsTypedNotFound(t *testing.T) {
	r := &Runner{Exec: fixtureExec(t, "missing")}
	_, err := r.Status(context.Background(), KindPlan, testRunKey)
	var nf *NotFoundError
	if !errors.As(err, &nf) || !strings.Contains(nf.Message, "was not found") {
		t.Fatalf("fixture missing: err = %v (%T)", err, err)
	}
}

func TestFixture_FinalWithoutUpdatedAtAdvances(t *testing.T) {
	// The fixture drops updated_at for a closed artifact with no matching
	// workflow state; the advance is decided on document_status alone.
	reg := newFakeRegistry(StageSpec)
	r := &Runner{Exec: fixtureExec(t, "final-no-updated-at"), Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("fixture final-no-updated-at: %+v", res)
	}
	if len(reg.receipts) != 1 || reg.receipts[0].EndedAt != "2026-09-22T00:00:00Z" {
		t.Fatalf("receipt ended at closed_at expected, got %+v", reg.receipts)
	}
}

func TestFixture_LeaseSpelledAsFileAddressPollsBareName(t *testing.T) {
	// The fixture answers artifact_not_found for `<name>.md`, exactly as the
	// real store does, so an advance proves the runner asked by bare name.
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = testRunKey + ".md"
	reg.stage.Artifact = ArtifactKey(reg.stage.RunKey)
	r := &Runner{Exec: fixtureExec(t, "final-no-updated-at"), Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("fixture .md lease: %+v", res)
	}
	if _, err := (&Runner{Exec: fixtureExec(t, "draft-final")}).Status(context.Background(), KindSpec, testRunKey+"/plan.md"); err != nil {
		t.Fatalf("plan-path spelling through the fixture: %v", err)
	}
}

func TestFixture_FinalThenDraftAndNeverFinal(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	reg.advanceErr = errors.New("persist failed")
	r := &Runner{Exec: fixtureExec(t, "final-then-draft"), Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.Tick(context.Background(), t0)
	reg.advanceErr = nil
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Refused != 1 {
		t.Fatalf("fixture final-then-draft: %+v", res)
	}

	reg2 := newFakeRegistry(StageSpec)
	esc := &escalations{}
	r2 := &Runner{Exec: fixtureExec(t, "never-final"), Poll: testPoll, Registry: reg2, Escalate: esc.record, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := t0
	for i := 0; i < 3; i++ {
		now = now.Add(testLeaseTTL + time.Second)
		r2.Tick(context.Background(), now)
	}
	if len(reg2.retries) != 1 || len(esc.events) != 1 || reg2.stage.Gen != 2 {
		t.Fatalf("fixture never-final: retries=%d esc=%d gen=%d", len(reg2.retries), len(esc.events), reg2.stage.Gen)
	}
}

func TestBinaryExec_ReturnsStdoutOnFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out, err := BinaryExec("sh")(context.Background(), []string{"-c", `printf '{"error":"x"}'; echo oops >&2; exit 3`})
	if err == nil || !strings.Contains(string(out), `"error"`) || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if out, err := BinaryExec("sh")(context.Background(), []string{"-c", "printf ok"}); err != nil || string(out) != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// --- Invariant: Hive never opens a Spektacular file -----------------------

// TestNoDirectFileAccess scans this package's non-test sources: every fact
// about an artifact must arrive through Exec. The fixture directory is the
// only place a Spektacular-shaped file exists, and nothing here reads it.
func TestNoDirectFileAccess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"os.Open", "os.ReadFile", "os.ReadDir", "os.OpenFile", "filepath.Walk", "ioutil.Read", "testdata"}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Fatalf("%s reaches for %q; Hive must only learn about Spektacular artifacts through Exec", name, f)
			}
		}
	}
}

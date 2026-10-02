package spektacular

import (
	"bytes"
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

// scriptedExec answers status calls from a fixed sequence (the last entry
// repeats) and plan export calls from exportJSON; it records every args slice.
type scriptedExec struct {
	mu         sync.Mutex
	statuses   []string
	exportJSON string
	exportErr  error
	listJSON   string
	files      map[string]string
	calls      [][]string
	dirs       []string
	idx        int
}

func (s *scriptedExec) exec(_ context.Context, dir string, args []string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string(nil), args...))
	s.dirs = append(s.dirs, dir)
	if len(args) >= 2 && args[1] == verbExport {
		if s.exportErr != nil {
			if s.exportJSON != "" {
				return []byte(s.exportJSON), s.exportErr
			}
			return nil, s.exportErr
		}
		return []byte(s.exportJSON), nil
	}
	if len(args) >= 3 && args[1] == verbFile && args[2] == "list" {
		if s.listJSON != "" {
			return []byte(s.listJSON), nil
		}
		return []byte(`{"error":true,"code":"not_found","message":"no artifacts"}`), errors.New("exit status 1")
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

func newRunner(reg Registry, ex *scriptedExec) *Runner {
	return &Runner{
		Exec:     ex.exec,
		Poll:     testPoll,
		Registry: reg,
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

func TestRunArtifactName(t *testing.T) {
	cases := map[string]string{
		"KubeStellar/Console#23735": "kubestellar-console-23735",
		"owner/repo#1":              "owner-repo-1",
		"owner/repo!ENG-7":          "owner-repo-eng-7",
		"000057_git-commit.md":      "000057_git-commit",
	}
	for in, want := range cases {
		if got := RunArtifactName(in); got != want {
			t.Fatalf("RunArtifactName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveArtifactAcceptsTimestampedIDs(t *testing.T) {
	ex := &scriptedExec{listJSON: `["20260925160000-kubestellar-console-23725.md","20260925163042-kubestellar-console-23725.md","other.md"]`}
	r := &Runner{Exec: ex.exec}
	got, err := r.ResolveArtifact(context.Background(), ".", KindSpec, "kubestellar-console-23725")
	if err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	if got != "20260925163042-kubestellar-console-23725" {
		t.Fatalf("resolved = %q", got)
	}
}

func TestTickResolvesTimestampedArtifactAfterNotFound(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = "kubestellar-console-23725"
	var calls [][]string
	exec := func(_ context.Context, _ string, args []string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 3 && args[1] == verbFile && args[2] == "list" {
			return []byte(`["20260925163042-kubestellar-console-23725.md"]`), nil
		}
		if len(args) >= 3 && args[1] == verbStatus && args[2] == "kubestellar-console-23725" {
			return []byte(`{"error":true,"code":"artifact_not_found","message":"missing","resource":"kubestellar-console-23725"}`), errors.New("exit status 1")
		}
		if len(args) >= 3 && args[1] == verbStatus && args[2] == "20260925163042-kubestellar-console-23725" {
			return []byte(statusJSON(KindSpec, "20260925163042-kubestellar-console-23725", DocumentFinal)), nil
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	r := &Runner{Exec: exec, Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if len(calls) < 3 || calls[2][2] != "20260925163042-kubestellar-console-23725" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestTickReresolvesArtifactAfterGenerationChange(t *testing.T) {
	// Generation 1 resolves an older timestamped document that never goes
	// final; the retry generation leaves a newer document in the same work
	// dir. The runner must re-resolve rather than keep polling the old id.
	const oldID = "20260925160000-kubestellar-console-23725"
	const newID = "20260925163042-kubestellar-console-23725"
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = "kubestellar-console-23725"
	var mu sync.Mutex
	listing := `["` + oldID + `.md"]`
	var statused []string
	exec := func(_ context.Context, _ string, args []string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(args) >= 3 && args[1] == verbFile && args[2] == "list" {
			return []byte(listing), nil
		}
		if len(args) >= 3 && args[1] == verbStatus {
			statused = append(statused, args[2])
			switch args[2] {
			case oldID:
				return []byte(statusJSON(KindSpec, oldID, DocumentDraft)), nil
			case newID:
				return []byte(statusJSON(KindSpec, newID, DocumentFinal)), nil
			}
			return []byte(`{"error":true,"code":"artifact_not_found","message":"missing","resource":"` + args[2] + `"}`), errors.New("exit status 1")
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	r := &Runner{Exec: exec, Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 0 || res.Errors != 0 {
		t.Fatalf("gen1 tick = %+v", res)
	}
	mu.Lock()
	listing = `["` + oldID + `.md","` + newID + `.md"]`
	mu.Unlock()
	reg.mu.Lock()
	reg.stage.Gen = 2
	reg.mu.Unlock()
	if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("gen2 tick = %+v (status calls %v)", res, statused)
	}
	if last := statused[len(statused)-1]; last != newID {
		t.Fatalf("gen2 polled %q, want %q (status calls %v)", last, newID, statused)
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
		"unknown status":  {statusJSON(KindSpec, testRunKey, DocumentStatus("withdrawn")), new(*ContractError)},
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
	r := newRunner(reg, ex)

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

func TestTick_PlanFinalImportsStructuredPlanAsDraft(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportJSON: exportJSON}
	r := newRunner(reg, ex)

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
	r := newRunner(reg, ex)
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
	r := newRunner(reg, ex)
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("plan.md fallback tick = %+v", res)
	}
	if len(reg.plans) != 1 || len(reg.plans[0].Tasks) != 2 || reg.plans[0].Tasks[1].DependsOn[0] != "T1" {
		t.Fatalf("markdown fallback plan = %+v", reg.plans)
	}
}

// spekNativePlanMD mirrors the plan.md Spektacular 0.22 wrote for
// kubestellar/console#23725 on the hosted hive: phases are headings, every
// other bullet is prose or acceptance criteria.
const spekNativePlanMD = `---
created_date: "2026-09-25"
document_status: final
---

# Plan: kubestellar-console-23725

## Component Breakdown

- **` + "`dashboards.Registrar`" + `** (new) — implements the contract.
- **Five ` + "`_aliases.go`" + ` shim files** — deleted.

## Milestones & Phases

### Milestone 1: Dashboard wiring no longer depends on shims

#### - [ ] Phase 1.1: Port the dashboards domain to ` + "`handlers.Registrar`" + `

**Repo:** console

**Acceptance criteria**:
- [ ] Every dashboard endpoint responds as before.
- [ ] The shim file no longer exists.

#### - [ ] Phase 1.2: Port the persistence domain

**Repo:** kubestellar/console

- [ ] The persistence test suite passes unmodified.

### Milestone 2: Route assembly is uniform

#### - [x] Phase 2.1: Collapse route assembly to a uniform registrar list
`

func TestParsePlanMarkdown_SpektacularNativePhases(t *testing.T) {
	plan, err := ParsePlanMarkdown(testRunKey, []byte(spekNativePlanMD))
	if err != nil {
		t.Fatalf("ParsePlanMarkdown: %v", err)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("phases parsed = %d, want 3 (acceptance/component bullets must not become tasks): %+v", len(plan.Tasks), plan.Tasks)
	}
	p1 := plan.Tasks[0]
	if p1.Ref != "P1.1" || p1.Title != "Port the dashboards domain to `handlers.Registrar`" || p1.Repo != "" || len(p1.DependsOn) != 0 || p1.Execution != "agent_suitable" {
		t.Fatalf("P1.1 = %+v (bare project name must not override the run repo)", p1)
	}
	p2 := plan.Tasks[1]
	if p2.Ref != "P1.2" || p2.Repo != "kubestellar/console" || len(p2.DependsOn) != 1 || p2.DependsOn[0] != "P1.1" {
		t.Fatalf("P1.2 = %+v", p2)
	}
	if p3 := plan.Tasks[2]; p3.Ref != "P2.1" || p3.DependsOn[0] != "P1.2" {
		t.Fatalf("P2.1 = %+v", p3)
	}
	rendered := RenderTaskList(plan)
	if !strings.Contains(rendered, "1. [P1.1] Port the dashboards domain") || !strings.Contains(rendered, "[P1.2] Port the persistence domain [repo:kubestellar/console] (depends: P1.1)") {
		t.Fatalf("rendered = %q", rendered)
	}
}

// Spektacular 0.22 has no `plan export`; cobra swallows "export" as a
// positional and rejects the flag with an internal_error envelope. That must
// route to the on-disk fallback rather than blocking the run.
func TestTick_PlanFinalFallsBackWhenExportFlagIsRejected(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{
		statuses:   []string{statusJSON(KindPlan, testRunKey, DocumentFinal)},
		exportJSON: `{"error":true,"code":"internal_error","message":"unknown flag: --format","next_action":""}`,
		exportErr:  errors.New("exit status 1"),
		files: map[string]string{
			testRunKey + "/plan.md": spekNativePlanMD,
		},
	}
	r := newRunner(reg, ex)
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("plan final fallback tick = %+v", res)
	}
	if reg.stage.Stage != StageImplement {
		t.Fatalf("stage after fallback = %q, want implement", reg.stage.Stage)
	}
	if len(reg.plans) != 1 || len(reg.plans[0].Tasks) != 3 || reg.plans[0].Tasks[0].Ref != "P1.1" {
		t.Fatalf("fallback plan = %+v", reg.plans)
	}
}

// The live shape: the lease names the bare slug, Spektacular reports the
// timestamp-prefixed artifact id, and the on-disk plan.md lives under that
// id. The fallback must read the resolved id, not the slug.
func TestTick_PlanFallbackReadsResolvedArtifactID(t *testing.T) {
	const slug, resolved = "kubestellar-console-23725", "20260925183347-kubestellar-console-23725"
	reg := newFakeRegistry(StagePlan)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = slug
	var reads []string
	exec := func(_ context.Context, _ string, args []string) ([]byte, error) {
		switch {
		case len(args) >= 3 && args[1] == verbFile && args[2] == "list":
			return []byte(`["` + resolved + `"]`), nil
		case len(args) >= 3 && args[1] == verbStatus && args[2] == slug:
			return []byte(`{"error":true,"code":"artifact_not_found","message":"missing","resource":"` + slug + `"}`), errors.New("exit status 1")
		case len(args) >= 3 && args[1] == verbStatus && args[2] == resolved:
			return []byte(statusJSON(KindPlan, resolved, DocumentFinal)), nil
		case len(args) >= 2 && args[1] == verbExport:
			return []byte(`{"error":true,"code":"internal_error","message":"unknown flag: --format","next_action":""}`), errors.New("exit status 1")
		case len(args) >= 4 && args[1] == verbFile && args[2] == verbRead:
			reads = append(reads, args[3])
			if args[3] == resolved+"/plan.md" {
				return []byte(spekNativePlanMD), nil
			}
			return []byte(`{"error":true,"code":"not_found","message":"file ` + args[3] + ` not found"}`), errors.New("exit status 1")
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	r := &Runner{Exec: exec, Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v (reads %v)", res, reads)
	}
	if len(reg.plans) != 1 || len(reg.plans[0].Tasks) != 3 {
		t.Fatalf("plan = %+v", reg.plans)
	}
	for _, p := range reads {
		if strings.HasPrefix(p, slug+"/") {
			t.Fatalf("fallback read the bare slug path %q; want %s/…", p, resolved)
		}
	}
}

// Projects configured with `spec.id_method: counter` name artifacts
// `000001_<slug>`; the resolver must find them so a final document advances.
func TestTick_ResolvesCounterArtifactID(t *testing.T) {
	const slug, resolved = "kubestellar-console-23725", "000001_kubestellar-console-23725"
	reg := newFakeRegistry(StageSpec)
	reg.stage.RunKey = "kubestellar/console#23725"
	reg.stage.Artifact = slug
	exec := func(_ context.Context, _ string, args []string) ([]byte, error) {
		switch {
		case len(args) >= 3 && args[1] == verbFile && args[2] == "list":
			return []byte(`["` + resolved + `.md"]`), nil
		case len(args) >= 3 && args[1] == verbStatus && args[2] == slug:
			return []byte(`{"error":true,"code":"artifact_not_found","message":"missing","resource":"` + slug + `"}`), errors.New("exit status 1")
		case len(args) >= 3 && args[1] == verbStatus && args[2] == resolved:
			return []byte(statusJSON(KindSpec, resolved, DocumentFinal)), nil
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
	r := &Runner{Exec: exec, Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r.Tick(context.Background(), t0); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if reg.stage.Stage != StagePlan {
		t.Fatalf("stage = %q, want plan", reg.stage.Stage)
	}
}

func TestArtifactMatchesSlug(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"my-slug", true},
		{"20261002143509-my-slug", true},
		{"000001_my-slug", true},
		{"000001_my-slug.md", true},
		{"000001_other-slug", false},
		{"my-slug-2", false},
	} {
		if got := artifactMatchesSlug(tc.id, "my-slug"); got != tc.want {
			t.Errorf("artifactMatchesSlug(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestStatus_AcceptsEveryCLIDocumentStatus(t *testing.T) {
	for _, status := range []DocumentStatus{DocumentDraft, DocumentFinal, DocumentStale, DocumentSuperseded, DocumentArchived} {
		ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, status)}}
		got, err := (&Runner{Exec: ex.exec}).Status(context.Background(), KindSpec, testRunKey)
		if err != nil || got.DocumentStatus != status {
			t.Fatalf("status %q: got %q err=%v", status, got.DocumentStatus, err)
		}
	}
	// A document without a document_status key is open; the CLI prints "".
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, "")}}
	got, err := (&Runner{Exec: ex.exec}).Status(context.Background(), KindSpec, testRunKey)
	if err != nil || got.DocumentStatus != DocumentDraft {
		t.Fatalf("blank status: got %q err=%v", got.DocumentStatus, err)
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
			ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, status)}, exportJSON: exportJSON}
			r := newRunner(reg, ex)
			if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Errors != 0 || res.Advanced != 0 {
				t.Fatalf("tick = %+v", res)
			}
			if len(reg.refusals) != 1 || reg.refusals[0] != reason {
				t.Fatalf("refusals = %v, want [%s]", reg.refusals, reason)
			}
			if res := r.Tick(context.Background(), t0.Add(testPoll)); res.Polled != 0 || ex.statusCalls() != 1 {
				t.Fatalf("parked tick = %+v status calls = %d", res, ex.statusCalls())
			}
		})
	}
}

func TestTick_PlanFinalWithFailedExportRefusesAndParks(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportErr: errors.New("exit status 2")}
	r := newRunner(reg, ex)
	if res := r.Tick(context.Background(), t0); res.Advanced != 0 || res.Refused != 1 || res.Errors != 0 {
		t.Fatalf("tick = %+v", res)
	}
	if len(reg.advances) != 0 {
		t.Fatal("advanced without a plan export")
	}
	if len(reg.refusals) != 1 || reg.refusals[0] != RefusePlanImportFailed {
		t.Fatalf("refusals = %v", reg.refusals)
	}
	// Parked: later polls neither call the CLI again nor re-refuse.
	calls := len(ex.calls)
	if res := r.Tick(context.Background(), t0.Add(3*testPoll)); res.Polled != 0 || res.Refused != 0 || len(ex.calls) != calls {
		t.Fatalf("parked tick = %+v calls %d -> %d", res, calls, len(ex.calls))
	}
	// A new generation (operator reset or executor retry) looks again.
	reg.stage.Gen = 2
	ex.exportErr = nil
	ex.exportJSON = exportJSON
	if res := r.Tick(context.Background(), t0.Add(4*testPoll)); res.Advanced != 1 {
		t.Fatalf("post-reset tick = %+v", res)
	}
}

func TestTick_PlanImportRejectedByRegistryRefuses(t *testing.T) {
	reg := newFakeRegistry(StagePlan)
	reg.advanceErr = &PlanImportError{RunKey: testRunKey, Artifact: testRunKey, Err: errors.New("no bead store configured for plan import")}
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportJSON: exportJSON}
	r := newRunner(reg, ex)
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
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentStale)}, exportJSON: exportJSON}
	r := newRunner(reg, ex)

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
	r := newRunner(reg, ex)

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
	ex.statuses = []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}
	ex.idx = 0
	if res := r.Tick(context.Background(), t0.Add(testLeaseTTL*3+testPoll)); res.Advanced != 1 {
		t.Fatalf("post-reset tick = %+v", res)
	}
}

func TestTick_VanishedAfterObservationRefusesRebind(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft), notFoundJSON}}
	r := newRunner(reg, ex)
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
	ex := &scriptedExec{statuses: []string{notFoundJSON}}
	r := newRunner(reg, ex)
	if res := r.Tick(context.Background(), t0); res.Errors != 1 || res.Refused != 0 {
		t.Fatalf("missing tick = %+v", res)
	}
	if len(reg.refusals) != 0 || len(reg.advances) != 0 {
		t.Fatalf("refusals=%v advances=%d", reg.refusals, len(reg.advances))
	}
}

func TestTick_PollIntervalAndHousekeeping(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, ex)
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
	// Nil receivers and a runner without a registry are inert.
	var nilRunner *Runner
	if res := nilRunner.Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("nil runner tick = %+v", res)
	}
	if res := (&Runner{}).Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("registry-less tick = %+v", res)
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
	r := newRunner(reg, ex)
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
	dateOnly := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	receipt = BuildReceipt(st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: dateOnly, ClosedAt: dateOnly}, now)
	if receipt.StartedAt == dateOnly.Format(time.RFC3339Nano) || receipt.EndedAt == dateOnly.Format(time.RFC3339Nano) {
		t.Fatalf("receipt used Spektacular date-only frontmatter timestamps: %+v", receipt)
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
	return fixtureExecVersion(t, scenario, "0.22.0")
}

// fixtureExecVersion drives the in-tree fake as a specific Spektacular
// release: 0.22 has no `plan export`, never reports `stale` and never emits
// artifact_id, so a test wanting any of those must ask for 0.23+ (#10074).
func fixtureExecVersion(t *testing.T, scenario, version string) ExecFunc {
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
	return func(ctx context.Context, dir string, args []string) ([]byte, error) {
		t.Setenv("SPEK_FAKE_SCENARIO", scenario)
		t.Setenv("SPEK_FAKE_STATE", state)
		t.Setenv("SPEK_FAKE_VERSION", version)
		return inner(ctx, dir, args)
	}
}

// fixtureStore runs the fake's `init` and `<kind> new` verbs in a fresh
// directory and returns the directory plus the ID-prefixed artifact name the
// CLI minted for slug — the id Hive has to resolve, never the slug itself.
func fixtureStore(t *testing.T, kind, slug, version string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := fixtureExecVersion(t, "draft-final", version)
	if _, err := run(context.Background(), dir, []string{"init", "copilot", "--name", "proj"}); err != nil {
		t.Fatalf("fixture init: %v", err)
	}
	out, err := run(context.Background(), dir, []string{kind, "new", "--data", `{"name":"` + slug + `"}`})
	if err != nil {
		t.Fatalf("fixture %s new: %v", kind, err)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &created); err != nil {
		t.Fatalf("fixture %s new output %q: %v", kind, out, err)
	}
	if created.Name == slug || !strings.HasSuffix(created.Name, "-"+slug) {
		t.Fatalf("fixture %s new minted %q, want a timestamped id for %q", kind, created.Name, slug)
	}
	return dir, created.Name
}

// TestFixture_StoreBackedResolutionAndRead drives the resolver and the file
// verbs through the fake binary instead of a scriptedExec stub (#10074): the
// store holds a timestamped id, so the bare slug is artifact_not_found and
// `file list` is what turns it into the real artifact name.
func TestFixture_StoreBackedResolutionAndRead(t *testing.T) {
	dir, id := fixtureStore(t, KindSpec, "hcl-encoding-helpers", "0.22.0")
	r := &Runner{Exec: fixtureExec(t, "draft-final")}
	if _, err := r.statusInDir(context.Background(), dir, KindSpec, "hcl-encoding-helpers"); !isNotFound(err) {
		t.Fatalf("bare slug status against a timestamped store: %v (%T)", err, err)
	}
	resolved, err := r.ResolveArtifact(context.Background(), dir, KindSpec, "hcl-encoding-helpers")
	if err != nil || resolved != id {
		t.Fatalf("ResolveArtifact = %q, %v; want %q", resolved, err, id)
	}
	st, err := r.statusInDir(context.Background(), dir, KindSpec, id)
	if err != nil || st.DocumentStatus != DocumentDraft {
		t.Fatalf("resolved status = %+v, %v", st, err)
	}
	if st.ArtifactID != "" {
		t.Fatalf("0.22 does not emit artifact_id, got %q", st.ArtifactID)
	}
	body, err := r.readSpecInDir(context.Background(), dir, id)
	if err != nil || !strings.Contains(body, id) {
		t.Fatalf("spec file read = %q, %v", body, err)
	}
}

// TestFixture_FileReadExtensionDiffersByVersion pins the retry in
// readFileInDir against both CLIs: 0.22 wants the extension, 0.23+ rejects it
// with unexpected_extension and is served the bare path.
func TestFixture_FileReadExtensionDiffersByVersion(t *testing.T) {
	dir, id := fixtureStore(t, KindSpec, "hcl-encoding-helpers", "0.23.1")
	r := &Runner{Exec: fixtureExecVersion(t, "draft-final", "0.23.1")}
	if _, err := r.readFileOnceInDir(context.Background(), dir, KindSpec, id, id+".md"); !isUnexpectedExtension(err) {
		t.Fatalf("0.23 file read with an extension: %v (%T)", err, err)
	}
	body, err := r.readSpecInDir(context.Background(), dir, id)
	if err != nil || !strings.Contains(body, id) {
		t.Fatalf("0.23 spec read after the retry = %q, %v", body, err)
	}
	st, err := r.statusInDir(context.Background(), dir, KindSpec, id)
	if err != nil || st.ArtifactID != id {
		t.Fatalf("0.23 status = %+v, %v; want artifact_id %q", st, err, id)
	}
}

// TestFixture_PlanExportIsVersionGated pins that the pinned 0.22 CLI has no
// export verb: the runner falls back to the plan document through
// `plan file read`, and only 0.23+ answers the export with UUID task ids.
func TestFixture_PlanExportIsVersionGated(t *testing.T) {
	dir, id := fixtureStore(t, KindPlan, "hcl-encoding-helpers", "0.22.0")
	planDoc := "---\ndocument_status: final\n---\n\n- [T1] Add encoding helpers (repo: myorg/repo1) [agent_suitable]\n- [T2] Wire helpers into the parser (depends: T1) [agent_suitable]\n"
	if err := os.WriteFile(filepath.Join(dir, ".spektacular", "plans", id, "plan.md"), []byte(planDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Exec: fixtureExec(t, "draft-final")}
	if _, err := r.exportPlanInDir(context.Background(), dir, id); !planExportUnavailable(err) {
		t.Fatalf("0.22 plan export: %v (%T)", err, err)
	}
	plan, err := r.exportPlanWithFallbackInDir(context.Background(), dir, id)
	if err != nil || len(plan.Tasks) != 2 || plan.Tasks[0].Repo != testRepo {
		t.Fatalf("0.22 fallback plan = %+v, %v", plan, err)
	}

	r23 := &Runner{Exec: fixtureExecVersion(t, "draft-final", "0.23.1")}
	exported, err := r23.exportPlanInDir(context.Background(), dir, id)
	if err != nil || len(exported.Tasks) != 3 {
		t.Fatalf("0.23 plan export = %+v, %v", exported, err)
	}
	if exported.Tasks[0].Ref != "" || exported.Tasks[0].ID == "" {
		t.Fatalf("0.23 identifies tasks by UUID id, got %+v", exported.Tasks[0])
	}
	if exported.Tasks[1].DependsOn[0] != exported.Tasks[0].ID {
		t.Fatalf("0.23 depends_on must reference task ids: %+v", exported.Tasks)
	}
	if _, err := r23.exportPlanInDir(context.Background(), dir, "hcl-encoding-helpers"); !isNotFound(err) {
		t.Fatalf("0.23 export of an unresolved slug: %v (%T)", err, err)
	}
}

// TestFixture_StaleNeedsAModernCLI pins that the stale document_status is not
// something the pinned release can produce.
func TestFixture_StaleNeedsAModernCLI(t *testing.T) {
	r := &Runner{Exec: fixtureExec(t, "stale-plan")}
	if _, err := r.Status(context.Background(), KindPlan, testRunKey); err == nil {
		t.Fatal("0.22 cannot report stale; the fixture must refuse the scenario")
	}
	st, err := (&Runner{Exec: fixtureExecVersion(t, "stale-plan", "0.23.1")}).Status(context.Background(), KindPlan, testRunKey)
	if err != nil || st.DocumentStatus != DocumentStale {
		t.Fatalf("0.23 stale status = %+v, %v", st, err)
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
	// The plan export assumption round-trips through the real CLI boundary
	// too — on a CLI that has the verb, which 0.22 does not.
	plan, err := (&Runner{Exec: fixtureExecVersion(t, "draft-final", "0.23.1")}).ExportPlan(context.Background(), testRunKey)
	if err != nil || len(plan.Tasks) != 3 {
		t.Fatalf("fixture export: %v %+v", err, plan)
	}
	if plan.Tasks[0].Repo != testRepo || plan.Tasks[0].Execution != "agent_suitable" || plan.Tasks[2].Execution != "human_required" {
		t.Fatalf("fixture export object fields: %+v", plan.Tasks)
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
	if len(reg.receipts) != 1 || reg.receipts[0].EndedAt == "2026-09-22T00:00:00Z" || reg.receipts[0].EndedAt != t0.Format(time.RFC3339Nano) {
		t.Fatalf("receipt should use the advance instant instead of date-only closed_at, got %+v", reg.receipts)
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

	// A never-final document leaves the lease alone: no advance, no refusal,
	// no new generation, however far past the lease window the runner ticks.
	reg2 := newFakeRegistry(StageSpec)
	r2 := &Runner{Exec: fixtureExec(t, "never-final"), Poll: testPoll, Registry: reg2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := t0
	for i := 0; i < 3; i++ {
		res := r2.Tick(context.Background(), now)
		if res.Polled != 1 || res.Advanced != 0 || res.Refused != 0 || res.Errors != 0 {
			t.Fatalf("fixture never-final tick %d: %+v", i, res)
		}
		now = now.Add(testLeaseTTL + time.Second)
	}
	if len(reg2.advances) != 0 || len(reg2.refusals) != 0 || reg2.stage.Gen != 1 {
		t.Fatalf("fixture never-final: advances=%d refusals=%v gen=%d", len(reg2.advances), reg2.refusals, reg2.stage.Gen)
	}
}

func TestFixture_SupersededAndArchivedPark(t *testing.T) {
	for scenario, reason := range map[string]string{"superseded": RefuseReplacedDocument, "archived": RefuseArchivedDocument} {
		reg := newFakeRegistry(StagePlan)
		r := &Runner{Exec: fixtureExec(t, scenario), Poll: testPoll, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if res := r.Tick(context.Background(), t0); res.Refused != 1 || res.Errors != 0 || res.Advanced != 0 {
			t.Fatalf("fixture %s: %+v", scenario, res)
		}
		if len(reg.refusals) != 1 || reg.refusals[0] != reason {
			t.Fatalf("fixture %s: refusals = %v", scenario, reg.refusals)
		}
	}
}

func TestBinaryExec_ReturnsStdoutOnFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out, err := BinaryExec("sh")(context.Background(), "", []string{"-c", `printf '{"error":"x"}'; echo oops >&2; exit 3`})
	if err == nil || !strings.Contains(string(out), `"error"`) || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if out, err := BinaryExec("sh")(context.Background(), "", []string{"-c", "printf ok"}); err != nil || string(out) != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// --- Invariant: Hive never reads Spektacular document bodies ---------------

// TestNoDirectSpekDocumentReads scans this package's non-test sources: status
// facts still arrive through Exec. The v0.22 timestamped-id fallback may walk
// project directories to discover artifact names, but must not read document
// bodies or test fixtures.
func TestNoDirectFileAccess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"os.Open", "os.ReadFile", "os.ReadDir", "os.OpenFile", "ioutil.Read", "testdata"}
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
				t.Fatalf("%s reaches for %q; Hive must not read Spektacular document bodies directly", name, f)
			}
		}
	}
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
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentFinal)}}
	r := newRunner(reg, ex)

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
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, ex)

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

func TestBinaryExec_Deadline(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		parentTimeout, execTimeout time.Duration
	}{
		{"per invocation", time.Hour, 50 * time.Millisecond},
		{"earlier caller", 50 * time.Millisecond, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.parentTimeout)
			defer cancel()
			start := time.Now()
			out, err := binaryExec("sh", tc.execTimeout, 20*time.Millisecond)(ctx, "", []string{"-c", "printf partial; exec sleep 5"})
			if !errors.Is(err, context.DeadlineExceeded) || string(out) != "partial" {
				t.Fatalf("got %q, %v; want partial stdout and deadline exceeded", out, err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("invocation did not return promptly")
			}
		})
	}
}

func TestBinaryExec_BoundsInheritedOutputPipes(t *testing.T) {
	start := time.Now()
	out, err := binaryExec("sh", time.Second, 20*time.Millisecond)(context.Background(), "", []string{"-c", "sleep 5 & echo $!"})
	// The child holds stdout/stderr open after the shell exits. Clean it up.
	var pid int
	if _, scanErr := fmt.Sscan(string(out), &pid); scanErr != nil {
		t.Fatalf("child pid: %q: %v", out, scanErr)
	}
	child, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	defer child.Kill()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("got %v, want exec.ErrWaitDelay", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("inherited output pipes blocked invocation")
	}
}

func TestTick_CanceledContextSkipsPolling(t *testing.T) {
	reg := newFakeRegistry(StageSpec)
	ex := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}
	r := newRunner(reg, ex)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Tick(ctx, t0)
	if ex.statusCalls() != 0 {
		t.Fatal("canceled tick still polled a stage")
	}
}

// Spektacular 0.23+ exports task repo and execution as objects; the pre-0.23
// string shape keeps decoding too.
func TestExportPlan_AcceptsObjectRepoAndExecution(t *testing.T) {
	const export023 = `{"kind":"plan","name":"` + testRunKey + `","tasks":[` +
		`{"ref":"T1","title":"A","repo":{"name":"hivecommons/hive","location":"/w/hive"},"execution":{"type":"agent_suitable","reason":"code"}},` +
		`{"ref":"T2","title":"B","repo":{"name":"hive","location":"https://github.com/hivecommons/hive.git"},"execution":{"type":"human_required","reason":"sign-off"},"depends_on":["T1"]},` +
		`{"ref":"T3","title":"C","repo":{"name":"hive","location":"/w/hive"},"execution":null},` +
		`{"ref":"T4","title":"D","repo":"myorg/repo1","execution":"agent_suitable"}]}`
	r := &Runner{Exec: (&scriptedExec{exportJSON: export023}).exec}
	plan, err := r.ExportPlan(context.Background(), testRunKey)
	if err != nil {
		t.Fatalf("ExportPlan: %v", err)
	}
	want := []PlanTask{
		{Ref: "T1", Title: "A", Repo: "hivecommons/hive", Execution: "agent_suitable"},
		{Ref: "T2", Title: "B", Repo: "hivecommons/hive", Execution: "human_required", DependsOn: []string{"T1"}},
		{Ref: "T3", Title: "C", Repo: "hive"},
		{Ref: "T4", Title: "D", Repo: "myorg/repo1", Execution: "agent_suitable"},
	}
	if len(plan.Tasks) != len(want) {
		t.Fatalf("tasks = %+v", plan.Tasks)
	}
	for i, w := range want {
		got := plan.Tasks[i]
		if got.Ref != w.Ref || got.Title != w.Title || got.Repo != w.Repo || got.Execution != w.Execution || strings.Join(got.DependsOn, ",") != strings.Join(w.DependsOn, ",") {
			t.Fatalf("task %d = %+v, want %+v", i, got, w)
		}
	}
	for name, body := range map[string]string{
		"bad repo":      `{"tasks":[{"ref":"T1","title":"A","repo":7}]}`,
		"bad execution": `{"tasks":[{"ref":"T1","title":"A","execution":[1]}]}`,
		"bad task":      `{"tasks":[{"ref":1}]}`,
	} {
		r := &Runner{Exec: (&scriptedExec{exportJSON: body}).exec}
		var ce *ContractError
		if _, err := r.ExportPlan(context.Background(), testRunKey); !errors.As(err, &ce) {
			t.Fatalf("%s: err = %v, want ContractError", name, err)
		}
	}
}

func TestPlanTaskRepo(t *testing.T) {
	for _, tc := range []struct{ name, location, want string }{
		{"owner/repo", "", "owner/repo"},
		{"repo", "git@github.com:owner/repo.git", "owner/repo"},
		{"repo", "github.com/owner/repo/", "owner/repo"},
		{"repo", "/abs/path/repo", "repo"},
		{"repo", "", "repo"},
	} {
		if got := planTaskRepo(tc.name, tc.location); got != tc.want {
			t.Errorf("planTaskRepo(%q, %q) = %q, want %q", tc.name, tc.location, got, tc.want)
		}
	}
}

// unexpectedExtensionExec mimics Spektacular 0.23+ `file read`: extension-
// bearing paths are rejected and the extension-less address is served.
func unexpectedExtensionExec(files map[string]string, reads *[]string) ExecFunc {
	return func(_ context.Context, _ string, args []string) ([]byte, error) {
		if len(args) >= 2 && args[1] == verbExport {
			return []byte(`{"error":true,"code":"plan_structure_invalid","message":"plan has no task headings"}`), errors.New("exit status 1")
		}
		if len(args) >= 4 && args[1] == verbFile && args[2] == verbRead {
			*reads = append(*reads, args[3])
			if filepath.Ext(args[3]) != "" {
				return []byte(`{"error":true,"code":"unexpected_extension","message":"path must not carry an extension"}`), errors.New("exit status 1")
			}
			if out, ok := files[args[3]]; ok {
				return []byte(out), nil
			}
			return []byte(`{"error":true,"code":"not_found","message":"file not found"}`), errors.New("exit status 1")
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}
}

func TestReadSpec_RetriesWithoutExtension(t *testing.T) {
	var reads []string
	r := &Runner{Exec: unexpectedExtensionExec(map[string]string{"feature-x": "# spec body"}, &reads)}
	body, err := r.ReadSpec(context.Background(), "feature-x")
	if err != nil || body != "# spec body" {
		t.Fatalf("ReadSpec = %q, %v", body, err)
	}
	if strings.Join(reads, ",") != "feature-x.md,feature-x" {
		t.Fatalf("reads = %v", reads)
	}
	reads = nil
	if _, err := r.ReadSpec(context.Background(), "missing"); !isNotFound(err) {
		t.Fatalf("missing spec err = %v, want not found", err)
	}
	if _, err := r.readFileInDir(context.Background(), "", KindSpec, "x", ".md"); !isUnexpectedExtension(err) {
		t.Fatalf("extension-only path err = %v, want unexpected_extension without retry", err)
	}
}

func TestExportPlanWithFallback_PlanStructureInvalidReadsExtensionlessPlan(t *testing.T) {
	var reads []string
	r := &Runner{Exec: unexpectedExtensionExec(map[string]string{"feature-y/plan": spekNativePlanMD}, &reads)}
	plan, err := r.ExportPlanWithFallback(context.Background(), "feature-y")
	if err != nil {
		t.Fatalf("ExportPlanWithFallback: %v (reads %v)", err, reads)
	}
	if len(plan.Tasks) != 3 || plan.Tasks[0].Ref != "P1.1" {
		t.Fatalf("plan = %+v", plan)
	}
	if got := strings.Join(reads, ","); got != "feature-y/tasks.json,feature-y/tasks,feature-y/plan.md,feature-y/plan" {
		t.Fatalf("reads = %s", got)
	}
}

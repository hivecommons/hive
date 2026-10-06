package vibekanban

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
)

type fakeClient struct {
	mu                 sync.Mutex
	projectID          string
	issues             []map[string]any
	tags               []map[string]any
	workspaces         []map[string]any
	calls              []string
	failStartWorkspace bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{projectID: "project-1", tags: []map[string]any{{"id": "tag-hive", "name": "hive"}}}
}

func (f *fakeClient) CallTool(_ context.Context, name string, args map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	switch name {
	case "list_tags":
		if args["project_id"] != f.projectID {
			return nil, errors.New("project not found")
		}
		return map[string]any{"tags": mapsToAny(f.tags)}, nil
	case "list_issues":
		if args["project_id"] != f.projectID {
			return nil, errors.New("project not found")
		}
		needle, _ := args["search"].(string)
		var issues []any
		for _, issue := range f.issues {
			haystack := fmt.Sprintf("%s\n%s", issue["title"], issue["description"])
			if needle == "" || strings.Contains(haystack, needle) {
				issues = append(issues, cloneMap(issue))
			}
		}
		return map[string]any{"issues": issues}, nil
	case "create_issue":
		id := fmt.Sprintf("card-%d", len(f.issues)+1)
		issue := map[string]any{"id": id, "title": args["title"], "description": args["description"], "status": "Backlog"}
		f.issues = append(f.issues, issue)
		return map[string]any{"issue_id": id}, nil
	case "update_issue":
		id, _ := args["issue_id"].(string)
		for _, issue := range f.issues {
			if issue["id"] == id {
				if v, ok := args["status"]; ok {
					issue["status"] = v
				}
				if v, ok := args["title"]; ok {
					issue["title"] = v
				}
				if v, ok := args["description"]; ok {
					issue["description"] = v
				}
				return cloneMap(issue), nil
			}
		}
		return nil, errors.New("issue not found")
	case "add_issue_tag":
		return map[string]any{"issue_tag_id": "issue-tag-1"}, nil
	case "start_workspace":
		if f.failStartWorkspace {
			return nil, errors.New("unknown tool start_workspace")
		}
		id := fmt.Sprintf("workspace-%d", len(f.workspaces)+1)
		f.workspaces = append(f.workspaces, map[string]any{"id": id, "issue_id": args["issue_id"], "prompt": args["prompt"]})
		if issueID, _ := args["issue_id"].(string); issueID != "" {
			for _, issue := range f.issues {
				if issue["id"] == issueID {
					issue["status"] = defaultInProgressStatus
				}
			}
		}
		return map[string]any{"workspace_id": id}, nil
	default:
		return nil, fmt.Errorf("unknown tool %s", name)
	}
}

func mapsToAny(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, cloneMap(m))
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func testStateDir(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-", "#", "-").Replace(t.Name())
	dir := filepath.Join("teststate", name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newAdapterForTest(t *testing.T, fc *fakeClient) *Adapter {
	t.Helper()
	a, err := New(Config{ProjectID: fc.projectID, StateDir: testStateDir(t), WorkflowVersion: "vibe-kanban/phase2", Client: fc, Now: func() time.Time { return time.Unix(100, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func admissionAndPayload(t *testing.T) (extwork.Admission, []byte) {
	t.Helper()
	payload := []byte(`{"summary":"do the work","repo":"acme/widgets"}`)
	adm := extwork.Admission{
		WorkKey:          "acme/widgets#42",
		AssignmentID:     "task-42",
		Generation:       1,
		Stage:            "implement",
		ContractRevision: "external-workflow-admission/v1",
		Engine:           Engine,
		WorkflowVersion:  "vibe-kanban/phase2",
		InputRevision:    "task@sha256:abc",
		RequestDigest:    extwork.RequestDigest(payload),
		Authority: extwork.AuthorityBinding{
			Identity:   "clubanderson",
			Tier:       "C4",
			Capability: Capability,
			Mode:       extwork.ModeReportOnly,
		},
	}
	return adm, payload
}

func TestStartCreatesCardWorkspaceAndRecord(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	res, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteRunID != "workspace-1" || res.RemoteIncarnation != fc.projectID || res.Deduplicated {
		t.Fatalf("StartResult = %+v", res)
	}
	if len(fc.issues) != 1 || len(fc.workspaces) != 1 {
		t.Fatalf("issues=%d workspaces=%d", len(fc.issues), len(fc.workspaces))
	}
	if desc, _ := fc.issues[0]["description"].(string); !strings.Contains(desc, trailer(adm.WorkKey)) || !strings.Contains(desc, "hive-execution-key") {
		t.Fatalf("description lacks correlation trailers: %s", desc)
	}
	if rec, ok, err := a.loadByWorkKey(adm.WorkKey); err != nil || !ok || rec.WorkspaceID != "workspace-1" || rec.CardID != "card-1" {
		t.Fatalf("record ok=%v err=%v rec=%+v", ok, err, rec)
	}
}

func TestStartIsIdempotentAfterRecord(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	first, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated || second.RemoteRunID != first.RemoteRunID {
		t.Fatalf("second = %+v first=%+v", second, first)
	}
	if len(fc.issues) != 1 || len(fc.workspaces) != 1 {
		t.Fatalf("redispatch created duplicates: issues=%d workspaces=%d", len(fc.issues), len(fc.workspaces))
	}
}

func TestObserveReattachesFromRecord(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{ProjectID: fc.projectID, StateDir: a.stateDir, WorkflowVersion: a.version, Client: fc, Now: a.now})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := restarted.Observe(context.Background(), adm.ExecutionKey(), fc.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != extwork.StateRunning || obs.RemoteRunID != "workspace-1" || obs.RemoteIncarnation != fc.projectID {
		t.Fatalf("observation = %+v", obs)
	}
}

func TestStartWorkspaceUnsupportedFailsAfterDurableCardRecord(t *testing.T) {
	fc := newFakeClient()
	fc.failStartWorkspace = true
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	_, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	rec, ok, err := a.loadByWorkKey(adm.WorkKey)
	if err != nil || !ok || rec.CardID == "" || rec.WorkspaceID != "" {
		t.Fatalf("record ok=%v err=%v rec=%+v", ok, err, rec)
	}
}

func TestNewRequiresExplicitConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with zero config succeeded")
	}
	fc := newFakeClient()
	if _, err := New(Config{ProjectID: fc.projectID, WorkflowVersion: "v", StateDir: testStateDir(t), Client: fc}); err != nil {
		t.Fatalf("explicit in-process config failed: %v", err)
	}
}

func TestAdmitRefusesWrongCapability(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, _ := admissionAndPayload(t)
	adm.Authority.Capability = "ext-exec/flue"
	if err := a.Admit(adm); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestObserveMapsTerminalAndCancelFailure(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cancel(context.Background(), adm.ExecutionKey(), fc.projectID); err != nil {
		t.Fatal(err)
	}
	obs, err := a.Observe(context.Background(), adm.ExecutionKey(), fc.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != extwork.StateTerminal || obs.ResultClass != "failed" {
		t.Fatalf("observation = %+v", obs)
	}
}

func TestProjectChangeDoesNotAdoptRecord(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	otherClient := newFakeClient()
	otherClient.projectID = "project-2"
	moved, err := New(Config{ProjectID: otherClient.projectID, StateDir: a.stateDir, WorkflowVersion: a.version, Client: otherClient, Now: a.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := moved.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("Start after project change = %v, want ErrIncarnationMismatch", err)
	}
	if _, err := moved.Observe(context.Background(), adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("Observe after project change = %v, want ErrIncarnationMismatch", err)
	}
}

func TestStartRefusesPinnedDifferentProject(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	adm.EngineIncarnation = "other-project"
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("Start with pinned different project = %v, want ErrIncarnationMismatch", err)
	}
}

func TestFactoryAndNewValidation(t *testing.T) {
	cases := []Config{
		{},
		{ProjectID: "p"},
		{ProjectID: "p", WorkflowVersion: "v"},
		{ProjectID: "p", WorkflowVersion: "v", StateDir: "state"},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Fatalf("case %d succeeded", i)
		}
	}
	fc := newFakeClient()
	dir := testStateDir(t)
	adapter, err := Factory(map[string]string{SettingProjectID: fc.projectID, SettingWorkflowVersion: "v", SettingStateDir: dir, SettingMCPCommand: "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Engine() != Engine {
		t.Fatalf("engine = %q", adapter.Engine())
	}
}

func TestAdmitRefusals(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, _ := admissionAndPayload(t)
	cases := []struct {
		name string
		edit func(*extwork.Admission)
	}{
		{"engine", func(a *extwork.Admission) { a.Engine = "flue" }},
		{"version", func(a *extwork.Admission) { a.WorkflowVersion = "other" }},
		{"mode", func(a *extwork.Admission) { a.Authority.Mode = "publish" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adm
			tc.edit(&got)
			if err := a.Admit(got); !errors.Is(err, extwork.ErrRefused) {
				t.Fatalf("Admit = %v, want ErrRefused", err)
			}
		})
	}
}

func TestStartRefusesDigestAndConflictingRecord(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: []byte("different")}); !errors.Is(err, extwork.ErrPayloadDigest) {
		t.Fatalf("digest error = %v", err)
	}
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	conflict := adm
	conflict.Generation = 2
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: conflict, Payload: payload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("conflicting key = %v", err)
	}
	rec, ok, err := a.loadByWorkKey(adm.WorkKey)
	if err != nil || !ok {
		t.Fatalf("load record ok=%v err=%v", ok, err)
	}
	rec.RequestDigest = extwork.RequestDigest([]byte("other"))
	if err := a.saveRecord(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("conflicting digest = %v", err)
	}
}

func TestStartResumesRecordWithoutWorkspace(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	fc.failStartWorkspace = true
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("first start = %v", err)
	}
	fc.failStartWorkspace = false
	res, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deduplicated || res.RemoteRunID != "workspace-1" || len(fc.issues) != 1 || len(fc.workspaces) != 1 {
		t.Fatalf("resume result=%+v issues=%d workspaces=%d", res, len(fc.issues), len(fc.workspaces))
	}
}

func TestObserveEdges(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if obs, err := a.Observe(context.Background(), adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrNotFound) || obs.State != extwork.StateUnknown {
		t.Fatalf("missing observe obs=%+v err=%v", obs, err)
	}
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Observe(context.Background(), adm.ExecutionKey(), "other-project"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("pinned mismatch = %v", err)
	}
	fc.issues[0]["status"] = defaultInReviewStatus
	obs, err := a.Observe(context.Background(), adm.ExecutionKey(), fc.projectID)
	if err != nil || obs.State != extwork.StateWaiting {
		t.Fatalf("in review obs=%+v err=%v", obs, err)
	}
	fc.issues[0]["status"] = defaultDoneStatus
	obs, err = a.Observe(context.Background(), adm.ExecutionKey(), fc.projectID)
	if err != nil || obs.State != extwork.StateTerminal || obs.ResultClass != "done" {
		t.Fatalf("done obs=%+v err=%v", obs, err)
	}
	fc.issues[0]["status"] = "Mystery"
	obs, err = a.Observe(context.Background(), adm.ExecutionKey(), fc.projectID)
	if err != nil || obs.State != extwork.StateUnknown {
		t.Fatalf("unknown obs=%+v err=%v", obs, err)
	}
}

func TestCancelEdgesAndOpenArtifact(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, payload := admissionAndPayload(t)
	if _, err := a.Cancel(context.Background(), adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("missing cancel = %v", err)
	}
	if _, err := a.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cancel(context.Background(), adm.ExecutionKey(), "other-project"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("cancel mismatch = %v", err)
	}
	if rc, err := a.OpenArtifact(context.Background(), adm.ExecutionKey(), fc.projectID, "receipt.json"); !errors.Is(err, extwork.ErrNotFound) || rc != nil {
		t.Fatalf("OpenArtifact rc=%v err=%v", rc, err)
	}
}

func TestCorruptRecordReportsError(t *testing.T) {
	fc := newFakeClient()
	a := newAdapterForTest(t, fc)
	adm, _ := admissionAndPayload(t)
	if err := os.WriteFile(a.recordPath(adm.WorkKey), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.loadByWorkKey(adm.WorkKey); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("loadByWorkKey corrupt = %v", err)
	}
	if _, _, err := a.loadByExecutionKey(adm.ExecutionKey()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("loadByExecutionKey corrupt = %v", err)
	}
}

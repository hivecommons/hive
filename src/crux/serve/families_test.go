package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
)

// famServer is a Server over a cruxtest.Recorder behind httptest, with
// LaunchContext set the way Serve would (no listener plumbing needed for
// handler tests).
type famServer struct {
	t   *testing.T
	rt  *cruxtest.Recorder
	srv *Server
	ts  *httptest.Server
}

func newFamServer(t *testing.T) *famServer {
	t.Helper()
	rt := cruxtest.NewRecorder()
	srv := New(rt)
	srv.Token = famToken
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &famServer{t: t, rt: rt, srv: srv, ts: ts}
}

// famToken is the bearer token handler tests use; httptest serves over TCP,
// which the server refuses without one.
const famToken = "fam-token"

func (f *famServer) request(method, path string, rd io.Reader) *http.Request {
	f.t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+famToken)
	return req
}

func (f *famServer) do(method, path string, body any) (int, []byte) {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		data, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	resp, err := http.DefaultClient.Do(f.request(method, path, rd))
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (f *famServer) expect(method, path string, body any, status int, into any) []byte {
	f.t.Helper()
	got, data := f.do(method, path, body)
	if got != status {
		f.t.Fatalf("%s %s: status %d want %d; body %s", method, path, got, status, data)
	}
	if into != nil {
		if err := json.Unmarshal(data, into); err != nil {
			f.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
	return data
}

func (f *famServer) expectCode(method, path string, body any, status int, code string) {
	f.t.Helper()
	var e api.Error
	f.expect(method, path, body, status, &e)
	if e.Code != code {
		f.t.Fatalf("%s %s: code %q want %q (%s)", method, path, e.Code, code, e.Error)
	}
}

func (f *famServer) called(method string) cruxtest.Call {
	f.t.Helper()
	c, ok := f.rt.Last(method)
	if !ok {
		f.t.Fatalf("%s was not called; calls: %v", method, f.rt.Methods())
	}
	return c
}

// TestEveryRouteIsRegistered walks api.RuntimeRoutes and api.WatchdogRoutes
// and checks each one reaches a handler: an unregistered pattern gets the
// mux's plain-text 404 or 405, a registered one answers JSON (any status).
// This is the server half of the parity test; crux/api/client holds the
// interface half.
func TestEveryRouteIsRegistered(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	routes := map[string]api.Route{}
	for k, v := range api.RuntimeRoutes {
		routes[k] = v
	}
	for k, v := range api.WatchdogRoutes {
		routes[k] = v
	}
	fill := strings.NewReplacer("{name}", "alpha", "{id}", "k1", "{kind}", api.PinCLI, "{backend}", "claude")
	seen := map[api.Route]bool{}
	for method, route := range routes {
		if seen[route] {
			continue
		}
		seen[route] = true
		if route.Path == api.PathEvents {
			continue // streams; covered by TestHostObserversReachTheEventStream
		}
		var body any
		if route.Method == http.MethodPost || route.Method == http.MethodPut {
			body = "{}"
		}
		req := f.request(route.Method, fill.Replace(route.Path), strings.NewReader(""))
		if body != nil {
			req.Body = io.NopCloser(strings.NewReader("{}"))
			req.ContentLength = 2
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode == http.StatusNotFound && ct != api.ContentTypeJSON || resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s: %s %s is not registered (status %d, %s)", method, route.Method, route.Path, resp.StatusCode, ct)
		}
	}
}

func TestLifecycleRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateStopped)

	var resp api.AgentResponse
	f.expect(http.MethodPost, api.StartPath("a1"), nil, http.StatusOK, &resp)
	if resp.Agent != "alpha" || resp.Process.State != string(crux.StateRunning) {
		t.Fatalf("start: %+v", resp)
	}
	f.expectCode(http.MethodPost, api.StartPath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	f.expect(http.MethodPost, api.RestartPath("alpha"), nil, http.StatusOK, &resp)
	f.called("Restart")
	f.expect(http.MethodPost, api.RestartPath("alpha"), api.RestartRequest{Bootstrap: "hello"}, http.StatusOK, &resp)
	if c := f.called("RestartWithBootstrap"); c.Args[1] != "hello" {
		t.Fatalf("bootstrap: %v", c)
	}
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPost, api.RestartPath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodPost, api.StartPath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil

	f.expect(http.MethodPost, api.PausePath("alpha"), api.PauseRequest{Trigger: "op", Reason: "r"}, http.StatusOK, &resp)
	f.called("Pause")
	if !resp.Process.Paused {
		t.Fatal("pause not reflected")
	}
	f.expect(http.MethodPost, api.PausePath("alpha"), api.PauseRequest{Trigger: "op", Reason: "r", By: "me"}, http.StatusOK, &resp)
	f.called("PauseBy")
	f.expect(http.MethodPost, api.PausePath("alpha"), api.PauseRequest{Trigger: "op", Reason: "r", Cause: &api.PauseCausation{Depth: 2, HookName: "h"}}, http.StatusOK, &resp)
	if c := f.called("PauseByCause"); c.Args[4].(crux.PauseCausation).Depth != 2 {
		t.Fatalf("cause: %v", c)
	}
	f.expectCode(http.MethodPost, api.PausePath("alpha"), api.PauseRequest{Reason: "r"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PausePath("alpha"), "{bad", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.ResumePath("alpha"), api.ResumeRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.ResumePath("alpha"), "{bad", http.StatusBadRequest, api.CodeBadRequest)
	f.expect(http.MethodPost, api.ResumePath("alpha"), api.ResumeRequest{Trigger: "op", Reason: "r"}, http.StatusOK, &resp)
	if resp.Process.Paused {
		t.Fatal("resume not reflected")
	}
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPost, api.PausePath("alpha"), api.PauseRequest{Trigger: "op"}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodPost, api.ResumePath("alpha"), api.ResumeRequest{Trigger: "op"}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodDelete, api.RestartCountPath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil
	f.rt.Procs["alpha"].RestartCount = 4
	f.expect(http.MethodDelete, api.RestartCountPath("alpha"), nil, http.StatusNoContent, nil)
	if f.rt.Procs["alpha"].RestartCount != 0 {
		t.Fatal("restart count not reset")
	}
}

func TestSnapshotRoutes(t *testing.T) {
	f := newFamServer(t)
	p := f.rt.Add("alpha", "a1", crux.StateRunning)
	p.RestartCount = 7
	p.OutputBuffer = crux.NewRingBuffer(4)
	f.rt.Pane["alpha"] = []string{"one", "two"}

	var snap api.Snapshot
	f.expect(http.MethodGet, api.SnapshotPath("a1"), nil, http.StatusOK, &snap)
	f.called("GetStatus")
	var proc crux.Process
	if err := json.Unmarshal(snap.Process, &proc); err != nil {
		t.Fatal(err)
	}
	if snap.Agent != "alpha" || proc.RestartCount != 7 || proc.OutputBuffer != nil || len(snap.Pane) != 2 {
		t.Fatalf("snapshot: %+v %+v", snap, proc)
	}
	snap = api.Snapshot{}
	f.expect(http.MethodGet, api.SnapshotPath("alpha")+"?"+api.QueryFast+"=1", nil, http.StatusOK, &snap)
	f.called("GetStatusFast")
	if len(snap.Pane) != 0 {
		t.Fatal("fast snapshot carries a pane")
	}
	f.expectCode(http.MethodGet, api.SnapshotPath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	f.rt.Add("beta", "b1", crux.StateStopped)
	var list api.SnapshotList
	f.expect(http.MethodGet, api.PathSnapshots, nil, http.StatusOK, &list)
	if len(list.Snapshots) != 2 || list.Snapshots[0].Agent != "alpha" || list.Snapshots[1].Agent != "beta" {
		t.Fatalf("snapshots: %+v", list)
	}
}

func TestKickerRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	f.rt.KickStarted = true

	var resp api.AgentResponse
	f.expect(http.MethodPost, api.KickPath("alpha"), api.KickRequest{Message: "go", Async: true}, http.StatusAccepted, &resp)
	f.called("SendKickAsync")
	if resp.Started == nil || !*resp.Started {
		t.Fatalf("async: %+v", resp)
	}
	f.rt.KickErr = errors.New("agent alpha is paused")
	f.expectCode(http.MethodPost, api.KickPath("alpha"), api.KickRequest{Message: "go", Async: true}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodPost, api.KickValidatePath("alpha"), nil, http.StatusConflict, api.CodeKickRefused)
	f.rt.KickErr = nil
	f.expect(http.MethodPost, api.KickValidatePath("alpha"), nil, http.StatusNoContent, nil)
	f.expectCode(http.MethodPost, api.KickValidatePath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	var disp api.KickDispatchResponse
	f.expect(http.MethodGet, api.KickPath("alpha"), nil, http.StatusOK, &disp)
	if disp.Dispatch != nil {
		t.Fatal("no dispatch expected")
	}
	f.rt.Dispatch["alpha"] = crux.KickDispatch{Agent: "alpha", Phase: crux.KickPhaseDelivered, QueuedAt: time.Unix(10, 0)}
	f.expect(http.MethodGet, api.KickPath("alpha"), nil, http.StatusOK, &disp)
	if disp.Dispatch == nil || disp.Dispatch.Phase != crux.KickPhaseDelivered {
		t.Fatalf("dispatch: %+v", disp)
	}

	f.rt.KickLogs["alpha"] = []crux.KickLogInfo{{ID: "k1", Reason: "kick", SizeBytes: 3}}
	f.rt.KickLogContent["alpha"] = map[string]string{"k1": "abc"}
	var logs api.KickLogList
	f.expect(http.MethodGet, api.KickLogsPath("alpha"), nil, http.StatusOK, &logs)
	if len(logs.Logs) != 1 || logs.Logs[0].ID != "k1" || logs.Logs[0].SizeBytes != 3 {
		t.Fatalf("kick logs: %+v", logs)
	}
	var log api.KickLog
	f.expect(http.MethodGet, api.KickLogPath("alpha", "k1"), nil, http.StatusOK, &log)
	if log.Content != "abc" {
		t.Fatalf("kick log: %+v", log)
	}
	f.expectCode(http.MethodGet, api.KickLogPath("alpha", "k2"), nil, http.StatusNotFound, api.CodeNotFound)
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodGet, api.KickLogsPath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
}

func TestDefinitionRoutes(t *testing.T) {
	f := newFamServer(t)
	def := json.RawMessage(`{"id":"b1","backend":"claude","model":"opus"}`)

	// Verbatim: enabled was not sent, so it stays false (unlike POST /v1/agents).
	var proc api.Process
	f.expect(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: "beta", Definition: def}, http.StatusCreated, &proc)
	if proc.Name != "beta" || proc.ID != "b1" || f.rt.Procs["beta"].Config.Enabled {
		t.Fatalf("add: %+v %+v", proc, f.rt.Procs["beta"].Config)
	}
	f.expect(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: "beta", Definition: def}, http.StatusOK, &proc)
	f.expectCode(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: "", Definition: def}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: "x"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: "x", Definition: json.RawMessage(`[1]`)}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathDefinitions, "nope", http.StatusBadRequest, api.CodeBadRequest)

	var resp api.AgentResponse
	f.expect(http.MethodPut, api.DefinitionPath("beta"), api.DefinitionRequest{Definition: json.RawMessage(`{"id":"b1","model":"sonnet"}`)}, http.StatusOK, &resp)
	if f.rt.Procs["beta"].Config.Model != "sonnet" {
		t.Fatal("update not applied")
	}
	f.expectCode(http.MethodPut, api.DefinitionPath("beta"), api.DefinitionRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.DefinitionPath("beta"), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.DefinitionPath("nope"), api.DefinitionRequest{Definition: def}, http.StatusNotFound, api.CodeNotFound)
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPut, api.DefinitionPath("beta"), api.DefinitionRequest{Definition: def}, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil

	f.rt.Reconciled = []string{"beta"}
	var names api.Names
	f.expect(http.MethodPost, api.PathReconcile, api.ReconcileRequest{Definitions: map[string]json.RawMessage{"beta": def}}, http.StatusOK, &names)
	if len(names.Names) != 1 || names.Names[0] != "beta" {
		t.Fatalf("reconcile: %+v", names)
	}
	if c := f.called("ReconcileAgents"); !reflect.DeepEqual(c.Args[0], []string{"beta"}) {
		t.Fatalf("reconcile args: %v", c)
	}
	f.expectCode(http.MethodPost, api.PathReconcile, api.ReconcileRequest{Definitions: map[string]json.RawMessage{"beta": json.RawMessage(`[]`)}}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathReconcile, "x", http.StatusBadRequest, api.CodeBadRequest)

	model, backend := "haiku", "copilot"
	f.expect(http.MethodPut, api.OverridesPath("beta"), api.OverridesRequest{Model: &model, Backend: &backend}, http.StatusOK, &resp)
	if resp.Process.Model != "haiku" || resp.Process.Backend != "copilot" {
		t.Fatalf("overrides: %+v", resp.Process)
	}
	f.expectCode(http.MethodPut, api.OverridesPath("beta"), api.OverridesRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.OverridesPath("beta"), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPut, api.OverridesPath("beta"), api.OverridesRequest{Model: &model}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodPut, api.OverridesPath("beta"), api.OverridesRequest{Backend: &backend}, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil

	f.expect(http.MethodPut, api.PinPath("beta", api.PinCLI), api.PinRequest{Value: "1.2"}, http.StatusNoContent, nil)
	f.called("PinCLI")
	f.expect(http.MethodPut, api.PinPath("beta", api.PinModel), api.PinRequest{Value: "opus"}, http.StatusNoContent, nil)
	f.called("PinModel")
	f.expectCode(http.MethodPut, api.PinPath("beta", "other"), api.PinRequest{Value: "x"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.PinPath("beta", api.PinCLI), api.PinRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.PinPath("beta", api.PinCLI), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.expect(http.MethodDelete, api.PinPath("beta", api.PinCLI), nil, http.StatusNoContent, nil)
	f.called("UnpinCLI")
	f.expect(http.MethodDelete, api.PinPath("beta", api.PinModel), nil, http.StatusNoContent, nil)
	f.called("UnpinModel")
	f.expectCode(http.MethodDelete, api.PinPath("beta", "other"), nil, http.StatusBadRequest, api.CodeBadRequest)
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPut, api.PinPath("beta", api.PinCLI), api.PinRequest{Value: "1"}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodDelete, api.PinPath("beta", api.PinCLI), nil, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil

	f.expect(http.MethodPost, api.PathModesClear, api.Names{Names: []string{"beta"}}, http.StatusNoContent, nil)
	f.called("ClearModeOverrides")
	f.expect(http.MethodPost, api.PathModesSync, api.Level{Level: 3}, http.StatusNoContent, nil)
	if c := f.called("SyncModeFiles"); c.Args[0] != 3 {
		t.Fatalf("sync: %v", c)
	}
	f.expect(http.MethodPost, api.PathInference, api.Backend{Backend: "claude"}, http.StatusNoContent, nil)
	f.called("RefreshInferenceRoutes")
	for _, p := range []string{api.PathModesClear, api.PathModesSync, api.PathInference} {
		f.expectCode(http.MethodPost, p, "x", http.StatusBadRequest, api.CodeBadRequest)
	}

	f.expect(http.MethodDelete, api.AgentPath("beta"), nil, http.StatusNoContent, nil)
	if _, ok := f.rt.Procs["beta"]; ok {
		t.Fatal("remove not applied")
	}
	f.expectCode(http.MethodDelete, api.AgentPath("beta"), nil, http.StatusNotFound, api.CodeNotFound)
}

func TestFleetRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.ACMM = 2
	f.rt.Shared = true
	f.rt.WithModel = 5
	f.rt.Wedged = []string{"w"}
	var info api.FleetInfo
	f.expect(http.MethodGet, api.PathFleet, nil, http.StatusOK, &info)
	if info.ACMMLevel != 2 || !info.SharedDevUID || info.AgentsWithModel != 5 || len(info.ConsentWedged) != 1 {
		t.Fatalf("fleet: %+v", info)
	}

	f.rt.BreakerPaused = []string{"alpha"}
	f.rt.Resumed = []string{"alpha"}
	var names api.Names
	f.expect(http.MethodPost, api.PathBreakerOn, nil, http.StatusOK, &names)
	var br api.Breaker
	f.expect(http.MethodGet, api.PathBreaker, nil, http.StatusOK, &br)
	if !br.Engaged || len(br.Paused) != 1 || len(names.Names) != 1 {
		t.Fatalf("breaker: %+v %+v", br, names)
	}
	f.expect(http.MethodPost, api.PathBreakerOff, nil, http.StatusOK, &names)
	f.expect(http.MethodGet, api.PathBreaker, nil, http.StatusOK, &br)
	if br.Engaged {
		t.Fatal("breaker still engaged")
	}
	f.expect(http.MethodPut, api.PathBreaker, api.Breaker{Engaged: true, Paused: []string{"x", "y"}}, http.StatusNoContent, nil)
	f.expect(http.MethodGet, api.PathBreaker, nil, http.StatusOK, &br)
	if !br.Engaged || len(br.Paused) != 2 {
		t.Fatalf("restore: %+v", br)
	}
	f.expectCode(http.MethodPut, api.PathBreaker, "x", http.StatusBadRequest, api.CodeBadRequest)

	f.expect(http.MethodPut, api.PathACMM, api.Level{Level: 4}, http.StatusNoContent, nil)
	if f.rt.ACMM != 4 {
		t.Fatal("acmm")
	}
	f.expectCode(http.MethodPut, api.PathACMM, "x", http.StatusBadRequest, api.CodeBadRequest)
	f.rt.Relaunched = []string{"bob"}
	f.expect(http.MethodPost, api.PathRelaunchBob, nil, http.StatusOK, &names)
	if len(names.Names) != 1 {
		t.Fatal("relaunch")
	}
	f.expect(http.MethodPost, api.PathStartupQueued, api.Names{Names: []string{"a", "b"}}, http.StatusNoContent, nil)
	if c := f.called("MarkStartupLaunchQueued"); !reflect.DeepEqual(c.Args[0], []string{"a", "b"}) {
		t.Fatalf("queued: %v", c)
	}
	f.expectCode(http.MethodPost, api.PathStartupQueued, "x", http.StatusBadRequest, api.CodeBadRequest)
	f.rt.Crashed = []string{"c"}
	f.expect(http.MethodPost, api.PathRestartCrashed, nil, http.StatusOK, &names)
	if len(names.Names) != 1 || names.Names[0] != "c" {
		t.Fatal("crashed")
	}
	f.expect(http.MethodPost, api.PathTokensRefresh, nil, http.StatusNoContent, nil)
	f.called("RefreshAgentTokens")
	f.expect(http.MethodPost, api.PathKickLogsArchive, api.Reason{Reason: "shutdown"}, http.StatusNoContent, nil)
	if c := f.called("ArchiveAllKickLogs"); c.Args[0] != "shutdown" {
		t.Fatalf("archive: %v", c)
	}
	f.expectCode(http.MethodPost, api.PathKickLogsArchive, "x", http.StatusBadRequest, api.CodeBadRequest)
}

func TestCredentialRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	f.expect(http.MethodPost, api.LoginCodePath("alpha"), api.LoginCodeRequest{Code: "1234"}, http.StatusNoContent, nil)
	if c := f.called("SubmitLoginCode"); c.Args[1] != "1234" {
		t.Fatalf("login: %v", c)
	}
	f.expectCode(http.MethodPost, api.LoginCodePath("alpha"), api.LoginCodeRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.LoginCodePath("alpha"), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.LoginCodePath("nope"), api.LoginCodeRequest{Code: "1"}, http.StatusNotFound, api.CodeNotFound)
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPost, api.LoginCodePath("alpha"), api.LoginCodeRequest{Code: "1"}, http.StatusInternalServerError, api.CodeRuntime)
	f.expectCode(http.MethodPut, api.PathCopilotCred, api.CopilotCredential{Token: "t", Activate: true}, http.StatusUnprocessableEntity, api.CodeRuntime)
	f.rt.Err = nil

	f.rt.CopilotSource = "env"
	f.expect(http.MethodPut, api.PathCopilotCred, api.CopilotCredential{Token: "tok"}, http.StatusNoContent, nil)
	f.called("SetCopilotToken")
	var cred api.CopilotCredential
	f.expect(http.MethodGet, api.PathCopilotCred, nil, http.StatusOK, &cred)
	if cred.Token != "tok" || cred.Source != "env" {
		t.Fatalf("copilot: %+v", cred)
	}
	f.expect(http.MethodPut, api.PathCopilotCred, api.CopilotCredential{Token: "tok2", Activate: true}, http.StatusNoContent, nil)
	f.called("ActivateCopilotToken")
	f.expectCode(http.MethodPut, api.PathCopilotCred, "x", http.StatusBadRequest, api.CodeBadRequest)

	f.expect(http.MethodPost, api.PathClaudeReload, nil, http.StatusNoContent, nil)
	f.called("ReloadClaudeToken")
	f.rt.ClaudePaths = []string{"/a", "/b"}
	var cl api.ClaudeCredential
	f.expect(http.MethodGet, api.PathClaudeCred, nil, http.StatusOK, &cl)
	if len(cl.CandidatePaths) != 2 {
		t.Fatalf("claude: %+v", cl)
	}
}

func TestResumerRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	var sess api.Session
	f.expect(http.MethodGet, api.SessionPath("alpha"), nil, http.StatusOK, &sess)
	if sess.HasSession || sess.Handle != nil || sess.Missing {
		t.Fatalf("empty session: %+v", sess)
	}
	f.rt.Sessions["alpha"] = "s1"
	f.rt.Missing["alpha"] = true
	f.rt.Handles["alpha"] = crux.ResumeHandle{Backend: "claude", SessionID: "s1", CapturedAt: time.Unix(5, 0)}
	f.expect(http.MethodGet, api.SessionPath("alpha"), nil, http.StatusOK, &sess)
	if !sess.HasSession || sess.SessionID != "s1" || !sess.Missing || sess.Handle == nil || sess.Handle.Backend != "claude" {
		t.Fatalf("session: %+v", sess)
	}
	f.expectCode(http.MethodGet, api.SessionPath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	req := api.ResumeKickRequest{Message: "continue", SessionID: "s1"}
	var resp api.AgentResponse
	f.expect(http.MethodPost, api.SessionKickPath("alpha"), req, http.StatusOK, &resp)
	if c := f.called("SendResumeKick"); c.Args[2] != "s1" {
		t.Fatalf("resume kick: %v", c)
	}
	f.expectCode(http.MethodPost, api.SessionKickPath("alpha"), api.ResumeKickRequest{Message: "x"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.SessionKickPath("alpha"), "x", http.StatusBadRequest, api.CodeBadRequest)
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{crux.ErrResumeBusy, http.StatusConflict, api.CodeResumeBusy},
		{crux.ErrResumeSessionGone, http.StatusGone, api.CodeResumeSessionGone},
		{crux.ErrResumeUnsupported, http.StatusUnprocessableEntity, api.CodeResumeUnsupported},
		{errors.New("tmux exploded"), http.StatusInternalServerError, api.CodeRuntime},
	} {
		f.rt.ResumeErr = tc.err
		f.expectCode(http.MethodPost, api.SessionKickPath("alpha"), req, tc.status, tc.code)
	}
}

func TestSupervisorRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	var sup api.Supervision
	f.expect(http.MethodGet, api.SupervisionPath("alpha"), nil, http.StatusOK, &sup)
	if sup.Capabilities != nil || sup.Backend != nil || sup.Invocation != nil || sup.Restarts != nil || sup.StartFailure != nil || sup.ProviderBackoff != nil {
		t.Fatalf("unknowns should be absent: %+v", sup)
	}
	f.rt.MayStart, f.rt.Valid, f.rt.OOM = true, true, true
	f.rt.Refreshable, f.rt.RefreshKnown = true, true
	f.rt.AuthAvail, f.rt.AuthKnown = false, true
	f.rt.Caps = [4]bool{true, false, true, true}
	f.rt.Backend, f.rt.BackendOK = "copilot", true
	f.rt.Invocation, f.rt.InvocationOK = [3]string{"copilot", "gpt", "high"}, true
	f.rt.Restarts.Total, f.rt.Restarts.Last24h, f.rt.Restarts.LastReason, f.rt.Restarts.OK = 3, 1, "crash", true
	code := 137
	f.rt.StartFailure, f.rt.StartFailureOK = crux.StartFailureSnapshot{Reason: "oom", Count: 2, LastExitCode: &code, Blocked: true}, true
	f.rt.Backoff.Remaining, f.rt.Backoff.Provider, f.rt.Backoff.OK = 1500*time.Millisecond, "anthropic", true
	f.expect(http.MethodGet, api.SupervisionPath("alpha"), nil, http.StatusOK, &sup)
	switch {
	case !sup.MayStart || !sup.HasValidCredential || !sup.CrashOOMSuspected:
		t.Fatalf("bools: %+v", sup)
	case !sup.CredentialRefreshable || !sup.CredentialKnown || sup.AuthAvailable || !sup.AuthKnown:
		t.Fatalf("credential: %+v", sup)
	case sup.Capabilities == nil || !sup.Capabilities.CanOpenIssue || sup.Capabilities.CanOpenPR || !sup.Capabilities.CanMerge:
		t.Fatalf("caps: %+v", sup.Capabilities)
	case sup.Backend == nil || *sup.Backend != "copilot":
		t.Fatalf("backend: %+v", sup.Backend)
	case sup.Invocation == nil || sup.Invocation.Effort != "high":
		t.Fatalf("invocation: %+v", sup.Invocation)
	case sup.Restarts == nil || sup.Restarts.Total != 3 || sup.Restarts.LastReason != "crash":
		t.Fatalf("restarts: %+v", sup.Restarts)
	case sup.StartFailure == nil || sup.StartFailure.LastExitCode == nil || *sup.StartFailure.LastExitCode != 137 || !sup.StartFailure.Blocked:
		t.Fatalf("start failure: %+v", sup.StartFailure)
	case sup.ProviderBackoff == nil || sup.ProviderBackoff.RemainingMS != 1500 || sup.ProviderBackoff.Provider != "anthropic":
		t.Fatalf("backoff: %+v", sup.ProviderBackoff)
	}
	f.expectCode(http.MethodGet, api.SupervisionPath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	f.expect(http.MethodPost, api.AgentTokenRefreshPath("alpha"), nil, http.StatusNoContent, nil)
	f.called("RefreshAgentTokenFor")
	f.rt.Err = cruxtest.ErrRecorder
	f.expectCode(http.MethodPost, api.AgentTokenRefreshPath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
	f.rt.Err = nil

	f.rt.BackendAuth["claude"] = [2]bool{true, true}
	var auth api.BackendAuth
	f.expect(http.MethodGet, api.BackendAuthPath("claude"), nil, http.StatusOK, &auth)
	if auth.Backend != "claude" || !auth.Available || !auth.Known {
		t.Fatalf("backend auth: %+v", auth)
	}
	f.expect(http.MethodGet, api.BackendAuthPath("codex"), nil, http.StatusOK, &auth)
	if auth.Known {
		t.Fatal("unknown backend must not be known")
	}

	f.expect(http.MethodPost, api.NudgePath("alpha"), api.NudgeRequest{Message: "slow down"}, http.StatusNoContent, nil)
	if c := f.called("SendGitHubAPIBudgetNudge"); c.Args[1] != "slow down" {
		t.Fatalf("nudge: %v", c)
	}
	f.expectCode(http.MethodPost, api.NudgePath("alpha"), api.NudgeRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.NudgePath("alpha"), "x", http.StatusBadRequest, api.CodeBadRequest)

	f.expect(http.MethodPost, api.PathAudit, api.AuditRequest{Action: "pause", Agent: "alpha", Fields: map[string]any{"by": "me"}}, http.StatusNoContent, nil)
	if c := f.called("RecordAudit"); c.Args[0] != "pause" || c.Args[2].(map[string]any)["by"] != "me" {
		t.Fatalf("audit: %v", c)
	}
	f.expectCode(http.MethodPost, api.PathAudit, api.AuditRequest{}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathAudit, "x", http.StatusBadRequest, api.CodeBadRequest)

	f.rt.Views = []trajectory.AgentView{{Name: "alpha", Running: true, Intent: "fix", Idle: true, LastOutputAt: time.Unix(9, 0)}}
	var tl api.TrajectoryList
	f.expect(http.MethodGet, api.PathTrajectory, nil, http.StatusOK, &tl)
	if len(tl.Agents) != 1 || tl.Agents[0].Intent != "fix" || !tl.Agents[0].Idle {
		t.Fatalf("trajectory: %+v", tl)
	}
}

func TestSeedRoute(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateStopped)
	at := time.Unix(100, 0).UTC()
	loss, _ := json.Marshal(crux.TurnLoss{Interruptions: 2, Producing: 1})
	req := api.SeedRequest{
		Pause:       &api.PauseSeed{PausedAt: at, Trigger: "op", Reason: "r", By: "me"},
		Restarts:    &api.RestartSeed{Count: 2, Events: []api.RestartEvent{{At: at, Reason: "crash"}}, LastReason: "crash"},
		TurnLoss:    loss,
		LastKick:    &at,
		KickHistory: &[]api.KickRecord{{Timestamp: at, Agent: "alpha", Snippet: "go"}},
	}
	f.expect(http.MethodPost, api.SeedPath("alpha"), req, http.StatusNoContent, nil)
	for _, m := range []string{"SeedPauseState", "SeedRestartTelemetry", "SeedTurnLoss", "SeedLastKick", "SeedKickHistory"} {
		f.called(m)
	}
	if c := f.called("SeedRestartTelemetry"); c.Args[1] != 2 || len(c.Args[2].([]crux.RestartEvent)) != 1 {
		t.Fatalf("restarts: %v", c)
	}
	if c := f.called("SeedTurnLoss"); c.Args[1].(crux.TurnLoss).Interruptions != 2 {
		t.Fatalf("turn loss: %v", c)
	}
	if c := f.called("SeedKickHistory"); c.Args[1].([]crux.KickRecord)[0].Snippet != "go" {
		t.Fatalf("kick history: %v", c)
	}
	f.rt.Reset()
	f.expect(http.MethodPost, api.SeedPath("alpha"), api.SeedRequest{}, http.StatusNoContent, nil)
	if len(f.rt.Methods()) != 2 { // ResolveAgent + GetStatusFast only
		t.Fatalf("empty seed called %v", f.rt.Methods())
	}
	f.expectCode(http.MethodPost, api.SeedPath("alpha"), api.SeedRequest{TurnLoss: json.RawMessage(`[]`)}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.SeedPath("alpha"), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.SeedPath("nope"), api.SeedRequest{}, http.StatusNotFound, api.CodeNotFound)
}

func TestAuthorizeRoute(t *testing.T) {
	f := newFamServer(t)
	for action, method := range map[string]string{
		api.AuthorizePROpen:        "AuthorizePROpen",
		api.AuthorizePushBranch:    "AuthorizePushBranch",
		api.AuthorizeIssueOpen:     "AuthorizeIssueOpen",
		api.AuthorizeMerge:         "AuthorizeMerge",
		api.AuthorizeReviewRequest: "AuthorizeReviewRequest",
	} {
		f.expect(http.MethodPost, api.PathAuthorize, api.AuthorizeRequest{Action: action, Agent: "alpha", FileUID: 501, Kind: "bug", Repo: "o/r"}, http.StatusNoContent, nil)
		if c := f.called(method); c.Args[0] != "alpha" || c.Args[1] != 501 {
			t.Fatalf("%s: %v", action, c)
		}
	}
	if c := f.called("AuthorizeIssueOpen"); c.Args[2] != "bug" {
		t.Fatalf("kind: %v", c)
	}
	if c := f.called("AuthorizeMerge"); c.Args[2] != "o/r" {
		t.Fatalf("repo: %v", c)
	}
	f.rt.AuthorizeErr = errors.New("agent alpha may not merge in o/r")
	var e api.Error
	f.expect(http.MethodPost, api.PathAuthorize, api.AuthorizeRequest{Action: api.AuthorizeMerge, Agent: "alpha"}, http.StatusForbidden, &e)
	if e.Code != api.CodeForbidden || !strings.Contains(e.Error, "may not merge") {
		t.Fatalf("deny: %+v", e)
	}
	f.expectCode(http.MethodPost, api.PathAuthorize, api.AuthorizeRequest{Action: "fly", Agent: "alpha"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathAuthorize, api.AuthorizeRequest{Action: api.AuthorizePROpen}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathAuthorize, "x", http.StatusBadRequest, api.CodeBadRequest)
}

func TestWatchdogRoutes(t *testing.T) {
	f := newFamServer(t)
	f.rt.Add("alpha", "a1", crux.StateRunning)
	f.rt.WatchdogNames = []string{"alpha"}
	f.rt.Observations["alpha"] = watchdog.Observation{Backend: "claude", SessionExists: true, Pane: "❯", HasCLIMarker: true, AuthKnown: true, ProviderErrorClass: "quota"}
	var names api.Names
	f.expect(http.MethodGet, api.PathWatchdogAgents, nil, http.StatusOK, &names)
	if len(names.Names) != 1 {
		t.Fatalf("names: %+v", names)
	}
	var obs api.WatchdogObservation
	f.expect(http.MethodGet, api.WatchdogPath("a1"), nil, http.StatusOK, &obs)
	if obs.Agent != "alpha" || !obs.SessionExists || !obs.HasCLIMarker || obs.ProviderErrorClass != "quota" || obs.LastProduction != nil {
		t.Fatalf("observe: %+v", obs)
	}
	f.rt.Production["alpha"] = time.Unix(7, 0)
	f.expect(http.MethodGet, api.WatchdogPath("alpha"), nil, http.StatusOK, &obs)
	if obs.LastProduction == nil || !obs.LastProduction.Equal(time.Unix(7, 0)) {
		t.Fatalf("production: %+v", obs)
	}
	f.expectCode(http.MethodGet, api.WatchdogPath("nope"), nil, http.StatusNotFound, api.CodeNotFound)

	conds := api.Conditions{Conditions: []api.Condition{{Type: "Ready", Status: "False", Reason: "dead"}}}
	f.expect(http.MethodPut, api.WatchdogConditionsPath("alpha"), conds, http.StatusNoContent, nil)
	if got := f.rt.SetConds["alpha"]; len(got) != 1 || got[0].Type != "Ready" || got[0].Reason != "dead" {
		t.Fatalf("conditions: %+v", got)
	}
	f.expectCode(http.MethodPut, api.WatchdogConditionsPath("alpha"), "x", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPut, api.WatchdogConditionsPath("nope"), conds, http.StatusNotFound, api.CodeNotFound)

	// A runtime without the seam fails closed on every watchdog route.
	plain := New(&noWatchdog{Runtime: f.rt})
	plain.Token = famToken
	ts := httptest.NewServer(plain.Handler())
	defer ts.Close()
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, api.PathWatchdogAgents},
		{http.MethodGet, api.WatchdogPath("alpha")},
		{http.MethodPut, api.WatchdogConditionsPath("alpha")},
	} {
		req, _ := http.NewRequest(r.method, ts.URL+r.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+famToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e api.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented || e.Code != api.CodeNotSupported {
			t.Fatalf("%s %s without seam: %d %+v", r.method, r.path, resp.StatusCode, e)
		}
	}
	// So does one whose seam answers nil.
	nilSeam := New(&nilWatchdog{Runtime: f.rt})
	nilSeam.Token = famToken
	ts2 := httptest.NewServer(nilSeam.Handler())
	defer ts2.Close()
	req, _ := http.NewRequest(http.MethodGet, ts2.URL+api.PathWatchdogAgents, nil)
	req.Header.Set("Authorization", "Bearer "+famToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("nil seam: %d", resp.StatusCode)
	}
}

// noWatchdog is a Runtime without WatchdogSource.
type noWatchdog struct{ crux.Runtime }

// nilWatchdog has the seam but no source behind it.
type nilWatchdog struct{ crux.Runtime }

func (nilWatchdog) WatchdogSource() watchdog.Source { return nil }

// TestHostObserversReachTheEventStream runs Serve over a Recorder, fires
// every crux.Host observer, and checks each arrives on /v1/events with its
// typed payload; the audit and condition tees also reach the Server's own
// sinks. Background loops start and are reported by the health route.
func TestHostObserversReachTheEventStream(t *testing.T) {
	rt := cruxtest.NewRecorder()
	rt.Add("alpha", "a1", crux.StateRunning)
	srv := New(rt)
	srv.PollInterval = 10 * time.Millisecond
	srv.Background = true
	audit := &recSink{}
	srv.AuditSink = audit
	srv.ConditionReporter = audit
	socket := shortSocketDir(t) + "/crux.sock"
	ls, err := Listen(config.ServeConfig{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ls) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client := unixClient(socket)
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := client.Get("http://crux" + path)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	waitUntil(t, "background loops started", func() bool { return len(rt.Background()) == 4 })
	var h api.Health
	resp := get(api.PathHealth)
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !reflect.DeepEqual(h.Background, backgroundLoops) {
		t.Fatalf("health background: %v", h.Background)
	}

	stream := get(api.PathEvents)
	waitUntil(t, "subscriber registered", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.subscribers) == 1
	})
	ko, po, ro, pp, rp, sa, as, cr := rt.Observers()
	if ko == nil || po == nil || ro == nil || pp == nil || rp == nil || sa == nil || as == nil || cr == nil {
		t.Fatal("host observers not installed")
	}
	ko("alpha", crux.KickOutcome{Kind: "ended", At: time.Unix(1, 0)})
	po(crux.PauseTransitionEvent{Agent: "alpha", Paused: true, Trigger: "op", Causation: crux.PauseCausation{Depth: 1}})
	ro("alpha")
	pp("alpha", false)
	rp("alpha", "cadence", "do the thing")
	sa("alpha", "push", "refs/heads/x")
	as.Record("system", "pause", "alpha", map[string]any{"k": "v"})
	cr.Observe("agent_runtime", "x", "msg", "diag")
	cr.Clear("agent_runtime", "x")

	got := readStream(t, stream, 9)
	wantTypes := []string{api.EventKickOutcome, api.EventPauseTransition, api.EventRelaunch, api.EventPausePersist, api.EventPrompt, api.EventSandboxAudit, api.EventAudit, api.EventCondition, api.EventCondition}
	for i, w := range wantTypes {
		if !strings.HasPrefix(got[i], w+"|") {
			t.Fatalf("event %d: want %s got %s", i, w, got[i])
		}
	}
	var ev api.Event
	decode := func(i int) {
		t.Helper()
		ev = api.Event{}
		if err := json.Unmarshal([]byte(got[i][strings.Index(got[i], "|")+1:]), &ev); err != nil {
			t.Fatal(err)
		}
	}
	decode(0)
	var outcome api.KickOutcome
	if ev.Agent != "alpha" || json.Unmarshal(ev.Data, &outcome) != nil || outcome.Kind != "ended" {
		t.Fatalf("kick outcome: %+v", ev)
	}
	decode(1)
	var pt api.PauseTransition
	if json.Unmarshal(ev.Data, &pt) != nil || !pt.Paused || pt.Causation.Depth != 1 {
		t.Fatalf("pause transition: %s", ev.Data)
	}
	decode(2)
	if ev.Agent != "alpha" || len(ev.Data) != 0 {
		t.Fatalf("relaunch: %+v", ev)
	}
	decode(4)
	var prompt api.Prompt
	if json.Unmarshal(ev.Data, &prompt) != nil || prompt.Prompt != "do the thing" {
		t.Fatalf("prompt: %s", ev.Data)
	}
	decode(6)
	var au api.Audit
	if json.Unmarshal(ev.Data, &au) != nil || au.Actor != "system" || au.Fields["k"] != "v" {
		t.Fatalf("audit: %s", ev.Data)
	}
	decode(8)
	var ce api.ConditionEvent
	if json.Unmarshal(ev.Data, &ce) != nil || ce.Action != api.ConditionClear || ce.Key != "x" {
		t.Fatalf("condition: %s", ev.Data)
	}
	if audit.records != 1 || audit.observed != 1 || audit.cleared != 1 {
		t.Fatalf("tee: %+v", audit)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	done <- nil
	// Uninstalled on shutdown; the tees stay (replace-only setters).
	ko, po, ro, pp, rp, sa, _, _ = rt.Observers()
	if ko != nil || po != nil || ro != nil || pp != nil || rp != nil || sa != nil {
		t.Fatal("host observers not uninstalled")
	}
}

type recSink struct{ records, observed, cleared int }

func (r *recSink) Record(string, string, string, map[string]any) { r.records++ }
func (r *recSink) Observe(string, string, string, string)        { r.observed++ }
func (r *recSink) Clear(string, string)                          { r.cleared++ }

// TestServeWithoutHostOrBackground covers a Runtime that is not a Host and
// a Server not running the loops: nothing is installed, health lists none.
func TestServeWithoutHostOrBackground(t *testing.T) {
	rt := cruxtest.NewRecorder()
	srv := New(&noWatchdog{Runtime: rt})
	srv.installHostObservers()()
	if _, ok := rt.Last("SetKickOutcomeObserver"); ok {
		t.Fatal("observers installed on a non-Host")
	}
	srv.startBackground(context.Background())
	if len(rt.Background()) != 0 {
		t.Fatal("background loops started while disabled")
	}
	var h api.Health
	newFamServer(t).expect(http.MethodGet, api.PathHealth, nil, http.StatusOK, &h)
	if len(h.Background) != 0 {
		t.Fatalf("health: %+v", h)
	}
}

func TestPublishDataSkipsUnencodable(t *testing.T) {
	srv := New(cruxtest.NewRecorder())
	ch := srv.subscribe()
	srv.publishData(api.EventAudit, "a", map[string]any{"bad": make(chan int)})
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

// TestServerConditionsAndHiveCallbackHealth covers the server's own
// condition reporter (event stream plus the tee) and the hive callback
// status on the health route.
func TestServerConditionsAndHiveCallbackHealth(t *testing.T) {
	srv := New(cruxtest.NewRecorder())
	sink := &recSink{}
	srv.ConditionReporter = sink
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv.HiveCallback = func() api.HiveCallback {
		return api.HiveCallback{Address: "unix:///run/hive.sock", LastError: "refused", Since: &since, FailedCalls: 2}
	}
	ch := srv.subscribe()
	srv.Conditions().Observe("hive_callback_unavailable", "endpoint", "down", "d")
	srv.Conditions().Clear("hive_callback_unavailable", "endpoint")
	for i, want := range []string{api.ConditionObserve, api.ConditionClear} {
		var ce api.ConditionEvent
		select {
		case ev := <-ch:
			if ev.Type != api.EventCondition || json.Unmarshal(ev.Data, &ce) != nil || ce.Action != want || ce.Class != "hive_callback_unavailable" {
				t.Fatalf("event %d: %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d not published", i)
		}
	}
	if sink.observed != 1 || sink.cleared != 1 {
		t.Fatalf("tee: %+v", sink)
	}

	srv.Token = "t"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+api.PathHealth, nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h api.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.HiveCallback == nil || h.HiveCallback.Reachable || h.HiveCallback.LastError != "refused" || h.HiveCallback.FailedCalls != 2 || h.HiveCallback.Since == nil || !h.HiveCallback.Since.Equal(since) {
		t.Fatalf("health: %+v", h.HiveCallback)
	}
}

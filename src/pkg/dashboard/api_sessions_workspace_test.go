package dashboard

import (
	"net/http"
	"testing"
)

// Sessions open in the hive's project repo workspace: the runtime is asked to
// start in it, the record and the API carry it, resume reopens the same one.
func TestSessionsOpenInTheProjectWorkspace(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Project.Org, e.s.deps.Config.Project.PrimaryRepo = "acme", "widgets"
	e.s.deps.Config.Runs.Spektacular.Binary = "/opt/spek/bin/spektacular"
	wantPath := e.rt.WorkspaceDir("acme/widgets")

	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	ws, _ := list["workspace"].(map[string]any)
	if ws["repo"] != "acme/widgets" || ws["path"] != wantPath {
		t.Errorf("list workspace = %v", list["workspace"])
	}

	id := e.create(t, callerRW, "spek")
	if len(e.rt.started) != 1 {
		t.Fatalf("started = %v", e.rt.started)
	}
	spec := e.rt.started[0]
	if spec.WorkspaceRepo != "acme/widgets" || spec.SpektacularBinary != "/opt/spek/bin/spektacular" {
		t.Errorf("start spec = %+v", spec)
	}
	rec, _ := e.s.sessionStoreOrNil().get(id)
	if rec.WorkspaceRepo != "acme/widgets" || rec.Workspace != wantPath {
		t.Errorf("record = %+v", rec)
	}
	list = decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", list["sessions"])
	}
	view, _ := sessions[0].(map[string]any)
	if view["workspace_repo"] != "acme/widgets" || view["workspace"] != wantPath {
		t.Errorf("view = %v", view)
	}

	// Close, then resume: the workspace recorded at creation is reused even
	// though the hive is now configured for another repo, and the resume
	// handle is looked up in that workspace.
	if rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(e.rt.resumeDirs); n == 0 || e.rt.resumeDirs[n-1] != wantPath {
		t.Errorf("ResumeInfo work dirs = %v, want the workspace last", e.rt.resumeDirs)
	}
	e.s.deps.Config.Dashboard.Sessions.WorkspaceRepo = "acme/other"
	_, _, _ = e.s.sessionStoreOrNil().update(id, func(r *sessionRecord) { r.ResumeCommand = "claude --resume abcdefgh" })
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/resume", map[string]any{}); rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	if got := e.waitSettled(t, id); got.Status != sessionStatusRunning {
		t.Fatalf("record after resume = %+v", got)
	}
	if started := e.rt.startedSpecs(); len(started) != 2 || started[1].WorkspaceRepo != "acme/widgets" {
		t.Errorf("resume spec = %+v, want the original workspace", started)
	}
}

// Without a project repo (or with a malformed one) sessions keep their own
// empty work dir and the API says so.
func TestSessionsWithoutWorkspaceRepo(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Dashboard.Sessions.WorkspaceRepo = "not-a-repo"
	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	ws, _ := list["workspace"].(map[string]any)
	if ws["repo"] != "" || ws["path"] != "" {
		t.Errorf("list workspace = %v, want none", list["workspace"])
	}
	id := e.create(t, callerRW, "plain")
	if e.rt.started[0].WorkspaceRepo != "" {
		t.Errorf("spec carries a workspace: %+v", e.rt.started[0])
	}
	rec, _ := e.s.sessionStoreOrNil().get(id)
	if rec.Workspace != "" || rec.WorkspaceRepo != "" {
		t.Errorf("record = %+v", rec)
	}
}

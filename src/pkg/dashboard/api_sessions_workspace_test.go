package dashboard

import (
	"net/http"
	"testing"
	"time"
)

// Sessions open in their own worktree of the hive's project repo workspace:
// the runtime is asked to start in it, the record and the API carry the
// session's worktree (the list names the shared clone), resume reopens the
// same one.
func TestSessionsOpenInTheProjectWorkspace(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Project.Org, e.s.deps.Config.Project.PrimaryRepo = "acme", "widgets"
	e.s.deps.Config.Runs.Spektacular.Binary = "/opt/spek/bin/spektacular"
	clonePath := e.rt.WorkspaceDir("acme/widgets")

	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	ws, _ := list["workspace"].(map[string]any)
	if ws["repo"] != "acme/widgets" || ws["path"] != clonePath {
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
	wantPath := e.rt.SessionWorkspaceDir(id, "acme/widgets")
	if wantPath == "" || wantPath == clonePath {
		t.Fatalf("session worktree path = %q, want one of its own (clone %q)", wantPath, clonePath)
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

// A record from before per-session worktrees names the shared clone; resuming
// it moves it into its own worktree, so seeds and the next resume follow the
// pane.
func TestSessionsResumeMovesLegacyRecordIntoItsWorktree(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Project.Org, e.s.deps.Config.Project.PrimaryRepo = "acme", "widgets"
	id := e.create(t, callerRW, "legacy")
	if rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body.String())
	}
	_, _, _ = e.s.sessionStoreOrNil().update(id, func(r *sessionRecord) {
		r.Workspace = e.rt.WorkspaceDir("acme/widgets")
		r.ResumeCommand = "claude --resume abcdefgh"
	})
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/resume", map[string]any{}); rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	got := e.waitSettled(t, id)
	if want := e.rt.SessionWorkspaceDir(id, "acme/widgets"); got.Workspace != want {
		t.Errorf("record workspace after resume = %q, want the session's worktree %q", got.Workspace, want)
	}
}

// The sweep hands the runtime a liveness check backed by the store: only a
// worktree whose session record is gone is reported for removal.
func TestSessionsSweepRemovesOnlyOrphanedWorktrees(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Project.Org, e.s.deps.Config.Project.PrimaryRepo = "acme", "widgets"
	id := e.create(t, callerRW, "live")
	const orphan = "deadbeef00000000"
	e.rt.mu.Lock()
	e.rt.worktreeIDs = []string{id, orphan}
	e.rt.mu.Unlock()
	e.s.sweepSessions(time.Now())
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	if n := len(e.rt.swept); n == 0 || len(e.rt.swept[n-1]) != 1 || e.rt.swept[n-1][0] != orphan {
		t.Errorf("sweep reported %v gone, want only %s", e.rt.swept, orphan)
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

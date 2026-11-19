package dashboard

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// Fail safe: a sessions store that could not be loaded starts empty, so the
// worktree sweep must not run against it (it would remove the worktree of
// every session that still exists), neither in that process nor in a later
// one while the unreadable store is still set aside for recovery.
func TestSessionsSweepSkipsWorktreesWhenTheStoreFailedToLoad(t *testing.T) {
	e := newSessionTestEnv(t, true)
	dir := os.Getenv(sessionsDirEnv)
	if err := os.WriteFile(filepath.Join(dir, sessionsStoreFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	const orphan = "deadbeef00000000"
	e.rt.mu.Lock()
	e.rt.worktreeIDs = []string{orphan}
	e.rt.mu.Unlock()
	e.s.sweepSessions(time.Now())
	e.rt.mu.Lock()
	swept := len(e.rt.swept)
	e.rt.mu.Unlock()
	if swept != 0 {
		t.Fatalf("worktree sweep ran against a store that failed to load: %v", e.rt.swept)
	}
	if reason := e.s.sessionStoreOrNil().untrustedForSweep(); !strings.Contains(reason, "parsing sessions store") {
		t.Errorf("untrusted reason = %q", reason)
	}

	// A later process: the store now opens clean (the bad file was moved
	// aside), but the copy waiting for recovery still holds the sweep.
	later := newSessionTestEnvInDir(t, dir)
	later.rt.mu.Lock()
	later.rt.worktreeIDs = []string{orphan}
	later.rt.mu.Unlock()
	later.s.sweepSessions(time.Now())
	later.rt.mu.Lock()
	swept = len(later.rt.swept)
	later.rt.mu.Unlock()
	if swept != 0 {
		t.Fatalf("worktree sweep ran while an unreadable store is set aside: %v", later.rt.swept)
	}
	aside, _ := filepath.Glob(filepath.Join(dir, sessionsStoreFileName+sessionsStoreCorruptSuffix+"*"))
	if len(aside) != 1 {
		t.Fatalf("set-aside copies = %v, want one", aside)
	}
	// Once the operator has dealt with it, the sweep resumes.
	if err := os.Remove(aside[0]); err != nil {
		t.Fatal(err)
	}
	later.s.sweepSessions(time.Now())
	later.rt.mu.Lock()
	defer later.rt.mu.Unlock()
	if n := len(later.rt.swept); n != 1 || len(later.rt.swept[0]) != 1 || later.rt.swept[0][0] != orphan {
		t.Errorf("sweep after recovery reported %v, want %s", later.rt.swept, orphan)
	}
}

// At boot the sweeper first has the runtime lock every shared clone's git
// config and hooks (a clone from an earlier release left them group-writable).
func TestSessionsSweeperLocksSharedClonesAtBoot(t *testing.T) {
	e := newSessionTestEnv(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.s.StartSessionSweeper(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.rt.mu.Lock()
		n := e.rt.secured
		e.rt.mu.Unlock()
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SecureWorkspaces ran %d times at boot, want once", n)
		}
		time.Sleep(10 * time.Millisecond)
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

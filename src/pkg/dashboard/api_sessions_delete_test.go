package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// waitDeleted waits until the delete job of id has removed its record.
func (e *sessionTestEnv) waitDeleted(t *testing.T, id string) {
	t.Helper()
	st := e.s.sessionStoreOrNil()
	waitSessionsFor(t, "session "+id+" to be deleted", func() bool {
		_, ok := st.get(id)
		return !ok && !st.deleteInFlight(id)
	})
}

// waitDeleteFailed waits until the delete job of id has given up and put the
// record back with a reason, and returns the record.
func (e *sessionTestEnv) waitDeleteFailed(t *testing.T, id string) sessionRecord {
	t.Helper()
	st := e.s.sessionStoreOrNil()
	var rec sessionRecord
	waitSessionsFor(t, "the delete of session "+id+" to fail", func() bool {
		var ok bool
		rec, ok = st.get(id)
		return ok && rec.Status != sessionStatusDeleting && rec.DeleteError != "" && !st.deleteInFlight(id)
	})
	return rec
}

// endedSession creates a session as c and closes it, leaving it `ended` with
// its files on disk.
func (e *sessionTestEnv) endedSession(t *testing.T, c sessionCaller, name string, shared bool) string {
	t.Helper()
	id := e.createWith(t, c, name, shared)
	if got := e.statusOf(t, c, "DELETE", "/api/sessions/"+id, nil); got != http.StatusOK {
		t.Fatalf("close = %d", got)
	}
	return id
}

// closedAudit returns the detail of the newest session_closed audit entry for
// id that carries want.
func (e *sessionTestEnv) closedAudit(id, want string) (AuditEntry, bool) {
	var found AuditEntry
	ok := false
	for _, a := range e.auditActions() {
		if a.Action == auditSessionClosed && strings.Contains(a.Detail, "session="+id) && strings.Contains(a.Detail, want) {
			found, ok = a, true
		}
	}
	return found, ok
}

func sessionViewOf(t *testing.T, e *sessionTestEnv, c sessionCaller, id string) map[string]any {
	t.Helper()
	list := decodeSessionJSON(t, e.do(t, c, "GET", "/api/sessions", nil))
	views, _ := list["sessions"].([]any)
	for _, v := range views {
		if m, _ := v.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

type sessionResumeLookupRuntime struct {
	SessionRuntime
	onLookup func()
}

func (rt sessionResumeLookupRuntime) ResumeInfo(_, _, _ string) (crux.UserSessionResume, bool) {
	rt.onLookup()
	return crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}, true
}

func TestSessionResumeRechecksConcurrentDelete(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		deleteCode, resumeCode int
	}{
		{"deleting", http.StatusAccepted, http.StatusConflict},
		{"deleted", http.StatusOK, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSessionTestEnv(t, true)
			const id = "abcdef012345"
			if err := e.s.sessionStoreOrNil().put(sessionRecord{
				ID: id, Vendor: "claude", Creator: callerRW.user, Status: sessionStatusEnded, UIDSlot: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.deleteCode == http.StatusOK {
				e.rt.markGone(id)
			} else {
				release := make(chan struct{})
				e.rt.removeHook = func(string) error {
					<-release
					return nil
				}
				t.Cleanup(func() {
					close(release)
					e.waitDeleted(t, id)
				})
			}
			path := "/api/sessions/" + id
			e.s.deps.Sessions = sessionResumeLookupRuntime{SessionRuntime: e.rt, onLookup: func() {
				// Resume has read the ended record, but has not claimed it.
				res := e.do(t, callerRW, "DELETE", path, nil)
				if res.Code != tc.deleteCode {
					t.Fatalf("delete = %d, want %d: %s", res.Code, tc.deleteCode, res.Body.String())
				}
			}}
			res := e.do(t, callerRW, "POST", path+"/resume", nil)
			if res.Code != tc.resumeCode {
				t.Errorf("resume = %d, want %d: %s", res.Code, tc.resumeCode, res.Body.String())
			}
			if e.s.sessionStartInFlight(id) || len(e.rt.startedSpecs()) != 0 {
				t.Error("resume started after delete won")
			}
		})
	}
}

type sessionKillHookRuntime struct {
	SessionRuntime
	beforeKill func() error
}

func (rt sessionKillHookRuntime) Kill(id string) error {
	if err := rt.beforeKill(); err != nil {
		return err
	}
	return rt.SessionRuntime.Kill(id)
}

func TestSessionShutdownKeepsResumeOutUntilKillFinishes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		idle, remove bool
		killErr      error
		want         int
	}{
		{name: "close", want: http.StatusOK},
		{name: "close and remove", remove: true, want: http.StatusAccepted},
		{name: "idle sweep", idle: true},
		{name: "failed shutdown", killErr: errors.New("kill failed"), want: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSessionTestEnv(t, true)
			id := e.create(t, callerRW, "closing")
			st := e.s.sessionStoreOrNil()
			path := "/api/sessions/" + id
			e.rt.mu.Lock()
			e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
			e.rt.mu.Unlock()

			kills := 0
			e.s.deps.Sessions = sessionKillHookRuntime{SessionRuntime: e.rt, beforeKill: func() error {
				kills++
				if kills > 1 {
					return errors.New("duplicate shutdown")
				}
				if !e.s.sessionsMu.TryLock() {
					t.Error("shutdown held the shared session lock during Kill")
					return errors.New("lock held")
				}
				e.s.sessionsMu.Unlock()
				if rec, ok := st.get(id); !ok || rec.Status != sessionStatusRunning {
					t.Errorf("session before Kill = %+v, want running", rec)
				}
				if got := e.statusOf(t, callerRW, http.MethodPost, path+"/resume", nil); got != http.StatusConflict {
					t.Errorf("Resume during Kill = %d, want 409", got)
				}
				res := e.do(t, callerRW, http.MethodPost, path+"/input", map[string]any{"text": "draft", "submit": true})
				if res.Code != http.StatusConflict {
					t.Errorf("Send during Kill = %d, want 409: %s", res.Code, res.Body.String())
				}
				if got := e.statusOf(t, callerRW, http.MethodDelete, path, nil); got != http.StatusConflict {
					t.Errorf("second close during Kill = %d, want 409", got)
				}
				return tc.killErr
			}}
			if tc.idle {
				now := time.Now()
				idleSince := now.Add(-100 * time.Hour)
				if _, _, err := st.update(id, func(r *sessionRecord) {
					r.LastActivity = idleSince.UTC().Format(time.RFC3339)
				}); err != nil {
					t.Fatal(err)
				}
				e.rt.mu.Lock()
				e.rt.alive[id] = crux.UserSessionProbe{Exists: true, Activity: idleSince}
				e.rt.mu.Unlock()
				e.s.sweepSessions(now)
			} else {
				closePath := path
				if tc.remove {
					closePath += "?remove=true"
				}
				if got := e.statusOf(t, callerRW, http.MethodDelete, closePath, nil); got != tc.want {
					t.Fatalf("close = %d, want %d", got, tc.want)
				}
			}
			if kills != 1 || len(e.rt.startedSpecs()) != 1 {
				t.Fatalf("kills = %d, starts = %d; want one kill and only the original start", kills, len(e.rt.startedSpecs()))
			}

			if tc.remove {
				e.waitDeleted(t, id)
				if got := e.statusOf(t, callerRW, http.MethodPost, path+"/resume", nil); got != http.StatusNotFound {
					t.Fatalf("Resume after removal = %d, want 404", got)
				}
				return
			}
			if tc.killErr != nil {
				if rec, ok := st.get(id); !ok || rec.Status != sessionStatusRunning {
					t.Fatalf("session after failed Kill = %+v, want running", rec)
				}
				e.s.deps.Sessions = e.rt
				if got := e.statusOf(t, callerRW, http.MethodDelete, path, nil); got != http.StatusOK {
					t.Fatalf("close retry = %d, want 200", got)
				}
			}
			if got := e.statusOf(t, callerRW, http.MethodPost, path+"/resume", nil); got != http.StatusAccepted {
				t.Fatalf("Resume after shutdown = %d, want 202", got)
			}
			if rec := e.waitSettled(t, id); rec.Status != sessionStatusRunning || !e.rt.Probe(id).Exists {
				t.Fatalf("replacement after shutdown = %+v, want a live terminal", rec)
			}
		})
	}
}

type sessionCloseLogHook struct {
	slog.Handler
	afterClose func()
}

func (h sessionCloseLogHook) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "sessions: session closed" {
		h.afterClose()
	}
	return h.Handler.Handle(ctx, rec)
}

func TestSessionCloseAndRemoveKeepsResumeOutAfterKill(t *testing.T) {
	e := newSessionTestEnv(t, true)
	st := e.s.sessionStoreOrNil()
	var id string
	attemptedResume := false
	e.s.logger = slog.New(sessionCloseLogHook{
		Handler: slog.NewTextHandler(io.Discard, nil),
		afterClose: func() {
			rec, found := st.get(id)
			if !found || rec.Status != sessionStatusEnded || rec.ResumeCommand == "" {
				t.Fatalf("session before delete = %+v, exists = %v; want ended with a resume command", rec, found)
			}
			if !e.s.sessionsMu.TryLock() {
				t.Fatal("close held the shared lock while logging")
			}
			e.s.sessionsMu.Unlock()
			attemptedResume = true
			res := e.do(t, callerRW, http.MethodPost, "/api/sessions/"+id+"/resume", nil)
			if res.Code != http.StatusConflict {
				t.Errorf("Resume before delete claims the session = %d, want 409: %s", res.Code, res.Body.String())
			}
		},
	})
	id = e.create(t, callerRW, "close and remove")
	e.rt.mu.Lock()
	e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
	e.rt.mu.Unlock()

	res := e.do(t, callerRW, http.MethodDelete, "/api/sessions/"+id+"?remove=true", nil)
	if res.Code != http.StatusAccepted || !attemptedResume {
		t.Fatalf("close and remove = %d, attempted Resume = %v: %s", res.Code, attemptedResume, res.Body.String())
	}
	e.waitDeleted(t, id)
	if starts := len(e.rt.startedSpecs()); starts != 1 {
		t.Fatalf("terminal starts = %d, want only the original start", starts)
	}
}

// The incident (#18767): removing a session's files outlasts any request
// deadline. The request is answered at once, the session is visibly
// `deleting`, the removal carries on by itself, the record goes when it is
// done, and a second click meanwhile does not start a second removal.
func TestSessionDeleteRunsInTheBackgroundOnce(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.endedSession(t, callerRW, "slow", false)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	e.rt.mu.Lock()
	e.rt.removeHook = func(rid string) error {
		entered <- struct{}{}
		<-release
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		e.rt.removed = append(e.rt.removed, rid)
		e.rt.markGoneLocked(rid)
		return nil
	}
	e.rt.mu.Unlock()

	rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeSessionJSON(t, rec)
	sess, _ := body["session"].(map[string]any)
	if body["deleting"] != true || sess["status"] != sessionStatusDeleting {
		t.Fatalf("delete answered %v", body)
	}
	<-entered
	// The request is long over; the removal is still running.
	if view := sessionViewOf(t, e, callerRW, id); view == nil || view["status"] != sessionStatusDeleting {
		t.Fatalf("view while deleting = %v", view)
	}
	// A second click, a list (the reconciler) and a sweep do not start
	// another removal of the same session.
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusAccepted {
		t.Errorf("second delete = %d, want 202", got)
	}
	e.s.sweepSessions(time.Now())
	if n := e.rt.removeCount(); n != 1 {
		t.Fatalf("Remove ran %d times while one delete was in flight, want 1", n)
	}
	// A deleting session can be neither resumed nor typed into.
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/resume", map[string]any{}); got != http.StatusConflict {
		t.Errorf("resume while deleting = %d, want 409", got)
	}

	close(release)
	e.waitDeleted(t, id)
	if n := e.rt.removeCount(); n != 1 {
		t.Errorf("Remove ran %d times, want 1", n)
	}
	if view := sessionViewOf(t, e, callerRW, id); view != nil {
		t.Errorf("deleted session is still listed: %v", view)
	}
	a, ok := e.closedAudit(id, "outcome=success")
	if !ok || a.User != "rita" || !strings.Contains(a.Detail, "removed=true") || !strings.Contains(a.Detail, "duration=") || !strings.Contains(a.Detail, "trigger="+sessionDeleteReasonRequest) {
		t.Errorf("audit = %+v, %v", a, ok)
	}
}

// A real failure (here: the session holds the hive's current sign-in and
// adopting it failed) keeps the session intact with the reason, on the record,
// in the view and in the audit log; a retry then completes.
func TestSessionDeleteFailureKeepsTheSessionWithTheReason(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.endedSession(t, callerRW, "holder", false)
	e.rt.setRemoveErr(fmt.Errorf("%w; adopting it failed: disk full", crux.ErrUserSessionHoldsLogin))

	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusAccepted {
		t.Fatalf("delete = %d", got)
	}
	rec := e.waitDeleteFailed(t, id)
	const want = "this session holds the hive's current sign-in; adopting it failed: disk full"
	if rec.Status != sessionStatusEnded || rec.DeleteError != want || rec.DeleteFrom != "" {
		t.Fatalf("record after a refused delete = %+v", rec)
	}
	if got := e.rt.removedIDs(); len(got) != 0 {
		t.Errorf("a refused delete removed files: %v", got)
	}
	if view := sessionViewOf(t, e, callerRW, id); view == nil || view["status"] != sessionStatusEnded || view["delete_error"] != want {
		t.Errorf("view = %v", view)
	}
	if a, ok := e.closedAudit(id, "outcome=failed"); !ok || a.User != "rita" || !strings.Contains(a.Detail, "removed=false") || !strings.Contains(a.Detail, want) {
		t.Errorf("audit = %+v, %v", a, ok)
	}

	e.rt.setRemoveErr(nil)
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusAccepted {
		t.Fatalf("retry = %d", got)
	}
	e.waitDeleted(t, id)
	if _, ok := e.closedAudit(id, "outcome=success"); !ok {
		t.Error("no success audit entry for the retry")
	}
}

// A failed session whose delete fails goes back to `failed`, not `ended`.
func TestSessionDeleteFailureRestoresTheStatusItHad(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.endedSession(t, callerRW, "was-failed", false)
	st := e.s.sessionStoreOrNil()
	if _, _, err := st.update(id, func(r *sessionRecord) { r.Status, r.Error = sessionStatusFailed, "boom" }); err != nil {
		t.Fatal(err)
	}
	e.rt.setRemoveErr(errors.New("removing session sandbox: exit status 1: Device or resource busy"))
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusAccepted {
		t.Fatalf("delete = %d", got)
	}
	rec := e.waitDeleteFailed(t, id)
	if rec.Status != sessionStatusFailed || rec.Error != "boom" || !strings.Contains(rec.DeleteError, "Device or resource busy") {
		t.Errorf("record = %+v", rec)
	}
}

// The removal command reported an error, yet nothing of the session is left
// on disk: that is a successful delete, and the record goes.
func TestSessionDeleteErrorWithFilesGoneIsSuccess(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.endedSession(t, callerRW, "cancelled", false)
	e.rt.mu.Lock()
	e.rt.removeHook = func(rid string) error {
		e.rt.markGone(rid)
		return errors.New("removing session sandbox: exec: canceling Cmd: killing privileged child: exit status 1: su-exec: kill: No such file or directory")
	}
	e.rt.mu.Unlock()
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusAccepted {
		t.Fatalf("delete = %d", got)
	}
	e.waitDeleted(t, id)
	if _, ok := e.closedAudit(id, "outcome=success"); !ok {
		t.Error("no success audit entry")
	}
	if _, ok := e.closedAudit(id, "outcome=failed"); ok {
		t.Error("a delete that removed everything was audited as failed")
	}
}

// Repair: a record left behind by a delete that removed the files and then
// reported a failure. A delete click finishes it at once, without running a
// removal; the reconciler finishes it without anybody clicking.
func TestSessionDeleteRepairsARecordWhoseFilesAreGone(t *testing.T) {
	e := newSessionTestEnv(t, true)
	clicked := e.endedSession(t, callerRW, "clicked", false)
	swept := e.endedSession(t, callerRW, "swept", false)
	intact := e.endedSession(t, callerRW, "intact", false)
	e.rt.markGone(clicked)

	rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+clicked, nil)
	if rec.Code != http.StatusOK || decodeSessionJSON(t, rec)["removed"] != true {
		t.Fatalf("delete of a record with no files = %d: %s", rec.Code, rec.Body.String())
	}
	st := e.s.sessionStoreOrNil()
	if _, ok := st.get(clicked); ok {
		t.Error("the stale record survived a delete")
	}
	if n := e.rt.removeCount(); n != 0 {
		t.Errorf("Remove ran %d times for a session with nothing on disk", n)
	}

	e.rt.markGone(swept)
	// Any list reconciles.
	if view := sessionViewOf(t, e, callerRW, swept); view != nil {
		t.Errorf("the stale record is still listed: %v", view)
	}
	if _, ok := st.get(swept); ok {
		t.Error("the reconciler kept an ended record whose files are gone")
	}
	if a, ok := e.closedAudit(swept, "trigger="+sessionDeleteReasonFilesGone); !ok || a.User != sessionDeleteActorSystem {
		t.Errorf("audit = %+v, %v", a, ok)
	}
	if got, ok := st.get(intact); !ok || got.Status != sessionStatusEnded {
		t.Errorf("an ended session with its files was touched: %+v, %v", got, ok)
	}
	if n := e.rt.removeCount(); n != 0 {
		t.Errorf("Remove ran %d times", n)
	}
}

// A hive that restarted in the middle of a delete finds a `deleting` record
// with no job: the sweep at start finishes it.
func TestSessionDeleteInterruptedByARestartIsFinished(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.endedSession(t, callerRW, "interrupted", false)
	st := e.s.sessionStoreOrNil()
	if _, _, err := st.update(id, func(r *sessionRecord) { r.Status, r.DeleteFrom = sessionStatusDeleting, sessionStatusEnded }); err != nil {
		t.Fatal(err)
	}
	e.s.sweepSessions(time.Now())
	e.waitDeleted(t, id)
	if got := e.rt.removedIDs(); !idsContain(got, id) {
		t.Errorf("the interrupted delete did not remove the files: %v", got)
	}
	if a, ok := e.closedAudit(id, "trigger="+sessionDeleteReasonInterrupted); !ok || a.User != sessionDeleteActorSystem {
		t.Errorf("audit = %+v, %v", a, ok)
	}
}

// Every state can be deleted: a running session with ?remove=true, and a
// record a restart left `preparing`.
func TestSessionDeleteFromEveryState(t *testing.T) {
	e := newSessionTestEnv(t, true)
	running := e.create(t, callerRW, "running")
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+running+"?remove=true", nil); got != http.StatusAccepted {
		t.Fatalf("delete of a running session = %d, want 202", got)
	}
	e.waitDeleted(t, running)
	e.rt.mu.Lock()
	killed := idsContain(e.rt.killed, running)
	e.rt.mu.Unlock()
	if !killed || !idsContain(e.rt.removedIDs(), running) {
		t.Error("a running session was deleted without being closed and removed")
	}

	orphan := e.endedSession(t, callerRW, "orphan", false)
	st := e.s.sessionStoreOrNil()
	if _, _, err := st.update(orphan, func(r *sessionRecord) { r.Status, r.ResumeCommand = sessionStatusPreparing, "" }); err != nil {
		t.Fatal(err)
	}
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+orphan, nil); got != http.StatusAccepted {
		t.Fatalf("delete of an orphaned preparing session = %d, want 202", got)
	}
	e.waitDeleted(t, orphan)
}

// Delete is for the creator (with a write role) or an owner; sharing a
// session never grants it, and a refusal starts nothing.
func TestSessionDeleteRoleMatrix(t *testing.T) {
	cases := []struct {
		name   string
		shared bool
		caller sessionCaller
		want   int
	}{
		{"creator", false, callerRW, http.StatusAccepted},
		{"owner", false, callerOwner2, http.StatusAccepted},
		{"another writer, private session", false, callerRW2, http.StatusNotFound},
		{"another writer, shared session", true, callerRW2, http.StatusForbidden},
		{"read role, shared session", true, callerRead, http.StatusForbidden},
		{"merger role, shared session", true, callerMerger, http.StatusForbidden},
		{"unverified owner claim, shared session", true, callerForgedOwn, http.StatusForbidden},
		{"unverified owner claim, private session", false, callerForgedOwn, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSessionTestEnv(t, true)
			id := e.endedSession(t, callerRW, "matrix", tc.shared)
			if got := e.statusOf(t, tc.caller, "DELETE", "/api/sessions/"+id, nil); got != tc.want {
				t.Fatalf("delete as %s (%s) = %d, want %d", tc.caller.user, tc.caller.role, got, tc.want)
			}
			st := e.s.sessionStoreOrNil()
			if tc.want == http.StatusAccepted {
				e.waitDeleted(t, id)
				return
			}
			if rec, ok := st.get(id); !ok || rec.Status != sessionStatusEnded || st.deleteInFlight(id) {
				t.Errorf("a refused delete changed the session: %+v, %v", rec, ok)
			}
			if n := e.rt.removeCount(); n != 0 {
				t.Errorf("a refused delete ran Remove %d times", n)
			}
		})
	}
}

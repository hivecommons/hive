package dashboard

import (
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// The interleaving behind the false "the hive restarted while this session was
// being prepared": reconcileSessions lists a record as `preparing`, the
// background start then finishes (clears the in-flight mark and saves
// `running` under sessionsMu), and only then does the reconcile ask whether a
// start is in flight. It must judge the record as it is now, not as listed.
func TestSessionsReconcileDoesNotFailASessionWhoseStartJustFinished(t *testing.T) {
	e := newSessionTestEnv(t, true)
	st := e.s.sessionStoreOrNil()
	const id = "abcdef012345"
	now := time.Now().UTC().Format(time.RFC3339)
	if err := st.put(sessionRecord{ID: id, Name: "racing", Vendor: "claude", Creator: callerRW.user, CreatedAt: now, LastActivity: now, Status: sessionStatusPreparing, UIDSlot: 1, Message: "starting"}); err != nil {
		t.Fatal(err)
	}
	e.s.sessionsMu.Lock()
	e.s.setSessionStartInFlightLocked(id, true)
	e.s.sessionsMu.Unlock()

	finished := 0
	hook := func(srv *Server) {
		if srv != e.s {
			return
		}
		// Exactly what startSessionInBackground does on success, in one hold
		// of sessionsMu, after the reconcile took its snapshot.
		finished++
		e.s.sessionsMu.Lock()
		defer e.s.sessionsMu.Unlock()
		e.s.setSessionStartInFlightLocked(id, false)
		if _, _, err := st.update(id, func(r *sessionRecord) {
			r.Status, r.Message, r.Error = sessionStatusRunning, "", ""
		}); err != nil {
			t.Errorf("saving running state: %v", err)
		}
	}
	sessionsAfterReconcileSnapshot.Store(&hook)
	t.Cleanup(func() { sessionsAfterReconcileSnapshot.Store(nil) })
	e.s.reconcileSessions(e.rt, st)

	if finished != 1 {
		t.Fatalf("the snapshot hook ran %d times, want 1", finished)
	}
	rec, ok := st.get(id)
	if !ok {
		t.Fatal("record is gone")
	}
	if rec.Status != sessionStatusRunning || rec.Error != "" || rec.EndedAt != "" {
		t.Errorf("record after the race = status %q error %q ended %q, want running with no error", rec.Status, rec.Error, rec.EndedAt)
	}
}

// The other half of the rule: a record still `preparing` with a start in
// flight is left alone, and one with no start behind it is failed (the real
// restart case), using the record's current resume state.
func TestSessionsReconcileFailsOnlyAnOrphanedPreparingRecord(t *testing.T) {
	e := newSessionTestEnv(t, true)
	st := e.s.sessionStoreOrNil()
	now := time.Now().UTC().Format(time.RFC3339)
	const inFlight, orphan, resumed = "aaaaaaaa0001", "aaaaaaaa0002", "aaaaaaaa0003"
	for i, rec := range []sessionRecord{
		{ID: inFlight, Status: sessionStatusPreparing},
		{ID: orphan, Status: sessionStatusPreparing},
		{ID: resumed, Status: sessionStatusPreparing, ResumeCommand: "claude --resume 0123456789abcdef"},
	} {
		rec.Name, rec.Vendor, rec.Creator, rec.CreatedAt, rec.LastActivity, rec.UIDSlot = rec.ID, "claude", callerRW.user, now, now, i+1
		if err := st.put(rec); err != nil {
			t.Fatal(err)
		}
	}
	e.s.sessionsMu.Lock()
	e.s.setSessionStartInFlightLocked(inFlight, true)
	e.s.sessionsMu.Unlock()

	e.s.reconcileSessions(e.rt, st)

	if rec, _ := st.get(inFlight); rec.Status != sessionStatusPreparing || rec.Error != "" {
		t.Errorf("in-flight record = %q (%q), want preparing", rec.Status, rec.Error)
	}
	if rec, _ := st.get(orphan); rec.Status != sessionStatusFailed || rec.Error != sessionRestartedWhilePreparing {
		t.Errorf("orphan record = %q (%q), want failed with the restart reason", rec.Status, rec.Error)
	}
	if rec, _ := st.get(resumed); rec.Status != sessionStatusEnded || rec.Error != sessionRestartedWhilePreparing {
		t.Errorf("orphaned resume = %q (%q), want ended with the restart reason", rec.Status, rec.Error)
	}
}

type sessionStaleProbeRuntime struct {
	SessionRuntime
	afterProbe func()
}

func (rt *sessionStaleProbeRuntime) Probe(id string) crux.UserSessionProbe {
	p := rt.SessionRuntime.Probe(id)
	if rt.afterProbe != nil {
		hook := rt.afterProbe
		rt.afterProbe = nil
		hook()
	}
	return p
}

func TestSessionsReconcileRechecksProbeAfterResume(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "stale probe")
	st := e.s.sessionStoreOrNil()
	e.rt.mu.Lock()
	delete(e.rt.alive, id)
	e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
	e.rt.mu.Unlock()
	rt := &sessionStaleProbeRuntime{SessionRuntime: e.rt, afterProbe: func() {
		path := "/api/sessions/" + id
		if got := e.statusOf(t, callerRW, http.MethodDelete, path, nil); got != http.StatusOK {
			t.Fatalf("close = %d, want 200", got)
		}
		if got := e.statusOf(t, callerRW, http.MethodPost, path+"/resume", nil); got != http.StatusAccepted {
			t.Fatalf("Resume = %d, want 202", got)
		}
		e.waitSettled(t, id)
	}}
	if probes := e.s.reconcileSessions(rt, st); !probes[id].Exists {
		t.Fatal("a stale probe hid the replacement terminal")
	}
	if rec, ok := st.get(id); !ok || rec.Status != sessionStatusRunning {
		t.Fatalf("record after stale probe = %+v, want running", rec)
	}
}

package dashboard

import (
	"testing"
	"time"
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

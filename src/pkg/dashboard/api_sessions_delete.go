package dashboard

import (
	"fmt"
	"net/http"
	"time"
)

// Deleting a session (#18767).
//
// Removing a session's files can take a minute or more (a HOME with a CLI's
// caches, deleted through the privileged helper), so it never runs inside the
// request that asked for it. DELETE answers 202 at once, the record becomes
// `deleting`, and one background job per session id does the work:
//
//  1. the runtime removes the files (SessionRuntime.Remove: login check, then
//     the worktree with its work kept on a branch, then the sandbox);
//  2. the record is removed LAST, and only when that succeeded, so its uid
//     slot is never handed to a new session over old files;
//  3. a failure puts the record back to the status it had, with the reason on
//     it (delete_error), and a later DELETE retries.
//
// What decides the outcome is what is on disk, not what a command said: a
// removal that reports an error after everything is gone is a success. For
// the same reason a record whose files are already gone is finished without a
// job, by a DELETE (answered 200) and by the reconciler, which also restarts
// the job of a `deleting` record that a hive restart interrupted.
//
// Every outcome gets one log line with the duration and one session_closed
// audit entry, failures included.

const (
	// sessionDeleteActorSystem is the audit actor of a delete nobody asked
	// for by name (retention, repair).
	sessionDeleteActorSystem = "system"

	// Triggers of a delete, recorded in the log and the audit detail.
	sessionDeleteReasonRequest     = "request"
	sessionDeleteReasonRetention   = "retention"
	sessionDeleteReasonInterrupted = "interrupted-delete"
	sessionDeleteReasonFilesGone   = "files-already-gone"

	// sessionDeleteRemoveParam, set to "true" on DELETE of a running session,
	// deletes it after closing it instead of keeping it resumable.
	sessionDeleteRemoveParam = "remove"

	// sessionDeleteErrorMaxRunes bounds the stored failure reason.
	sessionDeleteErrorMaxRunes = 500
)

// sessionDeleteOutcome is what beginSessionDelete did.
type sessionDeleteOutcome int

const (
	// sessionDeleteStarted: the record is `deleting` and its job is running.
	sessionDeleteStarted sessionDeleteOutcome = iota
	// sessionDeleteInFlight: a job for this id is already running.
	sessionDeleteInFlight
	// sessionDeleteDone: nothing was left on disk, so the record is gone
	// already (or there never was one).
	sessionDeleteDone
	// sessionDeleteNotDeletable: the record is not in a state that can be
	// deleted (it is running or being prepared, or it changed meanwhile).
	sessionDeleteNotDeletable
	// sessionDeleteStoreFailed: the sessions store could not be saved.
	sessionDeleteStoreFailed
)

// sessionDeleteFailureText is the reason shown to people and stored on the
// record: the error with tokens redacted, bounded.
func sessionDeleteFailureText(err error) string {
	reason := []rune(redactTokens(err.Error()))
	if len(reason) > sessionDeleteErrorMaxRunes {
		reason = append(reason[:sessionDeleteErrorMaxRunes], '…')
	}
	return string(reason)
}

// reportSessionDelete writes the one log line and the one audit entry of a
// delete outcome.
func (s *Server) reportSessionDelete(user, id, trigger string, took time.Duration, err error) {
	dur := took.Round(time.Millisecond).String()
	if err != nil {
		reason := sessionDeleteFailureText(err)
		if s.logger != nil {
			s.logger.Warn("sessions: session delete failed; the session is kept and the delete can be retried",
				"id", id, "user", user, "trigger", trigger, "duration", dur, "error", reason)
		}
		s.auditSession(user, auditSessionClosed, auditDetail("session", id, "removed", "false", "outcome", "failed", "trigger", trigger, "duration", dur, "error", reason))
		return
	}
	if s.logger != nil {
		s.logger.Info("sessions: session deleted", "id", id, "user", user, "trigger", trigger, "duration", dur)
	}
	s.auditSession(user, auditSessionClosed, auditDetail("session", id, "removed", "true", "outcome", "success", "trigger", trigger, "duration", dur))
}

// beginSessionDelete starts the delete of session id, or finishes it at once
// when nothing of it is left on disk. from, when not empty, is the status the
// record must still have (the reconciler acts on a snapshot). It never blocks
// on the removal itself.
func (s *Server) beginSessionDelete(rt SessionRuntime, st *sessionStore, id, user, trigger, from string) (sessionDeleteOutcome, sessionRecord, error) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	cur, ok := st.get(id)
	if !ok {
		return sessionDeleteDone, cur, nil
	}
	if st.deleteInFlight(id) {
		return sessionDeleteInFlight, cur, nil
	}
	switch cur.Status {
	case sessionStatusEnded, sessionStatusFailed, sessionStatusDeleting:
	default:
		return sessionDeleteNotDeletable, cur, nil
	}
	if from != "" && cur.Status != from {
		return sessionDeleteNotDeletable, cur, nil
	}
	if rt.Removed(id) {
		if err := st.remove(id); err != nil {
			err = fmt.Errorf("the session's files are gone, but the sessions store could not be saved: %w", err)
			s.reportSessionDelete(user, id, trigger, 0, err)
			return sessionDeleteStoreFailed, cur, err
		}
		s.dropSessionQueue(id)
		s.reportSessionDelete(user, id, trigger, 0, nil)
		return sessionDeleteDone, cur, nil
	}
	if !st.claimDelete(id) {
		return sessionDeleteInFlight, cur, nil
	}
	updated, _, err := st.update(id, func(r *sessionRecord) {
		if r.Status != sessionStatusDeleting {
			r.DeleteFrom = r.Status
		}
		r.Status, r.DeleteError = sessionStatusDeleting, ""
	})
	if err != nil {
		// Nothing was removed; the record goes back to what it was.
		_, _, _ = st.update(id, func(r *sessionRecord) { *r = cur })
		st.releaseDelete(id)
		err = fmt.Errorf("the sessions store could not be saved: %w", err)
		s.reportSessionDelete(user, id, trigger, 0, err)
		return sessionDeleteStoreFailed, cur, err
	}
	go s.runSessionDelete(rt, st, id, user, trigger)
	return sessionDeleteStarted, updated, nil
}

// runSessionDelete is the delete job of one session: files first, the record
// last. It is tied to no request; the runtime's privileged steps run under
// their own deadlines.
func (s *Server) runSessionDelete(rt SessionRuntime, st *sessionStore, id, user, trigger string) {
	start := time.Now()
	err := rt.Remove(id)
	if err != nil && rt.Removed(id) {
		// The removal reported an error and left nothing behind: it removed
		// the session.
		if s.logger != nil {
			s.logger.Info("sessions: the removal reported an error, but nothing of the session is left on disk; treated as removed", "id", id, "error", sessionDeleteFailureText(err))
		}
		err = nil
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	defer st.releaseDelete(id)
	if err == nil {
		if rerr := st.remove(id); rerr != nil {
			err = fmt.Errorf("the session's files are removed, but the sessions store could not be saved: %w", rerr)
		}
	}
	if err == nil {
		s.dropSessionQueue(id)
		s.reportSessionDelete(user, id, trigger, time.Since(start), nil)
		return
	}
	reason := sessionDeleteFailureText(err)
	if _, _, uerr := st.update(id, func(r *sessionRecord) {
		r.Status = r.DeleteFrom
		if r.Status == "" || r.Status == sessionStatusDeleting {
			r.Status = sessionStatusEnded
		}
		r.DeleteFrom, r.DeleteError = "", reason
	}); uerr != nil && s.logger != nil {
		s.logger.Warn("sessions: persisting a failed delete failed", "id", id, "error", uerr)
	}
	s.reportSessionDelete(user, id, trigger, time.Since(start), err)
}

// reconcileSessionRemoval finishes what a delete left unfinished: it restarts
// the job of a `deleting` record that has none (the hive restarted in the
// middle of it), and drops an `ended` record whose files are all gone (a
// delete from before #18767 that removed the files and then reported a
// failure). Neither needs a person.
func (s *Server) reconcileSessionRemoval(rt SessionRuntime, st *sessionStore, rec sessionRecord) {
	switch rec.Status {
	case sessionStatusDeleting:
		if !st.deleteInFlight(rec.ID) {
			s.beginSessionDelete(rt, st, rec.ID, sessionDeleteActorSystem, sessionDeleteReasonInterrupted, sessionStatusDeleting)
		}
	case sessionStatusEnded:
		if rt.Removed(rec.ID) {
			s.beginSessionDelete(rt, st, rec.ID, sessionDeleteActorSystem, sessionDeleteReasonFilesGone, sessionStatusEnded)
		}
	}
}

// handleSessionDelete closes a running session (tmux is killed and the record
// is kept, marked ended, so the session can be resumed where the vendor
// supports it; with ?remove=true it is then deleted as well). On an ended or
// failed session it starts the delete job and answers 202 with the record as
// `deleting`; 200 with removed=true means there was nothing left to remove
// and the record is gone already.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	rec, actor, ok := s.sessionForMutation(w, r, st)
	if !ok {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if rec.Status == sessionStatusPreparing {
		// A record left `preparing` by a restart has no start behind it: it
		// is failed here rather than on the next list, so it can be deleted.
		s.failOrphanedPreparingSession(st, rec.ID, now)
		if rec, ok = st.get(rec.ID); !ok {
			jsonError(w, "session not found", http.StatusNotFound)
			return
		}
		if rec.Status == sessionStatusPreparing {
			// A start is in flight; the record changes under it as soon as
			// it finishes, so the client is asked to come back then.
			jsonError(w, "session is still being prepared; close it once it is running or failed", http.StatusConflict)
			return
		}
	}
	if rec.Status == sessionStatusRunning {
		// Capture the resume handle before the tmux server goes away.
		s.markSessionEnded(rt, st, rec, now)
		if err := rt.Kill(rec.ID); err != nil {
			reason := redactTokens(err.Error())
			if s.logger != nil {
				s.logger.Warn("sessions: kill failed", "id", rec.ID, "user", actor.user, "error", reason)
			}
			s.auditSession(actor.user, auditSessionClosed, auditDetail("session", rec.ID, "outcome", "failed", "error", reason))
			jsonError(w, "could not close the session: "+reason, http.StatusBadGateway)
			return
		}
		if s.logger != nil {
			s.logger.Info("sessions: session closed", "id", rec.ID, "user", actor.user)
		}
		s.auditSession(actor.user, auditSessionClosed, auditDetail("session", rec.ID))
		if r.URL.Query().Get(sessionDeleteRemoveParam) != "true" {
			jsonResponse(w, map[string]any{"ok": true})
			return
		}
	}
	outcome, cur, err := s.beginSessionDelete(rt, st, rec.ID, actor.user, sessionDeleteReasonRequest, "")
	switch outcome {
	case sessionDeleteDone:
		jsonResponse(w, map[string]any{"ok": true, "removed": true})
	case sessionDeleteStarted, sessionDeleteInFlight:
		jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "deleting": true, "session": s.sessionViewFor(cur, nil, rt, actor)})
	case sessionDeleteNotDeletable:
		jsonError(w, "the session changed state while it was being deleted; try again", http.StatusConflict)
	default:
		jsonError(w, "could not delete the session: "+sessionDeleteFailureText(err), http.StatusInternalServerError)
	}
}

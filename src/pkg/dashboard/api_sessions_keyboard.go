package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Keyboard lease and presence for shared sessions.
//
// Several people can watch one session, but exactly one of them, the keyboard
// holder, has a writable terminal; everyone else is attached read-only (tmux
// `attach-session -r`, decided here and enforced by ttyd-tmux.sh, never by the
// browser). The creator holds the keyboard from the start. Owners and the
// creator can always take it; another read-write user can take it only when
// the holder has released it or has been idle for sessionKeyboardGrace. Taking
// it detaches the previous holder's writable tmux client, so the hand-over is
// immediate on both sides: the new holder's browser re-attaches writable, the
// old holder's re-attaches read-only.
//
// Presence is the set of live terminal websockets: guardTerminalSession
// registers one record per attach for as long as the proxied websocket lasts.
const (
	// sessionKeyboardGraceEnv overrides the idle grace, in seconds, after
	// which a read-write user who is neither owner nor creator may take the
	// keyboard from its holder.
	sessionKeyboardGraceEnv     = "HIVE_SESSIONS_KEYBOARD_GRACE_SEC"
	defaultSessionKeyboardGrace = 60 * time.Second

	sessionKeyboardActionTake    = "take"
	sessionKeyboardActionRelease = "release"

	// auditSessionKeyboard records a keyboard hand-over: from=<old> to=<new>
	// (to is empty on a release).
	auditSessionKeyboard = "session_keyboard"

	// ttydAttachWriteDomain keys the second per-session token, appended as the
	// THIRD ttyd arg only for the keyboard holder's attach. ttyd-tmux.sh
	// verifies it exactly like the attach token and attaches read-only when
	// it is missing or wrong (api_sessions_keyboard.go; terminal_proxy.go).
	ttydAttachWriteDomain = "hive-ttyd-attach-rw-v1"
)

// sessionKeyboardGrace resolves the idle grace: env (seconds) over default.
func sessionKeyboardGrace() time.Duration {
	return time.Duration(sessionEnvInt64(sessionKeyboardGraceEnv, int64(defaultSessionKeyboardGrace/time.Second))) * time.Second
}

// ttydAttachWriteToken is the token that makes a session attach writable:
// hex(HMAC-SHA256(key, "hive-ttyd-attach-rw-v1\x00" + session)). "" without a
// key.
func ttydAttachWriteToken(session string) string {
	return ttydAttachTokenIn(ttydAttachWriteDomain, session)
}

// sessionAttach is one live terminal websocket on a user session.
type sessionAttach struct {
	ID       string
	Session  string
	User     string
	Since    time.Time
	Keyboard bool
}

// sessionViewer is the presence entry of GET /api/sessions.
type sessionViewer struct {
	User          string `json:"user"`
	AttachedSince string `json:"attached_since"`
	Keyboard      bool   `json:"keyboard"`
}

// holdsKeyboard reports whether the actor is the session's keyboard holder.
func holdsKeyboard(a sessionActor, rec sessionRecord) bool {
	return a.user != "" && rec.KeyboardHolder == a.user
}

// sessionAttachWritable decides, server-side, whether this attach gets the
// keyboard: the actor must be allowed to use the session AND hold its
// keyboard. Nothing the browser sends can change the answer.
func sessionAttachWritable(a sessionActor, rec sessionRecord) bool {
	return canUseSession(a, rec) && holdsKeyboard(a, rec)
}

// canTakeKeyboardFreely: owners and the creator never wait for the grace.
func canTakeKeyboardFreely(a sessionActor, rec sessionRecord) bool {
	return a.owner || a.isCreatorOf(rec)
}

// sessionKeyboardIdle reports whether the holder has been idle long enough
// for another writer to take the keyboard: there is no holder, the holder is
// not attached at all, or nothing has happened in the pane since the later of
// the hand-over and the last activity for at least grace.
func sessionKeyboardIdle(rec sessionRecord, p crux.UserSessionProbe, holderAttached bool, now time.Time, grace time.Duration) bool {
	if rec.KeyboardHolder == "" || !holderAttached {
		return true
	}
	last := p.Activity
	for _, ts := range []string{rec.LastActivity, rec.KeyboardSince} {
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(last) {
			last = t
		}
	}
	return now.Sub(last) >= grace
}

// beginSessionAttach registers a live attach and returns its id.
func (s *Server) beginSessionAttach(sessionID, user string, keyboard bool) string {
	s.sessionAttachMu.Lock()
	defer s.sessionAttachMu.Unlock()
	if s.sessionAttaches == nil {
		s.sessionAttaches = map[string]*sessionAttach{}
	}
	s.sessionAttachSeq++
	id := strconv.FormatUint(s.sessionAttachSeq, 10)
	s.sessionAttaches[id] = &sessionAttach{ID: id, Session: sessionID, User: user, Since: time.Now(), Keyboard: keyboard}
	return id
}

func (s *Server) endSessionAttach(id string) {
	s.sessionAttachMu.Lock()
	defer s.sessionAttachMu.Unlock()
	delete(s.sessionAttaches, id)
}

// sessionViewers lists who is attached to a session, one entry per user
// (earliest attach, keyboard when any of their attaches is writable), sorted
// keyboard holder first then by user.
func (s *Server) sessionViewers(sessionID string) []sessionViewer {
	s.sessionAttachMu.Lock()
	byUser := map[string]*sessionViewer{}
	for _, a := range s.sessionAttaches {
		if a.Session != sessionID {
			continue
		}
		v, ok := byUser[a.User]
		if !ok {
			v = &sessionViewer{User: a.User, AttachedSince: a.Since.UTC().Format(time.RFC3339)}
			byUser[a.User] = v
		}
		if a.Since.UTC().Format(time.RFC3339) < v.AttachedSince {
			v.AttachedSince = a.Since.UTC().Format(time.RFC3339)
		}
		v.Keyboard = v.Keyboard || a.Keyboard
	}
	s.sessionAttachMu.Unlock()
	out := make([]sessionViewer, 0, len(byUser))
	for _, v := range byUser {
		out = append(out, *v)
	}
	sortSessionViewers(out)
	return out
}

func sortSessionViewers(vs []sessionViewer) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && sessionViewerLess(vs[j], vs[j-1]); j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

func sessionViewerLess(a, b sessionViewer) bool {
	if a.Keyboard != b.Keyboard {
		return a.Keyboard
	}
	return a.User < b.User
}

// sessionUserAttached reports whether user has a live attach on the session.
func (s *Server) sessionUserAttached(sessionID, user string) bool {
	if user == "" {
		return false
	}
	s.sessionAttachMu.Lock()
	defer s.sessionAttachMu.Unlock()
	for _, a := range s.sessionAttaches {
		if a.Session == sessionID && a.User == user {
			return true
		}
	}
	return false
}

// handleSessionKeyboard is POST /api/sessions/{id}/keyboard with body
// {"action":"take"|"release"}.
func (s *Server) handleSessionKeyboard(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "the keyboard requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, ok := s.sessionForUse(w, st, r.PathValue("id"), actor)
	if !ok {
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if !readSmallJSON(w, r, &body) {
		return
	}
	now := time.Now()
	from := rec.KeyboardHolder
	switch strings.TrimSpace(body.Action) {
	case sessionKeyboardActionTake:
		if holdsKeyboard(actor, rec) {
			p := rt.Probe(rec.ID)
			jsonResponse(w, map[string]any{"ok": true, "session": s.sessionViewFor(rec, &p, rt, actor)})
			return
		}
		if !canTakeKeyboardFreely(actor, rec) {
			p := rt.Probe(rec.ID)
			grace := sessionKeyboardGrace()
			if !sessionKeyboardIdle(rec, p, s.sessionUserAttached(rec.ID, rec.KeyboardHolder), now, grace) {
				jsonStatusResponse(w, http.StatusConflict, map[string]any{"ok": false,
					"error":  rec.KeyboardHolder + " has the keyboard; it can be taken once they release it or have been idle for " + strconv.Itoa(int(grace/time.Second)) + "s",
					"holder": rec.KeyboardHolder, "grace_seconds": int(grace / time.Second)})
				return
			}
		}
		s.handSessionKeyboard(w, rt, st, rec, actor, from, actor.user, now)
	case sessionKeyboardActionRelease:
		if !holdsKeyboard(actor, rec) && !canManageSession(actor, rec) {
			jsonError(w, "only the keyboard holder, the session's creator or an owner may release the keyboard", http.StatusForbidden)
			return
		}
		if from == "" {
			p := rt.Probe(rec.ID)
			jsonResponse(w, map[string]any{"ok": true, "session": s.sessionViewFor(rec, &p, rt, actor)})
			return
		}
		s.handSessionKeyboard(w, rt, st, rec, actor, from, "", now)
	default:
		jsonError(w, `action must be "take" or "release"`, http.StatusBadRequest)
	}
}

// handSessionKeyboard moves the lease from -> to (to "" releases it), detaches
// the previous holder's writable client, audits and answers with the session.
func (s *Server) handSessionKeyboard(w http.ResponseWriter, rt SessionRuntime, st *sessionStore, rec sessionRecord, actor sessionActor, from, to string, now time.Time) {
	updated, _, err := st.update(rec.ID, func(x *sessionRecord) {
		x.KeyboardHolder = to
		x.KeyboardSince = ""
		if to != "" {
			x.KeyboardSince = now.UTC().Format(time.RFC3339)
		}
	})
	if err != nil {
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	// The old holder's tmux client is the only writable one; detaching it ends
	// their typing at once. Their browser re-attaches read-only on its own.
	if from != "" {
		if err := rt.DetachWritableClients(rec.ID); err != nil && s.logger != nil {
			s.logger.Warn("sessions: detaching the previous keyboard holder failed", "id", rec.ID, "error", err)
		}
		s.markSessionAttachesReadOnly(rec.ID)
	}
	s.auditSession(actor.user, auditSessionKeyboard, auditDetail("session", rec.ID, "from", from, "to", to))
	p := rt.Probe(rec.ID)
	jsonResponse(w, map[string]any{"ok": true, "session": s.sessionViewFor(updated, &p, rt, actor)})
}

// markSessionAttachesReadOnly flips every presence record of the session to
// read-only; the detached client's record goes away when its websocket closes,
// and until then presence must not show the old holder with the keyboard.
func (s *Server) markSessionAttachesReadOnly(sessionID string) {
	s.sessionAttachMu.Lock()
	defer s.sessionAttachMu.Unlock()
	for _, a := range s.sessionAttaches {
		if a.Session == sessionID {
			a.Keyboard = false
		}
	}
}

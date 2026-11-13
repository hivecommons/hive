package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

// Sessions: interactive, named CLI terminals shared by everyone on a spoke.
// The tmux/ttyd engine sits behind SessionRuntime so it can be swapped; this
// file holds policy (roles, limits, validation, audit) and the JSON API.
// The terminal itself is attached through /terminal (terminal_handoff.go);
// keystrokes typed there are NOT attributable, only /input and /upload are.

const (
	sessionsLogDefaultLines = 1000
	sessionsLogMaxLines     = 5000
	// sessionsSweepInterval is how often idle sessions are checked.
	sessionsSweepInterval = 5 * time.Minute
	// sessionsJSONOverheadBytes is slack on top of the input cap for the JSON
	// envelope (and escaping) when bounding the request body.
	sessionsJSONOverheadBytes = 4096
	// sessionsMultipartOverheadBytes is slack on top of the upload cap for
	// multipart framing.
	sessionsMultipartOverheadBytes = 64 << 10
	// sessionsSmallBodyBytes bounds create/patch/handoff bodies.
	sessionsSmallBodyBytes = 8 << 10

	sessionsMaxInputBytesEnv  = "HIVE_SESSIONS_MAX_INPUT_BYTES"
	sessionsMaxUploadBytesEnv = "HIVE_SESSIONS_MAX_UPLOAD_BYTES"
	sessionsMaxUploadsEnv     = "HIVE_SESSIONS_MAX_UPLOADS"

	defaultSessionsMaxInputBytes  = 64 << 10
	defaultSessionsMaxUploadBytes = 10 << 20
	defaultSessionsMaxUploads     = 25

	sessionUploadsDirName = "uploads"
	sessionUploadDirMode  = 0o700
	sessionUploadFileMode = 0o600
	sessionFileNameMax    = 100
	sessionUploadFormName = "file"

	auditSessionCreated = "session_created"
	auditSessionClosed  = "session_closed"
	auditSessionRenamed = "session_renamed"
	auditSessionInput   = "session_input"
	auditSessionUpload  = "session_upload"
)

// SessionRuntime is the engine behind Sessions (implemented by
// crux/agent.UserSessionRuntime). Tests substitute a fake.
type SessionRuntime interface {
	Vendors() []crux.UserSessionVendor
	SupportsResume(vendor string) bool
	WorkDir(id string) string
	Start(spec crux.UserSessionSpec) error
	Probe(id string) crux.UserSessionProbe
	Kill(id string) error
	Capture(id string, lines int) (string, error)
	SendText(id, text string, submit bool) error
	ResumeInfo(id, vendor string) (crux.UserSessionResume, bool)
}

type sessionLimits struct {
	MaxSessions   int   `json:"max_sessions"`
	MaxPerUser    int   `json:"max_per_user"`
	IdleTTLHours  int   `json:"idle_ttl_hours"`
	MaxInputBytes int64 `json:"max_input_bytes"`
	MaxUploadByte int64 `json:"max_upload_bytes"`
	MaxUploads    int   `json:"max_uploads_per_session"`
}

type sessionView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Vendor       string `json:"vendor"`
	Creator      string `json:"creator"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity"`
	Status       string `json:"status"`
	Attached     int    `json:"attached"`
	TmuxName     string `json:"tmux_name"`
	// Resumable (additive) is true for an ended session whose vendor can
	// reopen its last conversation.
	Resumable bool `json:"resumable"`
}

func (s *Server) registerSessionRoutes() {
	s.mux.HandleFunc("GET /api/sessions", s.handleSessionsList)
	s.mux.HandleFunc("POST /api/sessions", s.handleSessionCreate)
	s.mux.HandleFunc("PATCH /api/sessions/{id}", s.handleSessionPatch)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	s.mux.HandleFunc("GET /api/sessions/{id}/log", s.handleSessionLog)
	s.mux.HandleFunc("POST /api/sessions/{id}/input", s.handleSessionInput)
	s.mux.HandleFunc("POST /api/sessions/{id}/upload", s.handleSessionUpload)
	s.mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleSessionResume)
}

// sessionsRuntime returns the runtime when the feature is enabled and wired.
func (s *Server) sessionsRuntime() (SessionRuntime, bool) {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.SessionsEnabled() || s.deps.Sessions == nil {
		return nil, false
	}
	return s.deps.Sessions, true
}

func (s *Server) sessionStoreOrNil() *sessionStore {
	s.sessionsOnce.Do(func() {
		dir := sessionsDir()
		st, err := openSessionStore(dir)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("sessions store unreadable; starting empty and keeping the old file aside", "error", err)
			}
			_ = os.Rename(st.path, fmt.Sprintf("%s.corrupt-%d", st.path, time.Now().Unix()))
		}
		s.sessionStore = st
	})
	return s.sessionStore
}

func (s *Server) sessionLimits() sessionLimits {
	cfg := s.deps.Config
	return sessionLimits{
		MaxSessions:   cfg.SessionsMax(),
		MaxPerUser:    cfg.SessionsMaxPerUser(),
		IdleTTLHours:  cfg.SessionsIdleTTLHours(),
		MaxInputBytes: sessionEnvInt64(sessionsMaxInputBytesEnv, defaultSessionsMaxInputBytes),
		MaxUploadByte: sessionEnvInt64(sessionsMaxUploadBytesEnv, defaultSessionsMaxUploadBytes),
		MaxUploads:    int(sessionEnvInt64(sessionsMaxUploadsEnv, defaultSessionsMaxUploads)),
	}
}

func sessionEnvInt64(name string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// sessionActor classifies the caller. owner requires the server-verified
// owner marker (or a genuinely open spoke with no role at all); write is owner
// or read-write, the same set that may use the terminal.
type sessionActor struct {
	user  string
	owner bool
	write bool
}

func (s *Server) sessionActorOf(r *http.Request) sessionActor {
	role := strings.TrimSpace(r.Header.Get("X-Hive-Role"))
	a := sessionActor{user: requestUser(r)}
	switch {
	case isOwnerRole(role):
		a.owner = r.Header.Get(ownerRoleVerifiedHeader) == "true"
	case role == "":
		a.owner = s.requestRoleAllowsOwner(r)
	}
	a.write = a.owner || strings.EqualFold(role, config.RoleReadWrite)
	return a
}

func (s *Server) auditSession(user, action, detail string) {
	if s.audit != nil {
		s.audit.Log(user, action, detail, "")
	}
}

func (st sessionRecord) view(p *crux.UserSessionProbe, rt SessionRuntime) sessionView {
	v := sessionView{
		ID: st.ID, Name: st.Name, Group: st.Group, Vendor: st.Vendor, Creator: st.Creator,
		CreatedAt: st.CreatedAt, LastActivity: st.LastActivity, Status: st.Status,
		TmuxName: "hive-sess-" + st.ID,
	}
	if st.Status == sessionStatusRunning && p != nil {
		v.Attached = p.Attached
		if !p.Activity.IsZero() {
			if t, err := time.Parse(time.RFC3339, st.LastActivity); err != nil || p.Activity.After(t) {
				v.LastActivity = p.Activity.UTC().Format(time.RFC3339)
			}
		}
	}
	v.Resumable = st.Status == sessionStatusEnded && st.ResumeCommand != "" && rt.SupportsResume(st.Vendor)
	return v
}

// reconcileSessions probes every running session once, marks those whose tmux
// session is gone (pod restart, CLI exit) as ended and returns the probes of
// the survivors. A restart of the hive process alone leaves tmux running, so
// those sessions stay running and reattach.
func (s *Server) reconcileSessions(rt SessionRuntime, st *sessionStore) map[string]crux.UserSessionProbe {
	probes := map[string]crux.UserSessionProbe{}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, rec := range st.list() {
		if rec.Status != sessionStatusRunning {
			continue
		}
		p := rt.Probe(rec.ID)
		if p.Exists && !p.Dead {
			probes[rec.ID] = p
			continue
		}
		s.markSessionEnded(rt, st, rec, now)
	}
	return probes
}

func (s *Server) markSessionEnded(rt SessionRuntime, st *sessionStore, rec sessionRecord, now string) {
	resume := ""
	if info, ok := rt.ResumeInfo(rec.ID, rec.Vendor); ok {
		resume = info.Command
	}
	if _, _, err := st.update(rec.ID, func(r *sessionRecord) {
		r.Status = sessionStatusEnded
		r.EndedAt = now
		if resume != "" {
			r.ResumeCommand = resume
		}
	}); err != nil && s.logger != nil {
		s.logger.Warn("sessions: persisting ended state failed", "id", rec.ID, "error", err)
	}
}

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.sessionsRuntime()
	if !ok {
		jsonResponse(w, map[string]any{"enabled": false, "sessions": []sessionView{}, "vendors": []crux.UserSessionVendor{}, "limits": map[string]any{}})
		return
	}
	st := s.sessionStoreOrNil()
	probes := s.reconcileSessions(rt, st)
	views := []sessionView{}
	for _, rec := range st.list() {
		var p *crux.UserSessionProbe
		if pr, ok := probes[rec.ID]; ok {
			p = &pr
		}
		views = append(views, rec.view(p, rt))
	}
	vendors := []crux.UserSessionVendor{}
	for _, v := range rt.Vendors() {
		if s.deps.Config.BackendAllowed(v.ID) {
			vendors = append(vendors, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{"enabled": true, "sessions": views, "vendors": vendors, "limits": s.sessionLimits()})
}

// sessionsGate answers 404 when the feature is off or unwired.
func (s *Server) sessionsGate(w http.ResponseWriter) (SessionRuntime, *sessionStore, bool) {
	rt, ok := s.sessionsRuntime()
	if !ok {
		jsonError(w, "sessions are not enabled on this hive", http.StatusNotFound)
		return nil, nil, false
	}
	return rt, s.sessionStoreOrNil(), true
}

func readSmallJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, sessionsSmallBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) liveSessionCounts(st *sessionStore, user string) (total, mine int) {
	for _, rec := range st.list() {
		if rec.Status == sessionStatusRunning {
			total++
			if rec.Creator == user {
				mine++
			}
		}
	}
	return total, mine
}

// checkSessionCapacity answers 429 when a new live session would exceed a cap.
func (s *Server) checkSessionCapacity(w http.ResponseWriter, st *sessionStore, user string) bool {
	lim := s.sessionLimits()
	total, mine := s.liveSessionCounts(st, user)
	switch {
	case total >= lim.MaxSessions:
		jsonError(w, fmt.Sprintf("this hive already has the maximum of %d live sessions", lim.MaxSessions), http.StatusTooManyRequests)
		return false
	case mine >= lim.MaxPerUser:
		jsonError(w, fmt.Sprintf("you already have the maximum of %d live sessions", lim.MaxPerUser), http.StatusTooManyRequests)
		return false
	}
	return true
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "creating a session requires owner or read-write role", http.StatusForbidden)
		return
	}
	var body struct {
		Name   string `json:"name"`
		Group  string `json:"group"`
		Vendor string `json:"vendor"`
	}
	if !readSmallJSON(w, r, &body) {
		return
	}
	body.Name, body.Group, body.Vendor = strings.TrimSpace(body.Name), strings.TrimSpace(body.Group), strings.TrimSpace(body.Vendor)
	if err := validateSessionLabel("name", body.Name, sessionNameMaxRunes, true); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSessionLabel("group", body.Group, sessionGroupMaxRunes, false); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var vendor *crux.UserSessionVendor
	for _, v := range rt.Vendors() {
		if v.ID == body.Vendor {
			v := v
			vendor = &v
		}
	}
	if vendor == nil {
		jsonError(w, "unknown vendor", http.StatusBadRequest)
		return
	}
	if !s.deps.Config.BackendAllowed(vendor.ID) {
		jsonError(w, "this vendor is not allowed on this hive", http.StatusForbidden)
		return
	}
	if !vendor.Available {
		jsonError(w, vendor.Label+" is not installed on this hive", http.StatusConflict)
		return
	}

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if !s.checkSessionCapacity(w, st, actor.user) {
		return
	}
	id, err := st.newID()
	if err != nil {
		jsonError(w, "could not allocate a session id", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := sessionRecord{ID: id, Name: body.Name, Group: body.Group, Vendor: vendor.ID, Creator: actor.user, CreatedAt: now, LastActivity: now, Status: sessionStatusRunning}
	if err := rt.Start(crux.UserSessionSpec{ID: id, Vendor: vendor.ID}); err != nil {
		if s.logger != nil {
			s.logger.Warn("sessions: start failed", "id", id, "vendor", vendor.ID, "error", err)
		}
		jsonError(w, "could not start the session", http.StatusInternalServerError)
		return
	}
	if err := st.put(rec); err != nil {
		_ = rt.Kill(id)
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionCreated, auditDetail("session", id, "vendor", vendor.ID))
	jsonStatusResponse(w, http.StatusCreated, map[string]any{"ok": true, "session": rec.view(&crux.UserSessionProbe{}, rt)})
}

// sessionOwnedBy loads the record and enforces creator-or-owner.
func (s *Server) sessionForMutation(w http.ResponseWriter, r *http.Request, st *sessionStore) (sessionRecord, sessionActor, bool) {
	rec, ok := st.get(r.PathValue("id"))
	if !ok {
		jsonError(w, "session not found", http.StatusNotFound)
		return rec, sessionActor{}, false
	}
	actor := s.sessionActorOf(r)
	if !actor.owner && !(actor.write && rec.Creator == actor.user) {
		jsonError(w, "only the session's creator or an owner may change it", http.StatusForbidden)
		return rec, actor, false
	}
	return rec, actor, true
}

func (s *Server) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	rec, actor, ok := s.sessionForMutation(w, r, st)
	if !ok {
		return
	}
	var body struct {
		Name  *string `json:"name"`
		Group *string `json:"group"`
	}
	if !readSmallJSON(w, r, &body) {
		return
	}
	if body.Name == nil && body.Group == nil {
		jsonError(w, "nothing to change", http.StatusBadRequest)
		return
	}
	name, group := rec.Name, rec.Group
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if err := validateSessionLabel("name", name, sessionNameMaxRunes, true); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if body.Group != nil {
		group = strings.TrimSpace(*body.Group)
		if err := validateSessionLabel("group", group, sessionGroupMaxRunes, false); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	updated, _, err := st.update(rec.ID, func(x *sessionRecord) { x.Name, x.Group = name, group })
	if err != nil {
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionRenamed, auditDetail("session", rec.ID))
	p := rt.Probe(rec.ID)
	jsonResponse(w, map[string]any{"ok": true, "session": updated.view(&p, rt)})
}

// handleSessionDelete closes a running session: tmux is killed and the
// metadata is kept, marked ended, so the session can be resumed where the
// vendor supports it. DELETE on an already ended session removes its metadata.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	rec, actor, ok := s.sessionForMutation(w, r, st)
	if !ok {
		return
	}
	if rec.Status == sessionStatusEnded {
		if err := rt.Kill(rec.ID); err != nil && s.logger != nil {
			s.logger.Warn("sessions: killing ended session's leftover tmux failed", "id", rec.ID, "error", err)
		}
		if err := st.remove(rec.ID); err != nil {
			jsonError(w, "could not save the session store", http.StatusInternalServerError)
			return
		}
		s.auditSession(actor.user, auditSessionClosed, auditDetail("session", rec.ID, "removed", "true"))
		jsonResponse(w, map[string]any{"ok": true})
		return
	}
	// Capture the resume handle before the tmux server goes away.
	s.markSessionEnded(rt, st, rec, time.Now().UTC().Format(time.RFC3339))
	if err := rt.Kill(rec.ID); err != nil {
		if s.logger != nil {
			s.logger.Warn("sessions: kill failed", "id", rec.ID, "error", err)
		}
		jsonError(w, "could not close the session", http.StatusBadGateway)
		return
	}
	s.auditSession(actor.user, auditSessionClosed, auditDetail("session", rec.ID))
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) handleSessionLog(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	rec, found := st.get(r.PathValue("id"))
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	n := sessionsLogDefaultLines
	if v := r.URL.Query().Get("lines"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			jsonError(w, "lines must be a positive integer", http.StatusBadRequest)
			return
		}
		n = parsed
	}
	if n > sessionsLogMaxLines {
		n = sessionsLogMaxLines
	}
	lines := []string{}
	text, err := rt.Capture(rec.ID, n)
	if err != nil {
		if rec.Status == sessionStatusRunning {
			jsonError(w, "could not read the session log", http.StatusBadGateway)
			return
		}
	} else {
		// Every pane surface redacts tokens before output leaves the server.
		lines = strings.Split(strings.TrimRight(redactTokens(text), "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{"ok": true, "lines": lines, "status": rec.Status})
}

func (s *Server) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "sending input requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, found := st.get(r.PathValue("id"))
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if rec.Status != sessionStatusRunning {
		jsonError(w, "session is not running", http.StatusConflict)
		return
	}
	lim := s.sessionLimits()
	r.Body = http.MaxBytesReader(w, r.Body, lim.MaxInputBytes+sessionsJSONOverheadBytes)
	var body struct {
		Text   string `json:"text"`
		Submit bool   `json:"submit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, "input is too large", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if int64(len(body.Text)) > lim.MaxInputBytes {
		jsonError(w, fmt.Sprintf("input exceeds %d bytes", lim.MaxInputBytes), http.StatusRequestEntityTooLarge)
		return
	}
	if body.Text == "" && !body.Submit {
		jsonError(w, "nothing to send", http.StatusBadRequest)
		return
	}
	if err := rt.SendText(rec.ID, body.Text, body.Submit); err != nil {
		if strings.Contains(err.Error(), "control character") || strings.Contains(err.Error(), "UTF-8") {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonError(w, "could not send input to the session", http.StatusBadGateway)
		return
	}
	s.touchSession(st, rec.ID)
	// Who typed, how much, and into which session; never the text itself.
	s.auditSession(actor.user, auditSessionInput, auditDetail("session", rec.ID, "bytes", strconv.Itoa(len(body.Text)), "submit", strconv.FormatBool(body.Submit)))
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) touchSession(st *sessionStore, id string) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, _ = st.update(id, func(x *sessionRecord) { x.LastActivity = now })
}

func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "resuming a session requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, found := st.get(r.PathValue("id"))
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if rec.Status != sessionStatusEnded {
		jsonError(w, "session is not ended", http.StatusConflict)
		return
	}
	if !rt.SupportsResume(rec.Vendor) {
		jsonError(w, "this vendor cannot resume a session", http.StatusConflict)
		return
	}
	cmd := rec.ResumeCommand
	if cmd == "" {
		if info, ok := rt.ResumeInfo(rec.ID, rec.Vendor); ok {
			cmd = info.Command
		}
	}
	if cmd == "" {
		jsonError(w, "no conversation to resume was found", http.StatusConflict)
		return
	}
	if !s.deps.Config.BackendAllowed(rec.Vendor) {
		jsonError(w, "this vendor is not allowed on this hive", http.StatusForbidden)
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if !s.checkSessionCapacity(w, st, actor.user) {
		return
	}
	if err := rt.Start(crux.UserSessionSpec{ID: rec.ID, Vendor: rec.Vendor, ResumeCommand: cmd}); err != nil {
		if s.logger != nil {
			s.logger.Warn("sessions: resume failed", "id", rec.ID, "error", err)
		}
		jsonError(w, "could not resume the session", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated, _, err := st.update(rec.ID, func(x *sessionRecord) {
		x.Status, x.EndedAt, x.LastActivity = sessionStatusRunning, "", now
	})
	if err != nil {
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionCreated, auditDetail("session", rec.ID, "vendor", rec.Vendor, "resumed", "true"))
	jsonResponse(w, map[string]any{"ok": true, "session": updated.view(&crux.UserSessionProbe{}, rt)})
}

// StartSessionSweeper reconciles sessions at start (hive restarted or the pod
// was replaced) and then closes idle ones every sessionsSweepInterval until ctx
// ends. It is a no-op loop while the feature is off.
func (s *Server) StartSessionSweeper(ctx context.Context) {
	go func() {
		s.sweepSessions(time.Now())
		t := time.NewTicker(sessionsSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.sweepSessions(now)
			}
		}
	}()
}

// sweepSessions marks vanished sessions ended, closes sessions idle past the
// TTL (no attached client and no activity), and prunes old ended metadata.
func (s *Server) sweepSessions(now time.Time) {
	rt, ok := s.sessionsRuntime()
	if !ok {
		return
	}
	st := s.sessionStoreOrNil()
	probes := s.reconcileSessions(rt, st)
	ttl := time.Duration(s.deps.Config.SessionsIdleTTLHours()) * time.Hour
	for _, rec := range st.list() {
		switch rec.Status {
		case sessionStatusRunning:
			p, alive := probes[rec.ID]
			if !alive || p.Attached > 0 {
				continue
			}
			last, err := time.Parse(time.RFC3339, rec.LastActivity)
			if err != nil {
				continue
			}
			if p.Activity.After(last) {
				last = p.Activity
			}
			if now.Sub(last) < ttl {
				continue
			}
			s.markSessionEnded(rt, st, rec, now.UTC().Format(time.RFC3339))
			if err := rt.Kill(rec.ID); err != nil && s.logger != nil {
				s.logger.Warn("sessions: idle kill failed", "id", rec.ID, "error", err)
			}
			s.auditSession("system", auditSessionClosed, auditDetail("session", rec.ID, "reason", "idle"))
		case sessionStatusEnded:
			if ended, err := time.Parse(time.RFC3339, rec.EndedAt); err == nil && now.Sub(ended) > sessionEndedRetention {
				_ = st.remove(rec.ID)
			}
		}
	}
}

// sessionHandoffTarget validates a terminal handoff request for a session id:
// the feature must be on and the session must be running. It writes the error
// response itself.
func (s *Server) sessionHandoffTarget(w http.ResponseWriter, id string) bool {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return false
	}
	rec, found := st.get(id)
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return false
	}
	if rec.Status != sessionStatusRunning {
		jsonError(w, "session is not running", http.StatusConflict)
		return false
	}
	if p := rt.Probe(id); !p.Exists {
		jsonError(w, "session is not running", http.StatusConflict)
		return false
	}
	return true
}

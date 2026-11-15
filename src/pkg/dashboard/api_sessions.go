package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

// Sessions: interactive, named CLI terminals. The tmux/ttyd engine sits behind
// SessionRuntime so it can be swapped; this file holds policy (roles, limits,
// validation, ownership, audit) and the JSON API. The terminal itself is
// attached through /terminal (terminal_handoff.go, terminal_session_gate.go).
// Keystrokes typed there are attributable only to the keyboard holder (one
// writable attach at a time, api_sessions_keyboard.go); /input is queued and
// audited per sender at delivery (api_sessions_queue.go), /upload at once.
//
// Ownership model. A session is PRIVATE by default: only its creator and
// owners may see it, read its log, watch its terminal, type into it or upload
// to it. The creator (or an owner) may mark it shared, which makes it visible
// to every role and usable (Send, upload, keyboard) by every read-write user:
// the "jam session" mode. The read role only ever watches: the scrollback and
// a read-only live terminal, never the keyboard, Send or upload. Whatever the
// flag, only the creator or an owner may rename, close, resume or delete a
// session or change the flag.

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
	// auditSessionStarted records the OUTCOME of a background start
	// (outcome=running or outcome=failed with the reason), since the create
	// entry is written before the workspace is even cloned.
	auditSessionStarted = "session_started"
	auditSessionClosed  = "session_closed"
	auditSessionRenamed = "session_renamed"
	auditSessionInput   = "session_input"
	auditSessionUpload  = "session_upload"

	// sessionTmuxPrefix is the tmux session name prefix of a user session
	// (crux/agent.UserSessionTmuxPrefix).
	sessionTmuxPrefix = "hive-sess-"
)

// SessionRuntime is the engine behind Sessions (implemented by
// crux/agent.UserSessionRuntime). Tests substitute a fake.
type SessionRuntime interface {
	Vendors() []crux.UserSessionVendor
	SupportsResume(vendor string) bool
	WorkDir(id string) string
	// WorkspaceDir is the path sessions of the "org/repo" workspace open in
	// ("" for an empty or malformed repo). Start clones it on first use.
	WorkspaceDir(repo string) string
	Start(spec crux.UserSessionSpec) error
	Probe(id string) crux.UserSessionProbe
	Kill(id string) error
	Capture(id string, lines int) (string, error)
	SendText(id, text string, submit bool) error
	// DetachWritableClients detaches every tmux client of the session that is
	// not read-only: the previous keyboard holder's browser, when the keyboard
	// changes hands, so it cannot keep typing until it re-attaches (read-only).
	DetachWritableClients(id string) error
	// ResumeInfo captures the vendor's newest conversation for the session;
	// workDir is where the CLI ran ("" = the session's own work dir).
	ResumeInfo(id, vendor, workDir string) (crux.UserSessionResume, bool)
	// StoreUpload writes src as the named file under the session's uploads
	// directory, as the session's own uid, and returns its absolute path.
	StoreUpload(id, fileName string, src io.Reader, maxBytes int64, maxFiles int) (string, int64, error)
	// Remove ends the session and deletes everything it owned on disk.
	Remove(id string) error
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
	// Shared (additive) is true when every read-write user may type into the
	// session and every role may watch it; false means creator + owners only.
	Shared bool `json:"shared"`
	// WorkspaceRepo and Workspace (additive) are the "org/repo" and path of the
	// hive's shared checkout the session runs in; empty when it runs in its
	// own empty work dir.
	WorkspaceRepo string `json:"workspace_repo"`
	Workspace     string `json:"workspace"`
	// Message (additive) says what a `preparing` session is waiting on; Error
	// (additive) is why a `failed` session, or a resume that fell back to
	// `ended`, did not start.
	Message string `json:"message"`
	Error   string `json:"error"`
	// KeyboardHolder and KeyboardSince (additive) are the keyboard lease: the
	// one user whose terminal is writable and since when ("" = released).
	// KeyboardMine is true when the caller holds it.
	KeyboardHolder string `json:"keyboard_holder"`
	KeyboardSince  string `json:"keyboard_since"`
	KeyboardMine   bool   `json:"keyboard_mine"`
	// Viewers (additive) is who has a live terminal on the session and whether
	// theirs is the writable one; Queue (additive) is the pending Send queue.
	Viewers []sessionViewer  `json:"viewers"`
	Queue   sessionQueueView `json:"queue"`
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
	s.mux.HandleFunc("POST /api/sessions/{id}/keyboard", s.handleSessionKeyboard)
	s.mux.HandleFunc("DELETE /api/sessions/{id}/input/{queue_id}", s.handleSessionInputCancel)
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

func (a sessionActor) isCreatorOf(rec sessionRecord) bool {
	return a.user != "" && rec.Creator == a.user
}

// canViewSession: owners, the creator, and everyone for a shared session.
func canViewSession(a sessionActor, rec sessionRecord) bool {
	return a.owner || a.isCreatorOf(rec) || rec.Shared
}

// canUseSession (input, upload, terminal): owners, the writing creator, and
// writers for a shared session. The read role never types.
func canUseSession(a sessionActor, rec sessionRecord) bool {
	return a.owner || (a.write && (a.isCreatorOf(rec) || rec.Shared))
}

// canManageSession (rename, share flag, close, resume, delete): the creator
// with a write role, or an owner. Sharing never grants this.
func canManageSession(a sessionActor, rec sessionRecord) bool {
	return a.owner || (a.write && a.isCreatorOf(rec))
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
		TmuxName: sessionTmuxPrefix + st.ID, Shared: st.Shared,
		WorkspaceRepo: st.WorkspaceRepo, Workspace: st.Workspace,
		Message: st.Message, Error: st.Error,
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
	v.KeyboardHolder, v.KeyboardSince = st.KeyboardHolder, st.KeyboardSince
	v.Viewers = []sessionViewer{}
	v.Queue = sessionQueueView{Items: []sessionQueueItemView{}}
	return v
}

// sessionViewFor is view() plus the per-caller and live parts: whether the
// caller holds the keyboard, who is attached, and the Send queue with the
// caller's own items marked.
func (s *Server) sessionViewFor(rec sessionRecord, p *crux.UserSessionProbe, rt SessionRuntime, actor sessionActor) sessionView {
	v := rec.view(p, rt)
	v.KeyboardMine = holdsKeyboard(actor, rec)
	if rec.Status == sessionStatusRunning {
		v.Viewers = s.sessionViewers(rec.ID)
		v.Queue = s.sessionQueueSnapshot(rec.ID, actor.user)
	}
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
		if rec.Status == sessionStatusPreparing && !s.sessionStartInFlight(rec.ID) {
			// Only a restart leaves a preparing record with no goroutine
			// behind it; nothing will ever move it on, so say so.
			status := sessionStatusFailed
			if rec.ResumeCommand != "" {
				status = sessionStatusEnded
			}
			s.markSessionFailed(st, rec.ID, status, sessionRestartedWhilePreparing, now)
			continue
		}
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
	if info, ok := rt.ResumeInfo(rec.ID, rec.Vendor, rec.Workspace); ok {
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
	// Nothing queued can be delivered to an ended session.
	s.dropSessionQueue(rec.ID)
}

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.sessionsRuntime()
	if !ok {
		jsonResponse(w, map[string]any{"enabled": false, "sessions": []sessionView{}, "vendors": []crux.UserSessionVendor{}, "limits": map[string]any{}})
		return
	}
	st := s.sessionStoreOrNil()
	probes := s.reconcileSessions(rt, st)
	actor := s.sessionActorOf(r)
	views := []sessionView{}
	for _, rec := range st.list() {
		// Ids of sessions the caller may not see are not disclosed at all.
		if !canViewSession(actor, rec) {
			continue
		}
		var p *crux.UserSessionProbe
		if pr, ok := probes[rec.ID]; ok {
			p = &pr
		}
		views = append(views, s.sessionViewFor(rec, p, rt, actor))
	}
	vendors := []crux.UserSessionVendor{}
	for _, v := range rt.Vendors() {
		if s.deps.Config.BackendAllowed(v.ID) {
			vendors = append(vendors, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	repo := s.sessionsWorkspaceRepo(rt)
	jsonResponse(w, map[string]any{"enabled": true, "sessions": views, "vendors": vendors, "limits": s.sessionLimits(),
		"workspace": map[string]string{"repo": repo, "path": rt.WorkspaceDir(repo)}})
}

// sessionsWorkspaceRepo is the "org/repo" new sessions open in: the
// configured override, else the hive's project repo. "" when the repo is
// unset or malformed, in which case sessions open in their own empty dir.
func (s *Server) sessionsWorkspaceRepo(rt SessionRuntime) string {
	repo := s.deps.Config.SessionsWorkspaceRepo()
	if repo == "" || rt.WorkspaceDir(repo) == "" {
		return ""
	}
	return repo
}

// sessionsSpektacularBinary is the spektacular executable sessions init their
// workspace with (runs.spektacular.binary, default resolved through PATH).
func (s *Server) sessionsSpektacularBinary() string {
	if bin := strings.TrimSpace(s.deps.Config.Runs.Spektacular.Binary); bin != "" {
		return bin
	}
	return config.DefaultSpektacularBinary
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

// sessionIsLive reports whether a record holds (or is about to hold) a tmux
// server: running sessions and those still being prepared both count against
// the caps, or two quick clicks would overshoot them.
func sessionIsLive(rec sessionRecord) bool {
	return rec.Status == sessionStatusRunning || rec.Status == sessionStatusPreparing
}

func (s *Server) liveSessionCounts(st *sessionStore, user string) (total, mine int) {
	for _, rec := range st.list() {
		if sessionIsLive(rec) {
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
		Shared bool   `json:"shared"`
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
	repo := s.sessionsWorkspaceRepo(rt)
	// The record is saved (and answered) as `preparing` BEFORE anything is
	// started: the first session on a hive clones the whole project
	// repository, hands it to the sessions group and runs spek init, which
	// takes minutes and used to outlive the ingress's request timeout (a 504
	// in the browser for a session that then started anyway). The start runs
	// in the background and moves the record to running or failed.
	// The creator holds the keyboard from the start (api_sessions_keyboard.go).
	rec := sessionRecord{ID: id, Name: body.Name, Group: body.Group, Vendor: vendor.ID, Creator: actor.user, CreatedAt: now, LastActivity: now, Status: sessionStatusPreparing, UIDSlot: st.freeSlot(), Shared: body.Shared,
		WorkspaceRepo: repo, Workspace: rt.WorkspaceDir(repo), Message: s.sessionPreparingMessage(rt, repo, vendor.Label),
		KeyboardHolder: actor.user, KeyboardSince: now}
	s.setSessionStartInFlightLocked(id, true)
	if err := st.put(rec); err != nil {
		s.setSessionStartInFlightLocked(id, false)
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionCreated, auditDetail("session", id, "vendor", vendor.ID, "status", sessionStatusPreparing))
	spec := crux.UserSessionSpec{ID: id, Vendor: vendor.ID, UIDSlot: rec.UIDSlot, WorkspaceRepo: repo, SpektacularBinary: s.sessionsSpektacularBinary()}
	go s.startSessionInBackground(rt, st, spec, actor.user, sessionStatusFailed)
	jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "session": rec.view(&crux.UserSessionProbe{}, rt)})
}

// sessionRestartedWhilePreparing is the error a preparing record is given when
// the hive comes back up with no start behind it.
const sessionRestartedWhilePreparing = "the hive restarted while this session was being prepared; start it again"

// sessionPreparingMessage is the human line a preparing session shows: the
// first session for a repository clones it, later ones only wait for the
// vendor to start.
func (s *Server) sessionPreparingMessage(rt SessionRuntime, repo, vendorLabel string) string {
	if repo == "" {
		return "starting " + vendorLabel
	}
	if _, err := os.Lstat(rt.WorkspaceDir(repo)); err != nil {
		return "preparing workspace: cloning " + repo + " (first session on this hive)"
	}
	return "preparing workspace " + repo + ", starting " + vendorLabel
}

// sessionStartInFlight reports whether this process is starting the session.
func (s *Server) sessionStartInFlight(id string) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return s.sessionsPreparing[id]
}

// setSessionStartInFlightLocked records (or clears) an in-flight start. The
// caller holds sessionsMu.
func (s *Server) setSessionStartInFlightLocked(id string, inFlight bool) {
	if s.sessionsPreparing == nil {
		s.sessionsPreparing = map[string]bool{}
	}
	if inFlight {
		s.sessionsPreparing[id] = true
		return
	}
	delete(s.sessionsPreparing, id)
}

// startSessionInBackground runs the runtime's Start for a record that was saved
// as `preparing` and moves it to running, or to failStatus with the error on
// the record (`failed` for a create, whose sandbox is removed; `ended` for a
// resume, which keeps the session resumable). Concurrent starts that need the
// same workspace are serialised inside the runtime, so a second click while
// the clone runs waits for that clone instead of starting another. Nothing
// here is tied to the request that asked: the runtime's privileged steps run
// under their own deadlines.
func (s *Server) startSessionInBackground(rt SessionRuntime, st *sessionStore, spec crux.UserSessionSpec, user, failStatus string) {
	err := rt.Start(spec)
	now := time.Now().UTC().Format(time.RFC3339)
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.setSessionStartInFlightLocked(spec.ID, false)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("sessions: start failed", "id", spec.ID, "vendor", spec.Vendor, "error", err)
		}
		if failStatus == sessionStatusFailed {
			s.removeSessionFiles(rt, spec.ID)
		}
		reason := redactTokens(err.Error())
		s.markSessionFailed(st, spec.ID, failStatus, reason, now)
		s.auditSession(user, auditSessionStarted, auditDetail("session", spec.ID, "vendor", spec.Vendor, "outcome", failStatus, "error", reason))
		return
	}
	if _, _, err := st.update(spec.ID, func(r *sessionRecord) {
		r.Status, r.LastActivity, r.EndedAt, r.Message, r.Error = sessionStatusRunning, now, "", "", ""
	}); err != nil && s.logger != nil {
		s.logger.Warn("sessions: persisting running state failed", "id", spec.ID, "error", err)
	}
	s.auditSession(user, auditSessionStarted, auditDetail("session", spec.ID, "vendor", spec.Vendor, "outcome", sessionStatusRunning))
}

// markSessionFailed records why a start did not happen: status is `failed`
// for a session that never ran, `ended` for a resume that fell through (the
// session stays resumable).
func (s *Server) markSessionFailed(st *sessionStore, id, status, reason, now string) {
	if _, _, err := st.update(id, func(r *sessionRecord) {
		r.Status = status
		if r.EndedAt == "" {
			r.EndedAt = now
		}
		r.Message, r.Error = "", reason
	}); err != nil && s.logger != nil {
		s.logger.Warn("sessions: persisting failed state failed", "id", id, "error", err)
	}
}

// sessionOwnedBy loads the record and enforces creator-or-owner.
func (s *Server) sessionForMutation(w http.ResponseWriter, r *http.Request, st *sessionStore) (sessionRecord, sessionActor, bool) {
	rec, ok := st.get(r.PathValue("id"))
	if !ok {
		jsonError(w, "session not found", http.StatusNotFound)
		return rec, sessionActor{}, false
	}
	actor := s.sessionActorOf(r)
	if !canViewSession(actor, rec) {
		jsonError(w, "session not found", http.StatusNotFound)
		return rec, actor, false
	}
	if !canManageSession(actor, rec) {
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
		Name   *string `json:"name"`
		Group  *string `json:"group"`
		Shared *bool   `json:"shared"`
	}
	if !readSmallJSON(w, r, &body) {
		return
	}
	if body.Name == nil && body.Group == nil && body.Shared == nil {
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
	shared := rec.Shared
	if body.Shared != nil {
		shared = *body.Shared
	}
	updated, _, err := st.update(rec.ID, func(x *sessionRecord) { x.Name, x.Group, x.Shared = name, group, shared })
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
	if rec.Status == sessionStatusPreparing {
		// A start is in flight; the record changes under it as soon as it
		// finishes, so the client is asked to come back then.
		jsonError(w, "session is still being prepared; close it once it is running or failed", http.StatusConflict)
		return
	}
	if rec.Status == sessionStatusEnded || rec.Status == sessionStatusFailed {
		// Files first: if they cannot be removed the record (and its uid slot)
		// stays, so the slot is never handed to a new session over old data.
		if err := rt.Remove(rec.ID); err != nil {
			if s.logger != nil {
				s.logger.Warn("sessions: removing session files failed", "id", rec.ID, "error", err)
			}
			jsonError(w, "could not remove the session's files", http.StatusInternalServerError)
			return
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
	if !found || !canViewSession(s.sessionActorOf(r), rec) {
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
	rec, ok := s.sessionForUse(w, st, r.PathValue("id"), actor)
	if !ok {
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
	// Validate now (so the sender hears about an escape character at once),
	// deliver later: the text is queued and typed by the session's worker when
	// the pane is idle (api_sessions_queue.go), audited then, as this user.
	clean, err := crux.SanitizeUserSessionInput(body.Text)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	item, pos, ok := s.enqueueSessionInput(rec.ID, sessionQueueItem{User: actor.user, Text: clean, Submit: body.Submit})
	if !ok {
		jsonError(w, fmt.Sprintf("this session already has %d queued sends; wait for some to be delivered", sessionQueueMaxItems), http.StatusTooManyRequests)
		return
	}
	s.ensureSessionQueueWorker(rt, st, rec.ID)
	jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "queued": map[string]any{"id": item.ID, "position": pos},
		"queue": s.sessionQueueSnapshot(rec.ID, actor.user)})
}

// sessionForUse loads a running session the actor may type into, writing the
// 404 (not visible), 403 (visible, not usable) or 409 (not running) itself.
func (s *Server) sessionForUse(w http.ResponseWriter, st *sessionStore, id string, actor sessionActor) (sessionRecord, bool) {
	rec, found := st.get(id)
	if !found || !canViewSession(actor, rec) {
		jsonError(w, "session not found", http.StatusNotFound)
		return rec, false
	}
	if !canUseSession(actor, rec) {
		jsonError(w, "only the session's creator or an owner may use a private session", http.StatusForbidden)
		return rec, false
	}
	if rec.Status != sessionStatusRunning {
		jsonError(w, "session is not running", http.StatusConflict)
		return rec, false
	}
	return rec, true
}

// removeSessionFiles deletes a session's on-disk state, logging a failure.
func (s *Server) removeSessionFiles(rt SessionRuntime, id string) {
	if err := rt.Remove(id); err != nil && s.logger != nil {
		s.logger.Warn("sessions: removing session files failed", "id", id, "error", err)
	}
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
	if !found || !canViewSession(actor, rec) {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if !canManageSession(actor, rec) {
		jsonError(w, "only the session's creator or an owner may resume it", http.StatusForbidden)
		return
	}
	if rec.UIDSlot < 1 {
		jsonError(w, "this session predates per-session isolation and cannot be resumed; start a new one", http.StatusConflict)
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
		if info, ok := rt.ResumeInfo(rec.ID, rec.Vendor, rec.Workspace); ok {
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
	// A resume reopens the workspace the session was created in (or its own
	// work dir for a record without one), never a repo configured since. Like
	// a create it is answered as `preparing` and started in the background
	// (the workspace may have to be cloned again, spek init may run); a
	// failure puts the session back to ended with the reason, still resumable.
	vendorLabel := rec.Vendor
	for _, v := range rt.Vendors() {
		if v.ID == rec.Vendor && v.Label != "" {
			vendorLabel = v.Label
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	message := s.sessionPreparingMessage(rt, rec.WorkspaceRepo, vendorLabel)
	s.setSessionStartInFlightLocked(rec.ID, true)
	// Whoever resumes the session holds its keyboard.
	updated, _, err := st.update(rec.ID, func(x *sessionRecord) {
		x.Status, x.LastActivity, x.Message, x.Error = sessionStatusPreparing, now, message, ""
		x.KeyboardHolder, x.KeyboardSince = actor.user, now
	})
	if err != nil {
		s.setSessionStartInFlightLocked(rec.ID, false)
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionCreated, auditDetail("session", rec.ID, "vendor", rec.Vendor, "resumed", "true", "status", sessionStatusPreparing))
	spec := crux.UserSessionSpec{ID: rec.ID, Vendor: rec.Vendor, UIDSlot: rec.UIDSlot, ResumeCommand: cmd,
		WorkspaceRepo: rec.WorkspaceRepo, SpektacularBinary: s.sessionsSpektacularBinary()}
	go s.startSessionInBackground(rt, st, spec, actor.user, sessionStatusEnded)
	jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "session": updated.view(&crux.UserSessionProbe{}, rt)})
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
		case sessionStatusEnded, sessionStatusFailed:
			if ended, err := time.Parse(time.RFC3339, rec.EndedAt); err == nil && now.Sub(ended) > sessionEndedRetention {
				// Files first; a failure keeps the record (and its uid slot)
				// so the next sweep retries instead of orphaning the data.
				if err := rt.Remove(rec.ID); err != nil {
					if s.logger != nil {
						s.logger.Warn("sessions: pruning session files failed", "id", rec.ID, "error", err)
					}
					continue
				}
				_ = st.remove(rec.ID)
			}
		}
	}
}

// sessionForWatch loads a running session the actor may at least watch (the
// read role attaches read-only), writing the 404 or 409 itself.
func (s *Server) sessionForWatch(w http.ResponseWriter, st *sessionStore, id string, actor sessionActor) (sessionRecord, bool) {
	rec, found := st.get(id)
	if !found || !canViewSession(actor, rec) {
		jsonError(w, "session not found", http.StatusNotFound)
		return rec, false
	}
	if rec.Status != sessionStatusRunning {
		jsonError(w, "session is not running", http.StatusConflict)
		return rec, false
	}
	return rec, true
}

// sessionHandoffTarget validates a terminal handoff request for a session id:
// the feature must be on, the caller must be allowed to see the session (a
// viewer attaches read-only; only the keyboard holder's attach is writable,
// decided in guardTerminalSession), the session must be running and the hive
// must hold the ttyd attach key. It writes the error response itself.
func (s *Server) sessionHandoffTarget(w http.ResponseWriter, r *http.Request, id string) bool {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return false
	}
	rec, ok := s.sessionForWatch(w, st, id, s.sessionActorOf(r))
	if !ok {
		return false
	}
	if ttydAttachKey() == "" {
		jsonError(w, "session terminals are unavailable: the ttyd attach key is not configured", http.StatusServiceUnavailable)
		return false
	}
	if p := rt.Probe(rec.ID); !p.Exists {
		jsonError(w, "session is not running", http.StatusConflict)
		return false
	}
	return true
}

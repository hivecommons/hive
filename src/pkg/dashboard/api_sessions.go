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
	"sync/atomic"
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
	// WorkspaceDir is the path of the hive's shared clone of the "org/repo"
	// workspace ("" for an empty or malformed repo). Start clones it on first
	// use.
	WorkspaceDir(repo string) string
	// SessionWorkspaceDir is the path session id opens in for "org/repo": its
	// own git worktree of the shared clone ("" for an empty or malformed id or
	// repo). Start adds it; Remove removes it.
	SessionWorkspaceDir(id, repo string) string
	// SweepWorktrees removes the worktrees of every session for which live
	// reports false (no record any more).
	SweepWorktrees(live func(id string) bool)
	// SecureWorkspaces locks the git config and hooks of every shared clone
	// on disk, so no session can plant a command another session's git runs;
	// called once at boot.
	SecureWorkspaces()
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
	// StartupStep reads the session's visible screen once for the startup
	// auto-answer and, when answer is true and it is a known startup screen
	// nobody has typed into, answers it. It never answers a permission prompt
	// or a login screen (api_sessions_autostart.go).
	StartupStep(id, vendor string, answer bool) crux.UserSessionStartupStep
	// StoreUpload writes src as the named file under the session's uploads
	// directory, as the session's own uid, and returns its absolute path.
	StoreUpload(id, fileName string, src io.Reader, maxBytes int64, maxFiles int) (string, int64, error)
	// StoreInboxFile writes data as <cwd>/.hive/inbox/<fileName>, as the
	// session's own uid with the sessions group and mode 0640, where cwd is
	// workspace ("" = the session's own work dir); it returns the path.
	StoreInboxFile(id, workspace, fileName string, data []byte) (string, error)
	// Remove ends the session and deletes everything it owned on disk. Safe
	// to call again after a failure or on a session that is already gone.
	Remove(id string) error
	// Removed reports whether nothing of the session is left on disk; false
	// when that cannot be told.
	Removed(id string) bool
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
	// session's own worktree of the hive's shared clone it runs in; empty when
	// it runs in its own empty work dir.
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
	// Startup (additive) is the startup auto-answer's state for a running
	// session: getting the CLI ready, started, or the screen it stopped at
	// (api_sessions_autostart.go). Absent when the hive has none for it.
	Startup *sessionStartupView `json:"startup,omitempty"`
	// DeleteError (additive) is why the last delete of the session failed.
	DeleteError string `json:"delete_error"`
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
	s.mux.HandleFunc("POST /api/sessions/{id}/seed", s.handleSessionSeed)
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
			_ = os.Rename(st.path, fmt.Sprintf("%s%s%d", st.path, sessionsStoreCorruptSuffix, time.Now().Unix()))
			st.loadErr = err
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
		Message: st.Message, Error: st.Error, DeleteError: st.DeleteError,
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
		v.Startup = s.sessionStartupViewOf(rec.ID)
	}
	return v
}

// reconcileSessions probes running sessions, marks those whose tmux session
// is gone (pod restart, CLI exit) as ended and returns the probes of the
// survivors. A restart of the hive process alone leaves tmux running, so
// those sessions stay running and reattach.
func (s *Server) reconcileSessions(rt SessionRuntime, st *sessionStore) map[string]crux.UserSessionProbe {
	probes := map[string]crux.UserSessionProbe{}
	now := time.Now().UTC().Format(time.RFC3339)
	recs := st.list()
	if hook := sessionsAfterReconcileSnapshot.Load(); hook != nil {
		(*hook)(s)
	}
	for _, rec := range recs {
		if rec.Status == sessionStatusPreparing {
			// The snapshot is not what decides: a start may have finished
			// since it was taken (failOrphanedPreparingSession).
			s.failOrphanedPreparingSession(st, rec.ID, now)
			continue
		}
		if rec.Status != sessionStatusRunning {
			s.reconcileSessionRemoval(rt, st, rec)
			continue
		}
		p := rt.Probe(rec.ID)
		if !p.Exists || p.Dead {
			cur, claimed := s.claimSessionClose(st, rec.ID)
			if !claimed {
				continue
			}
			// Close and Resume may have replaced the session since the first probe.
			p = rt.Probe(cur.ID)
			if !p.Exists || p.Dead {
				s.markSessionEnded(st, cur.ID, now, sessionResumeCommand(rt, cur))
			}
			s.releaseSessionClose(st, cur.ID)
		}
		if p.Exists && !p.Dead {
			probes[rec.ID] = p
		}
	}
	return probes
}

func sessionResumeCommand(rt SessionRuntime, rec sessionRecord) string {
	if info, ok := rt.ResumeInfo(rec.ID, rec.Vendor, rec.Workspace); ok {
		return info.Command
	}
	return ""
}

// markSessionEnded records the stopped terminal while its close claim is held.
func (s *Server) markSessionEnded(st *sessionStore, id, now, resume string) {
	if _, _, err := st.update(id, func(r *sessionRecord) {
		r.Status = sessionStatusEnded
		r.EndedAt = now
		if resume != "" {
			r.ResumeCommand = resume
		}
	}); err != nil && s.logger != nil {
		s.logger.Warn("sessions: persisting ended state failed", "id", id, "error", err)
	}
	// Nothing queued can be delivered to an ended session.
	s.dropSessionQueue(id)
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
		// SeedItems (additive) sends Inbox items into the new session once it
		// is running (api_sessions_seed.go).
		SeedItems []sessionSeedItemRef `json:"seed_items"`
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
	// Seed items are read and rendered BEFORE anything is created, so an item
	// without text (409) or an unknown one (404) never leaves a session behind.
	// The cheap capacity check runs first so a create the caps refuse never
	// reads the items; it is repeated under the lock below.
	var seed *sessionSeed
	if len(body.SeedItems) > 0 {
		if !requireInboxShareRole(w, r) {
			return
		}
		s.sessionsMu.Lock()
		capOK := s.checkSessionCapacity(w, st, actor.user)
		s.sessionsMu.Unlock()
		if !capOK {
			return
		}
		var ok bool
		if seed, ok = s.prepareSessionSeed(w, r, actor.user, body.SeedItems); !ok {
			return
		}
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
		WorkspaceRepo: repo, Workspace: rt.SessionWorkspaceDir(id, repo), Message: s.sessionPreparingMessage(rt, repo, vendor.Label),
		KeyboardHolder: actor.user, KeyboardSince: now}
	s.setSessionStartInFlightLocked(id, true)
	if err := st.put(rec); err != nil {
		s.setSessionStartInFlightLocked(id, false)
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.auditSession(actor.user, auditSessionCreated, auditDetail("session", id, "vendor", vendor.ID, "status", sessionStatusPreparing))
	resp := map[string]any{"ok": true, "session": rec.view(&crux.UserSessionProbe{}, rt)}
	if seed != nil {
		// Parked under sessionsMu before the start goroutine exists, which
		// takes it under the same lock once the session is running.
		if len(seed.files) > 0 {
			s.setPendingSessionSeedLocked(id, &sessionSeed{user: seed.user, files: seed.files})
		}
		resp["seed"] = map[string]any{"items": len(seed.files) + len(seed.waiting), "files": seed.rels(), "waiting": seed.waitingKeys(), "status": "pending"}
		if len(seed.waiting) > 0 {
			// Transcribe-first items: their intents are queued once this
			// handler has released sessionsMu (a runner may need it), with
			// their own deadline; a refusal is audited as failed.
			name := body.Name
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), sessionSeedReadTimeout)
				defer cancel()
				if err := s.startSessionSeedIntents(ctx, id, name, seed); err != nil && s.logger != nil {
					s.logger.Warn("sessions: transcribe-first seed failed", "id", id, "error", err)
				}
			}()
		}
	}
	spec := crux.UserSessionSpec{ID: id, Vendor: vendor.ID, UIDSlot: rec.UIDSlot, WorkspaceRepo: repo, SpektacularBinary: s.sessionsSpektacularBinary()}
	go s.startSessionInBackground(rt, st, spec, actor.user, sessionStatusFailed)
	jsonStatusResponse(w, http.StatusAccepted, resp)
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
	// First line of the startup auto-answer: a seeded HOME skips the CLI's
	// first-run screens altogether.
	spec.SeedOnboarding = s.deps.Config.SessionsAutostart()
	err := rt.Start(spec)
	now := time.Now().UTC().Format(time.RFC3339)
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.setSessionStartInFlightLocked(spec.ID, false)
	// An Inbox seed parked by the create is delivered only after the record
	// says running (below), outside this lock.
	if seed := s.takePendingSessionSeedLocked(spec.ID); seed != nil {
		startErr := err
		defer func() { go s.deliverPendingSessionSeed(rt, st, spec.ID, seed, startErr) }()
	}
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
	// Take the CLI through its startup screens to the input prompt; the Send
	// queue (and with it a parked Inbox seed) waits for that.
	s.beginSessionAutostart(rt, st, spec.ID, spec.Vendor, user)
}

// sessionsAfterReconcileSnapshot, when set, runs in reconcileSessions right
// after the records are listed and before any is judged. A test seam: it
// places "a start finishes" exactly in the window the list and the judgement
// used to race over. Never set in production.
var sessionsAfterReconcileSnapshot atomic.Pointer[func(*Server)]

// failOrphanedPreparingSession fails a `preparing` record that has no start
// behind it, which only a restart of the hive leaves: nothing will ever move
// it on, so the record says so.
//
// The decision is made under sessionsMu on the record as it is NOW, never on
// the caller's snapshot. reconcileSessions lists the records without that
// lock, and startSessionInBackground clears the in-flight mark and saves
// `running` in one hold of it. A reconcile that listed the record as
// `preparing` and then asked "is a start in flight?" used to get "no" from a
// start that had just finished, and overwrote the running record with "the
// hive restarted while this session was being prepared". Any session list
// that overlapped the end of a start could do it.
func (s *Server) failOrphanedPreparingSession(st *sessionStore, id, now string) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.failOrphanedPreparingSessionLocked(st, id, now)
}

func (s *Server) failOrphanedPreparingSessionLocked(st *sessionStore, id, now string) {
	if s.sessionsPreparing[id] {
		return
	}
	cur, ok := st.get(id)
	if !ok || cur.Status != sessionStatusPreparing {
		return
	}
	status := sessionStatusFailed
	if cur.ResumeCommand != "" {
		status = sessionStatusEnded
	}
	s.markSessionFailed(st, id, status, sessionRestartedWhilePreparing, now)
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
	id := r.PathValue("id")
	if _, ok := s.sessionForUse(w, st, id, actor); !ok {
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
	// The body may have arrived while shutdown was claiming this session.
	// Check and queue the input under the same lock as that claim.
	s.sessionsMu.Lock()
	if _, ok := s.sessionForUse(w, st, id, actor); !ok {
		s.sessionsMu.Unlock()
		return
	}
	if st.closing[id] {
		s.sessionsMu.Unlock()
		jsonError(w, "the session is already closing; try again", http.StatusConflict)
		return
	}
	item, pos, ok := s.enqueueSessionInput(id, sessionQueueItem{User: actor.user, Text: clean, Submit: body.Submit})
	s.sessionsMu.Unlock()
	if !ok {
		jsonError(w, fmt.Sprintf("this session already has %d queued sends; wait for some to be delivered", sessionQueueMaxItems), http.StatusTooManyRequests)
		return
	}
	s.ensureSessionQueueWorker(rt, st, id)
	jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "queued": map[string]any{"id": item.ID, "position": pos},
		"queue": s.sessionQueueSnapshot(id, actor.user)})
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
		cmd = sessionResumeCommand(rt, rec)
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
	// ResumeInfo can take time. Delete and other starts may have won while
	// it ran, so claim only a session that is still ended under their lock.
	rec, found = st.get(rec.ID)
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if rec.Status != sessionStatusEnded || st.closing[rec.ID] || st.deleteInFlight(rec.ID) || s.sessionsPreparing[rec.ID] {
		jsonError(w, "session is not ended", http.StatusConflict)
		return
	}
	if rec.ResumeCommand != "" {
		cmd = rec.ResumeCommand
	}
	if !s.checkSessionCapacity(w, st, actor.user) {
		return
	}
	// A resume reopens the workspace the session was created in (or its own
	// work dir for a record without one), never a repo configured since, in
	// the session's own worktree of it (a record from before per-session
	// worktrees moves into one here). Like a create it is answered as
	// `preparing` and started in the background (the workspace may have to be
	// cloned again, spek init may run); a failure puts the session back to
	// ended with the reason, still resumable.
	vendorLabel := rec.Vendor
	for _, v := range rt.Vendors() {
		if v.ID == rec.Vendor && v.Label != "" {
			vendorLabel = v.Label
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	message := s.sessionPreparingMessage(rt, rec.WorkspaceRepo, vendorLabel)
	// Whoever resumes the session holds its keyboard.
	updated, found, err := st.updatePersisted(rec.ID, func(x *sessionRecord) {
		x.Status, x.LastActivity, x.Message, x.Error = sessionStatusPreparing, now, message, ""
		x.KeyboardHolder, x.KeyboardSince = actor.user, now
		if x.WorkspaceRepo != "" {
			x.Workspace = rt.SessionWorkspaceDir(x.ID, x.WorkspaceRepo)
		}
	})
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "could not save the session", http.StatusInternalServerError)
		return
	}
	s.setSessionStartInFlightLocked(rec.ID, true)
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
		// A shared clone made by an earlier release has a group-writable git
		// config and hooks: lock it now rather than on its next session start.
		if rt, ok := s.sessionsRuntime(); ok {
			rt.SecureWorkspaces()
		}
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
// TTL (no attached client and no activity), prunes old ended metadata and
// removes worktrees whose session record is gone.
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
			if p, alive := probes[rec.ID]; alive && sessionIdle(rec, p, now, ttl) {
				s.closeIdleSession(rt, st, rec.ID, now, ttl)
			}
		case sessionStatusEnded, sessionStatusFailed:
			if ended, err := time.Parse(time.RFC3339, rec.EndedAt); err == nil && now.Sub(ended) > sessionEndedRetention {
				// The same job a person's delete runs: files first, the
				// record (and its uid slot) only once they are gone.
				s.beginSessionDelete(rt, st, rec.ID, sessionDeleteActorSystem, sessionDeleteReasonRetention, "")
			}
		}
	}
	// The worktree sweep removes worktrees whose record is gone, so it runs
	// only on a store that loaded cleanly: an unreadable store starts empty,
	// and sweeping against it would remove every session's worktree.
	if reason := st.untrustedForSweep(); reason != "" {
		if s.logger != nil {
			s.logger.Warn("sessions: worktree sweep skipped; no session worktree is removed until the sessions store is trusted again", "reason", reason)
		}
		return
	}
	rt.SweepWorktrees(func(id string) bool {
		_, found := st.get(id)
		return found
	})
}

func sessionIdle(rec sessionRecord, p crux.UserSessionProbe, now time.Time, ttl time.Duration) bool {
	if !p.Exists || p.Dead || p.Attached > 0 {
		return false
	}
	last, err := time.Parse(time.RFC3339, rec.LastActivity)
	if err != nil {
		return false
	}
	if p.Activity.After(last) {
		last = p.Activity
	}
	return now.Sub(last) >= ttl
}

func (s *Server) closeIdleSession(rt SessionRuntime, st *sessionStore, id string, now time.Time, ttl time.Duration) {
	rec, claimed := s.claimSessionClose(st, id)
	if !claimed {
		return
	}
	defer s.releaseSessionClose(st, id)
	// An idle probe from before the claim may describe a different terminal.
	if !sessionIdle(rec, rt.Probe(id), now, ttl) {
		return
	}
	if err := s.stopSession(rt, st, rec, now.UTC().Format(time.RFC3339)); err != nil {
		if s.logger != nil {
			s.logger.Warn("sessions: idle kill failed", "id", id, "error", err)
		}
		return
	}
	s.auditSession("system", auditSessionClosed, auditDetail("session", id, "reason", "idle"))
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

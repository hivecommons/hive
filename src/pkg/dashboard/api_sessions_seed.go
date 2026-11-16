package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/inbox"
)

// Inbox → Inception (spek) session. POST /api/sessions/{id}/seed (and
// POST /api/sessions with seed_items for a new session) sends Inbox items
// into an interactive session: the hive fetches each item's current content,
// writes it as ONE markdown file per item under <cwd>/.hive/inbox/ in the
// session's workspace (as the session's uid, 0640, sessions group,
// untrusted-data envelope, provenance outside it; SessionRuntime.StoreInboxFile)
// and then queues a fixed Send through the ordinary queued-Send path
// (api_sessions_queue.go): delivered when the CLI's prompt is idle, audited
// as session_input for the sender. The Send text is
// inbox.SessionInstruction over the sanitised file paths; it never contains
// item text. Each seed is audited as inbox_to_session and the items show
// "routed: session/<id>".
const (
	// sessionSeedMaxItems bounds one seed (the dialog's multi-select).
	sessionSeedMaxItems = 20
	// sessionSeedMaxFileBytesEnv overrides the per-item cap on enveloped text.
	sessionSeedMaxFileBytesEnv = "HIVE_SESSIONS_SEED_MAX_BYTES"
	// sessionSeedReadTimeout bounds reading every item of one seed (a direct
	// source read may spawn the source's MCP process).
	sessionSeedReadTimeout = 2 * time.Minute

	auditInboxToSession = "inbox_to_session"
	// sessionSeedReasonNoText is the 409 reason for an item without text.
	sessionSeedReasonNoText = "no_text"
	// sessionSeedOutcomeQueued / Failed are the inbox_to_session outcomes.
	sessionSeedOutcomeQueued = "queued"
	sessionSeedOutcomeFailed = "failed"
)

// sessionSeedItemRef names one Inbox item; item_id is accepted as an alias
// of id (the Inbox's own route body uses item_id).
type sessionSeedItemRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	ItemID string `json:"item_id"`
}

// sessionSeedFile is one rendered seed file.
type sessionSeedFile struct {
	item inbox.Item
	name string
	rel  string
	data []byte
}

// sessionSeed is a prepared seed: who sent it and the files to write.
type sessionSeed struct {
	user  string
	files []sessionSeedFile
}

func (sd *sessionSeed) keys() string {
	keys := make([]string, 0, len(sd.files))
	for _, f := range sd.files {
		keys = append(keys, f.item.Key())
	}
	return strings.Join(keys, " ")
}

func (sd *sessionSeed) rels() []string {
	out := make([]string, 0, len(sd.files))
	for _, f := range sd.files {
		out = append(out, f.rel)
	}
	return out
}

// sessionSeedError is the 409 body of an item that has no text yet.
type sessionSeedError struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Reason     string `json:"reason"`
	Item       string `json:"item"`
	Transcribe bool   `json:"transcribe"`
}

// prepareSessionSeed validates refs and renders one file per item from its
// current content, writing the 4xx/5xx itself. Nothing is written to any
// session here, so a refusal leaves no trace.
func (s *Server) prepareSessionSeed(w http.ResponseWriter, r *http.Request, user string, refs []sessionSeedItemRef) (*sessionSeed, bool) {
	if s.deps == nil || s.deps.Inbox == nil {
		jsonError(w, "the Inbox is not available on this hive", http.StatusNotFound)
		return nil, false
	}
	svc := s.deps.Inbox
	if len(refs) == 0 {
		jsonError(w, "items[] is required", http.StatusBadRequest)
		return nil, false
	}
	if len(refs) > sessionSeedMaxItems {
		jsonError(w, fmt.Sprintf("at most %d items can be sent into a session at once", sessionSeedMaxItems), http.StatusBadRequest)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionSeedReadTimeout)
	defer cancel()
	items, listErr := svc.Items(ctx)
	byKey := make(map[string]inbox.Item, len(items))
	for _, it := range items {
		byKey[it.Key()] = it
	}
	publicURL := ""
	if s.deps.Config != nil {
		publicURL = s.deps.Config.Dashboard.PublicURL
	}
	maxBytes := int(sessionEnvInt64(sessionSeedMaxFileBytesEnv, inbox.DefaultSessionFileMaxBytes))
	seed := &sessionSeed{user: user}
	seen := map[string]bool{}
	now := time.Now().UTC()
	for _, ref := range refs {
		source, id := strings.TrimSpace(ref.Source), strings.TrimSpace(ref.ID)
		if id == "" {
			id = strings.TrimSpace(ref.ItemID)
		}
		if source == "" || id == "" {
			jsonError(w, "every item needs a source and an id", http.StatusBadRequest)
			return nil, false
		}
		key := inbox.Key(source, id)
		if seen[key] {
			continue
		}
		seen[key] = true
		it, ok := byKey[key]
		if !ok {
			msg := "unknown inbox item " + key
			if listErr != nil {
				msg += ": " + listErr.Error()
			}
			jsonError(w, msg, http.StatusNotFound)
			return nil, false
		}
		c, err := svc.Content(ctx, it)
		if err != nil {
			jsonError(w, "reading "+key+": "+err.Error(), http.StatusBadGateway)
			return nil, false
		}
		if err := inbox.CheckSessionText(it, c); err != nil {
			var se *inbox.SessionSeedError
			if errors.As(err, &se) {
				jsonStatusResponse(w, http.StatusConflict, sessionSeedError{Error: se.Error(), Reason: sessionSeedReasonNoText, Item: se.Key, Transcribe: se.Transcribe})
				return nil, false
			}
			jsonError(w, err.Error(), http.StatusConflict)
			return nil, false
		}
		data := inbox.RenderSessionFile(it, c, inbox.SessionFileOptions{
			InboxURL: inbox.ItemDeepLink(publicURL, it.Source, it.ID),
			SentBy:   user, SentAt: now, MaxBytes: maxBytes,
		})
		seed.files = append(seed.files, sessionSeedFile{item: it, name: inbox.SessionFileName(it.Source, it.ID), rel: inbox.SessionFilePath(it.Source, it.ID), data: data})
	}
	return seed, true
}

// sessionSeedResult is what one delivered seed wrote and queued.
type sessionSeedResult struct {
	Files       []string `json:"files"`
	Instruction string   `json:"instruction"`
	QueueID     string   `json:"queue_id"`
	Position    int      `json:"position"`
}

// errSessionSeedQueueFull is returned when the session's Send queue is full.
var errSessionSeedQueueFull = errors.New("this session's Send queue is full; wait for some sends to be delivered")

// errSessionSeedNotRunning is returned when the session stopped before the
// seed could be written.
var errSessionSeedNotRunning = errors.New("session is not running")

// deliverSessionSeed writes the seed's files into a RUNNING session and
// queues the fixed instruction as the sender, then marks the items routed and
// audits inbox_to_session (with the outcome on failure too).
func (s *Server) deliverSessionSeed(rt SessionRuntime, st *sessionStore, id string, seed *sessionSeed) (sessionSeedResult, error) {
	res, err := s.deliverSessionSeedFiles(rt, st, id, seed)
	outcome := sessionSeedOutcomeQueued
	detail := []string{"session", id, "items", seed.keys(), "files", fmt.Sprint(len(seed.files))}
	if err != nil {
		outcome = sessionSeedOutcomeFailed
		detail = append(detail, "error", redactTokens(err.Error()))
	}
	s.auditSession(seed.user, auditInboxToSession, auditDetail(append(detail, "outcome", outcome)...))
	return res, err
}

func (s *Server) deliverSessionSeedFiles(rt SessionRuntime, st *sessionStore, id string, seed *sessionSeed) (sessionSeedResult, error) {
	rec, found := st.get(id)
	if !found || rec.Status != sessionStatusRunning {
		return sessionSeedResult{}, errSessionSeedNotRunning
	}
	for _, f := range seed.files {
		if _, err := rt.StoreInboxFile(rec.ID, rec.Workspace, f.name, f.data); err != nil {
			return sessionSeedResult{}, fmt.Errorf("writing %s into the session: %w", f.rel, err)
		}
	}
	text, err := crux.SanitizeUserSessionInput(inbox.SessionInstruction(seed.rels()))
	if err != nil {
		return sessionSeedResult{}, err
	}
	item, pos, ok := s.enqueueSessionInput(rec.ID, sessionQueueItem{User: seed.user, Text: text, Submit: true})
	if !ok {
		return sessionSeedResult{}, errSessionSeedQueueFull
	}
	s.ensureSessionQueueWorker(rt, st, rec.ID)
	s.touchSession(st, rec.ID)
	if svc := s.deps.Inbox; svc != nil {
		for _, f := range seed.files {
			if err := svc.MarkSession(f.item, rec.ID, rec.Name); err != nil && s.logger != nil {
				s.logger.Warn("sessions: marking inbox item routed failed", "item", f.item.Key(), "session", rec.ID, "error", err)
			}
		}
	}
	return sessionSeedResult{Files: seed.rels(), Instruction: text, QueueID: item.ID, Position: pos}, nil
}

// handleSessionSeed is POST /api/sessions/{id}/seed: send Inbox items into a
// running session the caller may type into (owner, or read-write on their
// own or a shared session; the read role only watches).
func (s *Server) handleSessionSeed(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "sending Inbox items into a session requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, ok := s.sessionForUse(w, st, r.PathValue("id"), actor)
	if !ok {
		return
	}
	var body struct {
		Items []sessionSeedItemRef `json:"items"`
	}
	if !readSmallJSON(w, r, &body) {
		return
	}
	seed, ok := s.prepareSessionSeed(w, r, actor.user, body.Items)
	if !ok {
		return
	}
	res, err := s.deliverSessionSeed(rt, st, rec.ID, seed)
	switch {
	case errors.Is(err, errSessionSeedQueueFull):
		jsonError(w, err.Error(), http.StatusTooManyRequests)
		return
	case errors.Is(err, crux.ErrUserSessionInboxSymlink), errors.Is(err, errSessionSeedNotRunning):
		jsonError(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		if s.logger != nil {
			s.logger.Warn("sessions: seeding from the Inbox failed", "id", rec.ID, "error", err)
		}
		jsonError(w, "could not send the items into the session: "+redactTokens(err.Error()), http.StatusInternalServerError)
		return
	}
	p := rt.Probe(rec.ID)
	cur, _ := st.get(rec.ID)
	jsonStatusResponse(w, http.StatusAccepted, map[string]any{"ok": true, "session": s.sessionViewFor(cur, &p, rt, actor), "seed": res})
}

// setPendingSessionSeedLocked parks a seed for a session being created; the
// background start delivers it once the session is running. The caller holds
// sessionsMu.
func (s *Server) setPendingSessionSeedLocked(id string, seed *sessionSeed) {
	if s.sessionSeeds == nil {
		s.sessionSeeds = map[string]*sessionSeed{}
	}
	s.sessionSeeds[id] = seed
}

// takePendingSessionSeedLocked removes and returns the parked seed of id.
// The caller holds sessionsMu.
func (s *Server) takePendingSessionSeedLocked(id string) *sessionSeed {
	seed := s.sessionSeeds[id]
	delete(s.sessionSeeds, id)
	return seed
}

// deliverPendingSessionSeed runs a parked seed after the start finished:
// delivered when the session is running, audited as failed otherwise.
func (s *Server) deliverPendingSessionSeed(rt SessionRuntime, st *sessionStore, id string, seed *sessionSeed, startErr error) {
	if seed == nil {
		return
	}
	if startErr != nil {
		s.auditSession(seed.user, auditInboxToSession, auditDetail("session", id, "items", seed.keys(), "files", "0", "error", "the session did not start", "outcome", sessionSeedOutcomeFailed))
		return
	}
	if _, err := s.deliverSessionSeed(rt, st, id, seed); err != nil && s.logger != nil {
		s.logger.Warn("sessions: seeding a new session from the Inbox failed", "id", id, "error", err)
	}
}

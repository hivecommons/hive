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
	// sessionSeedOutcomeWaiting: an audio-only item is being transcribed and
	// seeds the session when its transcript is ready.
	sessionSeedOutcomeWaiting = "waiting_transcript"
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
	// shareID is the share-history record of a transcribe-first share
	// (ShareIntent.RecordID); empty for items that had text already.
	shareID string
}

// sessionSeed is a prepared seed: who sent it, the files to write and the
// audio-only items that are transcribed first (each seeds the session when
// its transcript is ready, through the "spek" share intent).
type sessionSeed struct {
	user    string
	files   []sessionSeedFile
	waiting []inbox.Item
}

func (sd *sessionSeed) waitingKeys() []string {
	out := make([]string, 0, len(sd.waiting))
	for _, it := range sd.waiting {
		out = append(out, it.Key())
	}
	return out
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
			// Audio-only and transcribable now (or already transcribing):
			// transcribe first, then seed (the "spek" intent runner).
			if sessionSeedCanWait(it) {
				seed.waiting = append(seed.waiting, it)
				continue
			}
			var se *inbox.SessionSeedError
			if errors.As(err, &se) {
				jsonStatusResponse(w, http.StatusConflict, sessionSeedError{Error: se.Error(), Reason: sessionSeedReasonNoText, Item: se.Key, Transcribe: se.Transcribe})
				return nil, false
			}
			jsonError(w, err.Error(), http.StatusConflict)
			return nil, false
		}
		seed.files = append(seed.files, s.renderSessionSeedFile(it, c, user, now))
	}
	return seed, true
}

// sessionSeedCanWait reports whether an item without text will get one from
// a machine transcription: it is transcribable now, or a run is in flight.
func sessionSeedCanWait(it inbox.Item) bool {
	if it.Transcribable {
		return true
	}
	st := it.Transcription
	return st != nil && (st.Status == inbox.TranscriptionQueued || st.Status == inbox.TranscriptionRunning)
}

// renderSessionSeedFile renders one item's seed file from its content.
func (s *Server) renderSessionSeedFile(it inbox.Item, c inbox.Content, user string, now time.Time) sessionSeedFile {
	publicURL := ""
	if s.deps != nil && s.deps.Config != nil {
		publicURL = s.deps.Config.Dashboard.PublicURL
	}
	data := inbox.RenderSessionFile(it, c, inbox.SessionFileOptions{
		InboxURL: inbox.ItemDeepLink(publicURL, it.Source, it.ID),
		SentBy:   user, SentAt: now,
		MaxBytes: int(sessionEnvInt64(sessionSeedMaxFileBytesEnv, inbox.DefaultSessionFileMaxBytes)),
	})
	return sessionSeedFile{item: it, name: inbox.SessionFileName(it.Source, it.ID), rel: inbox.SessionFilePath(it.Source, it.ID), data: data}
}

// sessionSeedResult is what one delivered seed wrote and queued.
type sessionSeedResult struct {
	Files       []string `json:"files"`
	Instruction string   `json:"instruction"`
	QueueID     string   `json:"queue_id"`
	Position    int      `json:"position"`
	// Waiting are the audio-only items transcribed first; each seeds the
	// session (its own file and Send) when its transcript is ready.
	Waiting []string `json:"waiting"`
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
			s.recordSpekShare(svc, f, seed.user, rec.ID, rec.Name)
		}
	}
	return sessionSeedResult{Files: seed.rels(), Instruction: text, QueueID: item.ID, Position: pos}, nil
}

// recordSpekShare records the share of one seeded item in its history: a
// finished record carrying the session id and name as its target. A
// transcribe-first share continues the record made when it was queued.
func (s *Server) recordSpekShare(svc *inbox.Service, f sessionSeedFile, actor, sessionID, sessionName string) {
	ref := &inbox.ShareRef{ID: sessionID, Name: sessionName}
	var err error
	if f.shareID != "" {
		err = svc.FinishShare(f.item.Key(), f.shareID, inbox.ShareDone, "", ref)
	} else {
		now := time.Now().UTC()
		_, err = svc.RecordShare(f.item.Key(), inbox.ShareRecord{Destination: inbox.ShareDestSpek, Actor: actor, Status: inbox.ShareDone, FinishedAt: &now, Ref: ref})
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("sessions: recording the inbox share failed", "item", f.item.Key(), "session", sessionID, "error", err)
	}
}

// failSpekShares marks the transcribe-first records of a parked seed failed
// when the session never started.
func (s *Server) failSpekShares(seed *sessionSeed, reason string) {
	svc := s.deps.Inbox
	if svc == nil {
		return
	}
	for _, f := range seed.files {
		if f.shareID != "" {
			_ = svc.FinishShare(f.item.Key(), f.shareID, inbox.ShareFailed, reason, nil)
		}
	}
}

// handleSessionSeed is POST /api/sessions/{id}/seed: send Inbox items into a
// running session the caller may type into (owner, or read-write on their
// own or a shared session; the read role only watches).
func (s *Server) handleSessionSeed(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	// The Inbox share rule (owner verified, or read-write), the same as every
	// other Share → Hive destination.
	if !requireInboxShareRole(w, r) {
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
	// Audio-only items first: their transcriptions are queued (or, if one
	// already has text by now, they are seeded at once), so a refusal there
	// leaves the session untouched.
	if err := s.startSessionSeedIntents(r.Context(), rec.ID, rec.Name, seed); err != nil {
		s.writeSessionSeedIntentError(w, err)
		return
	}
	res := sessionSeedResult{Files: []string{}}
	var err error
	if len(seed.files) > 0 {
		res, err = s.deliverSessionSeed(rt, st, rec.ID, seed)
	}
	res.Waiting = seed.waitingKeys()
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

// inboxSpekIntentDest is the Inbox share destination the seed registers as an
// intent runner: an audio-only item is transcribed first, then seeds the
// session. sessionSeedParamSession / ...Name are its ShareIntent.Params.
const (
	inboxSpekIntentDest     = "spek"
	sessionSeedParamSession = "session"
	sessionSeedParamName    = "session_name"
)

// registerInboxSpekRunner hooks the seed into the Inbox's transcribe-first
// path. Only the seed endpoints create "spek" intents (POST /api/inbox/route
// refuses the destination), after their role and session checks, so the
// session id in the params is one the actor was allowed to use.
func (s *Server) registerInboxSpekRunner() {
	if s.deps != nil && s.deps.Inbox != nil {
		s.deps.Inbox.RegisterIntentRunner(inboxSpekIntentDest, s.runSpekIntent)
	}
}

// startSessionSeedIntents queues a "spek" share intent per audio-only item of
// the seed, for session id.
func (s *Server) startSessionSeedIntents(ctx context.Context, id, name string, seed *sessionSeed) error {
	if len(seed.waiting) == 0 {
		return nil
	}
	svc := s.deps.Inbox
	for _, it := range seed.waiting {
		in := inbox.ShareIntent{Destination: inboxSpekIntentDest, Actor: seed.user, Params: map[string]string{sessionSeedParamSession: id, sessionSeedParamName: name}}
		if _, err := svc.ShareWhenReady(ctx, it, in, s.inboxTranscribeOptions(seed.user)); err != nil {
			s.auditSession(seed.user, auditInboxToSession, auditDetail("session", id, "items", it.Key(), "transcribe_first", "true", "error", redactTokens(err.Error()), "outcome", sessionSeedOutcomeFailed))
			return err
		}
		s.auditSession(seed.user, auditInboxToSession, auditDetail("session", id, "items", it.Key(), "transcribe_first", "true", "outcome", sessionSeedOutcomeWaiting))
	}
	return nil
}

// writeSessionSeedIntentError maps a transcribe-first refusal to a response.
func (s *Server) writeSessionSeedIntentError(w http.ResponseWriter, err error) {
	var se *inbox.SessionSeedError
	switch {
	case errors.As(err, &se):
		jsonStatusResponse(w, http.StatusConflict, sessionSeedError{Error: se.Error(), Reason: sessionSeedReasonNoText, Item: se.Key, Transcribe: se.Transcribe})
	case errors.Is(err, inbox.ErrTranscribeUnavailable):
		jsonError(w, "this item has audio but no text yet, and "+err.Error(), http.StatusPreconditionFailed)
	case errors.Is(err, errSessionSeedNotRunning):
		jsonError(w, err.Error(), http.StatusConflict)
	default:
		jsonError(w, "could not transcribe the item first: "+redactTokens(err.Error()), http.StatusInternalServerError)
	}
}

// runSpekIntent is the "spek" intent runner: the item now has text, so its
// seed file is rendered and written into the session and the instruction
// queued (or, while the session is still being prepared, parked for the
// start like a create's seed).
func (s *Server) runSpekIntent(ctx context.Context, it inbox.Item, in inbox.ShareIntent) error {
	rt, ok := s.sessionsRuntime()
	if !ok {
		return errors.New("sessions are not enabled on this hive")
	}
	st := s.sessionStoreOrNil()
	id := strings.TrimSpace(in.Params[sessionSeedParamSession])
	if _, found := st.get(id); id == "" || !found {
		return errors.New("the session this item was sent to is gone")
	}
	c, err := s.deps.Inbox.Content(ctx, it)
	if err != nil {
		return fmt.Errorf("reading %s: %w", it.Key(), err)
	}
	if err := inbox.CheckSessionText(it, c); err != nil {
		return err
	}
	file := s.renderSessionSeedFile(it, c, in.Actor, time.Now().UTC())
	file.shareID = in.RecordID
	seed := &sessionSeed{user: in.Actor, files: []sessionSeedFile{file}}
	s.sessionsMu.Lock()
	if rec, found := st.get(id); found && rec.Status == sessionStatusPreparing && s.sessionsPreparing[id] {
		if prev := s.sessionSeeds[id]; prev != nil {
			prev.files = append(prev.files, seed.files...)
		} else {
			s.setPendingSessionSeedLocked(id, seed)
		}
		s.sessionsMu.Unlock()
		return nil
	}
	s.sessionsMu.Unlock()
	_, err = s.deliverSessionSeed(rt, st, id, seed)
	return err
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
		s.failSpekShares(seed, "the session did not start")
		return
	}
	if _, err := s.deliverSessionSeed(rt, st, id, seed); err != nil {
		s.failSpekShares(seed, err.Error())
		if s.logger != nil {
			s.logger.Warn("sessions: seeding a new session from the Inbox failed", "id", id, "error", err)
		}
	}
}

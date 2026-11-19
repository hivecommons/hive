package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Queued Send. POST /api/sessions/{id}/input no longer pastes at once: with
// several people on one session two Sends would interleave in the prompt, or
// land while the CLI is still running and be read as an answer to its
// permission prompt. Each session has one bounded FIFO and one worker that
// delivers the head only when the pane is idle (no run in progress, empty
// prompt line; sessionPaneIdle), waits for idle again, and so on. Idle
// detection is a heuristic over the pane's last lines, so it fails OPEN: an
// item that has waited sessionQueueDeliverAfter is delivered anyway, and a
// message is never stuck forever. The one exception is a session whose CLI is
// known not to be at its input prompt yet (startup, login or permission
// screen, api_sessions_autostart.go): the queue waits for that first. The
// delivery is audited as session_input with the user who queued it, at
// delivery time.
const (
	// sessionQueueMaxItems bounds one session's queue; a full queue answers 429.
	sessionQueueMaxItems = 32
	// sessionQueueDeliverAfterEnv overrides, in seconds, how long an item waits
	// for an idle pane before it is delivered regardless.
	sessionQueueDeliverAfterEnv     = "HIVE_SESSIONS_SEND_WAIT_SEC"
	defaultSessionQueueDeliverAfter = 45 * time.Second
	// sessionIdleCaptureLines is how many trailing pane lines the idle check
	// reads; the prompt box and the status line beneath it fit in far fewer.
	sessionIdleCaptureLines = 12
	// sessionIdlePromptTail is how many trailing non-decoration lines the empty
	// prompt may sit among (the CLIs draw a shortcuts line under the prompt).
	sessionIdlePromptTail = 4
)

// The worker's pacing (overridable per Server by tests, see sessionQueuePacing).
const (
	// defaultSessionQueuePollInterval is how often the worker re-reads the
	// pane while waiting for it to become idle.
	defaultSessionQueuePollInterval = time.Second
	// defaultSessionQueuePostDeliverDelay is the pause after a delivery before
	// the next idle check, so a just-submitted prompt is seen as running
	// rather than as the still-empty prompt it replaced a moment ago.
	defaultSessionQueuePostDeliverDelay = 1500 * time.Millisecond
)

// sessionQueuePacing is the worker's poll interval and post-delivery pause;
// zero values mean the defaults. Set on the Server before any worker starts.
// now, when set, replaces the wall clock the fail-open deadline is measured
// against, and deliverAfter, when positive, replaces the env/default deadline
// (both are test seams, so a test advances a fake clock instead of sleeping).
type sessionQueuePacing struct {
	poll, postDeliver time.Duration
	deliverAfter      time.Duration
	now               func() time.Time
}

func (s *Server) sessionQueueNow() time.Time {
	if s.sessionQueueTiming.now != nil {
		return s.sessionQueueTiming.now()
	}
	return time.Now()
}

func (s *Server) sessionQueueDeadline() time.Duration {
	if s.sessionQueueTiming.deliverAfter > 0 {
		return s.sessionQueueTiming.deliverAfter
	}
	return sessionQueueDeliverAfter()
}

func (s *Server) sessionQueuePoll() time.Duration {
	if s.sessionQueueTiming.poll > 0 {
		return s.sessionQueueTiming.poll
	}
	return defaultSessionQueuePollInterval
}

func (s *Server) sessionQueuePostDeliver() time.Duration {
	if s.sessionQueueTiming.postDeliver > 0 {
		return s.sessionQueueTiming.postDeliver
	}
	return defaultSessionQueuePostDeliverDelay
}

// sessionQueueDeliverAfter resolves the fail-open deadline: env over default.
func sessionQueueDeliverAfter() time.Duration {
	return time.Duration(sessionEnvInt64(sessionQueueDeliverAfterEnv, int64(defaultSessionQueueDeliverAfter/time.Second))) * time.Second
}

// sessionIdleHeuristic says how one vendor's pane looks when it is waiting for
// input: a line that is exactly one of promptPrefixes (ignoring surrounding
// box-drawing) with nothing typed after it, and none of busyMarkers anywhere in
// the window. Vendors not in the table use the default row.
type sessionIdleHeuristic struct {
	vendors        []string
	promptPrefixes []string
	busyMarkers    []string
}

// sessionSpinnerGlyphs are the braille spinner frames the CLIs animate while a
// run is in progress; any of them in the window means busy.
const sessionSpinnerGlyphs = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⠂⠐"

// sessionIdleHeuristics is the per-vendor table (documented in
// docs/api-reference/sessions.md, "Queued Send"). The last row is the default.
var sessionIdleHeuristics = []sessionIdleHeuristic{
	{vendors: []string{"claude"}, promptPrefixes: []string{"›", ">"}, busyMarkers: []string{"esc to interrupt", "to interrupt"}},
	{vendors: []string{"copilot"}, promptPrefixes: []string{">", "›"}, busyMarkers: []string{"esc to interrupt", "to interrupt", "to cancel"}},
	{vendors: []string{"codex"}, promptPrefixes: []string{"›", ">"}, busyMarkers: []string{"esc to interrupt", "to interrupt", "working"}},
	{vendors: nil, promptPrefixes: []string{"›", ">", "❯"}, busyMarkers: []string{"to interrupt"}},
}

func sessionIdleHeuristicFor(vendor string) sessionIdleHeuristic {
	for _, h := range sessionIdleHeuristics {
		for _, v := range h.vendors {
			if v == vendor {
				return h
			}
		}
	}
	return sessionIdleHeuristics[len(sessionIdleHeuristics)-1]
}

// sessionPaneIdle reports whether a capture of the pane's last lines shows the
// vendor CLI waiting at an empty prompt. An empty capture (nothing drawn yet)
// is not idle.
func sessionPaneIdle(vendor, capture string) bool {
	h := sessionIdleHeuristicFor(vendor)
	var lines []string
	for _, raw := range strings.Split(capture, "\n") {
		line := strings.TrimFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || sessionIsBoxDrawing(r) })
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.ContainsAny(line, sessionSpinnerGlyphs) {
			return false
		}
		for _, m := range h.busyMarkers {
			if strings.Contains(lower, m) {
				return false
			}
		}
	}
	tail := lines
	if len(tail) > sessionIdlePromptTail {
		tail = tail[len(tail)-sessionIdlePromptTail:]
	}
	for _, line := range tail {
		for _, p := range h.promptPrefixes {
			if line == p {
				return true
			}
		}
	}
	return false
}

// sessionIsBoxDrawing reports the box-drawing and block runes the CLIs frame
// their prompt with (U+2500..U+259F).
func sessionIsBoxDrawing(r rune) bool { return r >= 0x2500 && r <= 0x259F }

// sessionQueueItem is one queued Send.
type sessionQueueItem struct {
	ID         string
	User       string
	Text       string
	Submit     bool
	QueuedAt   time.Time
	Delivering bool
}

// sessionQueueItemView is the item as listed to the browser: never the text.
type sessionQueueItemView struct {
	ID         string `json:"id"`
	User       string `json:"user"`
	Bytes      int    `json:"bytes"`
	Submit     bool   `json:"submit"`
	QueuedAt   string `json:"queued_at"`
	Delivering bool   `json:"delivering"`
	// Mine is true for the caller's own items (the ones they may cancel).
	Mine bool `json:"mine"`
}

// sessionQueueView is the `queue` block of a session in GET /api/sessions.
type sessionQueueView struct {
	Length int    `json:"length"`
	Head   string `json:"head"`
	// Position is the caller's first item's 1-based place, 0 when none.
	Position int                    `json:"position"`
	Items    []sessionQueueItemView `json:"items"`
}

// sessionQueue is one session's FIFO. running is true while a worker drains
// it. Guarded by the server's sessionQueuesMu.
type sessionQueue struct {
	items   []*sessionQueueItem
	running bool
	seq     uint64
}

func (s *Server) sessionQueueFor(id string, create bool) *sessionQueue {
	if s.sessionQueues == nil {
		s.sessionQueues = map[string]*sessionQueue{}
	}
	q := s.sessionQueues[id]
	if q == nil && create {
		q = &sessionQueue{}
		s.sessionQueues[id] = q
	}
	return q
}

// enqueueSessionInput appends an item and returns its id and 1-based position,
// or ok=false when the queue is full. The caller starts the worker.
func (s *Server) enqueueSessionInput(id string, item sessionQueueItem) (sessionQueueItem, int, bool) {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	q := s.sessionQueueFor(id, true)
	if len(q.items) >= sessionQueueMaxItems {
		return item, 0, false
	}
	q.seq++
	item.ID = strconv.FormatUint(q.seq, 10)
	item.QueuedAt = time.Now()
	q.items = append(q.items, &item)
	return item, len(q.items), true
}

// cancelSessionInput removes a queued item. 404 when unknown, 409 when it is
// already being delivered, 403 when it is someone else's and the caller may
// not manage the session.
func (s *Server) cancelSessionInput(id, itemID string, actor sessionActor, manage bool) int {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	q := s.sessionQueueFor(id, false)
	if q == nil {
		return http.StatusNotFound
	}
	for i, it := range q.items {
		if it.ID != itemID {
			continue
		}
		if it.User != actor.user && !manage {
			return http.StatusForbidden
		}
		if it.Delivering {
			return http.StatusConflict
		}
		q.items = append(q.items[:i], q.items[i+1:]...)
		return http.StatusOK
	}
	return http.StatusNotFound
}

// dropSessionQueue discards every queued item (the session ended or closed).
func (s *Server) dropSessionQueue(id string) {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	if q := s.sessionQueueFor(id, false); q != nil {
		q.items = nil
	}
}

// sessionQueueSnapshot renders the queue for one caller.
func (s *Server) sessionQueueSnapshot(id, user string) sessionQueueView {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	v := sessionQueueView{Items: []sessionQueueItemView{}}
	q := s.sessionQueueFor(id, false)
	if q == nil {
		return v
	}
	v.Length = len(q.items)
	for i, it := range q.items {
		mine := user != "" && it.User == user
		if mine && v.Position == 0 {
			v.Position = i + 1
		}
		if i == 0 {
			v.Head = it.User
		}
		v.Items = append(v.Items, sessionQueueItemView{ID: it.ID, User: it.User, Bytes: len(it.Text), Submit: it.Submit,
			QueuedAt: it.QueuedAt.UTC().Format(time.RFC3339), Delivering: it.Delivering, Mine: mine})
	}
	return v
}

// ensureSessionQueueWorker starts the session's worker unless one is running.
func (s *Server) ensureSessionQueueWorker(rt SessionRuntime, st *sessionStore, id string) {
	s.sessionQueuesMu.Lock()
	q := s.sessionQueueFor(id, true)
	start := !q.running && len(q.items) > 0
	if start {
		q.running = true
	}
	s.sessionQueuesMu.Unlock()
	if start {
		go s.runSessionQueue(rt, st, id)
	}
}

// takeSessionQueueHead marks the head as being delivered and returns a copy;
// ok=false (and the worker stops) when the queue is empty.
func (s *Server) takeSessionQueueHead(id string) (sessionQueueItem, bool) {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	q := s.sessionQueueFor(id, true)
	if len(q.items) == 0 {
		q.running = false
		return sessionQueueItem{}, false
	}
	q.items[0].Delivering = true
	return *q.items[0], true
}

// popSessionQueueHead removes the delivered (or dropped) head.
func (s *Server) popSessionQueueHead(id, itemID string) {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	q := s.sessionQueueFor(id, false)
	if q != nil && len(q.items) > 0 && q.items[0].ID == itemID {
		q.items = q.items[1:]
	}
}

// runSessionQueue is the per-session worker: deliver the head when the pane
// is idle (or the fail-open deadline passes), then the next, until empty.
func (s *Server) runSessionQueue(rt SessionRuntime, st *sessionStore, id string) {
	for {
		item, ok := s.takeSessionQueueHead(id)
		if !ok {
			return
		}
		rec, found := st.get(id)
		if !found || rec.Status != sessionStatusRunning {
			// The session is gone: nothing can be delivered any more.
			s.dropSessionQueue(id)
			s.popSessionQueueHead(id, item.ID)
			continue
		}
		waited := s.waitSessionIdle(rt, rec.ID, rec.Vendor, item.ID)
		// The session may have ended, or the queue been dropped, while waiting.
		if rec, found = st.get(id); !found || rec.Status != sessionStatusRunning || !s.sessionQueueHeadIs(id, item.ID) {
			s.popSessionQueueHead(id, item.ID)
			continue
		}
		if err := rt.SendText(rec.ID, item.Text, item.Submit); err != nil {
			if s.logger != nil {
				s.logger.Warn("sessions: delivering queued input failed", "id", rec.ID, "user", item.User, "error", err)
			}
			s.auditSession(item.User, auditSessionInput, auditDetail("session", rec.ID, "bytes", strconv.Itoa(len(item.Text)), "submit", strconv.FormatBool(item.Submit), "outcome", "failed"))
		} else {
			s.touchSession(st, rec.ID)
			// Who typed, how much, into which session and how long it queued;
			// never the text itself.
			s.auditSession(item.User, auditSessionInput, auditDetail("session", rec.ID, "bytes", strconv.Itoa(len(item.Text)), "submit", strconv.FormatBool(item.Submit), "queued_sec", strconv.Itoa(int(waited/time.Second))))
		}
		s.popSessionQueueHead(id, item.ID)
		time.Sleep(s.sessionQueuePostDeliver())
	}
}

// sessionQueueHeadIs reports whether itemID is still the queue's head (it is
// not once the queue was dropped).
func (s *Server) sessionQueueHeadIs(id, itemID string) bool {
	s.sessionQueuesMu.Lock()
	defer s.sessionQueuesMu.Unlock()
	q := s.sessionQueueFor(id, false)
	return q != nil && len(q.items) > 0 && q.items[0].ID == itemID
}

// waitSessionIdle blocks until the pane looks idle, the fail-open deadline
// passes or the item is gone from the queue, and returns how long it waited.
func (s *Server) waitSessionIdle(rt SessionRuntime, id, vendor, itemID string) time.Duration {
	start := s.sessionQueueNow()
	// The fail-open deadline only starts once the startup auto-answer lets go:
	// while the CLI is on a startup, login or permission screen a delivery
	// would be typed into that screen (api_sessions_autostart.go).
	s.waitSessionStartup(id, itemID)
	deadline := s.sessionQueueNow().Add(s.sessionQueueDeadline())
	for {
		if text, err := rt.Capture(id, sessionIdleCaptureLines); err == nil && sessionPaneIdle(vendor, text) {
			return s.sessionQueueNow().Sub(start)
		}
		if !s.sessionQueueNow().Before(deadline) || !s.sessionQueueHeadIs(id, itemID) {
			return s.sessionQueueNow().Sub(start)
		}
		time.Sleep(s.sessionQueuePoll())
	}
}

// handleSessionInputCancel is DELETE /api/sessions/{id}/input/{queue_id}:
// the caller's own queued item, or any item for the creator or an owner.
func (s *Server) handleSessionInputCancel(w http.ResponseWriter, r *http.Request) {
	_, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "cancelling input requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, found := st.get(r.PathValue("id"))
	if !found || !canViewSession(actor, rec) {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if !canUseSession(actor, rec) {
		jsonError(w, "only the session's creator or an owner may use a private session", http.StatusForbidden)
		return
	}
	switch code := s.cancelSessionInput(rec.ID, r.PathValue("queue_id"), actor, canManageSession(actor, rec)); code {
	case http.StatusOK:
		jsonResponse(w, map[string]any{"ok": true, "queue": s.sessionQueueSnapshot(rec.ID, actor.user)})
	case http.StatusNotFound:
		jsonError(w, "no such queued input", code)
	case http.StatusConflict:
		jsonError(w, "that input is being delivered right now", code)
	default:
		jsonError(w, "only your own queued input can be cancelled", code)
	}
}

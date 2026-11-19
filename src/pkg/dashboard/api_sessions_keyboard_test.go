package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// fastSessionQueue shortens this server's worker pacing. It is set before any
// worker starts, so no global is touched while a worker may still be running.
func fastSessionQueue(t *testing.T, e *sessionTestEnv) {
	t.Helper()
	e.s.sessionQueueTiming = sessionQueuePacing{poll: 5 * time.Millisecond, postDeliver: 5 * time.Millisecond}
}

// claudeIdlePane is what a Claude Code pane looks like waiting at its prompt.
const claudeIdlePane = "✻ Welcome back\n\n──────────────\n› \n──────────────\n  ? for shortcuts\n"

// claudeBusyPane is a run in progress.
const claudeBusyPane = "⠹ Thinking… (esc to interrupt)\n\n──────────────\n› \n──────────────\n"

func sessionOf(t *testing.T, e *sessionTestEnv, c sessionCaller, id string) map[string]any {
	t.Helper()
	for _, v := range decodeSessionJSON(t, e.do(t, c, "GET", "/api/sessions", nil))["sessions"].([]any) {
		m := v.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("session %s not listed for %s", id, c.user)
	return nil
}

// ---- role matrix

func TestSessionsRoleMatrixReadWatchesOnly(t *testing.T) {
	e := handoffEnv(t)
	fastSessionQueue(t, e)
	id := e.createShared(t, callerRW, "jam")
	name := sessionTmuxPrefix + id
	type row struct {
		who                                  sessionCaller
		attach, input, upload, keyboard, log int
	}
	// The creator is attached and active, so only owners (and the creator)
	// may take the keyboard; another writer waits (409).
	attach := e.s.beginSessionAttach(id, callerRW.user, true)
	defer e.s.endSessionAttach(attach)
	rows := []row{
		{callerOwner, 200, 202, 200, 200, 200},
		{callerRW, 200, 202, 200, 200, 200},  // creator
		{callerRW2, 200, 202, 200, 409, 200}, // another writer: may Send, waits for the keyboard
		{callerRead, 200, 403, 403, 403, 200},
		{callerMerger, 200, 403, 403, 403, 200},
	}
	for _, r := range rows {
		if code, _ := guardRequest(t, e, r.who, "arg="+name); code != r.attach {
			t.Errorf("%s attach = %d, want %d", r.who.user, code, r.attach)
		}
		if got := e.statusOf(t, r.who, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != r.input {
			t.Errorf("%s input = %d, want %d", r.who.user, got, r.input)
		}
		if got := e.upload(t, r.who, id, "file", "a.txt", []byte("x")).Code; got != r.upload {
			t.Errorf("%s upload = %d, want %d", r.who.user, got, r.upload)
		}
		if got := e.statusOf(t, r.who, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); got != r.keyboard {
			t.Errorf("%s take keyboard = %d, want %d", r.who.user, got, r.keyboard)
		}
		if got := e.statusOf(t, r.who, "GET", "/api/sessions/"+id+"/log", nil); got != r.log {
			t.Errorf("%s log = %d, want %d", r.who.user, got, r.log)
		}
	}
	// Private: the read role sees nothing at all, not even read-only.
	priv := e.create(t, callerRW, "private")
	if code, _ := guardRequest(t, e, callerRead, "arg="+sessionTmuxPrefix+priv); code != http.StatusNotFound {
		t.Errorf("read role attach on private = %d, want 404", code)
	}
	if code, _ := guardRequest(t, e, callerRW2, "arg="+sessionTmuxPrefix+priv); code != http.StatusNotFound {
		t.Errorf("other writer attach on private = %d, want 404", code)
	}
}

// ---- keyboard lease

func TestSessionsKeyboardCreatorHoldsItAndRulesApply(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.createShared(t, callerRW, "jam")
	s := sessionOf(t, e, callerRW, id)
	if s["keyboard_holder"] != callerRW.user || s["keyboard_mine"] != true || s["keyboard_since"] == "" {
		t.Fatalf("creator does not hold the keyboard at start: %v", s)
	}
	if sessionOf(t, e, callerRW2, id)["keyboard_mine"] != false {
		t.Error("keyboard_mine is not per caller")
	}
	// Another writer cannot take it while the holder is attached and active.
	attach := e.s.beginSessionAttach(id, callerRW.user, true)
	defer e.s.endSessionAttach(attach)
	rec := e.do(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("writer took an active keyboard: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeSessionJSON(t, rec)
	if m["holder"] != callerRW.user || m["grace_seconds"] == nil {
		t.Errorf("409 body = %v", m)
	}
	// The creator and owners always can.
	rec = e.do(t, callerOwner, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner take = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeSessionJSON(t, rec)["session"].(map[string]any); got["keyboard_holder"] != callerOwner.user || got["keyboard_mine"] != true {
		t.Errorf("after owner take: %v", got)
	}
	if d := e.rt.detachedIDs(); len(d) != 1 || d[0] != id {
		t.Errorf("previous holder's client not detached: %v", d)
	}
	// Presence no longer shows the old holder with the keyboard.
	for _, v := range e.s.sessionViewers(id) {
		if v.Keyboard {
			t.Errorf("stale keyboard flag on %v", v)
		}
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); rec.Code != http.StatusOK {
		t.Errorf("creator take back = %d", rec.Code)
	}
	// Taking what you already hold is a no-op 200 with no hand-over.
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); rec.Code != http.StatusOK {
		t.Errorf("idempotent take = %d", rec.Code)
	}
	if n := len(e.rt.detachedIDs()); n != 2 {
		t.Errorf("detach calls = %d, want 2 (one per hand-over)", n)
	}
	// Release: only the holder (or creator/owner); then any writer may take.
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "release"}); got != http.StatusForbidden {
		t.Errorf("stranger release = %d, want 403", got)
	}
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "release"}); got != http.StatusOK {
		t.Errorf("holder release = %d", got)
	}
	if s := sessionOf(t, e, callerRW, id); s["keyboard_holder"] != "" || s["keyboard_mine"] != false {
		t.Errorf("after release: %v", s)
	}
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); got != http.StatusOK {
		t.Errorf("writer take after release = %d", got)
	}
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "bogus"}); got != http.StatusBadRequest {
		t.Errorf("bogus action = %d", got)
	}
	// Audited with from/to.
	// Recent() is newest first; read the hand-overs in the order they happened.
	var hands []string
	for _, a := range e.auditActions() {
		if a.Action == auditSessionKeyboard {
			hands = append([]string{a.User + ":" + a.Detail}, hands...)
		}
	}
	if len(hands) != 4 {
		t.Fatalf("keyboard audits = %v", hands)
	}
	if !strings.Contains(hands[0], "from="+callerRW.user) || !strings.Contains(hands[0], "to="+callerOwner.user) || !strings.HasPrefix(hands[0], callerOwner.user+":") {
		t.Errorf("first hand-over audit = %q", hands[0])
	}
	if !strings.Contains(hands[2], "to=") || strings.Contains(hands[2], "to="+callerRW.user) {
		t.Errorf("release audit = %q", hands[2])
	}
}

func TestSessionsKeyboardIdleGraceAndDetachedHolder(t *testing.T) {
	e := newSessionTestEnv(t, true)
	t.Setenv(sessionKeyboardGraceEnv, "30")
	id := e.createShared(t, callerRW, "jam")
	// Holder not attached at all: another writer takes it at once.
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); got != http.StatusOK {
		t.Fatalf("take from an unattached holder = %d", got)
	}
	// The new holder attaches; the pure rule decides the rest.
	rec, _ := e.s.sessionStoreOrNil().get(id)
	now := time.Now()
	grace := sessionKeyboardGrace()
	if grace != 30*time.Second {
		t.Fatalf("grace = %v, want the env override", grace)
	}
	active := crux.UserSessionProbe{Exists: true, Activity: now.Add(-5 * time.Second)}
	if sessionKeyboardIdle(rec, active, true, now, grace) {
		t.Error("an attached holder with recent pane activity is not idle")
	}
	quiet := crux.UserSessionProbe{Exists: true, Activity: now.Add(-2 * time.Minute)}
	if sessionKeyboardIdle(rec, quiet, true, now, grace) {
		t.Error("the hand-over itself counts as activity until the grace passes")
	}
	rec.KeyboardSince = now.Add(-time.Hour).UTC().Format(time.RFC3339)
	rec.LastActivity = rec.KeyboardSince
	if !sessionKeyboardIdle(rec, quiet, true, now, grace) {
		t.Error("an attached holder idle past the grace can be replaced")
	}
	if !sessionKeyboardIdle(rec, active, false, now, grace) {
		t.Error("a holder who is not attached is idle")
	}
	rec.KeyboardHolder = ""
	if !sessionKeyboardIdle(rec, active, true, now, grace) {
		t.Error("no holder means free")
	}
	// Through the API: the holder attached but idle for an hour.
	e.s.sessionStoreOrNil().update(id, func(x *sessionRecord) {
		x.KeyboardSince = now.Add(-time.Hour).UTC().Format(time.RFC3339)
		x.LastActivity = x.KeyboardSince
	})
	e.rt.mu.Lock()
	e.rt.alive[id] = crux.UserSessionProbe{Exists: true, Activity: now.Add(-time.Hour)}
	e.rt.mu.Unlock()
	attach := e.s.beginSessionAttach(id, callerRW2.user, true)
	defer e.s.endSessionAttach(attach)
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "release"}); got != http.StatusOK {
		t.Errorf("creator may release another holder's keyboard: %d", got)
	}
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); got != http.StatusOK {
		t.Fatalf("retake = %d", got)
	}
	e.s.sessionStoreOrNil().update(id, func(x *sessionRecord) {
		x.KeyboardSince = now.Add(-time.Hour).UTC().Format(time.RFC3339)
		x.LastActivity = x.KeyboardSince
	})
	third := sessionCaller{"rhea", callerRW.role, false}
	if got := e.statusOf(t, third, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); got != http.StatusOK {
		t.Errorf("take from an idle attached holder = %d, want 200", got)
	}
}

func TestSessionsKeyboardFollowsResume(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.createShared(t, callerRW, "jam")
	e.do(t, callerOwner, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"})
	e.rt.mu.Lock()
	e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
	e.rt.mu.Unlock()
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusOK {
		t.Fatalf("close = %d", got)
	}
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/resume", map[string]any{}); got != http.StatusAccepted {
		t.Fatalf("resume = %d", got)
	}
	if rec := e.waitSettled(t, id); rec.KeyboardHolder != callerRW.user {
		t.Errorf("holder after resume = %q, want the resumer", rec.KeyboardHolder)
	}
}

// ---- attach mode is decided server-side

func TestTerminalSessionAttachModeIsServerDecided(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.RawQuery }))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TTYD_PORT", bu.Port())
	t.Setenv("HIVE_TTYD_CREDENTIAL", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	e := handoffEnv(t)
	id := e.createShared(t, callerRW, "jam")
	name := sessionTmuxPrefix + id
	attachTok, writeTok := ttydAttachToken(name), ttydAttachWriteToken(name)
	if writeTok == "" || writeTok == attachTok {
		t.Fatalf("write token must exist and differ from the attach token")
	}
	ask := func(c sessionCaller, query string) []string {
		got = ""
		req := httptest.NewRequest("GET", "/terminal/ws?"+query, nil)
		req.Header.Set("X-Hive-User", c.user)
		req.Header.Set("X-Hive-Role", c.role)
		if c.verified {
			req.Header.Set(ownerRoleVerifiedHeader, "true")
		}
		e.s.mux.ServeHTTP(httptest.NewRecorder(), req)
		q, _ := url.ParseQuery(got)
		return q["arg"]
	}
	want := func(desc string, args []string, writable bool) {
		t.Helper()
		exp := []string{name, attachTok}
		if writable {
			exp = append(exp, writeTok)
		}
		if strings.Join(args, " ") != strings.Join(exp, " ") {
			t.Errorf("%s: ttyd args = %v, want %v", desc, args, exp)
		}
	}
	want("holder (creator)", ask(callerRW, "arg="+name), true)
	want("owner without the keyboard", ask(callerOwner, "arg="+name), false)
	want("other writer", ask(callerRW2, "arg="+name), false)
	want("read role", ask(callerRead, "arg="+name), false)
	// A browser cannot ask for the keyboard: a forged extra arg (the real
	// write token included) is a malformed request, not a writable attach.
	got = ""
	req := httptest.NewRequest("GET", "/terminal/ws?arg="+name+"&arg="+writeTok, nil)
	req.Header.Set("X-Hive-User", callerRead.user)
	req.Header.Set("X-Hive-Role", callerRead.role)
	rec := httptest.NewRecorder()
	e.s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || got != "" {
		t.Errorf("forged write arg: %d, ttyd got %q", rec.Code, got)
	}
	// After the owner takes the keyboard the modes swap.
	if code := e.statusOf(t, callerOwner, "POST", "/api/sessions/"+id+"/keyboard", map[string]string{"action": "take"}); code != 200 {
		t.Fatalf("take = %d", code)
	}
	want("old holder after hand-over", ask(callerRW, "arg="+name), false)
	want("new holder", ask(callerOwner, "arg="+name), true)
}

// ---- presence

func TestSessionsPresenceViewers(t *testing.T) {
	e := handoffEnv(t)
	id := e.createShared(t, callerRW, "jam")
	name := sessionTmuxPrefix + id
	release := make(chan struct{})
	entered := make(chan struct{}, 3)
	h := e.s.guardTerminalSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	open := func(c sessionCaller, path string) {
		req := httptest.NewRequest("GET", path+"?arg="+name, nil)
		req.Header.Set("X-Hive-User", c.user)
		req.Header.Set("X-Hive-Role", c.role)
		go h.ServeHTTP(httptest.NewRecorder(), req)
		<-entered
	}
	open(callerRW, terminalPathPrefix+ttydWebSocketPath)
	open(callerRead, terminalPathPrefix+ttydWebSocketPath)
	open(callerRead, terminalPathPrefix+ttydWebSocketPath) // second tab, same person
	open(callerRW2, terminalPathPrefix+"/")                // the document is not an attach
	s := sessionOf(t, e, callerRead, id)
	viewers := s["viewers"].([]any)
	if len(viewers) != 2 {
		t.Fatalf("viewers = %v, want holder + reader (deduplicated)", viewers)
	}
	first := viewers[0].(map[string]any)
	if first["user"] != callerRW.user || first["keyboard"] != true || first["attached_since"] == "" {
		t.Errorf("holder entry = %v", first)
	}
	if second := viewers[1].(map[string]any); second["user"] != callerRead.user || second["keyboard"] != false {
		t.Errorf("reader entry = %v", second)
	}
	close(release)
	waitSessionsFor(t, "attaches to end", func() bool { return len(e.s.sessionViewers(id)) == 0 })
}

// ---- queued Send

func TestSessionsSendIsQueuedUntilThePaneIsIdle(t *testing.T) {
	e := newSessionTestEnv(t, true)
	fastSessionQueue(t, e)
	t.Setenv(sessionQueueDeliverAfterEnv, "3600")
	id := e.createShared(t, callerRW, "jam")
	e.rt.setCapture(claudeBusyPane)

	first := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "first", "submit": true})
	if first.Code != http.StatusAccepted {
		t.Fatalf("input = %d: %s", first.Code, first.Body.String())
	}
	q := decodeSessionJSON(t, first)["queued"].(map[string]any)
	if q["position"] != float64(1) || q["id"] == "" {
		t.Errorf("first queued = %v", q)
	}
	second := decodeSessionJSON(t, e.do(t, callerRW2, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "second"}))
	if second["queued"].(map[string]any)["position"] != float64(2) {
		t.Errorf("second queued = %v", second)
	}
	// Nothing is typed while the CLI is running: once the worker has marked
	// the head as delivering it is polling the pane, and still typed nothing.
	waitSessionsFor(t, "the worker to pick up the head", func() bool {
		return sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["items"].([]any)[0].(map[string]any)["delivering"] == true
	})
	if typed := e.rt.typedTexts(); len(typed) != 0 {
		t.Fatalf("delivered into a busy pane: %v", typed)
	}
	// The list shows the queue, per caller.
	s := sessionOf(t, e, callerRW2, id)
	queue := s["queue"].(map[string]any)
	if queue["length"] != float64(2) || queue["head"] != callerRW.user || queue["position"] != float64(2) {
		t.Errorf("queue view for the second sender = %v", queue)
	}
	items := queue["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["mine"] != false || items[1].(map[string]any)["mine"] != true || items[0].(map[string]any)["bytes"] != float64(5) {
		t.Errorf("queue items = %v", items)
	}
	for _, it := range items {
		if _, has := it.(map[string]any)["text"]; has {
			t.Error("queue item exposes the text")
		}
	}
	// The pane goes idle: the head is delivered, then the next.
	e.rt.setCapture(claudeIdlePane)
	waitSessionsFor(t, "both sends to be delivered", func() bool { return len(e.rt.typedTexts()) == 2 })
	if typed := e.rt.typedTexts(); typed[0] != "first" || typed[1] != "second" {
		t.Errorf("delivery order = %v", typed)
	}
	waitSessionsFor(t, "queue to drain", func() bool { return sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["length"] == float64(0) })
	// Audited at delivery, as the user who queued each item (Recent() is
	// newest first, so collect oldest first).
	var users []string
	for _, a := range e.auditActions() {
		if a.Action == auditSessionInput {
			users = append([]string{a.User}, users...)
			if !strings.Contains(a.Detail, "session="+id) || !strings.Contains(a.Detail, "queued_sec=") {
				t.Errorf("audit detail = %q", a.Detail)
			}
		}
	}
	if strings.Join(users, ",") != callerRW.user+","+callerRW2.user {
		t.Errorf("audited users = %v", users)
	}
}

// fakeQueueClock is a hand-advanced clock for the queue worker's fail-open
// deadline, so the test never depends on how fast the machine runs.
type fakeQueueClock struct {
	mu  sync.Mutex
	cur time.Time
}

func (c *fakeQueueClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeQueueClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}

// waitPolls waits until the pane has been read n more times, which gives a
// "nothing else happened" assertion real polls to be wrong in, without a sleep.
func (e *sessionTestEnv) waitPolls(t *testing.T, n int) {
	t.Helper()
	target := e.rt.captureCount() + n
	waitSessionsFor(t, "more pane polls", func() bool { return e.rt.captureCount() >= target })
}

func TestSessionsSendFailsOpenAfterTheDeadline(t *testing.T) {
	const deadline = time.Minute
	e := newSessionTestEnv(t, true)
	fastSessionQueue(t, e)
	clock := &fakeQueueClock{cur: time.Unix(1_700_000_000, 0)}
	e.s.sessionQueueTiming.now = clock.Now
	e.s.sessionQueueTiming.deliverAfter = deadline
	id := e.create(t, callerRW, "stuck")
	e.rt.setCapture(claudeBusyPane)
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "eventually"}); got != http.StatusAccepted {
		t.Fatalf("input = %d", got)
	}
	// The worker is polling the busy pane; before the deadline nothing is typed.
	waitSessionsFor(t, "the worker to pick up the head", func() bool {
		return sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["items"].([]any)[0].(map[string]any)["delivering"] == true
	})
	clock.Advance(deadline - time.Second)
	e.waitPolls(t, 10)
	if typed := e.rt.typedTexts(); len(typed) != 0 {
		t.Fatalf("delivered before the deadline: %v", typed)
	}
	// Past the deadline the send fails open, and is delivered exactly once.
	clock.Advance(2 * time.Second)
	waitSessionsFor(t, "fail-open delivery", func() bool { return len(e.rt.typedTexts()) == 1 })
	waitSessionsFor(t, "queue to drain", func() bool { return sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["length"] == float64(0) })
	// The worker has exited with the queue empty, so nothing can type again.
	if typed := e.rt.typedTexts(); len(typed) != 1 || typed[0] != "eventually" {
		t.Errorf("typed = %v, want exactly one delivery of the queued text", typed)
	}
}

func TestSessionsQueueCancelBoundsAndEndedSession(t *testing.T) {
	e := newSessionTestEnv(t, true)
	fastSessionQueue(t, e)
	t.Setenv(sessionQueueDeliverAfterEnv, "3600")
	id := e.createShared(t, callerRW, "jam")
	e.rt.setCapture(claudeBusyPane)
	// Hold the head: the worker marks it delivering while it waits for idle.
	head := decodeSessionJSON(t, e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "head"}))["queued"].(map[string]any)["id"].(string)
	mine := decodeSessionJSON(t, e.do(t, callerRW2, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "mine"}))["queued"].(map[string]any)["id"].(string)
	theirs := decodeSessionJSON(t, e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "theirs"}))["queued"].(map[string]any)["id"].(string)
	waitSessionsFor(t, "head to be marked delivering", func() bool {
		return sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["items"].([]any)[0].(map[string]any)["delivering"] == true
	})
	if got := e.statusOf(t, callerRW2, "DELETE", "/api/sessions/"+id+"/input/"+theirs, nil); got != http.StatusForbidden {
		t.Errorf("cancel someone else's = %d, want 403", got)
	}
	if got := e.statusOf(t, callerRead, "DELETE", "/api/sessions/"+id+"/input/"+mine, nil); got != http.StatusForbidden {
		t.Errorf("read role cancel = %d, want 403", got)
	}
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id+"/input/"+head, nil); got != http.StatusConflict {
		t.Errorf("cancel the item being delivered = %d, want 409", got)
	}
	if got := e.statusOf(t, callerRW2, "DELETE", "/api/sessions/"+id+"/input/"+mine, nil); got != http.StatusOK {
		t.Errorf("cancel own = %d", got)
	}
	if got := e.statusOf(t, callerRW2, "DELETE", "/api/sessions/"+id+"/input/"+mine, nil); got != http.StatusNotFound {
		t.Errorf("cancel twice = %d, want 404", got)
	}
	// The creator (manage) may cancel anyone's.
	if got := e.statusOf(t, callerOwner, "DELETE", "/api/sessions/"+id+"/input/"+theirs, nil); got != http.StatusOK {
		t.Errorf("owner cancel = %d", got)
	}
	if n := sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["length"]; n != float64(1) {
		t.Errorf("queue length after cancels = %v, want 1", n)
	}
	// The queue is bounded.
	for i := 1; i < sessionQueueMaxItems; i++ {
		if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != http.StatusAccepted {
			t.Fatalf("fill %d = %d", i, got)
		}
	}
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != http.StatusTooManyRequests {
		t.Errorf("over the queue cap = %d, want 429", got)
	}
	// Closing the session drops the queue; nothing is delivered afterwards.
	if got := e.statusOf(t, callerRW, "DELETE", "/api/sessions/"+id, nil); got != http.StatusOK {
		t.Fatalf("close = %d", got)
	}
	e.rt.setCapture(claudeIdlePane)
	// The worker notices the dropped queue on its next poll and stops.
	waitSessionsFor(t, "the worker to stop", func() bool {
		e.s.sessionQueuesMu.Lock()
		defer e.s.sessionQueuesMu.Unlock()
		q := e.s.sessionQueueFor(id, false)
		return q != nil && !q.running
	})
	if typed := e.rt.typedTexts(); len(typed) != 0 {
		t.Errorf("delivered after close: %v", typed)
	}
	if n := sessionOf(t, e, callerRW, id)["queue"].(map[string]any)["length"]; n != float64(0) {
		t.Errorf("queue after close = %v", n)
	}
}

func TestSessionPaneIdleHeuristics(t *testing.T) {
	cases := []struct {
		name, vendor, pane string
		idle               bool
	}{
		{"claude empty prompt with shortcuts line", "claude", claudeIdlePane, true},
		{"claude boxed prompt", "claude", "╭──────╮\n│ >    │\n╰──────╯\n", true},
		{"claude running", "claude", claudeBusyPane, false},
		{"claude spinner only", "claude", "⠧ Reading files…\n> \n", false},
		{"claude typed text waiting", "claude", "──────\n> half a thought\n──────\n", false},
		{"claude permission prompt", "claude", "Allow Bash(rm -rf)?\n  1. Yes\n  2. No\n", false},
		{"codex empty prompt", "codex", "› \n  send ⏎  quit ctrl+c\n", true},
		{"codex working", "codex", "• Working (3s • esc to interrupt)\n› \n", false},
		{"copilot idle", "copilot", "> \n", true},
		{"copilot cancel hint", "copilot", "Thinking (ctrl+c to cancel)\n> \n", false},
		{"unknown vendor default prompt", "goose", "some output\n❯ \n", true},
		{"unknown vendor busy", "goose", "esc to interrupt\n❯ \n", false},
		{"nothing drawn yet", "claude", "", false},
		{"prompt scrolled off", "claude", "> \nline1\nline2\nline3\nline4\nline5\n", false},
	}
	for _, c := range cases {
		if got := sessionPaneIdle(c.vendor, c.pane); got != c.idle {
			t.Errorf("%s: idle = %v, want %v", c.name, got, c.idle)
		}
	}
}

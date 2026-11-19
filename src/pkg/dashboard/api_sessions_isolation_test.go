package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func (e *sessionTestEnv) statusOf(t *testing.T, c sessionCaller, method, path string, body any) int {
	t.Helper()
	return e.do(t, c, method, path, body).Code
}

func listedIDs(t *testing.T, e *sessionTestEnv, c sessionCaller) []string {
	t.Helper()
	var ids []string
	for _, v := range decodeSessionJSON(t, e.do(t, c, "GET", "/api/sessions", nil))["sessions"].([]any) {
		ids = append(ids, v.(map[string]any)["id"].(string))
	}
	return ids
}

func idsContain(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// The ownership matrix for a PRIVATE session (the default): only its creator
// and owners may see it, read its log, type, upload, attach or resume.
func TestSessionsPrivateByDefaultOwnershipMatrix(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "private")
	if rec, _ := e.s.sessionStoreOrNil().get(id); rec.Shared {
		t.Fatal("a new session must default to private")
	}
	type row struct {
		who                           sessionCaller
		list                          bool
		log, input, upload, patch, rs int
	}
	rows := []row{
		{callerRW, true, 200, 202, 200, 200, 409},      // creator (input is queued: 202)
		{callerOwner, true, 200, 202, 200, 200, 409},   // owner
		{callerRW2, false, 404, 404, 404, 404, 404},    // another writer
		{callerRead, false, 404, 403, 403, 404, 403},   // read role (write gate first)
		{callerMerger, false, 404, 403, 403, 404, 403}, // merger
		{callerForgedOwn, false, 404, 403, 403, 404, 403},
	}
	for _, r := range rows {
		name := r.who.user
		if got := idsContain(listedIDs(t, e, r.who), id); got != r.list {
			t.Errorf("%s: listed = %v, want %v", name, got, r.list)
		}
		if got := e.statusOf(t, r.who, "GET", "/api/sessions/"+id+"/log", nil); got != r.log {
			t.Errorf("%s: log = %d, want %d", name, got, r.log)
		}
		if got := e.statusOf(t, r.who, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != r.input {
			t.Errorf("%s: input = %d, want %d", name, got, r.input)
		}
		if got := e.upload(t, r.who, id, "file", "a.txt", []byte("x")).Code; got != r.upload {
			t.Errorf("%s: upload = %d, want %d", name, got, r.upload)
		}
		if got := e.statusOf(t, r.who, "PATCH", "/api/sessions/"+id, map[string]string{"name": "n2"}); got != r.patch {
			t.Errorf("%s: patch = %d, want %d", name, got, r.patch)
		}
		if got := e.statusOf(t, r.who, "POST", "/api/sessions/"+id+"/resume", nil); got != r.rs {
			t.Errorf("%s: resume = %d, want %d", name, got, r.rs)
		}
	}
	// A stranger's refusal must not reveal that the session exists.
	if rec := e.do(t, callerRW2, "GET", "/api/sessions/"+id+"/log", nil); strings.Contains(rec.Body.String(), "creator") {
		t.Errorf("404 body discloses the session: %s", rec.Body.String())
	}
}

func TestSessionsSharedFlagOpensWatchingAndTypingNotManagement(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "jam")
	if rec := e.do(t, callerRW2, "PATCH", "/api/sessions/"+id, map[string]any{"shared": true}); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger toggled the flag: %d", rec.Code)
	}
	rec := e.do(t, callerRW, "PATCH", "/api/sessions/"+id, map[string]any{"shared": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("creator share = %d: %s", rec.Code, rec.Body.String())
	}
	if decodeSessionJSON(t, rec)["session"].(map[string]any)["shared"] != true {
		t.Error("view does not report shared")
	}
	if !idsContain(listedIDs(t, e, callerRead), id) {
		t.Error("a shared session is not listed for the read role")
	}
	if got := e.statusOf(t, callerRead, "GET", "/api/sessions/"+id+"/log", nil); got != 200 {
		t.Errorf("read role log on shared = %d", got)
	}
	if got := e.statusOf(t, callerRead, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != 403 {
		t.Errorf("read role typed into a shared session: %d", got)
	}
	if got := e.statusOf(t, callerRW2, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); got != 202 {
		t.Errorf("writer input on shared = %d, want 202 (queued)", got)
	}
	if got := e.upload(t, callerRW2, id, "file", "a.txt", []byte("x")).Code; got != 200 {
		t.Errorf("writer upload on shared = %d", got)
	}
	for _, c := range []struct{ m, p string }{{"PATCH", ""}, {"DELETE", ""}, {"POST", "/resume"}} {
		if got := e.statusOf(t, callerRW2, c.m, "/api/sessions/"+id+c.p, map[string]string{"name": "x"}); got != 403 {
			t.Errorf("writer %s %s on shared = %d, want 403 (sharing never grants management)", c.m, c.p, got)
		}
	}
	// And it can be made private again.
	e.do(t, callerOwner, "PATCH", "/api/sessions/"+id, map[string]any{"shared": false})
	if idsContain(listedIDs(t, e, callerRead), id) {
		t.Error("unshared session still listed for the read role")
	}
}

func TestSessionsListNeverDisclosesPrivateIDs(t *testing.T) {
	e := newSessionTestEnv(t, true)
	priv := e.create(t, callerRW, "mine")
	for _, c := range []sessionCaller{callerRW2, callerRead, callerMerger} {
		body := e.do(t, c, "GET", "/api/sessions", nil).Body.String()
		if strings.Contains(body, priv) {
			t.Errorf("%s was shown a private session id", c.user)
		}
	}
	if !idsContain(listedIDs(t, e, callerRW), priv) || !idsContain(listedIDs(t, e, callerOwner), priv) {
		t.Error("creator and owner must see the session")
	}
}

func TestSessionsAllocateDistinctUIDSlotsAndReuseOnlyAfterRemoval(t *testing.T) {
	e := newSessionTestEnv(t, true)
	a := e.create(t, callerOwner, "a")
	b := e.create(t, callerOwner, "b")
	st := e.s.sessionStoreOrNil()
	ra, _ := st.get(a)
	rb, _ := st.get(b)
	if ra.UIDSlot < 1 || rb.UIDSlot < 1 || ra.UIDSlot == rb.UIDSlot {
		t.Fatalf("slots = %d, %d; want distinct and positive", ra.UIDSlot, rb.UIDSlot)
	}
	for _, spec := range e.rt.started {
		rec, _ := st.get(spec.ID)
		if spec.UIDSlot != rec.UIDSlot {
			t.Errorf("runtime started with slot %d, record holds %d", spec.UIDSlot, rec.UIDSlot)
		}
	}
	// Closing keeps the slot (the home still exists and is owned by that uid).
	e.do(t, callerOwner, "DELETE", "/api/sessions/"+a, nil)
	c := e.create(t, callerOwner, "c")
	rc, _ := st.get(c)
	if rc.UIDSlot == ra.UIDSlot {
		t.Error("slot of an ended (not removed) session was handed out again")
	}
	// A failed file removal keeps the record and therefore the slot.
	e.rt.setRemoveErr(errors.New("rm failed"))
	if got := e.statusOf(t, callerOwner, "DELETE", "/api/sessions/"+a, nil); got != http.StatusAccepted {
		t.Errorf("delete with failing cleanup = %d, want 202", got)
	}
	if got := e.waitDeleteFailed(t, a); got.UIDSlot != ra.UIDSlot {
		t.Errorf("record after a failed removal = %+v; it must keep its slot", got)
	}
	// Once removal succeeds the slot is free again.
	e.rt.setRemoveErr(nil)
	if got := e.statusOf(t, callerOwner, "DELETE", "/api/sessions/"+a, nil); got != http.StatusAccepted {
		t.Fatalf("delete = %d", got)
	}
	e.waitDeleted(t, a)
	d := e.create(t, callerOwner, "d")
	if rd, _ := st.get(d); rd.UIDSlot != ra.UIDSlot {
		t.Errorf("freed slot %d not reused, got %d", ra.UIDSlot, rd.UIDSlot)
	}
}

func TestSessionsDeleteAndPruneRemoveSessionFiles(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "bye")
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil) // ended
	if len(e.rt.removed) != 0 {
		t.Fatalf("closing a running session must keep its files for resume: %v", e.rt.removed)
	}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil) // removed
	e.waitDeleted(t, id)
	if got := e.rt.removedIDs(); !idsContain(got, id) {
		t.Errorf("deleting an ended session did not remove its files: %v", got)
	}

	pruned := e.create(t, callerRW, "old")
	st := e.s.sessionStoreOrNil()
	_, _, _ = st.update(pruned, func(r *sessionRecord) {
		r.Status = sessionStatusEnded
		r.EndedAt = time.Now().Add(-2 * sessionEndedRetention).UTC().Format(time.RFC3339)
	})
	e.rt.setRemoveErr(errors.New("busy"))
	e.s.sweepSessions(time.Now())
	// The prune is the same background job as a delete: a failed removal
	// keeps the record.
	e.waitDeleteFailed(t, pruned)
	e.rt.setRemoveErr(nil)
	e.s.sweepSessions(time.Now())
	e.waitDeleted(t, pruned)
	if got := e.rt.removedIDs(); !idsContain(got, pruned) {
		t.Errorf("prune did not remove files: %v", got)
	}
}

func TestSessionsFailedStartCleansUp(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.rt.startFn = func(crux.UserSessionSpec) error { return errors.New("boom") }
	rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "x", "vendor": "claude"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)["id"].(string)
	if got := e.waitSettled(t, id); got.Status != sessionStatusFailed {
		t.Fatalf("record after failed start = %+v", got)
	}
	waitSessionsFor(t, "sandbox removal", func() bool {
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		return len(e.rt.removed) == 1
	})
}

func TestSessionsLegacyRecordWithoutSlotCannotResume(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerOwner, "legacy")
	if _, _, err := e.s.sessionStoreOrNil().update(id, func(r *sessionRecord) {
		r.Status, r.UIDSlot, r.ResumeCommand = sessionStatusEnded, 0, "claude --resume abcdefgh"
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.statusOf(t, callerOwner, "POST", "/api/sessions/"+id+"/resume", nil); got != 409 {
		t.Errorf("resume of a pre-isolation record = %d, want 409", got)
	}
}

// ---- terminal handoff binding

func terminalHandoffPost(t *testing.T, e *sessionTestEnv, c sessionCaller, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest("POST", terminalHandoffPath, bytes.NewReader(raw))
	req.Header.Set("X-Hive-User", c.user)
	req.Header.Set("X-Hive-Role", c.role)
	if c.verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	e.s.handleCreateTerminalHandoff(rec, req)
	return rec
}

func handoffEnv(t *testing.T) *sessionTestEnv {
	t.Helper()
	t.Setenv("HIVE_TERMINAL_KEY", "test-terminal-key-0123456789")
	t.Setenv(ttydAttachKeyEnv, "test-attach-key")
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.HiveID = "hosted-alpha"
	return e
}

func TestTerminalHandoffIsBoundToTheSessionAndItsCreator(t *testing.T) {
	e := handoffEnv(t)
	id := e.create(t, callerRW, "mine")
	other := e.create(t, callerRW2, "theirs")

	if got := terminalHandoffPost(t, e, callerRW2, map[string]string{"session": id}).Code; got != 404 {
		t.Errorf("a stranger minted a code for a private session: %d", got)
	}
	if got := terminalHandoffPost(t, e, callerOwner, map[string]string{"session": id}).Code; got != 200 {
		t.Errorf("owner handoff = %d", got)
	}
	rec := terminalHandoffPost(t, e, callerRW, map[string]string{"session": id})
	if rec.Code != 200 {
		t.Fatalf("creator handoff = %d: %s", rec.Code, rec.Body.String())
	}
	code := decodeSessionJSON(t, rec)["code"].(string)
	_, _, bound, ok := e.s.redeemTerminalHandoffBound(code)
	if !ok || bound != id {
		t.Fatalf("code bound to %q (ok=%v), want %q", bound, ok, id)
	}
	if _, _, _, again := e.s.redeemTerminalHandoffBound(code); again {
		t.Error("a handoff code is single use")
	}

	// Binding semantics.
	if !handoffBindingMatches(id, []string{sessionTmuxPrefix + id}) {
		t.Error("code does not open its own session")
	}
	for _, args := range [][]string{
		{sessionTmuxPrefix + other}, {"hive-scanner"}, {}, {sessionTmuxPrefix + id, "hive-scanner"},
	} {
		if handoffBindingMatches(id, args) {
			t.Errorf("session-bound code accepted for %v", args)
		}
	}
	// An unbound (agent terminal) code never opens a user session.
	if handoffBindingMatches("", []string{sessionTmuxPrefix + id}) || handoffBindingMatches("", []string{"hive-scanner", sessionTmuxPrefix + id}) {
		t.Error("an unbound code opened a user session")
	}
	if !handoffBindingMatches("", []string{"hive-scanner"}) || !handoffBindingMatches("", nil) {
		t.Error("unbound codes must keep working for agent terminals")
	}
	// A session-less request mints an unbound code.
	rec = terminalHandoffPost(t, e, callerRW, nil)
	if _, _, bound, ok := e.s.redeemTerminalHandoffBound(decodeSessionJSON(t, rec)["code"].(string)); !ok || bound != "" {
		t.Errorf("session-less handoff bound to %q", bound)
	}
}

func TestTerminalHandoffForSessionFailsClosedWithoutAttachKey(t *testing.T) {
	e := handoffEnv(t)
	id := e.create(t, callerRW, "x")
	t.Setenv(ttydAttachKeyEnv, "")
	if got := terminalHandoffPost(t, e, callerRW, map[string]string{"session": id}).Code; got != http.StatusServiceUnavailable {
		t.Errorf("handoff without an attach key = %d, want 503", got)
	}
	// Agent terminals are unaffected by the session key.
	if got := terminalHandoffPost(t, e, callerRW, nil).Code; got != 200 {
		t.Errorf("agent handoff = %d", got)
	}
}

// ---- attach token and guard

func TestTTYDAttachTokenIsPerSessionAndKeyed(t *testing.T) {
	t.Setenv(ttydAttachKeyEnv, "k1")
	a, b := ttydAttachToken("hive-sess-aaaaaaaa"), ttydAttachToken("hive-sess-bbbbbbbb")
	if a == "" || a == b || len(a) != 64 {
		t.Fatalf("tokens = %q, %q", a, b)
	}
	if a != ttydAttachToken("hive-sess-aaaaaaaa") {
		t.Error("token is not deterministic")
	}
	t.Setenv(ttydAttachKeyEnv, "k2")
	if ttydAttachToken("hive-sess-aaaaaaaa") == a {
		t.Error("token does not depend on the key")
	}
	t.Setenv(ttydAttachKeyEnv, "")
	if ttydAttachToken("hive-sess-aaaaaaaa") != "" {
		t.Error("a token was minted without a key")
	}
}

func guardRequest(t *testing.T, e *sessionTestEnv, c sessionCaller, rawQuery string) (int, bool) {
	t.Helper()
	called := false
	h := e.s.guardTerminalSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", terminalPathPrefix+"/ws?"+rawQuery, nil)
	req.Header.Set("X-Hive-User", c.user)
	req.Header.Set("X-Hive-Role", c.role)
	if c.verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, called
}

func TestTerminalSessionGateEnforcesOwnershipAndShape(t *testing.T) {
	e := handoffEnv(t)
	id := e.create(t, callerRW, "mine")
	name := sessionTmuxPrefix + id
	cases := []struct {
		name  string
		who   sessionCaller
		query string
		want  int
	}{
		{"creator", callerRW, "arg=" + name, 200},
		{"owner", callerOwner, "arg=" + name, 200},
		{"other writer", callerRW2, "arg=" + name, 404},
		{"read role", callerRead, "arg=" + name, 404},
		{"two args", callerRW, "arg=" + name + "&arg=--evil", 400},
		{"agent arg then session", callerRW2, "arg=hive-scanner&arg=" + name, 400},
		{"bad id", callerOwner, "arg=hive-sess-../../x", 400},
		{"unknown session", callerOwner, "arg=hive-sess-ffffffffffff", 404},
		{"agent terminal passes through", callerRW2, "arg=hive-scanner", 200},
		{"no arg passes through", callerRW2, "", 200},
	}
	for _, tc := range cases {
		code, called := guardRequest(t, e, tc.who, tc.query)
		if code != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, code, tc.want)
		}
		if called != (tc.want == 200) {
			t.Errorf("%s: proxy reached = %v", tc.name, called)
		}
	}
	// Ended sessions and a missing attach key fail closed.
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	if code, _ := guardRequest(t, e, callerRW, "arg="+name); code != http.StatusConflict {
		t.Errorf("ended session = %d, want 409", code)
	}
	id2 := e.create(t, callerRW, "second")
	t.Setenv(ttydAttachKeyEnv, "")
	if code, called := guardRequest(t, e, callerRW, "arg="+sessionTmuxPrefix+id2); code != http.StatusServiceUnavailable || called {
		t.Errorf("no attach key = %d (proxy reached %v), want 503", code, called)
	}
	// Shared sessions open to writers and, read-only, to the read role; the
	// mode is decided here (see TestTerminalSessionAttachModeIsServerDecided).
	e.do(t, callerRW, "PATCH", "/api/sessions/"+id2, map[string]any{"shared": true})
	t.Setenv(ttydAttachKeyEnv, "test-attach-key")
	if code, _ := guardRequest(t, e, callerRW2, "arg="+sessionTmuxPrefix+id2); code != 200 {
		t.Errorf("writer on shared = %d", code)
	}
	if code, _ := guardRequest(t, e, callerRead, "arg="+sessionTmuxPrefix+id2); code != 200 {
		t.Errorf("read role on shared = %d, want 200 (read-only attach)", code)
	}
}

func TestTerminalProxyAppendsAttachTokenOnlyForSessions(t *testing.T) {
	// Use the real proxy Rewrite via a backend that echoes the query it got.
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
	}))
	defer backend.Close()
	bu, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TTYD_PORT", bu.Port())
	t.Setenv("HIVE_TTYD_CREDENTIAL", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	// NewServer registers the /terminal proxy itself, resolving the ttyd port
	// and credential from the environment set above.
	e := handoffEnv(t)
	id := e.create(t, callerRW, "tok")
	name := sessionTmuxPrefix + id

	req := httptest.NewRequest("GET", "/terminal/ws?arg="+name+"&code=zzz&token=t", nil)
	req.Header.Set("X-Hive-User", callerRW.user)
	req.Header.Set("X-Hive-Role", callerRW.role)
	e.s.mux.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(got, "arg="+name) || !strings.Contains(got, "arg="+ttydAttachToken(name)) {
		t.Errorf("ttyd got %q, want the session name then its attach token", got)
	}
	if strings.Contains(got, "code=") || strings.Contains(got, "token=t") {
		t.Errorf("credentials forwarded to ttyd: %q", got)
	}
	got = ""
	req = httptest.NewRequest("GET", "/terminal/ws?arg=hive-scanner", nil)
	req.Header.Set("X-Hive-User", callerRW.user)
	req.Header.Set("X-Hive-Role", callerRW.role)
	e.s.mux.ServeHTTP(httptest.NewRecorder(), req)
	if got != "arg=hive-scanner" {
		t.Errorf("agent terminal query = %q, want it untouched", got)
	}
}

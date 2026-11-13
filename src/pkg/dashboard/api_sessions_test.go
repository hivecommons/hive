package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/terminalassert"
)

// fakeSessionRuntime is a deterministic SessionRuntime: no tmux, no processes.
type fakeSessionRuntime struct {
	mu      sync.Mutex
	vendors []crux.UserSessionVendor
	workDir string
	alive   map[string]crux.UserSessionProbe
	started []crux.UserSessionSpec
	killed  []string
	typed   []string
	submits []bool
	capture string
	resume  map[string]crux.UserSessionResume
	startFn func(crux.UserSessionSpec) error
}

func newFakeSessionRuntime(t *testing.T) *fakeSessionRuntime {
	t.Helper()
	return &fakeSessionRuntime{
		vendors: []crux.UserSessionVendor{
			{ID: "claude", Label: "Claude Code", Available: true},
			{ID: "codex", Label: "Codex", Available: true},
			{ID: "goose", Label: "Goose", Available: false},
		},
		workDir: t.TempDir(),
		alive:   map[string]crux.UserSessionProbe{},
		resume:  map[string]crux.UserSessionResume{},
	}
}

func (f *fakeSessionRuntime) Vendors() []crux.UserSessionVendor { return f.vendors }
func (f *fakeSessionRuntime) SupportsResume(v string) bool      { return v == "claude" }
func (f *fakeSessionRuntime) WorkDir(id string) string          { return filepath.Join(f.workDir, id) }
func (f *fakeSessionRuntime) Start(spec crux.UserSessionSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startFn != nil {
		if err := f.startFn(spec); err != nil {
			return err
		}
	}
	f.started = append(f.started, spec)
	f.alive[spec.ID] = crux.UserSessionProbe{Exists: true, Activity: time.Now()}
	return nil
}
func (f *fakeSessionRuntime) Probe(id string) crux.UserSessionProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[id]
}
func (f *fakeSessionRuntime) Kill(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	delete(f.alive, id)
	return nil
}
func (f *fakeSessionRuntime) Capture(id string, lines int) (string, error) {
	return f.capture, nil
}
func (f *fakeSessionRuntime) SendText(id, text string, submit bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.ContainsRune(text, 0x1b) {
		return errors.New("input contains a control character")
	}
	f.typed = append(f.typed, text)
	f.submits = append(f.submits, submit)
	return nil
}
func (f *fakeSessionRuntime) ResumeInfo(id, vendor string) (crux.UserSessionResume, bool) {
	r, ok := f.resume[id]
	return r, ok
}

type sessionTestEnv struct {
	s  *Server
	rt *fakeSessionRuntime
}

func newSessionTestEnv(t *testing.T, enabled bool) *sessionTestEnv {
	t.Helper()
	t.Setenv(sessionsDirEnv, t.TempDir())
	t.Setenv(config.EnvSessionsEnabled, "")
	for _, e := range []string{config.EnvSessionsMax, config.EnvSessionsMaxPerUser, config.EnvSessionsIdleTTLHours,
		sessionsMaxInputBytesEnv, sessionsMaxUploadBytesEnv, sessionsMaxUploadsEnv} {
		t.Setenv(e, "")
	}
	rt := newFakeSessionRuntime(t)
	cfg := &config.Config{}
	cfg.Dashboard.Sessions.Enabled = enabled
	s := NewServer(0, nil)
	s.deps = &Dependencies{Config: cfg, Sessions: rt}
	// Routes are registered by RegisterAPI in production; the tests register
	// just the Sessions routes on the bare server.
	s.registerSessionRoutes()
	return &sessionTestEnv{s: s, rt: rt}
}

type sessionCaller struct {
	user, role string
	verified   bool
}

var (
	callerOwner     = sessionCaller{"olivia", config.RoleOwner, true}
	callerOwner2    = sessionCaller{"omar", config.RoleOwner, true}
	callerRW        = sessionCaller{"rita", config.RoleReadWrite, false}
	callerRW2       = sessionCaller{"ray", config.RoleReadWrite, false}
	callerMerger    = sessionCaller{"mia", config.RoleMerger, false}
	callerRead      = sessionCaller{"reed", config.RoleRead, false}
	callerForgedOwn = sessionCaller{"mallory", config.RoleOwner, false}
)

func (e *sessionTestEnv) do(t *testing.T, c sessionCaller, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Hive-User", c.user)
	req.Header.Set("X-Hive-Role", c.role)
	if c.verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	e.s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeSessionJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %q (%v)", rec.Body.String(), err)
	}
	return m
}

func (e *sessionTestEnv) create(t *testing.T, c sessionCaller, name string) string {
	t.Helper()
	rec := e.do(t, c, "POST", "/api/sessions", map[string]string{"name": name, "group": "rfc", "vendor": "claude"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sess, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)
	id, _ := sess["id"].(string)
	if id == "" {
		t.Fatalf("no id in %s", rec.Body.String())
	}
	return id
}

func (e *sessionTestEnv) auditActions() []AuditEntry { return e.s.audit.Recent(0) }

func TestSessionsDisabledReportsEmptyAndHidesRoutes(t *testing.T) {
	e := newSessionTestEnv(t, false)
	rec := e.do(t, callerRead, "GET", "/api/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET disabled = %d", rec.Code)
	}
	m := decodeSessionJSON(t, rec)
	if m["enabled"] != false {
		t.Errorf("enabled = %v", m["enabled"])
	}
	if s, _ := m["sessions"].([]any); s == nil || len(s) != 0 {
		t.Errorf("sessions = %v", m["sessions"])
	}
	if v, _ := m["vendors"].([]any); v == nil || len(v) != 0 {
		t.Errorf("vendors = %v", m["vendors"])
	}
	if l, _ := m["limits"].(map[string]any); l == nil || len(l) != 0 {
		t.Errorf("limits = %v", m["limits"])
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/sessions"}, {"PATCH", "/api/sessions/abc"}, {"DELETE", "/api/sessions/abc"},
		{"GET", "/api/sessions/abc/log"}, {"POST", "/api/sessions/abc/input"},
		{"POST", "/api/sessions/abc/upload"}, {"POST", "/api/sessions/abc/resume"},
	} {
		if rec := e.do(t, callerOwner, tc.method, tc.path, map[string]string{"name": "x"}); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s while disabled = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
	if len(e.rt.started) != 0 {
		t.Error("disabled feature started a session")
	}
}

func TestSessionsEnvOverridesConfigFlag(t *testing.T) {
	e := newSessionTestEnv(t, false)
	t.Setenv(config.EnvSessionsEnabled, "true")
	if m := decodeSessionJSON(t, e.do(t, callerRead, "GET", "/api/sessions", nil)); m["enabled"] != true {
		t.Errorf("env true did not enable: %v", m)
	}
	e = newSessionTestEnv(t, true)
	t.Setenv(config.EnvSessionsEnabled, "false")
	if m := decodeSessionJSON(t, e.do(t, callerRead, "GET", "/api/sessions", nil)); m["enabled"] != false {
		t.Errorf("env false did not disable: %v", m)
	}
}

func TestSessionsListShapeVendorsAndLimits(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Backends.Deny = []string{"codex"}
	rec := e.do(t, callerRead, "GET", "/api/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	m := decodeSessionJSON(t, rec)
	if m["enabled"] != true {
		t.Fatalf("enabled = %v", m["enabled"])
	}
	var ids []string
	for _, v := range m["vendors"].([]any) {
		ids = append(ids, v.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "claude,goose" {
		t.Errorf("vendors = %v, want denied codex filtered out", ids)
	}
	lim := m["limits"].(map[string]any)
	for _, k := range []string{"max_sessions", "max_per_user", "idle_ttl_hours", "max_input_bytes", "max_upload_bytes"} {
		if _, ok := lim[k]; !ok {
			t.Errorf("limits missing %s: %v", k, lim)
		}
	}
}

func TestSessionsRoleMatrix(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerOwner, "owner session")
	post := map[string]string{"name": "n", "group": "g", "vendor": "claude"}

	type row struct {
		who                  sessionCaller
		create, input, patch int // expected status codes
	}
	// Create/input need owner or read-write; patch needs creator or owner and
	// every non-owner here is not the creator of the owner's session.
	rows := []row{
		{callerOwner, 201, 200, 200},
		{callerRW, 201, 200, 403},
		{callerMerger, 403, 403, 403},
		{callerRead, 403, 403, 403},
		{callerForgedOwn, 403, 403, 403},
	}
	for _, r := range rows {
		if rec := e.do(t, r.who, "POST", "/api/sessions", post); rec.Code != r.create {
			t.Errorf("%s create = %d, want %d (%s)", r.who.role, rec.Code, r.create, rec.Body.String())
		}
		if rec := e.do(t, r.who, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "hi", "submit": true}); rec.Code != r.input {
			t.Errorf("%s input = %d, want %d", r.who.role, rec.Code, r.input)
		}
		if rec := e.do(t, r.who, "PATCH", "/api/sessions/"+id, map[string]string{"name": "renamed"}); rec.Code != r.patch {
			t.Errorf("%s patch = %d, want %d", r.who.role, rec.Code, r.patch)
		}
	}
	// Reads are open to every role, including read.
	for _, who := range []sessionCaller{callerOwner, callerRW, callerMerger, callerRead} {
		if rec := e.do(t, who, "GET", "/api/sessions", nil); rec.Code != http.StatusOK {
			t.Errorf("%s list = %d", who.role, rec.Code)
		}
		if rec := e.do(t, who, "GET", "/api/sessions/"+id+"/log", nil); rec.Code != http.StatusOK {
			t.Errorf("%s log = %d", who.role, rec.Code)
		}
	}
	// A forged owner role without the verified marker cannot delete.
	if rec := e.do(t, callerForgedOwn, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusForbidden {
		t.Errorf("forged owner delete = %d", rec.Code)
	}
}

func TestSessionsCreatorOrOwnerMayRenameAndClose(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "rita's")
	if rec := e.do(t, callerRW2, "PATCH", "/api/sessions/"+id, map[string]string{"group": "x"}); rec.Code != http.StatusForbidden {
		t.Errorf("other read-write patched = %d", rec.Code)
	}
	if rec := e.do(t, callerRW2, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusForbidden {
		t.Errorf("other read-write closed = %d", rec.Code)
	}
	rec := e.do(t, callerRW, "PATCH", "/api/sessions/"+id, map[string]string{"name": "renamed", "group": "new"})
	if rec.Code != http.StatusOK {
		t.Fatalf("creator patch = %d: %s", rec.Code, rec.Body.String())
	}
	sess := decodeSessionJSON(t, rec)["session"].(map[string]any)
	if sess["name"] != "renamed" || sess["group"] != "new" {
		t.Errorf("session after patch = %v", sess)
	}
	if rec := e.do(t, callerOwner2, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Errorf("owner close = %d", rec.Code)
	}
}

func TestSessionsCreateValidation(t *testing.T) {
	e := newSessionTestEnv(t, true)
	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"empty name", map[string]string{"name": "", "vendor": "claude"}, 400},
		{"long name", map[string]string{"name": strings.Repeat("a", sessionNameMaxRunes+1), "vendor": "claude"}, 400},
		{"bad chars", map[string]string{"name": "x<script>", "vendor": "claude"}, 400},
		{"newline", map[string]string{"name": "a\nb", "vendor": "claude"}, 400},
		{"bad group", map[string]string{"name": "ok", "group": "g;rm", "vendor": "claude"}, 400},
		{"unknown vendor", map[string]string{"name": "ok", "vendor": "emacs"}, 400},
		{"shell vendor", map[string]string{"name": "ok", "vendor": "bash"}, 400},
		{"not installed", map[string]string{"name": "ok", "vendor": "goose"}, 409},
		{"ok", map[string]string{"name": "Good name (v2)", "group": "RFC-1", "vendor": "claude"}, 201},
	}
	for _, tc := range cases {
		if rec := e.do(t, callerOwner, "POST", "/api/sessions", tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if rec := e.do(t, callerOwner, "POST", "/api/sessions", []byte("{not json")); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d", rec.Code)
	}
	e.s.deps.Config.Backends.Deny = []string{"claude"}
	if rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "x", "vendor": "claude"}); rec.Code != http.StatusForbidden {
		t.Errorf("denied backend = %d, want 403", rec.Code)
	}
	if len(e.rt.started) != 1 {
		t.Errorf("started %d sessions, want only the one valid create", len(e.rt.started))
	}
	for _, spec := range e.rt.started {
		if spec.ResumeCommand != "" {
			t.Errorf("fresh create carried a resume command")
		}
	}
}

func TestSessionsLimits(t *testing.T) {
	e := newSessionTestEnv(t, true)
	t.Setenv(config.EnvSessionsMaxPerUser, "2")
	t.Setenv(config.EnvSessionsMax, "3")
	e.create(t, callerRW, "a")
	e.create(t, callerRW, "b")
	rec := e.do(t, callerRW, "POST", "/api/sessions", map[string]string{"name": "c", "vendor": "claude"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("per-user cap = %d, want 429", rec.Code)
	}
	if m := decodeSessionJSON(t, rec); m["ok"] != false || m["error"] == "" {
		t.Errorf("429 body = %v", m)
	}
	e.create(t, callerRW2, "d")
	if rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "e", "vendor": "claude"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("per-spoke cap = %d, want 429", rec.Code)
	}
}

func TestSessionsStartFailureLeavesNoRecord(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.rt.startFn = func(crux.UserSessionSpec) error { return errors.New("secret internal detail") }
	rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "x", "vendor": "claude"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("start failure = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Error("internal error detail leaked to the client")
	}
	if got := e.s.sessionStoreOrNil().list(); len(got) != 0 {
		t.Errorf("records after failed start: %v", got)
	}
}

func TestSessionsInputCapsAndAudit(t *testing.T) {
	e := newSessionTestEnv(t, true)
	t.Setenv(sessionsMaxInputBytesEnv, "32")
	id := e.create(t, callerRW, "typing")
	secret := "my-secret-passphrase"
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": secret, "submit": true}); rec.Code != http.StatusOK {
		t.Fatalf("input = %d: %s", rec.Code, rec.Body.String())
	}
	if len(e.rt.typed) != 1 || e.rt.typed[0] != secret || !e.rt.submits[0] {
		t.Errorf("typed = %v submits = %v", e.rt.typed, e.rt.submits)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": strings.Repeat("x", 33)}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized input = %d, want 413", rec.Code)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": strings.Repeat("x", 100000)}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the cap = %d, want 413", rec.Code)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty input = %d, want 400", rec.Code)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "a\x1b[2J"}); rec.Code != http.StatusBadRequest {
		t.Errorf("escape input = %d, want 400", rec.Code)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/nope/input", map[string]any{"text": "x"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session input = %d", rec.Code)
	}
	var found bool
	for _, a := range e.auditActions() {
		if strings.Contains(a.Detail, secret) {
			t.Errorf("audit entry leaked input text: %+v", a)
		}
		if a.Action == auditSessionInput {
			found = true
			if a.User != "rita" || !strings.Contains(a.Detail, "session="+id) || !strings.Contains(a.Detail, "bytes=20") {
				t.Errorf("input audit = %+v", a)
			}
		}
	}
	if !found {
		t.Error("no session_input audit entry")
	}
	// An ended session refuses input.
	if rec := e.do(t, callerOwner, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("close = %d", rec.Code)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}); rec.Code != http.StatusConflict {
		t.Errorf("input to ended session = %d, want 409", rec.Code)
	}
}

func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *sessionTestEnv) upload(t *testing.T, c sessionCaller, id, field, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ctype := multipartBody(t, field, filename, content)
	req := httptest.NewRequest("POST", "/api/sessions/"+id+"/upload", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Hive-User", c.user)
	req.Header.Set("X-Hive-Role", c.role)
	if c.verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	e.s.mux.ServeHTTP(rec, req)
	return rec
}

func TestSessionsUploadStoresSanitizedFileInsideSessionDir(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "files")
	rec := e.upload(t, callerRW, id, "file", "../../etc/pass wd;.sh", []byte("echo hi"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	path, _ := decodeSessionJSON(t, rec)["path"].(string)
	dir := filepath.Join(e.rt.WorkDir(id), sessionUploadsDirName)
	if !filepath.IsAbs(path) || filepath.Dir(path) != dir {
		t.Fatalf("path = %q, want a file directly inside %q", path, dir)
	}
	base := filepath.Base(path)
	if strings.ContainsAny(base, " ;/\\") || strings.Contains(base, "..") {
		t.Errorf("unsanitized name %q", base)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != sessionUploadFileMode {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if st.Mode().Perm()&0o111 != 0 {
		t.Error("uploaded file is executable")
	}
	var found bool
	for _, a := range e.auditActions() {
		if a.Action == auditSessionUpload {
			found = true
			if a.User != "rita" || strings.Contains(a.Detail, "passwd") {
				t.Errorf("upload audit = %+v", a)
			}
		}
	}
	if !found {
		t.Error("no session_upload audit entry")
	}
}

func TestSessionsUploadRolesCapsAndFieldName(t *testing.T) {
	e := newSessionTestEnv(t, true)
	t.Setenv(sessionsMaxUploadBytesEnv, "10")
	t.Setenv(sessionsMaxUploadsEnv, "2")
	id := e.create(t, callerRW, "files")
	for _, who := range []sessionCaller{callerRead, callerMerger, callerForgedOwn} {
		if rec := e.upload(t, who, id, "file", "a.txt", []byte("x")); rec.Code != http.StatusForbidden {
			t.Errorf("%s upload = %d, want 403", who.role, rec.Code)
		}
	}
	if rec := e.upload(t, callerRW, id, "other", "a.txt", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong field = %d, want 400", rec.Code)
	}
	if rec := e.upload(t, callerRW, id, "file", "big.bin", bytes.Repeat([]byte("x"), 11)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, want 413", rec.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.rt.WorkDir(id), sessionUploadsDirName)); len(entries) != 0 {
		t.Errorf("oversize upload left %d files behind", len(entries))
	}
	if rec := e.upload(t, callerRW, id, "file", "huge.bin", bytes.Repeat([]byte("x"), 1<<20)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body far over the cap = %d, want 413", rec.Code)
	}
	for i := 0; i < 2; i++ {
		if rec := e.upload(t, callerRW, id, "file", "ok.txt", []byte("x")); rec.Code != http.StatusOK {
			t.Fatalf("upload %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := e.upload(t, callerRW, id, "file", "third.txt", []byte("x")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("file-count cap = %d, want 429", rec.Code)
	}
}

func TestSessionsLogRedactsAndCaps(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerOwner, "log")
	e.rt.capture = "line one\nghp_abcdefghijklmnopqrstuvwxyz0123456789 token\n" + strings.Repeat("filler\n", 9000)
	rec := e.do(t, callerRead, "GET", "/api/sessions/"+id+"/log?lines=3", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("log = %d", rec.Code)
	}
	m := decodeSessionJSON(t, rec)
	if lines := m["lines"].([]any); len(lines) != 3 {
		t.Errorf("lines = %d, want 3", len(lines))
	}
	if m["status"] != "running" || m["ok"] != true {
		t.Errorf("log meta = %v", m)
	}
	rec = e.do(t, callerRead, "GET", "/api/sessions/"+id+"/log?lines=999999", nil)
	if lines := decodeSessionJSON(t, rec)["lines"].([]any); len(lines) != sessionsLogMaxLines {
		t.Errorf("uncapped log returned %d lines, want %d", len(lines), sessionsLogMaxLines)
	}
	e.rt.capture = "line one\nghp_abcdefghijklmnopqrstuvwxyz0123456789 token\n"
	rec = e.do(t, callerRead, "GET", "/api/sessions/"+id+"/log", nil)
	if strings.Contains(rec.Body.String(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("log leaked a GitHub token: %s", rec.Body.String())
	}
	if rec := e.do(t, callerRead, "GET", "/api/sessions/"+id+"/log?lines=abc", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad lines = %d", rec.Code)
	}
	if rec := e.do(t, callerRead, "GET", "/api/sessions/zzz/log", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session log = %d", rec.Code)
	}
}

func TestSessionsCloseKeepsMetadataAndAudits(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "closing")
	if rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("close = %d", rec.Code)
	}
	if len(e.rt.killed) == 0 || e.rt.killed[0] != id {
		t.Errorf("tmux not killed: %v", e.rt.killed)
	}
	rec, ok := e.s.sessionStoreOrNil().get(id)
	if !ok || rec.Status != sessionStatusEnded {
		t.Fatalf("record after close = %+v, %v", rec, ok)
	}
	// A second DELETE removes the metadata.
	if rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("second delete = %d", rec.Code)
	}
	if _, ok := e.s.sessionStoreOrNil().get(id); ok {
		t.Error("metadata not removed by deleting an ended session")
	}
	want := map[string]bool{auditSessionCreated: false, auditSessionClosed: false}
	for _, a := range e.auditActions() {
		if _, ok := want[a.Action]; ok {
			want[a.Action] = true
			if a.User != "rita" || !strings.Contains(a.Detail, id) {
				t.Errorf("audit %s = %+v", a.Action, a)
			}
		}
		if !auditSensitiveActions[a.Action] && strings.HasPrefix(a.Action, "session_") {
			t.Errorf("%s missing from the sensitive allowlist", a.Action)
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("no %s audit entry", k)
		}
	}
}

func TestSessionsRenameAudited(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "before")
	e.do(t, callerRW, "PATCH", "/api/sessions/"+id, map[string]string{"name": "after"})
	for _, a := range e.auditActions() {
		if a.Action == auditSessionRenamed && a.User == "rita" {
			return
		}
	}
	t.Error("no session_renamed audit entry")
}

func TestSessionsResume(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "resumable")
	e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/resume", nil); rec.Code != http.StatusConflict {
		t.Errorf("resume of running session = %d, want 409", rec.Code)
	}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	list := decodeSessionJSON(t, e.do(t, callerRead, "GET", "/api/sessions", nil))
	sess := list["sessions"].([]any)[0].(map[string]any)
	if sess["status"] != "ended" || sess["resumable"] != true {
		t.Errorf("ended session view = %v", sess)
	}
	if rec := e.do(t, callerRead, "POST", "/api/sessions/"+id+"/resume", nil); rec.Code != http.StatusForbidden {
		t.Errorf("read resume = %d", rec.Code)
	}
	rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	last := e.rt.started[len(e.rt.started)-1]
	if last.ID != id || last.ResumeCommand != "claude --resume abc" {
		t.Errorf("resume start = %+v", last)
	}
	if got := decodeSessionJSON(t, rec)["session"].(map[string]any)["status"]; got != "running" {
		t.Errorf("status after resume = %v", got)
	}

	// A vendor without resume support yields 409.
	id2 := e.create(t, callerRW, "codex")
	e.do(t, callerOwner, "PATCH", "/api/sessions/"+id2, map[string]string{"name": "codex"})
	if _, _, err := e.s.sessionStoreOrNil().update(id2, func(r *sessionRecord) { r.Vendor, r.Status = "codex", sessionStatusEnded }); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id2+"/resume", nil); rec.Code != http.StatusConflict {
		t.Errorf("unsupported vendor resume = %d, want 409", rec.Code)
	}
}

func TestSessionsStoreRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := openSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := sessionRecord{ID: "aabbccddeeff", Name: "n", Group: "g", Vendor: "claude", Creator: "u",
		CreatedAt: "2026-01-01T00:00:00Z", LastActivity: "2026-01-01T00:00:00Z", Status: sessionStatusRunning, ResumeCommand: "claude --resume x"}
	if err := st.put(rec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, sessionsStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != sessionsStoreFileMode {
		t.Errorf("store mode = %v, want 0600", info.Mode().Perm())
	}
	reopened, err := openSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.get(rec.ID)
	if !ok || got != rec {
		t.Errorf("round trip = %+v, %v; want %+v", got, ok, rec)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".sessions-*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "missing", sessionsStoreFileName)); err == nil {
		t.Fatal("sanity")
	}
	empty, err := openSessionStore(filepath.Join(dir, "missing"))
	if err != nil || len(empty.list()) != 0 {
		t.Errorf("missing store = %v, %v", empty.list(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionsStoreFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSessionStore(dir); err == nil {
		t.Error("corrupt store did not report an error")
	}
}

func TestSessionsResumeCommandNeverExposed(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "x")
	e.rt.resume[id] = crux.UserSessionResume{Command: "claude --resume abc-secret-id"}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	rec := e.do(t, callerRead, "GET", "/api/sessions", nil)
	if strings.Contains(rec.Body.String(), "abc-secret-id") || strings.Contains(rec.Body.String(), "resume_command") {
		t.Errorf("resume command leaked to the browser: %s", rec.Body.String())
	}
}

func TestSessionsReconcileMarksVanishedSessionsEnded(t *testing.T) {
	e := newSessionTestEnv(t, true)
	live := e.create(t, callerRW, "live")
	gone := e.create(t, callerRW, "gone")
	delete(e.rt.alive, gone) // tmux did not survive the restart
	list := decodeSessionJSON(t, e.do(t, callerRead, "GET", "/api/sessions", nil))
	status := map[string]string{}
	for _, v := range list["sessions"].([]any) {
		m := v.(map[string]any)
		status[m["id"].(string)] = m["status"].(string)
	}
	if status[live] != "running" || status[gone] != "ended" {
		t.Errorf("statuses = %v", status)
	}
	// And it persists.
	if rec, _ := e.s.sessionStoreOrNil().get(gone); rec.Status != sessionStatusEnded || rec.EndedAt == "" {
		t.Errorf("persisted = %+v", rec)
	}
}

func TestSessionsAttachedCountSurfaced(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "x")
	e.rt.alive[id] = crux.UserSessionProbe{Exists: true, Attached: 3, Activity: time.Now()}
	sess := decodeSessionJSON(t, e.do(t, callerRead, "GET", "/api/sessions", nil))["sessions"].([]any)[0].(map[string]any)
	if sess["attached"] != float64(3) || sess["tmux_name"] != "hive-sess-"+id {
		t.Errorf("session = %v", sess)
	}
	for _, k := range []string{"id", "name", "group", "vendor", "creator", "created_at", "last_activity", "status"} {
		if _, ok := sess[k]; !ok {
			t.Errorf("session missing %s", k)
		}
	}
}

func TestSessionsIdleSweeper(t *testing.T) {
	e := newSessionTestEnv(t, true)
	idle := e.create(t, callerRW, "idle")
	busy := e.create(t, callerRW, "attached")
	old := time.Now().Add(-100 * time.Hour)
	for _, id := range []string{idle, busy} {
		if _, _, err := e.s.sessionStoreOrNil().update(id, func(r *sessionRecord) { r.LastActivity = old.UTC().Format(time.RFC3339) }); err != nil {
			t.Fatal(err)
		}
		e.rt.alive[id] = crux.UserSessionProbe{Exists: true, Activity: old}
	}
	e.rt.alive[busy] = crux.UserSessionProbe{Exists: true, Attached: 1, Activity: old}
	e.s.sweepSessions(time.Now())
	if rec, _ := e.s.sessionStoreOrNil().get(idle); rec.Status != sessionStatusEnded {
		t.Errorf("idle session status = %s, want ended", rec.Status)
	}
	if rec, _ := e.s.sessionStoreOrNil().get(busy); rec.Status != sessionStatusRunning {
		t.Errorf("attached session was closed: %s", rec.Status)
	}
	var closed bool
	for _, a := range e.auditActions() {
		if a.Action == auditSessionClosed && a.User == "system" && strings.Contains(a.Detail, "reason=idle") {
			closed = true
		}
	}
	if !closed {
		t.Error("no idle session_closed audit entry")
	}
	// Ended metadata older than the retention window is pruned.
	if _, _, err := e.s.sessionStoreOrNil().update(idle, func(r *sessionRecord) {
		r.EndedAt = time.Now().Add(-2 * sessionEndedRetention).UTC().Format(time.RFC3339)
	}); err != nil {
		t.Fatal(err)
	}
	e.s.sweepSessions(time.Now())
	if _, ok := e.s.sessionStoreOrNil().get(idle); ok {
		t.Error("expired ended session was not pruned")
	}
}

func TestSessionLabelValidation(t *testing.T) {
	good := []string{"a", "My session", "RFC-12 (draft)", "héllo", "a/b", "x_y.z+1"}
	for _, v := range good {
		if err := validateSessionLabel("name", v, sessionNameMaxRunes, true); err != nil {
			t.Errorf("%q rejected: %v", v, err)
		}
	}
	bad := []string{" lead", "trail ", "a<b", "a\"b", "a\tb", "a\\b", "a`b", "a$b", "a;b", "a|b", "a*b"}
	for _, v := range bad {
		if err := validateSessionLabel("name", v, sessionNameMaxRunes, true); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if err := validateSessionLabel("group", "", sessionGroupMaxRunes, false); err != nil {
		t.Errorf("empty optional group rejected: %v", err)
	}
}

func TestSanitizeUploadName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":         "report.pdf",
		"../../etc/passwd":   "passwd",
		`C:\dir\file name.t`: "file_name.t",
		".hidden":            "hidden",
		"":                   "upload",
		"..":                 "upload",
		"a b;c.txt":          "a_b_c.txt",
	}
	for in, want := range cases {
		if got := sanitizeUploadName(in); got != want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeUploadName(strings.Repeat("a", 500)); len(got) > sessionFileNameMax {
		t.Errorf("name not bounded: %d", len(got))
	}
}

// Terminal handoff with a session id.
func TestTerminalHandoffWithSession(t *testing.T) {
	t.Setenv("HIVE_TERMINAL_KEY", "test-terminal-key-0123456789")
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.HiveID = "hosted-alpha"
	if terminalassert.SigningKey() == "" {
		t.Skip("terminal signing key not derivable in this environment")
	}
	id := e.create(t, callerRW, "attach")

	post := func(c sessionCaller, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest("POST", terminalHandoffPath, bytes.NewReader(raw))
		req.Header.Set("X-Hive-User", c.user)
		req.Header.Set("X-Hive-Role", c.role)
		rec := httptest.NewRecorder()
		e.s.handleCreateTerminalHandoff(rec, req)
		return rec
	}
	rec := post(callerRW, map[string]string{"session": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("handoff for session = %d: %s", rec.Code, rec.Body.String())
	}
	m := decodeSessionJSON(t, rec)
	if m["code"] == "" || m["expires_in"] != "60" {
		t.Errorf("handoff response shape changed: %v", m)
	}
	if rec := post(callerRW, nil); rec.Code != http.StatusOK {
		t.Errorf("handoff without body = %d, want unchanged behaviour", rec.Code)
	}
	if rec := post(callerRead, map[string]string{"session": id}); rec.Code != http.StatusForbidden {
		t.Errorf("read role handoff = %d, want 403", rec.Code)
	}
	if rec := post(callerRW, map[string]string{"session": "ffffffffffff"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session handoff = %d, want 404", rec.Code)
	}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	if rec := post(callerRW, map[string]string{"session": id}); rec.Code != http.StatusConflict {
		t.Errorf("ended session handoff = %d, want 409", rec.Code)
	}
	e.s.deps.Config.Dashboard.Sessions.Enabled = false
	if rec := post(callerRW, map[string]string{"session": id}); rec.Code != http.StatusNotFound {
		t.Errorf("disabled handoff = %d, want 404", rec.Code)
	}
}

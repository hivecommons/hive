package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/terminalassert"
)

// fakeSessionRuntime is a deterministic SessionRuntime: no tmux, no processes.
type fakeSessionRuntime struct {
	mu       sync.Mutex
	vendors  []crux.UserSessionVendor
	workDir  string
	alive    map[string]crux.UserSessionProbe
	started  []crux.UserSessionSpec
	killed   []string
	typed    []string
	submits  []bool
	capture  string
	captures int // times Capture was called
	resume   map[string]crux.UserSessionResume
	startFn  func(crux.UserSessionSpec) error
	removed  []string
	// removeErr, when set, makes Remove fail (the files could not be deleted).
	removeErr error
	// removeHook, when set, replaces Remove's own work and runs without the
	// fake's lock, so a test can hold a removal open; removeCalls counts
	// every Remove; gone holds the ids Removed reports as having nothing left
	// on disk (set by a successful Remove, or by a test).
	removeHook  func(id string) error
	removeCalls int
	gone        map[string]bool
	// resumeDirs records the workDir each ResumeInfo call was given.
	resumeDirs []string
	// detached records each DetachWritableClients call (keyboard hand-over).
	detached []string
	// inboxFiles records each StoreInboxFile call (Inbox seeds).
	inboxFiles []fakeInboxFile
	// swept records the ids each SweepWorktrees call found gone, given the
	// ids in worktreeIDs (sessions with a worktree on disk).
	worktreeIDs []string
	swept       [][]string
	// secured counts SecureWorkspaces calls (the boot lock of shared clones).
	secured int
	// startup scripts StartupStep (api_sessions_autostart_test.go); nil means
	// the CLI is at its input prompt from the first read.
	startup *fakeStartup
}

// fakeInboxFile is one StoreInboxFile call.
type fakeInboxFile struct {
	id, workspace, name string
	data                []byte
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
func (f *fakeSessionRuntime) WorkspaceDir(repo string) string {
	if repo == "" || !strings.Contains(repo, "/") {
		return ""
	}
	return filepath.Join(f.workDir, "workspace", repo)
}
func (f *fakeSessionRuntime) SessionWorkspaceDir(id, repo string) string {
	if id == "" || f.WorkspaceDir(repo) == "" {
		return ""
	}
	return filepath.Join(f.workDir, "workspace", ".sessions", id, repo)
}
func (f *fakeSessionRuntime) SweepWorktrees(live func(id string) bool) {
	f.mu.Lock()
	ids := append([]string(nil), f.worktreeIDs...)
	f.mu.Unlock()
	var gone []string
	for _, id := range ids {
		if !live(id) {
			gone = append(gone, id)
		}
	}
	f.mu.Lock()
	f.swept = append(f.swept, gone)
	f.mu.Unlock()
}
func (f *fakeSessionRuntime) SecureWorkspaces() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secured++
}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures++
	return f.capture, nil
}

// captureCount is how many times the pane has been read so far.
func (f *fakeSessionRuntime) captureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captures
}

// setCapture replaces what Capture returns (the pane's last lines).
func (f *fakeSessionRuntime) setCapture(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capture = text
}

// typedTexts copies the texts delivered so far.
func (f *fakeSessionRuntime) typedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

func (f *fakeSessionRuntime) DetachWritableClients(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, id)
	return nil
}

// detachedIDs copies the sessions whose writable clients were detached.
func (f *fakeSessionRuntime) detachedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.detached...)
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
func (f *fakeSessionRuntime) Remove(id string) error {
	f.mu.Lock()
	f.removeCalls++
	hook := f.removeHook
	f.mu.Unlock()
	if hook != nil {
		return hook(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	delete(f.alive, id)
	f.markGoneLocked(id)
	return os.RemoveAll(filepath.Join(f.workDir, id))
}

func (f *fakeSessionRuntime) markGoneLocked(id string) {
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[id] = true
}

// markGone makes Removed report that nothing of id is left on disk.
func (f *fakeSessionRuntime) markGone(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markGoneLocked(id)
}

// setRemoveErr makes Remove fail with err (nil: succeed again).
func (f *fakeSessionRuntime) setRemoveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeErr = err
}

// removeCount is how many times Remove has been called.
func (f *fakeSessionRuntime) removeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removeCalls
}

// removedIDs copies the ids whose files Remove deleted.
func (f *fakeSessionRuntime) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *fakeSessionRuntime) Removed(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gone[id]
}

// StoreUpload mimics the real runtime's contract: an exclusive 0600 file under
// <work dir>/uploads, a file-count cap and a byte cap with the typed errors.
func (f *fakeSessionRuntime) StoreUpload(id, fileName string, src io.Reader, maxBytes int64, maxFiles int) (string, int64, error) {
	dir := filepath.Join(f.WorkDir(id), sessionUploadsDirName)
	if err := os.MkdirAll(dir, sessionUploadDirMode); err != nil {
		return "", 0, err
	}
	if entries, _ := os.ReadDir(dir); len(entries) >= maxFiles {
		return "", 0, crux.ErrUserSessionUploadLimit
	}
	b, err := io.ReadAll(io.LimitReader(src, maxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if int64(len(b)) > maxBytes {
		return "", int64(len(b)), crux.ErrUserSessionUploadTooLarge
	}
	dest := filepath.Join(dir, fileName)
	if err := os.WriteFile(dest, b, sessionUploadFileMode); err != nil {
		return "", 0, err
	}
	return dest, int64(len(b)), nil
}

// StoreInboxFile mimics the real runtime: <cwd>/.hive/inbox/<name>, 0640,
// cwd = workspace or the session's own work dir.
func (f *fakeSessionRuntime) StoreInboxFile(id, workspace, fileName string, data []byte) (string, error) {
	f.mu.Lock()
	f.inboxFiles = append(f.inboxFiles, fakeInboxFile{id: id, workspace: workspace, name: fileName, data: append([]byte(nil), data...)})
	f.mu.Unlock()
	cwd := workspace
	if cwd == "" {
		cwd = f.WorkDir(id)
	}
	dir := filepath.Join(cwd, ".hive", "inbox")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, fileName)
	return dest, os.WriteFile(dest, data, 0o640)
}

// storedInboxFiles copies the StoreInboxFile calls so far.
func (f *fakeSessionRuntime) storedInboxFiles() []fakeInboxFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeInboxFile(nil), f.inboxFiles...)
}

func (f *fakeSessionRuntime) ResumeInfo(id, vendor, workDir string) (crux.UserSessionResume, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeDirs = append(f.resumeDirs, workDir)
	r, ok := f.resume[id]
	return r, ok
}

type sessionTestEnv struct {
	s  *Server
	rt *fakeSessionRuntime
}

func newSessionTestEnv(t *testing.T, enabled bool) *sessionTestEnv {
	t.Helper()
	return newSessionTestEnvAt(t, enabled, t.TempDir())
}

// newSessionTestEnvInDir is an enabled test env whose sessions store lives in
// dir, so a test can open a second "process" on the first one's store.
func newSessionTestEnvInDir(t *testing.T, dir string) *sessionTestEnv {
	t.Helper()
	return newSessionTestEnvAt(t, true, dir)
}

func newSessionTestEnvAt(t *testing.T, enabled bool, dir string) *sessionTestEnv {
	t.Helper()
	t.Setenv(sessionsDirEnv, dir)
	t.Setenv(config.EnvSessionsEnabled, "")
	for _, e := range []string{config.EnvSessionsMax, config.EnvSessionsMaxPerUser, config.EnvSessionsIdleTTLHours, config.EnvSessionsWorkspaceRepo,
		sessionsMaxInputBytesEnv, sessionsMaxUploadBytesEnv, sessionsMaxUploadsEnv} {
		t.Setenv(e, "")
	}
	rt := newFakeSessionRuntime(t)
	cfg := &config.Config{}
	cfg.Dashboard.Sessions.Enabled = &enabled
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
	return e.createWith(t, c, name, false)
}

// createShared makes a session every role can see and every writer can use.
func (e *sessionTestEnv) createShared(t *testing.T, c sessionCaller, name string) string {
	t.Helper()
	return e.createWith(t, c, name, true)
}

// createWith posts a create, which answers 202 with the session `preparing`
// (the start runs in the background), and waits for that start to finish so
// the caller gets a RUNNING session as the synchronous API used to give.
func (e *sessionTestEnv) createWith(t *testing.T, c sessionCaller, name string, shared bool) string {
	t.Helper()
	rec := e.do(t, c, "POST", "/api/sessions", map[string]any{"name": name, "group": "rfc", "vendor": "claude", "shared": shared})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sess, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)
	id, _ := sess["id"].(string)
	if id == "" {
		t.Fatalf("no id in %s", rec.Body.String())
	}
	if sess["status"] != sessionStatusPreparing {
		t.Fatalf("create answered status %v, want %s", sess["status"], sessionStatusPreparing)
	}
	if got := e.waitSettled(t, id); got.Status != sessionStatusRunning {
		t.Fatalf("session %s settled as %s (%s), want running", id, got.Status, got.Error)
	}
	return id
}

// sessionTestSettleTimeout bounds the wait for a background start in tests.
const sessionTestSettleTimeout = 5 * time.Second

// waitSessionsFor polls cond until it holds or the settle timeout passes.
func waitSessionsFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.Eventually(t, sessionTestSettleTimeout, cond, "timed out waiting for %s", what)
}

// waitSettled waits until the background start of id has moved the record out
// of `preparing` and returns the record.
func (e *sessionTestEnv) waitSettled(t *testing.T, id string) sessionRecord {
	t.Helper()
	var rec sessionRecord
	waitSessionsFor(t, "session "+id+" to leave preparing", func() bool {
		var ok bool
		rec, ok = e.s.sessionStoreOrNil().get(id)
		return ok && rec.Status != sessionStatusPreparing
	})
	return rec
}

// startedSpecs copies the specs the fake runtime was asked to start.
func (f *fakeSessionRuntime) startedSpecs() []crux.UserSessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]crux.UserSessionSpec(nil), f.started...)
}

func (e *sessionTestEnv) auditActions() []AuditEntry { return e.s.audit.Recent(0) }

// waitStartedAudit waits for the session_started audit of a background start
// and returns its detail. The record leaves `preparing` before that audit is
// written, so a waitSettled caller must not read the audit log just once.
func (e *sessionTestEnv) waitStartedAudit(t *testing.T, outcome string) string {
	t.Helper()
	var detail string
	waitSessionsFor(t, "the session_started audit", func() bool {
		for _, a := range e.auditActions() {
			if a.Action == auditSessionStarted {
				detail = a.Detail
			}
		}
		return strings.Contains(detail, "outcome="+outcome)
	})
	return detail
}

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
	// This test exercises authorization, not CLI onboarding. Keep the fake pane
	// idle so accepted inputs drain promptly, then join both kinds of workers
	// before TempDir removes the session store.
	t.Setenv(config.EnvSessionsAutostart, "false")
	e := newSessionTestEnv(t, true)
	fastSessionQueue(t, e)
	e.rt.setCapture(claudeIdlePane)
	t.Cleanup(func() {
		waitSessionsFor(t, "session starts to finish", func() bool {
			e.s.sessionsMu.Lock()
			defer e.s.sessionsMu.Unlock()
			return len(e.s.sessionsPreparing) == 0
		})
		waitSessionsFor(t, "session input workers to stop", func() bool {
			e.s.sessionQueuesMu.Lock()
			defer e.s.sessionQueuesMu.Unlock()
			for _, q := range e.s.sessionQueues {
				if q.running {
					return false
				}
			}
			return true
		})
	})
	id := e.createShared(t, callerOwner, "owner session")
	post := map[string]string{"name": "n", "group": "g", "vendor": "claude"}

	type row struct {
		who                  sessionCaller
		create, input, patch int // expected status codes
	}
	// Create and input answer 202 (the workspace is prepared, and the text delivered, in the background); create/input need owner or read-write; patch needs creator or owner and
	// every non-owner here is not the creator of the owner's session.
	rows := []row{
		{callerOwner, 202, 202, 200},
		{callerRW, 202, 202, 403},
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
	// A SHARED session is readable by every role, including read.
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
	id := e.createShared(t, callerRW, "ritas session")
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
		{"ok", map[string]string{"name": "Good name (v2)", "group": "RFC-1", "vendor": "claude"}, http.StatusAccepted},
	}
	for _, tc := range cases {
		if rec := e.do(t, callerOwner, "POST", "/api/sessions", tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if rec := e.do(t, callerOwner, "POST", "/api/sessions", []byte("{not json")); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d", rec.Code)
	}
	waitSessionsFor(t, "the one valid create to start", func() bool { return len(e.rt.startedSpecs()) >= 1 })
	// Let the valid create's background worker finish before mutating the
	// shared config, which that worker reads.
	for _, spec := range e.rt.startedSpecs() {
		e.waitSettled(t, spec.ID)
	}
	e.s.deps.Config.Backends.Deny = []string{"claude"}
	if rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "x", "vendor": "claude"}); rec.Code != http.StatusForbidden {
		t.Errorf("denied backend = %d, want 403", rec.Code)
	}
	if n := len(e.rt.startedSpecs()); n != 1 {
		t.Errorf("started %d sessions, want only the one valid create", n)
	}
	for _, spec := range e.rt.startedSpecs() {
		// Start records the spec before the background worker persists its
		// outcome. Wait for that write before TempDir cleanup removes the store.
		if got := e.waitSettled(t, spec.ID); got.Status != sessionStatusRunning {
			t.Errorf("session %s settled as %s (%s), want running", spec.ID, got.Status, got.Error)
		}
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

// A start that fails in the background leaves a `failed` record carrying the
// reason (tokens redacted) so the tab can show it; its sandbox is removed, and
// DELETE on it removes the record. The failure is audited with its outcome.
func TestSessionsStartFailureMarksRecordFailed(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.rt.startFn = func(crux.UserSessionSpec) error {
		return errors.New("preparing session workspace: git clone acme/widgets: exit 128 (token ghp_abcdefghijklmnopqrstuvwxyz0123456789)")
	}
	rec := e.do(t, callerOwner, "POST", "/api/sessions", map[string]string{"name": "x", "vendor": "claude"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)["id"].(string)
	got := e.waitSettled(t, id)
	if got.Status != sessionStatusFailed || !strings.Contains(got.Error, "git clone acme/widgets") || got.EndedAt == "" {
		t.Fatalf("record after failed start = %+v", got)
	}
	if strings.Contains(got.Error, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Error("a token in the start error reached the record unredacted")
	}
	waitSessionsFor(t, "sandbox removal", func() bool {
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		return len(e.rt.removed) == 1
	})
	list := decodeSessionJSON(t, e.do(t, callerOwner, "GET", "/api/sessions", nil))
	view, _ := list["sessions"].([]any)[0].(map[string]any)
	if view["status"] != sessionStatusFailed || view["error"] != got.Error || view["message"] != "" {
		t.Errorf("failed view = %v", view)
	}
	e.waitStartedAudit(t, sessionStatusFailed)
	if rec := e.do(t, callerOwner, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete failed session = %d: %s", rec.Code, rec.Body.String())
	}
	if got := e.s.sessionStoreOrNil().list(); len(got) != 0 {
		t.Errorf("records after deleting the failed session: %v", got)
	}
}

// The create answers before the start finishes: the record is `preparing`
// with a message, listed as such, counted against the caps, refused for input
// and close, and moves to running (audited) once the runtime returns.
func TestSessionsCreateAnswersBeforeTheStartFinishes(t *testing.T) {
	e := newSessionTestEnv(t, true)
	e.s.deps.Config.Project.Org, e.s.deps.Config.Project.PrimaryRepo = "acme", "widgets"
	release := make(chan struct{})
	e.rt.startFn = func(crux.UserSessionSpec) error { <-release; return nil }
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- e.do(t, callerRW, "POST", "/api/sessions", map[string]string{"name": "slow", "vendor": "claude"})
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(sessionTestSettleTimeout):
		t.Fatal("create blocked on the start")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sess, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)
	id, _ := sess["id"].(string)
	if sess["status"] != sessionStatusPreparing || !strings.Contains(sess["message"].(string), "cloning acme/widgets") {
		t.Fatalf("create answered %v", sess)
	}
	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	views, _ := list["sessions"].([]any)
	if len(views) != 1 || views[0].(map[string]any)["status"] != sessionStatusPreparing {
		t.Fatalf("list while preparing = %v", list["sessions"])
	}
	if got := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "x"}).Code; got != http.StatusConflict {
		t.Errorf("input while preparing = %d, want 409", got)
	}
	if got := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil).Code; got != http.StatusConflict {
		t.Errorf("close while preparing = %d, want 409", got)
	}
	t.Setenv(config.EnvSessionsMaxPerUser, "1")
	if got := e.do(t, callerRW, "POST", "/api/sessions", map[string]string{"name": "second", "vendor": "claude"}).Code; got != http.StatusTooManyRequests {
		t.Errorf("a preparing session does not count against the cap: %d", got)
	}
	close(release)
	got := e.waitSettled(t, id)
	if got.Status != sessionStatusRunning || got.Message != "" || got.Error != "" {
		t.Fatalf("record after start = %+v", got)
	}
	e.waitStartedAudit(t, sessionStatusRunning)
}

// Two creates while the first start is still running are both accepted as
// preparing and both finish; the runtime serialises the shared clone (that
// invariant is asserted on the runtime itself in crux/agent).
func TestSessionsConcurrentCreatesBothPrepare(t *testing.T) {
	e := newSessionTestEnv(t, true)
	release := make(chan struct{})
	e.rt.startFn = func(crux.UserSessionSpec) error { <-release; return nil }
	var ids []string
	for _, name := range []string{"one", "two"} {
		rec := e.do(t, callerRW, "POST", "/api/sessions", map[string]string{"name": name, "vendor": "claude"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create %s = %d: %s", name, rec.Code, rec.Body.String())
		}
		id, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)["id"].(string)
		ids = append(ids, id)
	}
	for _, id := range ids {
		if rec, _ := e.s.sessionStoreOrNil().get(id); rec.Status != sessionStatusPreparing {
			t.Errorf("%s = %s while the starts run, want preparing", id, rec.Status)
		}
	}
	close(release)
	for _, id := range ids {
		if got := e.waitSettled(t, id); got.Status != sessionStatusRunning {
			t.Errorf("%s settled as %s (%s)", id, got.Status, got.Error)
		}
	}
	if n := len(e.rt.startedSpecs()); n != 2 {
		t.Errorf("started %d, want 2", n)
	}
}

// A `preparing` record with no start behind it (the hive restarted mid-clone)
// is marked failed with a reason on the next list, never left spinning.
func TestSessionsPreparingLeftoverFromRestartIsFailed(t *testing.T) {
	e := newSessionTestEnv(t, true)
	st := e.s.sessionStoreOrNil()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := st.put(sessionRecord{ID: "abcdef012345", Name: "orphan", Vendor: "claude", Creator: callerRW.user, CreatedAt: now, LastActivity: now, Status: sessionStatusPreparing, UIDSlot: 1, Message: "preparing workspace"}); err != nil {
		t.Fatal(err)
	}
	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	view, _ := list["sessions"].([]any)[0].(map[string]any)
	if view["status"] != sessionStatusFailed || view["error"] != sessionRestartedWhilePreparing {
		t.Errorf("leftover view = %v", view)
	}
}

// A resume is asynchronous too; when its start fails the session goes back to
// ended, with the reason, and stays resumable.
func TestSessionsResumeFailureKeepsSessionEnded(t *testing.T) {
	e := newSessionTestEnv(t, true)
	id := e.create(t, callerRW, "resumable")
	e.rt.resume[id] = crux.UserSessionResume{SessionID: "abc", Command: "claude --resume abc"}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	e.rt.startFn = func(crux.UserSessionSpec) error { return errors.New("spoke is out of session uids") }
	rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/resume", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	got := e.waitSettled(t, id)
	if got.Status != sessionStatusEnded || !strings.Contains(got.Error, "out of session uids") || got.ResumeCommand == "" {
		t.Fatalf("record after failed resume = %+v", got)
	}
	e.rt.mu.Lock()
	removed := len(e.rt.removed)
	e.rt.mu.Unlock()
	if removed != 0 {
		t.Error("a failed resume removed the session's files")
	}
	list := decodeSessionJSON(t, e.do(t, callerRW, "GET", "/api/sessions", nil))
	view, _ := list["sessions"].([]any)[0].(map[string]any)
	if view["resumable"] != true || view["error"] != got.Error {
		t.Errorf("view after failed resume = %v", view)
	}
}

func TestSessionsInputCapsAndAudit(t *testing.T) {
	e := newSessionTestEnv(t, true)
	t.Setenv(sessionsMaxInputBytesEnv, "32")
	fastSessionQueue(t, e)
	id := e.create(t, callerRW, "typing")
	e.rt.setCapture(claudeIdlePane)
	secret := "my-secret-passphrase"
	// Input is queued (202) and delivered by the session's worker once the
	// pane is idle (it is, here), then audited as the sender.
	if rec := e.do(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": secret, "submit": true}); rec.Code != http.StatusAccepted {
		t.Fatalf("input = %d: %s", rec.Code, rec.Body.String())
	}
	waitSessionsFor(t, "queued input to be delivered", func() bool { return len(e.rt.typedTexts()) == 1 })
	e.rt.mu.Lock()
	typed, submits := append([]string(nil), e.rt.typed...), append([]bool(nil), e.rt.submits...)
	e.rt.mu.Unlock()
	if len(typed) != 1 || typed[0] != secret || !submits[0] {
		t.Errorf("typed = %v submits = %v", typed, submits)
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
	id := e.createShared(t, callerOwner, "log")
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
	// A second DELETE removes the files and then the metadata, in the
	// background.
	if rec := e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("second delete = %d", rec.Code)
	}
	e.waitDeleted(t, id)
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
	id := e.createShared(t, callerRW, "resumable")
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
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeSessionJSON(t, rec)["session"].(map[string]any)["status"]; got != sessionStatusPreparing {
		t.Errorf("status answered by resume = %v, want preparing", got)
	}
	if got := e.waitSettled(t, id); got.Status != sessionStatusRunning || got.EndedAt != "" {
		t.Fatalf("record after resume = %+v", got)
	}
	started := e.rt.startedSpecs()
	last := started[len(started)-1]
	if last.ID != id || last.ResumeCommand != "claude --resume abc" {
		t.Errorf("resume start = %+v", last)
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
	id := e.createShared(t, callerRW, "x")
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
	list := decodeSessionJSON(t, e.do(t, callerOwner, "GET", "/api/sessions", nil))
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
	sess := decodeSessionJSON(t, e.do(t, callerOwner, "GET", "/api/sessions", nil))["sessions"].([]any)[0].(map[string]any)
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
	e.waitDeleted(t, idle)
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
	t.Setenv(ttydAttachKeyEnv, "test-attach-key")
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
	// The read role cannot see a private session (404, the id is not disclosed)
	// but may watch a shared one read-only, so a session-bound code is minted.
	if rec := post(callerRead, map[string]string{"session": id}); rec.Code != http.StatusNotFound {
		t.Errorf("read role handoff on a private session = %d, want 404", rec.Code)
	}
	if rec := post(callerRead, nil); rec.Code != http.StatusForbidden {
		t.Errorf("read role agent-terminal handoff = %d, want 403", rec.Code)
	}
	e.do(t, callerRW, "PATCH", "/api/sessions/"+id, map[string]any{"shared": true})
	if rec := post(callerRead, map[string]string{"session": id}); rec.Code != http.StatusOK {
		t.Errorf("read role handoff on a shared session = %d, want 200 (read-only viewer)", rec.Code)
	}
	if rec := post(callerRW, map[string]string{"session": "ffffffffffff"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session handoff = %d, want 404", rec.Code)
	}
	e.do(t, callerRW, "DELETE", "/api/sessions/"+id, nil)
	if rec := post(callerRW, map[string]string{"session": id}); rec.Code != http.StatusConflict {
		t.Errorf("ended session handoff = %d, want 409", rec.Code)
	}
	off := false
	e.s.deps.Config.Dashboard.Sessions.Enabled = &off
	if rec := post(callerRW, map[string]string{"session": id}); rec.Code != http.StatusNotFound {
		t.Errorf("disabled handoff = %d, want 404", rec.Code)
	}
}

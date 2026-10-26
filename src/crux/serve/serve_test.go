package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// fakeRuntime answers the Runtime methods the server uses from canned state
// and records what it was asked. Anything else panics through the embedded
// nil interface, so a handler reaching past its documented methods fails
// loudly.
type fakeRuntime struct {
	crux.Runtime

	mu        sync.Mutex
	procs     map[string]*crux.Process
	buffer    map[string][][]string
	pane      map[string][]string
	fullLog   map[string]string
	header    map[string]string
	kicks     []string
	startErr  error
	outputErr error
	observer  func(string, string, string)
	startCtx  context.Context
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		procs:   map[string]*crux.Process{},
		buffer:  map[string][][]string{},
		pane:    map[string][]string{},
		fullLog: map[string]string{},
		header:  map[string]string{},
	}
}

func (f *fakeRuntime) add(name, id string, state crux.ProcessState) *crux.Process {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &crux.Process{Name: name, ID: id, State: state, Config: config.AgentConfig{ID: id, Backend: "claude", Model: "opus"}}
	f.procs[name] = p
	return p
}

// AddAgent matches the manager: a no-op for a name it already holds (and,
// standing in for the ACMM gate, for the name "refused").
func (f *fakeRuntime) AddAgent(name string, cfg config.AgentConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.procs[name]; exists || name == "refused" {
		return
	}
	f.procs[name] = &crux.Process{Name: name, ID: cfg.ID, Config: cfg, State: crux.StateStopped}
}

func (f *fakeRuntime) Start(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCtx = ctx
	if f.startErr != nil {
		return f.startErr
	}
	p, ok := f.procs[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	p.State = crux.StateRunning
	now := time.Now()
	p.StartedAt = &now
	return nil
}

func (f *fakeRuntime) Stop(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	if p.State == crux.StateFailed {
		return errors.New("tmux kill-session: exit status 1")
	}
	p.State = crux.StateStopped
	return nil
}

func (f *fakeRuntime) AllStatuses() map[string]*crux.Process {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*crux.Process, len(f.procs))
	for k, v := range f.procs {
		out[k] = clone(v)
	}
	out["nil-entry"] = nil
	return out
}

func (f *fakeRuntime) GetStatus(name string) (*crux.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	return clone(p), nil
}

// clone copies the fields the server projects; crux.Process carries a mutex
// and cannot be copied whole.
func clone(p *crux.Process) *crux.Process {
	return &crux.Process{
		Name: p.Name, ID: p.ID, Config: p.Config, State: p.State, PID: p.PID,
		StartedAt: p.StartedAt, LastKick: p.LastKick, Paused: p.Paused,
		PausedReason: p.PausedReason, PausedTrigger: p.PausedTrigger, PausedBy: p.PausedBy,
		ModelOverride: p.ModelOverride, BackendOverride: p.BackendOverride,
		RestartCount: p.RestartCount, LastRestartReason: p.LastRestartReason,
		KickRefused: p.KickRefused, KickRefusalReason: p.KickRefusalReason,
		KicksUndeliverable: p.KicksUndeliverable, BusyCondition: p.BusyCondition,
		StartFailureClass: p.StartFailureClass,
	}
}

func (f *fakeRuntime) ResolveAgent(nameOrID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, p := range f.procs {
		if p.ID == nameOrID {
			return name
		}
	}
	return nameOrID
}

func (f *fakeRuntime) GetOutput(name string, lines int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	out := f.pane[name]
	if lines < len(out) {
		out = out[len(out)-lines:]
	}
	return out, nil
}

func (f *fakeRuntime) GetBufferOutput(name string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	answers := f.buffer[name]
	if len(answers) == 0 {
		return nil, nil
	}
	cur := answers[0]
	if len(answers) > 1 {
		f.buffer[name] = answers[1:]
	}
	return cur, nil
}

func (f *fakeRuntime) CaptureFullLog(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return "", f.outputErr
	}
	return f.fullLog[name], nil
}

func (f *fakeRuntime) FullLogHeader(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.header[name]
}

func (f *fakeRuntime) SendKick(name, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	if p.State != crux.StateRunning {
		return errors.New("agent is not running")
	}
	f.kicks = append(f.kicks, message)
	return nil
}

func (f *fakeRuntime) SetKickObserver(fn func(agentName, event, detail string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observer = fn
}

func (f *fakeRuntime) Kicks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kicks...)
}

// shortSocketDir returns a directory short enough for a unix socket on every
// platform (macOS caps sun_path at 104 bytes and t.TempDir can exceed it).
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// unixClient is an http.Client that dials the socket at path.
func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// served starts a Server over a real unix socket (and TCP when listen is
// set) and returns it with its runtime, socket path and TCP address.
type served struct {
	t      *testing.T
	rt     *fakeRuntime
	srv    *Server
	socket string
	tcp    string
	client *http.Client
	cancel context.CancelFunc
	done   chan error
}

func startServer(t *testing.T, token string, tcp bool) *served {
	return startServerPolling(t, token, tcp, 10*time.Millisecond)
}

func startServerPolling(t *testing.T, token string, tcp bool, poll time.Duration) *served {
	t.Helper()
	rt := newFakeRuntime()
	srv := New(rt)
	srv.Token = token
	srv.PollInterval = poll
	cfg := config.ServeConfig{Socket: filepath.Join(shortSocketDir(t), "crux.sock"), Token: token}
	if tcp {
		cfg.Listen = "127.0.0.1:0"
	}
	ls, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, rt: rt, srv: srv, socket: cfg.Socket, client: unixClient(cfg.Socket), cancel: cancel, done: make(chan error, 1)}
	if ls.TCP != nil {
		s.tcp = "http://" + ls.TCP.Addr().String()
	}
	go func() { s.done <- srv.Serve(ctx, ls) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-s.done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return s
}

func (s *served) do(method, path string, body any) (*http.Response, []byte) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://crux"+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if s.srv.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.srv.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp, data
}

func (s *served) expect(method, path string, body any, status int, into any) {
	s.t.Helper()
	resp, data := s.do(method, path, body)
	if resp.StatusCode != status {
		s.t.Fatalf("%s %s: status %d body %s", method, path, resp.StatusCode, data)
	}
	if into != nil {
		if err := json.Unmarshal(data, into); err != nil {
			s.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
}

func TestListenCreatesAnOwnerOnlySocketAndRefusesADouble(t *testing.T) {
	dir := shortSocketDir(t)
	cfg := config.ServeConfig{Socket: filepath.Join(dir, "nested", "crux.sock")}
	ls, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	fi, err := os.Stat(cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != config.ServeSocketMode {
		t.Fatalf("socket mode %o, want %o", fi.Mode().Perm(), config.ServeSocketMode)
	}
	di, _ := os.Stat(filepath.Dir(cfg.Socket))
	if di.Mode().Perm() != config.ServeSocketDirMode {
		t.Fatalf("socket dir mode %o, want %o", di.Mode().Perm(), config.ServeSocketDirMode)
	}
	if ls.TCP != nil || len(ls.Addrs()) != 1 || ls.Addrs()[0] != "unix://"+cfg.Socket {
		t.Fatalf("Addrs = %v", ls.Addrs())
	}

	// Something is listening: a second Listen must refuse, not steal.
	go func() {
		conn, err := ls.Unix.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	if _, err := Listen(cfg); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second Listen: %v", err)
	}

	// A stale socket file (nobody answering) is replaced.
	ls.Close()
	if err := os.WriteFile(cfg.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ls2, err := Listen(cfg)
	if err != nil {
		t.Fatalf("stale socket should be replaced: %v", err)
	}
	ls2.Close()
	if _, err := os.Stat(cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close should remove the socket, stat err = %v", err)
	}
}

func TestListenFailsClosed(t *testing.T) {
	// TCP without a token is refused before anything binds.
	if _, err := Listen(config.ServeConfig{Socket: filepath.Join(shortSocketDir(t), "a.sock"), Listen: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("TCP without token: %v", err)
	}
	// An unbindable TCP address releases the socket it already took.
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "b.sock")
	if _, err := Listen(config.ServeConfig{Socket: sock, Listen: "256.256.256.256:1", Token: "t"}); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("bad TCP address: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be released after a TCP bind failure, stat err = %v", err)
	}
	// A socket path whose directory cannot be created.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(config.ServeConfig{Socket: filepath.Join(file, "c.sock")}); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("unmakeable dir: %v", err)
	}
	// A path that cannot be listened on (too long for sun_path).
	if _, err := Listen(config.ServeConfig{Socket: filepath.Join(dir, strings.Repeat("x", 120)+".sock")}); err == nil {
		t.Fatal("over-long socket path should fail")
	}
	// A socket directory this user does not own outright is refused: one
	// readable by others (a squatter could have created it)...
	loose := filepath.Join(dir, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(config.ServeConfig{Socket: filepath.Join(loose, "d.sock")}); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("world-readable dir: %v", err)
	}
	// ...or reached through a symlink.
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(config.ServeConfig{Socket: filepath.Join(link, "e.sock")}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked dir: %v", err)
	}
	if err := ownedPrivateDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("regular file: %v", err)
	}
	if err := ownedPrivateDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing dir should fail")
	}
	// The dir Listen creates itself passes.
	if err := ownedPrivateDir(private); err != nil {
		t.Fatal(err)
	}
}

func TestAuthRefusalOnTCPAndWithToken(t *testing.T) {
	s := startServer(t, "s3cret", true)

	// The socket requires the token because the server has one.
	bare := unixClient(s.socket)
	resp, err := bare.Get("http://crux" + api.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	var e api.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || e.Code != api.CodeUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token over socket: %d %+v %q", resp.StatusCode, e, resp.Header.Get("WWW-Authenticate"))
	}

	// A wrong token is refused over TCP; the right one is accepted.
	for _, c := range []struct {
		token  string
		status int
	}{{"wrong", http.StatusUnauthorized}, {"", http.StatusUnauthorized}, {"s3cret", http.StatusOK}} {
		req, _ := http.NewRequest(http.MethodGet, s.tcp+api.PathHealth, nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Fatalf("token %q over TCP: %d, want %d", c.token, resp.StatusCode, c.status)
		}
	}
	// The health body names the version.
	var h api.Health
	s.expect(http.MethodGet, api.PathHealth, nil, http.StatusOK, &h)
	if h.Version != api.Version || h.StartedAt.IsZero() || h.UptimeSeconds < 0 || h.StartedAt.After(time.Now()) {
		t.Fatalf("health = %+v", h)
	}
}

func TestHandlerRefusesTCPWithoutTokenEvenIfBound(t *testing.T) {
	// The second layer of fail-closed: a handler tagged as serving TCP with
	// no token refuses every request, independent of Listen's check.
	srv := New(newFakeRuntime())
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, api.PathHealth, nil)
	req = req.WithContext(context.WithValue(req.Context(), networkKey, "tcp"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("TCP without token: %d %s", rec.Code, rec.Body)
	}
	// An untagged request (Handler mounted on someone else's http.Server)
	// is refused the same way: only Serve's unix tag opens the door.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.PathHealth, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("untagged without token: %d %s", rec.Code, rec.Body)
	}
	foreign := httptest.NewServer(h)
	defer foreign.Close()
	resp, err := http.Get(foreign.URL + api.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("foreign TCP server without token: %d", resp.StatusCode)
	}
	// With a token the foreign mount works like any other listener.
	srv.Token = "t"
	freq, _ := http.NewRequest(http.MethodGet, foreign.URL+api.PathHealth, nil)
	freq.Header.Set("Authorization", "Bearer t")
	resp, err = http.DefaultClient.Do(freq)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("foreign TCP server with token: %d", resp.StatusCode)
	}
	srv.Token = ""
	// The same request on the unix network is served.
	req = httptest.NewRequest(http.MethodGet, api.PathHealth, nil)
	req = req.WithContext(context.WithValue(req.Context(), networkKey, "unix"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unix without token: %d %s", rec.Code, rec.Body)
	}
}

func TestRunListGetOverTheSocket(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("scout", "s1", crux.StateRunning)

	// run with a definition registers and starts.
	def, _ := json.Marshal(config.AgentConfig{ID: "f1", Backend: "copilot", Model: "gpt-5"})
	var ar api.AgentResponse
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "fixer", Definition: def}, http.StatusCreated, &ar)
	if ar.Agent != "fixer" || ar.Process == nil || ar.Process.State != string(crux.StateRunning) || ar.Process.Backend != "copilot" || ar.Process.Model != "gpt-5" || ar.Process.StartedAt == nil {
		t.Fatalf("run = %+v / %+v", ar, ar.Process)
	}
	// The agent was launched under the server's context, not the request's.
	s.rt.mu.Lock()
	launchCtx := s.rt.startCtx
	gotCfg := s.rt.procs["fixer"].Config
	s.rt.mu.Unlock()
	if launchCtx == nil || launchCtx.Err() != nil {
		t.Fatalf("launch context should be the live server context, got %v", launchCtx)
	}
	// The defaults a start depends on were filled; the given ID was kept.
	if gotCfg.ID != "f1" || gotCfg.Role != "fixer" || gotCfg.Replicas != 1 || !gotCfg.Enabled {
		t.Fatalf("defaults not applied: %+v", gotCfg)
	}
	// A definition that sets enabled or role keeps them.
	def2 := json.RawMessage(`{"backend":"copilot","role":"scout-role","enabled":false,"replicas":3}`)
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "quiet", Definition: def2}, http.StatusCreated, &ar)
	s.rt.mu.Lock()
	gotCfg = s.rt.procs["quiet"].Config
	s.rt.mu.Unlock()
	if gotCfg.ID != "quiet" || gotCfg.Role != "scout-role" || gotCfg.Replicas != 3 || gotCfg.Enabled {
		t.Fatalf("explicit fields overridden: %+v", gotCfg)
	}
	// Re-posting a definition for a known name is a conflict, not a silent
	// start of the old definition.
	var e api.Error
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "fixer", Definition: def}, http.StatusConflict, &e)
	if e.Code != api.CodeConflict || !strings.Contains(e.Error, "already defined") {
		t.Fatalf("conflict: %+v", e)
	}
	// A bare name may be an agent ID, as for every other verb.
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "s1"}, http.StatusCreated, &ar)
	if ar.Agent != "scout" {
		t.Fatalf("run by id = %+v", ar)
	}

	// run without a definition starts an agent the server already knows.
	s.rt.add("idle", "i1", crux.StateStopped)
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "idle"}, http.StatusCreated, &ar)
	if ar.Process.State != string(crux.StateRunning) {
		t.Fatalf("run known = %+v", ar.Process)
	}

	// list is sorted by name and skips nil entries.
	var list api.ProcessList
	s.expect(http.MethodGet, api.PathAgents, nil, http.StatusOK, &list)
	var names []string
	for _, p := range list.Processes {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "fixer,idle,quiet,scout" {
		t.Fatalf("list = %v", names)
	}

	// get by ID resolves to the name and carries the full projection.
	s.rt.mu.Lock()
	p := s.rt.procs["scout"]
	p.ModelOverride = "sonnet"
	p.Paused, p.PausedReason, p.PausedBy = true, "quiesced", "andy"
	p.KickRefused, p.KickRefusalReason = true, "busy"
	p.StartFailureClass = string(crux.StartFailureNoOutput)
	s.rt.mu.Unlock()
	var got api.Process
	s.expect(http.MethodGet, api.AgentPath("s1"), nil, http.StatusOK, &got)
	if got.Name != "scout" || got.Model != "sonnet" || !got.Paused || got.PausedBy != "andy" || !got.KickRefused || got.StartFailureClass != string(crux.StartFailureNoOutput) {
		t.Fatalf("get = %+v", got)
	}

	// health counts the definitions.
	var h api.Health
	s.expect(http.MethodGet, api.PathHealth, nil, http.StatusOK, &h)
	if h.Agents != 5 { // four agents plus the fake's nil entry
		t.Fatalf("health agents = %d", h.Agents)
	}
}

func TestRunErrors(t *testing.T) {
	s := startServer(t, "", false)
	var e api.Error
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{}, http.StatusBadRequest, &e)
	if e.Code != api.CodeBadRequest || !strings.Contains(e.Error, "name") {
		t.Fatalf("empty name: %+v", e)
	}
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "ghost"}, http.StatusNotFound, &e)
	if e.Code != api.CodeNotFound {
		t.Fatalf("unknown without definition: %+v", e)
	}
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "x", Definition: json.RawMessage(`"not an object"`)}, http.StatusBadRequest, &e)
	if !strings.Contains(e.Error, "definition") {
		t.Fatalf("bad definition: %+v", e)
	}
	s.expect(http.MethodPost, api.PathAgents, map[string]any{"name": "x", "bogus": 1}, http.StatusBadRequest, &e)
	if !strings.Contains(e.Error, "request body") {
		t.Fatalf("unknown field: %+v", e)
	}
	for _, bad := range []string{"Fixer", "a b", "../x", "-lead", strings.Repeat("a", 65)} {
		s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: bad, Definition: json.RawMessage(`{}`)}, http.StatusBadRequest, &e)
		if e.Code != api.CodeBadRequest || !strings.Contains(e.Error, "name must match") {
			t.Fatalf("name %q: %+v", bad, e)
		}
	}
	// The runtime declining the definition (the fake's stand-in for the
	// ACMM gate) is reported, not followed by a start that cannot work.
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "refused", Definition: json.RawMessage(`{}`)}, http.StatusUnprocessableEntity, &e)
	if e.Code != api.CodeBadRequest || !strings.Contains(e.Error, "not accepted") {
		t.Fatalf("refused: %+v", e)
	}
	s.rt.add("broken", "b1", crux.StateStopped)
	s.rt.startErr = errors.New("tmux: no server")
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "broken"}, http.StatusInternalServerError, &e)
	if e.Code != api.CodeRuntime || !strings.Contains(e.Error, "no server") {
		t.Fatalf("start error: %+v", e)
	}
	s.expect(http.MethodGet, api.AgentPath("ghost"), nil, http.StatusNotFound, &e)
	if e.Code != api.CodeNotFound {
		t.Fatalf("get ghost: %+v", e)
	}
	// Unknown routes and wrong methods are the mux's 404/405.
	if resp, _ := s.do(http.MethodDelete, api.PathAgents, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE agents: %d", resp.StatusCode)
	}
	if resp, _ := s.do(http.MethodGet, "/v1/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route: %d", resp.StatusCode)
	}
}

func TestLogsSources(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("fixer", "f1", crux.StateRunning)
	s.rt.pane["fixer"] = []string{"p1", "p2", "p3"}
	s.rt.buffer["fixer"] = [][]string{{"b1", "b2"}}
	s.rt.fullLog["fixer"] = "everything"
	s.rt.header["fixer"] = "# header"

	var logs api.Logs
	s.expect(http.MethodGet, api.LogsPath("fixer"), nil, http.StatusOK, &logs)
	if logs.Source != api.LogSourcePane || strings.Join(logs.Lines, ",") != "p1,p2,p3" {
		t.Fatalf("pane default = %+v", logs)
	}
	s.expect(http.MethodGet, api.LogsPath("f1")+"?n=2", nil, http.StatusOK, &logs)
	if logs.Agent != "fixer" || strings.Join(logs.Lines, ",") != "p2,p3" {
		t.Fatalf("pane n=2 by id = %+v", logs)
	}
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?source=buffer", nil, http.StatusOK, &logs)
	if logs.Source != api.LogSourceBuffer || strings.Join(logs.Lines, ",") != "b1,b2" {
		t.Fatalf("buffer = %+v", logs)
	}
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?source=full", nil, http.StatusOK, &logs)
	if logs.Source != api.LogSourceFull || logs.Header != "# header" || logs.Content != "everything" {
		t.Fatalf("full = %+v", logs)
	}

	var e api.Error
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?source=tape", nil, http.StatusBadRequest, &e)
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?n=-1", nil, http.StatusBadRequest, &e)
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?n=abc", nil, http.StatusBadRequest, &e)
	s.rt.outputErr = errors.New("pane gone")
	s.expect(http.MethodGet, api.LogsPath("fixer"), nil, http.StatusInternalServerError, &e)
	if e.Code != api.CodeRuntime || !strings.Contains(e.Error, "pane gone") {
		t.Fatalf("output error: %+v", e)
	}
	s.expect(http.MethodGet, api.LogsPath("fixer")+"?follow=1", nil, http.StatusInternalServerError, &e)
	if !strings.Contains(e.Error, "pane gone") {
		t.Fatalf("follow with output error: %+v", e)
	}
}

// readStream reads SSE records from resp until want have arrived or the
// deadline passes, then closes the body.
func readStream(t *testing.T, resp *http.Response, want int) []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = api.ReadEvents(resp.Body, func(typ string, data []byte) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, typ+"|"+string(data))
			if len(got) >= want {
				return io.EOF
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = resp.Body.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("stream delivered %d of %d records: %v", len(got), want, got)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestFollowLogsStreamsNewLines(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("fixer", "f1", crux.StateRunning)
	// Successive buffer windows: the initial tail, then overlap, then an
	// unchanged window (no record), then a reset.
	s.rt.buffer["fixer"] = [][]string{{"a", "b"}, {"b", "c"}, {"b", "c"}, {"z"}}

	req, _ := http.NewRequest(http.MethodGet, "http://crux"+api.LogsPath("fixer")+"?follow=true", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != api.ContentTypeEvent {
		t.Fatalf("follow: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	got := readStream(t, resp, 3)
	want := []string{
		api.EventLog + `|{"agent":"fixer","lines":["a","b"]}`,
		api.EventLog + `|{"agent":"fixer","lines":["c"]}`,
		api.EventLog + `|{"agent":"fixer","lines":["z"]}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("follow records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFollowEndsWithAStateRecordWhenOutputFails(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("fixer", "f1", crux.StateRunning)
	req, _ := http.NewRequest(http.MethodGet, "http://crux"+api.LogsPath("fixer")+"?follow=1", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s.rt.mu.Lock()
	s.rt.outputErr = errors.New("session ended")
	s.rt.mu.Unlock()
	got := readStream(t, resp, 1)
	if len(got) != 1 || !strings.HasPrefix(got[0], api.EventState+"|") || !strings.Contains(got[0], "session ended") {
		t.Fatalf("records = %v", got)
	}
}

func TestKickAndStop(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("fixer", "f1", crux.StateRunning)
	s.rt.add("idle", "i1", crux.StateStopped)
	s.rt.add("wedged", "w1", crux.StateFailed)

	var ar api.AgentResponse
	s.expect(http.MethodPost, api.KickPath("f1"), api.KickRequest{Message: "look at main"}, http.StatusOK, &ar)
	if ar.Agent != "fixer" || ar.Process == nil || strings.Join(s.rt.Kicks(), ",") != "look at main" {
		t.Fatalf("kick = %+v kicks %v", ar, s.rt.Kicks())
	}
	var e api.Error
	s.expect(http.MethodPost, api.KickPath("fixer"), api.KickRequest{Message: "  "}, http.StatusBadRequest, &e)
	s.expect(http.MethodPost, api.KickPath("fixer"), map[string]int{"nope": 1}, http.StatusBadRequest, &e)
	s.expect(http.MethodPost, api.KickPath("ghost"), api.KickRequest{Message: "x"}, http.StatusNotFound, &e)
	if e.Code != api.CodeNotFound {
		t.Fatalf("kick ghost: %+v", e)
	}
	s.expect(http.MethodPost, api.KickPath("idle"), api.KickRequest{Message: "x"}, http.StatusInternalServerError, &e)
	if e.Code != api.CodeRuntime || !strings.Contains(e.Error, "not running") {
		t.Fatalf("kick idle: %+v", e)
	}

	s.expect(http.MethodPost, api.StopPath("fixer"), nil, http.StatusOK, &ar)
	if ar.Process == nil || ar.Process.State != string(crux.StateStopped) {
		t.Fatalf("stop = %+v", ar.Process)
	}
	s.expect(http.MethodPost, api.StopPath("ghost"), nil, http.StatusNotFound, &e)
	s.expect(http.MethodPost, api.StopPath("wedged"), nil, http.StatusInternalServerError, &e)
	if !strings.Contains(e.Error, "kill-session") {
		t.Fatalf("stop wedged: %+v", e)
	}
}

// watched is the state the server's watcher last recorded for name.
func (s *served) watched(name string) string {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	return s.srv.lastStates[name]
}

// waitUntil polls cond on a short ticker until it holds or the deadline
// passes; it is the package's stand-in for testutil.Eventually, which the
// nested module cannot import.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestEventStream(t *testing.T) {
	s := startServer(t, "", false)
	s.rt.add("fixer", "f1", crux.StateStopped)
	// Let the watcher seed its table before subscribing so the stream
	// carries transitions only.
	waitUntil(t, "watcher seeded", func() bool { return s.watched("fixer") == string(crux.StateStopped) })

	req, _ := http.NewRequest(http.MethodGet, "http://crux"+api.PathEvents, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != api.ContentTypeEvent {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Make sure the subscription is registered before acting.
	waitUntil(t, "subscriber registered", func() bool {
		s.srv.mu.Lock()
		defer s.srv.mu.Unlock()
		return len(s.srv.subscribers) == 1
	})

	// An API-driven start, a kick, a stop; then a relayed kick-observer
	// event; then a state change the API did not cause. After each state
	// change wait for the watcher to record it so it sees every transition.
	settle := func(state crux.ProcessState) {
		waitUntil(t, "watcher saw "+string(state), func() bool { return s.watched("fixer") == string(state) })
	}
	s.expect(http.MethodPost, api.PathAgents, api.RunRequest{Name: "fixer"}, http.StatusCreated, nil)
	settle(crux.StateRunning)
	s.expect(http.MethodPost, api.KickPath("fixer"), api.KickRequest{Message: "go"}, http.StatusOK, nil)
	s.rt.mu.Lock()
	observer := s.rt.observer
	s.rt.mu.Unlock()
	if observer == nil {
		t.Fatal("Serve should install a kick observer")
	}
	observer("fixer", "delivered", "typed 2 lines")
	s.expect(http.MethodPost, api.StopPath("fixer"), nil, http.StatusOK, nil)
	settle(crux.StateStopped)
	s.rt.mu.Lock()
	s.rt.procs["fixer"].State = crux.StateFailed
	s.rt.procs["fixer"].LastRestartReason = "crash loop"
	s.rt.mu.Unlock()

	got := readStream(t, resp, 7)
	var types []string
	var events []api.Event
	for _, rec := range got {
		typ, data, _ := strings.Cut(rec, "|")
		types = append(types, typ)
		var ev api.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("%s: %v", rec, err)
		}
		if ev.Time.IsZero() || ev.Type != typ {
			t.Fatalf("event %+v is not stamped or typed", ev)
		}
		events = append(events, ev)
	}
	// started, state(running), kicked, kick relay, stopped, state(stopped),
	// state(failed). The watcher's state records interleave with the
	// lifecycle ones depending on timing, so check the multiset and the
	// relative order of the lifecycle events.
	count := map[string]int{}
	for _, ty := range types {
		count[ty]++
	}
	if count[api.EventStarted] != 1 || count[api.EventKicked] != 1 || count[api.EventKick] != 1 || count[api.EventStopped] != 1 || count[api.EventState] != 3 {
		t.Fatalf("event types = %v", types)
	}
	if idx(types, api.EventStarted) > idx(types, api.EventKicked) || idx(types, api.EventKicked) > idx(types, api.EventKick) || idx(types, api.EventKick) > idx(types, api.EventStopped) {
		t.Fatalf("lifecycle order = %v", types)
	}
	var states []string
	for _, ev := range events {
		if ev.Type == api.EventState {
			states = append(states, ev.State)
			if ev.State == string(crux.StateFailed) && ev.Detail != "crash loop" {
				t.Fatalf("failed state should carry the restart reason: %+v", ev)
			}
		}
		if ev.Type == api.EventKick && (ev.State != "delivered" || ev.Detail != "typed 2 lines") {
			t.Fatalf("kick relay = %+v", ev)
		}
	}
	if strings.Join(states, ",") != "running,stopped,failed" {
		t.Fatalf("state transitions = %v", states)
	}
}

func idx(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

func TestStateWatcherReportsPausedAndForgetsRemovedAgents(t *testing.T) {
	rt := newFakeRuntime()
	srv := New(rt)
	rt.add("a", "a1", crux.StateRunning)
	srv.diffStates(false) // seed
	ch := srv.subscribe()
	rt.mu.Lock()
	rt.procs["a"].Paused = true
	rt.mu.Unlock()
	srv.diffStates(true)
	select {
	case ev := <-ch:
		if ev.Type != api.EventState || ev.State != string(crux.StatePaused) {
			t.Fatalf("paused event = %+v", ev)
		}
	default:
		t.Fatal("no event for the pause")
	}
	rt.mu.Lock()
	delete(rt.procs, "a")
	rt.mu.Unlock()
	srv.diffStates(true)
	if _, ok := srv.lastStates["a"]; ok {
		t.Fatal("removed agent should leave the state table")
	}
	select {
	case ev := <-ch:
		t.Fatalf("removal should publish nothing, got %+v", ev)
	default:
	}
	srv.unsubscribe(ch)
	srv.unsubscribe(ch) // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("unsubscribe should close the channel")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	srv := New(newFakeRuntime())
	ch := srv.subscribe()
	for i := 0; i < subscriberBuffer+1; i++ {
		srv.publish(api.Event{Type: api.EventKicked, Agent: "a"})
	}
	n := 0
	for range ch {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("drained %d events, want %d then a closed channel", n, subscriberBuffer)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.subscribers) != 0 {
		t.Fatal("dropped subscriber should be removed")
	}
}

// noFlush is a ResponseWriter that cannot stream.
type noFlush struct{ http.ResponseWriter }

func TestStreamsNeedAFlusher(t *testing.T) {
	srv := New(newFakeRuntime())
	srv.Runtime.(*fakeRuntime).add("a", "a1", crux.StateRunning)
	for _, path := range []string{api.PathEvents, api.LogsPath("a") + "?follow=1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), networkKey, networkUnix))
		srv.Handler().ServeHTTP(noFlush{rec}, req)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "streaming") {
			t.Fatalf("%s without flusher: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestEventStreamKeepAlive(t *testing.T) {
	s := startServerPolling(t, "", false, time.Millisecond) // keep-alive every 15ms
	req, _ := http.NewRequest(http.MethodGet, "http://crux"+api.PathEvents, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := io.ReadAtLeast(resp.Body, buf, len(": keep-alive\n\n"))
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(buf[:n]), "keep-alive") {
		t.Fatalf("keep-alive: %q %v", buf[:n], err)
	}
}

func TestServeReturnsListenerErrors(t *testing.T) {
	// A listener that fails on Accept makes Serve return its error.
	srv := New(newFakeRuntime())
	ls := &Listeners{Unix: failingListener{}}
	err := srv.Serve(context.Background(), ls)
	if err == nil || !strings.Contains(err.Error(), "accept boom") {
		t.Fatalf("Serve = %v", err)
	}
}

type failingListener struct{}

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept boom") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr            { return &net.UnixAddr{Name: "fail", Net: "unix"} }

func TestWriteJSONLogsEncodeFailures(t *testing.T) {
	var logged bytes.Buffer
	srv := New(newFakeRuntime())
	srv.Logger = slogTo(&logged)
	rec := httptest.NewRecorder()
	srv.writeJSON(rec, http.StatusOK, make(chan int))
	if !strings.Contains(logged.String(), "writing response") {
		t.Fatalf("encode failure should be logged, got %q", logged.String())
	}
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func TestIsTrueAndNewTail(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", "on"} {
		if !isTrue(v) {
			t.Errorf("%q should be true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "maybe"} {
		if isTrue(v) {
			t.Errorf("%q should be false", v)
		}
	}
	cases := []struct{ prev, cur, want []string }{
		{nil, []string{"a"}, []string{"a"}},
		{[]string{"a", "b"}, []string{"a", "b"}, nil},
		{[]string{"a", "b"}, []string{"b", "c"}, []string{"c"}},
		{[]string{"a", "b"}, []string{"x", "y"}, []string{"x", "y"}},
		{[]string{"a", "b", "c"}, []string{"c"}, nil},
	}
	for _, c := range cases {
		if got := newTail(c.prev, c.cur); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("newTail(%v,%v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

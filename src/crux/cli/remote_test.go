package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// fakeAPI is a `crux serve` stand-in speaking crux/api, so remote mode is
// tested against the wire contract with full control over timing.
type fakeAPI struct {
	mu       sync.Mutex
	token    string
	requests []string
	run      api.RunRequest
	kick     string
	// hold keeps a followed log stream open until closed.
	hold chan struct{}
}

func (f *fakeAPI) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.requests, "\n")
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
			f.mu.Unlock()
			if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(api.Error{Error: "token required", Code: api.CodeUnauthorized})
				return
			}
			h(w, r)
		}
	}
	notFound := func(w http.ResponseWriter, name string) bool {
		if name != "ghost" {
			return false
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.Error{Error: "agent ghost not found", Code: api.CodeNotFound})
		return true
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mux.HandleFunc("GET "+api.PathAgents, wrap(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProcessList{Processes: []api.Process{
			{Name: "fixer", ID: "f1", State: "running", Backend: "claude", Model: "opus", StartedAt: &started, RestartCount: 2},
			{Name: "scout", ID: "s1", State: "paused", Backend: "codex", Paused: true},
		}})
	}))
	mux.HandleFunc("POST "+api.PathAgents, wrap(func(w http.ResponseWriter, r *http.Request) {
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.run = req
		f.mu.Unlock()
		if notFound(w, req.Name) {
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentResponse{Agent: req.Name})
	}))
	mux.HandleFunc("GET "+api.PathAgents+"/{name}/logs", wrap(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if notFound(w, name) {
			return
		}
		q := r.URL.Query()
		if q.Get(api.QueryFollow) == "" {
			out := api.Logs{Agent: name, Source: q.Get(api.QuerySource)}
			if out.Source == api.LogSourceFull {
				out.Header, out.Content = "# header", "all of it"
			} else {
				out.Lines = []string{out.Source + " n=" + q.Get(api.QueryLines)}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		w.Header().Set("Content-Type", api.ContentTypeEvent)
		fl := w.(http.Flusher)
		_ = api.WriteEvent(w, api.EventLog, api.LogChunk{Agent: name, Lines: []string{"boot", "ready"}})
		fl.Flush()
		if name == "lost" {
			_ = api.WriteEvent(w, api.EventState, api.Event{Type: api.EventState, Agent: name, Detail: "agent lost not found"})
			fl.Flush()
			return
		}
		if f.hold != nil {
			select {
			case <-f.hold:
			case <-r.Context().Done():
			}
		}
	}))
	mux.HandleFunc("POST "+api.PathAgents+"/{name}/kick", wrap(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if notFound(w, name) {
			return
		}
		var req api.KickRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.kick = req.Message
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.AgentResponse{Agent: name})
	}))
	mux.HandleFunc("POST "+api.PathAgents+"/{name}/stop", wrap(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if notFound(w, name) {
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentResponse{Agent: name})
	}))
	return mux
}

// waitUntil polls cond on a short ticker until it holds or the deadline
// passes; the nested module cannot import testutil.Eventually.
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

// lockedBuffer is a bytes.Buffer safe to read while a command writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startFakeAPI serves f on a unix socket and returns the socket path.
func startFakeAPI(t *testing.T, f *fakeAPI) string {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

func TestRemotePSLogsKickStop(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	f := &fakeAPI{token: "tok"}
	socket := startFakeAPI(t, f)

	h := newHarness(t)
	if code := h.run("--server", socket, "--token", "tok", "ps"); code != ExitOK {
		t.Fatalf("ps: %d %s", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "fixer  f1  running") || !strings.Contains(out, "paused (paused)") || !strings.Contains(out, "opus") {
		t.Fatalf("ps table:\n%s", out)
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("remote ps touched the local runtime: %v", h.rt.Calls())
	}

	h = newHarness(t)
	if code := h.run("--server", socket, "--token", "tok", "ps", "--json"); code != ExitOK {
		t.Fatalf("ps --json: %d %s", code, h.stderr)
	}
	var rows []processRow
	if err := json.Unmarshal(h.stdout.Bytes(), &rows); err != nil || len(rows) != 2 || rows[0].RestartCount != 2 {
		t.Fatalf("ps json %v: %s", err, h.stdout)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "fixer"}, "pane n=100\n"},
		{[]string{"logs", "-n", "5", "-buffer", "fixer"}, "buffer n=5\n"},
		{[]string{"logs", "-full", "fixer"}, "# header\nall of it\n"},
	} {
		h = newHarness(t)
		if code := h.run(append([]string{"--server", socket, "--token", "tok"}, c.args...)...); code != ExitOK || h.stdout.String() != c.want {
			t.Fatalf("%v: code %d stdout %q stderr %q", c.args, code, h.stdout, h.stderr)
		}
	}
	h = newHarness(t)
	if code := h.run("--server", socket, "--token", "tok", "logs", "-json", "-buffer", "fixer"); code != ExitOK {
		t.Fatalf("logs -json: %d %s", code, h.stderr)
	}
	var lo logsOutput
	if err := json.Unmarshal(h.stdout.Bytes(), &lo); err != nil || lo.Source != "buffer" || lo.Agent != "fixer" {
		t.Fatalf("logs json %v: %s", err, h.stdout)
	}

	h = newHarness(t)
	h.env.Stdin = strings.NewReader("from stdin\n")
	if code := h.run("--server", socket, "--token", "tok", "kick", "fixer", "-"); code != ExitOK || h.stdout.String() != "kicked fixer\n" {
		t.Fatalf("kick: %d %q %q", code, h.stdout, h.stderr)
	}
	if f.kick != "from stdin" {
		t.Fatalf("kick message %q", f.kick)
	}
	h = newHarness(t)
	if code := h.run("--server", socket, "--token", "tok", "kick", "fixer", "do", "it"); code != ExitOK || f.kick != "do it" {
		t.Fatalf("kick words: %d %q", code, f.kick)
	}

	h = newHarness(t)
	if code := h.run("--server", socket, "--token", "tok", "stop", "fixer"); code != ExitOK || h.stdout.String() != "stopped fixer\n" {
		t.Fatalf("stop: %d %q %q", code, h.stdout, h.stderr)
	}

	// The token and server can come from the environment.
	t.Setenv(config.ServerEnvVar, socket)
	t.Setenv(config.ServeTokenEnvVar, "tok")
	h = newHarness(t)
	if code := h.run("stop", "fixer"); code != ExitOK {
		t.Fatalf("env stop: %d %s", code, h.stderr)
	}
	if !strings.Contains(f.seen(), "POST /v1/agents/fixer/stop") {
		t.Fatalf("requests:\n%s", f.seen())
	}
}

func TestRemoteRunFollowsAndStops(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	f := &fakeAPI{hold: make(chan struct{})}
	t.Cleanup(func() { close(f.hold) })
	socket := startFakeAPI(t, f)
	def := writeFile(t, filepath.Join(t.TempDir(), "scout.yaml"), portableDefinition)

	h := newHarness(t)
	out := &lockedBuffer{}
	h.env.Stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "--server", socket, "run", def))
	}()
	waitUntil(t, "first chunk printed", func() bool { return strings.Contains(out.String(), "ready\n") })
	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("code %d stderr %q", code, h.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote run did not stop")
	}
	if out.String() != "started scout-agent\nboot\nready\nstopped scout-agent\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if f.run.Name != "scout-agent" || !strings.Contains(string(f.run.Definition), `"role":"scout"`) {
		t.Fatalf("run request %+v %s", f.run, f.run.Definition)
	}
	if !strings.Contains(f.seen(), "POST /v1/agents/scout-agent/stop") {
		t.Fatalf("requests:\n%s", f.seen())
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("remote run touched the local runtime: %v", h.rt.Calls())
	}

	// A bare name is sent without a definition; -detach returns after start.
	h = newHarness(t)
	if code := h.run("--server", socket, "run", "-detach", "fixer"); code != ExitOK || h.stdout.String() != "started fixer\n" {
		t.Fatalf("detach: %d %q %q", code, h.stdout, h.stderr)
	}
	if f.run.Name != "fixer" || len(f.run.Definition) != 0 {
		t.Fatalf("bare run request %+v", f.run)
	}

	// The server losing the agent ends the follow with a message, then stops.
	h = newHarness(t)
	if code := h.run("--server", socket, "run", "lost"); code != ExitOK || !strings.Contains(h.stderr.String(), "agent lost not found") || !strings.Contains(h.stdout.String(), "stopped lost") {
		t.Fatalf("lost: %d %q %q", code, h.stdout, h.stderr)
	}

	// logs -f follows the same stream.
	h = newHarness(t)
	if code := h.run("--server", socket, "logs", "-f", "lost"); code != ExitError || h.stdout.String() != "boot\nready\n" {
		t.Fatalf("logs -f: %d %q %q", code, h.stdout, h.stderr)
	}
}

func TestRemoteErrors(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	f := &fakeAPI{token: "tok"}
	socket := startFakeAPI(t, f)

	cases := []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"--server", "", "ps"}, ExitOK, ""}, // empty --server is not remote mode
		{[]string{"--server", "::bad::", "ps"}, ExitUsage, "--server"},
		{[]string{"--server", socket, "ps"}, ExitError, "bearer token"},
		{[]string{"--server", socket, "--token", "tok", "serve", "-h"}, ExitOK, ""}, // serve always runs locally
		{[]string{"--server", socket, "--token", "tok", "stop", "ghost"}, ExitError, "agent ghost not found"},
		{[]string{"--server", socket, "--token", "tok", "kick", "ghost", "hi"}, ExitError, "agent ghost not found"},
		{[]string{"--server", socket, "--token", "tok", "logs", "ghost"}, ExitError, "agent ghost not found"},
		{[]string{"--server", socket, "--token", "tok", "run", "ghost"}, ExitError, "agent ghost not found"},
		{[]string{"--server", socket, "--token", "tok", "run", filepath.Join(t.TempDir(), "missing.yaml")}, ExitError, "reading definition"},
		{[]string{"--server", socket, "--token", "tok", "run"}, ExitUsage, "usage: crux --server"},
		{[]string{"--server", socket, "--token", "tok", "run", "-h"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "ps", "-h"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "logs"}, ExitUsage, "usage: crux --server"},
		{[]string{"--server", socket, "--token", "tok", "logs", "-h"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "logs", "-buffer", "-full", "fixer"}, ExitUsage, "exclusive"},
		{[]string{"--server", socket, "--token", "tok", "logs", "-f", "-json", "fixer"}, ExitUsage, "exclusive"},
		{[]string{"--server", socket, "--token", "tok", "kick", "fixer"}, ExitUsage, "usage: crux kick"},
		{[]string{"--server", socket, "--token", "tok", "kick", "-h"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "kick", "fixer", "  "}, ExitUsage, "empty"},
		{[]string{"--server", socket, "--token", "tok", "stop"}, ExitUsage, "usage: crux stop"},
		{[]string{"--server", socket, "--token", "tok", "stop", "-h"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "help"}, ExitOK, ""},
		{[]string{"--server", socket, "--token", "tok", "bogus"}, ExitUsage, "not available with --server"},
	}
	for _, c := range cases {
		h := newHarness(t)
		code := h.run(c.args...)
		if code != c.code || !strings.Contains(h.stderr.String(), c.stderr) {
			t.Errorf("%v: code %d (want %d) stderr %q (want %q)", c.args, code, c.code, h.stderr, c.stderr)
		}
	}

	// Stdin failure and a dead stop after a run follow.
	h := newHarness(t)
	h.env.Stdin = failingReader{}
	if code := h.run("--server", socket, "--token", "tok", "kick", "fixer", "-"); code != ExitError || !strings.Contains(h.stderr.String(), "broken stdin") {
		t.Fatalf("stdin: %d %q", code, h.stderr)
	}
}

// TestRemoteAgainstRealServe round-trips `crux --server` against a real
// `crux serve` on a socket: the two halves agree on the wire.
func TestRemoteAgainstRealServe(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server := newHarness(t)
	cfgPath := writeFile(t, filepath.Join(t.TempDir(), "hive.yaml"), "agents: {fixer: {}, scout: {}}\n")
	socket := filepath.Join(shortSocketDir(t), "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		server.env.Run(ctx, append(server.workDirArgs(), "--config", cfgPath, "serve", "--socket", socket, "--token", "tok"))
	}()
	waitForSocket(t, socket)

	client := newHarness(t)
	if code := client.run("--server", "unix://"+socket, "--token", "tok", "ps", "--json"); code != ExitOK {
		t.Fatalf("ps: %d %s", code, client.stderr)
	}
	var rows []processRow
	if err := json.Unmarshal(client.stdout.Bytes(), &rows); err != nil || len(rows) != 2 || rows[0].Name != "fixer" {
		t.Fatalf("rows %v %s", err, client.stdout)
	}
	client = newHarness(t)
	if code := client.run("--server", socket, "--token", "tok", "kick", "scout", "hello"); code != ExitOK || client.stdout.String() != "kicked scout\n" {
		t.Fatalf("kick: %d %q %q", code, client.stdout, client.stderr)
	}
	if kicks := server.rt.Kicks(); len(kicks) != 1 || kicks[0] != "hello" {
		t.Fatalf("server kicks %v", kicks)
	}
	client = newHarness(t)
	if code := client.run("--server", socket, "ps"); code != ExitError || !strings.Contains(client.stderr.String(), "bearer token") {
		t.Fatalf("no token: %d %q", code, client.stderr)
	}
}

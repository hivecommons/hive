package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a hand-rolled handler speaking the wire contract, so the
// client is tested against the contract and not against crux/serve.
type fakeServer struct {
	mu       sync.Mutex
	token    string
	requests []string
	bodies   []string
	// streamHold, when set, keeps SSE handlers open until closed. Guarded
	// by mu: a handler from an earlier stream may still be running when a
	// test sets it.
	streamHold chan struct{}
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
			f.mu.Unlock()
			if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(Error{Error: "token required", Code: CodeUnauthorized})
				return
			}
			h(w, r)
		}
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET "+PathHealth, auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, Health{Version: Version, Agents: 2})
	}))
	mux.HandleFunc("GET "+PathAgents, auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, ProcessList{Processes: []Process{{Name: "a"}, {Name: "b"}}})
	}))
	mux.HandleFunc("POST "+PathAgents, auth(func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(req.Definition))
		f.mu.Unlock()
		if req.Name == "boom" {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, Error{Error: "start boom: tmux is down", Code: CodeRuntime})
			return
		}
		writeJSON(w, AgentResponse{Agent: req.Name, Process: &Process{Name: req.Name, State: "running"}})
	}))
	mux.HandleFunc("GET "+PathAgents+"/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "ghost" {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, Error{Error: "agent ghost not found", Code: CodeNotFound})
			return
		}
		writeJSON(w, Process{Name: r.PathValue("name"), State: "running"})
	}))
	mux.HandleFunc("GET "+PathAgents+"/{name}/logs", auth(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if r.URL.Query().Get(QueryFollow) == "" {
			writeJSON(w, Logs{Agent: name, Source: r.URL.Query().Get(QuerySource), Lines: []string{"n=" + r.URL.Query().Get(QueryLines)}})
			return
		}
		w.Header().Set("Content-Type", ContentTypeEvent)
		fl := w.(http.Flusher)
		_ = WriteEvent(w, EventLog, LogChunk{Agent: name, Lines: []string{"one", "two"}})
		_ = WriteEvent(w, "future.type", Event{Type: "future.type"}) // ignored by the client
		fl.Flush()
		if name == "lost" {
			_ = WriteEvent(w, EventState, Event{Type: EventState, Agent: name, Detail: "agent lost not found"})
			fl.Flush()
			return
		}
		if name == "garbage" {
			_, _ = w.Write([]byte("event: " + EventLog + "\ndata: {not json\n\n"))
			fl.Flush()
			return
		}
		if hold := f.hold(); hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}))
	mux.HandleFunc("POST "+PathAgents+"/{name}/kick", auth(func(w http.ResponseWriter, r *http.Request) {
		var req KickRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.bodies = append(f.bodies, req.Message)
		f.mu.Unlock()
		writeJSON(w, AgentResponse{Agent: r.PathValue("name")})
	}))
	mux.HandleFunc("POST "+PathAgents+"/{name}/stop", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "plain" {
			// A non-JSON error body: the client must still surface the status.
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		writeJSON(w, AgentResponse{Agent: r.PathValue("name")})
	}))
	mux.HandleFunc("GET "+PathEvents, auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ContentTypeEvent)
		fl := w.(http.Flusher)
		_ = WriteEvent(w, EventStarted, Event{Type: EventStarted, Agent: "a"})
		// A record typed only by the event: line.
		_, _ = w.Write([]byte("event: " + EventKick + "\ndata: {\"agent\":\"a\"}\n\n"))
		if r.URL.Query().Get("garbage") != "" {
			_, _ = w.Write([]byte("event: x\ndata: {\n\n"))
		}
		fl.Flush()
		if hold := f.hold(); hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}))
	return mux
}

func (f *fakeServer) hold() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamHold
}

func (f *fakeServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// startUnix serves f on a unix socket and returns a client for it.
func startUnix(t *testing.T, f *fakeServer, token string) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "cruxapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := NewClient(path, token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientVerbsOverUnixSocket(t *testing.T) {
	f := &fakeServer{token: "s3cret"}
	c := startUnix(t, f, "s3cret")
	ctx := context.Background()

	if c.Address().Network != "unix" {
		t.Fatalf("address %+v", c.Address())
	}
	h, err := c.Health(ctx)
	if err != nil || h.Agents != 2 || h.Version != Version {
		t.Fatalf("health %+v %v", h, err)
	}
	procs, err := c.List(ctx)
	if err != nil || len(procs) != 2 {
		t.Fatalf("list %+v %v", procs, err)
	}
	p, err := c.Get(ctx, "a b")
	if err != nil || p.Name != "a b" {
		t.Fatalf("get %+v %v", p, err)
	}
	resp, err := c.Run(ctx, RunRequest{Name: "fixer", Definition: json.RawMessage(`{"backend":"claude"}`)})
	if err != nil || resp.Agent != "fixer" || resp.Process == nil {
		t.Fatalf("run %+v %v", resp, err)
	}
	logs, err := c.Logs(ctx, "fixer", LogSourceBuffer, 7)
	if err != nil || logs.Source != LogSourceBuffer || logs.Lines[0] != "n=7" {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if logs, err = c.Logs(ctx, "fixer", "", 0); err != nil || logs.Lines[0] != "n=" {
		t.Fatalf("default logs %+v %v", logs, err)
	}
	if resp, err = c.Kick(ctx, "fixer", "go"); err != nil || resp.Agent != "fixer" {
		t.Fatalf("kick %+v %v", resp, err)
	}
	if resp, err = c.Stop(ctx, "fixer"); err != nil || resp.Agent != "fixer" {
		t.Fatalf("stop %+v %v", resp, err)
	}

	seen := strings.Join(f.seen(), "\n")
	for _, want := range []string{"GET /v1/health", "GET /v1/agents\n", "GET /v1/agents/a%20b", "POST /v1/agents\n",
		"GET /v1/agents/fixer/logs?n=7&source=buffer", "GET /v1/agents/fixer/logs?", "POST /v1/agents/fixer/kick", "POST /v1/agents/fixer/stop"} {
		if !strings.Contains(seen+"\n", want) {
			t.Errorf("missing %q in\n%s", want, seen)
		}
	}
	f.mu.Lock()
	bodies := strings.Join(f.bodies, "|")
	f.mu.Unlock()
	if bodies != `{"backend":"claude"}|go` {
		t.Fatalf("bodies %q", bodies)
	}
}

func TestClientErrors(t *testing.T) {
	f := &fakeServer{token: "s3cret"}
	ctx := context.Background()

	// Wrong token: 401 with the server's api.Error.
	c := startUnix(t, f, "wrong")
	_, err := c.Health(ctx)
	if !IsUnauthorized(err) || IsNotFound(err) || err.Error() != "token required" {
		t.Fatalf("unauthorized: %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized || se.Body.Code != CodeUnauthorized {
		t.Fatalf("status error %+v", se)
	}
	if err := c.FollowLogs(ctx, "x", func(LogChunk) error { return nil }); !IsUnauthorized(err) {
		t.Fatalf("follow unauthorized: %v", err)
	}

	c = startUnix(t, f, "s3cret")
	if _, err := c.Get(ctx, "ghost"); !IsNotFound(err) || err.Error() != "agent ghost not found" {
		t.Fatalf("not found: %v", err)
	}
	if _, err := c.Run(ctx, RunRequest{Name: "boom"}); err == nil || !strings.Contains(err.Error(), "tmux is down") {
		t.Fatalf("runtime error: %v", err)
	}
	// Non-JSON error body falls back to the status.
	if _, err := c.Stop(ctx, "plain"); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("plain error: %v", err)
	}
	if IsNotFound(errors.New("x")) || IsUnauthorized(nil) {
		t.Fatal("classifiers on non-status errors")
	}

	// Unreachable socket.
	dead, err := NewClient(filepath.Join(os.TempDir(), "crux-no-such.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.List(ctx); err == nil || !strings.Contains(err.Error(), "unix://") {
		t.Fatalf("dead socket: %v", err)
	}
	if err := dead.Events(ctx, func(Event) error { return nil }); err == nil {
		t.Fatal("dead socket events should fail")
	}

	// Bad address and bad request construction.
	if _, err := NewClient("", ""); err == nil {
		t.Fatal("empty address should fail")
	}
	bad, _ := NewClient("127.0.0.1:1", "")
	bad.addr.BaseURL = "http://[::1"
	if _, err := bad.Health(ctx); err == nil {
		t.Fatal("unparseable base URL should fail")
	}
	if err := bad.Events(ctx, func(Event) error { return nil }); err == nil {
		t.Fatal("unparseable base URL should fail for streams")
	}
	if _, err := bad.Run(ctx, RunRequest{Name: "x", Definition: json.RawMessage(`{`)}); err == nil {
		t.Fatal("unmarshalable body should fail")
	}

	// A JSON body that is not the expected shape.
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[not an object"))
	}))
	t.Cleanup(srvBad.Close)
	c, err = NewClient(srvBad.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(ctx); err == nil || !strings.Contains(err.Error(), "decoding GET") {
		t.Fatalf("decode error: %v", err)
	}
}

func TestClientStreams(t *testing.T) {
	f := &fakeServer{}
	c := startUnix(t, f, "")
	ctx := context.Background()

	var lines []string
	if err := c.FollowLogs(ctx, "fixer", func(ch LogChunk) error {
		lines = append(lines, ch.Lines...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "one,two" {
		t.Fatalf("lines %v", lines)
	}

	// The server reports the agent gone: the stream ends with an error.
	err := c.FollowLogs(ctx, "lost", func(LogChunk) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "agent lost not found") {
		t.Fatalf("lost: %v", err)
	}
	if err := c.FollowLogs(ctx, "garbage", func(LogChunk) error { return nil }); err == nil || !strings.Contains(err.Error(), "log stream") {
		t.Fatalf("garbage: %v", err)
	}
	// fn's error is returned.
	stop := errors.New("enough")
	if err := c.FollowLogs(ctx, "fixer", func(LogChunk) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("fn error: %v", err)
	}

	var evs []Event
	if err := c.Events(ctx, func(ev Event) error {
		evs = append(evs, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != EventStarted || evs[1].Type != EventKick || evs[1].Agent != "a" {
		t.Fatalf("events %+v", evs)
	}
	// Garbage on a stream read through the generic reader surfaces fn's error.
	err = c.stream(ctx, PathEvents+"?garbage=1", func(_ string, data []byte) error {
		var ev Event
		return json.Unmarshal(data, &ev)
	})
	if err == nil {
		t.Fatal("garbage via stream should fail")
	}

	// Cancelling the context ends a held stream without an error.
	hold := make(chan struct{})
	f.mu.Lock()
	f.streamHold = hold
	f.mu.Unlock()
	t.Cleanup(func() { close(hold) })
	cctx, cancel := context.WithCancel(ctx)
	got := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.FollowLogs(cctx, "fixer", func(LogChunk) error {
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no chunk before cancel")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled follow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end on cancel")
	}
}

func TestClientEventsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentTypeEvent)
		_, _ = w.Write([]byte("event: x\ndata: {\n\n"))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if c.Address().Network != "tcp" {
		t.Fatalf("address %+v", c.Address())
	}
	if err := c.Events(context.Background(), func(Event) error { return nil }); err == nil || !strings.Contains(err.Error(), "event stream") {
		t.Fatalf("decode error: %v", err)
	}
	// TCP error target is the base URL.
	deadTCP, _ := NewClient("127.0.0.1:1", "")
	if _, err := deadTCP.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "http://127.0.0.1:1") {
		t.Fatalf("dead tcp: %v", err)
	}
}

func TestClientSetHTTPClientAndWaitReady(t *testing.T) {
	f := &fakeServer{}
	c := startUnix(t, f, "")
	// A replacement client without a transport keeps the socket dialer.
	c.SetHTTPClient(&http.Client{Timeout: 5 * time.Second})
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One with its own transport is used as given.
	c.SetHTTPClient(&http.Client{Transport: http.DefaultTransport})
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("default transport cannot reach a unix base URL")
	}

	c = startUnix(t, f, "")
	if err := c.WaitReady(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	dead, _ := NewClient(filepath.Join(os.TempDir(), "crux-no-such.sock"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := dead.WaitReady(ctx, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dead wait: %v", err)
	}
	// Unauthorized is final: polling cannot fix it.
	wrong := startUnix(t, &fakeServer{token: "t"}, "bad")
	if err := wrong.WaitReady(context.Background(), time.Hour); !IsUnauthorized(err) {
		t.Fatalf("wrong token wait: %v", err)
	}
}

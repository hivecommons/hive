package dashboard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer() *Server {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewServer(0, logger)
}

func newTestMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	return mux
}

func minimalPayload() *StatusPayload {
	return &StatusPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Governor: FrontendGovernor{
			Active: true,
			Mode:   "IDLE",
			Issues: 2,
			PRs:    1,
			Thresholds: FrontendThresholds{
				Quiet: 2,
				Busy:  10,
				Surge: 20,
			},
		},
		Agents: []FrontendAgent{
			{Name: "scanner", State: "running", CLI: "claude", Model: "sonnet"},
		},
		Tokens: FrontendTokens{
			LookbackHours: 24,
			Sessions:      []FrontendSession{{ID: "test-1", Agent: "scanner", Model: "sonnet", Total: 1000, Messages: 5}},
			Totals:        FrontendTokenTotals{Input: 1000, Sessions: 1},
			ByAgent:       map[string]FrontendTokenBucket{"scanner": {Input: 1000}},
			ByModel:       map[string]FrontendTokenBucket{"sonnet": {Input: 1000}},
		},
		Repos:         []FrontendRepo{},
		Beads:         FrontendBeads{},
		Health:        map[string]any{"ci": 100},
		Budget:        FrontendBudget{},
		CadenceMatrix: []FrontendCadence{},
		GHRateLimits:  map[string]any{},
		AgentMetrics:  map[string]any{},
		Hold:          FrontendHold{Items: []any{}},
		IssueToMerge:  map[string]any{},
	}
}

func TestNewServer_DefaultFields(t *testing.T) {
	s := newTestServer()
	if s.port != 0 {
		t.Errorf("expected port 0, got %d", s.port)
	}
	if s.sseClients == nil {
		t.Error("sseClients map should be initialised")
	}
	if s.status != nil {
		t.Error("status should be nil on fresh server")
	}
	if s.logger == nil {
		t.Error("logger should not be nil")
	}
}

func TestAllRoutesRegisterWithoutPanic(t *testing.T) {
	s := newTestServer()
	s.registerContributeRoutes()
}

func TestHandleHealth_StatusCode(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())
	s.MarkReady()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}
}

func TestHandleHealth_ContentType(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
}

func TestHandleHealth_Body(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())
	s.MarkReady()
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if payload["status"] != "ok" {
		t.Errorf(`expected {"status":"ok"}, got %v`, payload)
	}
}

func TestHandleStatus_NilStatus_StatusCode(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestHandleStatus_NilStatus_ReturnsInitializing(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if payload["status"] != "initializing" {
		t.Errorf(`expected "initializing", got %q`, payload["status"])
	}
}

func TestHandleStatus_NilStatus_ContentType(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
}

func TestHandleStatus_AfterUpdate_ReturnsPayload(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	var payload StatusPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if payload.Governor.Mode != "IDLE" {
		t.Errorf("unexpected governor mode: %q", payload.Governor.Mode)
	}
	found := false
	for _, a := range payload.Agents {
		if a.Name == "scanner" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected scanner agent in payload")
	}
}

func TestHandleStatus_AfterUpdate_DoesNotReturnInitializing(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	body := rec.Body.String()
	if strings.Contains(body, `"initializing"`) {
		t.Errorf("should not contain 'initializing' after UpdateStatus, got: %s", body)
	}
}

func TestUpdateStatus_SetsTimestamp(t *testing.T) {
	s := newTestServer()
	before := time.Now().Add(-time.Second)
	p := minimalPayload()
	p.Timestamp = ""
	s.UpdateStatus(p)
	after := time.Now().Add(time.Second)

	s.statusMu.RLock()
	ts := s.status.Timestamp
	s.statusMu.RUnlock()

	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("failed to parse timestamp %q: %v", ts, err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("timestamp %v not within [%v, %v]", parsed, before, after)
	}
}

func TestUpdateStatus_StoresStatus(t *testing.T) {
	s := newTestServer()
	p := minimalPayload()
	s.UpdateStatus(p)

	s.statusMu.RLock()
	stored := s.status
	s.statusMu.RUnlock()

	if stored == nil {
		t.Fatal("stored status is nil")
	}
	if stored.Governor.Issues != 2 {
		t.Errorf("expected Issues=2, got %d", stored.Governor.Issues)
	}
}

func TestUpdateStatus_OverwritesPreviousStatus(t *testing.T) {
	s := newTestServer()
	p1 := minimalPayload()
	p1.Governor.Issues = 5
	s.UpdateStatus(p1)

	p2 := minimalPayload()
	p2.Governor.Issues = 99
	s.UpdateStatus(p2)

	s.statusMu.RLock()
	issues := s.status.Governor.Issues
	s.statusMu.RUnlock()

	if issues != 99 {
		t.Errorf("expected Issues=99 after second update, got %d", issues)
	}
}

func TestUpdateStatus_BroadcastsToClients(t *testing.T) {
	s := newTestServer()

	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	s.UpdateStatus(minimalPayload())

	select {
	case frame := <-ch:
		raw := strings.TrimPrefix(string(frame), "data: ")
		raw = strings.TrimSpace(raw)
		var payload StatusPayload
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("unmarshal error: %v (frame=%q)", err, string(frame))
		}
		if payload.Governor.Mode != "IDLE" {
			t.Errorf("unexpected mode in broadcast: %q", payload.Governor.Mode)
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for broadcast")
	}
}

func sseTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := newTestServer()
	mux := newTestMux(s)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, s
}

func TestHandleSSE_Headers(t *testing.T) {
	ts, _ := sseTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("request error: %v", err)
	}
	if resp == nil {
		t.Fatal("no response")
	}
	defer resp.Body.Close()

	checks := map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		"Connection":    "keep-alive",
	}
	for header, want := range checks {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("header %q: want %q, got %q", header, want, got)
		}
	}
}

func TestHandleSSE_StatusCode(t *testing.T) {
	ts, _ := sseTestServer(t)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleSSE_SendsRetryLine(t *testing.T) {
	ts, _ := sseTestServer(t)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(2 * time.Second)
	for {
		done := make(chan bool, 1)
		go func() { done <- scanner.Scan() }()
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("SSE stream closed before retry line")
			}
			line := scanner.Text()
			if strings.HasPrefix(line, "retry:") {
				if !strings.Contains(line, "3000") {
					t.Errorf("unexpected retry value: %q", line)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for retry line")
		}
	}
}

func TestHandleSSE_InitialSnapshot_WhenStatusExists(t *testing.T) {
	ts, s := sseTestServer(t)

	s.UpdateStatus(minimalPayload())

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(3 * time.Second)
	for {
		done := make(chan bool, 1)
		go func() { done <- scanner.Scan() }()
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("stream closed before snapshot")
			}
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				jsonPart := strings.TrimPrefix(line, "data: ")
				var payload StatusPayload
				if err := json.Unmarshal([]byte(jsonPart), &payload); err != nil {
					t.Fatalf("unmarshal error: %v — line: %q", err, line)
				}
				if payload.Governor.Mode != "IDLE" {
					t.Errorf("unexpected mode in snapshot: %q", payload.Governor.Mode)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for initial snapshot")
		}
	}
}

func TestHandleSSE_NoInitialSnapshot_WhenStatusNil(t *testing.T) {
	ts, _ := sseTestServer(t)

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(400 * time.Millisecond)
	for {
		done := make(chan bool, 1)
		go func() { done <- scanner.Scan() }()
		select {
		case ok := <-done:
			if !ok {
				return
			}
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				t.Errorf("unexpected data line when status is nil: %q", line)
			}
		case <-deadline:
			return
		}
	}
}

func TestHandleSSE_ReceivesBroadcast(t *testing.T) {
	ts, s := sseTestServer(t)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)

	p := minimalPayload()
	p.Governor.PRs = 42
	s.UpdateStatus(p)

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(3 * time.Second)
	for {
		done := make(chan bool, 1)
		go func() { done <- scanner.Scan() }()
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("stream closed before broadcast")
			}
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				jsonPart := strings.TrimPrefix(line, "data: ")
				var payload StatusPayload
				if err := json.Unmarshal([]byte(jsonPart), &payload); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if payload.Governor.PRs == 42 {
					return
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for broadcast data event")
		}
	}
}

func TestHandleSSE_ClientCleanupOnDisconnect(t *testing.T) {
	ts, s := sseTestServer(t)

	s.sseMu.Lock()
	before := len(s.sseClients)
	s.sseMu.Unlock()

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	resp.Body.Close()

	time.Sleep(150 * time.Millisecond)

	s.sseMu.Lock()
	after := len(s.sseClients)
	s.sseMu.Unlock()

	if after != before {
		t.Errorf("expected %d clients after disconnect, got %d", before, after)
	}
}

func TestBroadcast_MultipleClients(t *testing.T) {
	s := newTestServer()

	const numClients = 5
	channels := make([]chan []byte, numClients)
	for i := range channels {
		ch := make(chan []byte, 4)
		channels[i] = ch
		s.sseMu.Lock()
		s.sseClients[ch] = struct{}{}
		s.sseMu.Unlock()
	}

	p := minimalPayload()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	frame := fmt.Sprintf("data: %s\n\n", data)
	s.broadcastFrame(frame)

	for i, ch := range channels {
		select {
		case got := <-ch:
			if string(got) != frame {
				t.Errorf("client %d: unexpected payload", i)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: timed out waiting for broadcast", i)
		}
	}
}

func TestBroadcast_DropsEventForSlowClient(t *testing.T) {
	s := newTestServer()

	full := make(chan []byte, 1)
	full <- []byte("old-data")

	fast := make(chan []byte, 4)

	s.sseMu.Lock()
	s.sseClients[full] = struct{}{}
	s.sseClients[fast] = struct{}{}
	s.sseMu.Unlock()

	data, _ := json.Marshal(minimalPayload())
	frame := fmt.Sprintf("data: %s\n\n", data)
	s.broadcastFrame(frame)

	select {
	case got := <-fast:
		if string(got) != frame {
			t.Error("fast client: unexpected payload")
		}
	case <-time.After(time.Second):
		t.Error("fast client: timed out")
	}

	if len(full) != 1 {
		t.Errorf("slow client channel length: want 1, got %d", len(full))
	}
	old := <-full
	if string(old) != "old-data" {
		t.Errorf("slow client: expected old-data, got %q", old)
	}
}

func TestBroadcast_NoClients(t *testing.T) {
	s := newTestServer()
	data, _ := json.Marshal(minimalPayload())
	s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))
}

func TestConcurrency_UpdateAndReadStatus(t *testing.T) {
	s := newTestServer()

	var wg sync.WaitGroup
	const workers = 20

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.UpdateStatus(minimalPayload())
		}()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		}()
	}

	wg.Wait()
}

func TestConcurrency_BroadcastWithClientChurn(t *testing.T) {
	s := newTestServer()
	data, _ := json.Marshal(minimalPayload())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan []byte, 4)
			s.sseMu.Lock()
			s.sseClients[ch] = struct{}{}
			s.sseMu.Unlock()

			s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))

			s.sseMu.Lock()
			delete(s.sseClients, ch)
			s.sseMu.Unlock()
		}()
	}
	wg.Wait()
}

func TestHandleSSE_MultipleSequentialBroadcasts(t *testing.T) {
	ts, s := sseTestServer(t)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil && resp == nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)

	const broadcasts = 3
	for i := 0; i < broadcasts; i++ {
		p := minimalPayload()
		p.Governor.Issues = i + 1
		s.UpdateStatus(p)
	}

	scanner := bufio.NewScanner(resp.Body)
	seen := 0
	deadline := time.After(4 * time.Second)
	for seen < broadcasts {
		done := make(chan bool, 1)
		go func() { done <- scanner.Scan() }()
		select {
		case ok := <-done:
			if !ok {
				t.Fatalf("stream closed after %d broadcasts", seen)
			}
			if strings.HasPrefix(scanner.Text(), "data:") {
				seen++
			}
		case <-deadline:
			t.Fatalf("timed out after receiving %d/%d broadcasts", seen, broadcasts)
		}
	}
}

type plainResponseWriter struct {
	headers http.Header
	code    int
	body    strings.Builder
}

func newPlainResponseWriter() *plainResponseWriter {
	return &plainResponseWriter{headers: make(http.Header)}
}

func (w *plainResponseWriter) Header() http.Header         { return w.headers }
func (w *plainResponseWriter) WriteHeader(code int)        { w.code = code }
func (w *plainResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func TestHandleSSE_NonFlusher_Returns500(t *testing.T) {
	s := newTestServer()
	w := newPlainResponseWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	s.handleSSE(w, req)

	if w.code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.code)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestStart_ServesEndpoints(t *testing.T) {
	port := freePort(t)
	s := NewServer(port, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	s.UpdateStatus(minimalPayload())
	s.MarkReady()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start()
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.Get(addr + "/api/health")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("server did not start within 2 s")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health: expected 200, got %d", resp.StatusCode)
	}

	resp2, err := client.Get(addr + "/api/status")
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status: expected 200, got %d", resp2.StatusCode)
	}

	resp3, err := client.Get(addr + "/")
	if err != nil {
		t.Fatalf("static request: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("static index: expected 200, got %d", resp3.StatusCode)
	}
}

func TestSecurityHeaders_Present(t *testing.T) {
	s := newTestServer()
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	headers := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"X-Xss-Protection":      "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for name, want := range headers {
		got := resp.Header.Get(name)
		if got != want {
			t.Errorf("header %q = %q, want %q", name, got, want)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP missing default-src: %q", csp)
	}
}

func TestAuthMiddleware_RejectsUnauthorized(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret-token-123", logger)
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_AcceptsBearerToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret-token-123", logger)
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer secret-token-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for authorized request, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_AcceptsQueryToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret-token-123", logger)
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status?token=secret-token-123")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for query-token request, got %d", resp.StatusCode)
	}
}

func TestAuthMiddleware_HealthBypassesAuth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret-token-123", logger)
	s.UpdateStatus(minimalPayload())
	s.MarkReady()
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for /api/health (no auth required), got %d", resp.StatusCode)
	}
}

func TestNoAuth_AllEndpointsAccessible(t *testing.T) {
	s := newTestServer()
	handler := s.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 when auth is not configured, got %d", resp.StatusCode)
	}
}

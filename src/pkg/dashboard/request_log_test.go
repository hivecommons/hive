package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logread"
)

const requestLogTestToken = "request-log-good-dashboard-token"

// useOutputLogDir points the request log and the output sources at a fresh
// directory for one test.
func useOutputLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	saved := outputLogDir
	outputLogDir = func() string { return dir }
	t.Cleanup(func() { outputLogDir = saved })
	return dir
}

// requestLogServer is a token-protected dashboard with the request log open
// and three test routes: a plain JSON reply, a streamed reply that flushes
// between chunks, and a websocket echo.
func requestLogServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServerWithAuth(0, requestLogTestToken, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.openRequestLog()
	if s.requestLog == nil {
		t.Fatal("openRequestLog left the request log closed")
	}
	s.mux.HandleFunc("GET /api/rltest/plain", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]string{"ok": "yes"})
	})
	s.mux.HandleFunc("GET /api/rltest/stream", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first;"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the request log: %v", err)
		}
		_, _ = w.Write([]byte("second"))
	})
	upgrader := websocket.Upgrader{}
	s.mux.HandleFunc("GET /api/rltest/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade through the request log: %v", err)
			return
		}
		defer conn.Close()
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(mt, append([]byte("echo:"), msg...))
		_, _, _ = conn.ReadMessage()
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func getWith(t *testing.T, target, authHeader string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type requestRecord struct {
	Level      string  `json:"level"`
	Msg        string  `json:"msg"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	DurationMS float64 `json:"duration_ms"`
	Auth       string  `json:"auth"`
}

// readRequests reads the requests source back through the log read engine.
func readRequests(t *testing.T, dir string) ([]requestRecord, *logread.Result) {
	t.Helper()
	res := logread.Read(logread.Request{Source: logread.SourceRequests, Lines: logread.MaxLines}, logread.Options{OutputDir: dir})
	if res.Status != logread.StatusOK {
		t.Fatalf("requests source: %s (%s)", res.Status, res.Reason)
	}
	out := make([]requestRecord, 0, len(res.Lines))
	for _, line := range res.Lines {
		var r requestRecord
		if err := json.Unmarshal([]byte(line.Text), &r); err != nil {
			t.Fatalf("request record %q: %v", line.Text, err)
		}
		out = append(out, r)
	}
	return out, res
}

func findRequest(records []requestRecord, path string) (requestRecord, bool) {
	for _, r := range records {
		if r.Path == path {
			return r, true
		}
	}
	return requestRecord{}, false
}

// AC5: method, path with query values removed, status, duration and the
// authentication outcome for each kind of request.
func TestRequestLogRecordsOutcomes(t *testing.T) {
	dir := useOutputLogDir(t)
	_, srv := requestLogServer(t)

	for _, tc := range []struct {
		target, header string
	}{
		{"/api/health", ""},
		{"/api/rltest/plain?view=full", "Bearer " + requestLogTestToken},
		{"/api/rltest/plain?bad=1", "Bearer not-the-dashboard-token"},
		{"/api/rltest/plain?anon=1", ""},
		{"/api/rltest/plain?token=abc&lines=5", ""},
	} {
		resp := getWith(t, srv.URL+tc.target, tc.header)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	records, res := readRequests(t, dir)
	for _, want := range []struct {
		path   string
		status int
		auth   string
		level  string
	}{
		{"/api/rltest/plain?view=", http.StatusOK, authOutcomeOK, "INFO"},
		{"/api/rltest/plain?bad=", http.StatusUnauthorized, authOutcomeFailed, "WARN"},
		{"/api/rltest/plain?anon=", http.StatusUnauthorized, authOutcomeNone, "WARN"},
		{"/api/rltest/plain?token=&lines=", http.StatusUnauthorized, authOutcomeFailed, "WARN"},
	} {
		got, ok := findRequest(records, want.path)
		if !ok {
			t.Fatalf("no record for %s in %+v", want.path, records)
		}
		if got.Method != http.MethodGet || got.Status != want.status || got.Auth != want.auth || got.Level != want.level || got.Msg != "request" || got.DurationMS < 0 {
			t.Errorf("%s: %+v, want status %d auth %s level %s", want.path, got, want.status, want.auth, want.level)
		}
	}
	health, ok := findRequest(records, "/api/health")
	if !ok || health.Auth != authOutcomePublic {
		t.Errorf("health check = %+v (found %v), want auth public", health, ok)
	}
	for _, line := range res.Lines {
		if strings.Contains(line.Text, "abc") || strings.Contains(line.Text, requestLogTestToken) {
			t.Errorf("a query value or credential reached the request log: %s", line.Text)
		}
		if line.Time == "" || line.Level == "" {
			t.Errorf("request line without time or level: %+v", line)
		}
	}
	info, err := os.Stat(filepath.Join(dir, logread.SourceRequests+".log"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("request log file mode = %v (%v), want 0600", info, err)
	}
}

// AC5: a websocket connection and a streamed response still work while being
// recorded.
func TestRequestLogKeepsWebsocketAndStreaming(t *testing.T) {
	dir := useOutputLogDir(t)
	_, srv := requestLogServer(t)
	auth := "Bearer " + requestLogTestToken

	resp := getWith(t, srv.URL+"/api/rltest/stream", auth)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "first;second" {
		t.Fatalf("streamed reply = %d %q", resp.StatusCode, body)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/rltest/ws"
	conn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {auth}})
	if err != nil {
		t.Fatalf("websocket dial through the request log: %v", err)
	}
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status %d", wsResp.StatusCode)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "echo:ping" {
		t.Fatalf("websocket echo = %q, %v", msg, err)
	}
	conn.Close()

	testutil.Eventually(t, 5*time.Second, func() bool {
		records, _ := readRequests(t, dir)
		ws, ok := findRequest(records, "/api/rltest/ws")
		return ok && ws.Status == http.StatusSwitchingProtocols && ws.Auth == authOutcomeOK
	}, "websocket request not recorded as a 101 with auth ok")
	records, _ := readRequests(t, dir)
	if stream, ok := findRequest(records, "/api/rltest/stream"); !ok || stream.Status != http.StatusOK || stream.Auth != authOutcomeOK {
		t.Fatalf("streamed request record = %+v (found %v)", stream, ok)
	}
}

type markerHandler struct{ id int }

func (markerHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestRequestLogDisabledLeavesChainUnwrapped(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	inner := markerHandler{id: 7}
	if got, ok := s.withRequestLog(inner).(markerHandler); !ok || got.id != 7 {
		t.Fatal("withRequestLog wrapped the chain with the request log closed")
	}
	if got, ok := s.markAuthenticated(inner).(markerHandler); !ok || got.id != 7 {
		t.Fatal("markAuthenticated wrapped the chain with the request log closed")
	}

	// Open, the marker passes the request on and notes the admission only
	// when the request log's state rides on the request.
	s.requestLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	marked := s.markAuthenticated(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	marked.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("markAuthenticated must pass the request on")
	}
}

func TestOpenRequestLogFailureAndReuse(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	saved := outputLogDir
	t.Cleanup(func() { outputLogDir = saved })
	outputLogDir = func() string { return filepath.Join(blocker, "logs") }

	var logs bytes.Buffer
	s := &Server{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	s.openRequestLog()
	if s.requestLog != nil || !strings.Contains(logs.String(), "request log unavailable") {
		t.Fatalf("unopenable request log: logger=%v logs=%q", s.requestLog, logs.String())
	}
	(&Server{}).openRequestLog() // no logger: the failure is silent

	existing := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.requestLog = existing
	s.openRequestLog()
	if s.requestLog != existing {
		t.Fatal("openRequestLog replaced an open request log")
	}
}

func TestRequestLogHelpers(t *testing.T) {
	for _, tc := range []struct {
		public, admitted, presented bool
		want                        string
	}{
		{true, false, true, authOutcomePublic},
		{false, true, true, authOutcomeOK},
		{false, false, true, authOutcomeFailed},
		{false, false, false, authOutcomeNone},
	} {
		if got := authOutcome(tc.public, tc.admitted, tc.presented); got != tc.want {
			t.Errorf("authOutcome(%v,%v,%v) = %s, want %s", tc.public, tc.admitted, tc.presented, got, tc.want)
		}
	}
	for status, want := range map[int]slog.Level{200: slog.LevelInfo, 101: slog.LevelInfo, 404: slog.LevelWarn, 503: slog.LevelError} {
		if got := requestLogLevel(status); got != want {
			t.Errorf("requestLogLevel(%d) = %v, want %v", status, got, want)
		}
	}
	for raw, want := range map[string]string{
		"/api/x":                      "/api/x",
		"/api/x?token=abc&flag&a=b=c": "/api/x?token=&flag=&a=",
		"/a%2Fb?q=1":                  "/a%2Fb?q=",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := requestLogPath(u); got != want {
			t.Errorf("requestLogPath(%q) = %q, want %q", raw, got, want)
		}
	}
	long, _ := url.Parse("/" + strings.Repeat("p", 3*requestLogPathMax))
	if got := requestLogPath(long); len(got) != requestLogPathMax {
		t.Errorf("long path recorded at %d bytes, want %d", len(got), requestLogPathMax)
	}

	for name, mutate := range map[string]func(r *http.Request){
		"bearer":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer x") },
		"internal": func(r *http.Request) { r.Header.Set("X-Hive-Internal", "x") },
		"proof":    func(r *http.Request) { r.Header.Set(proxyAuthHeader, "x") },
		"identity": func(r *http.Request) { r.Header.Set("X-Hive-User", "someone") },
		"query":    func(r *http.Request) { r.URL.RawQuery = "token=x" },
		"handoff":  func(r *http.Request) { r.URL.RawQuery = terminalHandoffCodeParam + "=x" },
		"session":  func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "x"}) },
		"terminal": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: terminalAssertionCookieName, Value: "x"}) },
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		mutate(r)
		if !presentsCredentials(r) {
			t.Errorf("%s: credentials not seen", name)
		}
	}
	bare := httptest.NewRequest(http.MethodGet, "/api/x?lines=5", nil)
	bare.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	if presentsCredentials(bare) {
		t.Error("a request with no credentials was seen as presenting some")
	}
}

type pushReadFromWriter struct {
	*httptest.ResponseRecorder
	pushed   string
	readFrom bool
}

func (w *pushReadFromWriter) Push(target string, _ *http.PushOptions) error {
	w.pushed = target
	return nil
}

func (w *pushReadFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFrom = true
	return io.Copy(w.ResponseRecorder, r)
}

func TestRequestLogResponseWriterPassThrough(t *testing.T) {
	// A writer with none of the optional interfaces beyond Flush.
	plain := &requestLogResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if plain.finalStatus() != http.StatusOK {
		t.Fatalf("no response written: status %d, want the implicit 200", plain.finalStatus())
	}
	if _, _, err := plain.Hijack(); err != http.ErrNotSupported || plain.hijacked {
		t.Fatalf("Hijack on a non-hijacker = %v (hijacked=%v)", err, plain.hijacked)
	}
	if err := plain.Push("/x", nil); err != http.ErrNotSupported {
		t.Fatalf("Push on a non-pusher = %v", err)
	}
	if n, err := plain.ReadFrom(strings.NewReader("abc")); n != 3 || err != nil {
		t.Fatalf("ReadFrom fallback = %d, %v", n, err)
	}
	if plain.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}

	informational := &requestLogResponseWriter{ResponseWriter: httptest.NewRecorder()}
	informational.WriteHeader(http.StatusEarlyHints)
	informational.WriteHeader(http.StatusNoContent)
	informational.WriteHeader(http.StatusTeapot)
	if informational.finalStatus() != http.StatusNoContent {
		t.Fatalf("status after 103 then 204 = %d, want 204", informational.finalStatus())
	}

	flushed := &requestLogResponseWriter{ResponseWriter: httptest.NewRecorder()}
	flushed.Flush()
	if flushed.status != http.StatusOK {
		t.Fatalf("Flush before a header noted %d, want 200", flushed.status)
	}
	flushed.ResponseWriter = struct{ http.ResponseWriter }{httptest.NewRecorder()}
	flushed.Flush() // a writer that cannot flush is left alone

	full := &pushReadFromWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &requestLogResponseWriter{ResponseWriter: full}
	if err := w.Push("/asset.js", nil); err != nil || full.pushed != "/asset.js" {
		t.Fatalf("Push passthrough = %v, pushed %q", err, full.pushed)
	}
	if _, err := w.ReadFrom(strings.NewReader("body")); err != nil || !full.readFrom || w.status != http.StatusOK {
		t.Fatalf("ReadFrom passthrough = %v (used %v, status %d)", err, full.readFrom, w.status)
	}

	hijacked := &requestLogResponseWriter{hijacked: true}
	if hijacked.finalStatus() != http.StatusSwitchingProtocols {
		t.Fatalf("hijacked with no status = %d, want 101", hijacked.finalStatus())
	}
}

// AC4/AC3: which output sources a dashboard reports, and why.
func TestLogSourcesAvailability(t *testing.T) {
	dir := useOutputLogDir(t)
	proxyEntry := filepath.Join(t.TempDir(), "server.js")
	savedEntry := authProxyEntryPath
	t.Cleanup(func() { authProxyEntryPath = savedEntry })

	byName := func(list logread.SourcesResult) map[string]logread.SourceInfo {
		out := map[string]logread.SourceInfo{}
		for _, src := range list.Sources {
			out[src.Name] = src
		}
		return out
	}

	// Hub-like image: no auth proxy installed, local litellm off.
	authProxyEntryPath = proxyEntry
	s := logTestServer(t, writeHiveLog(t, 1))
	got := byName(logread.Sources(s.logReadOptions(nil)))
	if got[logread.SourceProxy].Available || got[logread.SourceProxy].Reason == "" {
		t.Errorf("proxy with no auth proxy installed = %+v", got[logread.SourceProxy])
	}
	if got[logread.SourceLiteLLM].Available || got[logread.SourceLiteLLM].Reason == "" {
		t.Errorf("litellm with the local proxy off = %+v", got[logread.SourceLiteLLM])
	}
	for _, name := range []string{logread.SourceHiveOutput, logread.SourceRequests} {
		if !got[name].Available {
			t.Errorf("%s must be available on every deployment: %+v", name, got[name])
		}
	}

	// Spoke image with the local litellm proxy on.
	if err := os.WriteFile(proxyEntry, []byte("// proxy"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.LiteLLM.LocalProxy = true
	opts := s.logReadOptions(nil)
	if opts.OutputDir != dir || !opts.AuthProxyInstalled || !opts.LocalLiteLLM {
		t.Fatalf("options = %+v", opts)
	}
	got = byName(logread.Sources(opts))
	if !got[logread.SourceProxy].Available || !got[logread.SourceLiteLLM].Available {
		t.Errorf("spoke sources = %+v", got)
	}

	authProxyEntryPath = filepath.Dir(proxyEntry)
	if authProxyInstalled() {
		t.Error("a directory at the auth proxy path counted as an installed proxy")
	}

	// The REST route serves the new sources, and the read role may list them.
	rec := serveLog(s, config.RoleRead, "/api/logs/sources")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"requests"`) {
		t.Fatalf("sources route = %d %s", rec.Code, rec.Body.String())
	}
}

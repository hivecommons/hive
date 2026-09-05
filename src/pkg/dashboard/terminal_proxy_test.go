package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// startFakeTtyd runs a local HTTP server standing in for ttyd and points
// HIVE_TTYD_PORT at it. It records the last path+query it was asked for so
// tests can assert the /terminal prefix is stripped and the query survives.
func startFakeTtyd(t *testing.T) (lastURL *string) {
	t.Helper()
	var last string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ttyd-ok")
	}))
	t.Cleanup(backend.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting backend addr: %v", err)
	}
	t.Setenv("HIVE_TTYD_PORT", port)
	return &last
}

func newTerminalTestHandler(t *testing.T, authToken string) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if authToken == "" {
		return NewServer(0, logger).Handler()
	}
	t.Setenv(hub.EnvTerminalKey, "terminal-proxy-test-key")
	s := NewServerWithAuth(0, authToken, logger)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "terminal-proxy-test"}}
	return s.Handler()
}

// The dashboard builds terminal links as /terminal/?arg=hive-<agent> (see
// terminalUrl in static/index.html). On hosted spokes whose route sends the
// whole host to the Go server, that exact URL returned the FileServer's bare
// 404 — the regression under test. It must reach ttyd with the prefix
// stripped and the ?arg= query intact.
func TestTerminalProxyServesDashboardLink(t *testing.T) {
	last := startFakeTtyd(t)
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /terminal/?arg=hive-quality: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if *last != "/?arg=hive-quality" {
		t.Fatalf("backend saw %q, want %q", *last, "/?arg=hive-quality")
	}
}

func TestTerminalProxyPathRewrites(t *testing.T) {
	last := startFakeTtyd(t)
	h := newTerminalTestHandler(t, "")

	cases := []struct{ in, want string }{
		{"/terminal", "/"},
		{"/terminal/", "/"},
		{"/terminal/token", "/token"},
		{"/terminal/ws?arg=hive-quality", "/ws?arg=hive-quality"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.in, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want 200", c.in, rec.Code)
			continue
		}
		if *last != c.want {
			t.Errorf("GET %s: backend saw %q, want %q", c.in, *last, c.want)
		}
	}
}

// Terminal access must sit behind the same auth gate as the rest of the
// dashboard: no credential → 401, ?token= is rejected, and a short-lived
// handoff code minted by an authenticated request is accepted once.
func TestTerminalProxyAuth(t *testing.T) {
	last := startFakeTtyd(t)
	const token = "test-terminal-token"
	h := newTerminalTestHandler(t, token)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality&token="+token, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("with ?token=: got %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, terminalHandoffPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create handoff: got %d (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code == "" {
		t.Fatalf("handoff response did not include code: body=%q err=%v", rec.Body.String(), err)
	}
	code := body.Code

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality&code="+code, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("with handoff code: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(*last, "code=") || strings.Contains(*last, "token=") {
		t.Fatalf("backend saw sensitive query value: %q", *last)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality&code="+code, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed handoff code: got %d, want 401", rec.Code)
	}
}

// When ttyd is down the proxy must answer 502 "Terminal unavailable" (the
// Node proxy's contract), never the FileServer's bare 404.
func TestTerminalProxyTtydDown(t *testing.T) {
	// Reserve a port with nothing listening on it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	t.Setenv("HIVE_TTYD_PORT", strconv.Itoa(port))
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ttyd down: got %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Terminal unavailable") {
		t.Fatalf("ttyd down: body %q, want it to mention Terminal unavailable", rec.Body.String())
	}
}

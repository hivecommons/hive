package dashboard

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// startFakeTtydRecordingAuth stands in for ttyd and records the Authorization
// header of the last request. Plain GETs answer 200; a websocket upgrade is
// completed and immediately closed so the proxy's upgrade pass-through can be
// asserted on the same header.
func startFakeTtydRecordingAuth(t *testing.T) (lastAuth *string) {
	t.Helper()
	var last string
	upgrader := websocket.Upgrader{Subprotocols: []string{"tty"}}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.Header.Get("Authorization")
		if websocket.IsWebSocketUpgrade(r) {
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_ = c.Close()
			return
		}
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

func basicHeader(cred string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
}

// The entrypoint starts ttyd with -c hive:<HIVE_DASHBOARD_TOKEN> (or an explicit
// HIVE_TTYD_CREDENTIAL). The proxy must present that exact credential to ttyd
// on every forwarded request, or ttyd re-challenges the browser the dashboard
// has already authenticated with a native user/password dialog.
func TestTerminalProxyTtydCredential(t *testing.T) {
	cases := []struct {
		name        string
		token       string
		credential  string
		browserAuth string
		wantAuth    string
	}{
		{
			name:     "dashboard token derives hive:<token>",
			token:    "dash-tok-123",
			wantAuth: basicHeader("hive:dash-tok-123"),
		},
		{
			name:       "explicit HIVE_TTYD_CREDENTIAL wins over the token",
			token:      "dash-tok-123",
			credential: "ops:override-pass",
			wantAuth:   basicHeader("ops:override-pass"),
		},
		{
			name:       "HIVE_TTYD_CREDENTIAL alone",
			credential: "ops:only",
			wantAuth:   basicHeader("ops:only"),
		},
		{
			name:        "credential keeps colons in the password",
			credential:  "hive:pa:ss",
			wantAuth:    basicHeader("hive:pa:ss"),
			browserAuth: "",
		},
		{
			name:     "neither set forwards no Authorization",
			wantAuth: "",
		},
		{
			name:        "browser Authorization is replaced, not forwarded",
			token:       "dash-tok-123",
			browserAuth: "Bearer browser-dashboard-token",
			wantAuth:    basicHeader("hive:dash-tok-123"),
		},
		{
			name:        "browser Authorization is dropped when ttyd is uncredentialed",
			browserAuth: basicHeader("hive:guess"),
			wantAuth:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_DASHBOARD_TOKEN", tc.token)
			t.Setenv("HIVE_TTYD_CREDENTIAL", tc.credential)
			last := startFakeTtydRecordingAuth(t)
			h := newTerminalTestHandler(t, "")

			req := httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil)
			if tc.browserAuth != "" {
				req.Header.Set("Authorization", tc.browserAuth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if *last != tc.wantAuth {
				t.Fatalf("ttyd saw Authorization %q, want %q", *last, tc.wantAuth)
			}
		})
	}
}

// ttyd checks its credential on the websocket upgrade request too (check_auth
// in its protocol.c). The upgrade goes through the same Rewrite, so the header
// must ride along with it.
func TestTerminalProxyTtydCredentialOnWebsocketUpgrade(t *testing.T) {
	t.Setenv("HIVE_DASHBOARD_TOKEN", "ws-tok")
	t.Setenv("HIVE_TTYD_CREDENTIAL", "")
	last := startFakeTtydRecordingAuth(t)
	front := httptest.NewServer(newTerminalTestHandler(t, ""))
	t.Cleanup(front.Close)

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/terminal/ws?arg=hive-quality"
	hdr := http.Header{"Authorization": {"Bearer browser-dashboard-token"}}
	dialer := websocket.Dialer{Subprotocols: []string{"tty"}}
	conn, resp, err := dialer.Dial(wsURL, hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial through proxy: %v (status %d)", err, status)
	}
	_ = conn.Close()
	if want := basicHeader("hive:ws-tok"); *last != want {
		t.Fatalf("ttyd saw Authorization %q on upgrade, want %q", *last, want)
	}
}

// The credential half is the dashboard token on default deployments; the
// proxy's logger must never see it.
func TestTerminalProxyCredentialNotLogged(t *testing.T) {
	t.Setenv("HIVE_DASHBOARD_TOKEN", "never-log-me")
	t.Setenv("HIVE_TTYD_CREDENTIAL", "")
	t.Setenv("HIVE_TTYD_PORT", "1")
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewServer(0, logger).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 with ttyd down", rec.Code)
	}
	if strings.Contains(logs.String(), "never-log-me") || strings.Contains(logs.String(), basicHeader("hive:never-log-me")) {
		t.Fatalf("credential leaked into logs: %s", logs.String())
	}
}

package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a Client at a fixture server, bypassing New() so a test
// never depends on the ambient environment.
func newTestClient(t *testing.T, server *httptest.Server, token string) *Client {
	t.Helper()
	c := New()
	c.baseURL = server.URL
	c.token = token
	return c
}

// TestHealthSendsExpectedRequest pins the three things about the request that a
// server cares about: the path, the method, and the Authorization header.
//
// The header spelling is the load-bearing assertion. The published spec carries
// no security scheme (#4912), so `Bearer` here is transcribed from the two
// implementations that actually verify it — the proxy's requireAuth and
// hivectl's client — and this test is what keeps it transcribed correctly.
func TestHealthSendsExpectedRequest(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newTestClient(t, server, "s3cret").Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}

	if gotPath != "/api/health" {
		t.Errorf("path = %q, want /api/health", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cret")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

// TestHealthOmitsAuthorizationWithoutToken covers the unauthenticated path.
//
// /api/health is a public path on the Go API, so a tokenless health check must
// work — it is how the TUI tells "dashboard unreachable" apart from "token
// wrong". Sending an empty `Bearer ` would be worse than sending nothing: the
// proxy's requireAuth matches `Bearer (.+)` and a bare prefix fails the regex,
// turning a public endpoint into a 401.
func TestHealthOmitsAuthorizationWithoutToken(t *testing.T) {
	var hadAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newTestClient(t, server, "").Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}
	if hadAuth {
		t.Error("Authorization header sent with an empty token; it must be omitted entirely")
	}
}

// TestHealthNonOKReturnsAPIError checks the typed-error contract across the
// statuses that mean different things to an operator.
func TestHealthNonOKReturnsAPIError(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{name: "unauthorized", code: http.StatusUnauthorized, body: `{"error":"Unauthorized"}`},
		{name: "not found", code: http.StatusNotFound, body: "not found"},
		{name: "bad gateway", code: http.StatusBadGateway, body: `{"error":"Go API unavailable"}`},
		{name: "empty body", code: http.StatusInternalServerError, body: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			err := newTestClient(t, server, "t").Health(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("Health() = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.code)
			}
			if apiErr.Path != "/api/health" {
				t.Errorf("Path = %q, want /api/health", apiErr.Path)
			}
			if apiErr.Body != tc.body {
				t.Errorf("Body = %q, want %q", apiErr.Body, tc.body)
			}
			// The message must carry the status; a pane renders this string.
			if !strings.Contains(apiErr.Error(), http.StatusText(tc.code)) {
				t.Errorf("Error() = %q, does not name the status", apiErr.Error())
			}
		})
	}
}

// TestHealthTruncatesHugeErrorBody guards the maxErrorBodyBytes cap. A
// misconfigured reverse proxy answering with a large HTML page must not become
// a multi-megabyte string held in the model.
func TestHealthTruncatesHugeErrorBody(t *testing.T) {
	huge := strings.Repeat("x", maxErrorBodyBytes*4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(huge))
	}))
	defer server.Close()

	err := newTestClient(t, server, "t").Health(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Health() = %v, want *APIError", err)
	}
	if len(apiErr.Body) > maxErrorBodyBytes {
		t.Fatalf("Body is %d bytes, want at most %d", len(apiErr.Body), maxErrorBodyBytes)
	}
}

// TestGetJSONDecodes covers the helper every later task builds on.
func TestGetJSONDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			t.Errorf("path = %q, want /api/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"BUSY","agents":4}`))
	}))
	defer server.Close()

	var got struct {
		Mode   string `json:"mode"`
		Agents int    `json:"agents"`
	}
	if err := newTestClient(t, server, "t").getJSON(context.Background(), "/api/status", &got); err != nil {
		t.Fatalf("getJSON() = %v, want nil", err)
	}
	if got.Mode != "BUSY" || got.Agents != 4 {
		t.Fatalf("decoded %+v, want {BUSY 4}", got)
	}
}

// TestGetJSONMalformedBody: a 200 carrying something that is not the expected
// JSON is a decode error, not a silent zero value.
func TestGetJSONMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	var got struct{ Mode string }
	err := newTestClient(t, server, "t").getJSON(context.Background(), "/api/status", &got)
	if err == nil {
		t.Fatal("getJSON() = nil on a non-JSON 200, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
}

// TestGetJSONTransportError: an unreachable dashboard is an error naming the
// path, not a panic or a nil.
func TestGetJSONTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close() // nothing is listening now

	c := New()
	c.baseURL = url
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health() against a closed server = nil, want an error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("transport failure surfaced as *APIError (%v); it is not an API response", err)
	}
	if !strings.Contains(err.Error(), "/api/health") {
		t.Fatalf("error = %v, want it to name the path", err)
	}
}

// TestGetJSONHonoursContextCancellation: the TUI cancels in-flight requests
// when a pane is torn down, and that must abort the request rather than block
// until requestTimeout.
func TestGetJSONHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := newTestClient(t, server, "t").Health(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Health() = %v, want context.Canceled", err)
	}
}

// TestNewReadsEnvironment pins the two variable names and the default.
func TestNewReadsEnvironment(t *testing.T) {
	t.Setenv(BaseURLEnv, "https://hive.example.com:8443/")
	t.Setenv(TokenEnv, "  tok  ")

	c := New()
	// The trailing slash is trimmed so path joining cannot produce "//api/...".
	if c.baseURL != "https://hive.example.com:8443" {
		t.Errorf("baseURL = %q, want the trailing slash trimmed", c.baseURL)
	}
	if c.token != "tok" {
		t.Errorf("token = %q, want surrounding whitespace trimmed", c.token)
	}
	if c.http.Timeout != requestTimeout {
		t.Errorf("timeout = %v, want %v", c.http.Timeout, requestTimeout)
	}
}

// TestNewDefaultsBaseURL: an unset (or blank) HIVE_DASHBOARD_URL falls back to
// the proxy's default port, not to an empty base that would make every request
// a relative-URL error.
func TestNewDefaultsBaseURL(t *testing.T) {
	for _, value := range []string{"", "   "} {
		t.Setenv(BaseURLEnv, value)
		if got := New().baseURL; got != DefaultBaseURL {
			t.Errorf("baseURL with %q = %q, want %q", value, got, DefaultBaseURL)
		}
	}
}

// TestNewUsesItsOwnTransport guards the reason newTransport exists: a nil
// Transport silently shares http.DefaultTransport, whose connection pool other
// packages' httptest teardowns call CloseIdleConnections on.
func TestNewUsesItsOwnTransport(t *testing.T) {
	c := New()
	if c.http.Transport == nil {
		t.Fatal("client has a nil Transport; it would share http.DefaultTransport")
	}
	if c.http.Transport == http.DefaultTransport {
		t.Fatal("client shares http.DefaultTransport; it must own a clone")
	}
}

// TestMalformedBaseURLFailsOnRequest pins the behaviour New()'s doc comment
// promises: a bad HIVE_DASHBOARD_URL is not rejected at construction, it
// surfaces on the first call. If that ever changes to a constructor error, this
// test is the reminder that the doc comment changes with it.
func TestMalformedBaseURLFailsOnRequest(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://a b c")

	c := New()
	if c == nil {
		t.Fatal("New() = nil on a malformed base URL; it must not fail at construction")
	}
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health() = nil with a malformed base URL, want a request-build error")
	}
	if !strings.Contains(err.Error(), "/api/health") {
		t.Fatalf("error = %v, want it to name the path", err)
	}
}

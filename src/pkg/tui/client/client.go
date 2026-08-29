// Package client is the TUI's HTTP client for the Hive dashboard API
// (kubestellar/hive#4907).
//
// It is deliberately small: one transport, one auth header, one JSON decode
// helper. Every later TUI task adds typed methods to this package rather than
// its own HTTP handling, so there is exactly one place that knows how a request
// to the dashboard is addressed and authenticated.
//
// WHY NOT REUSE pkg/hivectl's CLIENT. It was evaluated. It speaks to the same
// API with the same token and would have brought its SSE reader along, but its
// Do() returns `any` because it feeds the CLI's table/json/yaml printer — a TUI
// decoding into typed structs would have to re-marshal that map on every poll —
// and its constructor returns an error, which this package's New() cannot per
// its contract. What it does have that is worth copying outright is the
// transport lesson recorded at src/pkg/hivectl/client.go:69; see newTransport.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is where the dashboard listens on a self-hosted hive: the
	// Node auth proxy (src/proxy/server.js), not the Go API behind it on :3002.
	// The proxy is where the token check and the SSE stream live, and it
	// forwards /api/* to the Go API, so every endpoint the TUI needs is
	// reachable here.
	DefaultBaseURL = "http://localhost:3001"

	// BaseURLEnv and TokenEnv are the two environment variables the TUI reads.
	// TokenEnv is the same variable hivectl's --token-env defaults to, so an
	// operator who has hivectl working has the TUI working.
	BaseURLEnv = "HIVE_DASHBOARD_URL"
	TokenEnv   = "HIVE_DASHBOARD_TOKEN"

	// requestTimeout bounds a single dashboard request. The TUI repaints on a
	// timer, so a request that outlives its frame is not worth waiting for —
	// better to fail the pane and retry on the next tick than to stall the UI
	// behind a hung socket.
	requestTimeout = 5 * time.Second

	// maxErrorBodyBytes caps how much of a failed response is read back into an
	// APIError. Enough to carry the dashboard's `{"error":"..."}` payload, small
	// enough that an HTML error page from a misconfigured reverse proxy cannot
	// become a multi-megabyte string held in the model.
	maxErrorBodyBytes = 4 << 10
)

// APIError is returned when the dashboard answers with a non-2xx status.
//
// It carries the status and the (truncated) body rather than collapsing to a
// string so a pane can distinguish the cases that mean different things to an
// operator: 401 is a missing or wrong HIVE_DASHBOARD_TOKEN, 404 is a dashboard
// too old to serve the endpoint, 502 is the proxy up but the Go API behind it
// down.
type APIError struct {
	StatusCode int
	Path       string
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("GET %s: %s", e.Path, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("GET %s: %s: %s", e.Path, http.StatusText(e.StatusCode), e.Body)
}

// Client talks to one dashboard.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client from the environment: base URL from HIVE_DASHBOARD_URL
// (default DefaultBaseURL), token from HIVE_DASHBOARD_TOKEN.
//
// It cannot fail. A malformed HIVE_DASHBOARD_URL surfaces on the first request
// as a request error rather than at construction, because the alternative — a
// constructor that returns an error the TUI must render before it has a frame
// to render into — buys nothing: the operator sees the same message either way,
// one tick later.
func New() *Client {
	base := strings.TrimSpace(os.Getenv(BaseURLEnv))
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		// Trailing slash is trimmed so joining a leading-slash path cannot
		// produce "//api/health", which some reverse proxies do not normalize.
		baseURL: strings.TrimRight(base, "/"),
		token:   strings.TrimSpace(os.Getenv(TokenEnv)),
		http:    &http.Client{Timeout: requestTimeout, Transport: newTransport()},
	}
}

// newTransport gives the client its OWN transport rather than letting a nil
// Transport silently use http.DefaultTransport.
//
// This is copied deliberately from src/pkg/hivectl/client.go:69, which records
// why: every parallel client test tears down its httptest.Server, and
// Server.Close() calls CloseIdleConnections on http.DefaultTransport — racing
// whichever other parallel test has a request in flight on a pooled connection.
// Under a loaded CI runner that surfaces as a flaky "connection broken" from an
// unrelated test. Clone() keeps DefaultTransport's dial/TLS/proxy defaults.
func newTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// Health reports whether the dashboard is up, returning nil on 2xx.
//
// /api/health is a PUBLIC path on the Go API (src/pkg/dashboard/server.go,
// isPublicPath), so this succeeds without a token — which makes it the right
// first call for the TUI: it separates "the dashboard is unreachable" from
// "the dashboard is there and my token is wrong", and those are different
// things to tell an operator.
func (c *Client) Health(ctx context.Context) error {
	return c.getJSON(ctx, "/api/health", nil)
}

// getJSON performs a GET and decodes the response body into v.
//
// It is the single request path for this package; later tasks add typed methods
// that call it rather than building requests themselves. A nil v discards the
// body, which is what a liveness check wants — Health cares about the status,
// not the payload, and decoding a body it will not read would turn a healthy
// dashboard serving an unexpected shape into a failure.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		// Authorization: Bearer is what the proxy's requireAuth verifies
		// (src/proxy/server.js) and what hivectl already sends
		// (src/pkg/hivectl/client.go). The published spec documents no security
		// scheme at all, so this is read from the verifier, not from the spec —
		// see kubestellar/hive#4912.
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read a bounded prefix so the error can quote the dashboard's own
		// message; a body we cannot read is not worth failing differently for.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &APIError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(body)),
		}
	}

	if v == nil {
		// Drain before the deferred Close so the connection returns to the pool
		// instead of being torn down — this client polls the same few endpoints
		// on a timer, so keeping the connection is the common case.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

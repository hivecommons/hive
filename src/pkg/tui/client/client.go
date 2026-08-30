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
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Method is the HTTP method of the failed request. It exists because this
	// package is no longer GET-only: the pause/resume writes (#5134) share this
	// error type, and a 403 from POST /api/pause reported as "GET /api/pause"
	// would send a reader looking for a request that was never made.
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	// TrimSpace covers a zero-value Method on a hand-constructed error; every
	// APIError this package builds sets it.
	req := strings.TrimSpace(e.Method + " " + e.Path)
	if e.Body == "" {
		return fmt.Sprintf("%s: %s", req, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("%s: %s: %s", req, http.StatusText(e.StatusCode), e.Body)
}

// IsForbidden reports whether err is an APIError carrying 403.
//
// Every mutating dashboard endpoint is owner-gated (requireOwnerRole,
// pkg/dashboard/api.go), and 403 is the ONE failure a pane should render
// differently: it is not a broken hive, a bad path, or a dead proxy — it is a
// working request from someone whose role does not permit the action, and the
// only useful thing to tell them is that they are not the owner. Retrying,
// which is the right response to most other errors, will never help.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden
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
// A nil v discards the body, which is what a liveness check wants — Health
// cares about the status, not the payload, and decoding a body it will not read
// would turn a healthy dashboard serving an unexpected shape into a failure.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, v)
}

// postJSON performs a POST and decodes the response body into v.
//
// body is marshalled as JSON when non-nil and omitted entirely when nil — which
// is what the pause/resume operations want: dashboard/openapi.json declares no
// requestBody for either, so they are POSTs whose whole payload is the agent
// name in the path. The parameter exists because POST can carry one and the
// later write tasks (ACMM apply, kick) will; passing nil is not an oversight.
func (c *Client) postJSON(ctx context.Context, path string, body, v any) error {
	return c.doJSON(ctx, http.MethodPost, path, body, v)
}

// putJSON performs a PUT and decodes the response body into v.
//
// PUT rather than POST because that is the method the operation declares:
// /api/packs/level is registered as "PUT /api/packs/level" and Go's mux matches
// on the method, so a POST would not reach the handler at all.
func (c *Client) putJSON(ctx context.Context, path string, body, v any) error {
	return c.doJSON(ctx, http.MethodPut, path, body, v)
}

// doJSON is the single request path for this package; typed methods call it
// rather than building requests themselves, so there is exactly one place that
// knows how a dashboard request is addressed, authenticated and error-wrapped.
func (c *Client) doJSON(ctx context.Context, method, path string, body, v any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode body for %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read a bounded prefix so the error can quote the dashboard's own
		// message; a body we cannot read is not worth failing differently for.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Body:       strings.TrimSpace(string(errBody)),
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

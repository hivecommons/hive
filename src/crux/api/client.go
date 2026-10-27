package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Client drives a `crux serve` over the API. It is the Go form of the curl
// calls in src/docs/crux-serve.md and what the `crux` CLI uses in remote
// mode (--server / CRUX_SERVER). The zero value is not usable; call
// NewClient.
type Client struct {
	addr  Address
	token string
	http  *http.Client

	mu     sync.Mutex
	status Status
}

// ErrUnreachable marks a call that never got an answer from the server:
// the dial or the request failed at the transport. A host distinguishes
// it from a route that answered with an error (*StatusError): the runtime
// is gone, not disagreeing.
var ErrUnreachable = errors.New("crux runtime unreachable")

// Status is the client's view of the server's reachability, updated by
// every call and stream.
type Status struct {
	Reachable bool
	// LastError is the most recent transport failure, "" when Reachable.
	LastError string
	// Since is when the current outage began (zero when Reachable).
	Since time.Time
	// FailedCalls counts calls that failed at the transport during the
	// current outage.
	FailedCalls int
	// LastSuccess is when the server last answered (zero if never).
	LastSuccess time.Time
}

// Status returns the reachability as of the last call. A client that has
// never called reports Reachable (nothing is known against it).
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// observe records the outcome of one transport attempt.
func (c *Client) observe(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if err != nil {
		if c.status.Reachable {
			c.status.Since = now
			c.status.FailedCalls = 0
		}
		c.status.Reachable = false
		c.status.FailedCalls++
		c.status.LastError = err.Error()
		return
	}
	c.status = Status{Reachable: true, LastSuccess: now}
}

// NewClient parses server (see ParseAddress) and returns a client that
// presents token as a bearer token when it is non-empty. A unix-socket
// address gets a transport that dials the socket; TCP uses the default.
func NewClient(server, token string) (*Client, error) {
	addr, err := ParseAddress(server)
	if err != nil {
		return nil, err
	}
	c := &Client{addr: addr, token: token, http: &http.Client{}, status: Status{Reachable: true}}
	if addr.Network == "unix" {
		path := addr.Path
		c.http.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		}
	}
	return c, nil
}

// Address is the parsed server address the client was built with.
func (c *Client) Address() Address { return c.addr }

// SetHTTPClient replaces the underlying http.Client, keeping the socket
// transport when the address is a unix socket and the caller's client has
// none of its own.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h.Transport == nil && c.http != nil {
		h.Transport = c.http.Transport
	}
	c.http = h
}

// StatusError is a non-2xx response: the HTTP status and the server's
// api.Error body when it sent one.
type StatusError struct {
	Status int
	Body   Error
}

func (e *StatusError) Error() string {
	if e.Body.Error != "" {
		return e.Body.Error
	}
	return fmt.Sprintf("crux server returned HTTP %d", e.Status)
}

// IsNotFound reports whether err is a 404 from the server.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// IsUnauthorized reports whether err is a 401 from the server.
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusUnauthorized
}

// Health is GET /v1/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, PathHealth, nil, &out)
	return out, err
}

// List is GET /v1/agents.
func (c *Client) List(ctx context.Context) ([]Process, error) {
	var out ProcessList
	if err := c.do(ctx, http.MethodGet, PathAgents, nil, &out); err != nil {
		return nil, err
	}
	return out.Processes, nil
}

// Get is GET /v1/agents/{name}.
func (c *Client) Get(ctx context.Context, name string) (Process, error) {
	var out Process
	err := c.do(ctx, http.MethodGet, AgentPath(url.PathEscape(name)), nil, &out)
	return out, err
}

// Run is POST /v1/agents: register the definition (when given) and start.
func (c *Client) Run(ctx context.Context, req RunRequest) (AgentResponse, error) {
	var out AgentResponse
	err := c.do(ctx, http.MethodPost, PathAgents, req, &out)
	return out, err
}

// Logs is GET /v1/agents/{name}/logs for one source; lines <= 0 means the
// server default.
func (c *Client) Logs(ctx context.Context, name, source string, lines int) (Logs, error) {
	q := url.Values{}
	if source != "" {
		q.Set(QuerySource, source)
	}
	if lines > 0 {
		q.Set(QueryLines, strconv.Itoa(lines))
	}
	var out Logs
	err := c.do(ctx, http.MethodGet, LogsPath(url.PathEscape(name))+"?"+q.Encode(), nil, &out)
	return out, err
}

// FollowLogs is GET /v1/agents/{name}/logs?follow=1: fn is called with
// each chunk of new lines until ctx ends, the server closes the stream, or
// fn returns an error (which is returned). A terminal api.EventState record
// (the server lost the agent's output) is returned as an error.
func (c *Client) FollowLogs(ctx context.Context, name string, fn func(LogChunk) error) error {
	return c.stream(ctx, LogsPath(url.PathEscape(name))+"?"+QueryFollow+"=1", func(typ string, data []byte) error {
		switch typ {
		case EventLog:
			var chunk LogChunk
			if err := json.Unmarshal(data, &chunk); err != nil {
				return fmt.Errorf("log stream: %w", err)
			}
			return fn(chunk)
		case EventState:
			var ev Event
			_ = json.Unmarshal(data, &ev)
			return fmt.Errorf("log stream ended: %s", ev.Detail)
		}
		return nil
	})
}

// Kick is POST /v1/agents/{name}/kick.
func (c *Client) Kick(ctx context.Context, name, message string) (AgentResponse, error) {
	var out AgentResponse
	err := c.do(ctx, http.MethodPost, KickPath(url.PathEscape(name)), KickRequest{Message: message}, &out)
	return out, err
}

// Stop is POST /v1/agents/{name}/stop.
func (c *Client) Stop(ctx context.Context, name string) (AgentResponse, error) {
	var out AgentResponse
	err := c.do(ctx, http.MethodPost, StopPath(url.PathEscape(name)), nil, &out)
	return out, err
}

// Events is GET /v1/events: fn is called with each runtime event until ctx
// ends, the server closes the stream, or fn returns an error.
func (c *Client) Events(ctx context.Context, fn func(Event) error) error {
	return c.stream(ctx, PathEvents, func(typ string, data []byte) error {
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("event stream: %w", err)
		}
		if ev.Type == "" {
			ev.Type = typ
		}
		return fn(ev)
	})
}

// Do sends one JSON request to a route and decodes the JSON response into
// out (when non-nil). Non-2xx is a *StatusError. It is the generic call the
// typed crux.Runtime client (crux/api/client) builds every method on; the
// CLI-shaped methods above are conveniences over it.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, in, out)
}

// do sends one JSON request and decodes a JSON response into out (when
// non-nil). Non-2xx is a *StatusError.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", ContentTypeJSON)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.unreachable(ctx, err)
	}
	c.observe(nil)
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("crux server %s: decoding %s %s: %w", c.addr.target(), method, path, err)
	}
	return nil
}

// stream opens an SSE route and feeds its records to fn.
func (c *Client) stream(ctx context.Context, path string, fn func(typ string, data []byte) error) error {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", ContentTypeEvent)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.unreachable(ctx, err)
	}
	c.observe(nil)
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}
	err = ReadEvents(resp.Body, fn)
	if ctx.Err() != nil {
		// The caller ended the stream; the transport error that follows is
		// not news.
		return nil
	}
	return err
}

// unreachable wraps a transport failure as ErrUnreachable and records it,
// unless the caller cancelled its own context first (a cancelled caller is
// not evidence about the server; an expired deadline is).
func (c *Client) unreachable(ctx context.Context, err error) error {
	wrapped := fmt.Errorf("%w: crux server %s: %w", ErrUnreachable, c.addr.target(), err)
	// A caller that cancelled its own request learned nothing about the
	// server; one whose deadline expired waiting on the transport did.
	if !errors.Is(ctx.Err(), context.Canceled) {
		c.observe(wrapped)
	}
	return wrapped
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.addr.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// checkStatus turns a non-2xx response into a *StatusError, reading the
// api.Error body when there is one.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	se := &StatusError{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(data, &se.Body)
	return se
}

// target is the address as a human reads it in an error.
func (a Address) target() string {
	if a.Network == "unix" {
		return "unix://" + a.Path
	}
	return a.BaseURL
}

// WaitReady polls GET /v1/health until it answers or ctx ends; it is for a
// caller that just started `crux serve` and needs to know when to begin.
func (c *Client) WaitReady(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := c.Health(ctx); err == nil {
			return nil
		} else if IsUnauthorized(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

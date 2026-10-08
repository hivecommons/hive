package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Default caps for connector HTTP traffic.
const (
	DefaultHTTPTimeout  = 30 * time.Second
	DefaultMaxBodyBytes = 20 * 1024 * 1024
	DefaultMaxRetries   = 3
	DefaultBaseBackoff  = time.Second
	DefaultMaxBackoff   = 60 * time.Second
	userAgent           = "HiveKnowledge/1.0 (connector)"
)

// HTTPOptions tunes NewHTTPClient. Zero values take the defaults above.
type HTTPOptions struct {
	Timeout      time.Duration
	MaxBodyBytes int64
	MaxRetries   int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	// AllowPrivate disables the private-address SSRF guard. Only for tests and
	// operators who deliberately point a connector at an in-cluster service.
	AllowPrivate bool
	// Transport overrides the underlying round tripper (tests).
	Transport http.RoundTripper
}

// HTTPClient is the shared SSRF-hardened HTTP client for connectors: http(s)
// only, private/internal hosts rejected before the request and on every
// redirect (the same policy as knowledge.documents), per-request timeout,
// response-size cap, and Retry-After-aware backoff on 429/5xx rate limits.
type HTTPClient struct {
	client       *http.Client
	maxBodyBytes int64
	maxRetries   int
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	allowPrivate bool
	sleep        func(context.Context, time.Duration) error
	hostPrivate  func(context.Context, string) bool
}

// NewHTTPClient builds an HTTPClient from opts.
func NewHTTPClient(opts HTTPOptions) *HTTPClient {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultHTTPTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	} else if opts.MaxRetries == 0 {
		opts.MaxRetries = DefaultMaxRetries
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = DefaultBaseBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	c := &HTTPClient{
		maxBodyBytes: opts.MaxBodyBytes,
		maxRetries:   opts.MaxRetries,
		baseBackoff:  opts.BaseBackoff,
		maxBackoff:   opts.MaxBackoff,
		allowPrivate: opts.AllowPrivate,
		sleep:        sleepCtx,
		hostPrivate:  knowledge.HostIsPrivate,
	}
	check := knowledge.CheckRedirectNoPrivate
	if opts.AllowPrivate {
		check = capRedirects
	}
	c.client = &http.Client{Timeout: opts.Timeout, CheckRedirect: check, Transport: opts.Transport}
	return c
}

func capRedirects(_ *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	return nil
}

// ValidateURL rejects non-http(s) URLs and, unless private hosts are allowed,
// URLs whose host is or resolves to a private/internal address.
func (c *HTTPClient) ValidateURL(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("URL scheme %q is not allowed (http or https only)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("URL host is required")
	}
	if !c.allowPrivate && c.hostPrivate(ctx, u.Hostname()) {
		return fmt.Errorf("URL host %q resolves to a private/internal address", u.Hostname())
	}
	return nil
}

// Response is a fully-read, size-capped HTTP response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// StatusError is returned for a non-2xx final response.
type StatusError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.URL)
}

// ErrBodyTooLarge is returned when a response exceeds MaxBodyBytes.
var ErrBodyTooLarge = errors.New("response body exceeds size cap")

// Do performs method on rawURL with header and body, retrying rate-limited
// and transient responses with backoff. Non-2xx final responses return a
// *StatusError.
func (c *HTTPClient) Do(ctx context.Context, method, rawURL string, header http.Header, body []byte) (*Response, error) {
	if err := c.ValidateURL(ctx, rawURL); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, method, rawURL, header, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var se *StatusError
		retryable := errors.As(err, &se) && httpStatusRetryable(se.StatusCode)
		if !retryable || attempt >= c.maxRetries {
			return nil, lastErr
		}
		if err := c.sleep(ctx, c.backoff(attempt, retryAfter)); err != nil {
			return nil, err
		}
	}
}

// Get is Do with GET and no body.
func (c *HTTPClient) Get(ctx context.Context, rawURL string, header http.Header) (*Response, error) {
	return c.Do(ctx, http.MethodGet, rawURL, header, nil)
}

func (c *HTTPClient) once(ctx context.Context, method, rawURL string, header http.Header, body []byte) (*Response, time.Duration, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBodyBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading response: %w", err)
	}
	if int64(len(data)) > c.maxBodyBytes {
		return nil, 0, fmt.Errorf("%w (%d bytes) from %s", ErrBodyTooLarge, c.maxBodyBytes, rawURL)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := string(data)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			&StatusError{StatusCode: resp.StatusCode, URL: rawURL, Body: snippet}
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}, 0, nil
}

// backoff returns the wait before retry attempt+1: the server's Retry-After
// when given, else exponential from baseBackoff, both capped at maxBackoff.
func (c *HTTPClient) backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := retryAfter
	if d <= 0 {
		d = c.baseBackoff << uint(attempt)
	}
	if d <= 0 || d > c.maxBackoff {
		d = c.maxBackoff
	}
	return d
}

// parseRetryAfter understands both delta-seconds and HTTP-date forms.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// HTMLToMarkdown converts an upstream HTML page body to markdown text using
// the same converter as knowledge.documents. It returns the <title> (may be
// empty) and the text.
func HTMLToMarkdown(html []byte) (title, markdown string) {
	return knowledge.HTMLToMarkdown(html)
}

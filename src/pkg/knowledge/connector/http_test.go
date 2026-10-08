package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testHTTPClient allows loopback (httptest) and records backoff sleeps
// instead of waiting.
func testHTTPClient(opts HTTPOptions) (*HTTPClient, *[]time.Duration) {
	opts.AllowPrivate = true
	c := NewHTTPClient(opts)
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return c, &slept
}

func TestNewHTTPClientDefaults(t *testing.T) {
	c := NewHTTPClient(HTTPOptions{})
	if c.maxBodyBytes != DefaultMaxBodyBytes || c.maxRetries != DefaultMaxRetries || c.baseBackoff != DefaultBaseBackoff ||
		c.maxBackoff != DefaultMaxBackoff || c.client.Timeout != DefaultHTTPTimeout || c.allowPrivate {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c := NewHTTPClient(HTTPOptions{MaxRetries: -1}); c.maxRetries != 0 {
		t.Fatalf("negative retries = %d, want 0 (disabled)", c.maxRetries)
	}
}

func TestHTTPClientValidateURL(t *testing.T) {
	c := NewHTTPClient(HTTPOptions{})
	c.hostPrivate = func(_ context.Context, host string) bool { return host == "internal.example" }
	tests := []struct {
		url     string
		wantErr string
	}{
		{"https://public.example/x", ""},
		{"http://public.example/x", ""},
		{"://bad", "invalid URL"},
		{"ftp://public.example/x", "scheme \"ftp\""},
		{"file:///etc/passwd", "scheme \"file\""},
		{"https://", "host is required"},
		{"https://internal.example/x", "private/internal"},
	}
	for _, tt := range tests {
		err := c.ValidateURL(context.Background(), tt.url)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateURL(%q) = %v", tt.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ValidateURL(%q) = %v, want %q", tt.url, err, tt.wantErr)
		}
	}
	// The real guard blocks loopback literals without DNS.
	if err := NewHTTPClient(HTTPOptions{}).ValidateURL(context.Background(), "http://127.0.0.1:1/x"); err == nil {
		t.Fatal("loopback URL allowed by default client")
	}
	if err := NewHTTPClient(HTTPOptions{AllowPrivate: true}).ValidateURL(context.Background(), "http://127.0.0.1:1/x"); err != nil {
		t.Fatalf("AllowPrivate client rejected loopback: %v", err)
	}
}

func TestHTTPClientDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-UA", r.UserAgent())
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		_, _ = fmt.Fprintf(w, "%s %s", r.Method, body)
	}))
	defer srv.Close()
	c, slept := testHTTPClient(HTTPOptions{})

	resp, err := c.Get(context.Background(), srv.URL, http.Header{"Authorization": {"Bearer t"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != "GET " || resp.Header.Get("X-Echo-UA") != userAgent || resp.Header.Get("X-Echo-Auth") != "Bearer t" {
		t.Fatalf("resp = %d %q %v", resp.StatusCode, resp.Body, resp.Header)
	}
	resp, err = c.Do(context.Background(), http.MethodPost, srv.URL, http.Header{"User-Agent": {"custom"}}, []byte("payload"))
	if err != nil || string(resp.Body) != "POST payload" || resp.Header.Get("X-Echo-UA") != "custom" {
		t.Fatalf("POST = %+v, %v", resp, err)
	}
	if len(*slept) != 0 {
		t.Fatalf("unexpected backoff: %v", *slept)
	}
}

func TestHTTPClientRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch r.URL.Path {
		case "/ratelimited":
			if n == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if n == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		case "/down":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(strings.Repeat("x", 600)))
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, slept := testHTTPClient(HTTPOptions{BaseBackoff: 10 * time.Millisecond, MaxBackoff: time.Minute})
	resp, err := c.Get(context.Background(), srv.URL+"/ratelimited", nil)
	if err != nil || string(resp.Body) != "ok" {
		t.Fatalf("retry = %+v, %v", resp, err)
	}
	if got := *slept; len(got) != 2 || got[0] != 7*time.Second || got[1] != 20*time.Millisecond {
		t.Fatalf("backoff = %v, want [7s 20ms]", got)
	}

	hits.Store(0)
	*slept = nil
	_, err = c.Get(context.Background(), srv.URL+"/down", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadGateway || len(se.Body) != 256 || hits.Load() != int32(DefaultMaxRetries+1) {
		t.Fatalf("exhausted retries: err=%v hits=%d", err, hits.Load())
	}
	if !strings.Contains(se.Error(), "HTTP 502 from") {
		t.Fatalf("StatusError.Error() = %q", se.Error())
	}

	hits.Store(0)
	if _, err := c.Get(context.Background(), srv.URL+"/missing", nil); !errors.As(err, &se) || se.StatusCode != 404 || hits.Load() != 1 {
		t.Fatalf("non-retryable: err=%v hits=%d", err, hits.Load())
	}

	// A cancelled context aborts the backoff wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	hits.Store(0)
	if _, err := c.Get(ctx, srv.URL+"/down", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backoff err = %v", err)
	}
}

func TestHTTPClientCapsAndTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/to-loopback":
			http.Redirect(w, r, "http://127.0.0.1:1/meta", http.StatusFound)
		}
	}))
	c, _ := testHTTPClient(HTTPOptions{MaxBodyBytes: 10})
	if _, err := c.Get(context.Background(), srv.URL+"/big", nil); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("size cap err = %v", err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/loop", nil); err == nil || !strings.Contains(err.Error(), "stopped after 3 redirects") {
		t.Fatalf("redirect loop err = %v", err)
	}
	if _, err := c.Do(context.Background(), "BAD METHOD", srv.URL, nil, nil); err == nil || !strings.Contains(err.Error(), "creating request") {
		t.Fatalf("bad method err = %v", err)
	}

	// Default (SSRF-guarded) redirect policy: the first hop is allowed by a
	// stubbed pre-check, the redirect to loopback is refused by the shared
	// knowledge.documents policy.
	guarded := NewHTTPClient(HTTPOptions{MaxRetries: -1})
	guarded.hostPrivate = func(context.Context, string) bool { return false }
	if _, err := guarded.Get(context.Background(), srv.URL+"/to-loopback", nil); err == nil || !strings.Contains(err.Error(), "private/internal host blocked") {
		t.Fatalf("guarded redirect err = %v", err)
	}

	url := srv.URL
	srv.Close()
	if _, err := c.Get(context.Background(), url+"/big", nil); err == nil || !strings.Contains(err.Error(), "HTTP GET") {
		t.Fatalf("closed server err = %v", err)
	}
	if _, err := c.Get(context.Background(), "ftp://x/y", nil); err == nil {
		t.Fatal("invalid URL fetched")
	}
}

func TestBackoff(t *testing.T) {
	c := NewHTTPClient(HTTPOptions{BaseBackoff: time.Second, MaxBackoff: 10 * time.Second})
	tests := []struct {
		attempt    int
		retryAfter time.Duration
		want       time.Duration
	}{
		{0, 0, time.Second},
		{1, 0, 2 * time.Second},
		{3, 0, 8 * time.Second},
		{4, 0, 10 * time.Second},
		{70, 0, 10 * time.Second},
		{0, 3 * time.Second, 3 * time.Second},
		{0, time.Hour, 10 * time.Second},
	}
	for _, tt := range tests {
		if got := c.backoff(tt.attempt, tt.retryAfter); got != tt.want {
			t.Errorf("backoff(%d, %v) = %v, want %v", tt.attempt, tt.retryAfter, got, tt.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Thu, 08 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Thu, 08 Oct 2026 11:00:00 GMT": 0,
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHTMLToMarkdownHelper(t *testing.T) {
	title, md := HTMLToMarkdown([]byte("<html><head><title>T</title></head><body><p>Hello &amp; bye</p></body></html>"))
	if title != "T" || !strings.Contains(md, "Hello & bye") {
		t.Fatalf("HTMLToMarkdown = %q, %q", title, md)
	}
}

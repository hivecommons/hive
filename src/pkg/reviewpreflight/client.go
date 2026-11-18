package reviewpreflight

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// Client limits and identity.
const (
	userAgent         = "hive-review-preflight"
	maxResponseBytes  = 8 << 20
	tlsTimeout        = 10 * time.Second
	idleConnTimeout   = 90 * time.Second
	maxIdlePerHost    = 2
	maxCacheEntries   = 4096
	totalBudgetFactor = 6 // total time for one PR = request timeout * this
)

type cacheEntry struct {
	value   any
	expires time.Time
}

// Client performs the evidence lookups. It is safe for concurrent use and
// keeps an in-memory cache so repeated reviews do not hammer the services.
type Client struct {
	HTTP *http.Client
	Now  func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewClient returns a client whose requests follow the hive's upstream proxy
// rules (network.upstream_proxy, HIVE_UPSTREAM_PROXY_*, HTTPS_PROXY), read per
// request. No credential of any kind is attached to a request.
func NewClient() *Client {
	tr := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return upstreamproxy.Default().ProxyFunc()(req)
		},
		TLSHandshakeTimeout: tlsTimeout,
		IdleConnTimeout:     idleConnTimeout,
		MaxIdleConnsPerHost: maxIdlePerHost,
		ForceAttemptHTTP2:   true,
	}
	return &Client{HTTP: &http.Client{Transport: tr}, Now: time.Now}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// getter returns a GetFunc with a per-request timeout.
func (c *Client) getter(timeout time.Duration) GetFunc {
	return func(ctx context.Context, u string) ([]byte, error) {
		return c.do(ctx, http.MethodGet, u, nil, timeout)
	}
}

func (c *Client) do(ctx context.Context, method, u string, body io.Reader, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	host := req.URL.Host
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, RateLimitError{Host: host}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("%s answered HTTP %d", host, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// lookup returns an unexpired cached value.
func (c *Client) lookup(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || !c.now().Before(e.expires) {
		return nil, false
	}
	return e.value, true
}

// store caches a successful answer.
func (c *Client) store(key string, ttl time.Duration, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil || len(c.cache) >= maxCacheEntries {
		c.cache = map[string]cacheEntry{}
	}
	c.cache[key] = cacheEntry{value: v, expires: c.now().Add(ttl)}
}

// remember returns the cached value for key, or computes and stores it. Only
// successful answers are cached.
func (c *Client) remember(key string, ttl time.Duration, compute func() (any, error)) (any, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.value, nil
	}
	c.mu.Unlock()
	v, err := compute()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= maxCacheEntries {
		c.cache = map[string]cacheEntry{}
	}
	c.cache[key] = cacheEntry{value: v, expires: now.Add(ttl)}
	c.mu.Unlock()
	return v, nil
}

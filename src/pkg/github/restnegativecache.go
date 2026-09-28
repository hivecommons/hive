package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultPullRequest404TTL       = 30 * time.Minute
	rest404NegativeCacheMaxEntries = 4096
	pullRequest404TTLEnv           = "HIVE_GITHUB_PULL_404_TTL"
	contents404TTLEnv              = "HIVE_GITHUB_CONTENTS_404_TTL"
)

type restCallerContextKey struct{}

// WithRESTCaller tags GitHub REST requests issued with ctx so /api/gh-rate-limits
// can attribute hot endpoints to a component rather than the undifferentiated
// hive process.
func WithRESTCaller(ctx context.Context, caller string) context.Context {
	caller = strings.TrimSpace(caller)
	if caller == "" {
		return ctx
	}
	return context.WithValue(ctx, restCallerContextKey{}, caller)
}

func restCallerFromContext(ctx context.Context) string {
	if ctx == nil {
		return "hive"
	}
	if v, ok := ctx.Value(restCallerContextKey{}).(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return "hive"
}

type rest404NegativeCacheTransport struct {
	inner http.RoundTripper
	cache *rest404NegativeCache
}

type rest404NegativeCache struct {
	mu      sync.Mutex
	entries map[string]rest404NegativeEntry
	now     func() time.Time
	prTTL   time.Duration
	ctTTL   time.Duration
}

type rest404NegativeEntry struct {
	until  time.Time
	caller string
}

var sharedREST404NegativeCache = newREST404NegativeCache()

func newREST404NegativeCache() *rest404NegativeCache {
	return &rest404NegativeCache{
		entries: map[string]rest404NegativeEntry{},
		now:     time.Now,
		prTTL:   durationFromEnv(pullRequest404TTLEnv, defaultPullRequest404TTL),
		ctTTL:   durationFromEnv(contents404TTLEnv, defaultPullRequest404TTL),
	}
}

func rest404NegativeCacheWrap(inner http.RoundTripper) http.RoundTripper {
	return &rest404NegativeCacheTransport{inner: inner, cache: sharedREST404NegativeCache}
}

func (t *rest404NegativeCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	kind, key, label, ttl, ok := negativeCacheable404(req)
	caller := restCallerFromContext(req.Context())
	if ok {
		if _, hit := t.cache.get(key); hit {
			return cached404Response(req), nil
		}
	}
	resp, err := t.inner.RoundTrip(req)
	if ok && err == nil && resp != nil && resp.StatusCode == http.StatusNotFound {
		t.cache.put(key, ttl, caller)
		slog.Warn("github REST 404 cached; suppressing repeated fetches", "caller", caller, "target", label, "kind", kind, "ttl", ttl.String())
	}
	return resp, err
}

func (c *rest404NegativeCache) get(key string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[key]
	if !ok {
		return time.Time{}, false
	}
	now := c.now()
	if now.After(ent.until) {
		delete(c.entries, key)
		return time.Time{}, false
	}
	return ent.until, true
}

func (c *rest404NegativeCache) put(key string, ttl time.Duration, caller string) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	c.entries[key] = rest404NegativeEntry{until: now.Add(ttl), caller: caller}
	c.evictLocked()
}

func (c *rest404NegativeCache) pruneLocked(now time.Time) {
	for key, ent := range c.entries {
		if now.After(ent.until) {
			delete(c.entries, key)
		}
	}
}

func (c *rest404NegativeCache) evictLocked() {
	for len(c.entries) > rest404NegativeCacheMaxEntries {
		var victim string
		var oldest time.Time
		for key, ent := range c.entries {
			if victim == "" || ent.until.Before(oldest) {
				victim = key
				oldest = ent.until
			}
		}
		if victim == "" {
			return
		}
		delete(c.entries, victim)
	}
}

func cached404Response(req *http.Request) *http.Response {
	body := []byte(`{"message":"cached GitHub 404"}` + "\n")
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("X-Hive-Negative-Cache", "404")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		Status:        "404 Not Found",
		StatusCode:    http.StatusNotFound,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

var pullRequestGetPathRE = regexp.MustCompile(`(?:^|.*/)repos/([^/]+)/([^/]+)/pulls/([0-9]+)$`)

func negativeCacheable404(req *http.Request) (kind, key, label string, ttl time.Duration, ok bool) {
	if req == nil || req.Method != http.MethodGet || req.URL == nil {
		return "", "", "", 0, false
	}
	path := req.URL.EscapedPath()
	if m := pullRequestGetPathRE.FindStringSubmatch(path); m != nil {
		label = m[1] + "/" + m[2] + "#" + m[3]
		return "pull_request", "pr404:" + restNegativeAuthHash(req) + ":" + strings.ToLower(req.URL.Host) + ":" + strings.ToLower(label), label, sharedREST404NegativeCache.prTTL, true
	}
	if strings.HasSuffix(path, "/contents/.claude/settings.json") {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		for i := 0; i+4 < len(parts); i++ {
			if parts[i] != "repos" {
				continue
			}
			label = parts[i+1] + "/" + parts[i+2] + ":.claude/settings.json"
			return "contents", "contents404:" + restNegativeAuthHash(req) + ":" + strings.ToLower(req.URL.Host) + ":" + strings.ToLower(label) + ":" + req.URL.RawQuery, label, sharedREST404NegativeCache.ctTTL, true
		}
	}
	return "", "", "", 0, false
}

func restNegativeAuthHash(req *http.Request) string {
	if req == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8])
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	if mins, err := strconv.Atoi(raw); err == nil && mins >= 0 {
		return time.Duration(mins) * time.Minute
	}
	return fallback
}

func resetREST404NegativeCacheForTest(now func() time.Time, prTTL, ctTTL time.Duration) func() {
	old := sharedREST404NegativeCache
	sharedREST404NegativeCache = &rest404NegativeCache{entries: map[string]rest404NegativeEntry{}, now: now, prTTL: prTTL, ctTTL: ctTTL}
	return func() { sharedREST404NegativeCache = old }
}

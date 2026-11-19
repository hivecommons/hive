package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// Upstream key refresh (hive#18532). A route used to carry the key resolved
// when the route was installed, so a key rotated in its Secret kept 401ing
// until the hive restarted. A route may now carry an InferenceKeyResolver that
// re-reads the key while the proxy runs.
const (
	// inferenceKeyRecheckTTL bounds how often a cached key's source file is
	// re-stat'ed. Requests inside the window reuse the cached key with no
	// filesystem access at all.
	inferenceKeyRecheckTTL = 3 * time.Second
	// inferenceKeyFullRefreshInterval forces a full re-resolve even when the
	// source file looks unchanged, so a NEW higher-priority source (an
	// api_key_file created after boot, a source switch) is still noticed.
	inferenceKeyFullRefreshInterval = 30 * time.Second
	// inferenceKeySourceStatic is the key_source logged for a route whose key
	// was fixed when the route was set (no resolver, e.g. a minted watsonx
	// IAM token).
	inferenceKeySourceStatic = "static"
	// inferenceKeySourceNone is logged when the resolver found no key.
	inferenceKeySourceNone = "none"
)

// InferenceKeyResolveFunc returns the current upstream key and a log-safe
// description of where it came from ("file:<path>", "env:<NAME>"). The source
// must never contain key material.
type InferenceKeyResolveFunc func() (key, source string)

// keyFileStamp identifies one version of a key file. Kubernetes Secret volumes
// rotate by swapping the ..data symlink, which changes the resolved target
// path; a file rewritten in place changes its mtime or size.
type keyFileStamp struct {
	target  string
	modTime time.Time
	size    int64
	ok      bool
}

// equal reports whether two stamps describe the same file version.
func (s keyFileStamp) equal(o keyFileStamp) bool {
	return s.ok == o.ok && s.target == o.target && s.size == o.size && s.modTime.Equal(o.modTime)
}

func statKeyFile(source string) keyFileStamp {
	if !strings.HasPrefix(source, config.KeySourceFilePrefix) {
		return keyFileStamp{}
	}
	path := strings.TrimPrefix(source, config.KeySourceFilePrefix)
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return keyFileStamp{}
	}
	fi, err := os.Stat(target)
	if err != nil {
		return keyFileStamp{}
	}
	return keyFileStamp{target: target, modTime: fi.ModTime(), size: fi.Size(), ok: true}
}

// processFirstKeySHA records, per resolver name (the route's backend), the
// SHA-256 of the first key this process resolved, so the auth-failure log can
// say whether the key changed since start without keeping the key itself.
var processFirstKeySHA sync.Map

// InferenceKeyResolver caches an upstream key and refreshes it when its source
// changes. Safe for concurrent use; one resolver is shared by every copy of a
// route.
type InferenceKeyResolver struct {
	name    string
	resolve InferenceKeyResolveFunc
	now     func() time.Time

	mu         sync.Mutex
	loaded     bool
	key        string
	source     string
	stamp      keyFileStamp
	checkedAt  time.Time
	resolvedAt time.Time
}

// NewInferenceKeyResolver returns a resolver for the route named name (its
// backend), or nil when resolve is nil.
func NewInferenceKeyResolver(name string, resolve InferenceKeyResolveFunc) *InferenceKeyResolver {
	if resolve == nil {
		return nil
	}
	return &InferenceKeyResolver{name: name, resolve: resolve, now: time.Now}
}

// load re-runs the resolve func. Caller holds r.mu. Reports whether the key
// changed.
func (r *InferenceKeyResolver) load(now time.Time) bool {
	prev, hadPrev := r.key, r.loaded
	r.key, r.source = r.resolve()
	r.stamp = statKeyFile(r.source)
	r.loaded = true
	r.checkedAt, r.resolvedAt = now, now
	if sha := config.APIKeySHA256(r.key); sha != "" {
		processFirstKeySHA.LoadOrStore(r.name, sha)
	}
	return hadPrev && prev != r.key
}

// Current returns the key to send now. It re-stats the key file at most once
// per inferenceKeyRecheckTTL and re-resolves when the file's symlink target,
// mtime or size changed, or when inferenceKeyFullRefreshInterval has passed.
func (r *InferenceKeyResolver) Current() (key, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.loaded {
		r.load(now)
		return r.key, r.source
	}
	if now.Sub(r.checkedAt) < inferenceKeyRecheckTTL {
		return r.key, r.source
	}
	r.checkedAt = now
	if now.Sub(r.resolvedAt) >= inferenceKeyFullRefreshInterval || !statKeyFile(r.source).equal(r.stamp) {
		r.load(now)
	}
	return r.key, r.source
}

// Refresh forces a re-resolve (used after an upstream 401) and reports whether
// the key differs from the one cached before.
func (r *InferenceKeyResolver) Refresh() (key, source string, changed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded {
		r.load(r.now())
		return r.key, r.source, false
	}
	changed = r.load(r.now())
	return r.key, r.source, changed
}

// Source is the log-safe source of the cached key ("none" when no key).
func (r *InferenceKeyResolver) Source() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.source == "" {
		return inferenceKeySourceNone
	}
	return r.source
}

// ChangedSinceStart reports whether the cached key differs from the first key
// this process resolved for the same route name.
func (r *InferenceKeyResolver) ChangedSinceStart() bool {
	r.mu.Lock()
	sha := config.APIKeySHA256(r.key)
	r.mu.Unlock()
	first, ok := processFirstKeySHA.Load(r.name)
	if !ok {
		return false
	}
	firstSHA, _ := first.(string)
	return sha != firstSHA
}

// inferenceKeyLogFields returns the key_source and key_changed_since_start
// values for a route. Never key material.
func inferenceKeyLogFields(route *InferenceRoute) (source string, changedSinceStart bool) {
	if route == nil || route.KeyResolver == nil {
		return inferenceKeySourceStatic, false
	}
	return route.KeyResolver.Source(), route.KeyResolver.ChangedSinceStart()
}

// routeWithCurrentKey returns the route carrying the resolver's current key: a
// COPY when the key differs from the one stored, the route itself otherwise
// (routes are shared and must not be mutated in place).
func routeWithCurrentKey(route *InferenceRoute) *InferenceRoute {
	if route == nil || route.KeyResolver == nil {
		return route
	}
	key, _ := route.KeyResolver.Current()
	if key == route.APIKey {
		return route
	}
	updated := *route
	updated.APIKey = key
	return &updated
}

// inferenceRetryDrainLimit caps how much of a discarded 401 body is read so
// the connection can be reused.
const inferenceRetryDrainLimit = 64 << 10

// retryInferenceOnAuthFailure handles an upstream 401 on a route whose key
// comes from a resolver: it forces one re-resolve and, when that yields a
// DIFFERENT key, retries the request once with it (hive#18532). It returns the
// response to use and the route that produced it, so a failure is counted
// (recordInferenceError) only after the retry, against the key actually sent.
//
// An unchanged key is not retried: the same request with the same key would
// 401 again and only double the gateway's failed-auth traffic. If the retry
// cannot be sent, the original 401 is returned untouched.
func (p *GitHubProxy) retryInferenceOnAuthFailure(ctx context.Context, client *http.Client, route *InferenceRoute, agentName, upstreamURL string, body []byte, resp *http.Response) (*http.Response, *InferenceRoute) {
	if resp == nil || resp.StatusCode != http.StatusUnauthorized || route == nil || route.KeyResolver == nil {
		return resp, route
	}
	key, source, changed := route.KeyResolver.Refresh()
	if !changed {
		return resp, route
	}
	retryRoute := *route
	retryRoute.APIKey = key
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return resp, route
	}
	req.Header.Set("Content-Type", "application/json")
	applyInferenceAuth(req, &retryRoute)
	p.logger.Info("inference upstream 401; retrying once with the re-resolved key",
		"agent", agentName, "backend", route.Backend, "key_source", source)
	retryResp, err := client.Do(req)
	if err != nil {
		p.logger.Warn("inference auth retry failed to send; keeping the original 401",
			"agent", agentName, "backend", route.Backend, "error", err)
		return resp, route
	}
	_, _ = io.CopyN(io.Discard, resp.Body, inferenceRetryDrainLimit)
	_ = resp.Body.Close()
	return retryResp, &retryRoute
}

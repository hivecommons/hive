package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const keyResolverTestBody = `{"model":"claude-cli-alias","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

const keyResolverOKResponse = `{"id":"t","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

// fakeClock is a manually advanced clock for the resolver's TTL logic.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// secretVolume mimics a Kubernetes Secret volume: <dir>/key is a symlink to
// ..data/key, and ..data is a symlink to a timestamped directory that a
// rotation replaces atomically.
type secretVolume struct {
	t   *testing.T
	dir string
	gen int
}

func newSecretVolume(t *testing.T, key string) *secretVolume {
	t.Helper()
	v := &secretVolume{t: t, dir: t.TempDir()}
	v.rotate(key)
	if err := os.Symlink(filepath.Join("..data", "key"), v.path()); err != nil {
		t.Fatalf("symlink key: %v", err)
	}
	return v
}

func (v *secretVolume) path() string { return filepath.Join(v.dir, "key") }

func (v *secretVolume) rotate(key string) {
	v.t.Helper()
	v.gen++
	gen := filepath.Join(v.dir, "..gen"+strings.Repeat("x", v.gen))
	if err := os.MkdirAll(gen, 0o700); err != nil {
		v.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gen, "key"), []byte(key), 0o600); err != nil {
		v.t.Fatalf("write: %v", err)
	}
	tmp := filepath.Join(v.dir, "..data_tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(gen), tmp); err != nil {
		v.t.Fatalf("symlink: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(v.dir, "..data")); err != nil {
		v.t.Fatalf("swap ..data: %v", err)
	}
}

// fileResolver resolves a key from a file, counting resolutions.
func fileResolver(t *testing.T, name, path string, clock *fakeClock, calls *atomic.Int32) *InferenceKeyResolver {
	t.Helper()
	r := NewInferenceKeyResolver(name, func() (string, string) {
		calls.Add(1)
		b, err := os.ReadFile(path)
		if err != nil {
			return "", ""
		}
		return strings.TrimSpace(string(b)), "file:" + path
	})
	r.now = clock.Now
	return r
}

func TestNewInferenceKeyResolver_NilFunc(t *testing.T) {
	if NewInferenceKeyResolver("x", nil) != nil {
		t.Fatal("nil resolve func must yield a nil resolver")
	}
}

// TestInferenceKeyResolver_PicksUpSecretSwap is the hive#18532 core: a Secret
// rotation (..data symlink swap) is picked up once the recheck TTL passes,
// without a restart, and requests inside the TTL touch nothing.
func TestInferenceKeyResolver_PicksUpSecretSwap(t *testing.T) {
	vol := newSecretVolume(t, "key-one")
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var calls atomic.Int32
	r := fileResolver(t, t.Name(), vol.path(), clock, &calls)

	if key, src := r.Current(); key != "key-one" || src != "file:"+vol.path() {
		t.Fatalf("Current() = (%q, %q)", key, src)
	}
	vol.rotate("key-two")

	clock.Advance(inferenceKeyRecheckTTL / 2)
	if key, _ := r.Current(); key != "key-one" {
		t.Fatalf("inside the TTL the cached key must be reused, got %q", key)
	}
	clock.Advance(inferenceKeyRecheckTTL)
	if key, _ := r.Current(); key != "key-two" {
		t.Fatalf("after the TTL the swapped key must be picked up, got %q", key)
	}
	if !r.ChangedSinceStart() {
		t.Fatal("key changed since start but ChangedSinceStart() is false")
	}

	// Unchanged file past the TTL: stat only, no re-resolve.
	before := calls.Load()
	clock.Advance(inferenceKeyRecheckTTL)
	r.Current()
	if calls.Load() != before {
		t.Fatal("an unchanged file was re-resolved before the full refresh interval")
	}
	// Full refresh interval: re-resolve even though nothing changed.
	clock.Advance(inferenceKeyFullRefreshInterval)
	r.Current()
	if calls.Load() != before+1 {
		t.Fatal("the full refresh interval did not force a re-resolve")
	}
}

func TestInferenceKeyResolver_RefreshAndSource(t *testing.T) {
	clock := &fakeClock{now: time.Unix(2_000_000, 0)}
	var key atomic.Value
	key.Store("")
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) {
		k, _ := key.Load().(string)
		if k == "" {
			return "", ""
		}
		return k, "env:HIVE_TEST"
	})
	r.now = clock.Now

	if _, _, changed := r.Refresh(); changed {
		t.Fatal("the first Refresh has nothing to compare against")
	}
	if r.Source() != inferenceKeySourceNone {
		t.Fatalf("Source() = %q, want %q", r.Source(), inferenceKeySourceNone)
	}
	if r.ChangedSinceStart() {
		t.Fatal("no key ever resolved, so nothing changed")
	}
	key.Store("a")
	if k, src, changed := r.Refresh(); k != "a" || src != "env:HIVE_TEST" || !changed {
		t.Fatalf("Refresh() = (%q, %q, %v)", k, src, changed)
	}
	if r.ChangedSinceStart() {
		t.Fatal("the first key seen IS the start key")
	}
	if _, _, changed := r.Refresh(); changed {
		t.Fatal("an unchanged key reported as changed")
	}
	key.Store("b")
	if _, _, changed := r.Refresh(); !changed || !r.ChangedSinceStart() {
		t.Fatal("a new key must report changed and changed-since-start")
	}
}

func TestStatKeyFile(t *testing.T) {
	if (statKeyFile("env:X") != keyFileStamp{}) {
		t.Fatal("a non-file source has no stamp")
	}
	if statKeyFile("file:" + filepath.Join(t.TempDir(), "missing")).ok {
		t.Fatal("a missing file has no stamp")
	}
	vol := newSecretVolume(t, "k")
	a := statKeyFile("file:" + vol.path())
	if !a.ok || !a.equal(statKeyFile("file:"+vol.path())) {
		t.Fatal("the same file version must stamp equal")
	}
	vol.rotate("k")
	if a.equal(statKeyFile("file:" + vol.path())) {
		t.Fatal("a ..data swap must change the stamp even when the content is the same")
	}
}

func TestInferenceKeyLogFieldsAndRouteWithCurrentKey(t *testing.T) {
	if src, changed := inferenceKeyLogFields(nil); src != inferenceKeySourceStatic || changed {
		t.Fatal("a nil route logs as static")
	}
	static := &InferenceRoute{APIKey: "k"}
	if routeWithCurrentKey(static) != static || routeWithCurrentKey(nil) != nil {
		t.Fatal("a route without a resolver is returned as is")
	}
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) { return "fresh", "env:X" })
	route := &InferenceRoute{APIKey: "stale", KeyResolver: r}
	got := routeWithCurrentKey(route)
	if got == route || got.APIKey != "fresh" || route.APIKey != "stale" {
		t.Fatal("a changed key must come back on a COPY, leaving the shared route untouched")
	}
	if routeWithCurrentKey(got) != got {
		t.Fatal("an up-to-date route must not be copied")
	}
	if src, _ := inferenceKeyLogFields(route); src != "env:X" {
		t.Fatalf("key_source = %q", src)
	}
}

// keyedUpstream is a gateway that accepts only the bearer keys in accept.
type keyedUpstream struct {
	srv    *httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	seen   []string
	accept map[string]bool
}

func newKeyedUpstream(t *testing.T, accept ...string) *keyedUpstream {
	t.Helper()
	u := &keyedUpstream{accept: map[string]bool{}}
	for _, k := range accept {
		u.accept["Bearer "+k] = true
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		auth := r.Header.Get("Authorization")
		u.mu.Lock()
		u.seen = append(u.seen, auth)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !u.accept[auth] {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Authentication Error, Invalid proxy server token passed"}}`)
			return
		}
		_, _ = io.WriteString(w, keyResolverOKResponse)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *keyedUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.seen) == 0 {
		return ""
	}
	return u.seen[len(u.seen)-1]
}

func newKeyResolverTestProxy(logs *bytes.Buffer) *GitHubProxy {
	p := newInferenceTestProxy()
	p.inferenceLastErrors = newInferenceLastErrorState()
	p.inferenceAuth = &inferenceAuthState{}
	p.logger = slog.New(slog.NewTextHandler(logs, nil))
	return p
}

func (p *GitHubProxy) authFailures() int {
	p.inferenceAuth.mu.Lock()
	defer p.inferenceAuth.mu.Unlock()
	return p.inferenceAuth.consecutive
}

func serveTranslator(t *testing.T, p *GitHubProxy, route *InferenceRoute) int {
	t.Helper()
	p.inference.Set("scanner", route)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(keyResolverTestBody))
	req.Header.Set("x-api-key", "sk-hive-scanner")
	w := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(w, req)
	return w.Code
}

func serveMITM(t *testing.T, p *GitHubProxy, route *InferenceRoute) int {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(keyResolverTestBody))
		p.handleInferenceRequest(server, req, "scanner", route)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = client.Close()
	<-done
	return resp.StatusCode
}

// TestInferenceProxy_KeySwappedAtRuntime: the next request after a Secret
// rotation carries the new key, with no restart and no route re-install.
func TestInferenceProxy_KeySwappedAtRuntime(t *testing.T) {
	up := newKeyedUpstream(t, "key-one", "key-two")
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	vol := newSecretVolume(t, "key-one")
	clock := &fakeClock{now: time.Unix(3_000_000, 0)}
	var calls atomic.Int32
	r := fileResolver(t, t.Name(), vol.path(), clock, &calls)
	route := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "key-one", KeyResolver: r}

	if code := serveTranslator(t, p, route); code != http.StatusOK || up.lastAuth() != "Bearer key-one" {
		t.Fatalf("first request: status %d auth %q", code, up.lastAuth())
	}
	vol.rotate("key-two")
	clock.Advance(inferenceKeyRecheckTTL)
	if code := serveTranslator(t, p, route); code != http.StatusOK || up.lastAuth() != "Bearer key-two" {
		t.Fatalf("after the swap: status %d auth %q, want the new key", code, up.lastAuth())
	}
	if code := serveMITM(t, p, route); code != http.StatusOK || up.lastAuth() != "Bearer key-two" {
		t.Fatalf("MITM path: status %d auth %q, want the new key", code, up.lastAuth())
	}
	if route.APIKey != "key-one" {
		t.Fatal("the shared route was mutated in place")
	}
}

// TestInferenceProxy_401ReResolvesAndRetries: a key rotated inside the recheck
// TTL 401s once, is re-resolved, and the single retry succeeds with no auth
// failure counted. Both entry points.
func TestInferenceProxy_401ReResolvesAndRetries(t *testing.T) {
	for _, entry := range []string{"translator", "mitm"} {
		t.Run(entry, func(t *testing.T) {
			up := newKeyedUpstream(t, "new-key")
			var logs bytes.Buffer
			p := newKeyResolverTestProxy(&logs)
			var key atomic.Value
			key.Store("old-key")
			r := NewInferenceKeyResolver(t.Name(), func() (string, string) {
				k, _ := key.Load().(string)
				return k, "file:/secrets/litellm_api_key"
			})
			r.Current() // cache the old key
			key.Store("new-key")
			route := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "old-key", KeyResolver: r}

			serve := serveTranslator
			if entry == "mitm" {
				serve = serveMITM
			}
			if code := serve(t, p, route); code != http.StatusOK {
				t.Fatalf("status %d, want the retry's 200\n%s", code, logs.String())
			}
			if up.hits.Load() != 2 || up.lastAuth() != "Bearer new-key" {
				t.Fatalf("hits %d last auth %q, want one retry with the new key", up.hits.Load(), up.lastAuth())
			}
			if p.authFailures() != 0 {
				t.Fatal("a 401 that the retry recovered must not count as an auth failure")
			}
			if strings.Contains(logs.String(), "new-key") || strings.Contains(logs.String(), "old-key") {
				t.Fatalf("logs leaked key material:\n%s", logs.String())
			}
		})
	}
}

// TestInferenceProxy_Second401CountsFailure: when the re-resolved key is ALSO
// rejected, exactly one auth failure is counted and the WARN carries
// key_source and key_changed_since_start.
func TestInferenceProxy_Second401CountsFailure(t *testing.T) {
	up := newKeyedUpstream(t) // accepts nothing
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	var key atomic.Value
	key.Store("first-key")
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) {
		k, _ := key.Load().(string)
		return k, "file:/secrets/litellm_api_key"
	})
	r.Current()
	key.Store("second-key")
	route := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "first-key", KeyResolver: r}

	if code := serveTranslator(t, p, route); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if up.hits.Load() != 2 {
		t.Fatalf("hits %d, want the original plus one retry", up.hits.Load())
	}
	if p.authFailures() != 1 {
		t.Fatalf("auth failures %d, want exactly 1", p.authFailures())
	}
	out := logs.String()
	for _, want := range []string{"inference backend auth failure", "key_source=file:/secrets/litellm_api_key", "key_changed_since_start=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "first-key") || strings.Contains(out, "second-key") {
		t.Fatalf("logs leaked key material:\n%s", out)
	}
}

// TestInferenceProxy_401UnchangedKeyIsNotRetried: re-resolving to the SAME
// key does not resend the request; the failure counts once.
func TestInferenceProxy_401UnchangedKeyIsNotRetried(t *testing.T) {
	up := newKeyedUpstream(t)
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) { return "same-key", "env:HIVE_LITELLM_API_KEY" })
	route := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "same-key", KeyResolver: r}

	if code := serveMITM(t, p, route); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if up.hits.Load() != 1 || p.authFailures() != 1 {
		t.Fatalf("hits %d failures %d, want 1 and 1", up.hits.Load(), p.authFailures())
	}
	if !strings.Contains(logs.String(), "key_changed_since_start=false") {
		t.Fatalf("log missing key_changed_since_start=false:\n%s", logs.String())
	}
}

// TestInferenceProxy_StaticRouteLogsStaticSource: a route with no resolver
// keeps today's behaviour and logs key_source=static.
func TestInferenceProxy_StaticRouteLogsStaticSource(t *testing.T) {
	up := newKeyedUpstream(t)
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	route := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "k"}
	if code := serveTranslator(t, p, route); code != http.StatusUnauthorized || up.hits.Load() != 1 {
		t.Fatalf("status %d hits %d", code, up.hits.Load())
	}
	if !strings.Contains(logs.String(), "key_source=static") {
		t.Fatalf("log missing key_source=static:\n%s", logs.String())
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial refused")
}

// TestRetryInferenceOnAuthFailure_KeepsOriginalWhenRetryFails: if the retry
// cannot be sent, the original 401 is returned so it is counted normally.
func TestRetryInferenceOnAuthFailure_KeepsOriginalWhenRetryFails(t *testing.T) {
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	var key atomic.Value
	key.Store("a")
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) {
		k, _ := key.Load().(string)
		return k, "env:X"
	})
	r.Current()
	key.Store("b")
	route := &InferenceRoute{Backend: "litellm", APIKey: "a", KeyResolver: r}
	orig := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("no"))}
	client := &http.Client{Transport: failingRoundTripper{}}

	resp, used := p.retryInferenceOnAuthFailure(context.Background(), client, route, "scanner", "http://gw.invalid/v1/chat/completions", nil, orig)
	if resp != orig || used != route {
		t.Fatal("a failed retry must hand back the original response and route")
	}
	if !strings.Contains(logs.String(), "inference auth retry failed to send") {
		t.Fatalf("failed retry not logged:\n%s", logs.String())
	}

	// A bad URL cannot even build the retry request.
	key.Store("c")
	if resp, _ := p.retryInferenceOnAuthFailure(context.Background(), client, route, "scanner", "://bad", nil, orig); resp != orig {
		t.Fatal("an unbuildable retry must hand back the original response")
	}

	// Non-401s and resolver-less routes pass straight through.
	ok := &http.Response{StatusCode: http.StatusOK}
	if resp, _ := p.retryInferenceOnAuthFailure(context.Background(), client, route, "scanner", "", nil, ok); resp != ok {
		t.Fatal("a 200 must pass through")
	}
	static := &InferenceRoute{APIKey: "a"}
	if resp, used := p.retryInferenceOnAuthFailure(context.Background(), client, static, "scanner", "", nil, orig); resp != orig || used != static {
		t.Fatal("a resolver-less route must pass through")
	}
}

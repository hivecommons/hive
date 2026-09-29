package credsidecar

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fakeToken = "ghs_sidecarOnlyToken0123456789"

type fakeMinter struct {
	mu    sync.Mutex
	calls map[string]int
	err   error
}

func (m *fakeMinter) ScopedToken(_ context.Context, tier string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[tier]++
	if m.err != nil {
		return "", m.err
	}
	return fakeToken, nil
}

func (m *fakeMinter) count(tier string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[tier]
}

func (m *fakeMinter) setErr(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

func (m *fakeMinter) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		n += c
	}
	return n
}

// seenRequest is what the fake GitHub upstream received.
type seenRequest struct {
	method string
	uri    string
	host   string
	header http.Header
	body   string
}

type harness struct {
	t        *testing.T
	upstream *httptest.Server
	host     string // upstream host:port, allowlisted as a "GitHub host"
	hits     atomic.Int32
	mu       sync.Mutex
	seen     []seenRequest
	minter   *fakeMinter
	server   *Server
	logs     *bytes.Buffer
	now      time.Time
}

func newHarness(t *testing.T, opts ...ServerOption) *harness {
	t.Helper()
	h := &harness{t: t, minter: &fakeMinter{}, logs: &bytes.Buffer{}, now: time.Unix(1700000000, 0)}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, seenRequest{method: r.Method, uri: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: string(body)})
		h.mu.Unlock()
		w.Header().Set("X-Upstream", "github")
		w.Header().Set("Connection", "X-Drop-Me")
		w.Header().Set("X-Drop-Me", "hop")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(h.upstream.Close)
	u, _ := url.Parse(h.upstream.URL)
	h.host = u.Host
	cfg := ServerConfig{Key: testKey, AllowedHosts: allowedHosts(h.host), MaxBodyBytes: 1024}
	all := append([]ServerOption{withUpstream(h.upstream.Client(), "http"), withClock(func() time.Time { return h.now })}, opts...)
	srv, err := NewServer(cfg, h.minter, slog.New(slog.NewTextHandler(h.logs, nil)), all...)
	if err != nil {
		t.Fatal(err)
	}
	h.server = srv
	return h
}

// signed builds a request exactly as Client would, for direct ServeHTTP.
func (h *harness) signed(method, uri, host, agentName, tier, body string, mutate func(*SignedFields)) *http.Request {
	h.t.Helper()
	f := SignedFields{
		Method:     method,
		Host:       host,
		RequestURI: uri,
		Agent:      agentName,
		Tier:       tier,
		Timestamp:  h.now.Unix(),
		Nonce:      mustNonce(h.t),
		BodySHA256: BodyDigest([]byte(body)),
	}
	sig, err := Sign(testKey, f)
	if err != nil {
		h.t.Fatal(err)
	}
	if mutate != nil {
		mutate(&f)
	}
	r := httptest.NewRequest(method, uri, strings.NewReader(body))
	r.Header.Set(HeaderHost, f.Host)
	r.Header.Set(HeaderAgent, f.Agent)
	r.Header.Set(HeaderTier, f.Tier)
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(f.Timestamp, 10))
	r.Header.Set(HeaderNonce, f.Nonce)
	r.Header.Set(HeaderBodySHA256, f.BodySHA256)
	r.Header.Set(HeaderSignature, sig)
	return r
}

func mustNonce(t *testing.T) string {
	t.Helper()
	n, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// seenAt returns the i-th request the fake upstream received (locked: the
// upstream handler runs on its own goroutine).
func (h *harness) seenAt(i int) seenRequest {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.seen) {
		h.t.Fatalf("upstream saw %d requests, wanted index %d", len(h.seen), i)
	}
	return h.seen[i]
}

func (h *harness) seenCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

func (h *harness) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, r)
	return rec
}

func (h *harness) assertRefused(rec *httptest.ResponseRecorder, status int, want error) {
	h.t.Helper()
	if rec.Code != status {
		h.t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get(HeaderRefused); got == "" || !strings.Contains(got, want.Error()) {
		h.t.Fatalf("%s = %q, want it to name %q", HeaderRefused, got, want)
	}
	if h.hits.Load() != 0 {
		h.t.Fatalf("a refused request reached GitHub (%d upstream hits)", h.hits.Load())
	}
	if h.minter.total() != 0 {
		h.t.Fatal("a refused request caused a token mint")
	}
}

func TestServer_ForwardsSignedRequestWithTierToken(t *testing.T) {
	h := newHarness(t)
	r := h.signed("POST", "/repos/o/r/issues?x=1", h.host, "scanner", "contributor", `{"t":1}`, nil)
	r.Header.Set("Authorization", "token agent-smuggled-credential")
	r.Header.Set("Accept", "application/vnd.github+json")
	rec := h.serve(r)

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"ok":true}` || rec.Header().Get("X-Upstream") != "github" {
		t.Fatalf("relayed response = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get("X-Drop-Me") != "" {
		t.Fatal("a Connection-named hop-by-hop response header was relayed")
	}
	if n := h.seenCount(); n != 1 {
		t.Fatalf("upstream saw %d requests", n)
	}
	got := h.seenAt(0)
	if got.method != "POST" || got.uri != "/repos/o/r/issues?x=1" || got.body != `{"t":1}` || got.host != h.host {
		t.Fatalf("upstream request = %+v", got)
	}
	if auth := got.header.Get("Authorization"); auth != "token "+fakeToken {
		t.Fatalf("upstream Authorization = %q, want the sidecar's tier token (never the caller's)", auth)
	}
	if got.header.Get("Accept") != "application/vnd.github+json" {
		t.Fatal("ordinary request header not forwarded")
	}
	for k := range got.header {
		if strings.HasPrefix(k, HeaderPrefix) {
			t.Fatalf("protocol header %s leaked to GitHub", k)
		}
	}
	if strings.Contains(h.logs.String(), fakeToken) {
		t.Fatal("the token was logged")
	}
}

func TestServer_TokenCachedPerTier(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		if rec := h.serve(h.signed("GET", "/user", h.host, "a", "trusted", "", nil)); rec.Code != http.StatusCreated {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if h.minter.count("trusted") != 1 {
		t.Fatalf("minted %d times for 3 requests, want 1 (cached)", h.minter.count("trusted"))
	}
	h.now = h.now.Add(TokenTTL + time.Second)
	if rec := h.serve(h.signed("GET", "/user", h.host, "a", "trusted", "", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("after TTL: %d", rec.Code)
	}
	if h.minter.count("trusted") != 2 {
		t.Fatalf("minted %d times, want a re-mint after TokenTTL", h.minter.count("trusted"))
	}
}

func TestServer_GitPathUsesBasicAuth(t *testing.T) {
	h := newHarness(t)
	rec := h.serve(h.signed("POST", "/o/r.git/git-receive-pack", h.host, "coder", "contributor", "PACK", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitBasicUser+":"+fakeToken))
	if got := h.seenAt(0).header.Get("Authorization"); got != want {
		t.Fatalf("git Authorization = %q, want Basic x-access-token", got)
	}
}

func TestServer_NoCredentialForEmptyTierOrLoginPath(t *testing.T) {
	h := newHarness(t)
	for _, r := range []*http.Request{
		h.signed("GET", "/repos/o/r", h.host, "", "", "", nil),
		h.signed("POST", "/login/device/code", h.host, "coder", "contributor", "client_id=x", nil),
	} {
		r.Header.Set("Authorization", "token smuggled")
		if rec := h.serve(r); rec.Code != http.StatusCreated {
			t.Fatalf("status %d", rec.Code)
		}
	}
	for i := 0; i < h.seenCount(); i++ {
		if s := h.seenAt(i); s.header.Get("Authorization") != "" {
			t.Fatalf("request %d reached GitHub with Authorization %q, want none", i, s.header.Get("Authorization"))
		}
	}
	if h.minter.total() != 0 {
		t.Fatal("minted a token for a request that must carry none")
	}
}

func TestServer_RejectsUnsigned(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("GET", "/user", nil)
	r.Header.Set(HeaderHost, h.host)
	r.Header.Set(HeaderTier, "trusted")
	h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrUnsigned)
}

func TestServer_RejectsBadSignature(t *testing.T) {
	for name, mutate := range map[string]func(*SignedFields){
		"tier escalated": func(f *SignedFields) { f.Tier = "merger" },
		"agent swapped":  func(f *SignedFields) { f.Agent = "other" },
		"host swapped":   func(f *SignedFields) { f.Host = "api.github.com" },
		"garbage":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r := h.signed("GET", "/user", h.host, "a", "contributor", "", mutate)
			if mutate == nil {
				r.Header.Set(HeaderSignature, strings.Repeat("0", 64))
			}
			h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrBadSignature)
		})
	}
	t.Run("path changed after signing", func(t *testing.T) {
		h := newHarness(t)
		r := h.signed("GET", "/user", h.host, "a", "contributor", "", nil)
		r.RequestURI = "/repos/o/r/collaborators"
		h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrBadSignature)
	})
	t.Run("method changed after signing", func(t *testing.T) {
		h := newHarness(t)
		r := h.signed("GET", "/user", h.host, "a", "contributor", "", nil)
		r.Method = "DELETE"
		h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrBadSignature)
	})
}

func TestServer_RejectsTamperedBody(t *testing.T) {
	h := newHarness(t)
	r := h.signed("POST", "/repos/o/r/issues", h.host, "a", "contributor", `{"t":1}`, nil)
	r.Body = io.NopCloser(strings.NewReader(`{"t":2}`))
	h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrBodyDigest)
}

func TestServer_RejectsReplay(t *testing.T) {
	h := newHarness(t)
	r := h.signed("POST", "/repos/o/r/issues", h.host, "a", "contributor", "x", nil)
	replay := r.Clone(context.Background())
	replay.Body = io.NopCloser(strings.NewReader("x"))
	if rec := h.serve(r); rec.Code != http.StatusCreated {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := h.serve(replay)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get(HeaderRefused), ErrReplayed.Error()) {
		t.Fatalf("replay = %d %q, want 401 %v", rec.Code, rec.Header().Get(HeaderRefused), ErrReplayed)
	}
	if h.hits.Load() != 1 {
		t.Fatalf("the replay reached GitHub (%d hits)", h.hits.Load())
	}
}

func TestServer_RejectsExpired(t *testing.T) {
	for _, skew := range []time.Duration{-ReplayWindow - time.Second, ReplayWindow + time.Second} {
		h := newHarness(t)
		r := h.signed("GET", "/user", h.host, "a", "contributor", "", nil)
		h.now = h.now.Add(skew)
		h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrExpired)
	}
}

func TestServer_RefusesNonGitHubHost(t *testing.T) {
	h := newHarness(t)
	// Correctly signed - the hive process itself asked - and still refused:
	// the allowlist is the sidecar's own, not the signer's.
	r := h.signed("GET", "/v1/models", "api.openai.com", "a", "contributor", "", nil)
	h.assertRefused(h.serve(r), http.StatusForbidden, ErrHostNotAllowed)
}

func TestServer_RejectsMalformed(t *testing.T) {
	cases := map[string]func(*http.Request){
		"bad timestamp": func(r *http.Request) { r.Header.Set(HeaderTimestamp, "soon") },
		"short nonce":   func(r *http.Request) { r.Header.Set(HeaderNonce, "abc") },
		"long nonce":    func(r *http.Request) { r.Header.Set(HeaderNonce, strings.Repeat("a", maxNonceLen+1)) },
		"no host":       func(r *http.Request) { r.Header.Del(HeaderHost) },
		"no digest":     func(r *http.Request) { r.Header.Del(HeaderBodySHA256) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r := h.signed("GET", "/user", h.host, "a", "contributor", "", nil)
			mutate(r)
			h.assertRefused(h.serve(r), http.StatusUnauthorized, ErrMalformed)
		})
	}
}

func TestServer_RejectsOversizedBody(t *testing.T) {
	h := newHarness(t)
	body := strings.Repeat("x", 2048) // over the harness's 1024-byte limit
	h.assertRefused(h.serve(h.signed("POST", "/repos/o/r/issues", h.host, "a", "contributor", body, nil)), http.StatusRequestEntityTooLarge, ErrBodyTooLarge)
}

func TestServer_NonceCacheFullIsServiceUnavailable(t *testing.T) {
	h := newHarness(t)
	h.server.nonces = newNonceCache(1)
	if rec := h.serve(h.signed("GET", "/user", h.host, "a", "", "", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := h.serve(h.signed("GET", "/user", h.host, "a", "", "", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Header().Get(HeaderRefused), ErrNonceCacheFull.Error()) {
		t.Fatalf("full cache = %d %q", rec.Code, rec.Header().Get(HeaderRefused))
	}
}

func TestServer_MintFailuresAreBadGateway(t *testing.T) {
	h := newHarness(t)
	rec := h.serve(h.signed("GET", "/user", h.host, "a", "superuser", "", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Header().Get(HeaderRefused), ErrUnknownTier.Error()) {
		t.Fatalf("unknown tier = %d %q", rec.Code, rec.Header().Get(HeaderRefused))
	}
	h.minter.setErr(errors.New("github down"))
	rec = h.serve(h.signed("GET", "/user", h.host, "a", "contributor", "", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("mint error = %d", rec.Code)
	}
	if h.hits.Load() != 0 {
		t.Fatal("a request without a token reached GitHub")
	}
}

func TestTokenCache_EmptyTokenRefused(t *testing.T) {
	c := newTokenCache(emptyMinter{}, time.Now)
	if _, err := c.token(context.Background(), "contributor"); err == nil {
		t.Fatal("an empty minted token was accepted")
	}
}

type emptyMinter struct{}

func (emptyMinter) ScopedToken(context.Context, string) (string, error) { return "", nil }

func TestServer_UpstreamUnreachableIsBadGateway(t *testing.T) {
	h := newHarness(t)
	h.upstream.Close()
	rec := h.serve(h.signed("GET", "/user", h.host, "a", "", "", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

func TestNewServer_Validation(t *testing.T) {
	if _, err := NewServer(ServerConfig{Key: []byte("short")}, &fakeMinter{}, nil); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := NewServer(ServerConfig{Key: testKey}, nil, nil); err == nil {
		t.Fatal("nil minter accepted")
	}
	s, err := NewServer(ServerConfig{Key: testKey}, &fakeMinter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.maxBodyBytes != DefaultMaxBodyBytes || !s.allowedHosts["api.github.com"] || s.allowedHosts["api.openai.com"] {
		t.Fatalf("defaults: max=%d hosts=%v", s.maxBodyBytes, s.allowedHosts)
	}
	tr, ok := s.upstream.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the upstream transport must never use an HTTP proxy")
	}
	var dialed atomic.Bool
	s, _ = NewServer(ServerConfig{Key: testKey}, &fakeMinter{}, nil, WithDialContext(func(ctx context.Context, n, a string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("dial seam")
	}))
	tr, _ = s.upstream.Transport.(*http.Transport)
	_, _ = tr.DialContext(context.Background(), "tcp", "x:1")
	if !dialed.Load() {
		t.Fatal("WithDialContext did not install the dialer")
	}
}

// End to end through the real Client and a real listener: the proxy half
// signs, the sidecar half verifies and forwards.
func TestClientServer_EndToEnd(t *testing.T) {
	h := newHarness(t)
	h.server.now = time.Now
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v on shutdown", err)
		}
	})

	u, _ := url.Parse("http://" + ln.Addr().String())
	c, err := NewClient(ClientConfig{URL: u, Key: testKey, MaxBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/repos/o/r/issues?a=b", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "token hive-proxy-injected-coder")
	req.Header.Set(HeaderTier, "merger") // a caller-forged protocol header must not survive
	resp, err := c.Forward(req, h.host, "coder", "contributor")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != `{"ok":true}` {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if got := h.seenAt(0).header.Get("Authorization"); got != "token "+fakeToken {
		t.Fatalf("Authorization = %q", got)
	}
	if h.minter.count("contributor") != 1 || h.minter.count("merger") != 0 {
		t.Fatalf("mint calls: contributor=%d merger=%d, want only the signed tier", h.minter.count("contributor"), h.minter.count("merger"))
	}

	// Oversized body: refused on the proxy side, nothing sent.
	big := httptest.NewRequest("POST", "/repos/o/r/issues", strings.NewReader(strings.Repeat("x", 2048)))
	if _, err := c.Forward(big, h.host, "coder", "contributor"); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	// A GET with no body still signs the empty-body digest.
	get := httptest.NewRequest("GET", "/user", nil)
	get.Body = nil
	resp, err = c.Forward(get, h.host, "coder", "")
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
}

func TestClient_Errors(t *testing.T) {
	if _, err := NewClient(ClientConfig{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil URL: %v", err)
	}
	u, _ := url.Parse("http://127.0.0.1:1")
	if _, err := NewClient(ClientConfig{URL: u, Key: []byte("x")}); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short key: %v", err)
	}
	c, err := NewClient(ClientConfig{URL: u, Key: testKey})
	if err != nil || c.maxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("defaults: %v", err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the sidecar client must never use an HTTP proxy")
	}
	// Nothing listens on port 1: the error must say the sidecar is unreachable.
	if _, err := c.Forward(httptest.NewRequest("GET", "/user", nil), "api.github.com", "a", ""); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable sidecar: %v", err)
	}
	c.nonce = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := c.Forward(httptest.NewRequest("GET", "/user", nil), "api.github.com", "a", ""); err == nil {
		t.Fatal("nonce failure ignored")
	}
	c.nonce = newNonce
	if _, err := c.Forward(httptest.NewRequest("GET", "/user", nil), "api.github.com", "a\nb", ""); !errors.Is(err, ErrFieldHasNewline) {
		t.Fatalf("newline agent: %v", err)
	}
	bad := httptest.NewRequest("GET", "/user", nil)
	bad.Body = io.NopCloser(errReader{})
	if _, err := c.Forward(bad, "api.github.com", "a", ""); err == nil {
		t.Fatal("body read error ignored")
	}
	bad = httptest.NewRequest("GET", "/user", nil)
	bad.Method = "BAD METHOD"
	if _, err := c.Forward(bad, "api.github.com", "a", ""); err == nil {
		t.Fatal("invalid method accepted")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestServer_BodyReadErrorIsBadRequest(t *testing.T) {
	h := newHarness(t)
	r := h.signed("POST", "/repos/o/r/issues", h.host, "a", "contributor", "x", nil)
	r.Body = io.NopCloser(errReader{})
	h.assertRefused(h.serve(r), http.StatusBadRequest, ErrMalformed)
}

func TestListenAndServe_BadAddress(t *testing.T) {
	s, _ := NewServer(ServerConfig{Key: testKey}, &fakeMinter{}, nil)
	if err := s.ListenAndServe(context.Background(), "256.0.0.1:0"); err == nil {
		t.Fatal("listening on an invalid address succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ListenAndServe(ctx, "127.0.0.1:0"); err != nil {
		t.Fatalf("cancelled serve: %v", err)
	}
}

func TestServe_ReturnsListenerError(t *testing.T) {
	s, _ := NewServer(ServerConfig{Key: testKey}, &fakeMinter{}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := s.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

func TestBuildUpstreamRequest_BadURI(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("GET", "/user", nil)
	f := SignedFields{Method: "GET", Host: "api.github.com", RequestURI: "/%zz"}
	if _, err := h.server.buildUpstreamRequest(r, f, nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad URI: %v", err)
	}
	f = SignedFields{Method: "BAD METHOD", Host: "api.github.com", RequestURI: "/user"}
	if _, err := h.server.buildUpstreamRequest(r, f, nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad method: %v", err)
	}
}

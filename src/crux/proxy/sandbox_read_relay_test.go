package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testReadToken = "ghs_READONLYTOKEN"
	// relayTestHost is the endpoint host the test relay's certificate is for.
	relayTestHost = "hive.example.svc"
)

func TestSandboxReadPolicy(t *testing.T) {
	cases := []struct {
		name, host, method, path, query string
		allowed, inject, basic          bool
	}{
		{"api GET", "api.github.com", "GET", "/repos/o/r/tags", "", true, true, false},
		{"api HEAD", "api.github.com", "HEAD", "/repos/o/r", "", true, true, false},
		{"codeload GET", "codeload.github.com", "GET", "/o/r/tar.gz/main", "", true, true, false},
		{"raw GET", "raw.githubusercontent.com", "GET", "/o/r/main/a.txt", "", true, true, false},
		{"api POST refused", "api.github.com", "POST", "/repos/o/r/issues", "", false, false, false},
		{"api GraphQL POST refused", "api.github.com", "POST", "/graphql", "", false, false, false},
		{"api PUT refused", "api.github.com", "PUT", "/repos/o/r/pulls/1/merge", "", false, false, false},
		{"api PATCH refused", "api.github.com", "PATCH", "/repos/o/r/issues/1", "", false, false, false},
		{"api DELETE refused", "api.github.com", "DELETE", "/repos/o/r/git/refs/heads/x", "", false, false, false},
		{"codeload POST refused", "codeload.github.com", "POST", "/o/r", "", false, false, false},
		{"raw PUT refused", "raw.githubusercontent.com", "PUT", "/o/r/main/a", "", false, false, false},
		{"git discovery fetch", "github.com", "GET", "/o/r.git/info/refs", "service=git-upload-pack", true, true, true},
		{"git upload-pack POST", "github.com", "POST", "/o/r.git/git-upload-pack", "", true, true, true},
		{"git push discovery refused", "github.com", "GET", "/o/r.git/info/refs", "service=git-receive-pack", false, false, false},
		{"git receive-pack POST refused", "github.com", "POST", "/o/r.git/git-receive-pack", "", false, false, false},
		{"git receive-pack GET refused", "github.com", "GET", "/o/r.git/git-receive-pack", "", false, false, false},
		{"github.com page GET no credential", "github.com", "GET", "/o/r/archive/refs/heads/main.tar.gz", "", true, false, false},
		{"github.com other POST refused", "github.com", "POST", "/o/r/issues", "", false, false, false},
		{"non-GitHub host", "pypi.org", "GET", "/simple/x", "", false, false, false},
		{"lookalike host", "api.github.com.evil.example", "GET", "/", "", false, false, false},
		{"GHE host not injected", "ghe.example.com", "GET", "/api/v3/user", "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := SandboxReadPolicy(tc.host, tc.method, tc.path, tc.query)
			if d.Allowed != tc.allowed || d.Inject != tc.inject || d.Basic != tc.basic {
				t.Fatalf("SandboxReadPolicy(%s %s %s) = %+v, want allowed=%v inject=%v basic=%v", tc.method, tc.host, tc.path, d, tc.allowed, tc.inject, tc.basic)
			}
			if !d.Allowed && d.Inject {
				t.Fatal("a refused request must never be marked for injection")
			}
		})
	}
}

func TestSandboxReadPolicyNeverInjectsForAnyWriteMethod(t *testing.T) {
	for _, host := range []string{"api.github.com", "codeload.github.com", "raw.githubusercontent.com", "github.com"} {
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT"} {
			for _, path := range []string{"/", "/graphql", "/o/r.git/git-receive-pack", "/o/r/issues"} {
				d := SandboxReadPolicy(host, method, path, "")
				if d.Inject && !(host == "github.com" && method == "POST" && strings.HasSuffix(path, "/git-upload-pack")) {
					t.Errorf("%s %s%s must not receive a credential: %+v", method, host, path, d)
				}
			}
		}
	}
}

func TestApplySandboxReadAuth(t *testing.T) {
	newReq := func() *http.Request {
		r, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
		r.Header.Set("Authorization", "token AGENT_SUPPLIED")
		r.Header.Set("Proxy-Authorization", "Basic abc")
		r.Header.Set("Expect", "100-continue")
		return r
	}
	t.Run("token scheme", func(t *testing.T) {
		r := newReq()
		applySandboxReadAuth(r, SandboxReadDecision{Allowed: true, Inject: true}, testReadToken)
		if got := r.Header.Get("Authorization"); got != "token "+testReadToken {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Expect") != "" {
			t.Fatal("hop headers must be removed")
		}
	})
	t.Run("git basic", func(t *testing.T) {
		r := newReq()
		applySandboxReadAuth(r, SandboxReadDecision{Allowed: true, Inject: true, Basic: true}, testReadToken)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testReadToken))
		if got := r.Header.Get("Authorization"); got != want {
			t.Fatalf("Authorization = %q", got)
		}
	})
	t.Run("agent credential stripped when not injecting", func(t *testing.T) {
		r := newReq()
		applySandboxReadAuth(r, SandboxReadDecision{Allowed: true}, testReadToken)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want none", got)
		}
	})
	t.Run("no token means anonymous, not the agent's header", func(t *testing.T) {
		r := newReq()
		applySandboxReadAuth(r, SandboxReadDecision{Allowed: true, Inject: true}, "")
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want none", got)
		}
	})
	t.Run("refused request gets no credential", func(t *testing.T) {
		r := newReq()
		applySandboxReadAuth(r, SandboxReadDecision{}, testReadToken)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want none", got)
		}
	})
}

// relayHarness serves a relay on loopback and fakes the GitHub upstream.
type relayHarness struct {
	relay *SandboxReadRelay
	addr  string
	pool  *x509.CertPool
	dials atomic.Int32

	mu       sync.Mutex
	upstream []*http.Request
}

func newRelayHarness(t *testing.T, token SandboxReadTokenFunc) *relayHarness {
	t.Helper()
	return newRelayHarnessWithLogger(t, token, nil)
}

// newRelayHarnessWithLogger is newRelayHarness with the proxy's logger replaced
// before the listener starts, so a test can read back everything the relay logs.
func newRelayHarnessWithLogger(t *testing.T, token SandboxReadTokenFunc, logger *slog.Logger) *relayHarness {
	t.Helper()
	p, _ := newTestProxyWithCA(t)
	if logger != nil {
		p.logger = logger
	}
	relay, err := NewSandboxReadRelay(p, SandboxReadRelayOptions{Endpoint: "https://" + relayTestHost + ":18445", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	h := &relayHarness{relay: relay, pool: x509.NewCertPool()}
	h.pool.AddCert(p.caX509)
	relay.dial = func(host string) (net.Conn, error) {
		h.dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			br := bufio.NewReader(server)
			for {
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				h.mu.Lock()
				h.upstream = append(h.upstream, req)
				h.mu.Unlock()
				_, _ = fmt.Fprint(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}
		}()
		return client, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// The relay is only ever served over TLS, exactly as boot does it.
	go func() { _ = relay.Serve(tls.NewListener(ln, relay.TLSConfig())) }()
	h.addr = ln.Addr().String()
	return h
}

func (h *relayHarness) seen() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*http.Request(nil), h.upstream...)
}

// connect opens a CONNECT tunnel through the relay and returns the TLS client
// conn, or the raw HTTP status when the relay refused the CONNECT.
func (h *relayHarness) connect(t *testing.T, proxyURLUser, target string) (*tls.Conn, int) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Leg 1: TLS to the relay itself, verified against the hive CA for the
	// endpoint host. Leg 2 (below) is the CONNECT tunnel's own TLS.
	var conn net.Conn = tls.Client(raw, &tls.Config{RootCAs: h.pool, ServerName: relayTestHost})
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	hdr := ""
	if proxyURLUser != "" {
		hdr = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxyURLUser)) + "\r\n"
	}
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, hdr)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}
	host, _, _ := net.SplitHostPort(target)
	tc := tls.Client(conn, &tls.Config{RootCAs: h.pool, ServerName: host})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake through the relay: %v", err)
	}
	return tc, 200
}

func roundTrip(t *testing.T, tc *tls.Conn, method, host, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, "https://"+host+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatal(err)
	}
	// Drain the body so a keep-alive tunnel is positioned at the next
	// response; callers that reuse the connection would otherwise parse the
	// leftover body bytes as a status line.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp
}

func enrollKey(t *testing.T, r *SandboxReadRelay, agentName string) (userinfo string, release func()) {
	t.Helper()
	proxyURL, _, rel, err := r.Enroll(agentName)
	if err != nil {
		t.Fatal(err)
	}
	rest := strings.TrimPrefix(proxyURL, "https://")
	userinfo, _, _ = strings.Cut(rest, "@")
	return userinfo, rel
}

func TestSandboxReadRelayInjectsOnReadAndRefusesWrites(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()

	tc, status := h.connect(t, user, "api.github.com:443")
	if status != 200 {
		t.Fatalf("CONNECT status = %d", status)
	}
	resp := roundTrip(t, tc, "GET", "api.github.com", "/repos/o/r/tags", map[string]string{"Authorization": "token AGENT_SUPPLIED"})
	if resp.StatusCode != 200 {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	seen := h.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if got := seen[0].Header.Get("Authorization"); got != "token "+testReadToken {
		t.Fatalf("upstream Authorization = %q, want the relay's read token (the agent's own header must be replaced)", got)
	}

	// A write is refused by the relay and never reaches GitHub, whatever the
	// agent put in the request.
	tc2, _ := h.connect(t, user, "api.github.com:443")
	before := h.dials.Load()
	resp = roundTrip(t, tc2, "POST", "api.github.com", "/repos/o/r/issues", map[string]string{"Authorization": "token AGENT_SUPPLIED"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403", resp.StatusCode)
	}
	if h.dials.Load() != before {
		t.Fatal("a refused write opened an upstream connection")
	}
	if len(h.seen()) != 1 {
		t.Fatal("a refused write reached GitHub")
	}
}

func TestSandboxReadRelayGitPushDiscoveryRefused(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	tc, _ := h.connect(t, user, "github.com:443")
	resp := roundTrip(t, tc, "GET", "github.com", "/o/r.git/info/refs?service=git-receive-pack", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if len(h.seen()) != 0 {
		t.Fatal("push discovery reached GitHub")
	}
}

func TestSandboxReadRelayGitFetchUsesBasicWithReadToken(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	tc, _ := h.connect(t, user, "github.com:443")
	resp := roundTrip(t, tc, "GET", "github.com", "/o/r.git/info/refs?service=git-upload-pack", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testReadToken))
	if got := h.seen()[0].Header.Get("Authorization"); got != want {
		t.Fatalf("upstream Authorization = %q", got)
	}
}

func TestSandboxReadRelayRequiresLiveKey(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	if _, status := h.connect(t, "", "api.github.com:443"); status != http.StatusProxyAuthRequired {
		t.Fatalf("no credentials: status = %d, want 407", status)
	}
	if _, status := h.connect(t, SandboxReadProxyUser+":not-a-key", "api.github.com:443"); status != http.StatusProxyAuthRequired {
		t.Fatalf("wrong key: status = %d, want 407", status)
	}
	user, release := enrollKey(t, h.relay, "deps")
	if _, status := h.connect(t, user, "api.github.com:443"); status != 200 {
		t.Fatalf("live key: status = %d, want 200", status)
	}
	release()
	if _, status := h.connect(t, user, "api.github.com:443"); status != http.StatusProxyAuthRequired {
		t.Fatalf("released key: status = %d, want 407", status)
	}
	if h.dials.Load() != 0 {
		t.Fatal("no request was made, so no upstream dial was expected")
	}
}

// TestSandboxReadRelayReleasedKeyStopsEstablishedTunnel pins #18895: the key
// is authenticated at CONNECT, but a tunnel that is already open must stop
// serving once the kick's key is released.
func TestSandboxReadRelayReleasedKeyStopsEstablishedTunnel(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	tc, status := h.connect(t, user, "api.github.com:443")
	if status != 200 {
		t.Fatalf("CONNECT status = %d", status)
	}
	if resp := roundTrip(t, tc, "GET", "api.github.com", "/repos/o/r/tags", nil); resp.StatusCode != 200 {
		t.Fatalf("GET with live key: status = %d, want 200", resp.StatusCode)
	}
	release()
	resp := roundTrip(t, tc, "GET", "api.github.com", "/repos/o/r/tags", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET after release: status = %d, want 403", resp.StatusCode)
	}
	if n := len(h.seen()); n != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (the post-release request must not reach GitHub)", n)
	}
}

func TestSandboxReadRelayRefusesNon443Connect(t *testing.T) {
	h := newRelayHarness(t, nil)
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	if _, status := h.connect(t, user, "api.github.com:8080"); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

func TestSandboxReadRelayNoTokenForwardsAnonymously(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return "", fmt.Errorf("mint failed") })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	tc, _ := h.connect(t, user, "api.github.com:443")
	_ = roundTrip(t, tc, "GET", "api.github.com", "/rate_limit", map[string]string{"Authorization": "token AGENT_SUPPLIED"})
	if got := h.seen()[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q, want none: the agent's header must not ride through", got)
	}
}

func TestSandboxReadRelayEnrollCarriesNoGitHubToken(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	proxyURL, bundle, release, err := h.relay.Enroll("deps")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if strings.Contains(proxyURL, testReadToken) || strings.Contains(string(bundle), testReadToken) {
		t.Fatal("the GitHub token must never be in what a Job receives")
	}
	if !strings.HasPrefix(proxyURL, "https://"+SandboxReadProxyUser+":") || !strings.Contains(string(bundle), "BEGIN CERTIFICATE") {
		t.Fatalf("unexpected enrollment: %q", proxyURL)
	}
}

func TestSandboxReadRelayTokenIsCached(t *testing.T) {
	var calls atomic.Int32
	h := newRelayHarness(t, func() (string, error) { calls.Add(1); return testReadToken, nil })
	now := time.Now()
	h.relay.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if h.relay.readToken() != testReadToken {
			t.Fatal("no token")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("minted %d times, want 1", calls.Load())
	}
	now = now.Add(sandboxReadTokenTTL + time.Second)
	_ = h.relay.readToken()
	if calls.Load() != 2 {
		t.Fatalf("minted %d times after TTL, want 2", calls.Load())
	}
}

func TestNewSandboxReadRelayRejectsCredentialedEndpoint(t *testing.T) {
	p, _ := newTestProxyWithCA(t)
	for _, ep := range []string{"", "ftp://x:1", "http://host:1", "https://u:p@host:1", "host:1"} {
		if _, err := NewSandboxReadRelay(p, SandboxReadRelayOptions{Endpoint: ep}); err == nil {
			t.Errorf("endpoint %q accepted", ep)
		}
	}
}

// A request inside the tunnel that names another host (Host header or absolute
// URL) must not borrow the CONNECT host's credential decision.
func TestSandboxReadRelayRefusesHostMismatchInsideTunnel(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()

	for _, tc := range []struct{ name, hostHeader, target string }{
		{"host header", "github.com", "/repos/o/r"},
		{"absolute url", "api.github.com", "https://evil.example/repos/o/r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, status := h.connect(t, user, "api.github.com:443")
			if status != 200 {
				t.Fatalf("CONNECT status = %d", status)
			}
			_, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", tc.target, tc.hostHeader)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if len(h.seen()) != 0 {
				t.Fatal("a mismatched-host request reached upstream with the CONNECT host's credential")
			}
		})
	}
}

// The read token and the per-kick relay key must never appear in the relay's
// logs, at the most verbose level, across injected, refused and anonymous
// requests.
func TestSandboxReadRelayNeverLogsTokenOrKey(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newRelayHarnessWithLogger(t, func() (string, error) { return testReadToken, nil }, logger)
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	_, key, _ := strings.Cut(user, ":")

	tc, _ := h.connect(t, user, "api.github.com:443")
	_ = roundTrip(t, tc, "GET", "api.github.com", "/repos/o/r", map[string]string{"Authorization": "token AGENT_SUPPLIED"})
	tc2, _ := h.connect(t, user, "api.github.com:443")
	_ = roundTrip(t, tc2, "POST", "api.github.com", "/repos/o/r/issues", map[string]string{"Authorization": "token AGENT_SUPPLIED"})
	_, _ = h.connect(t, SandboxReadProxyUser+":"+key+"x", "api.github.com:443")

	out := logs.String()
	if out == "" {
		t.Fatal("expected the relay to log its decisions")
	}
	for _, secret := range []string{testReadToken, key, "AGENT_SUPPLIED"} {
		if strings.Contains(out, secret) {
			t.Fatalf("relay logs contain a secret (%q):\n%s", secret, out)
		}
	}
}

// Each kick gets its own key, keys are unguessable, and releasing one kick
// leaves the others live.
func TestSandboxReadRelayKeysArePerKick(t *testing.T) {
	h := newRelayHarness(t, nil)
	u1, rel1 := enrollKey(t, h.relay, "deps")
	u2, rel2 := enrollKey(t, h.relay, "deps")
	defer rel2()
	if u1 == u2 {
		t.Fatal("two kicks of one agent shared a relay key")
	}
	if _, key, _ := strings.Cut(u1, ":"); len(key) < 2*sandboxReadKeyBytes {
		t.Fatalf("relay key too short: %d chars", len(key))
	}
	rel1()
	if _, status := h.connect(t, u1, "api.github.com:443"); status != http.StatusProxyAuthRequired {
		t.Fatalf("released kick key: status = %d, want 407", status)
	}
	if _, status := h.connect(t, u2, "api.github.com:443"); status != 200 {
		t.Fatalf("other kick key: status = %d, want 200", status)
	}
}

// A failing token endpoint is not hammered: one mint attempt per retry
// interval, requests in between go out anonymously, and recovery is picked up.
func TestSandboxReadRelayFailedMintIsNotRetriedPerRequest(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	h := newRelayHarness(t, func() (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", fmt.Errorf("mint failed")
		}
		return testReadToken, nil
	})
	now := time.Now()
	h.relay.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if tok := h.relay.readToken(); tok != "" {
			t.Fatalf("token = %q during an outage", tok)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("mint attempted %d times for 5 requests during an outage, want 1", calls.Load())
	}
	fail.Store(false)
	now = now.Add(sandboxReadTokenRetryAfter + time.Second)
	if h.relay.readToken() != testReadToken {
		t.Fatal("relay did not recover after the retry interval")
	}
}

// The listener serves TLS only. A plain-HTTP client, even one holding a live
// key, gets no tunnel and no proxy handling, and the key it sent in the clear
// is never logged.
func TestSandboxReadRelayRefusesPlainHTTP(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newRelayHarnessWithLogger(t, func() (string, error) { return testReadToken, nil }, logger)
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	_, key, _ := strings.Cut(user, ":")

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(user))
	_, _ = fmt.Fprintf(conn, "CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	if strings.Contains(reply, "200") || strings.Contains(reply, "407") {
		t.Fatalf("plain HTTP reached the relay's proxy handler: %q", reply)
	}
	if h.dials.Load() != 0 || len(h.seen()) != 0 {
		t.Fatal("a plain-HTTP request produced upstream traffic")
	}
	if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), auth) {
		t.Fatalf("the key sent over plain HTTP was logged:\n%s", logs.String())
	}
}

// The listener certificate is issued by the hive CA for the endpoint host, as a
// DNS or IP SAN, and for nothing else.
func TestSandboxReadRelayCertificateHasEndpointSAN(t *testing.T) {
	for _, tc := range []struct{ endpoint, name string }{
		{"https://hive.example.svc:18445", "hive.example.svc"},
		{"https://10.20.30.40:18445", "10.20.30.40"},
	} {
		p, _ := newTestProxyWithCA(t)
		relay, err := NewSandboxReadRelay(p, SandboxReadRelayOptions{Endpoint: tc.endpoint})
		if err != nil {
			t.Fatal(err)
		}
		cert, err := relay.TLSConfig().GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(p.caX509)
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: tc.name, Roots: pool}); err != nil {
			t.Errorf("%s: certificate does not verify for the endpoint host against the hive CA: %v", tc.endpoint, err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "other.example", Roots: pool}); err == nil {
			t.Errorf("%s: certificate verifies for an unrelated host", tc.endpoint)
		}
	}
}

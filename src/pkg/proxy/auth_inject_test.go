package proxy

// Tests for proxy-side GitHub credential injection (#1861). Each test names
// the attack its guard closes and was verified to FAIL with that guard
// removed (see the PR body's mutation-check evidence):
//
//   - injection: upstream must see the identified agent's hub-held scoped
//     token, sourced from the #3967 lane, not anything the agent sent.
//   - stripping: an agent-supplied Authorization header must NEVER reach
//     upstream — even a real credential smuggled to an agent is unspendable.
//   - unknown agent: no injection, no fallback token — the request goes out
//     unauthenticated and fails loud at GitHub.
//   - flag off: byte-identical passthrough, so the fleet is untouched until a
//     spoke opts in.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

const (
	// testScopedToken is the hub-held scoped token the fake source serves. Its
	// value is arbitrary; assertions compare against it verbatim.
	testScopedToken = "ghs_scoped_token_for_test"
	// testStolenToken is a credential the AGENT supplies — the thing the strip
	// guard must keep off the wire.
	testStolenToken = "ghs_stolen_agent_credential"
	// testAgentName is the UID-identified caller in the happy-path tests.
	testAgentName = "quality"
	// exchangeTimeout bounds each test's wait for the captured upstream
	// request, so a wedged relay fails the test instead of hanging the run.
	exchangeTimeout = 5 * time.Second
)

// upstreamCapture is what the fake upstream saw: the parsed request plus the
// raw bytes (the raw form is what the security assertions grep — a parsed
// header set could mask a duplicate or misfolded header).
type upstreamCapture struct {
	req *http.Request
	raw string
}

// runInjectionExchange drives one request through proxyHTTP with a fake
// client and a fake upstream, returning what the upstream received.
func runInjectionExchange(t *testing.T, p *GitHubProxy, agentName string, mode agent.AgentMode, rawReq string) upstreamCapture {
	t.Helper()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	// Track proxyHTTP's exit so the helper returns with NO goroutine still
	// touching package state (the git-path test shortens tunnelHalfCloseDrain
	// and must not restore it while a leaked relay could still read it).
	proxyDone := make(chan struct{})
	go func() {
		p.proxyHTTP(proxyClient, proxyUpstream, agentName, mode, agent.AgentCapabilities{})
		close(proxyDone)
	}()

	captured := make(chan upstreamCapture, 1)
	go func() {
		var raw bytes.Buffer
		tee := io.TeeReader(upstreamConn, &raw)
		req, err := http.ReadRequest(bufio.NewReader(tee))
		if err != nil {
			captured <- upstreamCapture{}
			return
		}
		// Drain any body so raw captures it too.
		if req.Body != nil {
			io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}
		fmt.Fprintf(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		upstreamConn.Close()
		captured <- upstreamCapture{req: req, raw: raw.String()}
	}()

	go func() {
		if _, err := io.WriteString(clientConn, rawReq); err != nil {
			return
		}
		// Read whatever comes back, then hang up so both relay directions end.
		io.Copy(io.Discard, clientConn)
		clientConn.Close()
	}()

	var c upstreamCapture
	select {
	case c = <-captured:
	case <-time.After(exchangeTimeout):
		t.Fatal("timed out waiting for the upstream to receive the request")
	}

	// Tear down both conns, then wait for proxyHTTP to fully exit before
	// returning — see proxyDone above.
	clientConn.Close()
	upstreamConn.Close()
	select {
	case <-proxyDone:
	case <-time.After(exchangeTimeout):
		t.Fatal("timed out waiting for proxyHTTP to exit")
	}

	if c.req == nil {
		t.Fatal("fake upstream failed to read a request")
	}
	return c
}

// injectionTestProxy returns a proxy with injection ON and a token source that
// serves testScopedToken for every name, recording the names it was asked for.
func injectionTestProxy(calls *[]string) *GitHubProxy {
	p := &GitHubProxy{
		logger:       slog.Default(),
		violations:   make(map[string]int),
		certCache:    make(map[string]cachedCert),
		injectGHAuth: true,
	}
	p.agentTokenSource = func(name string) (string, bool) {
		if calls != nil {
			*calls = append(*calls, name)
		}
		return testScopedToken, true
	}
	return p
}

// TestInjectAuth_IdentifiedAgentGetsScopedToken: the core of #1861 — a request
// from a UID-identified agent reaches GitHub bearing that agent's hub-held
// scoped token, even though the agent itself sent no (usable) credential.
func TestInjectAuth_IdentifiedAgentGetsScopedToken(t *testing.T) {
	p := injectionTestProxy(nil)

	c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
		"GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token hive-proxy-injected-quality\r\n\r\n")

	if got, want := c.req.Header.Get("Authorization"), "token "+testScopedToken; got != want {
		t.Errorf("upstream Authorization = %q, want %q", got, want)
	}
	if strings.Contains(c.raw, "hive-proxy-injected") {
		t.Errorf("the agent-side placeholder reached upstream:\n%s", c.raw)
	}
}

// TestInjectAuth_AgentSuppliedAuthorizationNeverReachesUpstream is the
// security-critical strip guard: whatever credential the agent attached must
// not appear anywhere in the bytes GitHub receives — neither when the proxy
// substitutes a hub token nor when it has none to substitute.
func TestInjectAuth_AgentSuppliedAuthorizationNeverReachesUpstream(t *testing.T) {
	t.Run("replaced by hub token", func(t *testing.T) {
		p := injectionTestProxy(nil)
		c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
			"GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testStolenToken+"\r\n\r\n")

		if strings.Contains(c.raw, testStolenToken) {
			t.Fatalf("agent-supplied credential reached upstream:\n%s", c.raw)
		}
		if got, want := c.req.Header.Get("Authorization"), "token "+testScopedToken; got != want {
			t.Errorf("upstream Authorization = %q, want %q", got, want)
		}
		if n := len(c.req.Header.Values("Authorization")); n != 1 {
			t.Errorf("upstream saw %d Authorization headers, want exactly 1 (a second one would be the smuggled original)", n)
		}
	})

	t.Run("no hub token available — stripped, nothing substituted", func(t *testing.T) {
		p := injectionTestProxy(nil)
		p.agentTokenSource = func(string) (string, bool) { return "", false }
		c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
			"GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testStolenToken+"\r\n\r\n")

		if strings.Contains(c.raw, testStolenToken) {
			t.Fatalf("agent-supplied credential reached upstream:\n%s", c.raw)
		}
		if got := c.req.Header.Get("Authorization"); got != "" {
			t.Errorf("upstream Authorization = %q, want none (fail loud at GitHub, no fallback)", got)
		}
	})
}

// TestInjectAuth_UnknownAgentNoInjectionNoFallback: identity failure must not
// be rewarded with a credential. A shared or ambient fallback here would
// recreate the pre-#3888 hole where an unattributable process rides another
// identity's token — so the token source must not even be consulted.
func TestInjectAuth_UnknownAgentNoInjectionNoFallback(t *testing.T) {
	var calls []string
	p := injectionTestProxy(&calls)

	c := runInjectionExchange(t, p, "", agent.ModeAdvisory,
		"GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testStolenToken+"\r\n\r\n")

	if got := c.req.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream Authorization = %q, want none for an unidentified caller", got)
	}
	if strings.Contains(c.raw, testStolenToken) {
		t.Errorf("agent-supplied credential reached upstream:\n%s", c.raw)
	}
	if len(calls) != 0 {
		t.Errorf("token source consulted for an unidentified caller (%v) — that is a fallback path and must not exist", calls)
	}
}

// TestInjectAuth_FlagOffByteIdenticalPassthrough: with the flag off (the
// fleet-wide default while this soaks) the proxy must not touch any header —
// the agent's own credential flows exactly as before #1861.
func TestInjectAuth_FlagOffByteIdenticalPassthrough(t *testing.T) {
	var calls []string
	p := injectionTestProxy(&calls)
	p.injectGHAuth = false

	c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
		"GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testStolenToken+"\r\n\r\n")

	if got, want := c.req.Header.Get("Authorization"), "token "+testStolenToken; got != want {
		t.Errorf("flag-off Authorization = %q, want the agent's own untouched %q", got, want)
	}
	if len(calls) != 0 {
		t.Errorf("token source consulted with the flag off (%v)", calls)
	}
}

// TestInjectAuth_GitPathBasicAndConnectionClose: git smart HTTP gets the
// Basic x-access-token form (what git-credential-hive.sh produced before) and
// Connection: close, so git's next request cannot ride the raw relay past the
// rewrite carrying the placeholder.
func TestInjectAuth_GitPathBasicAndConnectionClose(t *testing.T) {
	origDrain := tunnelHalfCloseDrain.Load()
	tunnelHalfCloseDrain.Store(int64(100 * time.Millisecond))
	defer tunnelHalfCloseDrain.Store(origDrain)

	p := injectionTestProxy(nil)

	c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
		"GET /org/repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\nAuthorization: Basic ZHVtbXk6ZHVtbXk=\r\n\r\n")

	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser+":"+testScopedToken))
	if got := c.req.Header.Get("Authorization"); got != wantBasic {
		t.Errorf("git-path Authorization = %q, want %q", got, wantBasic)
	}
	if !c.req.Close && !strings.Contains(strings.ToLower(c.raw), "connection: close") {
		t.Errorf("git-path request not marked Connection: close — a keep-alive follow-up would bypass the rewrite via the raw relay:\n%s", c.raw)
	}
}

// TestInjectAuth_InternalCallerPassthrough: the hive's own control-plane
// requests (App mint, heartbeat, relay fulfillment) legitimately carry the
// hive's credential and must pass untouched — and the per-agent source must
// not be consulted for them.
func TestInjectAuth_InternalCallerPassthrough(t *testing.T) {
	var calls []string
	p := injectionTestProxy(&calls)

	const hiveOwnAuth = "token ghs_hive_control_plane"
	c := runInjectionExchange(t, p, internalCallerName, agent.ModeAdvisory,
		"GET /app/installations HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: "+hiveOwnAuth+"\r\n\r\n")

	if got := c.req.Header.Get("Authorization"); got != hiveOwnAuth {
		t.Errorf("internal caller Authorization = %q, want untouched %q", got, hiveOwnAuth)
	}
	if len(calls) != 0 {
		t.Errorf("token source consulted for the internal caller (%v)", calls)
	}
}

// TestInjectAuth_LoginPathStripOnly: OAuth device-flow endpoints authenticate
// via their form body; the rewrite must strip any agent-attached header but
// must NOT attach an App token to an OAuth flow.
func TestInjectAuth_LoginPathStripOnly(t *testing.T) {
	p := injectionTestProxy(nil)

	c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
		"POST /login/device/code HTTP/1.1\r\nHost: github.com\r\nAuthorization: token "+testStolenToken+"\r\nContent-Length: 0\r\n\r\n")

	if got := c.req.Header.Get("Authorization"); got != "" {
		t.Errorf("login-path Authorization = %q, want none (strip only)", got)
	}
	if strings.Contains(c.raw, testStolenToken) || strings.Contains(c.raw, testScopedToken) {
		t.Errorf("credential material on an OAuth flow request:\n%s", c.raw)
	}
}

// TestHostNeedsMITM_InjectionWidensInterception: without injection only
// api.github.com is intercepted (historical behavior); with injection every
// GitHub-family host must be, because an opaque tunnel would carry the
// agent's placeholder credential to GitHub un-replaced (and would let a
// smuggled real credential through un-stripped).
func TestHostNeedsMITM_InjectionWidensInterception(t *testing.T) {
	const gheHost = "ghe.injection-test.example.com"
	RegisterGitHubHost(gheHost)
	defer unregisterGitHubHost(gheHost)

	off := &GitHubProxy{injectGHAuth: false}
	on := &GitHubProxy{injectGHAuth: true}

	cases := []struct {
		p    *GitHubProxy
		host string
		want bool
	}{
		{off, "api.github.com", true},
		{off, "github.com", false},
		{off, gheHost, false},
		{on, "api.github.com", true},
		{on, "github.com", true},
		{on, gheHost, true},
		// Never MITM hosts the proxy does not front, flag or no flag.
		{off, "example.com", false},
		{on, "example.com", false},
	}
	for _, tc := range cases {
		flag := "off"
		if tc.p.injectGHAuth {
			flag = "on"
		}
		if got := tc.p.hostNeedsMITM(tc.host); got != tc.want {
			t.Errorf("hostNeedsMITM(%q) with injection %s = %v, want %v", tc.host, flag, got, tc.want)
		}
	}
}

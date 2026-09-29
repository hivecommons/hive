package proxy

// #9586 phase 2: in credential-sidecar mode the proxy holds no agent token.
// Every non-internal GitHub request that passes the policy gates is signed and
// sent to the sidecar; the proxy never consults the in-process token source,
// never forwards the agent's own credential, and never falls back to a direct
// upstream request when the sidecar leg fails.

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/credsidecar"
)

var sidecarTestKey = []byte(strings.Repeat("p", credsidecar.MinKeyBytes))

// sidecarSeen is what the fake sidecar received, with the signature checked
// against the shared key the way the real sidecar checks it.
type sidecarSeen struct {
	method, uri, host, agentName, tier string
	header                             http.Header
	signatureValid                     bool
}

type fakeSidecar struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []sidecarSeen
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(credsidecar.HeaderTimestamp), 10, 64)
		fields := credsidecar.SignedFields{
			Method:     r.Method,
			Host:       r.Header.Get(credsidecar.HeaderHost),
			RequestURI: r.RequestURI,
			Agent:      r.Header.Get(credsidecar.HeaderAgent),
			Tier:       r.Header.Get(credsidecar.HeaderTier),
			Timestamp:  ts,
			Nonce:      r.Header.Get(credsidecar.HeaderNonce),
			BodySHA256: credsidecar.BodyDigest(body),
		}
		want, _ := credsidecar.Sign(sidecarTestKey, fields)
		f.mu.Lock()
		f.seen = append(f.seen, sidecarSeen{
			method: r.Method, uri: r.RequestURI, host: fields.Host, agentName: fields.Agent, tier: fields.Tier,
			header: r.Header.Clone(),
			signatureValid: want != "" && want == r.Header.Get(credsidecar.HeaderSignature) &&
				fields.BodySHA256 == r.Header.Get(credsidecar.HeaderBodySHA256),
		})
		f.mu.Unlock()
		w.Header().Set("X-From", "sidecar")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"relayed":true}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSidecar) requests() []sidecarSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sidecarSeen(nil), f.seen...)
}

func (f *fakeSidecar) client(t *testing.T, maxBody int64) *credsidecar.Client {
	t.Helper()
	u, _ := url.Parse(f.srv.URL)
	c, err := credsidecar.NewClient(credsidecar.ClientConfig{URL: u, Key: sidecarTestKey, MaxBodyBytes: maxBody})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sidecarModeProxy is a proxy in sidecar mode whose in-process token source
// fails the test if it is ever consulted: in sidecar mode that registry is
// empty and must stay unused.
func sidecarModeProxy(t *testing.T, c *credsidecar.Client, tiers map[string]string) *GitHubProxy {
	t.Helper()
	p := &GitHubProxy{
		logger:          slog.Default(),
		violations:      make(map[string]int),
		certCache:       make(map[string]cachedCert),
		injectGHAuth:    true,
		credSidecarMode: true,
		credSidecar:     c,
	}
	p.SetAgentTokenSource(func(name string) (string, bool) {
		t.Errorf("in-process token source consulted for %q in sidecar mode", name)
		return testScopedToken, true
	})
	p.SetAgentTierSource(func(name string) (string, bool) {
		tier, ok := tiers[name]
		return tier, ok
	})
	return p
}

type sidecarExchange struct {
	resp        *http.Response
	body        string
	upstreamHit bool
}

// runSidecarExchange sends one raw request through proxyHTTPHost and returns
// the agent-visible response and whether the proxy wrote anything to its
// direct GitHub upstream.
func runSidecarExchange(t *testing.T, p *GitHubProxy, host, agentName string, mode agent.AgentMode, rawReq string) sidecarExchange {
	t.Helper()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	proxyDone := make(chan struct{})
	go func() {
		p.proxyHTTPHost(proxyClient, proxyUpstream, host, agentName, mode, agent.AgentCapabilities{})
		close(proxyDone)
	}()
	upstreamHit := make(chan bool, 1)
	go func() {
		buf := make([]byte, 1)
		n, _ := upstreamConn.Read(buf)
		upstreamHit <- n > 0
	}()

	go func() { _, _ = io.WriteString(clientConn, rawReq) }()
	_ = clientConn.SetReadDeadline(time.Now().Add(exchangeTimeout))
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("reading the proxy's response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	_ = clientConn.Close()
	select {
	case <-proxyDone:
	case <-time.After(exchangeTimeout):
		t.Fatal("proxyHTTPHost did not exit")
	}
	_ = upstreamConn.Close()
	hit := <-upstreamHit
	return sidecarExchange{resp: resp, body: string(body), upstreamHit: hit}
}

func TestCredSidecar_AgentRequestIsSignedAndRelayed(t *testing.T) {
	sc := newFakeSidecar(t)
	p := sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "contributor"})

	ex := runSidecarExchange(t, p, "api.github.com", testAgentName, agent.ModeIssuesAndPRs,
		"GET /repos/o/r/pulls?state=open HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testStolenToken+"\r\nAccept: application/json\r\n\r\n")

	if ex.resp.StatusCode != http.StatusCreated || ex.body != `{"relayed":true}` || ex.resp.Header.Get("X-From") != "sidecar" {
		t.Fatalf("agent saw %d %q, want the sidecar's relayed response", ex.resp.StatusCode, ex.body)
	}
	if ex.upstreamHit {
		t.Fatal("the proxy wrote to GitHub directly in sidecar mode")
	}
	reqs := sc.requests()
	if len(reqs) != 1 {
		t.Fatalf("sidecar saw %d requests", len(reqs))
	}
	got := reqs[0]
	if !got.signatureValid {
		t.Fatal("the proxy's signature did not verify under the shared key")
	}
	if got.method != "GET" || got.uri != "/repos/o/r/pulls?state=open" || got.host != "api.github.com" ||
		got.agentName != testAgentName || got.tier != "contributor" {
		t.Fatalf("signed fields = %+v", got)
	}
	if auth := got.header.Get("Authorization"); auth != "" {
		t.Fatalf("the agent's own credential reached the sidecar: %q", auth)
	}
	if got.header.Get("Accept") != "application/json" {
		t.Fatal("ordinary headers were not relayed")
	}
}

// An unidentified caller and an agent with no recorded tier are both sent with
// tier "": the sidecar forwards them with NO credential (fail loud), exactly
// like the in-process path's no-fallback rule.
func TestCredSidecar_NoTierMeansNoCredential(t *testing.T) {
	for _, name := range []string{"", "unknown-agent"} {
		sc := newFakeSidecar(t)
		p := sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "trusted"})
		ex := runSidecarExchange(t, p, "api.github.com", name, agent.ModeAdvisory,
			"GET /repos/o/r HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
		if ex.resp.StatusCode != http.StatusCreated || ex.upstreamHit {
			t.Fatalf("agent %q: status %d upstreamHit=%v", name, ex.resp.StatusCode, ex.upstreamHit)
		}
		if reqs := sc.requests(); len(reqs) != 1 || reqs[0].tier != "" {
			t.Fatalf("agent %q: sidecar saw %+v, want one request with no tier", name, reqs)
		}
	}
}

// The OAuth flow endpoints never carry a credential, sidecar or not; nor does
// a request when no tier source is wired.
func TestCredSidecar_LoginPathAndNoTierSourceHaveNoTier(t *testing.T) {
	sc := newFakeSidecar(t)
	p := sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "trusted"})
	_ = runSidecarExchange(t, p, "github.com", testAgentName, agent.ModeAdvisory,
		"GET /login/device HTTP/1.1\r\nHost: github.com\r\n\r\n")
	p = sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "trusted"})
	p.SetAgentTierSource(nil)
	_ = runSidecarExchange(t, p, "api.github.com", testAgentName, agent.ModeAdvisory,
		"GET /user HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	reqs := sc.requests()
	if len(reqs) != 2 {
		t.Fatalf("sidecar saw %d requests, want 2", len(reqs))
	}
	for _, r := range reqs {
		if r.tier != "" {
			t.Fatalf("%s signed with tier %q, want none", r.uri, r.tier)
		}
	}
}

// A GitHub-implied relay (empty host) is signed for api.github.com, so the
// sidecar's host allowlist has an explicit host to check.
func TestCredSidecar_ImpliedHostIsAPIGitHub(t *testing.T) {
	sc := newFakeSidecar(t)
	p := sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "contributor"})
	_ = runSidecarExchange(t, p, "", testAgentName, agent.ModeIssuesAndPRs, "GET /user HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	if reqs := sc.requests(); len(reqs) != 1 || reqs[0].host != defaultGitHubAPIHost {
		t.Fatalf("sidecar saw %+v, want host %s", reqs, defaultGitHubAPIHost)
	}
}

// Unusable sidecar config: refuse (502), never fall back to a direct request.
func TestCredSidecar_UnusableSidecarFailsClosed(t *testing.T) {
	p := sidecarModeProxy(t, nil, map[string]string{testAgentName: "contributor"})
	ex := runSidecarExchange(t, p, "api.github.com", testAgentName, agent.ModeIssuesAndPRs,
		"GET /user HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	if ex.resp.StatusCode != http.StatusBadGateway || ex.resp.Header.Get(credsidecar.HeaderRefused) == "" {
		t.Fatalf("status %d, want 502 with %s", ex.resp.StatusCode, credsidecar.HeaderRefused)
	}
	if ex.upstreamHit {
		t.Fatal("fell back to a direct GitHub request")
	}
}

// A sidecar that is down, and a body over the signing limit, are refused on
// the proxy side; nothing reaches GitHub either way.
func TestCredSidecar_RelayFailures(t *testing.T) {
	sc := newFakeSidecar(t)
	c := sc.client(t, 8)
	p := sidecarModeProxy(t, c, map[string]string{testAgentName: "contributor"})
	gql := `{"query":"query { viewer { id } }"}`
	ex := runSidecarExchange(t, p, "api.github.com", testAgentName, agent.ModeIssuesAndPRs,
		"POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: "+strconv.Itoa(len(gql))+"\r\n\r\n"+gql)
	if ex.resp.StatusCode != http.StatusRequestEntityTooLarge || ex.upstreamHit || len(sc.requests()) != 0 {
		t.Fatalf("oversized: status %d upstreamHit=%v sidecar=%d", ex.resp.StatusCode, ex.upstreamHit, len(sc.requests()))
	}

	sc.srv.Close()
	p = sidecarModeProxy(t, sc.client(t, 0), map[string]string{testAgentName: "contributor"})
	ex = runSidecarExchange(t, p, "api.github.com", testAgentName, agent.ModeIssuesAndPRs,
		"GET /user HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	if ex.resp.StatusCode != http.StatusBadGateway || ex.upstreamHit {
		t.Fatalf("sidecar down: status %d upstreamHit=%v", ex.resp.StatusCode, ex.upstreamHit)
	}
}

// Requests for a non-GitHub host (Linear) never go to the sidecar.
func TestCredSidecar_NonGitHubHostNotRelayed(t *testing.T) {
	if injectsGitHubAuthForHost("api.linear.app") {
		t.Fatal("Linear is treated as a GitHub host; it would be sent to the credential sidecar")
	}
}

// The constructor snapshots sidecar mode from the environment and builds the
// signer; with an unusable key it stays in sidecar mode with no client (fail
// closed), and without injection it never enters sidecar mode.
func TestNewGitHubProxy_CredSidecarMode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	keyPath := filepath.Join(t.TempDir(), "hmac")
	if err := os.WriteFile(keyPath, sidecarTestKey, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(config.ProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOnValue)
	t.Setenv(credsidecar.URLEnv, credsidecar.DefaultURL)
	t.Setenv(credsidecar.KeyFileEnv, keyPath)
	p, err := NewGitHubProxyEphemeral(logger, "o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.credSidecarMode || p.credSidecar == nil {
		t.Fatalf("sidecar mode=%v client nil=%v, want both set", p.credSidecarMode, p.credSidecar == nil)
	}

	t.Setenv(credsidecar.KeyFileEnv, filepath.Join(t.TempDir(), "absent"))
	p, err = NewGitHubProxyEphemeral(logger, "o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.credSidecarMode || p.credSidecar != nil {
		t.Fatalf("unusable key: mode=%v client nil=%v, want mode on with no client (fail closed)", p.credSidecarMode, p.credSidecar == nil)
	}

	t.Setenv(config.ProxyInjectGHAuthEnv, "")
	p, err = NewGitHubProxyEphemeral(logger, "o", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.credSidecarMode {
		t.Fatal("sidecar mode without injection")
	}
}

func TestEgressMarkDialContext_Dials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	conn, err := EgressMarkDialContext(time.Second)(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
}

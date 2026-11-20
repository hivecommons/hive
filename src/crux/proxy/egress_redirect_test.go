package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// redirectSink is the host a redirect points at; it records the credential
// headers it was sent.
type redirectSink struct {
	srv  *httptest.Server
	hits atomic.Int32
	auth atomic.Value
	proj atomic.Value
	tok  atomic.Value
}

func newRedirectSink(t *testing.T) *redirectSink {
	t.Helper()
	s := &redirectSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.auth.Store(r.Header.Get("Authorization"))
		s.proj.Store(r.Header.Get("X-IBM-Project-ID"))
		s.tok.Store(r.Header.Get("X-Route-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m","max_model_len":4096}],"info":{"models":["m"]}}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func loadString(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

// redirectingOrigin answers every request with a 307 to target+path. Both are
// 127.0.0.1 listeners on different ports: net/http's own policy treats that as
// the same domain and keeps Authorization, which is exactly the gap closed.
func redirectingOrigin(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEgressCheckRedirectStripsCredentialsOffHost(t *testing.T) {
	sink := newRedirectSink(t)
	origin := redirectingOrigin(t, sink.srv.URL)

	client := &http.Client{CheckRedirect: egressCheckRedirect("X-Route-Token")}
	req, _ := http.NewRequest(http.MethodPost, origin.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-IBM-Project-ID", "proj-1")
	req.Header.Set("X-Route-Token", "tok")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sink.hits.Load() != 1 {
		t.Fatalf("redirect target hit %d times, want 1", sink.hits.Load())
	}
	if got := loadString(&sink.auth); got != "" {
		t.Errorf("cross-host hop carried Authorization %q", got)
	}
	if got := loadString(&sink.proj); got != "" {
		t.Errorf("cross-host hop carried X-IBM-Project-ID %q", got)
	}
	if got := loadString(&sink.tok); got != "" {
		t.Errorf("cross-host hop carried route header %q", got)
	}
}

func TestEgressCheckRedirectKeepsCredentialsSameHost(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: egressCheckRedirect()}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/old", nil)
	req.Header.Set("Authorization", "Bearer keep")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := loadString(&auth); got != "Bearer keep" {
		t.Fatalf("same-host hop Authorization = %q, want it kept", got)
	}
}

func TestEgressCheckRedirectUnit(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		r := &http.Request{URL: u, Header: http.Header{}}
		r.Header.Set("Authorization", "Bearer x")
		return r
	}
	check := egressCheckRedirect()

	// https -> http on the same host is a downgrade: credentials go.
	r := mk("http://api.example/x")
	if err := check(r, []*http.Request{mk("https://api.example/x")}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("https -> http downgrade kept Authorization")
	}

	// Subdomain is another host for this policy.
	r = mk("https://evil.api.example/x")
	if err := check(r, []*http.Request{mk("https://api.example/x")}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("subdomain hop kept Authorization")
	}

	// Host comparison is case-insensitive.
	r = mk("https://API.example/y")
	if err := check(r, []*http.Request{mk("https://api.example/x")}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") == "" {
		t.Error("same host differing only in case lost Authorization")
	}

	// Non-http(s) schemes fail closed.
	if err := check(mk("file:///etc/passwd"), []*http.Request{mk("https://api.example/x")}); err == nil {
		t.Error("file:// redirect followed")
	}

	// The hop bound is egressMaxRedirects.
	via := make([]*http.Request, egressMaxRedirects)
	for i := range via {
		via[i] = mk("https://api.example/x")
	}
	if err := check(mk("https://api.example/x"), via); err == nil {
		t.Errorf("hop %d followed, want stop at %d", egressMaxRedirects+1, egressMaxRedirects)
	}
	if err := check(mk("https://api.example/x"), via[:egressMaxRedirects-1]); err != nil {
		t.Errorf("hop %d refused: %v", egressMaxRedirects, err)
	}
}

func TestEgressClientsStopAfterMaxRedirects(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/hop%d", n), http.StatusFound)
	}))
	defer srv.Close()

	client, err := inferenceHTTPClient(&InferenceRoute{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(srv.URL + "/start")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("endless redirect chain did not fail")
	}
	// net/http consults CheckRedirect with every request made so far in via,
	// so the bound is on the total requests in one chain (same reading as
	// ssrfMaxRedirects in pkg/dashboard).
	if got := hits.Load(); got != egressMaxRedirects {
		t.Fatalf("origin hit %d times, want %d", got, egressMaxRedirects)
	}
}

// inferenceHTTPClient must strip the route's Authorization and ExtraHeaders on
// a cross-host redirect.
func TestInferenceHTTPClientStripsRouteCredentialsOnRedirect(t *testing.T) {
	sink := newRedirectSink(t)
	origin := redirectingOrigin(t, sink.srv.URL)

	route := &InferenceRoute{
		Endpoint:     origin.URL,
		APIKey:       "sk-route",
		ExtraHeaders: map[string]string{"X-IBM-Project-ID": "proj-1", "X-Route-Token": "tok"},
	}
	client, err := inferenceHTTPClient(route)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, origin.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	applyInferenceAuth(req, route)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sink.hits.Load() != 1 {
		t.Fatalf("redirect target hit %d times, want 1", sink.hits.Load())
	}
	if a, p, k := loadString(&sink.auth), loadString(&sink.proj), loadString(&sink.tok); a != "" || p != "" || k != "" {
		t.Fatalf("redirect target saw Authorization=%q X-IBM-Project-ID=%q X-Route-Token=%q, want all stripped", a, p, k)
	}
}

// Model discovery, max_model_len lookup, the entitlement probe and the
// reference lookup all send a bearer token and must not carry it off-host.
func TestModelDiscoveryAndProbesStripAuthOnRedirect(t *testing.T) {
	cases := []struct {
		name string
		run  func(endpoint string)
	}{
		{"FindEndpointForModel", func(ep string) { _ = FindEndpointForModel([]string{ep}, "m", "sk-secret", "") }},
		{"queryMaxModelLen", func(ep string) { _ = queryMaxModelLen(ep, "m", "sk-secret", "") }},
		{"probeEntitledModels", func(ep string) { _ = probeEntitledModels(ep, "sk-secret", nil) }},
		{"defaultReferenceLookup", func(ep string) { _, _ = defaultReferenceLookup(ep+"/repos/o/r", "ghp-secret") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newRedirectSink(t)
			origin := redirectingOrigin(t, sink.srv.URL)
			tc.run(origin.URL)
			if sink.hits.Load() == 0 {
				t.Fatal("redirect was not followed at all; the test exercises nothing")
			}
			if got := loadString(&sink.auth); got != "" {
				t.Fatalf("cross-host hop carried Authorization %q", got)
			}
		})
	}
}

// forwardPlainDirect RoundTrips the agent's request: a redirect from the
// upstream is relayed to the agent verbatim and never followed by the proxy,
// so the proxy never makes a second request on the agent's behalf.
func TestForwardPlainDirectNeverFollowsRedirects(t *testing.T) {
	sink := newRedirectSink(t)
	origin := redirectingOrigin(t, sink.srv.URL)

	p := newTestProxy()
	p.allowPrivateForward = true // both servers are loopback httptest listeners

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent")
	resp := plainForwardRoundTrip(t, p, req)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the upstream's 307 relayed", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, sink.srv.URL) {
		t.Fatalf("Location = %q, want the upstream's redirect target", loc)
	}
	if sink.hits.Load() != 0 {
		t.Fatal("the proxy followed the redirect itself")
	}
}

// Decision for the hostname case of dialUpstreamRaw on the chained path
// (hivecommons/hive#19222): the upstream proxy resolves the name, so the hive
// does not resolve it locally (that answer can differ from the proxy's under
// split DNS, and a local check of a name someone else resolves is a TOCTOU
// with no enforcement value). The name is handed to the upstream proxy
// verbatim and the upstream proxy's own egress policy governs it. Literal IPs,
// including bracketed IPv6 and v4-mapped forms, are still classified locally
// and refused before the upstream proxy is asked.
func TestDialUpstreamRawHostnameDelegatedToUpstreamProxy(t *testing.T) {
	cp := startTestConnectProxy(t)
	cp.refuse.Store(true) // the name does not exist; the proxy answers 502
	p := newChainedTestProxy(t, cp, []string{"internal.hive.test", "fd00::/8", "10.0.0.0/8"})
	p.allowPrivateForward = false

	_, err := p.dialUpstreamRaw("internal.hive.test:443", 2*time.Second)
	var denied *plainForwardDenied
	if errors.As(err, &denied) {
		t.Fatalf("hostname refused locally (%v); it must be delegated to the upstream proxy", err)
	}
	if cp.targets.Load() != 1 {
		t.Fatalf("upstream proxy asked %d times, want 1", cp.targets.Load())
	}
	if got, _ := cp.last.Load().(string); got != "internal.hive.test:443" {
		t.Fatalf("upstream proxy asked for %q, want the hostname verbatim", got)
	}

	for _, target := range []string{
		net.JoinHostPort("fd00::1", "443"),
		net.JoinHostPort("::ffff:10.1.2.3", "443"),
	} {
		_, err := p.dialUpstreamRaw(target, 2*time.Second)
		if !errors.As(err, &denied) {
			t.Errorf("%s: err = %v, want plainForwardDenied", target, err)
		}
	}
	if cp.targets.Load() != 1 {
		t.Fatal("a refused literal reached the upstream proxy")
	}
}

func egressRedirectReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Api-Key", "k")
	r.Header.Set("Proxy-Authorization", "Basic x")
	return r
}

func stubEgressResolver(t *testing.T, m map[string]string) {
	t.Helper()
	orig := egressResolveHost
	egressResolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := m[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { egressResolveHost = orig })
}

func TestEgressCheckRedirectStripsAPIKeyCrossHost(t *testing.T) {
	stubEgressResolver(t, map[string]string{"a.example": "93.184.216.34", "b.example": "93.184.216.35"})
	check := egressCheckRedirect()
	via := []*http.Request{egressRedirectReq(t, "https://a.example/x")}
	off := egressRedirectReq(t, "https://b.example/x")
	if err := check(off, via); err != nil {
		t.Fatal(err)
	}
	if off.Header.Get("X-Api-Key") != "" || off.Header.Get("Proxy-Authorization") != "" {
		t.Error("cross-host hop kept X-Api-Key or Proxy-Authorization")
	}
	same := egressRedirectReq(t, "https://a.example/y")
	if err := check(same, via); err != nil {
		t.Fatal(err)
	}
	if same.Header.Get("X-Api-Key") == "" {
		t.Error("same-host hop lost X-Api-Key")
	}
}

func TestEgressCheckRedirectRefusesPrivateTargets(t *testing.T) {
	stubEgressResolver(t, map[string]string{
		"pub.example": "93.184.216.34", "other.example": "93.184.216.35",
		"rebind.example": "10.0.0.5", "meta.example": "169.254.169.254",
		"inner.example": "10.0.0.9",
	})
	check := egressCheckRedirect()
	pubVia := []*http.Request{egressRedirectReq(t, "https://pub.example/x")}
	for _, target := range []string{
		"https://127.0.0.1/x", "https://169.254.169.254/latest", "https://10.1.2.3/x",
		"https://[::1]/x", "https://rebind.example/x", "https://meta.example/x",
		"https://unresolvable.example/x", "https://localhost/x",
	} {
		if err := check(egressRedirectReq(t, target), pubVia); err == nil {
			t.Errorf("%s: redirect from a public endpoint was allowed", target)
		}
	}
	if err := check(egressRedirectReq(t, "https://other.example/x"), pubVia); err != nil {
		t.Errorf("public target refused: %v", err)
	}
	// An in-cluster backend (private original) may redirect within the estate.
	privVia := []*http.Request{egressRedirectReq(t, "http://inner.example/x")}
	if err := check(egressRedirectReq(t, "http://10.0.0.7/x"), privVia); err != nil {
		t.Errorf("private original to private target refused: %v", err)
	}
}

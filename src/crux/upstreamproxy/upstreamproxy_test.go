package upstreamproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveConfigHostsAndNoProxy(t *testing.T) {
	sel, err := Resolve(Config{
		URL:     "http://user:pw@proxy.example:3128",
		Hosts:   []string{"github.ibm.com", "*.github.ibm.com"},
		NoProxy: []string{"raw.github.ibm.com"},
	}, nil)
	if err != nil || sel == nil {
		t.Fatalf("Resolve: sel=%v err=%v", sel, err)
	}
	if sel.Source() != "config" || !sel.Restricted() {
		t.Fatalf("source=%q restricted=%v", sel.Source(), sel.Restricted())
	}
	cases := map[string]bool{
		"github.ibm.com":         true,
		"GITHUB.IBM.COM:443":     true,
		"api.github.ibm.com:443": true,
		"raw.github.ibm.com":     false, // no_proxy beats hosts
		"github.com":             false,
		"api.github.com":         false,
		"localhost:8080":         false,
		"127.0.0.1":              false,
	}
	for host, want := range cases {
		if got := sel.ProxyFor(host) != nil; got != want {
			t.Errorf("ProxyFor(%q) via proxy = %v, want %v", host, got, want)
		}
	}
	if u := sel.URL(); u.Host != "proxy.example:3128" || u.User.Username() != "user" {
		t.Fatalf("proxy URL = %v", u)
	}
}

func TestResolveEmptyHostsMeansAllButNoProxy(t *testing.T) {
	sel, err := Resolve(Config{URL: "proxy.example:8080", NoProxy: []string{".hivecommons.dev", "10.0.0.0/8", "192.168.1.5"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Restricted() {
		t.Fatal("empty hosts must not be restricted")
	}
	if sel.URL().String() != "http://proxy.example:8080" {
		t.Fatalf("scheme default: %s", sel.URL())
	}
	for host, want := range map[string]bool{
		"github.ibm.com":        true,
		"api.anthropic.com":     true,
		"hive.hivecommons.dev":  false,
		"hivecommons.dev":       false,
		"10.20.30.40:443":       false,
		"192.168.1.5":           false,
		"192.168.1.6":           true,
		"[::ffff:10.1.2.3]:443": false,
	} {
		if got := sel.ProxyFor(host) != nil; got != want {
			t.Errorf("ProxyFor(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestResolveEnvOverridesConfigAndFallsBackToStandardEnv(t *testing.T) {
	// HIVE_* env overrides the file.
	sel, err := Resolve(Config{URL: "http://file.example:1", Hosts: []string{"a.example"}}, mapEnv(map[string]string{
		URLEnv:   "http://env.example:2",
		HostsEnv: "b.example, c.example",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if sel.URL().Host != "env.example:2" || sel.ProxyFor("a.example") != nil || sel.ProxyFor("b.example") == nil {
		t.Fatalf("env override not applied: url=%s", sel.URL())
	}

	// No hive config at all: standard HTTPS_PROXY/NO_PROXY apply to every host.
	sel, err = Resolve(Config{}, mapEnv(map[string]string{
		"HTTPS_PROXY": "http://std.example:3128",
		"NO_PROXY":    "github.com,.githubusercontent.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Source() != "env" || sel.Restricted() {
		t.Fatalf("source=%q restricted=%v", sel.Source(), sel.Restricted())
	}
	if sel.ProxyFor("github.ibm.com") == nil || sel.ProxyFor("github.com") != nil || sel.ProxyFor("raw.githubusercontent.com") != nil {
		t.Fatal("standard NO_PROXY semantics not honoured")
	}

	// Hive config URL wins over the standard env.
	sel, err = Resolve(Config{URL: "http://cfg.example:1", Hosts: []string{"github.ibm.com"}}, mapEnv(map[string]string{
		"HTTPS_PROXY": "http://std.example:3128",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if sel.URL().Host != "cfg.example:1" || sel.ProxyFor("github.com") != nil {
		t.Fatalf("config did not win: %s", sel.URL())
	}

	// Nothing configured → nil selector, which is direct for everything.
	sel, err = Resolve(Config{}, mapEnv(nil))
	if err != nil || sel != nil {
		t.Fatalf("expected nil selector, got %v err=%v", sel, err)
	}
	if sel.ProxyFor("github.ibm.com") != nil || sel.GitEnv() != nil {
		t.Fatal("nil selector must be direct")
	}
	if u, err := sel.ProxyFunc()(&http.Request{URL: &url.URL{Host: "github.ibm.com"}}); u != nil || err != nil {
		t.Fatal("nil selector ProxyFunc must report direct")
	}
}

func TestResolveRejectsBadInput(t *testing.T) {
	if _, err := Resolve(Config{URL: "socks5://p:1080"}, nil); err == nil {
		t.Fatal("socks scheme accepted")
	}
	if _, err := Resolve(Config{URL: "http://p:3128", Hosts: []string{"bad host"}}, nil); err == nil {
		t.Fatal("bad pattern accepted")
	}
	if _, err := Resolve(Config{URL: "http://p:3128", NoProxy: []string{"10.0.0.0/99"}}, nil); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

func TestResolveErrorsRedactUserinfo(t *testing.T) {
	// The caller logs the error; the URL's userinfo is the proxy's Basic
	// credential and must not reach the log through any rejection branch.
	for _, raw := range []string{
		"socks5://alice:s3cret-pw@proxy.corp:1080", // unsupported scheme
		"http://alice:s3cret-pw@",                  // missing host
		"http://alice:s3cret-pw@proxy.corp:%zz",    // url.Parse failure
	} {
		_, err := Resolve(Config{URL: raw}, nil)
		if err == nil {
			t.Fatalf("%q: accepted", raw)
		}
		if strings.Contains(err.Error(), "s3cret-pw") {
			t.Fatalf("%q: error leaks password: %v", raw, err)
		}
		if !strings.Contains(err.Error(), "alice") && !strings.Contains(err.Error(), "xxxxx") {
			t.Fatalf("%q: error lost the redacted URL entirely: %v", raw, err)
		}
	}
}

func TestGitEnv(t *testing.T) {
	sel, _ := Resolve(Config{URL: "http://p:3128", Hosts: []string{"github.ibm.com", "*.github.ibm.com"}, NoProxy: []string{"github.com"}}, nil)
	got := strings.Join(sel.GitEnv(), "\n")
	for _, want := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.ibm.com/.proxy",
		"GIT_CONFIG_VALUE_0=http://p:3128",
		"no_proxy=github.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GitEnv missing %q in:\n%s", want, got)
		}
	}
	all, _ := Resolve(Config{URL: "http://p:3128"}, nil)
	if got := strings.Join(all.GitEnv(), "\n"); !strings.Contains(got, "GIT_CONFIG_KEY_0=http.proxy\n") {
		t.Errorf("unrestricted GitEnv = %s", got)
	}
}

// connectProxy is a minimal CONNECT-capable forward proxy for tests. It
// records the Proxy-Authorization it saw and the targets it was asked for.
type connectProxy struct {
	ln       net.Listener
	auth     atomic.Value // string
	targets  atomic.Int32
	wantAuth string
}

func startConnectProxy(t *testing.T, wantAuth string) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln, wantAuth: wantAuth}
	p.auth.Store("")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *connectProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	p.auth.Store(req.Header.Get("Proxy-Authorization"))
	if p.wantAuth != "" && req.Header.Get("Proxy-Authorization") != p.wantAuth {
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		return
	}
	p.targets.Add(1)
	up, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func TestDialTunnelsTLSThroughCONNECT(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello via "+r.Host)
	}))
	defer origin.Close()
	originHost := strings.TrimPrefix(origin.URL, "https://")

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	px := startConnectProxy(t, wantAuth)
	proxyURL, _ := url.Parse("http://alice:s3cret@" + px.ln.Addr().String())

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := Dial(ctx, dialer.DialContext, proxyURL, originHost)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = raw.Close() }()

	pool := origin.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	tlsConn := tls.Client(raw, &tls.Config{ServerName: "127.0.0.1", RootCAs: pool})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("TLS over tunnel: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+originHost+"/x", nil)
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "hello via") {
		t.Fatalf("origin reply: %d %q", resp.StatusCode, body)
	}
	if px.auth.Load().(string) != wantAuth {
		t.Fatalf("proxy saw auth %q", px.auth.Load())
	}
	if px.targets.Load() != 1 {
		t.Fatalf("proxy CONNECT count = %d", px.targets.Load())
	}
}

func TestDialSurfacesProxyRefusal(t *testing.T) {
	px := startConnectProxy(t, "Basic needed")
	proxyURL, _ := url.Parse("http://" + px.ln.Addr().String())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	_, err := Dial(context.Background(), dialer.DialContext, proxyURL, "example.invalid:443")
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected 407 refusal, got %v", err)
	}
}

func TestParseProxyURLDefaults(t *testing.T) {
	cases := map[string]string{
		"proxy.corp.example:3128":         "http://proxy.corp.example:3128",
		"http://proxy.corp.example":       "http://proxy.corp.example:80",
		"https://proxy.corp.example":      "https://proxy.corp.example:443",
		"http://u:p@proxy.corp.example:1": "http://u:p@proxy.corp.example:1",
	}
	for in, want := range cases {
		sel, err := New(in, nil, nil, "config")
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if got := sel.URL().String(); got != want {
			t.Errorf("New(%q).URL() = %s, want %s", in, got, want)
		}
		if sel.Source() != "config" {
			t.Errorf("Source() = %q", sel.Source())
		}
	}
	for _, bad := range []string{"", "   ", "socks5://p:1080", "http://", "http://:3128"} {
		if _, err := New(bad, nil, nil, "config"); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
}

func TestNilSelectorAccessors(t *testing.T) {
	var s *Selector
	if s.URL() != nil || s.Source() != "" || s.Restricted() || s.GitEnv() != nil || s.ProxyFor("ghe.corp.example:443") != nil {
		t.Fatal("nil selector must report no proxy")
	}
	if u, err := s.ProxyFunc()(nil); u != nil || err != nil {
		t.Fatalf("ProxyFunc(nil req) = %v, %v", u, err)
	}
	if u, err := s.ProxyFunc()(&http.Request{}); u != nil || err != nil {
		t.Fatalf("ProxyFunc(req without URL) = %v, %v", u, err)
	}
}

func TestProxyFuncUsesRequestHost(t *testing.T) {
	sel, err := New("http://p:3128", []string{"ghe.corp.example"}, nil, "config")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://ghe.corp.example/api/v3", nil)
	if u, _ := sel.ProxyFunc()(req); u == nil || u.Host != "p:3128" {
		t.Fatalf("ProxyFunc(ghe) = %v", u)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://api.github.com/", nil)
	if u, _ := sel.ProxyFunc()(req); u != nil {
		t.Fatalf("ProxyFunc(api.github.com) = %v, want direct", u)
	}
}

func TestSetDefaultAndDefault(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	if Default() != nil {
		t.Fatal("Default() must start nil")
	}
	sel, _ := New("http://p:3128", nil, nil, "env")
	SetDefault(sel)
	if Default() != sel {
		t.Fatal("Default() did not return the installed selector")
	}
	SetDefault(nil)
	if Default() != nil {
		t.Fatal("SetDefault(nil) must clear")
	}
}

func TestDialErrorPaths(t *testing.T) {
	ctx := context.Background()
	dial := (&net.Dialer{Timeout: time.Second}).DialContext
	if _, err := Dial(ctx, dial, nil, "ghe.corp.example:443"); err == nil {
		t.Fatal("nil proxy URL accepted")
	}
	u, _ := url.Parse("http://127.0.0.1:1")
	if _, err := Dial(ctx, dial, u, "ghe.corp.example"); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("target without port: %v", err)
	}
	refusing := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("boom") }
	if _, err := Dial(ctx, refusing, u, "ghe.corp.example:443"); err == nil || !strings.Contains(err.Error(), "dial: boom") {
		t.Fatalf("proxy dial failure: %v", err)
	}

	// A proxy that answers garbage instead of an HTTP status line.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "not http\r\n\r\n")
		_ = c.Close()
	}()
	gu, _ := url.Parse("http://" + ln.Addr().String())
	if _, err := Dial(ctx, dial, gu, "ghe.corp.example:443"); err == nil || !strings.Contains(err.Error(), "read CONNECT response") {
		t.Fatalf("garbage response: %v", err)
	}
}

// Bytes the proxy sends right after its 200 must not be lost.
func TestDialPreservesBytesAfterConnectResponse(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\nearly-bytes")
		_ = c.Close()
	}()
	u, _ := url.Parse("http://" + ln.Addr().String())
	conn, err := Dial(context.Background(), (&net.Dialer{Timeout: time.Second}).DialContext, u, "ghe.corp.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(*bufferedConn); !ok {
		t.Fatalf("expected bufferedConn, got %T", conn)
	}
	b, _ := io.ReadAll(conn)
	if string(b) != "early-bytes" {
		t.Fatalf("got %q", b)
	}
}

// An https:// proxy URL wraps the proxy leg in TLS before CONNECT: against
// a proxy presenting an untrusted certificate the failure is a TLS error on
// the proxy leg, not a plaintext CONNECT reaching the handler.
func TestDialHTTPSProxyLegIsTLS(t *testing.T) {
	var handled atomic.Int32
	tlsProxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
	}))
	defer tlsProxy.Close()

	u, _ := url.Parse(tlsProxy.URL)
	dial := (&net.Dialer{Timeout: time.Second}).DialContext
	_, err := Dial(context.Background(), dial, u, "ghe.corp.example:443")
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("https proxy with untrusted CA: err = %v, want tls error", err)
	}
	if handled.Load() != 0 {
		t.Fatal("CONNECT must not reach the proxy handler when the TLS leg fails")
	}
}

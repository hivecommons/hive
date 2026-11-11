package upstreamproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
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

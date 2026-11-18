package inbox

import (
	"bufio"
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
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakePublicIP is what the fake resolver answers for public test hosts; the
// fake dialer routes it to the local test server, so the real SSRF rules run
// on a public-looking address.
const fakePublicIP = "93.184.216.34"

type linkTestEnv struct {
	srv     *httptest.Server
	fetcher *LinkFetcher
	mu      sync.Mutex
	dialed  []string
	proxy   *url.URL
}

func newLinkTestEnv(t *testing.T, mux *http.ServeMux) *linkTestEnv {
	t.Helper()
	e := &linkTestEnv{srv: httptest.NewServer(mux)}
	t.Cleanup(e.srv.Close)
	answers := map[string][]string{
		"public.example": {fakePublicIP},
		"rebind.example": {"10.0.0.5"},
		"v6ula.example":  {"fd00::1"},
		"mixed.example":  {fakePublicIP, "127.0.0.1"},
		"meta.example":   {"169.254.169.254"},
		"cgnat.example":  {"100.64.1.1"},
		"0x7f000001":     {"127.0.0.1"},
	}
	e.fetcher = &LinkFetcher{
		UserAgent: "hive-inbox/test",
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			list, ok := answers[host]
			if !ok {
				return nil, fmt.Errorf("no such host %s", host)
			}
			out := make([]netip.Addr, len(list))
			for i, a := range list {
				out[i] = netip.MustParseAddr(a)
			}
			return out, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			e.mu.Lock()
			e.dialed = append(e.dialed, addr)
			e.mu.Unlock()
			host, _, _ := net.SplitHostPort(addr)
			if e.proxy != nil && addr == e.proxy.Host {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			if host != fakePublicIP {
				return nil, fmt.Errorf("test dialer: refusing %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, e.srv.Listener.Addr().String())
		},
		Proxy: func(string) *url.URL { return e.proxy },
	}
	return e
}

func (e *linkTestEnv) url(path string) string {
	_, port, _ := net.SplitHostPort(e.srv.Listener.Addr().String())
	return "http://public.example:" + port + path
}

func linkMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Launch notes</title><script>alert(1)</script><style>p{}</style></head>
<body><nav><a href="/">Home</a></nav><article><h2>Plan</h2><p>Ship the <strong>drop zone</strong> first.</p>
<ul><li>docs</li><li>tests</li></ul><script>document.write("evil")</script></article><footer>c</footer></body></html>`)
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ua":"`+r.Header.Get("User-Agent")+`"}`)
	})
	mux.HandleFunc("/files/data.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "a,b\n1,2\n")
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("MZ\x90\x00binary"))
	})
	mux.HandleFunc("/audio", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3\x04"))
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, strings.Repeat("x", 2048))
	})
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://rebind.example/secret", http.StatusFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-creds", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://user:pw@public.example/json", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/hop/"), "%d", &n)
		if n <= 0 {
			http.Redirect(w, r, "/json", http.StatusFound)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	return mux
}

func TestLinkAddrAllowedTable(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "100.100.100.200",
		"0.0.0.0", "224.0.0.1", "239.255.255.250", "255.255.255.255", "192.0.0.192", "198.18.0.1", "::1", "::", "fd00:ec2::254",
		"fc00::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1", "2001::1"}
	for _, a := range blocked {
		if LinkAddrAllowed(netip.MustParseAddr(a)) {
			t.Errorf("%s must be blocked", a)
		}
	}
	for _, a := range []string{fakePublicIP, "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808", "2002:808:808::"} {
		if !LinkAddrAllowed(netip.MustParseAddr(a)) {
			t.Errorf("%s must be allowed", a)
		}
	}
	if LinkAddrAllowed(netip.Addr{}) {
		t.Error("the zero address must be blocked")
	}
}

func TestValidateLinkURLRefusals(t *testing.T) {
	for _, raw := range []string{"", "ftp://public.example/x", "file:///etc/passwd", "javascript:alert(1)", "gopher://x",
		"http://user:pw@public.example/", "https://token@public.example/", "http://127.0.0.1/", "http://[::1]:8080/",
		"http://169.254.169.254/latest/meta-data/", "http://[fd00:ec2::254]/", "http://10.1.2.3/", "http://100.64.0.1/",
		"http://224.0.0.1/", "http://localhost:8080/", "http://app.localhost/", "http://metadata.google.internal/",
		"http://printer.local/", "http://[::ffff:127.0.0.1]/", "http://1.2.3.4.5/", "http://2130706433/", "http://0x7f.1/", "http://203.0.113.10/", "http://[2001:db8::1]/", "http:///nohost", "http://" + strings.Repeat("a", linkMaxURLBytes) + ".example/", "::not a url"} {
		if _, err := ValidateLinkURL(raw); !errors.Is(err, ErrLinkRefused) {
			t.Errorf("ValidateLinkURL(%q) = %v, want refused", raw, err)
		}
	}
	if u, err := ValidateLinkURL("  https://public.example/a?b=c "); err != nil || u.Host != "public.example" {
		t.Fatalf("public URL = %v, %v", u, err)
	}
}

func TestLinkFetchSSRF(t *testing.T) {
	e := newLinkTestEnv(t, linkMux(t))
	ctx := context.Background()
	_, port, _ := net.SplitHostPort(e.srv.Listener.Addr().String())
	refused := []string{
		"http://rebind.example:" + port + "/", // resolves private
		"http://v6ula.example:" + port + "/",  // resolves to IPv6 ULA
		"http://mixed.example:" + port + "/",  // one public, one loopback answer
		"http://meta.example:" + port + "/",   // resolves to metadata
		"http://cgnat.example:" + port + "/",  // CGNAT
		"http://0x7f000001:" + port + "/",     // a numeric name that resolves to loopback
		e.url("/to-private"),                  // DNS-rebinding style redirect to a private IP
		e.url("/to-metadata"),                 // redirect to the metadata literal
		e.url("/to-creds"),                    // redirect adds credentials
		e.url("/to-file"),                     // redirect to a non-http scheme
		e.url("/hop/4"),                       // four redirects
	}
	for _, raw := range refused {
		if _, err := e.fetcher.Fetch(ctx, raw, 1<<20); !errors.Is(err, ErrLinkRefused) {
			t.Errorf("Fetch(%s) = %v, want refused", raw, err)
		}
	}
	for _, d := range e.dialed {
		if host, _, _ := net.SplitHostPort(d); host != fakePublicIP {
			t.Fatalf("dialed a refused address: %s", d)
		}
	}
	// Three redirects are fine; the user agent is sent.
	fl, err := e.fetcher.Fetch(ctx, e.url("/hop/2"), 1<<20)
	if err != nil || fl.ContentType != "application/json" || !strings.Contains(string(fl.Body), "hive-inbox/test") || fl.URL.Path != "/json" {
		t.Fatalf("three hops = %+v, %v", fl, err)
	}
	// Oversize body, streamed without a length.
	if _, err := e.fetcher.Fetch(ctx, e.url("/big"), 1024); err == nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Fatalf("oversize = %v", err)
	}
	if _, err := e.fetcher.Fetch(ctx, e.url("/missing"), 1024); err == nil || errors.Is(err, ErrLinkRefused) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 = %v", err)
	}
	if _, err := e.fetcher.Fetch(ctx, "http://unknown.example/", 1024); err == nil || errors.Is(err, ErrLinkRefused) {
		t.Fatalf("unknown host = %v", err)
	}
}

// TestLinkFetchThroughUpstreamProxy pins that a configured upstream proxy is
// asked to CONNECT to the checked IP, never to the name (which the proxy
// would resolve again).
func TestLinkFetchThroughUpstreamProxy(t *testing.T) {
	e := newLinkTestEnv(t, linkMux(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	targets := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				targets <- req.Host
				up, err := net.Dial("tcp", e.srv.Listener.Addr().String())
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()
	e.proxy = &url.URL{Scheme: "http", Host: ln.Addr().String()}
	fl, err := e.fetcher.Fetch(context.Background(), e.url("/json"), 1<<20)
	if err != nil || fl.ContentType != "application/json" {
		t.Fatalf("via proxy = %+v, %v", fl, err)
	}
	if got := <-targets; !strings.HasPrefix(got, fakePublicIP+":") {
		t.Fatalf("CONNECT target = %q, want the checked IP", got)
	}
}

func TestAddLinkRunsTheTypePipeline(t *testing.T) {
	e := newLinkTestEnv(t, linkMux(t))
	env := newUploadEnv(t, config.InboxConfig{Uploads: config.InboxUploads{MaxFileBytes: 1024}})
	env.src.Fetcher = e.fetcher
	ctx := context.Background()

	it, err := env.svc.AddLink(ctx, LinkInput{URL: e.url("/page"), AddedBy: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Launch notes" || it.UploadKind != UploadKindHTML || !strings.HasPrefix(it.Origin, "link · public.example") || it.AddedBy != "ana" {
		t.Fatalf("page item = %+v", it)
	}
	c, err := env.svc.Content(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Plan", "Ship the **drop zone** first.", "- docs\n- tests"} {
		if !strings.Contains(c.Markdown, want) {
			t.Errorf("page markdown missing %q:\n%s", want, c.Markdown)
		}
	}
	for _, gone := range []string{"alert(1)", "evil", "Home", "p{}"} {
		if strings.Contains(c.Markdown, gone) {
			t.Errorf("page markdown kept %q:\n%s", gone, c.Markdown)
		}
	}
	if c.Provenance != e.url("/page") {
		t.Fatalf("provenance = %q", c.Provenance)
	}

	// A linked CSV served as text/plain is read by its file name.
	it, err = env.svc.AddLink(ctx, LinkInput{URL: e.url("/files/data.csv")})
	if err != nil || it.UploadKind != UploadKindCSV || it.Title != "data.csv" {
		t.Fatalf("csv link = %+v, %v", it, err)
	}
	code := func(err error) string {
		var ue *UploadError
		if errors.As(err, &ue) {
			return ue.Code
		}
		return "?"
	}
	for path, want := range map[string]string{"/bin": UploadErrUnsupported, "/audio": UploadErrUnsupported, "/big": UploadErrTooLarge, "/missing": UploadErrFetch, "/to-private": UploadErrLinkRefused} {
		if _, err := env.svc.AddLink(ctx, LinkInput{URL: e.url(path)}); code(err) != want {
			t.Errorf("%s = %v, want %s", path, err, want)
		}
	}
	if _, err := env.svc.AddLink(ctx, LinkInput{URL: "http://user:pw@public.example/"}); code(err) != UploadErrLinkRefused {
		t.Fatalf("credentials = %v", err)
	}
	env.src.Fetcher = nil
	if _, err := env.svc.AddLink(ctx, LinkInput{URL: e.url("/page")}); code(err) != UploadErrLinkRefused {
		t.Fatalf("no fetcher = %v", err)
	}
	if env.svc.Settings().Uploads.Links {
		t.Fatal("links must be off without a fetcher")
	}
}

func TestLinkPipelineName(t *testing.T) {
	cases := []struct {
		fl   FetchedLink
		want string
	}{
		{FetchedLink{Filename: "notes.md", ContentType: "text/plain"}, "notes.md"},
		{FetchedLink{Filename: "", ContentType: "text/html"}, "link.html"},
		{FetchedLink{Filename: "report.php", ContentType: "application/json"}, "report.json"},
		{FetchedLink{Filename: "x.bin", ContentType: "application/octet-stream"}, "x.bin"},
		{FetchedLink{}, "link"},
	}
	for _, tc := range cases {
		if got := linkPipelineName(tc.fl); got != tc.want {
			t.Errorf("linkPipelineName(%+v) = %q, want %q", tc.fl, got, tc.want)
		}
	}
}

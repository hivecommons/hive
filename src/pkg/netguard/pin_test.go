package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// rebindResolver answers each lookup of a name with the next entry of its
// script, repeating the last one: a DNS rebinding server in miniature.
type rebindResolver struct {
	mu      sync.Mutex
	script  map[string][][]string
	lookups map[string]int
}

func newRebindResolver(script map[string][][]string) *rebindResolver {
	return &rebindResolver{script: script, lookups: map[string]int{}}
}

func (r *rebindResolver) resolve(_ context.Context, host string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	answers, ok := r.script[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := r.lookups[host]
	r.lookups[host]++
	if i >= len(answers) {
		i = len(answers) - 1
	}
	return answers[i], nil
}

func (r *rebindResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups[host]
}

// recordingDial never touches the network: it records the address it was
// asked to dial and fails, so a test can see what would have been connected.
type recordingDial struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDial) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return nil, errors.New("recording dialer: not connecting")
}

func TestPinnedDialer_DialsTheCheckedAddressNotTheName(t *testing.T) {
	res := newRebindResolver(map[string][][]string{
		"docs.example": {{"93.184.216.34"}, {"127.0.0.1"}},
	})
	rec := &recordingDial{}
	p := PinnedDialer{Resolve: res.resolve, Dial: rec.dial}
	_, _ = p.DialContext(context.Background(), "tcp", "docs.example:443")
	if got := res.count("docs.example"); got != 1 {
		t.Fatalf("resolved %d times, want exactly once", got)
	}
	if len(rec.addrs) != 1 || rec.addrs[0] != "93.184.216.34:443" {
		t.Fatalf("dialled %v, want the validated 93.184.216.34:443", rec.addrs)
	}
}

func TestPinnedDialer_RefusesWhenAnyAnswerIsBarred(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"metadata second", []string{"93.184.216.34", "169.254.169.254"}},
		{"loopback first", []string{"127.0.0.1", "93.184.216.34"}},
		{"v6 unique local", []string{"2606:2800:220:1::1", "fd00::1"}},
		{"cgnat", []string{"100.64.0.1"}},
		{"nat64 private", []string{"64:ff9b::a00:1"}},
		{"unparseable", []string{"93.184.216.34", "not-an-ip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := newRebindResolver(map[string][][]string{"mixed.example": {tc.answers}})
			rec := &recordingDial{}
			p := PinnedDialer{Resolve: res.resolve, Dial: rec.dial}
			_, err := p.DialContext(context.Background(), "tcp", "mixed.example:80")
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
			if len(rec.addrs) != 0 {
				t.Fatalf("dialled %v after a barred answer", rec.addrs)
			}
		})
	}
}

func TestPinnedDialer_FailsClosed(t *testing.T) {
	rec := &recordingDial{}
	empty := func(context.Context, string) ([]string, error) { return nil, nil }
	broken := func(context.Context, string) ([]string, error) { return nil, errors.New("SERVFAIL") }
	for name, p := range map[string]PinnedDialer{
		"lookup error": {Resolve: broken, Dial: rec.dial},
		"no answers":   {Resolve: empty, Dial: rec.dial},
	} {
		if _, err := p.DialContext(context.Background(), "tcp", "x.example:443"); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
	p := PinnedDialer{Dial: rec.dial}
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "0x7f.1:80", "2130706433:80", "[fe80::1%25eth0]:80", "no-port"} {
		if _, err := p.DialContext(context.Background(), "tcp", addr); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", addr, err)
		}
	}
	if len(rec.addrs) != 0 {
		t.Fatalf("dialled %v on a refused target", rec.addrs)
	}
}

func TestPinnedDialer_TriesEachCheckedAnswer(t *testing.T) {
	res := newRebindResolver(map[string][][]string{"multi.example": {{"93.184.216.34", "2606:2800:220:1::1"}}})
	rec := &recordingDial{}
	p := PinnedDialer{Resolve: res.resolve, Dial: rec.dial}
	_, _ = p.DialContext(context.Background(), "tcp", "multi.example:443")
	want := []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"}
	if strings.Join(rec.addrs, ",") != strings.Join(want, ",") {
		t.Fatalf("dialled %v, want %v", rec.addrs, want)
	}
}

func TestPinnedDialer_RefuseHookSeesResolvedTarget(t *testing.T) {
	res := newRebindResolver(map[string][][]string{"spoke.example": {{"93.184.216.34"}}})
	var seen []string
	p := PinnedDialer{
		Resolve: res.resolve,
		Dial:    (&recordingDial{}).dial,
		Refuse: func(_ context.Context, target string) bool {
			seen = append(seen, target)
			return true
		},
	}
	if _, err := p.DialContext(context.Background(), "tcp", "spoke.example:8443"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused from the hook", err)
	}
	if len(seen) != 1 || seen[0] != "93.184.216.34:8443" {
		t.Fatalf("hook saw %v, want the resolved ip:port", seen)
	}
}

// The pre-fetch check and the client's own dial each resolve the name. With
// a resolver that answers public, then loopback, the check passes and an
// unpinned client would connect to loopback; the pinned transport refuses.
func TestTransport_RebindingBetweenCheckAndDialIsRefused(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	res := newRebindResolver(map[string][][]string{
		"rebind.example": {{"93.184.216.34"}, {"127.0.0.1"}},
	})
	target := "http://rebind.example:" + port + "/"
	if IsPrivateURL(context.Background(), target, res.resolve) {
		t.Fatal("pre-fetch check saw the first (public) answer as private")
	}
	client := &http.Client{Transport: PinnedDialer{Resolve: res.resolve}.Transport()}
	resp, err := client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRefused) || hit.Load() {
		t.Fatalf("rebound fetch: err = %v, server hit = %v; want ErrRefused and no hit", err, hit.Load())
	}
}

// The dial hook runs for every connection, so a redirect hop to a name that
// resolves privately is refused at dial time even with no CheckRedirect.
func TestTransport_EveryRedirectHopIsDialChecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.example/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	allowed := strings.TrimPrefix(srv.URL, "http://")
	res := newRebindResolver(map[string][][]string{"internal.example": {{"169.254.169.254"}}})
	rec := &recordingDial{}
	p := PinnedDialer{
		Resolve: res.resolve,
		Refuse: func(_ context.Context, target string) bool {
			if target == allowed {
				return false
			}
			host, _, _ := net.SplitHostPort(target)
			a, _ := ParseHostLiteral(host)
			return Denied(a)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == allowed {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return rec.dial(ctx, network, addr)
		},
	}
	resp, err := (&http.Client{Transport: p.Transport()}).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("redirect hop: err = %v, want ErrRefused", err)
	}
	if len(rec.addrs) != 0 {
		t.Fatalf("dialled %v for the redirect hop", rec.addrs)
	}
}

func TestProxyHostPort(t *testing.T) {
	for raw, want := range map[string]string{
		"http://proxy.corp:3128": "proxy.corp:3128",
		"http://Proxy.Corp":      "proxy.corp:80",
		"https://proxy.corp":     "proxy.corp:443",
		"socks5://10.0.0.9":      "10.0.0.9:1080",
		"http://[fd00::9]:8080":  "[fd00::9]:8080",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyHostPort(u); got != want {
			t.Errorf("proxyHostPort(%s) = %q, want %q", raw, got, want)
		}
	}
}

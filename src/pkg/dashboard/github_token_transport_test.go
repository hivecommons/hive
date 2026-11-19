package dashboard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/netguard"
)

// Resolve-once and dial pinning for validateGitHubToken (#19221, part of
// #18986). These tests drive a resolver that answers differently on later
// lookups and a recording dialer, so they never leave the machine.

const (
	tokenPinPublicIP = "140.82.112.6"
	tokenPinHost     = "rebind.example"
)

// stubTokenResolver answers first on the first lookup and later afterwards.
func stubTokenResolver(t *testing.T, first, later []string, err error) *int32 {
	t.Helper()
	var calls int32
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		if err != nil {
			atomic.AddInt32(&calls, 1)
			return nil, err
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return first, nil
		}
		return later, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
	return &calls
}

// recordTokenDials replaces githubTokenDial with one that records each
// target. When to is set, the dial connects there instead (an httptest
// server standing in for the checked public address).
func recordTokenDials(t *testing.T, to string) *[]string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	orig := githubTokenDial
	githubTokenDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		got = append(got, addr)
		mu.Unlock()
		if to == "" {
			return nil, errors.New("test dialer: no connection")
		}
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}
	t.Cleanup(func() { githubTokenDial = orig })
	return &got
}

// The invariant: a name that rebinds between the check and the dial cannot
// reach a private address. The pre-fetch check sees the public answer; the
// dial's own single lookup sees loopback and refuses, and the loopback server
// receives nothing.
func TestGitHubTokenTransport_RebindBetweenCheckAndDialRefused(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	stubTokenResolver(t, []string{tokenPinPublicIP}, []string{"127.0.0.1"}, nil)
	// Real dialer, so a dial to the rebound address would reach srv.
	orig := githubTokenDial
	githubTokenDial = (&net.Dialer{}).DialContext
	t.Cleanup(func() { githubTokenDial = orig })

	target := "http://" + tokenPinHost + ":" + u.Port() + "/user"
	if isPrivateURL(context.Background(), target) {
		t.Fatal("pre-fetch check should see the public first answer")
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: githubTokenTransport}).Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("dial after rebind succeeded")
	}
	if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("dial after rebind = %v; want netguard.ErrRefused", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("rebound loopback server received %d request(s)", n)
	}
}

// Default api.github.com: the dial connects to the address it checked, from
// one lookup, never to the name.
func TestValidateGitHubToken_DialsCheckedAddressOnce(t *testing.T) {
	calls := stubTokenResolver(t, []string{tokenPinPublicIP}, []string{"127.0.0.1"}, nil)
	dials := recordTokenDials(t, "")

	_ = validateGitHubToken("ghp_pin_checked_once", "")
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("resolver called %d times, want 1", got)
	}
	if len(*dials) != 1 || (*dials)[0] != net.JoinHostPort(tokenPinPublicIP, "443") {
		t.Fatalf("dials = %v, want only the checked %s:443", *dials, tokenPinPublicIP)
	}
}

// Default api.github.com: a private answer (any of several), an empty answer
// or a lookup error refuses the dial before any connection is attempted.
func TestValidateGitHubToken_DefaultHostRefusesPrivateAndFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		answers []string
		err     error
	}{
		{"loopback", []string{"127.0.0.1"}, nil},
		{"any answer private", []string{tokenPinPublicIP, "10.1.2.3"}, nil},
		{"metadata", []string{"169.254.169.254"}, nil},
		{"empty", nil, nil},
		{"lookup error", nil, errors.New("dns unavailable")},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubTokenResolver(t, tc.answers, tc.answers, tc.err)
			dials := recordTokenDials(t, "")
			if got := validateGitHubToken("ghp_pin_refused_"+string(rune('a'+i)), ""); got != "" {
				t.Fatalf("username = %q, want refusal", got)
			}
			if len(*dials) != 0 {
				t.Fatalf("dialled %v despite a barred or failed lookup", *dials)
			}
		})
	}
}

// A configured API URL is intentionally chosen and may be private: an
// httptest server on loopback answers. Link-local metadata stays refused.
func TestValidateGitHubToken_ConfiguredTargetMayBePrivateButNotMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octo-pin"}`))
	}))
	t.Cleanup(srv.Close)
	if got := validateGitHubToken("ghp_pin_configured_ok", srv.URL); got != "octo-pin" {
		t.Fatalf("configured loopback API: username = %q, want octo-pin", got)
	}

	stubTokenResolver(t, []string{"169.254.169.254"}, []string{"169.254.169.254"}, nil)
	dials := recordTokenDials(t, "")
	if got := validateGitHubToken("ghp_pin_configured_md", "http://ghe.internal.example"); got != "" {
		t.Fatalf("configured host resolving to metadata: username = %q, want refusal", got)
	}
	if len(*dials) != 0 {
		t.Fatalf("dialled %v for a metadata answer", *dials)
	}
}

// The configured-target allowance must not follow a cross-host redirect.
func TestValidateGitHubToken_CrossHostRedirectRefused(t *testing.T) {
	var otherHits int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&otherHits, 1)
		_, _ = w.Write([]byte(`{"login":"elsewhere"}`))
	}))
	t.Cleanup(other.Close)
	ou, err := url.Parse(other.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+ou.Port()+"/user", http.StatusFound)
	}))
	t.Cleanup(front.Close)

	if got := validateGitHubToken("ghp_pin_redirect", front.URL); got != "" {
		t.Fatalf("username = %q after a cross-host redirect, want refusal", got)
	}
	if n := atomic.LoadInt32(&otherHits); n != 0 {
		t.Fatalf("redirect target received %d request(s)", n)
	}
}

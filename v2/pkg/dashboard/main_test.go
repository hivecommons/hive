package dashboard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain isolates the dashboard test suite from the real internet.
//
// Why this exists: the device-flow login and contributor-API handlers resolve
// their endpoints through GitHubConfig.OAuthBaseURL/OAuthAPIURL, which are
// hardcoded to public github.com so that GHE hives still authenticate against a
// github.com identity (see the comment on OAuthBaseURL). Because those methods
// ignored their receiver, tests in auth_deviceflow_coverage_test.go and
// api_contribute_v1_coverage_test.go that carefully pointed
// Config.GitHub.BaseURL/APIURL at an httptest server had those URLs silently
// discarded, and every run POSTed to the REAL
// https://github.com/login/device/code.
//
// That made ~15 tests fail permanently (GitHub answers 404 for a test client
// ID), which aborted the `go test -coverprofile` step in coverage-hourly.yml
// before the per-package floor check ever ran — so the coverage gate had never
// executed. It also burned real github.com rate limit, shared with production
// hives, on every scheduled run.
//
// The fix has two halves. GitHubConfig grew OAuth*URLOverride fields (yaml:"-",
// settable only from Go) that tests point at an httptest server. This TestMain
// is the backstop: it replaces the default transport's DialContext so any
// attempt to open a connection to a host that is not loopback fails loudly with
// a descriptive error, instead of quietly succeeding against a real service.
// A new test that forgets the override therefore fails in CI rather than
// leaking traffic — the same "make it impossible, don't rely on discipline"
// posture as TestMain in pkg/hub (#2287).
func TestMain(m *testing.M) {
	// Belt and braces: proxy env vars could otherwise route a dial that looks
	// like loopback to an external proxy.
	for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		os.Unsetenv(v)
	}

	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("dashboard TestMain: http.DefaultTransport is not *http.Transport; " +
			"the outbound-network guard cannot be installed")
	}
	guarded := tr.Clone()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	guarded.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !isLoopbackAddr(addr) {
			return nil, fmt.Errorf(
				"dashboard tests must not reach the network: blocked dial to %q. "+
					"Point Config.GitHub.OAuthBaseURLOverride/OAuthAPIURLOverride at an "+
					"httptest server instead of relying on the real github.com endpoints",
				addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = guarded
	http.DefaultClient.Transport = guarded

	os.Exit(m.Run())
}

// isLoopbackAddr reports whether a dial target is a loopback host, i.e. an
// httptest server. Hostnames other than "localhost" are rejected outright
// rather than resolved: a DNS lookup is itself outbound traffic, and a name
// that resolves to 127.0.0.1 today could resolve elsewhere tomorrow.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

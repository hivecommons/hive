package dashboard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/netguard"
)

const (
	// githubTokenDialTimeout and githubTokenDialKeepAlive match the
	// net.Dialer http.DefaultTransport uses.
	githubTokenDialTimeout   = 30 * time.Second
	githubTokenDialKeepAlive = 30 * time.Second

	// githubTokenMaxRedirects bounds a token check's same-host redirects.
	githubTokenMaxRedirects = 3
)

// githubTokenConfiguredTargetKey marks a token-check request whose API URL is
// a configured override rather than public api.github.com.
type githubTokenConfiguredTargetKey struct{}

// githubTokenConfiguredRefusedReasons are the address classes refused even
// for a configured API URL. A configured target is intentionally chosen and
// may be private or loopback (an httptest server, an internal mirror), but no
// GitHub API lives on link-local (cloud instance metadata), multicast or an
// unspecified address.
var githubTokenConfiguredRefusedReasons = map[string]bool{
	netguard.ReasonLinkLocal:   true,
	netguard.ReasonMulticast:   true,
	netguard.ReasonUnspecified: true,
	netguard.ReasonThisNetwork: true,
	netguard.ReasonBroadcast:   true,
}

// githubTokenDial connects to an address githubTokenTransport already
// checked. A package var so TestMain routes it through the suite's
// loopback-only guard and tests can observe where a dial would go.
var githubTokenDial netguard.DialFunc = (&net.Dialer{
	Timeout:   githubTokenDialTimeout,
	KeepAlive: githubTokenDialKeepAlive,
}).DialContext

// githubTokenTransport is the transport for validateGitHubToken (#19221,
// part of #18986). The token check sends a user's bearer token to the API
// host, and a default http.Client resolved the name at dial time with nothing
// checking the answer. This transport resolves each dial once through
// privateURLResolver, checks every answer, fails closed when the lookup
// errors or returns nothing, and connects only to an address it checked.
//
// For public api.github.com every class isPrivateURL bars is refused. For a
// configured API URL (githubTokenConfiguredTargetKey on the request context)
// the target is intentionally chosen, so only
// githubTokenConfiguredRefusedReasons are refused; githubTokenCheckRedirect
// refuses a cross-host redirect, so that allowance never reaches another host.
var githubTokenTransport http.RoundTripper = netguard.PinnedDialer{
	Resolve: func(ctx context.Context, host string) ([]string, error) { return privateURLResolver(ctx, host) },
	Refuse:  githubTokenRefuse,
	Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return githubTokenDial(ctx, network, addr)
	},
}.Transport()

func githubTokenRefuse(ctx context.Context, target string) bool {
	configured, _ := ctx.Value(githubTokenConfiguredTargetKey{}).(bool)
	if !configured {
		return isPrivateURL(ctx, "http://"+target)
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return true
	}
	addr, ok := netguard.ParseHostLiteral(host)
	if !ok {
		return true
	}
	return githubTokenConfiguredRefusedReasons[netguard.DenyReason(addr)]
}

// githubTokenRoundTripper is githubTokenTransport, unless http.DefaultTransport
// was replaced with a non-*http.Transport RoundTripper (tests and
// instrumented builds inject one there, the same convention as the hub's
// pinnedOrReplacedDefault); then that replacement is used.
func githubTokenRoundTripper() http.RoundTripper {
	if _, ok := http.DefaultTransport.(*http.Transport); ok {
		return githubTokenTransport
	}
	return http.DefaultTransport
}

// githubTokenCheckRedirect allows only a bounded number of same-host
// redirects. A cross-host hop would carry the configured-target allowance
// and the bearer token to a host nobody configured.
func githubTokenCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= githubTokenMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", githubTokenMaxRedirects)
	}
	if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("%w: cross-host redirect to %s", netguard.ErrRefused, req.URL.Host)
	}
	return nil
}

package releasefeed

import (
	"net/http"
	"net/url"
	"time"

	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// Egress client timeouts.
const (
	egressRequestTimeout   = 30 * time.Second
	egressTLSTimeout       = 10 * time.Second
	egressResponseTimeout  = 20 * time.Second
	egressIdleConnTimeout  = 90 * time.Second
	egressMaxIdlePerHost   = 2
	egressMaxResponseBytes = 64 << 10
)

// NewEgressClient returns the HTTP client feed polls use. Every request
// follows the spoke's upstream proxy rules (crux/upstreamproxy.Default, which
// boot resolves from network.upstream_proxy, HIVE_UPSTREAM_PROXY_* or
// HTTPS_PROXY), read per request so a proxy installed after the watcher
// starts still applies. A redirect to another host drops the Authorization
// header (net/http's own rule), so a private-index token never leaves its
// host.
func NewEgressClient() *http.Client {
	tr := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return upstreamproxy.Default().ProxyFunc()(req)
		},
		TLSHandshakeTimeout:    egressTLSTimeout,
		ResponseHeaderTimeout:  egressResponseTimeout,
		IdleConnTimeout:        egressIdleConnTimeout,
		MaxIdleConnsPerHost:    egressMaxIdlePerHost,
		MaxResponseHeaderBytes: egressMaxResponseBytes,
		ForceAttemptHTTP2:      true,
	}
	return &http.Client{Transport: tr, Timeout: egressRequestTimeout}
}

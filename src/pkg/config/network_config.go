package config

import "github.com/hivecommons/hive/crux/upstreamproxy"

// NetworkConfig groups outbound-connectivity settings.
type NetworkConfig struct {
	UpstreamProxy UpstreamProxyConfig `yaml:"upstream_proxy,omitempty" json:"upstream_proxy,omitempty"`
}

// UpstreamProxyConfig names a corporate HTTP forward proxy and the hosts
// whose TLS egress (control-plane GitHub clients, in-process git, and every
// agent :443 flow intercepted by the MITM proxy) must be chained through it
// with CONNECT. Env mapping: HIVE_UPSTREAM_PROXY_URL, HIVE_UPSTREAM_PROXY_HOSTS
// (comma-separated), HIVE_NO_PROXY; when none of these nor the file block is
// set, standard HTTPS_PROXY/HTTP_PROXY/NO_PROXY apply to all hosts.
type UpstreamProxyConfig struct {
	// URL is the proxy, e.g. http://proxy.corp.example:3128 (userinfo is
	// sent as Proxy-Authorization: Basic).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Hosts restricts chaining to matching hosts: exact ("ghe.corp.example"),
	// wildcard ("*.ghe.corp.example", subdomains only), dotted suffix
	// (".corp.example"), IP, CIDR, or "*". Empty means every host not in
	// NoProxy.
	Hosts []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
	// NoProxy lists hosts (same grammar) that are always dialled directly.
	NoProxy []string `yaml:"no_proxy,omitempty" json:"no_proxy,omitempty"`
}

// UpstreamProxy converts the file block into the selector package's input.
func (c UpstreamProxyConfig) UpstreamProxy() upstreamproxy.Config {
	return upstreamproxy.Config{URL: c.URL, Hosts: c.Hosts, NoProxy: c.NoProxy}
}

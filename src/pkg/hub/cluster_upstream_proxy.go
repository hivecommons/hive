package hub

import "strings"

// ClusterUpstreamProxy is the per-cluster upstream forward-proxy setting the
// hosted-hive provisioner injects into pool-slot Deployments. Field semantics
// mirror the hive's network.upstream_proxy config block: URL is the proxy
// (userinfo becomes Proxy-Authorization), Hosts restricts chaining to the
// listed patterns (empty = every host not in NoProxy), NoProxy is always
// direct.
type ClusterUpstreamProxy struct {
	URL     string   `json:"url" yaml:"url"`
	Hosts   []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	NoProxy []string `json:"no_proxy,omitempty" yaml:"no_proxy,omitempty"`
}

func (c *ClusterUpstreamProxy) enabled() bool {
	return c != nil && strings.TrimSpace(c.URL) != ""
}

func (c *ClusterUpstreamProxy) url() string {
	if !c.enabled() {
		return ""
	}
	return strings.TrimSpace(c.URL)
}

// hosts and noProxy are comma-joined because the hive config loader reads
// HIVE_UPSTREAM_PROXY_HOSTS / HIVE_NO_PROXY as comma-separated lists.
func (c *ClusterUpstreamProxy) hosts() string {
	if !c.enabled() {
		return ""
	}
	return joinPatterns(c.Hosts)
}

func (c *ClusterUpstreamProxy) noProxy() string {
	if !c.enabled() {
		return ""
	}
	return joinPatterns(c.NoProxy)
}

func joinPatterns(in []string) string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

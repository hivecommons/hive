package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// SpektacularDiscoverySource declares one document, never a crawl seed.
// Repo/standards/landscape sources use the same exact-document boundary.
type SpektacularDiscoverySource struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
	URL  string `yaml:"url" json:"url"`
}

// ValidateDiscovery fails closed even when cadence is disabled: manual forced
// rechecks must obey the same egress policy. An empty source list is inert.
func (r SpektacularRecheckConfig) ValidateDiscovery() error {
	if len(r.Sources) == 0 {
		return nil
	}
	fail := func(reason string) error { return fmt.Errorf("runs.spektacular.recheck.sources: %s", reason) }
	if len(r.Sources) > 16 {
		return fail("at most 16 sources are permitted")
	}
	proxy, err := url.Parse(r.DiscoveryProxy)
	if err != nil || proxy.Hostname() == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.User != nil || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
		return fail("discovery_proxy must name the HTTP(S) relay proxy without credentials, path, query or fragment")
	}
	allowed := map[string]bool{}
	for _, host := range r.EgressAllowlist {
		if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "/:*@?# ") || net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return fail("egress_allowlist must contain exact DNS hostnames")
		}
		allowed[host] = true
	}
	names := map[string]bool{}
	for _, source := range r.Sources {
		if strings.TrimSpace(source.Name) == "" || len(source.Name) > 128 || names[source.Name] {
			return fail("source names must be unique, nonempty and at most 128 bytes")
		}
		names[source.Name] = true
		switch source.Kind {
		case "release", "changelog", "repo", "standards", "landscape":
		default:
			return fail("unknown source kind")
		}
		u, err := url.Parse(source.URL)
		if err != nil || len(source.URL) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
			return fail("sources must be exact HTTPS URLs without credentials or fragments")
		}
		if !allowed[u.Hostname()] {
			return fail("source " + source.Name + " is outside the egress allow-list")
		}
	}
	return nil
}

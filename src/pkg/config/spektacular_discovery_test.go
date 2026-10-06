package config

import (
	"strings"
	"testing"
)

func TestDiscoveryValidation(t *testing.T) {
	valid := func() SpektacularRecheckConfig {
		return SpektacularRecheckConfig{
			DiscoveryProxy: "http://relay:18443", EgressAllowlist: []string{"api.github.com"},
			Sources: []SpektacularDiscoverySource{{Name: "upstream", Kind: "release", URL: "https://api.github.com/repos/acme/tool/releases/latest"}},
		}
	}
	if err := (SpektacularRecheckConfig{}).ValidateDiscovery(); err != nil {
		t.Fatal(err)
	}
	if err := valid().ValidateDiscovery(); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"https://evil.example/news", "https://api.github.com.evil.example/news", "https://api.github.com@evil.example/news", "http://api.github.com/news", "https://api.github.com:444/news", "https://api.github.com/news#fragment"} {
		t.Run(address, func(t *testing.T) {
			cfg := valid()
			cfg.Sources[0].URL = address
			if cfg.ValidateDiscovery() == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
	cfg := valid()
	cfg.DiscoveryProxy = ""
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("direct fallback accepted")
	}
	for _, proxy := range []string{"socks5://relay:1080", "http://user:pass@relay:18443", "http://relay:18443/path", "http://relay:18443?x=1"} {
		cfg = valid()
		cfg.DiscoveryProxy = proxy
		if cfg.ValidateDiscovery() == nil {
			t.Fatalf("unsafe proxy %q accepted", proxy)
		}
	}
	cfg = SpektacularRecheckConfig{DiscoveryProxy: "ftp://relay"}
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("proxy without sources skipped validation")
	}
	cfg = valid()
	cfg.EgressAllowlist = []string{"*.github.com"}
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("wildcard accepted")
	}
	cfg = valid()
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("duplicate accepted")
	}
	cfg = valid()
	cfg.Sources[0].Kind = "crawl"
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("crawl accepted")
	}
	cfg = valid()
	for i := 0; i < SpektacularDiscoveryMaxSources; i++ {
		cfg.Sources = append(cfg.Sources, SpektacularDiscoverySource{Name: "extra" + strings.Repeat("x", i), Kind: "release", URL: cfg.Sources[0].URL})
	}
	if cfg.ValidateDiscovery() == nil {
		t.Fatal("too many sources accepted")
	}
	// Exercise the actual configuration entry point, with cadence still off.
	c := Config{}
	c.Project.Org = "acme"
	c.GitHub.Token = "test"
	c.Runs.Spektacular.Recheck = valid()
	if err := c.ValidateWithOptions(ValidateOptions{}); err != nil && strings.Contains(err.Error(), "runs.spektacular.recheck") {
		t.Fatalf("valid discovery rejected at load: %v", err)
	}
	c.Runs.Spektacular.Recheck.Sources[0].URL = "https://outside.example/news"
	if err := c.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "outside the egress allow-list") {
		t.Fatalf("validation = %v", err)
	}
	c.Runs.Spektacular.Recheck = valid()
	c.Runs.Spektacular.Recheck.DiscoveryProxy = ""
	if err := c.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "discovery_proxy") {
		t.Fatalf("missing proxy validation = %v", err)
	}
}

func TestValidateRecheckDiscoveryCoversTypedAndExactSources(t *testing.T) {
	var nilCfg *Config
	if err := nilCfg.ValidateRecheckDiscovery(); err != nil {
		t.Fatalf("nil config = %v", err)
	}
	c := &Config{}
	c.Runs.Spektacular.Recheck.Discovery = SpektacularRecheckDiscoveryConfig{
		Enabled: true,
		Sources: []SpektacularRecheckDiscoverySource{{Kind: "standards_feed", Name: "feed", URLOrRepo: "https://feed.example/atom"}},
	}
	if err := c.ValidateRecheckDiscovery(); err == nil || !strings.Contains(err.Error(), "http_allowlist") {
		t.Fatalf("typed source outside http_allowlist = %v", err)
	}
	c.Variables.Security.HTTPAllowlist = []string{"feed.example"}
	if err := c.ValidateRecheckDiscovery(); err != nil {
		t.Fatalf("allowlisted typed source = %v", err)
	}
	c.Runs.Spektacular.Recheck.Sources = []SpektacularDiscoverySource{{Name: "doc", Kind: "standards", URL: "https://doc.example/spec"}}
	if err := c.ValidateRecheckDiscovery(); err == nil || !strings.Contains(err.Error(), "discovery_proxy") {
		t.Fatalf("exact source without proxy = %v", err)
	}
	c.Runs.Spektacular.Recheck.DiscoveryProxy = "http://relay:18443"
	c.Runs.Spektacular.Recheck.EgressAllowlist = []string{"doc.example"}
	if err := c.ValidateRecheckDiscovery(); err != nil {
		t.Fatalf("exact source with proxy = %v", err)
	}
}

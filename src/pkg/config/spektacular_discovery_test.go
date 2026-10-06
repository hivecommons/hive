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
	// Exercise the actual configuration entry point, with cadence still off.
	c := Config{}
	c.Project.Org = "acme"
	c.GitHub.Token = "test"
	c.Runs.Spektacular.Recheck = valid()
	c.Runs.Spektacular.Recheck.Sources[0].URL = "https://outside.example/news"
	if err := c.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "outside the egress allow-list") {
		t.Fatalf("validation = %v", err)
	}
}

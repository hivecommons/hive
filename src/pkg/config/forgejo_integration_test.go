package config

import (
	"strings"
	"testing"
)

func validIntegration() *ForgejoIntegrationConfig {
	return &ForgejoIntegrationConfig{
		Issuer:    "https://keys.example.com",
		KeyFile:   "/etc/hive/forgejo-jwt.pem",
		Audiences: map[string]string{"contributor": "aud-c", "advisor": "aud-a"},
	}
}

func TestForgejoIntegrationValidate(t *testing.T) {
	var none *ForgejoIntegrationConfig
	if err := none.Validate(); err != nil {
		t.Fatalf("nil block: %v", err)
	}
	if err := validIntegration().Validate(); err != nil {
		t.Fatalf("valid block: %v", err)
	}
	withSlash := validIntegration()
	withSlash.Issuer = "https://keys.example.com/"
	if err := withSlash.Validate(); err != nil || withSlash.IssuerOrigin() != "https://keys.example.com" {
		t.Fatalf("trailing slash: %v %q", err, withSlash.IssuerOrigin())
	}
	for name, mutate := range map[string]func(*ForgejoIntegrationConfig){
		"http issuer":      func(c *ForgejoIntegrationConfig) { c.Issuer = "http://keys.example.com" },
		"issuer with path": func(c *ForgejoIntegrationConfig) { c.Issuer = "https://keys.example.com/jwt" },
		"issuer userinfo":  func(c *ForgejoIntegrationConfig) { c.Issuer = "https://u:p@keys.example.com" },
		"issuer query":     func(c *ForgejoIntegrationConfig) { c.Issuer = "https://keys.example.com?x=1" },
		"no issuer":        func(c *ForgejoIntegrationConfig) { c.Issuer = "" },
		"relative key":     func(c *ForgejoIntegrationConfig) { c.KeyFile = "key.pem" },
		"no key":           func(c *ForgejoIntegrationConfig) { c.KeyFile = "" },
		"no audiences":     func(c *ForgejoIntegrationConfig) { c.Audiences = nil },
		"unknown tier":     func(c *ForgejoIntegrationConfig) { c.Audiences["admin"] = "aud" },
		"blank audience":   func(c *ForgejoIntegrationConfig) { c.Audiences["advisor"] = " " },
	} {
		c := validIntegration()
		mutate(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "forgejo.integration") {
			t.Fatalf("%s: err = %v, want a forgejo.integration error", name, err)
		}
	}
	if got := none.IssuerOrigin(); got != "" {
		t.Fatalf("nil IssuerOrigin = %q", got)
	}
}

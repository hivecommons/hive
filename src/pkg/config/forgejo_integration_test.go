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
	withREST := validIntegration()
	withREST.RESTAudience = "aud-rest"
	if err := withREST.Validate(); err != nil {
		t.Fatalf("rest_audience: %v", err)
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
		"blank rest aud":   func(c *ForgejoIntegrationConfig) { c.RESTAudience = " " },
		"rest aud = tier":  func(c *ForgejoIntegrationConfig) { c.RESTAudience = " " + c.Audiences["advisor"] + " " },
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

func TestForgejoIntegrationPreviousKeyFiles(t *testing.T) {
	served := validIntegration()
	served.ServeKeyList = true
	served.PreviousKeyFiles = []string{" /etc/hive/forgejo-jwt.old.pem "}
	if err := served.Validate(); err != nil {
		t.Fatalf("served list with one previous key: %v", err)
	}
	if got := served.PreviousKeyPaths(); len(got) != 1 || got[0] != "/etc/hive/forgejo-jwt.old.pem" {
		t.Fatalf("PreviousKeyPaths = %q", got)
	}
	var none *ForgejoIntegrationConfig
	if got := none.PreviousKeyPaths(); got != nil {
		t.Fatalf("nil PreviousKeyPaths = %q", got)
	}
	tooMany := make([]string, MaxPreviousKeyFiles+1)
	for i := range tooMany {
		tooMany[i] = "/etc/hive/old-" + string(rune('a'+i)) + ".pem"
	}
	for name, mutate := range map[string]func(*ForgejoIntegrationConfig){
		"not served":    func(c *ForgejoIntegrationConfig) { c.ServeKeyList = false },
		"relative path": func(c *ForgejoIntegrationConfig) { c.PreviousKeyFiles = []string{"old.pem"} },
		"blank path":    func(c *ForgejoIntegrationConfig) { c.PreviousKeyFiles = []string{" "} },
		"same as key":   func(c *ForgejoIntegrationConfig) { c.PreviousKeyFiles = []string{"/etc/hive/./forgejo-jwt.pem"} },
		"repeated": func(c *ForgejoIntegrationConfig) {
			c.PreviousKeyFiles = []string{"/etc/hive/old.pem", "/etc/hive/old.pem"}
		},
		"too many": func(c *ForgejoIntegrationConfig) { c.PreviousKeyFiles = tooMany },
	} {
		c := validIntegration()
		c.ServeKeyList = true
		c.PreviousKeyFiles = []string{"/etc/hive/forgejo-jwt.old.pem"}
		mutate(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "forgejo.integration.previous_key_files") {
			t.Fatalf("%s: err = %v, want a previous_key_files error", name, err)
		}
	}
	// serve_key_list alone keeps the issuer rules: a path is still refused.
	withPath := validIntegration()
	withPath.ServeKeyList = true
	withPath.Issuer = "https://hive.example.com/keys"
	if err := withPath.Validate(); err == nil || !strings.Contains(err.Error(), "forgejo.integration.issuer") {
		t.Fatalf("served list with a path issuer: err = %v", err)
	}
}

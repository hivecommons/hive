package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// A placeholder booted against its pool's default forge (GHE App 5686) and
// then claimed onto github.com receives App 3568013 with installation_id 0.
// HasUsableApp is false for that identity, so the full client rebuild is
// (correctly) skipped — but installation discovery must still move to the new
// App/forge, or it polls the old one forever and the hive never adopts the
// installation its owner makes (live: `ash` → thinkscientist on vllm-d).
func TestDiscoveryAppAuthStaleOnForgeSwitchWithoutInstallation(t *testing.T) {
	gheCfg := &config.Config{}
	gheCfg.GitHub.AppID = 5686
	gheCfg.GitHub.APIURL = "https://github.ibm.com/api/v3"
	booted := testAppAuth(t, gheCfg)

	public := config.GitHubConfig{AppID: 3568013, InstallationID: 0, APIURL: "https://api.github.com"}

	if !discoveryAppAuthStale(booted, public, "/data/gh-app-key-3568013.pem") {
		t.Fatal("GHE-booted AppAuth must be replaced when the hub delivers the github.com App with no installation")
	}
	if !discoveryAppAuthStale(nil, public, "/data/gh-app-key-3568013.pem") {
		t.Fatal("a spoke that booted with no AppAuth must get one for discovery")
	}
}

func TestDiscoveryAppAuthStaleNoOpCases(t *testing.T) {
	cfg := &config.Config{}
	cfg.GitHub.AppID = 3568013
	cfg.GitHub.APIURL = "https://api.github.com"
	current := testAppAuth(t, cfg)

	cases := []struct {
		name string
		gh   config.GitHubConfig
		key  string
	}{
		{"same App and forge already held", config.GitHubConfig{AppID: 3568013, APIURL: "https://api.github.com/"}, "/k.pem"},
		{"installation known — the usable-App rebuild owns this", config.GitHubConfig{AppID: 5686, InstallationID: 42, APIURL: "https://github.ibm.com/api/v3"}, "/k.pem"},
		{"no key resolved", config.GitHubConfig{AppID: 5686, APIURL: "https://github.ibm.com/api/v3"}, ""},
		{"placeholder App is not a real App", config.GitHubConfig{AppID: config.PlaceholderAppID}, "/k.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if discoveryAppAuthStale(current, tc.gh, tc.key) {
				t.Errorf("expected no discovery AppAuth swap")
			}
		})
	}
}

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/forgejwt"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
)

func forgejoTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "team"
	cfg.Project.Repos = []string{"app"}
	cfg.Project.Forge = config.ForgeForgejo
	cfg.Forgejo.URL = "https://forgejo.example.com/"
	cfg.Forgejo.TokenEnv = "HIVE_TEST_FORGEJO_REMOTE_TOKEN"
	return cfg
}

func TestForgeRemoteResolverGitHubKeepsTheDefault(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	for name, cfg := range map[string]*config.Config{"nil": nil, "github": {}} {
		cfg := cfg
		_, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
		if err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v, want github.com kept", name, ok, err)
		}
	}
}

func TestForgeRemoteResolverForgejo(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	t.Setenv("HIVE_TEST_FORGEJO_REMOTE_TOKEN", "fj_remote")
	cfg := forgejoTestConfig()
	remote, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if remote.BaseURL != "https://forgejo.example.com" {
		t.Fatalf("BaseURL = %q", remote.BaseURL)
	}
	tok, err := remote.Minter.MintPushToken(context.Background(), "team/app")
	if err != nil || tok != "fj_remote" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if !proxy.IsForgeHost("forgejo.example.com") {
		t.Fatal("resolving a forgejo repo did not register its host with the proxy")
	}
}

// A repo_policies forge_url on another host would send the forgejo block's
// token there; the resolver refuses it (#19572's rule, applied to git).
func TestForgeRemoteResolverRefusesAForeignForgeURL(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	cfg := forgejoTestConfig()
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "app", Forge: config.ForgeForgejo, ForgeURL: "https://evil.example"}}
	_, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want the foreign host refused", ok, err)
	}
	if proxy.IsForgeHost("evil.example") {
		t.Fatal("a refused host was registered with the proxy")
	}
}

// With no forgejo block URL the host check has nothing to compare, so a
// policy forge_url alone must not receive the token.
func TestForgeRemoteResolverRefusesAPolicyURLWithoutABlockURL(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	cfg := forgejoTestConfig()
	cfg.Forgejo.URL = ""
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "app", Forge: config.ForgeForgejo, ForgeURL: "https://other.example"}}
	_, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
	if err == nil || ok || !strings.Contains(err.Error(), "forgejo.forgejo_url is not set") {
		t.Fatalf("ok=%v err=%v, want the policy-only URL refused", ok, err)
	}
	if proxy.IsForgeHost("other.example") {
		t.Fatal("a refused host was registered with the proxy")
	}
}

// Plain http would expose the token on the wire.
func TestForgeRemoteResolverRefusesPlainHTTP(t *testing.T) {
	cfg := forgejoTestConfig()
	cfg.Forgejo.URL = "http://forgejo.example.com"
	if _, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app"); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want http refused", ok, err)
	}
}

func TestForgeRemoteResolverRefusesForgejoWithoutURL(t *testing.T) {
	cfg := forgejoTestConfig()
	cfg.Forgejo.URL = ""
	if _, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app"); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error", ok, err)
	}
	cfg.Forgejo.URL = "not a url"
	if _, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app"); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error for an invalid URL", ok, err)
	}
}

// Clone and push are not implemented for GitLab and Gitea: the repo is
// refused, not cloned from github.com.
func TestForgeRemoteResolverRefusesUnimplementedForges(t *testing.T) {
	for _, kind := range []string{config.ForgeGitLab, config.ForgeGitea} {
		cfg := &config.Config{}
		cfg.Project.Org = "team"
		cfg.Project.Forge = kind
		cfg.Gitea.URL = "https://gitea.example.com"
		_, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
		if err == nil || ok || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("%s: ok=%v err=%v, want not implemented", kind, ok, err)
		}
	}
}

// With forgejo.integration set, git on a Forgejo repo uses a per-tier JWT
// minter, not the block's access token.
func TestForgejoMinterUsesTheIntegration(t *testing.T) {
	cfg := forgejoTestConfig()
	if _, ok := forgejoMinter(cfg).(pushbroker.StaticTokenMinter); !ok {
		t.Fatal("without an integration the static token minter is used")
	}
	cfg.Forgejo.Integration = &config.ForgejoIntegrationConfig{
		Issuer:    "https://keys.example.com/",
		KeyFile:   " /etc/hive/forgejo-jwt.pem ",
		Audiences: map[string]string{"contributor": " aud-c "},
	}
	m, ok := forgejoMinter(cfg).(forgejwt.Minter)
	if !ok {
		t.Fatalf("minter = %T, want forgejwt.Minter", forgejoMinter(cfg))
	}
	if m.Signer.Issuer != "https://keys.example.com" || m.Signer.KeyFile != "/etc/hive/forgejo-jwt.pem" || m.Audiences["contributor"] != "aud-c" {
		t.Fatalf("minter = %+v", m)
	}
	remote, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("team/app")
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	if err != nil || !ok {
		t.Fatalf("resolver ok=%v err=%v", ok, err)
	}
	if _, isJWT := remote.Minter.(forgejwt.Minter); !isJWT {
		t.Fatalf("resolver minter = %T, want the JWT minter", remote.Minter)
	}
}

func TestForgejoHosts(t *testing.T) {
	if got := forgejoHosts(nil); got != nil {
		t.Fatalf("nil config hosts = %v", got)
	}
	if got := forgejoHosts(&config.Config{}); len(got) != 0 {
		t.Fatalf("a GitHub-only config registers hosts %v", got)
	}
	cfg := forgejoTestConfig()
	cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "other", Forge: config.ForgeForgejo, ForgeURL: "https://Forgejo.Example.com"},
		{Repo: "gh", Forge: config.ForgeGitHub},
	}
	got := forgejoHosts(cfg)
	if len(got) != 1 || got[0] != "forgejo.example.com" {
		t.Fatalf("hosts = %v, want only the forgejo host", got)
	}
}

func TestWireForgeRemotesRegistersOnlyConfiguredHosts(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	b := &boot{cfg: &config.Config{}}
	b.wireForgeRemotes()
	if proxy.IsForgeHost("forgejo.example.com") {
		t.Fatal("a GitHub-only hive registered a forge host")
	}
	b.cfg = forgejoTestConfig()
	b.wireForgeRemotes()
	if !proxy.IsForgeHost("forgejo.example.com") {
		t.Fatal("the configured forgejo host is not inspected")
	}
	if _, ok, err := b.bootForgeRemoteResolver()("team/app"); err != nil || !ok {
		t.Fatalf("boot resolver ok=%v err=%v", ok, err)
	}
}

// Boot says so when every Forgejo token tier would push with the one write
// token (hivecommons/hive#19917), and stays quiet when forgejo.integration
// gives each tier its own token or no repo is on Forgejo.
func TestWireForgeRemotesWarnsWhenTiersShareTheWriteToken(t *testing.T) {
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	var logs bytes.Buffer
	b := &boot{cfg: forgejoTestConfig(), logger: slog.New(slog.NewTextHandler(&logs, nil))}
	b.wireForgeRemotes()
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "forgejo.integration is not set") || !strings.Contains(out, "HIVE_TEST_FORGEJO_REMOTE_TOKEN") {
		t.Fatalf("boot log = %q, want one warning naming forgejo.integration and the token env", out)
	}

	logs.Reset()
	b.cfg.Forgejo.Integration = &config.ForgejoIntegrationConfig{}
	b.wireForgeRemotes()
	if strings.Contains(logs.String(), "forgejo.integration is not set") {
		t.Fatalf("warned with forgejo.integration set: %q", logs.String())
	}

	logs.Reset()
	b.cfg = &config.Config{}
	b.wireForgeRemotes()
	if strings.Contains(logs.String(), "forgejo.integration is not set") {
		t.Fatalf("a GitHub-only hive warned: %q", logs.String())
	}
}

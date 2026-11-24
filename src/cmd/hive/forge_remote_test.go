package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/proxy"
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

package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/forgejwt"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
)

// forgeRemoteResolver is the crux-side resolver for where a repo is cloned
// from and pushed to (hivecommons/hive#19494 item 4). cfgFn is read per call so
// a reload takes effect without a restart.
//
//   - github (and an unknown kind, which falls back to GitHub everywhere
//     else too): ok=false, so the caller keeps github.com and its GitHub App
//     minter unchanged.
//   - forgejo: the repo's forge URL and forgejoMinter (a per-tier JWT with
//     forgejo.integration, else the forgejo block's access token). The
//     token goes only to the forgejo block's host
//     (CheckForgeTokenHost, #19572), and the resolver refuses a repo whose
//     forge_url would send it elsewhere.
//   - gitlab and gitea: clone and push are NOT implemented for them yet, so
//     the resolver refuses the repo outright rather than let a kick clone
//     github.com under the same name or hand a forge token to the wrong host.
func forgeRemoteResolver(cfgFn func() *config.Config) pushbroker.ForgeRemoteResolver {
	return func(repo string) (pushbroker.ForgeRemote, bool, error) {
		cfg := cfgFn()
		if cfg == nil {
			return pushbroker.ForgeRemote{}, false, nil
		}
		switch kind := cfg.ForgeKindForRepo(repo); kind {
		case config.ForgeForgejo:
			return forgejoRemote(cfg, repo)
		case config.ForgeGitLab, config.ForgeGitea:
			return pushbroker.ForgeRemote{}, false, fmt.Errorf("repo %s is on %s; cloning from and pushing to %s is not implemented (only github and forgejo are)", repo, kind, kind)
		default:
			return pushbroker.ForgeRemote{}, false, nil
		}
	}
}

func forgejoRemote(cfg *config.Config, repo string) (pushbroker.ForgeRemote, bool, error) {
	// A forge token goes only to its own block's host; see CheckForgeTokenHost.
	// That check has nothing to compare when the block names no URL, so git
	// additionally requires forgejo.forgejo_url: without it a repo_policies
	// forge_url alone, on any host, would receive the token.
	if strings.TrimSpace(cfg.Forgejo.URL) == "" {
		return pushbroker.ForgeRemote{}, false, fmt.Errorf("repo %s is on forgejo but forgejo.forgejo_url is not set; git sends the forgejo token only to that block's host", repo)
	}
	if err := cfg.CheckForgeTokenHost(repo); err != nil {
		return pushbroker.ForgeRemote{}, false, err
	}
	raw := cfg.ForgeURLForRepo(repo)
	base, err := pushbroker.NormalizeForgeBaseURL(raw)
	if err != nil || strings.TrimSpace(raw) == "" {
		return pushbroker.ForgeRemote{}, false, fmt.Errorf("repo %s is on forgejo but has no usable forge URL: %v", repo, err)
	}
	// The agents' proxy inspects this host from now on, in case it was added
	// by a reload after boot.
	if u, err := url.Parse(base); err == nil {
		proxy.RegisterForgeHost(u.Hostname())
	}
	return pushbroker.ForgeRemote{BaseURL: base, Minter: forgejoMinter(cfg)}, true, nil
}

// forgejoMinter is the minter for git on a Forgejo repo. With
// forgejo.integration set it signs a one-hour JWT per token tier for that
// tier's authorized integration (crux/forgejwt, #19576), the Forgejo
// analogue of a GitHub App installation token. Without it, every tier uses
// the forgejo block's access token, the same one the REST adapter sends.
func forgejoMinter(cfg *config.Config) pushbroker.TokenMinter {
	if in := cfg.Forgejo.Integration; in != nil {
		auds := make(map[agentmode.TokenTier]forgejwt.Audience, len(in.Audiences))
		for tier, aud := range in.Audiences {
			auds[agentmode.TokenTier(tier)] = forgejwt.Audience(strings.TrimSpace(aud))
		}
		return forgejwt.Minter{
			Signer:    forgejwt.Signer{Issuer: forgejwt.Issuer(in.IssuerOrigin()), KeyFile: strings.TrimSpace(in.KeyFile)},
			Audiences: auds,
		}
	}
	gov := cfg.Forgejo.Credentials().Governor()
	return pushbroker.StaticTokenMinter{Token: func() (string, error) {
		token, _, err := gov.Resolve()
		return token, err
	}}
}

// forgejoHosts lists the hosts of every forge URL a Forgejo repo of cfg
// resolves to: the project-wide forgejo block when project.forge is forgejo,
// and each repo_policies entry that lands on forgejo. A GitHub-only config
// yields none.
func forgejoHosts(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	repos := append([]string{""}, cfg.Project.Repos...)
	for _, rp := range cfg.Project.RepoPolicies {
		repos = append(repos, rp.Repo)
	}
	seen := map[string]bool{}
	var hosts []string
	for _, repo := range repos {
		if cfg.ForgeKindForRepo(repo) != config.ForgeForgejo {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(cfg.ForgeURLForRepo(repo)))
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// forgeRemoteSetter is the part of the agent runtime that takes the resolver.
// The in-process Manager implements it; a runtime that does not (the agent
// runtime sidecar) keeps every repo on github.com, as before.
type forgeRemoteSetter interface {
	SetSandboxForgeRemotes(pushbroker.ForgeRemoteResolver)
}

// bootForgeRemoteResolver is forgeRemoteResolver over the boot's live config.
func (b *boot) bootForgeRemoteResolver() pushbroker.ForgeRemoteResolver {
	return forgeRemoteResolver(func() *config.Config { return b.cfg })
}

// wireForgeRemotes points sandbox kicks at each repo's own forge, and
// registers the configured Forgejo hosts with the agents' proxy so its mode
// rules and the Forgejo hard denies apply to them. The Sessions workspace
// clone gets the same resolver when it is created (userSessionRuntime).
//
// It also warns, once per boot, when Forgejo repos have no
// forgejo.integration: every token tier then pushes with the one write
// token (hivecommons/hive#19917).
func (b *boot) wireForgeRemotes() {
	if s, ok := b.agentMgr.(forgeRemoteSetter); ok {
		s.SetSandboxForgeRemotes(b.bootForgeRemoteResolver())
	}
	proxy.SetForgeHosts(forgejoHosts(b.cfg))
	if w := b.cfg.ForgejoSharedWriteTokenWarning(); w != "" && b.logger != nil {
		b.logger.Warn(w, "fix", "forgejo.integration")
	}
}

package main

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/remoteitems"
)

// Own-item follow-up on remote repos (hivecommons/hive#18199).
//
// Every reference-repo pass (boot, config reload, the derive poller, an
// owner-identity change) ends by polling the write-tier reference repos for
// the issues and PRs the hive opened there (as the owner, or as the App bot
// on a repo that writes as the bot, #18516) and routing new feedback to the
// agents that opened them. The poller is bounded by design: it lists
// creator=<that login> only and reads comments on tracked items only.
// Nothing here scans a remote backlog, kicks from a remote queue or runs a
// stage on a remote item.

// newRemoteItemsPoller builds the poller over the reference-repo credentials:
// each repo's proxy policy token when a proxy is wired, else the owner token.
func (b *boot) newRemoteItemsPoller(rt *referenceRepoRuntime) *remoteitems.Poller {
	if rt == nil || (rt.deps.ownerToken == nil && b.githubProxy == nil) {
		return nil
	}
	apiURL := ""
	if b.ghClient != nil {
		if auth := b.ghClient.AppAuth(); auth != nil {
			apiURL = auth.APIURL()
		}
	}
	fetch := &github.RemoteItemsFetcher{Token: rt.deps.ownerToken, APIURL: apiURL}
	if b.githubProxy != nil {
		// Each watched repo is read under its own reference-repo credential,
		// the one the proxy injects on its writes: the scoped bot token on a
		// repo that writes as the App bot, the owner's token (or the
		// write_token_file) elsewhere (hivecommons/hive#18516).
		proxy := b.githubProxy
		fetch.TokenFor = func(repo string) (string, bool) {
			pol, ok := proxy.ReferenceRepoPolicyFor(repo)
			if !ok || pol.Token == nil {
				return "", false
			}
			return pol.Token()
		}
	}
	return &remoteitems.Poller{
		Store:        remoteitems.NewStore(""),
		Fetch:        fetch,
		Marker:       github.HiveMarkerAgent,
		ResolveAgent: func(name string) string { return resolveMarkerAgent(b.cfg, name) },
		Logger:       b.logger,
	}
}

// resolveMarkerAgent maps the name a hive marker carries to a configured
// agent: an exact agent name, else an agent's configured display name (what
// the gh-wrapper footer prints), else a case-insensitive match of either.
// "" when nothing matches.
func resolveMarkerAgent(cfg *config.Config, name string) string {
	name = strings.TrimSpace(name)
	if cfg == nil || name == "" {
		return ""
	}
	if _, ok := cfg.Agents[name]; ok {
		return name
	}
	for agent, ac := range cfg.Agents {
		if ac.DisplayName != "" && ac.DisplayName == name {
			return agent
		}
	}
	for agent, ac := range cfg.Agents {
		if strings.EqualFold(agent, name) || (ac.DisplayName != "" && strings.EqualFold(ac.DisplayName, name)) {
			return agent
		}
	}
	return ""
}

// remoteItemTargets returns the reference repos the poller watches: write
// tier with a resolved credential login, each under the login its writes
// appear as. That is the App bot's on a repo that writes as the bot
// (project.remote_write_identity, hivecommons/hive#18516), the owner's or the
// write_token_file user's elsewhere.
func remoteItemTargets(refs []config.ReferenceRepo) []remoteitems.Target {
	var out []remoteitems.Target
	for _, rr := range refs {
		if rr.ReadOnly() || rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin == "" {
			continue
		}
		out = append(out, remoteitems.Target{Repo: rr.Repo, Login: rr.CredentialLogin})
	}
	return out
}

// pollRemoteItems runs one follow-up pass over refs, each repo listed under
// the login its writes appear as and read under its own credential.
func (b *boot) pollRemoteItems(ctx context.Context, rt *referenceRepoRuntime, refs []config.ReferenceRepo) {
	if rt == nil || rt.remoteItems == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	targets := remoteItemTargets(refs)
	if len(targets) == 0 {
		return
	}
	var resumer remoteitems.Resumer
	if b.agentMgr != nil {
		resumer = b.agentMgr
	}
	out := rt.remoteItems.RunTargets(ctx, targets, resumer, time.Now())
	if out.Added+out.Pruned+out.Routed+out.Queued+out.Unrouted+out.ListErrs+out.FetchErrs > 0 {
		b.logger.Info("remote items: follow-up pass", "repos", out.Repos, "tracked", out.Tracked, "added", out.Added, "pruned", out.Pruned,
			"routed", out.Routed, "queued", out.Queued, "unrouted", out.Unrouted, "list_errors", out.ListErrs, "fetch_errors", out.FetchErrs)
	}
}

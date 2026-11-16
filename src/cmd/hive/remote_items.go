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
// the issues and PRs the hive opened there as the owner and routing new
// feedback to the agents that opened them. The poller is bounded by design:
// it lists creator=<owner> only and reads comments on tracked items only.
// Nothing here scans a remote backlog, kicks from a remote queue or runs a
// stage on a remote item.

// newRemoteItemsPoller builds the poller over the owner token source.
func (b *boot) newRemoteItemsPoller(rt *referenceRepoRuntime) *remoteitems.Poller {
	if rt == nil || rt.deps.ownerToken == nil {
		return nil
	}
	apiURL := ""
	if b.ghClient != nil {
		if auth := b.ghClient.AppAuth(); auth != nil {
			apiURL = auth.APIURL()
		}
	}
	return &remoteitems.Poller{
		Store:        remoteitems.NewStore(""),
		Fetch:        &github.RemoteItemsFetcher{Token: rt.deps.ownerToken, APIURL: apiURL},
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

// remoteItemRepos returns the reference repos the poller watches: write tier
// with a resolved credential login, which is the owner's login. Entries that
// write as the App bot (project.remote_write_identity) are left out: the
// poller reads under the owner token and lists creator=<owner>, so it would
// never find the bot's items there.
func remoteItemRepos(refs []config.ReferenceRepo) (repos []string, login string) {
	for _, rr := range refs {
		if rr.ReadOnly() || rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin == "" {
			continue
		}
		if rr.CredentialIdentity == config.ReferenceIdentityBot {
			continue
		}
		repos = append(repos, rr.Repo)
		if login == "" {
			login = rr.CredentialLogin
		}
	}
	return repos, login
}

// pollRemoteItems runs one follow-up pass over refs. Entries whose
// credential is a write_token_file are listed under that file's login too
// only when it matches the owner's; a mixed set follows the first login.
func (b *boot) pollRemoteItems(ctx context.Context, rt *referenceRepoRuntime, refs []config.ReferenceRepo) {
	if rt == nil || rt.remoteItems == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	repos, login := remoteItemRepos(refs)
	if len(repos) == 0 || login == "" {
		return
	}
	var resumer remoteitems.Resumer
	if b.agentMgr != nil {
		resumer = b.agentMgr
	}
	out := rt.remoteItems.Run(ctx, repos, login, resumer, time.Now())
	if out.Added+out.Pruned+out.Routed+out.Queued+out.Unrouted+out.ListErrs+out.FetchErrs > 0 {
		b.logger.Info("remote items: follow-up pass", "repos", out.Repos, "tracked", out.Tracked, "added", out.Added, "pruned", out.Pruned,
			"routed", out.Routed, "queued", out.Queued, "unrouted", out.Unrouted, "list_errors", out.ListErrs, "fetch_errors", out.FetchErrs)
	}
}

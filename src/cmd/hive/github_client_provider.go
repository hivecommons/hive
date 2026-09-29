package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/pushbroker"
)

// githubClientRef boxes the live client so the provider can tell "never
// published" (nil ref) from "published, and the hive has no client" (ref with
// a nil client).
type githubClientRef struct {
	client *github.Client
}

// publishGitHubClient makes client the one currentGitHubClient returns. Boot
// publishes the client initGitHubAuth built; adoptGitHubClient publishes every
// rebuilt one.
func (b *boot) publishGitHubClient(client *github.Client) {
	if b == nil {
		return
	}
	b.ghClientLive.Store(&githubClientRef{client: client})
}

// currentGitHubClient is the GitHub client provider for long-lived consumers
// (#9621). Collectors and the scheduler used to capture b.ghClient once at
// boot and kept that client for the life of the process: after an App
// credential rebuild they ran on the old credentials, and on a hive that
// booted without an App (hosted spokes get theirs over the heartbeat) they
// held a nil client forever. Reading through this on every use follows every
// rebuild. It is safe to call from any goroutine.
//
// Before boot publishes a client (only unit tests that set b.ghClient
// directly reach that state) it falls back to b.ghClient.
func (b *boot) currentGitHubClient() *github.Client {
	if b == nil {
		return nil
	}
	if ref := b.ghClientLive.Load(); ref != nil {
		return ref.client
	}
	return b.ghClient
}

// adoptGitHubClient makes a client rebuilt by newConfiguredGitHubAppClient
// the hive's client, everywhere at once (#9621). All three rebuild paths call
// it (the dashboard's ReinitGitHubFunc, the config watcher's App identity
// change and the heartbeat's App credential delivery), pinned by
// TestGitHubClientRebuildSitesUseSharedConstructor. Before this, each site
// re-pointed its own subset: only the config watcher moved the agent sandbox
// to the new client and push minter, and none of them moved the request
// relays, the collectors or the scheduler.
//
// newClient must already be configured (newConfiguredGitHubAppClient did
// that); this only swaps references.
func (b *boot) adoptGitHubClient(newClient *github.Client, newAppAuth *github.AppAuth) {
	if b == nil || newClient == nil {
		return
	}
	b.ghClient = newClient
	b.appAuth = newAppAuth
	// Provider consumers (metrics and fleet-stats collectors, the scheduler's
	// triage commenter) see the new client on their next read.
	b.publishGitHubClient(newClient)
	if b.agentMgr != nil {
		b.agentMgr.SetAppAuth(newAppAuth)
		// The sandbox kick executor opens PRs and mints push tokens itself;
		// leaving it on the old client/AppAuth is the stale pointer the
		// dashboard and heartbeat rebuilds used to miss.
		b.agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: newAppAuth})
		b.agentMgr.SetSandboxPRClient(newClient)
	}
	if b.dashSrv != nil {
		b.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
	}
	// Starts the relays for the first time on a hive that booted without a
	// usable App, or hands running relays over to the new client.
	b.armRequestRelays(newClient)
}

// armRequestRelays runs the request relays on client, if the hive has a
// usable App to author as. This is the usable-App gate the relays have always
// had (no App, no bot identity: requests accumulate on disk rather than open
// under a wrong identity), now evaluated every time a client arrives instead
// of once at boot. Returns whether a start or hand-over was scheduled.
func (b *boot) armRequestRelays(client *github.Client) bool {
	if b == nil || b.requestRelays == nil || client == nil || b.cfg == nil || !b.cfg.GitHub.HasUsableApp() {
		return false
	}
	return b.requestRelays.switchTo(client)
}

// liveTriageCommenter is the scheduler's runs-triage commenter, read through
// the client provider on every call so the scheduler never holds a captured
// (possibly nil, possibly stale) client (#9621).
type liveTriageCommenter struct {
	client func() *github.Client
}

func (l liveTriageCommenter) IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error) {
	c := l.current()
	if c == nil {
		return false, github.ErrNoGitHubClient
	}
	return c.IssueCommentsContain(ctx, repo, number, needle)
}

func (l liveTriageCommenter) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	c := l.current()
	if c == nil {
		return github.ErrNoGitHubClient
	}
	return c.CreateIssueComment(ctx, repo, number, body)
}

func (l liveTriageCommenter) current() *github.Client {
	if l.client == nil {
		return nil
	}
	return l.client()
}

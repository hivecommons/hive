package main

import (
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/repoagents"
)

const (
	// repoAgentsStateEnv overrides where discovered repo agents persist.
	repoAgentsStateEnv     = "HIVE_REPO_AGENTS_STATE"
	defaultRepoAgentsState = "/data/repo-agents.json"
	// repoAgentsIntervalEnv overrides the rescan interval (a Go duration).
	repoAgentsIntervalEnv = "HIVE_REPO_AGENTS_INTERVAL"
	// repoAgentsOrphanTrigger is the pause trigger an orphaned agent records.
	repoAgentsOrphanTrigger = "repo-agent-discovery"
)

// wireRepoAgentDiscovery builds the .hive/agents/ discoverer
// (hivecommons/hive#18491) so the dashboard dependencies carry it, and starts
// its scan loop. It reads managed repos and the repo-sourced prompt allowlist
// from the live config on every pass, reads files through the hive's GitHub
// App client, and pauses an approved agent whose file is removed. Without a
// GitHub client, discovery stays off.
func (b *boot) wireRepoAgentDiscovery() {
	if b.cfg == nil || b.ghClient == nil || b.ctx == nil {
		return
	}
	d := repoagents.New(repoagents.Options{
		Client:    b.ghClient,
		Allow:     func(slug string) bool { return b.cfg.GitHubDefinitionAllowed(slug) },
		Repos:     func() []string { return managedRepoSlugs(b.cfg) },
		StatePath: envOrDefault(repoAgentsStateEnv, defaultRepoAgentsState),
		OnOrphan:  b.pauseOrphanedRepoAgent,
		Logger:    b.logger,
	})
	b.repoAgents = d
	go d.Run(b.ctx, repoAgentsInterval(b))
}

// managedRepoSlugs is project.repos as "owner/repo" slugs.
func managedRepoSlugs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Project.Repos))
	for _, r := range cfg.Project.Repos {
		if slug := repoagents.RepoSlug(cfg.Project.Org, r); slug != "" {
			out = append(out, slug)
		}
	}
	return out
}

func repoAgentsInterval(b *boot) time.Duration {
	raw := os.Getenv(repoAgentsIntervalEnv)
	if raw == "" {
		return repoagents.DefaultInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		if b.logger != nil {
			b.logger.Warn("invalid repo agent discovery interval, using default", "env", repoAgentsIntervalEnv, "value", raw, "default", repoagents.DefaultInterval)
		}
		return repoagents.DefaultInterval
	}
	return d
}

// pauseOrphanedRepoAgent pauses (never deletes) the agent of a discovered
// definition whose file was removed, as long as that agent still points at
// the removed file.
func (b *boot) pauseOrphanedRepoAgent(e repoagents.Entry) {
	if b.agentMgr == nil || b.cfg == nil || e.Agent == "" {
		return
	}
	ac, ok := b.cfg.Agents[e.Agent]
	if !ok || !ac.DefinitionSource.IsSet() || ac.DefinitionSource.Slug() != e.Repo || ac.DefinitionSource.Path != e.Path {
		return
	}
	reason := "definition file " + e.Path + " was removed from " + e.Repo
	if err := b.agentMgr.PauseBy(e.Agent, repoAgentsOrphanTrigger, reason, repoAgentsOrphanTrigger); err != nil && b.logger != nil {
		b.logger.Warn("could not pause orphaned repo agent", "agent", e.Agent, "error", err)
	}
}

// noteRepoAgentPush rescans a repo whose push touched .hive/agents/ on its
// default branch.
func (b *boot) noteRepoAgentPush(event string, payload []byte) {
	if event != "push" || b.repoAgents == nil {
		return
	}
	if slug := repoagents.PushRescanRepo(payload); slug != "" {
		b.repoAgents.Trigger(slug)
	}
}

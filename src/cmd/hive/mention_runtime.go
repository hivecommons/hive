package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mention"
)

func (b *boot) ensureMentionStore() *mention.Store {
	if b.mentionStore != nil {
		return b.mentionStore
	}
	store, err := mention.NewStore("/data/github-mention-triggers.json")
	if err != nil {
		b.logger.Error("mention trigger store unavailable; GitHub mentions disabled and Actions OIDC dispatch will refuse", "error", err)
		return nil
	}
	b.mentionStore = store
	return store
}

func (b *boot) syncMentionRuntime() {
	if b == nil || b.cfg == nil {
		return
	}
	enabled := b.ghClient != nil && b.cfg.GitHub.HasUsableApp() && b.cfg.GitHub.Mentions.Enabled
	if !enabled {
		b.stopMentionPoller()
		return
	}
	if b.mentionPollerCancel != nil {
		return
	}
	store := b.ensureMentionStore()
	if store == nil {
		return
	}
	handler := mention.NewHandler(mention.Options{
		ConfigFunc:     func() config.GitHubMentionsConfig { return b.cfg.GitHub.Mentions },
		ActionsFunc:    func() config.GitHubActionsConfig { return b.cfg.GitHub.Actions },
		ReviewBotsFunc: func() config.ReviewBotsConfig { return b.cfg.Classification.ReviewBots },
		Roles: func(login string) (string, bool) {
			return b.cfg.Dashboard.AuthorizedRole(login)
		},
		Repos: func() []string {
			if b.ghClient == nil {
				return nil
			}
			return b.ghClient.ActiveRepositories()
		},
		Agents:     b.mentionAgents,
		GitHubFunc: func() mention.GitHub { return b.ghClient },
		Store:      store,
		Kick: func(agentName, message, source string) error {
			return b.agentMgr.SendKickWithSource(agentName, message, source)
		},
		Audit: func(action, detail, agentName string) {
			b.dashSrv.AuditLog("system", action, detail, agentName)
			recordLifecycleFromAudit(b.dashSrv, b.cfg.Project.Org, action, detail, agentName)
		},
	})
	poller := mention.NewPoller(nil, func() []string {
		if b.ghClient == nil {
			return nil
		}
		return b.ghClient.ActiveRepositories()
	}, store, handler, b.cfg.GitHub.Mentions.PollIntervalEffective(), b.logger)
	poller.SetGitHubGetter(func() mention.GitHub { return b.ghClient })
	poller.SetEnabledFunc(func() bool {
		return b.cfg.GitHub.Mentions.Enabled
	})
	if b.cfg.GitHub.Mentions.WebhookEnabled {
		receiver := mention.NewWebhookReceiver(func() string {
			return b.cfg.GitHub.Mentions.WebhookSecretEffective()
		}, poller, b.cfg.GitHub.Mentions.WebhookMinGapEffective(), b.logger)
		receiver.SetReposFunc(func() []string {
			if b.ghClient == nil {
				return nil
			}
			return b.ghClient.ActiveRepositories()
		})
		b.mentionWebhook = receiver
	}
	ctx, cancel := context.WithCancel(b.ctx)
	b.mentionPollerCancel = cancel
	go poller.Run(ctx)
	responder := mention.NewResponder(store, func() mention.GitHub { return b.ghClient }, b.mentionAgents, b.cfg.GitHub.Mentions.PerThreadMaxEffective(), b.logger)
	responder.SetPerThreadMaxFunc(func() int {
		return b.cfg.GitHub.Mentions.PerThreadMaxEffective()
	})
	b.agentMgr.SetKickObserver(responder.HandleAgentEvent)
	b.logger.Info("GitHub mention trigger poller started", "interval", b.cfg.GitHub.Mentions.PollIntervalEffective())
}

func (b *boot) stopMentionPoller() {
	if b == nil || b.mentionPollerCancel == nil {
		return
	}
	b.mentionPollerCancel()
	b.mentionPollerCancel = nil
	b.logger.Info("GitHub mention trigger poller stopped")
}

func (b *boot) mentionAgents() []mention.AgentInfo {
	out := make([]mention.AgentInfo, 0, len(b.cfg.Agents))
	for name, ac := range b.cfg.Agents {
		out = append(out, mention.AgentInfo{
			Name:         name,
			Enabled:      ac.Enabled,
			Converse:     ac.Converse != nil && *ac.Converse,
			Mention:      ac.HasEnabledChannel(config.ChannelTypeMention),
			GovernorKick: ac.UsesGovernorKick(),
		})
	}
	return out
}

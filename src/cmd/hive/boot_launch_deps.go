package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/discord"
)

// agentLaunchStagger is the pause between consecutive agent launches at
// startup so a fleet of agents does not all hit the model gateway at once.
const agentLaunchStagger = 15 * time.Second

// bootLaunchDeps are bootLaunch's effects (#7571, step 2): the goroutines
// (dashboard serve, staggered launch loop), the Discord bot, the stagger
// wait, the per-agent process start, and the pack on-demand lookup. The
// launch loop body itself runs for real so a test can drive it synchronously
// through spawn.
type bootLaunchDeps struct {
	spawn            func(name string, fn func())
	startDiscordBot  func(ctx context.Context, cfg discord.Config, agentNames []string, logger *slog.Logger) (func(string) error, error)
	onDemandFromPack func() map[string]bool
	// waitStagger blocks for the launch stagger; it returns false when ctx
	// ended first so the loop aborts instead of launching into a shutdown.
	waitStagger func(ctx context.Context) bool
	startAgent  func(ctx context.Context, mgr *agent.Manager, name string) error
	serve       func(srv *dashboard.Server) error
}

func defaultBootLaunchDeps() bootLaunchDeps {
	return bootLaunchDeps{
		spawn: func(_ string, fn func()) { go fn() },
		startDiscordBot: func(ctx context.Context, cfg discord.Config, agentNames []string, logger *slog.Logger) (func(string) error, error) {
			bot := discord.NewBot(cfg, logger)
			bot.SetAgentNames(agentNames)
			return bot.SendMessage, bot.Start(ctx)
		},
		onDemandFromPack: config.OnDemandAgentsFromPacks,
		waitStagger: func(ctx context.Context) bool {
			select {
			case <-time.After(agentLaunchStagger):
				return true
			case <-ctx.Done():
				return false
			}
		},
		startAgent: func(ctx context.Context, mgr *agent.Manager, name string) error { return mgr.Start(ctx, name) },
		serve:      (*dashboard.Server).Start,
	}
}

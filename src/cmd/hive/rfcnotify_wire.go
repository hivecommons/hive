package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rfcnotify"
)

// rfcNotifyStatePath persists the RFC lifecycle detector's per-issue
// records so a restart does not re-announce a transition.
const rfcNotifyStatePath = "/data/rfc-notify.json"

// rfcNotifyState is the process-lifetime half of the RFC lifecycle notices
// (notifications.discord.rfc_channel_id): the sweeper and the Discord
// poster, built once from the config that enabled them.
type rfcNotifyState struct {
	sweeper *rfcnotify.Sweeper
	post    rfcnotify.Poster
	// channel the poster was built for; a config change rebuilds it.
	channel string
	// statePath is rfcNotifyStatePath in production; tests point it at a
	// temp dir.
	statePath string
	// newPoster builds the Discord poster; nil means discordRFCPoster.
	// Tests inject a recorder.
	newPoster func(token, channel string) rfcnotify.Poster
}

// rfcNotifyState lazily builds the boot's notice state.
func (b *boot) rfcNotifyState() *rfcNotifyState {
	if b.rfcNotify == nil {
		b.rfcNotify = &rfcNotifyState{statePath: rfcNotifyStatePath}
	}
	return b.rfcNotify
}

// rfcNotifySettings returns the bot token and channel when the notices are
// configured, or ok=false when either is missing.
func rfcNotifySettings(cfg *config.Config) (token, channel string, ok bool) {
	if cfg == nil || cfg.Notifications.Discord == nil {
		return "", "", false
	}
	d := cfg.Notifications.Discord
	token, channel = strings.TrimSpace(d.BotToken), strings.TrimSpace(d.RFCChannelID)
	return token, channel, token != "" && channel != ""
}

// ensure (re)builds the sweeper and poster for the configured channel.
func (s *rfcNotifyState) ensure(cfg *config.Config, repos int, logger *slog.Logger) bool {
	token, channel, ok := rfcNotifySettings(cfg)
	if !ok {
		return false
	}
	newPoster := s.newPoster
	if newPoster == nil {
		newPoster = discordRFCPoster(logger)
	}
	if s.sweeper == nil {
		s.sweeper = rfcnotify.NewSweeper(rfcnotify.Options{
			StatePath:   s.statePath,
			IncludeRepo: repos > 1,
			Logger:      logger,
		})
	}
	if s.post == nil || s.channel != channel {
		s.post = newPoster(token, channel)
		s.channel = channel
	}
	return true
}

// discordRFCPoster posts through the bot token to the RFC channel without
// starting the command bot.
func discordRFCPoster(logger *slog.Logger) func(token, channel string) rfcnotify.Poster {
	return func(token, channel string) rfcnotify.Poster {
		bot := discord.NewBot(discord.Config{Token: token}, logger)
		return func(content string) error { return bot.SendMessageTo(channel, content) }
	}
}

// rfcNotifySource adapts the GitHub client to rfcnotify.Source.
type rfcNotifySource struct{ c *github.Client }

// newRFCNotifySource returns nil without a client, which turns the pass off.
func newRFCNotifySource(c *github.Client) rfcnotify.Source {
	if c == nil {
		return nil
	}
	return rfcNotifySource{c: c}
}

func (s rfcNotifySource) ListOpen(ctx context.Context) ([]rfcnotify.Issue, error) {
	snaps, err := s.c.ListIssueSnapshots(ctx, github.IssueSnapshotOptions{State: "open"})
	return rfcNotifyIssues(snaps), err
}

func (s rfcNotifySource) ListUpdatedSince(ctx context.Context, since time.Time) ([]rfcnotify.Issue, error) {
	snaps, err := s.c.ListIssueSnapshots(ctx, github.IssueSnapshotOptions{State: "all", Since: since})
	return rfcNotifyIssues(snaps), err
}

func rfcNotifyIssues(snaps []github.IssueSnapshot) []rfcnotify.Issue {
	out := make([]rfcnotify.Issue, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, rfcnotify.Issue{
			Repo: s.Repo, Number: s.Number, Title: s.Title, URL: s.URL, Author: s.Author,
			State: s.State, StateReason: s.StateReason, Labels: s.Labels, CreatedAt: s.CreatedAt,
		})
	}
	return out
}

// runRFCNotifySweep is the per-cycle RFC lifecycle pass
// (notifications.discord.rfc_channel_id, default off). The sweeper owns the
// cadence, so calling it every cycle costs nothing between polls; a poll is
// one list call per managed repo. Posts are audited per issue.
func runRFCNotifySweep(ctx context.Context, src rfcnotify.Source, repos int, cfg *config.Config, dashSrv *dashboard.Server, state *rfcNotifyState, logger *slog.Logger) rfcnotify.Result {
	if logger == nil {
		logger = slog.Default()
	}
	if src == nil || state == nil || !state.ensure(cfg, repos, logger) {
		return rfcnotify.Result{}
	}
	res, err := state.sweeper.Run(ctx, src, state.post)
	if err != nil {
		logger.Warn("rfc notify sweep failed", "error", err)
		return res
	}
	if res.Seeded {
		logger.Info("rfc notify: baseline recorded; transitions announce from the next poll", "channel", state.channel)
	}
	if dashSrv != nil {
		for i, ev := range res.Events {
			dashSrv.AuditLogRecord("system", "rfc-notify", fmt.Sprintf("channel=%s %s", state.channel, res.Posted[i]), "", ev.Issue.Repo, ev.Issue.Number)
		}
	}
	if len(res.Posted) > 0 || res.Errors > 0 {
		logger.Info("rfc notify sweep", "posted", len(res.Posted), "errors", res.Errors)
	}
	return res
}

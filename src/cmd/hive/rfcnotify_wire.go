package main

import (
	"context"
	"errors"
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

// rfcRepoVisibilityTTL bounds how long a resolved repo visibility is
// trusted before it is looked up again.
const rfcRepoVisibilityTTL = time.Hour

// rfcNotifyState is the process-lifetime half of the RFC lifecycle notices
// (notifications.discord.rfc_channel_id): the sweeper and the Discord
// poster, built once from the config that enabled them.
type rfcNotifyState struct {
	sweeper *rfcnotify.Sweeper
	post    rfcnotify.Poster
	// channel the poster was built for; a config change rebuilds it.
	channel string
	// gates the sweeper was built with; a config change rebuilds it.
	gates rfcNotifyGates
	// visibility caches each repo's resolved privacy, refreshed at most
	// every rfcRepoVisibilityTTL. Failed lookups are not cached.
	visibility map[string]rfcRepoVisibility
	// lookupPrivate overrides the GitHub visibility lookup in tests.
	lookupPrivate func(ctx context.Context, repo string) (bool, error)
	// now is the clock for the visibility cache; nil means time.Now.
	now func() time.Time
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

// rfcNotifyGates is the comparable part of rfcnotify.Options taken from
// the config (notifications.discord.rfc_*).
type rfcNotifyGates struct {
	includeRepo    bool
	format         string
	threads        bool
	announcer      bool
	backlink       bool
	includePrivate bool
}

func rfcNotifyGatesFor(cfg *config.Config, repos int) rfcNotifyGates {
	g := rfcNotifyGates{includeRepo: repos > 1, format: rfcnotify.FormatLine}
	if cfg == nil || cfg.Notifications.Discord == nil {
		return g
	}
	d := cfg.Notifications.Discord
	if d.RFCFormatMode() == config.RFCFormatCard {
		g.format = rfcnotify.FormatCard
	}
	g.threads, g.announcer, g.backlink, g.includePrivate = d.RFCThreads, d.RFCAnnouncer, d.RFCBacklinkComment, d.RFCIncludePrivate
	return g
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
	gates := rfcNotifyGatesFor(cfg, repos)
	if s.sweeper == nil || s.gates != gates {
		s.sweeper = rfcnotify.NewSweeper(rfcnotify.Options{
			StatePath:      s.statePath,
			IncludeRepo:    gates.includeRepo,
			Format:         gates.format,
			Threads:        gates.threads,
			Announcer:      gates.announcer,
			Backlink:       gates.backlink,
			IncludePrivate: gates.includePrivate,
			Logger:         logger,
		})
		s.gates = gates
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

type rfcRepoVisibility struct {
	private bool
	at      time.Time
}

// repoPrivate reports whether repo is private, from the cache when the
// entry is younger than rfcRepoVisibilityTTL. A failed lookup counts as
// private (fail closed) with a non-nil error, and is retried on the next
// call.
func (s *rfcNotifyState) repoPrivate(ctx context.Context, c *github.Client, repo string) (bool, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if v, ok := s.visibility[repo]; ok && now().Sub(v.at) < rfcRepoVisibilityTTL {
		return v.private, nil
	}
	lookup := s.lookupPrivate
	if lookup == nil {
		lookup = githubRepoPrivate(c)
	}
	private, err := lookup(ctx, repo)
	if err != nil {
		return true, err
	}
	if s.visibility == nil {
		s.visibility = map[string]rfcRepoVisibility{}
	}
	s.visibility[repo] = rfcRepoVisibility{private: private, at: now()}
	return private, nil
}

// githubRepoPrivate resolves an "owner/name" repo's visibility. Anything
// but a public repository counts as private.
func githubRepoPrivate(c *github.Client) func(ctx context.Context, repo string) (bool, error) {
	return func(ctx context.Context, repo string) (bool, error) {
		owner, name, ok := strings.Cut(repo, "/")
		if c == nil || !ok || owner == "" || name == "" {
			return true, fmt.Errorf("rfc notify: cannot resolve visibility of %q", repo)
		}
		r, _, err := c.GetRepo(ctx, owner, name)
		if err != nil {
			return true, err
		}
		if r == nil {
			return true, errors.New("rfc notify: empty repository response")
		}
		vis := strings.ToLower(strings.TrimSpace(r.GetVisibility()))
		return r.GetPrivate() || (vis != "" && vis != "public"), nil
	}
}

// rfcNotifySource adapts the GitHub client to rfcnotify.Source.
type rfcNotifySource struct {
	c     *github.Client
	state *rfcNotifyState
}

// newRFCNotifySource returns nil without a client, which turns the pass off.
// state caches repo visibility; nil marks every RFC private (fail closed).
func newRFCNotifySource(c *github.Client, state *rfcNotifyState) rfcnotify.Source {
	if c == nil {
		return nil
	}
	return rfcNotifySource{c: c, state: state}
}

func (s rfcNotifySource) ListOpen(ctx context.Context) ([]rfcnotify.Issue, error) {
	snaps, err := s.c.ListIssueSnapshots(ctx, github.IssueSnapshotOptions{State: "open"})
	return s.markPrivate(ctx, rfcNotifyIssues(snaps)), err
}

func (s rfcNotifySource) ListUpdatedSince(ctx context.Context, since time.Time) ([]rfcnotify.Issue, error) {
	snaps, err := s.c.ListIssueSnapshots(ctx, github.IssueSnapshotOptions{State: "all", Since: since})
	return s.markPrivate(ctx, rfcNotifyIssues(snaps)), err
}

// markPrivate sets Issue.Private on RFC issues from their repo's
// visibility; non-RFC issues are ignored by the detector and need no
// lookup.
func (s rfcNotifySource) markPrivate(ctx context.Context, issues []rfcnotify.Issue) []rfcnotify.Issue {
	for i := range issues {
		if !rfcnotify.IsRFC(issues[i].Title, issues[i].Labels) {
			continue
		}
		if s.state == nil {
			issues[i].Private = true
			continue
		}
		private, err := s.state.repoPrivate(ctx, s.c, issues[i].Repo)
		issues[i].Private = private
		issues[i].VisibilityUnknown = err != nil
	}
	return issues
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
	if len(res.Posted) > 0 || res.Errors > 0 || res.Withheld > 0 || res.Unresolved > 0 {
		logger.Info("rfc notify sweep", "posted", len(res.Posted), "errors", res.Errors, "withheld", res.Withheld, "unresolved", res.Unresolved)
	}
	return res
}

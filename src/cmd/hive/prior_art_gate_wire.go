package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/priorart"
	"github.com/hivecommons/hive/pkg/rfccoverage"
)

// Prior-art gate wiring (runs.triage.prior_art, hivecommons/hive#13575).
// The scheduler owns the park; this file builds the Searcher it asks and
// schedules the resume sweep that lifts the park.

// priorArtKnowledgeLimit bounds one knowledge search per issue.
const priorArtKnowledgeLimit = 8

// priorArtGateResumeSweepInterval spaces the resume passes, at the un-park
// sweep's cadence like the clarity gate.
const priorArtGateResumeSweepInterval = issueUnparkSweepInterval

// newPriorArtSearcher builds the gate's Searcher: knowledge beads, the
// dashboard Settings index (shared with the rfc_existing_check), and the
// forge (merged PRs, closed issues, docs, code) through the client provider
// so a swapped client is picked up (#9621).
// knowledgeAPI is resolved per search: bootKnowledge runs after
// bootDashboard, where the scheduler is wired, so the API is nil at wiring
// time; without one the knowledge source answers nothing.
func newPriorArtSearcher(cfg *config.Config, knowledgeAPI func() *knowledge.KnowledgeAPI, settings func() []rfccoverage.Setting, current func() *github.Client, logger *slog.Logger) *priorart.Searcher {
	var sources []priorart.Source
	if knowledgeAPI != nil {
		sources = append(sources, priorart.SourceFunc{SourceName: "knowledge", Fn: func(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
			api := knowledgeAPI()
			if api == nil {
				return nil, nil
			}
			return knowledgePriorArt(ctx, api, q)
		}})
	}
	if settings != nil {
		origin := gfiPublicOrigin(cfg)
		sources = append(sources, priorart.SourceFunc{SourceName: "settings", Fn: func(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
			return settingsPriorArt(origin, settings()), nil
		}})
	}
	if current != nil {
		sources = append(sources, liveForgePriorArtSources(current)...)
	}
	gate := cfg.Runs.Triage.PriorArt
	return priorart.NewSearcher(priorart.Options{
		Sources:     sources,
		MaxFindings: gate.EffectiveMaxFindings(),
		Logger:      logger,
	})
}

// knowledgePriorArt asks the knowledge layers for the issue's top terms.
// Deprecated and superseded facts are left out: pointing an author at a
// retired note would be worse than silence.
func knowledgePriorArt(ctx context.Context, api *knowledge.KnowledgeAPI, q priorart.Query) ([]priorart.Candidate, error) {
	query := strings.Join(q.TopTerms(4), " ")
	if query == "" {
		return nil, nil
	}
	facts := api.SearchAll(ctx, query, "", priorArtKnowledgeLimit)
	out := make([]priorart.Candidate, 0, len(facts))
	for _, f := range facts {
		switch f.EffectiveState() {
		case knowledge.StateDeprecated, knowledge.StateSuperseded:
			continue
		}
		out = append(out, priorart.Candidate{
			Kind:  priorart.KindKnowledge,
			Title: f.Title,
			Text:  f.Body,
			Ref:   f.Slug,
		})
	}
	return out, nil
}

// settingsPriorArt projects the Settings search index into candidates.
func settingsPriorArt(origin string, settings []rfccoverage.Setting) []priorart.Candidate {
	out := make([]priorart.Candidate, 0, len(settings))
	for _, s := range settings {
		if s.Key == "" {
			continue
		}
		url := rfccoverage.SettingsURL(origin, s)
		if url == "" {
			url = rfccoverage.DocsSearchURL(s.Key)
		}
		out = append(out, priorart.Candidate{
			Kind:  priorart.KindSetting,
			Title: strings.TrimSpace(s.Tab + " › " + s.Section + " › " + s.Label),
			Text:  s.Key + "\n" + s.Text,
			URL:   url,
			Ref:   s.Key,
		})
	}
	return out
}

// liveForgePriorArtSources resolves the GitHub client per search.
func liveForgePriorArtSources(current func() *github.Client) []priorart.Source {
	names := []string{"merged_prs", "closed_issues", "docs", "code"}
	out := make([]priorart.Source, 0, len(names))
	for i, name := range names {
		idx := i
		out = append(out, priorart.SourceFunc{SourceName: name, Fn: func(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
			c := current()
			if c == nil {
				return nil, github.ErrNoGitHubClient
			}
			srcs := c.PriorArtSources()
			if idx >= len(srcs) {
				return nil, nil
			}
			return srcs[idx].Search(ctx, q)
		}})
	}
	return out
}

// runPriorArtGateResumeSweepIfDue lifts stage/017-possible-existing once the
// author or a maintainer replies, closes it on `/hive decision exists`, and
// closes a clanker-filed park that nobody answered within bot_close_ttl.
// Every eligibility rule lives in SweepPriorArtGateResume; this is only the
// scheduler and the dashboard audit sink.
func runPriorArtGateResumeSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Runs.Triage.PriorArt.IsEnabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < priorArtGateResumeSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepPriorArtGateResume(ctx, github.PriorArtGateResumeOptions{
		MaxActions:  github.DefaultPriorArtGateResumeMaxActions,
		BotCloseTTL: cfg.Runs.Triage.PriorArt.EffectiveBotCloseTTL(),
		Audit: func(event github.PriorArtGateResumeEvent) {
			if dashSrv == nil {
				return
			}
			action := "prior-art-gate-resumed"
			switch {
			case event.AutoClosed:
				action = "prior-art-gate-autoclosed"
			case event.Closed:
				action = "prior-art-gate-closed-exists"
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s, decision=%d", event.Repo, event.Number, event.Actor, event.Decision)
			dashSrv.AuditLogRecord("system", action, detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("prior-art gate resume sweep failed", "error", err)
		return
	}
	if result != nil && (len(result.Resumed) > 0 || len(result.Closed) > 0 || result.Skipped > 0) {
		logger.Info("prior-art gate resume sweep complete", "seen", result.Seen, "resumed", len(result.Resumed), "closed", len(result.Closed), "skipped", result.Skipped)
	}
}

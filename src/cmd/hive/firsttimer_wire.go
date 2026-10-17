package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/firsttimer"
	"github.com/hivecommons/hive/pkg/github"
)

// firstTimerState is the process-lifetime half of the first-time contributor
// welcome (governor.first_timer_welcome.*): the sweeper's memory of who was
// welcomed, plus the operator's template read once from template_path.
type firstTimerState struct {
	sweeper *firsttimer.Sweeper
	mu      sync.Mutex
	// template is the override text ("" = default); loaded is true once
	// template_path was read (or found unset) so the file is read once.
	template string
	loaded   bool
}

// firstTimerState lazily builds the boot's welcome state.
func (b *boot) firstTimerState() *firstTimerState {
	if b.firstTimer == nil {
		b.firstTimer = newFirstTimerState()
	}
	return b.firstTimer
}

func newFirstTimerState() *firstTimerState {
	return &firstTimerState{sweeper: firsttimer.NewSweeper()}
}

// templateText returns the operator override, reading template_path on the
// first call. A file that cannot be read falls back to the default template
// (validation already rejected it at startup; this guards a file that
// vanished since).
func (s *firstTimerState) templateText(cfg config.FirstTimerWelcomeConfig, logger *slog.Logger) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.template
	}
	text, err := cfg.TemplateText()
	if err != nil && logger != nil {
		logger.Warn("first-timer welcome: template_path unreadable; using the default", "error", err)
	}
	s.template, s.loaded = text, true
	return s.template
}

// firstTimerCandidates projects the last enumeration's actionable PRs into
// the welcome's input. Held and exempt PRs are not in this list, so a PR
// parked as clanker-requested gets that notice and not this one.
func firstTimerCandidates(org string, result *github.ActionableResult) []firsttimer.Candidate {
	if result == nil {
		return nil
	}
	var out []firsttimer.Candidate
	for _, pr := range result.PRs.Items {
		if pr.Number <= 0 {
			continue
		}
		if pr.State != "" && !strings.EqualFold(pr.State, "open") {
			continue
		}
		out = append(out, firsttimer.Candidate{
			Repo: qualifyClaimRepo(org, pr.Repo), Number: pr.Number, Title: pr.Title, URL: pr.URL,
			Author: pr.Author, AuthorAssociation: pr.AuthorAssociation,
			AuthorNonHuman: pr.AuthorNonHuman || pr.AppAuthored, HiveAttributed: pr.HiveAttributed,
			Labels: pr.Labels, Draft: pr.Draft, CreatedAt: pr.CreatedAt,
		})
	}
	return out
}

// firstTimerKnownContributor reports whether login already runs a hive
// against this one: a live relay claim or a contributor-registry profile.
func (b *boot) firstTimerKnownContributor(login string) bool {
	if b.relayContributor(login) {
		return true
	}
	_, ok := dashboard.ContributorTrustTierByLogin(login)
	return ok
}

// firstTimerSweepOptions builds the per-call options.
func firstTimerSweepOptions(cfg *config.Config, state *firstTimerState, dashSrv *dashboard.Server, known func(string) bool, logger *slog.Logger) firsttimer.SweepOptions {
	fc := cfg.Governor.FirstTimerWelcome
	opts := firsttimer.SweepOptions{
		Links:            firsttimer.Links{Origin: gfiPublicOrigin(cfg), HiveID: cfg.HiveID, DocsBase: cfg.GoodFirstIssue.DocsURL},
		Template:         state.templateText(fc, logger),
		Logger:           logger,
		KnownContributor: known,
	}
	if dashSrv != nil {
		opts.Audit = func(e firsttimer.Event) {
			dashSrv.AuditLogRecord("system", "first-timer-"+e.Action, fmt.Sprintf("repo=%s, pr=%d, author=%s", e.Repo, e.Number, e.Author), "", e.Repo, e.Number)
		}
	}
	return opts
}

// runFirstTimerWelcome is the per-cycle welcome pass
// (governor.first_timer_welcome.enabled, default on). It reads the last
// enumeration rather than re-listing PRs, so it costs GitHub calls only
// for a first-time human author it has not seen: one comment scan, one
// search for their other PRs, one comment.
func runFirstTimerWelcome(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, state *firstTimerState, known func(string) bool, last *github.ActionableResult, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || state == nil || last == nil || !cfg.Governor.FirstTimerWelcome.IsEnabled() {
		return
	}
	candidates := firstTimerCandidates(cfg.Project.Org, last)
	if len(candidates) == 0 {
		return
	}
	opts := firstTimerSweepOptions(cfg, state, dashSrv, known, logger)
	if opts.Links.Origin == "" {
		logger.Debug("first-timer welcome skip", "reason", "no-public-url", "hint", "set dashboard.public_url so the welcome can link this hive's contribute page")
		return
	}
	res := state.sweeper.Sweep(ctx, ghClient.FirstTimerForge(), candidates, opts)
	if len(res.Welcomed) > 0 || res.Errors > 0 {
		logger.Info("first-timer welcome sweep", "scanned", res.Scanned, "welcomed", res.Welcomed, "errors", res.Errors, "skipped", fmt.Sprint(res.Skipped))
	}
}

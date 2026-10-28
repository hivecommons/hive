package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/github"
)

// gfiReleaseTimeout bounds the GitHub calls a release makes from a hook.
const gfiReleaseTimeout = 30 * time.Second

// gfiState is the process-lifetime half of good-first-issue curation: the
// sweeper's memory plus the last enumeration's labels, so a hook can tell
// cheaply whether an issue wears the label before spending API calls.
type gfiState struct {
	sweeper *gfi.Sweeper
	mu      sync.Mutex
	// labelled is the set of owner/repo#n carrying gfi.Label in the last
	// sweep's candidate set.
	labelled map[string]bool
	// agentsMD caches the AGENTS.md URL (or "") per owner/repo.
	agentsMD map[string]string
}

// gfiState lazily builds the boot's curation state; hooks may be installed
// before the run loop first sweeps.
func (b *boot) gfiState() *gfiState {
	if b.gfi == nil {
		b.gfi = newGFIState()
	}
	return b.gfi
}

func newGFIState() *gfiState {
	return &gfiState{sweeper: gfi.NewSweeper(), labelled: map[string]bool{}, agentsMD: map[string]string{}}
}

// known reports whether key is labelled per the last sweep, or was selected
// by this process.
func (g *gfiState) known(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.labelled[key] || g.sweeper.Selected(key)
}

func (g *gfiState) rememberLabelled(set map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labelled = set
}

// agentsMDURL returns the repository's AGENTS.md link when it has one.
func (g *gfiState) agentsMDURL(ctx context.Context, gh *github.Client, repo string) string {
	g.mu.Lock()
	url, ok := g.agentsMD[repo]
	g.mu.Unlock()
	if ok {
		return url
	}
	url = ""
	if owner, name, found := strings.Cut(repo, "/"); found && gh != nil {
		if _, err := gh.GetFileContent(ctx, owner, name, "AGENTS.md"); err == nil {
			url = "https://github.com/" + repo + "/blob/HEAD/AGENTS.md"
		}
	}
	g.mu.Lock()
	g.agentsMD[repo] = url
	g.mu.Unlock()
	return url
}

// gfiPublicOrigin is the externally reachable dashboard origin the welcome
// comment links: dashboard.public_url, else the hub-assigned dashboard URL.
// Empty when neither is known; the sweep then skips rather than publish a
// link nobody can follow.
func gfiPublicOrigin(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	for _, u := range []string{cfg.Dashboard.PublicURL, cfg.Hub.DashboardURL} {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			return u
		}
	}
	return ""
}

// gfiSweepOptions builds the per-call options shared by the sweep and hooks.
func gfiSweepOptions(cfg *config.Config, dashSrv *dashboard.Server, logger *slog.Logger) gfi.SweepOptions {
	gc := cfg.GoodFirstIssue
	opts := gfi.SweepOptions{
		Policy:   gc.Policy(github.HardSuppressIssueLabels()),
		Links:    gfi.Links{Origin: gfiPublicOrigin(cfg), HiveID: cfg.HiveID, DocsBase: gc.DocsURL},
		Template: gc.CommentTemplate,
		Logger:   logger,
	}
	if dashSrv != nil {
		opts.Audit = func(e gfi.Event) {
			detail := fmt.Sprintf("repo=%s, issue=%d", e.Repo, e.Number)
			if len(e.Reasons) > 0 {
				detail += ", reasons=" + strings.Join(e.Reasons, "; ")
			}
			if e.Reason != "" {
				detail += ", reason=" + e.Reason
			}
			dashSrv.AuditLogRecord("system", "gfi-"+e.Action, detail, "", e.Repo, e.Number)
		}
	}
	return opts
}

// gfiCandidates projects the last enumeration's actionable issues — already
// filtered of hard-suppressed, claimed and PR-covered items — into scorer
// input, and returns the set currently wearing the label.
func gfiCandidates(org string, result *github.ActionableResult, allowed func(string) bool) ([]gfi.Candidate, map[string]bool) {
	labelled := map[string]bool{}
	if result == nil {
		return nil, labelled
	}
	var out []gfi.Candidate
	for _, is := range result.Issues.Items {
		if is.Number <= 0 || is.SourceType != "" && is.SourceType != "github" {
			continue
		}
		repo := qualifyClaimRepo(org, is.Repo)
		if allowed != nil && !allowed(repo) {
			continue
		}
		c := gfi.Candidate{
			Repo: repo, Number: is.Number, Title: is.Title, Body: is.Body, URL: is.URL,
			Labels: is.Labels, Assignees: is.Assignees, CreatedAt: is.CreatedAt,
		}
		if is.State != "" && !strings.EqualFold(is.State, "open") {
			continue
		}
		if gfi.HasLabel(is.Labels) {
			labelled[c.Key()] = true
		}
		out = append(out, c)
	}
	// Issues the enumeration filtered out (blocked, held, parked on a
	// person) that still wear the label are handed to the sweep so it can
	// withdraw the advertisement (hivecommons/hive#13267); the labels alone
	// decide, so the missing body is harmless.
	for _, repo := range sortedRepoKeys(result.WorkDetailsByRepo) {
		for _, row := range result.WorkDetailsByRepo[repo].Issues {
			if row.Number <= 0 || row.Bucket != "filtered" || !gfi.HasLabel(row.Labels) {
				continue
			}
			full := qualifyClaimRepo(org, row.Repo)
			if allowed != nil && !allowed(full) {
				continue
			}
			c := gfi.Candidate{Repo: full, Number: row.Number, Title: row.Title, URL: row.URL, Labels: row.Labels, Assignees: row.Assignees, CreatedAt: row.CreatedAt}
			if row.Reason == gfi.SkipTracking {
				// The row has no body; carry the enumeration's verdict as the
				// marker so Score skips it as tracking and the sweep withdraws
				// the label (hivecommons/hive#14613).
				c.Body = gfi.TrackingMarker
			}
			if labelled[c.Key()] {
				continue
			}
			labelled[c.Key()] = true
			out = append(out, c)
		}
	}
	return out, labelled
}

func sortedRepoKeys(m map[string]github.RepoWorkDetails) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runGoodFirstIssueSweepIfDue is the periodic curation pass
// (good_first_issue.enabled, default on), once per
// good_first_issue.interval. It reads the last enumeration rather than
// re-listing issues, so it costs GitHub calls only for the few it labels.
// The labelled set is refreshed from every enumeration, not just the ones a
// sweep runs on, so an issue a maintainer labelled by hand between sweeps is
// still released the moment a hive agent starts it.
func runGoodFirstIssueSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, state *gfiState, last *github.ActionableResult, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || state == nil || !cfg.GoodFirstIssue.IsEnabled() || last == nil {
		return
	}
	candidates, labelled := gfiCandidates(cfg.Project.Org, last, cfg.GoodFirstIssue.RepoAllowed)
	state.rememberLabelled(labelled)
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < cfg.GoodFirstIssue.IntervalOrDefault() {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	opts := gfiSweepOptions(cfg, dashSrv, logger)
	opts.Now = now
	if opts.Links.Origin == "" {
		logger.Warn("gfi skip", "reason", "no-public-url", "hint", "set dashboard.public_url so the welcome comment can link this hive's relay")
		return
	}
	opts.AgentsMD = func(ctx context.Context, repo string) string { return state.agentsMDURL(ctx, ghClient, repo) }
	res := state.sweeper.Sweep(ctx, ghClient, candidates, opts)
	logger.Info("gfi sweep", "scanned", res.Scanned, "selected", len(res.Selected), "withdrawn", len(res.Withdrawn), "errors", res.Errors, "skipped", fmt.Sprint(res.Skipped))
}

// gfiReleaseOnHiveStart withdraws the advertisement when a hive agent starts
// on a labelled issue. Installed behind the agent-start hook, which fires
// whether or not the claims ledger is on.
func gfiReleaseOnHiveStart(ctx context.Context, cfg *config.Config, state *gfiState, client func() *github.Client, dashSrv *dashboard.Server, logger *slog.Logger) github.AgentStartHook {
	return func(agent, repo string, number int, signal string) {
		if cfg == nil || state == nil || !cfg.GoodFirstIssue.IsEnabled() {
			return
		}
		full := qualifyClaimRepo(cfg.Project.Org, repo)
		c := gfi.Candidate{Repo: full, Number: number}
		if !state.known(c.Key()) {
			return
		}
		gh := client()
		if gh == nil {
			return
		}
		go func() {
			cctx, cancel := context.WithTimeout(ctx, gfiReleaseTimeout)
			defer cancel()
			if err := state.sweeper.Release(cctx, gh, c, gfi.ReasonClaimedByHive, gfiSweepOptions(cfg, dashSrv, logger)); err != nil {
				logger.Warn("gfi: release failed", "issue", c.Key(), "agent", agent, "signal", signal, "error", err)
			}
			state.mu.Lock()
			delete(state.labelled, c.Key())
			state.mu.Unlock()
		}()
	}
}

// gfiClaimHooks chains conversion counting and hive-release onto the claims
// ledger hooks: a contributor or human claim on a labelled issue is the
// outcome the label exists for; an agent claim withdraws it.
func gfiClaimHooks(ctx context.Context, cfg *config.Config, state *gfiState, client func() *github.Client, dashSrv *dashboard.Server, logger *slog.Logger, base claims.Hooks) claims.Hooks {
	prev := base.OnClaimed
	release := gfiReleaseOnHiveStart(ctx, cfg, state, client, dashSrv, logger)
	base.OnClaimed = func(c claims.Claim, outcome claims.Outcome) {
		if prev != nil {
			prev(c, outcome)
		}
		if cfg == nil || state == nil || !cfg.GoodFirstIssue.IsEnabled() || outcome != claims.OutcomeClaimed {
			return
		}
		cand := gfi.Candidate{Repo: c.Repo, Number: c.Issue}
		switch c.Kind {
		case claims.KindContributor, claims.KindHuman:
			if state.known(cand.Key()) {
				state.sweeper.Convert(cand, c.Holder, string(c.Kind), gfiSweepOptions(cfg, dashSrv, logger))
			}
		case claims.KindAgent:
			release(c.Holder, c.Repo, c.Issue, "ledger")
		}
	}
	return base
}

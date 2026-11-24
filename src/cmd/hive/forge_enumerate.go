package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// Work discovery for a forge-only hive (hivecommons/hive#19669, #19494 item 3).
//
// A hive whose repos all live on GitLab, Gitea or Forgejo boots with no GitHub
// client (#19497), so github.Client.EnumerateActionable has nothing to call.
// enumerateForgeActionable lists each active repo through its forge.Forge
// adapter and builds the same github.ActionableResult the GitHub path builds,
// so the governor, scheduler and dashboard downstream run unchanged.
//
// Only gates that need nothing beyond the list payload are applied: hold,
// exempt and hard-suppress labels, project.issue_filter, and reporter trust.
// The GitHub-only enrichments (CI status, mergeability, review signals,
// dependency links) are not available, so those fields stay at their
// "unknown" zero values.
//
// GitHub hives, and hives that mix GitHub and other forges, never reach this
// file: the eval cycle calls it only when config.AllReposOffGitHub is true.

// forgeLister is the read half of forge.Forge that work discovery needs.
type forgeLister interface {
	ListOpenIssues(ctx context.Context, repo string) ([]forge.Issue, error)
	ListOpenChangeRequests(ctx context.Context, repo string) ([]forge.ChangeRequest, error)
}

// errForgeNotOffGitHub is returned by forgeListerForRepo for a repo that
// resolves to GitHub; the forge-only enumeration must never be handed one.
var errForgeNotOffGitHub = errors.New("forge enumeration: repo is on GitHub")

// forgeListerForRepo builds the read adapter for a repo on a non-GitHub forge,
// from the same config forgeForRepo and forgeMergerForRepo read: the instance
// URL and the env var named by the block's token_env. A token goes only to its
// own block's host (CheckForgeTokenHost). It is a variable so tests can
// substitute a fake.
var forgeListerForRepo = func(cfg *config.Config, repo string) (forgeLister, error) {
	var kind forge.Kind
	var tokenID string
	switch cfg.ForgeKindForRepo(repo) {
	case config.ForgeGitLab:
		kind, tokenID = forge.KindGitLab, cfg.GitLab.TokenEnvName()
	case config.ForgeGitea:
		kind, tokenID = forge.KindGitea, cfg.Gitea.TokenEnvName()
	case config.ForgeForgejo:
		kind, tokenID = forge.KindForgejo, cfg.Forgejo.TokenEnvName()
	default:
		return nil, errForgeNotOffGitHub
	}
	if err := cfg.CheckForgeTokenHost(repo); err != nil {
		return nil, err
	}
	return forge.NewForge(kind, os.Getenv(tokenID), forge.Options{
		BaseURL: cfg.ForgeURLForRepo(repo),
		Org:     cfg.Project.Org,
	})
}

// holdLabelsForConfig is the hive-specific hold label set the GitHub client is
// configured with (boot.githubHoldLabels): the hive's own pause label, the
// sentinel alert label and the reporter-trust parking labels.
func holdLabelsForConfig(cfg *config.Config) []string {
	labels := []string{github.CanonicalHiveHoldLabel(cfg.HiveID), cfg.Sentinel.LabelOrDefault()}
	return append(labels, cfg.Project.IssueFilter.ReporterTrust.ExtraHoldLabels()...)
}

// forgeEnumerationRepos is the configured repos the forge enumeration walks: a
// paused repo produces no work, as on GitHub (#6203).
func forgeEnumerationRepos(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if cfg.IsRepoPaused(repo) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// forgeIssueGate holds the label and filter config that classifyIssue applies.
type forgeIssueGate struct {
	holdExtra []string
	exempt    []string
	filter    config.IssueFilterConfig
}

const (
	forgeGateHold           = "hold"
	forgeGateExempt         = "exempt"
	forgeGateReporterTriage = "reporter-triage"
	forgeGateIssueFilter    = "issue-filter"
)

func newForgeIssueGate(cfg *config.Config) forgeIssueGate {
	return forgeIssueGate{
		holdExtra: holdLabelsForConfig(cfg),
		exempt:    cfg.Governor.Labels.Exempt,
		filter:    cfg.Project.IssueFilter,
	}
}

// classifyIssue returns "" for an actionable issue, forgeGateHold for a held
// one, and otherwise the reason it is filtered out.
func (g forgeIssueGate) classifyIssue(author string, labels []string) string {
	if github.HasHoldLabelWith(labels, g.holdExtra) {
		return forgeGateHold
	}
	if github.HasExemptLabel(labels, g.exempt) {
		return forgeGateExempt
	}
	if bucket := g.filter.HardSuppressIssueBucket(labels); bucket != "" {
		return bucket
	}
	// The forge list payload carries no author association, so only
	// reporter_trust.trusted_logins can trust an author here: any other
	// reporter's issue needs an untrusted-require label. This fails closed.
	if g.filter.ReporterTrustEnabled() && !g.filter.AdmitsReporter(labels, author, "") {
		return forgeGateReporterTriage
	}
	if !g.filter.Admits(labels) {
		return forgeGateIssueFilter
	}
	return ""
}

// forgeRepoEnumeration is one repo's share of a forge enumeration pass.
type forgeRepoEnumeration struct {
	issues  []github.Issue
	prs     []github.PullRequest
	held    []github.HoldItem
	heldPRs []github.PullRequest
	counts  github.RepoCounts
}

func enumerateForgeRepo(ctx context.Context, lister forgeLister, repo string, gate forgeIssueGate, now time.Time) (forgeRepoEnumeration, error) {
	var enum forgeRepoEnumeration
	issues, err := lister.ListOpenIssues(ctx, repo)
	if err != nil {
		return enum, fmt.Errorf("listing issues for %s: %w", repo, err)
	}
	crs, err := lister.ListOpenChangeRequests(ctx, repo)
	if err != nil {
		return enum, fmt.Errorf("listing change requests for %s: %w", repo, err)
	}
	enum.counts = github.RepoCounts{Issues: len(issues), PRs: len(crs)}

	for _, is := range issues {
		switch gate.classifyIssue(is.Author, is.Labels) {
		case "":
			enum.issues = append(enum.issues, github.Issue{
				Repo:   repo,
				Number: is.Number,
				Title:  is.Title,
				Body:   is.Body,
				Author: is.Author,
				// The forge payload cannot tell a person from a bot, so every
				// author is treated as a person: the injection scan and the
				// clarity gate then judge the text instead of trusting it.
				AuthorIsHuman: true,
				Labels:        is.Labels,
				Assignees:     is.Assignees,
				State:         is.State,
				CreatedAt:     is.CreatedAt,
				UpdatedAt:     is.UpdatedAt,
				AgeMinutes:    int(now.Sub(is.CreatedAt).Minutes()),
				URL:           is.URL,
			})
		case forgeGateHold:
			enum.held = append(enum.held, github.HoldItem{
				Number:    is.Number,
				Repo:      repo,
				Title:     is.Title,
				Type:      "issue",
				CreatedAt: is.CreatedAt,
				URL:       is.URL,
				Labels:    is.Labels,
				Assignees: is.Assignees,
			})
		}
	}

	for _, cr := range crs {
		// A draft is someone's work in progress and is not offered, as on
		// GitHub.
		if cr.Draft {
			continue
		}
		pr := github.PullRequest{
			Repo:        repo,
			Number:      cr.Number,
			Title:       cr.Title,
			Author:      cr.Author,
			Labels:      cr.Labels,
			CreatedAt:   cr.CreatedAt,
			State:       cr.State,
			URL:         cr.URL,
			HeadSHA:     cr.HeadSHA,
			HeadRef:     cr.SourceBranch,
			BaseRef:     cr.TargetBranch,
			ReviewClass: github.ClassifyReviewClass(cr.Title, cr.Labels),
		}
		if github.HasHoldLabelWith(cr.Labels, gate.holdExtra) {
			enum.held = append(enum.held, github.HoldItem{
				Number:      cr.Number,
				Repo:        repo,
				Title:       cr.Title,
				Type:        "pr",
				HeadSHA:     cr.HeadSHA,
				Author:      cr.Author,
				CreatedAt:   cr.CreatedAt,
				ReviewClass: pr.ReviewClass,
				URL:         cr.URL,
				Labels:      cr.Labels,
			})
			enum.heldPRs = append(enum.heldPRs, pr)
			continue
		}
		if github.HasExemptLabel(cr.Labels, gate.exempt) {
			continue
		}
		enum.prs = append(enum.prs, pr)
	}
	return enum, nil
}

// enumerateForgeActionable is EnumerateActionable for a forge-only hive. A
// repo whose listing fails is reported with a zero count and marked stale; if
// every repo fails the pass returns an error so the cycle keeps its prior
// state instead of reporting an empty queue, the same rule the GitHub path
// follows.
func enumerateForgeActionable(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*github.ActionableResult, error) {
	now := time.Now()
	result := &github.ActionableResult{
		GeneratedAt: now,
		TotalByRepo: map[string]github.RepoCounts{},
	}
	gate := newForgeIssueGate(cfg)
	repos := forgeEnumerationRepos(cfg)

	var (
		issues  []github.Issue
		prs     []github.PullRequest
		held    []github.HoldItem
		heldPRs []github.PullRequest
		failed  int
		lastErr error
	)
	for _, repo := range repos {
		lister, err := forgeListerForRepo(cfg, repo)
		if err == nil {
			var enum forgeRepoEnumeration
			enum, err = enumerateForgeRepo(ctx, lister, repo, gate, now)
			if err == nil {
				issues = append(issues, enum.issues...)
				prs = append(prs, enum.prs...)
				held = append(held, enum.held...)
				heldPRs = append(heldPRs, enum.heldPRs...)
				result.TotalByRepo[repo] = enum.counts
				continue
			}
		}
		failed++
		lastErr = err
		result.TotalByRepo[repo] = github.RepoCounts{}
		result.StaleRepos = append(result.StaleRepos, repo)
		logger.Warn("forge enumeration failed for repo; skipping it this pass (marked stale)",
			"repo", repo, "forge", cfg.ForgeKindForRepo(repo), "error", err)
	}
	if len(repos) > 0 && failed >= len(repos) {
		return nil, fmt.Errorf("all %d repos failed to enumerate on their forge (last error: %w)", len(repos), lastErr)
	}
	if len(result.StaleRepos) > 0 {
		// Nothing was served from a last-good copy: the stale repos have no
		// data at all, current as of this pass.
		result.StaleSince = now
	}

	github.RankActionableIssues(issues)
	github.SortPullRequestsForReview(prs)
	github.SortPullRequestsForReview(heldPRs)
	github.SortHoldItemsForReview(held)

	holdIssues := 0
	for _, h := range held {
		if h.Type == "issue" {
			holdIssues++
		}
	}
	result.Issues = github.IssueResultFromItems(issues)
	result.PRs = github.PRResult{Count: len(prs), Items: prs, Held: heldPRs}
	result.Hold = github.HoldResult{
		Issues: holdIssues,
		PRs:    len(held) - holdIssues,
		Total:  len(held),
		Items:  held,
	}
	return result, nil
}

// enumerateActionableForCycle is the eval cycle's enumeration step. A hive
// with any repo on GitHub takes the GitHub client path exactly as before; only
// a forge-only hive enumerates through pkg/forge.
func enumerateActionableForCycle(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger) (*github.ActionableResult, error) {
	if cfg.AllReposOffGitHub() {
		return enumerateForgeActionable(ctx, cfg, logger)
	}
	return ghClient.EnumerateActionable(ctx)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
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
// A GitHub-only hive never reaches this file. A hive that mixes GitHub and
// another forge (hivecommons/hive#19703) enumerates through the GitHub client
// first; mergeForgeIntoGitHubResult then replaces each non-GitHub repo's share
// of that result with the share its own forge reports.

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
	var gov cruxconfig.TokenSource
	switch cfg.ForgeKindForRepo(repo) {
	case config.ForgeGitLab:
		kind, gov = forge.KindGitLab, cfg.GitLab.Credentials().Governor()
	case config.ForgeGitea:
		kind, gov = forge.KindGitea, cfg.Gitea.Credentials().Governor()
	case config.ForgeForgejo:
		kind, gov = forge.KindForgejo, cfg.Forgejo.Credentials().Governor()
	default:
		return nil, errForgeNotOffGitHub
	}
	if err := cfg.CheckForgeTokenHost(repo); err != nil {
		return nil, err
	}
	token, _, err := gov.Resolve()
	if err != nil {
		return nil, fmt.Errorf("forge %s: %w", kind, err)
	}
	return forge.NewForge(kind, token, forge.Options{
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
	repos := forgeEnumerationRepos(cfg)
	result, failed, lastErr := enumerateForgeRepos(ctx, cfg, repos, logger)
	if len(repos) > 0 && failed >= len(repos) {
		return nil, fmt.Errorf("all %d repos failed to enumerate on their forge (last error: %w)", len(repos), lastErr)
	}
	return result, nil
}

// enumerateForgeRepos lists repos through their forge adapters. A repo whose
// listing fails gets a zero count and is marked stale; the caller decides
// what a pass in which every repo failed means. It returns the number of
// failed repos and the last failure.
func enumerateForgeRepos(ctx context.Context, cfg *config.Config, repos []string, logger *slog.Logger) (*github.ActionableResult, int, error) {
	now := time.Now()
	result := &github.ActionableResult{
		GeneratedAt: now,
		TotalByRepo: map[string]github.RepoCounts{},
	}
	gate := newForgeIssueGate(cfg)

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
	if len(result.StaleRepos) > 0 {
		// Nothing was served from a last-good copy: the stale repos have no
		// data at all, current as of this pass.
		result.StaleSince = now
	}

	github.RankActionableIssues(issues)
	github.SortPullRequestsForReview(prs)
	github.SortPullRequestsForReview(heldPRs)
	github.SortHoldItemsForReview(held)

	result.Issues = github.IssueResultFromItems(issues)
	result.PRs = github.PRResult{Count: len(prs), Items: prs, Held: heldPRs}
	setHoldItems(result, held)
	return result, failed, lastErr
}

// enumerateActionableForCycle is the eval cycle's enumeration step. A
// GitHub-only hive takes the GitHub client path exactly as before; a
// forge-only hive enumerates through pkg/forge; a mixed hive enumerates
// through the GitHub client and then serves its non-GitHub repos from their
// own forge.
func enumerateActionableForCycle(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger) (*github.ActionableResult, error) {
	if cfg.AllReposOffGitHub() {
		return enumerateForgeActionable(ctx, cfg, logger)
	}
	result, err := ghClient.EnumerateActionable(ctx)
	if err != nil || !hiveMixesForges(cfg) {
		return result, err
	}
	return mergeForgeIntoGitHubResult(ctx, cfg, result, logger), nil
}

// hiveMixesForges reports whether the hive has repos on GitHub and at least one
// configured repo on another forge. It is false for a GitHub-only hive, so
// every mixed-hive step below leaves such a hive untouched.
func hiveMixesForges(cfg *config.Config) bool {
	if cfg == nil || cfg.AllReposOffGitHub() {
		return false
	}
	for _, repo := range cfg.Project.Repos {
		if !cfg.RepoOnGitHub(repo) {
			return true
		}
	}
	return false
}

// mergeForgeIntoGitHubResult replaces every non-GitHub repo's share of a
// mixed hive's GitHub enumeration with the share its own forge reports. The
// GitHub client lists every configured repo, so a Forgejo, Gitea or GitLab
// repo comes back empty, failed or, worse, as a same-named GitHub repo; that
// share is dropped first. A forge repo whose listing fails is reported with a
// zero count and marked stale, as on a forge-only hive; the GitHub share of
// the pass is kept either way.
func mergeForgeIntoGitHubResult(ctx context.Context, cfg *config.Config, result *github.ActionableResult, logger *slog.Logger) *github.ActionableResult {
	if result == nil {
		return nil
	}
	dropOffGitHubRepos(cfg, result)

	var repos []string
	for _, repo := range forgeEnumerationRepos(cfg) {
		if !cfg.RepoOnGitHub(repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return result
	}
	forgeResult, _, _ := enumerateForgeRepos(ctx, cfg, repos, logger)

	issues := append(result.Issues.Items, forgeResult.Issues.Items...)
	prs := append(result.PRs.Items, forgeResult.PRs.Items...)
	heldPRs := append(result.PRs.Held, forgeResult.PRs.Held...)
	held := append(result.Hold.Items, forgeResult.Hold.Items...)
	github.RankActionableIssues(issues)
	github.SortPullRequestsForReview(prs)
	github.SortPullRequestsForReview(heldPRs)
	github.SortHoldItemsForReview(held)

	result.Issues = github.IssueResultFromItems(issues)
	result.PRs.Count, result.PRs.Items, result.PRs.Held = len(prs), prs, heldPRs
	setHoldItems(result, held)

	if result.TotalByRepo == nil {
		result.TotalByRepo = map[string]github.RepoCounts{}
	}
	for repo, counts := range forgeResult.TotalByRepo {
		result.TotalByRepo[repo] = counts
	}
	if len(forgeResult.StaleRepos) > 0 {
		result.StaleRepos = append(result.StaleRepos, forgeResult.StaleRepos...)
		if result.StaleSince.IsZero() || forgeResult.StaleSince.Before(result.StaleSince) {
			result.StaleSince = forgeResult.StaleSince
		}
	}
	return result
}

// dropOffGitHubRepos removes every item, count and stale mark the GitHub
// client reported for a repo that lives on another forge.
func dropOffGitHubRepos(cfg *config.Config, result *github.ActionableResult) {
	onGitHub := cfg.RepoOnGitHub
	result.Issues = github.IssueResultFromItems(keepGitHubItems(result.Issues.Items, func(i github.Issue) string { return i.Repo }, onGitHub))
	prRepo := func(pr github.PullRequest) string { return pr.Repo }
	result.PRs.Items = keepGitHubItems(result.PRs.Items, prRepo, onGitHub)
	result.PRs.Count = len(result.PRs.Items)
	result.PRs.Held = keepGitHubItems(result.PRs.Held, prRepo, onGitHub)
	result.PRs.Attributed = keepGitHubItems(result.PRs.Attributed, prRepo, onGitHub)
	result.PRs.StaleDrafts = keepGitHubItems(result.PRs.StaleDrafts, prRepo, onGitHub)
	setHoldItems(result, keepGitHubItems(result.Hold.Items, func(h github.HoldItem) string { return h.Repo }, onGitHub))

	for repo := range result.TotalByRepo {
		if !onGitHub(repo) {
			delete(result.TotalByRepo, repo)
		}
	}
	for repo := range result.WorkBreakdownByRepo {
		if !onGitHub(repo) {
			delete(result.WorkBreakdownByRepo, repo)
		}
	}
	for repo := range result.WorkDetailsByRepo {
		if !onGitHub(repo) {
			delete(result.WorkDetailsByRepo, repo)
		}
	}
	patchRepoOnGitHub := func(key string) bool {
		repo, _, _ := strings.Cut(key, "#")
		return onGitHub(repo)
	}
	for key := range result.ItemPatches {
		if !patchRepoOnGitHub(key) {
			delete(result.ItemPatches, key)
		}
	}
	for key := range result.WebhookPatches {
		if !patchRepoOnGitHub(key) {
			delete(result.WebhookPatches, key)
		}
	}
	if len(result.StaleRepos) > 0 {
		result.StaleRepos = keepGitHubItems(result.StaleRepos, func(r string) string { return r }, onGitHub)
		if len(result.StaleRepos) == 0 {
			// Only the dropped repos were stale; the GitHub share is current.
			result.StaleRepos = nil
			result.StaleSince = time.Time{}
		}
	}
}

// keepGitHubItems returns the items whose repo is on GitHub, preserving order.
// A nil or empty input is returned as is.
func keepGitHubItems[T any](items []T, repoOf func(T) string, onGitHub func(string) bool) []T {
	if len(items) == 0 {
		return items
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if onGitHub(repoOf(it)) {
			out = append(out, it)
		}
	}
	return out
}

// setHoldItems stores held items and recomputes the hold counts from them.
func setHoldItems(result *github.ActionableResult, held []github.HoldItem) {
	issues := 0
	for _, h := range held {
		if h.Type == "issue" {
			issues++
		}
	}
	result.Hold = github.HoldResult{
		Issues: issues,
		PRs:    len(held) - issues,
		Total:  len(held),
		Items:  held,
	}
}

// enrichGitHubPRs runs a GitHub enrichment step (CI status, workflow
// approvals) on the PRs that live on GitHub. On a GitHub-only hive it hands
// enrich the slice itself, exactly as before. On a mixed hive it passes only
// the GitHub PRs and writes the enriched copies back in place, so a PR on
// another forge keeps its "unknown" CI and mergeability instead of a GitHub
// lookup of a repo that is not there.
func enrichGitHubPRs(cfg *config.Config, prs []github.PullRequest, enrich func([]github.PullRequest)) {
	if !hiveMixesForges(cfg) {
		enrich(prs)
		return
	}
	idx := make([]int, 0, len(prs))
	sub := make([]github.PullRequest, 0, len(prs))
	for i, pr := range prs {
		if cfg.RepoOnGitHub(pr.Repo) {
			idx = append(idx, i)
			sub = append(sub, pr)
		}
	}
	if len(sub) == 0 {
		return
	}
	enrich(sub)
	for j, i := range idx {
		prs[i] = sub[j]
	}
}

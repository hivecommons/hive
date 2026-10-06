package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	metricsCollectInterval = 5 * time.Minute
	httpTimeout            = 10 * time.Second
	metricsCacheFile       = "/data/metrics/agent-metrics-cache.json"
	mttrCacheFile          = "/data/metrics/issue-to-merge.json"
	prIssueCountsCacheFile = "/data/metrics/pr-issue-counts.json"
)

type MetricsCollector struct {
	ghClient *ghpkg.Client
	// clientFn, when set, supplies the GitHub client on every collect instead
	// of ghClient, so a client rebuilt after an App credential change (or
	// first delivered after an App-less boot) is used without a restart
	// (#9621). See SetGitHubClientProvider.
	clientFn      func() *ghpkg.Client
	org           string
	repo          string
	badgeURL      string
	aiAuthor      string
	projectName   string
	logger        *slog.Logger
	mu            sync.RWMutex
	metrics       map[string]any
	mttrMu        sync.RWMutex
	mttr          *ghpkg.MTTRResult
	prIssueMu     sync.RWMutex
	prIssueCounts *ghpkg.PRIssueCounts
	// issuesDisabled is the latest proactive has_issues probe over every
	// watched repo (#9972); the dashboard banner renders it.
	issuesDisabledMu sync.RWMutex
	issuesDisabled   []IssuesDisabledRepo
}

func NewMetricsCollector(ghClient *ghpkg.Client, org, primaryRepo, badgeURL, aiAuthor, projectName string, logger *slog.Logger) *MetricsCollector {
	mc := &MetricsCollector{
		ghClient:    ghClient,
		org:         org,
		repo:        primaryRepo,
		badgeURL:    badgeURL,
		aiAuthor:    aiAuthor,
		projectName: projectName,
		logger:      logger,
		metrics:     make(map[string]any),
	}
	if projectName == "" {
		logger.Warn("project.name is empty — outreach PR counts will be zero (set project.name in config)")
	}
	mc.loadFromDisk()
	mc.loadMTTRFromDisk()
	mc.loadPRIssueCountsFromDisk()
	return mc
}

// SetGitHubClientProvider makes the collector read its GitHub client through
// fn on every use instead of the pointer it was constructed with. The hive
// swaps its client when App credentials change or first arrive over the
// heartbeat; a captured pointer would keep collecting with the old (or
// absent) client until the pod restarted (#9621). Call before Start.
func (mc *MetricsCollector) SetGitHubClientProvider(fn func() *ghpkg.Client) {
	if mc == nil {
		return
	}
	mc.clientFn = fn
}

// client returns the GitHub client to use now: the provider's current client
// when one is installed, otherwise the constructor's.
func (mc *MetricsCollector) client() *ghpkg.Client {
	if mc.clientFn != nil {
		return mc.clientFn()
	}
	return mc.ghClient
}

func (mc *MetricsCollector) Start(ctx context.Context) {
	mc.collect(ctx)
	ticker := time.NewTicker(metricsCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mc.collect(ctx)
		}
	}
}

func (mc *MetricsCollector) Get() map[string]any {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	result := make(map[string]any, len(mc.metrics))
	for k, v := range mc.metrics {
		result[k] = v
	}
	return result
}

// GetMTTR returns the latest MTTR result, or nil if no data is available.
func (mc *MetricsCollector) GetMTTR() *ghpkg.MTTRResult {
	mc.mttrMu.RLock()
	defer mc.mttrMu.RUnlock()
	return mc.mttr
}

// GetPRIssueCounts returns the latest merged-PR / closed-issue counts, or nil
// if no data is available yet.
func (mc *MetricsCollector) GetPRIssueCounts() *ghpkg.PRIssueCounts {
	mc.prIssueMu.RLock()
	defer mc.prIssueMu.RUnlock()
	return mc.prIssueCounts
}

func (mc *MetricsCollector) collect(ctx context.Context) {
	metrics := make(map[string]any)

	outreach := mc.collectOutreach(ctx)
	metrics["outreach"] = outreach

	ciMaintainer := mc.collectCoverage(ctx)
	metrics["ci-maintainer"] = ciMaintainer

	architect := mc.collectArchitect()
	metrics["architect"] = architect

	mc.collectMTTR(ctx)
	mc.collectPRIssueCounts(ctx)
	mc.collectIssuesDisabled(ctx)

	mc.mu.Lock()
	mc.metrics = metrics
	mc.mu.Unlock()

	mc.saveToDisk(metrics)
	mc.logger.Info("agent metrics collected",
		"stars", outreach["stars"],
		"coverage", ciMaintainer["coverage"],
	)
}

// collectMTTR computes issue-to-merge time from recently merged PRs with
// "Fixes #N" references, persists the result to disk, and stores it in memory.
func (mc *MetricsCollector) collectMTTR(ctx context.Context) {
	gh := mc.client()
	if gh == nil || mc.repo == "" {
		return
	}

	result, err := gh.ComputeMTTR(ctx, mc.repo)
	if err != nil {
		mc.logger.Warn("failed to compute MTTR", "error", err)
		return
	}

	mc.mttrMu.Lock()
	mc.mttr = result
	mc.mttrMu.Unlock()

	mc.saveMTTRToDisk(result)
	mc.logger.Info("MTTR computed",
		"avg_minutes", result.AvgMinutes,
		"median_minutes", result.MedianMinutes,
		"count", result.Count,
	)
}

// collectPRIssueCounts fetches the hive-attributed merged-PR and closed-issue
// counts for the primary repo, persists the result to disk, and stores it in
// memory. Used by the dashboard's Cost section to derive cost-per-PR /
// cost-per-issue.
func (mc *MetricsCollector) collectPRIssueCounts(ctx context.Context) {
	gh := mc.client()
	if gh == nil || mc.repo == "" {
		return
	}

	result, err := gh.ComputePRIssueCounts(ctx, mc.repo, mc.aiAuthor)
	if err != nil {
		mc.logger.Warn("failed to compute PR/issue counts", "error", err)
		return
	}

	mc.prIssueMu.Lock()
	mc.prIssueCounts = result
	mc.prIssueMu.Unlock()

	mc.savePRIssueCountsToDisk(result)
	mc.logger.Info("PR/issue counts computed",
		"merged_prs", result.MergedPRs,
		"closed_issues", result.ClosedIssues,
	)
}

func (mc *MetricsCollector) collectOutreach(ctx context.Context) map[string]any {
	result := map[string]any{
		"stars":          0,
		"forks":          0,
		"contributors":   0,
		"adopters":       0,
		"acmm":           0,
		"outreachOpen":   0,
		"outreachMerged": 0,
	}

	gh := mc.client()
	if gh == nil {
		return result
	}

	repoFull := mc.org + "/" + mc.repo
	parts := strings.SplitN(repoFull, "/", 2)
	if len(parts) != 2 {
		return result
	}

	repo, _, err := gh.GetRepo(ctx, parts[0], parts[1])
	if err == nil && repo != nil {
		result["stars"] = repo.GetStargazersCount()
		result["forks"] = repo.GetForksCount()
	}

	contribs, err := gh.GetContributorCount(ctx, parts[0], parts[1])
	if err == nil {
		result["contributors"] = contribs
	}

	// Adopters/ACMM/outreach are flagship-project counters. The repo moved to
	// the "hivecommons" org (org transfer), but hives configured before the
	// transfer still report org "kubestellar" — accept BOTH so the migration
	// does not silently zero these panels on older configs.
	if mc.org == "kubestellar" || mc.org == "hivecommons" {
		adopters := mc.countAdopters(ctx, mc.org, mc.repo)
		result["adopters"] = adopters

		acmm := mc.countACMM(ctx, mc.org, mc.repo)
		result["acmm"] = acmm

		open, merged := mc.countOutreachPRs(ctx)
		result["outreachOpen"] = open
		result["outreachMerged"] = merged
	}

	return result
}

// collectCoverage reads the primary repo's test-coverage percentage for the
// ci-maintainer card (and the ACMM advisor's coverage floors). The source is
// badgeURL, which is HIVE_COVERAGE_BADGE_URL: either an http(s) URL to a
// shields-style JSON or SVG badge, or "repo://<ref>/<path>" to read the badge
// file from the hive's own repo via the App client. Nothing configured, or
// anything unreadable, reports 0 — never a number for some OTHER project.
func (mc *MetricsCollector) collectCoverage(ctx context.Context) map[string]any {
	result := map[string]any{
		"coverage":       0,
		"coverageTarget": coverageTarget,
	}

	if mc.badgeURL == "" {
		return result
	}

	body, ok := mc.fetchCoverageBadge(ctx)
	if !ok {
		return result
	}
	if pct, ok := parseCoverageBadge(body); ok {
		result["coverage"] = pct
	}
	return result
}

func (mc *MetricsCollector) collectArchitect() map[string]any {
	return map[string]any{
		"prs":    0,
		"closed": 0,
	}
}

func (mc *MetricsCollector) countAdopters(ctx context.Context, owner, repo string) int {
	gh := mc.client()
	if gh == nil {
		return 0
	}
	content, err := gh.GetFileContent(ctx, owner, repo, "ADOPTERS.MD")
	if err != nil {
		content, err = gh.GetFileContent(ctx, owner, repo, "ADOPTERS.md")
		if err != nil {
			return 0
		}
	}
	count := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.HasPrefix(line, "|") && !strings.Contains(line, "---") && !strings.HasPrefix(line, "| Organization") {
			count++
		}
	}
	return count
}

// countACMM counts ACMM badge participants by reading the leaderboard page source
// from the docs repo (kubestellar/docs). It looks for entries in the
// BADGE_PARTICIPANTS Set definition in acmm-leaderboard/page.tsx.
func (mc *MetricsCollector) countACMM(ctx context.Context, owner, repo string) int {
	// ACMM leaderboard lives in the docs repo, not the primary repo
	const acmmLeaderboardPath = "src/app/[locale]/acmm-leaderboard/page.tsx"
	docsOwner := owner
	if docsOwner == "hivecommons" {
		docsOwner = "kubestellar"
	}
	gh := mc.client()
	if gh == nil {
		return 0
	}
	content, err := gh.GetFileContent(ctx, docsOwner, "docs", acmmLeaderboardPath)
	if err != nil {
		mc.logger.Warn("failed to fetch ACMM leaderboard page", "error", err)
		return 0
	}

	// Find the BADGE_PARTICIPANTS = new Set([...]) block and count quoted entries
	inSet := false
	count := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "BADGE_PARTICIPANTS") && strings.Contains(line, "new Set") {
			inSet = true
			continue
		}
		if inSet {
			trimmed := strings.TrimSpace(line)
			// End of the Set definition
			if strings.Contains(trimmed, "])") || strings.Contains(trimmed, "]);") {
				break
			}
			// Count lines that start with a quoted string (project name entries)
			if strings.HasPrefix(trimmed, "\"") && len(trimmed) > 1 {
				count++
			}
		}
	}
	return count
}

func (mc *MetricsCollector) countOutreachPRs(ctx context.Context) (open, merged int) {
	gh := mc.client()
	if gh == nil || mc.aiAuthor == "" {
		return 0, 0
	}

	openCount, err := gh.SearchOutreachPRCount(ctx, mc.aiAuthor, mc.org, mc.projectName, "open")
	if err != nil {
		mc.logger.Warn("failed to count open outreach PRs", "error", err)
	}

	mergedCount, err := gh.SearchOutreachPRCount(ctx, mc.aiAuthor, mc.org, mc.projectName, "merged")
	if err != nil {
		mc.logger.Warn("failed to count merged outreach PRs", "error", err)
	}

	return openCount, mergedCount
}

func (mc *MetricsCollector) loadFromDisk() {
	data, err := os.ReadFile(metricsCacheFile)
	if err != nil {
		return
	}
	var metrics map[string]any
	if json.Unmarshal(data, &metrics) == nil {
		mc.metrics = metrics
	}
}

func (mc *MetricsCollector) saveToDisk(metrics map[string]any) {
	data, err := json.Marshal(metrics)
	if err != nil {
		return
	}
	_ = os.MkdirAll("/data/metrics", 0o755)
	tmpPath := metricsCacheFile + ".tmp"
	if os.WriteFile(tmpPath, data, 0o644) == nil {
		_ = os.Rename(tmpPath, metricsCacheFile)
	}
}

func (mc *MetricsCollector) loadMTTRFromDisk() {
	data, err := os.ReadFile(mttrCacheFile)
	if err != nil {
		return
	}
	var result ghpkg.MTTRResult
	if json.Unmarshal(data, &result) == nil && result.Count > 0 {
		mc.mttr = &result
		mc.logger.Info("MTTR loaded from disk cache",
			"median_minutes", result.MedianMinutes,
			"count", result.Count,
		)
	}
}

func (mc *MetricsCollector) saveMTTRToDisk(result *ghpkg.MTTRResult) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = os.MkdirAll("/data/metrics", 0o755)
	tmpPath := mttrCacheFile + ".tmp"
	if os.WriteFile(tmpPath, data, 0o644) == nil {
		_ = os.Rename(tmpPath, mttrCacheFile)
	}
}

func (mc *MetricsCollector) loadPRIssueCountsFromDisk() {
	data, err := os.ReadFile(prIssueCountsCacheFile)
	if err != nil {
		return
	}
	var result ghpkg.PRIssueCounts
	if json.Unmarshal(data, &result) == nil && result.UpdatedAt != "" {
		mc.prIssueCounts = &result
		mc.logger.Info("PR/issue counts loaded from disk cache",
			"merged_prs", result.MergedPRs,
			"closed_issues", result.ClosedIssues,
		)
	}
}

func (mc *MetricsCollector) savePRIssueCountsToDisk(result *ghpkg.PRIssueCounts) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = os.MkdirAll("/data/metrics", 0o755)
	tmpPath := prIssueCountsCacheFile + ".tmp"
	if os.WriteFile(tmpPath, data, 0o644) == nil {
		_ = os.Rename(tmpPath, prIssueCountsCacheFile)
	}
}

// coverageBadgeRepoScheme is the badge-URL prefix that reads the badge from
// the hive's OWN primary repo through the GitHub App client instead of an
// anonymous HTTP GET: "repo://<ref>/<path>", e.g. "repo://badges/coverage.svg"
// for octocov's default layout. This is the only form that works for a
// private repo — the App has contents access, an unauthenticated fetch of a
// raw.githubusercontent.com URL there is a 404.
const coverageBadgeRepoScheme = "repo://"

// coveragePercentPattern finds the first percentage in badge TEXT. It is
// applied to a shields-style JSON "message" ("85%", "85.3%"), or to the text
// content of any other body once markup is stripped — which is what makes an
// SVG badge (octocov, shields.io) usable directly: the rendered number sits
// in a <text> element. Fractions are truncated because the dashboard renders
// whole percentages.
var coveragePercentPattern = regexp.MustCompile(`(\d{1,3})(?:\.\d+)?\s*%`)

// markupTagPattern strips tags (and with them every attribute) from an SVG
// or HTML badge before the percentage scan. Scanning the raw markup read the
// wrong number: a shields-style badge opens with
// <linearGradient x2="0" y2="100%"> — the gradient's extent, not the
// coverage — so a repo at 98.5% reported 100. Only text nodes carry the
// rendered figure.
var markupTagPattern = regexp.MustCompile(`<[^>]*>`)

// coverageTarget is the pct-bar target the ci-maintainer card renders against.
const coverageTarget = 91

// fetchCoverageBadge returns the raw badge body for badgeURL, dispatching on
// the scheme: repo:// goes through the GitHub client, anything else is a
// plain HTTP GET (the original behaviour, kept for public gists and badge
// services).
func (mc *MetricsCollector) fetchCoverageBadge(ctx context.Context) (string, bool) {
	if strings.HasPrefix(mc.badgeURL, coverageBadgeRepoScheme) {
		ref, path, ok := strings.Cut(strings.TrimPrefix(mc.badgeURL, coverageBadgeRepoScheme), "/")
		if !ok || ref == "" || path == "" {
			mc.logger.Warn("coverage badge: repo:// form must be repo://<ref>/<path>", "badge_url", mc.badgeURL)
			return "", false
		}
		gh := mc.client()
		if gh == nil || mc.org == "" || mc.repo == "" {
			// No App client (or no primary repo) — nothing to read it with.
			return "", false
		}
		content, err := gh.GetFileContentRef(ctx, mc.org, mc.repo, path, ref)
		if err != nil {
			mc.logger.Warn("coverage badge: could not read from primary repo",
				"repo", mc.org+"/"+mc.repo, "ref", ref, "path", path, "error", err)
			return "", false
		}
		return content, true
	}

	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mc.badgeURL, nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer closeHTTPBody(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(body), true
}

// parseCoverageBadge extracts a whole-number percentage from a badge body.
// A shields-style JSON badge is read from its "message" field; any other
// body (an SVG badge, plain text) has its markup stripped and its text
// scanned for the first percentage.
func parseCoverageBadge(body string) (int, bool) {
	var badge struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &badge) == nil && badge.Message != "" {
		return coveragePercent(badge.Message)
	}
	return coveragePercent(markupTagPattern.ReplaceAllString(body, " "))
}

func coveragePercent(text string) (int, bool) {
	m := coveragePercentPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	var val int
	if _, err := fmt.Sscanf(m[1], "%d", &val); err != nil || val > 100 {
		return 0, false
	}
	return val, true
}

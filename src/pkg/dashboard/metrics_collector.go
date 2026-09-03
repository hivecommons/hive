package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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
	ghClient      *ghpkg.Client
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

	ciMaintainer := mc.collectCoverage()
	metrics["ci-maintainer"] = ciMaintainer

	architect := mc.collectArchitect()
	metrics["architect"] = architect

	mc.collectMTTR(ctx)
	mc.collectPRIssueCounts(ctx)

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
	if mc.ghClient == nil || mc.repo == "" {
		return
	}

	result, err := mc.ghClient.ComputeMTTR(ctx, mc.repo)
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

// collectPRIssueCounts fetches the total merged-PR and closed-issue counts for
// the primary repo, persists the result to disk, and stores it in memory. Used
// by the dashboard's Cost section to derive cost-per-PR / cost-per-issue.
func (mc *MetricsCollector) collectPRIssueCounts(ctx context.Context) {
	if mc.ghClient == nil || mc.repo == "" {
		return
	}

	result, err := mc.ghClient.ComputePRIssueCounts(ctx, mc.repo)
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

	if mc.ghClient == nil {
		return result
	}

	repoFull := mc.org + "/" + mc.repo
	parts := strings.SplitN(repoFull, "/", 2)
	if len(parts) != 2 {
		return result
	}

	repo, _, err := mc.ghClient.GetRepo(ctx, parts[0], parts[1])
	if err == nil && repo != nil {
		result["stars"] = repo.GetStargazersCount()
		result["forks"] = repo.GetForksCount()
	}

	contribs, err := mc.ghClient.GetContributorCount(ctx, parts[0], parts[1])
	if err == nil {
		result["contributors"] = contribs
	}

	if mc.org == "kubestellar" {
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

func (mc *MetricsCollector) collectCoverage() map[string]any {
	const coverageTarget = 91
	result := map[string]any{
		"coverage":       0,
		"coverageTarget": coverageTarget,
	}

	if mc.badgeURL == "" {
		return result
	}

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(mc.badgeURL)
	if err != nil {
		return result
	}
	defer closeHTTPBody(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result
	}

	var badge struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &badge) == nil && badge.Message != "" {
		msg := strings.TrimSuffix(badge.Message, "%")
		var val int
		if _, err := fmt.Sscanf(msg, "%d", &val); err == nil {
			result["coverage"] = val
		}
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
	content, err := mc.ghClient.GetFileContent(ctx, owner, repo, "ADOPTERS.MD")
	if err != nil {
		content, err = mc.ghClient.GetFileContent(ctx, owner, repo, "ADOPTERS.md")
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
	content, err := mc.ghClient.GetFileContent(ctx, owner, "docs", acmmLeaderboardPath)
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
	if mc.ghClient == nil || mc.aiAuthor == "" {
		return 0, 0
	}

	openCount, err := mc.ghClient.SearchOutreachPRCount(ctx, mc.aiAuthor, mc.org, mc.projectName, "open")
	if err != nil {
		mc.logger.Warn("failed to count open outreach PRs", "error", err)
	}

	mergedCount, err := mc.ghClient.SearchOutreachPRCount(ctx, mc.aiAuthor, mc.org, mc.projectName, "merged")
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

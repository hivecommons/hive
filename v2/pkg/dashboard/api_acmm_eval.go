package dashboard

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

const acmmEvalTTL = time.Hour
const acmmLevelThreshold = 0.70
const acmmEvalTimeout = 30 * time.Second

// ACMMEvaluation is the combined codebase + operational ACMM evaluation result.
type ACMMEvaluation struct {
	CodebaseLevel     int               `json:"codebase_level"`
	CodebaseLevelName string            `json:"codebase_level_name"`
	OperationalLevel  int               `json:"operational_level"`
	OperationalName   string            `json:"operational_name"`
	OverallLevel      int               `json:"overall_level"`
	CriteriaTotal     int               `json:"criteria_total"`
	CriteriaPassed    int               `json:"criteria_passed"`
	LastEvaluatedAt   string            `json:"last_evaluated_at"`
	Levels            []ACMMLevelScore  `json:"levels"`
	CriteriaResults   []CriterionResult `json:"criteria_results,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// ACMMLevelScore summarizes pass/fail for a single ACMM level.
type ACMMLevelScore struct {
	Level     int     `json:"level"`
	Name      string  `json:"name"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`
	Passed    bool    `json:"passed"`
	Total     int     `json:"total"`
	Matched   int     `json:"matched"`
}

// CriterionResult records whether an individual criterion was detected.
type CriterionResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Level  int    `json:"level"`
	Passed bool   `json:"passed"`
}

func (s *Server) handleACMMEvaluation(w http.ResponseWriter, r *http.Request) {
	opsLevel := s.detectCurrentLevel()
	opsName := acmmLevelNames[opsLevel]
	if opsName == "" {
		opsName = "Unknown"
	}

	// Try returning cached codebase results with fresh operational level.
	s.acmmEvalMu.RLock()
	cached := s.acmmEvalCache
	cacheAge := time.Since(s.acmmEvalCachedAt)
	s.acmmEvalMu.RUnlock()

	if cached != nil && cacheAge < acmmEvalTTL {
		result := *cached
		result.OperationalLevel = opsLevel
		result.OperationalName = opsName
		result.OverallLevel = minInt(result.CodebaseLevel, opsLevel)
		jsonResponse(w, result)
		return
	}

	// Stale or missing — run fresh evaluation.
	s.acmmEvalMu.Lock()
	defer s.acmmEvalMu.Unlock()

	// Double-check after acquiring write lock.
	if s.acmmEvalCache != nil && time.Since(s.acmmEvalCachedAt) < acmmEvalTTL {
		result := *s.acmmEvalCache
		result.OperationalLevel = opsLevel
		result.OperationalName = opsName
		result.OverallLevel = minInt(result.CodebaseLevel, opsLevel)
		jsonResponse(w, result)
		return
	}

	eval := s.evaluateCodebase()
	eval.OperationalLevel = opsLevel
	eval.OperationalName = opsName
	eval.OverallLevel = minInt(eval.CodebaseLevel, opsLevel)

	s.acmmEvalCache = &eval
	s.acmmEvalCachedAt = time.Now()

	jsonResponse(w, eval)
}

// evaluateCodebase scans the configured primary repo for ACMM criteria.
func (s *Server) evaluateCodebase() ACMMEvaluation {
	if s.deps == nil || s.deps.Config == nil {
		return ACMMEvaluation{
			Error:           "config not loaded",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	owner := s.deps.Config.Project.Org
	repo := s.deps.Config.Project.PrimaryRepo
	if owner == "" || repo == "" {
		return ACMMEvaluation{
			Error:           "primary repo not configured",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	if s.deps.GHClient == nil {
		return ACMMEvaluation{
			Error:           "GitHub client not available",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		return ACMMEvaluation{
			Error:           "GitHub client not initialized",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), acmmEvalTimeout)
	defer cancel()

	// Pre-fetch directory listings for commonly checked parent directories
	// to batch API calls instead of checking each file individually.
	dirCache := s.prefetchDirectories(ctx, owner, repo)

	var results []CriterionResult
	for _, c := range universalCriteria {
		passed := s.checkCriterion(ctx, owner, repo, c, dirCache)
		results = append(results, CriterionResult{
			ID:     c.ID,
			Name:   c.Name,
			Level:  c.Level,
			Passed: passed,
		})
	}

	return s.scoreResults(results)
}

// prefetchDirectories fetches directory listings for parent dirs that
// many criteria share, reducing individual API calls.
func (s *Server) prefetchDirectories(ctx context.Context, owner, repo string) map[string]map[string]bool {
	cache := make(map[string]map[string]bool)

	dirs := []string{
		"",                   // root
		".github",            // templates, configs
		".github/workflows",  // CI workflows
		".github/ISSUE_TEMPLATE",
		".github/prompts",
		".github/agents",
		".claude",
		"docs",
		"docs/security",
	}

	ghClient := s.deps.GHClient.GoGitHub()
	for _, dir := range dirs {
		_, dirContents, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, dir, nil)
		if err != nil {
			continue
		}
		entries := make(map[string]bool)
		for _, entry := range dirContents {
			name := entry.GetName()
			entryType := entry.GetType()
			entries[name] = true
			if entryType == "dir" {
				entries[name+"/"] = true
			}
		}
		cache[dir] = entries
	}

	return cache
}

// checkCriterion returns true if any of the criterion's patterns exist in the repo.
func (s *Server) checkCriterion(ctx context.Context, owner, repo string, c ACMMCriterion, dirCache map[string]map[string]bool) bool {
	for _, pattern := range c.Patterns {
		if s.patternExists(ctx, owner, repo, pattern, dirCache) {
			return true
		}
	}
	return false
}

// patternExists checks if a file or directory path exists, using the
// pre-fetched directory cache when possible.
func (s *Server) patternExists(ctx context.Context, owner, repo, path string, dirCache map[string]map[string]bool) bool {
	isDir := strings.HasSuffix(path, "/")
	cleanPath := strings.TrimSuffix(path, "/")

	// Split into parent dir + base name for cache lookup.
	parent := ""
	base := cleanPath
	if idx := strings.LastIndex(cleanPath, "/"); idx >= 0 {
		parent = cleanPath[:idx]
		base = cleanPath[idx+1:]
	}

	if entries, ok := dirCache[parent]; ok {
		if isDir {
			return entries[base+"/"] || entries[base]
		}
		return entries[base]
	}

	// Cache miss — fall back to direct API check.
	ghClient := s.deps.GHClient.GoGitHub()
	_, _, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, cleanPath, nil)
	return err == nil
}

// scoreResults calculates per-level scores and the overall codebase level.
func (s *Server) scoreResults(results []CriterionResult) ACMMEvaluation {
	// Group by level.
	type levelBucket struct {
		total   int
		matched int
	}
	buckets := make(map[int]*levelBucket)

	totalPassed := 0
	for _, r := range results {
		b, ok := buckets[r.Level]
		if !ok {
			b = &levelBucket{}
			buckets[r.Level] = b
		}
		b.total++
		if r.Passed {
			b.matched++
			totalPassed++
		}
	}

	// Collect and sort level numbers.
	levels := make([]int, 0, len(buckets))
	for lvl := range buckets {
		levels = append(levels, lvl)
	}
	sort.Ints(levels)

	var levelScores []ACMMLevelScore
	for _, lvl := range levels {
		b := buckets[lvl]
		score := float64(0)
		if b.total > 0 {
			score = float64(b.matched) / float64(b.total)
		}
		name := acmmLevelNames[lvl]
		if name == "" {
			name = "Unknown"
		}
		levelScores = append(levelScores, ACMMLevelScore{
			Level:     lvl,
			Name:      name,
			Score:     score,
			Threshold: acmmLevelThreshold,
			Passed:    score >= acmmLevelThreshold,
			Total:     b.total,
			Matched:   b.matched,
		})
	}

	// Determine highest passing codebase level: all preceding levels must also pass.
	codebaseLevel := 0
	for _, ls := range levelScores {
		if ls.Passed {
			codebaseLevel = ls.Level
		} else {
			break
		}
	}

	codeLevelName := acmmLevelNames[codebaseLevel]
	if codeLevelName == "" {
		codeLevelName = "None"
	}

	return ACMMEvaluation{
		CodebaseLevel:     codebaseLevel,
		CodebaseLevelName: codeLevelName,
		CriteriaTotal:     len(results),
		CriteriaPassed:    totalPassed,
		LastEvaluatedAt:   time.Now().UTC().Format(time.RFC3339),
		Levels:            levelScores,
		CriteriaResults:   results,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

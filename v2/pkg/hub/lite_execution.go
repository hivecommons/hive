package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hiveadvisory "github.com/kubestellar/hive/v2/pkg/advisory"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	liteExecutionDefaultInterval = 6 * time.Hour
	liteExecutionMinScanInterval = 1 * time.Hour
	liteExecutionRequestTimeout  = 45 * time.Second
	liteExecutionIssueTitle      = "Hive lite advisory report"
	liteExecutionIssueMarker     = "<!-- hive-lite-advisory-report:v1 -->"
	liteACMMLevelThreshold       = 0.70
)

type LiteAdvisoryReport struct {
	GeneratedAt       string                `json:"generated_at"`
	Status            string                `json:"status"`
	Error             string                `json:"error,omitempty"`
	ACMMLevel         int                   `json:"acmm_level"`
	CodebaseLevel     int                   `json:"codebase_level"`
	CriteriaTotal     int                   `json:"criteria_total"`
	CriteriaPassed    int                   `json:"criteria_passed"`
	Levels            []LiteACMMLevelScore  `json:"levels,omitempty"`
	Findings          []LiteAdvisoryFinding `json:"findings,omitempty"`
	PostedIssueNumber int                   `json:"posted_issue_number,omitempty"`
	PostedIssueURL    string                `json:"posted_issue_url,omitempty"`
	LastPostedAt      string                `json:"last_posted_at,omitempty"`
}

type LiteAdvisoryFinding struct {
	ID       string   `json:"id"`
	Level    int      `json:"level"`
	Category string   `json:"category"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Patterns []string `json:"patterns,omitempty"`
	Passed   bool     `json:"passed"`
}

type LiteACMMLevelScore struct {
	Level   int     `json:"level"`
	Name    string  `json:"name"`
	Score   float64 `json:"score"`
	Passed  bool    `json:"passed"`
	Total   int     `json:"total"`
	Matched int     `json:"matched"`
}

type liteACMMCriterion struct {
	ID       string
	Level    int
	Category string
	Name     string
	Patterns []string
}

var liteACMMCriteria = []liteACMMCriterion{
	{ID: "acmm:prereq-test-suite", Level: 0, Category: "prerequisite", Name: "Test suite", Patterns: []string{"vitest.config.ts", "vitest.config.js", "jest.config.js", "jest.config.ts", "go.mod", "pytest.ini", "pyproject.toml", "test/", "tests/", "__tests__/", "spec/"}},
	{ID: "acmm:prereq-e2e", Level: 0, Category: "prerequisite", Name: "E2E tests", Patterns: []string{"playwright.config.ts", "playwright.config.js", "cypress.config.ts", "e2e/", "web/e2e/", "web/playwright.config.ts", "web/playwright.config.js", "test/e2e/", "tests/e2e/"}},
	{ID: "acmm:prereq-cicd", Level: 0, Category: "prerequisite", Name: "CI/CD pipeline", Patterns: []string{".github/workflows/", ".gitlab-ci.yml", "Jenkinsfile", ".circleci/"}},
	{ID: "acmm:prereq-pr-template", Level: 0, Category: "prerequisite", Name: "PR template", Patterns: []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md"}},
	{ID: "acmm:prereq-issue-template", Level: 0, Category: "prerequisite", Name: "Issue templates", Patterns: []string{".github/ISSUE_TEMPLATE/", ".github/issue_template.md"}},
	{ID: "acmm:prereq-contrib-guide", Level: 0, Category: "prerequisite", Name: "Contributing guide", Patterns: []string{"CONTRIBUTING.md", "docs/contributing.md", ".github/CONTRIBUTING.md"}},
	{ID: "acmm:prereq-code-style", Level: 0, Category: "prerequisite", Name: "Code style config", Patterns: []string{".eslintrc", ".eslintrc.json", ".eslintrc.js", "eslint.config.js", "eslint.config.mjs", ".prettierrc", ".prettierrc.json", "prettier.config.js", "ruff.toml", ".golangci.yml", "biome.json"}},
	{ID: "acmm:prereq-coverage-gate", Level: 0, Category: "prerequisite", Name: "Coverage gate", Patterns: []string{"codecov.yml", ".codecov.yml", ".github/workflows/coverage-gate.yml", "coverage.yml", ".coverage-thresholds.json"}},
	{ID: "acmm:claude-md", Level: 2, Category: "feedback-loop", Name: "CLAUDE.md instruction file", Patterns: []string{"CLAUDE.md", ".claude/CLAUDE.md"}},
	{ID: "acmm:copilot-instructions", Level: 2, Category: "feedback-loop", Name: "Copilot instructions", Patterns: []string{".github/copilot-instructions.md"}},
	{ID: "acmm:agents-md", Level: 2, Category: "feedback-loop", Name: "AGENTS.md", Patterns: []string{"AGENTS.md"}},
	{ID: "acmm:cursor-rules", Level: 2, Category: "feedback-loop", Name: "Cursor rules", Patterns: []string{".cursorrules", ".cursor/rules"}},
	{ID: "acmm:prompts-catalog", Level: 2, Category: "feedback-loop", Name: "Prompts catalog", Patterns: []string{"prompts/", ".prompts/", "docs/prompts/", ".github/prompts/", ".github/agents/"}},
	{ID: "acmm:editor-config", Level: 2, Category: "feedback-loop", Name: "EditorConfig", Patterns: []string{".editorconfig"}},
	{ID: "acmm:simple-skills", Level: 2, Category: "feedback-loop", Name: "Simple skills", Patterns: []string{".claude/skills/", ".claude/commands/", "skills/"}},
	{ID: "acmm:correction-capture", Level: 2, Category: "learning", Name: "Correction capture", Patterns: []string{".claude/memory/", ".memory/", "corrections.jsonl"}},
}

var liteACMMLevelNames = map[int]string{0: "Prerequisites not met", 1: "Inception", 2: "Advisory"}

type liteRepoEntry struct {
	Name string
	Type string
}

type liteRepoClient interface {
	ListDir(ctx context.Context, owner, repo, path string) ([]liteRepoEntry, error)
	Exists(ctx context.Context, owner, repo, path string) (bool, error)
	UpsertIssue(ctx context.Context, owner, repo, title, marker, body string) (int, string, error)
}

type liteExecutionState struct {
	mu      sync.Mutex
	started bool
	cfg     liteExecutionConfig
	scanner func(context.Context, RegistryEntry) (*LiteAdvisoryReport, error)
}

type liteExecutionConfig struct {
	Enabled  bool
	Interval time.Duration
}

func (s *HubServer) StartLiteExecution(ctx context.Context, enabled bool, interval time.Duration) {
	if !enabled {
		return
	}
	if interval <= 0 {
		interval = liteExecutionDefaultInterval
	}
	if interval < liteExecutionMinScanInterval {
		interval = liteExecutionMinScanInterval
	}
	s.ensureLiteExecutionState()
	s.liteExecution.mu.Lock()
	if s.liteExecution.started {
		s.liteExecution.mu.Unlock()
		return
	}
	s.liteExecution.started = true
	s.liteExecution.cfg = liteExecutionConfig{Enabled: true, Interval: interval}
	if s.liteExecution.scanner == nil {
		s.liteExecution.scanner = s.scanLiteEnrollment
	}
	s.liteExecution.mu.Unlock()
	go s.liteExecutionLoop(ctx, interval)
}

func (s *HubServer) ensureLiteExecutionState() {
	if s.liteExecution != nil {
		return
	}
	s.liteExecution = &liteExecutionState{}
}

func (s *HubServer) liteExecutionLoop(ctx context.Context, interval time.Duration) {
	s.runLiteExecutionOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runLiteExecutionOnce(ctx)
		}
	}
}

func (s *HubServer) runLiteExecutionOnce(ctx context.Context) {
	s.ensureLiteExecutionState()
	s.liteExecution.mu.Lock()
	scanner := s.liteExecution.scanner
	s.liteExecution.mu.Unlock()
	if scanner == nil {
		scanner = s.scanLiteEnrollment
	}

	now := time.Now().UTC()
	for _, entry := range s.liteEntriesDue(now) {
		report, err := scanner(ctx, entry)
		if err != nil {
			report = &LiteAdvisoryReport{
				GeneratedAt: now.Format(time.RFC3339),
				Status:      "error",
				ACMMLevel:   cappedLiteACMM(entry.ACMMLevel),
				Error:       err.Error(),
			}
		}
		s.storeLiteReport(entry.ID, report)
	}
}

func (s *HubServer) liteEntriesDue(now time.Time) []RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var due []RegistryEntry
	for _, h := range s.registry.Hives {
		if !h.Lite && h.HiveType != "lite" {
			continue
		}
		if h.GitHubInstallationID == 0 {
			continue
		}
		if h.LiteReport != nil {
			last, err := time.Parse(time.RFC3339, h.LiteReport.GeneratedAt)
			if err == nil && now.Sub(last) < liteExecutionMinScanInterval {
				continue
			}
		}
		due = append(due, h)
	}
	return due
}

func (s *HubServer) storeLiteReport(hiveID string, report *LiteAdvisoryReport) bool {
	if report == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].LiteReport = report
			s.registry.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			s.requestSave()
			return true
		}
	}
	return false
}

func (s *HubServer) scanLiteEnrollment(ctx context.Context, entry RegistryEntry) (*LiteAdvisoryReport, error) {
	auth, err := s.liteAppAuthForEntry(entry)
	if err != nil {
		return nil, err
	}
	read := "read"
	token, _, err := auth.MintInstallationToken(ctx, &gh.InstallationTokenOptions{
		Repositories: []string{entry.PrimaryRepo},
		Permissions:  &gh.InstallationPermissions{Metadata: &read, Contents: &read},
	})
	if err != nil {
		return nil, err
	}
	client := newLiteGitHubClient(token, auth.APIURL())
	report, err := evaluateLiteRepo(ctx, client, entry.Org, entry.PrimaryRepo, cappedLiteACMM(entry.ACMMLevel))
	if err != nil {
		return nil, err
	}
	if entry.LiteConfig != nil && entry.LiteConfig.PostAdvisoryIssue {
		if err := s.postLiteAdvisoryIssue(ctx, auth, entry, report); err != nil {
			report.Error = err.Error()
		}
	}
	return report, nil
}

func (s *HubServer) postLiteAdvisoryIssue(ctx context.Context, auth *hivegithub.AppAuth, entry RegistryEntry, report *LiteAdvisoryReport) error {
	write := "write"
	token, _, err := auth.MintInstallationToken(ctx, &gh.InstallationTokenOptions{
		Repositories: []string{entry.PrimaryRepo},
		Permissions:  &gh.InstallationPermissions{Metadata: gh.Ptr("read"), Issues: &write},
	})
	if err != nil {
		return err
	}
	client := newLiteGitHubClient(token, auth.APIURL())
	body := liteAdvisoryIssueBody(entry, report)
	num, url, err := client.UpsertIssue(ctx, entry.Org, entry.PrimaryRepo, liteExecutionIssueTitle, liteExecutionIssueMarker, body)
	if err != nil {
		return err
	}
	report.PostedIssueNumber = num
	report.PostedIssueURL = url
	report.LastPostedAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

func (s *HubServer) liteAppAuthForEntry(entry RegistryEntry) (*hivegithub.AppAuth, error) {
	identity := s.liteAppIdentityForEntry(entry)
	if identity == nil || identity.AppID == 0 || identity.PrivateKey == "" {
		return nil, fmt.Errorf("hub GitHub App key is not configured")
	}
	auth, err := hivegithub.NewAppAuthFromPEM(identity.AppID, entry.GitHubInstallationID, []byte(identity.PrivateKey), s.logger, identity.APIURL)
	if err != nil {
		return nil, fmt.Errorf("hub GitHub App key is not usable: %w", err)
	}
	return auth, nil
}

func (s *HubServer) liteAppIdentityForEntry(entry RegistryEntry) *clusterAppIdentity {
	host := githubHostLabel(entry.GitHubHost)
	if host == "" {
		host = publicGitHubHost
	}
	if host == publicGitHubHost {
		return s.appIdentityForCluster(defaultClusterID)
	}
	for id, cluster := range s.clusters {
		if clusterGitHubConfig(&cluster).HostLabel() == host {
			return s.appIdentityForCluster(id)
		}
	}
	return nil
}

func evaluateLiteRepo(ctx context.Context, client liteRepoClient, owner, repo string, acmmLevel int) (*LiteAdvisoryReport, error) {
	ctx, cancel := context.WithTimeout(ctx, liteExecutionRequestTimeout)
	defer cancel()
	dirCache := litePrefetchDirectories(ctx, client, owner, repo)
	var findings []LiteAdvisoryFinding
	var all []LiteAdvisoryFinding
	for _, c := range liteACMMCriteria {
		if c.Level > acmmLevel {
			continue
		}
		passed := liteCriterionPassed(ctx, client, owner, repo, c, dirCache)
		f := LiteAdvisoryFinding{
			ID:       c.ID,
			Level:    c.Level,
			Category: c.Category,
			Severity: liteCriterionSeverity(c.Level),
			Title:    c.Name,
			Detail:   liteCriterionDetail(c),
			Patterns: append([]string(nil), c.Patterns...),
			Passed:   passed,
		}
		all = append(all, f)
		if !passed {
			findings = append(findings, f)
		}
	}
	levels, codebase := liteScoreFindings(all)
	return &LiteAdvisoryReport{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:         "ok",
		ACMMLevel:      acmmLevel,
		CodebaseLevel:  codebase,
		CriteriaTotal:  len(all),
		CriteriaPassed: len(all) - len(findings),
		Levels:         levels,
		Findings:       findings,
	}, nil
}

func litePrefetchDirectories(ctx context.Context, client liteRepoClient, owner, repo string) map[string]map[string]bool {
	cache := make(map[string]map[string]bool)
	for _, dir := range []string{"", ".github", ".github/workflows", ".github/ISSUE_TEMPLATE", ".github/prompts", ".github/agents", ".claude", "docs", "docs/security"} {
		entries, err := client.ListDir(ctx, owner, repo, dir)
		if err != nil {
			continue
		}
		m := make(map[string]bool, len(entries))
		for _, e := range entries {
			m[e.Name] = true
			if e.Type == "dir" {
				m[e.Name+"/"] = true
			}
		}
		cache[dir] = m
	}
	return cache
}

func liteCriterionPassed(ctx context.Context, client liteRepoClient, owner, repo string, c liteACMMCriterion, cache map[string]map[string]bool) bool {
	for _, pattern := range c.Patterns {
		if litePatternExists(ctx, client, owner, repo, pattern, cache) {
			return true
		}
	}
	return false
}

func litePatternExists(ctx context.Context, client liteRepoClient, owner, repo, path string, cache map[string]map[string]bool) bool {
	isDir := strings.HasSuffix(path, "/")
	clean := strings.TrimSuffix(path, "/")
	parent, base := "", clean
	if idx := strings.LastIndex(clean, "/"); idx >= 0 {
		parent, base = clean[:idx], clean[idx+1:]
	}
	if entries, ok := cache[parent]; ok {
		if isDir {
			return entries[base+"/"] || entries[base]
		}
		return entries[base]
	}
	ok, err := client.Exists(ctx, owner, repo, clean)
	return err == nil && ok
}

func liteScoreFindings(findings []LiteAdvisoryFinding) ([]LiteACMMLevelScore, int) {
	type bucket struct{ total, matched int }
	buckets := map[int]*bucket{}
	for _, f := range findings {
		b := buckets[f.Level]
		if b == nil {
			b = &bucket{}
			buckets[f.Level] = b
		}
		b.total++
		if f.Passed {
			b.matched++
		}
	}
	levels := make([]int, 0, len(buckets))
	for level := range buckets {
		levels = append(levels, level)
	}
	sort.Ints(levels)
	scores := make([]LiteACMMLevelScore, 0, len(levels))
	codebase := 0
	for _, level := range levels {
		b := buckets[level]
		score := float64(b.matched) / float64(b.total)
		passed := score >= liteACMMLevelThreshold
		name := liteACMMLevelNames[level]
		if name == "" {
			name = "Unknown"
		}
		scores = append(scores, LiteACMMLevelScore{Level: level, Name: name, Score: score, Passed: passed, Total: b.total, Matched: b.matched})
		if passed {
			codebase = level
		} else {
			break
		}
	}
	if codebase == 0 {
		for _, score := range scores {
			if score.Level == 0 && score.Passed {
				codebase = 1
				break
			}
		}
	}
	return scores, capLiteCodebaseLevel(codebase)
}

func liteCriterionSeverity(level int) string {
	if level == 0 {
		return "medium"
	}
	return "low"
}

func liteCriterionDetail(c liteACMMCriterion) string {
	return fmt.Sprintf("Missing ACMM %s signal. Add one of: %s.", c.Category, strings.Join(c.Patterns, ", "))
}

func cappedLiteACMM(level int) int {
	if level <= 0 {
		return liteDefaultACMMLevel
	}
	if level > liteMaxACMMLevel {
		return liteMaxACMMLevel
	}
	return level
}

func capLiteCodebaseLevel(level int) int {
	if level < 0 {
		return 0
	}
	if level > liteMaxACMMLevel {
		return liteMaxACMMLevel
	}
	return level
}

func liteAdvisoryIssueBody(entry RegistryEntry, report *LiteAdvisoryReport) string {
	digest := &hiveadvisory.Digest{
		GeneratedAt: time.Now().UTC(),
		Mode:        "lite-advisory",
		ByAgent:     map[string][]hiveadvisory.Finding{"lite-acmm": {}},
		TotalCount:  len(report.Findings),
	}
	for _, f := range report.Findings {
		digest.ByAgent["lite-acmm"] = append(digest.ByAgent["lite-acmm"], hiveadvisory.Finding{
			Agent:     "lite-acmm",
			Timestamp: time.Now().UTC(),
			Type:      "advisory",
			Severity:  f.Severity,
			Title:     f.Title,
			Detail:    f.Detail,
		})
	}
	body := hiveadvisory.FormatDigestMarkdown(digest, entry.Org, entry.PrimaryRepo)
	if body == "" {
		body = "## 🐝 Hive Lite Advisory Report\n\nNo open ACMM advisory findings. ✅\n"
	}
	return liteExecutionIssueMarker + "\n" + body
}

type liteGitHubClient struct {
	gh *gh.Client
}

func newLiteGitHubClient(token, apiURL string) *liteGitHubClient {
	c := hivegithub.NewClient(token, "", nil, nil, apiURL)
	return &liteGitHubClient{gh: c.GoGitHub()}
}

func (c *liteGitHubClient) ListDir(ctx context.Context, owner, repo, path string) ([]liteRepoEntry, error) {
	_, dir, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		return nil, err
	}
	if len(dir) == 0 {
		return nil, fmt.Errorf("not a directory")
	}
	out := make([]liteRepoEntry, 0, len(dir))
	for _, e := range dir {
		out = append(out, liteRepoEntry{Name: e.GetName(), Type: e.GetType()})
	}
	return out, nil
}

func (c *liteGitHubClient) Exists(ctx context.Context, owner, repo, path string) (bool, error) {
	_, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *liteGitHubClient) UpsertIssue(ctx context.Context, owner, repo, title, marker, body string) (int, string, error) {
	query := fmt.Sprintf(`repo:%s/%s is:issue in:body %s`, owner, repo, marker)
	result, _, err := c.gh.Search.Issues(ctx, query, &gh.SearchOptions{})
	if err != nil {
		return 0, "", err
	}
	for _, issue := range result.Issues {
		if issue.GetState() == "closed" {
			continue
		}
		req := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body)}
		updated, _, err := c.gh.Issues.Edit(ctx, owner, repo, issue.GetNumber(), req)
		if err != nil {
			return 0, "", err
		}
		return updated.GetNumber(), updated.GetHTMLURL(), nil
	}
	created, _, err := c.gh.Issues.Create(ctx, owner, repo, &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body)})
	if err != nil {
		return 0, "", err
	}
	return created.GetNumber(), created.GetHTMLURL(), nil
}

func liteReportETag(report *LiteAdvisoryReport) string {
	data, _ := json.Marshal(report)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *HubServer) handleLiteReport(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	hiveID := strings.TrimSpace(r.PathValue("id"))
	if hiveID == "" {
		writeJSONError(w, http.StatusBadRequest, "hive id is required")
		return
	}
	if !s.canViewLiteReport(username, hiveID) {
		writeJSONError(w, http.StatusForbidden, "not authorized")
		return
	}
	s.mu.RLock()
	var report *LiteAdvisoryReport
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			report = s.registry.Hives[i].LiteReport
			break
		}
	}
	s.mu.RUnlock()
	if report == nil {
		writeJSONError(w, http.StatusNotFound, "lite advisory report not available yet")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", liteReportETag(report))
	json.NewEncoder(w).Encode(report)
}

func (s *HubServer) canViewLiteReport(username, hiveID string) bool {
	if username == hubAdminUsername {
		return true
	}
	if user := loadSaaSUser(username); user != nil {
		if _, ok := user.Hives[hiveID]; ok {
			return true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && strings.EqualFold(h.Owner, username) {
			return true
		}
	}
	return false
}

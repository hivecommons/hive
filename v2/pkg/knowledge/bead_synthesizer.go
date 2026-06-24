package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/beads"
)

const (
	beadSynthDefaultScheduleHours = 1
	beadSynthDailyScheduleHours   = 24
	beadSynthMaxBodyLen           = 500
	beadSynthDedupSearchLimit     = 5
	beadSynthHighPriorityBoost    = 0.1

	beadSynthMetaKeySynthesizedAt = "synthesized_at"

	prDescriptionMaxLen = 300
	prEnricherCacheSize = 200
)

// BeadSynthesizer periodically scans completed beads across all agents and
// synthesizes them into wiki facts for the shared knowledge layer.
type BeadSynthesizer struct {
	beadStores       map[string]*beads.Store
	knowledgeAPI     *KnowledgeAPI
	config           BeadSynthesizerConfig
	logger           *slog.Logger
	vaultBaseDir     string
	enricher         *PREnricher
	lifecycleManager *BeadLifecycleManager

	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
}

// NewBeadSynthesizer creates a synthesizer that bridges bead stores to the wiki.
func NewBeadSynthesizer(
	stores map[string]*beads.Store,
	api *KnowledgeAPI,
	config BeadSynthesizerConfig,
	logger *slog.Logger,
	ghClient *gh.Client,
) *BeadSynthesizer {
	s := &BeadSynthesizer{
		beadStores:   stores,
		knowledgeAPI: api,
		config:       config,
		logger:       logger,
		vaultBaseDir: config.VaultPath,
	}

	if ghClient != nil && config.Org != "" {
		s.enricher = NewPREnricher(ghClient, config.Org, config.Repos, logger)
	}

	if config.RetentionPolicy != nil {
		s.lifecycleManager = NewBeadLifecycleManager(stores, config.RetentionPolicy, logger)
	}

	return s
}

// SetLifecycleManager attaches or replaces the lifecycle manager.
func (s *BeadSynthesizer) SetLifecycleManager(lm *BeadLifecycleManager) {
	s.lifecycleManager = lm
}

// Start runs the synthesis loop in the background until ctx is cancelled.
func (s *BeadSynthesizer) Start(ctx context.Context) {
	interval := ParseSynthSchedule(s.config.Schedule)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.runOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// StartBackground launches the synthesis loop in a goroutine and tracks its
// lifecycle so it can be stopped and restarted via the dashboard toggle.
func (s *BeadSynthesizer) StartBackground(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.running = true
	go func() {
		s.Start(ctx)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
}

// Stop cancels the background synthesis loop.
func (s *BeadSynthesizer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// IsRunning returns whether the background synthesis loop is active.
func (s *BeadSynthesizer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *BeadSynthesizer) runOnce(ctx context.Context) {
	count, err := s.RunSynthesis(ctx)
	if err != nil {
		s.logger.Warn("bead synthesis cycle failed", "error", err)
		return
	}
	if count > 0 {
		s.logger.Info("bead synthesis cycle complete", "facts_synthesized", count)
	}

	if s.lifecycleManager != nil {
		archived, err := s.lifecycleManager.RunCulling(ctx)
		if err != nil {
			s.logger.Warn("bead lifecycle culling failed", "error", err)
		} else if archived > 0 {
			s.logger.Info("bead lifecycle culling complete", "beads_archived", archived)
		}
	}

	if s.enricher != nil {
		s.enricher.ClearCache()
	}
}

// beadCandidate pairs a bead with its source agent for provenance tracking.
type beadCandidate struct {
	bead  *beads.Bead
	agent string
}

// RunSynthesis scans all bead stores for unsynthesized done/closed beads,
// classifies them into facts, deduplicates, and ingests into the wiki.
func (s *BeadSynthesizer) RunSynthesis(ctx context.Context) (int, error) {
	var candidates []beadCandidate

	for agentName, store := range s.beadStores {
		if err := store.Reload(); err != nil {
			s.logger.Warn("failed to reload bead store", "agent", agentName, "error", err)
			continue
		}

		for _, b := range store.Unsynthesized() {
			candidates = append(candidates, beadCandidate{bead: b, agent: agentName})
		}
	}

	if len(candidates) == 0 {
		return 0, nil
	}

	var classified []classifiedFact
	for _, c := range candidates {
		factType, confidence := ClassifyBead(c.bead)
		if factType == "" {
			continue
		}
		if confidence < s.config.MinConfidence {
			continue
		}

		body := s.buildEnrichedBody(ctx, c.bead, c.agent)
		if body == "" {
			continue
		}

		fact := ExtractedFact{
			Title:      c.bead.Title,
			Body:       body,
			Type:       factType,
			Confidence: confidence,
			Tags:       extractBeadTags(c.bead),
			SourcePR:   fmt.Sprintf("bead:%s/%s", c.agent, c.bead.ID),
			SourceDate: c.bead.UpdatedAt.Time,
		}

		classified = append(classified, classifiedFact{fact: fact, candidate: c})
	}

	deduped := s.deduplicate(ctx, classified)

	cap := s.config.MaxFactsPerCycle
	if cap > 0 && len(deduped) > cap {
		deduped = deduped[:cap]
	}

	ingested := 0
	for _, cf := range deduped {
		if err := s.ingestFact(ctx, cf.fact); err != nil {
			s.logger.Warn("failed to ingest bead fact",
				"bead_id", cf.candidate.bead.ID,
				"agent", cf.candidate.agent,
				"error", err,
			)
			continue
		}

		store := s.beadStores[cf.candidate.agent]
		if err := store.SetMetadata(cf.candidate.bead.ID, beadSynthMetaKeySynthesizedAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
			s.logger.Warn("failed to mark bead as synthesized",
				"bead_id", cf.candidate.bead.ID,
				"error", err,
			)
		}

		ingested++
	}

	return ingested, nil
}

// ClassifyBead maps a bead's type and metadata to a wiki FactType and confidence score.
func ClassifyBead(b *beads.Bead) (FactType, float64) {
	findingType := b.Meta("finding_type")

	var factType FactType
	var confidence float64

	switch b.Type {
	case beads.TypeBug:
		if findingType == "regression" {
			factType = FactRegression
			confidence = 0.8
		} else {
			factType = FactGotcha
			confidence = 0.7
		}

	case beads.TypeDecision:
		factType = FactDecision
		confidence = 0.7

	case beads.TypeAdvisory:
		switch {
		case findingType == "pattern" || findingType == "convention":
			factType = FactPattern
			confidence = 0.6
		case findingType == "security" || findingType == "vulnerability":
			factType = FactGotcha
			confidence = 0.8
		case findingType == "test" || findingType == "coverage":
			factType = FactTestScaff
			confidence = 0.5
		default:
			factType, confidence = classifyByKeywords(b.Notes)
			if factType == "" {
				factType = FactPattern
				confidence = 0.5
			}
		}

	case beads.TypeFeature:
		factType = FactPattern
		confidence = 0.5

	case beads.TypeEpic:
		factType = FactDecision
		confidence = 0.6

	case beads.TypeTask:
		factType = FactPattern
		confidence = 0.4

	case beads.TypeChore:
		return "", 0

	default:
		return "", 0
	}

	if refined, refConf := classifyByKeywords(b.Notes); refined != "" && refConf > confidence {
		factType = refined
		confidence = refConf
	}

	if b.Priority <= beads.PriorityHigh {
		confidence += beadSynthHighPriorityBoost
		if confidence > 1.0 {
			confidence = 1.0
		}
	}

	return factType, confidence
}

// classifyByKeywords scans text for knowledge signal keywords, mirroring
// the approach in curator.go's classifyComment.
func classifyByKeywords(text string) (FactType, float64) {
	if text == "" {
		return "", 0
	}
	lower := strings.ToLower(text)

	switch {
	case containsAny(lower, "regression", "broke", "broke again", "reverted"):
		return FactRegression, 0.8
	case containsAny(lower, "always", "never", "must", "do not", "don't"):
		return FactGotcha, 0.7
	case containsAny(lower, "decided", "agreed", "going forward", "from now on"):
		return FactDecision, 0.7
	case containsAny(lower, "pattern", "convention", "prefer", "should use", "best practice"):
		return FactPattern, 0.6
	case containsAny(lower, "test", "coverage", "mock", "fixture", "assert"):
		return FactTestScaff, 0.5
	default:
		return "", 0
	}
}

// BuildFactBody composes a structured fact body from a bead's fields.
func BuildFactBody(b *beads.Bead, agent string) string {
	var buf strings.Builder

	if b.Notes != "" {
		buf.WriteString(b.Notes)
		buf.WriteString("\n")
	}

	detail := b.Meta("detail")
	if detail != "" {
		buf.WriteString("\n")
		buf.WriteString(detail)
		buf.WriteString("\n")
	}

	var meta []string
	if file := b.Meta("file"); file != "" {
		meta = append(meta, "File: "+file)
	}
	if sev := b.Meta("severity"); sev != "" {
		meta = append(meta, "Severity: "+sev)
	}
	if rec := b.Meta("recommendation"); rec != "" {
		meta = append(meta, "Recommendation: "+rec)
	}
	if b.ExternalRef != "" {
		meta = append(meta, "External: "+b.ExternalRef)
	}
	meta = append(meta, fmt.Sprintf("Source: bead:%s/%s", agent, b.ID))

	if len(meta) > 0 {
		buf.WriteString("\n")
		for _, m := range meta {
			buf.WriteString("- ")
			buf.WriteString(m)
			buf.WriteString("\n")
		}
	}

	body := buf.String()
	if len([]rune(body)) > beadSynthMaxBodyLen {
		body = string([]rune(body)[:beadSynthMaxBodyLen]) + "..."
	}

	return body
}

// extractBeadTags pulls tags from bead notes using the same keyword approach as curator.
func extractBeadTags(b *beads.Bead) []string {
	return extractTags(b.Title + " " + b.Notes)
}

type classifiedFact struct {
	fact      ExtractedFact
	candidate beadCandidate
}

// deduplicate removes duplicate facts within the batch and against existing wiki entries.
func (s *BeadSynthesizer) deduplicate(ctx context.Context, classified []classifiedFact) []classifiedFact {
	seen := make(map[string]bool)
	var result []classifiedFact

	for _, cf := range classified {
		slug := slugify(cf.fact.Title)
		if seen[slug] {
			continue
		}
		seen[slug] = true

		existing := s.knowledgeAPI.SearchAllWithVaults(ctx, cf.fact.Title, "", beadSynthDedupSearchLimit)
		if hasMatchingFact(existing, cf.fact.Title) {
			s.logger.Debug("bead fact already in wiki, skipping",
				"title", cf.fact.Title,
				"bead_id", cf.candidate.bead.ID,
			)
			continue
		}

		result = append(result, cf)
	}

	return result
}

// hasMatchingFact checks if any existing fact closely matches the given title.
func hasMatchingFact(existing []Fact, title string) bool {
	normalizedTitle := strings.ToLower(strings.TrimSpace(title))
	for _, f := range existing {
		existingTitle := strings.ToLower(strings.TrimSpace(f.Title))
		if existingTitle == normalizedTitle {
			return true
		}
		if strings.Contains(existingTitle, normalizedTitle) || strings.Contains(normalizedTitle, existingTitle) {
			return true
		}
	}
	return false
}

// ingestFact tries CreateFact, falling back to writing a vault file if no
// HTTP endpoint is configured for the target layer.
func (s *BeadSynthesizer) ingestFact(ctx context.Context, fact ExtractedFact) error {
	req := CreateFactRequest{
		Title:      fact.Title,
		Body:       fact.Body,
		Type:       string(fact.Type),
		Tags:       fact.Tags,
		Layer:      s.config.TargetLayer,
		Confidence: fact.Confidence,
	}

	err := s.knowledgeAPI.CreateFact(ctx, req)
	if err != nil && strings.Contains(err.Error(), "no configured endpoint") {
		return s.writeFactToVault(fact)
	}
	return err
}

// writeFactToVault writes a fact as a markdown file with YAML frontmatter,
// following the pattern from obsidianSyncToFile.
func (s *BeadSynthesizer) writeFactToVault(fact ExtractedFact) error {
	dir := s.vaultBaseDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating vault dir: %w", err)
	}

	slug := slugify(fact.Title)
	filename := strings.ReplaceAll(slug+".md", "/", "_")
	path := filepath.Join(dir, filename)

	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: %s\n", fact.Title)
	fmt.Fprintf(&buf, "type: %s\n", string(fact.Type))
	fmt.Fprintf(&buf, "layer: %s\n", s.config.TargetLayer)
	fmt.Fprintf(&buf, "confidence: %.2f\n", fact.Confidence)
	if len(fact.Tags) > 0 {
		fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(fact.Tags, ", "))
	}
	fmt.Fprintf(&buf, "source: %s\n", fact.SourcePR)
	fmt.Fprintf(&buf, "synthesized: %s\n", time.Now().UTC().Format(time.RFC3339))
	buf.WriteString("---\n\n")
	buf.WriteString(fact.Body)

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("writing vault file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming vault file: %w", err)
	}

	s.knowledgeAPI.triggerVaultReindex(dir)
	return nil
}

// buildEnrichedBody produces the fact body, enriching with GitHub PR data when possible.
// Returns empty string if the bead is too low-quality to synthesize.
func (s *BeadSynthesizer) buildEnrichedBody(ctx context.Context, b *beads.Bead, agent string) string {
	baseBody := BuildFactBody(b, agent)

	if s.enricher != nil && b.ExternalRef != "" {
		enriched := s.enricher.EnrichBody(ctx, b, agent)
		if enriched != "" {
			return enriched
		}
	}

	if isLowQualityMergeBead(b) && s.enricher == nil {
		s.logger.Debug("skipping low-quality merge bead without enricher",
			"bead_id", b.ID, "title", b.Title)
		return ""
	}

	return baseBody
}

// isLowQualityMergeBead detects beads that are just PR merge event titles
// with no substantive content.
func isLowQualityMergeBead(b *beads.Bead) bool {
	return strings.HasPrefix(b.Title, "Merged:") && strings.TrimSpace(b.Notes) == ""
}

// CleanupVault removes low-quality facts from the vault directory that were
// previously synthesized without enrichment. Call at startup to purge junk.
func (s *BeadSynthesizer) CleanupVault() (int, error) {
	if s.vaultBaseDir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(s.vaultBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading vault dir: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(s.vaultBaseDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		content := string(data)
		if isJunkFact(content) {
			if err := os.Remove(path); err != nil {
				s.logger.Warn("failed to remove junk fact", "path", path, "error", err)
				continue
			}
			removed++
		}
	}

	if removed > 0 {
		s.knowledgeAPI.triggerVaultReindex(s.vaultBaseDir)
	}

	return removed, nil
}

// isJunkFact detects vault files whose body contains only metadata lines
// (External/Source/Severity/File/Recommendation) with no substantive content.
func isJunkFact(content string) bool {
	bodyStart := strings.Index(content, "---\n")
	if bodyStart < 0 {
		return false
	}
	secondFence := strings.Index(content[bodyStart+4:], "---\n")
	if secondFence < 0 {
		return false
	}

	body := strings.TrimSpace(content[bodyStart+4+secondFence+4:])
	if body == "" {
		return true
	}

	lines := strings.Split(body, "\n")
	substantiveLines := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- External:") ||
			strings.HasPrefix(trimmed, "- Source:") ||
			strings.HasPrefix(trimmed, "- Severity:") ||
			strings.HasPrefix(trimmed, "- File:") ||
			strings.HasPrefix(trimmed, "- Recommendation:") {
			continue
		}
		substantiveLines++
	}

	return substantiveLines == 0
}

// PREnricher fetches structured PR data from GitHub to produce meaningful
// fact bodies instead of just echoing bead titles.
type PREnricher struct {
	ghClient *gh.Client
	org      string
	repos    []string
	logger   *slog.Logger
	mu       sync.Mutex
	cache    map[string]*gh.PullRequest
}

var (
	ghRefPattern   = regexp.MustCompile(`^gh-(\d+)$`)
	repoRefPattern = regexp.MustCompile(`^(?:([^/]+)/)?([^#]+)#(\d+)$`)
)

// NewPREnricher creates an enricher with the given GitHub client and repo context.
func NewPREnricher(client *gh.Client, org string, repos []string, logger *slog.Logger) *PREnricher {
	return &PREnricher{
		ghClient: client,
		org:      org,
		repos:    repos,
		logger:   logger,
		cache:    make(map[string]*gh.PullRequest),
	}
}

// ClearCache resets the in-memory PR cache between synthesis cycles.
func (e *PREnricher) ClearCache() {
	e.mu.Lock()
	e.cache = make(map[string]*gh.PullRequest)
	e.mu.Unlock()
}

// EnrichBody fetches PR data for a bead's ExternalRef and builds a fact body
// with the PR description, labels, and change stats.
func (e *PREnricher) EnrichBody(ctx context.Context, b *beads.Bead, agent string) string {
	owner, repo, number := e.parseRef(b.ExternalRef)
	if number == 0 {
		return ""
	}

	pr := e.fetchPR(ctx, owner, repo, number)
	if pr == nil {
		return ""
	}

	var buf strings.Builder

	desc := pr.GetBody()
	if desc != "" {
		desc = strings.TrimSpace(desc)
		if len([]rune(desc)) > prDescriptionMaxLen {
			desc = string([]rune(desc)[:prDescriptionMaxLen]) + "..."
		}
		buf.WriteString(desc)
		buf.WriteString("\n")
	}

	buf.WriteString("\n")

	if changed := pr.GetChangedFiles(); changed > 0 {
		fmt.Fprintf(&buf, "- Changed: %d files (+%d, -%d)\n",
			changed, pr.GetAdditions(), pr.GetDeletions())
	}

	var labelNames []string
	for _, l := range pr.Labels {
		labelNames = append(labelNames, l.GetName())
	}
	if len(labelNames) > 0 {
		fmt.Fprintf(&buf, "- Labels: %s\n", strings.Join(labelNames, ", "))
	}

	if mergedAt := pr.GetMergedAt(); !mergedAt.IsZero() {
		fmt.Fprintf(&buf, "- Merged: %s\n", mergedAt.Format("2006-01-02"))
	}

	fmt.Fprintf(&buf, "- Source: bead:%s/%s\n", agent, b.ID)

	body := buf.String()
	if len([]rune(body)) > beadSynthMaxBodyLen {
		body = string([]rune(body)[:beadSynthMaxBodyLen]) + "..."
	}

	return body
}

// parseRef extracts owner, repo, and PR number from an ExternalRef string.
func (e *PREnricher) parseRef(ref string) (string, string, int) {
	if m := ghRefPattern.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.Atoi(m[1])
		if len(e.repos) > 0 {
			return e.org, e.repos[0], n
		}
		return e.org, "", n
	}

	if m := repoRefPattern.FindStringSubmatch(ref); m != nil {
		owner := m[1]
		if owner == "" {
			owner = e.org
		}
		repo := m[2]
		n, _ := strconv.Atoi(m[3])
		return owner, repo, n
	}

	return "", "", 0
}

// fetchPR retrieves a PR from GitHub, using the in-memory cache first.
func (e *PREnricher) fetchPR(ctx context.Context, owner, repo string, number int) *gh.PullRequest {
	if e.ghClient == nil {
		return nil
	}
	if repo == "" {
		return e.fetchPRFromRepos(ctx, owner, number)
	}

	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)

	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return cached
	}
	e.mu.Unlock()

	pr, _, err := e.ghClient.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		e.logger.Debug("failed to fetch PR for enrichment",
			"owner", owner, "repo", repo, "number", number, "error", err)
		return nil
	}

	e.mu.Lock()
	if len(e.cache) < prEnricherCacheSize {
		e.cache[key] = pr
	}
	e.mu.Unlock()

	return pr
}

// fetchPRFromRepos tries each configured repo to find the PR.
func (e *PREnricher) fetchPRFromRepos(ctx context.Context, owner string, number int) *gh.PullRequest {
	for _, repo := range e.repos {
		pr := e.fetchPR(ctx, owner, repo, number)
		if pr != nil {
			return pr
		}
	}
	return nil
}

// ParseSynthSchedule converts a schedule string to a duration.
func ParseSynthSchedule(schedule string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(schedule)) {
	case "daily":
		return time.Duration(beadSynthDailyScheduleHours) * time.Hour
	case "hourly", "":
		return time.Duration(beadSynthDefaultScheduleHours) * time.Hour
	default:
		return time.Duration(beadSynthDefaultScheduleHours) * time.Hour
	}
}

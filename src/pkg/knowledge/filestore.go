package knowledge

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const accessCountsFile = "access_counts.json"

const (
	maxFileSizeBytes     = 512 * 1024 // skip files larger than 512KB
	indexRefreshInterval = 30 * time.Second
)

// FileStore reads markdown files from a local directory (e.g. an Obsidian vault)
// and exposes them as knowledge facts. It supports search, read, list, and stats
// operations compatible with the wiki Client interface.
//
// Search uses Fisher-Rao geodesic distance on diagonal Gaussian embeddings for
// ranking, graduated by per-fact access count. Cold facts fall back to cosine
// similarity; established facts get the richer geometric distance.
type FileStore struct {
	rootDir       string
	name          string
	mu            sync.RWMutex
	pages         map[string]filePage
	lastIndexed   time.Time
	logger        *slog.Logger
	prevPageCount int
	embedCache    *EmbeddingCache
	accessCounts  map[string]int
}

type filePage struct {
	Slug       string
	Title      string
	Body       string
	Tags       []string
	Related    []string
	Confidence float64
	Path       string
	ModTime    time.Time
	Embedding  []float64
}

// NewFileStore creates a store that indexes markdown files under rootDir.
func NewFileStore(rootDir string, name string, logger *slog.Logger) (*FileStore, error) {
	info, err := os.Stat(rootDir)
	if err != nil {
		return nil, fmt.Errorf("vault directory %s: %w", rootDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vault path %s is not a directory", rootDir)
	}

	embedder := NewEmbeddingCache(NewTFEmbedder(EmbeddingDim))
	s := &FileStore{
		rootDir:      rootDir,
		name:         name,
		pages:        make(map[string]filePage),
		logger:       logger,
		embedCache:   embedder,
		accessCounts: make(map[string]int),
	}

	s.loadAccessCounts()
	s.reindex()
	return s, nil
}

// Name returns the display name of this vault.
func (s *FileStore) Name() string { return s.name }

// SetName changes the display name of this vault (vanity rename).
// The underlying directory path is unchanged.
func (s *FileStore) SetName(name string) { s.name = name }

// RootDir returns the filesystem path.
func (s *FileStore) RootDir() string { return s.rootDir }

// WriteFacts persists facts into the vault as markdown files with YAML
// frontmatter, one file per fact. This is the local-write counterpart to the
// remote wiki Client.IngestFacts, and it is how user-imported facts land in a
// vault (issue #3581). It mirrors the bead synthesizer's on-disk format so a
// hand-imported fact and a synthesized one index identically. Files are written
// atomically (temp + rename); the caller triggers a reindex afterward.
func (s *FileStore) WriteFacts(facts []ExtractedFact) error {
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return fmt.Errorf("creating vault dir: %w", err)
	}
	for _, f := range facts {
		if strings.TrimSpace(f.Title) == "" {
			// A titleless fact has no stable slug/filename; skip rather than
			// clobber a shared "untitled.md".
			continue
		}
		slug := slugify(f.Title)
		filename := strings.ReplaceAll(slug+".md", "/", "_")
		path := filepath.Join(s.rootDir, filename)

		var buf strings.Builder
		buf.WriteString("---\n")
		fmt.Fprintf(&buf, "title: %s\n", f.Title)
		if f.Type != "" {
			fmt.Fprintf(&buf, "type: %s\n", string(f.Type))
		}
		if f.Confidence > 0 {
			fmt.Fprintf(&buf, "confidence: %.2f\n", f.Confidence)
		}
		if len(f.Tags) > 0 {
			fmt.Fprintf(&buf, "tags: [%s]\n", strings.Join(f.Tags, ", "))
		}
		if len(f.Related) > 0 {
			fmt.Fprintf(&buf, "related: [%s]\n", strings.Join(f.Related, ", "))
		}
		fmt.Fprintf(&buf, "imported: %s\n", time.Now().UTC().Format(time.RFC3339))
		buf.WriteString("---\n\n")
		buf.WriteString(f.Body)

		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o644); err != nil {
			return fmt.Errorf("writing vault file: %w", err)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("renaming vault file: %w", err)
		}
	}
	return nil
}

func (s *FileStore) reindex() {
	pages := make(map[string]filePage)

	err := filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSizeBytes {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		rel, _ := filepath.Rel(s.rootDir, path)
		slug := strings.TrimSuffix(rel, ext)
		slug = strings.ReplaceAll(slug, string(filepath.Separator), "/")

		title, body, tags, confidence, related := parseObsidianFile(string(data), filepath.Base(slug))

		embeddingText := title + " " + strings.Join(tags, " ") + " " + body
		embedding := s.embedCache.Embed(embeddingText)

		pages[slug] = filePage{
			Slug:       slug,
			Title:      title,
			Body:       body,
			Tags:       tags,
			Related:    related,
			Confidence: confidence,
			Path:       path,
			ModTime:    info.ModTime(),
			Embedding:  embedding,
		}

		return nil
	})

	if err != nil {
		s.logger.Warn("vault reindex error", "dir", s.rootDir, "error", err)
	}

	s.embedCache.Clear()
	s.mu.Lock()
	prevCount := s.prevPageCount
	s.pages = pages
	s.lastIndexed = time.Now()
	s.prevPageCount = len(pages)
	s.mu.Unlock()

	if len(pages) != prevCount {
		s.logger.Info("vault indexed",
			"name", s.name,
			"dir", s.rootDir,
			"pages", len(pages),
			"page_delta", len(pages)-prevCount,
		)
	}

	s.persistAccessCounts()
}

func (s *FileStore) refreshIfStale() {
	s.mu.RLock()
	stale := time.Since(s.lastIndexed) > indexRefreshInterval
	s.mu.RUnlock()
	if stale {
		s.reindex()
	}
}

// defaultSigmaInitial is the initial per-dimension standard deviation for
// Gaussian embeddings. Shrinks as more access data accumulates.
const defaultSigmaInitial = 1.0

// Search finds pages matching the query using Fisher-Rao geodesic distance
// on diagonal Gaussian embeddings. Scoring graduates from cosine similarity
// (cold facts) to full Fisher-Rao (frequently accessed facts).
func (s *FileStore) Search(query string, limit int) []Fact {
	s.refreshIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil
	}

	queryEmbedding := s.embedCache.Embed(query)
	queryGaussian := NewGaussianFromEmbedding(queryEmbedding, defaultSigmaInitial)

	type scored struct {
		page  filePage
		score float64
	}

	var matches []scored
	for _, p := range s.pages {
		accessCount := s.accessCounts[p.Slug]

		// Sigma narrows with usage — well-accessed facts have tighter distributions.
		sigma := defaultSigmaInitial / (1.0 + 0.1*float64(accessCount))
		factGaussian := GaussianParams{Mean: p.Embedding, Sigma: makeSigmaVec(len(p.Embedding), sigma)}

		score := GraduatedScore(queryGaussian, factGaussian, accessCount)

		// Exact term matches in title/tags get a bonus on top of the geometric score.
		bonus := termMatchBonus(p, terms)
		score = clampScore(score + bonus)

		const minScoreThreshold = 0.05
		if score > minScoreThreshold {
			matches = append(matches, scored{page: p, score: score})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	facts := make([]Fact, len(matches))
	for i, m := range matches {
		snippet := m.page.Body
		const maxSnippetRunes = 200
		if runes := []rune(snippet); len(runes) > maxSnippetRunes {
			snippet = string(runes[:maxSnippetRunes]) + "…"
		}
		facts[i] = Fact{
			Slug:       m.page.Slug,
			Title:      m.page.Title,
			Type:       FactPattern,
			Body:       snippet,
			Confidence: m.score,
			Tags:       m.page.Tags,
			Related:    m.page.Related,
			Layer:      LayerPersonal,
		}
	}
	return facts
}

// RecordAccess increments the access counter for a fact, which shifts its
// scoring from cosine toward Fisher-Rao over time.
func (s *FileStore) RecordAccess(slug string) {
	s.mu.Lock()
	s.accessCounts[slug]++
	s.mu.Unlock()
}

// AccessCount returns how many times a fact has been accessed.
func (s *FileStore) AccessCount(slug string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accessCounts[slug]
}

// termMatchBonus adds a small score bonus for exact keyword matches in
// title and tags, so keyword relevance isn't lost in the geometric score.
func termMatchBonus(p filePage, terms []string) float64 {
	const (
		titleMatchWeight = 0.15
		tagMatchWeight   = 0.10
	)
	titleLower := strings.ToLower(p.Title)
	var bonus float64
	for _, term := range terms {
		if strings.Contains(titleLower, term) {
			bonus += titleMatchWeight
		}
		for _, tag := range p.Tags {
			if strings.Contains(strings.ToLower(tag), term) {
				bonus += tagMatchWeight
			}
		}
	}
	return bonus
}

func makeSigmaVec(dim int, sigma float64) []float64 {
	v := make([]float64, dim)
	for i := range v {
		v[i] = sigma
	}
	return v
}

// ReadPage returns a single page by slug.
func (s *FileStore) ReadPage(slug string) (*Fact, error) {
	s.refreshIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.pages[slug]
	if !ok {
		return nil, fmt.Errorf("page not found: %s", slug)
	}
	return &Fact{
		Slug:       p.Slug,
		Title:      p.Title,
		Type:       FactPattern,
		Body:       p.Body,
		Confidence: effectiveConfidence(p.Confidence),
		Tags:       p.Tags,
		Related:    p.Related,
		Layer:      LayerPersonal,
	}, nil
}

// ListPages returns all pages, optionally filtered by a tag.
func (s *FileStore) ListPages(tagFilter string) []Fact {
	s.refreshIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()

	facts := make([]Fact, 0, len(s.pages))
	for _, p := range s.pages {
		if tagFilter != "" {
			found := false
			for _, t := range p.Tags {
				if strings.EqualFold(t, tagFilter) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		snippet := p.Body
		const maxSnippetRunes = 200
		if runes := []rune(snippet); len(runes) > maxSnippetRunes {
			snippet = string(runes[:maxSnippetRunes]) + "…"
		}
		facts = append(facts, Fact{
			Slug:       p.Slug,
			Title:      p.Title,
			Type:       FactPattern,
			Body:       snippet,
			Confidence: effectiveConfidence(p.Confidence),
			Tags:       p.Tags,
			Related:    p.Related,
			Layer:      LayerPersonal,
		})
	}

	sort.Slice(facts, func(i, j int) bool {
		return facts[i].Title < facts[j].Title
	})
	return facts
}

// Stats returns aggregate stats for this vault.
func (s *FileStore) Stats() FileStoreStats {
	s.refreshIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := FileStoreStats{
		TotalPages:  len(s.pages),
		LastIndexed: s.lastIndexed,
		RootDir:     s.rootDir,
		Name:        s.name,
		TagCounts:   make(map[string]int),
	}
	for _, p := range s.pages {
		for _, t := range p.Tags {
			stats.TagCounts[t]++
		}
	}
	return stats
}

// FileStoreStats holds aggregate info about a vault.
type FileStoreStats struct {
	TotalPages  int            `json:"total_pages"`
	LastIndexed time.Time      `json:"last_indexed"`
	RootDir     string         `json:"root_dir"`
	Name        string         `json:"name"`
	TagCounts   map[string]int `json:"tag_counts"`
}

// Reindex forces a full re-scan of the vault directory.
func (s *FileStore) Reindex() {
	s.reindex()
}

const defaultFactConfidence = 0.8

var wikilinkRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]+)?\]\]`)

func effectiveConfidence(parsed float64) float64 {
	if parsed < 0 {
		return defaultFactConfidence
	}
	return parsed
}

// parseObsidianFile extracts title, body, tags, confidence, and related slugs
// from an Obsidian-style markdown file. Supports YAML frontmatter and inline
// #tags and [[wikilinks]].
func parseObsidianFile(content string, fallbackTitle string) (title string, body string, tags []string, confidence float64, related []string) {
	title = fallbackTitle
	body = content
	const noConfidenceSentinel = -1.0
	confidence = noConfidenceSentinel

	if strings.HasPrefix(content, "---\n") {
		endIdx := strings.Index(content[4:], "\n---")
		if endIdx > 0 {
			frontmatter := content[4 : 4+endIdx]
			body = strings.TrimSpace(content[4+endIdx+4:])

			for _, line := range strings.Split(frontmatter, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "title:") {
					title = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
					title = strings.Trim(title, "\"'")
				}
				if strings.HasPrefix(line, "confidence:") {
					confStr := strings.TrimSpace(strings.TrimPrefix(line, "confidence:"))
					if v, err := strconv.ParseFloat(confStr, 64); err == nil {
						confidence = v
					}
				}
				if strings.HasPrefix(line, "related:") {
					relVal := strings.TrimSpace(strings.TrimPrefix(line, "related:"))
					if strings.HasPrefix(relVal, "[") {
						relVal = strings.Trim(relVal, "[]")
						for _, r := range strings.Split(relVal, ",") {
							r = strings.TrimSpace(r)
							r = strings.Trim(r, "\"'")
							if r != "" {
								related = append(related, r)
							}
						}
					}
				}
				if strings.HasPrefix(line, "tags:") {
					tagVal := strings.TrimSpace(strings.TrimPrefix(line, "tags:"))
					if strings.HasPrefix(tagVal, "[") {
						tagVal = strings.Trim(tagVal, "[]")
						for _, t := range strings.Split(tagVal, ",") {
							t = strings.TrimSpace(t)
							t = strings.Trim(t, "\"'")
							if t != "" {
								tags = append(tags, t)
							}
						}
					}
				}
				if strings.HasPrefix(line, "- ") && len(tags) > 0 {
					t := strings.TrimSpace(strings.TrimPrefix(line, "- "))
					t = strings.Trim(t, "\"'")
					if t != "" {
						tags = append(tags, t)
					}
				}
			}
		}
	}

	if strings.HasPrefix(body, "# ") {
		nlIdx := strings.Index(body, "\n")
		if nlIdx > 0 {
			title = strings.TrimSpace(body[2:nlIdx])
			body = strings.TrimSpace(body[nlIdx+1:])
		}
	}

	// Extract inline Obsidian #tags
	for _, word := range strings.Fields(body) {
		if strings.HasPrefix(word, "#") && len(word) > 1 && !strings.HasPrefix(word, "##") {
			tag := strings.Trim(word, "#.,;:!?")
			if tag != "" && !containsTag(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}

	// Extract [[wikilinks]] from body as related slugs
	for _, match := range wikilinkRe.FindAllStringSubmatch(body, -1) {
		slug := strings.TrimSpace(match[1])
		if slug != "" && !containsTag(related, slug) {
			related = append(related, slug)
		}
	}

	return title, body, tags, confidence, related
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// loadAccessCounts reads persisted access counts from disk.
func (s *FileStore) loadAccessCounts() {
	path := filepath.Join(s.rootDir, accessCountsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var counts map[string]int
	if err := json.Unmarshal(data, &counts); err != nil {
		s.logger.Warn("corrupted access counts file, resetting", "path", path, "error", err)
		return
	}

	s.mu.Lock()
	s.accessCounts = counts
	s.mu.Unlock()
}

// persistAccessCounts writes the current access counts to disk.
func (s *FileStore) persistAccessCounts() {
	s.mu.RLock()
	if len(s.accessCounts) == 0 {
		s.mu.RUnlock()
		return
	}
	data, err := json.Marshal(s.accessCounts)
	s.mu.RUnlock()

	if err != nil {
		s.logger.Warn("failed to marshal access counts", "error", err)
		return
	}

	path := filepath.Join(s.rootDir, accessCountsFile)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		s.logger.Warn("failed to write access counts", "path", path, "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		s.logger.Warn("failed to rename access counts into place", "path", path, "error", err)
	}
}

package retro

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// DefaultAnalysisCacheSize bounds the in-memory LRU of model analyses keyed
// by deterministic finding set (hivecommons/hive#12131). Retro runs once per
// closed bead, so the cache mainly absorbs re-scans and identical finding
// sets across beads; a few hundred entries covers a day of retro traffic.
const DefaultAnalysisCacheSize = 256

// AnalysisCacheStats are the cumulative cache counters for one Analyzer.
type AnalysisCacheStats struct {
	Hits uint64 `json:"hits"`
	// Misses counts finding sets that had to be sent to the model.
	Misses uint64 `json:"misses"`
	// SkippedEmpty counts Analyze calls that returned without a model call
	// because the deterministic finding set was EMPTY. This is the only
	// condition under which the analyzer skips the model: a non-empty set
	// always reaches the model (or the cache of a previous model reply).
	SkippedEmpty uint64 `json:"skipped_empty"`
	Size         int    `json:"size"`
}

// FindingSetHash returns a stable hash of a deterministic finding set, so
// identical inputs map to the same cache key regardless of finding order.
// The model name is folded in because a different analysis model is a
// different input. Finding.Detail is included: it carries the bead-specific
// numbers the analysis is about, so two beads only share an entry when the
// prompt the model would see is the same.
func FindingSetHash(model string, findings []Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Pattern+"\x00"+f.Severity+"\x00"+truncate(f.Detail, maxFindingDetailChars))
	}
	sort.Strings(parts)
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(model)))
	h.Write([]byte{0})
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type analysisCacheEntry struct {
	key      string
	analysis Analysis
}

// analysisCache is a small mutex-guarded LRU. In-memory only: retro has no
// existing durable cache pattern under /data, and a restart simply re-asks
// the model once per finding set.
type analysisCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
	stats AnalysisCacheStats
}

func newAnalysisCache(max int) *analysisCache {
	if max <= 0 {
		max = DefaultAnalysisCacheSize
	}
	return &analysisCache{max: max, order: list.New(), items: make(map[string]*list.Element, max)}
}

func (c *analysisCache) get(key string) (Analysis, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.stats.Misses++
		return Analysis{}, false
	}
	c.order.MoveToFront(el)
	c.stats.Hits++
	return el.Value.(*analysisCacheEntry).analysis, true
}

func (c *analysisCache) put(key string, a Analysis) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*analysisCacheEntry).analysis = a
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&analysisCacheEntry{key: key, analysis: a})
	c.items[key] = el
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*analysisCacheEntry).key)
	}
}

func (c *analysisCache) markSkippedEmpty() {
	c.mu.Lock()
	c.stats.SkippedEmpty++
	c.mu.Unlock()
}

func (c *analysisCache) snapshot() AnalysisCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Size = c.order.Len()
	return s
}

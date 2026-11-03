package intent

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// DefaultAlignmentCacheSize bounds the in-memory LRU of model alignment
// verdicts keyed by (head SHA, intent revision) (hivecommons/hive#12131). The
// governor re-evaluates every open agent PR each tick, so without a cache an
// unchanged PR is re-sent to the reviewer every cycle.
const DefaultAlignmentCacheSize = 512

// AlignmentCacheStats are the cumulative cache counters for one reviewer.
type AlignmentCacheStats struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
	Size   int    `json:"size"`
}

// IntentRevision hashes the intent-side inputs of an alignment review: the
// authorized intent text (linked issues and beads) plus the PR title and body.
// Any edit to those changes the revision, so a cached verdict is only reused
// while both the head SHA and the intent it was judged against are unchanged.
func IntentRevision(ac AlignmentContext) string {
	h := sha256.New()
	for _, part := range []string{ac.AuthorizedIntent, ac.PRTitle, ac.PRBody} {
		h.Write([]byte(strings.TrimSpace(part)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// alignmentCacheKey is (model, head SHA, intent revision). When no head SHA
// is known the diff summary stands in for it, so the entry is still bound to
// the exact file set that was judged rather than to the PR number alone.
func alignmentCacheKey(model string, ac AlignmentContext) string {
	head := strings.TrimSpace(ac.HeadSHA)
	if head == "" {
		sum := sha256.Sum256([]byte(ac.DiffSummary))
		head = "diff:" + hex.EncodeToString(sum[:])
	}
	return strings.TrimSpace(model) + "|" + head + "|" + IntentRevision(ac)
}

type alignmentCacheEntry struct {
	key     string
	verdict ModelAlignmentVerdict
}

type alignmentCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
	stats AlignmentCacheStats
}

func newAlignmentCache(max int) *alignmentCache {
	if max <= 0 {
		max = DefaultAlignmentCacheSize
	}
	return &alignmentCache{max: max, order: list.New(), items: make(map[string]*list.Element, max)}
}

func (c *alignmentCache) get(key string) (ModelAlignmentVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.stats.Misses++
		return ModelAlignmentVerdict{}, false
	}
	c.order.MoveToFront(el)
	c.stats.Hits++
	return el.Value.(*alignmentCacheEntry).verdict, true
}

func (c *alignmentCache) put(key string, v ModelAlignmentVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*alignmentCacheEntry).verdict = v
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&alignmentCacheEntry{key: key, verdict: v})
	c.items[key] = el
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*alignmentCacheEntry).key)
	}
}

func (c *alignmentCache) snapshot() AlignmentCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Size = c.order.Len()
	return s
}

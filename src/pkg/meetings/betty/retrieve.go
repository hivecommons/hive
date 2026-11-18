package betty

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// Retrieval defaults.
const (
	// DefaultRetrieveTimeout bounds one parallel retrieval.
	DefaultRetrieveTimeout = 1500 * time.Millisecond
	// retrieveLimit is the hits each retriever returns.
	retrieveLimit = 5
	// maxSnippetRunes bounds one snippet's text.
	maxSnippetRunes = 240
	// codeMapTag marks generated code map facts (knowledge.codeMapTag).
	codeMapTag = "codemap"
)

// Snippet is one retrieval hit: data for the model, never instructions.
type Snippet struct {
	Source string `json:"source"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	// URL is the page the snippet came from, when it has one: shown to the
	// meeting in chat after a spoken answer, never spoken (links.go).
	URL string `json:"url,omitempty"`
}

// Retriever is one fast, in-memory context source. Web search (phase B)
// plugs in as another Retriever.
type Retriever interface {
	Name() string
	Retrieve(ctx context.Context, query string, terms []string, limit int) []Snippet
}

// PublicRetriever marks a retriever whose query leaves the hive for a
// third party (a search engine, Wikipedia, a weather or release API).
type PublicRetriever interface {
	Retriever
	// Public reports that the retriever sends the question off the hive.
	Public() bool
}

// withoutPublic drops the retrievers that reach the public web.
func withoutPublic(rs []Retriever) []Retriever {
	out := make([]Retriever, 0, len(rs))
	for _, r := range rs {
		if pr, ok := r.(PublicRetriever); ok && pr.Public() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Gather runs every retriever in parallel and returns what arrived before
// the deadline, in retriever order.
func Gather(ctx context.Context, rs []Retriever, query string, terms []string, timeout time.Duration) []Snippet {
	if len(rs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	results := make([][]Snippet, len(rs))
	done := make(chan int, len(rs))
	var mu sync.Mutex
	for i, r := range rs {
		go func() {
			hits := r.Retrieve(ctx, query, terms, retrieveLimit)
			mu.Lock()
			results[i] = hits
			mu.Unlock()
			done <- i
		}()
	}
	for n := 0; n < len(rs); n++ {
		select {
		case i := <-done:
			// The hive's live state answers a question about the hive on
			// its own: do not wait for slower sources such as web search
			// (#18590, a live-state answer within HiveStatusBudget).
			mu.Lock()
			live := hasHiveStatus(results[i])
			mu.Unlock()
			if live {
				n = len(rs)
			}
		case <-ctx.Done():
			n = len(rs)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var out []Snippet
	for _, hits := range results {
		out = append(out, hits...)
	}
	return out
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// FactStore is the slice of *knowledge.FileStore retrieval uses.
type FactStore interface {
	Search(query string, limit int) []knowledge.Fact
	ListPages(tagFilter string) []knowledge.Fact
	ReadPage(slug string) (*knowledge.Fact, error)
}

// FileStores adapts KnowledgeAPI.FileStores to retrieval.
func FileStores(list func() []*knowledge.FileStore) func() []FactStore {
	return func() []FactStore {
		var out []FactStore
		for _, s := range list() {
			if s != nil {
				out = append(out, s)
			}
		}
		return out
	}
}

// KnowledgeRetriever searches the in-memory knowledge vaults
// (KnowledgeAPI.FileStores). Remote wiki layers are slow and never
// searched. Code maps are left to CodeMapRetriever.
type KnowledgeRetriever struct {
	Stores func() []FactStore
}

// Name implements Retriever.
func (KnowledgeRetriever) Name() string { return "knowledge" }

// Retrieve implements Retriever.
func (k KnowledgeRetriever) Retrieve(ctx context.Context, query string, _ []string, limit int) []Snippet {
	if k.Stores == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	var out []Snippet
	for _, s := range k.Stores() {
		if ctx.Err() != nil || len(out) >= limit {
			break
		}
		// Content words, page-text ranking and the best passage
		// (knowledge_docs.go).
		out = append(out, knowledgeHits(s, query, limit-len(out))...)
	}
	return out
}

// CodeMapRetriever returns code map lines (generated repository maps in a
// knowledge vault) that mention the question's words.
type CodeMapRetriever struct {
	Stores func() []FactStore
}

// Name implements Retriever.
func (CodeMapRetriever) Name() string { return "code-map" }

// Retrieve implements Retriever.
func (c CodeMapRetriever) Retrieve(ctx context.Context, _ string, terms []string, limit int) []Snippet {
	if c.Stores == nil || len(terms) == 0 {
		return nil
	}
	type hit struct {
		s     Snippet
		score int
	}
	var hits []hit
	for _, s := range c.Stores() {
		for _, page := range s.ListPages(codeMapTag) {
			if ctx.Err() != nil {
				return nil
			}
			full, err := s.ReadPage(page.Slug)
			if err != nil || full == nil {
				continue
			}
			for _, line := range strings.Split(full.Body, "\n") {
				lw := strings.ToLower(line)
				score := 0
				for _, t := range terms {
					if strings.Contains(lw, t) {
						score++
					}
				}
				if score > 0 {
					hits = append(hits, hit{Snippet{Source: "code map", Title: full.Title, Text: clip(line, maxSnippetRunes)}, score})
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Snippet
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].s)
	}
	return out
}

// BeadRetriever scans the in-memory bead stores (work items) for the
// question's words in titles and notes.
type BeadRetriever struct {
	Stores func() map[string]*beads.Store
}

// Name implements Retriever.
func (BeadRetriever) Name() string { return "beads" }

// titleWeight makes a title match count more than a notes match.
const titleWeight = 2

// Retrieve implements Retriever.
func (b BeadRetriever) Retrieve(ctx context.Context, _ string, terms []string, limit int) []Snippet {
	if b.Stores == nil || len(terms) == 0 {
		return nil
	}
	type hit struct {
		s     Snippet
		score int
	}
	stores := b.Stores()
	names := make([]string, 0, len(stores))
	for n := range stores {
		names = append(names, n)
	}
	sort.Strings(names)
	var hits []hit
	for _, n := range names {
		st := stores[n]
		if st == nil || ctx.Err() != nil {
			continue
		}
		for _, bd := range st.List(beads.ListFilter{}) {
			title, notes := strings.ToLower(bd.Title), strings.ToLower(bd.Notes)
			score := 0
			for _, t := range terms {
				if strings.Contains(title, t) {
					score += titleWeight
				}
				if strings.Contains(notes, t) {
					score++
				}
			}
			if score == 0 {
				continue
			}
			text := string(bd.Status)
			if bd.Notes != "" {
				text += ": " + bd.Notes
			}
			hits = append(hits, hit{Snippet{Source: "work item " + bd.ID, Title: bd.Title, Text: clip(text, maxSnippetRunes)}, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Snippet
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].s)
	}
	return out
}

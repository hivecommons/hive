package betty

import (
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Knowledge retrieval for the persona. Three things kept the hive's own
// docs and notes out of her answers: git sources were never searched (only
// vaults were listed), the whole utterance was the query (so "tell", "me"
// and "which" earned title bonuses), and a hit carried the first lines of
// its page rather than the passage that answers. The helpers here list
// every in-memory store, search with the question's content words, rank
// hits by those words in the page itself, and quote the best passage.

const (
	// knowledgeOverfetch is how many times the wanted hits are asked of a
	// store, so ranking by the page text has candidates to choose from.
	knowledgeOverfetch = 3
	// maxPassageRunes bounds a quoted passage; a docs paragraph is longer
	// than a fact's opening lines.
	maxPassageRunes = 480
	// minStemLen is the shortest word whose plural "s" is folded.
	minStemLen = 4
	// minContentWordLen is the shortest word that counts as content.
	minContentWordLen = 3
	// MeetingsVault is the vault meeting notes (and what the persona said
	// in them) are filed in (live.DefaultVault). Persona retrieval never
	// reads it: one meeting's notes and replies must not be quoted to the
	// next meeting's participants.
	MeetingsVault = "meetings"
	// meetingNotesTag marks a meeting-notes fact wherever it was filed.
	meetingNotesTag = "meeting-notes"
)

// KnowledgeSource is the slice of *knowledge.KnowledgeAPI the persona's
// retrieval lists stores from.
type KnowledgeSource interface {
	FileStores() []*knowledge.FileStore
	GitSources() []knowledge.GitSourceInfo
	GetGitSourceStore(name string) *knowledge.FileStore
}

// KnowledgeStores lists every in-memory store of a hive for retrieval: its
// vaults and its ready git sources (the hive's own docs are one). Remote
// wiki layers are slow and stay out. The meetings vault is never listed.
func KnowledgeStores(k KnowledgeSource) func() []FactStore {
	return func() []FactStore {
		var out []FactStore
		seen := map[*knowledge.FileStore]bool{}
		add := func(s *knowledge.FileStore) {
			if s != nil && !seen[s] && s.Name() != MeetingsVault {
				seen[s] = true
				out = append(out, s)
			}
		}
		for _, s := range k.FileStores() {
			add(s)
		}
		for _, info := range k.GitSources() {
			add(k.GetGitSourceStore(info.Name))
		}
		return out
	}
}

// stem folds a plural "s" so "competitors" finds "competitor".
func stem(w string) string {
	if len(w) >= minStemLen && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

// contentWords are a question's words that carry its topic: stopwords,
// framing and short words dropped, possessives folded, in order.
func contentWords(query string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range words(cleanQuery(query).Query) {
		w = strings.TrimSuffix(strings.Trim(w, "'"), "'s")
		if len([]rune(w)) < minContentWordLen || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// knowledgeQuery is what a store is searched with: the content words, or
// the question as asked when it has none.
func knowledgeQuery(query string) string {
	if w := contentWords(query); len(w) > 0 {
		return strings.Join(w, " ")
	}
	return query
}

// wordHits counts the stems that text mentions.
func wordHits(text string, stems []string) int {
	low := strings.ToLower(text)
	n := 0
	for _, s := range stems {
		if strings.Contains(low, s) {
			n++
		}
	}
	return n
}

// bestPassage is the paragraph of body that mentions the most stems, and
// how many; the body's opening when none does.
func bestPassage(body string, stems []string) (string, int) {
	best, score := "", 0
	for _, para := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if n := wordHits(para, stems); n > score {
			best, score = para, n
		}
	}
	if score == 0 {
		return clip(body, maxSnippetRunes), 0
	}
	return clip(best, maxPassageRunes), score
}

// knowledgeHits searches one store and returns up to limit snippets, the
// pages that mention most of the question's words first, each quoting its
// best passage. Code maps are left to CodeMapRetriever.
func knowledgeHits(s FactStore, query string, limit int) []Snippet {
	if limit <= 0 {
		return nil
	}
	var stems []string
	for _, w := range contentWords(query) {
		stems = append(stems, stem(w))
	}
	type hit struct {
		s     Snippet
		score int
	}
	var hits []hit
	for _, f := range s.Search(knowledgeQuery(query), limit*knowledgeOverfetch) {
		if hasTag(f.Tags, codeMapTag) || hasTag(f.Tags, meetingNotesTag) {
			continue
		}
		body := f.Body
		// Search returns a page's opening lines; the passage that answers
		// may be further down.
		if full, err := s.ReadPage(f.Slug); err == nil && full != nil && full.Body != "" {
			body = full.Body
		}
		text, score := bestPassage(body, stems)
		hits = append(hits, hit{Snippet{Source: "knowledge", Title: f.Title, Text: text}, score + wordHits(f.Title, stems)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Snippet
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].s)
	}
	return out
}

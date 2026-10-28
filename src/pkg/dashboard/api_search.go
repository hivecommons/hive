package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/search"
)

// Portfolio search (hivecommons/hive#14637, RFC #14491 Part A): the spoke's
// GET /api/search over the local pkg/search index. Owner/maintainer only,
// spoke-local (no hub fan-out), and 404 while the search index is off. Query
// strings are audited with actor and role; result bodies never are. Every
// text field surfaced from the index passes through ioscan as untrusted
// input, so injected text reaches a dashboard, chat or agent consumer only as
// a visible redaction marker.

// searchFacetFields are the facets every response carries, in this order.
var searchFacetFields = []string{search.FieldKind, search.FieldRepo, "verdict", "sentinel.rule"}

const searchMaxLimit = 100

// searchBackendState holds the backend the composition root wires once the
// indexer starts; nil until then.
type searchBackendState struct {
	mu      sync.RWMutex
	backend search.Backend
}

// SetSearchBackend wires the portfolio search backend GET /api/search reads.
func (s *Server) SetSearchBackend(b search.Backend) {
	s.portfolioSearch.mu.Lock()
	s.portfolioSearch.backend = b
	s.portfolioSearch.mu.Unlock()
}

// SearchBackend returns the wired search backend, or nil.
func (s *Server) SearchBackend() search.Backend {
	s.portfolioSearch.mu.RLock()
	defer s.portfolioSearch.mu.RUnlock()
	return s.portfolioSearch.backend
}

func (s *Server) searchEnabled() bool {
	return s.deps != nil && s.deps.Config != nil && s.deps.Config.Search.IsEnabled()
}

type searchHighlight struct {
	Snippet string   `json:"snippet"`
	Spans   [][2]int `json:"spans"`
}

type searchHit struct {
	ID         string            `json:"id"`
	Repo       string            `json:"repo"`
	Kind       string            `json:"kind"`
	At         time.Time         `json:"at"`
	URL        string            `json:"url"`
	Visibility string            `json:"visibility"`
	Title      string            `json:"title,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
	Score      int               `json:"score"`
	Terms      []string          `json:"terms"`
	Highlight  searchHighlight   `json:"highlight"`
	// Redacted is set when ioscan withheld any of the hit's text.
	Redacted bool `json:"redacted,omitempty"`
}

type searchFacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type searchResponse struct {
	Query  string                        `json:"query"`
	Total  int                           `json:"total"`
	Hits   []searchHit                   `json:"hits"`
	Facets map[string][]searchFacetCount `json:"facets"`
	// Coverage names repos whose index could not be read: their absence from
	// hits is not evidence that nothing matches there.
	Coverage map[string]string `json:"coverage,omitempty"`
}

// searchListParam splits repeated and comma-separated values.
func searchListParam(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// searchTime parses an RFC 3339 timestamp or a YYYY-MM-DD date (UTC). A
// date-only upper bound covers that whole day.
func searchTime(raw string, upper bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return time.Time{}, err
	}
	if upper {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// parseSearchQuery builds the backend query from ?q=&kind=&repo=&from=&to=
// &limit=&semantic=.
func parseSearchQuery(r *http.Request) (search.Query, error) {
	params := r.URL.Query()
	if raw := strings.TrimSpace(params.Get("semantic")); raw != "" {
		semantic, err := strconv.ParseBool(raw)
		if err != nil || semantic {
			return search.Query{}, errors.New("semantic search is reserved and not available (#14491 Part C)")
		}
	}
	q := search.Query{
		Text:    strings.TrimSpace(params.Get("q")),
		Filters: map[string][]string{},
		Facets:  []string{"verdict", "sentinel.rule"},
		Limit:   search.DefaultLimit,
	}
	if kinds := searchListParam(params["kind"]); len(kinds) > 0 {
		q.Filters[search.FieldKind] = kinds
	}
	if repos := searchListParam(params["repo"]); len(repos) > 0 {
		for _, repo := range repos {
			owner, name, ok := strings.Cut(repo, "/")
			if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return search.Query{}, errors.New("repo must be owner/repo")
			}
		}
		q.Filters[search.FieldRepo] = repos
	}
	var err error
	if raw := strings.TrimSpace(params.Get("from")); raw != "" {
		if q.From, err = searchTime(raw, false); err != nil {
			return search.Query{}, errors.New("from must be RFC 3339 or YYYY-MM-DD")
		}
	}
	if raw := strings.TrimSpace(params.Get("to")); raw != "" {
		if q.To, err = searchTime(raw, true); err != nil {
			return search.Query{}, errors.New("to must be RFC 3339 or YYYY-MM-DD")
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return search.Query{}, errors.New("from must be before to")
	}
	if raw := strings.TrimSpace(params.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > searchMaxLimit {
			return search.Query{}, errors.New("limit must be between 1 and " + strconv.Itoa(searchMaxLimit))
		}
		q.Limit = n
	}
	return q, nil
}

// searchSafeText scans index text as untrusted input; blocked text comes back
// as ioscan's visible redaction marker.
func searchSafeText(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	out, v := ioscan.EnforceInput(text)
	return out, v.Blocked
}

func newSearchHit(h search.Hit) searchHit {
	d := h.Document
	hit := searchHit{
		ID: d.ID, Repo: d.Repo, Kind: d.Kind, At: d.At, URL: d.URL, Visibility: d.Visibility,
		Score: h.Score, Terms: h.Terms,
		Highlight: searchHighlight{Snippet: h.Highlight.Snippet, Spans: h.Highlight.Spans},
	}
	if hit.Terms == nil {
		hit.Terms = []string{}
	}
	if hit.Highlight.Spans == nil {
		hit.Highlight.Spans = [][2]int{}
	}
	var blocked bool
	hit.Title, blocked = searchSafeText(d.Title)
	hit.Redacted = blocked
	if hit.Highlight.Snippet, blocked = searchSafeText(h.Highlight.Snippet); blocked {
		hit.Highlight.Spans = [][2]int{}
		hit.Redacted = true
	}
	if len(d.Fields) > 0 {
		hit.Fields = make(map[string]string, len(d.Fields))
		for k, v := range d.Fields {
			if hit.Fields[k], blocked = searchSafeText(v); blocked {
				hit.Redacted = true
			}
		}
	}
	return hit
}

// auditSearchQuery records who searched for what. Result bodies are never
// audited; only the match count.
func (s *Server) auditSearchQuery(r *http.Request, q search.Query, outcome string) {
	kv := []string{"role", strconv.Quote(r.Header.Get("X-Hive-Role")), "q", strconv.Quote(q.Text)}
	if kinds := q.Filters[search.FieldKind]; len(kinds) > 0 {
		kv = append(kv, "kind", strconv.Quote(strings.Join(kinds, ",")))
	}
	if repos := q.Filters[search.FieldRepo]; len(repos) > 0 {
		kv = append(kv, "repo", strconv.Quote(strings.Join(repos, ",")))
	}
	if !q.From.IsZero() {
		kv = append(kv, "from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		kv = append(kv, "to", q.To.UTC().Format(time.RFC3339))
	}
	kv = append(kv, "outcome", outcome)
	s.auditFromRequest(r, "search_query", auditDetail(kv...), "")
}

// handleSearch serves GET /api/search?q=&kind=&repo=&from=&to=[&limit=]:
// highlighted hits and facets by kind, repo, verdict and sentinel.rule from
// this spoke's portfolio search index. kind and repo accept repeated or
// comma-separated values; from is inclusive and to exclusive (a YYYY-MM-DD
// to covers that day). semantic=true is reserved and rejected.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	if !s.searchEnabled() {
		jsonError(w, "search not enabled", http.StatusNotFound)
		return
	}
	q, err := parseSearchQuery(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	backend := s.SearchBackend()
	if backend == nil {
		s.auditSearchQuery(r, q, "unavailable")
		jsonError(w, "search index unavailable", http.StatusServiceUnavailable)
		return
	}
	res, err := backend.Query(r.Context(), q)
	if err != nil {
		s.auditSearchQuery(r, q, "error")
		jsonError(w, "search failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditSearchQuery(r, q, "total="+strconv.Itoa(res.Total))

	resp := searchResponse{
		Query:  q.Text,
		Total:  res.Total,
		Hits:   make([]searchHit, 0, len(res.Hits)),
		Facets: make(map[string][]searchFacetCount, len(searchFacetFields)),
	}
	for _, h := range res.Hits {
		resp.Hits = append(resp.Hits, newSearchHit(h))
	}
	for _, field := range searchFacetFields {
		counts := make([]searchFacetCount, 0, len(res.Facets[field]))
		for _, c := range res.Facets[field] {
			value, _ := searchSafeText(c.Value)
			counts = append(counts, searchFacetCount{Value: value, Count: c.Count})
		}
		resp.Facets[field] = counts
	}
	if len(res.Coverage) > 0 {
		resp.Coverage = make(map[string]string, len(res.Coverage))
		for repo, cerr := range res.Coverage {
			resp.Coverage[repo] = "unreadable"
			if cerr != nil {
				resp.Coverage[repo] = cerr.Error()
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, resp)
}

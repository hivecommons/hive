package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	context7BaseURL       = "https://context7.com"
	context7SearchPath    = "/api/v2/libs/search"
	context7LLMsTxtSuffix = "/llms.txt"
	context7Timeout       = 30 * time.Second
	context7MaxBody       = 10 * 1024 * 1024 // 10 MB
	context7DefaultTokens = 10000
)

// Context7SearchResult is a single library match from Context7's search API.
type Context7SearchResult struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Snippets    int     `json:"totalSnippets,omitempty"`
	Trust       float64 `json:"trustScore,omitempty"`
	Score       float64 `json:"benchmarkScore,omitempty"`
	Stars       int     `json:"stars,omitempty"`
}

type context7SearchResponse struct {
	Results []Context7SearchResult `json:"results"`
}

// Context7DocsResult holds the documentation content returned by Context7.
// Content is bounded by Context7CapsFromEnv: snippets that did not fit were
// dropped whole and Content ends with a marker line when any were. Omitted
// reports what was dropped so callers can log it.
type Context7DocsResult struct {
	LibraryID string
	Title     string
	Content   string
	Omitted   Context7Omitted
}

// Context7Omitted reports snippets a cap dropped from a Context7 fetch.
type Context7Omitted struct {
	Snippets int
	Bytes    int
}

// Any reports whether anything was dropped.
func (o Context7Omitted) Any() bool { return o.Snippets > 0 }

// Context7 content caps. The llms.txt body Context7 returns is a sequence of
// snippets (TITLE / DESCRIPTION / SOURCE / CODE blocks separated by a dashed
// rule). Before #12124 the whole body — up to context7MaxBody — landed in the
// vault and from there in kicks. These caps bound it the way CodeMapCaps bound
// a code map: whole snippets are kept or dropped, never clipped mid-block, and
// the result says how much was left out.
const (
	// Context7DefaultMaxSnippets bounds how many snippets one fetch keeps.
	Context7DefaultMaxSnippets = 40
	// Context7DefaultMaxSnippetBytes drops any single snippet larger than this,
	// so one huge code block cannot consume the whole budget.
	Context7DefaultMaxSnippetBytes = 4096
	// Context7DefaultMaxTotalBytes bounds the kept content, marker included.
	Context7DefaultMaxTotalBytes = 16384
	// context7MinTotalBytes is the floor a configured total is raised to.
	context7MinTotalBytes = 1024
	// context7FooterReserve keeps room for the final marker line.
	context7FooterReserve = 96

	// Environment overrides. Context7 fetches run from import paths that have
	// no config handle, so the override is an env var rather than a yaml key.
	Context7EnvMaxSnippets     = "HIVE_CONTEXT7_MAX_SNIPPETS"
	Context7EnvMaxSnippetBytes = "HIVE_CONTEXT7_MAX_SNIPPET_BYTES"
	Context7EnvMaxTotalBytes   = "HIVE_CONTEXT7_MAX_TOTAL_BYTES"

	// context7SnippetSeparator is the rule Context7 puts between snippets in
	// an llms.txt body.
	context7SnippetSeparator = "----------------------------------------"
	// context7MinQueryTermLen skips stop-word-length query terms when scoring
	// snippets for relevance.
	context7MinQueryTermLen = 3
)

// Context7TruncatedMarker prefixes the line that reports dropped snippets; it
// is the same marker the code map and primer use.
const Context7TruncatedMarker = CodeMapTruncatedMarker

// Context7Caps bounds the content kept from one Context7 fetch. Zero fields
// take the matching default.
type Context7Caps struct {
	MaxSnippets     int
	MaxSnippetBytes int
	MaxTotalBytes   int
}

// DefaultContext7Caps are the caps used when nothing is configured.
var DefaultContext7Caps = Context7Caps{
	MaxSnippets:     Context7DefaultMaxSnippets,
	MaxSnippetBytes: Context7DefaultMaxSnippetBytes,
	MaxTotalBytes:   Context7DefaultMaxTotalBytes,
}

func (c Context7Caps) withDefaults() Context7Caps {
	if c.MaxSnippets <= 0 {
		c.MaxSnippets = DefaultContext7Caps.MaxSnippets
	}
	if c.MaxSnippetBytes <= 0 {
		c.MaxSnippetBytes = DefaultContext7Caps.MaxSnippetBytes
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = DefaultContext7Caps.MaxTotalBytes
	}
	if c.MaxTotalBytes < context7MinTotalBytes {
		c.MaxTotalBytes = context7MinTotalBytes
	}
	return c
}

// Context7CapsFromEnv returns DefaultContext7Caps with any HIVE_CONTEXT7_MAX_*
// environment overrides applied. Unparseable or non-positive values are
// ignored, so a bad override degrades to the default rather than to no cap.
func Context7CapsFromEnv() Context7Caps {
	caps := DefaultContext7Caps
	if n, ok := context7EnvInt(Context7EnvMaxSnippets); ok {
		caps.MaxSnippets = n
	}
	if n, ok := context7EnvInt(Context7EnvMaxSnippetBytes); ok {
		caps.MaxSnippetBytes = n
	}
	if n, ok := context7EnvInt(Context7EnvMaxTotalBytes); ok {
		caps.MaxTotalBytes = n
	}
	return caps.withDefaults()
}

func context7EnvInt(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func context7TruncLine(o Context7Omitted) string {
	return fmt.Sprintf("\n%s\n\n%s %d snippets / %d bytes omitted (size cap)\n", context7SnippetSeparator, Context7TruncatedMarker, o.Snippets, o.Bytes)
}

// splitContext7Snippets splits an llms.txt body on Context7's dashed rule.
// A body with no rule (plain prose) falls back to blank-line paragraphs, so
// there is always a boundary to drop at other than the whole document.
func splitContext7Snippets(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	var raw []string
	if strings.Contains(body, context7SnippetSeparator) {
		raw = strings.Split(body, context7SnippetSeparator)
	} else {
		raw = strings.Split(body, "\n\n")
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// context7QueryTerms lowercases and splits query, dropping very short terms.
func context7QueryTerms(query string) []string {
	var terms []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if len(t) >= context7MinQueryTermLen {
			terms = append(terms, t)
		}
	}
	return terms
}

// CapContext7Content bounds body to caps, preferring snippets that mention
// the query. Snippets are scored by how many query terms they contain and
// visited best-first (ties keep document order); each is kept only if it is
// within MaxSnippetBytes and fits the remaining total, otherwise it is
// dropped whole and later snippets are still considered. Kept snippets are
// emitted in their original document order so the result still reads like
// the source. When anything was dropped the content ends with a marker line.
func CapContext7Content(body, query string, caps Context7Caps) (string, Context7Omitted) {
	caps = caps.withDefaults()
	var omitted Context7Omitted
	snippets := splitContext7Snippets(body)
	if len(snippets) == 0 {
		return "", omitted
	}

	terms := context7QueryTerms(query)
	type ranked struct {
		idx   int
		score int
	}
	order := make([]ranked, len(snippets))
	for i, sn := range snippets {
		lower := strings.ToLower(sn)
		score := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				score++
			}
		}
		order[i] = ranked{idx: i, score: score}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].score > order[j].score })

	sep := "\n\n" + context7SnippetSeparator + "\n\n"
	remaining := caps.MaxTotalBytes - context7FooterReserve
	keep := make([]bool, len(snippets))
	kept := 0
	for _, r := range order {
		sn := snippets[r.idx]
		need := len(sn)
		if kept > 0 {
			need += len(sep)
		}
		if kept >= caps.MaxSnippets || len(sn) > caps.MaxSnippetBytes || need > remaining {
			omitted.Snippets++
			omitted.Bytes += len(sn)
			continue
		}
		keep[r.idx] = true
		kept++
		remaining -= need
	}

	var b strings.Builder
	first := true
	for i, sn := range snippets {
		if !keep[i] {
			continue
		}
		if !first {
			b.WriteString(sep)
		}
		first = false
		b.WriteString(sn)
	}
	if omitted.Any() {
		b.WriteString(context7TruncLine(omitted))
	}
	return b.String(), omitted
}

// SearchContext7 resolves a library name to Context7 IDs.
func SearchContext7(ctx context.Context, name, apiKey string) ([]Context7SearchResult, error) {
	params := url.Values{
		"libraryName": {name},
		"query":       {name},
	}
	reqURL := context7BaseURL + context7SearchPath + "?" + params.Encode()

	body, err := context7Get(ctx, reqURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("context7 search: %w", err)
	}

	var wrapper context7SearchResponse
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("context7 search decode: %w", err)
	}
	return wrapper.Results, nil
}

// FetchContext7Docs retrieves documentation for a library via its llms.txt
// endpoint, which returns curated content optimized for LLM consumption. The
// body is bounded by CapContext7Content before it is returned; see
// Context7DocsResult.Omitted.
func FetchContext7Docs(ctx context.Context, libraryID, query, apiKey string) (*Context7DocsResult, error) {
	// Validate that libraryID starts with "/" so that concatenating it with
	// context7BaseURL cannot produce a URL with an attacker-controlled authority
	// component. For example, a libraryID of "@evil.com/path" would produce
	// "https://context7.com@evil.com/path/llms.txt" where evil.com is the host.
	if !strings.HasPrefix(libraryID, "/") {
		return nil, fmt.Errorf("context7 library ID must start with '/': %q", libraryID)
	}
	params := url.Values{
		"tokens": {fmt.Sprintf("%d", context7DefaultTokens)},
	}
	if query != "" {
		params.Set("topic", query)
	}
	reqURL := context7BaseURL + libraryID + context7LLMsTxtSuffix + "?" + params.Encode()

	body, err := context7Get(ctx, reqURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("context7 docs: %w", err)
	}

	content, omitted := CapContext7Content(string(body), query, Context7CapsFromEnv())
	return &Context7DocsResult{
		LibraryID: libraryID,
		Title:     libraryID,
		Content:   content,
		Omitted:   omitted,
	}, nil
}

// context7Client is an HTTP client that refuses to follow redirects to
// private/internal addresses. context7.com requests go to a fixed hardcoded
// base URL, but an adversary-controlled redirect (or a crafted libraryID)
// could pivot to cloud metadata or internal services without this guard.
var context7Client = &http.Client{
	Timeout:       context7Timeout,
	CheckRedirect: knowledgeNoRedirectToPrivate,
}

func context7Get(ctx context.Context, reqURL, apiKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, context7Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("User-Agent", "hive-knowledge/1.0")

	resp, err := context7Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited (429), retry after %s", resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Context7", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, context7MaxBody))
}

// logContext7Omitted is the log-side half of the Context7 cap: the content
// carries its own marker, this tells the operator which library hit it.
func logContext7Omitted(logger *slog.Logger, libraryID string, o Context7Omitted) {
	if !o.Any() {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("context7: dropped snippets over the content cap",
		"library", libraryID, "omitted_snippets", o.Snippets, "omitted_bytes", o.Bytes)
}

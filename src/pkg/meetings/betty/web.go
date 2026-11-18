package betty

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Web search defaults (hivecommons/hive#18393).
const (
	// DefaultWebTimeout bounds one SearXNG query, inside the retrieval
	// deadline.
	DefaultWebTimeout = 1200 * time.Millisecond
	// webResults is how many search results reach the model.
	webResults = 3
	// maxWebResponseBytes bounds the SearXNG response read.
	maxWebResponseBytes = 1 << 20
	// webCategories are the SearXNG categories searched.
	webCategories = "general,it"
	// webSource prefixes a web snippet's source.
	webSource = "web"
)

// htmlTag matches markup in result titles and snippets.
var htmlTag = regexp.MustCompile(`<[^>]*>`)

// stripHTML turns a result field into plain text: tags removed, entities
// decoded, whitespace collapsed, delimiters stripped.
func stripHTML(s string) string {
	s = htmlTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// A second pass catches markup that was entity-encoded.
	s = htmlTag.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(sanitize(s)), " ")
}

// WebRetriever searches the web through a self-hosted SearXNG instance
// (meetings.persona.searxng_url) with its JSON output, and asks the direct
// sources (direct.go) beside it. Results are untrusted data: they reach the
// model inside the data block, stripped of markup, and never as
// instructions. A question about the hive itself is never sent (WebGuard),
// and what is sent is the cleaned query, not the utterance.
type WebRetriever struct {
	// BaseURL is the SearXNG root, for example
	// http://searxng.meetings.svc.cluster.local:8080.
	BaseURL string
	Client  *http.Client
	// Timeout bounds one query; zero means DefaultWebTimeout.
	Timeout time.Duration
	// Guard keeps hive questions off the public web; nil still applies
	// the checks that need no configured names.
	Guard *WebGuard
	// Direct, when set, answers weather, release and definition questions
	// from keyless public APIs, in parallel with the search.
	Direct *DirectRetriever
	Logger *slog.Logger
}

// Name implements Retriever.
func (WebRetriever) Name() string { return webSource }

// Public implements PublicRetriever.
func (WebRetriever) Public() bool { return true }

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	// Infoboxes are entity summaries (the wikipedia and wikidata engines).
	Infoboxes []struct {
		Infobox string `json:"infobox"`
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"infoboxes"`
	// UnresponsiveEngines are [engine, reason] pairs.
	UnresponsiveEngines [][]string `json:"unresponsive_engines"`
}

// What the model is told when the web gave nothing.
const (
	webNoteDown  = "Web search is unavailable right now: the search service did not answer."
	webNoteDead  = "Web search is unavailable right now: none of the search engines responded."
	webNoteEmpty = "Web search returned nothing for this question."
	// searxngSource names the SearXNG search in the retrieval log.
	searxngSource = "searxng"
)

// Retrieve implements Retriever: the direct sources' answer, then the top
// three search results as title, url and snippet. When the question was
// searched and nothing came back, one WebUnavailableSource snippet says
// whether the search was down or empty.
func (w WebRetriever) Retrieve(ctx context.Context, query string, _ []string, limit int) []Snippet {
	if w.BaseURL == "" || limit <= 0 {
		return nil
	}
	c, blocked := w.Guard.check(query)
	if blocked != "" {
		logWebBlocked(w.Logger, webSource, blocked)
		return nil
	}
	if place, ok := weatherPlace(c.Query); ok && place == "" && w.Direct != nil {
		// No place was named: nothing to search for, she asks where.
		return []Snippet{needsPlaceSnippet()}
	}
	direct := make(chan []Snippet, 1)
	if w.Direct != nil {
		go func() { direct <- w.Direct.answer(ctx, c) }()
	} else {
		direct <- nil
	}
	if _, isWeather := weatherPlace(c.Query); isWeather && w.Direct != nil {
		// A weather answer is live data or nothing: search pages are
		// cached forecasts of other days and contradict each other, so
		// they are never offered as current conditions.
		if got := <-direct; len(got) > 0 {
			return got
		}
		return []Snippet{unavailableSnippet()}
	}
	hits, note := w.search(ctx, c.Query, limit)
	out := append(<-direct, hits...)
	if len(out) == 0 {
		return []Snippet{{Source: WebUnavailableSource, Title: "Web search", Text: note}}
	}
	return out
}

// search runs one SearXNG query: GET /search?format=json&q=... It returns
// the usable hits, and what to tell the model when there are none.
func (w WebRetriever) search(ctx context.Context, query string, limit int) ([]Snippet, string) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	sr, ok := w.fetch(ctx, query)
	var out []Snippet
	n := min(limit, webResults)
	add := func(rawURL, rawTitle, rawText string) {
		if len(out) >= n {
			return
		}
		link, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" {
			return
		}
		title, text := stripHTML(rawTitle), stripHTML(rawText)
		if title == "" && text == "" {
			return
		}
		out = append(out, Snippet{
			Source: webSource + " " + sanitize(link.Host),
			Title:  clip(title, maxSnippetRunes),
			Text:   clip(text, maxSnippetRunes) + " (" + clip(sanitize(link.String()), maxSnippetRunes) + ")",
			URL:    link.String(),
		})
	}
	for _, b := range sr.Infoboxes {
		if strings.TrimSpace(b.Content) != "" {
			add(b.ID, b.Infobox, b.Content)
		}
	}
	for _, r := range sr.Results {
		add(r.URL, r.Title, r.Content)
	}
	logWebRetrieval(w.Logger, searxngSource, query, len(out), time.Since(start), "answered", ok, "unresponsive_engines", len(sr.UnresponsiveEngines))
	switch {
	case !ok:
		return nil, webNoteDown
	case len(out) == 0 && len(sr.UnresponsiveEngines) > 0 && len(sr.Results) == 0:
		return nil, webNoteDead
	}
	return out, webNoteEmpty
}

// fetch gets and decodes one SearXNG response.
func (w WebRetriever) fetch(ctx context.Context, query string) (searxResponse, bool) {
	var sr searxResponse
	// The it category brings in GitHub and Stack Overflow, which SearXNG
	// leaves out of plain general searches.
	u := strings.TrimRight(w.BaseURL, "/") + "/search?format=json&categories=" + webCategories + "&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return sr, false
	}
	req.Header.Set("Accept", "application/json")
	hc := w.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return sr, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sr, false
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWebResponseBytes)).Decode(&sr); err != nil {
		return searxResponse{}, false
	}
	return sr, true
}

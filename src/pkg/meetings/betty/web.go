package betty

import (
	"context"
	"encoding/json"
	"html"
	"io"
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
	return sanitize(strings.Join(strings.Fields(s), " "))
}

// WebRetriever searches the web through a self-hosted SearXNG instance
// (meetings.persona.searxng_url) with its JSON output. Results are
// untrusted data: they reach the model inside the data block, stripped of
// markup, and never as instructions.
type WebRetriever struct {
	// BaseURL is the SearXNG root, for example
	// http://searxng.meetings.svc.cluster.local:8080.
	BaseURL string
	Client  *http.Client
	// Timeout bounds one query; zero means DefaultWebTimeout.
	Timeout time.Duration
}

// Name implements Retriever.
func (WebRetriever) Name() string { return webSource }

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Retrieve implements Retriever: GET /search?format=json&q=..., the top
// three results as title, url and snippet.
func (w WebRetriever) Retrieve(ctx context.Context, query string, _ []string, limit int) []Snippet {
	query = strings.TrimSpace(query)
	if w.BaseURL == "" || query == "" {
		return nil
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The it category brings in GitHub and Stack Overflow, which SearXNG
	// leaves out of plain general searches.
	u := strings.TrimRight(w.BaseURL, "/") + "/search?format=json&categories=" + webCategories + "&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	hc := w.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var sr searxResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWebResponseBytes)).Decode(&sr); err != nil {
		return nil
	}
	n := min(limit, webResults)
	var out []Snippet
	for _, r := range sr.Results {
		if len(out) >= n {
			break
		}
		link, err := url.Parse(strings.TrimSpace(r.URL))
		if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" {
			continue
		}
		title, text := stripHTML(r.Title), stripHTML(r.Content)
		if title == "" && text == "" {
			continue
		}
		out = append(out, Snippet{
			Source: webSource + " " + sanitize(link.Host),
			Title:  clip(title, maxSnippetRunes),
			Text:   clip(text, maxSnippetRunes) + " (" + clip(sanitize(link.String()), maxSnippetRunes) + ")",
		})
	}
	return out
}

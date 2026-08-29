package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
type Context7DocsResult struct {
	LibraryID string
	Title     string
	Content   string
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
// endpoint, which returns curated content optimized for LLM consumption.
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

	return &Context7DocsResult{
		LibraryID: libraryID,
		Title:     libraryID,
		Content:   string(body),
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

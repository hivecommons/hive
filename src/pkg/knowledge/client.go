package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultTimeout    = 10 * time.Second
	searchResultLimit = 50
)

// Client talks to a single llm-wiki HTTP instance.
type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewClient creates a wiki client for the given HTTP endpoint.
func NewClient(baseURL string, logger *slog.Logger) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout:       defaultTimeout,
			CheckRedirect: knowledgeNoRedirectToPrivate,
		},
		logger: logger,
	}
}

// searchResponse matches llm-wiki's wiki_search MCP tool output.
type searchResponse struct {
	Results []searchResult `json:"results"`
	Total   int            `json:"total"`
}

type searchResult struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Score      float64  `json:"score"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	Snippet    string   `json:"snippet"`
}

// pageResponse matches llm-wiki's wiki_content_read output.
type pageResponse struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	Sources    []string `json:"sources"`
	Backlinks  []string `json:"backlinks"`
}

// statsResponse matches llm-wiki's wiki_stats output.
type statsResponse struct {
	TotalPages int            `json:"total_pages"`
	ByType     map[string]int `json:"by_type"`
	ByStatus   map[string]int `json:"by_status"`
	Stale      int            `json:"stale"`
	Orphaned   int            `json:"orphaned"`
}

// Search queries the wiki with BM25 ranked search.
func (c *Client) Search(ctx context.Context, query string, typeFilter string, limit int) ([]searchResult, error) {
	if limit <= 0 {
		limit = searchResultLimit
	}

	params := url.Values{
		"q":     {query},
		"limit": {fmt.Sprintf("%d", limit)},
	}
	if typeFilter != "" {
		params.Set("type", typeFilter)
	}

	var resp searchResponse
	if err := c.get(ctx, "/api/search", params, &resp); err != nil {
		return nil, fmt.Errorf("wiki search: %w", err)
	}

	return resp.Results, nil
}

// ReadPage fetches a single wiki page by slug.
func (c *Client) ReadPage(ctx context.Context, slug string) (*pageResponse, error) {
	var resp pageResponse
	if err := c.get(ctx, "/api/pages/"+slug, nil, &resp); err != nil {
		return nil, fmt.Errorf("wiki read %s: %w", slug, err)
	}
	return &resp, nil
}

// Stats returns aggregate wiki health statistics.
func (c *Client) Stats(ctx context.Context) (*statsResponse, error) {
	var resp statsResponse
	if err := c.get(ctx, "/api/stats", nil, &resp); err != nil {
		return nil, fmt.Errorf("wiki stats: %w", err)
	}
	return &resp, nil
}

// ListPages returns all pages, optionally filtered by type.
func (c *Client) ListPages(ctx context.Context, typeFilter string) ([]searchResult, error) {
	params := url.Values{}
	if typeFilter != "" {
		params.Set("type", typeFilter)
	}

	var resp searchResponse
	if err := c.get(ctx, "/api/pages", params, &resp); err != nil {
		return nil, fmt.Errorf("wiki list: %w", err)
	}

	return resp.Results, nil
}

// Healthy returns true if the wiki endpoint is reachable.
func (c *Client) Healthy(ctx context.Context) bool {
	err := c.get(ctx, "/api/stats", nil, &statsResponse{})
	return err == nil
}

// IngestFacts sends facts to the wiki for storage.
func (c *Client) IngestFacts(ctx context.Context, facts []ExtractedFact) error {
	return c.postJSON(ctx, "/api/ingest", facts)
}

// UpdatePage updates an existing wiki page by slug.
func (c *Client) UpdatePage(ctx context.Context, slug string, page pageUpdateRequest) error {
	return c.postJSON(ctx, "/api/pages/"+slug, page)
}

// DeletePage removes a wiki page by slug.
func (c *Client) DeletePage(ctx context.Context, slug string) error {
	reqCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, c.baseURL+"/api/pages/"+slug, nil)
	if err != nil {
		return fmt.Errorf("creating delete request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

type pageUpdateRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	Status     string   `json:"status"`
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request to %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, string(body))
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, dest any) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request to %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decoding response from %s: %w", path, err)
	}

	return nil
}

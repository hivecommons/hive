// Package ask is the backend for the optional Ask AI help (#19177): a
// server-side call to the vendor's Chat API, source filtering, limits, a
// circuit breaker and thread bookkeeping. It holds no HTTP route and no
// consent record; the hub wires those around it. See
// src/docs/design/dashboard-ask-ai-help.md.
//
// Everything that leaves the deployment is built in one place, Client.Chat,
// from named inputs, so a test against a fake vendor server can assert the
// exact request body.
package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the vendor's public API host.
const DefaultBaseURL = "https://api.kapa.ai"

// maxResponseBytes bounds how much of a vendor response is read.
const maxResponseBytes = 1 << 20

// Errors the client returns. None carries a request value, a response body, the
// key or a vendor identifier.
var (
	// ErrRateLimited is the vendor's HTTP 429.
	ErrRateLimited = errors.New("ask: vendor rate limited")
	// ErrUnavailable is a timeout, a transport failure or a 5xx.
	ErrUnavailable = errors.New("ask: vendor unavailable")
	// ErrRejected is any other non-200 answer (a bad key, a bad request).
	ErrRejected = errors.New("ask: vendor rejected the request")
	// ErrBadResponse is a 200 whose body is not the pinned shape.
	ErrBadResponse = errors.New("ask: vendor response not understood")
)

// chatRequest is the complete set of fields sent to the vendor. Adding a field
// here is adding to "What leaves the deployment" in the design record: it needs
// a reviewer and a test. In particular there is no user object, no metadata and
// no origin URL.
type chatRequest struct {
	IntegrationID string `json:"integration_id"`
	Query         string `json:"query"`
}

// Source is one cited source of an answer.
type Source struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// ChatResponse is the non-streamed response shape, pinned by a contract test
// against the vendor's API reference (POST /query/v1/projects/{project_id}/chat/
// and POST /query/v1/threads/{thread_id}/chat/): answer, thread_id,
// question_answer_id, is_uncertain and relevant_sources[{source_url, title,
// contains_internal_data}].
type ChatResponse struct {
	Answer           string         `json:"answer"`
	ThreadID         string         `json:"thread_id"`
	QuestionAnswerID string         `json:"question_answer_id"`
	IsUncertain      bool           `json:"is_uncertain"`
	RelevantSources  []VendorSource `json:"relevant_sources"`
}

// VendorSource is one entry of relevant_sources as the vendor returns it.
type VendorSource struct {
	SourceURL            string `json:"source_url"`
	Title                string `json:"title"`
	ContainsInternalData bool   `json:"contains_internal_data"`
}

// Client calls the vendor Chat API. The zero value is not usable; build one
// with NewClient. The key, project id and integration id are held in
// unexported fields, are never part of a returned error, and are not written to
// any log by this package.
type Client struct {
	baseURL       string
	projectID     string
	integrationID string
	apiKey        string
	http          *http.Client
}

// NewClient builds a client. baseURL may be empty for the vendor default (tests
// pass a fake server). timeout bounds one request.
func NewClient(baseURL, projectID, integrationID, apiKey string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		projectID:     projectID,
		integrationID: integrationID,
		apiKey:        apiKey,
		http: &http.Client{
			Timeout: timeout,
			// Never follow a redirect with the key attached.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Chat asks one question. An empty threadID starts a conversation; a non-empty
// one continues the vendor thread. Only query and the integration id are sent.
func (c *Client) Chat(ctx context.Context, query, threadID string) (ChatResponse, error) {
	var endpoint string
	if threadID == "" {
		endpoint = c.baseURL + "/query/v1/projects/" + url.PathEscape(c.projectID) + "/chat/"
	} else {
		endpoint = c.baseURL + "/query/v1/threads/" + url.PathEscape(threadID) + "/chat/"
	}
	body, err := json.Marshal(chatRequest{IntegrationID: c.integrationID, Query: query})
	if err != nil {
		return ChatResponse{}, ErrBadResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, ErrUnavailable
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// err is a *url.Error carrying the full URL (project id, thread id);
		// drop it.
		return ChatResponse{}, ErrUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return ChatResponse{}, ErrRateLimited
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return ChatResponse{}, ErrUnavailable
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return ChatResponse{}, ErrRejected
	}
	var out ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return ChatResponse{}, ErrBadResponse
	}
	return out, nil
}

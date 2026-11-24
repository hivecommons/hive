// Package askai is the backend of the optional Ask AI help panel
// (src/docs/design/dashboard-ask-ai-help.md, #19177). A dashboard posts a
// question to its own backend; this package validates it, enforces consent and
// limits, refuses credential-shaped input, calls the Kapa.ai Chat API server
// side, keeps only allowlisted sources and returns plain text. Nothing here
// runs in the browser, and nothing here logs or stores what a person typed.
package askai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Vendor contract, pinned from the public Kapa API reference ("Chat" and "Chat
// in thread", QueryResponse schema) and by TestKapaClientContract.
const (
	// KapaDefaultBaseURL is the vendor API host.
	KapaDefaultBaseURL = "https://api.kapa.ai"
	// kapaChatPath starts a conversation in a project.
	kapaChatPath = "/query/v1/projects/%s/chat/"
	// kapaThreadChatPath continues a conversation in an existing thread.
	kapaThreadChatPath = "/query/v1/threads/%s/chat/"
	// kapaAPIKeyHeader carries the key. It is set on the outbound request
	// only and never logged.
	kapaAPIKeyHeader = "X-API-KEY"

	// DefaultUpstreamTimeout bounds one vendor call (design: 30 s).
	DefaultUpstreamTimeout = 30 * time.Second
	// maxUpstreamResponseBytes caps what is read from the vendor.
	maxUpstreamResponseBytes = 1 << 20
	// defaultRetryAfter is used when the vendor rate-limits without a
	// usable Retry-After header.
	defaultRetryAfter = 60 * time.Second
	// maxRetryAfter keeps a hostile Retry-After from parking the feature.
	maxRetryAfter = 15 * time.Minute
)

// vendorIDPattern bounds ids interpolated into the vendor URL path.
var vendorIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Upstream is whatever answers a question: the Kapa client in direct mode,
// a fake in tests.
type Upstream interface {
	Chat(ctx context.Context, threadID, query string) (UpstreamAnswer, error)
}

// UpstreamAnswer is the vendor's answer before filtering.
type UpstreamAnswer struct {
	Answer    string
	ThreadID  string
	Uncertain bool
	Sources   []UpstreamSource
}

// UpstreamSource is one source the vendor cited.
type UpstreamSource struct {
	URL      string
	Title    string
	Internal bool
}

// UpstreamStatusError is a non-2xx vendor response. It carries the status
// and a retry hint, never the body.
type UpstreamStatusError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *UpstreamStatusError) Error() string {
	return "ask upstream returned HTTP " + strconv.Itoa(e.Status)
}

// RateLimited reports a vendor 429.
func (e *UpstreamStatusError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// KapaClient calls the Kapa Chat API. The key, the project id and the
// integration id stay in this struct: String and errors never print them.
type KapaClient struct {
	baseURL       string
	projectID     string
	integrationID string
	apiKey        string
	http          *http.Client
}

// NewKapaClient builds a client. baseURL "" means KapaDefaultBaseURL; a nil
// httpClient gets one bounded by DefaultUpstreamTimeout.
func NewKapaClient(baseURL, projectID, integrationID, apiKey string, httpClient *http.Client) (*KapaClient, error) {
	if baseURL == "" {
		baseURL = KapaDefaultBaseURL
	}
	if !vendorIDPattern.MatchString(projectID) || !vendorIDPattern.MatchString(integrationID) || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("ask: vendor client is not configured")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultUpstreamTimeout}
	}
	return &KapaClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		projectID:     projectID,
		integrationID: integrationID,
		apiKey:        strings.TrimSpace(apiKey),
		http:          httpClient,
	}, nil
}

// String never prints credentials.
func (c *KapaClient) String() string { return "KapaClient{configured}" }

// kapaRequest is the ONLY body we send: the integration id and the query.
// No user object, no metadata (origin_url), no source filters.
type kapaRequest struct {
	IntegrationID string `json:"integration_id"`
	Query         string `json:"query"`
}

// kapaResponse mirrors the vendor's QueryResponse.
type kapaResponse struct {
	Answer           string `json:"answer"`
	ThreadID         string `json:"thread_id"`
	QuestionAnswerID string `json:"question_answer_id"`
	IsUncertain      bool   `json:"is_uncertain"`
	RelevantSources  []struct {
		SourceURL            string `json:"source_url"`
		Title                string `json:"title"`
		ContainsInternalData bool   `json:"contains_internal_data"`
	} `json:"relevant_sources"`
}

// Chat asks a question, in threadID when it is a valid vendor thread id.
func (c *KapaClient) Chat(ctx context.Context, threadID, query string) (UpstreamAnswer, error) {
	path := fmt.Sprintf(kapaChatPath, url.PathEscape(c.projectID))
	if threadID != "" {
		if !vendorIDPattern.MatchString(threadID) {
			return UpstreamAnswer{}, errors.New("ask: invalid thread id")
		}
		path = fmt.Sprintf(kapaThreadChatPath, url.PathEscape(threadID))
	}
	body, err := json.Marshal(kapaRequest{IntegrationID: c.integrationID, Query: query})
	if err != nil {
		return UpstreamAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return UpstreamAnswer{}, errors.New("ask: could not build the vendor request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(kapaAPIKeyHeader, c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		// Do not wrap: a url.Error would print the request URL, which
		// carries the project id.
		if ctx.Err() != nil {
			return UpstreamAnswer{}, fmt.Errorf("ask: vendor call cancelled: %w", ctx.Err())
		}
		return UpstreamAnswer{}, errors.New("ask: vendor unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUpstreamResponseBytes))
		return UpstreamAnswer{}, &UpstreamStatusError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	var out kapaResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamResponseBytes)).Decode(&out); err != nil {
		return UpstreamAnswer{}, errors.New("ask: vendor response was not valid JSON")
	}
	answer := UpstreamAnswer{Answer: out.Answer, Uncertain: out.IsUncertain}
	if vendorIDPattern.MatchString(out.ThreadID) {
		answer.ThreadID = out.ThreadID
	}
	for _, s := range out.RelevantSources {
		answer.Sources = append(answer.Sources, UpstreamSource{URL: s.SourceURL, Title: s.Title, Internal: s.ContainsInternalData})
	}
	return answer, nil
}

// parseRetryAfter reads delta-seconds; anything else is the default. The
// result is clamped to maxRetryAfter.
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultRetryAfter
	}
	if n > int(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(n) * time.Second
}

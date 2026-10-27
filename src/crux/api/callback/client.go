package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// Client calls the hive-side callback endpoint. It implements Capabilities
// over HTTP and is what `crux serve` builds in sidecar mode. It has no
// logger: nothing it does is logged here, so no token, body or bearer can
// leak through it, and its errors never quote a request or response body.
// The zero value is not usable; call NewClient.
type Client struct {
	addr    api.Address
	token   string
	http    *http.Client
	timeout time.Duration

	mu     sync.Mutex
	status Status
}

// Status is the client's view of the endpoint: whether the last call
// reached hive, and when not, since when and how many calls have failed
// closed. It is the runtime condition ConditionClassUnavailable describes.
type Status struct {
	Reachable bool
	// LastError is the most recent failure, "" when Reachable.
	LastError string
	// Since is when the current outage began (zero when Reachable).
	Since time.Time
	// FailedCalls counts calls that failed closed during the current outage.
	FailedCalls int
	// LastSuccess is when a call last reached hive (zero if never).
	LastSuccess time.Time
}

// NewClient parses server (the forms api.ParseAddress accepts) and returns a
// client that presents token as a bearer token when it is non-empty. A
// unix-socket address gets a transport that dials the socket.
func NewClient(server, token string) (*Client, error) {
	addr, err := api.ParseAddress(server)
	if err != nil {
		return nil, err
	}
	c := &Client{addr: addr, token: token, http: &http.Client{}, timeout: DefaultTimeout}
	c.status.Reachable = true
	if addr.Network == "unix" {
		path := addr.Path
		c.http.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		}
	}
	return c, nil
}

// Address is the parsed endpoint address.
func (c *Client) Address() api.Address { return c.addr }

// SetTimeout replaces DefaultTimeout as the per-call bound applied when the
// caller's context has no deadline. Non-positive restores the default.
func (c *Client) SetTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultTimeout
	}
	c.timeout = d
}

// SetHTTPClient replaces the underlying http.Client, keeping the socket
// transport when the address is a unix socket and the caller's client has
// none of its own.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h.Transport == nil && c.http != nil {
		h.Transport = c.http.Transport
	}
	c.http = h
}

// Status returns the current reachability snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Health is GET /v1/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, PathHealth, nil, &out)
	return out, err
}

// MintToken is POST /v1/tokens.
func (c *Client) MintToken(ctx context.Context, req MintTokenRequest) (MintTokenResponse, error) {
	var out MintTokenResponse
	if err := c.do(ctx, http.MethodPost, PathTokens, req, &out); err != nil {
		return MintTokenResponse{}, err
	}
	if out.Token == "" {
		return MintTokenResponse{}, fmt.Errorf("%w: hive returned an empty token", ErrUnavailable)
	}
	return out, nil
}

// RecordREST is POST /v1/github/rest.
func (c *Client) RecordREST(ctx context.Context, rec RESTRecord) error {
	return c.do(ctx, http.MethodPost, PathGitHubREST, rec, nil)
}

// RateLimit is GET /v1/github/rate-limit.
func (c *Client) RateLimit(ctx context.Context) (RateLimitSnapshot, error) {
	var out RateLimitSnapshot
	err := c.do(ctx, http.MethodGet, PathGitHubRateLimit, nil, &out)
	return out, err
}

// RecordUsage is POST /v1/usage.
func (c *Client) RecordUsage(ctx context.Context, rec UsageRecord) error {
	return c.do(ctx, http.MethodPost, PathUsage, rec, nil)
}

// ScanCanary is POST /v1/scan/canary.
func (c *Client) ScanCanary(ctx context.Context, req CanaryScanRequest) (CanaryScanResponse, error) {
	var out CanaryScanResponse
	err := c.do(ctx, http.MethodPost, PathScanCanary, req, &out)
	return out, err
}

// ScanText is POST /v1/scan/text.
func (c *Client) ScanText(ctx context.Context, req TextScanRequest) (TextScanResponse, error) {
	var out TextScanResponse
	err := c.do(ctx, http.MethodPost, PathScanText, req, &out)
	return out, err
}

// ReportCondition is POST /v1/conditions.
func (c *Client) ReportCondition(ctx context.Context, rep ConditionReport) error {
	return c.do(ctx, http.MethodPost, PathConditions, rep, nil)
}

// LinearCredential is GET /v1/linear/credential.
func (c *Client) LinearCredential(ctx context.Context) (LinearCredential, error) {
	var out LinearCredential
	err := c.do(ctx, http.MethodGet, PathLinearCredential, nil, &out)
	return out, err
}

// CreatePR is POST /v1/pulls.
func (c *Client) CreatePR(ctx context.Context, req CreatePRRequest) (CreatePRResponse, error) {
	var out CreatePRResponse
	err := c.do(ctx, http.MethodPost, PathPulls, req, &out)
	return out, err
}

// Policy is GET /v1/policy.
func (c *Client) Policy(ctx context.Context) (Policy, error) {
	var out Policy
	err := c.do(ctx, http.MethodGet, PathPolicy, nil, &out)
	return out, err
}

// RepoAutoMergeEnabled is POST /v1/policy/repo-auto-merge.
func (c *Client) RepoAutoMergeEnabled(ctx context.Context, repo string) (bool, error) {
	var out RepoAutoMergeResponse
	err := c.do(ctx, http.MethodPost, PathRepoAutoMerge, RepoAutoMergeRequest{Repo: repo}, &out)
	return out.Enabled, err
}

// SetInferenceRoute is PUT /v1/inference/routes/{agent}.
func (c *Client) SetInferenceRoute(ctx context.Context, agent string, route InferenceRoute) error {
	return c.do(ctx, http.MethodPut, InferenceRoutePath(agent), route, nil)
}

// ClearInferenceRoute is DELETE /v1/inference/routes/{agent}.
func (c *Client) ClearInferenceRoute(ctx context.Context, agent string) error {
	return c.do(ctx, http.MethodDelete, InferenceRoutePath(agent), nil, nil)
}

// BeginEffect is POST /v1/effects.
func (c *Client) BeginEffect(ctx context.Context, claim EffectClaim) (BeginEffectResponse, error) {
	var out BeginEffectResponse
	if err := c.do(ctx, http.MethodPost, PathEffects, claim, &out); err != nil {
		return BeginEffectResponse{}, err
	}
	if out.ID == "" {
		return BeginEffectResponse{}, fmt.Errorf("%w: hive returned no effect id", ErrUnavailable)
	}
	return out, nil
}

// EndEffect is POST /v1/effects/{id}/end.
func (c *Client) EndEffect(ctx context.Context, id string, req EndEffectRequest) (EndEffectResponse, error) {
	var out EndEffectResponse
	err := c.do(ctx, http.MethodPost, EffectEndPath(id), req, &out)
	return out, err
}

var (
	_ Capabilities    = (*Client)(nil)
	_ PolicySource    = (*Client)(nil)
	_ InferenceRouter = (*Client)(nil)
	_ EffectBoundary  = (*Client)(nil)
)

// do performs one call. Transport failures, timeouts and 5xx become
// ErrUnavailable; 401 becomes ErrUnauthorized; 404 CodeNotConfigured becomes
// ErrNotConfigured; other non-2xx are *api.StatusError. Every outcome
// updates Status.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (err error) {
	defer func() { c.observe(err) }()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", ContentTypeJSON)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if err := c.checkStatus(resp, method, path); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decoding %s %s: %w", ErrUnavailable, method, path, err)
	}
	return nil
}

// redactURLError strips the request URL from a transport error so a TCP
// endpoint's URL form never appears in an error a caller might log; the
// method and path are added back by the caller.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func (c *Client) checkStatus(resp *http.Response, method, path string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	se := &api.StatusError{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(data, &se.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s %s", ErrUnauthorized, method, path)
	case resp.StatusCode == http.StatusNotFound && se.Body.Code == CodeNotConfigured:
		return fmt.Errorf("%w: %s %s", ErrNotConfigured, method, path)
	case resp.StatusCode == http.StatusForbidden && se.Body.Code == CodeEffectDenied:
		return fmt.Errorf("%w: %s", ErrEffectDenied, se.Body.Error)
	case resp.StatusCode == http.StatusNotFound && se.Body.Code == CodeEffectUnknown:
		return fmt.Errorf("%w: %s %s", ErrEffectUnknown, method, path)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s %s: HTTP %d %s: %w", ErrUnavailable, method, path, resp.StatusCode, se.Body.Code, se)
	}
	return se
}

// observe folds one call's outcome into Status. A refusal hive itself made
// (401, 404 not_configured, 4xx) still counts as reachable: hive answered.
func (c *Client) observe(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if err != nil && errors.Is(err, ErrUnavailable) {
		if c.status.Reachable {
			c.status.Reachable = false
			c.status.Since = now
			c.status.FailedCalls = 0
		}
		c.status.FailedCalls++
		c.status.LastError = err.Error()
		return
	}
	c.status.Reachable = true
	c.status.LastError = ""
	c.status.Since = time.Time{}
	c.status.FailedCalls = 0
	c.status.LastSuccess = now
}

// MintAgentToken is POST /v1/tokens/mint.
func (c *Client) MintAgentToken(ctx context.Context, req AgentMintRequest) (AgentMintResponse, error) {
	var out AgentMintResponse
	err := c.do(ctx, http.MethodPost, PathAgentMint, req, &out)
	return out, err
}

// BobCredential is GET /v1/bob/credential.
func (c *Client) BobCredential(ctx context.Context) (BobCredential, error) {
	var out BobCredential
	err := c.do(ctx, http.MethodGet, PathBobCredential, nil, &out)
	return out, err
}

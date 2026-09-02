package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Device-flow login client for the dashboard's existing gh-user-auth surface
// (#5651). Every route it calls is on the server's isPublicPath allowlist, so
// an unauthenticated client can complete the whole flow; nothing server-side
// changes for this feature.
const (
	ghAuthStartPath  = "/api/gh-user-auth/start"
	ghAuthPollPath   = "/api/gh-user-auth/poll"
	ghAuthLogoutPath = "/api/gh-user-auth/logout"
)

const (
	// defaultPollInterval is GitHub's documented device-flow minimum. Used only
	// when the start response carries no interval, so a server that omits the
	// field is polled politely rather than in a tight loop.
	defaultPollInterval = 5 * time.Second

	// slowDownBackoff is added to the interval on every slow_down, per the
	// GitHub device-flow contract ("wait at least 5 seconds more between
	// requests"). The server relays slow_down verbatim, so honouring it here is
	// what keeps a hive's GitHub OAuth app from being rate-limited by its own
	// operators.
	slowDownBackoff = 5 * time.Second

	// defaultFlowExpiry bounds the whole login when the start response carries
	// no expires_in. GitHub's device codes live 15 minutes; a flow that could
	// wait forever would leave a forgotten `hivectl login` polling the hive all
	// night.
	defaultFlowExpiry = 15 * time.Minute
)

// ErrDeviceFlowConflict reports the one-flow-at-a-time race: the spoke holds a
// single server-wide device-flow state, and another login's start overwrote
// ours (or ours never happened). The server answers a bare 400 — this wraps it
// into something an operator can act on rather than a mystery status code.
var ErrDeviceFlowConflict = errors.New(
	"the hive reports no device flow in progress — a hive runs one login at a time, " +
		"so another operator's login may have replaced yours; run 'hivectl login' again")

// DeviceLoginPrompt is what the operator must be shown to complete the flow:
// the one-time code to type, and where to type it.
type DeviceLoginPrompt struct {
	UserCode        string
	VerificationURI string
}

// DeviceLoginOptions injects the flow's side effects so tests can run it
// against an httptest fixture with no sleeping, no clock, and no terminal.
// Every nil field gets the production behaviour.
type DeviceLoginOptions struct {
	// Prompt shows the operator the code and URL. Called exactly once, after a
	// successful start.
	Prompt func(DeviceLoginPrompt)
	// Sleep waits between polls. The production default also honours context
	// cancellation, so ctrl+c does not have to wait out an interval.
	Sleep func(context.Context, time.Duration) error
	// Now supplies the clock for the flow's overall deadline.
	Now func() time.Time
}

// LoginResult is a completed login. Cookie is a full Cookie header value —
// every cookie the poll response set, joined with "; " — which is deliberately
// the HIVE_DASHBOARD_COOKIE convention (#5649): on a plain spoke that is
// `hive_session=...` alone, on a hosted one the terminal assertion rides
// alongside it, and consumers never need to know which they got.
type LoginResult struct {
	Username  string
	Cookie    string
	ExpiresAt time.Time
}

// deviceStartResponse mirrors handleGHUserAuthStart's payload.
//
// ExpiresIn and Interval are pointers to keep "the server said zero" distinct
// from "the server omitted the field": an omitted interval falls back to
// GitHub's documented 5s minimum, while an explicit zero is honoured as
// written — the server relays GitHub's values verbatim and is the authority on
// its own pacing.
type deviceStartResponse struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       *int   `json:"expires_in"`
	Interval        *int   `json:"interval"`
	// FlowID binds the flow to this client: the server refuses to complete a
	// poll (and mint the session) for any caller that cannot present it. Empty
	// when talking to a server predating the binding, in which case no body is
	// sent and the old contract applies.
	FlowID string `json:"flow_id"`
}

// devicePollResponse mirrors handleGHUserAuthPoll's payload. Note the error
// case: a REJECTED user (not on an allowlist spoke's authorized_users) arrives
// as {status:"error"} inside a 200, not as a 4xx — the server documents that
// choice, and a client classifying on HTTP status alone would spin forever.
type devicePollResponse struct {
	Status   string `json:"status"`
	Error    string `json:"error"`
	Username string `json:"username"`
}

// DeviceLogin runs the GitHub device flow against the hive and returns the
// minted per-user session.
//
// The shape is start → prompt → poll until terminal. The session cookie
// arrives on the POLL response's Set-Cookie when status reaches "complete";
// there is no separate exchange step. Terminal outcomes are "complete", any
// {status:"error"} (which carries the server's own explanation — e.g. "your
// GitHub account (x) is not authorized to access this hive" — and is returned
// verbatim so the operator sees the server's reason, not a generic failure),
// the flow's expiry, and the lost-start race (ErrDeviceFlowConflict).
//
// The result's cookie value is a credential. It is never logged and never
// embedded in an error by this function; callers must extend the same
// courtesy.
func (c *Client) DeviceLogin(ctx context.Context, opts DeviceLoginOptions) (*LoginResult, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}

	start, err := c.startDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Prompt != nil {
		opts.Prompt(DeviceLoginPrompt{UserCode: start.UserCode, VerificationURI: start.VerificationURI})
	}

	interval := defaultPollInterval
	if start.Interval != nil && *start.Interval >= 0 {
		interval = time.Duration(*start.Interval) * time.Second
	}
	expiry := defaultFlowExpiry
	if start.ExpiresIn != nil && *start.ExpiresIn > 0 {
		expiry = time.Duration(*start.ExpiresIn) * time.Second
	}
	deadline := now().Add(expiry)

	for {
		if now().After(deadline) {
			return nil, fmt.Errorf("the one-time code expired before the login was approved — run 'hivectl login' again")
		}
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}

		poll, cookies, err := c.pollDeviceFlow(ctx, start.FlowID)
		if err != nil {
			return nil, err
		}
		switch poll.Status {
		case "pending":
			continue
		case "slow_down":
			interval += slowDownBackoff
			continue
		case "complete":
			return loginResultFrom(poll, cookies, now())
		case "error":
			message := poll.Error
			if message == "" {
				message = "the hive reported an unspecified login error"
			}
			return nil, fmt.Errorf("login failed: %s", message)
		default:
			return nil, fmt.Errorf("login failed: the hive answered the device-flow poll with unexpected status %q", poll.Status)
		}
	}
}

// Logout ends the presented session server-side via the existing endpoint.
// The request carries whatever credentials the client was configured with; on
// a per-user session that deletes exactly this session (the server scopes the
// logout to the request's own cookie).
func (c *Client) Logout(ctx context.Context) error {
	_, err := c.Do(ctx, http.MethodPost, ghAuthLogoutPath, nil, nil)
	return err
}

func (c *Client) startDeviceFlow(ctx context.Context) (*deviceStartResponse, error) {
	data, _, err := c.do(ctx, http.MethodPost, ghAuthStartPath, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var start deviceStartResponse
	if err := json.Unmarshal(data, &start); err != nil {
		return nil, fmt.Errorf("decode device-flow start response: %w", err)
	}
	if start.UserCode == "" || start.VerificationURI == "" {
		return nil, errors.New("the hive's device-flow start response carried no user code — is this a hive dashboard?")
	}
	return &start, nil
}

// pollDeviceFlow needs the raw *http.Response — the session arrives as
// Set-Cookie headers, which Client.do discards — so it drives the request
// itself instead of going through do. flowID (from the start response) is the
// client-binding secret the server requires before it will complete the flow;
// empty (an older server that minted none) sends no body.
func (c *Client) pollDeviceFlow(ctx context.Context, flowID string) (*devicePollResponse, []*http.Cookie, error) {
	var body any
	if flowID != "" {
		body = map[string]string{"flow_id": flowID}
	}
	req, err := c.request(ctx, http.MethodPost, ghAuthPollPath, nil, body, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, connectionError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, nil, connectionError(err)
	}
	if resp.StatusCode == http.StatusBadRequest {
		// The server's only 400 on this path is "no device flow in progress" —
		// the single-flow-per-spoke race the issue calls out. Name it instead
		// of surfacing a bare status code.
		return nil, nil, fmt.Errorf("%w (server said: %s)", ErrDeviceFlowConflict, apiErrorMessage(data))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, apiError(resp.StatusCode, data)
	}
	var poll devicePollResponse
	if err := json.Unmarshal(data, &poll); err != nil {
		return nil, nil, fmt.Errorf("decode device-flow poll response: %w", err)
	}
	return &poll, resp.Cookies(), nil
}

// loginResultFrom assembles the completed login from the poll response and its
// Set-Cookie headers.
func loginResultFrom(poll *devicePollResponse, cookies []*http.Cookie, now time.Time) (*LoginResult, error) {
	var parts []string
	expiresAt := now.Add(sessionFallbackTTL)
	for _, ck := range cookies {
		// A cleared cookie (empty value or negative Max-Age) is a deletion, not
		// a credential; carrying one forward would present a tombstone on every
		// future request.
		if ck.Value == "" || ck.MaxAge < 0 {
			continue
		}
		parts = append(parts, ck.Name+"="+ck.Value)
		// The session's own lifetime comes from the hive_session cookie the
		// server minted, so the cache never claims to outlive the server-side
		// session. Companion cookies (the hosted terminal assertion) are
		// shorter-lived and re-minted by the server; they do not bound the
		// cache entry.
		if ck.Name == "hive_session" {
			switch {
			case ck.MaxAge > 0:
				expiresAt = now.Add(time.Duration(ck.MaxAge) * time.Second)
			case !ck.Expires.IsZero():
				expiresAt = ck.Expires
			}
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("the hive reported the login complete but set no session cookie — cannot cache a session")
	}
	return &LoginResult{
		Username:  poll.Username,
		Cookie:    strings.Join(parts, "; "),
		ExpiresAt: expiresAt,
	}, nil
}

// sleepContext is the production Sleep: a plain timer that also honours
// cancellation, so ctrl+c during the multi-minute wait for browser approval
// does not have to ride out a full poll interval.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// apiErrorMessage extracts the server's {"error": ...} text for embedding in a
// wrapped error, falling back to the raw body.
func apiErrorMessage(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(body))
}

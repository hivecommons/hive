package linearagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client is the CONTROL-PLANE Linear GraphQL client: it acknowledges sessions
// and emits AgentActivities on the hub's behalf. It is deliberately not the
// agent's client — agents talk to api.linear.app through the MITM proxy and
// are gated by pkg/proxy/linear_rules.go; this client runs inside the hive
// process, which is exempt the same way the worksource read path is.
//
// It refreshes the OAuth token ahead of expiry and persists the refreshed
// grant through the Store, so a hive that only receives webhooks (and never
// re-runs the install flow) keeps a live token indefinitely.
type Client struct {
	store      *Store
	creds      Credentials
	http       *http.Client
	tokenURL   string
	graphqlURL string
	logger     *slog.Logger
	now        func() time.Time

	// refreshSkew is how long before nominal expiry the token is treated as
	// expired. Linear tokens live 24h; refreshing a minute early costs
	// nothing and removes the race where a token expires mid-request.
	refreshSkew time.Duration
}

// NewClient constructs a control-plane client. A nil httpClient gets a sane
// timeout; tokenURL/graphqlURL default to production when empty (tests point
// them at fakes).
func NewClient(store *Store, creds Credentials, httpClient *http.Client, tokenURL, graphqlURL string, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: tokenHTTPTimeout}
	}
	if tokenURL == "" {
		tokenURL = TokenURL
	}
	if graphqlURL == "" {
		graphqlURL = GraphQLURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		store:       store,
		creds:       creds,
		http:        httpClient,
		tokenURL:    tokenURL,
		graphqlURL:  graphqlURL,
		logger:      logger,
		now:         time.Now,
		refreshSkew: time.Minute,
	}
}

// SetClock overrides the client's clock. Tests only.
func (c *Client) SetClock(f func() time.Time) { c.now = f }

// Activity content types Linear accepts from an agent. `prompt` is
// user-generated and deliberately absent — an agent cannot emit one.
const (
	ActivityThought     = "thought"
	ActivityElicitation = "elicitation"
	ActivityAction      = "action"
	ActivityResponse    = "response"
	ActivityError       = "error"
)

// ActivityContent is the polymorphic content payload of one AgentActivity.
// Body carries thought/elicitation/response/error text; Action/Parameter
// (/Result) carry the action shape. Only the fields for the given Type are
// serialized — Linear validates shapes server-side and rejects extras.
type ActivityContent struct {
	Type      string `json:"type"`
	Body      string `json:"body,omitempty"`
	Action    string `json:"action,omitempty"`
	Parameter string `json:"parameter,omitempty"`
	Result    string `json:"result,omitempty"`
}

// activityBodyLimit truncates activity bodies. Linear renders these in a
// session feed; a full kick log does not belong there (RFC open question 4 —
// start/finish/blocked, not every line).
const activityBodyLimit = 4000

// CreateActivity emits one AgentActivity onto a session.
func (c *Client) CreateActivity(ctx context.Context, sessionID string, content ActivityContent) error {
	if content.Body != "" {
		content.Body = truncate(content.Body, activityBodyLimit)
	}
	if content.Result != "" {
		content.Result = truncate(content.Result, activityBodyLimit)
	}
	const mutation = `mutation AgentActivityCreate($input: AgentActivityCreateInput!) {
  agentActivityCreate(input: $input) { success }
}`
	vars := map[string]interface{}{
		"input": map[string]interface{}{
			"agentSessionId": sessionID,
			"content":        content,
		},
	}
	var out struct {
		AgentActivityCreate struct {
			Success bool `json:"success"`
		} `json:"agentActivityCreate"`
	}
	if err := c.do(ctx, mutation, vars, &out); err != nil {
		return err
	}
	if !out.AgentActivityCreate.Success {
		return fmt.Errorf("agentActivityCreate reported failure")
	}
	return nil
}

// ExternalURL is one link shown on a session (Linear renders these next to
// the session header — "where the work is happening").
type ExternalURL struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// UpdateSessionExternalURLs points a session at external artifacts — the PR
// an agent opened is the canonical one. Linear documents agentSessionUpdate
// as the presence call alongside agentActivityCreate ("send an activity OR
// update your external URL"), and the proxy allowlist keeps both reachable
// at every tier for that reason; this is the control-plane caller.
func (c *Client) UpdateSessionExternalURLs(ctx context.Context, sessionID string, urls []ExternalURL) error {
	const mutation = `mutation AgentSessionUpdate($id: String!, $input: AgentSessionUpdateInput!) {
  agentSessionUpdate(id: $id, input: $input) { success }
}`
	vars := map[string]interface{}{
		"id":    sessionID,
		"input": map[string]interface{}{"externalUrls": urls},
	}
	var out struct {
		AgentSessionUpdate struct {
			Success bool `json:"success"`
		} `json:"agentSessionUpdate"`
	}
	if err := c.do(ctx, mutation, vars, &out); err != nil {
		return err
	}
	if !out.AgentSessionUpdate.Success {
		return fmt.Errorf("agentSessionUpdate reported failure")
	}
	return nil
}

// Identity is what FetchIdentity learns about an install: the app user's id in
// the workspace and the workspace itself.
type Identity struct {
	ViewerID           string
	OrganizationID     string
	OrganizationName   string
	OrganizationURLKey string
}

// FetchIdentity runs the post-install identity query with an explicit access
// token (the install flow calls it before the Store has the grant).
func FetchIdentity(ctx context.Context, hc *http.Client, graphqlURL, accessToken string) (Identity, error) {
	if hc == nil {
		hc = &http.Client{Timeout: tokenHTTPTimeout}
	}
	if graphqlURL == "" {
		graphqlURL = GraphQLURL
	}
	const query = `query Me { viewer { id } organization { id name urlKey } }`
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
		Organization struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			URLKey string `json:"urlKey"`
		} `json:"organization"`
	}
	if err := doGraphQL(ctx, hc, graphqlURL, accessToken, query, nil, &out); err != nil {
		return Identity{}, err
	}
	if out.Viewer.ID == "" {
		return Identity{}, fmt.Errorf("viewer query returned no id")
	}
	return Identity{
		ViewerID:           out.Viewer.ID,
		OrganizationID:     out.Organization.ID,
		OrganizationName:   out.Organization.Name,
		OrganizationURLKey: out.Organization.URLKey,
	}, nil
}

// do runs one GraphQL request with a live token, refreshing first if needed.
func (c *Client) do(ctx context.Context, query string, vars map[string]interface{}, out interface{}) error {
	tok, err := c.liveToken(ctx)
	if err != nil {
		return err
	}
	return doGraphQL(ctx, c.http, c.graphqlURL, tok, query, vars, out)
}

// AccessToken returns the installed workspace's live OAuth access token,
// refreshing it through the Store when it is at or past the skewed deadline.
//
// This is the credential the hive hands to ISSUES_ONLY+ agents so their
// Linear writes (issueCreate, commentCreate, …) are authored by the same
// "Hive" app identity that acknowledges sessions — the Linear analogue of the
// GitHub App installation token that pkg/agent injects as GITHUB_TOKEN. It
// returns "" with an error when no workspace is connected; callers fall back
// to the work-source API key or inject nothing.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	return c.liveToken(ctx)
}

// liveToken returns a non-expired access token, refreshing and persisting
// through the Store when the stored one is at or past the skewed deadline.
func (c *Client) liveToken(ctx context.Context) (string, error) {
	inst, ok := c.store.Get()
	if !ok {
		return "", fmt.Errorf("linear agent is not installed")
	}
	tok := inst.Token
	if tok.AccessToken == "" {
		return "", fmt.Errorf("linear install has no access token")
	}
	if tok.ExpiresAt.IsZero() || c.now().Before(tok.ExpiresAt.Add(-c.refreshSkew)) {
		return tok.AccessToken, nil
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("linear access token expired and no refresh token is stored; reconnect the workspace")
	}
	fresh, err := RefreshToken(ctx, c.http, c.tokenURL, c.creds, tok.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh linear token: %w", err)
	}
	if fresh.RefreshToken == "" {
		// Linear grants a 30-minute replay window on refresh; if the new
		// refresh token was lost in flight the old one may still be live.
		// Keep it rather than storing an empty one.
		fresh.RefreshToken = tok.RefreshToken
	}
	if err := c.store.UpdateToken(fresh); err != nil {
		// The refreshed token works for this request even if persisting it
		// failed; log and continue rather than dropping the activity.
		c.logger.Warn("linearagent: failed to persist refreshed token", "error", err)
	}
	return fresh.AccessToken, nil
}

// graphQLEnvelope is the wire shape of a GraphQL response.
type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// doGraphQL runs one authenticated GraphQL POST. Error strings carry only the
// HTTP status and GraphQL error message — never the token or variables (which
// hold issue content).
func doGraphQL(ctx context.Context, hc *http.Client, graphqlURL, accessToken, query string, vars map[string]interface{}, out interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oauthResponseLimit))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear graphql status %d", resp.StatusCode)
	}
	var env graphQLEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("linear graphql error: %s", env.Errors[0].Message)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// truncate bounds s to max runes, marking the cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

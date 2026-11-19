package dashboard

import (
	"context"
	"net/http"
	"time"
)

// WatsonxGateway is the dashboard's view of pkg/watsonx: endpoint templating
// and IAM bearer minting for gateway probes. Nil is safe — the watsonx probe
// paths then fail with an explicit error instead of sending a raw key.
type WatsonxGateway interface {
	// EndpointForRegion builds the model-gateway base URL for a region slug,
	// falling back to the default region when blank.
	EndpointForRegion(region string) string
	// MintToken exchanges an IBM Cloud API key for an IAM bearer token
	// (cached provider-side). Never log the key or the token.
	MintToken(ctx context.Context, apiKey string) (string, error)
	// ProjectIDHeader is the request header watsonx reads the project id from.
	ProjectIDHeader() string
	// GraniteFallbackModels is the static model list offered when live model
	// discovery fails AFTER the key has been validated by a successful mint.
	GraniteFallbackModels() []string
}

// OpenRouterCredit mirrors the used subset of the provider's credit report.
// Limit/LimitRemaining are pointers for the same reason as upstream: an
// unlimited key reports null, which must stay distinguishable from zero.
type OpenRouterCredit struct {
	Label          string
	Limit          *float64
	LimitRemaining *float64
	Usage          float64
}

// OpenRouterSuggestedModel is one curated funding-screen model choice. JSON
// tags match the provider's wire shape so the /api/openrouter/models payload
// is byte-identical.
type OpenRouterSuggestedModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// OpenRouterPricing is a catalog model's USD price PER TOKEN. A nil field is
// unknown (not published, or variable), never zero.
type OpenRouterPricing struct {
	Prompt         *float64 `json:"prompt,omitempty"`
	Completion     *float64 `json:"completion,omitempty"`
	InputCacheRead *float64 `json:"input_cache_read,omitempty"`
}

// OpenRouterModel is one entry of the provider's public catalog: id, display
// name, context window and published prices. The JSON shape is what the model
// pickers read (/api/openrouter/models and gateway discovery's model_info).
type OpenRouterModel struct {
	ID            string            `json:"id"`
	Name          string            `json:"name,omitempty"`
	ContextLength int               `json:"context_length,omitempty"`
	Pricing       OpenRouterPricing `json:"pricing"`
}

// OpenRouterFlow is a recovered pending PKCE flow: the server-held verifier
// plus the hive and model the flow was started for.
type OpenRouterFlow struct {
	Verifier string
	HiveID   string
	Model    string
}

// OpenRouterFlowStore is the single-use, TTL-bounded PKCE state store backing
// the scan-to-fund flow. Create records a pending flow and returns its state
// token; Consume redeems it exactly once (expired/replayed states report
// ok=false).
type OpenRouterFlowStore interface {
	Create(verifier, hiveID, model string) (string, error)
	Consume(state string) (OpenRouterFlow, bool)
}

// OpenRouterGateway is the dashboard's view of pkg/openrouter: the PKCE
// funding flow primitives plus the few published constants the handlers
// serve. Nil is safe — the funding routes then answer 503 instead of
// panicking (production always wires it; see cmd/hive).
type OpenRouterGateway interface {
	// GeneratePKCE creates a code_verifier and its S256 challenge. The
	// verifier never leaves the server.
	GeneratePKCE() (verifier, challenge string, err error)
	// BuildAuthorizeURL builds the provider authorize URL for this hive's
	// callback, challenge, and single-use state.
	BuildAuthorizeURL(callbackURL, codeChallenge, state string) (string, error)
	// AuthURL is the provider's authorize-page prefix; the QR endpoint
	// refuses to encode anything else.
	AuthURL() string
	// BaseURL is the provider's OpenAI-compatible API base, stored as the
	// funded gateway's endpoint.
	BaseURL() string
	// DefaultModel is the model recorded when a flow specifies none.
	DefaultModel() string
	// SuggestedModels is the curated funding-screen list.
	SuggestedModels() []OpenRouterSuggestedModel
	// PublicModels is the provider's public catalog (no key needed), cached
	// by the provider package; nil when it has never been readable.
	PublicModels() []OpenRouterModel
	// QRPNG renders the authorize URL as a PNG so the image stays same-origin.
	QRPNG(text string) ([]byte, error)
	// ExchangeCode redeems code+verifier for the user-scoped API key. The key
	// is a secret: stored via the gateway secret-file store, never logged.
	ExchangeCode(code, verifier string) (string, error)
	// FetchCredit reads the key's limit/usage for the "$X remaining" panel.
	FetchCredit(key string) (OpenRouterCredit, error)
	// NewFlowStore builds the single-use PKCE state store (one per Server,
	// created lazily with the production TTL).
	NewFlowStore() OpenRouterFlowStore
}

// LinearInstall mirrors the used subset of the connected workspace install.
// It deliberately carries no token material — HasAccessToken is the only fact
// the dashboard needs about the credential.
type LinearInstall struct {
	ViewerID           string
	OrganizationID     string
	OrganizationName   string
	OrganizationURLKey string
	ConnectedAt        time.Time
	HasAccessToken     bool
}

// LinearAgentPorts carries the dashboard-side callbacks the Linear agent
// service needs: kicking an agent with a message, and naming the agent that
// takes Linear sessions. Wired by the dashboard when it lazily constructs the
// service via Dependencies.NewLinearAgent.
type LinearAgentPorts struct {
	// Kick sends a kick message to the named agent (and records it with the
	// governor). Returns an error when no agent manager is available.
	Kick func(agentName, message string) error
	// ResolveSessionAgent names the agent for Linear sessions, or returns the
	// error the responder should report into the session.
	ResolveSessionAgent func() (string, error)
	// TokenURL / GraphqlURL override the provider endpoints; empty means
	// production Linear. Tests point them at fakes.
	TokenURL   string
	GraphqlURL string
}

// LinearAgentGateway is the dashboard's view of the pkg/linearagent service
// bundle (store, OAuth client, session tracker, responder, webhook receiver).
// Constructed once per Server through Dependencies.NewLinearAgent; a nil
// factory yields a nil service and every Linear route answers "unavailable".
type LinearAgentGateway interface {
	// StoreErr reports a store-open failure (corrupt token file); surfaced in
	// status while install/webhook fail cleanly.
	StoreErr() error
	// Configured reports whether LINEAR_CLIENT_ID/SECRET are present.
	Configured() bool
	// NewFlowState records a single-use install state token.
	NewFlowState() (string, error)
	// ConsumeFlowState redeems a state exactly once; unknown/expired/replayed
	// states report false.
	ConsumeFlowState(state string) bool
	// AuthorizeURL builds the actor=app authorize URL for this redirect URI
	// and state.
	AuthorizeURL(redirectURI, state string) string
	// CompleteInstall exchanges the code, fetches the app's per-workspace
	// identity, and persists the install. Returns the workspace name for the
	// audit detail. Step-level warn logging happens provider-side with the
	// same messages as before the cut.
	CompleteInstall(ctx context.Context, code, redirectURI string) (workspace string, err error)
	// AccessToken returns the connected workspace's live OAuth access token
	// (refreshing if needed). Callers never log it.
	AccessToken(ctx context.Context) (string, error)
	// HasInstallStore reports whether the install store opened; disconnect
	// answers 503 without it.
	HasInstallStore() bool
	// Install returns the connected-workspace facts, ok=false when no
	// workspace is connected (or the store is unavailable).
	Install() (LinearInstall, bool)
	// ClearInstall forgets the install (Linear-side revocation is manual).
	ClearInstall() error
	// WebhookHandler is the AgentSessionEvent receiver (HMAC verification
	// inside), or nil when unavailable.
	WebhookHandler() http.Handler
	// ActiveSessionForIssue reports the agent + session id holding the given
	// external work-item identifier, if any.
	ActiveSessionForIssue(externalID string) (agentName, sessionID string, ok bool)
	// SessionsSnapshot returns the tracked-session list for the status
	// payload, ok=false when no tracker is available.
	SessionsSnapshot() (any, bool)
	// HandlePROpened narrates an opened PR into the agent's active session,
	// if any (no-op otherwise).
	HandlePROpened(agentName, repo string, number int, url string)
	// AgentEventObserver is the agent-manager kick observer that maps run
	// completion back onto Linear sessions, or nil when unavailable.
	AgentEventObserver() func(agentName, event, detail string)
}

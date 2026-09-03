package main

// Concrete LLM-provider gateway adapters for pkg/dashboard's consumer-defined
// interfaces (kubestellar/hive#5565 slice 3). Each adapter is a thin
// delegation to the provider package; this file is the only place the
// dashboard's provider wiring names a concrete openrouter/watsonx/linearagent
// type, keeping cmd/hive the composition root exactly as Dependencies already
// does for hub/scheduler/governor concretes.

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/linearagent"
	"github.com/hivecommons/hive/pkg/openrouter"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// --- watsonx ---

// watsonxGateway adapts pkg/watsonx to dashboard.WatsonxGateway. Stateless:
// the token cache lives in watsonx.DefaultMinter, read at call time so tests
// that swap the minter keep working.
type watsonxGateway struct{}

func (watsonxGateway) EndpointForRegion(region string) string {
	return watsonx.EndpointForRegion(region)
}

func (watsonxGateway) MintToken(ctx context.Context, apiKey string) (string, error) {
	return watsonx.DefaultMinter.Token(ctx, apiKey)
}

func (watsonxGateway) ProjectIDHeader() string { return watsonx.ProjectIDHeader }

func (watsonxGateway) GraniteFallbackModels() []string {
	return append([]string(nil), watsonx.GraniteFallbackModels...)
}

// --- openrouter ---

// openRouterGateway adapts pkg/openrouter to dashboard.OpenRouterGateway.
type openRouterGateway struct{}

func (openRouterGateway) GeneratePKCE() (string, string, error) { return openrouter.GeneratePKCE() }

func (openRouterGateway) BuildAuthorizeURL(callbackURL, codeChallenge, state string) (string, error) {
	return openrouter.BuildAuthorizeURL(callbackURL, codeChallenge, state)
}

func (openRouterGateway) AuthURL() string      { return openrouter.AuthURL }
func (openRouterGateway) BaseURL() string      { return openrouter.BaseURL }
func (openRouterGateway) DefaultModel() string { return openrouter.DefaultModel }

func (openRouterGateway) SuggestedModels() []dashboard.OpenRouterSuggestedModel {
	out := make([]dashboard.OpenRouterSuggestedModel, 0, len(openrouter.SuggestedModels))
	for _, m := range openrouter.SuggestedModels {
		out = append(out, dashboard.OpenRouterSuggestedModel{ID: m.ID, Label: m.Label})
	}
	return out
}

func (openRouterGateway) QRPNG(text string) ([]byte, error) { return openrouter.QRPNG(text) }

func (openRouterGateway) ExchangeCode(code, verifier string) (string, error) {
	return openrouter.ExchangeCode(code, verifier)
}

func (openRouterGateway) FetchCredit(key string) (dashboard.OpenRouterCredit, error) {
	credit, err := openrouter.FetchCredit(key)
	if err != nil {
		return dashboard.OpenRouterCredit{}, err
	}
	return dashboard.OpenRouterCredit{
		Label:          credit.Label,
		Limit:          credit.Limit,
		LimitRemaining: credit.LimitRemaining,
		Usage:          credit.Usage,
	}, nil
}

func (openRouterGateway) NewFlowStore() dashboard.OpenRouterFlowStore {
	return openRouterFlowStore{store: openrouter.NewStateStore(openrouter.StateTTL)}
}

// openRouterFlowStore adapts *openrouter.StateStore to the flattened
// dashboard.OpenRouterFlowStore contract.
type openRouterFlowStore struct{ store *openrouter.StateStore }

func (f openRouterFlowStore) Create(verifier, hiveID, model string) (string, error) {
	return f.store.Create(verifier, hiveID, model)
}

func (f openRouterFlowStore) Consume(state string) (dashboard.OpenRouterFlow, bool) {
	flow, ok := f.store.Consume(state)
	if !ok {
		return dashboard.OpenRouterFlow{}, false
	}
	return dashboard.OpenRouterFlow{Verifier: flow.Verifier, HiveID: flow.HiveID, Model: flow.Model}, true
}

// --- linearagent ---

// linearStoredViewerID is the Dependencies.LinearStoredViewerID probe: the
// persisted install's viewer id ("" when none) plus the store path for the
// validation message.
func linearStoredViewerID() (string, string) {
	path := linearagent.DefaultStorePath()
	return linearagent.StoredViewerID(path), path
}

// newLinearAgentGateway is the Dependencies.NewLinearAgent factory. It
// reproduces the service construction that lived in pkg/dashboard before the
// interface cut — same components, same wiring order, same log lines — and
// returns it behind dashboard.LinearAgentGateway.
func newLinearAgentGateway(logger *slog.Logger) func(dashboard.LinearAgentPorts) dashboard.LinearAgentGateway {
	return func(ports dashboard.LinearAgentPorts) dashboard.LinearAgentGateway {
		return newLinearAgentService(logger, ports)
	}
}

// linearAgentService bundles the linearagent components behind the dashboard's
// LinearAgentGateway interface.
type linearAgentService struct {
	store     *linearagent.Store
	client    *linearagent.Client
	tracker   *linearagent.Tracker
	responder *linearagent.Responder
	receiver  *linearagent.WebhookReceiver
	states    *linearagent.StateStore
	creds     linearagent.Credentials
	// tokenURL / graphqlURL default to production; the dashboard's tests point
	// them at fakes through LinearAgentPorts.
	tokenURL   string
	graphqlURL string
	logger     *slog.Logger
	// storeErr is a store-open failure (corrupt token file). Kept rather than
	// swallowed so status can surface it; install/webhook fail cleanly.
	storeErr error
}

// newLinearAgentService constructs the service. Empty TokenURL/GraphqlURL in
// ports mean production Linear.
func newLinearAgentService(logger *slog.Logger, ports dashboard.LinearAgentPorts) *linearAgentService {
	svc := &linearAgentService{
		creds:      linearagent.CredentialsFromEnv(),
		states:     linearagent.NewStateStore(linearagent.StateTTL),
		tracker:    linearagent.NewTracker(),
		tokenURL:   ports.TokenURL,
		graphqlURL: ports.GraphqlURL,
		logger:     logger,
	}
	store, err := linearagent.NewStore(linearagent.DefaultStorePath())
	if err != nil {
		logger.Error("linear agent: install store unreadable", "error", err)
		svc.storeErr = err
		return svc
	}
	svc.store = store
	svc.client = linearagent.NewClient(store, svc.creds, nil, ports.TokenURL, ports.GraphqlURL, logger)
	svc.responder = linearagent.NewResponder(svc.client, ports.Kick, ports.ResolveSessionAgent, svc.tracker, logger)
	svc.receiver = linearagent.NewWebhookReceiver(svc.responder.HandleSessionEvent, logger)
	return svc
}

func (svc *linearAgentService) StoreErr() error { return svc.storeErr }

func (svc *linearAgentService) Configured() bool { return svc.creds.Configured() }

func (svc *linearAgentService) NewFlowState() (string, error) { return svc.states.Create() }

func (svc *linearAgentService) ConsumeFlowState(state string) bool { return svc.states.Consume(state) }

func (svc *linearAgentService) AuthorizeURL(redirectURI, state string) string {
	return linearagent.BuildAuthorizeURL(svc.creds.ClientID, redirectURI, state)
}

// CompleteInstall exchanges the code, fetches the app's per-workspace
// identity, and persists the install — the same three steps, in the same
// order, with the same log lines as the pre-cut dashboard callback handler.
func (svc *linearAgentService) CompleteInstall(ctx context.Context, code, redirectURI string) (string, error) {
	tokenURL := svc.tokenURL
	if tokenURL == "" {
		tokenURL = linearagent.TokenURL
	}
	tok, err := linearagent.ExchangeCode(ctx, nil, tokenURL, svc.creds, code, redirectURI)
	if err != nil {
		svc.logger.Warn("linear agent: code exchange failed", "error", err.Error())
		return "", err
	}
	ident, err := linearagent.FetchIdentity(ctx, nil, svc.graphqlURL, tok.AccessToken)
	if err != nil {
		svc.logger.Warn("linear agent: identity query failed", "error", err.Error())
		return "", err
	}
	inst := linearagent.Install{
		ViewerID:           ident.ViewerID,
		OrganizationID:     ident.OrganizationID,
		OrganizationName:   ident.OrganizationName,
		OrganizationURLKey: ident.OrganizationURLKey,
		Token:              tok,
		ConnectedAt:        time.Now(),
	}
	if err := svc.store.Set(inst); err != nil {
		svc.logger.Error("linear agent: failed to persist install", "error", err.Error())
		return "", err
	}
	return ident.OrganizationName, nil
}

// errLinearClientUnavailable is AccessToken's failure when the client never
// came up (store-open failure).
var errLinearClientUnavailable = &linearClientUnavailableError{}

type linearClientUnavailableError struct{}

func (*linearClientUnavailableError) Error() string { return "linear client unavailable" }

func (svc *linearAgentService) AccessToken(ctx context.Context) (string, error) {
	if svc.client == nil {
		return "", errLinearClientUnavailable
	}
	return svc.client.AccessToken(ctx)
}

func (svc *linearAgentService) HasInstallStore() bool { return svc.store != nil }

func (svc *linearAgentService) Install() (dashboard.LinearInstall, bool) {
	if svc.store == nil {
		return dashboard.LinearInstall{}, false
	}
	inst, ok := svc.store.Get()
	if !ok {
		return dashboard.LinearInstall{}, false
	}
	return dashboard.LinearInstall{
		ViewerID:           inst.ViewerID,
		OrganizationID:     inst.OrganizationID,
		OrganizationName:   inst.OrganizationName,
		OrganizationURLKey: inst.OrganizationURLKey,
		ConnectedAt:        inst.ConnectedAt,
		HasAccessToken:     inst.Token.AccessToken != "",
	}, true
}

func (svc *linearAgentService) ClearInstall() error {
	if svc.store == nil {
		return errLinearClientUnavailable
	}
	return svc.store.Clear()
}

func (svc *linearAgentService) WebhookHandler() http.Handler {
	if svc.receiver == nil {
		return nil
	}
	return svc.receiver
}

func (svc *linearAgentService) ActiveSessionForIssue(externalID string) (string, string, bool) {
	if svc.tracker == nil {
		return "", "", false
	}
	sess, ok := svc.tracker.ActiveSessionForIssue(externalID)
	if !ok {
		return "", "", false
	}
	return sess.Agent, sess.ID, true
}

func (svc *linearAgentService) SessionsSnapshot() (any, bool) {
	if svc.tracker == nil {
		return nil, false
	}
	return svc.tracker.Snapshot(), true
}

func (svc *linearAgentService) HandlePROpened(agentName, repo string, number int, url string) {
	if svc.responder == nil {
		return
	}
	svc.responder.HandlePROpened(agentName, repo, number, url)
}

func (svc *linearAgentService) AgentEventObserver() func(agentName, event, detail string) {
	if svc.responder == nil {
		return nil
	}
	return svc.responder.HandleAgentEvent
}

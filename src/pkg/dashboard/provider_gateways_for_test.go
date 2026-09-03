package dashboard

// Test-side adapters for the provider-gateway interfaces (#5565 slice 3).
// Production adapters live in cmd/hive (the composition root); these mirrors
// give the dashboard's tests the same real provider behavior. Test files may
// import the provider packages freely — only non-test imports count on the
// pkg/dashboard import graph the decomposition is shrinking.

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/linearagent"
	"github.com/hivecommons/hive/pkg/openrouter"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// --- watsonx ---

type testWatsonxGateway struct{}

func (testWatsonxGateway) EndpointForRegion(region string) string {
	return watsonx.EndpointForRegion(region)
}

// MintToken reads watsonx.DefaultMinter at call time so tests that swap the
// minter for a fake IAM endpoint keep working unchanged.
func (testWatsonxGateway) MintToken(ctx context.Context, apiKey string) (string, error) {
	return watsonx.DefaultMinter.Token(ctx, apiKey)
}

func (testWatsonxGateway) ProjectIDHeader() string { return watsonx.ProjectIDHeader }

func (testWatsonxGateway) GraniteFallbackModels() []string {
	return append([]string(nil), watsonx.GraniteFallbackModels...)
}

// --- openrouter ---

type testOpenRouterGateway struct{}

func (testOpenRouterGateway) GeneratePKCE() (string, string, error) { return openrouter.GeneratePKCE() }

func (testOpenRouterGateway) BuildAuthorizeURL(callbackURL, codeChallenge, state string) (string, error) {
	return openrouter.BuildAuthorizeURL(callbackURL, codeChallenge, state)
}

func (testOpenRouterGateway) AuthURL() string      { return openrouter.AuthURL }
func (testOpenRouterGateway) BaseURL() string      { return openrouter.BaseURL }
func (testOpenRouterGateway) DefaultModel() string { return openrouter.DefaultModel }

func (testOpenRouterGateway) SuggestedModels() []OpenRouterSuggestedModel {
	out := make([]OpenRouterSuggestedModel, 0, len(openrouter.SuggestedModels))
	for _, m := range openrouter.SuggestedModels {
		out = append(out, OpenRouterSuggestedModel{ID: m.ID, Label: m.Label})
	}
	return out
}

func (testOpenRouterGateway) QRPNG(text string) ([]byte, error) { return openrouter.QRPNG(text) }

func (testOpenRouterGateway) ExchangeCode(code, verifier string) (string, error) {
	return openrouter.ExchangeCode(code, verifier)
}

func (testOpenRouterGateway) FetchCredit(key string) (OpenRouterCredit, error) {
	credit, err := openrouter.FetchCredit(key)
	if err != nil {
		return OpenRouterCredit{}, err
	}
	return OpenRouterCredit{
		Label:          credit.Label,
		Limit:          credit.Limit,
		LimitRemaining: credit.LimitRemaining,
		Usage:          credit.Usage,
	}, nil
}

func (testOpenRouterGateway) NewFlowStore() OpenRouterFlowStore {
	return testOpenRouterFlowStore{store: openrouter.NewStateStore(openrouter.StateTTL)}
}

type testOpenRouterFlowStore struct{ store *openrouter.StateStore }

func (f testOpenRouterFlowStore) Create(verifier, hiveID, model string) (string, error) {
	return f.store.Create(verifier, hiveID, model)
}

func (f testOpenRouterFlowStore) Consume(state string) (OpenRouterFlow, bool) {
	flow, ok := f.store.Consume(state)
	if !ok {
		return OpenRouterFlow{}, false
	}
	return OpenRouterFlow{Verifier: flow.Verifier, HiveID: flow.HiveID, Model: flow.Model}, true
}

// --- linearagent ---

func testLinearStoredViewerID() (string, string) {
	path := linearagent.DefaultStorePath()
	return linearagent.StoredViewerID(path), path
}

// testLinearService mirrors cmd/hive's linearAgentService adapter, with the
// component fields reachable so tests can drive the tracker / state store /
// client directly (as they did when the concrete bundle lived here).
type testLinearService struct {
	store      *linearagent.Store
	client     *linearagent.Client
	tracker    *linearagent.Tracker
	responder  *linearagent.Responder
	receiver   *linearagent.WebhookReceiver
	states     *linearagent.StateStore
	creds      linearagent.Credentials
	tokenURL   string
	graphqlURL string
	logger     *slog.Logger
	storeErr   error
}

// newTestLinearAgentFactory returns a Dependencies.NewLinearAgent factory
// whose services talk to the given fake endpoints (empty = production).
func newTestLinearAgentFactory(logger *slog.Logger, tokenURL, graphqlURL string) func(LinearAgentPorts) LinearAgentGateway {
	return func(ports LinearAgentPorts) LinearAgentGateway {
		if ports.TokenURL == "" {
			ports.TokenURL = tokenURL
		}
		if ports.GraphqlURL == "" {
			ports.GraphqlURL = graphqlURL
		}
		return newTestLinearService(logger, ports)
	}
}

func newTestLinearService(logger *slog.Logger, ports LinearAgentPorts) *testLinearService {
	svc := &testLinearService{
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

func (svc *testLinearService) StoreErr() error { return svc.storeErr }

func (svc *testLinearService) Configured() bool { return svc.creds.Configured() }

func (svc *testLinearService) NewFlowState() (string, error) { return svc.states.Create() }

func (svc *testLinearService) ConsumeFlowState(state string) bool { return svc.states.Consume(state) }

func (svc *testLinearService) AuthorizeURL(redirectURI, state string) string {
	return linearagent.BuildAuthorizeURL(svc.creds.ClientID, redirectURI, state)
}

func (svc *testLinearService) CompleteInstall(ctx context.Context, code, redirectURI string) (string, error) {
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

type testLinearClientUnavailable struct{}

func (*testLinearClientUnavailable) Error() string { return "linear client unavailable" }

func (svc *testLinearService) AccessToken(ctx context.Context) (string, error) {
	if svc.client == nil {
		return "", &testLinearClientUnavailable{}
	}
	return svc.client.AccessToken(ctx)
}

func (svc *testLinearService) HasInstallStore() bool { return svc.store != nil }

func (svc *testLinearService) Install() (LinearInstall, bool) {
	if svc.store == nil {
		return LinearInstall{}, false
	}
	inst, ok := svc.store.Get()
	if !ok {
		return LinearInstall{}, false
	}
	return LinearInstall{
		ViewerID:           inst.ViewerID,
		OrganizationID:     inst.OrganizationID,
		OrganizationName:   inst.OrganizationName,
		OrganizationURLKey: inst.OrganizationURLKey,
		ConnectedAt:        inst.ConnectedAt,
		HasAccessToken:     inst.Token.AccessToken != "",
	}, true
}

func (svc *testLinearService) ClearInstall() error {
	if svc.store == nil {
		return &testLinearClientUnavailable{}
	}
	return svc.store.Clear()
}

func (svc *testLinearService) WebhookHandler() http.Handler {
	if svc.receiver == nil {
		return nil
	}
	return svc.receiver
}

func (svc *testLinearService) ActiveSessionForIssue(externalID string) (string, string, bool) {
	if svc.tracker == nil {
		return "", "", false
	}
	sess, ok := svc.tracker.ActiveSessionForIssue(externalID)
	if !ok {
		return "", "", false
	}
	return sess.Agent, sess.ID, true
}

func (svc *testLinearService) SessionsSnapshot() (any, bool) {
	if svc.tracker == nil {
		return nil, false
	}
	return svc.tracker.Snapshot(), true
}

func (svc *testLinearService) HandlePROpened(agentName, repo string, number int, url string) {
	if svc.responder == nil {
		return
	}
	svc.responder.HandlePROpened(agentName, repo, number, url)
}

func (svc *testLinearService) AgentEventObserver() func(agentName, event, detail string) {
	if svc.responder == nil {
		return nil
	}
	return svc.responder.HandleAgentEvent
}

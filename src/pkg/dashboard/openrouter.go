package dashboard

// OpenRouter "scan-to-fund" flow (spoke side).
//
// A sponsor funds THIS hive by scanning a QR (or clicking a link) that starts an
// OpenRouter OAuth PKCE flow. On return, the hive exchanges the code for a
// user-controlled, scoped OpenRouter API key and stores it as a model gateway
// named "openrouter" — reusing the EXISTING per-gateway secret-file store
// (storeGatewayAPIKey): the key VALUE is written to an owner-only file on the
// PVC and only the PATH is recorded in hive.yaml. The key is never logged,
// echoed, or inlined.
//
// The shared PKCE crypto, URL builders, key-exchange/credit HTTP calls, QR PNG
// encoder, and the single-use TTL state store all live in pkg/openrouter,
// reached through the consumer-defined OpenRouterGateway interface on
// Dependencies (#5565 slice 3); this file wires them to the spoke's HTTP
// routes and to the gateway config.
//
// SECURITY:
//   - PKCE S256; the code_verifier NEVER leaves the server.
//   - state is single-use + short-lived; the callback binds to the stored hive.
//   - callback_url is built server-side from an allowlisted own-origin
//     (dashboard.public_url, else hub.dashboard_url, else the request host —
//     see oauthPublicOrigin) — never accepted from the client.
//   - /openrouter/callback is a PUBLIC path (the returning browser has no
//     session yet); the state token is the credential and is verified server-side.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	// openRouterGatewayName is the fixed gateway name a funded key is stored
	// under, so re-funding replaces the same gateway rather than piling up.
	openRouterGatewayName = "openrouter"

	// openRouterCallbackPath is the PUBLIC path the sponsor's browser returns to
	// after authorizing on openrouter.ai. Kept in one const so the route
	// registration, the isPublicPath allowlist, and the callback_url builder
	// agree.
	openRouterCallbackPath = "/openrouter/callback"

	// openRouterConnectedFlag is appended to the dashboard redirect after a
	// successful connect so the UI can show a success indicator.
	openRouterConnectedFlag = "?openrouter=connected"
	// openRouterErrorFlag is appended when the flow fails, so the UI can show why.
	openRouterErrorFlag = "?openrouter=error"
)

// openRouter returns the wired provider gateway, or nil when Dependencies
// does not carry one (bare test servers — the funding routes then answer 503).
func (s *Server) openRouter() OpenRouterGateway {
	if s.deps == nil {
		return nil
	}
	return s.deps.OpenRouter
}

// openRouterState returns the lazily-initialized single-use PKCE state store,
// created once per Server with the production TTL. Nil when no provider
// gateway is wired.
func (s *Server) openRouterState() OpenRouterFlowStore {
	s.openRouterStateOnce.Do(func() {
		if or := s.openRouter(); or != nil {
			s.openRouterFlows = or.NewFlowStore()
		}
	})
	return s.openRouterFlows
}

// registerOpenRouterRoutes registers the spoke-side OpenRouter funding routes.
// Called from the dashboard route registration. The callback is public (added
// to isPublicPath); start/qr/credit ride the normal authenticated mux.
func (s *Server) registerOpenRouterRoutes() {
	s.mux.HandleFunc("GET /api/openrouter/connect/start", s.handleOpenRouterStart)
	s.mux.HandleFunc("GET /api/openrouter/qr", s.handleOpenRouterQR)
	s.mux.HandleFunc("GET /api/openrouter/models", s.handleOpenRouterModels)
	s.mux.HandleFunc("GET /api/openrouter/credit", s.handleOpenRouterCredit)
	s.mux.HandleFunc("GET "+openRouterCallbackPath, s.handleOpenRouterCallback)
}

// openRouterCallbackURL builds THIS hive's callback URL from an allowlisted own
// origin — see oauthPublicOrigin for the precedence (dashboard.public_url,
// hub.dashboard_url, then the forwarded/request host). It NEVER accepts a
// callback origin supplied by the client, so a code can only be redeemed back
// to this server.
func (s *Server) openRouterCallbackURL(r *http.Request) string {
	return s.oauthPublicOrigin(r) + openRouterCallbackPath
}

// handleOpenRouterStart begins a PKCE funding flow for THIS hive. It generates a
// verifier + S256 challenge, records the single-use state (with the chosen
// model), and returns the openrouter.ai/auth URL plus the state. The spoke
// implies "self" — the hive_id is always this hive.
func (s *Server) handleOpenRouterStart(w http.ResponseWriter, r *http.Request) {
	or := s.openRouter()
	if or == nil {
		jsonError(w, "openrouter gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))

	verifier, challenge, err := or.GeneratePKCE()
	if err != nil {
		jsonError(w, "failed to start flow", http.StatusInternalServerError)
		return
	}
	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}
	state, err := s.openRouterState().Create(verifier, hiveID, model)
	if err != nil {
		jsonError(w, "failed to start flow", http.StatusInternalServerError)
		return
	}
	authURL, err := or.BuildAuthorizeURL(s.openRouterCallbackURL(r), challenge, state)
	if err != nil {
		jsonError(w, "failed to build authorize URL", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "openrouter_connect_start", auditDetail("model", model), "")
	jsonResponse(w, map[string]interface{}{"authorize_url": authURL, "state": state})
}

// handleOpenRouterQR renders a QR PNG for the ?data=<authorize_url> query param.
// The image is same-origin (Go-generated PNG), so the dashboard CSP stays clean
// (no vendored JS). Only openrouter.ai/auth URLs are encoded — the data param is
// validated to that host so this cannot be turned into an open QR generator.
func (s *Server) handleOpenRouterQR(w http.ResponseWriter, r *http.Request) {
	or := s.openRouter()
	if or == nil {
		http.Error(w, "openrouter gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	data := r.URL.Query().Get("data")
	if !strings.HasPrefix(data, or.AuthURL()) {
		http.Error(w, "data must be an OpenRouter authorize URL", http.StatusBadRequest)
		return
	}
	png, err := or.QRPNG(data)
	if err != nil {
		http.Error(w, "failed to render QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// handleOpenRouterModels returns the curated suggested model list plus OpenRouter's
// live model catalog (best-effort) so the funding screen's picker can offer
// sensible options and manual entry. No key is needed — /v1/models is public.
func (s *Server) handleOpenRouterModels(w http.ResponseWriter, r *http.Request) {
	or := s.openRouter()
	if or == nil {
		jsonError(w, "openrouter gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	live, _ := fetchModelsFromEndpoint(or.BaseURL(), "")
	jsonResponse(w, map[string]interface{}{
		"suggested": or.SuggestedModels(),
		"models":    live,
		"default":   or.DefaultModel(),
	})
}

// handleOpenRouterCredit proxies GET /api/v1/key using the stored openrouter
// gateway key and returns {limit, limit_remaining, usage, label} so the UI can
// show "$X remaining". It NEVER returns the key itself. hive_id is accepted for
// symmetry with the hub route but is ignored on the spoke (always self).
func (s *Server) handleOpenRouterCredit(w http.ResponseWriter, r *http.Request) {
	or := s.openRouter()
	if or == nil {
		jsonError(w, "openrouter gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	key := s.resolveOpenRouterKey()
	if key == "" {
		jsonResponse(w, map[string]interface{}{"connected": false})
		return
	}
	credit, err := or.FetchCredit(key)
	if err != nil {
		jsonResponse(w, map[string]interface{}{"connected": true, "error": "could not read credit"})
		return
	}
	jsonResponse(w, map[string]interface{}{
		"connected":       true,
		"label":           credit.Label,
		"limit":           credit.Limit,
		"limit_remaining": credit.LimitRemaining,
		"usage":           credit.Usage,
	})
}

// handleOpenRouterCallback is the PUBLIC return endpoint. It recovers the flow
// from the single-use state, exchanges the code+verifier for the user key, and
// stores it as the "openrouter" gateway via the existing secret-file store. On
// success it redirects to the dashboard with ?openrouter=connected.
func (s *Server) handleOpenRouterCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	or := s.openRouter()
	if or == nil || code == "" || state == "" {
		s.redirectOpenRouter(w, r, openRouterErrorFlag)
		return
	}

	flow, ok := s.openRouterState().Consume(state)
	if !ok {
		// Unknown / expired / replayed state — reject.
		s.redirectOpenRouter(w, r, openRouterErrorFlag)
		return
	}

	// Bind to the hive the flow was started for: on the spoke this is always
	// self, but the check makes a code un-redeemable against a different hive.
	if s.deps != nil && s.deps.Config != nil && flow.HiveID != "" && flow.HiveID != s.deps.Config.HiveID {
		s.logger.Warn("openrouter callback hive mismatch", "flow_hive", flow.HiveID, "self", s.deps.Config.HiveID)
		s.redirectOpenRouter(w, r, openRouterErrorFlag)
		return
	}

	key, err := or.ExchangeCode(code, flow.Verifier)
	if err != nil {
		// Never echo the key material; ExchangeCode errors carry only OpenRouter
		// status text, but log at warn without the code/verifier.
		s.logger.Warn("openrouter key exchange failed", "error", err.Error())
		s.redirectOpenRouter(w, r, openRouterErrorFlag)
		return
	}

	model := flow.Model
	if model == "" {
		model = or.DefaultModel()
	}
	if err := s.upsertOpenRouterGateway(key, model); err != nil {
		s.logger.Error("failed to store openrouter gateway", "error", err.Error())
		s.redirectOpenRouter(w, r, openRouterErrorFlag)
		return
	}
	s.auditFromRequest(r, "openrouter_connect_complete", auditDetail("model", model), "")
	s.redirectOpenRouter(w, r, openRouterConnectedFlag)
}

// upsertOpenRouterGateway stores the received key VALUE via the existing
// per-gateway secret-file store (storeGatewayAPIKey) and upserts a gateway named
// "openrouter" (kind=openrouter, endpoint=OpenRouter base, api_key_file=<path>,
// default_model=<model>). Only the file PATH is recorded in hive.yaml — the key
// value never enters config, logs, or API responses. This deliberately reuses
// the same secret-file scheme as the Model Gateways manager (no new secret path).
func (s *Server) upsertOpenRouterGateway(key, model string) error {
	path, err := s.storeGatewayAPIKey(openRouterGatewayName, key)
	if err != nil {
		return fmt.Errorf("store key: %w", err)
	}

	cfg := s.deps.Config
	gws := append([]config.GatewayConfig(nil), cfg.Governor.Gateways...)
	idx := -1
	for i := range gws {
		if strings.EqualFold(gws[i].Name, openRouterGatewayName) {
			idx = i
			break
		}
	}
	endpoint := ""
	if or := s.openRouter(); or != nil {
		endpoint = or.BaseURL()
	}
	gw := config.GatewayConfig{
		Name:         openRouterGatewayName,
		Kind:         config.GatewayKindOpenRouter,
		Endpoint:     endpoint,
		APIKeyFile:   path,
		DefaultModel: model,
	}
	if idx >= 0 {
		// Preserve any operator-set env/CA fields on re-fund; only the key file
		// and default model are (re)written by the funding flow.
		gw.APIKeyEnv = gws[idx].APIKeyEnv
		gw.CABundle = gws[idx].CABundle
		gws[idx] = gw
	} else {
		gws = append(gws, gw)
	}
	cfg.Governor.Gateways = gws

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after openrouter gateway upsert", "error", err)
	}
	s.registerGatewayEndpoints()
	s.refreshAndPersist()
	return nil
}

// ApplyDeliveredGateway stores a gateway the HUB funded on this spoke's behalf
// and delivered over the heartbeat channel. It reuses the same secret-file store
// + gateway-upsert path as the local funding flow, so the delivered key is
// written to an owner-only PVC file and only its path enters hive.yaml. Idempotent:
// re-delivery of the same gateway just re-writes the file and default model.
// Returns an error only on a store/persist failure. The key value is never logged.
func (s *Server) ApplyDeliveredGateway(name, kind, endpoint, defaultModel, key string) error {
	// The current funding flow only mints "openrouter" gateways; guard the name
	// so a delivered payload cannot create an arbitrarily-named gateway.
	if !strings.EqualFold(name, openRouterGatewayName) {
		return fmt.Errorf("unsupported delivered gateway %q", name)
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("delivered gateway has no key")
	}
	model := strings.TrimSpace(defaultModel)
	if model == "" {
		if or := s.openRouter(); or != nil {
			model = or.DefaultModel()
		}
	}
	if err := s.upsertOpenRouterGateway(key, model); err != nil {
		return err
	}
	s.logger.Info("applied hub-delivered openrouter gateway", "default_model", model)
	return nil
}

// resolveOpenRouterKey returns the stored openrouter gateway's key value (from
// its secret file), or "" when no openrouter gateway is configured. Used only
// for the credit proxy — the value is never returned to a client.
func (s *Server) resolveOpenRouterKey() string {
	if s.deps == nil || s.deps.Config == nil {
		return ""
	}
	gw := s.deps.Config.Governor.ResolveGateway(openRouterGatewayName)
	if gw == nil {
		return ""
	}
	return gw.ResolveAPIKey()
}

// redirectOpenRouter sends the browser back to the dashboard root with a status
// flag the UI reads to show connected/error.
func (s *Server) redirectOpenRouter(w http.ResponseWriter, r *http.Request, flag string) {
	http.Redirect(w, r, "/"+flag, http.StatusFound)
}

// ConfiguredGatewayNames returns the names of the model gateways this spoke
// currently has, for the heartbeat read-back.
//
// It is what lets the hub stop re-offering a funded gateway: the hub clears its
// pending record only when the spoke reports that gateway back, so a delivery
// lost in flight — or one that failed inside ApplyDeliveredGateway — is offered
// again on the next beat instead of vanishing.
//
// Names only. A gateway carries a secret key and an endpoint; neither belongs
// in a heartbeat payload, and the name alone is sufficient to confirm arrival.
func (s *Server) ConfiguredGatewayNames() []string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	gws := s.deps.Config.Governor.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	out := make([]string, 0, len(gws))
	for _, gw := range gws {
		if n := strings.TrimSpace(gw.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

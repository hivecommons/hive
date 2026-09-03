package hub

// OpenRouter "scan-to-fund" flow (hub side).
//
// From My Hives, an owner/admin funds a SPECIFIC hive: they pick a default model
// and scan a QR (or click a link) that starts an OpenRouter OAuth PKCE flow on
// the hub. On return, the hub exchanges the code for a user-controlled OpenRouter
// API key and delivers it to the target hive as a model gateway named
// "openrouter" via the heartbeat channel (the only channel that reaches
// firewalled/heartbeat-only spokes like the heartbeat-only cluster uniformly). The spoke stores the
// key in its OWN per-gateway secret-file store — the hub never persists the key.
//
// SECURITY:
//   - PKCE S256; the code_verifier NEVER leaves the hub.
//   - state is single-use + short-lived; the callback binds to the stored
//     hive_id, so a code cannot fund a different hive.
//   - start is gated to the owner/admin of the target hive.
//   - callback_url is the hub's own fixed origin (never client-supplied).
//   - the funded key travels only over the TLS heartbeat channel and is drained
//     on delivery; it is never logged, echoed, or stored on the hub.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/openrouter"
)

const (
	// hubOpenRouterCallbackPath is the hub's PUBLIC OAuth return path.
	hubOpenRouterCallbackPath = "/openrouter/callback"
	// hubOpenRouterGatewayName is the gateway name a funded key is delivered as.
	hubOpenRouterGatewayName = "openrouter"
	// hubOpenRouterConnected / hubOpenRouterError are the dashboard redirect
	// flags the My Hives UI reads to show success/failure.
	hubOpenRouterConnected = "/dashboard?openrouter=connected"
	hubOpenRouterError     = "/dashboard?openrouter=error"
)

// registerOpenRouterRoutes registers the hub-side OpenRouter funding routes.
// start/qr/credit are owner/admin-gated (requireAuth + per-hive role check);
// the callback is PUBLIC (the returning browser's state token is the credential).
func (s *HubServer) registerOpenRouterRoutes() {
	s.mux.HandleFunc("GET /api/openrouter/connect/start", s.requireAuth(s.handleHubOpenRouterStart))
	s.mux.HandleFunc("GET /api/openrouter/qr", s.requireAuth(s.handleHubOpenRouterQR))
	s.mux.HandleFunc("GET /api/openrouter/models", s.requireAuth(s.handleHubOpenRouterModels))
	s.mux.HandleFunc("GET /api/openrouter/credit", s.requireAuth(s.handleHubOpenRouterCredit))
	s.mux.HandleFunc("GET "+hubOpenRouterCallbackPath, s.handleHubOpenRouterCallback)
}

// openRouterState returns the lazily-initialized single-use PKCE state store.
func (s *HubServer) openRouterState() *openrouter.StateStore {
	s.openRouterStateOnce.Do(func() {
		s.openRouterStateStore = openrouter.NewStateStore(openrouter.StateTTL)
	})
	return s.openRouterStateStore
}

// ownsHiveForFunding reports whether username may fund hiveID (its owner, a
// read-write grantee, or the hub admin). Funding stores a spend-capable key on
// the hive, so it is restricted to those who administer the hive.
func (s *HubServer) ownsHiveForFunding(username, hiveID string) bool {
	if isHubAdmin(username) {
		return true
	}
	h := loadSaaSHive(hiveID)
	if h != nil && canonicalEqual(h.Owner, username) {
		return true
	}
	if u := loadSaaSUser(username); u != nil {
		if role, ok := u.Hives[hiveID]; ok && (role == "owner" || role == "read-write") {
			return true
		}
	}
	return false
}

// handleHubOpenRouterStart begins a PKCE funding flow for ?hive_id=<id> (with an
// optional ?model=). It is gated to the owner/admin of that hive. It records the
// single-use state (bound to the hive + model) and returns the authorize URL.
func (s *HubServer) handleHubOpenRouterStart(w http.ResponseWriter, r *http.Request) {
	hiveID := strings.TrimSpace(r.URL.Query().Get("hive_id"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if hiveID == "" {
		http.Error(w, `{"error":"hive_id is required"}`, http.StatusBadRequest)
		return
	}
	username := s.getAuthUser(r)
	if !s.ownsHiveForFunding(username, hiveID) {
		http.Error(w, `{"error":"only the hive owner can fund it"}`, http.StatusForbidden)
		return
	}

	verifier, challenge, err := openrouter.GeneratePKCE()
	if err != nil {
		http.Error(w, `{"error":"failed to start flow"}`, http.StatusInternalServerError)
		return
	}
	state, err := s.openRouterState().Create(verifier, hiveID, model)
	if err != nil {
		http.Error(w, `{"error":"failed to start flow"}`, http.StatusInternalServerError)
		return
	}
	authURL, err := openrouter.BuildAuthorizeURL(hubPublicURL()+hubOpenRouterCallbackPath, challenge, state)
	if err != nil {
		http.Error(w, `{"error":"failed to build authorize URL"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"authorize_url": authURL, "state": state})
}

// handleHubOpenRouterQR renders a QR PNG for ?data=<authorize_url>. Same-origin
// PNG so the hub CSP stays clean. Only openrouter.ai/auth URLs are accepted.
func (s *HubServer) handleHubOpenRouterQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if !strings.HasPrefix(data, openrouter.AuthURL) {
		http.Error(w, "data must be an OpenRouter authorize URL", http.StatusBadRequest)
		return
	}
	png, err := openrouter.QRPNG(data)
	if err != nil {
		http.Error(w, "failed to render QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// handleHubOpenRouterModels returns the curated suggested list + OpenRouter's live
// model catalog so the funding modal's picker can offer options and manual entry.
func (s *HubServer) handleHubOpenRouterModels(w http.ResponseWriter, r *http.Request) {
	live := fetchOpenRouterModels()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"suggested": openrouter.SuggestedModels,
		"models":    live,
		"default":   openrouter.DefaultModel,
	})
}

// handleHubOpenRouterCredit reports whether the hub currently holds an undelivered
// funded gateway for ?hive_id=<id> (i.e. the fund completed but the spoke has not
// yet drained it on a heartbeat). Once delivered, credit is shown on the spoke's
// own dashboard against its stored key — the hub does not keep the key, so it
// cannot proxy /v1/key here. Owner/admin gated.
func (s *HubServer) handleHubOpenRouterCredit(w http.ResponseWriter, r *http.Request) {
	hiveID := strings.TrimSpace(r.URL.Query().Get("hive_id"))
	if hiveID == "" {
		http.Error(w, `{"error":"hive_id is required"}`, http.StatusBadRequest)
		return
	}
	username := s.getAuthUser(r)
	if !s.ownsHiveForFunding(username, hiveID) {
		http.Error(w, `{"error":"only the hive owner can view funding"}`, http.StatusForbidden)
		return
	}
	pendingDelivery := s.hasPendingGateway(hiveID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"pending_delivery": pendingDelivery,
	})
}

// handleHubOpenRouterCallback is the PUBLIC OAuth return. It recovers the flow
// from the single-use state, exchanges the code for the user key, and queues the
// funded gateway for delivery to the bound hive over the next heartbeat. On
// success it redirects to My Hives with ?openrouter=connected.
func (s *HubServer) handleHubOpenRouterCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		http.Redirect(w, r, hubOpenRouterError, http.StatusFound)
		return
	}
	flow, ok := s.openRouterState().Consume(state)
	if !ok {
		http.Redirect(w, r, hubOpenRouterError, http.StatusFound)
		return
	}
	if flow.HiveID == "" {
		http.Redirect(w, r, hubOpenRouterError, http.StatusFound)
		return
	}

	key, err := openrouter.ExchangeCode(code, flow.Verifier)
	if err != nil {
		s.logger.Warn("hub openrouter key exchange failed", "hive_id", flow.HiveID, "error", err.Error())
		http.Redirect(w, r, hubOpenRouterError, http.StatusFound)
		return
	}
	model := flow.Model
	if model == "" {
		model = openrouter.DefaultModel
	}
	s.queuePendingGateway(flow.HiveID, &HeartbeatGatewayConfig{
		Name:         hubOpenRouterGatewayName,
		Kind:         "openrouter",
		Endpoint:     openrouter.BaseURL,
		DefaultModel: model,
		Key:          key,
	})
	s.logger.Info("hub openrouter fund complete; queued gateway for delivery",
		"hive_id", flow.HiveID, "default_model", model)
	http.Redirect(w, r, hubOpenRouterConnected, http.StatusFound)
}

// queuePendingGateway stores a funded gateway for delivery to hiveID over the
// next heartbeat.
func (s *HubServer) queuePendingGateway(hiveID string, gw *HeartbeatGatewayConfig) {
	s.pendingGatewaysMu.Lock()
	defer s.pendingGatewaysMu.Unlock()
	s.pendingGateways[hiveID] = gw
}

// pendingGatewayFor returns the gateway queued for hiveID, clearing it only once
// the spoke has REPORTED that gateway back.
//
// It used to delete on send. That made delivery at-most-once for a value the
// user has already paid for: a beat lost in flight, a hub restart before the
// next beat, or a spoke-side ApplyDeliveredGateway error all dropped the
// gateway permanently, with nothing left to retry from and only a spoke log
// line to say so.
//
// Draining on CONFIRMATION instead makes it at-least-once-then-stop, the same
// contract the GitHub identity record uses. Re-sending until confirmed is safe:
// ApplyDeliveredGateway replaces a gateway of the same name, so a duplicate
// delivery is a no-op rather than a second charge.
//
// reported is what the spoke says it currently has. An EMPTY list means the
// spoke is too old to report — UNKNOWN, not "does not have it" — so it never
// counts as confirmation and never as failure; the delivery simply stays queued
// and keeps being offered.
func (s *HubServer) pendingGatewayFor(hiveID string, reported []string) *HeartbeatGatewayConfig {
	s.pendingGatewaysMu.Lock()
	defer s.pendingGatewaysMu.Unlock()
	gw, ok := s.pendingGateways[hiveID]
	if !ok {
		return nil
	}
	for _, name := range reported {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(gw.Name)) {
			delete(s.pendingGateways, hiveID)
			return nil
		}
	}
	return gw
}

// hasPendingGateway reports whether a funded gateway is queued but not yet
// delivered for hiveID.
func (s *HubServer) hasPendingGateway(hiveID string) bool {
	s.pendingGatewaysMu.Lock()
	defer s.pendingGatewaysMu.Unlock()
	_, ok := s.pendingGateways[hiveID]
	return ok
}

// fetchOpenRouterModels best-effort fetches OpenRouter's public /v1/models list
// for the funding picker. Failure returns nil (the UI falls back to the curated
// list + manual entry).
func fetchOpenRouterModels() []string {
	// Reuse the shared lenient decoder path indirectly: a tiny inline GET keeps
	// the hub free of a dashboard import. No key needed — /v1/models is public.
	return openrouter.PublicModelIDs()
}

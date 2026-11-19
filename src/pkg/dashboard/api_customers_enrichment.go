package dashboard

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// CustomersEnrichmentSettings is GET/POST /api/customers/enrichment/settings.
// The token is never returned, only whether one is set and whether the
// environment supplies it.
type CustomersEnrichmentSettings struct {
	CrowdDevTokenSet         bool `json:"crowddev_token_set"`
	CrowdDevTokenEnvOverride bool `json:"crowddev_token_env_override,omitempty"`
}

type customersEnrichmentSettingsRequest struct {
	// CrowdDevToken is sealed into customers.enrichment.crowddev.token_file;
	// "" clears it. Absent leaves it alone.
	CrowdDevToken *string `json:"crowddev_token,omitempty"`
}

// registerCustomersEnrichmentRoutes registers the owner-only endpoint for the
// one enrichment setting that is a secret: the crowd.dev API token. The
// switches and limits are saved through PUT /api/config/governor/features like
// every other Features toggle. The literals keep the docs citations and the
// OpenAPI route parity guard exact.
func (s *Server) registerCustomersEnrichmentRoutes() {
	s.mux.HandleFunc("GET /api/customers/enrichment/settings", s.handleCustomersEnrichmentSettingsGet)
	s.mux.HandleFunc("POST /api/customers/enrichment/settings", s.handleCustomersEnrichmentSettingsPost)
}

func customersEnrichmentSettings(ctx context.Context, c config.CustomersEnrichmentCrowdDevConfig) CustomersEnrichmentSettings {
	tok, err := c.Token(ctx)
	return CustomersEnrichmentSettings{
		CrowdDevTokenSet:         err == nil && tok != "",
		CrowdDevTokenEnvOverride: strings.TrimSpace(os.Getenv(config.EnvCustomersEnrichmentCrowdDevToken)) != "",
	}
}

func (s *Server) handleCustomersEnrichmentSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, customersEnrichmentSettings(r.Context(), s.deps.Config.Customers.Enrichment.CrowdDev))
}

// handleCustomersEnrichmentSettingsPost (owner) seals the crowd.dev token
// into its keystore file, or removes it for "". The audit line records only
// that the token changed, never its value.
func (s *Server) handleCustomersEnrichmentSettingsPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return
	}
	var req customersEnrichmentSettingsRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	crowd := s.deps.Config.Customers.Enrichment.CrowdDev
	if req.CrowdDevToken != nil {
		if err := config.WriteCustomersEnrichmentSecret(r.Context(), crowd.TokenFileOrDefault(), *req.CrowdDevToken); err != nil {
			jsonError(w, "saving the crowd.dev token failed", http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "customers_enrichment_settings", auditDetail(
			"crowddev_token_changed", "true",
			"crowddev_token_cleared", boolString(strings.TrimSpace(*req.CrowdDevToken) == "")), "")
	}
	jsonResponse(w, customersEnrichmentSettings(r.Context(), crowd))
}

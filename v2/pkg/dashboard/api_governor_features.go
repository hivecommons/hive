package dashboard

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// Sampling-ratio bounds for the tracing head-based sampler. The config treats a
// zero value as "sample everything" (see TracingConfig.SampleRatio), so the
// accepted operator range is the full closed interval [0.0, 1.0].
const (
	minTracingSampleRatio = 0.0
	maxTracingSampleRatio = 1.0
)

// handleGovernorFeatures toggles four already-functional opt-in features from
// the Governor config dialog so operators do not have to hand-edit hive.yaml:
//
//   - ioscan            (Config.Ioscan.Enabled — *bool, nil defaults ON)
//   - tracing           (Config.Tracing.Enabled + Endpoint + SampleRatio)
//   - mint              (Config.Mint.Enabled + Issuer — NOT KeyPath: the signing
//     key is a secret/PEM path and the dashboard overlay is deliberately
//     secret-free)
//   - plan_from_label   (Config.Planning.PlanFromLabel, a *bool tri-state)
//
// Every field is a pointer so an absent key leaves the corresponding config
// untouched — the same "only what you send is changed" contract the other
// governor-config handlers use (see handleGovernorHealth / handleGovernorTrajectory).
// This handler only exposes the features' configuration; it changes none of
// their runtime behavior. saveConfig() persists a secret-free overlay to the
// PVC that the entrypoint merges on restart, so a hosted hive picks the change
// up on its next boot.
func (s *Server) handleGovernorFeatures(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IoscanEnabled *bool `json:"ioscanEnabled"`

		TracingEnabled     *bool    `json:"tracingEnabled"`
		TracingEndpoint    *string  `json:"tracingEndpoint"`
		TracingSampleRatio *float64 `json:"tracingSampleRatio"`

		MintEnabled *bool   `json:"mintEnabled"`
		MintIssuer  *string `json:"mintIssuer"`

		PlanFromLabel *bool `json:"planFromLabel"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.TracingEndpoint != nil {
		ep := strings.TrimSpace(*body.TracingEndpoint)
		if ep != "" {
			u, err := url.Parse(ep)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				jsonError(w, "tracing endpoint must be empty or an http(s) URL", http.StatusBadRequest)
				return
			}
		}
	}
	if body.TracingSampleRatio != nil {
		if *body.TracingSampleRatio < minTracingSampleRatio || *body.TracingSampleRatio > maxTracingSampleRatio {
			jsonError(w, "tracing sample_ratio must be between 0.0 and 1.0", http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.IoscanEnabled != nil {
		v := *body.IoscanEnabled
		cfg.Ioscan.Enabled = &v
	}
	if body.TracingEnabled != nil {
		cfg.Tracing.Enabled = *body.TracingEnabled
	}
	if body.TracingEndpoint != nil {
		cfg.Tracing.Endpoint = strings.TrimSpace(*body.TracingEndpoint)
	}
	if body.TracingSampleRatio != nil {
		cfg.Tracing.SampleRatio = *body.TracingSampleRatio
	}
	if body.MintEnabled != nil {
		cfg.Mint.Enabled = *body.MintEnabled
	}
	if body.MintIssuer != nil {
		cfg.Mint.Issuer = strings.TrimSpace(*body.MintIssuer)
	}
	if body.PlanFromLabel != nil {
		v := *body.PlanFromLabel
		cfg.Planning.PlanFromLabel = &v
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after features update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_features", auditDetail("section", "features"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// featuresSectionResponse builds the opt-in-features payload for the governor
// config GET so the Features dialog can prefill its controls. The mint signing
// key (Config.Mint.KeyPath) is intentionally never returned — only whether mint
// is enabled and its issuer, keeping the response secret-free.
//
// planFromLabel is reported as a tri-state: null when the key is unset (falls
// back to the ACMM-level gate), otherwise the explicit true/false the operator
// chose, so the dialog can show "default" versus an explicit override.
func featuresSectionResponse(cfg *config.Config) map[string]interface{} {
	var planFromLabel interface{}
	if cfg.Planning.PlanFromLabel != nil {
		planFromLabel = *cfg.Planning.PlanFromLabel
	}
	return map[string]interface{}{
		"ioscanEnabled":      cfg.Ioscan.IsEnabled(),
		"tracingEnabled":     cfg.Tracing.Enabled,
		"tracingEndpoint":    cfg.Tracing.Endpoint,
		"tracingSampleRatio": cfg.Tracing.SampleRatio,
		"mintEnabled":        cfg.Mint.Enabled,
		"mintIssuer":         cfg.Mint.Issuer,
		"planFromLabel":      planFromLabel,
	}
}

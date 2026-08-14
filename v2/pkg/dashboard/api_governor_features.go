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

// handleGovernorFeatures updates non-security-posture feature/observability
// settings from the Governor config dialog so operators do not have to hand-edit
// hive.yaml:
//
//   - ioscan            (legacy API compatibility; UI moved to Security)
//   - otel/tracing      (Config.OTel/Tracing Enabled + advanced exporter fields)
//   - retro             (Config.Retro enabled + analysis model)
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
//
// OWNER-ONLY (audit F22, 2026-08-14). The body carries OTelEndpoint,
// OTelHeaders, OTelInsecure and TracingEndpoint, which feed a live OTLP
// exporter (pkg/tracing/tracing.go WithEndpointURL/WithHeaders/WithInsecure).
// Un-gated, any read-write or merger member could redirect the hive's entire
// trace stream — spans carry hive.agent, issue refs, model names and token
// counts (pkg/tracing/semconv.go) — to an attacker-controlled collector, and
// flip OTelInsecure to strip TLS off it. Eleven of the twelve governor-config
// writers already called requireOwnerRole; this one was the lone outlier,
// exactly the asymmetry that established F16.
func (s *Server) handleGovernorFeatures(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		IoscanEnabled *bool `json:"ioscanEnabled"`

		TracingEnabled     *bool              `json:"tracingEnabled"`
		TracingEndpoint    *string            `json:"tracingEndpoint"`
		TracingSampleRatio *float64           `json:"tracingSampleRatio"`
		OTelEnabled        *bool              `json:"otelEnabled"`
		OTelEndpoint       *string            `json:"otelEndpoint"`
		OTelServiceName    *string            `json:"otelServiceName"`
		OTelInsecure       *bool              `json:"otelInsecure"`
		OTelSampleRatio    *float64           `json:"otelSampleRatio"`
		OTelHeaders        *map[string]string `json:"otelHeaders"`

		RetroEnabled       *bool   `json:"retroEnabled"`
		RetroAnalysisModel *string `json:"retroAnalysisModel"`

		MintEnabled *bool   `json:"mintEnabled"`
		MintIssuer  *string `json:"mintIssuer"`

		PlanFromLabel *bool `json:"planFromLabel"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	endpoint := firstStringPtr(body.OTelEndpoint, body.TracingEndpoint)
	if endpoint != nil {
		ep := strings.TrimSpace(*endpoint)
		if ep != "" {
			u, err := url.Parse(ep)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				jsonError(w, "otel endpoint must be empty or an http(s) URL", http.StatusBadRequest)
				return
			}
		}
	}
	sampleRatio := firstFloatPtr(body.OTelSampleRatio, body.TracingSampleRatio)
	if sampleRatio != nil {
		if *sampleRatio < minTracingSampleRatio || *sampleRatio > maxTracingSampleRatio {
			jsonError(w, "otel sample_ratio must be between 0.0 and 1.0", http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.IoscanEnabled != nil {
		v := *body.IoscanEnabled
		cfg.Ioscan.Enabled = &v
	}
	otelEnabled := firstBoolPtr(body.OTelEnabled, body.TracingEnabled)
	if otelEnabled != nil || endpoint != nil || body.OTelServiceName != nil || body.OTelInsecure != nil || sampleRatio != nil || body.OTelHeaders != nil {
		merged := cfg.EffectiveOTel()
		if otelEnabled != nil {
			merged.Enabled = *otelEnabled
		}
		if endpoint != nil {
			merged.Endpoint = strings.TrimSpace(*endpoint)
		}
		if body.OTelServiceName != nil {
			merged.ServiceName = strings.TrimSpace(*body.OTelServiceName)
		}
		if body.OTelInsecure != nil {
			merged.Insecure = *body.OTelInsecure
		}
		if sampleRatio != nil {
			merged.SampleRatio = *sampleRatio
		}
		if body.OTelHeaders != nil {
			merged.Headers = sanitizedHeaderMap(*body.OTelHeaders)
		}
		cfg.OTel = merged
		cfg.Tracing = merged
	}
	if body.RetroEnabled != nil {
		cfg.Retro.Enabled = *body.RetroEnabled
	}
	if body.RetroAnalysisModel != nil {
		cfg.Retro.AnalysisModel = strings.TrimSpace(*body.RetroAnalysisModel)
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
	otelCfg := cfg.EffectiveOTel()
	return map[string]interface{}{
		"ioscanEnabled":      cfg.Ioscan.IsEnabled(),
		"tracingEnabled":     otelCfg.Enabled,
		"tracingEndpoint":    otelCfg.Endpoint,
		"tracingSampleRatio": otelCfg.SampleRatio,
		"otelEnabled":        otelCfg.Enabled,
		"otelEndpoint":       otelCfg.Endpoint,
		"otelServiceName":    otelCfg.ServiceName,
		"otelInsecure":       otelCfg.Insecure,
		"otelSampleRatio":    otelCfg.SampleRatio,
		"otelHasHeaders":     len(otelCfg.Headers) > 0,
		"retroEnabled":       cfg.Retro.Enabled,
		"retroAnalysisModel": cfg.Retro.AnalysisModel,
		"mintEnabled":        cfg.Mint.Enabled,
		"mintIssuer":         cfg.Mint.Issuer,
		"planFromLabel":      planFromLabel,
	}
}

func firstBoolPtr(primary, fallback *bool) *bool {
	if primary != nil {
		return primary
	}
	return fallback
}

func firstFloatPtr(primary, fallback *float64) *float64 {
	if primary != nil {
		return primary
	}
	return fallback
}

func firstStringPtr(primary, fallback *string) *string {
	if primary != nil {
		return primary
	}
	return fallback
}

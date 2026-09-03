package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

var projectObservabilityPlatforms = map[string][]string{
	"open_source": {"prometheus", "grafana", "opentelemetry", "loki", "jaeger", "tempo", "mimir"},
	"kube_native": {"servicemonitor", "podmonitor", "opentelemetry-operator", "grafana-alloy", "kube-state-metrics"},
	"commercial":  {"datadog", "new-relic", "dynatrace", "honeycomb", "splunk", "grafana-cloud", "google-analytics"},
}

var (
	envReferencePattern    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	secretReferencePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?/[A-Za-z0-9._-]+$`)
)

const defaultOperabilityCadence = "24h"

func (s *Server) handleGovernorProjectObservabilityGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.projectObservabilityResponse(s.deps.Config))
}

func (s *Server) handleGovernorProjectObservabilityPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		OpenSource        *[]string                                         `json:"open_source"`
		KubeNative        *[]string                                         `json:"kube_native"`
		Commercial        *[]string                                         `json:"commercial"`
		References        *map[string]config.ProjectObservabilityBackendRef `json:"references"`
		TelemetryEnabled  *bool                                             `json:"telemetry_enabled"`
		OperationsEnabled *bool                                             `json:"operations_enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	openSource, err := validateObservabilityPlatforms(body.OpenSource, "open_source")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	kubeNative, err := validateObservabilityPlatforms(body.KubeNative, "kube_native")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	commercial, err := validateObservabilityPlatforms(body.Commercial, "commercial")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	references, err := validateObservabilityReferences(body.References)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	effective := cfg.Governor.ProjectObservability
	if body.OpenSource != nil {
		effective.OpenSource = openSource
	}
	if body.KubeNative != nil {
		effective.KubeNative = kubeNative
	}
	if body.Commercial != nil {
		effective.Commercial = commercial
	}
	if (body.TelemetryEnabled != nil && *body.TelemetryEnabled) || (body.OperationsEnabled != nil && *body.OperationsEnabled) {
		if len(effective.OpenSource)+len(effective.KubeNative)+len(effective.Commercial) == 0 {
			jsonError(w, "select at least one project observability platform before enabling an agent", http.StatusBadRequest)
			return
		}
	}
	if body.OpenSource != nil {
		cfg.Governor.ProjectObservability.OpenSource = openSource
	}
	if body.KubeNative != nil {
		cfg.Governor.ProjectObservability.KubeNative = kubeNative
	}
	if body.Commercial != nil {
		cfg.Governor.ProjectObservability.Commercial = commercial
	}
	if body.References != nil {
		cfg.Governor.ProjectObservability.References = references
	}
	if body.TelemetryEnabled != nil {
		setOperabilityAgentEnabled(cfg, "telemetry", *body.TelemetryEnabled)
	}
	if body.OperationsEnabled != nil {
		setOperabilityAgentEnabled(cfg, "operations", *body.OperationsEnabled)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist project observability config", "error", err)
	}
	s.auditFromRequest(r, "config_governor_project_observability", auditDetail("section", "project-observability"), "")
	s.refreshAndPersist()
	jsonResponse(w, s.projectObservabilityResponse(cfg))
}

func validateObservabilityPlatforms(values *[]string, family string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	allowed := make(map[string]bool, len(projectObservabilityPlatforms[family]))
	for _, value := range projectObservabilityPlatforms[family] {
		allowed[value] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(*values))
	for _, raw := range *values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if !allowed[value] {
			return nil, fmt.Errorf("unsupported %s observability platform %q", family, raw)
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}

func validateObservabilityReferences(values *map[string]config.ProjectObservabilityBackendRef) (map[string]config.ProjectObservabilityBackendRef, error) {
	if values == nil {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, platforms := range projectObservabilityPlatforms {
		for _, platform := range platforms {
			allowed[platform] = true
		}
	}
	out := make(map[string]config.ProjectObservabilityBackendRef, len(*values))
	for rawName, rawRef := range *values {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !allowed[name] {
			return nil, fmt.Errorf("unsupported observability reference platform %q", rawName)
		}
		ref := config.ProjectObservabilityBackendRef{
			EndpointEnv:      strings.TrimSpace(rawRef.EndpointEnv),
			CredentialSecret: strings.TrimSpace(rawRef.CredentialSecret),
		}
		if ref.EndpointEnv != "" && !envReferencePattern.MatchString(ref.EndpointEnv) {
			return nil, fmt.Errorf("%s endpoint_env must be an environment-variable name, never a literal endpoint", name)
		}
		if ref.CredentialSecret != "" && !secretReferencePattern.MatchString(ref.CredentialSecret) {
			return nil, fmt.Errorf("%s credential_secret must use secret-name/key syntax, never a literal credential", name)
		}
		if ref.EndpointEnv != "" || ref.CredentialSecret != "" {
			out[name] = ref
		}
	}
	return out, nil
}

func setOperabilityAgentEnabled(cfg *config.Config, agent string, enabled bool) {
	for modeName, mode := range cfg.Governor.Modes {
		if mode.Cadences == nil {
			mode.Cadences = make(map[string]config.Cadence)
		}
		current, exists := mode.Cadences[agent]
		if enabled {
			if !exists || current.IsPaused() {
				mode.Cadences[agent] = config.NewIntervalCadence(defaultOperabilityCadence)
			}
		} else {
			mode.Cadences[agent] = config.NewIntervalCadence("paused")
		}
		cfg.Governor.Modes[modeName] = mode
	}
}

func operabilityAgentEnabled(cfg *config.Config, agent string) bool {
	for _, mode := range cfg.Governor.Modes {
		if cadence, ok := mode.Cadences[agent]; ok && !cadence.IsPaused() {
			return true
		}
	}
	return false
}

func projectObservabilitySectionResponse(cfg *config.Config) map[string]interface{} {
	p := cfg.Governor.ProjectObservability
	return map[string]interface{}{
		"open_source":        p.OpenSource,
		"kube_native":        p.KubeNative,
		"commercial":         p.Commercial,
		"references":         p.References,
		"telemetry_enabled":  operabilityAgentEnabled(cfg, "telemetry"),
		"operations_enabled": operabilityAgentEnabled(cfg, "operations"),
		"supported":          projectObservabilityPlatforms,
	}
}

func (s *Server) projectObservabilityResponse(cfg *config.Config) map[string]interface{} {
	response := projectObservabilitySectionResponse(cfg)
	response["detected"] = s.detectedProjectObservability()
	return response
}

// detectedProjectObservability turns the telemetry agent's first advisory run
// into tab suggestions. It never mutates config: the UI marks suggestions dirty
// and the operator's Save is the explicit confirmation/persistence step.
func (s *Server) detectedProjectObservability() map[string][]string {
	if s == nil || s.deps == nil || s.deps.BeadStores == nil {
		return nil
	}
	store := s.deps.BeadStores["telemetry"]
	if store == nil {
		return nil
	}
	texts := make([]string, 0)
	for _, bead := range store.List(beads.ListFilter{}) {
		if bead == nil {
			continue
		}
		texts = append(texts, bead.Title+"\n"+bead.Notes)
	}
	return detectProjectObservabilityPlatforms(texts)
}

func detectProjectObservabilityPlatforms(texts []string) map[string][]string {
	joined := strings.ToLower(strings.Join(texts, "\n"))
	out := map[string][]string{}
	for family, platforms := range projectObservabilityPlatforms {
		for _, platform := range platforms {
			needle := strings.ReplaceAll(platform, "-", " ")
			if strings.Contains(joined, platform) || strings.Contains(joined, needle) {
				out[family] = append(out[family], platform)
			}
		}
		sort.Strings(out[family])
	}
	return out
}

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
		OpenSource *[]string                                         `json:"open_source"`
		KubeNative *[]string                                         `json:"kube_native"`
		Commercial *[]string                                         `json:"commercial"`
		References *map[string]config.ProjectObservabilityBackendRef `json:"references"`
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

// operabilityAgentStatus reports, read-only, what the rest of the dashboard
// already says about one of the operability agents.
//
// Before #7261 this tab owned a third "is this agent on" switch whose
// semantics matched neither of the other two: writing it rewrote the agent's
// cadence in EVERY governor mode (destroying per-mode tuning), and reading it
// back asked "does any mode have a non-paused cadence", which is unrelated to
// the agent's own Enabled flag. The card could therefore say enabled while
// this tab said off. There is now exactly one writer for each fact -- the
// agent card owns Enabled, the Cadences tab owns cadences -- and this function
// only reports them so the operator can see the state without this tab being
// able to clobber it.
func operabilityAgentStatus(cfg *config.Config, agent string) map[string]interface{} {
	status := map[string]interface{}{
		"configured": false,
		"enabled":    false,
		"cadences":   map[string]string{},
	}
	if cfg == nil {
		return status
	}
	if ac, ok := cfg.Agents[agent]; ok {
		status["configured"] = true
		status["enabled"] = ac.Enabled
	}
	cadences := map[string]string{}
	for modeName, mode := range cfg.Governor.Modes {
		if cadence, ok := mode.Cadences[agent]; ok {
			cadences[modeName] = cadence.String()
		}
	}
	status["cadences"] = cadences
	return status
}

func projectObservabilitySectionResponse(cfg *config.Config) map[string]interface{} {
	p := cfg.Governor.ProjectObservability
	return map[string]interface{}{
		"open_source":       p.OpenSource,
		"kube_native":       p.KubeNative,
		"commercial":        p.Commercial,
		"references":        p.References,
		"supported":         projectObservabilityPlatforms,
		"telemetry_status":  operabilityAgentStatus(cfg, "telemetry"),
		"operations_status": operabilityAgentStatus(cfg, "operations"),
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

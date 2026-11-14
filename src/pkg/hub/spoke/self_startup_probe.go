package spoke

import (
	"encoding/json"
	"fmt"
	"time"
)

// Hosted startup probe settings are shared by provisioning and self-upgrades.
// Existing deployments must receive the same budget as newly provisioned ones.
const (
	HostedStartupBudget                   = 10 * time.Minute
	HostedStartupProbeInitialDelaySeconds = 10
	HostedStartupProbePeriodSeconds       = 5
	HostedStartupProbeFailureThreshold    = (int(HostedStartupBudget/time.Second) - HostedStartupProbeInitialDelaySeconds) / HostedStartupProbePeriodSeconds

	// HostedStartupProbePath and HostedDashboardPort are the hosted template's
	// startupProbe handler, used when a hive container has NO startupProbe at
	// all and the whole probe has to be installed (not just its threshold).
	HostedStartupProbePath = "/api/health"
	HostedDashboardPort    = 3002
)

// k8sUpgradePatch backfills the startup budget in the SAME template update as
// the image/restart annotation, avoiding a second rollout. Only the hive
// container is touched: sidecars and init containers keep their configuration.
// An existing hive probe is raised to the template budget (strategic merge
// preserves its handler and other timings). A hive container with NO
// startupProbe gets the template's full probe: pre-#9692 deployments on
// pull-only clusters (where the hub cannot reach the Deployment itself) were
// left with only the 3×30s livenessProbe, so a boot stalled >90s behind forge
// timeouts was SIGTERMed before the dashboard bound and the spoke crash-looped
// for the length of the forge outage.
// A nil body requests only a probe backfill and skips PATCH when none is needed.
func k8sUpgradePatch(path string, body []byte) error {
	current, err := k8sAPIGet(path)
	if err != nil {
		return fmt.Errorf("reading startup probe before upgrade: %w", err)
	}
	return k8sUpgradePatchWithDeployment(path, body, current)
}

// k8sUpgradePatchWithDeployment reuses a Deployment already read by the caller
// so image switching derives container names and probe settings from one GET.
func k8sUpgradePatchWithDeployment(path string, body, current []byte) error {
	var err error
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string `json:"name"`
						StartupProbe *struct {
							FailureThreshold int `json:"failureThreshold"`
						} `json:"startupProbe"`
						ReadinessProbe *probeHandler `json:"readinessProbe"`
						LivenessProbe  *probeHandler `json:"livenessProbe"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(current, &dep); err != nil {
		return fmt.Errorf("decoding startup probe before upgrade: %w", err)
	}
	probeOnly := body == nil
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != selfUpgradeDeployName || (c.StartupProbe != nil && c.StartupProbe.FailureThreshold >= HostedStartupProbeFailureThreshold) {
			continue
		}
		if probeOnly {
			body = []byte(`{"spec":{"template":{}}}`)
		}
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			return fmt.Errorf("decoding upgrade patch: %w", err)
		}
		// All callers supply spec.template; annotation-only restarts do not
		// yet have template.spec, so create it before adding containers.
		template := patch["spec"].(map[string]any)["template"].(map[string]any)
		spec, _ := template["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			template["spec"] = spec
		}
		containers, _ := spec["containers"].([]any)
		var target map[string]any
		for _, entry := range containers {
			container := entry.(map[string]any)
			if container["name"] == c.Name {
				target = container
				break
			}
		}
		if target == nil {
			target = map[string]any{"name": c.Name}
			containers = append(containers, target)
		}
		if c.StartupProbe == nil {
			target["startupProbe"] = fullStartupProbe(startupProbePort(c.ReadinessProbe, c.LivenessProbe))
		} else {
			target["startupProbe"] = map[string]any{"failureThreshold": HostedStartupProbeFailureThreshold}
		}
		spec["containers"] = containers
		body, err = json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("encoding upgrade startup probe: %w", err)
		}
		break
	}
	if body == nil {
		return nil
	}
	return k8sAPIPatch(path, body)
}

// probeHandler is the slice of a Kubernetes probe the backfill needs: the
// httpGet port the dashboard already answers on.
type probeHandler struct {
	HTTPGet *struct {
		Port json.Number `json:"port"`
	} `json:"httpGet"`
}

// startupProbePort picks the port for a newly installed startupProbe: the
// port the existing readiness or liveness probe already targets (the dashboard
// is proven to answer there), else the hosted template default.
func startupProbePort(probes ...*probeHandler) any {
	for _, p := range probes {
		if p == nil || p.HTTPGet == nil || p.HTTPGet.Port == "" {
			continue
		}
		if n, err := p.HTTPGet.Port.Int64(); err == nil && n > 0 {
			return n
		}
		return p.HTTPGet.Port.String()
	}
	return HostedDashboardPort
}

// fullStartupProbe renders the hosted template's startupProbe for a container
// that has none, so the self-upgrade path converges exactly what provisioning
// would have written.
func fullStartupProbe(port any) map[string]any {
	return map[string]any{
		"httpGet":             map[string]any{"path": HostedStartupProbePath, "port": port},
		"initialDelaySeconds": HostedStartupProbeInitialDelaySeconds,
		"periodSeconds":       HostedStartupProbePeriodSeconds,
		"failureThreshold":    HostedStartupProbeFailureThreshold,
	}
}

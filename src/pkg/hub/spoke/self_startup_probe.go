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
)

// k8sUpgradePatch backfills the startup budget in the SAME template update as
// the image/restart annotation, avoiding a second rollout. Only raise an existing
// hive probe: sidecars, init containers and deployments without a probe keep
// their configuration. Strategic merge preserves the handler and other timings.
// A nil body requests only a probe backfill and skips PATCH when none is needed.
func k8sUpgradePatch(path string, body []byte) error {
	current, err := k8sAPIGet(path)
	if err != nil {
		return fmt.Errorf("reading startup probe before upgrade: %w", err)
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string `json:"name"`
						StartupProbe *struct {
							FailureThreshold int `json:"failureThreshold"`
						} `json:"startupProbe"`
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
		if c.Name != selfUpgradeDeployName || c.StartupProbe == nil || c.StartupProbe.FailureThreshold >= HostedStartupProbeFailureThreshold {
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
		target["startupProbe"] = map[string]any{"failureThreshold": HostedStartupProbeFailureThreshold}
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

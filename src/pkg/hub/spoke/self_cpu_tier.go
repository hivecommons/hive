package spoke

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// CPU tiers for a hosted spoke pod, keyed on ACMM level.
//
// The base tier (250m request / 2 CPU limit) was right-sized from fleet p90s
// (see saas_provision.go), but that fleet was overwhelmingly L1–L3: one or two
// agents active at a time. An L5/L6 hive runs 7+ agents and routinely has four
// Copilot/Claude CLIs in flight at once, each burning ~0.5 core, plus the hive
// process itself. Observed 2026-09-15 on console-4vkt (L6): pinned at the 2-core
// limit with 71% of cgroup periods throttled, which turned a 180 ms config GET
// into 27 s and starved the status rebuild loop. CPU is compressible, so the
// pod never died — it just got slower and slower.
//
// L4 and below keep the base tier; the extra cores are only warranted where the
// pack actually grows the concurrent roster.
const (
	// HighCPUACMMLevel is the first ACMM level that receives the high CPU tier.
	HighCPUACMMLevel = 5

	CPURequestBase = "250m"
	CPULimitBase   = "2000m"
	CPURequestHigh = "1000m"
	CPULimitHigh   = "4000m"
)

// CPUTierForLevel returns the CPU request and limit the spoke pod should carry
// at the given ACMM level.
func CPUTierForLevel(level int) (request, limit string) {
	if level >= HighCPUACMMLevel {
		return CPURequestHigh, CPULimitHigh
	}
	return CPURequestBase, CPULimitBase
}

// EnsureCPUTierSelf grows this pod's own Deployment to the high CPU tier when
// the hive's ACMM level warrants it and the Deployment still carries a smaller
// limit. It is the level-change counterpart of the provisioning template:
// levels change long after provisioning (hub delivery, operator set-level),
// and upgrades never re-render the manifest, so without this an L6 hive
// provisioned at L2 keeps L2 compute forever.
//
// Only grows, never shrinks: the patch changes the pod template and therefore
// rolls the pod, which kills every running agent session. That is an
// acceptable one-time cost when an operator raises the level (they are asking
// for more agents), but an automatic shrink on a downgrade would be a
// surprise restart for no functional gain.
//
// Returns patched=true when a patch was issued; the caller should expect a
// SIGTERM shortly after. Off-cluster (no service-account token) is not an
// error — there is nothing to size.
func EnsureCPUTierSelf(logger *slog.Logger, level int) (patched bool, err error) {
	if level < HighCPUACMMLevel {
		return false, nil
	}
	if _, statErr := os.Stat(k8sTokenPath); statErr != nil {
		return false, nil
	}
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return false, fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return false, fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)

	current, err := selfDeploymentCPULimitMillis(path)
	if err != nil {
		return false, err
	}
	wantRequest, wantLimit := CPUTierForLevel(level)
	if current >= parseK8sCPU(wantLimit) {
		return false, nil
	}

	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%q,"resources":{"requests":{"cpu":%q},"limits":{"cpu":%q}}}]}}}}`,
		selfUpgradeDeployName, wantRequest, wantLimit)
	if err := k8sAPIPatch(path, []byte(patch)); err != nil {
		return false, fmt.Errorf("patching deployment cpu tier: %w", err)
	}
	logger.Info("deployment patched to high CPU tier for ACMM level; pod will roll",
		"level", level, "namespace", namespace,
		"cpu_limit_was_millis", current, "cpu_request", wantRequest, "cpu_limit", wantLimit)
	return true, nil
}

// selfDeploymentCPULimitMillis reads the hive container's CPU limit from the
// pod template. A missing limit reads as 0 so the caller treats it as "below
// tier" and sets one.
func selfDeploymentCPULimitMillis(path string) (int64, error) {
	body, err := k8sAPIGet(path)
	if err != nil {
		return 0, err
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name      string `json:"name"`
						Resources struct {
							Limits map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return 0, err
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == selfUpgradeDeployName {
			return parseK8sCPU(c.Resources.Limits["cpu"]), nil
		}
	}
	return 0, fmt.Errorf("deployment at %s has no %q container", path, selfUpgradeDeployName)
}

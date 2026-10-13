package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// heartbeatFallbackDirectPatchAfter is how long the hub lets an armed
	// hub-managed heartbeat fallback ride before assuming the spoke cannot
	// apply it itself. It is longer than the stale-upgrade window so ordinary
	// slow pulls still use the lower-blast-radius heartbeat path first.
	heartbeatFallbackDirectPatchAfter = staleUpgradeTimeout + 5*time.Minute

	// heartbeatFallbackDirectPatchCooldown prevents a still-crash-looping old
	// pod from causing a direct Deployment patch on every heartbeat.
	heartbeatFallbackDirectPatchCooldown = staleUpgradeTimeout

	spokeDeploymentName = "hive"
	restartAtAnnotation = "hive.hivecommons.dev/restart-at"
)

type heartbeatFallbackDirectPatchRecord struct {
	Target   string
	At       time.Time
	Attempts int
}

type heartbeatFallbackDirectPatchInput struct {
	HubManaged       bool
	Paused           bool
	DigestPinned     bool
	KubectlReachable bool
	Target           string
	Current          string
	UpgradeStarted   time.Time
	Now              time.Time
	LastPatch        heartbeatFallbackDirectPatchRecord
}

func shouldPatchHeartbeatFallbackDirectly(in heartbeatFallbackDirectPatchInput) (bool, string) {
	switch {
	case !in.HubManaged:
		return false, "not_hub_managed"
	case in.Paused:
		return false, "spoke_upgrades_paused"
	case in.DigestPinned:
		return false, "digest_pinned"
	case !in.KubectlReachable:
		return false, "cluster_not_kubectl_reachable"
	case in.Target == "":
		return false, "no_target"
	case in.Current != "" && sameCommit(in.Current, in.Target):
		return false, "already_at_target"
	case in.UpgradeStarted.IsZero():
		return false, "upgrade_start_unknown"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(in.UpgradeStarted) < heartbeatFallbackDirectPatchAfter {
		return false, "fallback_not_exhausted"
	}
	if in.LastPatch.Target == in.Target && !in.LastPatch.At.IsZero() &&
		now.Sub(in.LastPatch.At) < heartbeatFallbackDirectPatchCooldown {
		return false, "direct_patch_recently_attempted"
	}
	return true, "fallback_exhausted"
}

func (s *HubServer) patchHeartbeatFallbackDirectly(hive *SaaSHive, cluster *ClusterConfig, target string) error {
	if hive == nil {
		return fmt.Errorf("missing SaaS hive")
	}
	if cluster == nil || !cluster.KubectlReachable() {
		return fmt.Errorf("cluster is not kubectl-reachable")
	}
	image := fmt.Sprintf("ghcr.io/%s:%s", ghcrRepoSpoke, shortSHA(target))
	if err := validateImageRef(image); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradeKubectlTimeout)
	defer cancel()
	namespace := hostedNamespaceForHive(hive)
	getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", spokeDeploymentName,
		"-n", namespace, "-o", "json")
	rawDeployment, err := getCmd.Output()
	if err != nil {
		return fmt.Errorf("kubectl get deployment failed: %w", err)
	}
	patch, err := hostedDeploymentImagePatch(rawDeployment, image, time.Now().UTC())
	if err != nil {
		return err
	}
	patchCmd := kubectlForClusterContext(ctx, cluster, "patch", "deployment", spokeDeploymentName,
		"-n", namespace, "--type=strategic", "-p", patch)
	out, err := patchCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl patch deployment image failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func hostedDeploymentImagePatch(rawDeployment []byte, image string, at time.Time) (string, error) {
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
					} `json:"containers"`
					InitContainers []struct {
						Name string `json:"name"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rawDeployment, &dep); err != nil {
		return "", fmt.Errorf("parsing deployment JSON: %w", err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 && len(dep.Spec.Template.Spec.InitContainers) == 0 {
		return "", fmt.Errorf("no containers found on deployment %s", spokeDeploymentName)
	}

	containerPatches := make([]map[string]string, 0, len(dep.Spec.Template.Spec.Containers))
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "" {
			continue
		}
		containerPatches = append(containerPatches, map[string]string{"name": c.Name, "image": image})
	}
	initContainerPatches := make([]map[string]string, 0, len(dep.Spec.Template.Spec.InitContainers))
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		if c.Name == "" {
			continue
		}
		initContainerPatches = append(initContainerPatches, map[string]string{"name": c.Name, "image": image})
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						restartAtAnnotation: at.Format(time.RFC3339),
					},
				},
				"spec": map[string]any{
					"containers":     containerPatches,
					"initContainers": initContainerPatches,
				},
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return "", fmt.Errorf("building deployment image patch: %w", err)
	}
	return string(data), nil
}

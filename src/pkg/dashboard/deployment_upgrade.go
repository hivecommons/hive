package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	deploymentRuntimeUnknown       = "unknown"
	deploymentRuntimeKubernetes    = "kubernetes"
	deploymentRuntimePodmanQuadlet = "podman-quadlet"
	deploymentRuntimeDockerCompose = "docker-compose"

	podmanModeUnknown  = "unknown"
	podmanModeRootless = "rootless"
	podmanModeRootful  = "rootful"
)

var (
	kubernetesServiceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	defaultDashboardUpgradeHelper         = "/usr/local/libexec/hive-dashboard-upgrade-helper"
	standaloneUpgradeTimeout              = 10 * time.Minute
	standaloneUpgradeTargetRE             = regexp.MustCompile(`\A[0-9a-fA-F]{7,40}\z`)
)

type deploymentInfo struct {
	Runtime          string `json:"runtime"`
	PodmanMode       string `json:"podmanMode,omitempty"`
	UpgradeSupported bool   `json:"upgradeSupported"`
	UpgradeAction    string `json:"upgradeAction,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

func (s *Server) detectDeployment() deploymentInfo {
	cfg := (*config.Config)(nil)
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	info := deploymentInfo{Runtime: deploymentRuntimeUnknown, UpgradeSupported: false, Reason: "deployment runtime is not explicitly configured"}
	rawRuntime := firstNonEmpty(os.Getenv("HIVE_DEPLOYMENT_RUNTIME"), deploymentConfigValue(cfg, "runtime"))
	if rawRuntime != "" {
		switch normalizeDeploymentRuntime(rawRuntime) {
		case deploymentRuntimeKubernetes:
			info.Runtime = deploymentRuntimeKubernetes
			info.UpgradeSupported = true
			info.UpgradeAction = "hub"
			info.Reason = ""
			return info
		case deploymentRuntimePodmanQuadlet:
			info.Runtime = deploymentRuntimePodmanQuadlet
			info.PodmanMode = normalizePodmanMode(firstNonEmpty(os.Getenv("HIVE_DEPLOYMENT_PODMAN_MODE"), deploymentConfigValue(cfg, "podman_mode")))
			if info.PodmanMode == podmanModeUnknown {
				info.Reason = "Podman/Quadlet mode is not explicitly configured as rootless or rootful"
				return info
			}
			if standaloneUpgradeHelper(cfg) == "" {
				info.Reason = "standalone upgrade helper is not configured or executable"
				return info
			}
			info.UpgradeSupported = true
			info.UpgradeAction = "podman-quadlet"
			info.Reason = ""
			return info
		case deploymentRuntimeDockerCompose:
			info.Runtime = deploymentRuntimeDockerCompose
			if standaloneUpgradeHelper(cfg) == "" {
				info.Reason = "standalone upgrade helper is not configured or executable"
				return info
			}
			info.UpgradeSupported = true
			info.UpgradeAction = "docker-compose"
			info.Reason = ""
			return info
		default:
			info.Reason = fmt.Sprintf("unsupported HIVE_DEPLOYMENT_RUNTIME %q", rawRuntime)
			return info
		}
	}
	if cfg != nil && strings.TrimSpace(cfg.Hub.URL) != "" && strings.TrimSpace(cfg.HiveID) != "" && fileExists(kubernetesServiceAccountNamespacePath) {
		info.Runtime = deploymentRuntimeKubernetes
		info.UpgradeSupported = true
		info.UpgradeAction = "hub"
		info.Reason = ""
		return info
	}
	return info
}

func normalizeDeploymentRuntime(runtime string) string {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "kubernetes", "k8s", "hub":
		return deploymentRuntimeKubernetes
	case "podman", "quadlet", "podman-quadlet":
		return deploymentRuntimePodmanQuadlet
	case "docker", "compose", "docker-compose":
		return deploymentRuntimeDockerCompose
	case "", "unknown":
		return deploymentRuntimeUnknown
	default:
		return ""
	}
}

func normalizePodmanMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "rootless", "user":
		return podmanModeRootless
	case "rootful", "system":
		return podmanModeRootful
	default:
		return podmanModeUnknown
	}
}

func deploymentConfigValue(cfg *config.Config, key string) string {
	if cfg == nil {
		return ""
	}
	switch key {
	case "runtime":
		return cfg.Deployment.Runtime
	case "podman_mode":
		return cfg.Deployment.PodmanMode
	default:
		return ""
	}
}

func standaloneUpgradeHelper(cfg *config.Config) string {
	helper := firstNonEmpty(os.Getenv("HIVE_DASHBOARD_UPGRADE_HELPER"), func() string {
		if cfg == nil {
			return ""
		}
		return cfg.Deployment.UpgradeHelper
	}(), defaultDashboardUpgradeHelper)
	if helper == "" || !strings.HasPrefix(helper, "/") {
		return ""
	}
	if st, err := os.Stat(helper); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
		return helper
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func parseStandaloneUpgradeTarget(r *http.Request) (string, error) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" && r.Body != nil {
		var body struct {
			Target string `json:"target"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		target = strings.TrimSpace(body.Target)
	}
	if !standaloneUpgradeTargetRE.MatchString(target) {
		return "", fmt.Errorf("target must be a 7-40 character hexadecimal Hive image tag")
	}
	if len(target) > 7 {
		target = target[:7]
	}
	return "ghcr.io/hivecommons/hive:" + strings.ToLower(target), nil
}

func (s *Server) runStandaloneUpgrade(r *http.Request, info deploymentInfo) error {
	targetRef, err := parseStandaloneUpgradeTarget(r)
	if err != nil {
		return err
	}
	helper := standaloneUpgradeHelper(s.deps.Config)
	if helper == "" {
		return fmt.Errorf("standalone upgrade helper is not configured or executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), standaloneUpgradeTimeout)
	defer cancel()
	args := []string{"upgrade", "--runtime", info.Runtime, "--ref", targetRef}
	if info.Runtime == deploymentRuntimePodmanQuadlet {
		args = append(args, "--podman-mode", info.PodmanMode)
	}
	cmd := exec.CommandContext(ctx, helper, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("standalone upgrade helper failed: %s", msg)
	}
	return nil
}

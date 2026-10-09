package dashboard

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	kubernetesServiceAccountTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	kubernetesServiceAccountCACertPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	kubernetesAPIServerURL                = "https://kubernetes.default.svc"
	defaultDashboardUpgradeHelper         = "/usr/local/libexec/hive-dashboard-upgrade-helper"
	standaloneUpgradeTimeout              = 10 * time.Minute
	standaloneUpgradeTargetRE             = regexp.MustCompile(`\A[0-9a-fA-F]{7,40}\z`)
	standaloneUpgradeRefRE                = regexp.MustCompile(`\Aghcr\.io/hivecommons/hive(:[A-Za-z0-9._-]+|@sha256:[0-9a-f]{64})\z`)
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
	info := deploymentInfo{Runtime: deploymentRuntimeUnknown, UpgradeSupported: false, Reason: "deployment runtime is not explicitly configured; if this is a Podman/Quadlet host, upgrade from the host with `systemctl --user start podman-auto-update.service` (rootless, as the hive user) or `systemctl start podman-auto-update.service` (rootful), or rerun bin/hive-podman-setup.sh to add HIVE_DEPLOYMENT_RUNTIME"}
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
				info.Reason = "Podman/Quadlet mode is not explicitly configured as rootless or rootful; upgrade from the host with `systemctl --user start podman-auto-update.service` (rootless, as the hive user) or `systemctl start podman-auto-update.service` (rootful)"
				return info
			}
			if !upgradeRequestDirWritable(upgradeRequestDir(cfg)) {
				info.Reason = "standalone upgrade helper is not available inside the container; upgrade from the host with `" + podmanHostUpgradeCommand(info.PodmanMode) + "` (as the hive user when rootless)"
				return info
			}
			info.UpgradeSupported = true
			info.UpgradeAction = "podman-quadlet-request"
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

func podmanHostUpgradeCommand(mode string) string {
	if mode == podmanModeRootless {
		return "systemctl --user start podman-auto-update.service"
	}
	return "systemctl start podman-auto-update.service"
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
	case "upgrade_request_dir":
		return cfg.Deployment.UpgradeRequestDir
	default:
		return ""
	}
}

func upgradeRequestDir(cfg *config.Config) string {
	return firstNonEmpty(os.Getenv("HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR"), deploymentConfigValue(cfg, "upgrade_request_dir"))
}

// Probe actual write access (including read-only mounts and ACLs), not mode bits.
// The host watches only *.json; hidden probe/temp files never trigger an upgrade.
func upgradeRequestDirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".hive-upgrade-probe-*")
	if err != nil {
		return false
	}
	closeErr := f.Close()
	removeErr := os.Remove(f.Name())
	return closeErr == nil && removeErr == nil
}

type upgradeRequest struct {
	TargetRef   string `json:"target_ref"`
	Requester   string `json:"requester"`
	RequestedAt string `json:"requested_at"`
}

func writeUpgradeRequest(dir, targetRef, requester string) error {
	if dir == "" {
		return fmt.Errorf("upgrade request directory is not configured")
	}
	data, err := json.Marshal(upgradeRequest{
		TargetRef: targetRef, Requester: requester,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".hive-upgrade-*")
	if err != nil {
		return fmt.Errorf("creating upgrade request: %w", err)
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing upgrade request: %w", err)
	}
	// CreateTemp uses mode 0600. On rootless Podman the container uid maps to
	// a host subuid, so the host drain (hive-upgrade.service, running as the
	// hive user) could not read a 0600 file. The payload is an image ref, a
	// login and a timestamp, so 0644 exposes nothing secret; the directory
	// mode still limits who can reach it.
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return fmt.Errorf("making upgrade request readable: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing upgrade request: %w", err)
	}
	// Publish only after close, in the same directory/filesystem. CreateTemp
	// gives each request a unique name.
	final := filepath.Join(dir, strings.TrimPrefix(filepath.Base(temp), ".")+".json")
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("publishing upgrade request: %w", err)
	}
	return nil
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
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid upgrade query: %w", err)
	}
	target := strings.TrimSpace(query.Get("target"))
	if target == "" && r.Body != nil {
		var body struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && err != io.EOF {
			return "", fmt.Errorf("invalid upgrade target JSON: %w", err)
		}
		target = strings.TrimSpace(body.Target)
	}
	if target == "" {
		target = standaloneTrackedChannelRef()
		if target == "" {
			return "", fmt.Errorf("no tracked release channel; supply an explicit Hive image ref")
		}
	}
	if standaloneUpgradeRefRE.MatchString(target) {
		return target, nil
	}
	if !standaloneUpgradeTargetRE.MatchString(target) {
		return "", fmt.Errorf("target must be a Hive image ref or a 7-40 character hexadecimal Hive image tag")
	}
	if len(target) > 7 {
		target = target[:7]
	}
	return "ghcr.io/hivecommons/hive:" + strings.ToLower(target), nil
}

func dashboardUpgradeTargetFromRequest(r *http.Request) string {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" && r.Body != nil {
		var body struct {
			Target string `json:"target"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		target = strings.TrimSpace(body.Target)
	}
	if len(target) > 7 && standaloneUpgradeTargetRE.MatchString(target) {
		target = target[:7]
	}
	if standaloneUpgradeTargetRE.MatchString(target) {
		return strings.ToLower(target)
	}
	return target
}

// Use the installed deployment metadata, not the branch this binary was built
// from. A digest pin is not a channel subscription, even if it carries a tag.
func standaloneTrackedChannelRef() string {
	ref := strings.TrimSpace(os.Getenv("HIVE_SELF_IMAGE"))
	if !standaloneUpgradeRefRE.MatchString(ref) || standaloneReleaseChannel(ref) == "" {
		return ""
	}
	return ref
}

func (s *Server) precheckKubernetesSelfUpgrade(target string) error {
	target = strings.TrimSpace(target)
	if target != "" {
		if !standaloneUpgradeTargetRE.MatchString(target) {
			return fmt.Errorf("target must be a 7-40 character hexadecimal Hive image tag")
		}
		if len(target) > 7 {
			target = target[:7]
		}
		if !ghcrTagExistsCached(target) {
			return fmt.Errorf("target image ghcr.io/hivecommons/hive:%s is not published yet", target)
		}
	}
	if err := kubernetesSelfUpgradeRBACPrecheck(); err != nil {
		return err
	}
	return nil
}

func kubernetesSelfUpgradeRBACPrecheck() error {
	nsBytes, err := os.ReadFile(kubernetesServiceAccountNamespacePath)
	if err != nil {
		return fmt.Errorf("cannot verify spoke self-upgrade RBAC: reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(nsBytes))
	if namespace == "" {
		return fmt.Errorf("cannot verify spoke self-upgrade RBAC: empty namespace from %s", kubernetesServiceAccountNamespacePath)
	}
	token, err := os.ReadFile(kubernetesServiceAccountTokenPath)
	if err != nil {
		return fmt.Errorf("cannot verify spoke self-upgrade RBAC: reading service account token: %w", err)
	}
	caCert, err := os.ReadFile(kubernetesServiceAccountCACertPath)
	if err != nil {
		return fmt.Errorf("cannot verify spoke self-upgrade RBAC: reading cluster CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caCert) {
		return fmt.Errorf("cannot verify spoke self-upgrade RBAC: cluster CA bundle is invalid")
	}
	client := &http.Client{
		Timeout:   selfUpgradePrecheckTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeploymentName)
	if err := kubernetesSelfUpgradeRequest(client, http.MethodGet, path, nil, token); err != nil {
		return fmt.Errorf("spoke self-upgrade precheck failed: service account cannot get deployment/%s: %w", selfUpgradeDeploymentName, err)
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"hive.hivecommons.dev/upgrade-precheck-at":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err := kubernetesSelfUpgradeRequest(client, http.MethodPatch, path+"?dryRun=All", []byte(patch), token); err != nil {
		return fmt.Errorf("spoke self-upgrade precheck failed: service account cannot patch deployment/%s: %w", selfUpgradeDeploymentName, err)
	}
	return nil
}

const (
	selfUpgradePrecheckTimeout = 10 * time.Second
	selfUpgradeDeploymentName  = "hive"
)

func kubernetesSelfUpgradeRequest(client *http.Client, method, path string, body []byte, token []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), selfUpgradePrecheckTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, kubernetesAPIServerURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kubernetes API %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (s *Server) runStandaloneUpgrade(r *http.Request, info deploymentInfo) error {
	targetRef, err := parseStandaloneUpgradeTarget(r)
	if err != nil {
		return err
	}
	if info.Runtime == deploymentRuntimePodmanQuadlet {
		if info.UpgradeAction != "podman-quadlet-request" {
			return fmt.Errorf("Podman/Quadlet upgrades require the host request bridge")
		}
		return writeUpgradeRequest(upgradeRequestDir(s.deps.Config), targetRef, requestUser(r))
	}
	helper := standaloneUpgradeHelper(s.deps.Config)
	if helper == "" {
		return fmt.Errorf("standalone upgrade helper is not configured or executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), standaloneUpgradeTimeout)
	defer cancel()
	args := []string{"upgrade", "--runtime", info.Runtime, "--ref", targetRef}
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

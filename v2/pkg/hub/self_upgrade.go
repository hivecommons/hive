package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	selfUpgradeDeployName = "hive"
	selfUpgradeTimeout    = 10 * time.Second
)

// k8sNamespacePath is a var (not a const) so tests can redirect the
// service-account namespace file at a temp path. Production never reassigns it.
var k8sNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// RolloutRestartSelf patches this pod's own Deployment to trigger a
// rolling restart via the in-cluster K8s API.  This avoids os.Exit(0)
// which kills the pod immediately and causes downtime for spokes whose
// hub can't reach them via kubectl.
//
// Returns nil on success (K8s will send SIGTERM once the new pod is
// Ready).  On any failure the caller should fall back to os.Exit(0).
func RolloutRestartSelf(logger *slog.Logger) error {
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}

	restartAnnotation := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"hive.kubestellar.io/restart-at":"%s"}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)

	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	if err := k8sAPIPatch(path, []byte(restartAnnotation)); err != nil {
		return fmt.Errorf("patching deployment %s/%s: %w", namespace, selfUpgradeDeployName, err)
	}

	logger.Info("deployment patched for rolling restart, waiting for SIGTERM",
		"namespace", namespace,
		"deployment", selfUpgradeDeployName,
	)
	return nil
}

// SwitchImageSelf patches this pod's own Deployment to change the image of
// every container (including init containers) to the given tag via the
// in-cluster K8s API. Used for branch switches delivered over heartbeat to
// spokes whose hub can't reach them via kubectl (the spoke has no kubectl
// binary, but its SA holds the hive-self-upgrade role: patch on
// deployment/hive). A strategic-merge patch merges containers by name, so we
// enumerate the deployment's containers first and set each image.
func SwitchImageSelf(logger *slog.Logger, image string) error {
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)

	names, err := k8sDeploymentContainerNames(path)
	if err != nil {
		return fmt.Errorf("listing deployment containers: %w", err)
	}
	if len(names.containers) == 0 && len(names.initContainers) == 0 {
		return fmt.Errorf("no containers found on deployment %s/%s", namespace, selfUpgradeDeployName)
	}

	var cs, ics []string
	for _, n := range names.containers {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	for _, n := range names.initContainers {
		ics = append(ics, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[%s],"initContainers":[%s]},"metadata":{"annotations":{"hive.kubestellar.io/restart-at":%q}}}}}`,
		strings.Join(cs, ","), strings.Join(ics, ","), time.Now().UTC().Format(time.RFC3339),
	)
	if err := k8sAPIPatch(path, []byte(patch)); err != nil {
		return fmt.Errorf("patching deployment image: %w", err)
	}
	logger.Info("deployment image switched via in-cluster API, pod will roll",
		"namespace", namespace, "deployment", selfUpgradeDeployName, "image", image)
	return nil
}

type deploymentContainerNames struct {
	containers     []string
	initContainers []string
}

// k8sDeploymentContainerNames GETs the deployment and returns its container +
// init-container names, so a strategic-merge image patch targets them by name.
func k8sDeploymentContainerNames(path string) (deploymentContainerNames, error) {
	var out deploymentContainerNames
	body, err := k8sAPIGet(path)
	if err != nil {
		return out, err
	}
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
	if err := json.Unmarshal(body, &dep); err != nil {
		return out, err
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		out.containers = append(out.containers, c.Name)
	}
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		out.initContainers = append(out.initContainers, c.Name)
	}
	return out, nil
}

func k8sAPIPatch(path string, body []byte) error {
	token, err := os.ReadFile(k8sTokenPath)
	if err != nil {
		return fmt.Errorf("reading service account token: %w", err)
	}

	tlsConfig := &tls.Config{}
	if caCert, err := os.ReadFile(k8sCACertPath); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = pool
	} else {
		tlsConfig.InsecureSkipVerify = true
	}

	client := &http.Client{
		Timeout:   selfUpgradeTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	ctx, cancel := context.WithTimeout(context.Background(), selfUpgradeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, k8sAPIServer+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("k8s API PATCH %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("k8s API PATCH %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

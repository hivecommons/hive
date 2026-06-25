package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	k8sNamespacePath        = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	selfUpgradeDeployName   = "hive"
	selfUpgradeTimeout      = 10 * time.Second
)

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

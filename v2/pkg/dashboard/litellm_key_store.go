package dashboard

// Storage for API key VALUES entered in the dashboard (currently the
// LiteLLM gateway key). hive.yaml only ever stores a file PATH
// (api_key_file); the key value itself is written to:
//
//  1. A 0600 file on the PVC (/data/secrets/litellm_api_key) — the
//     authoritative store hive.yaml points at. It takes effect instantly
//     and works on every hive (hosted k8s, Docker, LXC).
//  2. Best-effort, the hive's own hive-secrets Kubernetes Secret, patched
//     via the in-cluster API (requires the hive-secrets-writer Role from
//     the provisioning template). This keeps a durable copy outside the
//     PVC; on hives whose deployment whole-volume-mounts the Secret it
//     also surfaces at /secrets/litellm_api_key, which ResolveAPIKey
//     consults ahead of the PVC file.
//
// The PVC file — not the Secret mount — is what api_key_file records,
// because the Secret path cannot be assumed readable: hives provisioned
// from older templates mount hive-secrets with an items filter that
// excludes litellm_api_key (or do not mount /secrets at all), and even on
// current-template hives the kubelet takes ~2 minutes to project a
// patched Secret into the mount. The PVC file has neither problem.
//
// The key value must never be logged, echoed in API responses, or
// written to hive.yaml.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

const (
	// hiveSecretsName is the namespace-local Secret the pod may patch
	// (matches the provisioning template and the hive-secrets-writer Role).
	hiveSecretsName = "hive-secrets"
	// litellmSecretDataKey is the key inside hive-secrets holding the
	// LiteLLM API key; it surfaces as /secrets/litellm_api_key on hives
	// that whole-volume-mount the Secret.
	litellmSecretDataKey = "litellm_api_key"
	// secretPatchTimeout bounds the in-cluster API PATCH call.
	secretPatchTimeout = 10 * time.Second
	// litellmKeyFileMode: the key file is read only by the main hive
	// process (dashboard + inference translator run in-process as the
	// same user), so owner-only is the tightest mode that works.
	litellmKeyFileMode = 0o600
	// litellmKeyDirMode keeps the PVC secrets dir owner-only too.
	litellmKeyDirMode = 0o700
)

// Package vars (not consts) so tests can point them at temp dirs / fake
// API servers. Production values never change at runtime.
var (
	// writableLiteLLMKeyFile is the PVC key file recorded in api_key_file.
	writableLiteLLMKeyFile = config.WritableLiteLLMAPIKeyFile
	// serviceAccountDir holds the pod's token/namespace/ca.crt files.
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// errNotInCluster marks the expected condition of not running inside a
// Kubernetes pod (Docker/LXC hives), where the Secret patch is skipped
// silently.
var errNotInCluster = errors.New("not running in a Kubernetes pod")

// storeLiteLLMAPIKey persists a LiteLLM API key value and returns the
// api_key_file path to record in hive.yaml. Only the chosen store is ever
// logged — never the key.
func (s *Server) storeLiteLLMAPIKey(key string) (string, error) {
	if err := writeLiteLLMKeyFile(key); err != nil {
		return "", err
	}
	s.logger.Info("litellm api key stored", "api_key_file", writableLiteLLMKeyFile)
	if err := patchKeyIntoHiveSecrets(litellmSecretDataKey, key); err == nil {
		s.logger.Info("litellm api key also patched into hive-secrets Secret",
			"mount_path", config.DefaultLiteLLMAPIKeyFile)
	} else if !errors.Is(err, errNotInCluster) {
		s.logger.Info("hive-secrets Secret not patched; PVC key file is the only store",
			"reason", err.Error())
	}
	return writableLiteLLMKeyFile, nil
}

// writeLiteLLMKeyFile writes the key value to the PVC key file with
// owner-only permissions.
func writeLiteLLMKeyFile(key string) error {
	dir := filepath.Dir(writableLiteLLMKeyFile)
	if err := os.MkdirAll(dir, litellmKeyDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.WriteFile(writableLiteLLMKeyFile, []byte(key), litellmKeyFileMode); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}
	// WriteFile does not change the mode of a pre-existing file.
	if err := os.Chmod(writableLiteLLMKeyFile, litellmKeyFileMode); err != nil {
		return fmt.Errorf("tightening key file mode: %w", err)
	}
	return nil
}

// patchKeyIntoHiveSecrets PATCHes the hive-secrets Secret in the pod's own
// namespace, setting data[dataKey] = value, using the mounted
// serviceaccount credentials. Errors never contain the value.
func patchKeyIntoHiveSecrets(dataKey, value string) error {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return errNotInCluster
	}
	token, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return fmt.Errorf("reading serviceaccount token: %w", err)
	}
	namespace, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
	if err != nil {
		return fmt.Errorf("reading serviceaccount namespace: %w", err)
	}
	caCert, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("reading serviceaccount ca.crt: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return fmt.Errorf("serviceaccount ca.crt contains no certificates")
	}

	patch, err := json.Marshal(map[string]interface{}{
		"data": map[string]string{
			dataKey: base64.StdEncoding.EncodeToString([]byte(value)),
		},
	})
	if err != nil {
		return fmt.Errorf("building patch: %w", err)
	}

	patchURL := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/secrets/%s",
		host, port, strings.TrimSpace(string(namespace)), hiveSecretsName)
	ctx, cancel := context.WithTimeout(context.Background(), secretPatchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")

	client := &http.Client{
		Timeout: secretPatchTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling kubernetes API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Deliberately status-only: API error bodies can echo patch
		// contents, which would leak the key value into logs/responses.
		return fmt.Errorf("kubernetes API returned %d patching %s (missing hive-secrets-writer Role?)",
			resp.StatusCode, hiveSecretsName)
	}
	return nil
}

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
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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
	pvcErr := writeLiteLLMKeyFile(key)
	if pvcErr == nil {
		s.logger.Info("litellm api key stored", "api_key_file", writableLiteLLMKeyFile)
	} else {
		// The PVC file is the store hive.yaml points at, but a misowned or
		// read-only volume must not sink the whole save: an in-cluster hive
		// with the hive-secrets-writer Role can still persist the key via the
		// Secret (which surfaces at /secrets/litellm_api_key). Attempt that
		// fallback below and only fail the save if BOTH stores fail.
		s.logger.Info("litellm api key PVC file write failed; attempting hive-secrets Secret fallback",
			"api_key_file", writableLiteLLMKeyFile, "reason", pvcErr.Error())
	}

	secretErr := patchKeyIntoHiveSecrets(litellmSecretDataKey, key)
	switch {
	case secretErr == nil:
		s.logger.Info("litellm api key also patched into hive-secrets Secret",
			"mount_path", config.DefaultLiteLLMAPIKeyFile)
	case errors.Is(secretErr, errNotInCluster):
		// Docker/LXC hive — Secret patch legitimately unavailable.
	default:
		s.logger.Info("hive-secrets Secret not patched; PVC key file is the only store",
			"reason", secretErr.Error())
	}

	if pvcErr == nil {
		// PVC file written: hive.yaml records that path (the authoritative,
		// instantly-effective store), regardless of the Secret patch outcome.
		return writableLiteLLMKeyFile, nil
	}

	// PVC write failed. If the Secret fallback succeeded, the key IS persisted
	// and surfaces at DefaultLiteLLMAPIKeyFile on Secret-mounting hives — record
	// that path so ResolveAPIKey finds it.
	if secretErr == nil || errors.Is(secretErr, errNotInCluster) {
		if secretErr == nil {
			return config.DefaultLiteLLMAPIKeyFile, nil
		}
		// Not in cluster AND the PVC write failed — nothing persisted the key.
		return "", pvcErr
	}
	// Both stores failed: surface the actionable PVC error (the primary path).
	return "", pvcErr
}

// writeLiteLLMKeyFile writes the key value to the PVC key file with
// owner-only permissions.
//
// The hive process runs as a non-root user (uid 1001, the /data owner) and
// therefore cannot chown. If /data/secrets already exists but is owned by
// root (or is otherwise unwritable), MkdirAll is a no-op and WriteFile fails
// with EACCES. We cannot fix ownership from here, but we CAN surface a clear,
// actionable error instead of a raw "permission denied" — the entrypoint's
// root-only setup is responsible for pre-creating /data/secrets as dev:node,
// and this message points the operator at that path when it hasn't.
func writeLiteLLMKeyFile(key string) error {
	dir := filepath.Dir(writableLiteLLMKeyFile)
	if err := os.MkdirAll(dir, litellmKeyDirMode); err != nil {
		return actionableWriteError(dir, err)
	}
	if err := os.WriteFile(writableLiteLLMKeyFile, []byte(key), litellmKeyFileMode); err != nil {
		return actionableWriteError(writableLiteLLMKeyFile, err)
	}
	// WriteFile does not change the mode of a pre-existing file.
	if err := os.Chmod(writableLiteLLMKeyFile, litellmKeyFileMode); err != nil {
		return actionableWriteError(writableLiteLLMKeyFile, err)
	}
	return nil
}

// actionableWriteError turns a low-level filesystem error into an operator-
// readable message. Permission and read-only-filesystem failures on the PVC
// key path almost always mean the hive's data volume is misowned (the dir is
// root-owned but the hive runs non-root) or mounted read-only — so we say so
// explicitly rather than leaking a bare "permission denied". The key value is
// never part of these errors (only the path is), so they are safe to log.
func actionableWriteError(path string, err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("could not write key to %s — the hive's data volume "+
			"may be read-only or misowned (expected owner uid 1001); "+
			"restart the hive so its entrypoint can repair the secrets directory: %w",
			path, err)
	}
	return fmt.Errorf("writing %s: %w", path, err)
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
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Deliberately status-only: API error bodies can echo patch
		// contents, which would leak the key value into logs/responses.
		return fmt.Errorf("kubernetes API returned %d patching %s (missing hive-secrets-writer Role?)",
			resp.StatusCode, hiveSecretsName)
	}
	return nil
}

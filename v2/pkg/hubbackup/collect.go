package hubbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Timeouts for external kubectl calls. Spoke collection touches ~50 pods
// across two clusters, so a single hung exec must not stall the backup.
const (
	// kubectlTimeout bounds one kubectl invocation.
	kubectlTimeout = 60 * time.Second

	// spokeExecTimeout bounds a single per-spoke file read.
	spokeExecTimeout = 45 * time.Second
)

// Spoke namespace and container naming on the hosting clusters.
const (
	// spokeNamespacePrefix is prepended to a hive ID to form its namespace.
	spokeNamespacePrefix = "hive-hosted-"

	// spokeContainer is the container inside a spoke pod holding /data.
	// Spoke pods also run a copy-config init container, so the container
	// must be named explicitly or kubectl exec is ambiguous.
	spokeContainer = "hive"
)

// Spoke files captured. This set is the minimum required to rebuild a working
// spoke; everything else on a spoke PVC is regenerable agent scratch.
//
// hive.yaml.bak is THE authoritative running config. The spoke's copy-config
// init container does:
//
//	if [ -f /data/hive.yaml.bak ]; then cp /data/hive.yaml.bak /etc/hive/hive.yaml
//	else cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml; fi
//
// so a restore that omits hive.yaml.bak silently falls back to the stale
// ConfigMap seed with NO error, losing every user customization. The plain
// /data/hive.yaml is NOT authoritative and is not what must be restored.
var spokeFiles = []string{
	"hive.yaml.bak",
	"hive-id",
}

// spokeKeyGlob matches the per-spoke GitHub App private keys, whose exact
// filenames vary by cluster (gh-app-key.pem, gh-app-key-<appid>.pem).
const spokeKeyGlob = "gh-app-key*.pem"

// SpokeConfig is one spoke's captured configuration.
type SpokeConfig struct {
	ID      string
	Cluster string
	Files   map[string][]byte
	// Err is non-empty when this spoke could not be captured. The backup
	// continues and records the gap in the manifest.
	Err string
}

// SpokeCollector gathers per-spoke configuration.
type SpokeCollector interface {
	Collect(logger *slog.Logger) ([]SpokeConfig, error)
}

// SecretItem is one Kubernetes Secret serialized for the archive.
type SecretItem struct {
	Name string
	JSON []byte
}

// SecretCollector gathers Kubernetes Secrets that live outside the PVC.
type SecretCollector interface {
	Collect(logger *slog.Logger) ([]SecretItem, error)
}

// ClusterTarget describes how to reach one hosting cluster with kubectl.
type ClusterTarget struct {
	ID             string
	InCluster      bool
	KubeconfigPath string
	Context        string
}

// kubectlArgs prefixes cluster-selection flags, mirroring the hub's existing
// kubectlArgsForCluster helper so remote clusters are addressed identically.
func (c ClusterTarget) kubectlArgs(args ...string) []string {
	out := []string{}
	if !c.InCluster {
		if c.KubeconfigPath != "" {
			out = append(out, "--kubeconfig", c.KubeconfigPath)
		}
		if c.Context != "" {
			out = append(out, "--context", c.Context)
		}
	}
	return append(out, args...)
}

// run executes kubectl against this cluster and returns stdout.
func (c ClusterTarget) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", c.kubectlArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// KubectlSecretCollector captures named Secrets from the hub namespace.
//
// These four Secrets live OUTSIDE the hub PVC. A PVC-only backup restores to a
// hub that cannot authenticate users (oauth-client-secret), cannot provision
// storage (oci-api-key) and cannot reach remote clusters (kubeconfigs) — that
// is why they are collected here and why a failure is fatal.
type KubectlSecretCollector struct {
	Cluster   ClusterTarget
	Namespace string
	Names     []string
}

// DefaultHubSecrets are the Secrets required for a restorable hub.
var DefaultHubSecrets = []string{
	"hive-hub-secrets",     // GitHub OAuth client secret
	"oci-api-key",          // OCI credentials for FSS + Object Storage
	"hive-hub-kubeconfigs", // kubeconfig for remote spoke clusters
	"hive-hub-tls",         // TLS cert (cert-manager can reissue, kept for speed)
}

// Collect fetches each Secret as JSON with server-side noise stripped.
func (k KubectlSecretCollector) Collect(logger *slog.Logger) ([]SecretItem, error) {
	names := k.Names
	if len(names) == 0 {
		names = DefaultHubSecrets
	}
	var out []SecretItem
	for _, name := range names {
		raw, err := k.Cluster.run("get", "secret", name, "-n", k.Namespace, "-o", "json")
		if err != nil {
			// cert-manager can reissue TLS, so a missing TLS secret is not
			// fatal. Any other missing secret breaks restore.
			if name == "hive-hub-tls" {
				logger.Warn("backup: TLS secret missing, cert-manager will reissue", "name", name)
				continue
			}
			return nil, fmt.Errorf("secret %q is required for a restorable backup: %w", name, err)
		}
		cleaned, err := stripSecretNoise(raw)
		if err != nil {
			return nil, fmt.Errorf("clean secret %q: %w", name, err)
		}
		out = append(out, SecretItem{Name: name, JSON: cleaned})
		logger.Info("backup: captured secret", "name", name)
	}
	return out, nil
}

// stripSecretNoise removes cluster-instance metadata so the Secret can be
// re-applied to a brand-new cluster without conflict.
func stripSecretNoise(raw []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		for _, f := range []string{"resourceVersion", "uid", "creationTimestamp",
			"managedFields", "selfLink", "generation"} {
			delete(md, f)
		}
	}
	return json.MarshalIndent(obj, "", "  ")
}

// KubectlSpokeCollector reads each spoke's config out of its running pod.
//
// Spoke state lives on per-spoke PVCs that the hub cannot mount, so the only
// read path is kubectl exec into the spoke pod. A spoke that is scaled to zero
// therefore cannot be captured; that gap is recorded in the manifest.
type KubectlSpokeCollector struct {
	// Clusters maps cluster ID to how kubectl reaches it.
	Clusters map[string]ClusterTarget

	// Hives maps hive ID to the cluster ID hosting it.
	Hives map[string]string
}

// Collect gathers config for every known hive.
func (k KubectlSpokeCollector) Collect(logger *slog.Logger) ([]SpokeConfig, error) {
	ids := make([]string, 0, len(k.Hives))
	for id := range k.Hives {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]SpokeConfig, 0, len(ids))
	for _, id := range ids {
		clusterID := k.Hives[id]
		target, ok := k.Clusters[clusterID]
		if !ok {
			out = append(out, SpokeConfig{ID: id, Cluster: clusterID,
				Err: fmt.Sprintf("unknown cluster %q", clusterID)})
			continue
		}
		sc := k.collectOne(target, id, logger)
		out = append(out, sc)
	}
	return out, nil
}

// collectOne captures a single spoke, tolerating failure.
func (k KubectlSpokeCollector) collectOne(target ClusterTarget, id string, logger *slog.Logger) SpokeConfig {
	sc := SpokeConfig{ID: id, Cluster: target.ID, Files: map[string][]byte{}}
	ns := spokeNamespacePrefix + id

	// Select only Running pods. Namespaces frequently retain Evicted/Failed
	// pods alongside a healthy one, and an unfiltered .items[0] can select a
	// dead pod — which silently loses that hive's config from the backup.
	podRaw, err := target.run("get", "pods", "-n", ns,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || len(bytes.TrimSpace(podRaw)) == 0 {
		sc.Err = fmt.Sprintf("no running pod in %s (scaled to zero?): %v", ns, err)
		logger.Warn("backup: spoke unreachable", "hive", id, "err", sc.Err)
		return sc
	}
	pod := string(bytes.TrimSpace(podRaw))

	// One exec emits every wanted file as a base64 stream with name markers,
	// so a spoke costs a single round trip rather than one per file.
	script := buildSpokeReadScript()
	ctx, cancel := context.WithTimeout(context.Background(), spokeExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl",
		target.kubectlArgs("exec", "-n", ns, "-c", spokeContainer, pod, "--", "sh", "-c", script)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		sc.Err = fmt.Sprintf("exec into %s/%s: %v: %s", ns, pod, err, strings.TrimSpace(stderr.String()))
		logger.Warn("backup: spoke exec failed", "hive", id, "err", sc.Err)
		return sc
	}

	files, err := parseSpokeStream(stdout.Bytes())
	if err != nil {
		sc.Err = fmt.Sprintf("parse spoke stream: %v", err)
		return sc
	}
	sc.Files = files

	// hive.yaml.bak is the authoritative config; without it the spoke cannot
	// be faithfully rebuilt, so surface that as an explicit error.
	if _, ok := sc.Files["hive.yaml.bak"]; !ok {
		sc.Err = "hive.yaml.bak absent — spoke config not recoverable from this backup"
		logger.Warn("backup: spoke missing authoritative config", "hive", id)
		return sc
	}
	logger.Info("backup: captured spoke", "hive", id, "files", len(sc.Files))
	return sc
}

// spokeStreamDelim separates the filename marker from its base64 payload.
const spokeStreamDelim = "::"

// spokeStreamPrefix marks the start of a file record in the exec output.
const spokeStreamPrefix = "@@FILE@@"

// buildSpokeReadScript emits a shell script that base64-encodes each wanted
// file. Encoding avoids binary/newline corruption over the exec stream.
func buildSpokeReadScript() string {
	var sb strings.Builder
	for _, f := range spokeFiles {
		fmt.Fprintf(&sb,
			"if [ -f /data/%s ]; then echo '%s%s%s'; base64 /data/%s; fi; ",
			f, spokeStreamPrefix, f, spokeStreamDelim, f)
	}
	// GitHub App keys use varying filenames across clusters.
	fmt.Fprintf(&sb,
		"for p in /data/%s; do if [ -f \"$p\" ]; then echo \"%s$(basename $p)%s\"; base64 \"$p\"; fi; done",
		spokeKeyGlob, spokeStreamPrefix, spokeStreamDelim)
	return sb.String()
}

// parseSpokeStream decodes the marker-delimited base64 stream.
func parseSpokeStream(data []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	var current string
	var b64 strings.Builder

	flush := func() error {
		if current == "" {
			return nil
		}
		decoded, err := decodeBase64(b64.String())
		if err != nil {
			return fmt.Errorf("decode %s: %w", current, err)
		}
		out[current] = decoded
		b64.Reset()
		return nil
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, spokeStreamPrefix) {
			if err := flush(); err != nil {
				return nil, err
			}
			name := strings.TrimSuffix(strings.TrimPrefix(line, spokeStreamPrefix), spokeStreamDelim)
			current = strings.TrimSpace(name)
			continue
		}
		if current != "" && strings.TrimSpace(line) != "" {
			b64.WriteString(strings.TrimSpace(line))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

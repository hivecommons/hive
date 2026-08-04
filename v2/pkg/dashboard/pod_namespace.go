package dashboard

import (
	"os"
	"strings"
)

// podNamespaceServiceAccountPath is the in-cluster fallback source for the
// pod's namespace, mirroring pkg/hub/self_upgrade.go's k8sNamespacePath. A
// var (not a const) so tests can redirect it to a temp file, same reason.
var podNamespaceServiceAccountPath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// podNamespace returns the Kubernetes namespace this spoke's pod is running
// in, for display in the dashboard's Hub tab (renderGovHub in
// pkg/dashboard/static/index.html) — the spoke-side complement to the
// hive.kubestellar.io/* namespace labels stamped hub-side in
// pkg/hub/hosted_namespace_identity.go.
//
// Preference order:
//  1. POD_NAMESPACE — set via the Kubernetes downward API
//     (fieldRef: metadata.namespace) on the hive Deployment (see the env
//     block in saas_provision.go's k8sManifestTemplate). This is the
//     authoritative, always-correct source once a spoke is provisioned by a
//     build that sets it.
//  2. NAMESPACE — an alternate env var name some deployments (or an operator
//     hand-editing a Deployment) may already use.
//  3. The service-account namespace file, present in every pod regardless of
//     how it was deployed, including spokes provisioned before POD_NAMESPACE
//     existed.
//
// Returns "" when none of the three resolve (e.g. running outside a
// cluster), so the caller can omit the line entirely rather than display an
// empty or misleading value.
func podNamespace() string {
	if v := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NAMESPACE")); v != "" {
		return v
	}
	data, err := os.ReadFile(podNamespaceServiceAccountPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

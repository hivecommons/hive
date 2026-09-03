package hub

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// N5 (CWE-732/522): the spoke manifest must not project GitHub App PEMs
// world-readable.
//
// Kubernetes defaults Secret projections to 0644, so without an explicit
// defaultMode every agent UID (2001+) in the pod could read the org App private
// key — and app_id + installation_id + key_file is enough to mint a full
// installation token (bin/gh-app-token.sh). The entrypoint's compensating
// `chmod 600 /secrets/*.pem` cannot fix it: /secrets is a read-only tmpfs
// projection, so chmod returns EROFS and the surrounding `2>/dev/null || true`
// swallows the error AND the exit code. It fails silently, which is why five
// audits went by with the mode still 0644 in every hosted pod.
//
// These tests render the REAL template. Nothing else in the package does, so a
// malformed template (an unbalanced action, or a stray backtick terminating the
// Go raw-string literal that holds it) would otherwise only surface when
// provisioning a live hive.

// renderManifest executes k8sManifestTemplate with a minimal data set, varying
// only the fields these assertions depend on.
func renderManifest(t *testing.T, requiresSCC bool) string {
	t.Helper()
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template does not parse: %v", err)
	}
	var buf bytes.Buffer
	data := map[string]any{
		"ID":                "test-hive",
		"ImageTag":          "v2-latest",
		"ImagePullPolicy":   "Always",
		"RequiresSCC":       requiresSCC,
		"InitContainerUID":  "1001",
		"InitContainerGID":  "1000",
		"TerminalPort":      7681,
		"DashboardPort":     8080,
		"CPURequest":        "500m",
		"MemRequest":        "1Gi",
		"CPULimit":          "2",
		"MemLimit":          "4Gi",
		"Namespace":         "hive-test",
		"StorageSize":       "10Gi",
		"HeartbeatKey":      "hb",
		"SessionKey":        "sk",
		"SSOPublicKey":      "pk",
		"TerminalKey":       "per-hive-terminal-key",
		"AuthorizedUsers":   "alice:owner",
		"IsNginxIngress":    false,
		"HasInference":      false,
		"StorageClass":      "standard",
		"Host":              "test-hive.hive.kubestellar.io",
		"Owner":             "alice",
		"HubPublicURL":      "https://hive.kubestellar.io",
		"ProjectName":       "test",
		"PrimaryRepo":       "org/repo",
		"AppKeys":           []provisionAppKey{},
		"HasPlaceholderIDs": false,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template does not execute: %v", err)
	}
	return buf.String()
}

// secretsVolumeBlock returns the lines of the `- name: secrets` volume entry,
// so an assertion cannot accidentally match a defaultMode belonging to some
// other volume.
func secretsVolumeBlock(t *testing.T, manifest string) string {
	t.Helper()
	idx := strings.Index(manifest, "- name: secrets")
	if idx < 0 {
		t.Fatal("no `- name: secrets` volume found in the rendered manifest")
	}
	rest := manifest[idx:]
	// The block ends at the next document separator or the next sibling volume.
	if end := strings.Index(rest, "\n---"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestSpokeManifestSecretsNotWorldReadable(t *testing.T) {
	for _, scc := range []bool{false, true} {
		name := "plain"
		if scc {
			name = "openshift-scc"
		}
		t.Run(name, func(t *testing.T) {
			manifest := renderManifest(t, scc)
			block := secretsVolumeBlock(t, manifest)

			if !strings.Contains(block, "defaultMode:") {
				t.Fatalf("secrets volume has no defaultMode — Kubernetes will project the App PEMs 0644, "+
					"readable by every agent UID in the pod (N5).\nblock:\n%s", block)
			}
			// 0440 pairs with fsGroup; 0400 would lock out dev. Anything
			// group/other-writable or other-readable is a regression.
			if !strings.Contains(block, "defaultMode: 0440") {
				t.Errorf("secrets defaultMode is not 0440:\n%s", block)
			}
		})
	}
}

// TestSpokeManifestFsGroupOnlyOffSCC pins the deliberate asymmetry: fsGroup is
// what makes 0440 readable by dev (whose primary group `node` is shared with
// every agent UID, so a node-group mode would defeat the point). It must NOT be
// set on the OpenShift lane, where the restricted SCC allocates fsGroup from
// the namespace's own range and rejects an out-of-range pin — that would fail
// admission and wedge a rollout that cannot tolerate a non-Ready surge pod.
func TestSpokeManifestFsGroupOnlyOffSCC(t *testing.T) {
	plain := renderManifest(t, false)
	if !strings.Contains(plain, "fsGroup: 1002") {
		t.Error("non-SCC spoke is missing `fsGroup: 1002`; the 0440 secrets projection would be unreadable by dev")
	}

	scc := renderManifest(t, true)
	if strings.Contains(scc, "fsGroup:") {
		t.Error("SCC spoke pins an fsGroup; OpenShift's restricted SCC assigns its own and would reject this at admission")
	}
	// The SCC lane keeps its own service account — guard against the
	// if/else refactor having dropped it.
	if !strings.Contains(scc, "serviceAccountName: hive-sa") {
		t.Error("SCC spoke lost `serviceAccountName: hive-sa`")
	}
	if strings.Contains(plain, "serviceAccountName: hive-sa") {
		t.Error("non-SCC spoke should not set the SCC service account")
	}
}

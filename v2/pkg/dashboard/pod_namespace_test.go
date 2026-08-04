package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// pod_namespace.go — podNamespace(), and its wiring into the "hub" config
// GET response (handleGovernorConfigGet) that feeds the dashboard's Hub tab
// (renderGovHub in pkg/dashboard/static/index.html).
// ============================================================

func TestPodNamespacePrefersPodNamespaceEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "hive-hosted-hosted-avail-y99x")
	t.Setenv("NAMESPACE", "should-not-be-used")
	if got := podNamespace(); got != "hive-hosted-hosted-avail-y99x" {
		t.Errorf("podNamespace() = %q, want POD_NAMESPACE value", got)
	}
}

func TestPodNamespaceFallsBackToNamespaceEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("NAMESPACE", "hive-hosted-x")
	if got := podNamespace(); got != "hive-hosted-x" {
		t.Errorf("podNamespace() = %q, want NAMESPACE fallback value", got)
	}
}

func TestPodNamespaceFallsBackToServiceAccountFile(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("hive-hosted-from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := podNamespaceServiceAccountPath
	podNamespaceServiceAccountPath = path
	defer func() { podNamespaceServiceAccountPath = old }()

	if got := podNamespace(); got != "hive-hosted-from-file" {
		t.Errorf("podNamespace() = %q, want trimmed file contents", got)
	}
}

func TestPodNamespaceEmptyWhenNoSourceAvailable(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")
	old := podNamespaceServiceAccountPath
	podNamespaceServiceAccountPath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { podNamespaceServiceAccountPath = old }()

	if got := podNamespace(); got != "" {
		t.Errorf("podNamespace() = %q, want empty when no source resolves", got)
	}
}

// TestHubTabRendersNamespace pins the spoke dashboard's Hub config tab
// (renderGovHub in static/index.html) to include the namespace row — the
// spoke-side complement to the hub dashboard's status-hover namespace line
// and the hive.kubestellar.io/* namespace labels stamped hub-side. Uses the
// spokeIndexHTML helper from gh_app_banner_test.go, matching that file's
// structure-pin style.
func TestHubTabRendersNamespace(t *testing.T) {
	html := spokeIndexHTML(t)

	required := []struct {
		snippet string
		why     string
	}{
		{"const namespaceRow = hub.namespace", "the Hub tab must read hub.namespace from the config payload"},
		{"<label>Namespace ", "the Hub tab must render a labeled Namespace row"},
		{"readonly disabled", "the namespace row must be read-only — no new mutation from the UI"},
	}
	for _, r := range required {
		if !strings.Contains(html, r.snippet) {
			t.Errorf("static/index.html is missing %q — %s", r.snippet, r.why)
		}
	}
}

// TestHandleGovernorConfigGetIncludesNamespace guards that the "hub" section
// of the governor config GET response — the payload renderGovHub() reads in
// the dashboard's Hub tab — carries the resolved namespace, gracefully empty
// when unset rather than absent.
func TestHandleGovernorConfigGetIncludesNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "hive-hosted-hosted-avail-nb01")
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	hub, ok := result["hub"].(map[string]interface{})
	if !ok {
		t.Fatal("expected hub key in governor config response")
	}
	if got, _ := hub["namespace"].(string); got != "hive-hosted-hosted-avail-nb01" {
		t.Errorf("hub.namespace = %q, want %q", got, "hive-hosted-hosted-avail-nb01")
	}
}

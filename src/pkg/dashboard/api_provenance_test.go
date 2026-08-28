package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// writeProvenanceFixture points the handler at temp seed/overlay files and
// restores the globals afterwards.
func writeProvenanceFixture(t *testing.T, seedYAML, overlayYAML string) {
	t.Helper()
	dir := t.TempDir()

	seedPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(seedPath, []byte(seedYAML), 0o644); err != nil {
		t.Fatalf("writing seed: %v", err)
	}
	t.Setenv("HIVE_CONFIG", seedPath)

	prevOverlay := config.DashboardOverlayFile
	t.Cleanup(func() { config.DashboardOverlayFile = prevOverlay })
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	if overlayYAML != "" {
		if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o644); err != nil {
			t.Fatalf("writing overlay: %v", err)
		}
	}
	config.DashboardOverlayFile = overlayPath
}

func getProvenance(t *testing.T, role string) (*httptest.ResponseRecorder, provenanceResponse) {
	t.Helper()
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/config/provenance", nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
		if role == "owner" {
			req.Header.Set(ownerRoleVerifiedHeader, "true")
		}
	}
	rec := httptest.NewRecorder()
	s.handleConfigProvenance(rec, req)

	var body provenanceResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding response: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func fieldByName(fields []config.FieldOrigin, name string) (config.FieldOrigin, bool) {
	for _, f := range fields {
		if f.Field == name {
			return f, true
		}
	}
	return config.FieldOrigin{}, false
}

// TestProvenanceAnswersTheGHEQuestion is the regression this endpoint exists
// for. During the GitHub Enterprise incident an operator patched meta.json,
// clusters.json and the ConfigMap — all inert for github.app_id — before
// finding that the dashboard overlay is the layer that wins. The report must
// name the overlay, and name its path, so the first attempt is the right one.
func TestProvenanceAnswersTheGHEQuestion(t *testing.T) {
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\ngithub:\n  app_id: 1\n  installation_id: 1\n  key_file: /secrets/PLACEHOLDER.pem\n",
		"project:\n  org: acme\nagents:\n  scanner: {}\ngithub:\n  app_id: 5686\n  installation_id: 146551814\n  key_file: /data/gh-app-key.pem\n")

	rec, body := getProvenance(t, "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	f, ok := fieldByName(body.Fields, "github.app_id")
	if !ok {
		t.Fatal("report omits github.app_id")
	}
	if f.Layer != "dashboard-overlay" {
		t.Errorf("github.app_id layer = %q, want dashboard-overlay", f.Layer)
	}
	if !strings.Contains(f.Path, "hive.yaml.dashboard") {
		t.Errorf("github.app_id path = %q, want it to name the overlay file", f.Path)
	}
	if !f.Writable {
		t.Error("github.app_id reported as not writable; the overlay is writable")
	}
	// The seed also sets app_id (with a placeholder) and lost. Saying so is
	// what stops an operator from trusting the ConfigMap's value.
	if len(f.AlsoSetBy) == 0 {
		t.Error("AlsoSetBy is empty; the losing seed value should be disclosed")
	}
}

// TestProvenanceReportsSeedUnwritable pins the single most valuable line in
// the report: in Kubernetes, nothing can write the ConfigMap.
//
// Explicitly forces Kubernetes mode (#4971): this test previously ran
// whatever IsKubernetesPod() detected on the test host — normally not a pod —
// so it was silently exercising the non-Kubernetes branch and would not have
// caught #4971's regression to the wrong environment's answer. See
// TestProvenanceReportsSeedWritableOutsideKubernetes for the other branch.
func TestProvenanceReportsSeedUnwritable(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1") // IsKubernetesPod() → true
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  is_public: true\n",
		"project:\n  org: acme\nagents:\n  scanner: {}\n")

	_, body := getProvenance(t, "owner")

	var seedLayer *layerInfo
	for i := range body.Layers {
		if body.Layers[i].Name == "configmap-seed" {
			seedLayer = &body.Layers[i]
		}
	}
	if seedLayer == nil {
		t.Fatal("layer legend omits configmap-seed")
	}
	if seedLayer.Writable {
		t.Error("configmap-seed reported writable, want false")
	}
	if !strings.Contains(seedLayer.Writer, "nobody") {
		t.Errorf("configmap-seed writer = %q, want it to say nobody", seedLayer.Writer)
	}
	if !strings.Contains(seedLayer.Path, "ConfigMap") {
		t.Errorf("configmap-seed path = %q, want it to name the ConfigMap", seedLayer.Path)
	}
}

// TestProvenanceReportsSeedWritableOutsideKubernetes is the #4971 regression
// test: on a non-Kubernetes hive (Docker/podman/LXC) there is no ConfigMap and
// no RBAC to omit — LayerSeed is RuntimeConfigFile, a plain file the spoke
// rewrites on every save (saveLocked, config.go:5141) and the entrypoint
// restores over the boot config on every restart (entrypoint.sh:267-311). The
// report must say so: writable=true, writer names the spoke, and the path
// names the real file — never the ConfigMap, which does not exist here.
func TestProvenanceReportsSeedWritableOutsideKubernetes(t *testing.T) {
	// No KUBERNETES_SERVICE_HOST set and no serviceaccount token on this host
	// — IsKubernetesPod() reads false, matching a real Docker/podman hive.
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  is_public: true\nacmm_level: 4\n",
		"project:\n  org: acme\nagents:\n  scanner: {}\n")

	_, body := getProvenance(t, "owner")

	var seedLayer *layerInfo
	for i := range body.Layers {
		if body.Layers[i].Name == "configmap-seed" {
			seedLayer = &body.Layers[i]
		}
	}
	if seedLayer == nil {
		t.Fatal("layer legend omits configmap-seed")
	}
	if !seedLayer.Writable {
		t.Error("configmap-seed reported writable=false outside Kubernetes, want true — RuntimeConfigFile is writable by the spoke")
	}
	if strings.Contains(seedLayer.Writer, "nobody") {
		t.Errorf("configmap-seed writer = %q, want it to name the spoke, not nobody (there is no RBAC to omit outside Kubernetes)", seedLayer.Writer)
	}
	if strings.Contains(seedLayer.Path, "ConfigMap") {
		t.Errorf("configmap-seed path = %q, want the real runtime file, not a ConfigMap that does not exist on this hive", seedLayer.Path)
	}
	if !strings.Contains(seedLayer.Path, "hive.yaml.runtime") {
		t.Errorf("configmap-seed path = %q, want it to name RuntimeConfigFile (hive.yaml.runtime)", seedLayer.Path)
	}

	// acmm_level in the fixture is set only in the seed (no overlay override),
	// so it is won by LayerSeed — the field-level report must carry the same
	// writable/writer/path facts as the layer legend above.
	acmm, ok := fieldByName(body.Fields, "acmm_level")
	if !ok {
		t.Fatal("acmm_level missing from fields")
	}
	if acmm.Layer != "configmap-seed" {
		t.Fatalf("acmm_level layer = %q, want configmap-seed (test setup assumption)", acmm.Layer)
	}
	if !acmm.Writable {
		t.Error("acmm_level reported writable=false outside Kubernetes, want true")
	}
	if strings.Contains(acmm.Writer, "nobody") {
		t.Errorf("acmm_level writer = %q, want it to name the spoke, not nobody", acmm.Writer)
	}
}

// TestProvenanceSurfacesHalfAppliedGHEIdentity covers the live regression: a
// GHE app_id/slug delivered WITHOUT api_url, so api_url stayed empty and
// defaulted to api.github.com, producing 404 Integration not found.
func TestProvenanceSurfacesHalfAppliedGHEIdentity(t *testing.T) {
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\n",
		"project:\n  org: acme\nagents:\n  scanner: {}\ngithub:\n  app_id: 5686\n  installation_id: 146551814\n  app_slug: kubestellar-hive-ghe\n")

	_, body := getProvenance(t, "owner")

	if len(body.IdentityIssues) == 0 {
		t.Fatal("no identity issues reported for a GHE slug with an empty api_url")
	}
	joined := strings.Join(body.IdentityIssues, " ")
	if !strings.Contains(joined, "api.github.com") {
		t.Errorf("issue text %q does not explain the 404 cause", joined)
	}
}

// TestProvenanceFlagsRejectedOverlay: a discarded overlay means the hive is
// running on the bare seed, which on the live fleet can be 4–11% of the real
// config. It must never be silent.
func TestProvenanceFlagsRejectedOverlay(t *testing.T) {
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\n",
		"project:\n  org: acme\n") // no agents — fails the guard

	_, body := getProvenance(t, "owner")

	if !body.OverlayRejected {
		t.Fatal("overlay_rejected = false, want true for an overlay with no agents")
	}
	if !strings.Contains(body.OverlayRejectReason, "agents") {
		t.Errorf("reason = %q, want it to name the missing agents", body.OverlayRejectReason)
	}
}

// TestProvenanceAbsentOverlayIsNotAnError: first boot has no overlay. That is
// normal and must not be reported as a rejection, or the signal is noise.
func TestProvenanceAbsentOverlayIsNotAnError(t *testing.T) {
	writeProvenanceFixture(t, "project:\n  org: acme\nagents:\n  scanner: {}\n", "")

	rec, body := getProvenance(t, "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.OverlayRejected {
		t.Error("overlay_rejected = true with no overlay on disk, want false")
	}
	if body.OverlayPresent {
		t.Error("overlay_present = true with no overlay on disk, want false")
	}
}

// TestProvenanceRequiresOwner matches handleConfigDownload's gating: the
// report names config paths and effective values.
func TestProvenanceRequiresOwner(t *testing.T) {
	writeProvenanceFixture(t, "project:\n  org: acme\nagents:\n  scanner: {}\n", "")

	for _, role := range []string{"viewer", "read-write"} {
		rec, _ := getProvenance(t, role)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d for role %q, want 403", rec.Code, role)
		}
	}
}

// TestProvenanceRedactsSecrets: the report is served over the dashboard API
// and must never carry credentials.
func TestProvenanceRedactsSecrets(t *testing.T) {
	writeProvenanceFixture(t,
		"project:\n  org: acme\nagents:\n  scanner: {}\n",
		"project:\n  org: acme\nagents:\n  scanner: {}\ngithub:\n  token: ghp_supersecret\n")

	rec, _ := getProvenance(t, "owner")
	if strings.Contains(rec.Body.String(), "supersecret") {
		t.Fatal("response leaked the github token")
	}
}

// TestProvenanceRejectsEmptyRole is the regression test for the empty-role
// authorization bypass: when X-Hive-Role is absent, the handler must fail
// CLOSED (403) rather than defaulting to "owner". The prior code granted owner
// access to any request that simply omitted the header.
func TestProvenanceRejectsEmptyRole(t *testing.T) {
	writeProvenanceFixture(t, "project:\n  org: acme\nagents:\n  scanner: {}\n", "")

	// getProvenance with "" does NOT set X-Hive-Role — simulating a request
	// from a caller that omits the header entirely.
	rec, _ := getProvenance(t, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty X-Hive-Role: status = %d, want 403 (fail closed); "+
			"before this fix an omitted header defaulted to owner", rec.Code)
	}
}

// A non-owner role must still be rejected, so the fail-closed change did not
// merely move the bypass to a different value.
func TestProvenanceRejectsNonOwnerRole(t *testing.T) {
	writeProvenanceFixture(t, "project:\n  org: acme\nagents:\n  scanner: {}\n", "")
	rec, _ := getProvenance(t, "viewer")
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer role: status = %d, want 403", rec.Code)
	}
}

// A genuine owner must still be served, so failing closed did not lock out the
// legitimate caller.
func TestProvenanceAllowsOwnerRole(t *testing.T) {
	writeProvenanceFixture(t, "project:\n  org: acme\nagents:\n  scanner: {}\n", "")
	rec, _ := getProvenance(t, "owner")
	if rec.Code != http.StatusOK {
		t.Errorf("owner role: status = %d, want 200", rec.Code)
	}
}

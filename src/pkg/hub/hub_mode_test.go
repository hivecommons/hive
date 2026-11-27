package hub

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubSelfHostedParsesMode(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},
		{"hosted", false},
		{"self-hosted", true},
		{" Self-Hosted ", true},
		{"selfhosted", false},
	} {
		t.Setenv(hubModeEnv, tc.val)
		if got := hubSelfHosted(); got != tc.want {
			t.Errorf("HIVE_HUB_MODE=%q: hubSelfHosted()=%v, want %v", tc.val, got, tc.want)
		}
	}
}

// A self-hosted hub with no clusters.json must not invent an OKE cluster
// (#20189), but it still gets an in-cluster entry under the default ID so
// hub operations that look the default cluster up keep working.
func TestDefaultClusterRegistrySelfHostedIsNeutral(t *testing.T) {
	t.Setenv(hubModeEnv, hubModeSelfHosted)
	reg := defaultClusterRegistry()
	c, ok := reg[defaultClusterID]
	if !ok || len(reg) != 1 {
		t.Fatalf("want exactly one %q entry, got %v", defaultClusterID, reg)
	}
	if !c.InCluster {
		t.Errorf("self-hosted default cluster must be in-cluster")
	}
	if c.Name != selfHostedClusterName || c.Arch != "" || c.StorageType != "" || c.IngressType != "" || c.ImageTag != "" {
		t.Errorf("self-hosted default cluster carries hosted-fleet assumptions: %+v", c)
	}
}

func TestDefaultClusterRegistryHostedUnchanged(t *testing.T) {
	t.Setenv(hubModeEnv, "")
	c := defaultClusterRegistry()[defaultClusterID]
	if c.Name != "OKE (default)" || c.Arch != "arm64" || c.ImageTag != "v4-latest" {
		t.Errorf("hosted default cluster changed: %+v", c)
	}
}

func TestLoadClustersCheckedAbsentSelfHosted(t *testing.T) {
	t.Setenv(hubModeEnv, hubModeSelfHosted)
	orig := clustersConfigPath
	clustersConfigPath = filepath.Join(t.TempDir(), "does-not-exist.json")
	t.Cleanup(func() { clustersConfigPath = orig })
	reg, outcome := loadClustersChecked(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if outcome != clustersAbsent {
		t.Fatalf("outcome = %v, want clustersAbsent", outcome)
	}
	if got := reg[defaultClusterID].Name; got != selfHostedClusterName {
		t.Errorf("name = %q, want %q", got, selfHostedClusterName)
	}
}

// The dashboard reads hub_self_hosted from /api/saas/my-hives and hides the
// hosted-fleet controls behind it. Pin that each control is wired to the flag.
func TestDashboardSelfHostedModeHidesHostedControls(t *testing.T) {
	raw, err := os.ReadFile("assets/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		"_hubSelfHosted = data.hub_self_hosted === true",
		"function applySelfHostedMode()",
		"'btn-add-hive'",
		"'btn-request-hive'",
		"'cluster-health-section'",
		"'scale-controls-section'",
		"'nps-section'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html missing %q", want)
		}
	}
}

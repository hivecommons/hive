package hub

import (
	"os"
	"path/filepath"
	"testing"
)

// helperRedirectScaleSettings points the persisted scale-settings document at
// a temp file and resets the in-memory cache, so tests never read the real
// /data/saas/scale_settings.json or each other's saves.
func helperRedirectScaleSettings(t *testing.T) {
	t.Helper()
	orig := scaleSettingsPath
	scaleSettingsPath = filepath.Join(t.TempDir(), "scale_settings.json")
	resetScaleSettingsForTest()
	t.Cleanup(func() {
		scaleSettingsPath = orig
		resetScaleSettingsForTest()
	})
}

// TestScaleSettingsPrecedence verifies the resolution chain for every knob:
// dashboard-saved value > env var > built-in default, and that per-cluster
// overrides beat clusters.json values (including explicit-zero overrides).
func TestScaleSettingsPrecedence(t *testing.T) {
	helperRedirectScaleSettings(t)

	// Built-in defaults with nothing set.
	os.Unsetenv("HIVE_UPGRADE_WAVE_SIZE")
	if got := upgradeWaveSize(); got != defaultUpgradeWaveSize {
		t.Fatalf("default wave size = %d, want %d", got, defaultUpgradeWaveSize)
	}

	// Env beats built-in.
	t.Setenv("HIVE_UPGRADE_WAVE_SIZE", "7")
	t.Setenv("HIVE_PROVISION_PER_CLUSTER", "5")
	if got := upgradeWaveSize(); got != 7 {
		t.Fatalf("env wave size = %d, want 7", got)
	}
	if got := provisionPerClusterCap(); got != 5 {
		t.Fatalf("env per-cluster cap = %d, want 5", got)
	}

	// Dashboard-saved beats env.
	three := 3
	zero := 0
	if err := saveScaleSettings(ScaleSettings{
		UpgradeWaveSize:     20,
		ProvisionPerCluster: 1,
		KubectlPerCluster:   2,
		Clusters: map[string]ClusterScaleOverride{
			"c1": {MaxHives: &three, PoolTarget: &zero},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := upgradeWaveSize(); got != 20 {
		t.Fatalf("saved wave size = %d, want 20", got)
	}
	if got := provisionPerClusterCap(); got != 1 {
		t.Fatalf("saved per-cluster cap = %d, want 1", got)
	}
	if got := kubectlMaxPerCluster(); got != 2 {
		t.Fatalf("saved kubectl cap = %d, want 2", got)
	}

	// Per-cluster override beats clusters.json; explicit 0 disables.
	c1 := &ClusterConfig{ID: "c1", MaxHives: 100, PoolMin: 4, PoolTarget: 8}
	if got := effectiveMaxHives(c1); got != 3 {
		t.Fatalf("effectiveMaxHives = %d, want override 3", got)
	}
	if got := effectivePoolTarget(c1); got != 0 {
		t.Fatalf("effectivePoolTarget = %d, want explicit-zero override", got)
	}
	if got := effectivePoolMin(c1); got != 4 {
		t.Fatalf("effectivePoolMin = %d, want clusters.json 4 (not overridden)", got)
	}

	// Un-overridden cluster falls through to clusters.json.
	c2 := &ClusterConfig{ID: "c2", MaxHives: 50}
	if got := effectiveMaxHives(c2); got != 50 {
		t.Fatalf("effectiveMaxHives(c2) = %d, want 50", got)
	}

	// Persistence survives a cache reset (fresh load from disk).
	resetScaleSettingsForTest()
	if got := upgradeWaveSize(); got != 20 {
		t.Fatalf("reloaded wave size = %d, want 20", got)
	}
}

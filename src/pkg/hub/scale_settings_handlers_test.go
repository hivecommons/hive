package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperScaleServer builds a HubServer with two clusters and redirects both
// the scale-settings document and the SaaS hive records at temp dirs, so the
// admin Scale Controls handlers can be exercised hermetically.
func helperScaleServer(t *testing.T) *HubServer {
	t.Helper()
	helperRedirectScaleSettings(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	return &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"zeta":  {ID: "zeta", MaxHives: 9, PoolMin: 1, PoolTarget: 2},
			"alpha": {ID: "alpha", MaxHives: 4},
		},
	}
}

// TestHandleGetScaleSettings verifies the GET payload: the saved document,
// the effective values after the resolution chain, built-in defaults, and
// per-cluster rows sorted by ID with overrides applied.
func TestHandleGetScaleSettings(t *testing.T) {
	s := helperScaleServer(t)

	two := 2
	if err := saveScaleSettings(ScaleSettings{
		UpgradeWaveSize: 6,
		Clusters:        map[string]ClusterScaleOverride{"alpha": {MaxHives: &two}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleGetScaleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/scale-settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var view scaleSettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Saved.UpgradeWaveSize != 6 {
		t.Fatalf("saved wave = %d, want 6", view.Saved.UpgradeWaveSize)
	}
	if got := view.Effective["upgrade_wave_size"]; got != float64(6) {
		t.Fatalf("effective wave = %v, want 6", got)
	}
	if view.Defaults["upgrade_wave_size"] != defaultUpgradeWaveSize {
		t.Fatalf("defaults wave = %d, want %d", view.Defaults["upgrade_wave_size"], defaultUpgradeWaveSize)
	}
	if len(view.Clusters) != 2 || view.Clusters[0].ID != "alpha" || view.Clusters[1].ID != "zeta" {
		t.Fatalf("cluster rows not sorted by ID: %+v", view.Clusters)
	}
	if view.Clusters[0].MaxHives != 2 {
		t.Fatalf("alpha max_hives = %d, want override 2", view.Clusters[0].MaxHives)
	}
	if view.Clusters[1].MaxHives != 9 || view.Clusters[1].PoolMin != 1 || view.Clusters[1].PoolTarget != 2 {
		t.Fatalf("zeta row should fall through to clusters.json: %+v", view.Clusters[1])
	}
	if view.Clusters[0].Hives != 0 || view.Clusters[0].AvailablePlacehold != 0 {
		t.Fatalf("empty hive store should count zero: %+v", view.Clusters[0])
	}
}

// TestHandleSetScaleSettingsValidation covers every 400 rejection: malformed
// JSON, out-of-range global knobs, unknown cluster overrides, and
// out-of-range cluster overrides. None of them may persist anything.
func TestHandleSetScaleSettingsValidation(t *testing.T) {
	s := helperScaleServer(t)

	bad := 10001
	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed JSON", `{nope`, "invalid JSON"},
		{"negative knob", `{"upgrade_wave_size":-1}`, "0..10000"},
		{"oversized knob", `{"kubectl_per_cluster":10001}`, "0..10000"},
		{"unknown cluster", `{"clusters":{"ghost":{"max_hives":1}}}`, "unknown cluster"},
		{"oversized override", `{"clusters":{"alpha":{"pool_min":` + jsonInt(bad) + `}}}`, "0..10000"},
		{"negative override", `{"clusters":{"alpha":{"pool_target":-2}}}`, "0..10000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/scale-settings", strings.NewReader(tc.body))
			s.handleSetScaleSettings(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q missing %q", rec.Body.String(), tc.want)
			}
		})
	}

	if _, err := os.Stat(scaleSettingsPath); !os.IsNotExist(err) {
		t.Fatalf("rejected input must not persist a settings file (stat err = %v)", err)
	}
	if got := getScaleSettings(); got.UpgradeWaveSize != 0 || len(got.Clusters) != 0 {
		t.Fatalf("rejected input mutated cached settings: %+v", got)
	}
}

// TestHandleSetScaleSettingsPersists verifies the happy path: a valid POST
// is persisted to disk, takes effect live, and echoes the GET view back.
func TestHandleSetScaleSettingsPersists(t *testing.T) {
	s := helperScaleServer(t)

	body := `{"upgrade_wave_size":8,"provision_workers":3,"clusters":{"alpha":{"max_hives":0}}}`
	rec := httptest.NewRecorder()
	s.handleSetScaleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/admin/scale-settings", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var view scaleSettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Saved.UpgradeWaveSize != 8 || view.Saved.ProvisionWorkers != 3 {
		t.Fatalf("echoed saved doc wrong: %+v", view.Saved)
	}
	// Explicit-zero override means "unlimited", not "fall through".
	for _, row := range view.Clusters {
		if row.ID == "alpha" && row.MaxHives != 0 {
			t.Fatalf("alpha max_hives = %d, want explicit-zero override", row.MaxHives)
		}
	}

	// Persisted: survives a cache reset (fresh load from disk).
	resetScaleSettingsForTest()
	if got := upgradeWaveSize(); got != 8 {
		t.Fatalf("reloaded wave size = %d, want 8", got)
	}
	if got := provisionWorkerCount(); got != 3 {
		t.Fatalf("reloaded worker count = %d, want 3", got)
	}

	data, err := os.ReadFile(scaleSettingsPath)
	if err != nil {
		t.Fatalf("settings file not written: %v", err)
	}
	var onDisk ScaleSettings
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("settings file not valid JSON: %v", err)
	}
	if onDisk.Clusters["alpha"].MaxHives == nil || *onDisk.Clusters["alpha"].MaxHives != 0 {
		t.Fatalf("explicit-zero override lost on disk: %+v", onDisk.Clusters["alpha"])
	}
}

// TestHandleSetScaleSettingsPersistFailure forces saveScaleSettings to fail
// (the settings path nests under a regular file, so MkdirAll errors) and
// expects a 500 without poisoning the cached document.
func TestHandleSetScaleSettingsPersistFailure(t *testing.T) {
	s := helperScaleServer(t)

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	orig := scaleSettingsPath
	scaleSettingsPath = filepath.Join(blocker, "nested", "scale_settings.json")
	t.Cleanup(func() {
		scaleSettingsPath = orig
		resetScaleSettingsForTest()
	})

	rec := httptest.NewRecorder()
	s.handleSetScaleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/admin/scale-settings", strings.NewReader(`{"upgrade_wave_size":8}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "failed to persist") {
		t.Fatalf("body %q missing persist error", rec.Body.String())
	}
	if got := getScaleSettings(); got.UpgradeWaveSize != 0 {
		t.Fatalf("failed save mutated cached settings: %+v", got)
	}
}

// TestSortClusterRows pins the insertion sort: by ID ascending, stable for
// already-sorted input, and safe on empty/single-element slices.
func TestSortClusterRows(t *testing.T) {
	rows := []clusterScaleRow{{ID: "c"}, {ID: "a"}, {ID: "b"}}
	sortClusterRows(rows)
	if rows[0].ID != "a" || rows[1].ID != "b" || rows[2].ID != "c" {
		t.Fatalf("not sorted: %+v", rows)
	}
	sortClusterRows(nil)
	one := []clusterScaleRow{{ID: "solo"}}
	sortClusterRows(one)
	if one[0].ID != "solo" {
		t.Fatalf("single element mangled: %+v", one)
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

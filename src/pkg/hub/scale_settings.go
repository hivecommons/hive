package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Admin-editable scale settings.
//
// Every fleet-scale tunable the hub grew (upgrade wave size, provisioning
// queue bounds, kubectl concurrency, per-cluster capacity/pool watermarks)
// is edited from the hub dashboard's admin Scale Controls card and persisted
// here — env vars and clusters.json remain only as INITIAL DEFAULTS, never
// the only way to change a knob. Reads go through the effective* helpers so
// a saved value takes effect live (no hub restart) wherever the machinery
// can honor it.

var scaleSettingsPath = "/data/saas/scale_settings.json"

// ClusterScaleOverride is a per-cluster overlay on clusters.json. Pointers
// distinguish "not overridden" (nil → clusters.json value stands) from an
// explicit 0 ("unlimited" for max_hives, "disabled" for pool_target).
type ClusterScaleOverride struct {
	MaxHives   *int `json:"max_hives,omitempty"`
	PoolMin    *int `json:"pool_min,omitempty"`
	PoolTarget *int `json:"pool_target,omitempty"`
}

// ScaleSettings is the persisted document. Zero values mean "not set" —
// the env-var / built-in default chain applies.
type ScaleSettings struct {
	UpgradeWaveSize     int                             `json:"upgrade_wave_size,omitempty"`
	ProvisionWorkers    int                             `json:"provision_workers,omitempty"`
	ProvisionPerCluster int                             `json:"provision_per_cluster,omitempty"`
	KubectlPerCluster   int                             `json:"kubectl_per_cluster,omitempty"`
	Clusters            map[string]ClusterScaleOverride `json:"clusters,omitempty"`
}

var (
	scaleSettingsMu sync.RWMutex
	scaleSettings   ScaleSettings
	scaleLoaded     bool
)

func loadScaleSettingsLocked() {
	if scaleLoaded {
		return
	}
	scaleLoaded = true
	data, err := os.ReadFile(scaleSettingsPath)
	if err != nil {
		return // no file yet — defaults apply
	}
	var s ScaleSettings
	if json.Unmarshal(data, &s) == nil {
		scaleSettings = s
	}
}

func getScaleSettings() ScaleSettings {
	scaleSettingsMu.Lock()
	loadScaleSettingsLocked()
	s := scaleSettings
	scaleSettingsMu.Unlock()
	return s
}

func saveScaleSettings(s ScaleSettings) error {
	scaleSettingsMu.Lock()
	defer scaleSettingsMu.Unlock()
	loadScaleSettingsLocked()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(scaleSettingsPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(scaleSettingsPath, data, 0o644); err != nil {
		return err
	}
	scaleSettings = s
	return nil
}

// resetScaleSettingsForTest clears the cached document so tests that redirect
// scaleSettingsPath see a fresh load.
func resetScaleSettingsForTest() {
	scaleSettingsMu.Lock()
	scaleSettings = ScaleSettings{}
	scaleLoaded = false
	scaleSettingsMu.Unlock()
}

// settingOrEnv resolves a knob: dashboard-saved value wins, else the env
// var, else the built-in default.
func settingOrEnv(saved int, envName string, def int) int {
	if saved >= 1 {
		return saved
	}
	return envInt(envName, def)
}

func clusterOverrideFor(clusterID string) (ClusterScaleOverride, bool) {
	s := getScaleSettings()
	o, ok := s.Clusters[clusterID]
	return o, ok
}

// effectiveMaxHives resolves a cluster's hive-count ceiling: dashboard
// override first, else the clusters.json value. 0 = unlimited.
func effectiveMaxHives(cluster *ClusterConfig) int {
	if cluster == nil {
		return 0
	}
	if o, ok := clusterOverrideFor(cluster.ID); ok && o.MaxHives != nil {
		return *o.MaxHives
	}
	return cluster.MaxHives
}

// effectivePoolMin / effectivePoolTarget resolve the replenisher watermarks
// the same way. Target 0 = replenishing disabled.
func effectivePoolMin(cluster *ClusterConfig) int {
	if cluster == nil {
		return 0
	}
	if o, ok := clusterOverrideFor(cluster.ID); ok && o.PoolMin != nil {
		return *o.PoolMin
	}
	return cluster.PoolMin
}

func effectivePoolTarget(cluster *ClusterConfig) int {
	if cluster == nil {
		return 0
	}
	if o, ok := clusterOverrideFor(cluster.ID); ok && o.PoolTarget != nil {
		return *o.PoolTarget
	}
	return cluster.PoolTarget
}

// scaleSettingsView is the GET payload: the saved document plus the EFFECTIVE
// values after the default chain, so the card can show what is actually in
// force next to each input.
type scaleSettingsView struct {
	Saved     ScaleSettings     `json:"saved"`
	Effective map[string]any    `json:"effective"`
	Clusters  []clusterScaleRow `json:"clusters"`
	Defaults  map[string]int    `json:"defaults"`
}

type clusterScaleRow struct {
	ID                 string `json:"id"`
	MaxHives           int    `json:"max_hives"`
	PoolMin            int    `json:"pool_min"`
	PoolTarget         int    `json:"pool_target"`
	Hives              int    `json:"hives"`
	AvailablePlacehold int    `json:"available_placeholders"`
}

func (s *HubServer) handleGetScaleSettings(w http.ResponseWriter, r *http.Request) {
	saved := getScaleSettings()
	rows := make([]clusterScaleRow, 0, len(s.clusters))
	for id := range s.clusters {
		c := s.clusters[id]
		rows = append(rows, clusterScaleRow{
			ID:                 id,
			MaxHives:           effectiveMaxHives(&c),
			PoolMin:            effectivePoolMin(&c),
			PoolTarget:         effectivePoolTarget(&c),
			Hives:              clusterHiveCount(id),
			AvailablePlacehold: countCleanAvailable(id),
		})
	}
	sortClusterRows(rows)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(scaleSettingsView{
		Saved: saved,
		Effective: map[string]any{
			"upgrade_wave_size":     upgradeWaveSize(),
			"provision_workers":     provisionWorkerCount(),
			"provision_per_cluster": provisionPerClusterCap(),
			"kubectl_per_cluster":   kubectlMaxPerCluster(),
		},
		Clusters: rows,
		Defaults: map[string]int{
			"upgrade_wave_size":     defaultUpgradeWaveSize,
			"provision_workers":     defaultProvisionWorkers,
			"provision_per_cluster": defaultProvisionPerCluster,
			"kubectl_per_cluster":   defaultKubectlMaxPerCluster,
		},
	})
}

func sortClusterRows(rows []clusterScaleRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].ID < rows[j-1].ID; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// maxScaleSettingValue rejects nonsense input (a wave size of a million is a
// typo, not a plan) while leaving generous operating room.
const maxScaleSettingValue = 10000

func (s *HubServer) handleSetScaleSettings(w http.ResponseWriter, r *http.Request) {
	var in ScaleSettings
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	for _, v := range []int{in.UpgradeWaveSize, in.ProvisionWorkers, in.ProvisionPerCluster, in.KubectlPerCluster} {
		if v < 0 || v > maxScaleSettingValue {
			http.Error(w, `{"error":"values must be 0..10000 (0 = use default)"}`, http.StatusBadRequest)
			return
		}
	}
	for id, o := range in.Clusters {
		if _, ok := s.clusters[id]; !ok {
			http.Error(w, `{"error":"unknown cluster in overrides: `+id+`"}`, http.StatusBadRequest)
			return
		}
		for _, p := range []*int{o.MaxHives, o.PoolMin, o.PoolTarget} {
			if p != nil && (*p < 0 || *p > maxScaleSettingValue) {
				http.Error(w, `{"error":"cluster overrides must be 0..10000"}`, http.StatusBadRequest)
				return
			}
		}
	}
	if err := saveScaleSettings(in); err != nil {
		s.logger.Error("failed to persist scale settings", "error", err)
		http.Error(w, `{"error":"failed to persist settings"}`, http.StatusInternalServerError)
		return
	}
	// Grow the provisioning worker pool live when the bound was raised;
	// shrinking takes effect on the next hub restart (workers are long-lived).
	provisionQueue.ensureWorkers()
	s.logger.Info("scale settings updated",
		"wave", upgradeWaveSize(), "provision_workers", provisionWorkerCount(),
		"provision_per_cluster", provisionPerClusterCap(), "kubectl_per_cluster", kubectlMaxPerCluster())
	s.handleGetScaleSettings(w, r)
}

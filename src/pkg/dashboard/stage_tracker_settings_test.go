package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestStageTrackerToastKnobs pins the default, clamp and reset semantics of
// dashboard.stage_tracker.toast_info_s / toast_error_s (hivecommons/hive#15813).
func TestStageTrackerToastKnobs(t *testing.T) {
	var nilCfg *config.Config
	if nilCfg.StageTrackerToastInfo() != 15*time.Second || nilCfg.StageTrackerToastError() != 30*time.Second {
		t.Fatal("nil config falls back to 15 s info / 30 s error")
	}
	cfg := &config.Config{}
	if cfg.StageTrackerToastInfo() != 15*time.Second || cfg.StageTrackerToastError() != 30*time.Second {
		t.Fatal("zero knobs mean the defaults")
	}
	cfg.Dashboard.StageTracker.ToastInfoS = 45
	cfg.Dashboard.StageTracker.ToastErrorS = 90
	if cfg.StageTrackerToastInfo() != 45*time.Second || cfg.StageTrackerToastError() != 90*time.Second {
		t.Fatal("knobs are read in seconds")
	}
	cfg.Dashboard.StageTracker.ToastInfoS = 1
	cfg.Dashboard.StageTracker.ToastErrorS = 999
	if cfg.StageTrackerToastInfo() != 3*time.Second || cfg.StageTrackerToastError() != 120*time.Second {
		t.Fatalf("out-of-range values clamp to 3..120 s, got %v / %v", cfg.StageTrackerToastInfo(), cfg.StageTrackerToastError())
	}
	s := newTestServer()
	got := s.stageTrackerSettings()
	if got.ToastInfoS != 15 || got.ToastErrorS != 30 || got.SoakMinutes != config.DefaultContributorLaneSoakMinutes {
		t.Fatalf("server without config reports the defaults, got %+v", got)
	}
}

// TestDashboardSettingsAPIStageTrackerToasts proves the toast knobs round
// trip through PUT /api/config/dashboard, land in the dashboard overlay the
// config watcher merges at boot, reject out-of-range values, and surface in
// the stage-tracker payload every viewer polls.
func TestDashboardSettingsAPIStageTrackerToasts(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1") // IsKubernetesPod() → true: the overlay is written
	s := govServer(t)
	dir := t.TempDir()
	s.deps.Config.SourcePath = filepath.Join(dir, "hive.yaml")
	oldOverlay, oldRuntime := config.DashboardOverlayFile, config.RuntimeConfigFile
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	t.Cleanup(func() { config.DashboardOverlayFile, config.RuntimeConfigFile = oldOverlay, oldRuntime })
	s.deps.Config.AutoMerge.ContributorLane.SoakMinutes = 12

	for _, body := range []map[string]any{
		{"stage_tracker": map[string]any{"toast_info_s": 2}},
		{"stage_tracker": map[string]any{"toast_error_s": 121}},
		{"stage_tracker": map[string]any{"toast_info_s": -5}},
	} {
		rec := doPut(s, "/api/config/dashboard", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %#v => %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if st := s.deps.Config.Dashboard.StageTracker; st.ToastInfoS != 0 || st.ToastErrorS != 0 {
		t.Fatalf("rejected PUT mutated knobs: %+v", st)
	}

	rec := doPut(s, "/api/config/dashboard", map[string]any{
		"stage_tracker": map[string]any{"toast_info_s": 20, "toast_error_s": 60},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		StageTracker struct {
			ToastInfoS  int `json:"toast_info_s"`
			ToastErrorS int `json:"toast_error_s"`
		} `json:"stage_tracker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StageTracker.ToastInfoS != 20 || resp.StageTracker.ToastErrorS != 60 {
		t.Fatalf("response did not echo the knobs: %s", rec.Body.String())
	}
	overlay, err := os.ReadFile(config.DashboardOverlayFile)
	if err != nil {
		t.Fatalf("overlay not written: %v", err)
	}
	if !strings.Contains(string(overlay), "toast_info_s: 20") || !strings.Contains(string(overlay), "toast_error_s: 60") {
		t.Fatalf("overlay lacks the toast knobs:\n%s", overlay)
	}

	got := s.stageTrackerSettings()
	if got.ToastInfoS != 20 || got.ToastErrorS != 60 || got.SoakMinutes != 12 {
		t.Fatalf("tracker settings block = %+v", got)
	}

	// A 0 resets a knob to its default without touching the other.
	rec = doPut(s, "/api/config/dashboard", map[string]any{"stage_tracker": map[string]any{"toast_info_s": 0}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got = s.stageTrackerSettings()
	if got.ToastInfoS != 15 || got.ToastErrorS != 60 {
		t.Fatalf("after reset: %+v", got)
	}

	// The memoised tracker payload carries the live block.
	s.deps.Config.SourcePath = ""
	s.statusMu.Lock()
	s.status = minimalPayload()
	s.statusMu.Unlock()
	body, _, _, _ := s.stageTrackerPayload(s.status, time.Now().UTC())
	var tracker struct {
		Settings StageTrackerSettings `json:"settings"`
	}
	if err := json.Unmarshal(body, &tracker); err != nil {
		t.Fatalf("decode tracker: %v", err)
	}
	if tracker.Settings != got {
		t.Fatalf("tracker payload settings = %+v, want %+v", tracker.Settings, got)
	}
}

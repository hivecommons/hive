package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/retro"
)

// The retro pacing knobs (retro.scan_interval_s, max_fix_attempts, max_kicks,
// long_stall_days, recent_closed_window_s) round-trip through the Features
// PUT/GET, negative values are rejected, and an absent key leaves its value
// untouched.
func TestGovernorFeaturesRetroPacingKnobs(t *testing.T) {
	s := covApiServer(t)

	for _, key := range []string{
		"retroScanIntervalS", "retroMaxFixAttempts", "retroMaxKicks",
		"retroLongStallDays", "retroRecentClosedWindowS",
	} {
		if rec := doPut(s, "/api/config/governor/features", map[string]any{key: -1}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s=-1: expected 400, got %d", key, rec.Code)
		}
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"retroScanIntervalS":       1800,
		"retroMaxFixAttempts":      4,
		"retroMaxKicks":            6,
		"retroLongStallDays":       10,
		"retroRecentClosedWindowS": 604800,
	}); rec.Code != http.StatusOK {
		t.Fatalf("retro pacing PUT: %d — %s", rec.Code, rec.Body.String())
	}
	r := s.deps.Config.Retro
	if r.ScanIntervalS != 1800 || r.MaxFixAttempts != 4 || r.MaxKicks != 6 || r.LongStallDays != 10 || r.RecentClosedWindowS != 604800 {
		t.Fatalf("retro pacing not applied: %+v", r)
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroMaxKicks": 0}); rec.Code != http.StatusOK {
		t.Fatalf("retro max_kicks=0 PUT: %d", rec.Code)
	}
	r = s.deps.Config.Retro
	if r.MaxKicks != 0 || r.ScanIntervalS != 1800 || r.LongStallDays != 10 {
		t.Fatalf("partial retro PUT changed unrelated knobs or did not reset max_kicks: %+v", r)
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var payload struct {
		Features struct {
			ScanIntervalS       int            `json:"retroScanIntervalS"`
			MaxFixAttempts      int            `json:"retroMaxFixAttempts"`
			MaxKicks            int            `json:"retroMaxKicks"`
			LongStallDays       int            `json:"retroLongStallDays"`
			RecentClosedWindowS int            `json:"retroRecentClosedWindowS"`
			Defaults            map[string]int `json:"retroDefaults"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	f := payload.Features
	if f.ScanIntervalS != 1800 || f.MaxFixAttempts != 4 || f.MaxKicks != 0 || f.LongStallDays != 10 || f.RecentClosedWindowS != 604800 {
		t.Fatalf("features payload retro knobs = %+v", f)
	}
	if f.Defaults["retroScanIntervalS"] != retro.DefaultScanIntervalS || f.Defaults["retroMaxKicks"] != retro.DefaultMaxKicks {
		t.Fatalf("retroDefaults = %v", f.Defaults)
	}
}

// Every retro.* key is rendered on the Features tab with its YAML-key hint, a
// restart note, and a control wired to the Features dirty map.
func TestRetroSettingsRenderEveryKey(t *testing.T) {
	body := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	if !strings.Contains(body, `data-key="retroEnabled"`) {
		t.Fatal("retro.enabled toggle missing from the Features tab")
	}
	for _, c := range []struct{ yaml, field string }{
		{"retro.analysis_model", ""},
		{"retro.scan_interval_s", "retroScanIntervalS"},
		{"retro.max_fix_attempts", "retroMaxFixAttempts"},
		{"retro.max_kicks", "retroMaxKicks"},
		{"retro.long_stall_days", "retroLongStallDays"},
		{"retro.recent_closed_window_s", "retroRecentClosedWindowS"},
	} {
		if !strings.Contains(body, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if c.field != "" && !strings.Contains(body, `data-arg0="features" data-arg1="`+c.field+`" data-arg-types="s,s,N"`) {
			t.Errorf("%s has no numeric Features control bound to %s", c.yaml, c.field)
		}
	}
	if strings.Contains(body, "are yaml-only and have no control here") {
		t.Error("retro tooltip still claims the pacing knobs are yaml-only")
	}
	if !strings.Contains(body, "every retro change requires a restart") {
		t.Error("retro section must say its changes require a restart")
	}
}

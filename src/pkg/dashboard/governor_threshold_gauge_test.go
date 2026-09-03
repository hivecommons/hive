package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// The dashboard gauge explains the governor's current mode with a set of
// thresholds. Before #3498 it kept its OWN copy of the defaults; once the
// governor started scaling them by repo count, that copy would have described
// the mode with numbers that did not produce it.

func gaugeConfig(repos []string, gov config.GovernorConfig) *config.Config {
	return &config.Config{
		Project:  config.ProjectConfig{Org: "my-org", Repos: repos},
		Governor: gov,
	}
}

func TestBuildGovernor_ThresholdsScaleWithRepoCount(t *testing.T) {
	repos := make([]string, 39)
	for i := range repos {
		repos[i] = "repo"
	}

	got := buildGovernor(governor.State{Mode: governor.ModeBusy}, gaugeConfig(repos, config.GovernorConfig{}))

	if got.Thresholds.Surge != 780 {
		t.Errorf("gauge surge = %d, want 780", got.Thresholds.Surge)
	}
	if got.Thresholds.Busy != 390 {
		t.Errorf("gauge busy = %d, want 390", got.Thresholds.Busy)
	}
	if got.Thresholds.Quiet != 78 {
		t.Errorf("gauge quiet = %d, want 78", got.Thresholds.Quiet)
	}
}

// The property that actually matters: whatever the config, the gauge must show
// exactly the numbers the governor laddered on. This compares the two
// independently-reached answers rather than restating one of them.
func TestBuildGovernor_GaugeAgreesWithGovernor(t *testing.T) {
	cases := []struct {
		name  string
		repos int
		gov   config.GovernorConfig
	}{
		{name: "defaults, single repo", repos: 1},
		{name: "defaults, many repos", repos: 39},
		{
			name:  "hand-tuned",
			repos: 39,
			gov: config.GovernorConfig{Modes: map[string]config.ModeConfig{
				"surge": {Threshold: 300}, "busy": {Threshold: 200}, "quiet": {Threshold: 100},
			}},
		},
		{
			name:  "mixed explicit and scaled",
			repos: 39,
			gov:   config.GovernorConfig{Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}}},
		},
		{name: "scaling disabled", repos: 39, gov: config.GovernorConfig{ThresholdScaling: config.ThresholdScalingNone}},
		{name: "sqrt scaling", repos: 39, gov: config.GovernorConfig{ThresholdScaling: config.ThresholdScalingSqrt}},
		{
			name:  "mode entry with cadences but no threshold",
			repos: 12,
			gov: config.GovernorConfig{Modes: map[string]config.ModeConfig{
				"busy": {Cadences: map[string]config.Cadence{"scanner": "5m"}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := make([]string, tc.repos)
			for i := range repos {
				repos[i] = "repo"
			}
			cfg := gaugeConfig(repos, tc.gov)

			gauge := buildGovernor(governor.State{}, cfg)

			// What the governor itself would use, via the same exported
			// resolver pkg/governor calls.
			wantSurge := cfg.Governor.EffectiveThreshold("surge", tc.repos)
			wantBusy := cfg.Governor.EffectiveThreshold("busy", tc.repos)
			wantQuiet := cfg.Governor.EffectiveThreshold("quiet", tc.repos)

			if gauge.Thresholds.Surge != wantSurge {
				t.Errorf("gauge surge = %d, governor uses %d", gauge.Thresholds.Surge, wantSurge)
			}
			if gauge.Thresholds.Busy != wantBusy {
				t.Errorf("gauge busy = %d, governor uses %d", gauge.Thresholds.Busy, wantBusy)
			}
			if gauge.Thresholds.Quiet != wantQuiet {
				t.Errorf("gauge quiet = %d, governor uses %d", gauge.Thresholds.Quiet, wantQuiet)
			}
		})
	}
}

// A hive with no repos: list still watches its primary repo, so the gauge must
// show the unscaled defaults rather than zeros.
func TestBuildGovernor_NoReposShowsUnscaledDefaults(t *testing.T) {
	got := buildGovernor(governor.State{}, gaugeConfig(nil, config.GovernorConfig{}))

	if got.Thresholds.Surge != 20 || got.Thresholds.Busy != 10 || got.Thresholds.Quiet != 2 {
		t.Errorf("no-repo gauge = %+v, want the unscaled 20/10/2", got.Thresholds)
	}
}

// Editing the repo list from the Repos tab moves every scaled default, so the
// repo count has to reach the governor at save time (saveConfig re-syncs it,
// alongside Governor.UpdateConfig). Nothing covered that end to end: the
// governor tests drive SetRepoCount directly, and the gauge tests never go
// through a handler.
//
// This is asserted through the MODE rather than through a getter, because the
// mode is what the operator actually sees change, and it only moves if the new
// count genuinely reached thresholdFor. A save path that persisted the repos
// but left the governor stale would keep the hive in SURGE — the exact symptom
// #3498 is about — until the next periodic reload happened to correct it.
func TestHandleGovernorRepos_ResyncsGovernorRepoCount(t *testing.T) {
	srv := newFullServer(t)
	gov := srv.deps.Governor

	// Baseline: the fixture watches one repo, so surge is the unscaled 20 and a
	// queue of 60 is a surge.
	gov.Evaluate(60, 0, 0, 0)
	if got := gov.GetState().Mode; got != governor.ModeSurge {
		t.Fatalf("baseline mode = %q, want %q — the fixture should start at one repo", got, governor.ModeSurge)
	}

	body := `{"repos":["testrepo","r2","r3","r4","r5"],"primaryRepo":"testrepo"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Five repos scale surge to 100 and busy to 50, so the SAME queue depth is
	// now BUSY. Still SURGE means the governor never saw the new count.
	gov.Evaluate(60, 0, 0, 0)
	if got := gov.GetState().Mode; got != governor.ModeBusy {
		t.Errorf("mode after adding repos = %q, want %q — the save path did not re-sync the governor's repo count", got, governor.ModeBusy)
	}
}

// The reject path restores the previous repo list and returns before saving, so
// it must leave the repo count alone too. A governor scaled for a repo set that
// was never persisted would ladder on thresholds no config explains.
func TestHandleGovernorRepos_RejectedEditLeavesRepoCountAlone(t *testing.T) {
	srv := newFullServer(t)
	gov := srv.deps.Governor

	// No primaryRepo among the submitted repos — the handler 400s and restores.
	body := `{"repos":["r1","r2","r3","r4","r5"]}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a repo list with no default, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Still the one-repo ladder: a queue of 60 is a surge at surge=20. If the
	// rejected list had reached the governor, surge would be 100 and this would
	// come back BUSY.
	gov.Evaluate(60, 0, 0, 0)
	if got := gov.GetState().Mode; got != governor.ModeSurge {
		t.Errorf("mode after a REJECTED repo edit = %q, want %q — a rejected edit must not move the repo count", got, governor.ModeSurge)
	}
}

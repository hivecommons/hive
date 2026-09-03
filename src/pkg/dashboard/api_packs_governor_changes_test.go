package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestApplyPackForceReportsGovernorChanges(t *testing.T) {
	srv := newFullServer(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv.logger = logger
	srv.deps.Logger = logger

	pack, err := config.ACMMPackByLevel(5)
	if err != nil {
		t.Fatal(err)
	}
	wantInterval := pack.Governor.EvalIntervalS
	wantCadence := pack.Governor.Cadences["surge"]["scanner"]
	if wantInterval == 0 || wantCadence == "" {
		t.Skip("L5 pack no longer has the governor settings this regression covers")
	}

	srv.deps.Config.Governor.EvalIntervalS = 3600
	srv.deps.Config.Governor.Modes = map[string]config.ModeConfig{
		"surge": {Cadences: map[string]config.Cadence{"scanner": "pause"}},
	}

	result, err := srv.ApplyPackForce(5)
	if err != nil {
		t.Fatalf("ApplyPackForce(5): %v", err)
	}
	if result.GovernorChanges == nil || result.GovernorChanges.EvalIntervalS == nil {
		t.Fatal("governor interval change was not reported")
	}
	if got := result.GovernorChanges.EvalIntervalS; got.From != 3600 || got.To != wantInterval {
		t.Errorf("interval change = %+v, want 3600 -> %d", got, wantInterval)
	}

	foundCadence := false
	for _, change := range result.GovernorChanges.Cadences {
		if change.Mode == "surge" && change.Agent == "scanner" {
			foundCadence = true
			if change.From != "pause" || change.To != wantCadence {
				t.Errorf("scanner surge change = %+v, want pause -> %q", change, wantCadence)
			}
		}
	}
	if !foundCadence {
		t.Errorf("scanner surge cadence change missing from %+v", result.GovernorChanges.Cadences)
	}
	if got := logs.String(); !strings.Contains(got, "ACMM pack changed governor settings") || !strings.Contains(got, "governor_eval_interval_s_from=3600") || !strings.Contains(got, "governor_eval_interval_s_to=") || !strings.Contains(got, "cadence_changes") {
		t.Errorf("WARN log does not identify governor changes: %s", got)
	}
}

func TestPackApplyResponseIncludesGovernorChanges(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.EvalIntervalS = 3600

	req := httptest.NewRequest("POST", "/api/packs/5/apply", nil)
	req.SetPathValue("level", "5")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		GovernorChanges *GovernorChanges `json:"governor_changes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.GovernorChanges == nil || response.GovernorChanges.EvalIntervalS == nil {
		t.Fatalf("response omits evaluation interval change: %+v", response.GovernorChanges)
	}
	if got := response.GovernorChanges.EvalIntervalS.From; got != 3600 {
		t.Errorf("response interval from = %d, want 3600", got)
	}
}

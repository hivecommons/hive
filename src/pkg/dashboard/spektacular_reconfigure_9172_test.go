package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #9172: a features save the config volume refused must not report success.
func TestFeaturesSaveFailureReturns500(t *testing.T) {
	s := covApiServer(t)
	// A config path whose parent is a regular file can never be written.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.SourcePath = filepath.Join(blocker, "hive.yaml")

	rec := doPut(s, "/api/config/governor/features", map[string]any{"spektacularEnabled": true})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("features PUT with unwritable config: got %d %s, want 500", rec.Code, rec.Body.String())
	}
}

func featuresApply(t *testing.T, s *Server, body map[string]any) (string, bool) {
	t.Helper()
	rec := doPut(s, "/api/config/governor/features", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("features PUT: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	v, ok := out["spektacularApply"].(string)
	return v, ok
}

// #9172: Spektacular settings saved from the Extensions tab are rewired into
// the running hub, and the response says whether that happened.
func TestFeaturesSpektacularChangeRewiresRunner(t *testing.T) {
	s := covApiServer(t)

	// Nothing wired (e.g. a build without the cmd/hive hook): honest "restart".
	if got, _ := featuresApply(t, s, map[string]any{"spektacularEnabled": true}); got != spektacularApplyRestart {
		t.Fatalf("unwired apply = %q, want %q", got, spektacularApplyRestart)
	}

	calls := 0
	applied := true
	s.SetSpektacularReconfigureFn(func() bool { calls++; return applied })

	if got, _ := featuresApply(t, s, map[string]any{"spektacularPollS": 11}); got != spektacularApplyLive || calls != 1 {
		t.Fatalf("live apply = %q calls=%d, want %q after one rewire", got, calls, spektacularApplyLive)
	}
	// A save that leaves runs config untouched does not rewire anything.
	if got, ok := featuresApply(t, s, map[string]any{"ioscanEnabled": true}); ok || calls != 1 {
		t.Fatalf("unrelated save reported %q / rewired (calls=%d)", got, calls)
	}

	// A busy hub executor defers: the cleanup loop keeps retrying until it
	// applies, and stops once it has.
	applied = false
	if got, _ := featuresApply(t, s, map[string]any{"spektacularHubExecutor": true}); got != spektacularApplyDeferred || !s.SpektacularReconfigurePending() {
		t.Fatalf("deferred apply = %q pending=%v", got, s.SpektacularReconfigurePending())
	}
	s.tickStageRunner(time.Now())
	if calls != 3 || !s.SpektacularReconfigurePending() {
		t.Fatalf("tick did not retry the deferred rewire: calls=%d pending=%v", calls, s.SpektacularReconfigurePending())
	}
	applied = true
	s.tickStageRunner(time.Now())
	s.tickStageRunner(time.Now())
	if calls != 4 || s.SpektacularReconfigurePending() {
		t.Fatalf("retry did not settle: calls=%d pending=%v", calls, s.SpektacularReconfigurePending())
	}
}

func TestClearSpektacularStatus(t *testing.T) {
	s := covApiServer(t)
	s.SetSpektacularStatus(FrontendSpektacular{Present: true, Version: "1.2.3"})
	s.ClearSpektacularStatus()
	if got := s.SpektacularStatus(); got != nil {
		t.Fatalf("status after clear = %+v", got)
	}
}

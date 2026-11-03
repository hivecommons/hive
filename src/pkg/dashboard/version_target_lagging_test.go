package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Right after a channel promotion the hub's cached target can lag the commit
// the spoke already runs. The dashboard offered "Upgrade available → 51320b7"
// to a spoke on 9ab9808 and the hub refused with "not armed — already at
// target". A target the spoke is at or ahead of must not read as behind.
func TestHandleVersionTargetBehindRunningCommitIsNotBehind(t *testing.T) {
	origHash, origShort, origBranch := versionHash, versionShort, versionBranch
	versionHash, versionShort, versionBranch = "9ab9808e97cfbb2d294902454358a907751a3cb0", "9ab9808", "v5"
	t.Cleanup(func() { versionHash, versionShort, versionBranch = origHash, origShort, origBranch })
	orig := upgradeMarkerPath
	upgradeMarkerPath = filepath.Join(t.TempDir(), "no-such-marker")
	t.Cleanup(func() { upgradeMarkerPath = orig })

	ghcrCacheMu.Lock()
	for _, k := range []string{"9ab9808", "51320b7", "ac68e58"} {
		ghcrCacheResult[k] = true
		ghcrCacheExpiry[k] = time.Now().Add(time.Hour)
	}
	ghcrCacheMu.Unlock()
	t.Cleanup(func() {
		ghcrCacheMu.Lock()
		for _, k := range []string{"9ab9808", "51320b7", "ac68e58"} {
			delete(ghcrCacheResult, k)
			delete(ghcrCacheExpiry, k)
		}
		ghcrCacheMu.Unlock()
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/git/ref/heads/v5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":    "refs/heads/v5",
			"object": map[string]any{"sha": "ac68e5825b76696f5a593844888a693ed625e2db"},
		})
	})
	mux.HandleFunc("/repos/hivecommons/hive/commits/ac68e5825b76696f5a593844888a693ed625e2db", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"message": "branch tip"}})
	})
	// The hub target is the running commit's parent: zero commits ahead.
	mux.HandleFunc("/repos/hivecommons/hive/compare/9ab9808...51320b7", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 0, "behind_by": 1, "status": "behind"})
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Config.Hub.AutoUpgrade = true
	deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)
	s.SetHubUpgradePolicy(&spoke.HeartbeatUpgradePolicy{
		HubManaged: true, Schedule: "daily", Branch: "v5", Channel: "candidate",
		TargetSHA: "51320b7", TargetResolved: true,
	})

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["behind"] != false || body["commitsBehind"] != float64(0) {
		t.Fatalf("behind=%v commitsBehind=%v, want false/0 when the spoke is ahead of a lagging target", body["behind"], body["commitsBehind"])
	}
}

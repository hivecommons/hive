package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestBuildAutoUpdateStatusStates pins the classifier's state machine and the
// central #6963 invariant: an unknown or failed comparison is NEVER healthy.
func TestBuildAutoUpdateStatusStates(t *testing.T) {
	behind := func(n int) *int { return &n }

	cases := []struct {
		name        string
		in          autoUpdateInputs
		wantState   string
		wantHealthy bool
	}{
		{
			name:        "disabled",
			in:          autoUpdateInputs{Enabled: false},
			wantState:   autoUpdateStateDisabled,
			wantHealthy: true,
		},
		{
			name:        "up to date",
			in:          autoUpdateInputs{Enabled: true, TargetBranch: "v4", CommitsBehind: behind(0)},
			wantState:   autoUpdateStateUpToDate,
			wantHealthy: true,
		},
		{
			name:        "behind",
			in:          autoUpdateInputs{Enabled: true, TargetBranch: "v4", CommitsBehind: behind(5)},
			wantState:   autoUpdateStateBehind,
			wantHealthy: false,
		},
		{
			// The heart of #6963: a nil comparison must not read as healthy.
			name:        "unknown comparison is not healthy",
			in:          autoUpdateInputs{Enabled: true, TargetBranch: "v4", CommitsBehind: nil},
			wantState:   autoUpdateStateUnknown,
			wantHealthy: false,
		},
		{
			name: "retrying",
			in: autoUpdateInputs{Enabled: true, Marker: map[string]any{
				"target": "abc1234", "attempts": 2, "maxAttempts": 5, "failed": false,
			}},
			wantState:   autoUpdateStateRetrying,
			wantHealthy: false,
		},
		{
			name: "failed carries the reason",
			in: autoUpdateInputs{Enabled: true, Marker: map[string]any{
				"target": "abc1234", "attempts": 5, "maxAttempts": 5, "failed": true,
				"lastError": "patch own deployment: forbidden",
			}},
			wantState:   autoUpdateStateFailed,
			wantHealthy: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAutoUpdateStatus(tc.in)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.Healthy != tc.wantHealthy {
				t.Fatalf("healthy = %v, want %v (state %q)", got.Healthy, tc.wantHealthy, got.State)
			}
			if got.Detail == "" {
				t.Fatalf("detail is empty for state %q — every state must explain itself", got.State)
			}
			if tc.wantState == autoUpdateStateFailed && got.LastError == "" {
				t.Fatalf("failed state dropped the reason: %+v", got)
			}
		})
	}
}

func TestNormalizeAutoUpdatePeriod(t *testing.T) {
	for in, want := range map[string]string{
		"instant": "instant",
		"daily":   "daily",
		"weekly":  "weekly",
		"":        autoUpdatePeriodUnknown,
		"bogus":   autoUpdatePeriodUnknown,
	} {
		if got := normalizeAutoUpdatePeriod(in); got != want {
			t.Errorf("normalizeAutoUpdatePeriod(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHandleVersionAutoUpdateFailedNotHealthy drives the REAL /api/version
// handler with a terminal upgrade marker on the PVC and asserts the emitted
// autoUpdate object is state=failed / healthy=false and carries the reason.
// This is the load-bearing #6963 assertion: it goes through the same handler
// the dashboard fetches, not the classifier in isolation.
func TestHandleVersionAutoUpdateFailedNotHealthy(t *testing.T) {
	origHash, origShort := versionHash, versionShort
	versionHash, versionShort = "old11112222", "old1111"
	t.Cleanup(func() { versionHash, versionShort = origHash, origShort })

	orig := upgradeMarkerPath
	upgradeMarkerPath = filepath.Join(t.TempDir(), "upgrade-requested")
	t.Cleanup(func() { upgradeMarkerPath = orig })
	if err := os.WriteFile(upgradeMarkerPath,
		[]byte(`{"target_sha":"abc1234","current_sha":"old1111","attempts":5,"last_error":"patch own deployment: forbidden"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	au := versionAutoUpdate(t, true, "daily")
	if au["state"] != autoUpdateStateFailed {
		t.Fatalf("state = %v, want failed", au["state"])
	}
	if au["healthy"] != false {
		t.Fatalf("healthy = %v, want false — a failed auto-update must never render healthy (#6963)", au["healthy"])
	}
	if au["lastError"] != "patch own deployment: forbidden" {
		t.Fatalf("lastError = %v, want the failure reason surfaced (#6963)", au["lastError"])
	}
	if au["period"] != "daily" {
		t.Fatalf("period = %v, want daily", au["period"])
	}
}

// TestHandleVersionAutoUpdateUnknownNotHealthy drives the REAL handler with an
// enabled hive whose stable tip has no built image, so commitsBehind is absent
// (unknown). The autoUpdate object must be state=unknown / healthy=false — an
// unknown comparison rendered as healthy is exactly #6963.
func TestHandleVersionAutoUpdateUnknownNotHealthy(t *testing.T) {
	origHash, origShort := versionHash, versionShort
	versionHash, versionShort = "old11112222", "old1111"
	t.Cleanup(func() { versionHash, versionShort = origHash, origShort })
	// No marker present.
	orig := upgradeMarkerPath
	upgradeMarkerPath = filepath.Join(t.TempDir(), "no-such-marker")
	t.Cleanup(func() { upgradeMarkerPath = orig })
	// Tip image does NOT exist → no compare → commitsBehind absent → unknown.
	ghcrCacheMu.Lock()
	ghcrCacheResult["abc1234"] = false
	ghcrCacheExpiry["abc1234"] = time.Now().Add(time.Hour)
	ghcrCacheMu.Unlock()
	t.Cleanup(func() {
		ghcrCacheMu.Lock()
		delete(ghcrCacheResult, "abc1234")
		delete(ghcrCacheExpiry, "abc1234")
		ghcrCacheMu.Unlock()
	})

	au := versionAutoUpdate(t, true, "daily")
	if au["state"] != autoUpdateStateUnknown {
		t.Fatalf("state = %v, want unknown", au["state"])
	}
	if au["healthy"] != false {
		t.Fatalf("healthy = %v, want false — unknown must never render healthy (#6963)", au["healthy"])
	}
}

// versionAutoUpdate builds a Server with the given auto-upgrade config, hits
// the real /api/version route, and returns the autoUpdate sub-object.
func versionAutoUpdate(t *testing.T, enabled bool, mode string) map[string]any {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/git/ref/heads/v4", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":    "refs/heads/v4",
			"object": map[string]any{"sha": "abc1234567890abcdef1234567890abcdef123456"},
		})
	})
	mux.HandleFunc("/repos/hivecommons/hive/commits/abc1234567890abcdef1234567890abcdef123456", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"message": "tip"}})
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Config.Hub.AutoUpgrade = enabled
	deps.Config.Hub.AutoUpgradeMode = mode
	deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	au, ok := body["autoUpdate"].(map[string]any)
	if !ok {
		t.Fatalf("response missing autoUpdate object: %s", rec.Body.String())
	}
	return au
}

// TestIndexHTMLHasAutoUpdateSection pins the findable dashboard section (#6962):
// the governor Settings overlay Hub tab must render an Auto-update section and
// its live-status wiring.
func TestIndexHTMLHasAutoUpdateSection(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="hub-autoupdate-status"`,
		"function fetchAutoUpdateStatus()",
		"fetchAutoUpdateStatus();",
		"autoUpdatePeriodLabel(hub.auto_upgrade_mode)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing %q — the Auto-update section the reporter searched for is gone (#6962)", snippet)
		}
	}
}

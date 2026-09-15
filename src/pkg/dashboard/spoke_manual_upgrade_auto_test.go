package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSpokeAutoUpgradeStillExposesManualUpgradeAction(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)

	start := strings.Index(html, "if (v.autoUpgrade && !um) {")
	if start < 0 {
		t.Fatal("spoke version renderer no longer has the auto-upgrade queued branch")
	}
	end := strings.Index(html[start:], "} else {")
	if end < 0 {
		t.Fatal("could not isolate the spoke auto-upgrade queued branch")
	}
	branch := html[start : start+end]

	for _, want := range []string{
		"Queued for auto-upgrade</span>",
		`<button id="spoke-upgrade-btn" data-action="gh27"`,
		`data-arg0="${escapeHtml(v.latestHash || '')}"`,
		"Upgrade now</button>",
		"gh27: function (event, A) { selfUpgrade(A[0]); }",
		"fetch('/api/self-upgrade', {",
		"body: JSON.stringify({ target: targetHash || '' })",
	} {
		haystack := branch
		if strings.HasPrefix(want, "gh27:") || strings.HasPrefix(want, "fetch(") || strings.HasPrefix(want, "body:") {
			haystack = html
		}
		if !strings.Contains(haystack, want) {
			t.Errorf("spoke auto-upgrade queued state is missing %q; owners must still be able to upgrade now", want)
		}
	}
}

func TestSelfUpgradeWithAutoUpgradeEnabledRelaysToHubUpgradePath(t *testing.T) {
	const hiveID = "hosted-test-hive"

	var calls atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("hub method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/saas/hives/"+hiveID+"/upgrade" {
			t.Errorf("hub path = %q, want /api/saas/hives/%s/upgrade", r.URL.Path, hiveID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"upgrading"}`))
	}))
	defer hub.Close()

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config: &config.Config{
			Project:    config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
			Agents:     map[string]config.AgentConfig{},
			Hub:        config.HubConfig{URL: hub.URL, AutoUpgrade: true},
			HiveID:     hiveID,
			Dashboard:  config.DashboardConfig{AuthToken: "spoke-token"},
			Deployment: config.DeploymentConfig{Runtime: "kubernetes"},
		},
		Logger: slog.Default(),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hub upgrade calls = %d, want 1", got)
	}
}

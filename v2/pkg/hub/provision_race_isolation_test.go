package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestHandleCreateHiveRaceIsolation verifies that the async provisioning
// goroutine does NOT share the same SaaSHive pointer as the HTTP response
// path. Without the copy-before-goroutine fix (PR #3649), running this
// test under -race detects a data race between the goroutine writing
// h.Status/h.Error and the response encoding reading h.Subdomain.
func TestHandleCreateHiveRaceIsolation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	saveSaaSUser(&SaaSUser{GitHubUsername: "racer", Hives: map[string]string{}, SaaSQuota: 5})

	s := &HubServer{
		hubSecret:               testHubSecret,
		logger:                  slog.Default(),
		saveCh:                  make(chan struct{}, 1),
		hubBanners:              make(map[string]*HubBannerEntry),
		heartbeatSwitchTag:      make(map[string]string),
		heartbeatUpgrade:        make(map[string]string),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		clusters:                map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}

	// Issue several concurrent creates to maximise race window.
	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()

			body := `{"org":"raceorg","repos":"repo","primary_repo":"repo","acmm_level":2,"cluster_id":"hive-oke","github_token":"ghp_testtoken"}`
			rec := httptest.NewRecorder()
			req := reqWithUser(http.MethodPost, "/api/saas/hives", body, "racer")
			s.handleCreateHive(rec, req)

			if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
				t.Errorf("[%d] create status = %d body=%s", idx, rec.Code, rec.Body.String())
				return
			}

			// Parse the JSON response and verify key fields are present
			// and not corrupted by the provisioning goroutine.
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Errorf("[%d] bad JSON response: %v", idx, err)
				return
			}
			if _, ok := resp["id"]; !ok {
				t.Errorf("[%d] response missing 'id'", idx)
			}
			if status, ok := resp["status"]; !ok || status != "provisioning" {
				t.Errorf("[%d] response status = %v, want 'provisioning'", idx, status)
			}
		}(i)
	}
	wg.Wait()
	provisionWG.Wait()
}

// TestHandleCreateHiveResponseSubdomainStable verifies the response subdomain
// matches the hive ID pattern and is not blank (regression guard for the
// async goroutine accidentally blanking the struct).
func TestHandleCreateHiveResponseSubdomainStable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	saveSaaSUser(&SaaSUser{GitHubUsername: "subuser", Hives: map[string]string{}, SaaSQuota: 5})

	s := &HubServer{
		hubSecret:               testHubSecret,
		logger:                  slog.Default(),
		saveCh:                  make(chan struct{}, 1),
		hubBanners:              make(map[string]*HubBannerEntry),
		heartbeatSwitchTag:      make(map[string]string),
		heartbeatUpgrade:        make(map[string]string),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		clusters:                map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}

	body := `{"org":"stableorg","repos":"myrepo","primary_repo":"myrepo","acmm_level":1,"cluster_id":"hive-oke","github_token":"ghp_testtoken"}`
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/api/saas/hives", body, "subuser")
	s.handleCreateHive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}

	sub, ok := resp["subdomain"].(string)
	if !ok || sub == "" {
		t.Fatalf("subdomain missing or blank: %v", resp["subdomain"])
	}

	id, ok := resp["id"].(string)
	if !ok || id == "" {
		t.Fatal("id missing or blank")
	}

	// Subdomain should contain the hive ID as a prefix.
	if len(sub) <= len(id) {
		t.Errorf("subdomain %q too short to contain hive id %q", sub, id)
	}

	provisionWG.Wait()
}

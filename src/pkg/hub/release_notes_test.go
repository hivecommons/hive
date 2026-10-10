package hub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func resetHubReleaseNotesForTest() {
	hubReleaseNotesCache.Lock()
	hubReleaseNotesCache.entries = map[string]hubReleaseNotesCacheEntry{}
	hubReleaseNotesCache.inFlight = map[string]*hubReleaseNotesInFlight{}
	hubReleaseNotesCache.Unlock()
	hubReleaseNotesBuilds.Lock()
	hubReleaseNotesBuilds.inFlight = 0
	hubReleaseNotesBuilds.windowStart = hubReleaseNotesNow().Add(-2 * hubReleaseNotesBuildWindow)
	hubReleaseNotesBuilds.windowCount = 0
	hubReleaseNotesBuilds.Unlock()
}

func TestHubReleaseNotesBuildsOnceAndCaches(t *testing.T) {
	resetHubReleaseNotesForTest()
	t.Cleanup(resetHubReleaseNotesForTest)
	const from = "aaaaaaa1111111"
	const to = "bbbbbbb2222222"
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/repos/hivecommons/hive/contents/CHANGELOG.md" {
			content := "## 2026-10-07 (v5.144.0)\n### Added\n- old\n"
			if r.URL.Query().Get("ref") == to {
				content = "## 2026-10-08 (v5.145.0)\n### Added\n- new\n\n" + content
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
			return
		}
		if r.URL.Path == "/repos/hivecommons/hive/contents/changelog.d" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	oldAPI := githubAPIBase
	githubAPIBase = api.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	s := newHubServerForTest(t)
	oldHives := saasHivesDir
	saasHivesDir = filepath.Join(t.TempDir(), "hives")
	t.Cleanup(func() { saasHivesDir = oldHives })
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-a", Owner: "alice", DashboardTokenHash: HashDashboardToken("proof")}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/saas/release-notes?from="+from+"&to="+to+"&hive_id=hosted-a", nil)
	req.Header.Set(proxyAuthHeader, "proof")
	rec := httptest.NewRecorder()
	s.handleReleaseNotes(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out hubReleaseNotesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Source != "changelog" || len(out.Sections) != 1 || out.Sections[0].Version != "v5.145.0" {
		t.Fatalf("unexpected response %+v", out)
	}
	firstCalls := calls
	if firstCalls == 0 {
		t.Fatal("GitHub API was not read")
	}

	rec = httptest.NewRecorder()
	s.handleReleaseNotes(rec, req)
	if rec.Code != http.StatusOK || calls != firstCalls {
		t.Fatalf("cache miss: status=%d calls %d -> %d body=%s", rec.Code, firstCalls, calls, rec.Body.String())
	}
}

func TestHubReleaseNotesRejectsBadSpokeProof(t *testing.T) {
	resetHubReleaseNotesForTest()
	s := newHubServerForTest(t)
	oldHives := saasHivesDir
	saasHivesDir = filepath.Join(t.TempDir(), "hives")
	t.Cleanup(func() { saasHivesDir = oldHives })
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-a", Owner: "alice", DashboardTokenHash: HashDashboardToken("proof")}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/saas/release-notes?from=aaaaaaa1111111&to=bbbbbbb2222222&hive_id=hosted-a", nil)
	req.Header.Set(proxyAuthHeader, "wrong")
	rec := httptest.NewRecorder()
	s.handleReleaseNotes(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "proof") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

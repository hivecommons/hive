package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hubVersionPayload is the subset of /api/hub/version this test asserts on.
type hubVersionPayload struct {
	GitHash       string            `json:"git_hash"`
	LatestSHAs    map[string]string `json:"latest_shas"`
	LatestHubSHAs map[string]string `json:"latest_hub_shas"`
	HeadSHAs      map[string]string `json:"head_shas"`
	ImageStatuses map[string]string `json:"image_statuses"`
	UpgradeState  string            `json:"upgrade_state"`
}

func getHubVersion(t *testing.T, srv *HubServer) hubVersionPayload {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/hub/version", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got hubVersionPayload
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode version payload: %v (body=%s)", err, w.Body.String())
	}
	return got
}

// seedSHACaches populates the three branch caches for branch "v2".
func seedSHACaches(spokeSHA, hubSHA, headSHA, headStatus string) {
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	if spokeSHA != "" {
		latestSHAByBranch["v2"] = branchSHAInfo{SHA: spokeSHA}
	}
	if hubSHA != "" {
		latestHubSHAByBranch["v2"] = branchSHAInfo{SHA: hubSHA}
	}
	if headSHA != "" {
		headSHAByBranch["v2"] = branchHeadInfo{SHA: headSHA, ImageStatus: headStatus}
	}
}

// TestHubVersionDistinguishesBuildingFromWedgedPoller is the core of this
// change. Before it, "latest_shas.v2 is behind the branch HEAD" was a bare
// string with no way to tell a healthy mid-build window from a broken poller —
// an ambiguity that produced a real false alarm. Each case below is one
// operator-facing diagnosis; they must not collapse into the same payload.
func TestHubVersionDistinguishesBuildingFromWedgedPoller(t *testing.T) {
	cases := []struct {
		name       string
		spoke      string
		hub        string
		head       string
		status     string
		wantStatus string
		// wantBehind is whether the advertised spoke SHA trails HEAD.
		wantBehind bool
	}{
		{
			// The real 2026-07-31 race: #2375 (c6ec89d) merged 5 minutes after
			// f2b4719, and its spoke image was still building. The hub
			// correctly advertised the older SHA — nothing was wrong.
			name:  "spoke image still building is transient and healthy",
			spoke: "f2b4719", hub: "c6ec89d", head: "c6ec89d",
			status: imageStatusBuilding, wantStatus: imageStatusBuilding, wantBehind: true,
		},
		{
			// Same shape, but the image EXISTS and the advertised SHA still
			// did not advance. This is the genuine fault the old payload
			// could not express.
			name:  "ready image with a stale advertised SHA is a poller fault",
			spoke: "f2b4719", hub: "c6ec89d", head: "c6ec89d",
			status: imageStatusReady, wantStatus: imageStatusReady, wantBehind: true,
		},
		{
			name:  "failed build is distinct from building",
			spoke: "f2b4719", hub: "c6ec89d", head: "c6ec89d",
			status: imageStatusFailed, wantStatus: imageStatusFailed, wantBehind: true,
		},
		{
			name:  "fully rolled out",
			spoke: "c6ec89d", hub: "c6ec89d", head: "c6ec89d",
			status: imageStatusReady, wantStatus: imageStatusReady, wantBehind: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetSHACaches(t)
			defer resetSHACaches(t)
			seedSHACaches(tc.spoke, tc.hub, tc.head, tc.status)

			srv := NewHubServer(0, slog.Default(), tc.hub, "v2")
			got := getHubVersion(t, srv)

			if got.ImageStatuses["v2"] != tc.wantStatus {
				t.Errorf("image_statuses[v2] = %q, want %q", got.ImageStatuses["v2"], tc.wantStatus)
			}
			if got.HeadSHAs["v2"] != tc.head {
				t.Errorf("head_shas[v2] = %q, want %q", got.HeadSHAs["v2"], tc.head)
			}
			if got.LatestSHAs["v2"] != tc.spoke {
				t.Errorf("latest_shas[v2] = %q, want %q", got.LatestSHAs["v2"], tc.spoke)
			}
			if got.LatestHubSHAs["v2"] != tc.hub {
				t.Errorf("latest_hub_shas[v2] = %q, want %q", got.LatestHubSHAs["v2"], tc.hub)
			}
			behind := got.LatestSHAs["v2"] != got.HeadSHAs["v2"]
			if behind != tc.wantBehind {
				t.Errorf("advertised-behind-HEAD = %v, want %v", behind, tc.wantBehind)
			}
		})
	}
}

// TestHubVersionUpgradeStateUsesHubCacheNotSpokeCache pins the property that
// made the original "self-confirming bug" hypothesis wrong: upgrade_state is
// computed from the HUB image cache, so "current" alongside an older
// latest_shas is legitimate, not a contradiction. If someone ever repoints
// hubUpgradeState() at latestSHAByBranch, this fails.
func TestHubVersionUpgradeStateUsesHubCacheNotSpokeCache(t *testing.T) {
	resetSHACaches(t)
	defer resetSHACaches(t)
	// Hub image is at c6ec89d (what the hub runs); spoke image lags at f2b4719.
	seedSHACaches("f2b4719", "c6ec89d", "c6ec89d", imageStatusBuilding)

	srv := NewHubServer(0, slog.Default(), "c6ec89d", "v2")
	got := getHubVersion(t, srv)

	if got.UpgradeState != "current" {
		t.Errorf("upgrade_state = %q, want \"current\" — it must track the hub "+
			"image cache, not the lagging spoke cache", got.UpgradeState)
	}
	if got.LatestSHAs["v2"] == got.LatestHubSHAs["v2"] {
		t.Fatal("test setup is not exercising divergent hub/spoke caches")
	}
}

// TestHubVersionEmptyHeadSHAsSignalsPollerNotRunning covers the startup /
// wedged-poller case: no head fetch has ever succeeded, so head_shas is empty
// rather than silently mirroring latest_shas.
func TestHubVersionEmptyHeadSHAsSignalsPollerNotRunning(t *testing.T) {
	resetSHACaches(t)
	defer resetSHACaches(t)
	// Only the persisted spoke cache is warm (as after loadPersistedSHAs).
	seedSHACaches("f2b4719", "", "", "")

	srv := NewHubServer(0, slog.Default(), "f2b4719", "v2")
	got := getHubVersion(t, srv)

	if _, ok := got.HeadSHAs["v2"]; ok {
		t.Errorf("head_shas[v2] should be absent when no head fetch has "+
			"succeeded, got %q", got.HeadSHAs["v2"])
	}
	if got.LatestSHAs["v2"] != "f2b4719" {
		t.Errorf("latest_shas[v2] = %q, want restored value", got.LatestSHAs["v2"])
	}
	// Branches known only from the image-verified cache are ready by definition.
	if got.ImageStatuses["v2"] != imageStatusReady {
		t.Errorf("image_statuses[v2] = %q, want %q", got.ImageStatuses["v2"], imageStatusReady)
	}
}

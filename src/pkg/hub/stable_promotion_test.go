package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedStablePromotionChannels(t *testing.T) {
	t.Helper()
	resetStablePromotionCaches()
	t.Cleanup(resetStablePromotionCaches)
	stubChannelDigests(t, map[string]string{
		"v5-latest": "sha256:v5-latest",
		"stable":    "sha256:stable",
		"candidate": "sha256:candidate",
		"edge":      "sha256:edge",
	})
	stubChannelRevisions(t, map[string]string{
		"stable":    "0ba47d0",
		"candidate": "d5a638e",
		"edge":      "eeeeeee",
	})
	now := time.Now().UTC()
	stubChannelCommitDates(t, map[string]time.Time{
		"0ba47d0": now.Add(-48 * time.Hour),
		"d5a638e": now.Add(-2 * time.Hour),
		"eeeeeee": now,
	})
	origGen := ghcrTagGeneration
	ghcrTagGeneration = func(_ string, tag string, _ *slog.Logger) int {
		switch tag {
		case "stable":
			return 100
		case "candidate":
			return 200
		case "d5a638e":
			return 200
		default:
			return 0
		}
	}
	origRuns := stablePromotionFetchRuns
	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun {
		return []stablePromotionWorkflowRun{{
			RunNumber: 200,
			HeadSHA:   "d5a638e",
			UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
		}}
	}
	t.Cleanup(func() {
		ghcrTagGeneration = origGen
		stablePromotionFetchRuns = origRuns
	})
}

func TestStablePromotionDefaultTruePublicGETAndMaintainedSummary(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	seedStablePromotionChannels(t)
	s := newHubServerForTest(t, withHubIdentity("hubsha", "v5"))
	s.registry.Hives = []RegistryEntry{{
		ID:            "candidate-hive",
		Online:        true,
		ImageRef:      "ghcr.io/hivecommons/hive:candidate",
		GitHash:       "d5a638e",
		LastHeartbeat: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		Agents:        []AgentSummary{{Name: "scanner"}},
	}}
	if err := saveSaaSHive(&SaaSHive{ID: "candidate-hive", Owner: "alice", Status: statusAssigned, TrackedChannel: ReleaseChannelCandidate}); err != nil {
		t.Fatalf("save saas hive: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleGetStablePromotion(rec, httptest.NewRequest(http.MethodGet, "/api/hub/release/stable-promotion", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got StablePromotionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.AutoPromote || got.SoakHours != stablePromotionSoakHours {
		t.Fatalf("default state = %+v, want auto_promote true and 24h soak", got)
	}
	if got.PausedAt != nil || got.PausedBy != "" {
		t.Fatalf("unpaused response carried pause attribution: %+v", got)
	}
	if got.Candidate.SHA != "d5a638e" || got.Candidate.Generation != 200 || got.Stable.SHA != "0ba47d0" || got.Stable.Generation != 100 || got.EligibleAt == nil || got.EligibleBuild == nil {
		t.Fatalf("channel build summary missing: %+v", got)
	}
	if got.EligibleBuild.SHA != "d5a638e" || got.EligibleBuild.Generation != 200 {
		t.Fatalf("eligible build = %+v, want next build crossing the 24-hour line at generation 200", got.EligibleBuild)
	}
	if len(got.MaintainedHives) != 1 || !got.MaintainedHives[0].Healthy || got.MaintainedHives[0].ID != "candidate-hive" {
		t.Fatalf("maintained candidate summary = %+v, want one healthy candidate hive", got.MaintainedHives)
	}
}

func TestStablePromotionPUTAdminGatedAuditedAndPersists(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	seedStablePromotionChannels(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	s := newHubServerForTest(t, withHubIdentity("hubsha", "v5"), withHubLogger(logger))
	mkUser(t, "alice")
	mkUser(t, hubAdminUsername)
	handler := s.requireAdmin(s.handleSetStablePromotion)

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPut, "/api/hub/release/stable-promotion", `{"auto_promote":false}`, "alice")
	req.Header.Set("Origin", "https://hive.hivecommons.dev")
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPut, "/api/hub/release/stable-promotion", `{"auto_promote":false}`, hubAdminUsername)
	req.Header.Set("Origin", "https://hive.hivecommons.dev")
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "audit: stable auto-promotion toggled") || !strings.Contains(logs.String(), hubAdminUsername) {
		t.Fatalf("audit log missing admin toggle: %s", logs.String())
	}
	st := loadStablePromotionState()
	if st.AutoPromote || !strings.EqualFold(st.UpdatedBy, hubAdminUsername) || st.UpdatedAt == "" {
		t.Fatalf("persisted pause state = %+v", st)
	}

	rec = httptest.NewRecorder()
	s.handleGetStablePromotion(rec, httptest.NewRequest(http.MethodGet, "/api/hub/release/stable-promotion", nil))
	var got StablePromotionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode second GET: %v", err)
	}
	if got.AutoPromote || got.PausedBy == "" || got.PausedAt == nil {
		t.Fatalf("GET did not round-trip paused state: %+v", got)
	}
}

func TestStablePromotionEligibleBuildChoosesNewestSoakedSupersededBuild(t *testing.T) {
	resetStablePromotionCaches()
	t.Cleanup(resetStablePromotionCaches)
	now := time.Date(2033, 5, 18, 3, 30, 0, 0, time.UTC)
	origRuns := stablePromotionFetchRuns
	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun {
		return []stablePromotionWorkflowRun{
			{RunNumber: 300, HeadSHA: "new123456789", UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339)},
			{RunNumber: 200, HeadSHA: "old123456789", UpdatedAt: now.Add(-25 * time.Hour).Format(time.RFC3339)},
		}
	}
	origDigest := ghcrTagDigest
	ghcrTagDigest = func(_ string, tag string, _ *slog.Logger) string {
		return "sha256:" + tag
	}
	origGen := ghcrTagGeneration
	ghcrTagGeneration = func(_ string, tag string, _ *slog.Logger) int {
		switch tag {
		case "new1234":
			return 300
		case "old1234":
			return 200
		default:
			return 0
		}
	}
	t.Cleanup(func() {
		stablePromotionFetchRuns = origRuns
		ghcrTagDigest = origDigest
		ghcrTagGeneration = origGen
	})

	build, eligibleAt := stablePromotionEligibleBuild(100, now, slog.Default())
	if build.Generation != 200 || build.SHA != "old1234" {
		t.Fatalf("eligible build = %+v, want superseded soaked generation 200", build)
	}
	if eligibleAt != now.Format(time.RFC3339) {
		t.Fatalf("eligibleAt = %q, want now %q", eligibleAt, now.Format(time.RFC3339))
	}
}

func TestMyHivesPayloadCarriesStablePromotionOnReleaseChannels(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	seedStablePromotionChannels(t)
	s := newHubServerForTest(t, withHubIdentity("hubsha", "v5"))
	mkUser(t, "alice")

	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		StablePromotion StablePromotionStatus `json:"stable_promotion"`
		ChannelTargets  []ChannelTarget       `json:"channel_targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode my-hives: %v", err)
	}
	if !payload.StablePromotion.AutoPromote {
		t.Fatalf("top-level stable promotion missing/default false: %+v", payload.StablePromotion)
	}
	found := false
	for _, ct := range payload.ChannelTargets {
		if ct.Channel == ReleaseChannelStable {
			found = ct.StablePromotion != nil && ct.StablePromotion.AutoPromote
		}
	}
	if !found {
		t.Fatalf("stable channel row did not carry promotion state: %+v", payload.ChannelTargets)
	}
}

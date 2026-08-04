package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// meSeedDir points HIVE_CONTRIBUTORS_DIR + HIVE_FEDERATION_REGISTRY_PATH at fresh
// temp dirs and returns the contributors dir so a test can write several profiles.
func meSeedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(t.TempDir(), "federation", "registry.json"))
	return dir
}

func meWriteProfile(t *testing.T, dir string, p *ContributorProfile) {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.GitHubUsername+".json"), data, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

// Part A: the hub endpoint aggregates a real contributor's central profile —
// identity, level (trust tier), stats, milestones, hives, and rank.
func TestMeProfile_AggregatesSeededContributor(t *testing.T) {
	dir := meSeedDir(t)
	// A Trusted contributor with 27 shipped tasks, 22 with PR, 3 failed.
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted",
		TasksCompleted: 27, TasksWithPR: 22, TasksFailed: 3, RegisteredAt: "2026-01-01T00:00:00Z",
		RegistrationToken: "hash", TokenPlain: "secret",
	})
	// A lower-ranked contributor so alice ranks #1 of 2.
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer",
		TasksCompleted: 2, TasksWithPR: 0, TasksFailed: 1,
	})
	// Register a federation hive so "my hives" is populated from the registry.
	reg := loadFederationRegistry()
	reg.Hives = append(reg.Hives, FederationHive{ID: "hive-ks", ProjectName: "KubeStellar", Org: "kubestellar", HubURL: "https://x"})
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	s := covContribServer(t)
	rec := doGet(s, "/api/leaderboard/contributor/alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var p ContributorProfileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !p.Found {
		t.Fatal("expected found=true for seeded contributor")
	}
	if p.GitHubUsername != "alice" || p.AvatarURL == "" {
		t.Fatalf("identity wrong: %+v", p)
	}
	if p.TrustTier != "trusted" {
		t.Fatalf("level (trust tier) = %q, want trusted", p.TrustTier)
	}
	if p.TasksCompleted != 27 || p.TasksWithPR != 22 || p.TasksFailed != 3 {
		t.Fatalf("stats wrong: %+v", p)
	}
	// Rank comes from LeaderboardForHub's ordering (agents may rank first, so
	// alice's absolute rank is not necessarily 1). What must hold: alice has a
	// real rank, ranks ABOVE bob among contributors, and total counts everyone.
	if p.Rank <= 0 || p.Total < 2 {
		t.Fatalf("rank wrong: rank=%d total=%d", p.Rank, p.Total)
	}
	bobRec := doGet(s, "/api/leaderboard/contributor/bob")
	var bp ContributorProfileResponse
	json.Unmarshal(bobRec.Body.Bytes(), &bp)
	if bp.Rank <= p.Rank {
		t.Fatalf("alice (%d shipped) should rank above bob: alice=#%d bob=#%d", p.TasksCompleted, p.Rank, bp.Rank)
	}
	// Milestones: Contributor + Trusted tiers attained (22 >= 20 PR-tasks), Advisor not.
	got := map[string]bool{}
	for _, m := range p.Milestones {
		got[m.ID] = m.Attained
	}
	if !got["tier-contributor"] || !got["tier-trusted"] {
		t.Fatalf("expected Contributor+Trusted milestones attained: %+v", p.Milestones)
	}
	if got["tier-advisor"] {
		t.Fatal("Advisor should NOT be attained for a trusted contributor")
	}
	// Tasks-shipped milestone: 25 attained (27 >= 25), 50 not.
	if !got["tasks-25"] || got["tasks-50"] {
		t.Fatalf("tasks-shipped milestone thresholds wrong: %+v", p.Milestones)
	}
	if p.NextMilestone == nil {
		t.Fatal("expected a next milestone (progression cue)")
	}
	// Hives from the registry.
	if len(p.Hives) != 1 || p.Hives[0].ProjectName != "KubeStellar" {
		t.Fatalf("my hives wrong: %+v", p.Hives)
	}
	// No private material must leak (mirrors the leaderboard's public exposure).
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "hash") {
		t.Fatalf("token material leaked: %s", rec.Body.String())
	}
}

// Unknown / anonymous viewer: found=false, NOT an HTTP error (the client shows a
// sign-in prompt). Revoked contributors are hidden just like on the leaderboard.
func TestMeProfile_UnknownAndRevoked(t *testing.T) {
	dir := meSeedDir(t)
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "revoked-user", ContributorID: "c-rev", TrustTier: "revoked", TasksCompleted: 99,
	})
	s := covContribServer(t)

	// Unknown username → 200 with found=false.
	rec := doGet(s, "/api/leaderboard/contributor/nobody")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown status: %d", rec.Code)
	}
	var p ContributorProfileResponse
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Found {
		t.Fatal("unknown user must report found=false")
	}

	// Revoked contributor is not shown.
	rec = doGet(s, "/api/leaderboard/contributor/revoked-user")
	var pr ContributorProfileResponse
	json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Found {
		t.Fatal("revoked contributor must not be shown (found=false)")
	}

	// Invalid username → 400.
	rec = doGet(s, "/api/leaderboard/contributor/bad..name")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("invalid username: got %d, want 400/404", rec.Code)
	}
}

// The leaderboard tab / landing HTML must include the Me-card mount, the profile
// endpoint call, the 7-style selector, the Credly placeholder, and the LinkedIn
// share affordance — with no live external Credly call.
func TestMeProfile_LandingHTMLWiring(t *testing.T) {
	meSeedDir(t)
	s := covContribServer(t)
	rec := doGet(s, "/contribute")
	if rec.Code != http.StatusOK {
		t.Fatalf("landing status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"me-card-mount",
		"/api/leaderboard/contributor/",
		"/api/gh-user-auth/status",
		"me-style-select",
		"hive.me.cardStyle",
		"linkedin.com/sharing/share-offsite",
		"Credly",
		"me-card--style7", // proves all 7 skins are present in the CSS
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing HTML missing %q", want)
		}
	}
	// Placeholder only — the page must not call an external Credly API.
	if strings.Contains(body, "credly.com/api") || strings.Contains(body, "api.credly.com") {
		t.Fatalf("Credly must be a placeholder — no live API call in the page")
	}
}

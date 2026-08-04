package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func setupContributeEnv(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", filepath.Join(tmpDir, "contributors"))
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(tmpDir, "federation", "registry.json"))

	// Stub DNS so tests with fake hostnames don't hit the fail-closed resolver.
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
}

func TestContributeRegister(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"testuser123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["contributor_id"] == "" {
		t.Error("missing contributor_id")
	}
	if resp["registration_token"] == "" {
		t.Error("missing registration_token")
	}
	if !strings.Contains(resp["message"], "Registered successfully") {
		t.Errorf("unexpected message: %s", resp["message"])
	}

}

func TestContributeRegisterInvalidUsername(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"bad user!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestContributeRegisterEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestContributeStatus(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["hub"] != "online" {
		t.Errorf("expected hub=online, got %v", resp["hub"])
	}
}

func TestContributeLanding(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("Contribute to")) {
		t.Error("landing page missing expected content")
	}

	body := w.Body.Bytes()

	// #2536/#2540a: Copilot must carry a real host-mode install command, not
	// an empty data-host-install that falls through to a comment-only line.
	if !bytes.Contains(body, []byte(`data-host-install="npm install -g @github/copilot`)) {
		t.Error("copilot option missing a non-empty data-host-install")
	}

	// #2536: an OS selector must exist so install guidance isn't macOS-only,
	// and macOS must remain the default selection with the unchanged
	// brew-based prerequisite line.
	if !bytes.Contains(body, []byte(`id="os-select"`)) {
		t.Error("landing page missing OS selector")
	}
	if !bytes.Contains(body, []byte(`<option value="macos" selected>macOS</option>`)) {
		t.Error("OS selector must default to macOS")
	}
	if !bytes.Contains(body, []byte("brew install just gh")) {
		t.Error("macOS default prerequisite line (brew install just gh) missing")
	}

	// #2536/#2540b: the "don't see your CLI?" prefill link must not reference
	// the non-existent "contribute" label — only "enhancement" exists.
	if bytes.Contains(body, []byte("labels=contribute")) {
		t.Error("CLI request link must not reference the non-existent 'contribute' label")
	}
	if !bytes.Contains(body, []byte("labels=enhancement")) {
		t.Error("CLI request link missing labels=enhancement")
	}

	// #2536/#2540: the server-rendered (no-JS) fallback command block must be
	// self-labeling as a default.
	if !bytes.Contains(body, []byte("Default shown: macOS + Claude Code + containerized mode")) {
		t.Error("server-rendered fallback command block missing default marker comment")
	}
}

// TestContributeLandingHasOpsTab pins the additive tab chrome: the landing page
// must now render the Onboarding / Management & Operations tab bar, the new ops
// panel, AND still carry the existing onboarding content unchanged.
func TestContributeLandingHasOpsTab(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// New tab chrome.
	for _, want := range []string{
		`class="page-tabs"`,
		`data-panel="tab-onboarding"`,
		`data-panel="tab-ops"`,
		`Management &amp; Operations`,
		`id="tab-ops"`,
		`Connected clankers`,
		`id="work-list"`,
		`/api/contribute/fleet`,
		`opened`, // pipeline node
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ops tab chrome missing %q", want)
		}
	}

	// Existing onboarding content must still be present and unchanged.
	for _, want := range []string{
		`id="cli-select"`,
		`id="mode-select"`,
		`id="runtime-select"`,
		`How it works`,
		`Powered by <strong style="color:#e6edf3">ClankeR</strong>`,
		`Trust tiers`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onboarding content missing %q (must remain unchanged)", want)
		}
	}
}

// TestContributeFleet pins the read-only fleet endpoint JSON shape.
func TestContributeFleet(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Clankers []FleetClanker            `json:"clankers"`
		Work     []FleetWorkItem           `json:"work"`
		Policy   ContributeAdmissionPolicy `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// With no connections the fleet is empty but the keys must be present arrays.
	if resp.Clankers == nil {
		t.Error("clankers should be a non-nil array")
	}
	if resp.Work == nil {
		t.Error("work should be a non-nil array")
	}
	// Policy always exposes the promotion thresholds sourced from constants.
	if resp.Policy.AutoPromoteAt != contributorAutoPromoteAt {
		t.Errorf("auto_promote_at = %d, want %d", resp.Policy.AutoPromoteAt, contributorAutoPromoteAt)
	}
	if resp.Policy.TrustedAt != contributorTrustedAt {
		t.Errorf("trusted_at = %d, want %d", resp.Policy.TrustedAt, contributorTrustedAt)
	}
}

func TestContributorNotFound(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contributors/nonexistent", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHivesRegisterAndList(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// Register
	body := `{"project_name":"test-proj","org":"test-org","hub_url":"wss://test:3001/contribute"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("register: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// List
	req2 := httptest.NewRequest(http.MethodGet, "/api/hives", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w2.Code)
	}

	var reg FederationRegistry
	if err := json.Unmarshal(w2.Body.Bytes(), &reg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	found := false
	for _, h := range reg.Hives {
		if h.ProjectName == "test-proj" {
			found = true
			break
		}
	}
	if !found {
		t.Error("registered hive not found in list")
	}

}

func TestHivesRegisterMissingFields(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── Leaderboard tests ─────────────────────────────────────────────────────

func seedContributor(t *testing.T, username string, completed, failed int) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "contributor",
		TasksCompleted: completed,
		TasksFailed:    failed,
		RegisteredAt:   "2025-01-01T00:00:00Z",
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("seedContributor(%s): %v", username, err)
	}
}

func TestLeaderboardAPIEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Leaderboard) != 0 {
		t.Errorf("expected empty leaderboard, got %d entries", len(resp.Leaderboard))
	}
}

func TestLeaderboardAPISorted(t *testing.T) {
	setupContributeEnv(t)

	seedContributor(t, "alice", 10, 2)
	seedContributor(t, "bob", 25, 1)
	seedContributor(t, "carol", 5, 0)

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(resp.Leaderboard) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(resp.Leaderboard))
	}

	// Verify sort order: bob (25) > alice (10) > carol (5)
	expectedOrder := []string{"bob", "alice", "carol"}
	for i, name := range expectedOrder {
		if resp.Leaderboard[i].GitHubUsername != name {
			t.Errorf("rank %d: expected %s, got %s", i+1, name, resp.Leaderboard[i].GitHubUsername)
		}
		if resp.Leaderboard[i].Rank != i+1 {
			t.Errorf("rank %d: expected rank=%d, got rank=%d", i+1, i+1, resp.Leaderboard[i].Rank)
		}
	}

	// Verify avatar URL format
	if resp.Leaderboard[0].AvatarURL != "https://github.com/bob.png" {
		t.Errorf("unexpected avatar URL: %s", resp.Leaderboard[0].AvatarURL)
	}
}

func TestLeaderboardPageHTML(t *testing.T) {
	setupContributeEnv(t)

	seedContributor(t, "alice", 10, 2)

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("expected text/html content-type, got %s", contentType)
	}

	body := w.Body.String()
	checks := map[string]string{
		"gradient-text":        "animated gradient header text",
		"Leaderboard":          "page heading",
		"alice":                "contributor username",
		"github.com/alice.png": "avatar URL",
		"github.com/alice":     "GitHub profile link",
		"search":               "search input",
		"sort-completed":       "sortable completed column",
		"Trust Tiers":          "trust tiers reference section",
		"bg-stars":             "starfield background",
		"var AGENTS":           "JavaScript agent entries data",
		"var CONTRIBUTORS":     "JavaScript contributor entries data",
		"toggleSort":           "sort toggle function",
		"renderRows":           "row rendering function",
		"hover-card":           "contributor hover card CSS",
		"hc-header":            "hover card header",
		"hc-bar":               "hover card success rate bar",
	}
	for needle, desc := range checks {
		if !strings.Contains(body, needle) {
			t.Errorf("page missing %s (looked for %q)", desc, needle)
		}
	}
}

func TestLeaderboardPageEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	// Empty entries arrays should be passed to JS
	if !strings.Contains(body, "var AGENTS = []") {
		t.Error("empty page should pass empty AGENTS array")
	}
	if !strings.Contains(body, "var CONTRIBUTORS = []") {
		t.Error("empty page should pass empty CONTRIBUTORS array")
	}
	if !strings.Contains(body, "/contribute") {
		t.Error("empty page should link to /contribute")
	}
}

func TestTrustTierColor(t *testing.T) {
	cases := []struct {
		tier  string
		color string
	}{
		{"newcomer", "#8b949e"},
		{"contributor", "#3fb950"},
		{"trusted", "#d29922"},
		{"advisor", "#a371f7"},
		{"revoked", "#f85149"},
		{"unknown", "#8b949e"},
	}
	for _, tc := range cases {
		got := trustTierColor(tc.tier)
		if got != tc.color {
			t.Errorf("trustTierColor(%q) = %q, want %q", tc.tier, got, tc.color)
		}
	}
}

func TestTrustTierBadgeCSS(t *testing.T) {
	cases := []struct {
		tier   string
		wantBg string
	}{
		{"newcomer", "rgba(107,114,128,0.2)"},
		{"contributor", "rgba(59,130,246,0.2)"},
		{"trusted", "rgba(34,197,94,0.2)"},
		{"advisor", "rgba(168,85,247,0.2)"},
		{"revoked", "rgba(239,68,68,0.2)"},
		{"unknown", "rgba(107,114,128,0.2)"},
	}
	for _, tc := range cases {
		bg, text, border := trustTierBadgeCSS(tc.tier)
		if bg != tc.wantBg {
			t.Errorf("trustTierBadgeCSS(%q) bg = %q, want %q", tc.tier, bg, tc.wantBg)
		}
		if text == "" {
			t.Errorf("trustTierBadgeCSS(%q) text is empty", tc.tier)
		}
		if border == "" {
			t.Errorf("trustTierBadgeCSS(%q) border is empty", tc.tier)
		}
	}
}

func TestRankDisplay(t *testing.T) {
	gold := rankDisplay(1)
	if !strings.Contains(gold, "medal") || !strings.Contains(gold, "1st place") {
		t.Errorf("rank 1 should show gold medal, got %q", gold)
	}

	silver := rankDisplay(2)
	if !strings.Contains(silver, "medal") || !strings.Contains(silver, "2nd place") {
		t.Errorf("rank 2 should show silver medal, got %q", silver)
	}

	bronze := rankDisplay(3)
	if !strings.Contains(bronze, "medal") || !strings.Contains(bronze, "3rd place") {
		t.Errorf("rank 3 should show bronze medal, got %q", bronze)
	}

	fourth := rankDisplay(4)
	if !strings.Contains(fourth, "#4") {
		t.Errorf("rank 4 should show #4, got %q", fourth)
	}
}

func TestIsValidUsername(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"testuser", true},
		{"test-user", true},
		{"test_user123", true},
		{"j.doe", true},
		{"user.name.with.dots", true},
		{"bad user!", false},
		{"", false},
		{"user@name", false},
		{"<script>alert(1)</script>", false},
		{"../../../etc/passwd", false},
		{strings.Repeat("a", 39), true},
		{strings.Repeat("a", 40), false},
	}
	for _, tc := range cases {
		got := isValidUsername(tc.input)
		if got != tc.want {
			t.Errorf("isValidUsername(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// ── Handler coverage tests ──────────────────────────────────────────────

func registerTestUser(t *testing.T, s *Server, username string) (contributorID, token string) {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register %s: %d %s", username, w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp["contributor_id"], resp["registration_token"]
}

func TestRegisterForceReissue(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// First registration — should succeed with a token
	cid, token1 := registerTestUser(t, s, "force-user")
	if token1 == "" {
		t.Fatal("expected token on first register")
	}

	// Second registration without force — should say already registered, no token
	body := `{"github_username":"force-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["registration_token"] != "" {
		t.Error("should not return token without force")
	}

	// Third registration with force — should reissue a new token
	body = `{"github_username":"force-user","force":true}`
	req = httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	token2 := resp["registration_token"]
	if token2 == "" {
		t.Fatal("expected new token with force:true")
	}
	if token2 == token1 {
		t.Error("reissued token should differ from original")
	}
	if resp["contributor_id"] != cid {
		t.Error("contributor_id should be preserved on reissue")
	}

	// Verify old token no longer works by checking the stored hash
	profile, _ := loadContributorProfile("force-user")
	if profile == nil {
		t.Fatal("profile should exist")
	}
	oldHash := sha256Hex(token1)
	if profile.RegistrationToken == oldHash {
		t.Error("old token hash should have been replaced")
	}
	newHash := sha256Hex(token2)
	if profile.RegistrationToken != newHash {
		t.Error("stored hash should match the new token")
	}
}

func TestContributorsList(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	registerTestUser(t, s, "list-a")
	registerTestUser(t, s, "list-b")

	req := httptest.NewRequest(http.MethodGet, "/api/contributors", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Contributors []ContributorProfile `json:"contributors"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Contributors) != 2 {
		t.Fatalf("expected 2, got %d", len(resp.Contributors))
	}
	for _, c := range resp.Contributors {
		if c.RegistrationToken != "" || c.TokenPlain != "" {
			t.Errorf("token leaked for %s", c.GitHubUsername)
		}
	}
}

func TestContributorGet(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "get-test")

	// Get by ID
	req := httptest.NewRequest(http.MethodGet, "/api/contributors/"+cid, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("by ID: expected 200, got %d", w.Code)
	}

	// Get by username
	req2 := httptest.NewRequest(http.MethodGet, "/api/contributors/get-test", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("by username: expected 200, got %d", w2.Code)
	}
}

func TestContributorTrust(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "trust-test")

	// Valid tier change
	body := `{"tier":"contributor"}`
	req := httptest.NewRequest(http.MethodPut, "/api/contributors/"+cid+"/trust", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Invalid tier
	body2 := `{"tier":"superadmin"}`
	req2 := httptest.NewRequest(http.MethodPut, "/api/contributors/"+cid+"/trust", bytes.NewBufferString(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid tier, got %d", w2.Code)
	}
}

func TestContributorDelete(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "delete-test")

	req := httptest.NewRequest(http.MethodDelete, "/api/contributors/"+cid, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify gone
	req2 := httptest.NewRequest(http.MethodGet, "/api/contributors/"+cid, nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", w2.Code)
	}
}

func TestContributorRevoke(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "revoke-test")

	req := httptest.NewRequest(http.MethodPost, "/api/contributors/"+cid+"/revoke", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Re-register blocked
	body := `{"github_username":"revoke-test"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for revoked re-register, got %d", w2.Code)
	}
}

func TestContributeActivity(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Activity []any `json:"activity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestHivesHeartbeat(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// Register a hive first
	body := `{"project_name":"hb","org":"hb-org","hub_url":"wss://x:3001/c"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	// Heartbeat
	hb := `{"active_contributors":5,"active_agents":3}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/hives/hive-hb-org-hb/heartbeat", bytes.NewBufferString(hb))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// 404 for unknown hive
	req3 := httptest.NewRequest(http.MethodPost, "/api/hives/nonexistent/heartbeat", bytes.NewBufferString(hb))
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHivesDelete(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"del","org":"del-org","hub_url":"wss://x:3001/c"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	req2 := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-del-org-del", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	// 404 for already deleted
	req3 := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-del-org-del", nil)
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHivesOnboard(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"ob","org":"ob-org","repos":["ob-org/repo1"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/onboard", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		NextSteps []string `json:"next_steps"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.NextSteps) < 3 {
		t.Errorf("expected >=3 steps, got %d", len(resp.NextSteps))
	}
}

func TestReservedUsernames(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	reserved := []string{"null", "undefined", "admin", "root", "system", "hive", "api", "contribute", "leaderboard"}
	for _, name := range reserved {
		body := `{"github_username":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("reserved name %q: expected 400, got %d", name, w.Code)
		}
	}
}

func TestBodySizeLimit(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	huge := `{"github_username":"x","padding":"` + strings.Repeat("A", 50000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(huge))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for huge body, got %d", w.Code)
	}
}

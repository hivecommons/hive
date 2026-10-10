package dashboard

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestTierWeightOrdersTiers(t *testing.T) {
	weights := map[string]int{
		achievementTierRaid:     4,
		achievementTierFireteam: 3,
		achievementTierDual:     2,
		achievementTierSolo:     1,
		"":                      0,
		"legendary":             0,
	}
	for tier, want := range weights {
		if got := tierWeight(tier); got != want {
			t.Fatalf("tierWeight(%q) = %d, want %d", tier, got, want)
		}
	}
}

func TestTopAttainedAchievementLabelsSortsByTier(t *testing.T) {
	got := topAttainedAchievementLabels([]ContributorAchievement{
		{Label: "solo", Tier: achievementTierSolo, Attained: true},
		{Label: "locked raid", Tier: achievementTierRaid},
		{Label: "raid", Tier: achievementTierRaid, Attained: true},
		{Label: "dual", Tier: achievementTierDual, Attained: true},
		{Label: "fireteam", Tier: achievementTierFireteam, Attained: true},
	}, 3)
	if strings.Join(got, ",") != "raid,fireteam,dual" {
		t.Fatalf("labels = %v", got)
	}
}

func TestAttainedMilestoneLabelsHonoursLimit(t *testing.T) {
	milestones := []ContributorMilestone{
		{Label: "one", Attained: true},
		{Label: "skip"},
		{Label: "two", Attained: true},
		{Label: "three", Attained: true},
	}
	if got := attainedMilestoneLabels(milestones, 2); strings.Join(got, ",") != "one,two" {
		t.Fatalf("limited labels = %v", got)
	}
	if got := attainedMilestoneLabels(milestones, 10); strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("all labels = %v", got)
	}
	if got := attainedMilestoneLabels(nil, 3); len(got) != 0 {
		t.Fatalf("nil milestones = %v", got)
	}
}

func TestFindAttainedAchievement(t *testing.T) {
	profile := ContributorProfileResponse{Achievements2: []ContributorAchievement{
		{ID: "locked", Attained: false},
		{ID: "open", Attained: true, Label: "Open"},
	}}
	if _, ok := findAttainedAchievement(profile, "locked"); ok {
		t.Fatal("locked achievement must not be found")
	}
	if got, ok := findAttainedAchievement(profile, "open"); !ok || got.Label != "Open" {
		t.Fatalf("open achievement = %+v, %v", got, ok)
	}
	if _, ok := findAttainedAchievement(profile, "missing"); ok {
		t.Fatal("missing achievement must not be found")
	}
}

func TestRankValueAndDisplayDate(t *testing.T) {
	if got := rankValue(0, 5); got != "—" {
		t.Fatalf("rankValue(0,5) = %q", got)
	}
	if got := rankValue(2, 0); got != "—" {
		t.Fatalf("rankValue(2,0) = %q", got)
	}
	if got := rankValue(2, 9); got != "#2/9" {
		t.Fatalf("rankValue(2,9) = %q", got)
	}
	if got := displayDate("2026-01-02T03:04:05+02:00"); got != "2026-01-02" {
		t.Fatalf("displayDate = %q", got)
	}
	if got := displayDate("yesterday"); got != "" {
		t.Fatalf("displayDate(garbage) = %q", got)
	}
}

func TestHiveNameFromProfileFallbackOrder(t *testing.T) {
	cases := []struct {
		name  string
		hives []ContributorHiveRel
		want  string
	}{
		{"none", nil, "Hive"},
		{"project name", []ContributorHiveRel{{ID: "id", Org: "org", ProjectName: " Widgets "}}, " Widgets "},
		{"org", []ContributorHiveRel{{ID: "id", Org: "acme", ProjectName: " "}}, "acme"},
		{"id", []ContributorHiveRel{{ID: "hive-1"}}, "hive-1"},
		{"first non-empty wins", []ContributorHiveRel{{}, {Org: "second"}}, "second"},
		{"all blank", []ContributorHiveRel{{ID: " ", Org: "", ProjectName: ""}}, "Hive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hiveNameFromProfile(ContributorProfileResponse{Hives: tc.hives}); got != tc.want {
				t.Fatalf("hiveNameFromProfile = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublicHiveNameFallsBackWithoutConfig(t *testing.T) {
	var nilServer *Server
	if got := nilServer.publicHiveName(); got != "Hive" {
		t.Fatalf("nil server = %q", got)
	}
	s := seedSocialCardProfile(t, "alice")
	if got := s.publicHiveName(); got != "Hive" {
		t.Fatalf("no deps = %q", got)
	}
	s.deps = &Dependencies{Config: &config.Config{}}
	if got := s.publicHiveName(); got != "Hive" {
		t.Fatalf("blank project name = %q", got)
	}
	s.deps.Config.Project.Name = "  Acme Hive  "
	if got := s.publicHiveName(); got != "Acme Hive" {
		t.Fatalf("configured name = %q", got)
	}
}

func TestTopTeamHighlightsCapsAtThreeAcrossLeagues(t *testing.T) {
	teams := TeamLeaderboardResponse{
		ByDistro: []TeamLeaderboardEntry{{Team: "fedora", TasksCompleted: 5}, {Team: "debian", TasksCompleted: 4}},
		ByOS:     []TeamLeaderboardEntry{{Team: "linux", TasksCompleted: 9}},
		ByAgent:  []TeamLeaderboardEntry{{Team: "claude", TasksCompleted: 1}},
	}
	got := topTeamHighlights(teams)
	if strings.Join(got, "|") != "fedora · 5 tasks|debian · 4 tasks|linux · 9 tasks" {
		t.Fatalf("highlights = %v", got)
	}
	if got := topTeamHighlights(TeamLeaderboardResponse{}); len(got) != 0 {
		t.Fatalf("empty teams = %v", got)
	}
}

func TestRarestTeamNameAndEmptyDash(t *testing.T) {
	if got := rarestTeamName(nil); got != "" {
		t.Fatalf("nil rarest = %q", got)
	}
	if got := rarestTeamName(&TeamRarestCallout{Team: "nixos"}); got != "nixos" {
		t.Fatalf("rarest = %q", got)
	}
	if got := emptyDash("  "); got != "—" {
		t.Fatalf("emptyDash(blank) = %q", got)
	}
	if got := emptyDash("x"); got != "x" {
		t.Fatalf("emptyDash(x) = %q", got)
	}
}

func TestTruncateSocialRunes(t *testing.T) {
	if got := truncateSocialRunes("  short  ", 10); got != "short" {
		t.Fatalf("short = %q", got)
	}
	if got := truncateSocialRunes("ééééé", 3); got != "éé…" {
		t.Fatalf("truncated = %q", got)
	}
	if got := truncateSocialRunes("abc", 1); got != "a" {
		t.Fatalf("max 1 = %q", got)
	}
	if got := truncateSocialRunes("abc", 0); got != "" {
		t.Fatalf("max 0 = %q", got)
	}
}

func TestParseShareAchievementPath(t *testing.T) {
	login, id, ok := parseShareAchievementPath(socialShareAchievementPrefix + "alice/first-useful-change")
	if !ok || login != "alice" || id != "first-useful-change" {
		t.Fatalf("parsed = %q %q %v", login, id, ok)
	}
	for _, bad := range []string{
		"/share/player/alice",
		socialShareAchievementPrefix + "alice",
		socialShareAchievementPrefix + "alice/",
		socialShareAchievementPrefix + "/id",
		socialShareAchievementPrefix + "alice/id/extra",
	} {
		if _, _, ok := parseShareAchievementPath(bad); ok {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestValidLeaderboardCardBoard(t *testing.T) {
	for board, want := range map[string]bool{"contributors": true, "teams": true, "swarm": true, "": false, "players": false} {
		if got := validLeaderboardCardBoard(board); got != want {
			t.Fatalf("validLeaderboardCardBoard(%q) = %v", board, got)
		}
	}
}

func TestAchievementSharePageRendersAttainedAchievement(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/achievement/alice/first-useful-change", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<h1>First Useful Change</h1>",
		"alice unlocked a",
		`content="http://example.com/cards/achievement/alice/first-useful-change.svg"`,
		"Explore the public dossier",
		"<title>First Useful Change · alice</title>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("share page missing %q in %s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestAchievementSharePageNotFoundCases(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	for _, path := range []string{
		"/share/achievement/alice/bad.id",
		"/share/achievement/al!ice/first-useful-change",
		"/share/achievement/ghost/first-useful-change",
		"/share/achievement/alice/no-such-achievement",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestPlayerSharePageRejectsInvalidLogin(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	for _, path := range []string{"/share/player/al!ice", "/share/player/ghost"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestLeaderboardSharePageAndCardForContributors(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/leaderboard/contributors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("share status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"<h1>Contributor standings</h1>", "<li>#1 alice · 7 tasks</li>", "/cards/leaderboard/contributors.svg", "Open the live leaderboard"} {
		if !strings.Contains(body, want) {
			t.Fatalf("share page missing %q in %s", want, body)
		}
	}

	card := httptest.NewRecorder()
	s.mux.ServeHTTP(card, httptest.NewRequest(http.MethodGet, "/api/cards/leaderboard/contributors.svg", nil))
	if card.Code != http.StatusOK {
		t.Fatalf("card status = %d", card.Code)
	}
	svg := card.Body.String()
	if !strings.Contains(svg, "Contributor standings") || !strings.Contains(svg, "#1 alice · 7 tasks") || !strings.Contains(svg, `href="/share/leaderboard/contributors"`) {
		t.Fatalf("card svg = %s", svg)
	}
	if ct := card.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestLeaderboardSharePageForTeamsWithoutOptIns(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/leaderboard/teams", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<h1>Team leagues</h1>") || !strings.Contains(body, "<ul></ul>") {
		t.Fatalf("teams share page = %s", body)
	}
	card := httptest.NewRecorder()
	s.mux.ServeHTTP(card, httptest.NewRequest(http.MethodGet, "/cards/leaderboard/teams.svg", nil))
	if card.Code != http.StatusOK || !strings.Contains(card.Body.String(), "RAREST") || !strings.Contains(card.Body.String(), ">—<") {
		t.Fatalf("teams card = %d %s", card.Code, card.Body.String())
	}
}

func TestLeaderboardSharePageForSwarmUsesHistory(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetSocialCardCacheForTest()
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	deps.Config.Project.Repos = []string{"api"}
	deps.Config.Project.Name = "Acme Hive"
	s.SetContributorsDir(t.TempDir())
	s.swarm = newSwarmStoreWithConfig(filepath.Join(s.contributorsDirOrDefault(), SwarmStateFileName), &fakeSwarmScorer{score: ghpkg.SwarmScore{PRsMerged: 2, Participants: []string{"alice"}, PRsByAuthor: map[string]int{"alice": 2}}}, deps.Config)

	empty := doGet(s, "/share/leaderboard/swarm")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), "<h1>Swarm leaderboard</h1>") {
		t.Fatalf("empty swarm share = %d %s", empty.Code, empty.Body.String())
	}

	if start := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "api"}); start.Code != http.StatusOK {
		t.Fatalf("start swarm = %d body=%s", start.Code, start.Body.String())
	}
	if end := doDeleteOwner(s, "/api/swarm", nil); end.Code != http.StatusOK {
		t.Fatalf("end swarm = %d body=%s", end.Code, end.Body.String())
	}

	rec := doGet(s, "/share/leaderboard/swarm")
	if rec.Code != http.StatusOK {
		t.Fatalf("swarm share = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<li>#1 acme/api · ") {
		t.Fatalf("swarm share page lacks leaderboard row: %s", rec.Body.String())
	}
	card := doGet(s, "/cards/leaderboard/swarm.svg")
	if card.Code != http.StatusOK {
		t.Fatalf("swarm card = %d", card.Code)
	}
	svg := card.Body.String()
	if !strings.Contains(svg, "Swarm leaderboard") || !strings.Contains(svg, "Acme Hive") || !strings.Contains(svg, "#1 acme/api") {
		t.Fatalf("swarm card svg = %s", svg)
	}
}

func TestLeaderboardSharePageRejectsUnknownBoard(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	for _, path := range []string{"/share/leaderboard/players", "/share/leaderboard/"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBattleLogScrubsPublicEvents(t *testing.T) {
	events := buildBattleLogEvents([]ActivityEntry{{
		Timestamp: "2026-09-21T12:00:00Z",
		Username:  "alice@example.com",
		Action:    "completed",
		Task:      "github hivecommons/private#8844 token=secret",
	}}, 10, publicReposFromEnv())
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	got := events[0]
	if got.Actor != "[email]" {
		t.Fatalf("actor = %q, want scrubbed email", got.Actor)
	}
	if strings.Contains(got.Target, "hivecommons/private") || strings.Contains(got.Target, "secret") {
		t.Fatalf("target leaked private/token data: %#v", got)
	}
	if got.Repo != "" {
		t.Fatalf("repo leaked without public allowlist: %#v", got)
	}
	if got.Kind != "merged_pr" || got.Icon == "" {
		t.Fatalf("unexpected classification: %#v", got)
	}
}

func TestGourceLogWeekAndMapping(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "hivecommons/hive")
	week := weekStartUTC(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	activity := []ActivityEntry{
		{Timestamp: week.Add(time.Hour).Format(time.RFC3339), Username: "bee", Action: "picked up", Task: "github hivecommons/hive#8844: Battle Log"},
		{Timestamp: week.AddDate(0, 0, -1).Format(time.RFC3339), Username: "old", Action: "completed", Task: "github hivecommons/hive#1: old"},
		{Timestamp: week.Add(time.Hour).Format(time.RFC3339), Username: "other", Action: "completed", Task: "github hivecommons/other#2: other"},
	}
	lines := gourceLogForProject(activity, "hivecommons/hive", week, publicReposFromEnv())
	if len(lines) != 1 {
		t.Fatalf("lines = %#v, want one current-week project line", lines)
	}
	if !strings.Contains(lines[0], "|bee|A|hivecommons/hive/work-in-flight/") || !strings.HasSuffix(lines[0], "|FFB000") {
		t.Fatalf("unexpected gource line: %s", lines[0])
	}
	if strings.Contains(lines[0], "Battle") || strings.Contains(lines[0], "token") || strings.Contains(lines[0], "hivecommons/private") {
		t.Fatalf("gource line leaked raw task data: %s", lines[0])
	}
}

func TestBusiestProjectTieBreaksAndWeekWindow(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "z/repo,a/repo")
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	week := weekStartUTC(now)
	activity := []ActivityEntry{
		{Timestamp: week.Add(time.Hour).Format(time.RFC3339), Task: "github z/repo#1"},
		{Timestamp: week.Add(2 * time.Hour).Format(time.RFC3339), Task: "github a/repo#2"},
		{Timestamp: week.AddDate(0, 0, -1).Format(time.RFC3339), Task: "github z/repo#3"},
	}
	project, count := busiestProject(activity, now, publicReposFromEnv())
	if project != "a/repo" || count != 1 {
		t.Fatalf("project,count = %q,%d; want tie-broken a/repo,1", project, count)
	}
}

func TestGourceLogSkipsNonAllowlistedActivity(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "hivecommons/hive")
	week := weekStartUTC(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	activity := []ActivityEntry{{
		Timestamp: week.Add(time.Hour).Format(time.RFC3339),
		Username:  "private@example.com",
		Action:    "completed",
		Task:      "github private/repo#9: fix production secret abc123",
	}}
	if lines := gourceLogForProject(activity, hiveGourceDefaultProject, week, publicReposFromEnv()); len(lines) != 0 {
		t.Fatalf("non-allowlisted activity leaked into hive gource log: %#v", lines)
	}
	project, count := busiestProject(activity, week.Add(time.Hour), publicReposFromEnv())
	if project != hiveGourceDefaultProject || count != 0 {
		t.Fatalf("busiest project = %q,%d; want hive fallback with zero public activity", project, count)
	}
}

func TestBattleLogAPIRouteIsPublicJSON(t *testing.T) {
	s, _ := apiServer(t)
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.activity = append(s.contributeHub.activity, ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Username:  "alice",
		Action:    "joined",
		Role:      "worker",
		CLI:       "hive",
	})
	rec := doGet(s, "/api/leaderboard/battle-log?limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET battle log = %d, want 200", rec.Code)
	}
	var body struct {
		Events []battleLogEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].Target != battleLogJoinTarget {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestHiveOfWeekArchiveRequiresOwner(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/leaderboard/hive-of-week/archive?project=hivecommons/hive", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("archive without owner = %d, want 403", rec.Code)
	}
}

func TestBattleLogStructuredTaskVerbsAndRefs(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "hivecommons/hive")
	events := buildBattleLogEvents([]ActivityEntry{
		{Timestamp: "2026-10-15T12:00:00Z", Username: "bee", Action: "completed", Task: "issue hivecommons/hive#12063: ✨ evidence: GET /api/hives/summary"},
		{Timestamp: "2026-10-15T12:01:00Z", Username: "bee", Action: "completed", Task: "pr hivecommons/hive#12070: fix"},
		{Timestamp: "2026-10-15T12:02:00Z", Username: "bee", Action: "picked up", Task: "issue hivecommons/hive#12071: next"},
		{Timestamp: "2026-10-15T12:03:00Z", Username: "bee", Action: "completed", Task: "issue acme/private#7: secret work"},
	}, 10, publicReposFromEnv())
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	private, pickedUp, mergedPR, closedIssue := events[0], events[1], events[2], events[3]
	if closedIssue.Action != "closed issue" || closedIssue.Ref != "hive#12063" || closedIssue.URL != "https://github.com/hivecommons/hive/issues/12063" {
		t.Fatalf("closed issue = %+v", closedIssue)
	}
	if closedIssue.Title != "✨ evidence: GET /api/hives/summary" || strings.Contains(closedIssue.Target, "[project]") {
		t.Fatalf("url path must not be scrubbed as a repo: %+v", closedIssue)
	}
	if mergedPR.Action != "merged PR" || mergedPR.URL != "https://github.com/hivecommons/hive/pull/12070" {
		t.Fatalf("merged pr = %+v", mergedPR)
	}
	if pickedUp.Action != "picked up issue" || pickedUp.Target != "hive#12071: next" {
		t.Fatalf("picked up = %+v", pickedUp)
	}
	if private.Ref != "#7" || private.URL != "" || private.Repo != "" || strings.Contains(private.Target, "acme") || strings.Contains(private.Target, "[project]") {
		t.Fatalf("non-public repo must drop the repo name without a placeholder: %+v", private)
	}
}

func TestBattleLogDedupesRepeatedJoins(t *testing.T) {
	events := buildBattleLogEvents([]ActivityEntry{
		{Timestamp: "2026-10-15T12:00:00Z", Username: "bee", Action: "joined"},
		{Timestamp: "2026-10-15T12:05:00Z", Username: "bee", Action: "left"},
		{Timestamp: "2026-10-15T12:15:00Z", Username: "bee", Action: "joined"},
		{Timestamp: "2026-10-15T12:16:00Z", Username: "wasp", Action: "joined"},
	}, 10, publicReposFromEnv())
	if len(events) != 2 {
		t.Fatalf("events = %+v, want one join per actor", events)
	}
	if events[0].Actor != "wasp" || events[1].Actor != "bee" || events[1].Timestamp != "2026-10-15T12:15:00Z" {
		t.Fatalf("expected newest join kept per actor: %+v", events)
	}
}

func TestScrubRepoRefsLeavesPathsAlone(t *testing.T) {
	cases := map[string]string{
		"GET /api/hives/summary":       "GET /api/hives/summary",
		"see acme/widgets for details": "see [project] for details",
		"acme/widgets#12 and b/c":      "[project]#12 and [project]",
		"path a/b/c stays":             "path a/b/c stays",
		"version 1.2/3 is not a repo":  "version [project] is not a repo",
		"docs at https://x.y/z/w":      "docs at [link]",
	}
	for in, want := range cases {
		if got := scrubBattleLogText(in); got != want {
			t.Fatalf("scrubBattleLogText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHiveOfWeekPayloadUsesConfiguredRender(t *testing.T) {
	t.Setenv(hiveOfWeekVideoURLEnv, "https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.mp4")
	t.Setenv(hiveOfWeekPosterURLEnv, "https://github.com/hivecommons/hive/releases/download/hive-of-the-week/latest.jpg")
	week := time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)
	resp := hiveOfWeekPayload("hivecommons/hive", week, 4, httptest.NewRequest(http.MethodGet, "/api/leaderboard/hive-of-week", nil))
	if !strings.HasSuffix(resp.VideoURL, "latest.mp4") || !strings.HasSuffix(resp.PosterURL, "latest.jpg") {
		t.Fatalf("media = %+v", resp)
	}
	if !strings.Contains(resp.EmbedHTML, "<video") || !strings.Contains(resp.ReadmeMarkdown, "[![Hive of the Week: hivecommons/hive]") {
		t.Fatalf("embed = %q readme = %q", resp.EmbedHTML, resp.ReadmeMarkdown)
	}
}

func TestRegistryPublicReposDerivedFromPublicHive(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "")
	// A public hive's own project repos are public; the primary repo counts
	// even when it is not repeated in repos.
	pub := registryPublicRepos(publicReposFromEnv(), true, "HiveCommons/Hive", []string{"hivecommons/docs", "not-a-repo", ""})
	for repo, want := range map[string]bool{
		"hivecommons/hive": true,
		"hivecommons/docs": true,
		"acme/private":     false,
		"not-a-repo":       false,
	} {
		if got := pub.allows(repo); got != want {
			t.Fatalf("public hive allows(%q) = %v, want %v", repo, got, want)
		}
	}
	// A hive the registry does not list as public contributes nothing.
	if pub := registryPublicRepos(publicReposFromEnv(), false, "hivecommons/hive", []string{"hivecommons/hive"}); pub.allows("hivecommons/hive") {
		t.Fatalf("private hive must not publish its repos")
	}
	// HIVE_PUBLIC_REPOS remains an explicit union with the registry-derived set.
	t.Setenv("HIVE_PUBLIC_REPOS", "acme/widgets")
	pub = registryPublicRepos(publicReposFromEnv(), true, "hivecommons/hive", nil)
	if !pub.allows("acme/widgets") || !pub.allows("hivecommons/hive") {
		t.Fatalf("env allowlist and registry repos must union: %v", pub)
	}
}

func TestBattleLogAndHiveOfWeekUseRegistryPublicRepos(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "")
	s, deps := apiServer(t)
	deps.Config.Hub.IsPublic = true
	deps.Config.Project.Repos = []string{"hivecommons/hive"}
	stamp := weekStartUTC(time.Now().UTC()).Add(26 * time.Hour).Format(time.RFC3339)
	seedBattleLogActivity(t, s,
		ActivityEntry{Timestamp: stamp, Username: "bee", Action: "completed", Task: "pr hivecommons/hive#12110: leaderboard TLC"},
		ActivityEntry{Timestamp: stamp, Username: "bee", Action: "completed", Task: "issue acme/private#3: hush"},
	)

	rec := doGet(s, "/api/leaderboard/battle-log?limit=5")
	var body struct {
		Events []battleLogEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Events) != 2 {
		t.Fatalf("battle log = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if body.Events[1].Ref != "hive#12110" || body.Events[1].URL != "https://github.com/hivecommons/hive/pull/12110" {
		t.Fatalf("public hive repo must link: %+v", body.Events[1])
	}
	if body.Events[0].Ref != "#3" || body.Events[0].URL != "" {
		t.Fatalf("other repo must stay anonymous: %+v", body.Events[0])
	}

	rec = doGet(s, "/api/leaderboard/hive-of-week")
	var hotw hiveOfWeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hotw); err != nil {
		t.Fatalf("json: %v", err)
	}
	if hotw.Project != "hivecommons/hive" || hotw.ActivityCount != 1 {
		t.Fatalf("hive of week = %+v, want registry-derived public project", hotw)
	}

	// Flip the registry verdict off and the same hive is anonymous again.
	deps.Config.Hub.IsPublic = false
	rec = doGet(s, "/api/leaderboard/hive-of-week")
	if err := json.Unmarshal(rec.Body.Bytes(), &hotw); err != nil || hotw.Project != hiveGourceDefaultProject || hotw.ActivityCount != 0 {
		t.Fatalf("private hive of week = %+v (%v)", hotw, err)
	}
}

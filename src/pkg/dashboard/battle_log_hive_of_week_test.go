package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyBattleLogActionBranches(t *testing.T) {
	cases := []struct {
		name   string
		action string
		entry  ActivityEntry
		kind   string
		verb   string
	}{
		{"completed", "completed", ActivityEntry{}, "merged_pr", "merged"},
		{"picked up local cli", "picked up", ActivityEntry{CLI: "ollama-local"}, "local_model_work", "started local-model work on"},
		{"picked up local model", "picked up", ActivityEntry{Model: "Local/llama"}, "local_model_work", "started local-model work on"},
		{"picked up remote", "picked up", ActivityEntry{CLI: "hive", Model: "sonnet"}, "review", "picked up"},
		{"joined", "joined", ActivityEntry{}, "joined_hive", "joined"},
		{"promoted", "promoted", ActivityEntry{}, "achievement_unlocked", "unlocked"},
		{"achievement substring", "earned achievement", ActivityEntry{}, "achievement_unlocked", "unlocked"},
		{"approved", "spec approved", ActivityEntry{}, "spec_approved", "approved"},
		{"swarm", "swarm start", ActivityEntry{}, "swarm_joined", "joined swarm"},
		{"unknown dropped", "failed", ActivityEntry{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, icon, verb := classifyBattleLogAction(tc.action, tc.entry)
			if kind != tc.kind || verb != tc.verb {
				t.Fatalf("classify(%q) = %q/%q, want %q/%q", tc.action, kind, verb, tc.kind, tc.verb)
			}
			if (kind == "") != (icon == "") {
				t.Fatalf("icon %q must be set exactly when kind %q is", icon, kind)
			}
		})
	}
}

func TestBattleLogDropsUnknownActions(t *testing.T) {
	events := buildBattleLogEvents([]ActivityEntry{
		{Timestamp: "2026-09-21T12:00:00Z", Username: "alice", Action: "failed", Task: "x"},
		{Timestamp: "2026-09-21T12:01:00Z", Username: "", Action: "joined"},
	}, 0)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want only the joined event", events)
	}
	if events[0].Actor != battleLogContributorSubject || events[0].Target != battleLogJoinTarget {
		t.Fatalf("anonymous join event = %+v", events[0])
	}
}

func TestBattleLogTargetFallbacks(t *testing.T) {
	cases := []struct {
		name  string
		entry ActivityEntry
		kind  string
		want  string
	}{
		{"join", ActivityEntry{Task: "anything"}, "joined_hive", battleLogJoinTarget},
		{"achievement without task", ActivityEntry{Task: "  "}, "achievement_unlocked", "achievement"},
		{"achievement with task", ActivityEntry{Task: "first merge"}, "achievement_unlocked", "first merge"},
		{"empty task", ActivityEntry{}, "merged_pr", battleLogGenericWorkTarget},
		{"plain task", ActivityEntry{Task: "fix flaky test"}, "merged_pr", "fix flaky test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := battleLogTarget(tc.entry, tc.kind); got != tc.want {
				t.Fatalf("battleLogTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBattleLogTargetNeverLeaksRepoReference(t *testing.T) {
	// scrubBattleLogText replaces the owner/repo token before battleLogTarget
	// inspects it, so the target is a placeholder, never the private name.
	got := battleLogTarget(ActivityEntry{Task: "org/private#12 token=abc"}, "merged_pr")
	if got == "" || strings.Contains(got, "org/private") || strings.Contains(got, "abc") {
		t.Fatalf("target leaked task data: %q", got)
	}
	if !strings.HasPrefix(got, "[project]") {
		t.Fatalf("expected scrubbed placeholder prefix, got %q", got)
	}
}

func TestScrubBattleLogTextControlCharsAndTruncation(t *testing.T) {
	got := scrubBattleLogText("bad\x01chars\x7fhere")
	if got != "badcharshere" {
		t.Fatalf("control chars not stripped: %q", got)
	}
	long := strings.Repeat("é", 130)
	got = scrubBattleLogText(long)
	if r := []rune(got); len(r) != 121 || r[120] != '…' {
		t.Fatalf("long text should truncate to 120 runes plus ellipsis, got %d runes", len(r))
	}
}

func TestParseWeekQueryFormats(t *testing.T) {
	now := time.Date(2026, time.October, 7, 15, 0, 0, 0, time.UTC)
	monday := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"", weekStartUTC(now)},
		{"2026-09-23", monday},
		{"2026-09-21", monday},
		{"2026-09-27", monday},
		{"2026-W39", monday},
		{"2026-W01", time.Date(2025, time.December, 29, 0, 0, 0, 0, time.UTC)},
		{"2026-W60", weekStartUTC(now)},
		{"2026-W0", weekStartUTC(now)},
		{"garbage", weekStartUTC(now)},
	}
	for _, tc := range cases {
		if got := parseWeekQuery(tc.raw, now); !got.Equal(tc.want) {
			t.Fatalf("parseWeekQuery(%q) = %s, want %s", tc.raw, got, tc.want)
		}
	}
	if got := formatHiveWeek(monday); got != "2026-W39" {
		t.Fatalf("formatHiveWeek = %q", got)
	}
	if got := formatHiveWeek(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)); got != "2026-W01" {
		t.Fatalf("formatHiveWeek(jan 1) = %q", got)
	}
}

func TestWeekStartUTCNormalizesZone(t *testing.T) {
	loc := time.FixedZone("plus14", 14*3600)
	// Monday 00:30 in +14 is still Sunday 10:30 UTC, so the UTC week starts
	// the previous Monday.
	local := time.Date(2026, time.September, 21, 0, 30, 0, 0, loc)
	if got := weekStartUTC(local); !got.Equal(time.Date(2026, time.September, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekStartUTC = %s", got)
	}
}

func TestHTMLAttrAndURLQueryEscape(t *testing.T) {
	if got := htmlAttr(`a&b<c>d"e'f`); got != "a&amp;b&lt;c&gt;d&quot;e&#39;f" {
		t.Fatalf("htmlAttr = %q", got)
	}
	if got := htmlAttr("plain"); got != "plain" {
		t.Fatalf("htmlAttr(plain) = %q", got)
	}
	if got := urlQueryEscape("a b#c&d?e+f"); got != "a%20b%23c%26d%3Fe%2Bf" {
		t.Fatalf("urlQueryEscape = %q", got)
	}
}

func TestHiveOfWeekPayloadDefaultsAndEscaping(t *testing.T) {
	week := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/hive-of-week", nil)
	resp := hiveOfWeekPayload("", week, 3, req)
	if resp.Project != hiveGourceDefaultProject || resp.Week != "2026-W39" || resp.ActivityCount != 3 {
		t.Fatalf("defaults = %+v", resp)
	}
	if resp.VideoURL != defaultHiveOfWeekVideoURL || resp.PosterURL != defaultHiveOfWeekPosterURL {
		t.Fatalf("media defaults = %+v", resp)
	}
	if resp.GourceLogURL != "/api/leaderboard/gource-log?project=hive&week=2026-W39" {
		t.Fatalf("gource log url = %q", resp.GourceLogURL)
	}
	if !strings.Contains(resp.ReadmeMarkdown, resp.GourceLogURL) || !strings.Contains(resp.ReadmeMarkdown, "[Leaderboard](/contribute/leaderboard)") {
		t.Fatalf("readme = %q", resp.ReadmeMarkdown)
	}

	req = httptest.NewRequest(http.MethodGet, `/api/leaderboard/hive-of-week?video=https://cdn.example/v.mp4"><script>&poster=/p.jpg`, nil)
	resp = hiveOfWeekPayload("acme/widgets", week, 1, req)
	if resp.VideoURL != `https://cdn.example/v.mp4"><script>` || resp.PosterURL != "/p.jpg" {
		t.Fatalf("overrides = %+v", resp)
	}
	if strings.Contains(resp.EmbedHTML, "<script>") || !strings.Contains(resp.EmbedHTML, "&quot;&gt;&lt;script&gt;") {
		t.Fatalf("embed html must escape attribute values: %s", resp.EmbedHTML)
	}
	if !strings.Contains(resp.EmbedHTML, `poster="/p.jpg"`) || !strings.Contains(resp.GourceLogURL, "project=acme/widgets") {
		t.Fatalf("embed html = %s; log url = %s", resp.EmbedHTML, resp.GourceLogURL)
	}
}

func TestValidPublicGourceProject(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "Acme/Widgets")
	cases := map[string]bool{
		hiveGourceDefaultProject: true,
		"acme/widgets":           true,
		"acme/other":             false,
		"":                       false,
		"../etc":                 false,
		"acme/wid gets":          false,
	}
	for project, want := range cases {
		if got := validPublicGourceProject(project); got != want {
			t.Fatalf("validPublicGourceProject(%q) = %v, want %v", project, got, want)
		}
	}
}

func TestGourceMappingAndLeafFallbacks(t *testing.T) {
	for _, tc := range []struct {
		action string
		typ    string
		area   string
	}{
		{"completed", "M", "merged-prs"},
		{"Picked Up", "A", "work-in-flight"},
		{"joined", "A", "contributors"},
		{"failed", "D", "setbacks"},
		{"released lease", "D", "setbacks"},
		{"promoted", "A", "achievements"},
		{"achievement: first", "A", "achievements"},
		{"something else", "M", "activity"},
	} {
		typ, area, colour := gourceMapping(tc.action)
		if typ != tc.typ || area != tc.area || colour == "" {
			t.Fatalf("gourceMapping(%q) = %q/%q/%q", tc.action, typ, area, colour)
		}
	}
	if got := gourceLeaf(ActivityEntry{Task: " task "}); got != " task " {
		t.Fatalf("leaf with task = %q", got)
	}
	if got := gourceLeaf(ActivityEntry{Username: "bob"}); got != "bob" {
		t.Fatalf("leaf with user = %q", got)
	}
	if got := gourceLeaf(ActivityEntry{}); got != "event" {
		t.Fatalf("leaf fallback = %q", got)
	}
}

func TestSanitizeGourceUserAndPathParts(t *testing.T) {
	if got := sanitizeGourceUser(""); got != battleLogContributorSubject {
		t.Fatalf("empty user = %q", got)
	}
	got := sanitizeGourceUser("a|b token=abc " + strings.Repeat("x", 80))
	if strings.Contains(got, "|") || strings.Contains(got, "abc") || len([]rune(got)) != 60 {
		t.Fatalf("sanitized user = %q", got)
	}
	if got := sanitizeGourcePathPart("see #42 and more"); got != "issue-42" {
		t.Fatalf("issue ref path = %q", got)
	}
	if got := sanitizeGourcePathPart("../|.."); got != "event" {
		t.Fatalf("empty-after-scrub path = %q", got)
	}
	if got := sanitizeGourcePathPart(strings.Repeat("a", 130)); len([]rune(got)) != 120 {
		t.Fatalf("long path part len = %d", len([]rune(got)))
	}
	if got := sanitizeGourceProjectPath("|..//"); got != hiveGourceDefaultProject {
		t.Fatalf("empty project path = %q", got)
	}
	if got := sanitizeGourceProjectPath("acme/wid gets"); got != "acme/wid-gets" {
		t.Fatalf("project path = %q", got)
	}
}

func TestBoundedQueryIntClampsAndIgnoresGarbage(t *testing.T) {
	for raw, want := range map[string]int{"": 30, "abc": 30, "-5": 30, "0": 30, "7": 7, "500": 100} {
		req := httptest.NewRequest(http.MethodGet, "/x?limit="+raw, nil)
		if got := boundedQueryInt(req, "limit", 30, 100); got != want {
			t.Fatalf("boundedQueryInt(%q) = %d, want %d", raw, got, want)
		}
	}
}

func seedBattleLogActivity(t *testing.T, s *Server, entries ...ActivityEntry) {
	t.Helper()
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.activity = append(s.contributeHub.activity, entries...)
}

func TestHiveOfWeekAPIRouteReportsBusiestPublicProject(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "acme/widgets")
	s, _ := apiServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	seedBattleLogActivity(t, s,
		ActivityEntry{Timestamp: now, Username: "alice", Action: "completed", Task: "acme/widgets#1"},
		ActivityEntry{Timestamp: now, Username: "bob", Action: "picked up", Task: "acme/widgets#2"},
		ActivityEntry{Timestamp: now, Username: "carol", Action: "completed", Task: "acme/private#3"},
	)
	rec := doGet(s, "/api/leaderboard/hive-of-week?poster=/custom.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hive-of-week = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp hiveOfWeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if resp.Project != "acme/widgets" || resp.ActivityCount != 2 || resp.PosterURL != "/custom.jpg" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Week != formatHiveWeek(weekStartUTC(time.Now().UTC())) {
		t.Fatalf("week = %q", resp.Week)
	}
}

func TestHiveOfWeekAPIRouteFallsBackToHiveWithoutActivity(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/leaderboard/hive-of-week")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hive-of-week = %d", rec.Code)
	}
	var resp hiveOfWeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if resp.Project != hiveGourceDefaultProject || resp.ActivityCount != 0 || resp.VideoURL != defaultHiveOfWeekVideoURL {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestGourceLogRouteValidatesProjectAndStreamsPlainText(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "acme/widgets")
	s, _ := apiServer(t)
	stamp := weekStartUTC(time.Now().UTC()).Add(26 * time.Hour).Format(time.RFC3339)
	seedBattleLogActivity(t, s,
		ActivityEntry{Timestamp: stamp, Username: "alice", Action: "completed", Task: "acme/widgets#7"},
		ActivityEntry{Timestamp: stamp, Username: "bob", Action: "completed", Task: "acme/private#8"},
	)

	rec := doGet(s, "/api/leaderboard/gource-log?project=acme/private")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-public project = %d, want 400", rec.Code)
	}
	rec = doGet(s, "/api/leaderboard/gource-log?project=../etc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal project = %d, want 400", rec.Code)
	}

	rec = doGet(s, "/api/leaderboard/gource-log")
	if rec.Code != http.StatusOK {
		t.Fatalf("busiest-project log = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != hiveGourceLogContentType {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n") || strings.Count(body, "\n") != 1 {
		t.Fatalf("expected one newline-terminated line, got %q", body)
	}
	if !strings.Contains(body, "|alice|M|acme/widgets/merged-prs/issue-7|") || strings.Contains(body, "private") {
		t.Fatalf("gource line = %q", body)
	}

	rec = doGet(s, "/api/leaderboard/gource-log?project=acme/widgets&week=2020-W02")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("other week = %d body=%q, want empty 200", rec.Code, rec.Body.String())
	}
}

func TestHiveOfWeekArchiveRejectsInvalidProjectForOwner(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/leaderboard/hive-of-week/archive?project=../etc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid project = %d, want 400", rec.Code)
	}
}

func TestHiveOfWeekArchiveWithoutKnowledgeStoreIs503(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/leaderboard/hive-of-week/archive", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("archive without knowledge = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHiveOfWeekArchiveWritesGourceFactToVault(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "acme/widgets")
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/leaderboard/hive-of-week/archive?project=acme/widgets", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no writable file vault") {
		t.Fatalf("archive without vault = %d body=%s", rec.Code, rec.Body.String())
	}

	vault := t.TempDir()
	if err := s.deps.Knowledge.ConnectVault(vault, "project"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	stamp := time.Date(2026, time.September, 22, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	seedBattleLogActivity(t, s, ActivityEntry{Timestamp: stamp, Username: "alice", Action: "completed", Task: "acme/widgets#7"})

	rec = doPost(s, "/api/leaderboard/hive-of-week/archive?project=acme/widgets&week=2026-W39", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Project string `json:"project"`
		Week    string `json:"week"`
		Lines   int    `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !resp.OK || resp.Project != "acme/widgets" || resp.Week != "2026-W39" || resp.Lines != 1 {
		t.Fatalf("resp = %+v", resp)
	}

	matches, _ := filepath.Glob(filepath.Join(vault, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("vault files = %v, want exactly one archived fact", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{"title: Hive of the Week gource source acme/widgets 2026-W39", hiveOfWeekKnowledgeTag, "```gource", "|alice|M|acme/widgets/merged-prs/issue-7|"} {
		if !strings.Contains(content, want) {
			t.Fatalf("archived fact missing %q:\n%s", want, content)
		}
	}
}

func TestArchiveHiveOfWeekLogNilServer(t *testing.T) {
	var s *Server
	if err := s.archiveHiveOfWeekLog("hive", time.Now(), ""); err == nil {
		t.Fatal("nil server should report unconfigured knowledge store")
	}
}

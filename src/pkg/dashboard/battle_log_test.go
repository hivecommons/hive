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
	}}, 10)
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
	lines := gourceLogForProject(activity, "hivecommons/hive", week)
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
	project, count := busiestProject(activity, now)
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
	if lines := gourceLogForProject(activity, hiveGourceDefaultProject, week); len(lines) != 0 {
		t.Fatalf("non-allowlisted activity leaked into hive gource log: %#v", lines)
	}
	project, count := busiestProject(activity, week.Add(time.Hour))
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

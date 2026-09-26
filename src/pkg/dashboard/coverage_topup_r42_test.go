package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestDiscardingWriteRecorder(t *testing.T) {
	r := newDiscardingWriteRecorder()
	r.Header().Set("X-Test", "1")
	if r.Header().Get("X-Test") != "1" {
		t.Fatal("header not retained")
	}
	if r.statusCode() != http.StatusOK {
		t.Fatalf("default status = %d", r.statusCode())
	}
	n, err := r.Write([]byte("ok-body"))
	if err != nil || n != 7 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if r.statusCode() != http.StatusOK || r.bytesWritten() != 7 || len(r.bodyBytes()) != 0 {
		t.Fatalf("2xx body must be discarded: code=%d bytes=%d body=%q", r.statusCode(), r.bytesWritten(), r.bodyBytes())
	}

	e := newDiscardingWriteRecorder()
	e.WriteHeader(http.StatusBadGateway)
	e.WriteHeader(http.StatusOK) // first status wins
	_, _ = e.Write([]byte("boom"))
	if e.statusCode() != http.StatusBadGateway || string(e.bodyBytes()) != "boom" || e.bytesWritten() != 4 {
		t.Fatalf("error body must be kept: code=%d body=%q bytes=%d", e.statusCode(), e.bodyBytes(), e.bytesWritten())
	}
}

func TestAdminMCPWritesEnabledParsesEnv(t *testing.T) {
	for raw, want := range map[string]bool{"1": true, " TRUE ": true, "yes": true, "On": true, "": false, "0": false, "nope": false} {
		t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", raw)
		if got := adminMCPWritesEnabled(); got != want {
			t.Errorf("%q: got %v want %v", raw, got, want)
		}
	}
}

func TestCampaignArchiveErrorStatus(t *testing.T) {
	cases := map[error]int{
		knowledge.ErrCampaignLeaseHeld:   http.StatusConflict,
		knowledge.ErrCampaignNoLease:     http.StatusConflict,
		errors.New("campaign not found"): http.StatusNotFound,
		errors.New("disk on fire"):       http.StatusInternalServerError,
	}
	for err, want := range cases {
		if got := campaignArchiveErrorStatus(err); got != want {
			t.Errorf("%v: got %d want %d", err, got, want)
		}
	}
}

func TestRunCheckpointStatus(t *testing.T) {
	cases := map[error]int{
		errRunCheckpointNotHeld:            http.StatusConflict,
		errors.New("checkpoint not found"): http.StatusNotFound,
		errors.New("store offline"):        http.StatusServiceUnavailable,
	}
	for err, want := range cases {
		if got := runCheckpointStatus(err); got != want {
			t.Errorf("%v: got %d want %d", err, got, want)
		}
	}
}

func TestRunAuditAndDetailKeyHelpers(t *testing.T) {
	if got := repoFromRunAuditKey("owner/repo#12"); got != "owner/repo" {
		t.Errorf("worksource key repo = %q", got)
	}
	if got := repoFromRunAuditKey("owner/repo!abc"); got != "owner/repo" {
		t.Errorf("bang key repo = %q", got)
	}
	if got := repoFromRunAuditKey("garbage"); got != "" {
		t.Errorf("garbage repo = %q", got)
	}
	if got := runIssueNumber("owner/repo#12"); got != 12 {
		t.Errorf("worksource issue = %d", got)
	}
	if got := runIssueNumber("weird#7"); got != 7 {
		t.Errorf("fallback issue = %d", got)
	}
	if got := runIssueNumber("nohash"); got != 0 {
		t.Errorf("nohash issue = %d", got)
	}
	if _, ok := parseRunAuditTime("not-a-time"); ok {
		t.Error("bad time parsed")
	}
	if ts, ok := parseRunAuditTime(" 2026-01-02T03:04:05Z "); !ok || ts.Year() != 2026 {
		t.Errorf("good time: %v %v", ts, ok)
	}
	if !oldestRunAuditTimelineAt(nil).IsZero() {
		t.Error("nil store must yield zero time")
	}
	if githubIssueURL("", 1) != "" || githubIssueURL("o/r", 0) != "" || githubIssueURL("o/r", 3) != "https://github.com/o/r/issues/3" {
		t.Error("githubIssueURL")
	}
	if githubRepoURL("") != "" || githubRepoURL("o/r") != "https://github.com/o/r" {
		t.Error("githubRepoURL")
	}
	m := map[string]any{"b": 2, "c": 3}
	if firstAny(m, "a", "b", "c") != 2 || firstAny(m, "z") != nil {
		t.Error("firstAny")
	}
	for raw, want := range map[any]int{1: 1, int64(2): 2, 3.0: 3, json.Number("4"): 4, "5": 5, "x": 0, true: 0} {
		if got := runDetailIntFromAny(raw); got != want {
			t.Errorf("runDetailIntFromAny(%v) = %d want %d", raw, got, want)
		}
	}
	if runDetailStringFromAny("  hi ") != "hi" || runDetailStringFromAny(4) != "" {
		t.Error("runDetailStringFromAny")
	}
	if stringMapAny(nil) != nil || stringMapAny(map[string]string{"a": "b"})["a"] != "b" {
		t.Error("stringMapAny")
	}
	var w errReviewerAccuracyWindow
	if !strings.Contains(w.Error(), "days must be an integer") {
		t.Errorf("window error = %q", w.Error())
	}
}

func TestBattleLogClassificationAndGourceHelpers(t *testing.T) {
	cases := []struct {
		action string
		entry  ActivityEntry
		kind   string
	}{
		{"completed", ActivityEntry{}, "merged_pr"},
		{"picked up", ActivityEntry{CLI: "ollama", Model: "Local-7b"}, "local_model_work"},
		{"picked up", ActivityEntry{Model: "gpt"}, "review"},
		{"joined", ActivityEntry{}, "joined_hive"},
		{"promoted", ActivityEntry{}, "achievement_unlocked"},
		{"achievement unlocked", ActivityEntry{}, "achievement_unlocked"},
		{"spec approved", ActivityEntry{}, "spec_approved"},
		{"swarm join", ActivityEntry{}, "swarm_joined"},
		{"mystery", ActivityEntry{}, ""},
	}
	for _, tc := range cases {
		kind, icon, verb := classifyBattleLogAction(tc.action, tc.entry)
		if kind != tc.kind {
			t.Errorf("%s: kind %q want %q", tc.action, kind, tc.kind)
		}
		if (kind == "") != (icon == "" && verb == "") {
			t.Errorf("%s: icon/verb mismatch %q %q", tc.action, icon, verb)
		}
	}

	if got := battleLogTarget(ActivityEntry{}, "joined_hive"); got != battleLogJoinTarget {
		t.Errorf("join target = %q", got)
	}
	if got := battleLogTarget(ActivityEntry{}, "achievement_unlocked"); got != "achievement" {
		t.Errorf("achievement target = %q", got)
	}
	if got := battleLogTarget(ActivityEntry{Task: "  "}, "review"); got != battleLogGenericWorkTarget {
		t.Errorf("empty task target = %q", got)
	}
	if got := battleLogTarget(ActivityEntry{Task: "owner/repo#12"}, "review"); strings.Contains(got, "owner/repo") {
		t.Errorf("repo-ref task must be scrubbed, got %q", got)
	}
	if got := battleLogTarget(ActivityEntry{Task: "fix the tests"}, "review"); got != "fix the tests" {
		t.Errorf("plain task = %q", got)
	}
	long := strings.Repeat("x", 130)
	if got := scrubBattleLogText(long); len([]rune(got)) != 121 || !strings.HasSuffix(got, "…") {
		t.Errorf("long text not truncated: %d", len([]rune(got)))
	}
	if got := scrubBattleLogText("mail me@example.com at https://x.y/z\x01"); strings.Contains(got, "example.com") || strings.Contains(got, "https://") || strings.Contains(got, "\x01") {
		t.Errorf("scrub left sensitive content: %q", got)
	}

	for action, want := range map[string]string{
		"completed": "merged-prs", "Picked Up": "work-in-flight", "joined": "contributors",
		"failed": "setbacks", "released lease": "setbacks", "promoted": "achievements",
		"achievement x": "achievements", "other": "activity",
	} {
		typ, area, colour := gourceMapping(action)
		if area != want || typ == "" || len(colour) != 6 {
			t.Errorf("gourceMapping(%q) = %q %q %q", action, typ, area, colour)
		}
	}
	if gourceLeaf(ActivityEntry{Task: "t"}) != "t" || gourceLeaf(ActivityEntry{Username: "u"}) != "u" || gourceLeaf(ActivityEntry{}) != "event" {
		t.Error("gourceLeaf")
	}

	now := time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC) // Saturday
	monday := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	if got := parseWeekQuery("", now); !got.Equal(monday) {
		t.Errorf("empty week = %v", got)
	}
	if got := parseWeekQuery("2026-09-23", now); !got.Equal(monday) {
		t.Errorf("date week = %v", got)
	}
	if got := parseWeekQuery("2026-W39", now); !got.Equal(monday) {
		t.Errorf("iso week = %v", got)
	}
	if got := parseWeekQuery("2026-W99", now); !got.Equal(monday) {
		t.Errorf("bad iso week must fall back, got %v", got)
	}
	if got := parseWeekQuery("garbage", now); !got.Equal(monday) {
		t.Errorf("garbage week = %v", got)
	}
	if got := formatHiveWeek(monday); got != "2026-W39" {
		t.Errorf("formatHiveWeek = %q", got)
	}

	t.Setenv("HIVE_PUBLIC_REPOS", "Org/Public, other/x")
	if !validPublicGourceProject(hiveGourceDefaultProject) {
		t.Error("default project must be valid")
	}
	if !validPublicGourceProject("org/public") || validPublicGourceProject("org/private") || validPublicGourceProject("") {
		t.Error("validPublicGourceProject allowlist")
	}
	if got := publicRepoFromTask("work on org/public#4 now"); got != "org/public" {
		t.Errorf("publicRepoFromTask = %q", got)
	}
	if got := publicRepoFromTask("work on org/private#4"); got != "" {
		t.Errorf("private repo leaked: %q", got)
	}

	if got := htmlAttr(`a&b<c>d"e'f`); got != "a&amp;b&lt;c&gt;d&quot;e&#39;f" {
		t.Errorf("htmlAttr = %q", got)
	}
	if got := urlQueryEscape("a b#c&d?e+f"); got != "a%20b%23c%26d%3Fe%2Bf" {
		t.Errorf("urlQueryEscape = %q", got)
	}
}

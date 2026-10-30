package dashboard

import (
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A merge the hive makes is audited twice: by its merge path as owner/repo,
// and by the PR poller as the repo is configured (bare in a single-org hive),
// at GitHub's merged_at so it sorts first. Both writers' entries for one
// merge must count once, under the merge path that made it.
func prThroughputBothWritersEntries(now time.Time) []AuditEntry {
	merged := now.Add(-2 * time.Hour)
	return []AuditEntry{
		// The hive's merge of x#821: observed (bare, path=hive) at merged_at,
		// then the sweep's own record seconds later.
		{Timestamp: rfc3339(merged), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=x, number=821, path=hive, actor=hive-app[bot]", Agent: "governor", Repo: "x", Target: 821},
		{Timestamp: rfc3339(merged.Add(5 * time.Second)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=acme/x, number=821, method=squash, path=sweep", Agent: "governor", Repo: "acme/x", Target: 821},
		// A human merge and a Renovate merge, observed only.
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=x, number=822, path=human, actor=alice", Agent: "governor", Repo: "x", Target: 822},
		{Timestamp: rfc3339(now.Add(-4 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=y, number=5, path=other_automation, actor=renovate[bot]", Agent: "governor", Repo: "y", Target: 5},
		// An older entry with the repo only in the detail, observed again.
		{Timestamp: rfc3339(now.Add(-5 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=acme/y, number=6, path=relay"},
		{Timestamp: rfc3339(now.Add(-5*time.Hour - time.Minute)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=y, number=6, path=hive, actor=hive-app[bot]"},
	}
}

func acmeQualifier(repo string) string { return config.QualifyRepo("acme", repo) }

func sumPaths(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func TestPRThroughputBothWritersCountOneMerge(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 40, 0, 0, time.UTC)
	entries := qualifyPRThroughputEntries(prThroughputBothWritersEntries(now), acmeQualifier)
	got := buildPRThroughputWindow(entries, now, 12, "", prThroughputRoleMerged)

	if got.Merged != 4 {
		t.Fatalf("merged = %d, want 4 (x#821, x#822, y#5, y#6); by path %v", got.Merged, got.MergedByPath)
	}
	want := map[string]int{"sweep": 1, "relay": 1, "human": 1, "other_automation": 1}
	for path, n := range want {
		if got.MergedByPath[path] != n {
			t.Errorf("merged_by_path[%s] = %d, want %d (all %v)", path, got.MergedByPath[path], n, got.MergedByPath)
		}
	}
	if got.MergedByPath["hive"] != 0 {
		t.Errorf("merged_by_path[hive] = %d, want 0: the observed copy of a swept merge is not a second merge", got.MergedByPath["hive"])
	}
	if s := sumPaths(got.MergedByPath); s != got.Merged {
		t.Errorf("sum(merged_by_path) = %d, merged = %d", s, got.Merged)
	}

	merges := got.ByActor[prThroughputKindPR][prThroughputRoleMerged]
	if merges[prThroughputActorHive] != 2 || merges[prThroughputActorHuman] != 1 || merges[prThroughputActorOtherAutomation] != 1 {
		t.Errorf("by_actor merged = %v, want hive=2 human=1 other=1", merges)
	}
	var hive, human, other int
	for _, p := range got.Series {
		hive, human, other = hive+p.Hive, human+p.Human, other+p.Other
	}
	if hive != 2 || human != 1 || other != 1 {
		t.Errorf("series totals hive=%d human=%d other=%d, want 2/1/1", hive, human, other)
	}
	bars := 0
	for _, b := range got.Buckets {
		bars += b.Merged
	}
	if bars != got.Merged {
		t.Errorf("hourly bars merged = %d, want %d", bars, got.Merged)
	}

	filtered := buildPRThroughputWindow(entries, now, 12, "acme/x", prThroughputRoleMerged)
	if filtered.Merged != 2 || filtered.MergedByPath["sweep"] != 1 || filtered.MergedByPath["human"] != 1 {
		t.Errorf("acme/x filter = merged %d by path %v, want 2 (sweep 1, human 1)", filtered.Merged, filtered.MergedByPath)
	}
}

// The previous window must not count a merge whose observed copy falls in it
// while the merge path's own entry, the one counted, falls in the current one.
func TestPRThroughputBothWritersSpanningWindowsCountOnce(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	entries := qualifyPRThroughputEntries([]AuditEntry{
		{Timestamp: rfc3339(since.Add(-time.Second)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=x, number=9, path=hive", Repo: "x", Target: 9},
		{Timestamp: rfc3339(since.Add(time.Second)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=acme/x, number=9, path=sweep", Repo: "acme/x", Target: 9},
	}, acmeQualifier)
	got := buildPRThroughputWindow(entries, now, 1, "", prThroughputRoleMerged)
	if got.Merged != 1 || got.MergedByPath["sweep"] != 1 {
		t.Fatalf("current window = merged %d by path %v, want one sweep merge", got.Merged, got.MergedByPath)
	}
	if got.PreviousActor != nil {
		t.Errorf("previous_actor = %+v, want none: the merge is counted in the current window", got.PreviousActor)
	}
}

func TestPRThroughputMergedByPathSumsToMerged(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 40, 0, 0, time.UTC)
	entries := qualifyPRThroughputEntries(append(prThroughputBothWritersEntries(now), prThroughputFixtureEntries(now)...), acmeQualifier)
	for _, hours := range []int{1, 3, 6, 12, 24, 168} {
		got := buildPRThroughputWindow(entries, now, hours, "", prThroughputRoleMerged)
		if s := sumPaths(got.MergedByPath); s != got.Merged {
			t.Errorf("hours=%d: sum(merged_by_path) = %d, merged = %d (%v)", hours, s, got.Merged, got.MergedByPath)
		}
	}
}

// The first terminal entry still wins otherwise: an observed merge after a
// sweep merge, or a close before an observed merge, adds nothing.
func TestPRThroughputTerminalPicks(t *testing.T) {
	at := func(m int) string { return rfc3339(time.Date(2026, 10, 30, 10, m, 0, 0, time.UTC)) }
	sorted := []AuditEntry{
		{Timestamp: at(0), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=1, path=hive", Target: 1, Repo: "o/r"},
		{Timestamp: at(1), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=1, path=queue", Target: 1, Repo: "o/r"},
		{Timestamp: at(2), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=2, path=sweep", Target: 2, Repo: "o/r"},
		{Timestamp: at(3), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=2, path=relay", Target: 2, Repo: "o/r"},
		{Timestamp: at(4), Action: ghpkg.AuditActionPRClosed, Detail: "repo=o/r, number=3, path=human", Target: 3, Repo: "o/r"},
		{Timestamp: at(5), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=3, path=sweep", Target: 3, Repo: "o/r"},
		{Timestamp: "not a time", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=4, path=sweep", Target: 4, Repo: "o/r"},
		{Timestamp: at(6), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=4, path=human", Target: 4, Repo: "o/r"},
	}
	picks := newPRThroughputTerminalPicks(sorted)
	want := map[string]int{"o/r#1": 1, "o/r#2": 2, "o/r#3": 4, "o/r#4": 7}
	for key, i := range want {
		if picks[key] != i {
			t.Errorf("pick[%s] = %d, want %d", key, picks[key], i)
		}
	}
	if !picks.counts(0, AuditEntry{Action: ghpkg.AuditActionAgentPRCreated}, "o/r#1") {
		t.Error("a non-terminal entry must always count")
	}
	if !picks.counts(9, AuditEntry{Action: ghpkg.AuditActionPRMerged}, "") {
		t.Error("a terminal entry without a PR key must count")
	}
}

func TestQualifyPRThroughputEntries(t *testing.T) {
	in := []AuditEntry{
		{Repo: "x"},
		{Repo: "other/x"},
		{Detail: "repo=y, number=1"},
		{Detail: "no repo"},
	}
	got := qualifyPRThroughputEntries(in, acmeQualifier)
	wantRepo := []string{"acme/x", "other/x", "acme/y", ""}
	for i, w := range wantRepo {
		if got[i].Repo != w {
			t.Errorf("entry %d repo = %q, want %q", i, got[i].Repo, w)
		}
	}
	if in[0].Repo != "x" {
		t.Error("the caller's entries must not be modified")
	}
	if same := qualifyPRThroughputEntries(in, nil); len(same) != len(in) || same[0].Repo != "x" {
		t.Errorf("nil qualifier = %+v, want entries unchanged", same)
	}
}

// The handler qualifies with the hive's org, so the card counts the merge once.
func TestHandlePRThroughputQualifiesWithOrg(t *testing.T) {
	now := time.Now().UTC()
	auditPath := writeAuditFixture(t, t.TempDir(), prThroughputBothWritersEntries(now))
	s := prThroughputServer(t, auditPath, &AuditLog{})
	s.deps = &Dependencies{Config: &config.Config{Project: config.ProjectConfig{Org: "acme"}}}

	code, body := getPRThroughput(t, s, "?hours=12")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if body.Merged != 4 || body.MergedByPath["hive"] != 0 || body.MergedByPath["sweep"] != 1 {
		t.Fatalf("merged = %d by path %v, want 4 with the swept merge once under sweep", body.Merged, body.MergedByPath)
	}
	if s := sumPaths(body.MergedByPath); s != body.Merged {
		t.Errorf("sum(merged_by_path) = %d, merged = %d", s, body.Merged)
	}
	code, body = getPRThroughput(t, s, "?hours=12&repo=acme/x")
	if code != 200 || body.Merged != 2 {
		t.Errorf("acme/x: status %d merged %d, want 2", code, body.Merged)
	}
}

// "Merged by repo" counts merges only, once each, not every PR action.
func TestPRThroughputTopReposCountsMerges(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 40, 0, 0, time.UTC)
	entries := prThroughputBothWritersEntries(now)
	for n := 1; n <= 5; n++ {
		entries = append(entries, AuditEntry{Timestamp: rfc3339(now.Add(-time.Hour)), User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=acme/z, number=" + strconv.Itoa(n), Repo: "acme/z", Target: n})
	}
	got := buildPRThroughputWindow(qualifyPRThroughputEntries(entries, acmeQualifier), now, 12, "", prThroughputRoleMerged)
	want := map[string]int{"acme/x": 2, "acme/y": 2}
	if len(got.TopRepos) != len(want) {
		t.Fatalf("top_repos = %+v, want %v", got.TopRepos, want)
	}
	for _, r := range got.TopRepos {
		if want[r.Name] != r.Count {
			t.Errorf("top_repos %s = %d, want %d (all %+v)", r.Name, r.Count, want[r.Name], got.TopRepos)
		}
	}
}

// The widget's 7-day throughput is the same de-duplicated count.
func TestHandleWidgetThroughputCountsHiveMergeOnce(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	now := time.Now().UTC()
	auditPath := writeAuditFixture(t, t.TempDir(), prThroughputBothWritersEntries(now))
	prev := prThroughputAuditPath
	prThroughputAuditPath = auditPath
	t.Cleanup(func() { prThroughputAuditPath = prev })
	s.audit = &AuditLog{}

	result := decodeJSON(t, doGet(s, "/api/widget"))
	tp, _ := result["prThroughput7d"].(map[string]any)
	if merged, _ := tp["merged"].(float64); merged != 4 {
		t.Fatalf("widget prThroughput7d = %v, want merged 4", tp)
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Fixed instants for the ledger tests: a Tuesday afternoon in UTC, which is
// still Monday evening in Los Angeles — so "today" differs by zone.
var (
	mergedTestNow = time.Date(2026, 10, 27, 2, 30, 0, 0, time.UTC)
	mergedTestLA  = mustLoadLocation("America/Los_Angeles")
)

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func mergedEntry(repo string, number int, at time.Time) MergedLedgerEntry {
	return MergedLedgerEntry{Repo: repo, Number: number, MergedAt: at}
}

// isolateMergedLedger points the server's ledger at a file under a temp
// dir for the test's lifetime.
func isolateMergedLedger(t *testing.T) string {
	t.Helper()
	old := mergedLedgerPath
	mergedLedgerPath = filepath.Join(t.TempDir(), "merged-ledger.json")
	t.Cleanup(func() { mergedLedgerPath = old })
	return mergedLedgerPath
}

// TestMergedLedgerPersistsAcrossReload: the file under the ledger path
// survives a new constructor (a roll), carrying the all-time total and the
// entries; a ledger that found a file reports Seeded so a boot never seeds
// twice.
func TestMergedLedgerPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged-ledger.json")
	l := newMergedLedger(path)
	if l.Seeded() {
		t.Fatal("a fresh ledger with no file must not report seeded")
	}
	if !l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 1, Title: "fix: one", MergedBy: MergedByHive, Lane: "hive"}, mergedTestNow) {
		t.Fatal("first record must be new")
	}
	if l.Record(mergedEntry("acme/console", 2, mergedTestNow.Add(-time.Hour)), mergedTestNow) != true {
		t.Fatal("second PR is new")
	}
	if l.Record(mergedEntry("acme/console", 1, mergedTestNow), mergedTestNow) {
		t.Fatal("the same PR again is not new")
	}

	again := newMergedLedger(path)
	if !again.Seeded() {
		t.Fatal("a ledger loaded from disk reports seeded")
	}
	sum := again.Summary(mergedTestNow, time.UTC)
	if sum.AllTime != 2 || again.Len() != 2 {
		t.Fatalf("after reload: all_time=%d len=%d", sum.AllTime, again.Len())
	}
	if len(sum.Recent) != 2 || sum.Recent[0].Number != 1 || sum.Recent[0].Title != "fix: one" || sum.Recent[0].MergedBy != MergedByHive || sum.Recent[0].Lane != "hive" {
		t.Fatalf("recent after reload: %+v", sum.Recent)
	}
	if sum.Recent[0].MergedAt.IsZero() {
		t.Fatal("a record with no merge time takes now")
	}
}

// TestMergedLedgerUpsertFillsBlanks: a second record of the same PR fills in
// what the first did not know (title, merger, lane, issue), never overwrites
// a known field, and the earliest merge time wins.
func TestMergedLedgerUpsertFillsBlanks(t *testing.T) {
	l := newMergedLedger("")
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 5, MergedAt: mergedTestNow.Add(-2 * time.Minute), MergedBy: "octocat"}, mergedTestNow)
	l.Record(MergedLedgerEntry{Repo: " acme/console ", Number: 5, MergedAt: mergedTestNow.Add(-10 * time.Minute), Title: "feat: five", MergedBy: MergedByHive, Lane: "contributor", Issue: 42}, mergedTestNow)
	sum := l.Summary(mergedTestNow, time.UTC)
	if sum.AllTime != 1 || len(sum.Recent) != 1 {
		t.Fatalf("one PR, two records: all_time=%d recent=%d", sum.AllTime, len(sum.Recent))
	}
	e := sum.Recent[0]
	if e.Title != "feat: five" || e.Lane != "contributor" || e.Issue != 42 {
		t.Fatalf("blanks not filled: %+v", e)
	}
	if e.MergedBy != "octocat" {
		t.Fatalf("a known merger must not be overwritten: %q", e.MergedBy)
	}
	if !e.MergedAt.Equal(mergedTestNow.Add(-10 * time.Minute)) {
		t.Fatalf("earliest merge time wins: %v", e.MergedAt)
	}
}

// TestMergedLedgerRetentionAndCap: entries older than the retention window
// are pruned, the oldest beyond the cap are dropped, and the all-time total
// is untouched by either.
func TestMergedLedgerRetentionAndCap(t *testing.T) {
	l := newMergedLedger("")
	l.Record(mergedEntry("acme/console", 1, mergedTestNow.Add(-mergedLedgerRetention-time.Hour)), mergedTestNow)
	l.Record(mergedEntry("acme/console", 2, mergedTestNow.Add(-29*24*time.Hour)), mergedTestNow)
	l.Record(mergedEntry("acme/console", 3, mergedTestNow.Add(-time.Hour)), mergedTestNow)
	sum := l.Summary(mergedTestNow, time.UTC)
	if sum.AllTime != 3 {
		t.Fatalf("all_time counts the pruned one: %d", sum.AllTime)
	}
	if l.Len() != 2 || len(sum.Recent) != 2 || sum.Recent[0].Number != 3 || sum.Recent[1].Number != 2 {
		t.Fatalf("retention: len=%d recent=%+v", l.Len(), sum.Recent)
	}
	if sum.Total30d != 2 || sum.Total7d != 1 {
		t.Fatalf("windows: 30d=%d 7d=%d", sum.Total30d, sum.Total7d)
	}

	capped := newMergedLedger("")
	for i := 1; i <= mergedLedgerMaxEntries+3; i++ {
		capped.Record(mergedEntry("acme/console", i, mergedTestNow.Add(-time.Duration(mergedLedgerMaxEntries+3-i)*time.Second)), mergedTestNow)
	}
	if capped.Len() != mergedLedgerMaxEntries {
		t.Fatalf("cap: len=%d", capped.Len())
	}
	capSum := capped.Summary(mergedTestNow, time.UTC)
	if capSum.AllTime != mergedLedgerMaxEntries+3 {
		t.Fatalf("cap leaves all_time alone: %d", capSum.AllTime)
	}
	if len(capSum.Recent) != mergedLedgerRecentMax || capSum.Recent[0].Number != mergedLedgerMaxEntries+3 {
		t.Fatalf("recent is the newest %d: len=%d first=%d", mergedLedgerRecentMax, len(capSum.Recent), capSum.Recent[0].Number)
	}
}

// TestMergedLedgerSummaryTodayRollsInZone: "today" starts at midnight in
// the given zone — the same instants count differently in UTC and in Los
// Angeles — and the day_start / tz fields say which day the number is for.
// nil means UTC. Entries stamped in the future are left out.
func TestMergedLedgerSummaryTodayRollsInZone(t *testing.T) {
	l := newMergedLedger("")
	// 2026-10-27 01:00Z: today in UTC, still Monday the 26th in LA.
	l.Record(mergedEntry("acme/console", 1, mergedTestNow.Add(-90*time.Minute)), mergedTestNow)
	// 2026-10-26 23:00Z: yesterday in UTC, Monday afternoon in LA.
	l.Record(mergedEntry("acme/console", 2, mergedTestNow.Add(-210*time.Minute)), mergedTestNow)
	// 2026-10-26 06:00Z: Sunday night in LA.
	l.Record(mergedEntry("acme/console", 3, mergedTestNow.Add(-20*time.Hour-30*time.Minute)), mergedTestNow)
	// Stamped an hour ahead: not counted anywhere.
	l.Record(mergedEntry("acme/console", 4, mergedTestNow.Add(time.Hour)), mergedTestNow)

	utc := l.Summary(mergedTestNow, nil)
	if utc.TZ != "UTC" || !utc.DayStart.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("utc zone: tz=%q day_start=%v", utc.TZ, utc.DayStart)
	}
	if utc.Today != 1 || utc.Total7d != 3 || utc.Total30d != 3 || utc.AllTime != 4 {
		t.Fatalf("utc: %+v", utc)
	}
	if len(utc.Recent) != 3 || utc.Recent[0].Number != 1 {
		t.Fatalf("utc recent (newest first, no future): %+v", utc.Recent)
	}

	la := l.Summary(mergedTestNow, mergedTestLA)
	if la.TZ != "America/Los_Angeles" || !la.DayStart.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, mergedTestLA)) {
		t.Fatalf("la zone: tz=%q day_start=%v", la.TZ, la.DayStart)
	}
	if la.Today != 2 {
		t.Fatalf("la today: %d (Monday the 26th has #1 and #2)", la.Today)
	}

	// Midnight rolls the count: one second past LA midnight on the 27th,
	// Monday's merges are yesterday's.
	rolled := time.Date(2026, 10, 27, 0, 0, 1, 0, mergedTestLA)
	l.Record(mergedEntry("acme/console", 9, rolled.Add(-time.Second)), rolled)
	after := l.Summary(rolled, mergedTestLA)
	if after.Today != 1 || after.Recent[0].Number != 9 {
		t.Fatalf("after midnight: today=%d first=%d", after.Today, after.Recent[0].Number)
	}
}

// TestMergedLedgerSeedFloorAndOnce: the first boot folds history in,
// deduplicated, with the throughput counter as the all-time floor; the
// ledger then reports seeded and a later seed is not needed.
func TestMergedLedgerSeedFloorAndOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merged-ledger.json")
	l := newMergedLedger(path)
	l.Seed([]MergedLedgerEntry{
		mergedEntry("acme/console", 1, mergedTestNow.Add(-time.Hour)),
		mergedEntry("acme/console", 1, mergedTestNow.Add(-time.Hour)),
		mergedEntry("acme/console", 2, mergedTestNow.Add(-2*time.Hour)),
		{Repo: "", Number: 3, MergedAt: mergedTestNow},
	}, 33, mergedTestNow)
	sum := l.Summary(mergedTestNow, time.UTC)
	if sum.AllTime != 33 || l.Len() != 2 {
		t.Fatalf("seed: all_time=%d (floor 33) len=%d", sum.AllTime, l.Len())
	}
	if !l.Seeded() {
		t.Fatal("seeded")
	}
	if !newMergedLedger(path).Seeded() {
		t.Fatal("the seed persisted")
	}
	// A seed smaller than what is already known keeps the larger count.
	l2 := newMergedLedger("")
	l2.Seed([]MergedLedgerEntry{mergedEntry("a/b", 1, mergedTestNow), mergedEntry("a/b", 2, mergedTestNow)}, 1, mergedTestNow)
	if got := l2.Summary(mergedTestNow, time.UTC).AllTime; got != 2 {
		t.Fatalf("floor below the entries: %d", got)
	}
}

// TestMergedEntryFromAudit: pr_merged audit lines project onto the ledger
// — the hive's own paths are MergedByHive, an observed merge names its
// actor, "unknown" is blank; short repo names are qualified; other actions
// and lines without a PR are rejected.
func TestMergedEntryFromAudit(t *testing.T) {
	qualify := func(r string) string {
		if r == "console" {
			return "acme/console"
		}
		return r
	}
	ts := mergedTestNow.Format(time.RFC3339)
	cases := []struct {
		name   string
		e      AuditEntry
		ok     bool
		repo   string
		number int
		by     string
	}{
		{"sweep", AuditEntry{Timestamp: ts, Action: github.AuditActionPRMerged, Repo: "console", Target: 7, Detail: "path=" + github.PRAuditPathSweep}, true, "acme/console", 7, MergedByHive},
		{"observed human", AuditEntry{Timestamp: ts, Action: github.AuditActionPRMerged, Repo: "acme/console", Target: 8, Detail: "path=observed, actor=octocat"}, true, "acme/console", 8, "octocat"},
		{"observed unknown", AuditEntry{Timestamp: ts, Action: github.AuditActionPRMerged, Repo: "acme/console", Target: 9, Detail: "path=observed, actor=unknown"}, true, "acme/console", 9, ""},
		{"legacy detail", AuditEntry{Timestamp: ts, Action: github.AuditActionPRMerged, Detail: "repo=console, number=10, path=queue"}, true, "acme/console", 10, MergedByHive},
		{"other action", AuditEntry{Timestamp: ts, Action: "pr_approved", Repo: "acme/console", Target: 11}, false, "", 0, ""},
		{"no pr", AuditEntry{Timestamp: ts, Action: github.AuditActionPRMerged, Repo: "acme/console"}, false, "", 0, ""},
		{"bad time", AuditEntry{Timestamp: "yesterday", Action: github.AuditActionPRMerged, Repo: "acme/console", Target: 12}, false, "", 0, ""},
	}
	for _, c := range cases {
		got, ok := mergedEntryFromAudit(c.e, qualify)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v", c.name, ok, c.ok)
		}
		if !ok {
			continue
		}
		if got.Repo != c.repo || got.Number != c.number || got.MergedBy != c.by || !got.MergedAt.Equal(mergedTestNow) {
			t.Fatalf("%s: %+v", c.name, got)
		}
	}
}

// TestMergedLedgerServerRecordsAuditAndSnapshots: on the server the audit
// log's pr_merged lines and webhook snapshots of merged PRs both land in
// the ledger (deduplicated), a non-merged or non-PR snapshot does not, and
// the tracker cache is dropped so the next /api/stage-tracker shows it.
func TestMergedLedgerServerRecordsAuditAndSnapshots(t *testing.T) {
	isolateMergedLedger(t)
	s := newTestServer()
	s.deps = testDeps(t)
	l := s.mergedLedger()
	if l == nil || l.Summary(mergedTestNow, nil).AllTime != 0 {
		t.Fatal("a fresh server starts an empty ledger")
	}

	s.audit.LogRecord("system", github.AuditActionPRMerged, "path="+github.PRAuditPathQueue+", number=21", "governor", "acme/console", 21)
	s.recordMergedSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 21, IsPR: true, Merged: true, Title: "fix: twenty-one", URL: "https://github.com/acme/console/pull/21", MergedAt: mergedTestNow, MergedBy: "hive-bot[bot]", MergedByHive: true},
		{Repo: "acme/console", Number: 22, IsPR: true, Merged: true, Title: "feat: twenty-two", MergedBy: "octocat", UpdatedAt: mergedTestNow},
		{Repo: "acme/console", Number: 23, IsPR: true, Merged: false, Title: "open"},
		{Repo: "acme/console", Number: 24, IsPR: false, Merged: true, Title: "an issue"},
	})
	sum := l.Summary(time.Now(), nil)
	if sum.AllTime != 2 || len(sum.Recent) != 2 {
		t.Fatalf("two merged PRs: all_time=%d recent=%+v", sum.AllTime, sum.Recent)
	}
	byNum := map[int]MergedLedgerEntry{}
	for _, e := range sum.Recent {
		byNum[e.Number] = e
	}
	if e := byNum[21]; e.Title != "fix: twenty-one" || e.URL == "" || e.MergedBy != MergedByHive {
		t.Fatalf("#21 audit then snapshot, blanks filled: %+v", e)
	}
	if e := byNum[22]; e.MergedBy != "octocat" || !e.MergedAt.Equal(mergedTestNow) {
		t.Fatalf("#22 human merge, updated_at as the merge time: %+v", e)
	}

	// Survives a roll: a new server over the same path sees the two and
	// does not seed on top of them.
	s2 := newTestServer()
	s2.deps = testDeps(t)
	if got := s2.mergedLedger().Summary(time.Now(), nil).AllTime; got != 2 {
		t.Fatalf("after roll: all_time=%d", got)
	}
}

// TestStageTrackerAPIMergedShape: /api/stage-tracker carries the merged
// block — today, total_7d, total_30d, all_time, recent (newest first, with
// the fields the chip's popover draws), tz and day_start — and a merge
// recorded between two requests shows in the next response.
func TestStageTrackerAPIMergedShape(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	s, _ := stageTrackerFixture(t, now)
	deps := testDeps(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(gh.Close)
	deps.GHClient = github.NewClientForTest(gh.URL, "acme", []string{"console"}, covBLogger())
	s.deps = deps
	isolateMergedLedger(t)

	get := func() map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil)
		rr := httptest.NewRecorder()
		s.handleStageTracker(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	var merged struct {
		Today    int                 `json:"today"`
		Total7d  int                 `json:"total_7d"`
		Total30d int                 `json:"total_30d"`
		AllTime  int                 `json:"all_time"`
		Recent   []MergedLedgerEntry `json:"recent"`
		TZ       string              `json:"tz"`
		DayStart time.Time           `json:"day_start"`
	}
	raw, ok := get()["merged"]
	if !ok {
		t.Fatal("response has no merged block")
	}
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatal(err)
	}
	if merged.AllTime != 0 || merged.Recent == nil || len(merged.Recent) != 0 || merged.TZ == "" || merged.DayStart.IsZero() {
		t.Fatalf("empty ledger block: %s", raw)
	}
	for _, k := range []string{"today", "total_7d", "total_30d", "all_time", "recent", "tz", "day_start"} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		if _, ok := m[k]; !ok {
			t.Fatalf("merged block lacks %q: %s", k, raw)
		}
	}

	s.recordMergedSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 102, IsPR: true, Merged: true, Title: "feat: soak done", URL: "https://github.com/acme/console/pull/102", MergedAt: time.Now().Add(-time.Minute), MergedBy: "octocat"}})
	if err := json.Unmarshal(get()["merged"], &merged); err != nil {
		t.Fatal(err)
	}
	if merged.AllTime != 1 || merged.Today != 1 || merged.Total7d != 1 || merged.Total30d != 1 || len(merged.Recent) != 1 {
		t.Fatalf("after a merge: %+v", merged)
	}
	e := merged.Recent[0]
	if e.Repo != "acme/console" || e.Number != 102 || e.Title != "feat: soak done" || e.MergedBy != "octocat" || e.URL == "" || e.MergedAt.IsZero() {
		t.Fatalf("recent entry: %+v", e)
	}
}

// TestClosedSweepBackfillsMergedLedger: a ledger seeded on this boot (no
// file under /data) makes the first closed sweep look back
// mergedLedgerBackfillLookback over GitHub's closed list, so merges by
// people and other bots the audit log never saw are counted; every merged
// PR it lists lands in the ledger, and once every repo was read the next
// sweep is an ordinary one. A ledger loaded from disk never widens.
func TestClosedSweepBackfillsMergedLedger(t *testing.T) {
	isolateMergedLedger(t)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return closedRowsViewedAt }))
	s := closedRowsFixture(t)
	s.deps = &Dependencies{Ctx: context.Background(), Logger: s.logger, Config: &config.Config{}, ApplyWebhookItems: func([]github.WebhookItemSnapshot) {}}
	var sinces []time.Time
	fail := true
	s.closedSweep.list = func(_ context.Context, repo string, since time.Time) ([]github.WebhookItemSnapshot, error) {
		sinces = append(sinces, since)
		if fail {
			return nil, errors.New("503")
		}
		return []github.WebhookItemSnapshot{
			{Repo: repo, Number: 13990, IsPR: true, State: "closed", Merged: true, Title: "fix: by a person", MergedBy: "octocat", MergedAt: closedRowsViewedAt.Add(-20 * 24 * time.Hour)},
			{Repo: repo, Number: 13991, IsPR: true, State: "closed", Merged: false, Title: "closed unmerged"},
		}, nil
	}
	if !s.mergedLedger().NeedsBackfill() {
		t.Fatal("a ledger seeded this boot needs the GitHub backfill")
	}
	// A failed read keeps the wide look for the retry.
	if _, err := s.sweepClosedRows(context.Background(), closedRowsViewedAt); err == nil {
		t.Fatal("list failure must surface")
	}
	fail = false
	if _, err := s.sweepClosedRows(context.Background(), closedRowsViewedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wide := closedRowsViewedAt.Add(-mergedLedgerBackfillLookback)
	if len(sinces) != 2 || !sinces[0].Equal(wide) || !sinces[1].Equal(wide.Add(time.Minute)) {
		t.Fatalf("sinces = %v, want both from %v back", sinces, mergedLedgerBackfillLookback)
	}
	if s.mergedLedger().NeedsBackfill() {
		t.Fatal("a sweep that read every repo ends the backfill")
	}
	sum := s.mergedLedger().Summary(closedRowsViewedAt.Add(time.Minute), nil)
	if sum.AllTime != 1 || len(sum.Recent) != 1 || sum.Recent[0].Number != 13990 || sum.Recent[0].MergedBy != "octocat" || sum.Total30d != 1 {
		t.Fatalf("the human merge is in the ledger: %+v", sum)
	}
	// The next sweep is an ordinary one.
	if _, err := s.sweepClosedRows(context.Background(), closedRowsViewedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if want := closedRowsViewedAt.Add(time.Minute - closedSweepOverlap); !sinces[2].Equal(want) {
		t.Fatalf("third since = %v, want %v", sinces[2], want)
	}

	// Loaded from disk on the next boot: no backfill.
	s2 := closedRowsFixture(t)
	if s2.mergedLedger().NeedsBackfill() || s2.mergedLedger().Summary(closedRowsViewedAt, nil).AllTime != 1 {
		t.Fatal("a ledger with history on disk does not widen the first sweep")
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func mergedPRsLedger(now time.Time, entries ...MergedLedgerEntry) *MergedLedger {
	l := newMergedLedger("")
	for _, e := range entries {
		l.Record(e, now)
	}
	return l
}

func mergedPRsNumbers(r MergedPRsResponse) []int {
	out := []int{}
	for _, e := range r.Entries {
		out = append(out, e.Number)
	}
	return out
}

func mergedPRsQueryAt(t *testing.T, l *MergedLedger, now time.Time, raw string) MergedPRsResponse {
	t.Helper()
	q, err := parseMergedPRsQuery(httptest.NewRequest(http.MethodGet, "/api/merged-prs?"+raw, nil), now)
	if err != nil {
		t.Fatalf("query %q: %v", raw, err)
	}
	return l.Query(q, now)
}

// The window is since..now: an entry exactly at since is in, one a second
// earlier is out, and the minute of clock skew Since allows still counts.
func TestMergedPRsQueryWindowEdges(t *testing.T) {
	now := mergedTestNow
	l := mergedPRsLedger(now,
		mergedEntry("acme/console", 1, now.Add(-24*time.Hour)),
		mergedEntry("acme/console", 2, now.Add(-24*time.Hour-time.Second)),
		mergedEntry("acme/console", 3, now.Add(30*time.Second)),
		mergedEntry("acme/console", 4, now.Add(2*time.Minute)),
		mergedEntry("acme/console", 5, now.Add(-time.Hour)),
	)
	got := mergedPRsQueryAt(t, l, now, "")
	if nums := mergedPRsNumbers(got); jsonInts(nums) != "[3,5,1]" {
		t.Fatalf("default 24h window, newest first = %v, want [3 5 1]", nums)
	}
	if got.Counts.Total != 3 || got.Matched != 3 || !got.Since.Equal(now.Add(-24*time.Hour)) || got.RetentionDays != 90 {
		t.Fatalf("counts=%+v matched=%d since=%v retention=%d", got.Counts, got.Matched, got.Since, got.RetentionDays)
	}
	if nums := mergedPRsNumbers(mergedPRsQueryAt(t, l, now, "since=2h")); jsonInts(nums) != "[3,5]" {
		t.Fatalf("since=2h = %v, want [3 5]", nums)
	}
}

func jsonInts(v []int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// merged_by: hive is the hive's own merges, human any other known login,
// a login exactly that login; counts cover the window either way.
func TestMergedPRsQueryMergedByFilter(t *testing.T) {
	now := mergedTestNow
	at := now.Add(-time.Hour)
	l := mergedPRsLedger(now,
		MergedLedgerEntry{Repo: "acme/console", Number: 1, MergedAt: at, MergedBy: MergedByHive},
		MergedLedgerEntry{Repo: "acme/console", Number: 2, MergedAt: at.Add(time.Minute), MergedBy: "octocat"},
		MergedLedgerEntry{Repo: "acme/console", Number: 3, MergedAt: at.Add(2 * time.Minute), MergedBy: "renovate[bot]"},
		MergedLedgerEntry{Repo: "acme/console", Number: 4, MergedAt: at.Add(3 * time.Minute)},
		MergedLedgerEntry{Repo: "acme/widgets", Number: 5, MergedAt: at.Add(4 * time.Minute), MergedBy: MergedByHive},
	)
	for _, tc := range []struct{ query, want string }{
		{"merged_by=hive", "[5,1]"},
		{"merged_by=human", "[3,2]"},
		{"merged_by=@OctoCat", "[2]"},
		{"merged_by=hive&repo=console", "[1]"},
		{"repo=acme/widgets", "[5]"},
	} {
		got := mergedPRsQueryAt(t, l, now, tc.query)
		if nums := jsonInts(mergedPRsNumbers(got)); nums != tc.want {
			t.Errorf("%s = %s, want %s", tc.query, nums, tc.want)
		}
	}
	got := mergedPRsQueryAt(t, l, now, "merged_by=human")
	if got.Counts != (MergedPRsCounts{Total: 5, ByHive: 2, ByHuman: 2, Unknown: 1}) || got.Matched != 2 {
		t.Fatalf("counts=%+v matched=%d, want the whole window counted", got.Counts, got.Matched)
	}
}

// An entry recorded before author and approvals were kept reads as
// unknown: it is listed without a filter, and an author or approval
// filter leaves it out and counts it instead of guessing. A ledger file
// written before the fields existed loads with them empty.
func TestMergedPRsQueryOldEntriesReadUnknown(t *testing.T) {
	now := mergedTestNow
	path := isolateMergedLedger(t)
	old := `{"version":1,"all_time":1,"entries":[{"repo":"acme/console","number":7,"merged_at":"` + now.Add(-time.Hour).Format(time.RFC3339) + `","merged_by":"hive"}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	l := newMergedLedger(path)
	got := mergedPRsQueryAt(t, l, now, "")
	if len(got.Entries) != 1 || got.Entries[0].Author != "" || got.Entries[0].AuthorKind != "" || got.Entries[0].ApprovalsKnown || got.Entries[0].ApprovedBy != nil {
		t.Fatalf("old entry = %+v, want author and approvals unknown", got.Entries)
	}
	got = mergedPRsQueryAt(t, l, now, "exclude_author=alice&approved=none")
	if len(got.Entries) != 0 || got.AuthorUnknown != 1 || got.Counts.Total != 1 {
		t.Fatalf("filtered old entry: entries=%v author_unknown=%d counts=%+v", got.Entries, got.AuthorUnknown, got.Counts)
	}
	got = mergedPRsQueryAt(t, l, now, "approved=human")
	if len(got.Entries) != 0 || got.ApprovalsUnknown != 1 {
		t.Fatalf("approval filter on an old entry: entries=%v approvals_unknown=%d", got.Entries, got.ApprovalsUnknown)
	}
}

// The brief's question in one call: merged by the hive in the last 24
// hours, no human approval recorded, not authored by the operator.
func TestMergedPRsAcceptanceQuery(t *testing.T) {
	now := mergedTestNow
	at := now.Add(-2 * time.Hour)
	hive := func(n int, author string, approvedBy ...string) MergedLedgerEntry {
		return MergedLedgerEntry{Repo: "acme/console", Number: n, MergedAt: at.Add(time.Duration(n) * time.Minute), MergedBy: MergedByHive,
			Author: author, AuthorKind: mergedAuthorKind(author, "", false, false), ApprovedBy: approvedBy, ApprovalsKnown: true}
	}
	l := mergedPRsLedger(now,
		hive(1, "contributor"),               // wanted
		hive(2, "operator"),                  // the operator's own
		hive(3, "contributor", "maintainer"), // a human approved it
		MergedLedgerEntry{Repo: "acme/console", Number: 4, MergedAt: at, MergedBy: "maintainer", Author: "contributor", ApprovalsKnown: true}, // a person merged it
		hive(5, "renovate[bot]"), // wanted: a bot's PR the hive merged alone
		MergedLedgerEntry{Repo: "acme/console", Number: 6, MergedAt: now.Add(-25 * time.Hour), MergedBy: MergedByHive, Author: "x", ApprovalsKnown: true}, // too old
	)
	got := mergedPRsQueryAt(t, l, now, "since=24h&merged_by=hive&approved=none&exclude_author=operator")
	if nums := jsonInts(mergedPRsNumbers(got)); nums != "[5,1]" {
		t.Fatalf("hive-only merges, last 24h = %s, want [5,1]", nums)
	}
	if got.Entries[0].AuthorKind != MergedAuthorBot || got.Entries[1].AuthorKind != MergedAuthorHuman {
		t.Fatalf("author kinds = %q, %q", got.Entries[0].AuthorKind, got.Entries[1].AuthorKind)
	}
}

func TestMergedPRsParseSince(t *testing.T) {
	now := mergedTestNow
	for raw, want := range map[string]time.Time{
		"":                     now.Add(-24 * time.Hour),
		"24h":                  now.Add(-24 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"90d":                  now.Add(-90 * 24 * time.Hour),
		"2026-10-26T00:00:00Z": time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseMergedPRsSince(raw, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("since %q = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"91d", "-1h", "0d", "soon", now.Add(time.Hour).Format(time.RFC3339)} {
		if _, err := parseMergedPRsSince(raw, now); err == nil {
			t.Errorf("since %q accepted", raw)
		}
	}
}

// GET /api/merged-prs answers the ledger with paging, and refuses a bad
// argument with 400.
func TestMergedPRsHandler(t *testing.T) {
	isolateMergedLedger(t)
	oldNow := mergedLedgerNow
	mergedLedgerNow = func() time.Time { return mergedTestNow }
	t.Cleanup(func() { mergedLedgerNow = oldNow })
	s := newTestServer()
	s.deps = testDeps(t)
	for n := 1; n <= 3; n++ {
		s.mergedLedger().Record(MergedLedgerEntry{Repo: "acme/console", Number: n, MergedAt: mergedTestNow.Add(-time.Duration(n) * time.Hour), MergedBy: MergedByHive}, mergedTestNow)
	}
	get := func(raw string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.handleMergedPRs(rr, httptest.NewRequest(http.MethodGet, "/api/merged-prs?"+raw, nil))
		return rr
	}
	rr := get("limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var body MergedPRsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if jsonInts(mergedPRsNumbers(body)) != "[1,2]" || body.NextOffset != 2 || body.Matched != 3 || body.Counts.ByHive != 3 {
		t.Fatalf("first page = %+v", body)
	}
	if err := json.Unmarshal(get("limit=2&offset=2").Body.Bytes(), &body); err != nil || jsonInts(mergedPRsNumbers(body)) != "[3]" || body.NextOffset != 0 {
		t.Fatalf("second page = %+v (%v)", body, err)
	}
	for _, bad := range []string{"since=100d", "approved=maybe", "limit=0", "limit=501", "offset=-1"} {
		if rr := get(bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rr.Code)
		}
	}
}

// Author and approvals fill in like the other fields: a later record
// fills blanks, and the first record that read the approvals wins.
func TestMergedLedgerUpsertFillsAuthorAndApprovals(t *testing.T) {
	now := mergedTestNow
	l := newMergedLedger("")
	l.Record(mergedEntry("acme/console", 9, now.Add(-time.Hour)), now)
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 9, Author: "alice", AuthorKind: MergedAuthorHuman, ApprovedBy: []string{"maintainer"}, ApprovalsKnown: true}, now)
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 9, Author: "bob", AuthorKind: MergedAuthorBot, ApprovalsKnown: true}, now)
	e := l.Since(now.Add(-2*time.Hour), now)[0]
	if e.Author != "alice" || e.AuthorKind != MergedAuthorHuman || !e.ApprovalsKnown || jsonStrings(e.ApprovedBy) != `["maintainer"]` {
		t.Fatalf("entry = %+v", e)
	}
}

func jsonStrings(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// At merge time the author comes from the snapshot, and the tracker's last
// row (when the server has one) refines the kind and supplies the human
// approvals the hive recorded. No GitHub call is made.
func TestMergedLedgerRecordsAuthorAndApprovalsAtMerge(t *testing.T) {
	isolateMergedLedger(t)
	s := newTestServer()
	s.deps = testDeps(t)
	body, _ := json.Marshal(map[string]any{"items": []StageTrackerItem{
		{Repo: "acme/console", Number: 31, Kind: "pr", Author: "danathar", AuthorKind: "clanker", HumanApprover: "maintainer", AdmittedBy: "operator-approval:owner"},
		{Repo: "acme/console", Number: 32, Kind: "pr", Author: "alice", AuthorKind: "human"},
	}})
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache.body = body
	s.stageTrackerCacheMu.Unlock()
	s.recordMergedSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 31, IsPR: true, Merged: true, MergedAt: mergedTestNow, MergedByHive: true, Author: "danathar"},
		{Repo: "acme/console", Number: 32, IsPR: true, Merged: true, MergedAt: mergedTestNow, MergedByHive: true, Author: "alice"},
		{Repo: "acme/console", Number: 33, IsPR: true, Merged: true, MergedAt: mergedTestNow, MergedBy: "octocat", Author: "renovate[bot]"},
		{Repo: "acme/console", Number: 34, IsPR: true, Merged: true, MergedAt: mergedTestNow, MergedByHive: true, Author: "hive-app[bot]", AppAuthored: true},
	})
	byNum := map[int]MergedLedgerEntry{}
	for _, e := range s.mergedLedger().Since(mergedTestNow.Add(-time.Hour), mergedTestNow) {
		byNum[e.Number] = e
	}
	if e := byNum[31]; e.AuthorKind != MergedAuthorAgent || !e.ApprovalsKnown || jsonStrings(e.ApprovedBy) != `["maintainer","owner"]` {
		t.Errorf("#31 relay agent PR, two recorded approvals: %+v", e)
	}
	if e := byNum[32]; e.AuthorKind != MergedAuthorHuman || !e.ApprovalsKnown || len(e.ApprovedBy) != 0 {
		t.Errorf("#32 tracker row with no approval: %+v", e)
	}
	if e := byNum[33]; e.AuthorKind != MergedAuthorBot || e.ApprovalsKnown {
		t.Errorf("#33 no tracker row: approvals unknown, bot author: %+v", e)
	}
	if e := byNum[34]; e.AuthorKind != MergedAuthorAgent {
		t.Errorf("#34 App-authored: %+v", e)
	}
}

func TestMergedAuthorKind(t *testing.T) {
	for _, tc := range []struct {
		author, tracker string
		app, hive       bool
		want            string
	}{
		{"alice", "", false, false, MergedAuthorHuman},
		{"alice", "clanker", false, false, MergedAuthorAgent},
		{"hive-app[bot]", "hive", false, false, MergedAuthorAgent},
		{"hive-app[bot]", "", true, false, MergedAuthorAgent},
		{"danathar", "", false, true, MergedAuthorAgent},
		{"renovate[bot]", "", false, false, MergedAuthorBot},
		{"app/dependabot", "", false, false, MergedAuthorBot},
		{"", "", false, false, ""},
	} {
		if got := mergedAuthorKind(tc.author, tc.tracker, tc.app, tc.hive); got != tc.want {
			t.Errorf("mergedAuthorKind(%q, %q, %v, %v) = %q, want %q", tc.author, tc.tracker, tc.app, tc.hive, got, tc.want)
		}
	}
}

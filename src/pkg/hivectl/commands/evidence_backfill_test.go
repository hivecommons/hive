package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// backfillAPI serves the recorded fixtures under testdata/evidence_backfill
// the way api.github.com would: one page of pulls (page 2 is empty) and the
// single-PR endpoint for merged PRs.
func backfillAPI(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	fixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "evidence_backfill", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/widgets/pulls":
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_, _ = w.Write(fixture("pulls_page1.json"))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"):
			n := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/")
			data, err := os.ReadFile(filepath.Join("testdata", "evidence_backfill", "pull_"+n+".json"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func runBackfill(t *testing.T, api *httptest.Server, args ...string) (map[string]any, string, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	cmd.SetArgs(append([]string{"--output", "json", "evidence", "backfill", "--repo", "acme/widgets", "--github-api", api.URL}, args...))
	err := cmd.Execute()
	var summary map[string]any
	if stdout.Len() > 0 {
		if jerr := json.Unmarshal(stdout.Bytes(), &summary); jerr != nil {
			t.Fatalf("summary not JSON: %v\n%s", jerr, stdout.String())
		}
	}
	return summary, stderr.String(), err
}

func loadBackfilled(t *testing.T, root string, number int, head string) *evidence.Bundle {
	t.Helper()
	b, err := ghpkg.LoadReviewEvidence(ghpkg.ReviewEvidencePath(root, "acme/widgets", number, head))
	if err != nil {
		t.Fatalf("bundle %d: %v", number, err)
	}
	return b
}

func TestEvidenceBackfillWritesBundlesFromHistory(t *testing.T) {
	api, calls := backfillAPI(t)
	root := t.TempDir()
	summary, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root, "--agent-login", "listed-agent", "--flag", "placeholder_hive")
	if err != nil {
		t.Fatalf("backfill: %v\n%s", err, stderr)
	}
	want := map[string]float64{"fetched": 7, "to_write": 7, "written": 7, "skipped": 0, "merged": 4, "closed": 1, "open": 2,
		"human": 3, "agent": 3, "bot": 1, "links": 2, "relinked": 0, "write_errors": 0}
	for k, v := range want {
		if got, _ := summary[k].(float64); got != v {
			t.Errorf("summary[%s] = %v, want %v (%v)", k, summary[k], v, summary)
		}
	}
	// Fewer than 10 merged → flagged, plus the operator's flag; nothing dropped.
	if got := summary["flags"]; got != "placeholder_hive,under_min_merged" {
		t.Errorf("flags = %v", got)
	}
	// One short list page (so no second page), one detail fetch per merged PR.
	if got := atomic.LoadInt64(calls); got != 5 {
		t.Errorf("api calls = %d, want 5", got)
	}

	merged := loadBackfilled(t, root, 105, "h105")
	if !merged.Backfilled || merged.Backfill == nil || merged.Backfill.State != evidence.BackfillStateMerged {
		t.Fatalf("105 not marked merged/backfilled: %+v", merged.Backfill)
	}
	if merged.Merge == nil || merged.Merge.Actor != "carol" || merged.Merge.Method != evidenceBackfillUnrecorded || merged.Merge.SHA != "m105" {
		t.Errorf("105 merge = %+v", merged.Merge)
	}
	if merged.Author.Kind != evidence.AuthorHuman || merged.Backfill.AuthorKindRule != authorRuleHuman {
		t.Errorf("105 author = %+v rule %s", merged.Author, merged.Backfill.AuthorKindRule)
	}
	if !merged.CreatedAt.Equal(time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("105 created_at = %v, want PR creation time", merged.CreatedAt)
	}
	if len(merged.Verdicts) != 0 || merged.Shadow != nil || len(merged.Sentinel) != 0 || !merged.CI.CapturedAt.IsZero() || merged.CI.Status != "" {
		t.Errorf("105 fabricated unavailable fields: %+v", merged)
	}
	if merged.Hash == "" || merged.Signed {
		t.Errorf("105 hash/signed = %q/%v", merged.Hash, merged.Signed)
	}
	if err := merged.Validate(); err != nil {
		t.Errorf("105 invalid: %v", err)
	}
	// Reverse links: #107 reverts 105, #106 fixes 105, both within 14 days.
	if got, want := merged.RevertedBy, []string{evidence.BundleID("acme/widgets", 107, "h107")}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("105 reverted_by = %v", got)
	}
	if got, want := merged.FollowedUpBy, []string{evidence.BundleID("acme/widgets", 106, "h106")}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("105 followed_up_by = %v", got)
	}

	revert := loadBackfilled(t, root, 107, "h107")
	if got := strings.Join(revert.RevertOf, ","); got != merged.ID {
		t.Errorf("107 revert_of = %q, want %q", got, merged.ID)
	}
	fix := loadBackfilled(t, root, 106, "h106")
	if got := strings.Join(fix.FollowupOf, ","); got != merged.ID {
		t.Errorf("106 followup_of = %q", got)
	}
	if fix.Author.Kind != evidence.AuthorAgent || fix.Author.Model != "gpt-5.6-luna" || fix.Backfill.AuthorKindRule != authorRuleTrailer {
		t.Errorf("106 author = %+v rule %s", fix.Author, fix.Backfill.AuthorKindRule)
	}
	if fix.Merge.Actor != evidenceBackfillUnrecorded {
		t.Errorf("106 merger without login should be unrecorded, got %q", fix.Merge.Actor)
	}

	closed := loadBackfilled(t, root, 104, "h104")
	if closed.Merge != nil || closed.Backfill.State != evidence.BackfillStateClosed || closed.Backfill.ClosedAt.IsZero() {
		t.Errorf("104 = %+v", closed.Backfill)
	}
	bot := loadBackfilled(t, root, 108, "h108")
	if bot.Author.Kind != evidence.AuthorBot || bot.Backfill.AuthorKindRule != authorRuleBot || bot.Backfill.State != evidence.BackfillStateOpen {
		t.Errorf("108 = %+v %+v", bot.Author, bot.Backfill)
	}
	footer := loadBackfilled(t, root, 103, "h103")
	if footer.Author.Kind != evidence.AuthorAgent || footer.Backfill.AuthorKindRule != authorRuleFooter {
		t.Errorf("103 = %+v %+v", footer.Author, footer.Backfill)
	}
	listed := loadBackfilled(t, root, 102, "h102")
	if listed.Author.Kind != evidence.AuthorAgent || listed.Backfill.AuthorKindRule != authorRuleAgentList {
		t.Errorf("102 = %+v %+v", listed.Author, listed.Backfill)
	}
	// "Fixes #50" names a PR with no bundle: nothing is linked, nothing invented.
	if len(listed.FollowupOf) != 0 {
		t.Errorf("102 linked an unknown PR: %v", listed.FollowupOf)
	}
	if _, err := os.Stat(ghpkg.ReviewEvidencePath(root, "acme/widgets", 101, "h101")); !os.IsNotExist(err) {
		t.Errorf("PR created before --since was written")
	}
	for _, b := range []*evidence.Bundle{merged, revert, fix, closed, bot, footer, listed} {
		if got := strings.Join(b.Backfill.Flags, ","); got != "placeholder_hive,under_min_merged" {
			t.Errorf("%s flags = %q", b.ID, got)
		}
	}
}

func TestEvidenceBackfillIsIdempotent(t *testing.T) {
	api, calls := backfillAPI(t)
	root := t.TempDir()
	if _, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root); err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr)
	}
	before := loadBackfilled(t, root, 105, "h105")
	atomic.StoreInt64(calls, 0)
	summary, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, stderr)
	}
	if summary["skipped"].(float64) != 7 || summary["written"].(float64) != 0 || summary["relinked"].(float64) != 0 {
		t.Errorf("second run summary = %v", summary)
	}
	// No per-PR detail fetches when every bundle already exists.
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("second run api calls = %d, want 1", got)
	}
	after := loadBackfilled(t, root, 105, "h105")
	if after.Hash != before.Hash || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("second run rewrote an existing bundle")
	}
}

func TestEvidenceBackfillBacklinksExistingBundle(t *testing.T) {
	api, _ := backfillAPI(t)
	root := t.TempDir()
	// A live bundle for #105 already exists (written by the hive, not backfilled).
	live := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("acme/widgets", 105, "h105"),
		Repo:          "acme/widgets",
		Number:        105,
		Author:        evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:       "b105",
		HeadSHA:       "h105",
		Merge:         &evidence.MergeEvent{Actor: "carol", Method: "squash", At: time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC), SHA: "m105"},
		CreatedAt:     time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC),
	}
	if err := writeBackfillBundle(ghpkg.ReviewEvidencePath(root, "acme/widgets", 105, "h105"), live, nil); err != nil {
		t.Fatal(err)
	}
	summary, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root, "--skip-mergers")
	if err != nil {
		t.Fatalf("backfill: %v\n%s", err, stderr)
	}
	if summary["skipped"].(float64) != 1 || summary["written"].(float64) != 6 || summary["relinked"].(float64) != 1 {
		t.Errorf("summary = %v", summary)
	}
	got := loadBackfilled(t, root, 105, "h105")
	if got.Backfilled || got.Merge.Method != "squash" {
		t.Errorf("live bundle was replaced: %+v", got)
	}
	if len(got.RevertedBy) != 1 || len(got.FollowedUpBy) != 1 {
		t.Errorf("live bundle backlinks = %v / %v", got.RevertedBy, got.FollowedUpBy)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
	// --skip-mergers leaves the merger unrecorded rather than guessing.
	if m := loadBackfilled(t, root, 107, "h107").Merge; m.Actor != evidenceBackfillUnrecorded {
		t.Errorf("107 merger = %q", m.Actor)
	}
}

func TestEvidenceBackfillDryRunWritesNothing(t *testing.T) {
	api, calls := backfillAPI(t)
	root := t.TempDir()
	summary, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--until", "2026-02-08", "--root", root, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, stderr)
	}
	if summary["dry_run"] != true || summary["fetched"].(float64) != 5 || summary["to_write"].(float64) != 5 {
		t.Errorf("summary = %v", summary)
	}
	if _, ok := summary["written"]; ok {
		t.Errorf("dry run reported writes: %v", summary)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("dry run made %d api calls, want 1 (no per-PR fetches)", got)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entries under root", len(entries))
	}
}

func TestEvidenceBackfillHiveOnHiveAutoFlag(t *testing.T) {
	api, _ := backfillAPI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.Replace(r.URL.Path, "/repos/hivecommons/hive", "/repos/acme/widgets", 1)
		api.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	cmd.SetArgs([]string{"--output", "json", "evidence", "backfill", "--repo", "hivecommons/hive", "--github-api", srv.URL, "--since", "2026-01-01", "--dry-run", "--min-merged", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"flags": "hive_on_hive"`) {
		t.Errorf("hive repo not flagged: %s", stdout.String())
	}
}

func TestEvidenceBackfillUsageErrors(t *testing.T) {
	api, _ := backfillAPI(t)
	for name, args := range map[string][]string{
		"no repo":      {"--repo", "", "--since", "2026-01-01"},
		"bad repo":     {"--repo", "nope", "--since", "2026-01-01"},
		"no since":     {},
		"bad since":    {"--since", "yesterday"},
		"until first":  {"--since", "2026-02-01", "--until", "2026-01-01"},
		"unknown flag": {"--since", "2026-01-01", "--flag", "tiny"},
		"neg acmm":     {"--since", "2026-01-01", "--acmm-level", "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runBackfill(t, api, args...)
			if ExitCode(err) != ExitUsage {
				t.Fatalf("err = %v, exit %d, want usage", err, ExitCode(err))
			}
		})
	}
}

func TestEvidenceBackfillAPIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, _, err := runBackfill(t, srv, "--since", "2026-01-01", "--root", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackfillFetcherWaitsForRateLimit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	f := newBackfillFetcher(srv.URL, "")
	var slept time.Duration
	f.sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }
	var out []backfillPR
	if err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out); err != nil {
		t.Fatal(err)
	}
	if slept != 7*time.Second || atomic.LoadInt32(&hits) != 2 {
		t.Errorf("slept %v after %d hits", slept, hits)
	}
}

func TestBackfillRateLimitWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	mk := func(status int, h map[string]string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	if _, ok := backfillRateLimitWait(mk(200, nil), now); ok {
		t.Error("200 treated as limited")
	}
	if _, ok := backfillRateLimitWait(mk(403, map[string]string{"X-RateLimit-Remaining": "5"}), now); ok {
		t.Error("403 with remaining budget treated as limited")
	}
	if d, ok := backfillRateLimitWait(mk(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1000090"}), now); !ok || d != 91*time.Second {
		t.Errorf("reset wait = %v %v", d, ok)
	}
	if d, ok := backfillRateLimitWait(mk(429, nil), now); !ok || d != time.Minute {
		t.Errorf("default wait = %v %v", d, ok)
	}
	if d, _ := backfillRateLimitWait(mk(429, map[string]string{"Retry-After": "3"}), now); d != 3*time.Second {
		t.Errorf("retry-after wait = %v", d)
	}
}

func TestBackfillAuthorRules(t *testing.T) {
	pr := func(login, typ, body string) backfillPR {
		var p backfillPR
		p.User.Login, p.User.Type, p.Body = login, typ, body
		return p
	}
	tests := []struct {
		name string
		pr   backfillPR
		kind string
		rule string
	}{
		{"trailer beats bot", pr("hive[bot]", "Bot", "x\n\n— hive: agent=a backend=b model=m"), evidence.AuthorAgent, authorRuleTrailer},
		{"listed login", pr("Robo", "User", ""), evidence.AuthorAgent, authorRuleAgentList},
		{"footer", pr("x", "User", "y\n---\n🐝 **Hive Agent**: `a` | **SHA:** `s`\n"), evidence.AuthorAgent, authorRuleFooter},
		{"bot type", pr("dependabot", "Bot", ""), evidence.AuthorBot, authorRuleBot},
		{"bot suffix", pr("renovate[bot]", "User", ""), evidence.AuthorBot, authorRuleBot},
		{"human", pr("alice", "User", "Fixes #1"), evidence.AuthorHuman, authorRuleHuman},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, rule := backfillAuthor(tc.pr, []string{"robo"})
			if a.Kind != tc.kind || rule != tc.rule {
				t.Errorf("got %s/%s, want %s/%s", a.Kind, rule, tc.kind, tc.rule)
			}
		})
	}
}

func TestParseBackfillDate(t *testing.T) {
	if d, err := parseBackfillDate("2026-02-01"); err != nil || !d.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v %v", d, err)
	}
	if d, err := parseBackfillDate("2026-02-01T05:00:00+02:00"); err != nil || !d.Equal(time.Date(2026, 2, 1, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("rfc3339 = %v %v", d, err)
	}
	if _, err := parseBackfillDate("Feb 1"); err == nil {
		t.Error("accepted junk")
	}
}

// TestEvidenceBackfillIsRegistered pins the one AddCommand line a conflict
// resolution could drop: `hivectl evidence backfill` must exist on the root.
func TestEvidenceBackfillIsRegistered(t *testing.T) {
	root := NewRootCommand(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	for _, command := range root.Commands() {
		if command.Name() != "evidence" {
			continue
		}
		for _, sub := range command.Commands() {
			if sub.Name() == "backfill" && sub.Short != "" && sub.Example != "" {
				return
			}
		}
		t.Fatal("`evidence` has no `backfill` subcommand")
	}
	t.Fatal("no `evidence` subcommand registered on the hivectl root")
}

func TestBackfillListPRsPagesUntilOlderThanSince(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	page := func(first, count int) []map[string]any {
		out := make([]map[string]any, 0, count)
		for i := 0; i < count; i++ {
			n := first - i
			out = append(out, map[string]any{
				"number": n, "title": "t", "body": "", "state": "open",
				"created_at": base.Add(-time.Duration(300-n) * time.Hour).Format(time.RFC3339),
				"updated_at": base.Format(time.RFC3339),
				"user":       map[string]any{"login": "a", "type": "User"},
				"head":       map[string]any{"sha": "h"}, "base": map[string]any{"sha": "b"},
			})
		}
		return out
	}
	pages := map[string][]map[string]any{
		"1": page(300, evidenceBackfillPerPage),
		"2": page(200, evidenceBackfillPerPage),
		"3": page(100, evidenceBackfillPerPage),
	}
	var served []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("page")
		served = append(served, p)
		_ = json.NewEncoder(w).Encode(pages[p])
	}))
	defer srv.Close()
	f := newBackfillFetcher(srv.URL, "")
	// since falls inside page 2: page 1 is fully in range, page 2 partially,
	// page 3 must not be requested.
	since := base.Add(-150 * time.Hour)
	prs, err := f.listPRs(context.Background(), "o/r", since, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 151 || prs[0].Number != 150 || prs[len(prs)-1].Number != 300 {
		t.Errorf("got %d PRs [%d..%d]", len(prs), prs[0].Number, prs[len(prs)-1].Number)
	}
	if strings.Join(served, ",") != "1,2,3" {
		// Page 2 still had in-window rows, so page 3 is fetched and found
		// entirely older; that is the stop condition.
		t.Errorf("pages served = %v", served)
	}
	// --until trims the newest side without extra requests.
	served = nil
	prs, err = f.listPRs(context.Background(), "o/r", since, base.Add(-100*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 50 || prs[len(prs)-1].Number != 199 {
		t.Errorf("until: got %d PRs, last %d", len(prs), prs[len(prs)-1].Number)
	}
}

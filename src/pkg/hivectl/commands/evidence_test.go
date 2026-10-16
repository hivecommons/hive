package commands

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// writeExportFixtures lays out schema v1 bundles under root the way the hive
// stores them (<org>/<repo>/<pr>/<sha>.json): two heads for o/r#7 (the newer
// one merged, reverted, with verdicts and shadow flags), an open o/r#3, a
// bundle in a second repo, a non-v1 bundle and a corrupt file. Returns the
// timestamp of the newest bundle so --since tests can pin a window.
func writeExportFixtures(t *testing.T, root string) time.Time {
	t.Helper()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	write := func(b *evidence.Bundle, sign bool) {
		t.Helper()
		var key = testEvidenceKey
		if !sign {
			key = nil
		}
		if err := evidence.Seal(b, key); err != nil {
			t.Fatal(err)
		}
		path := ghpkg.ReviewEvidencePath(root, b.Repo, b.Number, b.HeadSHA)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, mustJSON(t, b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// o/r#7, older head: open, no verdicts. Must be shadowed by the newer head.
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("o/r", 7, "aaaa111"), Repo: "o/r", Number: 7,
		Author:  evidence.Author{Login: "bee", Kind: evidence.AuthorAgent, Model: "claude-x"},
		BaseSHA: "base", HeadSHA: "aaaa111", CreatedAt: t0, UpdatedAt: t0,
	}, false)
	// o/r#7, newer head: merged, reverted, two verdicts, shadow flags, sentinel.
	t1 := t0.Add(24 * time.Hour)
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("o/r", 7, "bbbb222"), Repo: "o/r", Number: 7,
		Author:  evidence.Author{Login: "bee", Kind: evidence.AuthorAgent, Model: "claude-x"},
		BaseSHA: "base", HeadSHA: "bbbb222",
		Policy: evidence.Policy{ACMMLevel: 2, Perspectives: []string{"security", "correctness"}},
		Verdicts: []evidence.Verdict{
			{Perspective: "security", Verdict: "approve", Model: "m-sec", Backend: "openai", Confidence: 0.9, RecordedAt: t1},
			{Perspective: "correctness", Verdict: "block", Model: "m-cor", Backend: "anthropic", Confidence: 0.4, RecordedAt: t1},
		},
		CI:         evidence.CI{Status: "success", CapturedAt: t1.Add(time.Minute)},
		Sentinel:   []evidence.SentinelFinding{{Rule: "r1", Summary: "s"}, {Rule: "r2", Summary: "s"}},
		Merge:      &evidence.MergeEvent{Actor: "hive", Method: "squash", At: t1.Add(2 * time.Hour), SHA: "merged1"},
		RevertedBy: []string{evidence.BundleID("o/r", 9, "cccc333")},
		FollowupOf: []string{evidence.BundleID("o/r", 1, "dddd444")},
		Shadow: &evidence.Shadow{
			ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &yes, EvaluatedAt: t1},
			Intent:     &evidence.ShadowGate{WouldHaveBlocked: &no, EvaluatedAt: t1},
		},
		CreatedAt: t1, UpdatedAt: t1.Add(3 * time.Hour),
	}, true)
	// o/r#3: open human PR, older than #7's newer head.
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("o/r", 3, "eeee555"), Repo: "o/r", Number: 3,
		Author:  evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA: "base", HeadSHA: "eeee555", CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(-time.Hour),
	}, false)
	// a/z#1: second repo, sorts first.
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("a/z", 1, "ffff666"), Repo: "a/z", Number: 1,
		Author:  evidence.Author{Login: "bot[bot]", Kind: evidence.AuthorBot},
		BaseSHA: "base", HeadSHA: "ffff666", CreatedAt: t0, UpdatedAt: t0,
	}, false)
	// Unreadable: wrong schema and corrupt JSON.
	write(&evidence.Bundle{
		SchemaVersion: "v0", ID: "old", Repo: "o/r", Number: 8, BaseSHA: "base", HeadSHA: "0000000", CreatedAt: t0, UpdatedAt: t0,
	}, false)
	bad := filepath.Join(root, "o", "r", "11", "deadbeef.json")
	if err := os.MkdirAll(filepath.Dir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "o", "r", "11", "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	return t1
}

func TestEvidenceExportJSONL(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	server := httptest.NewServer(nil)
	defer server.Close()

	stdout, stderr, err := execute(t, server, "", "evidence", "export", "--root", root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "skipped 2 unreadable or non-v1 bundle(s)") {
		t.Fatalf("stderr = %q", stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 rows (latest per PR), got %d:\n%s", len(lines), stdout)
	}
	var rows []evidenceExportRow
	for _, l := range lines {
		var r evidenceExportRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("bad jsonl %q: %v", l, err)
		}
		rows = append(rows, r)
	}
	// Deterministic order: repo (case-insensitive) then PR number.
	if rows[0].Repo != "a/z" || rows[1].PR != 3 || rows[2].PR != 7 {
		t.Fatalf("order = %s#%d, %s#%d, %s#%d", rows[0].Repo, rows[0].PR, rows[1].Repo, rows[1].PR, rows[2].Repo, rows[2].PR)
	}
	r7 := rows[2]
	if r7.HeadSHA != "bbbb222" || r7.Outcome != "merged" || r7.MergedAt == nil || !r7.Signed {
		t.Fatalf("latest bundle not selected or merge missing: %+v", r7)
	}
	if r7.AuthorKind != "agent" || r7.AuthorModel != "claude-x" || r7.ACMMLevel != 2 || len(r7.Perspectives) != 2 {
		t.Fatalf("author/policy = %+v", r7)
	}
	if len(r7.Verdicts) != 2 || r7.Verdicts[0].Perspective != "correctness" || r7.Verdicts[1].Model != "m-sec" || r7.Verdicts[1].Confidence != 0.9 {
		t.Fatalf("verdicts = %+v", r7.Verdicts)
	}
	if strings.Join(r7.ReviewerModels, ",") != "m-cor,m-sec" {
		t.Fatalf("reviewer models = %v", r7.ReviewerModels)
	}
	if r7.CIConclusion != "success" || r7.CICapturedAt == nil || r7.SentinelHits != 2 {
		t.Fatalf("ci/sentinel = %+v", r7)
	}
	if len(r7.RevertedBy) != 1 || len(r7.FollowupOf) != 1 || len(r7.RevertOf) != 0 || r7.RevertOf == nil {
		t.Fatalf("links = %+v", r7)
	}
	if r7.ShadowReviewWouldHaveHeld == nil || !*r7.ShadowReviewWouldHaveHeld || r7.ShadowIntentWouldHaveBlocked == nil || *r7.ShadowIntentWouldHaveBlocked {
		t.Fatalf("shadow = %+v", r7)
	}
	r3 := rows[1]
	if r3.Outcome != "open" || r3.MergedAt != nil || r3.ShadowReviewWouldHaveHeld != nil || r3.Signed || len(r3.Verdicts) != 0 {
		t.Fatalf("open row = %+v", r3)
	}
	// Open row keeps empty arrays (not null) so consumers see a stable shape.
	if !strings.Contains(lines[1], `"verdicts":[]`) || !strings.Contains(lines[1], `"shadow_review_would_have_held":null`) {
		t.Fatalf("open row json = %s", lines[1])
	}

	// Running twice yields byte-identical output.
	again, _, err := execute(t, server, "", "evidence", "export", "--root", root)
	if err != nil || again != stdout {
		t.Fatalf("export is not deterministic (err %v)", err)
	}
}

func TestEvidenceExportCSV(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	server := httptest.NewServer(nil)
	defer server.Close()
	out := filepath.Join(t.TempDir(), "ev.csv")

	stdout, stderr, err := execute(t, server, "", "evidence", "export", "--root", root, "--format", "CSV", "--out", out)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || !strings.Contains(stderr, "wrote 3 row(s) to "+out) {
		t.Fatalf("stdout = %q stderr = %q", stdout, stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || strings.Join(recs[0], ",") != strings.Join(evidenceExportColumns, ",") {
		t.Fatalf("csv = %q", data)
	}
	col := func(rec []string, name string) string {
		for i, c := range evidenceExportColumns {
			if c == name {
				return rec[i]
			}
		}
		t.Fatalf("no column %q", name)
		return ""
	}
	r7 := recs[3]
	if col(r7, "repo") != "o/r" || col(r7, "pr") != "7" || col(r7, "head_sha") != "bbbb222" || col(r7, "outcome") != "merged" {
		t.Fatalf("row = %v", r7)
	}
	if col(r7, "perspectives") != "security;correctness" || col(r7, "reviewer_models") != "m-cor;m-sec" {
		t.Fatalf("lists = %v", r7)
	}
	if col(r7, "verdicts") != "correctness:block:m-cor:anthropic:0.4;security:approve:m-sec:openai:0.9" {
		t.Fatalf("verdicts = %q", col(r7, "verdicts"))
	}
	if col(r7, "shadow_review_would_have_held") != "true" || col(r7, "shadow_intent_would_have_blocked") != "false" || col(r7, "sentinel_hits") != "2" || col(r7, "signed") != "true" {
		t.Fatalf("flags = %v", r7)
	}
	if col(r7, "merged_at") != "2026-10-02T14:00:00Z" || col(r7, "ci_captured_at") != "2026-10-02T12:01:00Z" {
		t.Fatalf("times = %v", r7)
	}
	r3 := recs[2]
	if col(r3, "outcome") != "open" || col(r3, "merged_at") != "" || col(r3, "shadow_review_would_have_held") != "" || col(r3, "verdicts") != "" || col(r3, "ci_captured_at") != "" {
		t.Fatalf("open row = %v", r3)
	}
}

func TestEvidenceExportFilters(t *testing.T) {
	root := t.TempDir()
	newest := writeExportFixtures(t, root)
	server := httptest.NewServer(nil)
	defer server.Close()
	oldNow := evidenceExportNow
	evidenceExportNow = func() time.Time { return newest.Add(12 * time.Hour) }
	t.Cleanup(func() { evidenceExportNow = oldNow })

	prs := func(args ...string) string {
		t.Helper()
		stdout, _, err := execute(t, server, "", append([]string{"evidence", "export", "--root", root}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, l := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
			if l == "" {
				continue
			}
			var r evidenceExportRow
			if err := json.Unmarshal([]byte(l), &r); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, r.Repo+"#"+strconv.Itoa(r.PR)+"@"+r.HeadSHA)
		}
		return strings.Join(keys, " ")
	}
	// Duration: only the newest head of #7 was created in the last day.
	if got := prs("--since", "24h"); got != "o/r#7@bbbb222" {
		t.Fatalf("--since 24h = %q", got)
	}
	// RFC3339 at t0 keeps everything created at or after it. The older head of
	// #7 is in range too but loses to the newer one.
	if got := prs("--since", "2026-10-01T12:00:00Z"); got != "a/z#1@ffff666 o/r#7@bbbb222" {
		t.Fatalf("--since rfc3339 = %q", got)
	}
	// --since applies before latest-per-PR selection, as the summary API does:
	// a cutoff just after t0 drops a/z#1, #3 and the older head of #7.
	if got := prs("--since", "2026-10-01T12:00:01Z"); got != "o/r#7@bbbb222" {
		t.Fatalf("--since after t0 = %q", got)
	}
	if got := prs("--repo", "O/R"); got != "o/r#3@eeee555 o/r#7@bbbb222" {
		t.Fatalf("--repo = %q", got)
	}
	if got := prs("--repo", "none/here"); got != "" {
		t.Fatalf("--repo none = %q", got)
	}
	// Missing root is empty, not an error.
	if got := prs("--root", filepath.Join(root, "missing")); got != "" {
		t.Fatalf("missing root = %q", got)
	}
}

func TestEvidenceExportUsageErrors(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(nil)
	defer server.Close()
	for _, tc := range []struct {
		name string
		args []string
		exit int
		want string
	}{
		{"bad format", []string{"--format", "xml"}, ExitUsage, "invalid --format"},
		{"bad since", []string{"--since", "yesterday"}, ExitUsage, "invalid --since"},
		{"negative since", []string{"--since", "-1h"}, ExitUsage, "invalid --since"},
		{"bad repo", []string{"--repo", "hive"}, ExitUsage, "invalid --repo"},
		{"positional", []string{"extra"}, ExitUsage, ""},
		{"unwritable out", []string{"--out", filepath.Join(root, "missing", "x.csv")}, ExitFailure, "writing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execute(t, server, "", append([]string{"evidence", "export", "--root", root}, tc.args...)...)
			if ExitCode(err) != tc.exit {
				t.Fatalf("exit %d (err %v), want %d", ExitCode(err), err, tc.exit)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseEvidenceSince(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	if got, err := parseEvidenceSince("", now); err != nil || !got.IsZero() {
		t.Fatalf("empty = %v, %v", got, err)
	}
	if got, err := parseEvidenceSince(" 72h ", now); err != nil || !got.Equal(now.Add(-72*time.Hour)) {
		t.Fatalf("duration = %v, %v", got, err)
	}
	if got, err := parseEvidenceSince("2026-10-01T00:00:00+02:00", now); err != nil || !got.Equal(time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("rfc3339 = %v, %v", got, err)
	}
	if _, err := parseEvidenceSince("2026-10-01", now); err == nil {
		t.Fatal("date-only must be rejected")
	}
}

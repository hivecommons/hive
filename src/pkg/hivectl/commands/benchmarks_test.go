package commands

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	benchTestHiveID = "bench-test-hive"
	benchTestPublic = "pub-org/app"
	benchTestSecret = "acme/secret-repo"
)

// writeBenchmarkFixtures stores a merged agent PR in a public repo, an agent PR
// in a private repo, a human PR (no record), and a corrupt file. It returns
// the update time of the newest bundle.
func writeBenchmarkFixtures(t *testing.T, root string) time.Time {
	t.Helper()
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	write := func(b *evidence.Bundle) {
		t.Helper()
		if err := evidence.Seal(b, nil); err != nil {
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
	author := evidence.Author{Login: "hive-agent-login", Kind: evidence.AuthorAgent, Agent: "fixer", Backend: "copilot", Model: "claude-sonnet-5-5"}
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID(benchTestPublic, 11, "aaa1111"), Repo: benchTestPublic, Number: 11,
		Author: author, BaseSHA: "base", HeadSHA: "aaa1111", Policy: evidence.Policy{ACMMLevel: 3},
		CI: evidence.CI{Status: "success"},
		Verdicts: []evidence.Verdict{
			{Perspective: "security", Model: "claude-sonnet-5-5", Backend: "copilot", Verdict: "approve", Confidence: 0.9, RecordedAt: t0, Findings: []evidence.Finding{{Path: "internal/secret.go", Line: 7, Severity: "low", Summary: "SECRET-FINDING-TEXT"}}},
		},
		Merge:     &evidence.MergeEvent{Actor: "hive-merger-login", Method: "squash", At: t0.Add(time.Hour), SHA: "mergedsha1", OpenedAt: t0},
		CreatedAt: t0, UpdatedAt: t0.Add(time.Hour),
	})
	newest := t0.Add(48 * time.Hour)
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID(benchTestSecret, 7, "bbb2222"), Repo: benchTestSecret, Number: 7,
		Author: author, BaseSHA: "base", HeadSHA: "bbb2222",
		CreatedAt: t0.Add(24 * time.Hour), UpdatedAt: newest,
	})
	write(&evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID(benchTestPublic, 12, "ccc3333"), Repo: benchTestPublic, Number: 12,
		Author: evidence.Author{Login: "alice", Kind: evidence.AuthorHuman}, BaseSHA: "base", HeadSHA: "ccc3333",
		CreatedAt: t0, UpdatedAt: t0,
	})
	bad := filepath.Join(root, "x", "y", "1", "bad.json")
	if err := os.MkdirAll(filepath.Dir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newest
}

func runBenchmarksExport(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(nil)
	t.Cleanup(server.Close)
	prev := benchmarksNow
	benchmarksNow = func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { benchmarksNow = prev })
	return execute(t, server, "", append([]string{"benchmarks", "export"}, args...)...)
}

func decodeBenchmarkRecords(t *testing.T, s string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	if err := json.Unmarshal([]byte(s), &recs); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, s)
	}
	return recs
}

func TestBenchmarksExportAnonymizedByDefault(t *testing.T) {
	root := t.TempDir()
	writeBenchmarkFixtures(t, root)
	stdout, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "skipped 1 unreadable or non-v1 bundle(s)") {
		t.Fatalf("stderr = %q", stderr)
	}
	recs := decodeBenchmarkRecords(t, stdout)
	// Two agent PRs yield an author record each, the reviewed one a reviewer record, the human PR none.
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d:\n%s", len(recs), stdout)
	}
	wantHash := benchmarks.HiveHash(benchTestHiveID)
	for _, r := range recs {
		src := r["source"].(map[string]any)
		if src["hive_id"] != wantHash {
			t.Fatalf("source.hive_id = %v, want the salted hash %s", src["hive_id"], wantHash)
		}
	}
	// Privacy invariants: no identity, repo, number, sha or finding text anywhere in the output.
	for _, banned := range []string{benchTestHiveID, benchTestPublic, benchTestSecret, "pub-org", "acme", "hive-agent-login", "hive-merger-login", "alice", "aaa1111", "mergedsha1", "SECRET-FINDING-TEXT", "internal/secret.go"} {
		if strings.Contains(stdout, banned) {
			t.Fatalf("output leaks %q:\n%s", banned, stdout)
		}
	}
	if !strings.HasSuffix(stdout, "]\n") {
		t.Fatalf("output must end with a newline: %q", stdout[len(stdout)-5:])
	}
}

func TestBenchmarksExportNamesOnlyListedPublicRepos(t *testing.T) {
	root := t.TempDir()
	writeBenchmarkFixtures(t, root)
	stdout, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--public-repo", "PUB-ORG/app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, benchTestSecret) || strings.Contains(stdout, "acme") {
		t.Fatalf("unlisted repo leaked:\n%s", stdout)
	}
	named := 0
	for _, r := range decodeBenchmarkRecords(t, stdout) {
		task := r["task"].(map[string]any)
		if task["repo"] == benchTestPublic {
			named++
			if r["source"].(map[string]any)["org"] != "pub-org" || task["repo_visibility"] != "public" || task["repo_consent"] != true {
				t.Fatalf("public record = %v", r)
			}
		}
	}
	if named != 2 {
		t.Fatalf("want author and reviewer record naming %s, got %d", benchTestPublic, named)
	}
}

func TestBenchmarksExportHiveHashMatchesHiveID(t *testing.T) {
	root := t.TempDir()
	writeBenchmarkFixtures(t, root)
	byID, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID)
	if err != nil {
		t.Fatal(err)
	}
	byHash, _, err := runBenchmarksExport(t, "--root", root, "--hive-hash", strings.ToUpper(benchmarks.HiveHash(benchTestHiveID)))
	if err != nil {
		t.Fatal(err)
	}
	if byID != byHash {
		t.Fatalf("--hive-id and --hive-hash outputs differ:\n%s\n%s", byID, byHash)
	}
	again, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID)
	if err != nil || again != byID {
		t.Fatalf("export is not deterministic (err %v)", err)
	}
}

func TestBenchmarksExportWindow(t *testing.T) {
	root := t.TempDir()
	newest := writeBenchmarkFixtures(t, root)
	// Strictly after the first bundle's update time keeps only the private-repo PR (author record).
	after := newest.Add(-24 * time.Hour).Format(time.RFC3339)
	stdout, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--since", after)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(decodeBenchmarkRecords(t, stdout)); got != 1 {
		t.Fatalf("--since kept %d records, want 1:\n%s", got, stdout)
	}
	stdout, _, err = runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--until", "2026-10-31")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(decodeBenchmarkRecords(t, stdout)); got != 2 {
		t.Fatalf("--until kept %d records, want 2:\n%s", got, stdout)
	}
	stdout, _, err = runBenchmarksExport(t, "--root", t.TempDir(), "--hive-id", benchTestHiveID)
	if err != nil || stdout != "[]\n" {
		t.Fatalf("empty tree = %q, %v; want []", stdout, err)
	}
}

func TestBenchmarksExportByMonthMergesAndOutWrites(t *testing.T) {
	root := t.TempDir()
	writeBenchmarkFixtures(t, root)
	dir := t.TempDir()
	hash := benchmarks.HiveHash(benchTestHiveID)
	// A stale file with an old record and an unrelated one: the unrelated one survives, the overlap is replaced.
	monthFile := filepath.Join(dir, hash, "2026-10.json")
	if err := os.MkdirAll(filepath.Dir(monthFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monthFile, []byte(`[{"id":"keep-me","schema_version":"1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--by-month", dir); err != nil || !strings.Contains(stderr, "into 1 file(s)") {
		t.Fatalf("by-month: %v %q", err, stderr)
	}
	data, err := os.ReadFile(monthFile)
	if err != nil {
		t.Fatal(err)
	}
	recs := decodeBenchmarkRecords(t, string(data))
	if len(recs) != 4 || recs[0]["id"] == nil {
		t.Fatalf("want 3 exported + 1 kept record, got %d:\n%s", len(recs), data)
	}
	kept := false
	for _, r := range recs {
		kept = kept || r["id"] == "keep-me"
	}
	if !kept {
		t.Fatalf("existing record dropped:\n%s", data)
	}
	// Re-running is idempotent.
	if _, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--by-month", dir); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(monthFile)
	if string(again) != string(data) {
		t.Fatalf("by-month is not idempotent")
	}
	// A non-array file is refused, never overwritten.
	if err := os.WriteFile(monthFile, []byte("{not records}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--by-month", dir); err == nil || !strings.Contains(err.Error(), "not a JSON array") {
		t.Fatalf("want refusal, got %v", err)
	}
	if got, _ := os.ReadFile(monthFile); string(got) != "{not records}" {
		t.Fatalf("file was overwritten: %s", got)
	}
	// --out writes the array to a file and reports on stderr.
	out := filepath.Join(t.TempDir(), "records.json")
	stdout, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-id", benchTestHiveID, "--out", out)
	if err != nil || stdout != "" || !strings.Contains(stderr, "wrote 3 record(s) from 3 bundle(s) to "+out) {
		t.Fatalf("out: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestBenchmarksExportUsageErrors(t *testing.T) {
	root := t.TempDir()
	for name, args := range map[string][]string{
		"no hive":          {"--root", root},
		"both":             {"--root", root, "--hive-id", "a", "--hive-hash", "0123456789abcdef"},
		"short hash":       {"--root", root, "--hive-hash", "abc"},
		"bad since":        {"--root", root, "--hive-id", "a", "--since", "yesterday"},
		"until before":     {"--root", root, "--hive-id", "a", "--since", "2026-11-02", "--until", "2026-11-01"},
		"bad public repo":  {"--root", root, "--hive-id", "a", "--public-repo", "norepo"},
		"out and by-month": {"--root", root, "--hive-id", "a", "--out", "x", "--by-month", "y"},
	} {
		_, _, err := runBenchmarksExport(t, args...)
		if _, ok := err.(*usageError); !ok {
			t.Fatalf("%s: want usage error, got %v", name, err)
		}
	}
}

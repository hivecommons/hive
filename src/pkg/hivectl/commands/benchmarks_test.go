package commands

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
)

const testBenchmarksHash = "0123456789abcdef"

func runBenchmarksExport(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	benchmarksExportNow = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { benchmarksExportNow = func() time.Time { return time.Now().UTC() } })
	server := httptest.NewServer(nil)
	defer server.Close()
	return execute(t, server, "", append([]string{"benchmarks", "export"}, args...)...)
}

func decodeRecords(t *testing.T, s string) []benchmarks.Record {
	t.Helper()
	var recs []benchmarks.Record
	if err := json.Unmarshal([]byte(s), &recs); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, s)
	}
	return recs
}

func TestBenchmarksExportAnonymizedByDefault(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	stdout, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-hash", testBenchmarksHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "skipped 2 unreadable") {
		t.Fatalf("stderr = %q", stderr)
	}
	recs := decodeRecords(t, stdout)
	if len(recs) != 2 {
		t.Fatalf("want author and reviewer record for o/r#7, got %d:\n%s", len(recs), stdout)
	}
	for _, r := range recs {
		if r.Source.HiveID != testBenchmarksHash {
			t.Errorf("hive id = %q", r.Source.HiveID)
		}
		if r.Task.Repo != "" || r.Source.Org != "" || r.Task.RepoConsent {
			t.Errorf("repo leaked: %+v %+v", r.Task, r.Source)
		}
		if r.Cost.Basis != benchmarks.CostBasisUnknown {
			t.Errorf("cost basis = %q", r.Cost.Basis)
		}
	}
	for _, leak := range []string{"o/r", "bee", "bbbb222", "aaaa111"} {
		if strings.Contains(stdout, leak) {
			t.Errorf("output leaks %q:\n%s", leak, stdout)
		}
	}
}

func TestBenchmarksExportPublicRepoAndHiveID(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	stdout, _, err := runBenchmarksExport(t, "--root", root, "--hive-id", "my-hive", "--public-repo", "O/R")
	if err != nil {
		t.Fatal(err)
	}
	recs := decodeRecords(t, stdout)
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}
	for _, r := range recs {
		if r.Task.Repo != "o/r" || r.Source.Org != "o" || !r.Task.RepoConsent || r.Task.RepoVisibility != "public" {
			t.Errorf("public repo not named: %+v %+v", r.Task, r.Source)
		}
		if r.Source.HiveID != benchmarks.HiveHash("my-hive") {
			t.Errorf("hive id = %q", r.Source.HiveID)
		}
	}
}

func TestBenchmarksExportSinceUntilAndOut(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	// o/r#7 evidence time is 2026-10-02T15:00Z.
	stdout, _, err := runBenchmarksExport(t, "--root", root, "--hive-hash", testBenchmarksHash, "--since", "2026-10-03")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout); got != "[]" {
		t.Fatalf("since after evidence: %q", got)
	}
	out := filepath.Join(t.TempDir(), "r.json")
	_, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-hash", testBenchmarksHash, "--until", "2026-10-03", "--out", out)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if len(decodeRecords(t, string(data))) != 2 || !strings.Contains(stderr, "wrote 2 record(s)") {
		t.Fatalf("out = %s stderr = %q", data, stderr)
	}
	stdout, _, err = runBenchmarksExport(t, "--root", root, "--hive-hash", testBenchmarksHash, "--until", "2026-10-02")
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("until before evidence: %q %v", stdout, err)
	}
}

func TestBenchmarksExportByMonth(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	dir := filepath.Join(t.TempDir(), "fleet")
	stdout, stderr, err := runBenchmarksExport(t, "--root", root, "--hive-hash", testBenchmarksHash, "--by-month", dir)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || !strings.Contains(stderr, "wrote "+filepath.Join(dir, testBenchmarksHash, "2026-10.json")) {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, testBenchmarksHash, "2026-10.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(decodeRecords(t, string(data))) != 2 {
		t.Fatalf("month file:\n%s", data)
	}
}

func TestBenchmarksExportUsageErrors(t *testing.T) {
	root := t.TempDir()
	for name, args := range map[string][]string{
		"no hive":      {"--root", root},
		"both":         {"--root", root, "--hive-id", "x", "--hive-hash", testBenchmarksHash},
		"bad hash":     {"--root", root, "--hive-hash", "xyz"},
		"bad repo":     {"--root", root, "--hive-id", "x", "--public-repo", "nope"},
		"bad since":    {"--root", root, "--hive-id", "x", "--since", "yesterday"},
		"out+by-month": {"--root", root, "--hive-id", "x", "--out", "a", "--by-month", "b"},
	} {
		_, _, err := runBenchmarksExport(t, args...)
		if _, ok := err.(*usageError); !ok {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
}

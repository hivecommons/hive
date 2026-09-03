package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// writeReportFiles marshals one valid report per default perspective into dir
// using the collectable review-report file naming convention.
func writeReportFiles(t *testing.T, dir string) {
	t.Helper()
	for _, rep := range allApprove() {
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		name := ReviewReportFilePrefix + string(rep.Perspective) + ReviewReportFileSuffix
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}
}

func TestCollectAndWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeReportFiles(t, dir)
	out := filepath.Join(t.TempDir(), "nested", "review-verdicts.json")

	artifact, err := CollectAndWrite(dir, out, AggregateOptions{})
	if err != nil {
		t.Fatalf("CollectAndWrite: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(artifact.Items))
	}
	if got := artifact.Items[0]; !got.MergeEligible || got.Verdict != VerdictApprove {
		t.Fatalf("aggregate = verdict:%s merge:%v, want approve/true", got.Verdict, got.MergeEligible)
	}
	if artifact.GeneratedAt.IsZero() {
		t.Fatal("GeneratedAt not set")
	}

	loaded, err := LoadArtifact(out)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if len(loaded.Items) != 1 || !loaded.Items[0].MergeEligible {
		t.Fatalf("loaded artifact lost merge eligibility: %+v", loaded.Items)
	}
	if !loaded.HasAggregateApproval("hivecommons/hive", 2807, testSHA) {
		t.Fatal("HasAggregateApproval = false after round trip")
	}
	if loaded.HasAggregateApproval("hivecommons/hive", 2807, "deadbeef") {
		t.Fatal("HasAggregateApproval matched a different head SHA")
	}
	// WriteArtifact persists atomically (write temp, rename); the temp file
	// must not survive a successful write.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file must be renamed away, stat err=%v", err)
	}
}

func TestCollectAndWriteCollectError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "review-verdicts.json")
	if _, err := CollectAndWrite(filepath.Join(t.TempDir(), "missing"), out, AggregateOptions{}); err == nil {
		t.Fatal("expected error for missing report dir")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("artifact must not be written when collection fails")
	}
}

func TestWriteArtifactDefaultsPath(t *testing.T) {
	orig := ReviewVerdictsPath
	ReviewVerdictsPath = filepath.Join(t.TempDir(), "verdicts", ReviewVerdictsFile)
	defer func() { ReviewVerdictsPath = orig }()

	if err := WriteArtifact("", Artifact{}); err != nil {
		t.Fatalf("WriteArtifact with default path: %v", err)
	}
	if _, err := LoadArtifact(""); err != nil {
		t.Fatalf("LoadArtifact with default path: %v", err)
	}
}

func TestWriteArtifactMkdirError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Parent of the target path is a regular file: MkdirAll must fail.
	if err := WriteArtifact(filepath.Join(blocker, "sub", "verdicts.json"), Artifact{}); err == nil {
		t.Fatal("expected MkdirAll error")
	}
}

func TestWriteArtifactUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := WriteArtifact(filepath.Join(dir, "verdicts.json"), Artifact{}); err == nil {
		t.Fatal("expected write error in read-only dir")
	}
}

func TestLoadArtifactErrors(t *testing.T) {
	if _, err := LoadArtifact(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error for missing artifact")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArtifact(bad); err == nil {
		t.Fatal("expected error for malformed artifact JSON")
	}
}

func TestSeverityRankOrdering(t *testing.T) {
	ordered := []outputschema.Severity{
		outputschema.SeverityInfo,
		outputschema.SeverityLow,
		outputschema.SeverityMedium,
		outputschema.SeverityHigh,
		outputschema.SeverityCritical,
	}
	for i, s := range ordered {
		if got := severityRank(s); got != i+1 {
			t.Fatalf("severityRank(%s) = %d, want %d", s, got, i+1)
		}
		if !severityAtLeast(s, s) {
			t.Fatalf("severityAtLeast(%s, %s) = false", s, s)
		}
		if i > 0 && severityAtLeast(ordered[i-1], s) {
			t.Fatalf("severityAtLeast(%s, %s) = true", ordered[i-1], s)
		}
	}
	if got := severityRank(outputschema.Severity("bogus")); got != 0 {
		t.Fatalf("severityRank(bogus) = %d, want 0", got)
	}
}

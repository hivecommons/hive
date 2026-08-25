package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/outputschema"
)

func writeReportFiles(t *testing.T, dir string, reports []PerspectiveReport) {
	t.Helper()
	for _, r := range reports {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ReviewReportFilePrefix+string(r.Perspective)+ReviewReportFileSuffix)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectAndWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeReportFiles(t, dir, allApprove())

	// Nested output path also exercises WriteArtifact's MkdirAll branch.
	out := filepath.Join(dir, "nested", ReviewVerdictsFile)
	artifact, err := CollectAndWrite(dir, out, AggregateOptions{})
	if err != nil {
		t.Fatalf("CollectAndWrite: %v", err)
	}
	if len(artifact.Items) != 1 || !artifact.HasAggregateApproval("kubestellar/hive", 2807, testSHA) {
		t.Fatalf("unexpected returned artifact: %+v", artifact)
	}
	loaded, err := LoadArtifact(out)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if !loaded.HasAggregateApproval("kubestellar/hive", 2807, testSHA) {
		t.Fatalf("persisted artifact missing approval: %+v", loaded)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file must be renamed away, stat err=%v", err)
	}
}

func TestCollectAndWriteMissingDir(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, ReviewVerdictsFile)
	if _, err := CollectAndWrite(filepath.Join(dir, "does-not-exist"), out, AggregateOptions{}); err == nil {
		t.Fatal("expected error for missing report dir")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("no artifact must be written when collection fails, stat err=%v", err)
	}
}

func TestWriteArtifactMkdirFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Parent of the target path is a regular file, so MkdirAll must fail.
	if err := WriteArtifact(filepath.Join(blocker, ReviewVerdictsFile), Artifact{}); err == nil {
		t.Fatal("expected MkdirAll error when parent is a file")
	}
}

func TestLoadArtifactErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadArtifact(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing artifact file")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
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
			t.Errorf("severityRank(%s) = %d, want %d", s, got, i+1)
		}
	}
	if got := severityRank(outputschema.Severity("bogus")); got != 0 {
		t.Errorf("severityRank(bogus) = %d, want 0", got)
	}
	for i := 1; i < len(ordered); i++ {
		if !severityAtLeast(ordered[i], ordered[i-1]) {
			t.Errorf("severityAtLeast(%s, %s) = false, want true", ordered[i], ordered[i-1])
		}
		if severityAtLeast(ordered[i-1], ordered[i]) {
			t.Errorf("severityAtLeast(%s, %s) = true, want false", ordered[i-1], ordered[i])
		}
	}
}

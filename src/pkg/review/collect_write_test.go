package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/outputschema"
)

// CollectAndWrite is the one-call path the hive uses to aggregate perspective
// reports and persist the verdicts artifact; it had 0% coverage even though
// Collect and WriteArtifact were each tested in isolation.
func TestCollectAndWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, r := range allApprove() {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ReviewReportFilePrefix+string(r.Perspective)+ReviewReportFileSuffix)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(dir, "nested", ReviewVerdictsFile)
	artifact, err := CollectAndWrite(dir, out, AggregateOptions{})
	if err != nil {
		t.Fatalf("CollectAndWrite: %v", err)
	}
	if !artifact.HasAggregateApproval("kubestellar/hive", 2807, testSHA) {
		t.Fatalf("returned artifact missing approval: %+v", artifact)
	}

	loaded, err := LoadArtifact(out)
	if err != nil {
		t.Fatalf("LoadArtifact(%s): %v", out, err)
	}
	if !loaded.HasAggregateApproval("kubestellar/hive", 2807, testSHA) {
		t.Fatalf("persisted artifact missing approval: %+v", loaded)
	}
}

// A collect failure must not leave a stale or partial artifact behind.
func TestCollectAndWriteCollectErrorWritesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out := filepath.Join(t.TempDir(), ReviewVerdictsFile)
	if _, err := CollectAndWrite(missing, out, AggregateOptions{}); err == nil {
		t.Fatal("expected error for missing report dir")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("artifact must not be written on collect failure: stat err = %v", err)
	}
}

// severityRank orders the gate threshold comparison; the default (unknown
// severity) branch and full ordering were untested.
func TestSeverityRankOrderingAndUnknown(t *testing.T) {
	ordered := []outputschema.Severity{
		outputschema.SeverityInfo,
		outputschema.SeverityLow,
		outputschema.SeverityMedium,
		outputschema.SeverityHigh,
		outputschema.SeverityCritical,
	}
	prev := severityRank(outputschema.Severity("bogus"))
	if prev != 0 {
		t.Fatalf("unknown severity rank = %d, want 0", prev)
	}
	for _, s := range ordered {
		got := severityRank(s)
		if got <= prev {
			t.Fatalf("severityRank(%s) = %d, not above previous %d", s, got, prev)
		}
		prev = got
	}
	if !severityAtLeast(outputschema.SeverityHigh, outputschema.SeverityMedium) {
		t.Fatal("high must satisfy a medium threshold")
	}
	if severityAtLeast(outputschema.SeverityLow, outputschema.SeverityMedium) {
		t.Fatal("low must not satisfy a medium threshold")
	}
	// An unknown severity ranks 0 and must never pass a real threshold.
	if severityAtLeast(outputschema.Severity("bogus"), outputschema.SeverityInfo) {
		t.Fatal("unknown severity must not satisfy any threshold")
	}
}

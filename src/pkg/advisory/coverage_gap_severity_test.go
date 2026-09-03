package advisory

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// A coverage-gap finding must never surface as critical in the digest (#4734):
// the quality policy caps coverage gaps at high, and the digest enforces that
// cap mechanically even when a non-compliant agent files a priority-0 bead.
func TestBuildDigestFromBeadsCapsCriticalCoverageGap(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	gap, err := store.Create("maybeRemoveRequesterFinalizer has zero unit tests", beads.TypeAdvisory, beads.PriorityCritical, "quality", "inference-server.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetMetadata(gap.ID, "finding_type", "coverage-gap")

	sec, err := store.Create("token minted without audience check", beads.TypeAdvisory, beads.PriorityCritical, "quality", "mint.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetMetadata(sec.ID, "finding_type", "security")

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "busy", DigestOptions{})

	got := map[string]string{}
	for _, f := range d.ByAgent["quality"] {
		got[f.Type] = f.Severity
	}
	if got["coverage-gap"] != "high" {
		t.Errorf("coverage-gap severity = %q, want %q (critical must be capped)", got["coverage-gap"], "high")
	}
	if got["security"] != "critical" {
		t.Errorf("security severity = %q, want %q (cap must apply only to coverage-gap)", got["security"], "critical")
	}
}

// The direct-findings builder applies the same cap.
func TestBuildDigestCapsCriticalCoverageGap(t *testing.T) {
	d := BuildDigest([]Finding{
		{Agent: "quality", Type: "coverage-gap", Severity: "critical", Title: "untested handler"},
		{Agent: "quality", Type: "coverage-gap", Severity: "medium", Title: "partially tested path"},
	}, "busy")

	sev := map[string]string{}
	for _, f := range d.ByAgent["quality"] {
		sev[f.Title] = f.Severity
	}
	if sev["untested handler"] != "high" {
		t.Errorf("critical coverage-gap severity = %q, want high", sev["untested handler"])
	}
	if sev["partially tested path"] != "medium" {
		t.Errorf("medium coverage-gap severity = %q, want medium (below-cap severities untouched)", sev["partially tested path"])
	}
}

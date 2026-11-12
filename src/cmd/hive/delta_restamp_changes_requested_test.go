package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

func restampTestFinding() review.PerspectiveFinding {
	return review.PerspectiveFinding{Perspective: review.PerspectiveCorrectness, Finding: outputschema.Finding{
		Title: "update the line references in .github/lint-config.yml", Severity: outputschema.SeverityMedium,
		File: ".github/workflows/build.yml", Line: 40,
	}}
}

// The 072 route reads the head's verdict from review-verdicts.json. A
// changes_requested an older planner carried onto the author's fix head
// across a real change (nothing published for that head, nothing to read)
// must not send the PR back to its author: once the artifact is loaded the
// head has no verdict (contributor-lane:no-verdict, 050) and the planner
// reviews the fix. A changes_requested the swarm recorded for the head
// still routes back, and carries its findings for the nudge.
func TestContributorLaneVerdictIgnoresChangesRequestedCarriedAcrossAFix(t *testing.T) {
	now := time.Now().UTC()
	reviewed := review.Aggregate{
		Repo: "acme/widgets", Number: 7, HeadSHA: "sha1", Verdict: review.VerdictChangesRequested, RecordedAt: now.Add(-time.Hour),
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictChangesRequested},
		Findings:     []review.PerspectiveFinding{restampTestFinding()},
	}
	carried := reviewed
	carried.HeadSHA, carried.RecordedAt = "sha2", now
	carried.Delta = &review.DeltaReviewInfo{PreviousHead: "sha1", Trivial: true, ChangedLines: 13,
		Note: "re-stamped from sha1: delta touches 2 file(s), 13 line(s), none previously flagged"}
	path := filepath.Join(t.TempDir(), review.ReviewVerdictsFile)
	if err := review.WriteArtifact(path, review.Artifact{Items: []review.Aggregate{reviewed, carried}}); err != nil {
		t.Fatal(err)
	}
	artifact, err := review.LoadArtifact(path)
	if err != nil {
		t.Fatal(err)
	}

	fix := contributorLaneVerdictFromArtifact(artifact, "acme/widgets", 7, "sha2", false)
	if fix.Verdict != "" || stageengine.VerdictBlocks(prstage.Detail{Verdict: fix, VerdictKnown: true}) {
		t.Fatalf("fix head verdict = %+v, want none (review pending), not a carried changes_requested", fix)
	}

	old := contributorLaneVerdictFromArtifact(artifact, "acme/widgets", 7, "sha1", false)
	if !stageengine.VerdictBlocks(prstage.Detail{Verdict: old, VerdictKnown: true}) {
		t.Fatalf("reviewed head verdict = %+v, want changes_requested", old)
	}
	if len(old.FindingAsks) != 1 || old.FindingAsks[0] != "`.github/workflows/build.yml:40` update the line references in .github/lint-config.yml" {
		t.Fatalf("finding asks = %q", old.FindingAsks)
	}
}

// The withheld-fix line must not say a review was published for a head
// whose verdict was carried onto it without one.
func TestAuditWithheldReviewFixesCarriedVerdictDoesNotClaimPublication(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	auditor := &recordingFixAuditor{}
	plan := review.DispatchPlan{WithheldFixes: []review.WithheldFix{{
		Repo: "acme/widgets", Number: 7, HeadSHA: "sha2", Author: "contributor", Setting: review.WithheldFixSetting,
		CarriedFrom: "sha1", Reason: "verdict carried from sha1 without a review (nothing published for this head), fix not pushed", Withheld: time.Now().UTC(),
	}}}
	auditWithheldReviewFixes(plan, auditor, logger)
	out := buf.String()
	if strings.Contains(out, "review published") || !strings.Contains(out, "nothing published for this head") || !strings.Contains(out, "carried_from=sha1") {
		t.Fatalf("log line = %s", out)
	}
	if len(auditor.entries) != 1 || !strings.Contains(auditor.entries[0], "carried_from=sha1") {
		t.Fatalf("audit entries = %v", auditor.entries)
	}

	buf.Reset()
	reviewed := plan
	reviewed.WithheldFixes = []review.WithheldFix{{Repo: "acme/widgets", Number: 7, HeadSHA: "sha1", Setting: review.WithheldFixSetting}}
	auditWithheldReviewFixes(reviewed, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	if !strings.Contains(buf.String(), "review published, nothing pushed") {
		t.Fatalf("reviewed head's line changed: %s", buf.String())
	}
}

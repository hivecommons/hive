package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CollectAndMerge exists because the per-perspective reports it collects from
// live on the container's ephemeral layer while the verdict artifact is the
// hive's durable memory of what it has already judged. These tests pin the
// behaviour that keeps a restart from turning into a second round of review
// comments on PRs that were already handled.

func seedReports(t *testing.T, dir string, repo string, number int, headSHA string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range DefaultPerspectives {
		rep := baseReport(p, VerdictApprove)
		rep.Repo = repo
		rep.Number = number
		rep.HeadSHA = headSHA
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		name := ReviewReportFilePrefix + string(p) + ReviewReportFileSuffix
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectAndMergeKeepsVerdictsWhenReportDirIsWiped(t *testing.T) {
	// The exact production failure: the hive judged a PR, the container
	// restarted, the report files vanished. The verdict must survive, or the
	// PR is dispatched for review all over again.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Now().UTC()

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	first, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("expected the freshly collected verdict, got %+v", first.Items)
	}

	// Restart: the ephemeral report dir comes back empty.
	if err := os.RemoveAll(reportDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}

	second, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("merge after wipe: %v", err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("verdict lost after report dir wipe: %+v", second.Items)
	}
	if second.Items[0].Number != 1011 || second.Items[0].HeadSHA != "sha-one" {
		t.Fatalf("wrong verdict retained: %+v", second.Items[0])
	}
	if !second.HasAggregateApproval("projectbluefin/common", 1011, "sha-one") {
		t.Fatal("retained verdict must still register as an approval, otherwise the PR is re-dispatched")
	}
}

func TestCollectAndMergeFreshVerdictWinsForSameKey(t *testing.T) {
	// A re-review of the same head SHA is the authoritative result.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Now().UTC()

	stale := Artifact{Items: []Aggregate{{
		Repo: "projectbluefin/common", Number: 1011, HeadSHA: "sha-one",
		Verdict: VerdictRequiresHuman, RequiresHuman: true, RecordedAt: now.Add(-time.Hour),
	}}}
	if err := WriteArtifact(artifactPath, stale); err != nil {
		t.Fatal(err)
	}

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("same key must not duplicate: %+v", merged.Items)
	}
	if merged.Items[0].Verdict != VerdictApprove {
		t.Fatalf("fresh verdict must win, got %q", merged.Items[0].Verdict)
	}
}

func TestCollectAndMergeKeepsDistinctHeadSHAs(t *testing.T) {
	// A force-push creates a new head SHA and a genuinely new review subject.
	// The old verdict is still useful history and must not evict the new one.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Now().UTC()

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	if _, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(reportDir); err != nil {
		t.Fatal(err)
	}
	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-two")
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("both head SHAs should be recorded, got %+v", merged.Items)
	}
}

func TestCollectAndMergePrunesStaleVerdicts(t *testing.T) {
	// Without pruning the durable file grows forever as PRs close.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Now().UTC()

	old := Artifact{Items: []Aggregate{{
		Repo: "projectbluefin/common", Number: 1, HeadSHA: "ancient",
		RecordedAt: now.Add(-VerdictRetention - time.Hour),
	}}}
	if err := WriteArtifact(artifactPath, old); err != nil {
		t.Fatal(err)
	}

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range merged.Items {
		if item.HeadSHA == "ancient" {
			t.Fatalf("verdict older than the retention window should have been pruned: %+v", item)
		}
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected only the fresh verdict, got %+v", merged.Items)
	}
}

func TestCollectAndMergeStampsRecordedAt(t *testing.T) {
	// Pruning depends on the stamp, so an unstamped write would make entries
	// immortal.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Now().UTC().Truncate(time.Second)

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Items[0].RecordedAt; !got.Equal(now) {
		t.Fatalf("RecordedAt = %v, want %v", got, now)
	}
}

func TestCollectAndMergeSurvivesMissingPriorArtifact(t *testing.T) {
	// First ever run: nothing to merge with, and that is not an error.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)

	seedReports(t, reportDir, "projectbluefin/common", 1011, "sha-one")
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("first run must not fail on a missing artifact: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("unexpected items: %+v", merged.Items)
	}
}

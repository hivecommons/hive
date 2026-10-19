package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// seedVerdictReports writes one report per default perspective for
// repo#number@headSHA, all approving except the perspectives in override.
func seedVerdictReports(t *testing.T, dir string, repo string, number int, headSHA string, override map[Perspective]Verdict) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range DefaultPerspectives {
		v := VerdictApprove
		if o, ok := override[p]; ok {
			v = o
		}
		rep := baseReport(p, v)
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

var securityRequiresHuman = map[Perspective]Verdict{PerspectiveSecurity: VerdictRequiresHuman}

// hivecommons/hive#12957: every artifact refresh re-collects the same
// reports. RecordedAt moves (it is the retention clock); the first-recorded
// time must not, or a person's release of the verdict hold, judged against
// it, stops counting a few minutes after it was given.
func TestCollectAndMergeKeepsFirstRecordedAtAcrossRefreshes(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	t1 := time.Now().UTC().Truncate(time.Second)
	t2 := t1.Add(5 * time.Minute)

	seedVerdictReports(t, reportDir, "acme/widget", 192, "cacbd42", securityRequiresHuman)
	if _, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t1); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t2)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("same key must not duplicate: %+v", merged.Items)
	}
	item := merged.Items[0]
	if !item.RecordedAt.Equal(t2) {
		t.Fatalf("RecordedAt = %v, want the refresh time %v (retention clock)", item.RecordedAt, t2)
	}
	if !item.FirstRecordedAt.Equal(t1) {
		t.Fatalf("FirstRecordedAt = %v, want the first collect %v", item.FirstRecordedAt, t1)
	}
	hold, ok := merged.VerdictHoldFor("acme/widget", 192)
	if !ok {
		t.Fatal("requires_human verdict must hold the PR")
	}
	if !hold.RecordedAt.Equal(t1) {
		t.Fatalf("hold.RecordedAt = %v, want the first-recorded time %v", hold.RecordedAt, t1)
	}

	// Retention follows the last time the verdict was seen, not the first:
	// with the reports gone, a collect just past t1+VerdictRetention (but
	// within t2+VerdictRetention) still keeps the entry.
	if err := os.RemoveAll(reportDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	later, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t1.Add(VerdictRetention+time.Minute))
	if err != nil {
		t.Fatalf("collect after wipe: %v", err)
	}
	if len(later.Items) != 1 || !later.Items[0].FirstRecordedAt.Equal(t1) || !later.Items[0].RecordedAt.Equal(t2) {
		t.Fatalf("entry re-confirmed at %v must survive with its timestamps unchanged: %+v", t2, later.Items)
	}
}

// An artifact written before first_recorded_at existed falls back to the
// entry's RecordedAt on the first refresh after the upgrade.
func TestCollectAndMergeFirstRecordedAtFallsBackToLegacyRecordedAt(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	t0 := time.Now().UTC().Truncate(time.Second)

	seedVerdictReports(t, reportDir, "acme/widget", 192, "cacbd42", securityRequiresHuman)
	first, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := first
	legacy.Items = append([]Aggregate(nil), first.Items...)
	legacy.Items[0].FirstRecordedAt = time.Time{}
	if err := WriteArtifact(artifactPath, legacy); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "first_recorded_at") {
		t.Fatalf("a zero FirstRecordedAt must be omitted from the JSON: %s", raw)
	}

	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t0.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Items[0].FirstRecordedAt; !got.Equal(t0) {
		t.Fatalf("FirstRecordedAt = %v, want the legacy RecordedAt %v", got, t0)
	}
}

// A genuinely new verdict starts a new first-recorded time: a new head, or a
// different outcome for the same head. That keeps #12374's rule that a
// release given before the verdict does not count.
func TestCollectAndMergeNewVerdictResetsFirstRecordedAt(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	t1 := time.Now().UTC().Truncate(time.Second)

	seedVerdictReports(t, reportDir, "acme/widget", 192, "cacbd42", securityRequiresHuman)
	if _, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t1); err != nil {
		t.Fatal(err)
	}

	// Fix push: the per-perspective files are rewritten for the new head.
	t3 := t1.Add(10 * time.Minute)
	seedVerdictReports(t, reportDir, "acme/widget", 192, "d00dfee", securityRequiresHuman)
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t3)
	if err != nil {
		t.Fatal(err)
	}
	hold, ok := merged.VerdictHoldFor("acme/widget", 192)
	if !ok || hold.HeadSHA != "d00dfee" || !hold.RecordedAt.Equal(t3) {
		t.Fatalf("hold = %+v (held %v), want the new head's verdict first recorded at %v", hold, ok, t3)
	}

	// Same head, different outcome: the approve is a new verdict.
	t4 := t3.Add(10 * time.Minute)
	seedVerdictReports(t, reportDir, "acme/widget", 192, "d00dfee", nil)
	merged, err = CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, t4)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range merged.Items {
		if item.HeadSHA == "d00dfee" && !item.FirstRecordedAt.Equal(t4) {
			t.Fatalf("changed verdict on the same head kept FirstRecordedAt %v, want %v", item.FirstRecordedAt, t4)
		}
	}
	if hold, ok := merged.VerdictHoldFor("acme/widget", 192); ok {
		t.Fatalf("a merge-eligible approve recorded after the hold must release it, got %+v", hold)
	}
}

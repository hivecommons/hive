package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Review reports are the verdict's only form between the relay writing them
// and the next cycle's CollectAndMerge folding them into the artifact. On the
// container's ephemeral layer a pod replacement inside that window lost the
// verdict, the head looked unreviewed, and the same SHA was reviewed twice.
// Guard the location the same way the dispatch state and artifact are.
func TestReviewReportDirIsNotOnScratchStorage(t *testing.T) {
	if got := ReportDir(""); got == outputschema.AgentReportDir || strings.HasPrefix(got, outputschema.AgentReportDir+string(os.PathSeparator)) {
		t.Fatalf("review reports default to the regenerable scratch dir %q; a restart between relay write and collect would lose the verdict", got)
	}
	if !strings.HasPrefix(DefaultReportDir, DefaultDispatchStateDir+string(os.PathSeparator)) {
		t.Fatalf("report dir %q is not under the durable data dir %q", DefaultReportDir, DefaultDispatchStateDir)
	}
	if got := ReportDir("/explicit"); got != "/explicit" {
		t.Fatalf("ReportDir must honour an explicit dir, got %q", got)
	}
}

// Now that reports persist across restarts nothing wipes them, so
// CollectAndMerge must retire them on the artifact's own horizon.
func TestCollectAndMergePrunesReportsPastRetention(t *testing.T) {
	dir := t.TempDir()
	writeReportFiles(t, dir)
	now := time.Now().UTC()

	// Age one report past retention, leave the rest fresh, and drop in a
	// non-report file that must never be touched.
	aged := filepath.Join(dir, ReviewReportFilePrefix+string(PerspectiveStyle)+ReviewReportFileSuffix)
	old := now.Add(-VerdictRetention - time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "review-links.json")
	if err := os.WriteFile(other, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "review-verdicts.json")
	artifact, err := CollectAndMerge(dir, out, AggregateOptions{}, now)
	if err != nil {
		t.Fatalf("CollectAndMerge: %v", err)
	}
	// The aged report still counted in THIS collect; it is retired only after
	// the merge that consumed it is durable.
	if len(artifact.Items) != 1 || len(artifact.Items[0].Perspectives) != len(DefaultPerspectives) {
		t.Fatalf("aged report was not collected before pruning: %+v", artifact.Items)
	}
	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Fatalf("report past retention survived CollectAndMerge: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-report file was pruned: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var reports int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ReviewReportFilePrefix) {
			reports++
		}
	}
	if reports != len(DefaultPerspectives)-1 {
		t.Fatalf("fresh reports pruned: %d left, want %d", reports, len(DefaultPerspectives)-1)
	}
}

func TestPruneReportsBoundary(t *testing.T) {
	dir := t.TempDir()
	writeReportFiles(t, dir)
	now := time.Now().UTC()
	exactly := now.Add(-VerdictRetention)
	for _, p := range DefaultPerspectives {
		f := filepath.Join(dir, ReviewReportFilePrefix+string(p)+ReviewReportFileSuffix)
		if err := os.Chtimes(f, exactly, exactly); err != nil {
			t.Fatal(err)
		}
	}
	// Exactly at the horizon is kept: the artifact keeps an item until it is
	// strictly older than VerdictRetention, and the two must agree.
	if n, err := PruneReports(dir, VerdictRetention, now); err != nil || n != 0 {
		t.Fatalf("prune at boundary removed %d (err %v), want 0", n, err)
	}
	if n, err := PruneReports(dir, VerdictRetention, now.Add(time.Second)); err != nil || n != len(DefaultPerspectives) {
		t.Fatalf("prune past boundary removed %d (err %v), want %d", n, err, len(DefaultPerspectives))
	}
	if _, err := PruneReports(filepath.Join(dir, "missing"), VerdictRetention, now); err == nil {
		t.Fatal("missing dir should surface an error to the caller (CollectAndMerge ignores it)")
	}
}

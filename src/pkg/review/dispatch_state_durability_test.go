package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// The dispatch state is the reviewer's ONLY memory of which PRs it has already
// looked at: PlanDispatch keeps no cursor and re-walks the actionable list from
// the top every cycle. Storing it under AgentReportDir put it on the
// container's ephemeral writable layer, so every pod restart wiped it and the
// reviewer re-reviewed the same first PRs forever instead of advancing through
// the queue. Guard the location, not just the read/write round-trip.
func TestReviewDispatchStateIsNotOnScratchStorage(t *testing.T) {
	if got := filepath.Dir(ReviewDispatchStatePath); got == outputschema.AgentReportDir {
		t.Fatalf("dispatch state lives in the regenerable-artifact scratch dir %q; a restart would wipe the reviewer's memory of what it already reviewed", got)
	}
	if !strings.HasPrefix(ReviewDispatchStatePath, DefaultDispatchStateDir+string(os.PathSeparator)) {
		t.Fatalf("dispatch state path %q is not under the durable data dir %q", ReviewDispatchStatePath, DefaultDispatchStateDir)
	}
}

func TestLoadDispatchStateMigratesFromLegacyPath(t *testing.T) {
	oldPath, oldLegacy := ReviewDispatchStatePath, LegacyReviewDispatchStatePath
	t.Cleanup(func() {
		ReviewDispatchStatePath, LegacyReviewDispatchStatePath = oldPath, oldLegacy
	})

	dir := t.TempDir()
	ReviewDispatchStatePath = filepath.Join(dir, "durable", ReviewDispatchStateFile)
	LegacyReviewDispatchStatePath = filepath.Join(dir, "scratch", ReviewDispatchStateFile)

	legacy := DispatchState{Pending: []PendingReview{{Repo: "o/r", Number: 7, HeadSHA: "abc", Perspective: DefaultPerspectives[0], Agent: "reviewer"}}}
	if err := WriteDispatchState(LegacyReviewDispatchStatePath, legacy); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}

	// Upgrade case: nothing at the durable path yet, so the pre-migration
	// record must still be honoured — otherwise the upgrade looks like amnesia
	// and the reviewer restarts its sweep from the top of the queue.
	got, err := LoadDispatchState("")
	if err != nil {
		t.Fatalf("load with only legacy state present: %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0].Number != 7 {
		t.Fatalf("legacy dispatch state not carried forward: %+v", got.Pending)
	}

	// Once the durable file exists it is authoritative and the stale scratch
	// copy is ignored, so the fallback stops firing on its own.
	current := DispatchState{Pending: []PendingReview{{Repo: "o/r", Number: 99, HeadSHA: "def", Perspective: DefaultPerspectives[0], Agent: "reviewer"}}}
	if err := WriteDispatchState("", current); err != nil {
		t.Fatalf("write durable state: %v", err)
	}
	got, err = LoadDispatchState("")
	if err != nil {
		t.Fatalf("load durable state: %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0].Number != 99 {
		t.Fatalf("stale legacy state won over the durable file: %+v", got.Pending)
	}
}

func TestLoadDispatchStateExplicitPathDoesNotFallBack(t *testing.T) {
	oldLegacy := LegacyReviewDispatchStatePath
	t.Cleanup(func() { LegacyReviewDispatchStatePath = oldLegacy })

	dir := t.TempDir()
	LegacyReviewDispatchStatePath = filepath.Join(dir, ReviewDispatchStateFile)
	if err := WriteDispatchState(LegacyReviewDispatchStatePath, DispatchState{Pending: []PendingReview{{Repo: "o/r", Number: 1}}}); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}

	// An explicit path means the caller asked for exactly that file; silently
	// serving a different one would hide a misconfiguration.
	if _, err := LoadDispatchState(filepath.Join(dir, "absent", ReviewDispatchStateFile)); !os.IsNotExist(err) {
		t.Fatalf("explicit missing path: expected not-exist error, got %v", err)
	}
}

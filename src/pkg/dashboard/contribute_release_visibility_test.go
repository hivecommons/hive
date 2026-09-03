package dashboard

import (
	"strings"
	"testing"
)

// contribute_release_visibility_test.go pins the fix for kubestellar/hive#5097.
//
// A task that is RELEASED rather than finished — the socket dropped, or the
// relay asked for new work while still holding one — left no trace in the
// activity feed. The issue showed a "picked up" entry with no terminal event
// ever following it, which reads exactly like an issue nobody touched.
//
// Measured on a live hub: a contributor restarting its relay four times in ten
// minutes touched four different issues (#5056, #5058, #5057, #5055) and
// completed none. Three were abandoned mid-implementation and the hub's own
// history recorded nothing for any of them. "This contributor keeps dropping
// work" was inferable only by noticing an absence.
//
// The verb matters as much as the entry. #4260 established that a dropped socket
// is NOT a failure of the work — booking it as one is what turned three dropped
// sockets on one issue into a quarantine of an issue nobody had failed. These
// entries say "released", and the tests hold them to it.

// TestTaskDescOfRendersGitHubWorkLikeItsPickup keeps the common case aligned: a
// GitHub issue's release reads the same as the "picked up" entry it closes out.
//
// HISTORY: when this test landed (#5097), the two DID diverge for external
// work — the pickup path formatted with %d and rendered a Linear/Jira item as
// "repo#0" while this helper used the canonical key, a divergence the test
// comment flagged as a pre-existing gap worth its own fix. #5120 was that fix:
// every feed entry now renders through assignDesc, and taskDescOf is a typed
// wrapper over it, so the verbs can no longer disagree about the same item.
func TestTaskDescOfRendersGitHubWorkLikeItsPickup(t *testing.T) {
	task := &WSTaskAssign{
		TaskID: "t-1",
		Kind:   "issue",
		Repo:   "hivecommons/hive",
		Number: 5061,
		Title:  "✨ tui T12: poll loop",
	}
	got := taskDescOf(task)
	want := "issue hivecommons/hive#5061: ✨ tui T12: poll loop"
	if got != want {
		t.Errorf("taskDescOf() = %q, want %q (the same shape as the picked-up entry)", got, want)
	}
}

// TestTaskDescOfUsesSourceAwareIdentity covers external work items. A Linear or
// Jira item deliberately carries Number == 0 and puts its identity in
// Key/ExternalID (#4245). An earlier version of this helper treated "numberless"
// as "synthetic" and returned only the internal task id, discarding exactly the
// identity such an item has — every external release would have read as an opaque
// id nobody could match to a work item.
func TestTaskDescOfUsesSourceAwareIdentity(t *testing.T) {
	external := &WSTaskAssign{
		TaskID:     "t-2",
		Kind:       "issue",
		Repo:       "acme/team",
		Number:     0,
		Key:        "acme/team!ENG-123",
		SourceType: "linear",
		ExternalID: "ENG-123",
		Title:      "Ship the poll loop",
	}
	got := taskDescOf(external)
	if !strings.Contains(got, "acme/team!ENG-123") {
		t.Errorf("taskDescOf() = %q, want it to carry the canonical external key", got)
	}
	if !strings.Contains(got, "Ship the poll loop") {
		t.Errorf("taskDescOf() = %q, want it to carry the title", got)
	}
	if got == external.TaskID {
		t.Error("an external work item must not collapse to its internal task id")
	}
	if strings.Contains(got, "#0") {
		t.Errorf("taskDescOf() = %q, must never render a numberless item as issue #0", got)
	}
}

// TestTaskDescOfHandlesSyntheticAndNilTasks covers the two inputs that genuinely
// have no work item behind them. A pr-review sweep carries no canonical key at
// all — not merely no number — and its task id is all it has ever had.
func TestTaskDescOfHandlesSyntheticAndNilTasks(t *testing.T) {
	synthetic := &WSTaskAssign{TaskID: "pr-review-1730", Kind: "review", Repo: "hivecommons/hive"}
	if got := taskDescOf(synthetic); got != "pr-review-1730" {
		t.Errorf("a synthetic task should fall back to its id, got %q", got)
	}
	if strings.Contains(taskDescOf(synthetic), "#0") {
		t.Error("a numberless task must never render as issue #0")
	}
	if got := taskDescOf(nil); got != "" {
		t.Errorf("taskDescOf(nil) = %q, want empty", got)
	}
}

// TestReleaseActivityIsNotRecordedAsAFailure guards the verb choice against a
// well-meaning future edit. A release and a failure have different consequences
// downstream — the failure path increments the consecutive-failure counter that
// quarantines an issue — so the two must stay distinguishable in the feed as
// well as in the ledger.
func TestReleaseActivityIsNotRecordedAsAFailure(t *testing.T) {
	src := fileSource(t, "src/pkg/dashboard/contribute_ws.go")

	for _, want := range []string{
		`"released: connection lost"`,
		`"released: gave the task back"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the release activity verb %s is missing — an abandoned task would be invisible again", want)
		}
	}
	// Both release sites must describe the task; an entry with no task attached
	// says a release happened but not of what, which is barely better than
	// silence.
	if strings.Count(src, "taskDescOf(") < 3 {
		t.Error("a release entry must name the task it released")
	}
}

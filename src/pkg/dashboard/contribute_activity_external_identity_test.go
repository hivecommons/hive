package dashboard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

// contribute_activity_external_identity_test.go pins the fix for
// kubestellar/hive#5120.
//
// Every activity-feed entry derived its label from Number. External work items
// — Linear, Jira — deliberately carry Number == 0 and put their identity in
// Key/ExternalID (#4245), so one Linear ticket was announced as
// "issue acme/team#0: …" when picked up and as a bare internal task id when it
// completed: the two entries for the SAME item did not match each other, and
// every zero-numbered item in a repo rendered identically. The assignment path
// fixed exactly this collision in #4245 (two external items both keying as
// "repo#0" in the double-assignment guard); the display layer kept it.

// TestAssignDescGitHubOutputIsByteIdentical pins the compatibility half: for
// numbered GitHub work the new rendering must be byte-for-byte what the old
// %s#%d format produced, because operators and any log tooling read these
// strings today.
func TestAssignDescGitHubOutputIsByteIdentical(t *testing.T) {
	key := worksource.Ref{Repo: "hivecommons/hive", Number: 5061}.Key()
	got := assignDesc("issue", key, "✨ tui T12: poll loop", "t-1")
	want := fmt.Sprintf("%s %s#%d: %s", "issue", "hivecommons/hive", 5061, "✨ tui T12: poll loop")
	if got != want {
		t.Errorf("assignDesc() = %q, want the old GitHub rendering %q unchanged", got, want)
	}
}

// TestAssignDescCarriesExternalIdentity is the core assertion: an external item
// keeps its canonical key and its title, never collapses to the internal task
// id, and never renders as issue #0.
func TestAssignDescCarriesExternalIdentity(t *testing.T) {
	got := assignDesc("issue", "acme/team!ENG-123", "Ship the poll loop", "t-2")
	if !strings.Contains(got, "acme/team!ENG-123") {
		t.Errorf("assignDesc() = %q, want the canonical external key carried", got)
	}
	if !strings.Contains(got, "Ship the poll loop") {
		t.Errorf("assignDesc() = %q, want the title carried", got)
	}
	if got == "t-2" {
		t.Error("an external item must not collapse to its internal task id")
	}
	if strings.Contains(got, "#0") {
		t.Errorf("assignDesc() = %q, must never render a numberless item as issue #0", got)
	}
}

// TestAssignDescFallsBackOnlyWithoutAnyIdentity covers the one input that
// legitimately has no canonical key: a synthetic pr-review sweep, which has no
// work item behind it. Its task id is all it has ever had.
func TestAssignDescFallsBackOnlyWithoutAnyIdentity(t *testing.T) {
	if got := assignDesc("review", "", "Review open PRs", "pr-review-1730"); got != "pr-review-1730" {
		t.Errorf("assignDesc() with no identity = %q, want the task-id fallback", got)
	}
	// And the Ref spelling of "no identity" — Number 0, no ExternalID — is the
	// empty key, so the same fallback fires without a special case at call sites.
	if key := (worksource.Ref{Repo: "acme/x", Number: 0}).Key(); key != "" {
		t.Errorf("Ref with neither number nor external id should key empty, got %q", key)
	}
}

// TestActivityFeedHasNoNumberDerivedRenderings pins all four call sites at
// once: the %s#%d rendering that produced "repo#0" for external work must not
// exist anywhere in contribute_ws.go. A single site quietly reverting brings
// the mismatched-entries bug back for whichever verb it renders.
func TestActivityFeedHasNoNumberDerivedRenderings(t *testing.T) {
	src := fileSource(t, "src/pkg/dashboard/contribute_ws.go")

	if strings.Contains(src, `fmt.Sprintf("%s %s#%d: %s"`) {
		t.Error("a feed entry still derives its label from Number — external work " +
			"renders as repo#0 or a bare task id again (#5120)")
	}
	// Four verbs, four call sites: picked up, reassigned by yank, completed,
	// failed. The definition plus its doc mention account for the rest.
	if strings.Count(src, "assignDesc(") < 5 {
		t.Error("not every feed entry renders through assignDesc — the verbs will disagree about the same item")
	}
	// The two WSMessage-shaped sites must fall back through the canonical Ref
	// spelling when an older record carries no TaskKey, keying exactly as it
	// always did — not silently dropping to the task id.
	if strings.Count(src, "worksource.Ref{Repo: msg.Repo, Number: msg.Number}.Key()") < 1 ||
		strings.Count(src, "worksource.Ref{Repo: task.Repo, Number: task.Number}.Key()") < 1 {
		t.Error("a WSMessage site lost its Ref fallback for records that predate TaskKey")
	}
}

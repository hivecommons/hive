package github

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#12683: a parked issue carrying a retired park label gets
// stage/085 written first, then the retired label removed, and the
// definitions are deleted once nothing open carries them.
func TestSyncIssueStageLabels_MigratesLegacyDecisionLabels(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{"help wanted", "needs-direction"}}, "filtered", "needs-direction")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{"needs-decision", stagelabels.NeedsDecision.Label()}}, "filtered", "needs-decision")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 3, Labels: []string{"needs-human"}}, "filtered", "needs-human")

	res := syncSeeded(fake, c, result, store, stageNow())
	if res.LegacyDecisionMigrated != 2 {
		t.Fatalf("migrated = %d, want 2 (%+v)", res.LegacyDecisionMigrated, res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.added[1]; strings.Join(got, ",") != stagelabels.NeedsDecision.Label() {
		t.Errorf("issue 1 added %v, want stage/085 only", got)
	}
	if got := fake.removed[1]; strings.Join(got, ",") != "needs-direction" {
		t.Errorf("issue 1 removed %v, want needs-direction", got)
	}
	if len(fake.added[2]) != 0 {
		t.Errorf("issue 2 already at 085, added %v", fake.added[2])
	}
	if got := fake.removed[2]; strings.Join(got, ",") != "needs-decision" {
		t.Errorf("issue 2 removed %v, want needs-decision", got)
	}
	if len(fake.removed[3]) != 0 {
		t.Errorf("needs-human must stay: removed %v", fake.removed[3])
	}
	if got := fake.added[3]; strings.Join(got, ",") != stagelabels.NeedsDecision.Label() {
		t.Errorf("issue 3 added %v, want derived stage/085", got)
	}
}

func TestSyncIssueStageLabels_RetiresLegacyDefinitionsOnlyWhenUncarried(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	// First cycle: issue 1 migrates, but an item the issue cycle never
	// enumerates (a PR, say) still carries needs-decision, so the confirming
	// list-by-label call blocks deletion of both definitions.
	fake.mu.Lock()
	fake.truth[77] = []string{"needs-decision"}
	fake.mu.Unlock()
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{"needs-direction"}}, "filtered", "needs-direction")
	res := syncSeeded(fake, c, result, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 0 {
		t.Fatalf("definitions deleted on a cycle that saw a carrier: %+v", res)
	}
	fake.mu.Lock()
	if len(fake.deletedDefs) != 0 {
		t.Fatalf("deleted %v while a carrier was open", fake.deletedDefs)
	}
	fake.mu.Unlock()

	// Second cycle: nothing carries the retired labels → both definitions go.
	fake.mu.Lock()
	delete(fake.truth, 77)
	fake.mu.Unlock()
	clean := &ActionableResult{}
	appendRepoWorkIssueDetail(clean, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{stagelabels.NeedsDecision.Label()}}, "filtered", NeedsDecisionStageLabel)
	res = syncSeeded(fake, c, clean, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 2 {
		t.Fatalf("definitions deleted = %d, want 2 (%+v)", res.LegacyDecisionDefinitionsDeleted, res)
	}
	fake.mu.Lock()
	got := strings.Join(fake.deletedDefs, ",")
	fake.mu.Unlock()
	if got != "needs-decision,needs-direction" {
		t.Fatalf("deleted definitions = %q", got)
	}

	// Third cycle: once per process.
	res = syncSeeded(fake, c, clean, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 0 {
		t.Fatalf("definition retirement re-ran: %+v", res)
	}
}

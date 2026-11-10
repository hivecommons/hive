package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// mergeListAutoMergeFixture is merge-eligible.json as writeMergeEligible
// writes it at an L6 hive: a PR on a repo whose auto-merge switch is off
// stays in merge_eligible (#13874), next to one on a repo that merges itself.
const mergeListAutoMergeFixture = `{"merge_eligible":[
  {"number":591,"repo":"acme/server","title":"fix the join check"},
  {"number":12,"repo":"acme/widget","title":"ready to merge"}
],"needs_maintainer_merge":[
  {"number":77,"repo":"acme/widget","title":"bot PR","reason":"contributor-lane:no-linked-issue"}
]}`

func mergeListAutoMergeScheduler(t *testing.T, level int) *Scheduler {
	t.Helper()
	orig := mergeEligiblePath
	mergeEligiblePath = filepath.Join(t.TempDir(), "merge-eligible.json")
	t.Cleanup(func() { mergeEligiblePath = orig })
	if err := os.WriteFile(mergeEligiblePath, []byte(mergeListAutoMergeFixture), 0o600); err != nil {
		t.Fatalf("write merge-eligible fixture: %v", err)
	}
	off := false
	return New(&config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org:          "acme",
			Repos:        []string{"acme/server", "acme/widget"},
			PrimaryRepo:  "widget",
			RepoPolicies: []config.RepoPolicy{{Repo: "acme/server", AutoMerge: &off}},
		},
	}, testLogger())
}

// splitMergeList returns the MERGE-ELIGIBLE rows and the NEEDS MAINTAINER
// MERGE section of a rendered list.
func splitMergeList(out string) (eligible, maintainer string) {
	const heading = "NEEDS MAINTAINER MERGE"
	if i := strings.Index(out, heading); i >= 0 {
		return out[:i], out[i:]
	}
	return out, ""
}

// knuckle, 2026-11-10: the scheduled scanner kick listed server, actions,
// website and documentation PRs as MERGE-ELIGIBLE although those repos have
// auto-merge off. The relay refused all nine hive-merge requests and the
// turn ended with no issue work. A PR the relay will refuse belongs under
// NEEDS MAINTAINER MERGE, with the reason.
func TestKickMergeListMovesRepoAutoMergeOffPRsToMaintainer(t *testing.T) {
	s := mergeListAutoMergeScheduler(t, 6)
	eligible, maintainer := splitMergeList(s.buildMergeEligibleListFor(nil, false))

	if !strings.Contains(eligible, "#12 acme/widget — ready to merge") {
		t.Fatalf("PR on an auto-merge repo dropped from MERGE-ELIGIBLE:\n%s", eligible)
	}
	if strings.Contains(eligible, "591") {
		t.Fatalf("PR on a repo with auto-merge off is still listed as MERGE-ELIGIBLE:\n%s", eligible)
	}
	want := "#591 acme/server — fix the join check: " + mergeListRepoAutoMergeOffReason
	if !strings.Contains(maintainer, want) {
		t.Fatalf("NEEDS MAINTAINER MERGE missing %q:\n%s", want, maintainer)
	}
	if !strings.Contains(maintainer, "#77 acme/widget — bot PR: contributor-lane:no-linked-issue") {
		t.Fatalf("existing NEEDS MAINTAINER MERGE row lost:\n%s", maintainer)
	}
}

// The task-MCP kick renders the list as bare refs; the move applies there too.
func TestKickMergeListRefsOnlyMovesRepoAutoMergeOffPRs(t *testing.T) {
	s := mergeListAutoMergeScheduler(t, 6)
	eligible, maintainer := splitMergeList(s.buildMergeEligibleListFor(nil, true))

	if strings.Contains(eligible, "acme/server#591") {
		t.Fatalf("refs-only MERGE-ELIGIBLE still lists the auto-merge-off PR:\n%s", eligible)
	}
	if !strings.Contains(eligible, "acme/widget#12") {
		t.Fatalf("refs-only MERGE-ELIGIBLE dropped the auto-merge PR:\n%s", eligible)
	}
	if !strings.Contains(maintainer, "#591 acme/server") {
		t.Fatalf("refs-only kick lost the moved PR entirely:\n%s", maintainer)
	}
}

// When every eligible PR is on a switched-off repo the list reads "(none)",
// so the agent is not told there is something to merge.
func TestKickMergeListAllAutoMergeOffReadsNone(t *testing.T) {
	s := mergeListAutoMergeScheduler(t, 6)
	off := false
	s.cfg.Project.RepoPolicies = append(s.cfg.Project.RepoPolicies, config.RepoPolicy{Repo: "acme/widget", AutoMerge: &off})
	eligible, _ := splitMergeList(s.buildMergeEligibleListFor(nil, false))
	if strings.TrimSpace(eligible) != "(none)" {
		t.Fatalf("MERGE-ELIGIBLE = %q, want (none)", eligible)
	}
}

// Below L6 every repo reads auto-merge off and the merge lanes are gated by
// agent mode instead, so the list is rendered as before.
func TestKickMergeListBelowL6Unchanged(t *testing.T) {
	s := mergeListAutoMergeScheduler(t, 5)
	eligible, _ := splitMergeList(s.buildMergeEligibleListFor(nil, false))
	for _, want := range []string{"#591 acme/server — fix the join check", "#12 acme/widget — ready to merge"} {
		if !strings.Contains(eligible, want) {
			t.Fatalf("below L6 MERGE-ELIGIBLE missing %q:\n%s", want, eligible)
		}
	}
}

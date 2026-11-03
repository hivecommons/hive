package review

import "testing"

// The two reviewer questions that parked hivecommons/hive#15647 and #15648
// at 085 on 2026-10-29: both name the PR that landed the change, neither
// carried its number anywhere the stage engine read it.
const (
	alreadyLandedQuestion15647 = "@clubanderson should decide whether to close this adoption of #15324 as already landed, since v5 already contains the identical two-line test change via #15506."
	alreadyLandedQuestion15648 = "@clubanderson should decide whether to close this adoption of #15343 as already landed, since #15341 (same author) removed the identical \"Ways to run Hive\" section from src/docs/README.md on v5."
)

func TestAlreadyLandedPRFromReviewerWording(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		{name: "15647 via", text: alreadyLandedQuestion15647, want: 15506},
		{name: "15648 since", text: alreadyLandedQuestion15648, want: 15341},
		{name: "owner/repo ref", text: "This is a no-op: the identical fix merged via hivecommons/hive#15301.", want: 15301},
		{name: "pull url", text: "Already landed through https://github.com/hivecommons/hive/pull/15301 — close?", want: 15301},
		// hivecommons/hive#15732: the question that parked #15544.
		{name: "15544 merged annotation", text: "@clubanderson should decide whether #15544 can be closed as a no-op now that #15545 (merged) carries this change", want: 15545},
		{name: "question mentions a PR but not redundancy", text: "@clubanderson should decide whether the flag introduced in #15506 should default on before this PR merges.", want: 0},
		{name: "lineage mention only", text: "@clubanderson should decide whether to close this adoption of #15324 as already landed.", want: 0},
		{name: "two different landing refs are ambiguous", text: "Already landed via #15506 and via #15507.", want: 0},
		{name: "no text", text: "", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AlreadyLandedPR(0, tc.text); got != tc.want {
				t.Fatalf("AlreadyLandedPR(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
	if got := AlreadyLandedPR(15506, "rename the flag?"); got != 15506 {
		t.Fatalf("marker must win: got %d", got)
	}
}

func TestAlreadyLandedFromVerdict(t *testing.T) {
	landed, claimed := AlreadyLandedFromVerdict(0, alreadyLandedQuestion15647, nil)
	if landed != 15506 || !claimed {
		t.Fatalf("15647: landed=%d claimed=%v", landed, claimed)
	}
	landed, claimed = AlreadyLandedFromVerdict(0, "@clubanderson should confirm this can be closed as a no-op.", []string{"correctness: the base already contains this change."})
	if landed != 0 || !claimed {
		t.Fatalf("claim without a number: landed=%d claimed=%v", landed, claimed)
	}
	landed, claimed = AlreadyLandedFromVerdict(0, "rename the flag to --dry-run?", nil)
	if landed != 0 || claimed {
		t.Fatalf("ordinary question: landed=%d claimed=%v", landed, claimed)
	}
}

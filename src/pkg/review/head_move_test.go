package review

import (
	"strconv"
	"testing"
)

// hivecommons/hive#15823: a pending row follows its PR across a head move
// when the new head is the same patch, so the in-flight review still counts.

func TestRebindPendingHeadMovesOnlyTheNamedPerspective(t *testing.T) {
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "h1", Perspective: PerspectiveCorrectness, Agent: "r1"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "h1", Perspective: PerspectiveSecurity, Agent: "r2"},
		{Repo: "acme/hive", Number: 8, HeadSHA: "h1", Perspective: PerspectiveCorrectness, Agent: "r3"},
	}}
	got, changed := RebindPendingHead(state, "acme/hive", 7, PerspectiveCorrectness, "h1", "h2")
	if !changed {
		t.Fatal("changed = false")
	}
	heads := map[string]string{}
	for _, p := range got.Pending {
		heads[string(p.Perspective)+"#"+strconv.Itoa(p.Number)] = p.HeadSHA
	}
	if heads["correctness#7"] != "h2" || heads["security#7"] != "h1" || heads["correctness#8"] != "h1" {
		t.Fatalf("heads = %v", heads)
	}
	if _, changed := RebindPendingHead(got, "acme/hive", 7, PerspectiveCorrectness, "h1", "h2"); changed {
		t.Fatal("second rebind reported a change")
	}
}

func TestRebindPendingHeadDropsRowWhenPerspectiveAlreadyAtNewHead(t *testing.T) {
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "h1", Perspective: PerspectiveCorrectness, Agent: "r1"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "h2", Perspective: PerspectiveCorrectness, Agent: "r2"},
	}}
	got, changed := RebindPendingHead(state, "acme/hive", 7, "", "h1", "h2")
	if !changed || len(got.Pending) != 1 || got.Pending[0].HeadSHA != "h2" || got.Pending[0].Agent != "r2" {
		t.Fatalf("pending = %+v changed=%v, want the single h2 row kept", got.Pending, changed)
	}
}

func TestCarryPendingAcrossHeadMovesAsksOncePerHeadAndSkipsDifferentPatches(t *testing.T) {
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "h1", Perspective: PerspectiveCorrectness, Agent: "r1"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "h1", Perspective: PerspectiveSecurity, Agent: "r2"},
		{Repo: "acme/hive", Number: 8, HeadSHA: "x1", Perspective: PerspectiveCorrectness, Agent: "r1"},
		{Repo: "acme/hive", Number: 9, HeadSHA: "same", Perspective: PerspectiveCorrectness, Agent: "r1"},
	}}
	prs := []PullRequest{dispatchPRNum(7, "h2"), dispatchPRNum(8, "x2"), dispatchPRNum(9, "same")}
	asked := map[string]int{}
	same := func(repo string, number int, oldHead, newHead string) bool {
		asked[repo+"#"+strconv.Itoa(number)+"@"+oldHead+"->"+newHead]++
		return number == 7
	}

	got, rebinds := CarryPendingAcrossHeadMoves(state, prs, "acme", same)

	if asked["acme/hive#7@h1->h2"] != 1 || asked["acme/hive#8@x1->x2"] != 1 || len(asked) != 2 {
		t.Fatalf("samePatch calls = %v, want one per moved PR head", asked)
	}
	if len(rebinds) != 2 {
		t.Fatalf("rebinds = %+v, want both PR 7 perspectives", rebinds)
	}
	for _, p := range got.Pending {
		switch p.Number {
		case 7:
			if p.HeadSHA != "h2" {
				t.Fatalf("PR 7 %s head = %s, want h2", p.Perspective, p.HeadSHA)
			}
		case 8:
			if p.HeadSHA != "x1" {
				t.Fatalf("PR 8 (different patch) head = %s, want untouched x1", p.HeadSHA)
			}
		}
	}
}

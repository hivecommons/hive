package dashboard

import (
	"strings"
	"testing"
)

// A `blocked` issue whose enumeration reason is owner-action reads Needs
// human with the reason line, not Actionable.
func TestStageTrackerBlockedOwnerNeedsHuman(t *testing.T) {
	it := StageTrackerItem{Repo: "hivecommons/benchmarks", Number: 1, Kind: "issue", Stage: 10, Caption: "Actionable", Modifiers: []string{modBlocked}}
	stageTrackerBlockedOnPR(&it, "", "blocked owner-action: grant the hub app contents:write on o/other", "", nil)
	const wantLine = "needs an owner action: grant the hub app contents:write on o/other"
	if it.BlockedOwnerReason != wantLine {
		t.Fatalf("BlockedOwnerReason = %q, want %q", it.BlockedOwnerReason, wantLine)
	}
	if !strings.Contains(it.Caption, "Needs human: "+wantLine) {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.BlockedOnPR != nil || len(it.BlockedOn) != 0 {
		t.Fatalf("owner block must not name a PR or issue: %+v", it)
	}
	if !stageTrackerNeedsHuman(it) {
		t.Fatal("owner-action block is not Needs human")
	}
	items := []StageTrackerItem{it}
	if n, _ := stageTrackerHumanCounts(items); n != 1 || !items[0].NeedsHuman {
		t.Fatalf("needs human count = %d, flag = %v", n, items[0].NeedsHuman)
	}
	// A hold still wins, as for every Needs human wait.
	held := it
	held.Modifiers = []string{modBlocked, modHold}
	if stageTrackerNeedsHuman(held) {
		t.Fatal("held owner-action block must not be Needs human")
	}
	// A reasonless marker keeps the bare phrase.
	bare := StageTrackerItem{Repo: "o/r", Number: 2, Kind: "issue", Stage: 0, Modifiers: []string{modBlocked}}
	stageTrackerBlockedOnPR(&bare, "", "blocked owner-action:", "", nil)
	if bare.BlockedOwnerReason != stageTrackerBlockedOwnerPrefix || bare.Caption != "Needs human: needs an owner action" {
		t.Fatalf("bare = %q / %q", bare.BlockedOwnerReason, bare.Caption)
	}
	// An issue with no blocked label is untouched.
	plain := StageTrackerItem{Repo: "o/r", Number: 3, Kind: "issue"}
	stageTrackerBlockedOnPR(&plain, "", "blocked owner-action: x", "", nil)
	if plain.BlockedOwnerReason != "" {
		t.Fatal("unblocked row picked up an owner reason")
	}
	if needsHumanSourceBlockedOnOwner != "blocked-on-owner" {
		t.Fatal("source name drifted")
	}
}

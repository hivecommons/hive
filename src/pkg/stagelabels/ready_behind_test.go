package stagelabels

import (
	"strings"
	"testing"
)

// projectbluefin/utah#1072 (head c38126c, 2026-11-06/07): every required
// check was green, but GitHub reported the head BEHIND a `main` that
// requires up-to-date branches, and the 090 comment still said "Blocked
// only by `role/untrusted`" and offered "(or merge)". The comment now names
// the base merge as a second blocker; the stage stays 090.
func TestReadyCommentNamesBehindBase(t *testing.T) {
	e := Evidence{Head: "c38126c79dc536223897c2a99fdf9cb105f5254a", Verdict: VerdictApproved, Cause: CauseTrust,
		Checks: CheckSummary{Known: true, Total: 20, Green: 20}, BehindBase: "main"}
	r := RouteHuman(e)
	if r.Stage != ReadyForHumanMerge {
		t.Fatalf("stage = %v, want 090: behind is not a stage of its own", r.Stage)
	}
	if r.BehindBase != "main" {
		t.Fatalf("route BehindBase = %q, want main", r.BehindBase)
	}
	if !strings.Contains(r.Summary, "20/20 checks green, behind `main`") {
		t.Fatalf("summary = %q", r.Summary)
	}
	got := ReadyComment(r, "`role/untrusted` (no verified hive claim or registry tier)")
	for _, want := range []string{
		"Blocked by `role/untrusted` (no verified hive claim or registry tier).",
		"The branch is also behind `main`, which requires up-to-date branches: GitHub will not merge this head until `main` is merged into it.",
		"`/lgtm` `/approve` if you accept it; the branch still has to be brought up to date before it can merge.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Blocked only by", "(or merge)"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("comment still says %q while behind:\n%s", unwanted, got)
		}
	}

	// Up to date: the comment reads exactly as before.
	e.BehindBase = ""
	r = RouteHuman(e)
	got = ReadyComment(r, "`role/untrusted` (no verified hive claim or registry tier)")
	if r.BehindBase != "" || strings.Contains(r.Summary, "behind") ||
		!strings.Contains(got, "Blocked only by `role/untrusted` (no verified hive claim or registry tier).") ||
		!strings.Contains(got, "`/lgtm` `/approve` (or merge) if you accept it.") {
		t.Fatalf("up-to-date comment changed:\n%s", got)
	}
}

// A behind head whose required checks have not reported still waits at 060
// on the checks (projectbluefin/utah#1074): the behind note never lifts a
// head to 090.
func TestRouteHumanBehindWithMissingRequiredStillWaits(t *testing.T) {
	e := Evidence{Head: "0c56fd61c0000000000000000000000000000000", Verdict: VerdictApproved, Cause: CauseTrust,
		Checks: CheckSummary{Known: true, Total: 4, Green: 4, Missing: []string{"contract"}}, BehindBase: "main"}
	r := RouteHuman(e)
	if r.Stage != Reviewed {
		t.Fatalf("stage = %v, want 060 while a required check has not reported", r.Stage)
	}
	if !strings.Contains(r.Summary, "required not reported: contract") || !strings.Contains(r.Summary, "behind `main`") {
		t.Fatalf("summary = %q", r.Summary)
	}
}

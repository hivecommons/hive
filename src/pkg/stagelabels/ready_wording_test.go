package stagelabels

import (
	"strings"
	"testing"
)

// projectbluefin/website#1036 and projectbluefin/actions#757 (11-16/17):
// on a repo whose hive auto-merge is off, and which runs no Prow, the 090
// comment said "Blocked only by `role/trusted`" and asked for `/lgtm`
// `/approve`. A 090 a person merges for another reason now states that
// reason (Route.HumanMergeWhy), and a repo without Prow (Route.NoProw) is
// asked for a GitHub approve and merge. A Prow repo's trust-blocked text
// is unchanged.
func TestReadyCommentWordingByCauseAndProw(t *testing.T) {
	const trust = "`role/trusted`"
	const autoMergeOff = "Auto-merge is off for this repo, so a person merges"
	base := RouteHuman(Evidence{Head: "6cc067b0000000000000000000000000000000", Verdict: VerdictApproved, Cause: CauseTrust,
		Checks: CheckSummary{Known: true, Total: 5, Green: 5}})
	for _, tc := range []struct {
		name    string
		route   func(Route) Route
		want    []string
		notWant []string
	}{
		{
			name:    "prow, trust: unchanged",
			route:   func(r Route) Route { return r },
			want:    []string{"Blocked only by `role/trusted`.", "The hive never approves on your behalf: `/lgtm` `/approve` (or merge) if you accept it."},
			notWant: []string{"on GitHub"},
		},
		{
			name:    "no prow, trust",
			route:   func(r Route) Route { r.NoProw = true; return r },
			want:    []string{"Blocked only by `role/trusted`.", "The hive never approves on your behalf: approve and merge it on GitHub if you accept it."},
			notWant: []string{"/lgtm", "/approve", "tide"},
		},
		{
			name:    "no prow, auto-merge off",
			route:   func(r Route) Route { r.NoProw = true; r.HumanMergeWhy = autoMergeOff; return r },
			want:    []string{". " + autoMergeOff + ".", "approve and merge it on GitHub if you accept it."},
			notWant: []string{"Blocked", "role/", "/lgtm", "/approve", "tide"},
		},
		{
			name:    "prow, auto-merge off",
			route:   func(r Route) Route { r.HumanMergeWhy = autoMergeOff + "."; return r },
			want:    []string{autoMergeOff + ".", "`/lgtm` `/approve` (or merge) if you accept it."},
			notWant: []string{"Blocked", "role/", "merges.."},
		},
		{
			name:    "no prow, behind base",
			route:   func(r Route) Route { r.NoProw = true; r.BehindBase = "main"; return r },
			want:    []string{"Blocked by `role/trusted`.", "GitHub will not merge this head until `main` is merged into it.", "approve it on GitHub if you accept it; the branch still has to be brought up to date"},
			notWant: []string{"/lgtm", "/approve", "(or merge)"},
		},
		{
			name:    "no prow, auto-merge off and behind base",
			route:   func(r Route) Route { r.NoProw = true; r.HumanMergeWhy = autoMergeOff; r.BehindBase = "main"; return r },
			want:    []string{autoMergeOff + ". The branch is also behind `main`", "approve it on GitHub if you accept it"},
			notWant: []string{"Blocked", "role/", "/lgtm"},
		},
		{
			name: "no prow, held",
			route: func(r Route) Route {
				r.NoProw = true
				r.Gates = []string{"a sentinel alert is open on this PR"}
				return r
			},
			want:    []string{"Held: a sentinel alert is open on this PR.", "then approve and merge it on GitHub if you accept it."},
			notWant: []string{"/lgtm", "/approve"},
		},
		{
			name:    "prow, held: unchanged",
			route:   func(r Route) Route { r.Gates = []string{"a sentinel alert is open on this PR"}; return r },
			want:    []string{"then `/lgtm` `/approve` (or merge) if you accept it."},
			notWant: []string{"on GitHub"},
		},
		{
			name: "no prow, manual merge",
			route: func(r Route) Route {
				r.NoProw = true
				r.ManualMerge = true
				r.Reasons = []string{"the PR edits CI workflow / action files"}
				return r
			},
			want:    []string{"click **Merge** to land it."},
			notWant: []string{"tide", "/lgtm"},
		},
		{
			name: "prow, manual merge: unchanged",
			route: func(r Route) Route {
				r.ManualMerge = true
				r.Reasons = []string{"the PR edits CI workflow / action files"}
				return r
			},
			want: []string{"click **Merge** (or `/lgtm` + `/approve` for tide) to land it."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadyComment(tc.route(base), trust)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("comment lacks %q:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Fatalf("comment says %q:\n%s", nw, got)
				}
			}
		})
	}
}

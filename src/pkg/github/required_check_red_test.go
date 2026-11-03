package github

import (
	"context"
	"testing"
)

// A red check outside the required set is advisory: GitHub calls the PR
// unstable, not blocked, and the automerge sweep merges it. The fix loop's red
// predicate must agree, or it spends the PR's whole re-dispatch budget on a
// failure nothing can or needs to repair (Danathar/tududi#467: a
// continue-on-error mutation job, 6 FIX-BEFORE-NEW kicks on one head, then
// needs-human on a mergeable PR).
func TestEnrichCIStatus_RequiredRedHonoursTheRequiredSet(t *testing.T) {
	const advisory = "Mutation check on changed lines (advisory)"
	run := func(name, conclusion string) map[string]any {
		return map[string]any{"name": name, "status": "completed", "conclusion": conclusion}
	}
	for _, tc := range []struct {
		name      string
		runs      []map[string]any
		statuses  []string
		required  map[string]bool
		wantRed   bool
		wantFacts bool // RequiredChecksAllReported
	}{
		{
			name:      "only a non-required check failing",
			runs:      []map[string]any{run("test", "success"), run(advisory, "failure")},
			required:  map[string]bool{"test": true},
			wantRed:   false,
			wantFacts: true,
		},
		{
			name:      "a required check failing",
			runs:      []map[string]any{run("test", "failure"), run(advisory, "failure")},
			required:  map[string]bool{"test": true},
			wantRed:   true,
			wantFacts: true,
		},
		{
			name:    "required set unknown fails closed",
			runs:    []map[string]any{run("test", "success"), run(advisory, "failure")},
			wantRed: true,
		},
		{
			name:     "a required check that never reported fails closed",
			runs:     []map[string]any{run("test", "success"), run(advisory, "failure")},
			required: map[string]bool{"test": true, "validate": true},
			wantRed:  true,
		},
		{
			// The required context is a commit status, which the check-run
			// listing cannot see: nothing proves it is not the red one.
			name:     "a required commit status fails closed",
			runs:     []map[string]any{run(advisory, "failure")},
			statuses: []string{"validate"},
			required: map[string]bool{"validate": true},
			wantRed:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &protectionServer{mergeableState: "unstable", checkRuns: tc.runs, statuses: tc.statuses}
			pr := enrichOnePR(t, p, tc.required)
			if pr.CIStatus != "failure" || !pr.HasFailingCheck() {
				t.Fatalf("CIStatus = %q, HasFailingCheck = %v; want a failing PR either way", pr.CIStatus, pr.HasFailingCheck())
			}
			if got := pr.HasFailingRequiredCheck(); got != tc.wantRed {
				t.Errorf("HasFailingRequiredCheck = %v, want %v (failing %v, required %v)", got, tc.wantRed, pr.FailingChecks, tc.required)
			}
			gotFacts := pr.Protection != nil && pr.Protection.RequiredChecksAllReported
			if gotFacts != tc.wantFacts {
				t.Errorf("RequiredChecksAllReported = %v, want %v", gotFacts, tc.wantFacts)
			}
		})
	}
}

// A known-empty required set (an unprotected base, or protection without
// required checks) enforces nothing, so an empty FailingRequiredChecks proves
// nothing about the red check: the automerge sweep blocks completed failures
// there, and the PR is still its fixer's.
func TestProtectionFacts_KnownEmptyRequiredSetFailsClosed(t *testing.T) {
	p := &protectionServer{mergeableState: "unstable", checkRuns: []map[string]any{
		{"name": "advisory", "status": "completed", "conclusion": "failure"},
	}}
	c := newTestClient(t, p.start(t), "org", []string{"repo"})
	ctx := context.Background()
	pr := PullRequest{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"}
	reported := c.enrichPRCI(ctx, &pr)
	pc := newProtectionCollector(c)
	pc.required["org/repo@v4"] = requiredSet{set: map[string]bool{}, known: true}
	pc.attach(ctx, &pr, reported)

	if pr.Protection == nil || !pr.Protection.RequiredChecksKnown {
		t.Fatalf("Protection = %+v, want a known (empty) required set", pr.Protection)
	}
	if pr.Protection.RequiredChecksAllReported {
		t.Error("RequiredChecksAllReported = true for an empty required set")
	}
	if !pr.HasFailingRequiredCheck() {
		t.Error("HasFailingRequiredCheck = false on a known-empty required set; want fail closed")
	}
}

func TestHasFailingRequiredCheck_Facts(t *testing.T) {
	red := PullRequest{CIStatus: "failure", FailingChecks: []string{"advisory"}}
	for _, tc := range []struct {
		name  string
		facts *ProtectionFacts
		want  bool
	}{
		{"no protection facts", nil, true},
		{"required set not known", &ProtectionFacts{RequiredChecksAllReported: true}, true},
		// A known-empty set enforces nothing: the automerge sweep blocks on
		// completed failures there, so the PR is still the fixer's.
		{"known-empty required set", &ProtectionFacts{RequiredChecksKnown: true}, true},
		{"required check failing", &ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true, FailingRequiredChecks: []string{"test"}}, true},
		{"every required check reported, none failing", &ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := red
			pr.Protection = tc.facts
			if got := pr.HasFailingRequiredCheck(); got != tc.want {
				t.Errorf("HasFailingRequiredCheck = %v, want %v", got, tc.want)
			}
			if !pr.HasFailingCheck() {
				t.Error("HasFailingCheck = false; it must not consult the required set")
			}
		})
	}

	green := PullRequest{CIStatus: "success", Protection: &ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true}}
	if green.HasFailingCheck() || green.HasFailingRequiredCheck() {
		t.Error("a green PR must not be red")
	}
}

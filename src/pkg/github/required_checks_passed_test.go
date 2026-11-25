package github

import "testing"

// RequiredChecksPassed is the escalation ledger's proof that a head red only
// on advisory checks has nothing left that a fixer or a person must repair:
// every required check completed on the head with a passing conclusion. The
// weaker RequiredChecksAllReported also counts a running, cancelled or
// timed-out required check as reported, which is fine for "is a required
// check red" but not for "is every required check green" (Danathar/tududi#658).
func TestEnrichCIStatus_RequiredChecksPassed(t *testing.T) {
	const advisory = "Mutation check (advisory)"
	done := func(name, conclusion string) map[string]any {
		return map[string]any{"name": name, "status": "completed", "conclusion": conclusion}
	}
	for _, tc := range []struct {
		name        string
		runs        []map[string]any
		statuses    []string
		statusState string
		required    map[string]bool
		want        bool
	}{
		{
			name:     "every required check succeeded, advisory red",
			runs:     []map[string]any{done("test", "success"), done("lint", "success"), done(advisory, "failure")},
			required: map[string]bool{"test": true, "lint": true},
			want:     true,
		},
		{
			name:     "neutral and skipped required checks pass",
			runs:     []map[string]any{done("test", "neutral"), done("lint", "skipped"), done(advisory, "failure")},
			required: map[string]bool{"test": true, "lint": true},
			want:     true,
		},
		{
			name:     "a successful required commit status passes",
			runs:     []map[string]any{done(advisory, "failure")},
			statuses: []string{"validate"},
			required: map[string]bool{"validate": true},
			want:     true,
		},
		{
			name:     "a cancelled required check",
			runs:     []map[string]any{done("test", "cancelled"), done(advisory, "failure")},
			required: map[string]bool{"test": true},
		},
		{
			name:     "a timed-out required check",
			runs:     []map[string]any{done("test", "timed_out"), done(advisory, "failure")},
			required: map[string]bool{"test": true},
		},
		{
			name:     "a stale required check",
			runs:     []map[string]any{done("test", "stale"), done(advisory, "failure")},
			required: map[string]bool{"test": true},
		},
		{
			name:     "a required check still running",
			runs:     []map[string]any{{"name": "test", "status": "in_progress"}, done(advisory, "failure")},
			required: map[string]bool{"test": true},
		},
		{
			name:     "a failed required check",
			runs:     []map[string]any{done("test", "failure"), done(advisory, "failure")},
			required: map[string]bool{"test": true},
		},
		{
			name:     "a required check with no run",
			runs:     []map[string]any{done("test", "success"), done(advisory, "failure")},
			required: map[string]bool{"test": true, "validate": true},
		},
		{
			name:        "a pending required commit status",
			runs:        []map[string]any{done(advisory, "failure")},
			statuses:    []string{"validate"},
			statusState: "pending",
			required:    map[string]bool{"validate": true},
		},
		{
			name: "required set unknown",
			runs: []map[string]any{done("test", "success"), done(advisory, "failure")},
		},
		{
			name:     "known-empty required set",
			runs:     []map[string]any{done("test", "success"), done(advisory, "failure")},
			required: map[string]bool{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &protectionServer{mergeableState: "unstable", checkRuns: tc.runs, statuses: tc.statuses, statusState: tc.statusState}
			pr := enrichOnePR(t, p, tc.required)
			got := pr.Protection != nil && pr.Protection.RequiredChecksPassed
			if got != tc.want {
				t.Errorf("RequiredChecksPassed = %v, want %v (protection %+v)", got, tc.want, pr.Protection)
			}
		})
	}
}

// requiredCheckPassed reads only what the pass holds: no activity, no
// observation of the name, or an observation whose producer the policy cannot
// attribute is never a pass.
func TestRequiredCheckPassed_FailsClosed(t *testing.T) {
	app := int64(15368)
	other := int64(99)
	ok := PRActivityCheck{Name: "test", State: "completed", Conclusion: "success", AppID: &other}
	if requiredCheckPassed(nil, "test") {
		t.Error("nil activity passed")
	}
	if requiredCheckPassed(&PRCIActivity{Head: "abc"}, "test") {
		t.Error("no observation passed")
	}
	if !requiredCheckPassed(&PRCIActivity{Head: "abc", Checks: []PRActivityCheck{ok}}, "test") {
		t.Error("a successful unbound check did not pass")
	}
	unattributed := ok
	unattributed.AppID = nil
	pinned := &PRCIActivity{
		Head:           "abc",
		Checks:         []PRActivityCheck{unattributed},
		RequiredChecks: []PRRequiredCheck{{Name: "test", AppID: &app, AppKnown: true}},
	}
	if requiredCheckPassed(pinned, "test") {
		t.Error("a check from an unattributed producer passed an App-pinned requirement")
	}
	mixed := &PRCIActivity{Head: "abc", Checks: []PRActivityCheck{ok, {Name: "test", State: "completed", Conclusion: "cancelled"}}}
	if requiredCheckPassed(mixed, "test") {
		t.Error("a cancelled observation beside a success passed")
	}
}

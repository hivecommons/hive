package github

import (
	"reflect"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func trailRun(name, status, conclusion string) *gh.CheckRun {
	cr := &gh.CheckRun{Name: gh.Ptr(name), Status: gh.Ptr(status), App: &gh.App{ID: gh.Ptr(int64(1))}}
	if conclusion != "" {
		cr.Conclusion = gh.Ptr(conclusion)
	}
	return cr
}

func trailStatus(context, state string) *gh.RepoStatus {
	return &gh.RepoStatus{Context: gh.Ptr(context), State: gh.Ptr(state)}
}

// A red check outside the required set must not make a head red: the hive's
// merge gate ignores it, so the CI-gate panel must too (#12610). Required
// contexts that never reported are missing, and a DCO context is satisfied
// by any sibling DCO success.
func TestPRCIHeadReadJudgesRequiredChecksOnly(t *testing.T) {
	required := map[string]bool{"gate": true, "test": true, "coverage": true, "DCO": true}
	runs := []*gh.CheckRun{
		trailRun("gate", "completed", "success"),
		trailRun("test", "completed", "success"),
		trailRun("coverage", "completed", "success"),
		trailRun("DCO", "completed", "action_required"),
		trailRun("PR Type", "completed", "failure"),
		trailRun("docs-links", "completed", "cancelled"),
	}
	statuses := []*gh.RepoStatus{trailStatus("dco", "success"), trailStatus("tide", "pending")}

	h := prCIHeadRead(statuses, runs, required)
	if h.State != PRCIHeadGreen {
		t.Fatalf("state = %q, want green (non-required reds must not block): %+v", h.State, h)
	}
	if want := []string{"PR Type", "docs-links"}; !reflect.DeepEqual(h.NonRequiredRed, want) {
		t.Fatalf("NonRequiredRed = %v, want %v", h.NonRequiredRed, want)
	}
	if len(h.RequiredRed) != 0 || len(h.MissingRequired) != 0 {
		t.Fatalf("unexpected required red/missing: %+v", h)
	}

	// A required check failing is red regardless of everything else.
	runs[1] = trailRun("test", "completed", "failure")
	h = prCIHeadRead(statuses, runs, required)
	if h.State != PRCIHeadRed || !reflect.DeepEqual(h.RequiredRed, []string{"test"}) {
		t.Fatalf("required red: %+v", h)
	}

	// A required check that never reported is pending (missing), not green.
	h = prCIHeadRead(statuses, runs[:1], required)
	if h.State != PRCIHeadPending {
		t.Fatalf("missing required: state = %q, want pending: %+v", h.State, h)
	}
	if want := []string{"coverage", "test"}; !reflect.DeepEqual(h.MissingRequired, want) {
		t.Fatalf("MissingRequired = %v, want %v", h.MissingRequired, want)
	}

	// Nothing at all on the head is none, with no missing list (CI never saw it).
	h = prCIHeadRead(nil, nil, required)
	if h.State != PRCIHeadNone || h.MissingRequired != nil {
		t.Fatalf("empty head: %+v", h)
	}

	// Without a required set the pre-existing any-check reading applies.
	h = prCIHeadRead(statuses, runs, nil)
	if h.State != PRCIHeadRed {
		t.Fatalf("fallback any-check state = %q, want red", h.State)
	}
}

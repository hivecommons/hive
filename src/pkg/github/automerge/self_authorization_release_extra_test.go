package automerge

// Guard-rail tests for the #5117 hold-release budget and its failure paths:
// the budget must be OFF unless the hive is explicitly at the skip level, and
// a failing eligibility lookup must skip the PR rather than releasing a hold
// on incomplete evidence.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestSelfAuthorizationReleaseBudgetGating(t *testing.T) {
	l6 := hgithub.SelfAuthorizationSkipACMMLevel
	l5 := hgithub.SelfAuthorizationSkipACMMLevel - 1
	cases := []struct {
		name  string
		level *int
		limit int
		want  int
	}{
		{"nil level is off", nil, 5, 0},
		{"below skip level is off", &l5, 5, 0},
		{"skip level uses the default limit", &l6, 0, defaultSelfAuthorizationReleaseLimit},
		{"skip level honours a custom limit", &l6, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Engine{selfAuthorizationACMMLevel: tc.level, selfAuthorizationReleaseLimit: tc.limit}
			if got := c.selfAuthorizationReleaseBudget(); got != tc.want {
				t.Fatalf("selfAuthorizationReleaseBudget() = %d, want %d", got, tc.want)
			}
		})
	}
	var nilEngine *Engine
	if got := nilEngine.selfAuthorizationReleaseBudget(); got != 0 {
		t.Fatalf("nil engine budget = %d, want 0", got)
	}
}

func TestReleaseSelfAuthorizationHoldIsInertWithoutAClient(t *testing.T) {
	released, err := (&Engine{}).releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || released {
		t.Fatalf("releaseSelfAuthorizationHoldIfEligible = (%v, %v), want inert (false, nil)", released, err)
	}
}

func TestSweepSelfAuthoredAutoMergesSkipsHeldPRWhenEligibilityLookupFails(t *testing.T) {
	var merged []int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			w.Write([]byte(`[{"number":11,"user":{"login":"` + testHiveAppBotLogin + `"},"labels":[{"name":"hold"}]}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/11/comments"):
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	l6 := hgithub.SelfAuthorizationSkipACMMLevel
	c.selfAuthorizationACMMLevel = &l6
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if result.Skipped != 1 || len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("result=%+v, want failing eligibility lookup to skip the held PR", result)
	}
}

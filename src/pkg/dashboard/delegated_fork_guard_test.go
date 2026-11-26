package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The delegate's fork-guard prefix is the stage engine's park reason.
func TestDelegatedForkGuardPrefixIsTheStageReason(t *testing.T) {
	if delegatedecision.ReasonForkGuardPrefix != stagelabels.LaneReasonForkGuardPrefix {
		t.Fatalf("delegatedecision.ReasonForkGuardPrefix = %q, stagelabels.LaneReasonForkGuardPrefix = %q",
			delegatedecision.ReasonForkGuardPrefix, stagelabels.LaneReasonForkGuardPrefix)
	}
}

// A PR parked for another reason (here an adoption that stalled on the
// fork) whose red check is fork-guarded carries the guard into the
// delegate's input, from the stage record or from the snapshot's copy of
// the PR at this head. Only the guarded red checks are named, so a guard
// beside another red check is still the maintainer's step; a same-repo head
// or a snapshot of an older head names none.
func TestDelegatedForkGuard(t *testing.T) {
	const repo, number, head = "acme/widget", 42, "abc1234def"
	key := stagelabels.Key(repo, number)
	recorded := func() *stagelabels.Store {
		store := stagelabels.NewStore("", nil)
		store.ObserveHead(key, head, stagelabels.NeedsDecision, "adoption-stalled:fork-rehome-refused")
		store.NoteForkGuard(key, []string{"publish"})
		return store
	}
	parkedOnGuard := func() *stagelabels.Store {
		store := stagelabels.NewStore("", nil)
		store.ObserveHead(key, head, stagelabels.NeedsDecision, "fork-guard:publish")
		return store
	}
	snapshot := func(at string) *StatusPayload {
		return &StatusPayload{Repos: []FrontendRepo{{Full: repo, OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{
			Repo: repo, Number: number, HeadSHA: at, FromFork: true, CIStatus: "failure",
			FailingChecks: []string{"publish"}, CIFailureExcerpt: "publish: a fork pull request cannot publish the image; a maintainer runs it",
		}}}}}}
	}
	for _, tc := range []struct {
		name   string
		store  *stagelabels.Store
		status *StatusPayload
		fork   bool
		red    []string
		want   []string
	}{
		{name: "recorded guard, park reason elsewhere", store: recorded(), status: &StatusPayload{}, fork: true, red: []string{"publish"}, want: []string{"publish"}},
		{name: "fork-guard park reason", store: parkedOnGuard(), status: &StatusPayload{}, fork: true, red: []string{"publish"}, want: []string{"publish"}},
		{name: "snapshot at this head", store: stagelabels.NewStore("", nil), status: snapshot(head), fork: true, red: []string{"publish"}, want: []string{"publish"}},
		{name: "snapshot of an older head", store: stagelabels.NewStore("", nil), status: snapshot("0ld0ld0"), fork: true, red: []string{"publish"}},
		{name: "another check red beside the guard: only the guard is named", store: recorded(), status: snapshot(head), fork: true, red: []string{"lint", "publish"}, want: []string{"publish"}},
		{name: "nothing known guarded", store: stagelabels.NewStore("", nil), status: &StatusPayload{}, fork: true, red: []string{"lint"}},
		{name: "same-repo head", store: recorded(), status: snapshot(head), red: []string{"publish"}},
		{name: "nothing red", store: recorded(), status: snapshot(head), fork: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFullServer(t)
			s.stageStore = tc.store
			s.status = tc.status
			if got := s.delegatedForkGuard(repo, number, head, tc.fork, tc.red); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("delegatedForkGuard = %v, want %v", got, tc.want)
			}
		})
	}
}

// End to end through gatherDelegatedInput: a contributor's fork PR parked
// on adoption-stalled:fork-rehome-refused, its author offline, whose only
// red check the stage record knows as fork-guarded. The input carries the
// guard, and "Let hive decide" recommends the maintainer's step and keeps
// the park instead of asking anyone to make CI green.
func TestGatherDelegatedForkGuardFromAnotherPark(t *testing.T) {
	const head = "abc1234def"
	old := time.Now().Add(-72 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/42"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "pull_request": map[string]any{}, "user": map[string]string{"login": "contributor"}})
		case strings.HasSuffix(r.URL.Path, "/pulls/42"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "user": map[string]string{"login": "contributor"},
				"head":       map[string]any{"sha": head, "repo": map[string]string{"full_name": "contributor/widget"}},
				"base":       map[string]any{"ref": "main", "repo": map[string]string{"full_name": "acme/widget"}},
				"created_at": old, "mergeable": true, "mergeable_state": "blocked"})
		case strings.HasSuffix(r.URL.Path, "/commits/"+head+"/status"):
			_, _ = w.Write([]byte(`{"state":"failure","statuses":[{"context":"publish","state":"failure"}]}`))
		case strings.HasSuffix(r.URL.Path, "/commits/"+head+"/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		case strings.Contains(r.URL.Path, "/permission"):
			_, _ = w.Write([]byte(`{"permission":"read"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "acme", nil, slog.Default(), server.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	s.status = &StatusPayload{}
	s.stageStore = stagelabels.NewStore("", nil)
	key := stagelabels.Key("acme/widget", 42)
	s.stageStore.ObserveHead(key, head, stagelabels.NeedsDecision, "adoption-stalled:fork-rehome-refused")
	s.stageStore.NoteForkGuard(key, []string{"publish"})

	in, err := s.gatherDelegatedInput(context.Background(), "acme/widget", 42, "pr", delegatedDecisionRequest{Reason: "adoption-stalled:fork-rehome-refused"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"publish"}; !reflect.DeepEqual(in.ForkGuardChecks, want) || !reflect.DeepEqual(in.RedChecks, want) {
		t.Fatalf("ForkGuardChecks = %v, RedChecks = %v, want %v for both", in.ForkGuardChecks, in.RedChecks, want)
	}
	for _, r := range in.Reasons {
		if strings.HasPrefix(r, delegatedecision.ReasonForkGuardPrefix) {
			t.Fatalf("the park reason is not a fork guard, yet Reasons carry %q", r)
		}
	}
	d := delegatedecision.Run(in)
	if d.Rule != delegatedecision.RuleForkGuard || !d.KeepsPark || strings.Contains(d.Text, "make CI green") || !strings.Contains(d.Text, "`publish`") {
		t.Fatalf("decision = rule %s keepsPark %v text %q", d.Rule, d.KeepsPark, d.Text)
	}
}

// The 085 question for a fork-guard park points at the checks' own
// messages, in the plural for more than one check.
func TestDecisionForPRForkGuardQuestion(t *testing.T) {
	for reason, want := range map[string]string{
		"fork-guard:publish":      "CI will not run for this fork PR (publish). A maintainer must act on the fork-guarded check as its own message asks (run or admit it for this head, or push the branch to this repo), or close it?",
		"fork-guard:publish,sign": "CI will not run for this fork PR (publish, sign). A maintainer must act on the fork-guarded checks as their own messages ask (run or admit them for this head, or push the branch to this repo), or close it?",
	} {
		it := StageTrackerItem{Repo: "acme/widget", Number: 42, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "abc1234def", Reason: reason}
		if d := decisionForPR(it, nil, "", false); d == nil || d.Question != want {
			t.Errorf("%s: decision = %+v, want question %q", reason, d, want)
		}
	}
}

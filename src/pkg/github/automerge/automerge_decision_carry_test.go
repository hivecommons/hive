package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// baseRefreshAPI is the slice of GitHub the decision carry reads: the merge
// commit, the PR (for its base branch) and the base...head compares.
type baseRefreshAPI struct {
	mu          sync.Mutex
	mergeAuthor string
	oldHead     string
	newHead     string
	baseTip     string
	newFiles    []map[string]any
	calls       map[string]int
}

func refreshFiles(blob string) []map[string]any {
	return []map[string]any{{"filename": "src/pkg/proxy/private.go", "status": "modified", "sha": blob, "additions": 3, "deletions": 1, "changes": 4, "patch": "@@ -1 +1 @@ " + blob}}
}

func (a *baseRefreshAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.calls == nil {
			a.calls = map[string]int{}
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/compare/"):
			a.calls["compare"]++
			spec := path[strings.Index(path, "/compare/")+len("/compare/"):]
			_, head, _ := strings.Cut(spec, "...")
			files, mergeBase := refreshFiles("blob1"), "0ld"
			if head == a.newHead {
				files, mergeBase = a.newFiles, a.baseTip
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": mergeBase}, "files": files})
		case r.Method == http.MethodGet && strings.Contains(path, "/commits/"):
			a.calls["commit"]++
			sha := path[strings.LastIndex(path, "/")+1:]
			if sha != a.newHead {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": map[string]any{"login": a.mergeAuthor}, "committer": map[string]any{"login": "web-flow"},
				"parents": []map[string]any{{"sha": a.oldHead}, {"sha": a.baseTip}}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/reviews"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/15649"):
			a.calls["pull"]++
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 15649, "head": map[string]any{"sha": a.newHead}, "base": map[string]any{"ref": "v5"}})
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	})
}

// hivecommons/hive#15649: the reviewer verdict route and the review gate
// read a `/hive decision approve` recorded on the previous head as the
// answer for the head the hive's own base refresh produced, and only then.
func TestRecordedDecisionCarriesOverHiveBaseRefresh(t *testing.T) {
	const oldHead, newHead, baseTip = "aaaa", "72246f98", "b45e"
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, HiveAuthoredKnown: true, HiveAuthored: true}
	for _, tc := range []struct {
		name        string
		mergeAuthor string
		newFiles    []map[string]any
		carried     bool
	}{
		{name: "pure base refresh by the App", mergeAuthor: testHiveAppBotLogin, newFiles: refreshFiles("blob1"), carried: true},
		{name: "base refresh that changed the diff", mergeAuthor: testHiveAppBotLogin, newFiles: refreshFiles("blob9")},
		{name: "merge by a person", mergeAuthor: "mendezr", newFiles: refreshFiles("blob1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &baseRefreshAPI{mergeAuthor: tc.mergeAuthor, oldHead: oldHead, newHead: newHead, baseTip: baseTip, newFiles: tc.newFiles}
			server := httptest.NewServer(api.handler(t))
			defer server.Close()
			store := review.NewHumanChangeRequests("")
			if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 15649, HeadSHA: oldHead, Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
				t.Fatal(err)
			}
			client := hgithub.NewClient("token", "hivecommons", []string{"hive"}, nil, server.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), HumanChangeRequests: store, MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})

			// Reviewer verdict route: the stage pass folds the recorded
			// decision into the fresh requires_human verdict on the new head.
			list := []stageObservation{{Lane: stageLaneSelf, Owner: "hivecommons", Repo: "hive", Number: 15649, HeadSHA: newHead, BaseRef: "v5", Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: asked}}}
			c.foldRecordedDecision(context.Background(), list)
			got, _, _ := resolveStage(list)
			answered := list[0].Detail.Verdict.DecisionAnswered && list[0].Detail.Verdict.DecisionAnsweredHead == newHead
			if answered != tc.carried {
				t.Fatalf("decision_answered = %v on %s, want %v (verdict %+v)", answered, newHead, tc.carried, list[0].Detail.Verdict)
			}
			if parked := got.Stage == stagelabels.NeedsDecision; parked == tc.carried {
				t.Fatalf("stage = %s %q, want parked at 085 = %v", got.Stage, got.Reason, !tc.carried)
			}

			// Review gate: the same record releases requires_human for the
			// sweep's merge decision.
			approver, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "hivecommons", "hive", 15649, newHead, asked)
			if ok != tc.carried || (tc.carried && approver != "clubanderson") {
				t.Fatalf("review gate approval = %q ok=%v, want carried=%v", approver, ok, tc.carried)
			}
			// Operator admission reads the same answer.
			if admitted := c.operatorAdmission(context.Background(), "hivecommons", "hive", 15649, newHead); (admitted != "") != tc.carried {
				t.Fatalf("operatorAdmission = %q, want carried=%v", admitted, tc.carried)
			}

			api.mu.Lock()
			commits, pulls := api.calls["commit"], api.calls["pull"]
			api.mu.Unlock()
			if commits != 1 {
				t.Fatalf("merge commit fetched %d times across three gates, want once (memoised)", commits)
			}
			if pulls != 0 {
				t.Fatalf("base branch known from the observation; PR fetched %d times, want 0", pulls)
			}
			if _, ok := store.For("hivecommons/hive", 15649, oldHead); ok == tc.carried {
				t.Fatalf("old-head record present=%v, want superseded only when carried (%v)", ok, tc.carried)
			}
		})
	}
}

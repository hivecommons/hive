package automerge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

func copyPreparationPR(t *testing.T, pr *gh.PullRequest) *gh.PullRequest {
	t.Helper()
	data, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	var out gh.PullRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func preparationOwnerForTest(t *testing.T, c *Engine) preparationOwnerSnapshot {
	t.Helper()
	snapshot, err := c.preparationStore().ownerSnapshot(preparationKey("acme", "widget", "main"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPreparationSettlementRejectsSupersededAdmissionIdentity(t *testing.T) {
	for _, changed := range []string{"head", "base", "base-sha", "head-repo", "author", "body", "title", "labels", "draft", "unavailable"} {
		t.Run(changed, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			if reason, err := c.admitBasePreparation(context.Background(), "acme", "widget", "main", 1, "head1", stageLaneSelf, false); err != nil || reason != "" {
				t.Fatalf("admission: %q %v", reason, err)
			}
			evaluated := copyPreparationPR(t, api.prs[1])
			before := preparationOwnerForTest(t, c)
			switch changed {
			case "head":
				api.prs[1].Head.SHA = gh.Ptr("new-head")
			case "base":
				api.prs[1].Base.Ref = gh.Ptr("other")
			case "base-sha":
				api.prs[1].Base.SHA = gh.Ptr("base2")
			case "head-repo":
				api.prs[1].Head.Repo.FullName = gh.Ptr("other/widget")
			case "author":
				api.prs[1].User.Login = gh.Ptr("other-author")
			case "body":
				api.prs[1].Body = gh.Ptr("Closes #200")
			case "title":
				api.prs[1].Title = gh.Ptr("new review input")
			case "labels":
				api.prs[1].Labels = []*gh.Label{{Name: gh.Ptr("role/trusted")}}
			case "draft":
				api.prs[1].Draft = gh.Ptr(true)
			case "unavailable":
				delete(api.prs, 1)
			}
			c.observeBasePreparation(stageLaneSelf, "acme", "widget", evaluated, "required-check-failing:build", false, nil)
			if after := preparationOwnerForTest(t, c); after.Owner != before.Owner || after.Attempt != before.Attempt {
				t.Fatalf("stale %s callback changed ownership: before=%+v after=%+v", changed, before, after)
			}
		})
	}
}

func TestPreparationUnobservedRefreshCannotBeReleasedByOldEvaluation(t *testing.T) {
	for _, reason := range []string{"required-check-failing:build", autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested, "untrusted-author-role"} {
		t.Run(reason, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			now := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)
			c.now = func() time.Time { return now }
			if _, _, _, err := preparationGate(c, 1, "main", "head1"); err != nil {
				t.Fatal(err)
			}
			before := preparationOwnerForTest(t, c)
			c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], reason, false, nil)
			if after := preparationOwnerForTest(t, c); after.Owner != before.Owner || after.Attempt != before.Attempt {
				t.Fatalf("unobserved mutation lost its exclusive owner: %+v", after)
			}
			now = now.Add(effects.DefaultUnresolvedTTL)
			c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], reason, false, nil)
			if after := preparationOwnerForTest(t, c); after.Owner.Number != 0 {
				t.Fatalf("validated unchanged head could not settle after normal reconciliation window: %+v", after)
			}
		})
	}
}

func TestPreparationObservedRefreshRetainsFreshReviewAndSettlesValidatedFailure(t *testing.T) {
	api, srv := newPreparationAPI(t)
	c := preparationEngine(srv.URL, NewBasePreparationStore(""))
	if _, _, _, err := preparationGate(c, 1, "main", "head1"); err != nil {
		t.Fatal(err)
	}
	api.prs[1].Head.SHA = gh.Ptr("observed-refresh")
	for _, reason := range []string{"required-check-pending:build", autoMergeReasonReviewGatePrefix + ReviewGateOutcomeNoBundle, autoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting} {
		c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], reason, false, nil)
		if after := preparationOwnerForTest(t, c); after.Owner.Number != 1 || after.Owner.Head != "head1" || after.Attempt.Head != "head1" {
			t.Fatalf("expected old source identity lost while %s: %+v", reason, after)
		}
	}
	c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], "required-check-failing:build", false, nil)
	if after := preparationOwnerForTest(t, c); after.Owner.Number != 0 {
		t.Fatalf("fresh observed-head failure did not settle old source reservation: %+v", after)
	}
	if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("normally admitted successor could not refresh: %+v %v", ev, err)
	}
}

func TestPreparationSettlementFencesOwnerAndAttemptChangesDuringRead(t *testing.T) {
	for _, change := range []string{"owner", "attempt", "promoted-waiter"} {
		t.Run(change, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			key := preparationKey("acme", "widget", "main")
			if reason, err := c.admitBasePreparation(context.Background(), "acme", "widget", "main", 1, "head1", stageLaneSelf, false); err != nil || reason != "" {
				t.Fatalf("admission: %q %v", reason, err)
			}
			if change == "promoted-waiter" {
				if err := c.preparationStore().change(key, func(st *basePreparation) error {
					st.Waiting = []preparationCandidate{st.Owner}
					st.Owner = preparationCandidate{Number: 2, Head: "head2", Lane: stageLaneSelf, Since: c.now()}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var concurrent preparationOwnerSnapshot
			api.onGet = func(a *preparationAPI, _ int) {
				a.onGet = nil
				if err := c.preparationStore().change(key, func(st *basePreparation) error {
					switch change {
					case "owner":
						st.Owner.Since = st.Owner.Since.Add(time.Second)
					case "attempt":
						st.Attempts = map[int]preparationAttempt{1: {Head: "head1", Base: "base2", At: c.now(), AuthoritativeBase: true}}
					case "promoted-waiter":
						st.Owner = st.Waiting[0]
						st.Waiting = nil
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
				concurrent = preparationOwnerForTest(t, c)
			}
			c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], "required-check-failing:build", false, nil)
			if after := preparationOwnerForTest(t, c); after.Owner != concurrent.Owner || after.Attempt != concurrent.Attempt {
				t.Fatalf("callback overwrote concurrent %s: concurrent=%+v after=%+v", change, concurrent, after)
			}
		})
	}
}

func TestPreparationStaleCallbackCannotRewriteYieldedHead(t *testing.T) {
	api, srv := newPreparationAPI(t)
	c := preparationEngine(srv.URL, NewBasePreparationStore(""))
	evaluated := copyPreparationPR(t, api.prs[1])
	api.prs[1].Head.SHA = gh.Ptr("new-head")
	key := preparationKey("acme", "widget", "main")
	if err := c.preparationStore().change(key, func(st *basePreparation) error {
		st.Owner = preparationCandidate{Number: 2, Head: "head2", Lane: stageLaneSelf, Since: c.now()}
		st.Waiting = []preparationCandidate{{Number: 1, Head: "new-head", Lane: stageLaneSelf, Yielded: true, Since: c.now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.observeBasePreparation(stageLaneSelf, "acme", "widget", evaluated, "required-check-pending:build", false, nil)
	if after := preparationOwnerForTest(t, c); after.Owner.Number != 2 || len(after.Waiters) != 1 || after.Waiters[0].Head != "new-head" {
		t.Fatalf("stale callback rewound yielded head: %+v", after)
	}
}

func TestPreparationValidatedYieldedRenewalDoesNotOverwriteNewerTicket(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "validated", true: "superseded-during-read"}[concurrent], func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			key := preparationKey("acme", "widget", "main")
			if err := c.preparationStore().change(key, func(st *basePreparation) error {
				st.Owner = preparationCandidate{Number: 2, Head: "head2", Lane: stageLaneSelf, Since: c.now()}
				st.Waiting = []preparationCandidate{{Number: 1, Head: "old-head", Lane: stageLaneSelf, Yielded: true, Since: c.now()}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if concurrent {
				api.onGet = func(a *preparationAPI, _ int) {
					a.onGet = nil
					if err := c.preparationStore().change(key, func(st *basePreparation) error {
						st.Waiting[0].Head = "newer-ticket"
						return nil
					}); err != nil {
						t.Error(err)
					}
				}
			}
			c.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], "required-check-pending:build", false, nil)
			want := "head1"
			if concurrent {
				want = "newer-ticket"
			}
			if after := preparationOwnerForTest(t, c); after.Owner.Number != 2 || len(after.Waiters) != 1 || after.Waiters[0].Head != want {
				t.Fatalf("invalid yielded renewal: %+v, want head %s", after, want)
			}
		})
	}
}

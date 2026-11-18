package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// removeFromQueue takes the PR out of the fake queue the way GitHub records
// it: a new RemovedFromMergeQueueEvent by actor (login, GraphQL typename).
func removeFromQueue(f *nativeQueueFake, id, login, typename string) {
	f.state.IsInMergeQueue = false
	f.state.MergeQueueEntry = nil
	removal := hgithub.NativeMergeQueueRemoval{ID: id, Reason: "failed_checks"}
	removal.Actor.Login = login
	removal.Actor.Typename = typename
	f.state.TimelineItems.Nodes = append(f.state.TimelineItems.Nodes, removal)
}

// projectbluefin/utah-packages#1199, 2026-11-18: the merge queue removed the
// hive's own entry for a head, and the hive queued the same head again.
// Only a removal by the merge queue itself, of an entry the hive's App
// made, is remembered as a rejection of that head.
func TestNativeQueueRejectionIsOnlyTheQueueRemovingTheHiveEntry(t *testing.T) {
	for _, tc := range []struct {
		name, login, typename string
		humanQueued           bool
		wantRejected          bool
	}{
		{name: "merge queue removed the hive entry", login: mergeQueueBotLogin, typename: "Bot", wantRejected: true},
		{name: "hive withdrew its own entry", login: strings.TrimSuffix(testHiveAppBotLogin, "[bot]"), typename: "Bot"},
		{name: "a person dequeued the hive entry", login: "maintainer", typename: "User"},
		{name: "merge queue removed a person's entry", login: mergeQueueBotLogin, typename: "Bot", humanQueued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			if tc.humanQueued {
				f.state.IsInMergeQueue = true
				f.state.MergeQueueEntry = queueEntry("maintainer")
			}
			if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || !strings.HasPrefix(reason, nativeQueueReason) {
				t.Fatalf("first pass: reason=%q error=%v", reason, err)
			}
			enqueued := f.enqueues
			removeFromQueue(f, "removal1", tc.login, tc.typename)

			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRejected {
				if reason != nativeQueueReasonRejected || f.enqueues != enqueued {
					t.Fatalf("reason=%q enqueues=%d, want %q and no new enqueue", reason, f.enqueues, nativeQueueReasonRejected)
				}
				if !c.knownNativeQueueRejection(claim.Repo, 7, "sha7") || c.knownNativeQueueRejection(claim.Repo, 7, "sha8") {
					t.Fatal("rejection must be remembered for sha7 only")
				}
				return
			}
			if !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != enqueued+1 || c.knownNativeQueueRejection(claim.Repo, 7, "sha7") {
				t.Fatalf("reason=%q enqueues=%d (was %d), want a re-enqueue and no rejection", reason, f.enqueues, enqueued)
			}
		})
	}
}

// A rejection waits on the head, not forever: a new head, or
// mergeRefusalRecheck, queues again.
func TestNativeQueueRejectionReArms(t *testing.T) {
	now := time.Date(2026, 11, 18, 19, 11, 0, 0, time.UTC)
	c, f, pr, claim := nativeQueueEngine()
	c.now = func() time.Time { return now }
	if _, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil {
		t.Fatal(err)
	}
	removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
	if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != nativeQueueReasonRejected {
		t.Fatalf("reason=%q, want %q", reason, nativeQueueReasonRejected)
	}
	now = now.Add(mergeRefusalRecheck)
	if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 2 {
		t.Fatalf("after recheck: reason=%q enqueues=%d, want a re-enqueue", reason, f.enqueues)
	}

	c2, f2, pr2, claim2 := nativeQueueEngine()
	if _, err := c2.tryNativeQueue(context.Background(), pr2, claim2); err != nil {
		t.Fatal(err)
	}
	removeFromQueue(f2, "removal1", mergeQueueBotLogin, "Bot")
	if reason, _ := c2.tryNativeQueue(context.Background(), pr2, claim2); reason != nativeQueueReasonRejected {
		t.Fatalf("reason=%q, want %q", reason, nativeQueueReasonRejected)
	}
	f2.state.HeadRefOid = "sha8"
	claim2.Inputs["expect_sha"] = "sha8"
	if reason, _ := c2.tryNativeQueue(context.Background(), pr2, claim2); !strings.HasPrefix(reason, nativeQueueReason) || f2.enqueues != 2 {
		t.Fatalf("new head: reason=%q enqueues=%d, want a re-enqueue", reason, f2.enqueues)
	}
}

// With skip_current_base_gate on, the self-authored and trusted-author
// sweeps enqueue a behind head. Once the queue rejects that head, a behind
// head is updated instead of re-queued, and a head that is not behind
// waits with native-merge-queue-rejected. Without a rejection the sweep
// behaves exactly as before.
func TestNativeQueueRejectedHeadSweepUpdatesInsteadOfRequeueing(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted} {
		for _, state := range []string{"behind", "clean"} {
			for _, rejected := range []bool{true, false} {
				t.Run(lane+"/"+state+"/"+map[bool]string{true: "rejected", false: "withdrawn-by-hive"}[rejected], func(t *testing.T) {
					var merged []int
					author := testHiveAppBotLogin
					if lane == stageLaneTrusted {
						author = "alice"
					}
					updates := 0
					api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: author, baseRef: "main", baseSHA: "base1", mergeableState: state, statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged, func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main":
							_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"sha": "base1"}})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/base1/status":
							_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
							w.WriteHeader(http.StatusNotFound)
							_ = json.NewEncoder(w).Encode(map[string]string{"message": "Branch not protected"})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/main":
							_ = json.NewEncoder(w).Encode([]any{})
						case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
							updates++
							w.WriteHeader(http.StatusAccepted)
							_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
						case strings.Contains(r.URL.Path, "/compare/"):
							behind := 0
							if state == "behind" {
								behind = 2
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": behind, "ahead_by": 1})
						default:
							t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
						}
					})
					defer api.Close()
					c := newAutoMergeSweepClient(api.URL)
					f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
					c.transport = f
					enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, SkipCurrentBaseGate: true})
					sweep := func() {
						t.Helper()
						var err error
						if lane == stageLaneSelf {
							_, err = c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
						} else {
							c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
								return TrustedAuthorDecision{Allowed: true, Role: "merger"}
							}
							c.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"} }
							_, err = c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{})
						}
						if err != nil {
							t.Fatal(err)
						}
					}

					sweep()
					if f.enqueues != 1 || updates != 0 {
						t.Fatalf("first sweep: enqueues=%d updates=%d, want the bypass to enqueue", f.enqueues, updates)
					}
					if rejected {
						removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
					} else {
						removeFromQueue(f, "removal1", strings.TrimSuffix(testHiveAppBotLogin, "[bot]"), "Bot")
					}
					sweep()
					sweep()

					wantEnqueues, wantUpdates := 1, 0
					switch {
					case !rejected:
						// The hive's own withdrawal is not a rejection: the
						// next sweep queues the head again, as before.
						wantEnqueues = 2
					case state == "behind":
						wantUpdates = 1
					}
					if f.enqueues != wantEnqueues || updates != wantUpdates || len(merged) != 0 {
						t.Fatalf("enqueues=%d updates=%d merged=%v, want enqueues=%d updates=%d", f.enqueues, updates, merged, wantEnqueues, wantUpdates)
					}
				})
			}
		}
	}
}

// The contributor lane takes the same path: a rejected behind head is
// updated, a rejected current head waits with native-merge-queue-rejected.
func TestContributorLaneNativeQueueRejectedHead(t *testing.T) {
	for _, state := range []string{"behind", "clean"} {
		for _, rejected := range []bool{true, false} {
			t.Run(state+"/"+map[bool]string{true: "rejected", false: "dequeued-by-person"}[rejected], func(t *testing.T) {
				fx := validContributorFixture()
				fx.nodeID = "PR_7"
				fx.mergeableState = state
				if state == "behind" {
					fx.behindBy = 2
				}
				api := newContributorLaneAPI(t, &fx)
				defer api.Close()
				policy := defaultContributorLanePolicy()
				clock := &contributorLaneClock{at: time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)}
				c := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
				f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
				c.transport = f
				enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, SkipCurrentBaseGate: true})
				sweep := func() string {
					t.Helper()
					_, reason, err := c.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
					if err != nil {
						t.Fatal(err)
					}
					return reason
				}

				if reason := sweep(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 1 || fx.updates != 0 {
					t.Fatalf("first pass: reason=%q enqueues=%d updates=%d, want the bypass to enqueue", reason, f.enqueues, fx.updates)
				}
				if rejected {
					removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
				} else {
					removeFromQueue(f, "removal1", "maintainer", "User")
				}
				second := sweep()
				third := sweep()

				switch {
				case !rejected:
					if f.enqueues != 2 || fx.updates != 0 {
						t.Fatalf("enqueues=%d updates=%d, want one re-enqueue as before", f.enqueues, fx.updates)
					}
				case state == "behind":
					if second != nativeQueueReasonRejected || f.enqueues != 1 || fx.updates != 1 {
						t.Fatalf("second=%q enqueues=%d updates=%d, want the rejection then one update-branch", second, f.enqueues, fx.updates)
					}
				default:
					if second != nativeQueueReasonRejected || third != nativeQueueReasonRejected || f.enqueues != 1 || fx.updates != 0 {
						t.Fatalf("second=%q third=%q enqueues=%d updates=%d, want %q twice and no queue or branch traffic", second, third, f.enqueues, fx.updates, nativeQueueReasonRejected)
					}
				}
				if fx.merges != 0 {
					t.Fatalf("merges=%d, want none", fx.merges)
				}
			})
		}
	}
}

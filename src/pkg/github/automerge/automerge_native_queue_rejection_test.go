package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
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
// behaves exactly as before. "blocked-behind" is a behind head on a base
// that does not require up-to-date branches, which GitHub reports as
// "blocked" (projectbluefin/utah-packages#1217).
func TestNativeQueueRejectedHeadSweepUpdatesInsteadOfRequeueing(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted} {
		for _, state := range []string{"behind", "blocked-behind", "clean"} {
			for _, rejected := range []bool{true, false} {
				t.Run(lane+"/"+state+"/"+map[bool]string{true: "rejected", false: "withdrawn-by-hive"}[rejected], func(t *testing.T) {
					var merged []int
					author := testHiveAppBotLogin
					if lane == stageLaneTrusted {
						author = "alice"
					}
					updates := 0
					mergeableState := state
					if state == "blocked-behind" {
						mergeableState = "blocked"
					}
					api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: author, baseRef: "main", baseSHA: "base1", mergeableState: mergeableState, statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged, func(w http.ResponseWriter, r *http.Request) {
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
							if state != "clean" {
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
					case state != "clean":
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

// projectbluefin/utah-packages#1217, 2026-11-19: the base does not require
// up-to-date branches, so GitHub reported the behind PR as "blocked", never
// "behind". The lane never updated the rejected head, and the hive queued
// it again each time the 30-minute rejection mark re-armed. A rejected head
// that is behind its base is now updated whatever mergeable_state says, and
// a failed_checks rejection of a behind head waits for a new head; another
// removal reason keeps the 30-minute recheck.
func TestContributorLaneNativeQueueRejectedBehindBlockedHead(t *testing.T) {
	for _, tc := range []struct {
		name              string
		removalReason     string
		updateStatus      int
		wantThird         string
		wantRecheckQueues int // enqueues after mergeRefusalRecheck; 0 skips the recheck
	}{
		{name: "update-branch succeeds", removalReason: nativeQueueRemovalFailedChecks, wantThird: "check-pending"},
		{name: "update-branch refused after failed checks", removalReason: nativeQueueRemovalFailedChecks, updateStatus: http.StatusForbidden, wantThird: autoMergeReasonWorkflowPermission, wantRecheckQueues: 1},
		{name: "update-branch refused after another removal reason", removalReason: "ci_timeout", updateStatus: http.StatusForbidden, wantThird: autoMergeReasonWorkflowPermission, wantRecheckQueues: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.nodeID = "PR_7"
			fx.mergeableState = "blocked"
			fx.behindBy = 18
			if tc.updateStatus != 0 {
				fx.updateStatus = tc.updateStatus
				fx.updateMessage = "refusing to allow a GitHub App to create or update workflow `.github/workflows/build-stage.yml` without `workflows` permission"
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 19, 14, 56, 0, 0, time.UTC)}
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
			removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
			f.state.TimelineItems.Nodes[len(f.state.TimelineItems.Nodes)-1].Reason = tc.removalReason
			if reason := sweep(); reason != nativeQueueReasonRejected {
				t.Fatalf("second pass: reason=%q, want %q", reason, nativeQueueReasonRejected)
			}
			if reason := sweep(); reason != tc.wantThird || f.enqueues != 1 || fx.updates != 1 {
				t.Fatalf("third pass: reason=%q enqueues=%d updates=%d, want %q from one update-branch and no second enqueue", reason, f.enqueues, fx.updates, tc.wantThird)
			}
			if tc.wantRecheckQueues == 0 {
				return
			}
			clock.advance(mergeRefusalRecheck + time.Minute)
			sweep()
			if f.enqueues != tc.wantRecheckQueues || fx.updates != 1 {
				t.Fatalf("after the recheck window: enqueues=%d updates=%d, want enqueues=%d and the refused update not sent again", f.enqueues, fx.updates, tc.wantRecheckQueues)
			}
			if fx.merges != 0 {
				t.Fatalf("merges=%d, want none", fx.merges)
			}
		})
	}
}

// A merge-group check that fails for a reason the PR does not carry (a
// broken dependency, a red base) fails the same head on the same base every
// time. The 30-minute re-arm is the retry for a flaky check, and each head
// and base tip gets it once. After a second failed_checks rejection on the
// same head and base tip the head waits at 085 and is retried only on the
// slow backoff (2 hours, then every 6 hours), so a cause fixed somewhere
// else is still picked up without a person. A new head or base tip retries
// at once and starts over.
func TestNativeQueueRejectionRetriesOncePerHeadAndBaseTip(t *testing.T) {
	type rig struct {
		c     *Engine
		f     *nativeQueueFake
		claim map[string]string
		now   *time.Time
		pass  func() string
	}
	// setup runs the queue up to the second rejection on base1 and returns
	// the time it was observed.
	setup := func(t *testing.T) (rig, time.Time) {
		t.Helper()
		now := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
		c, f, pr, claim := nativeQueueEngine()
		c.now = func() time.Time { return now }
		pass := func() string {
			t.Helper()
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil {
				t.Fatal(err)
			}
			return reason
		}
		if reason := pass(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 1 {
			t.Fatalf("first pass: reason=%q enqueues=%d, want an enqueue", reason, f.enqueues)
		}
		removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
		if reason := pass(); reason != nativeQueueReasonRejected {
			t.Fatalf("first rejection: reason=%q, want %q", reason, nativeQueueReasonRejected)
		}
		// The lane's compare reads the base tip on the next pass.
		c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", "base1")
		// The first rejection still re-arms after 30 minutes.
		now = now.Add(mergeRefusalRecheck + time.Minute)
		if reason := pass(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 2 {
			t.Fatalf("retry: reason=%q enqueues=%d, want the 30-minute retry", reason, f.enqueues)
		}
		removeFromQueue(f, "removal2", mergeQueueBotLogin, "Bot")
		rejectedAt := now
		// The head already spent its retry, but the base may have moved
		// under it: 085 waits for the compare to confirm the tip.
		if reason := pass(); reason != nativeQueueReasonRejected {
			t.Fatalf("second rejection: reason=%q, want %q until the tip is read", reason, nativeQueueReasonRejected)
		}
		c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", "base1")
		if reason := pass(); reason != nativeQueueReasonRejectedRepeatedly {
			t.Fatalf("after the compare read base1: reason=%q, want %q", reason, nativeQueueReasonRejectedRepeatedly)
		}
		return rig{c: c, f: f, claim: claim.Inputs, now: &now, pass: pass}, rejectedAt
	}

	t.Run("slow backoff on the same head and base tip", func(t *testing.T) {
		r, rejectedAt := setup(t)
		wait := func(at time.Time, wantReason string, wantEnqueues int) {
			t.Helper()
			*r.now = at
			reason := r.pass()
			if (wantReason != "" && reason != wantReason) || (wantReason == "" && !strings.HasPrefix(reason, nativeQueueReason)) {
				t.Fatalf("at +%s: reason=%q, want %q (empty: a re-enqueue)", at.Sub(rejectedAt), reason, wantReason)
			}
			if r.f.enqueues != wantEnqueues {
				t.Fatalf("at +%s: enqueues=%d, want %d", at.Sub(rejectedAt), r.f.enqueues, wantEnqueues)
			}
		}
		wait(rejectedAt.Add(mergeRefusalRecheck+time.Minute), nativeQueueReasonRejectedRepeatedly, 2)
		wait(rejectedAt.Add(2*mergeRefusalRecheck+time.Minute), nativeQueueReasonRejectedRepeatedly, 2)
		wait(rejectedAt.Add(nativeQueueRepeatedRecheckFirst-time.Minute), nativeQueueReasonRejectedRepeatedly, 2)
		wait(rejectedAt.Add(nativeQueueRepeatedRecheckFirst), "", 3)

		// The slow retry fails again on base1: back at 085 once the compare
		// confirms the tip, and the next retry waits the longer interval.
		removeFromQueue(r.f, "removal3", mergeQueueBotLogin, "Bot")
		rejectedAt = *r.now
		wait(rejectedAt, nativeQueueReasonRejected, 3)
		r.c.noteNativeQueueRejectionBase("acme/widget", 7, "sha7", "base1")
		wait(rejectedAt, nativeQueueReasonRejectedRepeatedly, 3)
		wait(rejectedAt.Add(nativeQueueRepeatedRecheckFirst), nativeQueueReasonRejectedRepeatedly, 3)
		wait(rejectedAt.Add(nativeQueueRepeatedRecheck-time.Minute), nativeQueueReasonRejectedRepeatedly, 3)
		wait(rejectedAt.Add(nativeQueueRepeatedRecheck), "", 4)
	})

	t.Run("base tip moves", func(t *testing.T) {
		r, _ := setup(t)
		r.c.noteNativeQueueRejectionBase("acme/widget", 7, "sha7", "base2")
		if reason := r.pass(); !strings.HasPrefix(reason, nativeQueueReason) || r.f.enqueues != 3 {
			t.Fatalf("base moved: reason=%q enqueues=%d, want a re-enqueue at once", reason, r.f.enqueues)
		}
		// A failure on the new tip is a first failure there: it gets its
		// own 30-minute retry.
		removeFromQueue(r.f, "removal3", mergeQueueBotLogin, "Bot")
		if reason := r.pass(); reason != nativeQueueReasonRejected {
			t.Fatalf("failure after the base moved: reason=%q, want %q (no false 085 before the tip is read)", reason, nativeQueueReasonRejected)
		}
		r.c.noteNativeQueueRejectionBase("acme/widget", 7, "sha7", "base2")
		if reason := r.pass(); reason != nativeQueueReasonRejected {
			t.Fatalf("first failure on base2: reason=%q, want %q", reason, nativeQueueReasonRejected)
		}
		*r.now = r.now.Add(mergeRefusalRecheck + time.Minute)
		if reason := r.pass(); !strings.HasPrefix(reason, nativeQueueReason) || r.f.enqueues != 4 {
			t.Fatalf("30-minute retry on base2: reason=%q enqueues=%d, want a re-enqueue", reason, r.f.enqueues)
		}
	})

	t.Run("new head", func(t *testing.T) {
		r, _ := setup(t)
		r.f.state.HeadRefOid = "sha8"
		r.claim["expect_sha"] = "sha8"
		if reason := r.pass(); !strings.HasPrefix(reason, nativeQueueReason) || r.f.enqueues != 3 {
			t.Fatalf("new head: reason=%q enqueues=%d, want a re-enqueue at once", reason, r.f.enqueues)
		}
	})
}

// The marks that wait for a change or a slow retry outlive the 30-minute
// prune, so a busy memory does not hand the head an early merge-group run.
func TestMergeRefusalPruneKeepsNativeQueueBackoffMarks(t *testing.T) {
	at := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		key  string
		r    mergeRefusal
		age  time.Duration
		want bool
	}{
		{name: "plain refusal past the recheck", key: "review|acme/widget#7", age: mergeRefusalRecheck, want: true},
		{name: "repeated rejection past the longest backoff", key: nativeQueueRejectionKey("acme/widget", 7), r: mergeRefusal{repeated: true}, age: nativeQueueRepeatedRecheck + time.Hour},
		{name: "spent retry past the longest backoff", key: nativeQueueRetryKey("acme/widget", 7), age: nativeQueueRepeatedRecheck + time.Hour},
		{name: "behind rejection", key: nativeQueueRejectionKey("acme/widget", 7), r: mergeRefusal{behind: true}, age: nativeQueueRepeatedRecheck + time.Hour},
		{name: "repeated rejection idle a day", key: nativeQueueRejectionKey("acme/widget", 7), r: mergeRefusal{repeated: true}, age: sweepMemoryIdle, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.at = at
			if got := mergeRefusalPrunable(tc.key, tc.r, at.Add(tc.age)); got != tc.want {
				t.Fatalf("prunable = %v, want %v", got, tc.want)
			}
		})
	}
	if sweepMemoryIdle <= nativeQueueRepeatedRecheck {
		t.Fatalf("sweepMemoryIdle %s must outlast the slow backoff %s", sweepMemoryIdle, nativeQueueRepeatedRecheck)
	}
}

// A rejection whose base tip was never read (the compare failed, or a lane
// that does not compare) keeps the 30-minute recheck every time, as before;
// so does a removal reason other than failed_checks.
func TestNativeQueueRejectionWithoutBaseTipOrFailedChecksKeepsRecheck(t *testing.T) {
	for _, tc := range []struct {
		name, removalReason, tip string
	}{
		{name: "base tip unknown", removalReason: nativeQueueRemovalFailedChecks},
		{name: "another removal reason", removalReason: "ci_timeout", tip: "base1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
			c, f, pr, claim := nativeQueueEngine()
			c.now = func() time.Time { return now }
			if _, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil {
				t.Fatal(err)
			}
			for round := 1; round <= 3; round++ {
				removeFromQueue(f, "removal"+strconv.Itoa(round), mergeQueueBotLogin, "Bot")
				f.state.TimelineItems.Nodes[len(f.state.TimelineItems.Nodes)-1].Reason = tc.removalReason
				if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != nativeQueueReasonRejected {
					t.Fatalf("round %d: reason=%q, want %q", round, reason, nativeQueueReasonRejected)
				}
				if tc.tip != "" {
					c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", tc.tip)
				}
				now = now.Add(mergeRefusalRecheck + time.Minute)
				if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != round+1 {
					t.Fatalf("round %d recheck: reason=%q enqueues=%d, want a re-enqueue", round, reason, f.enqueues)
				}
			}
		})
	}
}

// The contributor lane end to end: the base tip comes from the compare the
// lane already runs for a rejected head (nativeQueueRejectedHeadBehind), so
// no extra GitHub call is made. On the same tip the head waits for the slow
// retry; a moved tip re-queues at once; a base that moves past the head
// takes the behind path (update-branch) as before.
func TestContributorLaneNativeQueueRejectionRetriesOncePerBaseTip(t *testing.T) {
	for _, tc := range []struct {
		name        string
		move        func(fx *contributorLaneFixture)
		wantReason  string
		wantEnqueue int
		wantUpdates int
		slowRetry   bool // the head is queued again at the first slow retry
	}{
		{name: "same base tip", wantReason: nativeQueueReasonRejectedRepeatedly, wantEnqueue: 2, slowRetry: true},
		{name: "base tip moves, head still current", move: func(fx *contributorLaneFixture) { fx.compareBaseSHA = "base2" }, wantEnqueue: 3},
		{name: "base moves past the head", move: func(fx *contributorLaneFixture) { fx.compareBaseSHA, fx.behindBy = "base2", 2 }, wantReason: "check-pending", wantEnqueue: 2, wantUpdates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.nodeID = "PR_7"
			fx.compareBaseSHA = "base1"
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)}
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

			if reason := sweep(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 1 {
				t.Fatalf("first pass: reason=%q enqueues=%d, want an enqueue", reason, f.enqueues)
			}
			removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
			for i, want := range []string{nativeQueueReasonRejected, nativeQueueReasonRejected} {
				if reason := sweep(); reason != want {
					t.Fatalf("pass %d after the first rejection: reason=%q, want %q", i+1, reason, want)
				}
			}
			clock.advance(mergeRefusalRecheck + time.Minute)
			if reason := sweep(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 2 {
				t.Fatalf("retry: reason=%q enqueues=%d, want the one retry", reason, f.enqueues)
			}
			removeFromQueue(f, "removal2", mergeQueueBotLogin, "Bot")
			rejectedAt := clock.at
			for i, want := range []string{nativeQueueReasonRejected, nativeQueueReasonRejectedRepeatedly} {
				if reason := sweep(); reason != want {
					t.Fatalf("pass %d after the second rejection: reason=%q, want %q", i+1, reason, want)
				}
			}

			clock.advance(mergeRefusalRecheck + time.Minute)
			if tc.move != nil {
				tc.move(&fx)
			}
			reason := sweep()
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Fatalf("third pass: reason=%q, want %q", reason, tc.wantReason)
			}
			if tc.wantReason == "" && !strings.HasPrefix(reason, nativeQueueReason) {
				t.Fatalf("third pass: reason=%q, want a re-enqueue", reason)
			}
			if f.enqueues != tc.wantEnqueue || fx.updates != tc.wantUpdates || fx.merges != 0 {
				t.Fatalf("third pass: enqueues=%d updates=%d merges=%d, want enqueues=%d updates=%d and no merge", f.enqueues, fx.updates, fx.merges, tc.wantEnqueue, tc.wantUpdates)
			}
			if !tc.slowRetry {
				return
			}
			clock.at = rejectedAt.Add(2*mergeRefusalRecheck + time.Minute)
			if reason := sweep(); reason != nativeQueueReasonRejectedRepeatedly || f.enqueues != 2 {
				t.Fatalf("an hour on: reason=%q enqueues=%d, want %q and no enqueue", reason, f.enqueues, nativeQueueReasonRejectedRepeatedly)
			}
			clock.at = rejectedAt.Add(nativeQueueRepeatedRecheckFirst)
			if reason := sweep(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 3 {
				t.Fatalf("slow retry: reason=%q enqueues=%d, want the head queued again", reason, f.enqueues)
			}
		})
	}
}

// Once the retry is spent the PR is a person's to look at: the lane reason
// parks at 085 (stageengine), the entry comment says what failed and that
// the hive retries on its own, and a merge train never seats the head as a
// ready front.
func TestNativeQueueRejectedRepeatedlyIsAPersonsCall(t *testing.T) {
	cause := decisionCauseFor(nativeQueueReasonRejectedRepeatedly, false)
	if cause == "" || !strings.Contains(cause, "failed_checks") || !strings.Contains(cause, "merge_group") {
		t.Fatalf("decisionCauseFor = %q, want the failed_checks explanation", cause)
	}
	for _, want := range []string{"stopped queueing it every 30 minutes", "on its own 2 hours after the second failure", "then every 6 hours", "at once when a new commit is pushed, the base branch changes, or a maintainer asks for it", "**Retry now**", "`/hive decision approve`", "An approving review on GitHub does not start a retry"} {
		if !strings.Contains(cause, want) {
			t.Fatalf("decisionCauseFor = %q, want %q", cause, want)
		}
	}
	if strings.Contains(cause, "twice") || strings.Contains(cause, "Checks tab") {
		t.Fatalf("decisionCauseFor = %q: no count that goes stale, and the merge group is not on the PR's checks", cause)
	}
	linked := nativeQueueRejectedRepeatedlyCause("acme/widget")
	if !strings.Contains(linked, "(https://github.com/acme/widget/actions?query=event%3Amerge_group)") {
		t.Fatalf("cause for acme/widget = %q, want the merge-group runs link", linked)
	}
	if strings.ContainsAny(linked, "\u2013\u2014@") {
		t.Fatalf("cause = %q: no en/em dashes or @ in emitted text", linked)
	}
	if strings.ContainsAny(cause, "\u2013\u2014@") {
		t.Fatalf("decisionCauseFor = %q: no en/em dashes or @ in emitted text", cause)
	}
	if !mergeTrainBlockingReason(nativeQueueReasonRejectedRepeatedly) || mergeTrainBlockingReason(nativeQueueReasonRejected) {
		t.Fatal("only the repeated rejection blocks a train front")
	}
}

// A person who knows the cause is fixed does not wait for the slow retry:
// approving the head after its repeated rejection (the dashboard's "Retry
// now", an Approve on the 085 row) re-arms the mark on the next pass. An
// approval from before the failure does not, and neither does the same
// approval once the retry has failed again.
func TestNativeQueueRepeatedRejectionRetriesWhenAPersonApproves(t *testing.T) {
	now := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	c, f, pr, claim := nativeQueueEngine()
	c.now = func() time.Time { return now }
	store := review.NewHumanChangeRequests("")
	c.humanChangeRequests = store
	approve := func(id int64, at time.Time) {
		t.Helper()
		if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: id, Actor: "maintainer", Body: "approve", Kind: review.HumanChangeRequestKindApprove, RecordedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	pass := func() string {
		t.Helper()
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	pass()
	removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
	pass()
	c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", "base1")
	now = now.Add(mergeRefusalRecheck + time.Minute)
	pass()
	removeFromQueue(f, "removal2", mergeQueueBotLogin, "Bot")
	approve(1, now.Add(-time.Minute)) // before the failure was seen
	pass()
	c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", "base1")
	now = now.Add(10 * time.Minute)
	if reason := pass(); reason != nativeQueueReasonRejectedRepeatedly || f.enqueues != 2 {
		t.Fatalf("approval older than the failure: reason=%q enqueues=%d, want %q and no enqueue", reason, f.enqueues, nativeQueueReasonRejectedRepeatedly)
	}
	approve(2, now)
	now = now.Add(time.Minute)
	if reason := pass(); !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 3 {
		t.Fatalf("retry now: reason=%q enqueues=%d, want the head queued again at once", reason, f.enqueues)
	}
	removeFromQueue(f, "removal3", mergeQueueBotLogin, "Bot")
	pass()
	c.noteNativeQueueRejectionBase(claim.Repo, 7, "sha7", "base1")
	now = now.Add(10 * time.Minute)
	if reason := pass(); reason != nativeQueueReasonRejectedRepeatedly || f.enqueues != 3 {
		t.Fatalf("after the approved retry failed: reason=%q enqueues=%d, want %q and the slow backoff", reason, f.enqueues, nativeQueueReasonRejectedRepeatedly)
	}
}

// A repeated rejection whose base tip the compare never confirms (the
// compare keeps failing) still reaches a person: after a few passes it is
// reported as repeated on the last known tip. A first rejection never is.
func TestNativeQueueRepeatedRejectionReachesAPersonWithoutATipRead(t *testing.T) {
	at := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	unconfirmed := mergeRefusal{state: nativeQueueRemovalFailedChecks, repeated: true, priorBase: "base1", at: at}
	if got := unconfirmed.laneReason(at.Add(time.Minute)); got != nativeQueueReasonRejected {
		t.Fatalf("just seen: %q, want %q while the compare may still confirm the tip", got, nativeQueueReasonRejected)
	}
	if got := unconfirmed.laneReason(at.Add(nativeQueueTipUnconfirmedAfter)); got != nativeQueueReasonRejectedRepeatedly {
		t.Fatalf("tip still unread: %q, want %q", got, nativeQueueReasonRejectedRepeatedly)
	}
	first := mergeRefusal{state: nativeQueueRemovalFailedChecks, at: at}
	if got := first.laneReason(at.Add(time.Hour)); got != nativeQueueReasonRejected {
		t.Fatalf("first rejection: %q, want %q", got, nativeQueueReasonRejected)
	}
}

// The human label-queued lane has no behind check, so it reads the base tip
// of a held rejection itself (nativeQueueNoteRejectedBaseTip): the same
// bound applies there.
func TestQueuedLaneNativeQueueRejectionRetriesOncePerBaseTip(t *testing.T) {
	var merged []int
	compares := 0
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{number: 7, nodeID: "PR_7", author: "alice", queuedBy: "bob", label: true, baseRef: "main", baseSHA: "base1", mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/compare/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		compares++
		_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": 0, "ahead_by": 1, "base_commit": map[string]string{"sha": "base1"}})
	})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	now := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
	c.transport = f
	enableNativeQueue(c)
	c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
		return TrustedAuthorDecision{Allowed: true, Role: "merger"}
	}
	sweep := func(want int, when string) {
		t.Helper()
		if _, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
			t.Fatal(err)
		}
		if f.enqueues != want || len(merged) != 0 {
			t.Fatalf("%s: enqueues=%d merged=%v, want enqueues=%d and no direct merge", when, f.enqueues, merged, want)
		}
	}

	sweep(1, "first pass")
	removeFromQueue(f, "removal1", mergeQueueBotLogin, "Bot")
	sweep(1, "first rejection seen")
	sweep(1, "base tip read")
	if compares == 0 {
		t.Fatal("the queued lane never read the base tip of the held rejection")
	}
	// Once a first rejection's tip is known nothing reads it again until
	// the recheck: the result would be unused.
	read := compares
	sweep(1, "first rejection, tip known")
	if compares != read {
		t.Fatalf("compares=%d, want %d: no compare while a first rejection's tip is known", compares, read)
	}
	now = now.Add(mergeRefusalRecheck + time.Minute)
	sweep(2, "30-minute retry")
	removeFromQueue(f, "removal2", mergeQueueBotLogin, "Bot")
	rejectedAt := now
	sweep(2, "second rejection seen")
	sweep(2, "second rejection confirmed on base1")
	now = rejectedAt.Add(mergeRefusalRecheck + time.Minute)
	sweep(2, "30 minutes after the second rejection")
	now = rejectedAt.Add(2*mergeRefusalRecheck + time.Minute)
	sweep(2, "an hour after the second rejection")
	now = rejectedAt.Add(nativeQueueRepeatedRecheckFirst)
	sweep(3, "slow retry")
}

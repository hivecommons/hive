package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func TestDispatchDeferredFixQueueLifecycle(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%v", busy), func(t *testing.T) {
			gov := &governor.Governor{}
			msgs := []scheduler.KickMessage{
				{Agent: "scanner", Message: "fix first"},
				{Agent: "scanner", Message: "fix second"},
				{Agent: "scanner", Message: "review third"},
				{Agent: "scanner", Message: "routine governor"},
			}
			q := reviewFixKickQueue{governor: gov, kicks: map[string]review.DispatchKick{
				reviewKickKey(msgs[0]): {Kind: "fix", PRRef: "org/repo#1"},
				reviewKickKey(msgs[1]): {Kind: "fix", PRRef: "org/repo#2"},
				reviewKickKey(msgs[2]): {Kind: "review", PRRef: "org/repo#3"},
			}}
			r := newKickDispatchRecorder()
			if busy {
				r.sendErr["scanner"] = fmt.Errorf("prompt wait: %w", crux.ErrAgentBusy)
			}
			deps := r.deps()
			deps.onDeferred = q.Deferred
			deps.onReviewDelivered = q.Delivered
			dispatchAgentKicks(msgs, false, deps, discardLogger())
			queue := gov.FixerQueue()
			if busy {
				if len(queue) != 2 || queue["org/repo#1"].FixerQueuePosition != 1 || queue["org/repo#2"].FixerQueuePosition != 2 {
					t.Fatalf("busy lane queue = %+v", queue)
				}
				// Reassign the first fix to a fallback and actually deliver it.
				fallback := scheduler.KickMessage{Agent: "fallback", Message: "new fix prompt"}
				q.kicks[reviewKickKey(fallback)] = review.DispatchKick{Kind: "fix", PRRef: "org/repo#1"}
				dispatchAgentKicks([]scheduler.KickMessage{fallback}, false, deps, discardLogger())
			} else if len(queue) != 1 {
				t.Fatalf("kick behind delivered first fix must be queued: %+v", queue)
			}
			// Delivery from another source must not clear a queued fix.
			q.kicks[reviewKickKey(msgs[2])] = review.DispatchKick{Kind: "review", PRRef: "org/repo#2"}
			q.Delivered(msgs[2])
			q.Delivered(msgs[3])
			queue = gov.FixerQueue()
			second := queue["org/repo#2"]
			if len(queue) != 1 || second.FixerQueuePosition != 1 || second.FixerQueueAhead == nil || *second.FixerQueueAhead != 0 {
				t.Fatalf("after first delivery: %+v", queue)
			}
			delete(r.sendErr, "scanner")
			dispatchAgentKicks(msgs[1:2], false, deps, discardLogger())
			if len(gov.FixerQueue()) != 0 {
				t.Fatal("delivered second fix still queued")
			}
		})
	}
}

func TestFixQueueDoesNotRecordWithheldOrFailedKicks(t *testing.T) {
	for _, withheld := range []bool{false, true} {
		gov := &governor.Governor{}
		msg := scheduler.KickMessage{Agent: "scanner", Message: "fix"}
		q := reviewFixKickQueue{governor: gov, kicks: map[string]review.DispatchKick{
			reviewKickKey(msg): {Kind: "fix", PRRef: "org/repo#1"},
		}}
		r := newKickDispatchRecorder()
		r.backoff["scanner"] = withheld
		r.sendErr["scanner"] = errors.New("send failed")
		deps := r.deps()
		deps.onDeferred = q.Deferred
		deps.onReviewDelivered = q.Delivered
		dispatchAgentKicks([]scheduler.KickMessage{msg}, false, deps, discardLogger())
		if len(gov.FixerQueue()) != 0 {
			t.Fatal("non-busy failure or provider backoff recorded as a deferred fix")
		}
	}
}

func TestReconcileDeferredFixQueueOpenHeldAndClosedPRs(t *testing.T) {
	gov := &governor.Governor{}
	for i := 1; i <= 6; i++ {
		gov.RecordFixKickDeferred(fmt.Sprintf("org/repo#%d", i), time.Now())
	}
	reconcileDeferredFixQueue(gov, "org", nil)
	if len(gov.FixerQueue()) != 6 {
		t.Fatal("missing enumeration cleared queue")
	}
	reconcileDeferredFixQueue(gov, "org", &github.ActionableResult{PRs: github.PRResult{
		Items:       []github.PullRequest{{Repo: "repo", Number: 1}},
		Held:        []github.PullRequest{{Repo: "org/repo", Number: 2}},
		StaleDrafts: []github.PullRequest{{Repo: "repo", Number: 3}},
		Attributed: []github.PullRequest{
			{Repo: "org/repo", Number: 4, State: "open"},
			{Repo: "org/repo", Number: 5, State: "closed"},
			{Repo: "org/repo", Number: 6, MergedAt: time.Now()},
		},
	}})
	queue := gov.FixerQueue()
	if len(queue) != 4 || queue["org/repo#4"].FixerQueuePosition != 4 {
		t.Fatalf("reconciled queue = %+v", queue)
	}
	reconcileDeferredFixQueue(gov, "org", &github.ActionableResult{})
	if len(gov.FixerQueue()) != 0 {
		t.Fatal("closed or removed PRs still queued")
	}
}

func TestReconcilePlannedFixQueueDropsPRsNoLongerPlanningAFix(t *testing.T) {
	gov := &governor.Governor{}
	gov.RecordFixKickDeferred("org/repo#1", time.Now())
	gov.RecordFixKickDeferred("org/repo#2", time.Now().Add(time.Minute))
	reconcilePlannedFixQueue(gov, nil, nil)
	if len(gov.FixerQueue()) != 2 {
		t.Fatal("missing enumeration cleared queue")
	}
	// #1 stays open but its fix is no longer planned; #2 still needs one.
	reconcilePlannedFixQueue(gov, &github.ActionableResult{}, []review.DispatchKick{{Kind: "fix", PRRef: "Org/Repo#2"}})
	queue := gov.FixerQueue()
	second := queue["org/repo#2"]
	if len(queue) != 1 || second.FixerQueuePosition != 1 || second.FixerQueueAhead == nil || *second.FixerQueueAhead != 0 {
		t.Fatalf("planned queue = %+v", queue)
	}
}

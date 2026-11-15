package automerge

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// noteQueueFake is the native-queue fake with the GitHub client's status
// section writer, recording each section it was asked to write.
type noteQueueFake struct {
	*nativeQueueFake
	keys, notes []string
}

func (f *noteQueueFake) UpsertPRStatusSection(_ context.Context, _ string, _ int, key, text string) (bool, error) {
	f.keys = append(f.keys, key)
	f.notes = append(f.notes, text)
	return len(f.notes) == 1, nil
}

// A PR the hive withdraws from the merge queue gets one status line saying
// why; a PR whose entry is someone else's, or that stays eligible, gets none.
func TestNativeQueueWithdrawalWritesOneNote(t *testing.T) {
	for _, tc := range []struct {
		name, actor, reason string
		wantNote            bool
	}{
		{"hold on the hive's entry", testHiveAppBotLogin, "held", true},
		{"owner-merge label on the hive's entry", testHiveAppBotLogin, autoMergeReasonHumanMergeRequired, true},
		{"a person queued it", "maintainer", "held", false},
		{"still eligible", testHiveAppBotLogin, nativeQueueReason, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, base, pr, _ := nativeQueueEngine()
			base.state.IsInMergeQueue = true
			base.state.MergeQueueEntry = queueEntry(tc.actor)
			f := &noteQueueFake{nativeQueueFake: base}
			c.transport = f
			pr.Head = &gh.PullRequestBranch{SHA: gh.Ptr("4b02a944c20a80acd446c3bfec6fa23d6b5e1fd7")}
			c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, tc.reason)
			if !tc.wantNote {
				if len(f.notes) != 0 {
					t.Fatalf("notes = %v, want none", f.notes)
				}
				return
			}
			if base.dequeues != 1 || len(f.notes) != 1 || f.keys[0] != prstatus.KeyNativeQueueWithdrawn {
				t.Fatalf("dequeues=%d keys=%v notes=%v", base.dequeues, f.keys, f.notes)
			}
			if !strings.Contains(f.notes[0], nativeQueueWithdrawnMarker) || !strings.Contains(f.notes[0], "`4b02a944c`") || !strings.Contains(f.notes[0], "`"+tc.reason+"`") {
				t.Fatalf("note does not name the marker, head and reason:\n%s", f.notes[0])
			}
			// The entry is gone now: a second pass neither dequeues nor notes.
			c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, tc.reason)
			if base.dequeues != 1 || len(f.notes) != 1 {
				t.Fatalf("second pass: dequeues=%d notes=%d", base.dequeues, len(f.notes))
			}
		})
	}
}

// A transport without the status writer (the unit-test fakes) still
// dequeues; the note is best effort.
func TestNativeQueueWithdrawalWithoutStatusWriter(t *testing.T) {
	c, f, pr, _ := nativeQueueEngine()
	f.state.IsInMergeQueue = true
	f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
	c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, "label:hold")
	if f.dequeues != 1 {
		t.Fatalf("dequeues = %d, want 1", f.dequeues)
	}
}

func TestNativeQueueWithdrawnNote(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"held", "a hold label was added"},
		{"label:hold", "a hold label was added"},
		{"label:do-not-merge/hold", "the `do-not-merge/hold` label was added"},
		{autoMergeReasonLabelBlocked, "the `blocked` label was added"},
		{"label:needs-human", "the `needs-human` label was added"},
		{"excluded-label:wip", "the `wip` label keeps it out of this lane"},
		{autoMergeReasonHumanMergeRequired, "the owner merges it"},
		{"changes-requested", "it is no longer eligible to merge"},
	} {
		note := nativeQueueWithdrawnNote("abcdef0123456789", tc.reason)
		if !strings.Contains(note, tc.want) || !strings.Contains(note, "`abcdef012`") || !strings.HasPrefix(note, nativeQueueWithdrawnMarker+"\n") {
			t.Errorf("%s: note = %q, want it to contain %q", tc.reason, note, tc.want)
		}
		requeue := strings.Contains(note, "queues it again")
		if requeue == (tc.reason == autoMergeReasonHumanMergeRequired) {
			t.Errorf("%s: note = %q; only an owner-merge rule says it will not be queued again", tc.reason, note)
		}
	}
}

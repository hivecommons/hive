package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStoreNotePushWebhook: the webhook re-read of a synchronize delivery
// books a push once the record knows a head, ignores the head it already
// knows, and is dropped by a fresh waiting-on-push state
// (hivecommons/hive#15784).
func TestStoreNotePushWebhook(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("acme/widget", 15516)
	if s.NotePush(key, "bbb2222", now) {
		t.Fatal("NotePush must not create a record")
	}
	s.Observe(key, NeedsAuthor, "ci-red")
	rec, _ := s.Get(key)
	if !rec.WaitingPushSince.Equal(now) {
		t.Fatalf("entering 071 must start the waiting clock, got %+v", rec)
	}
	if s.NotePush(key, "aaa1111", now) {
		t.Fatal("a head the store has never seen is the initial head, not a push")
	}
	s.NoteActivity(key, Activity{Head: "aaa1111", Comments: -1, Reviews: -1})
	if s.NotePush(key, "AAA1111", now) {
		t.Fatal("the known head is not a push")
	}
	pushAt := now.Add(2 * time.Minute)
	now = now.Add(3 * time.Minute)
	s.dirty = false
	if !s.NotePush(key, " bbb2222 ", pushAt) {
		t.Fatal("a new head is a push")
	}
	rec, _ = s.Get(key)
	if rec.LastPushSha != "bbb2222" || !rec.LastPushAt.Equal(pushAt) || !rec.PushedSinceWaiting() || !s.dirty {
		t.Fatalf("push not booked: %+v dirty=%v", rec, s.dirty)
	}
	if s.NotePush(key, "bbb2222", now) {
		t.Fatal("the same push twice is one push")
	}
	// A future instant reads as now.
	s.NotePush(key, "ccc3333", now.Add(time.Hour))
	if rec, _ := s.Get(key); !rec.LastPushAt.Equal(now) || rec.LastPushSha != "ccc3333" {
		t.Fatalf("future push instant must clamp to now: %+v", rec)
	}
	// The sweep re-stages the PR to 070 on the pushed head: the push stays
	// on record, the waiting clock stops.
	s.Observe(key, ReviewPending, "ci-pending")
	s.NoteActivity(key, Activity{Head: "ccc3333", Comments: -1, Reviews: -1})
	rec, _ = s.Get(key)
	if rec.LastPushSha != "ccc3333" || !rec.WaitingPushSince.IsZero() || !rec.PushedSinceWaiting() {
		t.Fatalf("leaving 071 for 070 must keep the push and stop the clock: %+v", rec)
	}
	// Admission leaves the CI / Review column: nothing to show any more.
	s.Observe(key, AwaitingMergeAdmission, "green")
	if rec, _ := s.Get(key); rec.LastPushSha != "" || !rec.LastPushAt.IsZero() {
		t.Fatalf("leaving the column must drop the push: %+v", rec)
	}
	if s.NotePush(key, "ddd4444", now) {
		t.Fatal("a push outside the CI / Review column is not tracked")
	}
}

// TestStoreNotePushEnumerateFallback: with no webhook, the head change the
// next enumerate observes books the push; the same head seen again does
// not; a head the sweep still routes back to the author starts a new
// waiting-on-push state instead.
func TestStoreNotePushEnumerateFallback(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("acme/widget", 15517)
	s.Observe(key, ReviewPending, "ci-pending")
	s.NoteActivity(key, Activity{Head: "aaa1111", Comments: -1, Reviews: -1})
	if rec, _ := s.Get(key); rec.LastPushSha != "" {
		t.Fatalf("the first head is not a push: %+v", rec)
	}
	seen := now.Add(5 * time.Minute)
	now = now.Add(6 * time.Minute)
	s.Observe(key, ReviewPending, "ci-pending")
	s.NoteActivity(key, Activity{Head: "bbb2222", At: seen, Comments: -1, Reviews: -1})
	rec, _ := s.Get(key)
	if rec.LastPushSha != "bbb2222" || !rec.LastPushAt.Equal(seen) {
		t.Fatalf("enumerate head change must book the push: %+v", rec)
	}
	s.Observe(key, ReviewPending, "ci-pending")
	s.NoteActivity(key, Activity{Head: "bbb2222", Comments: -1, Reviews: -1})
	if rec, _ := s.Get(key); !rec.LastPushAt.Equal(seen) {
		t.Fatalf("re-seeing the pushed head must not move the push: %+v", rec)
	}
	// The webhook announced ccc3333 first; the enumerate that follows must
	// not book it a second time.
	webhookAt := now
	now = now.Add(time.Minute)
	s.NotePush(key, "ccc3333", webhookAt)
	s.Observe(key, ReviewPending, "ci-pending")
	s.NoteActivity(key, Activity{Head: "ccc3333", Comments: -1, Reviews: -1})
	if rec, _ := s.Get(key); !rec.LastPushAt.Equal(webhookAt) {
		t.Fatalf("enumerate after the webhook must keep the webhook's instant: %+v", rec)
	}
	// CI went red on ccc3333: 071. The waiting clock starts, the push is
	// history.
	s.Observe(key, NeedsAuthor, "ci-red")
	rec, _ = s.Get(key)
	if !rec.WaitingPushSince.Equal(now) || rec.LastPushSha != "" || rec.PushedSinceWaiting() {
		t.Fatalf("entering 071 must reset the push state: %+v", rec)
	}
	// The author pushes ddd4444, the sweep evaluates it and still says 071:
	// a new waiting-on state on the new head, not a push in progress.
	s.NotePush(key, "ddd4444", now)
	if rec, _ := s.Get(key); !rec.PushedSinceWaiting() {
		t.Fatalf("webhook push while waiting must read as pushed: %+v", rec)
	}
	later := now.Add(10 * time.Minute)
	now = later
	s.Observe(key, NeedsAuthor, "ci-red")
	s.NoteActivity(key, Activity{Head: "ddd4444", Comments: -1, Reviews: -1})
	rec, _ = s.Get(key)
	if !rec.WaitingPushSince.Equal(later) || rec.LastPushSha != "" || rec.PushedSinceWaiting() {
		t.Fatalf("a head still routed to the author must start a new waiting state: %+v", rec)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewStore(s.path, func() time.Time { return now })
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if rec, ok := reloaded.Get(key); !ok || !rec.WaitingPushSince.Equal(later) {
		t.Fatalf("waiting clock must survive a reload: %+v ok=%v", rec, ok)
	}
}

// TestStoreReworkPushSurvivesReAsk (hivecommons/hive#16021): the author
// answers a 071 ask with a new head; the sweep re-stages the PR to 050 and,
// the head red again, back to 071 within one pass, re-nudging for it. That
// re-entry restarts the waiting-on-push state and the nudge clock, but the
// record keeps the original ask and the push that answered it.
func TestStoreReworkPushSurvivesReAsk(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 48, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("hivecommons/hive", 15849)
	asked := now
	s.Observe(key, NeedsAuthor, "required-check-failing:build-and-test")
	s.NoteActivity(key, Activity{Head: "3746f58", Comments: -1, Reviews: -1})
	s.MarkNudged(key, "3746f58", 1, false, now)
	rec, _ := s.Get(key)
	if !rec.ReworkAskedAt.Equal(asked) || rec.ReworkPushedHead != "" {
		t.Fatalf("071 entry must open the ask and nothing else: %+v", rec)
	}
	// The webhook sees the force-push (rebased commits keep their old
	// dates; only the delivery instant counts).
	pushAt := now.Add(12 * time.Hour)
	now = pushAt.Add(time.Minute)
	if !s.NotePush(key, "0ff4ed1", pushAt) {
		t.Fatal("new head is a push")
	}
	// Sweep A: CI pending on the new head → 050, waiting on the hive.
	now = now.Add(time.Minute)
	s.Observe(key, ReviewPending, "required-check-pending:sign-off")
	s.NoteActivity(key, Activity{Head: "0ff4ed1", Comments: -1, Reviews: -1, WaitingOnHive: true})
	// Sweep B, seconds later: an optional check failed fast → 071 again,
	// nudged for the new head.
	now = now.Add(12 * time.Second)
	s.Observe(key, NeedsAuthor, "optional-check-failing:server-tests")
	s.MarkNudged(key, "0ff4ed1", 1, false, now)
	s.NoteActivity(key, Activity{Head: "0ff4ed1", Comments: -1, Reviews: -1})
	rec, _ = s.Get(key)
	if rec.LastPushSha != "" || !rec.WaitingPushSince.Equal(now) || rec.NudgeHead != "0ff4ed1" || !rec.FirstNudgeAt.Equal(now) {
		t.Fatalf("re-entry must restart the push wait and the nudge clock as before: %+v", rec)
	}
	head, at, ok := rec.ReworkPushed()
	if !ok || head != "0ff4ed1" || !at.Equal(pushAt) || !rec.ReworkAskedAt.Equal(asked) {
		t.Fatalf("the push that answered the ask must survive the re-ask: %+v", rec)
	}
	// A second push the webhook missed, seen by the enumerate while the
	// row still sits at 071, replaces it.
	now = now.Add(time.Hour)
	s.NoteActivity(key, Activity{Head: "0157cc9", Comments: -1, Reviews: -1})
	rec, _ = s.Get(key)
	if head, at, _ = rec.ReworkPushed(); head != "0157cc9" || !at.Equal(now) {
		t.Fatalf("enumerate head change must book the newer push: %+v", rec)
	}
	// The hive evaluates it, finds it green and waits on nobody: the
	// exchange is over, the next 071 is a fresh ask.
	now = now.Add(time.Minute)
	s.Observe(key, ReviewPending, "review-gate:awaiting-review-approval")
	s.NoteActivity(key, Activity{Head: "0157cc9", Comments: -1, Reviews: -1})
	rec, _ = s.Get(key)
	if _, _, ok := rec.ReworkPushed(); ok || !rec.ReworkAskedAt.IsZero() {
		t.Fatalf("settled head must close the ask: %+v", rec)
	}
	now = now.Add(time.Hour)
	s.Observe(key, NeedsAuthor, LaneReasonChangesRequested)
	if rec, _ := s.Get(key); !rec.ReworkAskedAt.Equal(now) || rec.ReworkPushedHead != "" {
		t.Fatalf("a later 071 is a new ask: %+v", rec)
	}
	// Leaving the CI / Review column forgets it all.
	s.Observe(key, AwaitingMergeAdmission, "green")
	if rec, _ := s.Get(key); !rec.ReworkAskedAt.IsZero() || rec.ReworkPushedHead != "" {
		t.Fatalf("leaving the column must clear the ask: %+v", rec)
	}
}

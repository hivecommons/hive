package stagelabels

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#15784: the stage record keeps when a waiting-on-push
// state began and the last push the hive saw, so the tracker can say
// whether the contributor acted.

func pushTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	return st, &now
}

func TestWaitingPushSinceSetOnEntryAndClearedOnLeave(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 1)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	if rec, _ := st.Get(key); !rec.WaitingPushSince.IsZero() {
		t.Fatalf("050 is not a wait on the author: %v", rec.WaitingPushSince)
	}
	*now = now.Add(time.Minute)
	entered := *now
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	if rec, _ := st.Get(key); !rec.WaitingPushSince.Equal(entered) {
		t.Fatalf("072 entry must start the wait: got %v want %v", rec.WaitingPushSince, entered)
	}
	*now = now.Add(time.Minute)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.Observe(key, NeedsAuthor, "contributor-lane:ci-red")
	if rec, _ := st.Get(key); !rec.WaitingPushSince.Equal(entered) {
		t.Fatalf("re-observing / 072→071 must keep the wait's start: got %v", rec.WaitingPushSince)
	}
	*now = now.Add(time.Minute)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	if rec, _ := st.Get(key); !rec.WaitingPushSince.IsZero() {
		t.Fatalf("leaving the wait must clear it: %v", rec.WaitingPushSince)
	}
}

func TestNotePushRecordsNewHeadOnly(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 2)
	if st.NotePush(key, "aaa", *now) {
		t.Fatal("no record: nothing to note")
	}
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	if st.NotePush(key, "aaa", *now) {
		t.Fatal("no observed head yet: a re-read cannot tell a push")
	}
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	if st.NotePush(key, "aaa", *now) {
		t.Fatal("the observed head is not a push")
	}
	*now = now.Add(5 * time.Minute)
	pushedAt := *now
	if !st.NotePush(key, "bbb", pushedAt) {
		t.Fatal("a new head must be recorded")
	}
	rec, _ := st.Get(key)
	if rec.LastPushSHA != "bbb" || !rec.LastPushAt.Equal(pushedAt) || !rec.PushedSinceWaiting() {
		t.Fatalf("push not recorded: %+v", rec)
	}
	*now = now.Add(time.Minute)
	if st.NotePush(key, "bbb", *now) {
		t.Fatal("the same push must not be re-recorded")
	}
	if rec, _ = st.Get(key); !rec.LastPushAt.Equal(pushedAt) {
		t.Fatalf("re-read moved the push time: %v", rec.LastPushAt)
	}
}

func TestNoteActivityRecordsPushAsFallback(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 3)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	if rec, _ := st.Get(key); rec.LastPushSHA != "" {
		t.Fatalf("the first observed head is not a push: %+v", rec)
	}
	*now = now.Add(10 * time.Minute)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	st.NoteActivity(key, Activity{Head: "bbb", Comments: -1, Reviews: -1})
	rec, _ := st.Get(key)
	if rec.LastPushSHA != "bbb" || !rec.LastPushAt.Equal(*now) {
		t.Fatalf("enumeration fallback did not record the push: %+v", rec)
	}
	// The webhook already saw it: no second record.
	st.NotePush(key, "ccc", *now)
	*now = now.Add(time.Minute)
	st.NoteActivity(key, Activity{Head: "ccc", Comments: -1, Reviews: -1})
	if rec, _ = st.Get(key); !rec.LastPushAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("a push the webhook recorded must keep its time: %v", rec.LastPushAt)
	}
}

func TestFreshWaitResetsPush(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 4)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	*now = now.Add(time.Minute)
	st.NotePush(key, "bbb", *now)
	*now = now.Add(time.Minute)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	if rec, _ := st.Get(key); rec.LastPushSHA != "bbb" {
		t.Fatalf("leaving the wait must keep the push for the CI / Review row: %+v", rec)
	}
	*now = now.Add(time.Minute)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	rec, _ := st.Get(key)
	if rec.LastPushSHA != "" || !rec.LastPushAt.IsZero() || rec.PushedSinceWaiting() || !rec.WaitingPushSince.Equal(*now) {
		t.Fatalf("a fresh wait must reset the push: %+v", rec)
	}
}

func TestFreshWaitOnNewHeadIsNotAPush(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 5)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	*now = now.Add(time.Minute)
	// One pass sees a new head that is already red: the wait is about it.
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.NoteActivity(key, Activity{Head: "bbb", Comments: -1, Reviews: -1})
	if rec, _ := st.Get(key); rec.PushedSinceWaiting() {
		t.Fatalf("the head the wait began on is not a push answering it: %+v", rec)
	}
}

func TestMarkNudgedOnPushedHeadStartsFreshWait(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 6)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	st.MarkNudged(key, "aaa", 1, true, *now)
	*now = now.Add(time.Minute)
	st.NotePush(key, "bbb", *now)
	if rec, _ := st.Get(key); !rec.PushedSinceWaiting() {
		t.Fatalf("push not seen: %+v", rec)
	}
	*now = now.Add(30 * time.Minute)
	st.MarkNudged(key, "bbb", 1, true, *now)
	rec, _ := st.Get(key)
	if rec.PushedSinceWaiting() || rec.LastPushSHA != "" || !rec.WaitingPushSince.Equal(*now) {
		t.Fatalf("a nudge on the pushed head must start a fresh wait: %+v", rec)
	}
}

func TestPushFieldsPersist(t *testing.T) {
	st, now := pushTestStore(t)
	key := Key("o/r", 7)
	st.Observe(key, ReturnedToContributor, "contributor-lane:ci-red")
	st.NoteActivity(key, Activity{Head: "aaa", Comments: -1, Reviews: -1})
	st.NotePush(key, "bbb", *now)
	rec, _ := st.Get(key)
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"waiting_push_since"`, `"last_push_at"`, `"last_push_sha":"bbb"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("record JSON lacks %s: %s", k, b)
		}
	}
}

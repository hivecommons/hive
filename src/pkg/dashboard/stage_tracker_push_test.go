package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15784: the webhook re-read after a synchronize records
// the push on the stage record and the tracker row carries it.

func TestWebhookReReadRecordsPushAndRowCarriesIt(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	key := stagelabels.Key("hivecommons/hive", 14524)
	store.NoteActivity(key, stagelabels.Activity{Head: "feed", Comments: -1, Reviews: -1})

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if it.WaitingPushSince == nil || !it.WaitingPushSince.Equal(now.Add(-26*time.Hour)) {
		t.Fatalf("waiting_push_since = %v, want the 072 entry", it.WaitingPushSince)
	}
	if it.LastPushAt != nil || it.LastPushSHA != "" || it.PushedSinceWaiting {
		t.Fatalf("no push yet: %+v", it)
	}

	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "hivecommons/hive", Number: 14524, IsPR: true, State: "open", HeadSHA: "abc1234ff", FetchedAt: now}})
	rec, _ := store.Get(key)
	if rec.LastPushSHA != "abc1234ff" || !rec.LastPushAt.Equal(now) {
		t.Fatalf("webhook re-read did not record the push: %+v", rec)
	}
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if it.LastPushSHA != "abc1234ff" || it.LastPushAt == nil || !it.LastPushAt.Equal(now) || !it.PushedSinceWaiting {
		t.Fatalf("row push progress = sha %q at %v since-waiting %v", it.LastPushSHA, it.LastPushAt, it.PushedSinceWaiting)
	}
	b, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"waiting_push_since"`, `"last_push_at"`, `"last_push_sha":"abc1234ff"`, `"pushed_since_waiting":true`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("row JSON lacks %s: %s", k, b)
		}
	}
}

func TestWebhookReReadIgnoresKnownHeadsClosedAndIssues(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, _ := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	key := stagelabels.Key("hivecommons/hive", 14524)
	store.NoteActivity(key, stagelabels.Activity{Head: "feed", Comments: -1, Reviews: -1})
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		{Repo: "hivecommons/hive", Number: 14524, IsPR: true, State: "open", HeadSHA: "feed", FetchedAt: now},
		{Repo: "hivecommons/hive", Number: 14524, IsPR: true, State: "closed", HeadSHA: "dead", FetchedAt: now},
		{Repo: "hivecommons/hive", Number: 14524, IsPR: false, State: "open", HeadSHA: "dead", FetchedAt: now},
	})
	if rec, _ := store.Get(key); rec.LastPushSHA != "" {
		t.Fatalf("no push should be recorded: %+v", rec)
	}
}

func TestStageTrackerPushProgressPROnly(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{WaitingPushSince: now, LastPushAt: now, LastPushSHA: "abc"}
	issue := StageTrackerItem{Kind: "issue"}
	stageTrackerPushProgress(&issue, rec)
	if issue.WaitingPushSince != nil || issue.LastPushSHA != "" || issue.PushedSinceWaiting {
		t.Fatalf("issues never carry push progress: %+v", issue)
	}
	pr := StageTrackerItem{Kind: "pr"}
	stageTrackerPushProgress(&pr, rec)
	if pr.LastPushSHA != "abc" || !pr.PushedSinceWaiting {
		t.Fatalf("pr push progress = %+v", pr)
	}
}

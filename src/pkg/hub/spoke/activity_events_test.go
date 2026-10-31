package spoke

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hubactivity"
)

// activitySnapshotBox is a tracker snapshot (items and merges) a test can
// swap.
type activitySnapshotBox struct {
	mu   sync.Mutex
	snap hubactivity.Snapshot
}

func (b *activitySnapshotBox) Set(items []hubactivity.Item, merged ...hubactivity.Merge) {
	b.mu.Lock()
	b.snap = hubactivity.Snapshot{Items: items, Merged: merged}
	b.mu.Unlock()
}

func (b *activitySnapshotBox) Snapshot(context.Context) (hubactivity.Snapshot, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return hubactivity.Snapshot{Items: append([]hubactivity.Item(nil), b.snap.Items...), Merged: append([]hubactivity.Merge(nil), b.snap.Merged...)}, true
}

func testEventPublisher(opts ActivityOptions, box *activitySnapshotBox, clock *activityClock) *activityPublisher {
	p := newActivityPublisher(ActivitySource{
		Options:  func() ActivityOptions { return opts },
		Snapshot: box.Snapshot,
	}, "", nil2Logger())
	p.now = clock.Now
	return p
}

func stagedItem(repo string, n int, stage string) hubactivity.Item {
	return hubactivity.Item{Repo: repo, Number: n, Kind: "pr", Title: fmt.Sprintf("%s #%d", repo, n), Stage: stage, StageNumber: 10}
}

func eventKeys(events []hubactivity.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s#%d:%s:%s>%s", e.Repo, e.Number, e.Type, e.From, e.Stage))
	}
	return out
}

func sameKeys(got, want []string) bool {
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// Acceptance (#15964): a merge on the spoke rides on the very next
// heartbeat's activity block, alongside the stage hops and opens seen
// since the previous one. The first snapshot only seeds the baseline.
func TestActivityEventsStageHopsOpensAndMergeOnTheNextBeat(t *testing.T) {
	box := &activitySnapshotBox{}
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "ci_review"), stagedItem("o/r", 2, "ci_review")})
	clock := &activityClock{now: activityTestT0}
	p := testEventPublisher(ActivityOptions{Enabled: true}, box, clock)
	ctx := context.Background()

	if seed := p.heartbeatDigestLocked(ctx); len(seed.Events) != 0 {
		t.Fatalf("first beat replayed the board as events: %v", eventKeys(seed.Events))
	}

	mergedAt := activityTestT0.Add(time.Minute)
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "merge_queue"), stagedItem("o/r", 3, "actionable")},
		hubactivity.Merge{Repo: "o/r", Number: 2, MergedAt: mergedAt})
	clock.Advance(2 * time.Minute)
	d := p.heartbeatDigestLocked(ctx)

	want := []string{"o/r#1:stage:ci_review>merge_queue", "o/r#3:opened:>actionable", "o/r#2:merged:ci_review>"}
	if got := eventKeys(d.Events); !sameKeys(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	merge := d.Events[2]
	if !merge.At.Equal(mergedAt) || merge.Title != "o/r #2" {
		t.Fatalf("merge event = %+v, want the merge time and the item's last title", merge)
	}
	for i := 1; i < len(d.Events); i++ {
		if d.Events[i].Seq <= d.Events[i-1].Seq {
			t.Fatalf("sequence numbers not increasing: %v", d.Events)
		}
	}

	// The merge stays in the tracker's recent list: no second event.
	clock.Advance(2 * time.Minute)
	p.acknowledgeEventsLocked(d.Events[len(d.Events)-1].Seq)
	if again := p.heartbeatDigestLocked(ctx); len(again.Events) != 0 {
		t.Fatalf("an already reported merge was reported again: %v", eventKeys(again.Events))
	}
}

// Events stay buffered (and are resent) until the hub acknowledges them;
// an acknowledgement drops exactly the events up to its sequence number.
func TestActivityEventsTrimmedOnlyByAcknowledgement(t *testing.T) {
	box := &activitySnapshotBox{}
	box.Set(nil)
	p := testEventPublisher(ActivityOptions{Enabled: true}, box, &activityClock{now: activityTestT0})
	ctx := context.Background()
	p.heartbeatDigestLocked(ctx)

	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a")})
	first := p.heartbeatDigestLocked(ctx)
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a"), stagedItem("o/r", 2, "a")})
	second := p.heartbeatDigestLocked(ctx)
	if len(first.Events) != 1 || len(second.Events) != 2 {
		t.Fatalf("unacknowledged events must be resent: %d then %d", len(first.Events), len(second.Events))
	}

	// An accepted response without an events ack (a hub before #15964,
	// or one that restarted) keeps the buffer.
	p.acknowledgeLocked(second.ETag)
	p.acknowledgeEventsLocked(0)
	marker := p.heartbeatDigestLocked(ctx)
	if !marker.IsUnchangedMarker() || len(marker.Events) != 2 {
		t.Fatalf("marker = %+v, want the etag only plus both pending events", marker)
	}

	p.acknowledgeEventsLocked(first.Events[0].Seq)
	if rest := p.heartbeatDigestLocked(ctx); len(rest.Events) != 1 || rest.Events[0].Number != 2 {
		t.Fatalf("after a partial ack events = %v, want only #2", eventKeys(rest.Events))
	}
	p.acknowledgeEventsLocked(second.Events[1].Seq)
	if done := p.heartbeatDigestLocked(ctx); len(done.Events) != 0 {
		t.Fatalf("after a full ack events = %v, want none", eventKeys(done.Events))
	}
}

// The buffer holds at most hubactivity.MaxEvents: the oldest go first and
// the drop count is disclosed until an acknowledgement covers it.
func TestActivityEventsCapAt200DisclosesDrops(t *testing.T) {
	box := &activitySnapshotBox{}
	box.Set(nil)
	p := testEventPublisher(ActivityOptions{Enabled: true, MaxItems: 1}, box, &activityClock{now: activityTestT0})
	ctx := context.Background()
	p.heartbeatDigestLocked(ctx)

	var items []hubactivity.Item
	for n := 1; n <= 250; n++ {
		items = append(items, stagedItem("o/r", n, "a"))
	}
	box.Set(items)
	d := p.heartbeatDigestLocked(ctx)

	if len(d.Events) != hubactivity.MaxEvents || d.EventsDropped != 50 {
		t.Fatalf("events = %d dropped = %d, want %d and 50", len(d.Events), d.EventsDropped, hubactivity.MaxEvents)
	}
	if d.Events[0].Number != 51 || d.Events[len(d.Events)-1].Number != 250 {
		t.Fatalf("kept #%d..#%d, want the newest (#51..#250)", d.Events[0].Number, d.Events[len(d.Events)-1].Number)
	}

	p.acknowledgeEventsLocked(d.Events[len(d.Events)-1].Seq)
	if after := p.heartbeatDigestLocked(ctx); len(after.Events) != 0 || after.EventsDropped != 0 {
		t.Fatalf("after the ack: %d events, %d dropped; want both cleared", len(after.Events), after.EventsDropped)
	}
}

// Private-repository items yield no events unless the spoke shares them.
func TestActivityEventsExcludePrivateItemsUnlessShared(t *testing.T) {
	for _, share := range []bool{false, true} {
		box := &activitySnapshotBox{}
		box.Set(nil)
		p := testEventPublisher(ActivityOptions{Enabled: true, SharePrivate: share}, box, &activityClock{now: activityTestT0})
		ctx := context.Background()
		p.heartbeatDigestLocked(ctx)

		secret := stagedItem("o/secret", 9, "a")
		secret.Private = true
		box.Set([]hubactivity.Item{secret, stagedItem("o/public", 1, "a")},
			hubactivity.Merge{Repo: "o/secret", Number: 8, MergedAt: activityTestT0, Private: true})
		d := p.heartbeatDigestLocked(ctx)

		var private int
		for _, e := range d.Events {
			if e.Repo == "o/secret" {
				private++
				if !e.Private {
					t.Fatalf("shared private event not marked private: %+v", e)
				}
			}
		}
		if want := map[bool]int{false: 0, true: 2}[share]; private != want {
			t.Fatalf("share_private=%v: %d private events, want %d (%v)", share, private, want, eventKeys(d.Events))
		}
	}
}

// Turning publishing off forgets the buffer, so turning it back on starts
// from a fresh baseline instead of replaying stale movement.
func TestActivityEventsResetWhenDisabled(t *testing.T) {
	opts := ActivityOptions{Enabled: true}
	box := &activitySnapshotBox{}
	box.Set(nil)
	p := newActivityPublisher(ActivitySource{Options: func() ActivityOptions { return opts }, Snapshot: box.Snapshot}, "", nil2Logger())
	ctx := context.Background()
	p.heartbeatDigestLocked(ctx)
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a")})
	p.heartbeatDigestLocked(ctx)

	opts.Enabled = false
	if d := p.heartbeatDigestLocked(ctx); d != nil {
		t.Fatalf("disabled publisher sent %+v", d)
	}
	opts.Enabled = true
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a"), stagedItem("o/r", 2, "a")})
	if d := p.heartbeatDigestLocked(ctx); len(d.Events) != 0 {
		t.Fatalf("re-enabled publisher replayed %v", eventKeys(d.Events))
	}
}

func TestRedactActivityAppliesToEvents(t *testing.T) {
	d := hubactivity.Digest{Events: []hubactivity.Event{{Seq: 1, Type: hubactivity.EventMerged, Repo: "o/r", Number: 1, Title: "secret"}}, EventsDropped: 4}

	repos := redactActivity(d, map[string]bool{config.HeartbeatOmitRepos: true})
	if len(repos.Events) != 0 || repos.EventsDropped != 0 {
		t.Fatalf("omit repos left events: %+v", repos)
	}
	titles := redactActivity(d, map[string]bool{config.HeartbeatOmitTaskTitles: true})
	if len(titles.Events) != 1 || titles.Events[0].Title != "" {
		t.Fatalf("omit task titles left a title: %+v", titles.Events)
	}
	if d.Events[0].Title != "secret" {
		t.Fatal("redaction modified the input")
	}
}

// An out-of-band update carries new events even when the digest itself is
// unchanged, as an unchanged marker, and applies the event ack.
func TestActivityUpdateSendsPendingEventsOnAMarker(t *testing.T) {
	box := &activitySnapshotBox{}
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a")})
	clock := &activityClock{now: activityTestT0}
	p := testEventPublisher(ActivityOptions{Enabled: true}, box, clock)
	rec := &recordingPost{sent: make(chan *hubactivity.Digest, 4), status: 200, ok: true}
	p.post = rec.post
	ctx := context.Background()

	if !p.update(ctx) {
		t.Fatal("first update not sent")
	}
	<-rec.sent
	if p.update(ctx) {
		t.Fatal("nothing changed: no update expected")
	}

	// A merge of an item no longer tracked leaves the digest unchanged.
	box.Set([]hubactivity.Item{stagedItem("o/r", 1, "a")}, hubactivity.Merge{Repo: "o/r", Number: 5, MergedAt: activityTestT0})
	if !p.update(ctx) {
		t.Fatal("pending merge event not sent")
	}
	sent := <-rec.sent
	if !sent.IsUnchangedMarker() || len(sent.Events) != 1 || sent.Events[0].Type != hubactivity.EventMerged {
		t.Fatalf("update = %+v, want a marker carrying the merge", sent)
	}
	if len(p.events) != 0 {
		t.Fatalf("acknowledged events still buffered: %v", eventKeys(p.events))
	}
}

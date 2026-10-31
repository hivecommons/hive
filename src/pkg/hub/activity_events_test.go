package hub

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hubactivity"
)

func activityEv(seq uint64, typ, repo string, n int) hubactivity.Event {
	return hubactivity.Event{Seq: seq, Type: typ, Repo: repo, Number: n, Kind: "pr", Title: fmt.Sprintf("%s #%d", repo, n), Stage: "ci_review", At: activityT0}
}

func withEvents(d *hubactivity.Digest, events ...hubactivity.Event) *hubactivity.Digest {
	out := *d
	out.Events = events
	return &out
}

func viewEventKeys(v ActivityView) []string {
	out := make([]string, 0, len(v.Events))
	for _, e := range v.Events {
		out = append(out, fmt.Sprintf("%s:%s#%d:%s", e.HiveID, e.Repo, e.Number, e.Type))
	}
	return out
}

// Acceptance (#15964): a merge a spoke reports on a heartbeat is served by
// /api/hub/activity right after that one heartbeat, and the ETag changes
// so a poller picks it up.
func TestHubActivityEvents_MergeAppearsWithinOneHeartbeat(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	mkUser(t, "root")

	d := activityDigest("o/a", 1, 2)
	postActivityHeartbeat(t, s, "hive-a", d)
	first := getActivity(t, s, "root", "", "")
	if v := decodeActivity(t, first); len(v.Events) != 0 {
		t.Fatalf("events before any movement = %v", viewEventKeys(v))
	}
	etag := first.Header().Get("ETag")

	marker := &hubactivity.Digest{ETag: d.ETag, GeneratedAt: activityT0.Add(time.Minute)}
	merge := activityEv(7, hubactivity.EventMerged, "o/a", 2)
	resp := postActivityHeartbeat(t, s, "hive-a", withEvents(marker, merge))
	if resp.ActivityEventsAck != 7 || resp.ActivityETag != d.ETag {
		t.Fatalf("heartbeat ack = %q / %d, want the held etag and event seq 7", resp.ActivityETag, resp.ActivityEventsAck)
	}

	rec := getActivity(t, s, "root", "", etag)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
		t.Fatalf("new events must change the etag: status %d etag %q (was %q)", rec.Code, rec.Header().Get("ETag"), etag)
	}
	v := decodeActivity(t, rec)
	if got := viewEventKeys(v); len(got) != 1 || got[0] != "hive-a:o/a#2:merged" {
		t.Fatalf("events = %v, want the merge", got)
	}
	if len(v.Items) != 2 {
		t.Fatalf("a marker carrying events must keep the held digest: items %d", len(v.Items))
	}
}

// An event of a hive the user is not authorized for is never delivered;
// filtering happens before the response (and its etag) is built.
func TestHubActivityEvents_PerUserScoping(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()

	postActivityHeartbeat(t, s, "hive-a", withEvents(activityDigest("o/a", 1), activityEv(1, hubactivity.EventOpened, "o/a", 1)))
	postActivityHeartbeat(t, s, "hive-b", withEvents(activityDigest("o/b", 3), activityEv(1, hubactivity.EventMerged, "o/b", 3)))
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hive-a": "viewer"}}); err != nil {
		t.Fatal(err)
	}
	mkUser(t, "mallory")
	mkUser(t, "root")

	rec := getActivity(t, s, "alice", "", "")
	if got := viewEventKeys(decodeActivity(t, rec)); len(got) != 1 || got[0] != "hive-a:o/a#1:opened" {
		t.Fatalf("alice events = %v, want only hive-a's", got)
	}
	if strings.Contains(rec.Body.String(), "o/b") {
		t.Fatalf("hive-b leaked to alice: %s", rec.Body.String())
	}
	aliceETag := rec.Header().Get("ETag")

	// Movement on a hive alice cannot see must not even change her etag.
	postActivityHeartbeat(t, s, "hive-b", withEvents(activityDigest("o/b", 3), activityEv(2, hubactivity.EventStage, "o/b", 3)))
	if again := getActivity(t, s, "alice", "", aliceETag); again.Code != http.StatusNotModified {
		t.Fatalf("hive-b movement changed alice's view: status %d", again.Code)
	}

	if v := decodeActivity(t, getActivity(t, s, "mallory", "", "")); len(v.Events) != 0 {
		t.Fatalf("ungranted user saw events %v", viewEventKeys(v))
	}
	if got := viewEventKeys(decodeActivity(t, getActivity(t, s, "root", "", ""))); len(got) != 3 {
		t.Fatalf("admin events = %v, want all three", got)
	}

	// Filters apply to events too.
	if got := viewEventKeys(decodeActivity(t, getActivity(t, s, "root", "?hive=hive-b", ""))); len(got) != 2 {
		t.Fatalf("hive filter events = %v, want hive-b's two", got)
	}
	if got := viewEventKeys(decodeActivity(t, getActivity(t, s, "root", "?mod=stalled", ""))); len(got) != 0 {
		t.Fatalf("modifier filter must drop events of items without it: %v", got)
	}
}

// Private items the spoke does not share never become events (the spoke's
// exclusion, tested in pkg/hub/spoke); an event the hub receives for a hive
// it then forgets is purged with it.
func TestHubActivityEvents_ForgetPurgesEvents(t *testing.T) {
	var st activityStore
	st.record("hive-a", withEvents(activityDigest("o/a", 1), activityEv(1, hubactivity.EventOpened, "o/a", 1)), activityT0)
	st.record("hive-b", withEvents(activityDigest("o/b", 1), activityEv(1, hubactivity.EventOpened, "o/b", 1)), activityT0)

	st.forget("hive-a")

	snap := st.eventsSnapshot([]string{"hive-a", "hive-b"})
	if len(snap.events) != 1 || snap.events[0].hiveID != "hive-b" {
		t.Fatalf("events after forget = %+v", snap.events)
	}
	if st.eventsAck("hive-a") != 0 {
		t.Fatal("a forgotten hive must lose its event acknowledgement too")
	}
}

func TestActivityEventRing_DedupesByAcknowledgedSeq(t *testing.T) {
	var st activityStore
	batch := withEvents(activityDigest("o/a", 1), activityEv(10, hubactivity.EventOpened, "o/a", 1), activityEv(11, hubactivity.EventStage, "o/a", 1))
	batch.EventsDropped = 3

	st.record("hive-a", batch, activityT0)
	// The spoke lost the ack and resends the same batch plus one new event;
	// its cumulative disclosure restates the 3 drops already counted.
	resend := withEvents(batch, append(batch.Events, activityEv(12, hubactivity.EventMerged, "o/a", 1))...)
	st.record("hive-a", resend, activityT0.Add(time.Minute))
	// A batch with nothing new does not count its drops again.
	st.record("hive-a", batch, activityT0.Add(2*time.Minute))

	snap := st.eventsSnapshot([]string{"hive-a"})
	if len(snap.events) != 3 {
		t.Fatalf("stored %d events, want 3 (no duplicates)", len(snap.events))
	}
	if got := st.eventsAck("hive-a"); got != 12 {
		t.Fatalf("ack = %d, want 12", got)
	}
	if snap.dropped["hive-a"] != 3 {
		t.Fatalf("disclosed drops = %d, want 3 (a resend restates, not repeats, them)", snap.dropped["hive-a"])
	}

	// A resend whose disclosure grew counts only the increase.
	grown := withEvents(resend, append(resend.Events, activityEv(13, hubactivity.EventStage, "o/a", 1))...)
	grown.EventsDropped = 5
	st.record("hive-a", grown, activityT0.Add(3*time.Minute))
	if got := st.eventsSnapshot([]string{"hive-a"}).dropped["hive-a"]; got != 5 {
		t.Fatalf("drops after grown resend = %d, want 5", got)
	}

	// After the ack lands the spoke trims and resets its disclosure, so a
	// batch of only new events discloses only new drops, counted in full.
	acked := withEvents(activityDigest("o/a", 1), activityEv(14, hubactivity.EventStage, "o/a", 1))
	acked.EventsDropped = 2
	st.record("hive-a", acked, activityT0.Add(4*time.Minute))
	if got := st.eventsSnapshot([]string{"hive-a"}).dropped["hive-a"]; got != 7 {
		t.Fatalf("drops after acked batch = %d, want 7", got)
	}
}

// Known limit, pinned so a change is deliberate: a lost ack followed by a
// full buffer turnover (more than hubactivity.MaxEvents new events) leaves
// no repeated Seq in the resend, so the hub cannot see that the spoke's
// cumulative disclosure restates drops already counted and counts the
// earlier batch's drops once more. The over-count is bounded by that one
// batch's disclosure; stored events are never duplicated.
func TestActivityEventRing_RestatedDropsAfterFullTurnoverOverCountOnce(t *testing.T) {
	var st activityStore
	first := withEvents(activityDigest("o/a", 1), activityEv(1, hubactivity.EventOpened, "o/a", 1), activityEv(2, hubactivity.EventStage, "o/a", 1))
	first.EventsDropped = 3
	st.record("hive-a", first, activityT0)

	// The ack is lost and the spoke buffers MaxEvents newer events, so the
	// resend holds only Seq 3.. and its disclosure 3 (old) + 2 (new) = 5.
	turned := make([]hubactivity.Event, 0, hubactivity.MaxEvents)
	for i := range hubactivity.MaxEvents {
		turned = append(turned, activityEv(uint64(3+i), hubactivity.EventStage, "o/a", 1))
	}
	resend := withEvents(first, turned...)
	resend.EventsDropped = 5
	st.record("hive-a", resend, activityT0.Add(time.Minute))

	snap := st.eventsSnapshot([]string{"hive-a"})
	if len(snap.events) != 2+hubactivity.MaxEvents {
		t.Fatalf("stored %d events, want %d (no duplicates)", len(snap.events), 2+hubactivity.MaxEvents)
	}
	if got := st.eventsAck("hive-a"); got != uint64(2+hubactivity.MaxEvents) {
		t.Fatalf("ack = %d, want %d", got, 2+hubactivity.MaxEvents)
	}
	// Exact accounting would be 5; the undetectable restatement adds the
	// first batch's 3 once more, and never more than that.
	if snap.dropped["hive-a"] != 8 {
		t.Fatalf("disclosed drops = %d, want 8 (5 exact + the 3 restated once, undetectable without a repeated Seq)", snap.dropped["hive-a"])
	}

	// A further resend that does repeat a Seq is detected again and only
	// the increase over the last counted disclosure is added.
	again := withEvents(resend, turned[len(turned)-1], activityEv(uint64(3+hubactivity.MaxEvents), hubactivity.EventMerged, "o/a", 1))
	again.EventsDropped = 6
	st.record("hive-a", again, activityT0.Add(2*time.Minute))
	if got := st.eventsSnapshot([]string{"hive-a"}).dropped["hive-a"]; got != 9 {
		t.Fatalf("drops after detected resend = %d, want 9", got)
	}
}

// The fleet-wide ring keeps the last 1,000 events across all hives, oldest
// dropped first; each block is also capped at hubactivity.MaxEvents with
// the overflow disclosed.
func TestActivityEventRing_FleetCapAndPerBlockCap(t *testing.T) {
	var st activityStore
	seq := uint64(0)
	batch := func(repo string, n int) []hubactivity.Event {
		out := make([]hubactivity.Event, 0, n)
		for range n {
			seq++
			out = append(out, activityEv(seq, hubactivity.EventStage, repo, int(seq)))
		}
		return out
	}

	// 250 events in one block: the hub keeps the newest 200 and discloses 50.
	st.record("hive-a", withEvents(activityDigest("o/a", 1), batch("o/a", 250)...), activityT0)
	snap := st.eventsSnapshot([]string{"hive-a"})
	if len(snap.events) != hubactivity.MaxEvents || snap.events[0].event.Seq != 51 || snap.dropped["hive-a"] != 50 {
		t.Fatalf("per-block cap: %d events from seq %d, dropped %d; want 200 from 51, 50 dropped", len(snap.events), snap.events[0].event.Seq, snap.dropped["hive-a"])
	}

	// Five more full blocks across two hives overflow the fleet ring.
	for i := range 5 {
		hive, repo := "hive-a", "o/a"
		if i%2 == 1 {
			hive, repo = "hive-b", "o/b"
		}
		st.record(hive, withEvents(activityDigest(repo, 1), batch(repo, 200)...), activityT0.Add(time.Duration(i+1)*time.Minute))
	}
	all := st.eventsSnapshot([]string{"hive-a", "hive-b"})
	if len(all.events) != activityEventRingSize {
		t.Fatalf("ring holds %d events, want %d", len(all.events), activityEventRingSize)
	}
	if last := all.events[len(all.events)-1].event.Seq; last != seq {
		t.Fatalf("ring ends at seq %d, want the newest %d", last, seq)
	}
}

// One spoke flooding the fleet-wide ring must not evict another hive's
// events; its own oldest events are evicted and counted as dropped.
func TestActivityEventRing_FloodCannotEvictOtherHive(t *testing.T) {
	var st activityStore
	st.record("hive-b", withEvents(activityDigest("o/b", 1), activityEv(1, hubactivity.EventStage, "o/b", 1)), activityT0)
	seq := uint64(1)
	for i := range 5 * activityEventRingSize / hubactivity.MaxEvents {
		batch := make([]hubactivity.Event, 0, hubactivity.MaxEvents)
		for range hubactivity.MaxEvents {
			seq++
			batch = append(batch, activityEv(seq, hubactivity.EventStage, "o/a", int(seq)))
		}
		st.record("hive-a", withEvents(activityDigest("o/a", 1), batch...), activityT0.Add(time.Duration(i+1)*time.Second))
	}
	b := st.eventsSnapshot([]string{"hive-b"})
	if len(b.events) != 1 || b.events[0].event.Seq != 1 || b.dropped["hive-b"] != 0 {
		t.Fatalf("hive-b has %d events, dropped %d; want its 1 event kept", len(b.events), b.dropped["hive-b"])
	}
	all := st.eventsSnapshot([]string{"hive-a", "hive-b"})
	if len(all.events) != activityEventRingSize {
		t.Fatalf("ring holds %d events, want %d", len(all.events), activityEventRingSize)
	}
	if a := st.eventsSnapshot([]string{"hive-a"}); a.dropped["hive-a"] == 0 {
		t.Fatalf("hive-a evictions were not counted as dropped")
	}
}

func TestActivityEventRing_OldEventExpiresWhileHiveKeepsHeartbeating(t *testing.T) {
	var st activityStore
	st.record("hive-b", withEvents(activityDigest("o/b", 1), activityEv(1, hubactivity.EventStage, "o/b", 1)), activityT0)
	for h := time.Duration(1); h <= 30; h++ {
		st.record("hive-b", activityDigest("o/b", 1), activityT0.Add(h*time.Hour))
	}
	b := st.eventsSnapshot([]string{"hive-b"})
	if len(b.events) != 0 || b.dropped["hive-b"] != 1 {
		t.Fatalf("hive-b has %d events, dropped %d; want its old event aged out and counted", len(b.events), b.dropped["hive-b"])
	}
}

func TestActivityEventRing_DropsMalformedEvents(t *testing.T) {
	var st activityStore
	st.record("hive-a", withEvents(activityDigest("o/a", 1),
		hubactivity.Event{Seq: 1, Type: "exploded", Repo: "o/a", Number: 1},
		hubactivity.Event{Seq: 2, Type: hubactivity.EventMerged, Repo: "not a repo", Number: 1},
		hubactivity.Event{Seq: 0, Type: hubactivity.EventMerged, Repo: "o/a", Number: 1},
		hubactivity.Event{Seq: 3, Type: hubactivity.EventMerged, Repo: "o/a", Number: 1, Title: strings.Repeat("x", 500)},
	), activityT0)

	snap := st.eventsSnapshot([]string{"hive-a"})
	if len(snap.events) != 1 || snap.events[0].event.Seq != 3 {
		t.Fatalf("events = %+v, want only the valid merge", snap.events)
	}
	if n := len([]rune(snap.events[0].event.Title)); n != hubactivity.MaxTitleRunes {
		t.Fatalf("title runes = %d, want bounded to %d", n, hubactivity.MaxTitleRunes)
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Stream-gap observability (#6218) ────────────────────────────────────────
//
// Dropping an event for a full subscriber is deliberate and stays: a slow
// observer must never back-pressure the hub's event path. What #6218 reported is
// that the loss was UNOBSERVABLE — the connection stayed open, the 25s heartbeat
// kept it looking healthy, and a long-lived client could not tell "nothing
// happened" from "I missed events". These tests pin the two things that now make
// it observable: a monotonic Seq on every frame, and a "gap" frame naming how
// many events went.

// Every broadcast advances the position, and one event carries the SAME number
// for every subscriber — a per-subscriber counter would make the numbers useless
// for comparing two clients or reasoning about the stream as a whole.
func TestSSEBroadcastAssignsMonotonicSeq(t *testing.T) {
	reg := newSSERegistry()
	a := reg.subscribe()
	b := reg.subscribe()

	if a.startSeq != 0 || b.startSeq != 0 {
		t.Fatalf("fresh registry should start at 0, got a=%d b=%d", a.startSeq, b.startSeq)
	}

	for i := 0; i < 3; i++ {
		reg.broadcast(sseEvent{Type: "activity", Activity: &ActivityEntry{Username: "u"}})
	}
	if got := reg.currentSeq(); got != 3 {
		t.Errorf("currentSeq() = %d, want 3", got)
	}

	var aSeqs, bSeqs []uint64
	for i := 0; i < 3; i++ {
		aSeqs = append(aSeqs, (<-a.events).Seq)
		bSeqs = append(bSeqs, (<-b.events).Seq)
	}
	for i, want := range []uint64{1, 2, 3} {
		if aSeqs[i] != want {
			t.Errorf("subscriber a event %d has seq %d, want %d", i, aSeqs[i], want)
		}
		if bSeqs[i] != aSeqs[i] {
			t.Errorf("the same event was numbered %d for one subscriber and %d for another", aSeqs[i], bSeqs[i])
		}
	}
}

// A subscriber that joins mid-stream records where it came in, so its hello frame
// can tell the client which events it was never entitled to.
func TestSSESubscribeRecordsStartPosition(t *testing.T) {
	reg := newSSERegistry()
	reg.broadcast(sseEvent{Type: "activity"})
	reg.broadcast(sseEvent{Type: "activity"})

	late := reg.subscribe()
	if late.startSeq != 2 {
		t.Fatalf("late subscriber startSeq = %d, want 2", late.startSeq)
	}
	reg.broadcast(sseEvent{Type: "activity"})

	ev := <-late.events
	if ev.Seq != late.startSeq+1 {
		t.Errorf("first event after joining has seq %d, want startSeq+1 (%d)", ev.Seq, late.startSeq+1)
	}
	if len(late.events) != 0 {
		t.Errorf("a late subscriber received %d event(s) from before it joined", len(late.events))
	}
}

// The counter is what turns a silent discard into something reportable. This is
// the paired half of TestBroadcastDropsSlowSubscriber: that one pins that the
// event is dropped, this one pins that the drop is RECORDED.
func TestSSEBroadcastCountsDroppedEvents(t *testing.T) {
	reg := newSSERegistry()
	sub := reg.subscribe()
	for i := 0; i < sseSubscriberBuffer; i++ {
		sub.events <- sseEvent{Type: "activity"}
	}
	if got := sub.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d before any overflow, want 0", got)
	}

	for i := 0; i < 3; i++ {
		reg.broadcast(sseEvent{Type: "activity", Activity: &ActivityEntry{Username: "overflow"}})
	}

	if got := sub.dropped.Load(); got != 3 {
		t.Errorf("dropped = %d after three overflowed broadcasts, want 3", got)
	}
	// The position still advances: the events happened, this subscriber just did
	// not get them. A client comparing its last seen seq against a later frame's
	// is exactly how it notices.
	if got := reg.currentSeq(); got != 3 {
		t.Errorf("currentSeq() = %d, want 3 — a dropped event still occupies a position", got)
	}
	if len(sub.events) != sseSubscriberBuffer {
		t.Errorf("channel length = %d, want %d (an overflow was enqueued anyway)", len(sub.events), sseSubscriberBuffer)
	}
}

// sseFrames splits a recorded SSE body into the decoded JSON data frames,
// ignoring `: ping` comments.
func sseFrames(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("undecodable SSE frame %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// stallingRecorder is a syncRecorder whose next Write can be parked on demand.
// TestSSEHandlerEmitsGapFrameAfterDrops needs the SSE writer goroutine held
// mid-frame while the test fills the subscriber channel; a recorder that never
// blocks lets the writer drain the channel concurrently, which turns "fill it,
// then broadcast" into a race the shuffle lane loses.
type stallingRecorder struct {
	*syncRecorder
	gateMu  sync.Mutex
	parked  chan struct{} // closed by Write once it has parked
	release chan struct{} // closed by the test to let the parked Write proceed
}

func newStallingRecorder() *stallingRecorder {
	return &stallingRecorder{syncRecorder: newSyncRecorder()}
}

// stallNextWrite arms a one-shot gate: the next Write parks until release is
// called. parked is closed the moment the writer is stuck inside Write.
func (r *stallingRecorder) stallNextWrite() (parked <-chan struct{}, release func()) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	p, rel := make(chan struct{}), make(chan struct{})
	r.parked, r.release = p, rel
	var once sync.Once
	return p, func() { once.Do(func() { close(rel) }) }
}

func (r *stallingRecorder) Write(b []byte) (int, error) {
	r.gateMu.Lock()
	p, rel := r.parked, r.release
	r.parked, r.release = nil, nil
	r.gateMu.Unlock()
	if p != nil {
		close(p)
		<-rel
	}
	return r.syncRecorder.Write(b)
}

// The end-to-end shape of the fix: a connected client whose channel overflows is
// told, over the SAME still-open connection, that it missed events — and the
// gap frame arrives BEFORE the next activity frame, so the client learns it is
// behind before it is handed anything to render.
func TestSSEHandlerEmitsGapFrameAfterDrops(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newStallingRecorder()

	done := make(chan struct{})
	go func() {
		s.handleContributeEvents(rec, req)
		close(done)
	}()
	waitFor(t, func() bool { return hub.sse.count() == 1 }, "subscriber to register")
	waitFor(t, func() bool { return strings.Contains(rec.BodyString(), `"hello"`) }, "the hello frame to be written")

	// Reach into the one registered subscriber and starve it: fill its channel so
	// the next broadcasts have nowhere to go. This is the state a slow browser or
	// a stalled monitor reaches on its own.
	hub.sse.mu.Lock()
	var sub *sseSubscriber
	for candidate := range hub.sse.subs {
		sub = candidate
	}
	hub.sse.mu.Unlock()
	if sub == nil {
		t.Fatal("no subscriber registered")
	}
	// Park the writer goroutine mid-frame first. It drains sub.events into the
	// recorder as fast as we can fill it, so without this the broadcasts below
	// land in freed slots and are delivered instead of dropped — the shuffle lane
	// caught exactly that as a timeout on the `dropped == 2` wait. With the
	// writer stuck inside Write, the channel stays full for as long as we need.
	filler := sseEvent{Type: "activity", Activity: &ActivityEntry{Username: "filler"}}
	parked, release := rec.stallNextWrite()
	sub.events <- filler
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer never picked up the filler event")
	}
	for len(sub.events) < cap(sub.events) {
		sub.events <- filler
	}

	// Two events the client will never see. broadcast is synchronous, so both
	// drops are recorded by the time addActivity returns.
	hub.addActivity("lost-one", "picked up", "contributor", "claude", "sonnet", "", "acme/repo#1")
	hub.addActivity("lost-two", "picked up", "contributor", "claude", "sonnet", "", "acme/repo#2")
	if got := sub.dropped.Load(); got != 2 {
		t.Fatalf("dropped = %d after two broadcasts into a full channel, want 2", got)
	}

	// Let the parked frame through. The writer then pops the next queued event
	// and runs flushGap before writing it, so the gap arrives on the event path.
	// (The heartbeat would report it too, but this test is not waiting 25 seconds
	// for it — TestSSEGapIsReportedOnTheHeartbeatPath covers that branch.)
	release()

	waitFor(t, func() bool { return strings.Contains(rec.BodyString(), `"gap"`) }, "a gap frame to be written")

	frames := sseFrames(t, rec.BodyString())
	var gap *sseEvent
	var gapIndex, firstActivityAfterGap = -1, -1
	for i := range frames {
		if frames[i].Type == "gap" && gap == nil {
			gap = &frames[i]
			gapIndex = i
		}
		if gap != nil && frames[i].Type == "activity" && firstActivityAfterGap < 0 && i > gapIndex {
			firstActivityAfterGap = i
		}
	}
	if gap == nil {
		t.Fatalf("no gap frame in the stream:\n%s", rec.BodyString())
	}
	if gap.Dropped != 2 {
		t.Errorf("gap frame reports %d dropped, want 2", gap.Dropped)
	}
	if gap.Seq == 0 {
		t.Error("gap frame carries no stream position, so a client cannot tell how far it is behind")
	}
	if frames[0].Type != "hello" {
		t.Errorf("first frame is %q, want hello", frames[0].Type)
	}
	if gapIndex == 0 {
		t.Error("the gap frame preceded the hello frame")
	}

	// Reported once, not on every subsequent wake-up.
	if got := sub.dropped.Load(); got != 0 {
		t.Errorf("dropped = %d after the gap was reported, want 0 (it would repeat forever)", got)
	}

	// And the connection is still open — the fix is a signal, not a disconnect.
	select {
	case <-done:
		t.Fatal("the handler exited; a reported gap must not tear down the stream")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
}

// The heartbeat branch, exercised on its own.
//
// A pending drop is marked DIRECTLY on the subscriber here rather than through
// broadcast, and that is deliberate: a real drop implies a full channel, so the
// event branch would pop one of the ~32 queued events and report the gap before
// the ticker ever fired. Driving broadcast would therefore test the event path
// again while looking like it tested this one — which is exactly what an earlier
// draft of this test did, until removing the heartbeat check left it green.
//
// What this pins is the branch's contract: with drops pending and NOTHING else
// arriving, the client still learns. That is the property the report asks for —
// a monitor that goes quiet must not read heartbeats as health forever — and it
// is the half the event path cannot provide.
//
// The heartbeat is shortened for the test rather than waited out; production
// stays at 25 seconds.
func TestSSEGapIsReportedOnAnIdleStream(t *testing.T) {
	setupContributeEnv(t)
	restore := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = restore })

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		s.handleContributeEvents(rec, req)
		close(done)
	}()
	waitFor(t, func() bool { return hub.sse.count() == 1 }, "subscriber to register")

	hub.sse.mu.Lock()
	var sub *sseSubscriber
	for candidate := range hub.sse.subs {
		sub = candidate
	}
	hub.sse.mu.Unlock()
	if sub == nil {
		t.Fatal("no subscriber registered")
	}
	// The stream is idle: the channel is empty and the writer is parked in its
	// select. Mark a drop as pending — the state broadcast leaves behind — and
	// then let nothing else happen at all.
	if len(sub.events) != 0 {
		t.Fatalf("expected an idle subscriber, found %d queued event(s)", len(sub.events))
	}
	sub.dropped.Add(1)

	// Deliberately no broadcast and no drain from here on — only the heartbeat
	// ticking against a client that is now behind.
	waitFor(t, func() bool { return strings.Contains(rec.BodyString(), `"gap"`) },
		"an idle stream to report the gap on its heartbeat")

	var gap *sseEvent
	for _, f := range sseFrames(t, rec.BodyString()) {
		if f.Type == "gap" {
			ev := f
			gap = &ev
			break
		}
	}
	if gap == nil {
		t.Fatalf("no gap frame:\n%s", rec.BodyString())
	}
	if gap.Dropped != 1 {
		t.Errorf("gap frame reports %d dropped, want 1", gap.Dropped)
	}
	// The heartbeat itself must keep going — the gap is a signal on a healthy
	// connection, not a teardown.
	if !strings.Contains(rec.BodyString(), ": ping") {
		t.Error("no heartbeat comment in the stream; the ping branch stopped pinging")
	}
	select {
	case <-done:
		t.Fatal("the handler exited; a reported gap must not tear down the stream")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
}

// A healthy stream must not gain a gap frame, and the hello frame has to report
// the position or a client cannot establish a baseline to compare against.
func TestSSEHealthyStreamCarriesSeqAndNoGap(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub

	// Two events BEFORE anyone connects, so the hello frame has a non-zero
	// position to report.
	hub.addActivity("early", "joined", "contributor", "claude", "sonnet", "", "")
	hub.addActivity("early", "left", "contributor", "claude", "sonnet", "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		s.handleContributeEvents(rec, req)
		close(done)
	}()
	waitFor(t, func() bool { return hub.sse.count() == 1 }, "subscriber to register")

	hub.addActivity("live", "picked up", "contributor", "claude", "sonnet", "", "acme/repo#7")
	waitFor(t, func() bool { return strings.Contains(rec.BodyString(), "live") }, "the live event to stream")

	frames := sseFrames(t, rec.BodyString())
	if len(frames) < 2 {
		t.Fatalf("want at least a hello and an activity frame, got %d:\n%s", len(frames), rec.BodyString())
	}
	hello, activity := frames[0], frames[1]
	if hello.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", hello.Type)
	}
	if hello.Seq != 2 {
		t.Errorf("hello reports seq %d, want 2 (the position when this client joined)", hello.Seq)
	}
	if activity.Type != "activity" || activity.Seq != hello.Seq+1 {
		t.Errorf("first activity frame is %q at seq %d, want activity at %d — a healthy stream must be contiguous",
			activity.Type, activity.Seq, hello.Seq+1)
	}
	for _, f := range frames {
		if f.Type == "gap" {
			t.Errorf("a healthy stream emitted a gap frame: %+v", f)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
}

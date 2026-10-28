package fixqueue

import (
	"testing"
	"time"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }
func newClock() *clock                   { return &clock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }
func newSched(c *clock) *Scheduler       { return New(Options{Now: c.now}) }
func red(n int, age time.Duration, c *clock) Item {
	return Item{Repo: "acme/widget", Number: n, HeadSHA: "h", OpenedAt: c.at.Add(-age)}
}

func keys(items []Item) []int {
	out := make([]int, 0, len(items))
	for _, it := range items {
		out = append(out, it.Number)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Capacity 3 with 5 red PRs → 2 in flight (spawn), 3 queued; completing one
// auto-refills the freed slot; the primary is never preempted.
func TestSpawnCapacityAndRefill(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cap := Capacity{Parallel: true, Max: 3}
	for i := 1; i <= 5; i++ {
		if !s.Enqueue("Alice", red(i, time.Duration(i)*time.Hour, c)) {
			t.Fatalf("enqueue %d not new", i)
		}
	}
	var got []Dispatch
	for d := s.Dispatch("alice", cap); d != nil; d = s.Dispatch("alice", cap) {
		if d.Mode != ModeSpawn {
			t.Fatalf("mode=%s want spawn", d.Mode)
		}
		got = append(got, *d)
	}
	if len(got) != 2 {
		t.Fatalf("dispatched %d want 2", len(got))
	}
	// Oldest first: PR 5 (5h) then PR 4.
	if got[0].Items[0].Number != 5 || got[1].Items[0].Number != 4 {
		t.Fatalf("order=%d,%d", got[0].Items[0].Number, got[1].Items[0].Number)
	}
	st := s.Status("alice", cap)
	if st.Queued != 3 || st.InFlight != 2 || st.Slots != 2 || st.Mode != "spawn" {
		t.Fatalf("status=%+v", st)
	}
	if !s.Complete("alice", "acme/widget", 5) {
		t.Fatal("complete did not free a slot")
	}
	d := s.Dispatch("alice", cap)
	if d == nil || d.Items[0].Number != 3 {
		t.Fatalf("refill=%+v", d)
	}
	if s.Dispatch("alice", cap) != nil {
		t.Fatal("over capacity")
	}
	st = s.Status("alice", cap)
	if st.Queued != 2 || st.InFlight != 2 || st.FocusUntil != nil {
		t.Fatalf("status after refill=%+v", st)
	}
}

// Unadvertised capacity: one preemption at a time, and after it completes
// the resumed primary gets MinFocusWindow before the next preemption.
func TestPreemptOnceThenFocusWindow(t *testing.T) {
	c := newClock()
	s := New(Options{Now: c.now, MinFocusWindow: 30 * time.Minute})
	cap := Capacity{}
	s.Enqueue("bob", red(1, 2*time.Hour, c))
	s.Enqueue("bob", red(2, time.Hour, c))
	d := s.Dispatch("bob", cap)
	if d == nil || d.Mode != ModePreempt || d.Items[0].Number != 1 {
		t.Fatalf("first=%+v", d)
	}
	if s.Dispatch("bob", cap) != nil {
		t.Fatal("second preemption while one is in flight")
	}
	s.Complete("bob", "acme/widget", 1)
	if s.Dispatch("bob", cap) != nil {
		t.Fatal("preempted again inside the focus window")
	}
	st := s.Status("bob", cap)
	if st.FocusUntil == nil || !st.FocusUntil.Equal(c.at.Add(30*time.Minute)) || st.Mode != "preempt" {
		t.Fatalf("status=%+v", st)
	}
	c.advance(29 * time.Minute)
	if s.Dispatch("bob", cap) != nil {
		t.Fatal("focus window cut short")
	}
	c.advance(time.Minute)
	d = s.Dispatch("bob", cap)
	if d == nil || d.Items[0].Number != 2 {
		t.Fatalf("after focus=%+v", d)
	}
}

// At a task boundary fixes are served before new work regardless of the
// preempt/focus rules, up to the full capacity.
func TestBoundaryServesFixesFirst(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cap := Capacity{}
	if s.DispatchAtBoundary("carol", cap) != nil || s.Pending("carol") {
		t.Fatal("empty queue dispatched")
	}
	s.Enqueue("carol", red(1, time.Hour, c))
	s.Enqueue("carol", red(2, time.Hour, c))
	d := s.DispatchAtBoundary("carol", cap)
	if d == nil || d.Mode != ModeBoundary || d.Items[0].Number != 1 {
		t.Fatalf("boundary=%+v", d)
	}
	if s.DispatchAtBoundary("carol", cap) != nil {
		t.Fatal("boundary exceeded capacity 1")
	}
	if !s.Pending("carol") {
		t.Fatal("PR 2 should still be pending")
	}
	s.Complete("carol", "acme/widget", 1)
	// A boundary dispatch is not a preemption: no focus window.
	if d := s.DispatchAtBoundary("carol", cap); d == nil || d.Items[0].Number != 2 {
		t.Fatalf("second boundary=%+v", d)
	}
	if s.Pending("carol") {
		t.Fatal("queue should be drained")
	}
}

// Human changes_requested outranks CI-red even when younger.
func TestHumanChangesRequestedOutranksCIRed(t *testing.T) {
	c := newClock()
	s := newSched(c)
	s.Enqueue("dan", red(1, 5*time.Hour, c))
	young := red(2, time.Minute, c)
	young.HumanChangesRequested = true
	s.Enqueue("dan", young)
	if got := keys(s.Queued("dan")); !eq(got, []int{2, 1}) {
		t.Fatalf("order=%v", got)
	}
}

// PRs red for the same cause ride one dispatch; unknown causes never coalesce.
func TestCoalesceByCause(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cause := Cause{Check: "unit", Signature: "ratchet: TestStyleRatchet"}
	for i := 1; i <= 3; i++ {
		it := red(i, time.Duration(i)*time.Hour, c)
		it.Cause = cause
		s.Enqueue("eve", it)
	}
	other := red(4, 10*time.Hour, c)
	other.Cause = Cause{Check: "lint", Signature: "gofmt"}
	s.Enqueue("eve", other)
	unknown := red(5, 20*time.Hour, c)
	s.Enqueue("eve", unknown)
	unknown2 := red(6, 19*time.Hour, c)
	s.Enqueue("eve", unknown2)

	d := s.DispatchAtBoundary("eve", Capacity{Max: 10})
	if d == nil || !eq(keys(d.Items), []int{5}) {
		t.Fatalf("unknown cause coalesced: %+v", d)
	}
	d = s.DispatchAtBoundary("eve", Capacity{Max: 10})
	if d == nil || !eq(keys(d.Items), []int{6}) {
		t.Fatalf("second unknown: %+v", d)
	}
	d = s.DispatchAtBoundary("eve", Capacity{Max: 10})
	if d == nil || !eq(keys(d.Items), []int{4}) {
		t.Fatalf("lint: %+v", d)
	}
	d = s.DispatchAtBoundary("eve", Capacity{Max: 10})
	if d == nil || !eq(keys(d.Items), []int{3, 2, 1}) {
		t.Fatalf("coalesced=%+v", d)
	}
	st := s.Status("eve", Capacity{Max: 10})
	if st.Queued != 0 || st.InFlight != 6 {
		t.Fatalf("status=%+v", st)
	}
	// Completing any member settles the whole group.
	if !s.Complete("eve", "acme/widget", 2) {
		t.Fatal("group not settled")
	}
	if st := s.Status("eve", Capacity{Max: 10}); st.InFlight != 3 {
		t.Fatalf("inflight after group complete=%d", st.InFlight)
	}
	if len(s.InFlight("eve")) != 3 {
		t.Fatalf("inflight dispatches=%d", len(s.InFlight("eve")))
	}
}

// Overflow past the grace is handed to the hive instead of the human.
func TestOverflowAdoptsAfterGrace(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cap := Capacity{}
	s.Enqueue("fay", red(1, 3*time.Hour, c))
	s.Enqueue("fay", red(2, 2*time.Hour, c))
	s.Dispatch("fay", cap) // PR 1 in flight; PR 2 waits
	c.advance(30 * time.Minute)
	s.Enqueue("fay", red(3, time.Hour, c))
	if got := s.Overflow("fay", time.Hour); got != nil {
		t.Fatalf("overflow too early: %v", keys(got))
	}
	c.advance(30 * time.Minute)
	got := s.Overflow("fay", time.Hour)
	if !eq(keys(got), []int{2}) {
		t.Fatalf("overflow=%v", keys(got))
	}
	if got := keys(s.Queued("fay")); !eq(got, []int{3}) {
		t.Fatalf("queued=%v", got)
	}
	if s.Overflow("nobody", time.Hour) != nil {
		t.Fatal("unknown user overflowed")
	}
}

// Re-observing a red PR refreshes it in place; leaving the stage removes it.
func TestEnqueueIdempotentAndRemove(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cap := Capacity{Parallel: true, Max: 2}
	it := red(1, time.Hour, c)
	if !s.Enqueue("gus", it) {
		t.Fatal("first enqueue not new")
	}
	it.HeadSHA = "h2"
	if s.Enqueue("gus", it) {
		t.Fatal("refresh counted as new")
	}
	if q := s.Queued("gus"); len(q) != 1 || q[0].HeadSHA != "h2" {
		t.Fatalf("queued=%+v", q)
	}
	s.Enqueue("gus", red(2, time.Hour, c))
	if s.Remove("gus", "acme/widget", 2) {
		t.Fatal("removing a queued item freed a slot")
	}
	d := s.Dispatch("gus", cap)
	if d == nil {
		t.Fatal("no dispatch")
	}
	it.HeadSHA = "h3"
	if s.Enqueue("gus", it) {
		t.Fatal("in-flight refresh counted as new")
	}
	if got := s.InFlight("gus"); got[0].Items[0].HeadSHA != "h3" {
		t.Fatalf("inflight not refreshed: %+v", got)
	}
	if !s.Remove("gus", "acme/widget", 1) {
		t.Fatal("removing the in-flight item did not free the slot")
	}
	if s.Remove("gus", "acme/widget", 1) || s.Remove("zed", "acme/widget", 1) {
		t.Fatal("double remove reported a slot")
	}
	if s.Complete("gus", "acme/widget", 1) || s.Complete("zed", "acme/widget", 1) {
		t.Fatal("complete of unknown item freed a slot")
	}
}

// A dispatch the relay failed goes back in the queue with its attempt
// counted, frees the preemption, and is not interrupted back in until the
// backoff elapses; a task boundary may serve it at once (hivecommons/hive#14668).
func TestRequeueFailedDispatchBacksOff(t *testing.T) {
	c := newClock()
	s := newSched(c)
	s.Enqueue("ida", red(1, time.Hour, c))
	d := s.Dispatch("ida", Capacity{Max: 1})
	if d == nil || d.Mode != ModePreempt {
		t.Fatalf("dispatch = %+v", d)
	}
	if s.Requeue("ida", "nope", time.Minute) != nil || s.Requeue("zed", d.ID, time.Minute) != nil {
		t.Fatal("unknown dispatch requeued")
	}
	got := s.Requeue("ida", d.ID, time.Minute)
	if len(got) != 1 || got[0].Attempts != 1 || !got[0].NotBefore.Equal(c.at.Add(time.Minute)) {
		t.Fatalf("requeued = %+v", got)
	}
	if st := s.Status("ida", Capacity{Max: 1}); st.Queued != 1 || st.InFlight != 0 || st.FocusUntil != nil {
		t.Fatalf("status after requeue = %+v", st)
	}
	if s.Dispatch("ida", Capacity{Max: 1}) != nil {
		t.Fatal("interrupt re-dispatched inside the backoff")
	}
	// A refreshed nudge keeps the attempt count and the backoff.
	s.Enqueue("ida", red(1, time.Hour, c))
	if q := s.Queued("ida"); len(q) != 1 || q[0].Attempts != 1 || q[0].NotBefore.IsZero() {
		t.Fatalf("refresh lost the retry state: %+v", q)
	}
	c.advance(time.Minute)
	d2 := s.Dispatch("ida", Capacity{Max: 1})
	if d2 == nil || d2.ID == d.ID || d2.Items[0].Attempts != 1 {
		t.Fatalf("retry after backoff = %+v", d2)
	}
	s.Requeue("ida", d2.ID, time.Hour)
	if b := s.DispatchAtBoundary("ida", Capacity{Max: 1}); b == nil || b.Items[0].Attempts != 2 {
		t.Fatalf("boundary should serve a backing-off fix: %+v", b)
	}
}

func TestCompleteDispatchAndPartialRemove(t *testing.T) {
	c := newClock()
	s := newSched(c)
	cause := Cause{Check: "unit", Signature: "x"}
	for i := 1; i <= 2; i++ {
		it := red(i, time.Hour, c)
		it.Cause = cause
		s.Enqueue("hal", it)
	}
	d := s.DispatchAtBoundary("hal", Capacity{})
	if len(d.Items) != 2 {
		t.Fatalf("items=%d", len(d.Items))
	}
	// One PR of the group goes green: the group stays in flight.
	if s.Remove("hal", "acme/widget", 1) {
		t.Fatal("partial remove freed the slot")
	}
	if !s.CompleteDispatch("hal", d.ID) {
		t.Fatal("complete by id failed")
	}
	if s.CompleteDispatch("hal", d.ID) || s.CompleteDispatch("zed", d.ID) {
		t.Fatal("double complete by id")
	}
	if len(s.InFlight("hal")) != 0 || s.InFlight("zed") != nil || s.Queued("zed") != nil {
		t.Fatal("stale state")
	}
}

func TestLoggerAndOptions(t *testing.T) {
	c := newClock()
	s := New(Options{Now: c.now, MinFocusWindow: -1})
	var events []string
	s.SetLogger(func(event, user string, d Dispatch, queued int) { events = append(events, event) })
	s.SetMinFocusWindow(0)
	s.SetMinFocusWindow(time.Minute)
	s.Enqueue("ida", red(1, time.Hour, c))
	s.Enqueue("ida", red(2, time.Hour, c))
	s.Dispatch("ida", Capacity{})
	s.Complete("ida", "acme/widget", 1)
	c.advance(time.Minute)
	if s.Dispatch("ida", Capacity{}) == nil {
		t.Fatal("SetMinFocusWindow not applied")
	}
	s.Enqueue("ida", red(3, time.Hour, c))
	c.advance(time.Hour)
	s.Overflow("ida", time.Minute)
	s.Enqueue("ida", red(4, time.Hour, c))
	s.Remove("ida", "acme/widget", 4)
	want := []string{"enqueue", "enqueue", "dispatch", "dequeue", "dispatch", "enqueue", "overflow", "enqueue", "remove"}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events=%v want %v", events, want)
		}
	}
	if New(Options{}).opts.MinFocusWindow != DefaultMinFocusWindow {
		t.Fatal("default focus window")
	}
}

func TestCapacityHelpers(t *testing.T) {
	cases := []struct {
		c               Capacity
		slots, boundary int
		mode            Mode
	}{
		{Capacity{}, 1, 1, ModePreempt},
		{Capacity{Max: 3}, 1, 3, ModePreempt},
		{Capacity{Parallel: true, Max: 1}, 1, 1, ModePreempt},
		{Capacity{Parallel: true, Max: 3}, 2, 3, ModeSpawn},
	}
	for _, tc := range cases {
		if tc.c.Slots() != tc.slots || tc.c.BoundarySlots() != tc.boundary || tc.c.Mode() != tc.mode {
			t.Fatalf("%+v → %d/%d/%s", tc.c, tc.c.Slots(), tc.c.BoundarySlots(), tc.c.Mode())
		}
	}
	if Key(" Acme/Widget ", 7) != "acme/widget#7" {
		t.Fatal(Key(" Acme/Widget ", 7))
	}
	if (Item{Repo: "a/b", Number: 1}).Key() != "a/b#1" {
		t.Fatal("item key")
	}
}

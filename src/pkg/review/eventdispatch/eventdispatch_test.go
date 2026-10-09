package eventdispatch

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestDispatcher(opts Options) (*Dispatcher, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	opts.Now = clk.now
	return New(opts), clk
}

func req(repo string, n int, sha string) Request {
	return Request{Repo: repo, Number: n, HeadSHA: sha, Event: "pull_request", Action: "synchronize"}
}

func TestNewDefaults(t *testing.T) {
	d := New(Options{})
	if d.minInterval != DefaultMinInterval || d.maxPending != DefaultMaxPending || d.maxRecent != DefaultMaxRecent ||
		d.ttl != DefaultDispatchedTTL || d.maxFired != DefaultMaxDispatched || d.now == nil || d.after == nil {
		t.Fatalf("defaults not applied: %+v", d)
	}
}

func TestEnqueueOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		setup func(d *Dispatcher, clk *fakeClock)
		req   Request
		want  string
	}{
		{name: "missing repo", req: req("", 1, "a"), want: OutcomeIgnored},
		{name: "missing number", req: req("o/r", 0, "a"), want: OutcomeIgnored},
		{name: "first delivery", req: req("o/r", 1, "a"), want: OutcomeQueued},
		{
			name:  "second push coalesces",
			setup: func(d *Dispatcher, _ *fakeClock) { d.Enqueue(req("o/r", 1, "a"), time.Minute) },
			req:   req("o/r", 1, "b"),
			want:  OutcomeCoalesced,
		},
		{
			name: "fired head is a duplicate",
			setup: func(d *Dispatcher, clk *fakeClock) {
				d.Enqueue(req("o/r", 1, "a"), time.Minute)
				clk.advance(time.Minute)
				d.TakeDue()
			},
			req:  req("r", 1, "A"),
			want: OutcomeDuplicate,
		},
		{
			name: "fired head expires after the TTL",
			setup: func(d *Dispatcher, clk *fakeClock) {
				d.Enqueue(req("o/r", 1, "a"), time.Minute)
				clk.advance(time.Minute)
				d.TakeDue()
				clk.advance(DefaultDispatchedTTL)
			},
			req:  req("o/r", 1, "a"),
			want: OutcomeQueued,
		},
		{
			name: "queue full drops",
			setup: func(d *Dispatcher, _ *fakeClock) {
				d.Enqueue(req("o/r", 1, "a"), time.Minute)
				d.Enqueue(req("o/r", 2, "b"), time.Minute)
			},
			req:  req("o/r", 3, "c"),
			want: OutcomeDropped,
		},
		{
			name: "full queue still coalesces a queued PR",
			setup: func(d *Dispatcher, _ *fakeClock) {
				d.Enqueue(req("o/r", 1, "a"), time.Minute)
				d.Enqueue(req("o/r", 2, "b"), time.Minute)
			},
			req:  req("o/r", 2, "c"),
			want: OutcomeCoalesced,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, clk := newTestDispatcher(Options{MaxPending: 2})
			if tc.setup != nil {
				tc.setup(d, clk)
			}
			if got := d.Enqueue(tc.req, time.Minute); got != tc.want {
				t.Fatalf("Enqueue = %q, want %q", got, tc.want)
			}
		})
	}
	var nilD *Dispatcher
	if got := nilD.Enqueue(req("o/r", 1, "a"), 0); got != OutcomeIgnored {
		t.Fatalf("nil dispatcher Enqueue = %q", got)
	}
}

func TestPushBurstFiresOnceForFinalHead(t *testing.T) {
	d, clk := newTestDispatcher(Options{})
	for _, sha := range []string{"s1", "s2", "s3"} {
		d.Enqueue(req("o/r", 7, sha), 0)
		clk.advance(30 * time.Second)
	}
	if got := d.TakeDue(); got != nil {
		t.Fatalf("fired inside the debounce window: %+v", got)
	}
	clk.advance(DefaultDebounce)
	got := d.TakeDue()
	if len(got) != 1 || got[0].HeadSHA != "s3" || got[0].Coalesced != 2 || got[0].FiredAt.IsZero() {
		t.Fatalf("burst = %+v, want one dispatch for s3 with 2 coalesced", got)
	}
	if d.Enqueue(req("o/r", 7, "s3"), 0) != OutcomeDuplicate {
		t.Fatal("re-delivery of the fired head was not a duplicate")
	}
	if d.Enqueue(req("o/r", 7, "s4"), 0) != OutcomeQueued {
		t.Fatal("a new head after firing was not queued")
	}
	st := d.Snapshot()
	want := Stats{Received: 5, Queued: 2, Coalesced: 2, Duplicates: 1, Fired: 1, Wakes: 1}
	if st.Stats != want {
		t.Fatalf("stats = %+v, want %+v", st.Stats, want)
	}
}

func TestContinuousPushesHitTheMaxWait(t *testing.T) {
	d, clk := newTestDispatcher(Options{})
	debounce := time.Minute
	d.Enqueue(req("o/r", 1, "a"), debounce)
	for i := 0; i < 10; i++ {
		clk.advance(50 * time.Second)
		if i < 4 {
			d.Enqueue(req("o/r", 1, "a"+string(rune('b'+i))), debounce)
		}
		if got := d.TakeDue(); got != nil {
			if elapsed := got[0].FiredAt.Sub(got[0].FirstSeen); elapsed > maxWaitFactor*debounce+50*time.Second {
				t.Fatalf("fired after %v, past the max wait", elapsed)
			}
			return
		}
	}
	t.Fatal("continuously pushed PR never fired")
}

func TestEnqueueKeepsHeadWhenDeliveryHasNoSHA(t *testing.T) {
	d, clk := newTestDispatcher(Options{})
	d.Enqueue(req("o/r", 1, "a"), time.Minute)
	d.Enqueue(Request{Repo: "o/r", Number: 1, Event: "pull_request", Action: "review_requested"}, time.Minute)
	clk.advance(time.Minute)
	got := d.TakeDue()
	if len(got) != 1 || got[0].HeadSHA != "a" || got[0].Action != "review_requested" {
		t.Fatalf("got %+v", got)
	}
	// No SHA: never recorded as fired, so a later SHA-less delivery queues.
	d2, clk2 := newTestDispatcher(Options{})
	d2.Enqueue(Request{Repo: "o/r", Number: 2}, time.Minute)
	clk2.advance(time.Minute)
	d2.TakeDue()
	if d2.Enqueue(Request{Repo: "o/r", Number: 2}, time.Minute) != OutcomeQueued {
		t.Fatal("SHA-less request was treated as a duplicate")
	}
}

func TestTakeDueOrderingAndMinInterval(t *testing.T) {
	d, clk := newTestDispatcher(Options{MinInterval: time.Minute, MaxRecent: 2})
	d.Enqueue(req("o/b", 2, "b"), time.Second)
	d.Enqueue(req("o/a", 1, "a"), time.Second)
	clk.advance(time.Millisecond)
	d.Enqueue(req("o/c", 3, "c"), time.Second)
	clk.advance(time.Second)
	got := d.TakeDue()
	if len(got) != 3 || got[0].Repo != "o/a" || got[1].Repo != "o/b" || got[2].Repo != "o/c" {
		t.Fatalf("order = %+v", got)
	}
	d.Enqueue(req("o/d", 4, "d"), time.Second)
	clk.advance(time.Second)
	if got := d.TakeDue(); got != nil {
		t.Fatalf("fired inside MinInterval: %+v", got)
	}
	next, ok := d.NextDue()
	if !ok || !next.Equal(d.lastWake.Add(time.Minute)) {
		t.Fatalf("NextDue = %v %v, want the MinInterval gate", next, ok)
	}
	clk.advance(time.Minute)
	if got := d.TakeDue(); len(got) != 1 || got[0].Repo != "o/d" {
		t.Fatalf("after MinInterval = %+v", got)
	}
	st := d.Snapshot()
	if len(st.Recent) != 2 || st.Recent[0].Repo != "o/d" || st.Recent[1].Repo != "o/c" {
		t.Fatalf("recent = %+v, want newest first capped at 2", st.Recent)
	}
}

func TestNextDueEmptyAndEarliest(t *testing.T) {
	d, clk := newTestDispatcher(Options{})
	if _, ok := d.NextDue(); ok {
		t.Fatal("NextDue with nothing pending")
	}
	start := clk.now()
	d.Enqueue(req("o/r", 1, "a"), 2*time.Minute)
	d.Enqueue(req("o/r", 2, "b"), time.Minute)
	if next, ok := d.NextDue(); !ok || !next.Equal(start.Add(time.Minute)) {
		t.Fatalf("NextDue = %v %v", next, ok)
	}
}

func TestFiredMemoryIsBounded(t *testing.T) {
	d, clk := newTestDispatcher(Options{MaxDispatched: 2, MinInterval: time.Nanosecond})
	for i, sha := range []string{"a", "b", "c"} {
		d.Enqueue(req("o/r", i+1, sha), time.Second)
		clk.advance(time.Second)
		d.TakeDue()
	}
	if len(d.fired) != 2 {
		t.Fatalf("fired = %v, want 2 entries", d.fired)
	}
	if d.Enqueue(req("o/r", 1, "a"), time.Second) != OutcomeQueued {
		t.Fatal("oldest fired head was not evicted")
	}
	if d.Enqueue(req("o/r", 3, "c"), time.Second) != OutcomeDuplicate {
		t.Fatal("newest fired head was evicted")
	}
}

func TestTrigger(t *testing.T) {
	d, clk := newTestDispatcher(Options{})
	var nilD *Dispatcher
	if nilD.Trigger("o/r", 1, "a") != "" {
		t.Fatal("nil dispatcher trigger")
	}
	d.Enqueue(req("Org/Repo", 1, "abc"), time.Minute)
	tests := []struct {
		repo string
		n    int
		sha  string
		want string
	}{
		{"repo", 1, "abc", TriggerEventPending},
		{"org/repo", 2, "abc", ""},
	}
	for _, tc := range tests {
		if got := d.Trigger(tc.repo, tc.n, tc.sha); got != tc.want {
			t.Fatalf("Trigger(%s,%d,%s) = %q, want %q", tc.repo, tc.n, tc.sha, got, tc.want)
		}
	}
	clk.advance(time.Minute)
	d.TakeDue()
	fired := []struct {
		repo string
		sha  string
		want string
	}{
		{"repo", "ABC", TriggerEvent},
		{"org/repo", "other", ""},
		{"org/repo", "", ""},
	}
	for _, tc := range fired {
		if got := d.Trigger(tc.repo, 1, tc.sha); got != tc.want {
			t.Fatalf("Trigger(%s,%s) = %q, want %q", tc.repo, tc.sha, got, tc.want)
		}
	}
}

func TestSnapshot(t *testing.T) {
	var nilD *Dispatcher
	if st := nilD.Snapshot(); st.Pending == nil || st.Recent == nil {
		t.Fatal("nil snapshot must carry empty slices")
	}
	d, _ := newTestDispatcher(Options{MaxPending: 9})
	d.Enqueue(req("o/b", 2, "b"), 2*time.Minute)
	d.Enqueue(req("o/a", 1, "a"), time.Minute)
	d.Enqueue(req("o/c", 3, "c"), time.Minute)
	st := d.Snapshot()
	if st.MaxPending != 9 || len(st.Pending) != 3 || st.Pending[0].Repo != "o/a" || st.Pending[1].Repo != "o/c" || st.Pending[2].Repo != "o/b" {
		t.Fatalf("snapshot = %+v", st)
	}
}

func TestRunWakesWithDueBatch(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	waits := make(chan time.Duration, 64)
	fire := make(chan time.Time)
	d := New(Options{
		Now: clk.now,
		After: func(dur time.Duration) <-chan time.Time {
			waits <- dur
			return fire
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	woke := make(chan []Entry, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, func(b []Entry) { woke <- b })
	}()
	d.Enqueue(req("o/r", 1, "a"), time.Minute)
	if got := <-waits; got != time.Minute {
		t.Fatalf("timer = %v, want the debounce", got)
	}
	clk.advance(time.Minute)
	fire <- clk.now()
	batch := <-woke
	if len(batch) != 1 || batch[0].Number != 1 {
		t.Fatalf("batch = %+v", batch)
	}
	// A timer that fires inside MinInterval wakes nobody.
	d.Enqueue(req("o/r", 2, "b"), time.Minute)
	fire <- clk.now()
	cancel()
	<-done
	select {
	case b := <-woke:
		t.Fatalf("unexpected wake %+v", b)
	default:
	}
	if st := d.Snapshot(); len(st.Pending) != 1 || st.Stats.Wakes != 1 {
		t.Fatalf("snapshot after early fire = %+v", st)
	}
}

func TestRunNilGuards(t *testing.T) {
	var nilD *Dispatcher
	nilD.Run(context.Background(), func([]Entry) {})
	New(Options{}).Run(context.Background(), nil)
}

func TestRequestFromWebhook(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		action string
		repo   string
		number int
		state  string
		draft  bool
		ok     bool
	}{
		{"synchronize", "pull_request", "synchronize", "o/r", 1, "open", false, true},
		{"opened", "pull_request", "opened", "o/r", 1, "open", false, true},
		{"reopened", "pull_request", "reopened", "o/r", 1, "", false, true},
		{"ready for review", "pull_request", "ready_for_review", "o/r", 1, "open", false, true},
		{"review requested", "pull_request", "review_requested", "o/r", 1, "open", false, true},
		{"closed action", "pull_request", "closed", "o/r", 1, "closed", false, false},
		{"labeled", "pull_request", "labeled", "o/r", 1, "open", false, false},
		{"draft", "pull_request", "synchronize", "o/r", 1, "open", true, false},
		{"closed state", "pull_request", "synchronize", "o/r", 1, "closed", false, false},
		{"other event", "push", "synchronize", "o/r", 1, "open", false, false},
		{"no repo", "pull_request", "opened", " ", 1, "open", false, false},
		{"no number", "pull_request", "opened", "o/r", 0, "open", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RequestFromWebhook(tc.event, tc.action, tc.repo, tc.number, "sha", tc.state, tc.draft)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && (got.Repo != tc.repo || got.Number != tc.number || got.HeadSHA != "sha" || got.Action != tc.action || got.Event != tc.event) {
				t.Fatalf("request = %+v", got)
			}
		})
	}
}

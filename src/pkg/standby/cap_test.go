package standby

import (
	"testing"
	"time"
)

func TestDefaultCapDispatchesNothing(t *testing.T) {
	// daily_cap_per_contributor defaults to 0, and 0 means nothing is
	// dispatched. This is what makes the configuration safe to adopt before
	// the dispatch path exists.
	tm := tiers(t)
	c := approved(T1)
	for _, cap := range []int{0, -1, -50} {
		lane := LanePolicy{Floor: T1, DailyCap: cap}
		if got := CapRemaining(c, lane, now); got != 0 {
			t.Errorf("DailyCap %d: CapRemaining = %d, want 0", cap, got)
		}
		ok, reason := Qualifies(c, lane, noItem, tm, now)
		if ok {
			t.Errorf("DailyCap %d: a candidate qualified", cap)
		}
		if reason != ReasonCapExhausted {
			t.Errorf("DailyCap %d: reason = %q, want %q", cap, reason, ReasonCapExhausted)
		}
	}
}

func TestCapDecrementsAtDispatchAndResetsByExpiry(t *testing.T) {
	tm := tiers(t)
	lane := LanePolicy{Floor: T1, DailyCap: 2}
	c := approved(T1)

	if got := CapRemaining(c, lane, now); got != 2 {
		t.Fatalf("fresh candidate: CapRemaining = %d, want 2", got)
	}
	if ok, reason := Qualifies(c, lane, noItem, tm, now); !ok {
		t.Fatalf("fresh candidate did not qualify: %q", reason)
	}

	// Dispatch once: the cap decrements at dispatch, not at completion, so an
	// abandoned donated task still costs a slot.
	c.Dispatches = RecordDispatch(c.Dispatches, now)
	if got := CapRemaining(c, lane, now); got != 1 {
		t.Fatalf("after one dispatch: CapRemaining = %d, want 1", got)
	}
	if ok, _ := Qualifies(c, lane, noItem, tm, now); !ok {
		t.Error("a candidate with one slot left did not qualify")
	}

	// Dispatch again, an hour later: the cap is spent.
	later := now.Add(time.Hour)
	c.Dispatches = RecordDispatch(c.Dispatches, later)
	if got := CapRemaining(c, lane, later); got != 0 {
		t.Fatalf("after two dispatches: CapRemaining = %d, want 0", got)
	}
	ok, reason := Qualifies(c, lane, noItem, tm, later)
	if ok {
		t.Error("a candidate with a spent cap qualified")
	}
	if reason != ReasonCapExhausted {
		t.Errorf("reason = %q, want %q", reason, ReasonCapExhausted)
	}

	// The window is trailing, so the first dispatch expires 24h after it was
	// made and one slot comes back — with no reset event ever firing.
	justBefore := now.Add(DispatchWindow - time.Second)
	if got := CapRemaining(c, lane, justBefore); got != 0 {
		t.Errorf("one second before the first dispatch expires: CapRemaining = %d, want 0", got)
	}
	atExpiry := now.Add(DispatchWindow)
	if got := CapRemaining(c, lane, atExpiry); got != 1 {
		t.Errorf("exactly %v after the first dispatch: CapRemaining = %d, want 1", DispatchWindow, got)
	}
	if ok, _ := Qualifies(c, lane, noItem, tm, atExpiry); !ok {
		t.Error("the candidate did not qualify again once a slot expired")
	}

	// Both expire, and the candidate is back to a full cap.
	bothGone := later.Add(DispatchWindow)
	if got := CapRemaining(c, lane, bothGone); got != 2 {
		t.Errorf("after both dispatches expired: CapRemaining = %d, want 2", got)
	}
}

func TestDispatchesInWindowIsASlidingWindowNotACalendarBucket(t *testing.T) {
	// A burst must not be able to straddle a fixed boundary. These four
	// dispatches sit either side of midnight; a calendar bucket would count
	// two, the trailing window counts all four.
	midnight := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	burst := []time.Time{
		midnight.Add(-2 * time.Hour),
		midnight.Add(-time.Hour),
		midnight.Add(time.Hour),
		midnight.Add(2 * time.Hour),
	}
	at := midnight.Add(3 * time.Hour)
	if got := DispatchesInWindow(burst, at); got != 4 {
		t.Errorf("DispatchesInWindow across midnight = %d, want 4", got)
	}
}

func TestDispatchesInWindowEdges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dispatches []time.Time
		want       int
	}{
		{"none", nil, 0},
		{"zero timestamps are not dispatches", []time.Time{{}, {}}, 0},
		{"just inside the window", []time.Time{now.Add(-DispatchWindow + time.Nanosecond)}, 1},
		{"exactly at the window edge has expired", []time.Time{now.Add(-DispatchWindow)}, 0},
		{"long expired", []time.Time{now.Add(-72 * time.Hour)}, 0},
		{"at now", []time.Time{now}, 1},
		// Clock skew must fail closed: a future timestamp still costs a slot.
		{"in the future", []time.Time{now.Add(time.Hour)}, 1},
		{"a mix", []time.Time{now.Add(-time.Hour), now.Add(-48 * time.Hour), {}, now.Add(-time.Minute)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DispatchesInWindow(tc.dispatches, now); got != tc.want {
				t.Errorf("DispatchesInWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCapRemainingNeverGoesNegative(t *testing.T) {
	c := Candidate{Dispatches: []time.Time{now, now, now, now}}
	if got := CapRemaining(c, LanePolicy{Floor: T1, DailyCap: 2}, now); got != 0 {
		t.Errorf("CapRemaining = %d, want 0", got)
	}
}

func TestRecordDispatchPrunesAndDoesNotMutate(t *testing.T) {
	original := []time.Time{now.Add(-48 * time.Hour), now.Add(-time.Hour)}
	before := append([]time.Time(nil), original...)

	got := RecordDispatch(original, now)

	if len(original) != len(before) {
		t.Fatalf("RecordDispatch mutated its argument: len %d, was %d", len(original), len(before))
	}
	for i := range before {
		if !original[i].Equal(before[i]) {
			t.Fatalf("RecordDispatch mutated its argument at %d", i)
		}
	}
	// The 48h-old entry is dropped; the recent one and the new one remain.
	if len(got) != 2 {
		t.Fatalf("RecordDispatch returned %d entries, want 2", len(got))
	}
	if !got[0].Equal(now.Add(-time.Hour)) || !got[1].Equal(now) {
		t.Errorf("RecordDispatch = %v, want [%v %v]", got, now.Add(-time.Hour), now)
	}
	if got := RecordDispatch(nil, now); len(got) != 1 || !got[0].Equal(now) {
		t.Errorf("RecordDispatch(nil) = %v, want [%v]", got, now)
	}
}

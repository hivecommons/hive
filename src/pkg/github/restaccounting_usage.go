package github

import (
	"sort"
	"time"
)

// RESTCallerTotal is one caller's share of the REST accounting window: every
// request it made in the window, summed across its endpoints and methods.
type RESTCallerTotal struct {
	Caller      string `json:"caller"`
	Requests    int    `json:"requests"`
	Charged     int    `json:"charged"`
	NotModified int    `json:"not_modified"`
	RateLimited int    `json:"rate_limited"`
}

// RESTUsage is one reading of the REST accounting window.
//
// RESTTopConsumers answers "which calls cost the most" with a cut list, so a
// caller that spreads its spend over many endpoints can be missing from it, or
// look small in it. ByCaller is the whole window rolled up per caller, and
// Totals the whole window, so "who spends the budget" does not depend on where
// the list was cut. All three come from the same events under one lock, so a
// caller's rows in TopConsumers never add up to more than its ByCaller total.
type RESTUsage struct {
	// TopConsumers is the first top rows of the window, most charged first,
	// exactly as RESTTopConsumers(top) returns them.
	TopConsumers []RESTConsumer
	// Consumers is how many caller + method + endpoint rows the window holds,
	// so a reader can tell whether TopConsumers was cut.
	Consumers int
	// ByCaller has one row per caller, most charged first.
	ByCaller []RESTCallerTotal
	// Totals sums every event in the window. Its Caller is empty.
	Totals RESTCallerTotal
}

// RESTAccountingWindow is how far back the REST accounting looks: the counts
// in RESTTopConsumers and RESTUsageSnapshot cover this long, ending now.
func RESTAccountingWindow() time.Duration { return restAccountingWindow }

// RESTCallerTotals rolls the REST accounting window up per caller, most
// charged first. Unlike RESTTopConsumers it is never cut.
func RESTCallerTotals() []RESTCallerTotal {
	return sharedRESTAccounting().usage(1).ByCaller
}

// RESTUsageSnapshot reads the REST accounting window once: the top rows (as
// RESTTopConsumers(top)), the per-caller totals and the window totals. It
// makes no GitHub call; the counts are what the hive's own transports recorded.
func RESTUsageSnapshot(top int) RESTUsage {
	return sharedRESTAccounting().usage(top)
}

func (s *restAccountingStore) usage(top int) RESTUsage {
	if top <= 0 {
		top = 10
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	consumers := s.consumersLocked()
	out := RESTUsage{Consumers: len(consumers)}
	byCaller := make(map[string]*RESTCallerTotal)
	for _, c := range consumers {
		t := byCaller[c.Caller]
		if t == nil {
			t = &RESTCallerTotal{Caller: c.Caller}
			byCaller[c.Caller] = t
		}
		t.Requests += c.Requests
		t.Charged += c.Charged
		t.NotModified += c.NotModified
		t.RateLimited += c.RateLimited
		out.Totals.Requests += c.Requests
		out.Totals.Charged += c.Charged
		out.Totals.NotModified += c.NotModified
		out.Totals.RateLimited += c.RateLimited
	}
	out.ByCaller = make([]RESTCallerTotal, 0, len(byCaller))
	for _, t := range byCaller {
		out.ByCaller = append(out.ByCaller, *t)
	}
	sort.Slice(out.ByCaller, func(i, j int) bool {
		a, b := out.ByCaller[i], out.ByCaller[j]
		if a.Charged != b.Charged {
			return a.Charged > b.Charged
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Caller < b.Caller
	})
	if len(consumers) > top {
		consumers = consumers[:top]
	}
	out.TopConsumers = consumers
	return out
}

// ResetRESTAccountingForTest swaps in an empty REST accounting store on the
// given clock (time.Now when nil) and returns the restore func, so a test in
// another package can record known requests and read them back.
func ResetRESTAccountingForTest(now func() time.Time) func() {
	if now == nil {
		now = time.Now
	}
	old := sharedRESTAccountingPtr.Swap(&restAccountingStore{now: now})
	return func() { sharedRESTAccountingPtr.Store(old) }
}

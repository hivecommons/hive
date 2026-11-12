package github

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// A caller that spreads its spend over many endpoints is small in the cut
// top-consumers list and first in the per-caller roll-up, and the roll-up
// adds up to exactly the requests recorded.
func TestRESTUsageRollsTheWindowUpPerCaller(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	none := http.Header{}
	revalidated := http.Header{}
	revalidated.Set(etagCacheHeader, "revalidated")
	exhausted := http.Header{}
	exhausted.Set("X-RateLimit-Remaining", "0")

	// hive:wide_sweep: 12 endpoints, one charged call each.
	for i := range 12 {
		RecordRESTRequest("hive:wide_sweep", http.MethodGet, fmt.Sprintf("/repos/acme/widgets/check%02d", i), http.StatusOK, none)
	}
	// hive:narrow_sweep: one endpoint, 5 charged and 3 free 304s.
	for range 5 {
		RecordRESTRequest("hive:narrow_sweep", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, none)
	}
	for range 3 {
		RecordRESTRequest("hive:narrow_sweep", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, revalidated)
	}
	// agent:builder: 2 charged, one of them refused for an exhausted limit.
	RecordRESTRequest("agent:builder", http.MethodGet, "/repos/acme/widgets/pulls/7", http.StatusOK, none)
	RecordRESTRequest("agent:builder", http.MethodPost, "/repos/acme/widgets/issues/7/comments", http.StatusForbidden, exhausted)
	// GET /rate_limit is never charged.
	RecordRESTRequest("hive", http.MethodGet, "/rate_limit", http.StatusOK, none)

	usage := RESTUsageSnapshot(3)

	wantCallers := []RESTCallerTotal{
		{Caller: "hive:wide_sweep", Requests: 12, Charged: 12},
		{Caller: "hive:narrow_sweep", Requests: 8, Charged: 5, NotModified: 3},
		{Caller: "agent:builder", Requests: 2, Charged: 2, RateLimited: 1},
		{Caller: "hive", Requests: 1},
	}
	if !reflect.DeepEqual(usage.ByCaller, wantCallers) {
		t.Fatalf("by caller = %+v\nwant        %+v", usage.ByCaller, wantCallers)
	}
	if !reflect.DeepEqual(RESTCallerTotals(), wantCallers) {
		t.Fatalf("RESTCallerTotals() = %+v, want the snapshot's ByCaller", RESTCallerTotals())
	}
	if want := (RESTCallerTotal{Requests: 23, Charged: 19, NotModified: 3, RateLimited: 1}); usage.Totals != want {
		t.Fatalf("totals = %+v, want %+v (23 requests recorded, 19 of them charged)", usage.Totals, want)
	}
	var sum RESTCallerTotal
	for _, c := range usage.ByCaller {
		sum.Requests += c.Requests
		sum.Charged += c.Charged
		sum.NotModified += c.NotModified
		sum.RateLimited += c.RateLimited
	}
	if sum != usage.Totals {
		t.Fatalf("by-caller rows add up to %+v, totals say %+v", sum, usage.Totals)
	}

	// 12 + 1 + 2 + 1 caller + method + endpoint rows; the cut list keeps 3
	// and is the list RESTTopConsumers returns for the same cut.
	if usage.Consumers != 16 {
		t.Fatalf("consumers = %d, want 16 rows in the window", usage.Consumers)
	}
	if !reflect.DeepEqual(usage.TopConsumers, RESTTopConsumers(3)) {
		t.Fatalf("top consumers = %+v, want RESTTopConsumers(3) = %+v", usage.TopConsumers, RESTTopConsumers(3))
	}
	if len(usage.TopConsumers) != 3 || usage.TopConsumers[0].Caller != "hive:narrow_sweep" || usage.TopConsumers[0].Charged != 5 {
		t.Fatalf("top consumers = %+v, want 3 rows led by hive:narrow_sweep with 5 charged", usage.TopConsumers)
	}
	// The wide sweep spent the most, yet the cut list shows only 2 of its 12
	// calls: the roll-up is what names it.
	wideInTop := 0
	for _, c := range usage.TopConsumers {
		if c.Caller == "hive:wide_sweep" {
			wideInTop += c.Charged
		}
	}
	if wideInTop != 2 {
		t.Fatalf("hive:wide_sweep charged in the cut list = %d, want 2 of its 12", wideInTop)
	}
}

// Rows that tie on charged, requests and endpoint come back in one order, so
// a list cut at a limit holds the same rows on every read.
func TestRESTTopConsumersOrderIsStableAcrossTies(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	for _, caller := range []string{"hive:d", "hive:b", "hive:a", "hive:c"} {
		RecordRESTRequest(caller, http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, http.Header{})
	}
	for range 20 {
		top := RESTTopConsumers(2)
		if len(top) != 2 || top[0].Caller != "hive:a" || top[1].Caller != "hive:b" {
			t.Fatalf("top 2 of four tied rows = %+v, want hive:a then hive:b every time", top)
		}
	}
}

// The roll-up covers the accounting window and nothing older.
func TestRESTUsageDropsRequestsOlderThanTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	advance := resetRESTAccountingForTest(t, now)
	RecordRESTRequest("hive:old_sweep", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, http.Header{})
	advance(now.Add(RESTAccountingWindow() - time.Minute))
	RecordRESTRequest("hive:new_sweep", http.MethodGet, "/repos/acme/widgets/pulls", http.StatusOK, http.Header{})
	if got := RESTUsageSnapshot(10); len(got.ByCaller) != 2 || got.Totals.Requests != 2 {
		t.Fatalf("inside the window: %+v, want both callers", got)
	}
	advance(now.Add(RESTAccountingWindow() + time.Minute))
	got := RESTUsageSnapshot(10)
	if len(got.ByCaller) != 1 || got.ByCaller[0].Caller != "hive:new_sweep" || got.Totals.Requests != 1 || got.Consumers != 1 {
		t.Fatalf("after the old request aged out: %+v, want hive:new_sweep alone", got)
	}
	if RESTAccountingWindow() != time.Hour {
		t.Fatalf("accounting window = %s, want one hour", RESTAccountingWindow())
	}
}

// ResetRESTAccountingForTest gives another package an empty store and puts
// the previous one back.
func TestResetRESTAccountingForTestSwapsAndRestores(t *testing.T) {
	resetRESTAccountingForTest(t, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	RecordRESTRequest("hive:before", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, http.Header{})
	restore := ResetRESTAccountingForTest(nil)
	if got := RESTCallerTotals(); len(got) != 0 {
		t.Fatalf("fresh store holds %+v", got)
	}
	RecordRESTRequest("hive:during", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, http.Header{})
	restore()
	got := RESTCallerTotals()
	if len(got) != 1 || got[0].Caller != "hive:before" {
		t.Fatalf("after restore: %+v, want the store that held hive:before", got)
	}
}

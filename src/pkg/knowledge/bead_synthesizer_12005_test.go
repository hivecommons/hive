package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPREnricher_12005_NotModifiedReusesCachedPR(t *testing.T) {
	var gets, conditional int
	client, closeSrv := mockGHClient(t, func(w http.ResponseWriter, r *http.Request) {
		gets++
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": "Fix the thing"})
	})
	defer closeSrv()

	e := NewPREnricher(client, "o", []string{"r"}, covLogger())
	pr, status := e.fetchPRWithStatus(context.Background(), "o", "r", 7)
	if status != prFetchFound || pr.GetTitle() != "Fix the thing" {
		t.Fatalf("first fetch: status=%v pr=%v", status, pr)
	}

	e.BeginCycle()
	e.ClearCache()
	pr, status = e.fetchPRWithStatus(context.Background(), "o", "r", 7)
	if status != prFetchFound || pr.GetTitle() != "Fix the thing" {
		t.Fatalf("304 fetch: status=%v pr=%v", status, pr)
	}
	if gets != 2 || conditional != 1 {
		t.Fatalf("gets=%d conditional=%d, want 2 and 1", gets, conditional)
	}
}

func TestPREnricher_12005_CapStopsFetching(t *testing.T) {
	var gets int
	client, closeSrv := mockGHClient(t, func(w http.ResponseWriter, r *http.Request) {
		gets++
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1})
	})
	defer closeSrv()

	e := NewPREnricher(client, "o", []string{"r"}, covLogger())
	e.maxAPICalls = 2
	var last prFetchStatus
	for n := 1; n <= 5; n++ {
		_, last = e.fetchPRWithStatus(context.Background(), "o", "r", n)
	}
	if gets != 2 || last != prFetchBudgetExhausted {
		t.Fatalf("gets=%d last=%v, want 2 and budget exhausted", gets, last)
	}
}

func TestBeadSynthMaxAPICalls_12005(t *testing.T) {
	t.Setenv(prEnricherMaxAPICallsEnv, "")
	if got := beadSynthMaxAPICalls(0); got != 40 {
		t.Fatalf("default = %d, want 40", got)
	}
	if got := beadSynthMaxAPICalls(15); got != 15 {
		t.Fatalf("configured = %d, want 15", got)
	}
	t.Setenv(prEnricherMaxAPICallsEnv, "7")
	if got := beadSynthMaxAPICalls(15); got != 7 {
		t.Fatalf("env override = %d, want 7", got)
	}
}

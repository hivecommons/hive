package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type contributeStatusCounts struct {
	ActionableItems int `json:"actionable_items"`
	CandidateItems  int `json:"candidate_items"`
}

type contributeQueueCounts struct {
	Queue      []ReadyQueueItem `json:"queue"`
	QueueTotal int              `json:"queue_total"`
	HeldTotal  int              `json:"held_total"`
}

func decodeContributeResponse[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("request returned %d: %s", rec.Code, rec.Body.String())
	}
	var got T
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

// TestContributeStatusAndTriageRetainDisabledCandidates reproduces #6449's live
// Project Bluefin shape: the scanner found work, but its repository was disabled
// for contributor admission. The raw population must remain observable without
// being advertised as assignable, and it must not disappear from the triage view.
func TestContributeStatusAndTriageRetainDisabledCandidates(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	deps.Config.Hub.DisabledRepos = []string{"projectbluefin/common"}
	s.RegisterAPI(deps)
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", 3)
	s.statusMu.Unlock()

	status := decodeContributeResponse[contributeStatusCounts](t, doGet(s, "/api/contribute/status"))
	if status.ActionableItems != 0 || status.CandidateItems != 3 {
		t.Fatalf("status counts = actionable:%d candidate:%d, want 0 and 3", status.ActionableItems, status.CandidateItems)
	}

	queue := decodeContributeResponse[contributeQueueCounts](t, doGet(s, "/api/contribute/queue"))
	if queue.QueueTotal != 0 || queue.HeldTotal != 0 || len(queue.Queue) != 0 {
		t.Fatalf("disabled repository leaked into queue: %+v", queue)
	}

	snap := s.buildTriageSnapshot(t.Context())
	if got := triageCount(snap, triageTriaging); got != 3 {
		t.Fatalf("triaging count = %d, want 3 raw candidates", got)
	}
	if got := triageCount(snap, triageReady); got != 0 {
		t.Fatalf("ready count = %d, want 0", got)
	}
}

// TestContributeStatusReportsUncappedOfferableTotal proves the status/queue
// contract is about the whole admitted population, even when the row payload is
// capped to protect the browser DOM.
func TestContributeStatusReportsUncappedOfferableTotal(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	want := readyQueueDefaultLimit + 17
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", want)
	s.statusMu.Unlock()

	status := decodeContributeResponse[contributeStatusCounts](t, doGet(s, "/api/contribute/status"))
	if status.ActionableItems != want || status.CandidateItems != want {
		t.Fatalf("status counts = actionable:%d candidate:%d, want %d and %d", status.ActionableItems, status.CandidateItems, want, want)
	}

	queue := decodeContributeResponse[contributeQueueCounts](t, doGet(s, "/api/contribute/queue"))
	if queue.QueueTotal != want {
		t.Fatalf("queue_total = %d, want uncapped %d", queue.QueueTotal, want)
	}
	if len(queue.Queue) != readyQueueDefaultLimit {
		t.Fatalf("rendered queue rows = %d, want cap %d", len(queue.Queue), readyQueueDefaultLimit)
	}
}

// TestContributeStatusExcludesHeldRowsFromActionableCount guards the less-obvious
// queue shape: held rows remain in the rendered payload so an operator can resume
// them, but neither status nor queue_total may claim that they are offerable.
func TestContributeStatusExcludesHeldRowsFromActionableCount(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#2"}
	s.RegisterAPI(deps)
	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3)
	s.statusMu.Unlock()

	status := decodeContributeResponse[contributeStatusCounts](t, doGet(s, "/api/contribute/status"))
	if status.ActionableItems != 2 || status.CandidateItems != 3 {
		t.Fatalf("status counts = actionable:%d candidate:%d, want 2 and 3", status.ActionableItems, status.CandidateItems)
	}

	queue := decodeContributeResponse[contributeQueueCounts](t, doGet(s, "/api/contribute/queue"))
	if queue.QueueTotal != 2 || queue.HeldTotal != 1 || len(queue.Queue) != 3 {
		t.Fatalf("held queue counts = total:%d held:%d rows:%d, want 2, 1, 3", queue.QueueTotal, queue.HeldTotal, len(queue.Queue))
	}

	snap := s.buildTriageSnapshot(t.Context())
	if got := triageCount(snap, triageReady); got != 2 {
		t.Fatalf("ready count = %d, want 2", got)
	}
	if got := triageCount(snap, triageTriaging); got != 1 {
		t.Fatalf("triaging count = %d, want held row retained outside Ready", got)
	}
}

func triageCount(snap triageSnapshot, level string) int {
	for _, group := range snap.Groups {
		if group.Level == level {
			return group.Count
		}
	}
	return 0
}

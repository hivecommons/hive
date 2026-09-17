package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The decision buffer's whole value is that it answers a question an operator
// could not otherwise ask, so the tests below are about the answer's shape and
// its bounds — not about the recording call sites, which are exercised by the
// contribute_ws tests that drive the message loop.

func TestContributorDecisionsRecordsAndReturnsNewestFirst(t *testing.T) {
	var log contributorDecisionLog
	log.record("alice", decisionTaskAbandoned, "task-1", "")
	log.record("alice", decisionStaleFailureRejected, "task-2", "client_gen=7")

	got := log.forUser("alice", 0)
	if len(got) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(got))
	}
	// Newest first: an operator opening this is asking "what just happened".
	if got[0].Kind != decisionStaleFailureRejected {
		t.Errorf("want newest decision first, got %q", got[0].Kind)
	}
	if got[0].Task != "task-2" || got[0].Detail != "client_gen=7" {
		t.Errorf("task/detail not preserved: %+v", got[0])
	}
	if got[0].Username != "alice" {
		t.Errorf("want original-case username, got %q", got[0].Username)
	}
	if got[0].At.IsZero() {
		t.Error("want a timestamp on every decision")
	}
}

func TestContributorDecisionsUsernameMatchIsCaseInsensitive(t *testing.T) {
	// GitHub logins are case-preserving but case-insensitive, and an operator
	// pastes them off the activity rail. Matches readTaskRunsForUser.
	var log contributorDecisionLog
	log.record("Alice", decisionTaskAbandoned, "task-1", "")

	if got := log.forUser("alice", 0); len(got) != 1 {
		t.Fatalf("lowercase lookup found %d, want 1", len(got))
	}
	if got := log.forUser("ALICE", 0); len(got) != 1 {
		t.Fatalf("uppercase lookup found %d, want 1", len(got))
	}
	// The stored spelling is the one the contributor actually uses.
	if got := log.forUser("alice", 0); got[0].Username != "Alice" {
		t.Errorf("want preserved case %q, got %q", "Alice", got[0].Username)
	}
}

func TestContributorDecisionsAreScopedPerUser(t *testing.T) {
	var log contributorDecisionLog
	log.record("alice", decisionTaskAbandoned, "task-1", "")
	log.record("bob", decisionNoTasksAvailable, "", "")

	if got := log.forUser("alice", 0); len(got) != 1 || got[0].Kind != decisionTaskAbandoned {
		t.Errorf("alice got %+v", got)
	}
	if got := log.forUser("bob", 0); len(got) != 1 || got[0].Kind != decisionNoTasksAvailable {
		t.Errorf("bob got %+v", got)
	}
	if got := log.forUser("carol", 0); len(got) != 0 {
		t.Errorf("unknown user should be empty, got %+v", got)
	}
}

func TestContributorDecisionsRingDropsOldest(t *testing.T) {
	var log contributorDecisionLog
	for i := 0; i < maxDecisionsPerUser+10; i++ {
		log.record("alice", decisionTaskAbandoned, fmt.Sprintf("task-%d", i), "")
	}
	got := log.forUser("alice", 0)
	if len(got) != maxDecisionsPerUser {
		t.Fatalf("want ring capped at %d, got %d", maxDecisionsPerUser, len(got))
	}
	// A full buffer forgets the oldest decision; it never refuses a new one.
	newest := fmt.Sprintf("task-%d", maxDecisionsPerUser+9)
	if got[0].Task != newest {
		t.Errorf("want newest %q retained, got %q", newest, got[0].Task)
	}
	oldest := fmt.Sprintf("task-%d", 10)
	if got[len(got)-1].Task != oldest {
		t.Errorf("want oldest retained %q, got %q", oldest, got[len(got)-1].Task)
	}
}

func TestContributorDecisionsEvictsLeastRecentlyRecordedUser(t *testing.T) {
	var log contributorDecisionLog
	for i := 0; i < maxDecisionUsers; i++ {
		log.record(fmt.Sprintf("user-%d", i), decisionNoTasksAvailable, "", "")
	}
	// Touch the first user so it is no longer the least recent.
	log.record("user-0", decisionTaskAbandoned, "task-x", "")
	// One more distinct user forces an eviction.
	log.record("newcomer", decisionNoTasksAvailable, "", "")

	if got := log.forUser("user-0", 0); len(got) != 2 {
		t.Errorf("recently-touched user should survive eviction, got %d", len(got))
	}
	if got := log.forUser("user-1", 0); len(got) != 0 {
		t.Errorf("least-recently-recorded user should be evicted, got %d", len(got))
	}
	if got := log.forUser("newcomer", 0); len(got) != 1 {
		t.Errorf("newcomer should be recorded, got %d", len(got))
	}
	log.mu.Lock()
	n := len(log.byUser)
	log.mu.Unlock()
	if n > maxDecisionUsers {
		t.Errorf("user map unbounded: %d > %d", n, maxDecisionUsers)
	}
}

func TestContributorDecisionsIgnoresEmptyUsernameAndKind(t *testing.T) {
	// A decision nobody can look up is not worth the memory, and an empty key
	// would collide unrelated contributors into one bucket.
	var log contributorDecisionLog
	log.record("", decisionTaskAbandoned, "task-1", "")
	log.record("   ", decisionTaskAbandoned, "task-1", "")
	log.record("alice", "", "task-1", "")

	log.mu.Lock()
	n := len(log.byUser)
	log.mu.Unlock()
	if n != 0 {
		t.Errorf("want nothing recorded, got %d users", n)
	}
	if got := log.forUser("", 0); len(got) != 0 {
		t.Errorf("empty lookup should be empty, got %+v", got)
	}
}

func TestContributorDecisionsForUserRespectsLimit(t *testing.T) {
	var log contributorDecisionLog
	for i := 0; i < 10; i++ {
		log.record("alice", decisionTaskAbandoned, fmt.Sprintf("task-%d", i), "")
	}
	got := log.forUser("alice", 3)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].Task != "task-9" {
		t.Errorf("limit must keep the NEWEST, got %q", got[0].Task)
	}
}

func TestContributorDecisionsNewestFirstWithIdenticalTimestamps(t *testing.T) {
	// Regression: forUser once sorted by timestamp. A burst of decisions —
	// precisely what this endpoint exists to explain — can share one coarse
	// clock reading, and a stable sort then leaves the tied entries in arrival
	// order, i.e. oldest-first, inverting the answer. Insertion order is the
	// ground truth.
	var log contributorDecisionLog
	log.record("alice", decisionTaskAbandoned, "task-1", "")
	log.record("alice", decisionTaskAbandoned, "task-2", "")
	log.record("alice", decisionTaskAbandoned, "task-3", "")

	// Force the tie the real clock only sometimes produces.
	log.mu.Lock()
	for i := range log.byUser["alice"] {
		log.byUser["alice"][i].At = log.byUser["alice"][0].At
	}
	log.mu.Unlock()

	got := log.forUser("alice", 0)
	want := []string{"task-3", "task-2", "task-1"}
	for i, w := range want {
		if got[i].Task != w {
			t.Fatalf("position %d = %q, want %q (order: %+v)", i, got[i].Task, w, got)
		}
	}
	// And the limit must keep the newest of a tied burst, not the oldest.
	if top := log.forUser("alice", 1); len(top) != 1 || top[0].Task != "task-3" {
		t.Errorf("limit over tied timestamps kept %+v, want task-3", top)
	}
}

func TestContributorDecisionsForUserReturnsCopy(t *testing.T) {
	// The caller must never hold a slice the recorder can append into.
	var log contributorDecisionLog
	log.record("alice", decisionTaskAbandoned, "task-1", "")
	got := log.forUser("alice", 0)
	got[0].Task = "mutated"

	if again := log.forUser("alice", 0); again[0].Task != "task-1" {
		t.Errorf("caller mutation leaked into the buffer: %q", again[0].Task)
	}
}

func TestRecordContributorDecisionNilServerIsNoOp(t *testing.T) {
	var s *Server
	s.recordContributorDecision("alice", decisionTaskAbandoned, "task-1", "")
}

func TestContributorDecisionsConcurrentRecordAndRead(t *testing.T) {
	// Recording happens inside the hub message loop while the endpoint reads;
	// this runs under -race in CI.
	var log contributorDecisionLog
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				log.record(fmt.Sprintf("user-%d", n%3), decisionTaskAbandoned, "t", "")
			}
		}(i)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = log.forUser(fmt.Sprintf("user-%d", n%3), 10)
			}
		}(i)
	}
	wg.Wait()
}

func TestHandleContributeDecisionsServesOneUser(t *testing.T) {
	s := &Server{}
	s.recordContributorDecision("alice", decisionTaskAbandoned, "task-1", "")
	s.recordContributorDecision("alice", decisionStaleFailureRejected, "task-2", "client_gen=7")
	s.recordContributorDecision("bob", decisionNoTasksAvailable, "", "")

	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got struct {
		Username  string                `json:"username"`
		Limit     int                   `json:"limit"`
		Returned  int                   `json:"returned"`
		Decisions []ContributorDecision `json:"decisions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "alice" || got.Returned != 2 || len(got.Decisions) != 2 {
		t.Fatalf("want alice with 2 decisions, got %+v", got)
	}
	if got.Decisions[0].Kind != decisionStaleFailureRejected {
		t.Errorf("want newest first, got %q", got.Decisions[0].Kind)
	}
	if got.Decisions[0].Detail != "client_gen=7" {
		t.Errorf("detail lost: %q", got.Decisions[0].Detail)
	}
	if got.Limit != decisionsMaxLimit {
		t.Errorf("default limit %d, want %d", got.Limit, decisionsMaxLimit)
	}
}

func TestHandleContributeDecisionsLimitIsClamped(t *testing.T) {
	s := &Server{}
	for i := 0; i < 10; i++ {
		s.recordContributorDecision("alice", decisionTaskAbandoned, fmt.Sprintf("task-%d", i), "")
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"&limit=2", 2},
		{"&limit=0", decisionsMaxLimit},
		{"&limit=-5", decisionsMaxLimit},
		{"&limit=9999", decisionsMaxLimit},
		{"&limit=abc", decisionsMaxLimit},
		{"", decisionsMaxLimit},
	} {
		rr := httptest.NewRecorder()
		s.handleContributeDecisions(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice"+tc.query, nil))
		var got struct {
			Limit int `json:"limit"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("%q decode: %v", tc.query, err)
		}
		if got.Limit != tc.want {
			t.Errorf("%q limit = %d, want %d", tc.query, got.Limit, tc.want)
		}
	}
}

func TestHandleContributeDecisionsUnknownUserIsEmptyArrayNotNull(t *testing.T) {
	// The dashboard iterates this; a JSON null would be a client-side crash.
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=nobody", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["decisions"]) != "[]" {
		t.Errorf("want [], got %s", raw["decisions"])
	}
}

func TestHandleContributeDecisionsCarriesNoPaneOutput(t *testing.T) {
	// Item 3 (pane output) wants a gate and is deliberately NOT served here.
	// This locks the response surface to the four declared fields.
	s := &Server{}
	s.recordContributorDecision("alice", decisionTaskAbandoned, "task-1", "d")
	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice", nil))

	var got struct {
		Decisions []map[string]any `json:"decisions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	allowed := map[string]bool{"at": true, "username": true, "kind": true, "task": true, "detail": true}
	for k := range got.Decisions[0] {
		if !allowed[k] {
			t.Errorf("unexpected field %q in decision payload", k)
		}
	}
}

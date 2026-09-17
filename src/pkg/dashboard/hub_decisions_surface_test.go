package dashboard

// #7343: PRs #7332 and #7339 independently built this same ring, and the
// quality agent's comparison found each had tests the other lacked. #7332
// merged; #7339 was closed as a duplicate and its branch deleted. These are the
// three properties that only #7339 held, rewritten against the implementation
// that actually shipped, plus the nil paths both PRs left uncovered.
//
// Rewritten rather than cherry-picked on purpose: #7339's ring had a different
// vocabulary (5 kinds, not 6), different bounds (50x500, not 200x200) and an
// ungated handler, so its assertions would not have compiled and its handler
// test asserted a posture this repo decided against.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The ordering regression BOTH implementations hit independently, which is the
// strongest evidence it is worth a permanent test.
//
// TS is RFC3339 at second resolution. A burst of generation fences — the exact
// event this ring exists to capture, and one that arrives in bursts because a
// wedged relay retries — lands many entries inside one second, all sharing a
// TS. Any ordering that keys on TS therefore has nothing to break the tie, and
// a stable sort leaves tied entries in ARRIVAL order: "newest first" silently
// serves oldest first, and the operator reading the top of the list sees the
// beginning of the incident instead of the end.
//
// Verified to have teeth: swapping forUser's reverse for
// sort.SliceStable(out, func(i, j int) bool { return out[i].TS > out[j].TS })
// fails this test with "position 0 = n=0, want n=49".
func TestHubDecisionRing_NewestFirstWithinASingleSecond(t *testing.T) {
	var l hubDecisionLog
	const burst = 50
	// No sleeps: the point is that these all land in one clock second.
	for i := 0; i < burst; i++ {
		l.record(HubDecision{
			Username: "alice",
			Event:    decisionStaleGenRejected,
			Detail:   "n=" + strconv.Itoa(i),
		})
	}

	got, _ := l.forUser("alice", 0)
	if len(got) != burst {
		t.Fatalf("got %d entries, want %d", len(got), burst)
	}

	// Guard the premise: if this burst somehow straddled a second boundary the
	// test would pass for the wrong reason, and a future reader would think the
	// tie case is covered when it is not.
	distinct := map[string]struct{}{}
	for _, d := range got {
		distinct[d.TS] = struct{}{}
	}
	if len(distinct) == len(got) {
		t.Fatalf("every entry got a distinct ts (%d of %d) — the burst did not tie, "+
			"so this run did not exercise the case", len(distinct), len(got))
	}

	for i, d := range got {
		want := "n=" + strconv.Itoa(burst-1-i)
		if d.Detail != want {
			t.Fatalf("position %d = %s, want %s (newest first is not holding "+
				"when timestamps tie)", i, d.Detail, want)
		}
	}
}

// Eviction reads the same arrival counter, so it inherits the same tie problem.
// TestHubDecisionRing_BoundsDistinctUsers already holds that the least recently
// active login is the one evicted; this adds the property that RE-activity moves
// a login back out of danger, so the contributor an operator is mid-way through
// investigating cannot be dropped while a long-idle one survives.
//
// Verified to have teeth: keying the eviction scan on entries[len-1].TS instead
// of .seq fails this with "user1 survived".
func TestHubDecisionRing_EvictsLeastRecentlyActiveWhenUsersTie(t *testing.T) {
	var l hubDecisionLog
	for i := 0; i < hubDecisionUsers; i++ {
		l.record(HubDecision{Username: "user" + strconv.Itoa(i), Event: decisionRefused})
	}
	// Re-touch the FIRST login, which is the one a TS-blind eviction would drop:
	// it arrived first and, within this second, its entry is indistinguishable
	// by timestamp from every other.
	l.record(HubDecision{Username: "user0", Event: decisionAbandoned, Detail: "still active"})

	// One more login overflows the map.
	l.record(HubDecision{Username: "newcomer", Event: decisionRefused})

	if got, _ := l.forUser("user0", 0); len(got) != 2 {
		t.Errorf("user0 was evicted despite being the most recently active login "+
			"(got %d entries, want 2)", len(got))
	}
	if got, _ := l.forUser("user1", 0); len(got) != 0 {
		t.Errorf("user1 survived; the least recently active login should have gone "+
			"(got %d entries, want 0)", len(got))
	}
	if got, _ := l.forUser("newcomer", 0); len(got) != 1 {
		t.Errorf("newcomer = %d entries, want 1", len(got))
	}
}

// The ring is written from every connection goroutine and read from an HTTP
// handler. Meaningful under -race, which CI runs: this is the test that would
// catch someone "optimising" the mutex away, or returning l.byUser[user]
// directly instead of copying into out.
func TestHubDecisionRing_ConcurrentRecordAndRead(t *testing.T) {
	var l hubDecisionLog
	var wg sync.WaitGroup
	const writers, readers, each = 8, 4, 200

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				l.record(HubDecision{
					Username: "user" + strconv.Itoa(w%3),
					Event:    decisionLeaseExpired,
					TaskID:   fmt.Sprintf("ct-%d-%d", w, i),
				})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				// Read the returned entries, not just their count: a handler
				// that escaped the lock would be caught here and not by len().
				got, _ := l.forUser("user0", 25)
				for _, d := range got {
					_ = d.TaskID + d.Event
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for u := 0; u < 3; u++ {
		got, _ := l.forUser("user"+strconv.Itoa(u), 0)
		total += len(got)
		if len(got) > hubDecisionsPerUser {
			t.Errorf("user%d ring = %d, over the %d bound", u, len(got), hubDecisionsPerUser)
		}
	}
	if total == 0 {
		t.Fatal("concurrent writers recorded nothing")
	}
}

// The response is an operator-facing API surface and is in api-reference.md and
// openapi.json. A field silently added, renamed or dropped is a broken client,
// and — given this endpoint is gated precisely because it carries protocol
// state — an added field is also a disclosure that nobody reviewed. Lock the
// exact key sets in both directions.
func TestHandleContributeDecisions_ResponseSurfaceIsExact(t *testing.T) {
	s := &Server{authToken: "tok", contributeHub: &ContributeWSHub{}}
	s.contributeHub.recordDecision("alice", decisionStaleGenRejected, "ct-1", "myorg/repo1", 7,
		"task_failed fenced: client_gen 0 no longer matches the assignment")

	r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice", nil)
	r.Header.Set("X-Hive-Role", config.RoleOwner)
	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	assertExactKeys(t, "response", body,
		"username", "limit", "returned", "decisions", "in_memory_only", "since")

	var decisions []map[string]json.RawMessage
	if err := json.Unmarshal(body["decisions"], &decisions); err != nil {
		t.Fatalf("decisions not a JSON array: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(decisions))
	}
	// seq is deliberately unexported: it is the ring's internal arrival counter
	// and must never reach the wire.
	assertExactKeys(t, "decision", decisions[0],
		"ts", "username", "event", "task_id", "repo", "number", "detail")

	// omitempty is load bearing: a decision with no task must not emit
	// "number":0, which an operator would read as issue #0.
	s2 := &Server{authToken: "tok", contributeHub: &ContributeWSHub{}}
	s2.contributeHub.recordDecision("bob", decisionRefused, "", "", 0, "rate limited")
	r2 := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=bob", nil)
	r2.Header.Set("X-Hive-Role", config.RoleOwner)
	rr2 := httptest.NewRecorder()
	s2.handleContributeDecisions(rr2, r2)
	for _, absent := range []string{`"task_id"`, `"repo"`, `"number"`} {
		if strings.Contains(rr2.Body.String(), absent) {
			t.Errorf("taskless decision emitted %s: %s", absent, rr2.Body.String())
		}
	}
}

func assertExactKeys(t *testing.T, what string, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	have := make([]string, 0, len(got))
	for k := range got {
		have = append(have, k)
	}
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("%s keys = [%s], want [%s]", what, strings.Join(have, ","), strings.Join(want, ","))
	}
}

// limit is caller-supplied and reaches a slice bound, so every way of getting it
// wrong has to land on the default rather than on a panic or an unbounded read.
func TestHandleContributeDecisions_LimitIsClampedNotTrusted(t *testing.T) {
	s := &Server{authToken: "tok", contributeHub: &ContributeWSHub{}}
	for i := 0; i < 150; i++ {
		s.contributeHub.recordDecision("alice", decisionAbandoned, "ct-"+strconv.Itoa(i), "o/r", i, "")
	}

	get := func(limit string) (int, int) {
		t.Helper()
		q := "username=alice"
		if limit != "" {
			q += "&limit=" + limit
		}
		r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?"+q, nil)
		r.Header.Set("X-Hive-Role", config.RoleOwner)
		rr := httptest.NewRecorder()
		s.handleContributeDecisions(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("limit=%q got %d, want 200", limit, rr.Code)
		}
		var body struct {
			Limit    int `json:"limit"`
			Returned int `json:"returned"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("limit=%q: %v", limit, err)
		}
		return body.Limit, body.Returned
	}

	for _, bad := range []string{"", "0", "-5", "abc", "999999", strconv.Itoa(hubDecisionsPerUser + 1), "1e9"} {
		if limit, returned := get(bad); limit != 100 || returned != 100 {
			t.Errorf("limit=%q => limit %d returned %d, want the 100 default", bad, limit, returned)
		}
	}
	if limit, returned := get("5"); limit != 5 || returned != 5 {
		t.Errorf("limit=5 => limit %d returned %d", limit, returned)
	}
	// The top of the list is still the newest entry after clamping — a limit
	// that silently returned the OLDEST 5 would look identical in the counts.
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice&limit=1", nil)
	r.Header.Set("X-Hive-Role", config.RoleOwner)
	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, r)
	if !strings.Contains(rr.Body.String(), `"task_id":"ct-149"`) {
		t.Errorf("limit=1 did not return the newest entry: %s", rr.Body.String())
	}
}

// Every nil guard in this file exists so that a diagnostic can never be the
// reason a routing path panics — the ring DECLARES, it never ROUTES. That
// promise is only worth making if it is tested: these are the branches #7343
// measured at 50.0% (recordTaskDecision) and 66.7% (recordDecision).
func TestHubDecisions_NilPathsNeverPanic(t *testing.T) {
	var nilLog *hubDecisionLog
	nilLog.record(HubDecision{Username: "alice", Event: decisionRefused})
	got, since := nilLog.forUser("alice", 10)
	if len(got) != 0 || !since.IsZero() {
		t.Errorf("nil ring = %d entries, since %v; want empty", len(got), since)
	}

	var nilHub *ContributeWSHub
	nilHub.recordDecision("alice", decisionRefused, "ct-1", "o/r", 1, "d")
	nilHub.recordTaskDecision("alice", decisionRefused, nil, "d")
	nilHub.recordTaskDecision("alice", decisionRefused, &WSTaskAssign{TaskID: "ct-1"}, "d")

	// A real hub with no task: the fields come out empty rather than the call
	// site having to invent them, which is the whole reason the helper exists.
	h := &ContributeWSHub{}
	h.recordTaskDecision("alice", decisionRefused, nil, "concurrency cap reached")
	entries, _ := h.decisions.forUser("alice", 0)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].TaskID != "" || entries[0].Repo != "" || entries[0].Number != 0 {
		t.Errorf("taskless decision invented task fields: %+v", entries[0])
	}
	if entries[0].Detail != "concurrency cap reached" {
		t.Errorf("detail = %q", entries[0].Detail)
	}

	h.recordTaskDecision("alice", decisionLeaseExpired,
		&WSTaskAssign{TaskID: "ct-9", Repo: "myorg/repo2", Number: 42}, "ttl elapsed")
	entries, _ = h.decisions.forUser("alice", 1)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].TaskID != "ct-9" || entries[0].Repo != "myorg/repo2" || entries[0].Number != 42 {
		t.Errorf("task fields not lifted off the assignment: %+v", entries[0])
	}
}

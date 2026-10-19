package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jamGetAs(t *testing.T, s *Server, path, role, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Hive-Role", role)
	req.Header.Set("X-Hive-User", user)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeJamCollection(t *testing.T, rec *httptest.ResponseRecorder, key string) []json.RawMessage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d body=%s", key, rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v body=%s", key, err, rec.Body.String())
	}
	if string(resp["ok"]) != "true" {
		t.Fatalf("%s response not ok: %s", key, rec.Body.String())
	}
	raw, present := resp[key]
	if !present {
		t.Fatalf("%s response missing %q key: %s", key, key, rec.Body.String())
	}
	for _, other := range []string{"jam", "threads", "suggestions", "polls"} {
		if other != key {
			if _, leaked := resp[other]; leaked {
				t.Fatalf("%s response also carries %q: %s", key, other, rec.Body.String())
			}
		}
	}
	if string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s items: %v raw=%s", key, err, raw)
	}
	return items
}

func TestCampaignJamCollectionGetsReturnOnlyTheirSlice(t *testing.T) {
	s := jamTestServer(t)
	const base = "/api/campaigns/spec-collections/jam"

	for _, key := range []string{"threads", "suggestions", "polls"} {
		if items := decodeJamCollection(t, jamGetAs(t, s, base+"/"+key, "read", "reader"), key); len(items) != 0 {
			t.Fatalf("fresh campaign %s = %d items, want 0", key, len(items))
		}
	}

	if rec := jamPostAs(t, s, base+"/threads", "read-write", "alice", map[string]any{
		"section": "Goals", "title": "Reviewers", "body": "Can we include async reviewers?",
	}, false); rec.Code != http.StatusOK {
		t.Fatalf("thread post = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := jamPostAs(t, s, base+"/suggestions", "read-write", "bob", map[string]any{
		"section": "Goals", "proposed_text": "Include async reviewers.",
	}, false); rec.Code != http.StatusOK {
		t.Fatalf("suggestion post = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := jamPostAs(t, s, base+"/polls", "read-write", "carol", map[string]any{
		"section": "Scope", "question": "Which milestone?", "options": []string{"alpha", "beta"},
	}, false); rec.Code != http.StatusOK {
		t.Fatalf("poll post = %d body=%s", rec.Code, rec.Body.String())
	}

	threads := decodeJamCollection(t, jamGetAs(t, s, base+"/threads", "read", "reader"), "threads")
	if len(threads) != 1 || !strings.Contains(string(threads[0]), `"section":"Goals"`) || !strings.Contains(string(threads[0]), `"title":"Reviewers"`) {
		t.Fatalf("threads = %s", threads)
	}
	suggestions := decodeJamCollection(t, jamGetAs(t, s, base+"/suggestions", "read", "reader"), "suggestions")
	if len(suggestions) != 1 || !strings.Contains(string(suggestions[0]), `"proposed_text":"Include async reviewers."`) {
		t.Fatalf("suggestions = %s", suggestions)
	}
	polls := decodeJamCollection(t, jamGetAs(t, s, base+"/polls", "read", "reader"), "polls")
	if len(polls) != 1 || !strings.Contains(string(polls[0]), `"question":"Which milestone?"`) {
		t.Fatalf("polls = %s", polls)
	}

	// Read-only callers may read every collection; the role gate is on writes only.
	for _, key := range []string{"threads", "suggestions", "polls"} {
		if rec := jamGetAs(t, s, base+"/"+key, "read", "reader"); rec.Code != http.StatusOK {
			t.Fatalf("read role GET %s = %d body=%s", key, rec.Code, rec.Body.String())
		}
	}

	// A different campaign id does not see this campaign's items.
	for _, key := range []string{"threads", "suggestions", "polls"} {
		if items := decodeJamCollection(t, jamGetAs(t, s, "/api/campaigns/spec-other/jam/"+key, "read", "reader"), key); len(items) != 0 {
			t.Fatalf("other campaign %s = %d items, want 0", key, len(items))
		}
	}
}

func TestCampaignJamGetsReportUnreadableStore(t *testing.T) {
	s := jamTestServer(t)
	path := s.campaignJamPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/threads", "/suggestions", "/polls"} {
		rec := jamGetAs(t, s, "/api/campaigns/spec-broken/jam"+suffix, "owner", "owner")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET jam%s with corrupt store = %d body=%s", suffix, rec.Code, rec.Body.String())
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode error body: %v body=%s", err, rec.Body.String())
		}
		if resp.OK || resp.Error == "" {
			t.Fatalf("GET jam%s error body = %s", suffix, rec.Body.String())
		}
	}
	// The corrupt file is left alone: a read never rewrites the store.
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "{not json" {
		t.Fatalf("store after failed reads = %q err=%v", raw, err)
	}
}

func TestCampaignJamPollVoteChangeMovesCount(t *testing.T) {
	s := jamTestServer(t)
	const polls = "/api/campaigns/spec-votes/jam/polls"
	create := jamPostAs(t, s, polls, "read-write", "alice", map[string]any{
		"section": "Storage", "question": "JSON or sqlite?", "options": []string{"json", "sqlite"},
	}, false)
	if create.Code != http.StatusOK {
		t.Fatalf("poll create = %d body=%s", create.Code, create.Body.String())
	}
	jam := decodeJam(t, create)
	pollID := jam.Polls[0].ID
	jsonOpt, sqliteOpt := jam.Polls[0].Options[0].ID, jam.Polls[0].Options[1].ID

	vote := func(user, option string) *httptest.ResponseRecorder {
		return jamPostAs(t, s, polls, "read-write", user, map[string]any{"action": "vote", "poll_id": pollID, "option_id": option}, false)
	}
	counts := func(rec *httptest.ResponseRecorder) (int, int, int) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("vote = %d body=%s", rec.Code, rec.Body.String())
		}
		poll := decodeJam(t, rec).Polls[0]
		var j, q int
		for _, option := range poll.Options {
			switch option.ID {
			case jsonOpt:
				j = option.Count
			case sqliteOpt:
				q = option.Count
			}
		}
		return j, q, len(poll.Votes)
	}

	if j, q, n := counts(vote("bob", jsonOpt)); j != 1 || q != 0 || n != 1 {
		t.Fatalf("after first vote json=%d sqlite=%d votes=%d", j, q, n)
	}
	if j, q, n := counts(vote("carol", jsonOpt)); j != 2 || q != 0 || n != 2 {
		t.Fatalf("after second voter json=%d sqlite=%d votes=%d", j, q, n)
	}
	// Changing a vote moves the count instead of adding a second ballot.
	if j, q, n := counts(vote("bob", sqliteOpt)); j != 1 || q != 1 || n != 2 {
		t.Fatalf("after bob changed vote json=%d sqlite=%d votes=%d", j, q, n)
	}
	// Re-casting the same choice is idempotent on counts and ballots.
	if j, q, n := counts(vote("bob", sqliteOpt)); j != 1 || q != 1 || n != 2 {
		t.Fatalf("after bob repeated vote json=%d sqlite=%d votes=%d", j, q, n)
	}

	for name, body := range map[string]map[string]any{
		"missing option":   {"action": "vote", "poll_id": pollID},
		"missing poll":     {"action": "vote", "option_id": jsonOpt},
		"unknown poll":     {"action": "vote", "poll_id": "poll-nope", "option_id": jsonOpt},
		"unknown option":   {"action": "vote", "poll_id": pollID, "option_id": "option-nope"},
		"unsupported verb": {"action": "tally", "poll_id": pollID},
	} {
		rec := jamPostAs(t, s, polls, "read-write", "dave", body, false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: vote = %d body=%s", name, rec.Code, rec.Body.String())
		}
	}
	// Rejected votes did not disturb the tallies.
	if j, q, n := counts(vote("carol", jsonOpt)); j != 1 || q != 1 || n != 2 {
		t.Fatalf("after rejected votes json=%d sqlite=%d votes=%d", j, q, n)
	}

	decide := jamPostAs(t, s, polls, "merger", "maintainer", map[string]any{
		"action": "decide", "poll_id": pollID, "outcome": "sqlite", "rationale": "queryable",
	}, false)
	if decide.Code != http.StatusOK {
		t.Fatalf("decide = %d body=%s", decide.Code, decide.Body.String())
	}
	closed := vote("erin", jsonOpt)
	if closed.Code != http.StatusBadRequest || !strings.Contains(closed.Body.String(), "poll is closed") {
		t.Fatalf("vote on decided poll = %d body=%s", closed.Code, closed.Body.String())
	}
}

func TestDecrementPollOptionNeverGoesNegative(t *testing.T) {
	poll := &CampaignPoll{Options: []CampaignPollOption{{ID: "a", Count: 0}, {ID: "b", Count: 2}}}
	decrementPollOption(poll, "a")
	decrementPollOption(poll, "missing")
	decrementPollOption(poll, "b")
	if poll.Options[0].Count != 0 || poll.Options[1].Count != 1 {
		t.Fatalf("options after decrement = %+v", poll.Options)
	}
}

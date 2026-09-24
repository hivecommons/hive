package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func jamTestServer(t *testing.T) *Server {
	t.Helper()
	s := newMinimalServer(t)
	s.deps.Config.Data.MetricsDir = filepath.Join(t.TempDir(), "metrics")
	return s
}

func jamPostAs(t *testing.T, s *Server, path, role, user string, body any, ownerVerified bool) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", role)
	req.Header.Set("X-Hive-User", user)
	if ownerVerified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeJam(t *testing.T, rec *httptest.ResponseRecorder) CampaignJamState {
	t.Helper()
	var resp struct {
		OK  bool             `json:"ok"`
		Jam CampaignJamState `json:"jam"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode jam: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("jam response not ok: %s", rec.Body.String())
	}
	return resp.Jam
}

func TestCampaignJamPersistsAcrossRestart(t *testing.T) {
	s := jamTestServer(t)
	dir := s.deps.Config.Data.MetricsDir
	rec := jamPostAs(t, s, "/api/campaigns/spec-1/jam/threads", "read-write", "alice", map[string]any{
		"section": "Goals",
		"body":    "Can we include async reviewers?",
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("thread post = %d body=%s", rec.Code, rec.Body.String())
	}

	restarted := newMinimalServer(t)
	restarted.deps.Config.Data.MetricsDir = dir
	get := doOwnerGet(restarted, "/api/campaigns/spec-1/jam")
	if get.Code != http.StatusOK {
		t.Fatalf("jam get after restart = %d body=%s", get.Code, get.Body.String())
	}
	jam := decodeJam(t, get)
	if len(jam.Threads) != 1 || len(jam.Threads[0].Comments) != 1 || jam.Threads[0].Comments[0].Author.Name != "alice" {
		t.Fatalf("persisted jam = %+v", jam)
	}
}

func TestCampaignJamPermissions(t *testing.T) {
	s := jamTestServer(t)
	readOnly := jamPostAs(t, s, "/api/campaigns/spec-2/jam/threads", "read", "reader", map[string]any{
		"section": "Goals",
		"body":    "readers cannot mutate",
	}, false)
	if readOnly.Code != http.StatusForbidden {
		t.Fatalf("read-only thread post = %d body=%s", readOnly.Code, readOnly.Body.String())
	}

	poll := jamPostAs(t, s, "/api/campaigns/spec-2/jam/polls", "read-write", "alice", map[string]any{
		"section":  "Scope",
		"question": "Which milestone?",
		"options":  []string{"alpha", "beta"},
	}, false)
	if poll.Code != http.StatusOK {
		t.Fatalf("poll create = %d body=%s", poll.Code, poll.Body.String())
	}
	jam := decodeJam(t, poll)
	decision := jamPostAs(t, s, "/api/campaigns/spec-2/jam/polls", "read-write", "alice", map[string]any{
		"action":    "decide",
		"poll_id":   jam.Polls[0].ID,
		"outcome":   "alpha",
		"rationale": "keeps the phase small",
	}, false)
	if decision.Code != http.StatusForbidden {
		t.Fatalf("read-write decision = %d body=%s", decision.Code, decision.Body.String())
	}
}

func TestCampaignJamPollDecisionStoredOnRevision(t *testing.T) {
	s := jamTestServer(t)
	create := jamPostAs(t, s, "/api/campaigns/spec-3/jam/polls", "read-write", "alice", map[string]any{
		"section":  "Storage",
		"question": "JSON or sqlite?",
		"options":  []string{"json", "sqlite"},
	}, false)
	if create.Code != http.StatusOK {
		t.Fatalf("poll create = %d body=%s", create.Code, create.Body.String())
	}
	jam := decodeJam(t, create)
	pollID := jam.Polls[0].ID
	optionID := jam.Polls[0].Options[1].ID
	vote := jamPostAs(t, s, "/api/campaigns/spec-3/jam/polls", "read-write", "bob", map[string]any{
		"action":    "vote",
		"poll_id":   pollID,
		"option_id": optionID,
	}, false)
	if vote.Code != http.StatusOK {
		t.Fatalf("poll vote = %d body=%s", vote.Code, vote.Body.String())
	}
	decide := jamPostAs(t, s, "/api/campaigns/spec-3/jam/polls", "merger", "maintainer", map[string]any{
		"action":    "decide",
		"poll_id":   pollID,
		"outcome":   "sqlite",
		"rationale": "queryable decisions matter",
	}, false)
	if decide.Code != http.StatusOK {
		t.Fatalf("poll decide = %d body=%s", decide.Code, decide.Body.String())
	}
	jam = decodeJam(t, decide)
	if jam.Polls[0].Decision == nil || jam.Polls[0].Decision.Rationale != "queryable decisions matter" {
		t.Fatalf("decision not stored on poll: %+v", jam.Polls[0])
	}
	if len(jam.Revisions) != 1 || len(jam.Revisions[0].Decisions) != 1 || jam.Revisions[0].Decisions[0].RevisionID != jam.Revisions[0].ID {
		t.Fatalf("decision not stored on revision: %+v", jam.Revisions)
	}
}

func TestCampaignJamRevisionAttributionAndSuggestionAcceptance(t *testing.T) {
	s := jamTestServer(t)
	rec := jamPostAs(t, s, "/api/campaigns/spec-4/jam", "read-write", "alice", map[string]any{
		"spec_content": "## Goals\nInitial",
		"reason":       "seed",
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed revision = %d body=%s", rec.Code, rec.Body.String())
	}
	suggestion := jamPostAs(t, s, "/api/campaigns/spec-4/jam/suggestions", "read-write", "agent-runner", map[string]any{
		"section":       "Goals",
		"proposed_text": "Initial\nAdd async reviewers",
		"agent":         "architect",
		"model":         "gpt-5.4",
	}, false)
	if suggestion.Code != http.StatusOK {
		t.Fatalf("suggestion create = %d body=%s", suggestion.Code, suggestion.Body.String())
	}
	jam := decodeJam(t, suggestion)
	if jam.Suggestions[0].Author.Type != "agent" || jam.Suggestions[0].Author.Model != "gpt-5.4" {
		t.Fatalf("suggestion attribution = %+v", jam.Suggestions[0].Author)
	}
	accept := jamPostAs(t, s, "/api/campaigns/spec-4/jam/suggestions", "owner", "maintainer", map[string]any{
		"action":        "accept",
		"suggestion_id": jam.Suggestions[0].ID,
	}, true)
	if accept.Code != http.StatusOK {
		t.Fatalf("suggestion accept = %d body=%s", accept.Code, accept.Body.String())
	}
	jam = decodeJam(t, accept)
	if len(jam.Revisions) != 2 || jam.Revisions[1].Author.Name != "maintainer" || jam.Revisions[1].Diff == "" {
		t.Fatalf("revision attribution/diff = %+v", jam.Revisions)
	}
	if jam.Suggestions[0].Status != jamSuggestionAccepted || jam.Suggestions[0].AppliedRevisionID != jam.Revisions[1].ID {
		t.Fatalf("suggestion not accepted: %+v", jam.Suggestions[0])
	}
}

func TestCampaignJamSuggestionMatchesExactSectionHeading(t *testing.T) {
	current := "## Goals\nKeep team async review\n\n## Goal\nShort term target"
	got := applySectionSuggestion(current, "Goal", "Ship phase 1 only")
	want := "## Goals\nKeep team async review\n\n## Goal\nShip phase 1 only"
	if got != want {
		t.Fatalf("section replacement mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

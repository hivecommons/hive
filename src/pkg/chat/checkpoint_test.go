package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRunCheckpointPromptAndSinglePendingApproveReply(t *testing.T) {
	var posts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs/acme%2Fwidgets%237/checkpoint":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"run_key": "acme/widgets#7", "title": "Ship widgets", "stage": "plan", "gen": 2, "repo": "acme/widgets",
					"summary": "bounded phone summary", "summary_max_bytes": 2048, "dashboard_url": "/?view=runs&run=acme%2Fwidgets%237#checkpoint",
					"decisions": []map[string]string{
						{"action": "approve", "label": "Approve checkpoint", "method": "POST", "url": "/api/runs/acme%2Fwidgets%237/checkpoint"},
						{"action": "reject", "label": "Reject checkpoint", "method": "POST", "url": "/api/runs/acme%2Fwidgets%237/checkpoint"},
					},
					"staleness": map[string]any{"fence": "lease_gen", "gen": 2},
					"approvers": map[string]any{"role": "owner", "verified_owner_required": true},
				})
				return
			}
			var body runCheckpointDecisionRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode checkpoint decision: %v", err)
			}
			if body.Action != "approve" || body.Gen != 2 {
				t.Fatalf("checkpoint decision body = %+v, want approve gen 2", body)
			}
			posts++
			w.WriteHeader(http.StatusOK)
		case "/api/runs/acme%2Fwidgets%237":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": "acme/widgets#7", "title": "Ship widgets", "stage": "plan", "waiting_on": "human", "plan_epic_id": "epic-7",
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	prev := []runSnapshot{{Key: "acme/widgets#7", Stage: "spec", Gen: 1, WaitingOn: "agent"}}
	cur := []runSnapshot{{Key: "acme/widgets#7", Title: "Ship widgets", Stage: "plan", Gen: 2, WaitingOn: "human", PlanEpicID: "epic-7"}}
	s.diffRuns(prev, cur)
	s.diffRuns(cur, cur)
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "needs a decision") || !strings.Contains(sent[0], "bounded phone summary") || !strings.Contains(sent[0], "Full artifact") {
		t.Fatalf("checkpoint prompts = %#v", sent)
	}
	if len(s.pendingCheckpoints) != 1 {
		t.Fatalf("pending checkpoints = %#v, want 1", s.pendingCheckpoints)
	}

	s.routeMessage(context.Background(), makeMsg("1", "approve", false))
	drainQueue(s, &sent)
	if posts != 1 {
		t.Fatalf("approve posts = %d, want 1", posts)
	}
	if len(s.pendingCheckpoints) != 0 {
		t.Fatalf("pending checkpoint not cleared: %#v", s.pendingCheckpoints)
	}
	if !strings.Contains(sent[len(sent)-1], "Approved") {
		t.Fatalf("approve reply = %#v", sent)
	}
}

func writeRunCheckpointPayload(w http.ResponseWriter, key, title string, gen uint64) {
	if title == "" {
		title = key
	}
	checkpointPath := "/api/runs/" + url.PathEscape(key) + "/checkpoint"
	_ = json.NewEncoder(w).Encode(map[string]any{
		"run_key":           key,
		"title":             title,
		"stage":             "plan",
		"gen":               gen,
		"repo":              strings.Split(key, "#")[0],
		"summary":           title,
		"summary_max_bytes": 2048,
		"dashboard_url":     "/?view=runs&run=" + url.QueryEscape(key) + "#checkpoint",
		"decisions": []map[string]string{
			{"action": "approve", "label": "Approve checkpoint", "method": "POST", "url": checkpointPath},
			{"action": "reject", "label": "Reject checkpoint", "method": "POST", "url": checkpointPath},
		},
		"staleness": map[string]any{"fence": "lease_gen", "gen": gen},
		"approvers": map[string]any{"role": "owner", "verified_owner_required": true},
	})
}

func TestRunCheckpointMultiplePendingRequiresExplicitForm(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#1")] = &pendingCheckpoint{RunKey: "repo/a#1", Authors: map[string]struct{}{"uid": {}}}
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#2")] = &pendingCheckpoint{RunKey: "repo/a#2", Authors: map[string]struct{}{"uid": {}}}

	s.routeMessage(context.Background(), makeMsg("1", "approve", false))
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Multiple pending run decisions") || !strings.Contains(sent[0], "!runs approve <key>") {
		t.Fatalf("multiple pending reply = %#v", sent)
	}
	if len(s.pendingCheckpoints) != 2 {
		t.Fatalf("pending checkpoints were consumed: %#v", s.pendingCheckpoints)
	}
}

func TestRunCheckpointNonOwnerRefused(t *testing.T) {
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:read"}}, discardLogger())
	s.client = ts.Client()
	s.registerBuiltinCommands()
	s.routeMessage(context.Background(), makeMsg("1", "!runs approve acme/widgets#7", false))
	var sent []string
	drainQueue(s, &sent)
	if posts != 0 {
		t.Fatalf("non-owner made %d dashboard posts, want 0", posts)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "owner role required") {
		t.Fatalf("non-owner reply = %#v", sent)
	}
}

func TestRunCheckpointReadOnlyApproveWithoutPendingNotConsumed(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:read"}}, discardLogger())
	if s.handlePendingCheckpointReply(context.Background(), makeMsg("1", "approve", false), "approve") {
		t.Fatal("read-only approve without a pending run checkpoint should not be consumed")
	}
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 0 {
		t.Fatalf("unexpected reply without pending checkpoint: %#v", sent)
	}
}

func TestRunCheckpointClearsWhenRunCompletes(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#1")] = &pendingCheckpoint{RunKey: "repo/a#1", Authors: map[string]struct{}{"uid": {}}}
	s.diffRuns(
		[]runSnapshot{{Key: "repo/a#1", Stage: "plan", WaitingOn: "human"}},
		[]runSnapshot{{Key: "repo/a#1", Stage: "implement", WaitingOn: "agent"}},
	)
	if len(s.pendingCheckpoints) != 0 {
		t.Fatalf("agent transition should clear pending checkpoint: %#v", s.pendingCheckpoints)
	}
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#2")] = &pendingCheckpoint{RunKey: "repo/a#2", Authors: map[string]struct{}{"uid": {}}}
	s.diffRuns(
		[]runSnapshot{{Key: "repo/a#2", Stage: "plan", WaitingOn: "human"}},
		nil,
	)
	if len(s.pendingCheckpoints) != 0 {
		t.Fatalf("missing run should clear pending checkpoint: %#v", s.pendingCheckpoints)
	}
}

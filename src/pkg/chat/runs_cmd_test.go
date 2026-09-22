package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdRunsListAndShow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"key":              "acme/widgets#7",
				"title":            "Ship widgets",
				"repo":             "acme/widgets",
				"stage":            "plan",
				"gen":              2,
				"stage_started_at": "2026-09-22T18:00:00Z",
				"waiting_on":       "human",
				"waiting_since":    "2026-09-22T18:30:00Z",
				"plan_epic_id":     "epic-7",
				"last_receipt":     "https://example.invalid/receipt",
			}})
		case "/api/runs/acme%2Fwidgets%237":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":          "acme/widgets#7",
				"title":        "Ship widgets",
				"repo":         "acme/widgets",
				"stage":        "plan",
				"gen":          2,
				"waiting_on":   "human",
				"plan_epic_id": "epic-7",
				"last_receipt": "https://example.invalid/receipt",
				"stages": []map[string]any{{
					"name": "spec", "status": "completed", "gen": 1, "receipt": "https://example.invalid/spec",
				}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("cmdRuns list: %v", err)
	}
	for _, want := range []string{"Active runs", "acme/widgets#7", "waiting_on=human"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list reply %q missing %q", got, want)
		}
	}
	got, err = s.cmdRuns(context.Background(), "acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns show: %v", err)
	}
	for _, want := range []string{"Ship widgets", "Plan: epic-7", "https://example.invalid/receipt", "https://example.invalid/spec"} {
		if !strings.Contains(got, want) {
			t.Fatalf("show reply %q missing %q", got, want)
		}
	}
}

func TestCmdRunsApproveRejectOwnerGuardAndPlanRoutes(t *testing.T) {
	var posts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs/acme%2Fwidgets%237":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": "acme/widgets#7", "stage": "plan", "waiting_on": "human", "plan_epic_id": "epic-7",
			})
		case "/api/plan/epic-7/approve", "/api/plan/epic-7/reject":
			posts = append(posts, r.URL.EscapedPath())
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	readCtx := context.WithValue(context.Background(), commandRoleContextKey{}, "read")
	got, err := s.cmdRuns(readCtx, "approve acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns read approve: %v", err)
	}
	if !strings.Contains(got, "owner role required") {
		t.Fatalf("read approve reply = %q", got)
	}
	ownerCtx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err = s.cmdRuns(ownerCtx, "approve acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns approve: %v", err)
	}
	if !strings.Contains(got, "Approved") {
		t.Fatalf("approve reply = %q", got)
	}
	got, err = s.cmdRuns(ownerCtx, "reject acme/widgets#7 needs changes")
	if err != nil {
		t.Fatalf("cmdRuns reject: %v", err)
	}
	if !strings.Contains(got, "Rejected") {
		t.Fatalf("reject reply = %q", got)
	}
	if strings.Join(posts, ",") != "/api/plan/epic-7/approve,/api/plan/epic-7/reject" {
		t.Fatalf("posts = %#v", posts)
	}
}

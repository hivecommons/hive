package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type prowQueueRecorder struct {
	statuses []map[string]string
	comments int
}

func prowQueueServer(t *testing.T, label string, statusCode int, rec *prowQueueRecorder) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "0123456789abcdef"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+label:
			json.NewEncoder(w).Encode(map[string]any{"name": label})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			json.NewEncoder(w).Encode([]map[string]any{{"name": label}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/statuses/0123456789abcdef":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode status request: %v", err)
			}
			rec.statuses = append(rec.statuses, req)
			if statusCode != http.StatusCreated {
				w.WriteHeader(statusCode)
				json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			rec.comments++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestQueuePRAutoMergeBindsProwLgtm(t *testing.T) {
	for _, label := range []string{"lgtm", "LGTM"} {
		t.Run(label, func(t *testing.T) {
			rec := &prowQueueRecorder{}
			api := prowQueueServer(t, label, http.StatusCreated, rec)
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			c.SetAutoMergeLabel(label)
			if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
				t.Fatalf("QueuePRAutoMerge returned error: %v", err)
			}
			if len(rec.statuses) != 1 {
				t.Fatalf("status calls = %d, want 1", len(rec.statuses))
			}
			got := rec.statuses[0]
			if got["context"] != "prow/lgtm" || got["state"] != "success" || got["description"] != "lgtm by alice at 0123456" {
				t.Fatalf("status = %v", got)
			}
			if rec.comments != 0 {
				t.Fatalf("comments = %d, want 0", rec.comments)
			}
		})
	}
}

func TestQueuePRAutoMergeOtherLabelSetsNoStatus(t *testing.T) {
	rec := &prowQueueRecorder{}
	api := prowQueueServer(t, "hive/auto-merge", http.StatusCreated, rec)
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel("hive/auto-merge")
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
		t.Fatalf("QueuePRAutoMerge returned error: %v", err)
	}
	if len(rec.statuses) != 0 {
		t.Fatalf("status calls = %d, want 0", len(rec.statuses))
	}
}

func TestQueuePRAutoMergeStatusFailureCommentsAndSucceeds(t *testing.T) {
	rec := &prowQueueRecorder{}
	api := prowQueueServer(t, "lgtm", http.StatusForbidden, rec)
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel("lgtm")
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
		t.Fatalf("QueuePRAutoMerge returned error: %v", err)
	}
	if len(rec.statuses) != 1 || rec.comments != 1 {
		t.Fatalf("status calls = %d, comments = %d, want 1 and 1", len(rec.statuses), rec.comments)
	}
}

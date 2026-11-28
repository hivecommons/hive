package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// A reviewer's standing review is their latest by submission time
// (reviewgate.Standing), not the last one the API happened to list.
func TestHasOutstandingChangesRequestedUsesSubmissionOrder(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 2, "state": "CHANGES_REQUESTED", "submitted_at": "2026-11-02T00:00:00Z", "user": map[string]string{"login": "reviewer"}},
				{"id": 1, "state": "APPROVED", "submitted_at": "2026-11-01T00:00:00Z", "user": map[string]string{"login": "reviewer"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/8/reviews":
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 4, "state": "APPROVED", "submitted_at": "2026-11-02T00:00:00Z", "user": map[string]string{"login": "Reviewer"}},
				{"id": 3, "state": "CHANGES_REQUESTED", "submitted_at": "2026-11-01T00:00:00Z", "user": map[string]string{"login": "reviewer"}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()
	engine := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL), Options{})

	if blocked, err := engine.hasOutstandingChangesRequested(context.Background(), "acme", "widget", 7); err != nil || !blocked {
		t.Fatalf("PR 7 blocked=%v err=%v, want the later-submitted change request to stand", blocked, err)
	}
	if blocked, err := engine.hasOutstandingChangesRequested(context.Background(), "acme", "widget", 8); err != nil || blocked {
		t.Fatalf("PR 8 blocked=%v err=%v, want the later-submitted approval to clear it", blocked, err)
	}
}

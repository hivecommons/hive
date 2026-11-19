package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The sentinel label's pre-fix default was over GitHub's 100-character cap.
// EnsureIssueLabel only creates, so the sweep rewrites the exact legacy text
// and leaves any other description alone.
func TestResyncSentinelLabelDescription(t *testing.T) {
	cases := []struct {
		name      string
		existing  string
		wantPatch bool
	}{
		{"legacy default is rewritten", config.LegacySentinelLabelDescription, true},
		{"maintainer text is kept", "custom maintainer text", false},
		{"already the new default", config.DefaultSentinelLabelDescription, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var patched string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]string{"name": "sentinel-alert", "color": "b60205", "description": tc.existing})
				case http.MethodPatch:
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					patched = body["description"]
					_ = json.NewEncoder(w).Encode(body)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.resyncSentinelLabelDescription(context.Background(), "o", "r", "sentinel-alert", config.DefaultSentinelLabelDescription)
			mu.Lock()
			defer mu.Unlock()
			if tc.wantPatch && patched != config.DefaultSentinelLabelDescription {
				t.Fatalf("patched description = %q, want the new default", patched)
			}
			if !tc.wantPatch && patched != "" {
				t.Fatalf("unexpected description patch %q", patched)
			}
		})
	}
}

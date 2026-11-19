package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// shaSoftRecorder captures the label writes the SHA-hold sweep makes.
type shaSoftRecorder struct {
	mu       sync.Mutex
	added    []string
	removed  []string
	comments int
}

func buildSHASoftMux(t *testing.T, org, repo string, issue wireIssue, comments []wireComment, events []map[string]any, rec *shaSoftRecorder) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	n := issue.Number
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, []wireIssue{issue}))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			rec.mu.Lock()
			rec.comments++
			rec.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1}`))
			return
		}
		w.Write(mustMarshal(t, comments))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/events", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, events))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var names []string
			_ = json.Unmarshal(body, &names)
			rec.mu.Lock()
			rec.added = append(rec.added, names...)
			rec.mu.Unlock()
		}
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels/", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			rec.mu.Lock()
			rec.removed = append(rec.removed, r.URL.Path[len(fmt.Sprintf("/repos/%s/%s/issues/%d/labels/", org, repo, n)):])
			rec.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	return mux
}

func TestEnforceSHAHold_TrustedReporterGetsSoftLabel(t *testing.T) {
	org, repo := "testorg", "console"
	cases := []struct {
		name         string
		association  string
		labels       []wireLabel
		comments     []wireComment
		events       []map[string]any
		wantAdded    []string
		wantRemoved  []string
		wantComments int
		want         SHAHoldResult
	}{
		{
			// hivecommons/hive#18866: the gate is for outside reporters; a
			// maintainer's issue is exempt and never gets the label.
			name:        "member without SHA is exempt, not held",
			association: "MEMBER",
			labels:      []wireLabel{{Name: "kind/bug"}},
			want:        SHAHoldResult{Skipped: 1},
		},
		{
			// hivecommons/hive#18468: the hive never applies `hold`
			// itself, so an untrusted reporter gets needs-sha too.
			name:         "untrusted reporter gets needs-sha, never hold",
			association:  "NONE",
			labels:       []wireLabel{{Name: "kind/bug"}},
			wantAdded:    []string{"needs-sha"},
			want:         SHAHoldResult{Held: 1},
			wantComments: 1,
		},
		{
			name:        "member supplies SHA and the soft label lifts",
			association: "MEMBER",
			labels:      []wireLabel{{Name: "kind/bug"}, {Name: "needs-sha"}},
			comments:    []wireComment{{User: wireUser{"maintainer"}, Body: "running abc1234def"}},
			wantRemoved: []string{"needs-sha"},
			want:        SHAHoldResult{Unheld: 1},
		},
		{
			// Exempt since #18866, so the sweep's own legacy hold is lifted
			// rather than swapped for needs-sha.
			name:        "sweep's legacy hard hold on a member issue is lifted",
			association: "OWNER",
			labels:      []wireLabel{{Name: "kind/bug"}, {Name: "hold"}},
			comments:    []wireComment{{User: wireUser{testHiveAppBotLogin}, Body: shaHoldComment, CreatedAt: "2026-09-15T12:00:01Z"}},
			events: []map[string]any{{
				"event": "labeled", "created_at": "2026-09-15T12:00:00Z",
				"actor": map[string]string{"login": testHiveAppBotLogin}, "label": map[string]string{"name": "hold"},
			}},
			wantRemoved: []string{"hold"},
			want:        SHAHoldResult{Unheld: 1},
		},
		{
			name:        "human hold on a member issue is left alone",
			association: "COLLABORATOR",
			labels:      []wireLabel{{Name: "kind/bug"}, {Name: "hold"}},
			events: []map[string]any{{
				"event": "labeled", "created_at": "2026-09-15T12:00:00Z",
				"actor": map[string]string{"login": "alice"}, "label": map[string]string{"name": "hold"},
			}},
			want: SHAHoldResult{Skipped: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &shaSoftRecorder{}
			issue := wireIssue{Number: 11732, Title: "bug", User: wireUser{"maintainer"}, Labels: tc.labels, CreatedAt: hoursAgo(1), AuthorAssociation: tc.association}
			server := httptest.NewServer(buildSHASoftMux(t, org, repo, issue, tc.comments, tc.events, rec))
			defer server.Close()
			c := newTestClient(t, server, org, []string{repo})
			c.SetAppBotLogin(testHiveAppBotLogin)
			got, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{
				PrimaryRepo:         repo,
				AIAuthor:            "hive-bot",
				TrustedAssociations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
				TrustedLabel:        "needs-sha",
			})
			if err != nil {
				t.Fatalf("EnforceSHAHold: %v", err)
			}
			if *got != tc.want {
				t.Errorf("result = %+v, want %+v", *got, tc.want)
			}
			if fmt.Sprint(rec.added) != fmt.Sprint(tc.wantAdded) {
				t.Errorf("added = %v, want %v", rec.added, tc.wantAdded)
			}
			if fmt.Sprint(rec.removed) != fmt.Sprint(tc.wantRemoved) {
				t.Errorf("removed = %v, want %v", rec.removed, tc.wantRemoved)
			}
			if rec.comments != tc.wantComments {
				t.Errorf("comments = %d, want %d", rec.comments, tc.wantComments)
			}
		})
	}
}

func TestSHAHoldConfig_LabelIsNeverHold(t *testing.T) {
	for _, label := range []string{"", "hold", " HOLD "} {
		cfg := SHAHoldConfig{TrustedAssociations: []string{"MEMBER"}, TrustedLabel: label}
		if got := cfg.shaHoldLabel(); got != defaultSHAHoldLabel {
			t.Errorf("TrustedLabel %q: label = %q, want %q (never hold)", label, got, defaultSHAHoldLabel)
		}
	}
	cfg := SHAHoldConfig{TrustedLabel: "awaiting-sha"}
	if got := cfg.shaHoldLabel(); got != "awaiting-sha" {
		t.Errorf("configured label = %q, want awaiting-sha", got)
	}
}

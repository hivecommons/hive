package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestReleaseHumanMergePathHold(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name          string
		actor         string
		extraEvent    bool
		timelineError bool
		qualified     bool
		legacy        bool
		edited        bool
		changedHead   bool
		noGate        bool
		mode          writeback.RepoMode
		paused        bool
		want          bool
	}{
		{name: "App-only stale path hold", actor: "hive[bot]", qualified: true, want: true},
		{name: "missing writeback gate", actor: "hive[bot]", qualified: true, noGate: true},
		{name: "writeback off", actor: "hive[bot]", qualified: true, mode: writeback.RepoModeOff},
		{name: "writeback issues only", actor: "hive[bot]", qualified: true, mode: writeback.RepoModeIssuesOnly},
		{name: "writeback paused", actor: "hive[bot]", qualified: true, paused: true},
		{name: "human hold", actor: "person", qualified: true},
		{name: "human re-added hold", actor: "hive[bot]", extraEvent: true, qualified: true},
		{name: "non-qualifying head", actor: "hive[bot]"},
		{name: "unknown timeline", actor: "hive[bot]", timelineError: true, qualified: true},
		{name: "legacy marker", actor: "hive[bot]", legacy: true, qualified: true},
		{name: "edited marker", actor: "hive[bot]", edited: true, qualified: true},
		{name: "changed head", actor: "hive[bot]", changedHead: true, qualified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes, calls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
					currentHead := head
					if tc.changedHead {
						currentHead = oldHead
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": currentHead}, "labels": []map[string]string{{"name": "hold"}}})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					body := HumanMergePathComment([]string{"Containerfile"})
					if !tc.legacy {
						body += humanMergeHeadMarker(oldHead)
					}
					updated := "2026-11-02T12:00:02Z"
					if tc.edited {
						updated = "2026-11-02T12:00:03Z"
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{{"body": body, "created_at": "2026-11-02T12:00:02Z", "updated_at": updated, "user": map[string]string{"login": "hive[bot]", "type": "Bot"}}})
				case strings.HasSuffix(r.URL.Path, "/events"):
					if tc.timelineError {
						w.WriteHeader(500)
						return
					}
					events := []map[string]any{{"event": "labeled", "created_at": "2026-11-02T12:00:01Z", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": tc.actor, "type": "Bot"}}}
					if tc.extraEvent {
						events = append(events, map[string]any{"event": "labeled", "created_at": "2026-11-02T12:00:03Z", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": "person", "type": "User"}})
					}
					_ = json.NewEncoder(w).Encode(events)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/hold"):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			mode := tc.mode
			if mode == writeback.RepoModeDefault {
				mode = writeback.RepoModeFull
			}
			gate := writeback.New(writeback.Config{
				PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: mode}},
			}, writeback.Deps{
				RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
					return writeback.RepoPauseInfo{}, tc.paused
				},
			}, "")
			if tc.noGate {
				gate = nil
			}
			released, err := ReleaseHumanMergePathHold(context.Background(), client, gate, "o", "r", 1, "hive[bot]", head, tc.qualified)
			if released != tc.want || (deletes == 1) != tc.want {
				t.Fatalf("released=%v deletes=%d err=%v", released, deletes, err)
			}
			if tc.want && err != nil {
				t.Fatalf("successful release returned an error: %v", err)
			}
			if tc.timelineError && err == nil {
				t.Fatal("timeline failure must be reported")
			}
			if (!tc.qualified || tc.noGate) && calls != 0 {
				t.Fatalf("non-qualifying request made %d calls", calls)
			}
		})
	}
}

func TestHumanMergeHoldHeadAttribution(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, existingHold := range []bool{false, true} {
		t.Run(map[bool]string{false: "new hold", true: "existing unrelated hold"}[existingHold], func(t *testing.T) {
			var posted string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					labels := []map[string]string{}
					if existingHold {
						labels = append(labels, map[string]string{"name": "hold"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": head}, "labels": labels})
				case strings.HasSuffix(r.URL.Path, "/labels"):
					_, _ = w.Write([]byte(`[{"name":"hold"}]`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
					_, _ = w.Write([]byte(`[]`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					var comment gh.IssueComment
					_ = json.NewDecoder(r.Body).Decode(&comment)
					posted = comment.GetBody()
					_, _ = w.Write([]byte(`{"id":1}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := HoldForHumanMergePaths(context.Background(), client, "o", "r", 1, []string{"Containerfile"}, head); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(posted, HumanMergePathMarker) {
				t.Fatal("legacy marker must remain matchable")
			}
			if strings.Contains(posted, humanMergeHeadMarker(head)) == existingHold {
				t.Fatalf("wrong attribution: %s", posted)
			}
		})
	}
}

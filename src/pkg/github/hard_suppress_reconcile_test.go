package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type hardSuppressFixture struct {
	number   int
	label    string
	events   []map[string]any
	comments []wireComment
	timeline []map[string]any
}

func labelEvent(kind, actor, at, label string) map[string]any {
	return map[string]any{
		"event": kind, "created_at": at,
		"actor": map[string]string{"login": actor},
		"label": map[string]string{"name": label},
	}
}

func crossRefMergedPR(number int, mergedAt string) map[string]any {
	pr := map[string]any{"url": "https://api.github.com/repos/testorg/console/pulls/" + fmt.Sprint(number)}
	if mergedAt != "" {
		pr["merged_at"] = mergedAt
	}
	return map[string]any{
		"event":      "cross-referenced",
		"created_at": "2026-10-14T19:00:00Z",
		"source": map[string]any{
			"type":  "issue",
			"issue": map[string]any{"number": number, "pull_request": pr},
		},
	}
}

func runHardSuppressFixture(t *testing.T, appBot string, fx hardSuppressFixture) (*HardSuppressReconcileResult, []string, []string) {
	t.Helper()
	org, repo := "testorg", "console"
	var mu sync.Mutex
	var removed, posted []string
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("labels") != fx.label {
			w.Write([]byte(`[]`))
			return
		}
		w.Write(mustMarshal(t, []wireIssue{{Number: fx.number, Title: "parked", CreatedAt: "2026-10-14T17:00:00Z", User: wireUser{"maintainer"}, Labels: []wireLabel{{Name: fx.label}}}}))
	})
	base := fmt.Sprintf("/repos/%s/%s/issues/%d", org, repo, fx.number)
	mux.HandleFunc(base+"/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, fx.events))
	})
	mux.HandleFunc(base+"/timeline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, fx.timeline))
	})
	mux.HandleFunc(base+"/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			mu.Lock()
			posted = append(posted, "comment")
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1}`))
			return
		}
		w.Write(mustMarshal(t, fx.comments))
	})
	mux.HandleFunc(base+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			removed = append(removed, strings.TrimPrefix(r.URL.Path, base+"/labels/"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin(appBot)
	res, err := c.ReconcileHiveHardSuppress(context.Background(), HardSuppressReconcileConfig{Repos: []string{repo}})
	if err != nil {
		t.Fatalf("ReconcileHiveHardSuppress: %v", err)
	}
	return res, removed, posted
}

func TestReconcileHiveHardSuppress(t *testing.T) {
	shaNotice := wireComment{User: wireUser{testHiveAppBotLogin}, Body: shaHoldComment, CreatedAt: "2026-10-13T12:00:01Z"}
	cases := []struct {
		name        string
		appBot      string
		fx          hardSuppressFixture
		wantRemoved []string
		wantLifted  int
	}{
		{
			name:   "sha hold lifted after referencing PR merged (#11732)",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 11732, label: "hold",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-13T12:00:00Z", "hold")},
				comments: []wireComment{shaNotice},
				timeline: []map[string]any{crossRefMergedPR(11736, "2026-10-14T20:00:00Z")},
			},
			wantRemoved: []string{"hold"},
			wantLifted:  1,
		},
		{
			name:   "app-applied blocked lifted after PRs merged since (#11854)",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 11854, label: "blocked",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-14T18:12:08Z", "blocked")},
				timeline: []map[string]any{crossRefMergedPR(11990, "2026-10-15T01:00:00Z")},
			},
			wantRemoved: []string{"blocked"},
			wantLifted:  1,
		},
		{
			name:   "human-applied blocked is never lifted",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 1, label: "blocked",
				events:   []map[string]any{labelEvent("labeled", "alice", "2026-10-14T18:12:08Z", "blocked")},
				timeline: []map[string]any{crossRefMergedPR(2, "2026-10-15T01:00:00Z")},
			},
		},
		{
			name:   "human re-applied after hive is never lifted",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 1, label: "blocked",
				events: []map[string]any{
					labelEvent("labeled", testHiveAppBotLogin, "2026-10-14T18:00:00Z", "blocked"),
					labelEvent("unlabeled", "alice", "2026-10-14T18:30:00Z", "blocked"),
					labelEvent("labeled", "alice", "2026-10-14T19:00:00Z", "blocked"),
				},
				timeline: []map[string]any{crossRefMergedPR(2, "2026-10-15T01:00:00Z")},
			},
		},
		{
			name:   "app hold without the sha-hold notice is not the sha sweep's",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 1, label: "hold",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-13T12:00:00Z", "hold")},
				timeline: []map[string]any{crossRefMergedPR(2, "2026-10-14T20:00:00Z")},
			},
		},
		{
			name:   "PR merged before the park does not lift",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 1, label: "blocked",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-14T18:12:08Z", "blocked")},
				timeline: []map[string]any{crossRefMergedPR(2, "2026-10-14T10:00:00Z")},
			},
		},
		{
			name:   "open referencing PR does not lift",
			appBot: testHiveAppBotLogin,
			fx: hardSuppressFixture{
				number: 1, label: "blocked",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-14T18:12:08Z", "blocked")},
				timeline: []map[string]any{crossRefMergedPR(2, "")},
			},
		},
		{
			name:   "no app bot login fails closed",
			appBot: "",
			fx: hardSuppressFixture{
				number: 1, label: "blocked",
				events:   []map[string]any{labelEvent("labeled", testHiveAppBotLogin, "2026-10-14T18:12:08Z", "blocked")},
				timeline: []map[string]any{crossRefMergedPR(2, "2026-10-15T01:00:00Z")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, removed, posted := runHardSuppressFixture(t, tc.appBot, tc.fx)
			if res.Lifted != tc.wantLifted {
				t.Errorf("Lifted = %d, want %d (result %+v)", res.Lifted, tc.wantLifted, res)
			}
			if fmt.Sprint(removed) != fmt.Sprint(tc.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tc.wantRemoved)
			}
			if len(posted) != tc.wantLifted {
				t.Errorf("notes posted = %d, want %d", len(posted), tc.wantLifted)
			}
		})
	}
}

func TestHardSuppressLiftedNoteCarriesMarker(t *testing.T) {
	at, err := time.Parse(time.RFC3339, "2026-10-14T18:12:08Z")
	if err != nil {
		t.Fatal(err)
	}
	note := hardSuppressLiftedNote("blocked", at, "#11990")
	for _, want := range []string{hardSuppressLiftedMarker, "`blocked`", "#11990", "2026-10-14T18:12:08Z"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
}

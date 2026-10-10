package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type supersessionPRFixture struct {
	Number int
	Title  string
	Body   string
	Author string
	Merged bool
	SHA    string
	Files  []string
	// Fields below drive the contributor auto-close path (#11418).
	Labels        []string
	Additions     int
	Comments      []supersessionCommentFixture
	Threads       []supersessionThreadFixture
	ThreadsBroken bool
}

type supersessionCommentFixture struct {
	ID        int64
	Body      string
	Author    string
	Bot       bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type supersessionThreadFixture struct {
	Resolved bool
	Comments []supersessionCommentFixture
}

type supersessionIssueFixture struct {
	Number          int
	State           string
	GraphQLCloserPR int
	ClosingCommit   string
}

type supersessionObservations struct {
	mu       sync.Mutex
	closed   []int
	comments map[int][]string
	labels   map[int][]string
	edits    map[int64]string
}

func supersessionSweepServer(t *testing.T, org, repo string, prs []supersessionPRFixture, issues []supersessionIssueFixture) (*httptest.Server, *supersessionObservations) {
	t.Helper()
	obs := &supersessionObservations{comments: map[int][]string{}, labels: map[int][]string{}, edits: map[int64]string{}}
	now := time.Now().Format(time.RFC3339)
	prByNumber := map[int]supersessionPRFixture{}
	for _, pr := range prs {
		prByNumber[pr.Number] = pr
	}
	issueByNumber := map[int]supersessionIssueFixture{}
	for _, issue := range issues {
		issueByNumber[issue.Number] = issue
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if strings.Contains(payload.Query, "reviewThreads") {
			n, _ := strconv.Atoi(fmt.Sprint(payload.Variables["number"]))
			pr := prByNumber[n]
			if pr.ThreadsBroken {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "boom"}}})
				return
			}
			threads := []map[string]any{}
			for _, th := range pr.Threads {
				cms := []map[string]any{}
				for _, cm := range th.Comments {
					typename := "User"
					if cm.Bot {
						typename = "Bot"
					}
					cms = append(cms, map[string]any{"createdAt": cm.CreatedAt.Format(time.RFC3339), "author": map[string]any{"__typename": typename, "login": cm.Author}})
				}
				threads = append(threads, map[string]any{"isResolved": th.Resolved, "comments": map[string]any{"nodes": cms}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"nodes": threads}}}}})
			return
		}
		n, _ := strconv.Atoi(fmt.Sprint(payload.Variables["issue"]))
		issue := issueByNumber[n]
		nodes := []map[string]any{}
		if issue.GraphQLCloserPR > 0 {
			nodes = append(nodes, map[string]any{
				"number":   issue.GraphQLCloserPR,
				"url":      fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, issue.GraphQLCloserPR),
				"merged":   true,
				"mergedAt": "2026-10-10T11:40:00Z",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": map[string]any{"nodes": nodes}}}}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		state := r.URL.Query().Get("state")
		wire := []map[string]any{}
		for _, pr := range prs {
			if state == "open" && pr.Merged {
				continue
			}
			if state == "closed" && !pr.Merged {
				continue
			}
			entry := map[string]any{
				"number":   pr.Number,
				"title":    pr.Title,
				"body":     pr.Body,
				"state":    "open",
				"user":     map[string]any{"login": pr.Author, "type": "Bot"},
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, pr.Number),
			}
			if len(pr.Labels) > 0 {
				labels := []map[string]any{}
				for _, l := range pr.Labels {
					labels = append(labels, map[string]any{"name": l})
				}
				entry["labels"] = labels
			}
			if pr.Merged {
				entry["state"] = "closed"
				entry["merged_at"] = now
				entry["updated_at"] = now
			}
			wire = append(wire, entry)
		}
		_ = json.NewEncoder(w).Encode(wire)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/commits/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/commits/", org, repo)), "/pulls")
		wire := []map[string]any{}
		for _, pr := range prs {
			if pr.SHA == sha && pr.Merged {
				wire = append(wire, map[string]any{
					"number":    pr.Number,
					"state":     "closed",
					"merged_at": now,
					"html_url":  fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, pr.Number),
				})
			}
		}
		_ = json.NewEncoder(w).Encode(wire)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/pulls/", org, repo))
		parts := strings.Split(rest, "/")
		n, _ := strconv.Atoi(parts[0])
		if len(parts) == 1 && r.Method == "GET" {
			pr := prByNumber[n]
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "additions": pr.Additions, "deletions": 0})
			return
		}
		if len(parts) == 2 && parts[1] == "files" {
			wire := []map[string]any{}
			for _, f := range prByNumber[n].Files {
				wire = append(wire, map[string]any{"filename": f})
			}
			_ = json.NewEncoder(w).Encode(wire)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo))
		if strings.HasPrefix(rest, "comments/") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "comments/"), 10, 64)
			if r.Method != "PATCH" || id == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			obs.mu.Lock()
			obs.edits[id] = payload.Body
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": payload.Body})
			return
		}
		parts := strings.Split(rest, "/")
		n, _ := strconv.Atoi(parts[0])
		switch {
		case len(parts) == 1 && r.Method == "GET":
			issue := issueByNumber[n]
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": issue.State})
		case len(parts) == 1 && r.Method == "PATCH":
			obs.mu.Lock()
			obs.closed = append(obs.closed, n)
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "closed"})
		case len(parts) == 2 && parts[1] == "comments" && r.Method == "GET":
			wire := []map[string]any{}
			for _, cm := range prByNumber[n].Comments {
				userType := "User"
				if cm.Bot {
					userType = "Bot"
				}
				updated := cm.UpdatedAt
				if updated.IsZero() {
					updated = cm.CreatedAt
				}
				wire = append(wire, map[string]any{
					"id":         cm.ID,
					"body":       cm.Body,
					"user":       map[string]any{"login": cm.Author, "type": userType},
					"created_at": cm.CreatedAt.Format(time.RFC3339),
					"updated_at": updated.Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(wire)
		case len(parts) == 2 && parts[1] == "comments" && r.Method == "POST":
			var payload struct {
				Body *string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			body := ""
			if payload.Body != nil {
				body = *payload.Body
			}
			obs.mu.Lock()
			obs.comments[n] = append(obs.comments[n], body)
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": n + 1000, "body": body})
		case len(parts) == 2 && parts[1] == "labels" && r.Method == "POST":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			obs.mu.Lock()
			obs.labels[n] = append(obs.labels[n], labels...)
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case len(parts) == 2 && parts[1] == "timeline" && r.Method == "GET":
			issue := issueByNumber[n]
			wire := []map[string]any{}
			if issue.ClosingCommit != "" {
				wire = append(wire, map[string]any{"event": "closed", "commit_id": issue.ClosingCommit, "created_at": now})
			}
			_ = json.NewEncoder(w).Encode(wire)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": SupersededLabel})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": SupersededLabel})
	})
	return httptest.NewServer(mux), obs
}

func TestSweepSupersededOpenPRs_ClosedByPullRequestReferenceClosesOwnAtL6(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 10, Title: "fix thing", Body: "Refs #1", Author: "hive-app[bot]", Files: []string{"pkg/a.go"}},
			{Number: 20, Title: "landed", Body: "Fixes #1", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 1, State: "closed", GraphQLCloserPR: 20}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelFullyAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Closed) != 1 || len(obs.closed) != 1 || obs.closed[0] != 10 {
		t.Fatalf("closed result=%v wire=%v, want PR #10 closed", res.Closed, obs.closed)
	}
	if body := obs.comments[10][0]; !strings.Contains(body, "#1") || !strings.Contains(body, "/pull/20") {
		t.Fatalf("comment %q does not link issue and closer PR", body)
	}
}

func TestSweepSupersededOpenPRs_ClosedByCommitYieldsCandidate(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 11, Title: "fix thing", Body: "Refs #2", Author: "hive-app[bot]", Files: []string{"pkg/a.go"}},
			{Number: 21, Title: "landed", Body: "Fixes #2", Author: "hive-app[bot]", Merged: true, SHA: "abc123", Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 2, State: "closed", ClosingCommit: "abc123"}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelFullyAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Closed) != 1 || res.Closed[0].CloserPR != 21 || len(obs.closed) != 1 {
		t.Fatalf("commit-closed candidate not acted on: result=%+v closed=%v", res, obs.closed)
	}
}

func TestSweepSupersededOpenPRs_ContributorCommentOnly(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 12, Title: "fix thing", Body: "Refs #3", Author: "contributor", Files: []string{"pkg/a.go"}},
			{Number: 22, Title: "landed", Body: "Fixes #3", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 3, State: "closed", GraphQLCloserPR: 22}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelFullyAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Commented) != 1 || len(obs.comments[12]) != 1 {
		t.Fatalf("contributor PR should receive one comment: result=%+v comments=%v", res, obs.comments)
	}
	if len(obs.closed) != 0 || len(obs.labels[12]) != 0 {
		t.Fatalf("contributor PR mutated too far: closed=%v labels=%v", obs.closed, obs.labels)
	}
}

func TestSweepSupersededOpenPRs_BelowL6LabelsOwnPR(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 13, Title: "fix thing", Body: "Refs #4", Author: "hive-app[bot]", Files: []string{"pkg/a.go"}},
			{Number: 23, Title: "landed", Body: "Fixes #4", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 4, State: "closed", GraphQLCloserPR: 23}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelFullyAutonomous - 1 }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Commented) != 1 || len(obs.closed) != 0 {
		t.Fatalf("below L6 should comment without closing: result=%+v closed=%v", res, obs.closed)
	}
	if got := obs.labels[13]; len(got) != 1 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("labels on #13 = %v, want needs-human", got)
	}
}

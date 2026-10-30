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

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
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
	// Fields below drive the hive self-resolution path (#13799).
	HeadSHA string
	BaseRef string
	BaseSHA string
	// BehindBy is what compare base...head answers; 0 means the head
	// contains the base tip.
	BehindBy int
	// UpdateBranchStatus, when non-zero, is the HTTP status the update-branch
	// endpoint answers instead of 202.
	UpdateBranchStatus int
	// LabelEvents feed the needs-human attribution read (actor per label).
	LabelEvents []supersessionLabelEventFixture
	// CrossRefs are the timeline's cross-referenced events from other PRs
	// (the declared-supersession read, hivecommons/hive#15208).
	CrossRefs []supersessionCrossRefFixture
}

type supersessionCrossRefFixture struct {
	Number int
	Body   string
	Merged bool
	// Repo overrides the source repository ("" = same repo).
	Repo string
}

type supersessionLabelEventFixture struct {
	Event string
	Label string
	Actor string
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
	StateReason     string
	GraphQLCloserPR int
	ClosingCommit   string
}

type supersessionObservations struct {
	mu            sync.Mutex
	closed        []int
	comments      map[int][]string
	labels        map[int][]string
	removedLabels map[int][]string
	edits         map[int64]string
	bodies        map[int]string
	updated       map[int]string // PR number → expected_head_sha sent to update-branch
}

func supersessionSweepServer(t *testing.T, org, repo string, prs []supersessionPRFixture, issues []supersessionIssueFixture) (*httptest.Server, *supersessionObservations) {
	t.Helper()
	obs := &supersessionObservations{comments: map[int][]string{}, labels: map[int][]string{}, removedLabels: map[int][]string{}, edits: map[int64]string{}, bodies: map[int]string{}, updated: map[int]string{}}
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
			headSHA, baseRef, baseSHA := pr.HeadSHA, pr.BaseRef, pr.BaseSHA
			if headSHA == "" {
				headSHA = fmt.Sprintf("head%dsha0000000", pr.Number)
			}
			if baseRef == "" {
				baseRef = "main"
			}
			if baseSHA == "" {
				baseSHA = "basesha000000000"
			}
			entry := map[string]any{
				"number":   pr.Number,
				"title":    pr.Title,
				"body":     pr.Body,
				"state":    "open",
				"user":     map[string]any{"login": pr.Author, "type": "Bot"},
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, pr.Number),
				"head":     map[string]any{"sha": headSHA, "ref": "topic/supersession"},
				"base":     map[string]any{"sha": baseSHA, "ref": baseRef},
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
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/compare/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/compare/", org, repo))
		_, head, _ := strings.Cut(spec, "...")
		behind := 0
		for _, pr := range prs {
			if pr.HeadSHA != "" && pr.HeadSHA == head {
				behind = pr.BehindBy
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": behind, "ahead_by": 1, "status": "ahead"})
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
		if len(parts) == 1 && r.Method == "PATCH" {
			var payload struct {
				Body *string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			obs.mu.Lock()
			if payload.Body != nil {
				obs.bodies[n] = *payload.Body
			}
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open"})
			return
		}
		if len(parts) == 2 && parts[1] == "update-branch" && r.Method == "PUT" {
			var payload struct {
				ExpectedHeadSHA string `json:"expected_head_sha"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			obs.mu.Lock()
			obs.updated[n] = payload.ExpectedHeadSHA
			obs.mu.Unlock()
			if status := prByNumber[n].UpdateBranchStatus; status != 0 {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "merge conflict between base and head"})
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
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
		// A label name may contain slashes; keep the label suffix intact.
		parts := strings.SplitN(rest, "/", 3)
		n, _ := strconv.Atoi(parts[0])
		switch {
		case len(parts) == 1 && r.Method == "GET":
			issue := issueByNumber[n]
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": issue.State, "state_reason": issue.StateReason})
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
		case len(parts) == 3 && parts[1] == "labels" && r.Method == "DELETE":
			obs.mu.Lock()
			obs.removedLabels[n] = append(obs.removedLabels[n], parts[2])
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]any{})
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
			for i, ev := range prByNumber[n].LabelEvents {
				wire = append(wire, map[string]any{"id": i + 1, "event": ev.Event, "label": map[string]any{"name": ev.Label}, "actor": map[string]any{"login": ev.Actor}, "created_at": now})
			}
			for _, ref := range prByNumber[n].CrossRefs {
				srcRepo := ref.Repo
				if srcRepo == "" {
					srcRepo = org + "/" + repo
				}
				links := map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", srcRepo, ref.Number)}
				if ref.Merged {
					links["merged_at"] = "2026-10-10T11:40:00Z"
				}
				wire = append(wire, map[string]any{"event": "cross-referenced", "created_at": now, "source": map[string]any{"type": "issue", "issue": map[string]any{
					"number": ref.Number, "body": ref.Body, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", srcRepo, ref.Number),
					"repository_url": "https://api.github.com/repos/" + srcRepo, "pull_request": links,
				}}})
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

// newSupersessionTestClient is newTestClient with the hive App identity set
// and a recording stage FactSink installed, so a test can assert the sweep
// parked a PR through the stage engine (and never wrote a stage label itself).
func newSupersessionTestClient(t *testing.T, server *httptest.Server, org, repo string) (*Client, *stageenginetest.Recorder) {
	t.Helper()
	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	rec := &stageenginetest.Recorder{}
	c.SetStageFactSink(rec)
	return c, rec
}

// parkedPRs lists the PR numbers the recorder saw Parked with needs-human.
func parkedPRs(rec *stageenginetest.Recorder) []int {
	var out []int
	for _, f := range rec.All() {
		if f.Method == "Parked" && f.Label == issueNeedsHumanLabel && f.Ref.Kind == stagelabels.PRItem {
			out = append(out, f.Ref.Number)
		}
	}
	return out
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
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
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
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
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
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
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

	c, rec := newSupersessionTestClient(t, server, org, repo)
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous - 1 }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Commented) != 1 || len(obs.closed) != 0 {
		t.Fatalf("below L6 should comment without closing: result=%+v closed=%v", res, obs.closed)
	}
	if got := obs.labels[13]; len(got) != 2 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("labels on #13 = %v, want needs-human", got)
	}
	if got := parkedPRs(rec); len(got) != 1 || got[0] != 13 {
		t.Fatalf("Parked facts = %v, want #13 parked at 085 through the stage engine", got)
	}
}

func TestSweepSupersededOpenPRs_HeldOwnPRIsLeftAlone(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 14, Title: "fix thing", Body: "Refs #5", Author: "hive-app[bot]", Files: []string{"pkg/a.go"}, Labels: []string{"hold"}},
			{Number: 24, Title: "landed", Body: "Fixes #5", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 5, State: "closed", GraphQLCloserPR: 24}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Closed) != 0 || len(res.Commented) != 0 || len(obs.closed) != 0 || len(obs.comments[14]) != 0 || len(obs.labels[14]) != 0 {
		t.Fatalf("held hive-authored PR must not be touched: result=%+v closed=%v comments=%v labels=%v", res, obs.closed, obs.comments, obs.labels)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (held PR counted as skipped)", res.Skipped)
	}
}

func TestSweepSupersededOpenPRs_DoNotMergeContributorPRIsLeftAlone(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 15, Title: "fix thing", Body: "Refs #6", Author: "contributor", Files: []string{"pkg/a.go"}, Labels: []string{"do-not-merge"}},
			{Number: 25, Title: "landed", Body: "Fixes #6", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 6, State: "closed", GraphQLCloserPR: 25}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{CloseContributorPRs: true, ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Closed) != 0 || len(res.Commented) != 0 || len(obs.closed) != 0 || len(obs.comments[15]) != 0 || len(obs.labels[15]) != 0 {
		t.Fatalf("do-not-merge contributor PR must not be touched: result=%+v closed=%v comments=%v labels=%v", res, obs.closed, obs.comments, obs.labels)
	}
}

func TestSweepSupersededOpenPRs_HivePauseLabelIsLeftAlone(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := supersessionSweepServer(t, org, repo,
		[]supersessionPRFixture{
			{Number: 16, Title: "fix thing", Body: "Refs #7", Author: "hive-app[bot]", Files: []string{"pkg/a.go"}, Labels: []string{"hive-pause/abc123"}},
			{Number: 26, Title: "landed", Body: "Fixes #7", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
		},
		[]supersessionIssueFixture{{Number: 7, State: "closed", GraphQLCloserPR: 26}},
	)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	c.SetHoldLabels([]string{CanonicalHiveHoldLabel("abc123")})
	res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	if len(res.Closed) != 0 || len(obs.closed) != 0 || len(obs.comments[16]) != 0 {
		t.Fatalf("hive-pause PR must not be touched: result=%+v closed=%v comments=%v", res, obs.closed, obs.comments)
	}
}

// A PR that closes an open issue is live work whatever happened to the other
// issues it mentions. #12322 (`Fixes #12117`, `Part of #12080`) was flagged
// needs-human because closed parent #12080 had been closed by merged #12242.
func TestSweepSupersededOpenPRs_OpenClosingClaimKeepsPRLive(t *testing.T) {
	org, repo := "hivecommons", "hive"
	for _, tc := range []struct {
		name   string
		body   string
		issues []supersessionIssueFixture
	}{
		{
			name:   "closing claim open, bare reference closed by another PR",
			body:   "Fixes #30\n\nPart of #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "open"}, {Number: 31, State: "closed", GraphQLCloserPR: 40}},
		},
		{
			name:   "two closing claims, one still open",
			body:   "Fixes #30\nCloses #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "open"}, {Number: 31, State: "closed", GraphQLCloserPR: 40}},
		},
		{
			name:   "bare references only, one still open",
			body:   "Refs #30\nRefs #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "open"}, {Number: 31, State: "closed", GraphQLCloserPR: 40}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 20, Title: "quadrant drill-down", Body: tc.body, Author: "hive-app[bot]", Files: []string{"pkg/a.go"}},
					{Number: 40, Title: "landed parent", Body: "Fixes #31", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
				},
				tc.issues,
			)
			defer server.Close()

			c := newTestClient(t, server, org, []string{repo})
			c.SetAppBotLogin("hive-app[bot]")
			for _, level := range []int{acmmLevelAutonomous - 1, acmmLevelAutonomous} {
				res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return level }})
				if err != nil {
					t.Fatalf("SweepSupersededOpenPRs: %v", err)
				}
				if len(res.Commented) != 0 || len(res.Closed) != 0 || res.Skipped != 1 {
					t.Fatalf("level %d: live PR must not be escalated: result=%+v", level, res)
				}
				if len(obs.comments[20]) != 0 || len(obs.labels[20]) != 0 || len(obs.closed) != 0 {
					t.Fatalf("level %d: live PR touched: comments=%v labels=%v closed=%v", level, obs.comments[20], obs.labels[20], obs.closed)
				}
			}
		})
	}
}

// With closing claims present, bare references are context only: a closed
// closing target supersedes the PR even while a `Part of` parent stays open,
// and a PR whose closing targets are all closed by other merged PRs escalates.
func TestSweepSupersededOpenPRs_AllClosingClaimsClosedEscalates(t *testing.T) {
	org, repo := "hivecommons", "hive"
	for _, tc := range []struct {
		name   string
		body   string
		issues []supersessionIssueFixture
	}{
		{
			name:   "closing claim closed, bare reference still open",
			body:   "Fixes #30\n\nPart of #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "closed", GraphQLCloserPR: 40}, {Number: 31, State: "open"}},
		},
		{
			name:   "closing claim and bare reference both closed",
			body:   "Fixes #30\n\nPart of #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "closed", GraphQLCloserPR: 40}, {Number: 31, State: "closed", GraphQLCloserPR: 40}},
		},
		{
			name:   "two closing claims both closed",
			body:   "Fixes #30\nCloses #31",
			issues: []supersessionIssueFixture{{Number: 30, State: "closed", GraphQLCloserPR: 40}, {Number: 31, State: "closed", GraphQLCloserPR: 40}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 21, Title: "fix thing", Body: tc.body, Author: "hive-app[bot]", Files: []string{"pkg/a.go"}},
					{Number: 40, Title: "landed", Body: "Fixes #30", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
				},
				tc.issues,
			)
			defer server.Close()

			c, rec := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous - 1 }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			if len(res.Commented) != 1 || res.Commented[0].Issue != 30 || res.Commented[0].CloserPR != 40 || res.Commented[0].Action != SupersessionActionEscalated {
				t.Fatalf("expected needs-human escalation on #30 via #40: %+v", res)
			}
			if got := obs.labels[21]; len(got) != 2 || got[0] != issueNeedsHumanLabel {
				t.Fatalf("labels on #21 = %v, want needs-human", got)
			}
			if got := parkedPRs(rec); len(got) != 1 || got[0] != 21 {
				t.Fatalf("Parked facts = %v, want #21 parked at 085", got)
			}
		})
	}
}

// Once the escalation comment is on the PR, a later cycle must not re-apply
// needs-human after a human or hive cleared it: #12322 got the label back 20
// minutes after it was cleared. A changed closer or reason is a new
// escalation and labels again.
func TestSweepSupersededOpenPRs_DoesNotReapplyNeedsHumanAfterClear(t *testing.T) {
	org, repo := "hivecommons", "hive"
	// newTestClient registers the repo by short name, so the sweep's display
	// repo is "hive" and the GraphQL closer resolves to org/repo.
	closer := supersessionCloser{Repo: org + "/" + repo, Number: 40, URL: fmt.Sprintf("https://github.com/%s/%s/pull/40", org, repo), MergedAt: time.Date(2026, 10, 10, 11, 40, 0, 0, time.UTC)}
	event := SupersessionSweepEvent{Repo: repo, Number: 22, Author: "hive-app[bot]", IssueRepo: repo, Issue: 30, CloserPR: 40}
	const belowL6 = "this hive is below ACMM L6, so the sweep will not close or update PRs automatically. A human should decide whether to close this PR."
	sameBody := renderSupersessionComment(event, closer, belowL6)
	staleBody := renderSupersessionComment(event, supersessionCloser{Repo: closer.Repo, Number: 39, URL: "https://github.com/x/y/pull/39"}, belowL6)

	for _, tc := range []struct {
		name       string
		existing   string
		wantLabel  bool
		wantEdited bool
	}{
		{name: "identical escalation already posted", existing: sameBody, wantLabel: false},
		{name: "escalation for a different closer", existing: staleBody, wantLabel: true, wantEdited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 22, Title: "fix thing", Body: "Fixes #30", Author: "hive-app[bot]", Files: []string{"pkg/a.go"},
						Comments: []supersessionCommentFixture{{ID: 501, Body: tc.existing, Author: "hive-app[bot]", Bot: true, CreatedAt: time.Now().Add(-time.Hour)}}},
					{Number: 40, Title: "landed", Body: "Fixes #30", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
				},
				[]supersessionIssueFixture{{Number: 30, State: "closed", GraphQLCloserPR: 40}},
			)
			defer server.Close()

			c, rec := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous - 1 }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			if parked := parkedPRs(rec); (len(parked) == 1) != tc.wantLabel {
				t.Fatalf("Parked facts = %v, want parked=%v", parked, tc.wantLabel)
			}
			if len(obs.comments[22]) != 0 {
				t.Fatalf("marker comment must be edited in place, not duplicated: %v", obs.comments[22])
			}
			if _, edited := obs.edits[501]; edited != tc.wantEdited {
				t.Fatalf("edited = %v, want %v (edit body: %q)", edited, tc.wantEdited, obs.edits[501])
			}
			if tc.wantLabel {
				if len(res.Commented) != 1 || len(obs.labels[22]) != 2 || obs.labels[22][0] != issueNeedsHumanLabel {
					t.Fatalf("new escalation must label: result=%+v labels=%v", res, obs.labels[22])
				}
				return
			}
			if len(res.Commented) != 0 || res.Skipped != 1 || len(obs.labels[22]) != 0 {
				t.Fatalf("unchanged escalation must not re-label: result=%+v labels=%v", res, obs.labels[22])
			}
		})
	}
}

func TestSupersessionCandidateClaims(t *testing.T) {
	closingA := IssueClaim{Repo: "o/r", Issue: 1, Closing: true}
	closingB := IssueClaim{Repo: "o/r", Issue: 2, Closing: true}
	ref := IssueClaim{Repo: "o/r", Issue: 3, Reference: true}
	branch := IssueClaim{Repo: "o/r", Issue: 4}
	if got := supersessionCandidateClaims([]IssueClaim{ref, closingA, branch, closingB}); len(got) != 2 || got[0].Issue != 1 || got[1].Issue != 2 {
		t.Fatalf("closing claims must win: %+v", got)
	}
	if got := supersessionCandidateClaims([]IssueClaim{ref, branch}); len(got) != 2 {
		t.Fatalf("without closing claims the weaker tiers stand in: %+v", got)
	}
	if got := supersessionCandidateClaims(nil); len(got) != 0 {
		t.Fatalf("nil in, empty out: %+v", got)
	}
}

func TestSweepSupersededOpenPRs_AuditsDetectionWithoutAutoClose(t *testing.T) {
	event := SupersessionSweepEvent{Repo: autoCloseRepo, Number: 12, Author: "contributor", IssueRepo: autoCloseRepo, Issue: 3, CloserPR: 22}
	closer := supersessionCloser{Repo: event.IssueRepo, Number: 22, URL: "https://github.com/" + autoCloseOrg + "/" + autoCloseRepo + "/pull/22"}
	body := renderSupersessionComment(event, closer, supersessionDecisionNotice(autoCloseOrg, closer, ""))
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-notice=%t", existing), func(t *testing.T) {
			pr := supersessionPRFixture{}
			if existing {
				pr.Comments = []supersessionCommentFixture{noticeComment(body, time.Now().Add(-48*time.Hour))}
			}
			var events []SupersessionSweepEvent
			res, obs := runAutoCloseSweep(t, pr, SupersessionSweepOptions{
				Audit: func(e SupersessionSweepEvent) { events = append(events, e) },
			})
			if len(events) == 0 || events[0].Action != "detected-contributor" || events[0].Number != 12 || events[0].CloserPR != 22 {
				t.Fatalf("missing detection: %+v", events)
			}
			if events[0].Outcome != SupersessionOutcomeSubset || len(events[0].ExtraFiles) != 0 {
				t.Fatalf("subset detection outcome = %q %v, want %q", events[0].Outcome, events[0].ExtraFiles, SupersessionOutcomeSubset)
			}
			if len(res.Closed) != 0 || len(obs.closed) != 0 || len(obs.labels[12]) != 0 {
				t.Fatalf("detection must not close or label: result=%+v observations=%+v", res, obs)
			}
			if existing && (len(events) != 1 || len(res.Commented) != 0 || len(obs.comments[12]) != 0 || len(obs.edits) != 0) {
				t.Fatalf("existing notice should only audit detection: events=%+v result=%+v observations=%+v", events, res, obs)
			}
		})
	}
}

func TestSweepSupersededOpenPRs_KeptOpenDetection(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var events []SupersessionSweepEvent
		res, _ := runAutoCloseSweep(t, supersessionPRFixture{Labels: []string{"size/S", SupersessionKeepOpenLabel}}, SupersessionSweepOptions{
			CloseContributorPRs: enabled,
			Audit:               func(e SupersessionSweepEvent) { events = append(events, e) },
		})
		if len(res.Closed) != 0 || len(events) < 2 || events[0].Action != "detected-contributor" || events[1].Action != "kept-open" {
			t.Fatalf("auto-close=%t: result=%+v events=%+v", enabled, res, events)
		}
	}
}

func TestSupersessionDecisionNotice(t *testing.T) {
	closer := supersessionCloser{Repo: "o/r", Number: 334}
	cases := []struct {
		name, operator, note string
		want                 []string
		reject               []string
	}{
		{"duplicate", "owner", "", []string{"**Decision needed (@owner):**", "duplicate #334", "**close as superseded**", "file names, not contents"}, []string{"did not close it automatically"}},
		{"duplicate with keep reason", "owner", "the author replied", []string{"**Decision needed (@owner):**", "**close as superseded**", "did not close it automatically: the author replied."}, nil},
		{"extra files", "owner", supersessionReasonExtraFiles, []string{"**Decision needed (@owner):**", supersessionReasonExtraFiles, "**keep open**"}, []string{"close as superseded**"}},
		{"kept open", "owner", supersessionReasonKeptOpen, []string{"An operator chose to keep this PR open", SupersessionKeepOpenLabel, "#334"}, []string{"Decision needed", "Recommended", "@owner"}},
		{"no operator", "", "", []string{"**Decision needed (maintainers):**"}, []string{"@"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := supersessionDecisionNotice(tc.operator, closer, tc.note)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("notice missing %q: %q", w, got)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(got, r) {
					t.Fatalf("notice must not contain %q: %q", r, got)
				}
			}
		})
	}
}

// TestSweepSupersededOpenPRs_ReleasesOwnParkWhenCauseIsGone covers
// hivecommons/hive#14155: the sweep's `needs-human` clears with its cause.
// #13959 and #14090 sat at stage/085-needs-decision, verdict approved and CI
// green, for a park the sweep had put on a `Part of` / `Refs` link to a
// closed tracker, until a maintainer removed the label by hand.
func TestSweepSupersededOpenPRs_ReleasesOwnParkWhenCauseIsGone(t *testing.T) {
	org, repo := "hivecommons", "hive"
	closer := supersessionCloser{Repo: org + "/" + repo, Number: 22, URL: fmt.Sprintf("https://github.com/%s/%s/pull/22", org, repo)}
	escalation := func() string {
		event := SupersessionSweepEvent{Repo: repo, Number: 12, Author: "contributor", IssueRepo: repo, Issue: 3, CloserPR: 22}
		return renderSupersessionComment(event, closer, supersessionDecisionNotice(org, closer, supersessionReasonExtraFiles)+"\n\nFiles the merged PR did not touch: `pkg/c.go`.")
	}
	hiveApplied := []supersessionLabelEventFixture{{Event: "labeled", Label: issueNeedsHumanLabel, Actor: "hive-app[bot]"}}
	personApplied := []supersessionLabelEventFixture{{Event: "labeled", Label: issueNeedsHumanLabel, Actor: "maintainer"}}
	notice := func(body string) []supersessionCommentFixture {
		return []supersessionCommentFixture{{ID: 900, Body: body, Author: "hive-app[bot]", Bot: true, CreatedAt: time.Now().Add(-time.Hour)}}
	}

	for _, tc := range []struct {
		name        string
		body        string
		issueState  string
		labels      []string
		labelEvents []supersessionLabelEventFixture
		comments    []supersessionCommentFixture
		wantRelease bool
		wantPark    bool
	}{
		{
			name: "claimed issue reopened: hive park released",
			body: "Fixes #3", issueState: "open",
			labels: []string{issueNeedsHumanLabel}, labelEvents: hiveApplied, comments: notice(escalation()),
			wantRelease: true,
		},
		{
			name: "claimed issue reopened: a person's needs-human stays",
			body: "Fixes #3", issueState: "open",
			labels: []string{issueNeedsHumanLabel}, labelEvents: personApplied, comments: notice(escalation()),
		},
		{
			name: "claimed issue reopened: needs-human with no sweep notice stays",
			body: "Fixes #3", issueState: "open",
			labels: []string{issueNeedsHumanLabel}, labelEvents: hiveApplied,
		},
		{
			name: "claimed issue reopened: sweep notice that parked nothing leaves the label",
			body: "Fixes #3", issueState: "open",
			labels: []string{issueNeedsHumanLabel}, labelEvents: hiveApplied,
			comments: notice(renderSupersessionComment(SupersessionSweepEvent{Repo: repo, Number: 12, IssueRepo: repo, Issue: 3, CloserPR: 22}, closer, supersessionDecisionNotice(org, closer, supersessionReasonKeptOpen))),
		},
		{
			name: "claimed issue reopened: no needs-human, nothing to do",
			body: "Fixes #3", issueState: "open",
			comments: notice(escalation()),
		},
		{
			name: "reference-only link still closed: stale park from an earlier escalation released",
			body: "Refs #3", issueState: "closed",
			labels: []string{issueNeedsHumanLabel}, labelEvents: hiveApplied, comments: notice(escalation()),
			wantRelease: true,
		},
		{
			name: "reference-only link still closed: a person's needs-human stays",
			body: "Part of #3", issueState: "closed",
			labels: []string{issueNeedsHumanLabel}, labelEvents: personApplied, comments: notice(escalation()),
		},
		{
			name: "closing claim still closed: park stands",
			body: "Fixes #3", issueState: "closed",
			labels: []string{issueNeedsHumanLabel}, labelEvents: hiveApplied, comments: notice(escalation()),
			wantPark: true,
		},
		{
			name: "closing claim still closed, not yet parked: escalates",
			body: "Closes #3", issueState: "closed",
			wantPark: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 12, Title: "fix thing", Body: tc.body, Author: "contributor", Files: []string{"pkg/a.go", "pkg/c.go"},
						Labels: tc.labels, LabelEvents: tc.labelEvents, Comments: tc.comments},
					{Number: 22, Title: "landed", Body: "Fixes #3", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
				},
				[]supersessionIssueFixture{{Number: 3, State: tc.issueState, GraphQLCloserPR: 22}},
			)
			defer server.Close()

			c, rec := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			removed := obs.removedLabels[12]
			if tc.wantRelease {
				if len(removed) != 1 || removed[0] != issueNeedsHumanLabel {
					t.Fatalf("removed labels = %v, want the sweep's needs-human released", removed)
				}
				if len(res.Resolved) != 1 || res.Resolved[0].Action != SupersessionActionReleased || res.Resolved[0].Number != 12 {
					t.Fatalf("result = %+v, want one %q", res, SupersessionActionReleased)
				}
			} else if len(removed) != 0 || len(res.Resolved) != 0 {
				t.Fatalf("label must stay: removed=%v result=%+v", removed, res)
			}
			if parked := parkedPRs(rec); (len(parked) == 1) != tc.wantPark {
				t.Fatalf("Parked facts = %v, want parked=%v", parked, tc.wantPark)
			}
			if len(obs.closed) != 0 || len(obs.comments[12])+len(obs.edits) > 1 {
				t.Fatalf("sweep wrote too much: closed=%v comments=%v edits=%v", obs.closed, obs.comments[12], obs.edits)
			}
		})
	}
}

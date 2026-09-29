package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// questionCommentFixture is one comment on the fixture issue.
type questionCommentFixture struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	MinusOne  int
}

type questionIssueFixture struct {
	Number   int
	Author   string
	Labels   []string
	Comments []questionCommentFixture
	// IsPR marks the fixture as a pull request in the issues listing, which
	// the sweep must skip.
	IsPR bool
	// CommentsStatus, when non-zero, makes the comments listing for this
	// issue fail with that HTTP status.
	CommentsStatus int
}

// questionSweepServerOpts injects failures and pagination into the fixture
// server so tests can exercise the sweep's error-skip and paging branches.
type questionSweepServerOpts struct {
	failEditCommentIDs map[int64]bool
	failReactionIDs    map[int64]bool
	failLabelIssues    map[int]bool
	failCloseIssues    map[int]bool
	// paginate serves list responses one element per page with a rel="next"
	// Link header, so every pagination loop in the sweep takes a second lap.
	paginate bool
}

type questionAutoCloseObservations struct {
	mu             sync.Mutex
	closed         []int
	stateReasons   map[int]string
	labels         map[int][]string
	editedComments map[int64]string
}

func questionAutoCloseSweepServer(t *testing.T, org, repo string, issues []questionIssueFixture, downvoters map[int64][]string, extra ...questionSweepServerOpts) (*httptest.Server, *questionAutoCloseObservations) {
	t.Helper()
	var o questionSweepServerOpts
	if len(extra) > 0 {
		o = extra[0]
	}
	obs := &questionAutoCloseObservations{
		stateReasons:   map[int]string{},
		labels:         map[int][]string{},
		editedComments: map[int64]string{},
	}
	issueByNumber := map[int]*questionIssueFixture{}
	for i := range issues {
		issueByNumber[issues[i].Number] = &issues[i]
	}

	// requestPage parses the page query parameter, defaulting to 1.
	requestPage := func(r *http.Request) int {
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
			return p
		}
		return 1
	}
	// slicePage serves one element per page with a rel="next" Link header
	// when pagination is enabled; otherwise it returns 0..n unchanged.
	slicePage := func(w http.ResponseWriter, r *http.Request, n int) (int, int) {
		if !o.paginate || n == 0 {
			return 0, n
		}
		page := requestPage(r)
		if page > n {
			return n, n
		}
		if page < n {
			w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, page+1))
		}
		return page - 1, page
	}

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labelFilter := r.URL.Query().Get("labels")
		wire := []map[string]any{}
		for _, issue := range issues {
			if labelFilter != "" && !hasLabel(issue.Labels, labelFilter) {
				continue
			}
			labels := []map[string]any{}
			for _, l := range issue.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			entry := map[string]any{
				"number": issue.Number,
				"user":   map[string]any{"login": issue.Author},
				"labels": labels,
			}
			if issue.IsPR {
				entry["pull_request"] = map[string]any{"url": "https://example.invalid/pr"}
			}
			wire = append(wire, entry)
		}
		lo, hi := slicePage(w, r, len(wire))
		_ = json.NewEncoder(w).Encode(wire[lo:hi])
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo))
		if strings.HasPrefix(rest, "comments/") {
			idStr := strings.TrimSuffix(strings.TrimPrefix(rest, "comments/"), "/reactions")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			if strings.HasSuffix(rest, "/reactions") {
				if o.failReactionIDs[id] {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				// []any so a fixture can inject a JSON null reaction (an
				// empty login), which the sweep must skip over.
				wire := []any{}
				for _, login := range downvoters[id] {
					if login == "" {
						wire = append(wire, nil)
						continue
					}
					wire = append(wire, map[string]any{"content": "-1", "user": map[string]any{"login": login}})
				}
				lo, hi := slicePage(w, r, len(wire))
				_ = json.NewEncoder(w).Encode(wire[lo:hi])
				return
			}
			if r.Method == "PATCH" {
				if o.failEditCommentIDs[id] {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var payload struct {
					Body *string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				obs.mu.Lock()
				if payload.Body != nil {
					obs.editedComments[id] = *payload.Body
				}
				obs.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": payload.Body})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		parts := strings.Split(rest, "/")
		n, _ := strconv.Atoi(parts[0])
		switch {
		case len(parts) == 1 && r.Method == "PATCH":
			if o.failCloseIssues[n] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var payload struct {
				State       *string `json:"state"`
				StateReason *string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			obs.mu.Lock()
			if payload.State != nil && *payload.State == "closed" {
				obs.closed = append(obs.closed, n)
			}
			if payload.StateReason != nil {
				obs.stateReasons[n] = *payload.StateReason
			}
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "closed"})
		case len(parts) == 2 && parts[1] == "comments" && r.Method == "GET":
			issue := issueByNumber[n]
			if issue != nil && issue.CommentsStatus != 0 {
				w.WriteHeader(issue.CommentsStatus)
				return
			}
			wire := []map[string]any{}
			if issue != nil {
				for _, c := range issue.Comments {
					body := c.Body
					obs.mu.Lock()
					if edited, ok := obs.editedComments[c.ID]; ok {
						body = edited
					}
					obs.mu.Unlock()
					wire = append(wire, map[string]any{
						"id":         c.ID,
						"user":       map[string]any{"login": c.Author},
						"body":       body,
						"created_at": c.CreatedAt.Format(time.RFC3339),
						"reactions":  map[string]any{"-1": c.MinusOne},
					})
				}
			}
			lo, hi := slicePage(w, r, len(wire))
			_ = json.NewEncoder(w).Encode(wire[lo:hi])
		case len(parts) == 2 && parts[1] == "labels" && r.Method == "POST":
			if o.failLabelIssues[n] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			obs.mu.Lock()
			obs.labels[n] = append(obs.labels[n], labels...)
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux), obs
}

func TestSweepAnsweredQuestions_MarksNewestHiveAnswer(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 1,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 101, Author: "hive-app[bot]", Body: "Yes, X is supported.", CreatedAt: time.Now().Add(-time.Minute)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: DefaultQuestionAutoCloseWindow})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Marked) != 1 || res.Marked[0].Number != 1 {
		t.Fatalf("want issue #1 marked, got %+v", res)
	}
	body := obs.editedComments[101]
	if !strings.Contains(body, questionAutoCloseMarker) || !strings.Contains(body, "👎") {
		t.Fatalf("marked comment missing marker/footer: %q", body)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("issue should not close on the same tick it is first marked: closed=%v", obs.closed)
	}
}

func TestSweepAnsweredQuestions_ClosesAfterDeadlineWithNoObjection(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 2,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 201, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 1 || len(obs.closed) != 1 || obs.closed[0] != 2 {
		t.Fatalf("want issue #2 closed, got result=%+v wire=%v", res, obs.closed)
	}
	if obs.stateReasons[2] != "completed" {
		t.Fatalf("close reason = %q, want completed", obs.stateReasons[2])
	}
}

func TestSweepAnsweredQuestions_WaitsBeforeDeadline(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 3,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 301, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-1 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("issue should still be waiting for the deadline: result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_AuthorDownvoteRelabelsInsteadOfClosing(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 4,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 401, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour), MinusOne: 1},
			},
		},
	}, map[int64][]string{401: {"asker"}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Relabeled) != 1 || len(obs.labels[4]) == 0 || obs.labels[4][0] != issueNeedsHumanLabel {
		t.Fatalf("want issue #4 relabeled needs-human, got result=%+v labels=%v", res, obs.labels)
	}
	if len(res.Closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("an author objection must never close the issue: result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_NonAuthorDownvoteDoesNotBlockClose(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 5,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 501, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour), MinusOne: 1},
			},
		},
	}, map[int64][]string{501: {"bystander"}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 1 || len(obs.closed) != 1 {
		t.Fatalf("only the issue author's 👎 should keep this open: result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_FollowUpCommentCancelsSchedule(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 6,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 601, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
				{ID: 602, Author: "asker", Body: "Actually, one more thing...", CreatedAt: time.Now().Add(-4 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 0 || len(res.Marked) != 0 || len(res.Relabeled) != 0 {
		t.Fatalf("a follow-up comment after the hive's answer must cancel the schedule: result=%+v", res)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("issue must stay open once someone replied after the hive's answer: closed=%v", obs.closed)
	}
}

func TestSweepAnsweredQuestions_HeldIssueIsSkipped(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 7,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel, "hold"},
			Comments: []questionCommentFixture{
				{ID: 701, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("a held question must never be auto-closed: result=%+v wire=%v", res, obs.closed)
	}
}

func TestQuestionAutoCloseWindow_EnvOverride(t *testing.T) {
	t.Setenv(questionAutoCloseWindowEnv, "2")
	if got := QuestionAutoCloseWindow(); got != 2*time.Hour {
		t.Fatalf("QuestionAutoCloseWindow() = %v, want 2h", got)
	}
	t.Setenv(questionAutoCloseWindowEnv, "")
	if got := QuestionAutoCloseWindow(); got != DefaultQuestionAutoCloseWindow {
		t.Fatalf("QuestionAutoCloseWindow() with unset env = %v, want default %v", got, DefaultQuestionAutoCloseWindow)
	}
	t.Setenv(questionAutoCloseWindowEnv, "not-a-duration")
	if got := QuestionAutoCloseWindow(); got != DefaultQuestionAutoCloseWindow {
		t.Fatalf("QuestionAutoCloseWindow() with garbage env = %v, want default %v", got, DefaultQuestionAutoCloseWindow)
	}
}

func TestQuestionAutoCloseWindow_DurationStringAndNegative(t *testing.T) {
	t.Setenv(questionAutoCloseWindowEnv, "30m")
	if got := QuestionAutoCloseWindow(); got != 30*time.Minute {
		t.Fatalf("QuestionAutoCloseWindow() = %v, want 30m", got)
	}
	t.Setenv(questionAutoCloseWindowEnv, "-2")
	if got := QuestionAutoCloseWindow(); got != DefaultQuestionAutoCloseWindow {
		t.Fatalf("QuestionAutoCloseWindow() with negative env = %v, want default %v", got, DefaultQuestionAutoCloseWindow)
	}
	t.Setenv(questionAutoCloseWindowEnv, "0")
	if got := QuestionAutoCloseWindow(); got != DefaultQuestionAutoCloseWindow {
		t.Fatalf("QuestionAutoCloseWindow() with zero env = %v, want default %v", got, DefaultQuestionAutoCloseWindow)
	}
}

func TestSweepAnsweredQuestions_NilClient(t *testing.T) {
	var c *Client
	if _, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{}); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client error = %v, want ErrNoGitHubClient", err)
	}
}

func TestSweepAnsweredQuestions_ZeroWindowFallsBackToEnv(t *testing.T) {
	t.Setenv(questionAutoCloseWindowEnv, "1")
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 30,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3001, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-2 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Closed) != 1 || len(obs.closed) != 1 {
		t.Fatalf("zero Window must fall back to the env window (1h < 2h answer age): result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_MaxActionsCapsAcrossRepos(t *testing.T) {
	org, repo := "hivecommons", "hive"
	now := time.Now()
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 31,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3101, Author: "hive-app[bot]", Body: "First answer.", CreatedAt: now.Add(-time.Minute)},
			},
		},
		{
			Number: 32,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3201, Author: "hive-app[bot]", Body: "Second answer.", CreatedAt: now.Add(-time.Minute)},
			},
		},
	}, nil)
	defer server.Close()

	// The second repo has no fixture server routes at all: the budget is
	// spent on the first repo, so the sweep must break before ever listing it.
	c := newTestClient(t, server, org, []string{repo, "unlisted"})
	c.SetAppBotLogin("hive-app[bot]")
	var audited []QuestionAutoCloseEvent
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{
		MaxActions: 1,
		Window:     DefaultQuestionAutoCloseWindow,
		Audit:      func(e QuestionAutoCloseEvent) { audited = append(audited, e) },
	})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Marked) != 1 || res.Seen != 1 {
		t.Fatalf("MaxActions=1 must stop after one action: %+v", res)
	}
	if len(audited) != 1 || audited[0].Action != "marked" {
		t.Fatalf("audit sink should have seen exactly the one action: %+v", audited)
	}
	if len(obs.editedComments) != 1 {
		t.Fatalf("exactly one comment should be marked: %v", obs.editedComments)
	}
}

func TestSweepAnsweredQuestions_ListIssuesErrorPropagates(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, _ := questionAutoCloseSweepServer(t, org, repo, nil, nil)
	defer server.Close()

	// The configured repo has no routes on the fixture server, so listing
	// its issues 404s and the sweep must surface that error.
	c := newTestClient(t, server, org, []string{"unlisted"})
	c.SetAppBotLogin("hive-app[bot]")
	if _, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: time.Hour}); err == nil {
		t.Fatal("want an error when the issue listing fails, got nil")
	}
}

func TestSweepAnsweredQuestions_SkipsPullRequests(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 33,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			IsPR:   true,
			Comments: []questionCommentFixture{
				{ID: 3301, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if res.Seen != 0 || len(obs.closed) != 0 {
		t.Fatalf("a kind/question PR must be ignored entirely: result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_CommentListErrorSkipsIssue(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number:         34,
			Author:         "asker",
			Labels:         []string{questionAutoCloseLabel},
			CommentsStatus: http.StatusInternalServerError,
		},
		{
			Number: 35,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3501, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
			},
		},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("a per-issue failure must not abort the sweep: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("the unreadable issue should be skipped: %+v", res)
	}
	if len(res.Closed) != 1 || len(obs.closed) != 1 || obs.closed[0] != 35 {
		t.Fatalf("the healthy issue must still be swept: result=%+v wire=%v", res, obs.closed)
	}
}

func TestSweepAnsweredQuestions_NoCommentsNoClock(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{Number: 36, Author: "asker", Labels: []string{questionAutoCloseLabel}},
	}, nil)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if res.actions() != 0 || len(obs.closed) != 0 || len(obs.editedComments) != 0 {
		t.Fatalf("an unanswered question has no clock to start: result=%+v", res)
	}
}

func TestSweepAnsweredQuestions_MarkEditErrorSkipsIssue(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 37,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3701, Author: "hive-app[bot]", Body: "Answer.", CreatedAt: time.Now().Add(-time.Minute)},
			},
		},
	}, nil, questionSweepServerOpts{failEditCommentIDs: map[int64]bool{3701: true}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("a failed comment edit must not abort the sweep: %v", err)
	}
	if res.Skipped != 1 || len(res.Marked) != 0 || len(obs.editedComments) != 0 {
		t.Fatalf("the issue whose mark failed should be skipped: %+v", res)
	}
}

func TestSweepAnsweredQuestions_ReactionsErrorSkipsIssue(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 38,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 3801, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour), MinusOne: 1},
			},
		},
	}, nil, questionSweepServerOpts{failReactionIDs: map[int64]bool{3801: true}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("a failed reactions listing must not abort the sweep: %v", err)
	}
	if res.Skipped != 1 || len(obs.closed) != 0 {
		t.Fatalf("an unreadable objection check must skip, never close: %+v", res)
	}
}

func TestSweepAnsweredQuestions_ObjectionAlreadyRelabeledIsIdempotent(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 39,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel, issueNeedsHumanLabel},
			Comments: []questionCommentFixture{
				{ID: 3901, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour), MinusOne: 1},
			},
		},
	}, map[int64][]string{3901: {"asker"}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Relabeled) != 0 || len(obs.labels[39]) != 0 || len(obs.closed) != 0 {
		t.Fatalf("an already-relabeled objection must not act again: result=%+v labels=%v", res, obs.labels)
	}
}

func TestSweepAnsweredQuestions_RelabelErrorSkipsIssue(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 40,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 4001, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour), MinusOne: 1},
			},
		},
	}, map[int64][]string{4001: {"asker"}}, questionSweepServerOpts{failLabelIssues: map[int]bool{40: true}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("a failed relabel must not abort the sweep: %v", err)
	}
	if res.Skipped != 1 || len(res.Relabeled) != 0 || len(obs.closed) != 0 {
		t.Fatalf("a failed relabel must skip, never close: %+v", res)
	}
}

func TestSweepAnsweredQuestions_CloseErrorSkipsIssue(t *testing.T) {
	org, repo := "hivecommons", "hive"
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 41,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 4101, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: time.Now().Add(-5 * time.Hour)},
			},
		},
	}, nil, questionSweepServerOpts{failCloseIssues: map[int]bool{41: true}})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("a failed close must not abort the sweep: %v", err)
	}
	if res.Skipped != 1 || len(res.Closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("a failed close call must be recorded as a skip: %+v", res)
	}
}

func TestSweepAnsweredQuestions_PaginatesIssuesCommentsAndReactions(t *testing.T) {
	org, repo := "hivecommons", "hive"
	now := time.Now()
	server, obs := questionAutoCloseSweepServer(t, org, repo, []questionIssueFixture{
		{
			Number: 42,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 4201, Author: "asker", Body: "How do I X?", CreatedAt: now.Add(-6 * time.Hour)},
				// Two -1 reactions: a JSON-null placeholder on page one and
				// the author's own on page two.
				{ID: 4202, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: now.Add(-5 * time.Hour), MinusOne: 2},
			},
		},
		{
			Number: 43,
			Author: "asker",
			Labels: []string{questionAutoCloseLabel},
			Comments: []questionCommentFixture{
				{ID: 4301, Author: "asker", Body: "How do I Y?", CreatedAt: now.Add(-6 * time.Hour)},
				{ID: 4302, Author: "hive-app[bot]", Body: "Answer." + questionAutoCloseFooter, CreatedAt: now.Add(-5 * time.Hour)},
			},
		},
	}, map[int64][]string{4202: {"", "asker"}}, questionSweepServerOpts{paginate: true})
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin("hive-app[bot]")
	res, err := c.SweepAnsweredQuestions(context.Background(), QuestionAutoCloseOptions{Window: 4 * time.Hour})
	if err != nil {
		t.Fatalf("SweepAnsweredQuestions: %v", err)
	}
	if len(res.Relabeled) != 1 || res.Relabeled[0].Number != 42 || len(obs.labels[42]) == 0 {
		t.Fatalf("paginated objection lookup should relabel #42: result=%+v labels=%v", res, obs.labels)
	}
	if len(res.Closed) != 1 || res.Closed[0].Number != 43 || len(obs.closed) != 1 || obs.closed[0] != 43 {
		t.Fatalf("paginated listings should still close #43: result=%+v wire=%v", res, obs.closed)
	}
}

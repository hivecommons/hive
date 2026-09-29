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
}

type questionAutoCloseObservations struct {
	mu             sync.Mutex
	closed         []int
	stateReasons   map[int]string
	labels         map[int][]string
	editedComments map[int64]string
}

func questionAutoCloseSweepServer(t *testing.T, org, repo string, issues []questionIssueFixture, downvoters map[int64][]string) (*httptest.Server, *questionAutoCloseObservations) {
	t.Helper()
	obs := &questionAutoCloseObservations{
		stateReasons:   map[int]string{},
		labels:         map[int][]string{},
		editedComments: map[int64]string{},
	}
	issueByNumber := map[int]*questionIssueFixture{}
	for i := range issues {
		issueByNumber[issues[i].Number] = &issues[i]
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
			wire = append(wire, map[string]any{
				"number": issue.Number,
				"user":   map[string]any{"login": issue.Author},
				"labels": labels,
			})
		}
		_ = json.NewEncoder(w).Encode(wire)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo))
		if strings.HasPrefix(rest, "comments/") {
			idStr := strings.TrimSuffix(strings.TrimPrefix(rest, "comments/"), "/reactions")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			if strings.HasSuffix(rest, "/reactions") {
				wire := []map[string]any{}
				for _, login := range downvoters[id] {
					wire = append(wire, map[string]any{"content": "-1", "user": map[string]any{"login": login}})
				}
				_ = json.NewEncoder(w).Encode(wire)
				return
			}
			if r.Method == "PATCH" {
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
			_ = json.NewEncoder(w).Encode(wire)
		case len(parts) == 2 && parts[1] == "labels" && r.Method == "POST":
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

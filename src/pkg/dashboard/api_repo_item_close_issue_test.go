package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCloseIssueDecisionTargetsOnlySelectedIssue(t *testing.T) {
	for _, labels := range [][]string{
		nil, {"stage/045-needs-decision"}, {"stage/085-needs-decision", "needs-human"},
		{"hive/hold", "agent/scanner", "claimed"}, {"tracking", "triage/advisory"},
	} {
		t.Run(strings.Join(labels, ","), func(t *testing.T) {
			m := &repoItemDecisionMock{issueLabels: labels}
			s := newRepoItemDecisionServer(t, m)
			w := postRepoItemDecision(t, s, 7, "writer", `{"decision":"close-issue","type":"issue","comment":"Finished independently"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("close: %d %s", w.Code, w.Body.String())
			}
			if len(m.edits) != 1 || m.editPaths[0] != "/repos/testorg/testrepo/issues/7" ||
				m.edits[0]["state"] != "closed" || m.edits[0]["state_reason"] != issueStateReasonCompleted {
				t.Fatalf("must close only selected issue: paths=%v edits=%v", m.editPaths, m.edits)
			}
			if len(m.comments) != 1 || !strings.Contains(m.comments[0], "completed by @writer") || !strings.Contains(m.comments[0], "Finished independently") {
				t.Fatalf("closing attribution: %v", m.comments)
			}
			var resp repoItemDecisionResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Status != "closed" || resp.Result == nil || resp.Result.Action != rowActionClosed || resp.Result.Target.Kind != "issue" || resp.Result.Target.Number != 7 {
				t.Fatalf("result: %+v", resp)
			}
			if len(m.added) != 0 || len(m.merges) != 0 || len(m.reviews) != 0 {
				t.Fatal("issue close must not resolve a PR or move other items")
			}
		})
	}
}

func TestCloseIssueDecisionGuardsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, user, body, state string

		number, commentError, editError, want int

		paused bool
	}{
		{name: "anonymous", number: 7, want: http.StatusUnauthorized},
		{name: "read-only", user: "reader", number: 7, want: http.StatusForbidden},
		{name: "PR type", user: "writer", number: 8, body: `{"decision":"close-issue","type":"pr"}`, want: http.StatusBadRequest},
		{name: "PR disguised as issue", user: "writer", number: 8, want: http.StatusBadRequest},
		{name: "already closed", user: "writer", number: 7, state: "closed", want: http.StatusConflict},
		{name: "missing issue", user: "writer", number: 404, want: http.StatusNotFound},
		{name: "required attribution refused", user: "writer", number: 7, commentError: http.StatusForbidden, want: http.StatusBadGateway},
		{name: "remote close refused", user: "writer", number: 7, editError: http.StatusForbidden, want: http.StatusBadGateway},
		{name: "remote close unavailable", user: "writer", number: 7, editError: http.StatusServiceUnavailable, want: http.StatusBadGateway},
		{name: "repo paused", user: "writer", number: 7, paused: true, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &repoItemDecisionMock{issueState: tc.state, commentError: tc.commentError, editError: tc.editError}
			s := newRepoItemDecisionServer(t, m)
			if tc.paused {
				s.deps.Config.Project.PausedRepos = []config.RepoPause{{Repo: "testrepo", Reason: "read only"}}
			}
			body := tc.body
			if body == "" {
				body = `{"decision":"close-issue","type":"issue"}`
			}
			w := postRepoItemDecision(t, s, tc.number, tc.user, body)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if len(m.edits) != 0 || strings.Contains(w.Body.String(), `"status":"closed"`) {
				t.Fatalf("failure must not close or report closed: %v %s", m.edits, w.Body.String())
			}
			if tc.editError == 0 && len(m.comments) != 0 {
				t.Fatalf("guard failure posted a comment: %v", m.comments)
			}
			if tc.paused && !strings.Contains(w.Body.String(), `"action":"refused"`) {
				t.Fatalf("pause must explain refusal: %s", w.Body.String())
			}
		})
	}
}

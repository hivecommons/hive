package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// unparkWireIssue is the minimum of GitHub's issue shape the sweep reads.
type unparkWireIssue struct {
	Number   int                 `json:"number"`
	Title    string              `json:"title"`
	Body     string              `json:"body"`
	User     wireUser            `json:"user"`
	Labels   []wireLabel         `json:"labels"`
	Comments []unparkWireComment `json:"-"`
	// PullRequest marks the issue as a PR in the issues list; HeadSHA and
	// State feed the pulls/{n} endpoint (hivecommons/hive#13041).
	PullRequest *unparkWirePRLink `json:"pull_request,omitempty"`
	HeadSHA     string            `json:"-"`
	State       string            `json:"state,omitempty"`
}

type unparkWirePRLink struct {
	URL string `json:"url"`
}

type unparkWireComment struct {
	ID        int64    `json:"id"`
	Body      string   `json:"body"`
	User      wireUser `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
}

// unparkServer serves one repo's open issues, their comments and the
// permission endpoint, and records every write the sweep performs.
type unparkServer struct {
	mu sync.Mutex
	// posted maps issue number to the comment bodies POSTed on it.
	posted map[int][]string
	// edited maps comment ID to the body it was edited to.
	edited map[int64]string
	// added maps issue number to the labels added to it.
	added map[int][]string
	// removed maps issue number to the labels removed from it.
	removed map[int][]string
	// permissions maps login to the permission level the API reports.
	permissions map[string]string
	// roles maps login to the role_name the permission endpoint reports
	// (hivecommons/hive#11313); unset logins report none.
	roles map[string]string
	// orgMembers lists logins GET /orgs/{org}/members/{user} answers 204 for.
	orgMembers map[string]bool
	// timelines maps issue number to its label events; a missing issue
	// yields an empty timeline. timelineStatus, when non-zero, fails every
	// timeline read with that status.
	timelines      map[int][]unparkWireTimelineEvent
	timelineStatus int
	// removeStatus, when non-zero, fails every label DELETE with that status.
	removeStatus int
	// closedIssues is what GET /issues?state=closed answers; the plain
	// listing answers the open issues the server was built with.
	closedIssues []unparkWireIssue
	// patched maps issue number to the state_reason of each PATCH that
	// closed it.
	patched map[int][]string
	// register adds an issue GET /issues/{n} answers for without listing
	// it as open — a closed issue the closed-issue sweep finds via
	// ?state=closed.
	register func(unparkWireIssue)
	// orgLookups counts membership lookups, permLookups permission lookups.
	orgLookups, permLookups, timelineReads int
}

type unparkWireTimelineEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Actor     wireUser  `json:"actor"`
	Label     wireLabel `json:"label"`
	CreatedAt string    `json:"created_at"`
}

func labeledAt(id int64, actor, label string, at time.Time) unparkWireTimelineEvent {
	return unparkWireTimelineEvent{ID: id, Event: "labeled", Actor: wireUser{Login: actor}, Label: wireLabel{Name: label}, CreatedAt: at.UTC().Format(time.RFC3339)}
}

func unlabeledAt(id int64, actor, label string, at time.Time) unparkWireTimelineEvent {
	ev := labeledAt(id, actor, label, at)
	ev.Event = "unlabeled"
	return ev
}

func newUnparkServer(t *testing.T, owner, repo string, issues []unparkWireIssue, permissions map[string]string) (*httptest.Server, *unparkServer) {
	t.Helper()
	rec := &unparkServer{
		posted:      map[int][]string{},
		edited:      map[int64]string{},
		added:       map[int][]string{},
		removed:     map[int][]string{},
		patched:     map[int][]string{},
		permissions: permissions,
	}
	byNum := map[int]unparkWireIssue{}
	for _, issue := range issues {
		byNum[issue.Number] = issue
	}
	rec.register = func(issue unparkWireIssue) { byNum[issue.Number] = issue }

	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.orgLookups++
		login := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if rec.orgMembers[login] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	base := fmt.Sprintf("/repos/%s/%s/", owner, repo)
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, base)
		rec.mu.Lock()
		defer rec.mu.Unlock()

		switch {
		case path == "issues" && r.Method == http.MethodGet && r.URL.Query().Get("state") == "closed":
			closed := rec.closedIssues
			if closed == nil {
				closed = []unparkWireIssue{}
			}
			_ = json.NewEncoder(w).Encode(closed)

		case path == "issues" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(issues)

		case strings.HasPrefix(path, "issues/") && !strings.Contains(strings.TrimPrefix(path, "issues/"), "/") && r.Method == http.MethodPatch:
			var n int
			fmt.Sscanf(path, "issues/%d", &n)
			issue, ok := byNum[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.State != "" {
				issue.State = body.State
				byNum[n] = issue
			}
			rec.patched[n] = append(rec.patched[n], body.StateReason)
			_ = json.NewEncoder(w).Encode(issue)

		case strings.HasPrefix(path, "issues/") && !strings.Contains(strings.TrimPrefix(path, "issues/"), "/") && r.Method == http.MethodGet:
			var n int
			fmt.Sscanf(path, "issues/%d", &n)
			issue, ok := byNum[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(issue)

		case strings.HasPrefix(path, "pulls/") && r.Method == http.MethodGet:
			var n int
			fmt.Sscanf(path, "pulls/%d", &n)
			issue, ok := byNum[n]
			if !ok || issue.PullRequest == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			state := issue.State
			if state == "" {
				state = "open"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": n, "state": state, "title": issue.Title, "body": issue.Body,
				"user": issue.User, "labels": issue.Labels,
				"head": map[string]any{"sha": issue.HeadSHA},
			})

		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodGet:
			var n int
			fmt.Sscanf(path, "issues/%d/comments", &n)
			_ = json.NewEncoder(w).Encode(byNum[n].Comments)

		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodPost:
			var n int
			fmt.Sscanf(path, "issues/%d/comments", &n)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.posted[n] = append(rec.posted[n], body.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9000 + len(rec.posted[n])})

		case strings.HasPrefix(path, "issues/comments/") && r.Method == http.MethodPatch:
			var id int64
			fmt.Sscanf(path, "issues/comments/%d", &id)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.edited[id] = body.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})

		case strings.HasSuffix(path, "/labels") && r.Method == http.MethodPost:
			var n int
			fmt.Sscanf(path, "issues/%d/labels", &n)
			// go-github posts the label names as a bare JSON array.
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			rec.added[n] = append(rec.added[n], labels...)
			// GET /issues/{n} reflects the write, as GitHub does, so the
			// stage engine's fresh read sees the labels the sweep left.
			if issue, ok := byNum[n]; ok {
				for _, l := range labels {
					issue.Labels = append(issue.Labels, wireLabel{Name: l})
				}
				byNum[n] = issue
			}
			_ = json.NewEncoder(w).Encode([]wireLabel{})

		case strings.Contains(path, "/labels/") && r.Method == http.MethodDelete:
			if rec.removeStatus != 0 {
				w.WriteHeader(rec.removeStatus)
				return
			}
			var n int
			var label string
			fmt.Sscanf(path, "issues/%d/labels/%s", &n, &label)
			rec.removed[n] = append(rec.removed[n], label)
			if issue, ok := byNum[n]; ok {
				kept := issue.Labels[:0:0]
				for _, l := range issue.Labels {
					if !strings.EqualFold(l.Name, label) {
						kept = append(kept, l)
					}
				}
				issue.Labels = kept
				byNum[n] = issue
			}
			_ = json.NewEncoder(w).Encode([]wireLabel{})

		case strings.HasSuffix(path, "/timeline") && r.Method == http.MethodGet:
			rec.timelineReads++
			if rec.timelineStatus != 0 {
				w.WriteHeader(rec.timelineStatus)
				return
			}
			var n int
			fmt.Sscanf(path, "issues/%d/timeline", &n)
			events := rec.timelines[n]
			if events == nil {
				events = []unparkWireTimelineEvent{}
			}
			_ = json.NewEncoder(w).Encode(events)

		case strings.HasPrefix(path, "collaborators/") && strings.HasSuffix(path, "/permission"):
			rec.permLookups++
			login := strings.TrimSuffix(strings.TrimPrefix(path, "collaborators/"), "/permission")
			level, ok := rec.permissions[login]
			if !ok {
				level = "read"
			}
			out := map[string]any{"permission": level}
			if role, ok := rec.roles[login]; ok {
				out["role_name"] = role
			}
			_ = json.NewEncoder(w).Encode(out)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, rec
}

func unparkNow() string  { return time.Now().UTC().Format(time.RFC3339) }
func unparkOld() string  { return time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339) }
func unparkSoon() string { return time.Now().UTC().Add(time.Minute).Format(time.RFC3339) }

func parkedIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	return unparkWireIssue{
		Number:   number,
		Title:    "parked work",
		Body:     "Recommendation: option A.\n\nOption A: strip the secret\nOption B: document only\n",
		User:     wireUser{Login: "hive[bot]"},
		Labels:   []wireLabel{{Name: "needs-human"}, {Name: "needs-decision"}, {Name: "hold"}},
		Comments: comments,
	}
}

func directionParkedIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	issue := parkedIssue(number, comments...)
	issue.Labels = []wireLabel{{Name: issueNeedsDirectionLabel}, {Name: "hold"}}
	return issue
}

func humanComment(id int64, login, body string) unparkWireComment {
	return unparkWireComment{
		ID:        id,
		Body:      body,
		User:      wireUser{Login: login},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
		HTMLURL:   fmt.Sprintf("https://example.test/c/%d", id),
	}
}

func TestSweepIssueUnparkCommandsNeedsDirectionClearsParkingLabel(t *testing.T) {
	issues := []unparkWireIssue{directionParkedIssue(6, humanComment(10, "maintainer", "/hive approve"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Number != 6 {
		t.Fatalf("expected needs-direction issue 6 un-parked, got %+v", result.Unparked)
	}
	removed := strings.Join(rec.removed[6], ",")
	if !strings.Contains(removed, issueNeedsDirectionLabel) {
		t.Fatalf("expected %s removed, got %v", issueNeedsDirectionLabel, rec.removed[6])
	}
	if strings.Contains(removed, "hold") {
		t.Fatalf("hold must never be removed, got %v", rec.removed[6])
	}
	if len(rec.added[6]) != 1 || rec.added[6][0] != HumanAckLabel {
		t.Fatalf("expected %s added, got %v", HumanAckLabel, rec.added[6])
	}
	// v6 names the labels actually removed rather than the built-in ones.
	if joined := strings.Join(rec.posted[6], "\n"); !strings.Contains(joined, "`"+issueNeedsDirectionLabel+"` removed") {
		t.Fatalf("accepted reply should name %s as removed, got %v", issueNeedsDirectionLabel, rec.posted[6])
	}
}

func TestSweepIssueUnparkCommandsApproveClearsParkingLabels(t *testing.T) {
	issues := []unparkWireIssue{parkedIssue(7, humanComment(11, "maintainer", "/hive approve"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Number != 7 {
		t.Fatalf("expected issue 7 un-parked, got %+v", result.Unparked)
	}
	if result.Unparked[0].Actor != "maintainer" {
		t.Fatalf("actor = %q", result.Unparked[0].Actor)
	}
	removed := strings.Join(rec.removed[7], ",")
	if !strings.Contains(removed, "needs-decision") {
		t.Fatalf("expected the decision park label removed, got %v", rec.removed[7])
	}
	if !strings.Contains(removed, "needs-human") {
		t.Fatalf("needs-human is answered by the maintainer's command and comes off, got %v", rec.removed[7])
	}
	if strings.Contains(removed, "hold") {
		t.Fatalf("hold must never be removed, got %v", rec.removed[7])
	}
	if len(rec.added[7]) == 0 || rec.added[7][0] != HumanAckLabel {
		t.Fatalf("expected %s added, got %v", HumanAckLabel, rec.added[7])
	}
	var accepted bool
	for _, body := range rec.posted[7] {
		if strings.Contains(body, "Un-parked by @maintainer") {
			accepted = true
		}
	}
	if !accepted {
		t.Fatalf("expected an acceptance reply, got %v", rec.posted[7])
	}
}

func TestSweepIssueUnparkCommandsAIAuthorCommenterGetsRefusalReply(t *testing.T) {
	command := humanComment(11, "owner", "/hive approve")
	issues := []unparkWireIssue{parkedIssue(7, command)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"owner": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "owner", AppLogin: "hive[bot]"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("an ai_author command must not un-park, got %+v", result.Unparked)
	}
	if result.Replies != 1 {
		t.Fatalf("replies = %d, want 1", result.Replies)
	}
	if len(rec.removed[7]) != 0 || len(rec.added[7]) != 0 {
		t.Fatalf("labels changed: removed=%v added=%v", rec.removed[7], rec.added[7])
	}
	var refusals int
	for _, body := range rec.posted[7] {
		if strings.Contains(body, unparkNoticeMarker) {
			continue
		}
		if !strings.Contains(body, "project.ai_author") || !strings.Contains(body, "hive:unpark-reply:9879 id=11") {
			t.Fatalf("unexpected reply %q", body)
		}
		refusals++
	}
	if refusals != 1 {
		t.Fatalf("expected one refusal reply, got %v", rec.posted[7])
	}

	// Second sweep sees what the first one posted and must not post again.
	comments := []unparkWireComment{command}
	for i, body := range rec.posted[7] {
		comments = append(comments, unparkWireComment{
			ID:        int64(100 + i),
			Body:      body,
			User:      wireUser{Login: "hive[bot]"},
			CreatedAt: unparkNow(),
			UpdatedAt: unparkNow(),
		})
	}
	server2, rec2 := newUnparkServer(t, "org", "repo", []unparkWireIssue{parkedIssue(7, comments...)}, map[string]string{"owner": "admin"})
	c2 := newTestClient(t, server2, "org", []string{"repo"})
	c2.SetHiveIdentity(HiveIdentity{AIAuthor: "owner", AppLogin: "hive[bot]"})

	result, err = c2.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if result.Replies != 0 || len(rec2.posted[7]) != 0 {
		t.Fatalf("second sweep must not post again: replies=%d posted=%v", result.Replies, rec2.posted[7])
	}
	if len(rec2.removed[7]) != 0 || len(rec2.added[7]) != 0 {
		t.Fatalf("labels changed on second sweep: removed=%v added=%v", rec2.removed[7], rec2.added[7])
	}
}

func TestSweepIssueUnparkCommandsAIAuthorProseStaysSilent(t *testing.T) {
	issues := []unparkWireIssue{parkedIssue(7, humanComment(11, "owner", "go ahead"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"owner": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "owner", AppLogin: "hive[bot]"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Replies != 0 {
		t.Fatalf("replies = %d, want 0", result.Replies)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("expected no un-park, got %+v", result.Unparked)
	}
	for _, body := range rec.posted[7] {
		if !strings.Contains(body, unparkNoticeMarker) {
			t.Fatalf("only the What-to-reply notice may be posted, got %q", body)
		}
	}
}

func TestSweepIssueUnparkCommandsDecisionRecordsText(t *testing.T) {
	issues := []unparkWireIssue{parkedIssue(8, humanComment(21, "maintainer", "/hive decision keep 10 stable tags"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "maintain"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != "keep 10 stable tags" {
		t.Fatalf("decision not recorded: %+v", result.Unparked)
	}
	if result.Unparked[0].CommentURL != "https://example.test/c/21" {
		t.Fatalf("comment URL = %q", result.Unparked[0].CommentURL)
	}
	joined := strings.Join(rec.posted[8], "\n")
	if !strings.Contains(joined, "Decision: keep 10 stable tags") {
		t.Fatalf("expected the decision in the reply, got %v", rec.posted[8])
	}
}

func TestSweepIssueUnparkCommandsIgnoresIneligibleCommenters(t *testing.T) {
	cases := []struct {
		name    string
		comment unparkWireComment
		perms   map[string]string
	}{
		{"read only", humanComment(31, "drive-by", "/hive approve"), map[string]string{"drive-by": "read"}},
		{"bot account", humanComment(32, "other[bot]", "/hive approve"), map[string]string{"other[bot]": "admin"}},
		{"quoted command", humanComment(33, "maintainer", "> /hive approve\n\nnot mine"), map[string]string{"maintainer": "admin"}},
		{"fenced command", humanComment(34, "maintainer", "```\n/hive approve\n```"), map[string]string{"maintainer": "admin"}},
		{"unknown command", humanComment(35, "maintainer", "/hive yolo"), map[string]string{"maintainer": "admin"}},
		{"decision with no text", humanComment(36, "maintainer", "/hive decision"), map[string]string{"maintainer": "admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := []unparkWireIssue{parkedIssue(9, tc.comment)}
			server, rec := newUnparkServer(t, "org", "repo", issues, tc.perms)
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.Unparked) != 0 {
				t.Fatalf("expected no un-park, got %+v", result.Unparked)
			}
			if len(rec.removed[9]) != 0 || len(rec.added[9]) != 0 {
				t.Fatalf("labels changed: removed=%v added=%v", rec.removed[9], rec.added[9])
			}
			for _, body := range rec.posted[9] {
				if !strings.Contains(body, unparkNoticeMarker) {
					t.Fatalf("only the What-to-reply notice may be posted, got %q", body)
				}
			}
		})
	}
}

func TestSweepIssueUnparkCommandsIgnoresEditedAndStaleComments(t *testing.T) {
	edited := humanComment(41, "maintainer", "/hive approve")
	edited.UpdatedAt = unparkSoon()
	stale := humanComment(42, "maintainer", "/hive approve")
	stale.CreatedAt = unparkOld()
	stale.UpdatedAt = unparkOld()

	for name, comment := range map[string]unparkWireComment{"edited": edited, "stale": stale} {
		t.Run(name, func(t *testing.T) {
			issues := []unparkWireIssue{parkedIssue(10, comment)}
			server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.Unparked) != 0 {
				t.Fatalf("expected no un-park, got %+v", result.Unparked)
			}
			if len(rec.removed[10]) != 0 {
				t.Fatalf("labels changed: %v", rec.removed[10])
			}
		})
	}
}

func TestSweepIssueUnparkCommandsPostsNoticeOnceAndAnswersOnce(t *testing.T) {
	notice := unparkWireComment{
		ID:        51,
		Body:      unparkNoticeBody(parkedIssue(11).Body),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}

	answered := unparkWireComment{
		ID:        53,
		Body:      fmt.Sprintf("%s52 -->\nUn-parked by @maintainer.", unparkReplyMarkerPrefix),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}
	issues := []unparkWireIssue{parkedIssue(11, notice, humanComment(52, "maintainer", "/hive approve"), answered)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("an answered command must not be replayed: %+v", result.Unparked)
	}
	if len(rec.posted[11]) != 0 {
		t.Fatalf("no comment should be posted, got %v", rec.posted[11])
	}
	if len(rec.edited) != 0 {
		t.Fatalf("an up-to-date notice must not be edited, got %v", rec.edited)
	}
	if result.Seen != 1 || result.Skipped != 1 {
		t.Fatalf("seen=%d skipped=%d", result.Seen, result.Skipped)
	}
}

func TestSweepIssueUnparkCommandsNeedsDirectionIdempotency(t *testing.T) {
	notice := unparkWireComment{
		ID:        54,
		Body:      unparkNoticeBody(directionParkedIssue(16).Body),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}
	answered := unparkWireComment{
		ID:        56,
		Body:      fmt.Sprintf("%s55 -->\nUn-parked by @maintainer.", unparkReplyMarkerPrefix),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}
	issues := []unparkWireIssue{directionParkedIssue(16, notice, humanComment(55, "maintainer", "/hive decision option C"), answered)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("an answered needs-direction command must not be replayed: %+v", result.Unparked)
	}
	if len(rec.posted[16]) != 0 || len(rec.removed[16]) != 0 || len(rec.added[16]) != 0 {
		t.Fatalf("no writes expected, posted=%v removed=%v added=%v", rec.posted[16], rec.removed[16], rec.added[16])
	}
}

// TestSweepIssueUnparkCommandsNeverReplaysSupersededDecision covers
// hivecommons/hive#14344: two decisions, the newer one already answered, the
// issue parked again. The older (retracted) decision must not be applied.
func TestSweepIssueUnparkCommandsNeverReplaysSupersededDecision(t *testing.T) {
	notice := unparkWireComment{
		ID:        60,
		Body:      unparkNoticeBody(parkedIssue(17).Body),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}
	answeredNewer := unparkWireComment{
		ID:        63,
		Body:      fmt.Sprintf("%s62 -->\nUn-parked by @maintainer.\n\nDecision: Hold", unparkReplyMarkerPrefix),
		User:      wireUser{Login: "hive[bot]"},
		CreatedAt: unparkNow(),
		UpdatedAt: unparkNow(),
	}
	issues := []unparkWireIssue{parkedIssue(17,
		notice,
		humanComment(61, "maintainer", "/hive decision Proceed with the membership grant"),
		humanComment(62, "maintainer", "/hive decision Hold. Retracting the earlier instruction"),
		answeredNewer,
	)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("a superseded decision must not be replayed: %+v", result.Unparked)
	}
	if len(rec.posted[17]) != 0 || len(rec.removed[17]) != 0 || len(rec.added[17]) != 0 {
		t.Fatalf("no writes expected, posted=%v removed=%v added=%v", rec.posted[17], rec.removed[17], rec.added[17])
	}
}

// TestSweepIssueUnparkCommandsAppliesNewestOfSeveralDecisions: with two
// unanswered decisions only the newest is applied, with a single reply.
func TestSweepIssueUnparkCommandsAppliesNewestOfSeveralDecisions(t *testing.T) {
	issues := []unparkWireIssue{parkedIssue(18,
		humanComment(71, "maintainer", "/hive decision Proceed"),
		humanComment(72, "maintainer", "/hive decision Hold"),
	)}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != "Hold" {
		t.Fatalf("expected only the newest decision applied, got %+v", result.Unparked)
	}
	var replies int
	for _, body := range rec.posted[18] {
		if strings.Contains(body, unparkReplyMarkerPrefix) {
			replies++
			if !strings.Contains(body, unparkReplyMarkerPrefix+"72 ") {
				t.Fatalf("reply must answer the newest decision, got %q", body)
			}
		}
	}
	if replies != 1 {
		t.Fatalf("expected exactly one unpark reply, got %v", rec.posted[18])
	}
}

func TestSweepIssueUnparkCommandsHelpAndProseDoNotUnpark(t *testing.T) {
	cases := []struct {
		name    string
		comment unparkWireComment
		want    string
	}{
		{"help", humanComment(61, "maintainer", "/hive help"), "Maintainer commands"},
		{"prose", humanComment(62, "maintainer", "approved, go with A"), "To start work, reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := []unparkWireIssue{parkedIssue(12, tc.comment)}
			server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.Unparked) != 0 {
				t.Fatalf("expected no un-park, got %+v", result.Unparked)
			}
			if result.Replies != 1 {
				t.Fatalf("replies = %d", result.Replies)
			}
			if len(rec.removed[12]) != 0 {
				t.Fatalf("labels changed: %v", rec.removed[12])
			}
			joined := strings.Join(rec.posted[12], "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("expected %q in the reply, got %v", tc.want, rec.posted[12])
			}
		})
	}
}

func TestSweepIssueUnparkCommandsSkipsUnparkedIssues(t *testing.T) {
	open := unparkWireIssue{
		Number:   13,
		Title:    "ordinary work",
		User:     wireUser{Login: "hive[bot]"},
		Comments: []unparkWireComment{humanComment(71, "maintainer", "/hive approve")},
	}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{open}, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 0 || len(result.Unparked) != 0 {
		t.Fatalf("an unparked issue is not this sweep's business: %+v", result)
	}
	if len(rec.posted[13]) != 0 {
		t.Fatalf("no notice on an unparked issue, got %v", rec.posted[13])
	}
}

func TestSweepIssueUnparkCommandsAuditAndCap(t *testing.T) {
	issues := []unparkWireIssue{
		parkedIssue(14, humanComment(81, "maintainer", "/hive approve")),
		parkedIssue(15, humanComment(82, "maintainer", "/hive approve")),
	}
	server, _ := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})

	var audited []IssueUnparkEvent
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		MaxActions: 1,
		Audit:      func(e IssueUnparkEvent) { audited = append(audited, e) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 {
		t.Fatalf("cap not honoured: %+v", result.Unparked)
	}
	if len(audited) != 1 || audited[0].Repo != "repo" {
		t.Fatalf("audit = %+v", audited)
	}
}

func TestParseUnparkCommand(t *testing.T) {
	cases := []struct {
		body     string
		wantKind unparkCommandKind
		wantText string
		wantOK   bool
	}{
		{"/hive approve", unparkCommandApprove, "", true},
		{"/HIVE Approve\nthanks", unparkCommandApprove, "", true},
		{"/hive decision B", unparkCommandDecision, "B", true},
		{"/hive decision keep 10 stable tags", unparkCommandDecision, "keep 10 stable tags", true},
		{"/hive help", unparkCommandHelp, "", true},
		{"/fixed", unparkCommandFixed, "", true},
		{"/Fixed admin change made out-of-band\nthanks", unparkCommandFixed, "admin change made out-of-band", true},
		{"/close", unparkCommandFixed, "", true},
		{"> /fixed", unparkCommandNone, "", false},
		{"thanks!\n/hive approve", unparkCommandNone, "", false},
		{"/hive", unparkCommandNone, "", false},
		{"", unparkCommandNone, "", false},
		{"please /hive approve", unparkCommandNone, "", false},
	}
	for _, tc := range cases {
		cmd, ok := parseUnparkCommand(tc.body)
		if ok != tc.wantOK || cmd.Kind != tc.wantKind || cmd.Decision != tc.wantText {
			t.Errorf("parseUnparkCommand(%q) = %+v, %v; want %s/%q/%v", tc.body, cmd, ok, tc.wantKind, tc.wantText, tc.wantOK)
		}
	}
}

func TestUnparkNoticeBodyListsIssueOptions(t *testing.T) {
	body := unparkNoticeBody("Recommendation: A.\n\n- **Option A** — strip the secret\n- Option B: document only\n")
	for _, want := range []string{unparkNoticeMarker, "What to reply", "`/hive approve`", "option A", "/hive decision B", "`/hive help`", "/fixed", maintainerCommandsDocURL} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice missing %q:\n%s", want, body)
		}
	}
	plain := unparkNoticeBody("no options here")
	if !strings.Contains(plain, "/hive decision <your instructions>") {
		t.Fatalf("an issue with no options needs the free-text form:\n%s", plain)
	}
}

func TestIsUnparkProseAssent(t *testing.T) {
	for _, body := range []string{"approved", "Go ahead", "LGTM", "sounds good to me"} {
		if !isUnparkProseAssent(body) {
			t.Errorf("isUnparkProseAssent(%q) = false, want true", body)
		}
	}
	for _, body := range []string{"/hive approve", "/fixed approved", "> approved", "what do you think?", ""} {
		if isUnparkProseAssent(body) {
			t.Errorf("isUnparkProseAssent(%q) = true, want false", body)
		}
	}
}

func TestHasIssueParkingLabel(t *testing.T) {
	if !hasIssueParkingLabel([]string{"Needs-Human"}) || !hasIssueParkingLabel([]string{"needs-decision"}) || !hasIssueParkingLabel([]string{"Needs-Direction"}) {
		t.Fatal("parking labels must be recognised case-insensitively")
	}
	if hasIssueParkingLabel([]string{"hold", "bug"}) {
		t.Fatal("hold is not a parking label this sweep answers")
	}
}

func TestRenderUnparkHelpReplyListsDocumentedCommands(t *testing.T) {
	body := renderUnparkHelpReply("Recommendation: option A.")
	doc, err := os.ReadFile("../../docs/maintainer-commands.md")
	if err != nil {
		t.Fatalf("read maintainer commands doc: %v", err)
	}
	docBody := string(doc)
	for _, want := range []string{
		"/hive approve",
		"/hive decision",
		"/hive help",
		"/fixed",
		"/reopen",
		"/help-wanted",
		"/good-first-issue",
		"/hacktober-fest",
		"/kind",
		"/area",
		"/assign",
		"/unassign",
		maintainerCommandsDocURL,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("help reply missing %q:\n%s", want, body)
		}
		if !strings.Contains(docBody, want) {
			t.Fatalf("maintainer commands doc missing %q", want)
		}
	}
}

// hivecommons/docs#419 (2026-10-22): the maintainer wrote "Adding
// `needs-human`", put the label on, and later replied `/hive decision …`.
// The un-park attributed the label to them and refused to lift it, so the
// issue re-derived stage/045 and the tracker kept reading "Needs human
// decision". A maintainer's command is the human input the label asked
// for, whoever applied it: the un-park clears `needs-human` with the park
// label, the reply attributes what it cleared and names `hold` as the way
// to keep agents off, and the issue re-derives as actionable. `hold` is
// still never touched.
func TestSweepIssueUnparkCommandsDecisionLiftsPersonAppliedNeedsHuman(t *testing.T) {
	issue := parkedIssue(419, humanComment(31, "owner", "/hive decision Agents stand down; owner/Console owners handle this"))
	issue.Labels = []wireLabel{{Name: "needs-human"}, {Name: NeedsDecisionStageLabel}, {Name: "kind/bug"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"owner": "admin"})
	rec.timelines = map[int][]unparkWireTimelineEvent{419: {labeledAt(1, "owner", "needs-human", time.Now().Add(-time.Hour))}}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != "Agents stand down; owner/Console owners handle this" {
		t.Fatalf("decision not recorded: %+v", result.Unparked)
	}
	if got := strings.Join(rec.removed[419], ","); got != "needs-human,"+NeedsDecisionStageLabel {
		t.Fatalf("removed = %v, want needs-human then %s", rec.removed[419], NeedsDecisionStageLabel)
	}
	if got := strings.Join(rec.added[419], ","); got != HumanAckLabel+","+stagelabels.Actionable.Label() {
		t.Fatalf("added = %v, want %s then the engine's %s", rec.added[419], HumanAckLabel, stagelabels.Actionable.Label())
	}
	var accepted string
	for _, body := range rec.posted[419] {
		if strings.Contains(body, "Un-parked by @owner") {
			accepted = body
		}
	}
	if accepted == "" || !strings.Contains(accepted, "`needs-human` cleared (added by @owner)") || !strings.Contains(accepted, "add `hold` to keep agents off it") {
		t.Fatalf("accepted reply must attribute the cleared needs-human and name hold, got %v", rec.posted[419])
	}

	// What the issue carries after the sweep, as the stage reconciler sees it.
	after := []string{"kind/bug", HumanAckLabel, stagelabels.Actionable.Label()}
	if stage, _, _ := IssueStageFor(IssueStageInput{Repo: "org/repo", Number: 419, Labels: after}, nil); stage == stagelabels.IssueNeedsDecision {
		t.Fatalf("stage after un-park = %s, want anything but 045 once needs-human is gone", stage)
	}
	if hasHardSuppressIssueLabel(after) {
		t.Fatal("an issue whose needs-human lifted must no longer be hard-suppressed")
	}
}

// An issue carrying only `needs-human` is a person's to work until a
// maintainer answers; `/hive approve` is that answer: the label comes off,
// approved-direction goes on, and the Unparked fact makes the stage engine
// re-read the issue as actionable.
func TestSweepIssueUnparkCommandsNeedsHumanOnlyApproveLifts(t *testing.T) {
	issue := parkedIssue(12681, humanComment(32, "owner", "/hive approve"))
	issue.Labels = []wireLabel{{Name: "needs-human"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"owner": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := strings.Join(rec.removed[12681], ","); got != "needs-human" {
		t.Fatalf("removed = %v, want needs-human", rec.removed[12681])
	}
	if got := strings.Join(rec.added[12681], ","); got != HumanAckLabel+","+stagelabels.Actionable.Label() {
		t.Fatalf("added = %v, want %s then the engine's %s", rec.added[12681], HumanAckLabel, stagelabels.Actionable.Label())
	}
}

// `/hive approve` on an issue project.issue_filter refuses (projectbluefin
// bluefin-bling#110: stage/010 written, then removed by the sweep two
// minutes later as "outside the pipeline: filtered", under an "un-parked"
// reply). The park lifts, but the engine re-reads the issue against the
// filter and writes no stage — the result the sweep derives, so nothing is
// added only to be taken off — and the reply names the label that would
// admit it instead of promising the queue. The hive never adds that label.
func TestSweepIssueUnparkCommandsApproveOnFilteredIssue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		labels    []wireLabel
		wantAdded string
		wantNote  bool
	}{
		{"refused by the filter", []wireLabel{{Name: "needs-human"}, {Name: "kind/bug"}}, HumanAckLabel, true},
		{"admitted by the filter", []wireLabel{{Name: "needs-human"}, {Name: "triage/accepted"}}, HumanAckLabel + "," + stagelabels.Actionable.Label(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := parkedIssue(110, humanComment(33, "owner", "/hive approve"))
			issue.Labels = tc.labels
			server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"owner": "write"})
			c := newTestClient(t, server, "org", []string{"repo"})
			c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})

			if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if got := strings.Join(rec.added[110], ","); got != tc.wantAdded {
				t.Fatalf("added = %v, want %s", rec.added[110], tc.wantAdded)
			}
			var accepted string
			for _, body := range rec.posted[110] {
				if strings.Contains(body, "Un-parked by @owner") {
					accepted = body
				}
			}
			if accepted == "" {
				t.Fatalf("no acceptance reply, got %v", rec.posted[110])
			}
			hasNote := strings.Contains(accepted, "still outside the agent queue") && strings.Contains(accepted, "`triage/accepted`")
			if hasNote != tc.wantNote {
				t.Fatalf("filter note present = %v, want %v:\n%s", hasNote, tc.wantNote, accepted)
			}
			if tc.wantNote && strings.Contains(accepted, "returns to the agent queue") {
				t.Fatalf("reply promises the agent queue to an issue the filter refuses:\n%s", accepted)
			}
		})
	}
}

// hivecommons/hive#13075 (seen on #12984): an agent parked the issue with
// `needs-human` + `/unclaim`, the engine derived stage/045, the maintainer
// replied `/hive decision …`, the un-park cleared 045 and added
// approved-direction but left the source label, and two minutes later the
// engine re-derived 045 from it — the decision was a silent no-op. The rule
// now lives in stageengine.DecisionLiftsNeedsHuman: the hive's own
// `needs-human` is answered by the maintainer's decision and lifts with the
// park — and since hivecommons/docs#419 so does a person's or an
// unattributed one; the reply attributes what it cleared. The stage engine
// then derives the post-decision stage from the labels it reads back.
func TestSweepIssueUnparkCommandsNeedsHumanByApplier(t *testing.T) {
	const hiveBot = "hivecommons-hive[bot]"
	base := time.Now().Add(-time.Hour)
	cases := []struct {
		name        string
		command     string
		commenter   string
		perms       map[string]string
		labels      []string
		timeline    []unparkWireTimelineEvent
		wantRemoved []string
		wantAdded   []string
		wantReply   string
		wantUnpark  bool
	}{
		{
			name:        "decision clears hive-applied needs-human",
			command:     "/hive decision proceed — scope it to the dashboard",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "write"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel, "kind/bug"},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), labeledAt(2, hiveBot, NeedsDecisionStageLabel, base.Add(time.Minute))},
			wantRemoved: []string{"needs-human", NeedsDecisionStageLabel},
			wantAdded:   []string{HumanAckLabel, stagelabels.Actionable.Label()},
			wantReply:   "`needs-human` cleared",
			wantUnpark:  true,
		},
		{
			name:        "approve clears hive-applied needs-human",
			command:     "/hive approve",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "maintain"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base)},
			wantRemoved: []string{"needs-human", NeedsDecisionStageLabel},
			wantAdded:   []string{HumanAckLabel, stagelabels.Actionable.Label()},
			wantReply:   "`needs-human` cleared",
			wantUnpark:  true,
		},
		{
			name:        "hold is preserved while needs-human lifts",
			command:     "/hive decision go",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "admin"},
			labels:      []string{"needs-human", "hold"},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), labeledAt(2, "maintainer", "hold", base)},
			wantRemoved: []string{"needs-human"},
			wantAdded:   []string{HumanAckLabel},
			wantReply:   "any `hold` label is left exactly as it was. `needs-human` cleared",
			wantUnpark:  true,
		},
		{
			name:      "commenter without write access is ignored",
			command:   "/hive decision proceed",
			commenter: "drive-by",
			perms:     map[string]string{"drive-by": "read"},
			labels:    []string{"needs-human", NeedsDecisionStageLabel},
			timeline:  []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base)},
		},
		{
			name:        "person-applied needs-human lifts and is attributed (docs#419)",
			command:     "/hive decision Agents stand down",
			commenter:   "owner",
			perms:       map[string]string{"owner": "admin"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), unlabeledAt(2, hiveBot, "needs-human", base.Add(time.Minute)), labeledAt(3, "owner", "needs-human", base.Add(2*time.Minute))},
			wantRemoved: []string{"needs-human", NeedsDecisionStageLabel},
			wantAdded:   []string{HumanAckLabel, stagelabels.Actionable.Label()},
			wantReply:   "`needs-human` cleared (added by @owner)",
			wantUnpark:  true,
		},
		{
			name:        "unattributed needs-human lifts",
			command:     "/hive approve",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "write"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel},
			timeline:    nil,
			wantRemoved: []string{"needs-human", NeedsDecisionStageLabel},
			wantAdded:   []string{HumanAckLabel, stagelabels.Actionable.Label()},
			wantReply:   "`needs-human` cleared: your command is the human input",
			wantUnpark:  true,
		},
		{
			name:        "issue without needs-human is unchanged",
			command:     "/hive decision proceed",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "write"},
			labels:      []string{NeedsDecisionStageLabel, "needs-decision"},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, NeedsDecisionStageLabel, base)},
			wantRemoved: []string{"needs-decision", NeedsDecisionStageLabel},
			wantAdded:   []string{HumanAckLabel, stagelabels.Actionable.Label()},
			wantReply:   "left exactly as it was.",
			wantUnpark:  true,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			number := 13075 + i
			issue := parkedIssue(number, humanComment(int64(500+i), tc.commenter, tc.command))
			issue.Labels = nil
			for _, l := range tc.labels {
				issue.Labels = append(issue.Labels, wireLabel{Name: l})
			}
			server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, tc.perms)
			rec.timelines = map[int][]unparkWireTimelineEvent{number: tc.timeline}
			c := newTestClient(t, server, "org", []string{"repo"})
			c.SetAppBotLogin(hiveBot)

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if got := len(result.Unparked) == 1; got != tc.wantUnpark {
				t.Fatalf("unparked = %+v, want unpark=%v", result.Unparked, tc.wantUnpark)
			}
			if !tc.wantUnpark {
				if len(rec.removed[number]) != 0 || len(rec.added[number]) != 0 {
					t.Fatalf("ignored commenter must change nothing: removed %v added %v", rec.removed[number], rec.added[number])
				}
				for _, body := range rec.posted[number] {
					if strings.Contains(body, "Un-parked by") {
						t.Fatalf("ignored commenter must get no acceptance, got %q", body)
					}
				}
				return
			}
			if got := strings.Join(rec.removed[number], ","); got != strings.Join(tc.wantRemoved, ",") {
				t.Fatalf("removed = %v, want %v", rec.removed[number], tc.wantRemoved)
			}
			if got := strings.Join(rec.added[number], ","); got != strings.Join(tc.wantAdded, ",") {
				t.Fatalf("added = %v, want %v", rec.added[number], tc.wantAdded)
			}
			if strings.Contains(strings.Join(rec.removed[number], ","), "hold") {
				t.Fatalf("hold must never be removed, got %v", rec.removed[number])
			}
			var accepted string
			for _, body := range rec.posted[number] {
				if strings.Contains(body, "Un-parked by @"+tc.commenter) {
					accepted = body
				}
			}
			if accepted == "" || !strings.Contains(accepted, tc.wantReply) {
				t.Fatalf("accepted reply must contain %q, got %v", tc.wantReply, rec.posted[number])
			}
		})
	}
}

// The applier classification the un-park reply attributes with: the last
// `labeled needs-human` event decides, a later re-add by a person outranks
// the hive's earlier one, and a missing or unreadable timeline is unknown.
// The lift itself (stageengine.DecisionLiftsNeedsHuman) no longer depends
// on the applier.
func TestNeedsHumanOutcomeClassifiesApplier(t *testing.T) {
	const hiveBot = "hive[bot]"
	base := time.Now().Add(-time.Hour)
	cases := []struct {
		name     string
		events   []unparkWireTimelineEvent
		status   int
		want     stageengine.NeedsHumanApplier
		wantLift bool
	}{
		{"hive", []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base)}, 0, stageengine.NeedsHumanApplierHive, true},
		{"person", []unparkWireTimelineEvent{labeledAt(1, "owner", "needs-human", base)}, 0, stageengine.NeedsHumanApplierPerson, true},
		{"person re-added after hive", []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), labeledAt(2, "owner", "needs-human", base.Add(time.Minute))}, 0, stageengine.NeedsHumanApplierPerson, true},
		{"hive re-added after person", []unparkWireTimelineEvent{labeledAt(1, "owner", "needs-human", base), labeledAt(2, hiveBot, "needs-human", base.Add(time.Minute))}, 0, stageengine.NeedsHumanApplierHive, true},
		{"other label only", []unparkWireTimelineEvent{labeledAt(1, hiveBot, "hold", base)}, 0, stageengine.NeedsHumanApplierUnknown, true},
		{"no events", nil, 0, stageengine.NeedsHumanApplierUnknown, true},
		{"timeline unreadable", nil, http.StatusInternalServerError, stageengine.NeedsHumanApplierUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := newUnparkServer(t, "org", "repo", nil, nil)
			rec.timelines = map[int][]unparkWireTimelineEvent{1: tc.events}
			rec.timelineStatus = tc.status
			c := newTestClient(t, server, "org", []string{"repo"})
			c.SetAppBotLogin(hiveBot)
			got := c.needsHumanOutcome(context.Background(), "org", "repo", 1, nil)
			if got.applier != tc.want || got.lift != tc.wantLift {
				t.Fatalf("outcome = %+v, want applier %v lift %v", got, tc.want, tc.wantLift)
			}
		})
	}
}

// hivecommons/dibs#459 (2026-10-22): the issue parked at stage/045 for an
// admin change outside the repository; the maintainer made it and replied
// `/fixed` with the evidence. Nothing consumed it — `/fixed` was only a
// per-repo Actions workflow — so the park stayed and the tracker kept
// asking. A maintainer's `/fixed` on a parked issue is a command the sweep
// honours: close as completed, take `needs-human` and the stage label off,
// post one acknowledgement, leave `hold` alone.
func TestSweepIssueUnparkCommandsFixedClosesParkedIssue(t *testing.T) {
	issue := parkedIssue(459, humanComment(41, "maintainer", "/fixed admin change made; see org settings audit log"))
	issue.Labels = []wireLabel{{Name: "needs-human"}, {Name: NeedsDecisionStageLabel}, {Name: "hold"}, {Name: "kind/bug"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	var audited []IssueUnparkEvent
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		ClosedAudit: func(ev IssueUnparkEvent) { audited = append(audited, ev) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("/fixed is a close, not an un-park: %+v", result.Unparked)
	}
	if len(result.Closed) != 1 || result.Closed[0].Number != 459 || result.Closed[0].Actor != "maintainer" || result.Closed[0].Decision != "admin change made; see org settings audit log" {
		t.Fatalf("closed = %+v", result.Closed)
	}
	if len(audited) != 1 || audited[0].Number != 459 {
		t.Fatalf("closed audit = %+v, want one event for #459", audited)
	}
	if got := strings.Join(rec.patched[459], ","); got != string(IssueStateReasonCompleted) {
		t.Fatalf("patched state_reason = %v, want completed", rec.patched[459])
	}
	removed := strings.Join(rec.removed[459], ",")
	for _, want := range []string{"needs-human", NeedsDecisionStageLabel} {
		if !strings.Contains(removed, want) {
			t.Errorf("removed = %v, want %s off", rec.removed[459], want)
		}
	}
	if strings.Contains(removed, "hold") {
		t.Fatalf("hold must never be removed, got %v", rec.removed[459])
	}
	if len(rec.added[459]) != 0 {
		t.Fatalf("a close adds no labels, got %v", rec.added[459])
	}
	var acks int
	for _, body := range rec.posted[459] {
		if strings.Contains(body, "Closed as completed: @maintainer confirmed with `/fixed`") {
			acks++
			if !strings.Contains(body, "admin change made; see org settings audit log") || !strings.Contains(body, "`hold` label is left exactly as it was") {
				t.Errorf("ack = %q, want the note and the hold caveat", body)
			}
		}
	}
	if acks != 1 {
		t.Fatalf("want exactly one acknowledgement, got %v", rec.posted[459])
	}
}

// `/fixed` is a maintainer's word: a commenter without write access is
// ignored, and an issue that is not parked is not this sweep's to close
// (the per-repo workflow, where present, handles reporter confirmation).
func TestSweepIssueUnparkCommandsFixedGates(t *testing.T) {
	driveBy := parkedIssue(460, humanComment(42, "drive-by", "/fixed"))
	unparked := parkedIssue(461, humanComment(43, "maintainer", "/fixed"))
	unparked.Labels = []wireLabel{{Name: "kind/bug"}, {Name: stagelabels.Actionable.Label()}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{driveBy, unparked}, map[string]string{"maintainer": "write", "drive-by": "read"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Closed) != 0 || len(result.Unparked) != 0 {
		t.Fatalf("nothing should act: closed %+v unparked %+v", result.Closed, result.Unparked)
	}
	for _, n := range []int{460, 461} {
		if len(rec.patched[n]) != 0 || len(rec.removed[n]) != 0 {
			t.Errorf("#%d: patched %v removed %v, want no writes", n, rec.patched[n], rec.removed[n])
		}
	}
}

// hivecommons/homebrew-hive#127 (2026-10-22): PR #128 merged with
// "Fixes #127", GitHub closed the issue, and `stage/045-needs-decision` +
// the hive's `needs-human` stayed on it for the rest of the day — the
// issue-stage reconciler visits open issues only. The closed-issue sweep
// lists recently closed issues, and for each still wearing hive labels
// sends the stage engine a Closed fact (its writer strips every stage/*
// label) and lifts the hive's own `needs-human`. A person's `needs-human`
// stays, `hold` stays, PRs and clean issues are not touched.
func TestSweepClosedIssueStageLabels(t *testing.T) {
	const hiveBot = "hivecommons-hive[bot]"
	base := time.Now().Add(-time.Hour)
	server, rec := newUnparkServer(t, "org", "repo", nil, nil)
	closed := []unparkWireIssue{
		{Number: 127, Title: "fixed by a merged PR", State: "closed", Labels: []wireLabel{{Name: NeedsDecisionStageLabel}, {Name: "needs-human"}, {Name: "kind/bug"}}},
		{Number: 128, Title: "closed by hand, person's needs-human", State: "closed", Labels: []wireLabel{{Name: NeedsDecisionStageLabel}, {Name: "needs-human"}, {Name: "hold"}}},
		{Number: 129, Title: "closed clean", State: "closed", Labels: []wireLabel{{Name: "kind/bug"}}},
		{Number: 130, Title: "retired park spelling only", State: "closed", Labels: []wireLabel{{Name: "needs-decision"}}},
		{Number: 131, Title: "a merged PR", State: "closed", PullRequest: &unparkWirePRLink{URL: "https://example.test/pulls/131"}, Labels: []wireLabel{{Name: "stage/085-needs-decision"}}},
	}
	rec.closedIssues = closed
	for _, issue := range closed {
		rec.register(issue)
	}
	rec.timelines = map[int][]unparkWireTimelineEvent{
		127: {labeledAt(1, hiveBot, "needs-human", base), labeledAt(2, hiveBot, NeedsDecisionStageLabel, base)},
		128: {labeledAt(3, "owner", "needs-human", base), labeledAt(4, hiveBot, NeedsDecisionStageLabel, base)},
	}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(hiveBot)

	var audited []ClosedIssueStageEvent
	result, err := c.SweepClosedIssueStageLabels(context.Background(), ClosedIssueStageSweepOptions{
		Audit: func(ev ClosedIssueStageEvent) { audited = append(audited, ev) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 3 || len(result.Cleaned) != 3 || result.Skipped != 0 {
		t.Fatalf("result = seen %d cleaned %d skipped %d, want 3/3/0: %+v", result.Seen, len(result.Cleaned), result.Skipped, result.Cleaned)
	}
	if len(audited) != 3 {
		t.Fatalf("audit = %+v, want three events", audited)
	}
	if got := strings.Join(rec.removed[127], ","); got != "needs-human,"+NeedsDecisionStageLabel {
		t.Errorf("#127 removed = %v, want the hive's needs-human then the stage label", rec.removed[127])
	}
	if got := strings.Join(rec.removed[128], ","); got != NeedsDecisionStageLabel {
		t.Errorf("#128 removed = %v, want only the stage label (person's needs-human and hold stay)", rec.removed[128])
	}
	if got := strings.Join(rec.removed[130], ","); got != "needs-decision" {
		t.Errorf("#130 removed = %v, want the retired park spelling", rec.removed[130])
	}
	for _, n := range []int{129, 131} {
		if len(rec.removed[n]) != 0 || len(rec.added[n]) != 0 {
			t.Errorf("#%d: removed %v added %v, want untouched", n, rec.removed[n], rec.added[n])
		}
	}
	for n, rm := range rec.removed {
		if strings.Contains(strings.Join(rm, ","), "hold") {
			t.Errorf("#%d: hold must never be removed, got %v", n, rm)
		}
	}
	for n, added := range rec.added {
		if len(added) != 0 {
			t.Errorf("#%d: a closed issue gets no stage label back, got %v", n, added)
		}
	}
	for _, ev := range audited {
		switch ev.Number {
		case 127:
			if !ev.NeedsHuman || len(ev.Stage) != 1 {
				t.Errorf("#127 event = %+v, want needs_human=true stage=[045]", ev)
			}
		case 128:
			if ev.NeedsHuman {
				t.Errorf("#128 event = %+v, want needs_human=false", ev)
			}
		}
	}
}

// The per-pass cap bounds the writes; a second pass picks up the rest.
func TestSweepClosedIssueStageLabelsCap(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", nil, nil)
	for n := 200; n < 205; n++ {
		issue := unparkWireIssue{Number: n, State: "closed", Labels: []wireLabel{{Name: NeedsDecisionStageLabel}}}
		rec.closedIssues = append(rec.closedIssues, issue)
		rec.register(issue)
	}
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepClosedIssueStageLabels(context.Background(), ClosedIssueStageSweepOptions{MaxActions: 2})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Cleaned) != 2 {
		t.Fatalf("cleaned = %d, want the cap of 2", len(result.Cleaned))
	}
	if _, err := (*Client)(nil).SweepClosedIssueStageLabels(context.Background(), ClosedIssueStageSweepOptions{}); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v, want ErrNoGitHubClient", err)
	}
}

// hivecommons/hive#14679 (seen on #14628): the owner's `/hive decision
// approve` followed by rationale retains the whole text and reads as approval.
func TestParseUnparkCommandKeepsDecisionTextAfterFirstLine(t *testing.T) {
	body := "/hive decision approve\r\n\r\nPlease do the following before merging:\r\n\r\n1. Change `Closes #14605` to `Refs #14605`.\n2. Keep #14605 open.\n3. Merge on green.\n"
	cmd, ok := parseUnparkCommand(body)
	if !ok || cmd.Kind != unparkCommandDecision || cmd.Decision != "approve" {
		t.Fatalf("parseUnparkCommand = %+v, %v", cmd, ok)
	}
	if !cmd.Approval() {
		t.Fatalf("approve with rationale did not read as approval: %q", cmd.Text)
	}
	for _, want := range []string{"approve\n\n", "1. Change `Closes #14605` to `Refs #14605`.", "2. Keep #14605 open.", "3. Merge on green."} {
		if !strings.Contains(cmd.DecisionText(), want) {
			t.Fatalf("decision text missing %q:\n%s", want, cmd.DecisionText())
		}
	}
	for _, bare := range []string{"/hive decision approve", "/hive decision Approved\n", "/hive decision approve.\n\n"} {
		cmd, ok := parseUnparkCommand(bare)
		if !ok || !cmd.Approval() || cmd.DecisionText() == "" {
			t.Fatalf("parseUnparkCommand(%q) = %+v, %v; want a bare approval", bare, cmd, ok)
		}
	}
	single, _ := parseUnparkCommand("/hive decision keep 10 stable tags")
	if single.DecisionText() != "keep 10 stable tags" || single.Approval() {
		t.Fatalf("single-line decision = %+v", single)
	}
}

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
		permissions: permissions,
	}
	byNum := map[int]unparkWireIssue{}
	for _, issue := range issues {
		byNum[issue.Number] = issue
	}

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
		case path == "issues" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(issues)

		case strings.HasPrefix(path, "issues/") && !strings.Contains(strings.TrimPrefix(path, "issues/"), "/") && r.Method == http.MethodGet:
			var n int
			fmt.Sscanf(path, "issues/%d", &n)
			issue, ok := byNum[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(issue)

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
	if joined := strings.Join(rec.posted[6], "\n"); !strings.Contains(joined, NeedsDecisionStageLabel) {
		t.Fatalf("accepted reply should name %s, got %v", NeedsDecisionStageLabel, rec.posted[6])
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
	if strings.Contains(removed, "needs-human") {
		t.Fatalf("needs-human must never be removed by an un-park, got %v", rec.removed[7])
	}
	if strings.Contains(removed, "hold") {
		t.Fatalf("hold must never be removed, got %v", rec.removed[7])
	}
	if len(rec.added[7]) != 1 || rec.added[7][0] != HumanAckLabel {
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
	for _, body := range []string{"/hive approve", "> approved", "what do you think?", ""} {
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

// The incident on hivecommons/hive#12680 / #12681 (2026-10-17): the owner
// replied `/hive decision … Agents stand down` on issues carrying
// `needs-human` + stage/085, the sweep removed every needs-* label including
// `needs-human`, and both issues became stage/010-actionable. An un-park
// records the decision and clears the decision park label, but never lifts
// `needs-human`; the accepted reply says so; and the resulting label set
// remains hard-suppressed. Under the owner decision that issues park at
// stage/045 (085 is PR-only), the kept `needs-human` re-derives 045 — the
// issue stays visibly a person's, never 085 and never actionable. The
// un-park is an Unparked fact for the stage engine (ADR-0025), which
// re-reads the issue and keeps 045 in place rather than removing and
// re-adding it.
func TestSweepIssueUnparkCommandsKeepsNeedsHuman(t *testing.T) {
	issue := parkedIssue(12680, humanComment(31, "owner", "/hive decision Agents stand down; owner/Console owners handle this"))
	issue.Labels = []wireLabel{{Name: "needs-human"}, {Name: NeedsDecisionStageLabel}, {Name: "kind/bug"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"owner": "admin"})
	// The owner put needs-human on the issue: a person's hold (#13075 lifts
	// only the hive's own).
	rec.timelines = map[int][]unparkWireTimelineEvent{12680: {labeledAt(1, "owner", "needs-human", time.Now().Add(-time.Hour))}}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != "Agents stand down; owner/Console owners handle this" {
		t.Fatalf("decision not recorded: %+v", result.Unparked)
	}
	if len(rec.removed[12680]) != 0 {
		t.Fatalf("removed = %v, want nothing: needs-human keeps the issue at %s", rec.removed[12680], NeedsDecisionStageLabel)
	}
	if len(rec.added[12680]) != 1 || rec.added[12680][0] != HumanAckLabel {
		t.Fatalf("added = %v, want only %s", rec.added[12680], HumanAckLabel)
	}
	var accepted string
	for _, body := range rec.posted[12680] {
		if strings.Contains(body, "Un-parked by @owner") {
			accepted = body
		}
	}
	if accepted == "" || !strings.Contains(accepted, "`needs-human` stays (added by @owner)") {
		t.Fatalf("accepted reply must say needs-human stays, got %v", rec.posted[12680])
	}

	// What the issue carries after the sweep, as the stage reconciler sees it.
	after := []string{"needs-human", "kind/bug", HumanAckLabel}
	stage, _, reason := IssueStageFor(IssueStageInput{Repo: "org/repo", Number: 12680, Labels: after}, nil)
	if stage != stagelabels.IssueNeedsDecision || reason != "held for a person: needs-human" {
		t.Fatalf("stage after un-park = %s (%s), want 045 while needs-human holds it", stage, reason)
	}
	if !hasHardSuppressIssueLabel(after) {
		t.Fatal("an issue still carrying needs-human must remain hard-suppressed after un-park")
	}
}

// An issue carrying only `needs-human` is a person's to work; `/hive approve`
// records the go-ahead but lifts no park: the Unparked fact makes the stage
// engine re-read the issue, and needs-human derives stage/045 — the stage
// it was missing.
func TestSweepIssueUnparkCommandsNeedsHumanOnlyRecordsApprovalOnly(t *testing.T) {
	issue := parkedIssue(12681, humanComment(32, "owner", "/hive approve"))
	issue.Labels = []wireLabel{{Name: "needs-human"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"owner": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rec.removed[12681]) != 0 {
		t.Fatalf("nothing should be removed, got %v", rec.removed[12681])
	}
	if got := strings.Join(rec.added[12681], ","); got != HumanAckLabel+","+NeedsDecisionStageLabel {
		t.Fatalf("added = %v, want %s then the engine's %s", rec.added[12681], HumanAckLabel, NeedsDecisionStageLabel)
	}
}

// hivecommons/hive#13075 (seen on #12984): an agent parked the issue with
// `needs-human` + `/unclaim`, the engine derived stage/045, the maintainer
// replied `/hive decision …`, the un-park cleared 045 and added
// approved-direction but left the source label, and two minutes later the
// engine re-derived 045 from it — the decision was a silent no-op. The rule
// now lives in stageengine.DecisionLiftsNeedsHuman: the hive's own
// `needs-human` is answered by the maintainer's decision and lifts with the
// park; a person's (#12680) or an unattributed one stays. The stage engine
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
			name:        "person-applied needs-human stays (#12680)",
			command:     "/hive decision Agents stand down",
			commenter:   "owner",
			perms:       map[string]string{"owner": "admin"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), unlabeledAt(2, hiveBot, "needs-human", base.Add(time.Minute)), labeledAt(3, "owner", "needs-human", base.Add(2*time.Minute))},
			wantRemoved: nil,
			wantAdded:   []string{HumanAckLabel},
			wantReply:   "`needs-human` stays (added by @owner)",
			wantUnpark:  true,
		},
		{
			name:        "unattributed needs-human stays",
			command:     "/hive approve",
			commenter:   "maintainer",
			perms:       map[string]string{"maintainer": "write"},
			labels:      []string{"needs-human", NeedsDecisionStageLabel},
			timeline:    nil,
			wantRemoved: nil,
			wantAdded:   []string{HumanAckLabel},
			wantReply:   "`needs-human` stays (Hive could not read who added it)",
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

// The applier classification the un-park rule feeds on: the last `labeled
// needs-human` event decides, a later re-add by a person outranks the
// hive's earlier one, and a missing or unreadable timeline is unknown.
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
		{"person", []unparkWireTimelineEvent{labeledAt(1, "owner", "needs-human", base)}, 0, stageengine.NeedsHumanApplierPerson, false},
		{"person re-added after hive", []unparkWireTimelineEvent{labeledAt(1, hiveBot, "needs-human", base), labeledAt(2, "owner", "needs-human", base.Add(time.Minute))}, 0, stageengine.NeedsHumanApplierPerson, false},
		{"hive re-added after person", []unparkWireTimelineEvent{labeledAt(1, "owner", "needs-human", base), labeledAt(2, hiveBot, "needs-human", base.Add(time.Minute))}, 0, stageengine.NeedsHumanApplierHive, true},
		{"other label only", []unparkWireTimelineEvent{labeledAt(1, hiveBot, "hold", base)}, 0, stageengine.NeedsHumanApplierUnknown, false},
		{"no events", nil, 0, stageengine.NeedsHumanApplierUnknown, false},
		{"timeline unreadable", nil, http.StatusInternalServerError, stageengine.NeedsHumanApplierUnknown, false},
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

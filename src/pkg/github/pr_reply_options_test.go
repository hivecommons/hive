package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestRenderPRReplyOptions(t *testing.T) {
	if got := renderPRReplyOptions(prWait{HiveAuthored: true}); got != "" {
		t.Fatalf("a PR waiting on nobody gets %q", got)
	}

	held := renderPRReplyOptions(prWait{Held: true, Verdict: "requires_human", HeadSHA: "300b43284abcdef", HiveAuthored: true})
	for _, want := range []string{
		"**Your move:** the hive's review (`requires_human` on `" + shortSHA("300b43284abcdef") + "`) holds this pull request for a person.",
		"- `/hive approve` approves the current head and releases the hold.",
		"- `/hive decision <what to change>` sends it back",
		"GitHub **Approve** review",
		"`/hive help`",
	} {
		if !strings.Contains(held, want) {
			t.Errorf("held hive PR options miss %q:\n%s", want, held)
		}
	}
	if strings.Contains(held, "/hive decision approve") {
		t.Errorf("a PR that is not parked is offered the 085 decision: %s", held)
	}

	parked := renderPRReplyOptions(prWait{Parked: true})
	for _, want := range []string{
		"parked for your decision (`" + PRNeedsDecisionStageLabel + "`)",
		"- `/hive decision approve <why>` approves the current head",
		"**Request changes** review. The hive does not push to a PR it did not open.",
	} {
		if !strings.Contains(parked, want) {
			t.Errorf("parked contributor PR options miss %q:\n%s", want, parked)
		}
	}
	if strings.Contains(parked, "`/hive approve`") || strings.Contains(parked, "/hive decision <what to change>") {
		t.Errorf("parked contributor PR is offered a reply that does not apply: %s", parked)
	}

	both := renderPRReplyOptions(prWait{Held: true, Parked: true, HiveAuthored: true})
	if !strings.Contains(both, "`/hive approve`") || !strings.Contains(both, "/hive decision approve <why>") || !strings.Contains(both, "and it is parked for your decision") {
		t.Errorf("held and parked PR options = %s", both)
	}
}

// replyOptionsServer is a small stateful GitHub for one PR: comments the
// sweep posts become part of the listing, as the hive bot's, so a later
// pass edits its own status comment; any other read answers an empty list.
type replyOptionsServer struct {
	mu       sync.Mutex
	labels   []string
	open     bool
	comments []unparkWireComment
	posts    int
	edits    int
	nextID   int64
}

const replyOptionsPR = 546

func (s *replyOptionsServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/repos/org/repo/")
		issuePath := fmt.Sprintf("issues/%d/comments", replyOptionsPR)
		switch {
		case path == "issues" && r.Method == http.MethodGet:
			if !s.open {
				_, _ = w.Write([]byte("[]"))
				return
			}
			labels := make([]wireLabel, 0, len(s.labels))
			for _, l := range s.labels {
				labels = append(labels, wireLabel{Name: l})
			}
			_ = json.NewEncoder(w).Encode([]unparkWireIssue{{
				Number: replyOptionsPR, Title: "fix(telegram): one poller", User: wireUser{Login: "hive[bot]"},
				Labels: labels, PullRequest: &unparkWirePRLink{URL: "https://example.test/pulls/546"},
				Comments: s.comments,
			}})
		case path == issuePath && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(s.comments)
		case path == issuePath && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.nextID++
			s.posts++
			now := time.Now().UTC().Format(time.RFC3339)
			c := unparkWireComment{ID: 7000 + s.nextID, Body: body.Body, User: wireUser{Login: "hive[bot]"}, CreatedAt: now, UpdatedAt: now}
			s.comments = append(s.comments, c)
			_ = json.NewEncoder(w).Encode(c)
		case strings.HasPrefix(path, "issues/comments/") && r.Method == http.MethodPatch:
			var id int64
			fmt.Sscanf(path, "issues/comments/%d", &id)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i := range s.comments {
				if s.comments[i].ID == id {
					s.comments[i].Body = body.Body
				}
			}
			s.edits++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("[]"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// section returns the "Your move" section of the PR's status comment.
func (s *replyOptionsServer) section() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.comments {
		if prstatus.IsStatusComment(c.Body) {
			text, _ := prstatus.Get(c.Body, prstatus.KeyReplyOptions)
			return text
		}
	}
	return ""
}

func (s *replyOptionsServer) say(login, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	now := time.Now().UTC().Format(time.RFC3339)
	s.comments = append(s.comments, unparkWireComment{ID: 7000 + s.nextID, Body: body, User: wireUser{Login: login}, CreatedAt: now, UpdatedAt: now})
}

func (s *replyOptionsServer) set(labels []string, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels, s.open = labels, open
}

func newReplyOptionsClient(t *testing.T, s *replyOptionsServer, held bool) *Client {
	t.Helper()
	server := httptest.NewServer(s.handler(t))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")
	recorded := time.Now().Add(-time.Hour)
	c.SetMergeReviewVerdictGate(func(string, int) ReviewVerdictHold {
		if !held {
			return ReviewVerdictHold{}
		}
		return ReviewVerdictHold{Held: true, Verdict: "requires_human", HeadSHA: "300b43284abcdef", RecordedAt: recorded}
	}, trustedMergerSet("danathar"))
	return c
}

func sweepReplyOptions(t *testing.T, c *Client, on bool) {
	t.Helper()
	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		HumanChangeRequests: review.NewHumanChangeRequests(""),
		TrustedMerger:       trustedMergerSet("danathar"),
		PRReplyOptions:      on,
	}); err != nil {
		t.Fatal(err)
	}
}

// Danathar/tududi#546: a hive PR at 070 that the review holds for a person
// said "review and approve it" and named no reply. The status comment now
// lists `/hive approve`, and the section goes away once a trusted merger
// replies with it.
func TestPRReplyOptionsListTheReleaseUntilItIsGiven(t *testing.T) {
	s := &replyOptionsServer{}
	s.set([]string{stagelabels.Fixing.Label(), "author/hive"}, true)
	c := newReplyOptionsClient(t, s, true)

	sweepReplyOptions(t, c, true)
	got := s.section()
	if !strings.Contains(got, "`/hive approve` approves the current head and releases the hold") || !strings.Contains(got, "`/hive decision <what to change>`") {
		t.Fatalf("held PR section = %q", got)
	}
	if s.posts != 1 {
		t.Fatalf("posts = %d, want the one status comment", s.posts)
	}

	// A second pass with nothing changed writes nothing.
	edits := s.edits
	sweepReplyOptions(t, c, true)
	if s.posts != 1 || s.edits != edits {
		t.Fatalf("an unchanged pass wrote: posts=%d edits=%d (was %d)", s.posts, s.edits, edits)
	}

	// A bot cannot release it; the trusted merger can.
	s.say("someone[bot]", ReviewHoldReleaseCommand)
	sweepReplyOptions(t, c, true)
	if s.section() == "" {
		t.Fatal("a bot's /hive approve cleared the section")
	}
	s.say("danathar", ReviewHoldReleaseCommand)
	sweepReplyOptions(t, c, true)
	if got := s.section(); got != "" {
		t.Fatalf("section after the release = %q, want it gone", got)
	}
	if s.posts != 1 {
		t.Fatalf("posts = %d: the release must edit the status comment, not post", s.posts)
	}
}

func TestPRReplyOptionsParkedContributorPR(t *testing.T) {
	s := &replyOptionsServer{}
	s.set([]string{PRNeedsDecisionStageLabel}, true)
	c := newReplyOptionsClient(t, s, false)
	sweepReplyOptions(t, c, true)
	got := s.section()
	if !strings.Contains(got, "`/hive decision approve <why>`") || strings.Contains(got, "`/hive approve`") {
		t.Fatalf("parked PR section = %q", got)
	}
}

// A PR that leaves 085/070, or closes, loses the section at the end of the
// next full pass.
func TestPRReplyOptionsClearedWhenThePRMovesOnOrCloses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		open   bool
	}{
		{"left the stage", []string{"stage/090-merge-ready", "author/hive"}, true},
		{"closed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &replyOptionsServer{}
			s.set([]string{stagelabels.Fixing.Label(), "author/hive"}, true)
			c := newReplyOptionsClient(t, s, true)
			sweepReplyOptions(t, c, true)
			if s.section() == "" {
				t.Fatal("no section written")
			}
			s.set(tc.labels, tc.open)
			sweepReplyOptions(t, c, true)
			if got := s.section(); got != "" {
				t.Fatalf("section after the PR %s = %q", tc.name, got)
			}
		})
	}
}

func TestPRReplyOptionsOffWritesNothing(t *testing.T) {
	s := &replyOptionsServer{}
	s.set([]string{stagelabels.Fixing.Label(), "author/hive"}, true)
	c := newReplyOptionsClient(t, s, true)
	sweepReplyOptions(t, c, false)
	if s.posts != 0 || s.edits != 0 {
		t.Fatalf("PRReplyOptions off wrote: posts=%d edits=%d", s.posts, s.edits)
	}
}

// A PR at 070 that waits on nobody is not written to once its empty state
// is known: no comment list is fetched on every pass for every fixing PR.
func TestPRReplyOptionsNothingToSayIsCheckedOnce(t *testing.T) {
	s := &replyOptionsServer{}
	s.set([]string{stagelabels.Fixing.Label(), "author/hive"}, true)
	c := newReplyOptionsClient(t, s, false)
	sweepReplyOptions(t, c, true)
	sweepReplyOptions(t, c, true)
	if s.posts != 0 || s.edits != 0 {
		t.Fatalf("a PR waiting on nobody was written to: posts=%d edits=%d", s.posts, s.edits)
	}
	if written, known := c.prReplyOptions.state(prReplyOptionsItem(c.getRepos()[0], replyOptionsPR)); !known || written {
		t.Fatalf("state = written %v known %v, want known and empty", written, known)
	}
}

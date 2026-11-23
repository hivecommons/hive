package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

const (
	rtRepo     = "cncf/techdocs"
	rtPR       = 424
	rtOwner    = "quality"
	rtHiveBot  = "hive-app[bot]"
	rtReplyID  = "4616203214"
	rtOtherPR  = 99
	rtCommentA = "6988449875"
	// rtPeerPR is a hive PR another agent (rtPeer) opened.
	rtPeerPR = 500
	rtPeer   = "scanner"
)

// rtFixture is the fake GitHub the gate's own GETs read.
type rtFixture struct {
	now      time.Time
	comments map[string]string // url suffix -> JSON
	lookups  []string
	tokens   []string
	failAll  bool
}

func (f *rtFixture) lookup(url, token string) ([]byte, error) {
	f.lookups = append(f.lookups, url)
	f.tokens = append(f.tokens, token)
	if f.failAll {
		return nil, errors.New("GitHub answered 502")
	}
	for suffix, body := range f.comments {
		if strings.HasSuffix(url, suffix) {
			return []byte(body), nil
		}
	}
	return nil, errors.New("GitHub answered 404")
}

func newRTFixture(now time.Time) *rtFixture {
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	issueURL := func(n int) string { return fmt.Sprintf("https://api.github.com/repos/cncf/techdocs/issues/%d", n) }
	pullURL := func(n int) string { return fmt.Sprintf("https://api.github.com/repos/cncf/techdocs/pulls/%d", n) }
	return &rtFixture{now: now, comments: map[string]string{
		// A person's review comment on the hive's PR, and one on another PR.
		"/repos/cncf/techdocs/pulls/comments/" + rtReplyID: fmt.Sprintf(`{"user":{"login":"nate"},"created_at":%q,"pull_request_url":"https://api.github.com/repos/cncf/techdocs/pulls/%d"}`, at(time.Hour), rtPR),
		"/repos/cncf/techdocs/pulls/comments/777":          fmt.Sprintf(`{"user":{"login":"nate"},"created_at":%q,"pull_request_url":"https://api.github.com/repos/cncf/techdocs/pulls/%d"}`, at(time.Hour), rtOtherPR),
		"/repos/cncf/techdocs/pulls/comments/778":          `{"user":{"login":"nate"},"created_at":"2026-01-01T00:00:00Z"}`,
		// The hive's own probe comment on rtOwner's PR, two minutes old.
		"/repos/cncf/techdocs/issues/comments/" + rtCommentA: fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"issue_url":%q}`, rtHiveBot, at(2*time.Minute), issueURL(rtPR)),
		// The hive's own comment on rtOwner's PR, but an hour old.
		"/repos/cncf/techdocs/issues/comments/100": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"issue_url":%q}`, rtHiveBot, at(time.Hour), issueURL(rtPR)),
		// A person's comment on rtOwner's PR, a minute old.
		"/repos/cncf/techdocs/issues/comments/101": fmt.Sprintf(`{"user":{"login":"nate"},"created_at":%q,"issue_url":%q}`, at(time.Minute), issueURL(rtPR)),
		// The hive's own review comment on rtOwner's PR, a minute old.
		"/repos/cncf/techdocs/pulls/comments/102": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"pull_request_url":%q}`, rtHiveBot, at(time.Minute), pullURL(rtPR)),
		// Hive-bot comments, a minute old, on a PR another agent opened.
		"/repos/cncf/techdocs/issues/comments/105": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"issue_url":%q}`, rtHiveBot, at(time.Minute), issueURL(rtPeerPR)),
		"/repos/cncf/techdocs/pulls/comments/106":  fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"pull_request_url":%q}`, rtHiveBot, at(time.Minute), pullURL(rtPeerPR)),
		// A hive-bot comment, a minute old, on a PR the hive did not open.
		"/repos/cncf/techdocs/issues/comments/107": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"issue_url":%q}`, rtHiveBot, at(time.Minute), issueURL(rtOtherPR)),
		// A hive-bot comment, a minute old, whose PR GitHub does not say.
		"/repos/cncf/techdocs/issues/comments/108": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q}`, rtHiveBot, at(time.Minute)),
		"/repos/cncf/techdocs/pulls/comments/109":  fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"pull_request_url":"https://api.github.com/repos/cncf/techdocs/pulls/x"}`, rtHiveBot, at(time.Minute)),
		// The hive's comment dated in the future (clock trouble): unknown.
		"/repos/cncf/techdocs/issues/comments/103": fmt.Sprintf(`{"user":{"login":%q},"created_at":%q}`, rtHiveBot, at(-time.Hour)),
		// No author at all.
		"/repos/cncf/techdocs/issues/comments/104": `{"created_at":"2026-01-01T00:00:00Z"}`,
	}}
}

func rtOwnPR(repo string, number int) (string, bool) {
	if strings.EqualFold(repo, rtRepo) && number == rtPR {
		return rtOwner, true
	}
	if strings.EqualFold(repo, rtRepo) && number == rtPeerPR {
		return rtPeer, true
	}
	return "", false
}

func newRTProxy(t *testing.T, f *rtFixture, refused *[]ReviewWriteRefusal) *GitHubProxy {
	t.Helper()
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.referenceLookup = f.lookup
	p.SetReviewThreadGuard(ReviewThreadGuard{
		OwnPR:      rtOwnPR,
		HiveLogins: func() []string { return []string{rtHiveBot} },
		OnRefused: func(r ReviewWriteRefusal) {
			if refused != nil {
				*refused = append(*refused, r)
			}
		},
		Now: func() time.Time { return f.now },
	})
	return p
}

func rtReq(method, path, body string) *http.Request {
	req, _ := http.NewRequest(method, "https://api.github.com"+path, strings.NewReader(body))
	req.Host = "api.github.com"
	return req
}

func TestReviewThreadRefusal_Matrix(t *testing.T) {
	replyPath := "/repos/cncf/techdocs/pulls/comments/" + rtReplyID + "/replies"
	cases := []struct {
		name       string
		agent      string
		method     string
		path       string
		wantRefuse bool
		wantWhy    string
	}{
		// Replies and PR review comments: the hive's PR, the agent that opened it.
		{"reply on hive PR by owning agent", rtOwner, "POST", replyPath, false, ""},
		{"reply owner match is case-insensitive", "Quality", "POST", replyPath, false, ""},
		{"reply via the numbered documented path", rtOwner, "POST", "/repos/cncf/techdocs/pulls/424/comments/" + rtReplyID + "/replies", false, ""},
		{"numbered path resolves the PR from the comment, not the path", rtOwner, "POST", "/repos/cncf/techdocs/pulls/424/comments/777/replies", true, "was not opened by the hive's own agent"},
		{"PR review comment by owning agent", rtOwner, "POST", "/repos/cncf/techdocs/pulls/424/comments", false, ""},
		{"reply by another agent", "scanner", "POST", replyPath, true, "opened by agent quality, not by scanner"},
		{"PR review comment by another agent", "scanner", "POST", "/repos/cncf/techdocs/pulls/424/comments", true, "not by scanner"},
		{"reply on a PR the hive did not open", rtOwner, "POST", "/repos/cncf/techdocs/pulls/comments/777/replies", true, "was not opened by the hive's own agent"},
		{"PR review comment on a PR the hive did not open", rtOwner, "POST", "/repos/cncf/techdocs/pulls/99/comments", true, "was not opened by the hive's own agent"},
		{"PR review comment on another repo", rtOwner, "POST", "/repos/cncf/toc/pulls/424/comments", true, "was not opened by the hive's own agent"},
		{"reply whose comment cannot be found", rtOwner, "POST", "/repos/cncf/techdocs/pulls/comments/5/replies", true, "could not resolve"},
		{"reply whose comment has no PR url", rtOwner, "POST", "/repos/cncf/techdocs/pulls/comments/778/replies", true, "no pull_request_url"},
		{"unidentified caller", "", "POST", replyPath, true, "could not be identified"},
		// Deletes: the hive's own comment, inside the window, on the caller's own PR only.
		{"delete own recent issue comment on own PR", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/" + rtCommentA, false, ""},
		{"delete own recent review comment on own PR", rtOwner, "DELETE", "/repos/cncf/techdocs/pulls/comments/102", false, ""},
		{"delete hive-bot issue comment on own PR by another agent", rtPeer, "DELETE", "/repos/cncf/techdocs/issues/comments/" + rtCommentA, true, "opened by agent quality, not by scanner"},
		{"delete hive-bot review comment on own PR by another agent", rtPeer, "DELETE", "/repos/cncf/techdocs/pulls/comments/102", true, "opened by agent quality, not by scanner"},
		{"delete hive-bot issue comment on a PR another agent opened", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/105", true, "opened by agent scanner, not by quality"},
		{"delete hive-bot review comment on a PR another agent opened", rtOwner, "DELETE", "/repos/cncf/techdocs/pulls/comments/106", true, "opened by agent scanner, not by quality"},
		{"delete hive-bot comment on a PR the hive did not open", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/107", true, "was not opened by the hive's own agent"},
		{"delete hive-bot comment with no issue_url", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/108", true, "no issue_url"},
		{"delete hive-bot review comment with unreadable pull_request_url", rtOwner, "DELETE", "/repos/cncf/techdocs/pulls/comments/109", true, "unreadable pull_request_url"},
		{"delete own old comment", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/100", true, "past the 15m0s retract window"},
		{"delete someone else's comment", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/101", true, "written by nate, not by the hive"},
		{"delete comment dated in the future", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/103", true, "no usable creation time"},
		{"delete comment with no author", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/104", true, "could not confirm"},
		{"delete unknown comment", rtOwner, "DELETE", "/repos/cncf/techdocs/issues/comments/1", true, "could not confirm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRTFixture(time.Now())
			p := newRTProxy(t, f, nil)
			reason, refused := p.reviewThreadRefusal(rtReq(tc.method, tc.path, `{"body":"x"}`), tc.agent)
			if refused != tc.wantRefuse {
				t.Fatalf("refused=%v want %v (reason %q)", refused, tc.wantRefuse, reason)
			}
			if tc.wantRefuse {
				for _, want := range []string{tc.wantWhy, "NEVER post a test or probe comment", "Do NOT retry"} {
					if !strings.Contains(reason, want) {
						t.Fatalf("reason missing %q: %q", want, reason)
					}
				}
			}
		})
	}
}

func TestReviewThreadRefusal_UnguardedRoutesUntouched(t *testing.T) {
	f := newRTFixture(time.Now())
	p := newRTProxy(t, f, nil)
	for _, rt := range []struct{ method, path string }{
		{"POST", "/repos/cncf/techdocs/issues/424/comments"},
		{"POST", "/repos/cncf/techdocs/pulls/424/reviews"},
		{"PATCH", "/repos/cncf/techdocs/issues/comments/1"},
		{"GET", "/repos/cncf/techdocs/pulls/comments/1"},
		{"DELETE", "/repos/cncf/techdocs/issues/424"},
		{"POST", "/repos/cncf/techdocs/pulls/0/comments"},
	} {
		if reason, refused := p.reviewThreadRefusal(rtReq(rt.method, rt.path, ""), rtOwner); refused {
			t.Errorf("%s %s must be left to the rest of the chain, got %q", rt.method, rt.path, reason)
		}
	}
	if len(f.lookups) != 0 {
		t.Fatalf("unguarded routes must not cost a lookup: %v", f.lookups)
	}
}

func TestReviewThreadRefusal_NoGuardRefusesEverything(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, rt := range []struct{ method, path string }{
		{"POST", "/repos/cncf/techdocs/pulls/comments/1/replies"},
		{"POST", "/repos/cncf/techdocs/pulls/424/comments"},
		{"DELETE", "/repos/cncf/techdocs/issues/comments/1"},
	} {
		reason, refused := p.reviewThreadRefusal(rtReq(rt.method, rt.path, ""), rtOwner)
		if !refused || !strings.Contains(reason, "no record of which PRs") {
			t.Errorf("%s %s with no guard: refused=%v reason=%q", rt.method, rt.path, refused, reason)
		}
	}
}

func TestReviewThreadRefusal_LookupFailureFailsClosed(t *testing.T) {
	f := newRTFixture(time.Now())
	f.failAll = true
	p := newRTProxy(t, f, nil)
	for _, path := range []string{"/repos/cncf/techdocs/pulls/comments/" + rtReplyID + "/replies", "/repos/cncf/techdocs/issues/comments/" + rtCommentA} {
		method := "POST"
		if strings.Contains(path, "issues/comments") {
			method = "DELETE"
		}
		if _, refused := p.reviewThreadRefusal(rtReq(method, path, ""), rtOwner); !refused {
			t.Errorf("%s %s must refuse when GitHub cannot be read", method, path)
		}
	}
}

// TestOwnCommentDelete_ScopedToCallersOwnPR asserts the delete invariant
// (maintainer decision on #19102): a hive-bot comment may be deleted only by
// the agent that opened the PR it sits on, only inside
// OwnCommentDeleteWindow, and every lookup failure refuses.
func TestOwnCommentDelete_ScopedToCallersOwnPR(t *testing.T) {
	const ownPath = "/repos/cncf/techdocs/issues/comments/" + rtCommentA
	t.Run("lookup failure refuses every caller", func(t *testing.T) {
		f := newRTFixture(time.Now())
		f.failAll = true
		p := newRTProxy(t, f, nil)
		for _, caller := range []string{rtOwner, rtPeer} {
			for _, path := range []string{ownPath, "/repos/cncf/techdocs/pulls/comments/102"} {
				reason, refused := p.reviewThreadRefusal(rtReq("DELETE", path, ""), caller)
				if !refused || !strings.Contains(reason, "could not confirm") {
					t.Fatalf("%s DELETE %s on lookup failure: refused=%v reason=%q", caller, path, refused, reason)
				}
			}
		}
	})
	t.Run("ownership read from the comment, not from the guard's first PR", func(t *testing.T) {
		f := newRTFixture(time.Now())
		p := newRTProxy(t, f, nil)
		if _, refused := p.reviewThreadRefusal(rtReq("DELETE", "/repos/cncf/techdocs/issues/comments/105", ""), rtPeer); refused {
			t.Fatal("the agent that opened the PR the comment sits on may retract it")
		}
		if _, refused := p.reviewThreadRefusal(rtReq("DELETE", "/repos/cncf/techdocs/issues/comments/105", ""), rtOwner); !refused {
			t.Fatal("a hive-bot comment on another agent's PR must be refused")
		}
	})
	t.Run("age limit is OwnCommentDeleteWindow", func(t *testing.T) {
		const margin = time.Second
		for _, tc := range []struct {
			age        time.Duration
			wantRefuse bool
		}{
			{OwnCommentDeleteWindow - margin, false},
			{OwnCommentDeleteWindow + margin, true},
		} {
			f := newRTFixture(time.Now())
			f.comments[ownPath] = fmt.Sprintf(`{"user":{"login":%q},"created_at":%q,"issue_url":"https://api.github.com/repos/cncf/techdocs/issues/%d"}`, rtHiveBot, f.now.Add(-tc.age).UTC().Format(time.RFC3339Nano), rtPR)
			p := newRTProxy(t, f, nil)
			reason, refused := p.reviewThreadRefusal(rtReq("DELETE", ownPath, ""), rtOwner)
			if refused != tc.wantRefuse {
				t.Fatalf("age %s: refused=%v want %v (%q)", tc.age, refused, tc.wantRefuse, reason)
			}
		}
	})
}

func TestReviewThreadRefusal_ReferenceRepoLoginCountsAsHive(t *testing.T) {
	f := newRTFixture(time.Now())
	f.comments["/repos/cncf/techdocs/issues/comments/200"] = fmt.Sprintf(`{"user":{"login":"hive-owner"},"created_at":%q,"issue_url":"https://api.github.com/repos/cncf/techdocs/issues/%d"}`, f.now.Add(-time.Minute).UTC().Format(time.RFC3339), rtPR)
	p := newRTProxy(t, f, nil)
	pol := ReferenceRepoPolicy{Repo: rtRepo, Issues: true, PRs: true, Login: "hive-owner", Token: func() (string, bool) { return "ref-tok", true }}
	p.SetReferenceRepos([]ReferenceRepoPolicy{pol})
	if reason, refused := p.reviewThreadRefusal(rtReq("DELETE", "/repos/cncf/techdocs/issues/comments/200", ""), rtOwner); refused {
		t.Fatalf("the reference repo's write login is the hive's identity there: %q", reason)
	}
	if got := f.tokens[len(f.tokens)-1]; got != "ref-tok" {
		t.Fatalf("lookup on a reference repo must use its write token, got %q", got)
	}
}

func TestReviewLookupToken_Preference(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := rtReq("POST", "/repos/o/r/pulls/1/comments", "")
	if got := p.reviewLookupToken(req, rtOwner, "o/r"); got != "" {
		t.Fatalf("no credential anywhere: got %q", got)
	}
	req.Header.Set("Authorization", "Bearer req-tok")
	if got := p.reviewLookupToken(req, rtOwner, "o/r"); got != "req-tok" {
		t.Fatalf("request credential: got %q", got)
	}
	req.Header.Set("Authorization", "token req-tok2")
	if got := p.reviewLookupToken(req, rtOwner, "o/r"); got != "req-tok2" {
		t.Fatalf("token scheme: got %q", got)
	}
	p.agentTokenSource = func(name string) (string, bool) { return "agent-" + name, name == rtOwner }
	if got := p.reviewLookupToken(req, rtOwner, "o/r"); got != "agent-quality" {
		t.Fatalf("hub-held agent token wins over the request's: got %q", got)
	}
	if got := p.reviewLookupToken(req, "scanner", "o/r"); got != "req-tok2" {
		t.Fatalf("never another agent's token: got %q", got)
	}
}

func TestNoteRefusedReviewReply_KeepsDraftForHiveOwnedPROnly(t *testing.T) {
	var refused []ReviewWriteRefusal
	f := newRTFixture(time.Now())
	p := newRTProxy(t, f, &refused)
	draft := `{"body":"Verified; fixed in abc123."}`
	cases := []struct {
		name          string
		method, path  string
		body          string
		wantKept      bool
		wantInReplyTo string
	}{
		{"thread reply on hive PR", "POST", "/repos/cncf/techdocs/pulls/comments/" + rtReplyID + "/replies", draft, true, rtReplyID},
		{"PR review comment with in_reply_to", "POST", "/repos/cncf/techdocs/pulls/424/comments", `{"body":"b","in_reply_to":42}`, true, "42"},
		{"reply on someone else's PR", "POST", "/repos/cncf/techdocs/pulls/comments/777/replies", draft, false, ""},
		{"unresolvable reply", "POST", "/repos/cncf/techdocs/pulls/comments/5/replies", draft, false, ""},
		{"delete is never a draft", "DELETE", "/repos/cncf/techdocs/issues/comments/" + rtCommentA, "", false, ""},
		{"unguarded route", "POST", "/repos/cncf/techdocs/issues/424/comments", draft, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refused = nil
			req := rtReq(tc.method, tc.path, tc.body)
			p.noteRefusedReviewReply(req, "scanner", "refused for test")
			if (len(refused) == 1) != tc.wantKept {
				t.Fatalf("kept=%d want %v", len(refused), tc.wantKept)
			}
			if tc.wantKept {
				r := refused[0]
				if r.Repo != rtRepo || r.Number != rtPR || r.Agent != "scanner" || r.InReplyTo != tc.wantInReplyTo || r.Reason != "refused for test" || r.Body == "" {
					t.Fatalf("refusal = %+v", r)
				}
				rest, _ := io.ReadAll(req.Body)
				if string(rest) != tc.body {
					t.Fatalf("request body must stay readable for the drain, got %q", rest)
				}
			}
		})
	}
}

func TestPeekReviewDraft_EdgeCases(t *testing.T) {
	if b, r := peekReviewDraft(&http.Request{}); b != "" || r != "" {
		t.Fatal("nil body yields nothing")
	}
	req, _ := http.NewRequest("POST", "https://api.github.com/x", bytes.NewReader([]byte("not json")))
	if b, _ := peekReviewDraft(req); b != "" {
		t.Fatal("non-JSON body yields no draft")
	}
	if rest, _ := io.ReadAll(req.Body); string(rest) != "not json" {
		t.Fatalf("body must be restored, got %q", rest)
	}
}

func TestReviewThreadRules_ModeTable(t *testing.T) {
	converse := agent.AgentCapabilities{Converse: true}
	cases := []struct {
		name   string
		mode   agent.AgentMode
		caps   agent.AgentCapabilities
		method string
		path   string
		want   bool
	}{
		{"reply at issues+prs", agent.ModeIssuesAndPRs, agent.AgentCapabilities{}, "POST", "/repos/o/r/pulls/comments/1/replies", true},
		{"reply at issues-only", agent.ModeIssuesOnly, agent.AgentCapabilities{}, "POST", "/repos/o/r/pulls/comments/1/replies", false},
		{"reply with converse", agent.ModeAdvisory, converse, "POST", "/repos/o/r/pulls/comments/1/replies", true},
		{"numbered reply at issues+prs", agent.ModeIssuesAndPRs, agent.AgentCapabilities{}, "POST", "/repos/o/r/pulls/7/comments/1/replies", true},
		{"pr comment at issues+prs", agent.ModeIssuesAndPRs, agent.AgentCapabilities{}, "POST", "/repos/o/r/pulls/7/comments", true},
		{"pr comment at advisory", agent.ModeAdvisory, agent.AgentCapabilities{}, "POST", "/repos/o/r/pulls/7/comments", false},
		{"delete comment at issues-only", agent.ModeIssuesOnly, agent.AgentCapabilities{}, "DELETE", "/repos/o/r/issues/comments/1", true},
		{"delete review comment at issues-only", agent.ModeIssuesOnly, agent.AgentCapabilities{}, "DELETE", "/repos/o/r/pulls/comments/1", true},
		{"delete comment at advisory", agent.ModeAdvisory, converse, "DELETE", "/repos/o/r/issues/comments/1", false},
		{"edit comment stays denied", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{}, "PATCH", "/repos/o/r/pulls/comments/1", false},
	}
	for _, tc := range cases {
		if got := AllowedByModeCaps(tc.mode, tc.caps, tc.method, tc.path); got != tc.want {
			t.Errorf("%s: AllowedByModeCaps=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestReferenceRepoDecision_ReviewThreadRoutes(t *testing.T) {
	both := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, PRs: true}
	ro := ReferenceRepoPolicy{Repo: "hivecommons/docs"}
	cases := []struct {
		name   string
		pol    ReferenceRepoPolicy
		method string
		path   string
		want   referenceDecision
	}{
		{"reply defers to the review-thread gate", both, "POST", "/repos/hivecommons/docs/pulls/comments/5/replies", referenceAllow},
		{"numbered reply defers to the gate", both, "POST", "/repos/hivecommons/docs/pulls/9/comments/5/replies", referenceAllow},
		{"delete issue comment defers to the gate", both, "DELETE", "/repos/hivecommons/docs/issues/comments/5", referenceAllow},
		{"delete review comment defers to the gate", both, "DELETE", "/repos/hivecommons/docs/pulls/comments/5", referenceAllow},
		{"pr review comment keeps the own-item check", both, "POST", "/repos/hivecommons/docs/pulls/9/comments", referenceOwnItem},
		{"read-only still refuses replies", ro, "POST", "/repos/hivecommons/docs/pulls/comments/5/replies", referenceDeny},
		{"read-only still refuses deletes", ro, "DELETE", "/repos/hivecommons/docs/issues/comments/5", referenceDeny},
		{"edit comment stays refused", both, "PATCH", "/repos/hivecommons/docs/issues/comments/5", referenceDeny},
	}
	for _, tc := range cases {
		if got, _, reason := ReferenceRepoDecision(tc.pol, tc.method, tc.path, ""); got != tc.want {
			t.Errorf("%s: decision=%v want %v (%q)", tc.name, got, tc.want, reason)
		}
	}
	if !referenceWriteAllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/hivecommons/docs/pulls/comments/5/replies") {
		t.Error("a reference-repo reply needs only the comment tier")
	}
}

// End to end through the MITM loop: the agent that opened the PR gets its PR
// review comment forwarded; another agent is refused with the review_thread
// reason and its drafted reply is kept for the sweep.
func TestProxyHTTP_ReviewThreadGate(t *testing.T) {
	cases := []struct {
		name      string
		agentName string
		wantCode  int
		wantKept  bool
	}{
		{"owning agent forwarded", rtOwner, http.StatusCreated, false},
		{"other agent refused, draft kept", "scanner", http.StatusForbidden, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var refused []ReviewWriteRefusal
			f := newRTFixture(time.Now())
			p := newTestProxy()
			p.referenceLookup = f.lookup
			p.SetReviewThreadGuard(ReviewThreadGuard{
				OwnPR:     rtOwnPR,
				OnRefused: func(r ReviewWriteRefusal) { refused = append(refused, r) },
				Now:       func() time.Time { return f.now },
			})
			clientConn, proxyClient := net.Pipe()
			upstreamConn, proxyUpstream := net.Pipe()
			defer clientConn.Close()
			defer upstreamConn.Close()
			go p.proxyHTTP(proxyClient, proxyUpstream, tc.agentName, agent.ModeIssuesAndPRs, agent.AgentCapabilities{})
			body := `{"body":"Verified; fixed in abc123.","in_reply_to":` + rtReplyID + `}`
			go func() {
				fmt.Fprintf(clientConn, "POST /repos/cncf/techdocs/pulls/424/comments HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			}()
			if tc.wantCode == http.StatusCreated {
				go answerDrained(upstreamConn, http.StatusCreated)
				_ = clientConn.SetReadDeadline(time.Now().Add(10 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
				if err != nil {
					t.Fatalf("read response: %v", err)
				}
				if resp.StatusCode != tc.wantCode {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
				}
			} else {
				raw := readRefusal(t, clientConn)
				if !strings.Contains(raw, "403") || !strings.Contains(raw, DenyReasonReviewThread) {
					t.Fatalf("want a review_thread 403, got:\n%s", raw)
				}
			}
			if (len(refused) == 1) != tc.wantKept {
				t.Fatalf("kept drafts = %d, want kept=%v", len(refused), tc.wantKept)
			}
			if tc.wantKept && (refused[0].Body != "Verified; fixed in abc123." || refused[0].InReplyTo != rtReplyID) {
				t.Fatalf("draft = %+v", refused[0])
			}
		})
	}
}

// answerDrained is answerOnce for a request with a body: over net.Pipe the
// proxy's body write blocks until it is read, so the body is drained before
// the response is written.
func answerDrained(upstream net.Conn, status int) {
	req, err := http.ReadRequest(bufio.NewReader(upstream))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, req.Body)
	resp := &http.Response{
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header), Body: http.NoBody, Request: req,
	}
	_ = resp.Write(upstream)
	_ = upstream.Close()
}

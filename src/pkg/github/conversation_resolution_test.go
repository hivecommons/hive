package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// Rules endpoint bodies for o/r@main.
const (
	rulesRequireThreadResolution = `[{"type":"deletion"},{"type":"pull_request","parameters":{"required_approving_review_count":0,"required_review_thread_resolution":true}}]`
	rulesNoThreadResolution      = `[{"type":"pull_request","parameters":{"required_approving_review_count":1,"required_review_thread_resolution":false}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"}]}}]`
)

// conversationRuleAPI serves the base branch's rule reads for o/r@main and
// hands GraphQL to the review-thread mock. Any other request fails the test.
type conversationRuleAPI struct {
	mu sync.Mutex
	// rules is the rules endpoint body; rulesCode its status (0 is 200).
	rules     string
	rulesCode int
	// probeCode / probeBody answer protection/required_status_checks; a
	// zero code is GitHub's 404 "Branch not protected".
	probeCode int
	probeBody string
	// protection is the full classic protection body; "" answers 404.
	protection string

	// failReplies answers every thread reply with a GraphQL error.
	failReplies bool
	// ops is every thread write in order: "reply:<id>" or "resolve:<id>".
	ops []string

	ruleReads, probeReads, protectionReads int
}

func (a *conversationRuleAPI) handler(t *testing.T, m *gqlMock) http.HandlerFunc {
	gql := m.handler(t)
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(body, &req)
			id, _ := req.Variables["id"].(string)
			a.mu.Lock()
			fail := false
			switch {
			case strings.Contains(req.Query, "addPullRequestReviewThreadReply("):
				a.ops = append(a.ops, "reply:"+id)
				fail = a.failReplies
			case strings.Contains(req.Query, "resolveReviewThread("):
				a.ops = append(a.ops, "resolve:"+id)
			}
			a.mu.Unlock()
			if fail {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"reply refused"}]}`))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			gql(w, r)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rules/branches/main":
			a.ruleReads++
			if a.rulesCode != 0 {
				w.WriteHeader(a.rulesCode)
			}
			_, _ = w.Write([]byte(a.rules))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/main/protection/required_status_checks":
			a.probeReads++
			if a.probeCode == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Branch not protected"}`))
				return
			}
			w.WriteHeader(a.probeCode)
			_, _ = w.Write([]byte(a.probeBody))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/main/protection":
			a.protectionReads++
			if a.protection == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(a.protection))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

func (a *conversationRuleAPI) opList() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ops...)
}

func (a *conversationRuleAPI) reads() (rules, probe, protection int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ruleReads, a.probeReads, a.protectionReads
}

// readyThreadsClient is a client over api whose PR o/r#9 has threads, with
// every audit entry it writes collected into audits.
func readyThreadsClient(t *testing.T, api *conversationRuleAPI, threads ...*gqlThread) (*Client, *gqlMock, *[]string) {
	t.Helper()
	m := ownThreadsMock(threads...)
	srv := httptest.NewServer(api.handler(t, m))
	t.Cleanup(srv.Close)
	c := reviewThreadTestClient(t, srv.URL, config.ReviewBotsConfig{})
	var mu sync.Mutex
	audits := &[]string{}
	c.SetAttributionAudit(func(action, detail, agent string) {
		mu.Lock()
		defer mu.Unlock()
		*audits = append(*audits, action+" "+detail)
	})
	return c, m, audits
}

func ownLowThread(id string) *gqlThread {
	return &gqlThread{ID: id, Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("**correctness (low):** rename this", "low")}}}
}

func auditsWith(audits []string, parts ...string) int {
	n := 0
	for _, a := range audits {
		all := true
		for _, p := range parts {
			if !strings.Contains(a, p) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// projectbluefin/actions#779: auto-merge off, a ruleset whose pull_request
// rule requires thread resolution, and two of the hive's own low notes. Both
// are resolved, each gets one reply saying why, every write is audited with
// the ready-for-human-merge trigger, and the ruleset answer is enough (no
// classic protection read). A second pass finds nothing left to do and
// reads the rule from the cache.
func TestResolveOwnReviewThreadsForHumanMerge_RulesetRequired(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	legacy := &gqlThread{ID: "T-legacy", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleLow}}}
	c, m, audits := readyThreadsClient(t, api, ownLowThread("T-low"), legacy)
	out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Cleared() || out.Resolved != 2 || out.Unresolved != 2 || out.Skipped != "" {
		t.Fatalf("outcome = %+v, want both threads resolved", out)
	}
	got := append([]string(nil), m.resolved...)
	slices.Sort(got)
	if want := []string{"T-legacy", "T-low"}; !slices.Equal(got, want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	for _, id := range []string{"T-low", "T-legacy"} {
		if r := m.replies[id]; len(r) != 1 || r[0] != ownThreadsReadyReply {
			t.Fatalf("replies in %s = %q, want one reply %q", id, r, ownThreadsReadyReply)
		}
	}
	if strings.ContainsAny(ownThreadsReadyReply, "@–—") {
		t.Fatalf("reply carries a mention or a dash: %q", ownThreadsReadyReply)
	}
	if n := auditsWith(*audits, AuditActionPRReviewed, "state=thread_resolved", "trigger=ready-for-human-merge"); n != 2 {
		t.Fatalf("resolve audits = %d, want 2: %q", n, *audits)
	}
	if n := auditsWith(*audits, AuditActionPRReviewed, "state=thread_replied", "trigger=ready-for-human-merge"); n != 2 {
		t.Fatalf("reply audits = %d, want 2: %q", n, *audits)
	}
	if rules, probe, protection := api.reads(); rules != 1 || probe != 0 || protection != 0 {
		t.Fatalf("rule reads rules=%d probe=%d protection=%d, want the ruleset only", rules, probe, protection)
	}
	// Each thread is replied in first and resolved after, the order a
	// person used on projectbluefin/actions#779.
	if got, want := api.opList(), []string{"reply:T-low", "resolve:T-low", "reply:T-legacy", "resolve:T-legacy"}; !slices.Equal(got, want) {
		t.Fatalf("thread writes = %v, want %v", got, want)
	}

	out, err = c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
	if err != nil || out.Resolved != 0 || out.Unresolved != 0 {
		t.Fatalf("second pass: %+v %v, want nothing left", out, err)
	}
	if len(m.resolved) != 2 || len(m.replies["T-low"]) != 1 || len(m.replies["T-legacy"]) != 1 {
		t.Fatalf("second pass wrote again: resolved=%v replies=%v", m.resolved, m.replies)
	}
	if rules, _, _ := api.reads(); rules != 1 {
		t.Fatalf("rule read %d times, want once (cached per repository and branch)", rules)
	}
}

// Any thread the hive may not resolve leaves every thread open and posts
// nothing: the #19421 rules, unchanged.
func TestResolveOwnReviewThreadsForHumanMerge_OneThreadNeedsAPerson(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other *gqlThread
		check func(OwnReviewThreadsOutcome) bool
	}{
		{"own blocking (medium) finding", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("x may be nil", "medium")}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"own thread a person replied in", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("rename", "low")}, {Author: "author", AuthorType: "User", Body: "keeping it"}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnAnswered == 1 }},
		{"security marker", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked(ownBodyStyleInfo, reviewFindingSeveritySecurity)}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"unmatched marker", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked(ownBodyStyleInfo, reviewFindingSeverityUnmatched)}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"a person's thread", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "maintainer", AuthorType: "User", Body: "Nit: rename."}}},
			func(o OwnReviewThreadsOutcome) bool { return o.Others == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
			c, m, audits := readyThreadsClient(t, api, ownLowThread("T-a"), ownLowThread("T-b"), tc.other)
			out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
			if err != nil {
				t.Fatal(err)
			}
			if out.Resolved != 0 || len(m.resolved) != 0 || len(m.replies) != 0 || len(*audits) != 0 || !out.NeedsPerson() || out.Skipped != "not-every-thread-resolvable" || !tc.check(out) {
				t.Fatalf("outcome = %+v resolved=%v replies=%v audits=%q, want nothing resolved", out, m.resolved, m.replies, *audits)
			}
		})
	}
}

// A base that does not require conversation resolution, or whose rule cannot
// be read, reads no thread at all.
func TestResolveOwnReviewThreadsForHumanMerge_RuleNotRequiredOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  *conversationRuleAPI
		want string
	}{
		{"ruleset without thread resolution, branch unprotected", &conversationRuleAPI{rules: rulesNoThreadResolution}, OwnThreadsSkippedRuleNotRequired},
		{"no rules, branch unprotected", &conversationRuleAPI{rules: `[]`}, OwnThreadsSkippedRuleNotRequired},
		{"classic protection without conversation resolution", &conversationRuleAPI{rules: `[]`, probeCode: http.StatusOK, probeBody: `{"contexts":["build"]}`, protection: `{"required_conversation_resolution":{"enabled":false}}`}, OwnThreadsSkippedRuleNotRequired},
		{"rules unreadable", &conversationRuleAPI{rules: `{"message":"Resource not accessible by integration"}`, rulesCode: http.StatusForbidden}, OwnThreadsSkippedRuleUnknown},
		{"rules not JSON", &conversationRuleAPI{rules: `not json`}, OwnThreadsSkippedRuleUnknown},
		{"classic protection unreadable", &conversationRuleAPI{rules: `[]`, probeCode: http.StatusForbidden, probeBody: `{"message":"Resource not accessible by integration"}`}, OwnThreadsSkippedRuleUnknown},
		{"protected, full read refused", &conversationRuleAPI{rules: `[]`, probeCode: http.StatusOK, probeBody: `{"contexts":[]}`}, OwnThreadsSkippedRuleUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, audits := readyThreadsClient(t, tc.api, ownLowThread("T-a"))
			out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
			if err != nil {
				t.Fatal(err)
			}
			if out.Skipped != tc.want || out.Resolved != 0 || m.queryCount() != 0 || len(*audits) != 0 {
				t.Fatalf("outcome = %+v graphql=%d audits=%q, want skipped %q and no thread read", out, m.queryCount(), *audits, tc.want)
			}
		})
	}
	// No base branch is unknown too, and reads nothing.
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	c, m, _ := readyThreadsClient(t, api, ownLowThread("T-a"))
	if out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, " ", true); err != nil || out.Skipped != OwnThreadsSkippedRuleUnknown || m.queryCount() != 0 {
		t.Fatalf("no base: %+v %v graphql=%d", out, err, m.queryCount())
	}
	if rules, probe, protection := api.reads(); rules+probe+protection != 0 {
		t.Fatalf("no base: rule reads %d/%d/%d, want none", rules, probe, protection)
	}
}

// Classic branch protection's required_conversation_resolution counts as
// well, including on a protected branch with no required checks (the probe's
// 404 that is not "Branch not protected").
func TestResolveOwnReviewThreadsForHumanMerge_ClassicProtection(t *testing.T) {
	for name, api := range map[string]*conversationRuleAPI{
		"required checks":    {rules: `[]`, probeCode: http.StatusOK, probeBody: `{"contexts":["build"]}`, protection: `{"required_conversation_resolution":{"enabled":true}}`},
		"no required checks": {rules: `[]`, probeCode: http.StatusNotFound, probeBody: `{"message":"Required status checks not enabled"}`, protection: `{"required_conversation_resolution":{"enabled":true}}`},
	} {
		t.Run(name, func(t *testing.T) {
			c, m, _ := readyThreadsClient(t, api, ownLowThread("T-a"))
			out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
			if err != nil || !out.Cleared() || len(m.resolved) != 1 || len(m.replies["T-a"]) != 1 {
				t.Fatalf("outcome = %+v %v resolved=%v replies=%v, want the thread resolved", out, err, m.resolved, m.replies)
			}
			if _, probe, protection := api.reads(); probe != 1 || protection != 1 {
				t.Fatalf("probe=%d protection=%d, want one of each", probe, protection)
			}
		})
	}
}

func TestResolveOwnReviewThreadsForHumanMerge_Paused(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	c, m, audits := readyThreadsClient(t, api, ownLowThread("T-a"))
	c.SetRepoPausedFunc(func(string) bool { return true })
	out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
	if err != nil || out.Skipped != "paused" || m.queryCount() != 0 || len(*audits) != 0 {
		t.Fatalf("paused: %+v %v graphql=%d audits=%q", out, err, m.queryCount(), *audits)
	}
	if rules, probe, protection := api.reads(); rules+probe+protection != 0 {
		t.Fatalf("paused: rule reads %d/%d/%d, want none", rules, probe, protection)
	}
}

// The merge-refusal path (#19421) is unchanged by the ready point: it reads
// no rule (GitHub's refusal already said conversations must be resolved),
// posts no reply and keeps its own audit trigger.
func TestResolveOwnReviewThreadsForMerge_NoRuleReadNoReply(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	c, m, audits := readyThreadsClient(t, api, ownLowThread("T-a"), ownLowThread("T-b"))
	out, err := c.ResolveOwnReviewThreadsForMerge(context.Background(), "o/r", 9)
	if err != nil || !out.Cleared() || out.Resolved != 2 {
		t.Fatalf("outcome = %+v %v, want both resolved", out, err)
	}
	if len(m.replies) != 0 {
		t.Fatalf("merge-refusal path replied: %v", m.replies)
	}
	if rules, probe, protection := api.reads(); rules+probe+protection != 0 {
		t.Fatalf("merge-refusal path read the rule: %d/%d/%d", rules, probe, protection)
	}
	if n := auditsWith(*audits, "state=thread_resolved", "trigger=merge-refused-unresolved-conversations"); n != 2 || len(*audits) != 2 {
		t.Fatalf("audits = %q, want two with the merge-refusal trigger", *audits)
	}
}

// Quiet notifications: the threads are resolved, nothing is replied, so a
// 090 that emails nobody stays that way.
func TestResolveOwnReviewThreadsForHumanMerge_QuietPostsNoReply(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	c, m, audits := readyThreadsClient(t, api, ownLowThread("T-a"), ownLowThread("T-b"))
	out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", false)
	if err != nil || !out.Cleared() || len(m.resolved) != 2 {
		t.Fatalf("outcome = %+v %v resolved=%v, want both resolved", out, err, m.resolved)
	}
	if len(m.replies) != 0 || auditsWith(*audits, "state=thread_replied") != 0 {
		t.Fatalf("quiet mode replied: replies=%v audits=%q", m.replies, *audits)
	}
	if n := auditsWith(*audits, "state=thread_resolved", "trigger=ready-for-human-merge"); n != 2 {
		t.Fatalf("resolve audits = %d, want 2: %q", n, *audits)
	}
}

// A reply that fails does not stop the resolve, and is not audited.
func TestResolveOwnReviewThreadsForHumanMerge_ReplyFailureStillResolves(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution, failReplies: true}
	c, m, audits := readyThreadsClient(t, api, ownLowThread("T-a"))
	out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
	if err != nil || !out.Cleared() || !slices.Equal(m.resolved, []string{"T-a"}) {
		t.Fatalf("outcome = %+v %v resolved=%v, want the thread resolved", out, err, m.resolved)
	}
	if got, want := api.opList(), []string{"reply:T-a", "resolve:T-a"}; !slices.Equal(got, want) {
		t.Fatalf("thread writes = %v, want %v", got, want)
	}
	if auditsWith(*audits, "state=thread_replied") != 0 || auditsWith(*audits, "state=thread_resolved") != 1 {
		t.Fatalf("audits = %q, want only the resolve", *audits)
	}
}

// A thread that already carries the hive's reply (a retry after every
// resolve failed, a note a person re-opened) is resolved without a second
// reply.
func TestResolveOwnReviewThreadsForHumanMerge_NoSecondReply(t *testing.T) {
	api := &conversationRuleAPI{rules: rulesRequireThreadResolution}
	replied := ownLowThread("T-a")
	replied.Comments = append(replied.Comments, gqlComment{Author: "hive", AuthorType: "Bot", Body: ownThreadsReadyReply})
	c, m, _ := readyThreadsClient(t, api, replied, ownLowThread("T-b"))
	out, err := c.ResolveOwnReviewThreadsForHumanMerge(context.Background(), "o/r", 9, "main", true)
	if err != nil || !out.Cleared() || len(m.resolved) != 2 {
		t.Fatalf("outcome = %+v %v resolved=%v, want both resolved", out, err, m.resolved)
	}
	if got, want := api.opList(), []string{"resolve:T-a", "reply:T-b", "resolve:T-b"}; !slices.Equal(got, want) {
		t.Fatalf("thread writes = %v, want %v", got, want)
	}
}

// Only a 403 counts as a refused permission; a rate limit or a server error
// does not.
func TestForbiddenRead(t *testing.T) {
	status := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&gh.ErrorResponse{Response: status(http.StatusForbidden), Message: "Resource not accessible by integration"}, true},
		{&gh.ErrorResponse{Response: status(http.StatusInternalServerError)}, false},
		{&gh.ErrorResponse{Response: status(http.StatusNotFound)}, false},
		{&gh.RateLimitError{Response: status(http.StatusForbidden), Message: "API rate limit exceeded"}, false},
		{&gh.AbuseRateLimitError{Response: status(http.StatusForbidden), Message: "secondary rate limit"}, false},
		{nil, false},
	} {
		if got := forbiddenRead(tc.err); got != tc.want {
			t.Errorf("forbiddenRead(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// An unknown answer is cached for the forbidden window when a read was
// refused with 403, for the normal window otherwise, and logged once per
// cache fill, not once per call.
func TestConversationResolutionUnknownCacheAndLog(t *testing.T) {
	for _, tc := range []struct {
		name      string
		api       *conversationRuleAPI
		forbidden bool
	}{
		{"rules refused (403)", &conversationRuleAPI{rules: `{"message":"Resource not accessible by integration"}`, rulesCode: http.StatusForbidden}, true},
		{"classic protection refused (403)", &conversationRuleAPI{rules: `[]`, probeCode: http.StatusForbidden, probeBody: `{"message":"Resource not accessible by integration"}`}, true},
		{"rules not JSON", &conversationRuleAPI{rules: `not json`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := readyThreadsClient(t, tc.api, ownLowThread("T-a"))
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			for i := 0; i < 3; i++ {
				if required, known := c.conversationResolutionRequired(context.Background(), "o", "r", "main"); required || known {
					t.Fatalf("call %d: required=%v known=%v, want unknown", i, required, known)
				}
			}
			if rules, _, _ := tc.api.reads(); rules != 1 {
				t.Fatalf("rules read %d times, want once (cached)", rules)
			}
			if n := strings.Count(logs.String(), "could not read whether the base requires conversation resolution"); n != 1 {
				t.Fatalf("logged %d times, want once:\n%s", n, logs.String())
			}
			cache := c.conversationRules()
			cache.mu.Lock()
			e := cache.entries["o/r@main"]
			cache.mu.Unlock()
			left := time.Until(e.until)
			if tc.forbidden && (left <= conversationRuleTTL || left > conversationRuleForbiddenTTL) {
				t.Fatalf("a 403 unknown is cached for %v, want about %v", left, conversationRuleForbiddenTTL)
			}
			if !tc.forbidden && (left <= 0 || left > conversationRuleTTL) {
				t.Fatalf("an unknown is cached for %v, want at most %v", left, conversationRuleTTL)
			}
		})
	}
}

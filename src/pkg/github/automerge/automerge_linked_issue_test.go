package automerge

// Tests for the linked-issue gate: an open same-repo issue the PR closes that
// is held or parked on a person stops every merge lane. As with the
// human-merge-path tests, a refusal asserts zero merge calls, not merely a
// skip reason.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// linkedIssue is one issue the front serves; status non-zero fails the read.
type linkedIssue struct {
	state  string
	labels []string
	pr     bool
	status int
}

// linkedIssueFront fronts a lane fixture: it rewrites PR 7's body (when
// body is set) and serves GET for the issues in issues, proxying everything
// else to the fixture so each lane's existing fake drives the merge path.
type linkedIssueFront struct {
	mu     sync.Mutex
	body   string
	issues map[string]linkedIssue // "owner/repo#N"
	reads  map[string]int
	// commits are the messages served for PR 7's commit list (nil leaves
	// the request to the fixture); commitsStatus non-zero fails the read.
	commits       []string
	commitsStatus int
	commitReads   int
	// baseRef and defaultBranch, when set, are written into PR 7's base
	// (base.ref and base.repo.default_branch) alongside body.
	baseRef       string
	defaultBranch string
}

func (f *linkedIssueFront) setBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *linkedIssueFront) currentBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body
}

func (f *linkedIssueFront) set(key string, issue linkedIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[key] = issue
}

func (f *linkedIssueFront) readsOf(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[key]
}

func newLinkedIssueFront(t *testing.T, backend string, f *linkedIssueFront) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	if f.issues == nil {
		f.issues = map[string]linkedIssue{}
	}
	f.reads = map[string]int{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A named base ref makes the sweep read the base branch (protection
		// for the required-check set, tip status for the release hold); the
		// backend fixture only knows PR 7's head, so answer those here.
		f.mu.Lock()
		baseRef := f.baseRef
		f.mu.Unlock()
		if baseRef != "" && r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/repos/acme/widget/branches":
				// A "vN" base makes the forward-merge guard list protected
				// release lines (HeadMergesReleaseLine); none here.
				json.NewEncoder(w).Encode([]any{})
				return
			case "/repos/acme/widget/branches/" + baseRef:
				json.NewEncoder(w).Encode(map[string]any{"name": baseRef, "commit": map[string]string{"sha": "base1"}})
				return
			case "/repos/acme/widget/branches/" + baseRef + "/protection/required_status_checks":
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"message": "Branch not protected"})
				return
			case "/repos/acme/widget/commits/base1/status":
				json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
				return
			}
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/commits" {
			f.mu.Lock()
			messages, status := f.commits, f.commitsStatus
			if messages != nil || status != 0 {
				f.commitReads++
			}
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]string{"message": "commit list error"})
				return
			}
			if messages != nil {
				out := make([]map[string]any, 0, len(messages))
				for i, m := range messages {
					out = append(out, map[string]any{"sha": "c" + strconv.Itoa(i), "commit": map[string]any{"message": m}})
				}
				json.NewEncoder(w).Encode(out)
				return
			}
		}
		if body := f.currentBody(); r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7" && body != "" {
			resp, err := http.Get(backend + r.URL.Path)
			if err != nil {
				t.Errorf("backend PR fetch: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			var pull map[string]any
			if err := json.Unmarshal(raw, &pull); err != nil {
				t.Errorf("decode backend PR: %v", err)
			}
			pull["body"] = body
			f.mu.Lock()
			baseRef, defaultBranch := f.baseRef, f.defaultBranch
			f.mu.Unlock()
			if baseRef != "" || defaultBranch != "" {
				base, _ := pull["base"].(map[string]any)
				if base == nil {
					base = map[string]any{}
				}
				if baseRef != "" {
					base["ref"] = baseRef
				}
				if defaultBranch != "" {
					repo, _ := base["repo"].(map[string]any)
					if repo == nil {
						repo = map[string]any{}
					}
					repo["default_branch"] = defaultBranch
					base["repo"] = repo
				}
				pull["base"] = base
			}
			json.NewEncoder(w).Encode(pull)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
			if len(parts) == 4 && parts[2] == "issues" {
				key := parts[0] + "/" + parts[1] + "#" + parts[3]
				f.mu.Lock()
				issue, ok := f.issues[key]
				if ok {
					f.reads[key]++
				}
				f.mu.Unlock()
				if ok {
					if issue.status != 0 {
						w.WriteHeader(issue.status)
						json.NewEncoder(w).Encode(map[string]string{"message": "issue read error"})
						return
					}
					labels := make([]map[string]string, 0, len(issue.labels))
					for _, l := range issue.labels {
						labels = append(labels, map[string]string{"name": l})
					}
					n, _ := strconv.Atoi(parts[3])
					out := map[string]any{"number": n, "state": issue.state, "labels": labels}
					if issue.pr {
						out["pull_request"] = map[string]any{"url": "https://api.example/pr/" + parts[3]}
					}
					json.NewEncoder(w).Encode(out)
					return
				}
			}
		}
		proxy.ServeHTTP(w, r)
	}))
}

// linkedIssueCase is one row of the cross-lane table: the issue state behind
// "Fixes #42" and what the lane must do with it.
type linkedIssueCase struct {
	name       string
	issue      linkedIssue
	wantReason string
	wantErr    bool
}

var linkedIssueCases = []linkedIssueCase{
	{name: "hold", issue: linkedIssue{state: "open", labels: []string{"bug", "hold"}}, wantReason: "linked-issue:hold:#42:hold"},
	{name: "on-hold", issue: linkedIssue{state: "open", labels: []string{"on-hold"}}, wantReason: "linked-issue:hold:#42:on-hold"},
	{name: "do-not-merge", issue: linkedIssue{state: "open", labels: []string{"do-not-merge/hold"}}, wantReason: "linked-issue:hold:#42:do-not-merge/hold"},
	{name: "stage/045", issue: linkedIssue{state: "open", labels: []string{"stage/045-needs-decision"}}, wantReason: "linked-issue:needs-decision:#42:stage/045-needs-decision"},
	{name: "needs-human", issue: linkedIssue{state: "open", labels: []string{"Needs-Human"}}, wantReason: "linked-issue:needs-decision:#42:Needs-Human"},
	{name: "retired needs-decision", issue: linkedIssue{state: "open", labels: []string{"needs-decision"}}, wantReason: "linked-issue:needs-decision:#42:needs-decision"},
	{name: "hold outranks park", issue: linkedIssue{state: "open", labels: []string{"needs-human", "hold"}}, wantReason: "linked-issue:hold:#42:hold"},
	{name: "read error fails closed", issue: linkedIssue{status: http.StatusInternalServerError}, wantReason: linkedIssueReasonCheck, wantErr: true},
	{name: "closed issue does not block", issue: linkedIssue{state: "closed", labels: []string{"hold"}}},
	{name: "missing issue does not block", issue: linkedIssue{status: http.StatusNotFound}},
	// "Fixes #42" where #42 is an open pull request would close that PR on
	// merge (hivecommons/hive#15025), whatever its labels; the issue read
	// above it is not repeated. Closing a closed PR is a no-op.
	{name: "open pull request target holds the merge", issue: linkedIssue{state: "open", labels: []string{"hold"}, pr: true}, wantReason: "closing-keyword-targets-pr:#42"},
	{name: "closed pull request target does not block", issue: linkedIssue{state: "closed", labels: []string{"hold"}, pr: true}},
	{name: "unlabelled issue does not block", issue: linkedIssue{state: "open", labels: []string{"bug"}}},
}

// linkedIssueBody closes acme/widget#42, and a held issue in another repo
// that must never be read.
const linkedIssueBody = "Fixes #42\nFixes acme/other#9\n\nSome change."

func newLinkedIssueFrontFor(t *testing.T, backend string, issue linkedIssue) (*linkedIssueFront, *httptest.Server) {
	t.Helper()
	front := &linkedIssueFront{body: linkedIssueBody, issues: map[string]linkedIssue{
		"acme/widget#42": issue,
		"acme/other#9":   {state: "open", labels: []string{"hold"}},
	}}
	return front, newLinkedIssueFront(t, backend, front)
}

func checkLinkedIssueOutcome(t *testing.T, tc linkedIssueCase, front *linkedIssueFront, reason string, err error, merges int) {
	t.Helper()
	if (err != nil) != tc.wantErr {
		t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
	}
	if reason != tc.wantReason {
		t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
	}
	wantMerges := 0
	if tc.wantReason == "" {
		wantMerges = 1
	}
	if merges != wantMerges {
		t.Fatalf("merges = %d, want %d", merges, wantMerges)
	}
	if got := front.readsOf("acme/widget#42"); got != 1 {
		t.Fatalf("linked issue reads = %d, want 1", got)
	}
	if got := front.readsOf("acme/other#9"); got != 0 {
		t.Fatalf("cross-repo issue reads = %d, want 0", got)
	}
}

func TestSelfAuthoredSweepLinkedIssueGate(t *testing.T) {
	for _, tc := range linkedIssueCases {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front, srv := newLinkedIssueFrontFor(t, api.URL, tc.issue)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			checkLinkedIssueOutcome(t, tc, front, reason, err, merges)
		})
	}
}

func TestTrustedAuthorSweepLinkedIssueGate(t *testing.T) {
	for _, tc := range linkedIssueCases {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			front, srv := newLinkedIssueFrontFor(t, api.URL, tc.issue)
			defer srv.Close()

			policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
			engine := newTrustedSweepEngine(srv.URL, policy, Options{})
			_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			checkLinkedIssueOutcome(t, tc, front, reason, err, fx.merges)
		})
	}
}

func TestQueuedSweepLinkedIssueGate(t *testing.T) {
	for _, tc := range linkedIssueCases {
		t.Run(tc.name, func(t *testing.T) {
			var merged []int
			api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
				number:          7,
				author:          "alice",
				queuedBy:        "bob",
				label:           true,
				mergeableState:  "clean",
				statusState:     "success",
				checkStatus:     "completed",
				checkConclusion: "success",
			}}, &merged)
			defer api.Close()
			front, srv := newLinkedIssueFrontFor(t, api.URL, tc.issue)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
			checkLinkedIssueOutcome(t, tc, front, reason, err, len(merged))
		})
	}
}

func TestContributorLaneLinkedIssueGate(t *testing.T) {
	for _, tc := range linkedIssueCases {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			front, srv := newLinkedIssueFrontFor(t, api.URL, tc.issue)
			defer srv.Close()

			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(srv.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			checkLinkedIssueOutcome(t, tc, front, reason, err, fx.merges)
		})
	}
}

// The gate keeps no state: once a person removes the label from the issue
// the next sweep merges.
func TestLinkedIssueGateReleasesWhenLabelRemoved(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front, srv := newLinkedIssueFrontFor(t, api.URL, linkedIssue{state: "open", labels: []string{"hold"}})
	defer srv.Close()

	c := newAutoMergeSweepClient(srv.URL)
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "linked-issue:hold:#42:hold" || merges != 0 {
		t.Fatalf("held tick: reason = %q, err = %v, merges = %d", reason, err, merges)
	}
	front.set("acme/widget#42", linkedIssue{state: "open"})
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != "" || merges != 1 || event.MergeSHA != "merge7" {
		t.Fatalf("released tick: reason = %q, err = %v, merges = %d, event = %+v; want a merge", reason, err, merges, event)
	}
}

// The configured dashboard hold (hive-pause/<id>) on the issue counts, via
// the transport's hold predicate.
func TestLinkedIssueGateHonorsHivePauseHold(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	_, srv := newLinkedIssueFrontFor(t, api.URL, linkedIssue{state: "open", labels: []string{"hive-pause/hive-a"}})
	defer srv.Close()

	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	client.SetHoldLabels([]string{"hive-pause/hive-a"})
	c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker()})
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != "linked-issue:hold:#42:hive-pause/hive-a" || merges != 0 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want hive-pause hold", reason, err, merges)
	}
}

func TestLinkedIssueDecisionCauseNamesIssueAndLabel(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   []string
	}{
		{reason: prstage.LinkedIssueReason(linkedIssueReasonHoldPrefix, 551, "hold"), want: []string{"issue #551", "on hold", "remove `hold` from #551"}},
		{reason: prstage.LinkedIssueReason(linkedIssueReasonNeedsDecisionPrefix, 551, "stage/045-needs-decision"), want: []string{"issue #551", "parked for a decision", "remove `stage/045-needs-decision` from #551"}},
	} {
		got := decisionCause(tc.reason)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("decisionCause(%q) = %q, want it to contain %q", tc.reason, got, w)
			}
		}
	}
}

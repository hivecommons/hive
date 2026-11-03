package automerge

// Tests for the closing-link gates (hivecommons/hive#15025): a PR body that
// closes an issue it says stays open, and a closing keyword that names
// another pull request. As with the linked-issue tests, a refusal asserts
// zero merge calls, not merely a skip reason.

import (
	"context"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// closingLinkLane builds one merge lane's fixture behind front and returns a
// tick that evaluates PR 7 once, and the lane's merge count.
type closingLinkLane struct {
	name  string
	build func(t *testing.T, front *linkedIssueFront) (tick func() (string, error), merges func() int)
}

var closingLinkLanes = []closingLinkLane{
	{name: "self-authored", build: func(t *testing.T, front *linkedIssueFront) (func() (string, error), func() int) {
		merges := 0
		api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
		t.Cleanup(api.Close)
		srv := newLinkedIssueFront(t, api.URL, front)
		t.Cleanup(srv.Close)
		c := newAutoMergeSweepClient(srv.URL)
		return func() (string, error) {
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			return reason, err
		}, func() int { return merges }
	}},
	{name: "trusted-author", build: func(t *testing.T, front *linkedIssueFront) (func() (string, error), func() int) {
		fx := &trustedSweepFixture{}
		api := newTrustedSweepAPI(t, fx)
		t.Cleanup(api.Close)
		srv := newLinkedIssueFront(t, api.URL, front)
		t.Cleanup(srv.Close)
		policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
		engine := newTrustedSweepEngine(srv.URL, policy, Options{})
		return func() (string, error) {
			_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			return reason, err
		}, func() int { return fx.merges }
	}},
	{name: "queued", build: func(t *testing.T, front *linkedIssueFront) (func() (string, error), func() int) {
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
		t.Cleanup(api.Close)
		srv := newLinkedIssueFront(t, api.URL, front)
		t.Cleanup(srv.Close)
		c := newAutoMergeSweepClient(srv.URL)
		return func() (string, error) {
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
			return reason, err
		}, func() int { return len(merged) }
	}},
	{name: "contributor", build: func(t *testing.T, front *linkedIssueFront) (func() (string, error), func() int) {
		fx := validContributorFixture()
		api := newContributorLaneAPI(t, &fx)
		t.Cleanup(api.Close)
		srv := newLinkedIssueFront(t, api.URL, front)
		t.Cleanup(srv.Close)
		policy := defaultContributorLanePolicy()
		clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
		engine := newContributorLaneEngine(srv.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
		return func() (string, error) {
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			return reason, err
		}, func() int { return fx.merges }
	}},
}

// closingContradictionBody is projectbluefin/testsuite#1057's shape: a
// partial `Refs` note and a `Closes` for the same issue.
const closingContradictionBody = "Refs #42 — partial: two items remain open.\n\nCloses #42\n"

func newClosingLinkFront(body string) *linkedIssueFront {
	return &linkedIssueFront{body: body, issues: map[string]linkedIssue{
		"acme/widget#42": {state: "open"},
	}}
}

// A contradiction stops every lane before admission, CI or soak, without an
// API read; editing the body (same head) lets the next pass merge.
func TestClosingContradictionGateAcrossLanes(t *testing.T) {
	for _, lane := range closingLinkLanes {
		t.Run(lane.name, func(t *testing.T) {
			front := newClosingLinkFront(closingContradictionBody)
			tick, merges := lane.build(t, front)

			reason, err := tick()
			if err != nil || reason != "closing-contradiction:#42" || merges() != 0 {
				t.Fatalf("contradiction tick: reason = %q, err = %v, merges = %d; want closing-contradiction:#42 and no merge", reason, err, merges())
			}
			if got := front.readsOf("acme/widget#42"); got != 0 {
				t.Fatalf("issue reads = %d, want 0: the contradiction gate costs no API call", got)
			}

			front.setBody("Closes #42\n")
			reason, err = tick()
			if err != nil || reason != "" || merges() != 1 {
				t.Fatalf("fixed-body tick: reason = %q, err = %v, merges = %d; want a merge", reason, err, merges())
			}
		})
	}
}

// Bodies the #10826 decision allows are unaffected.
func TestClosingContradictionGateAllowsConsistentBodies(t *testing.T) {
	for name, body := range map[string]string{
		"closes only":               "Closes #42\n",
		"reporter-confirmation ref": "Refs #42 (reporter confirmation required)\n\nCloses #42\n",
	} {
		for _, lane := range closingLinkLanes {
			t.Run(lane.name+"/"+name, func(t *testing.T) {
				tick, merges := lane.build(t, newClosingLinkFront(body))
				if reason, err := tick(); err != nil || reason != "" || merges() != 1 {
					t.Fatalf("reason = %q, err = %v, merges = %d; want a merge", reason, err, merges())
				}
			})
		}
	}
}

// commit1054Message is projectbluefin/testsuite#1054's commit, trimmed: its
// "fix #1036" closed the open PR #1036 when #1054 merged.
const commit1054Message = "fix(smoke): stop Text Editor, Calculator, Settings and Firefox checks over-waiting (#1054)\n\n" +
	"Apply the same fix #1036 makes for Files/apps: match the alias against\n" +
	"the single-shot tree.root.applications() snapshot.\n\nCloses #1044\n\n" +
	"Signed-off-by: Danathar <Danathar@users.noreply.github.com>\n"

func newCommitClosingFront(target linkedIssue) *linkedIssueFront {
	front := newClosingLinkFront("Closes #42\n")
	front.issues["acme/widget#1036"] = target
	front.issues["acme/widget#1044"] = linkedIssue{state: "open"}
	front.commits = []string{commit1054Message}
	return front
}

func TestClosingKeywordInCommitMessageTargetingPR(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target linkedIssue
		want   string
	}{
		{name: "open pull request", target: linkedIssue{state: "open", pr: true}, want: "closing-keyword-targets-pr:#1036"},
		// GitHub's close of a closed PR is a no-op, so "closed #N" about a
		// merged PR does not hold the merge.
		{name: "closed pull request", target: linkedIssue{state: "closed", pr: true}},
		{name: "issue", target: linkedIssue{state: "open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front := newCommitClosingFront(tc.target)
			srv := newLinkedIssueFront(t, api.URL, front)
			defer srv.Close()
			c := newAutoMergeSweepClient(srv.URL)
			c.scanCommitClosingKeywords = true

			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason != tc.want {
				t.Fatalf("reason = %q, err = %v, want %q", reason, err, tc.want)
			}
			if tc.want == "" {
				if merges != 1 {
					t.Fatalf("merges = %d, want 1: a closing keyword for an issue or a closed pull request behaves as before", merges)
				}
				return
			}
			if merges != 0 {
				t.Fatalf("merges = %d, want 0", merges)
			}
			// The next pass on the same head reads the commits once per
			// head, but reads the target pull request again: it may have
			// closed since.
			if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != tc.want || merges != 0 {
				t.Fatalf("second tick: reason = %q, err = %v, merges = %d", reason, err, merges)
			}
			if got := front.readsOf("acme/widget#1036"); got != 2 {
				t.Fatalf("target reads = %d, want 2: an open pull request is read every tick", got)
			}
			front.mu.Lock()
			commitReads := front.commitReads
			front.mu.Unlock()
			if commitReads != 1 {
				t.Fatalf("commit list reads = %d, want 1 per head", commitReads)
			}
		})
	}
}

// A held merge is released once the target PR closes: the next tick reads
// it again and finds nothing left to close.
func TestClosingKeywordTargetPRReleasesWhenClosed(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := newCommitClosingFront(linkedIssue{state: "open", pr: true})
	srv := newLinkedIssueFront(t, api.URL, front)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.scanCommitClosingKeywords = true

	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "closing-keyword-targets-pr:#1036" || merges != 0 {
		t.Fatalf("open-target tick: reason = %q, err = %v, merges = %d", reason, err, merges)
	}
	front.set("acme/widget#1036", linkedIssue{state: "closed", pr: true})
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" || merges != 1 {
		t.Fatalf("closed-target tick: reason = %q, err = %v, merges = %d; want a merge", reason, err, merges)
	}
}

// Without the option the commit messages are not read and the merge goes
// ahead on the body alone.
func TestClosingKeywordCommitScanOff(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := newCommitClosingFront(linkedIssue{state: "open", pr: true})
	srv := newLinkedIssueFront(t, api.URL, front)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)

	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" || merges != 1 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want a merge", reason, err, merges)
	}
	if got := front.readsOf("acme/widget#1036"); got != 0 {
		t.Fatalf("target reads = %d, want 0", got)
	}
}

// A commit list that cannot be read withholds the merge for the tick.
func TestClosingKeywordCommitReadFailsClosed(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := newCommitClosingFront(linkedIssue{state: "open"})
	front.commitsStatus = http.StatusInternalServerError
	srv := newLinkedIssueFront(t, api.URL, front)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.scanCommitClosingKeywords = true

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err == nil || reason != linkedIssueReasonCheck || merges != 0 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want %s, an error and no merge", reason, err, merges, linkedIssueReasonCheck)
	}
}

// GitHub acts on closing keywords only when the PR's base is the default
// branch. A PR into another branch (a forward-merge carrying upstream
// commit messages) closes nothing, so neither gate holds it or reads
// anything for it; a payload that does not name the default branch is
// gated as before.
func TestClosingKeywordsApply(t *testing.T) {
	for name, tc := range map[string]struct {
		baseRef, defaultBranch string
		want                   bool
	}{
		"default branch":           {"main", "main", true},
		"case differs":             {"Main", "main", true},
		"non-default base":         {"v6", "main", false},
		"default branch unknown":   {"v6", "", true},
		"base ref unknown":         {"", "main", true},
		"neither known (fixtures)": {"", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			pr := &gh.PullRequest{Base: &gh.PullRequestBranch{}}
			if tc.baseRef != "" {
				pr.Base.Ref = gh.Ptr(tc.baseRef)
			}
			if tc.defaultBranch != "" {
				pr.Base.Repo = &gh.Repository{DefaultBranch: gh.Ptr(tc.defaultBranch)}
			}
			if got := closingKeywordsApply(pr); got != tc.want {
				t.Fatalf("closingKeywordsApply(base %q, default %q) = %v, want %v", tc.baseRef, tc.defaultBranch, got, tc.want)
			}
		})
	}
}

func TestClosingLinkGatesSkipNonDefaultBase(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	// Both shapes at once: a contradictory body and a commit closing PR #1036.
	front := newCommitClosingFront(linkedIssue{state: "open", pr: true})
	front.setBody(closingContradictionBody)
	front.baseRef, front.defaultBranch = "v6", "main"
	srv := newLinkedIssueFront(t, api.URL, front)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.scanCommitClosingKeywords = true

	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" || merges != 1 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want a merge: closing keywords do nothing off the default branch", reason, err, merges)
	}
	if got := front.readsOf("acme/widget#1036"); got != 0 {
		t.Fatalf("target reads = %d, want 0", got)
	}
	front.mu.Lock()
	commitReads := front.commitReads
	front.mu.Unlock()
	if commitReads != 0 {
		t.Fatalf("commit list reads = %d, want 0", commitReads)
	}

	// The same PR into the default branch is held as before.
	front.mu.Lock()
	front.baseRef = "main"
	front.mu.Unlock()
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "closing-contradiction:#42" || merges != 1 {
		t.Fatalf("default-branch tick: reason = %q, err = %v, merges = %d; want closing-contradiction:#42", reason, err, merges)
	}
}

// A 404 is trusted only for closingMissingTTL: after it, the number is read
// again and a pull request allocated in the meantime is caught.
func TestClosingKeywordMissingTargetExpires(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true})
	defer api.Close()
	front := newClosingLinkFront("Closes #42\n")
	srv := newLinkedIssueFront(t, api.URL, front)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	const target = "acme/widget#1036"
	// Registered as missing so the front counts its reads; an unregistered
	// number falls through to the backend uncounted.
	front.set(target, linkedIssue{status: http.StatusNotFound})

	if open, err := c.numberIsOpenPullRequest(context.Background(), "acme", "widget", 1036); err != nil || open {
		t.Fatalf("missing target: open = %v, err = %v", open, err)
	}
	clock = clock.Add(closingMissingTTL - time.Second)
	if open, err := c.numberIsOpenPullRequest(context.Background(), "acme", "widget", 1036); err != nil || open {
		t.Fatalf("within TTL: open = %v, err = %v", open, err)
	}
	if got := front.readsOf(target); got != 1 {
		t.Fatalf("reads within TTL = %d, want 1", got)
	}
	front.set(target, linkedIssue{state: "open", pr: true})
	clock = clock.Add(2 * time.Second)
	if open, err := c.numberIsOpenPullRequest(context.Background(), "acme", "widget", 1036); err != nil || !open {
		t.Fatalf("after TTL: open = %v, err = %v, want an open pull request", open, err)
	}
	if got := front.readsOf(target); got != 2 {
		t.Fatalf("reads after TTL = %d, want 2", got)
	}
}

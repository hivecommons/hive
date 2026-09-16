package github

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// Coverage for #7208: a hive-opened PR credits the human who opened the issue
// it answers, via requested_by=@login in the attribution trailer and audit
// entry — and never credits the hive's own issues to the bot.

func TestAttributionTrailer_RequestedBy(t *testing.T) {
	m := InvocationMeta{Agent: "quality", Backend: "codex", RequestedBy: "hanthor"}
	want := "— hive: agent=quality backend=codex requested_by=@hanthor"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() = %q, want %q", got, want)
	}
	// A login handed over with its @ already on is not doubled.
	m.RequestedBy = "@hanthor"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() with @-prefixed login = %q, want %q", got, want)
	}
	if got := m.AuditDetail("repo", "o/r"); !strings.Contains(got, "requested_by=@hanthor") {
		t.Errorf("AuditDetail() = %q, want requested_by recorded", got)
	}
	m.RequestedBy = ""
	if got := m.Trailer(); strings.Contains(got, "requested_by") {
		t.Errorf("Trailer() with no opener = %q, want no requested_by token", got)
	}
}

func TestResolveRequestedBy(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	cases := []struct {
		name     string
		issues   map[int]*selfAuthIssue
		body     string
		declared []int
		want     string
	}{{
		name:   "human opener is credited",
		issues: map[int]*selfAuthIssue{581: {Author: "hanthor"}},
		body:   "Closes #581",
		want:   "hanthor",
	}, {
		name:   "hive-filed issue credits nobody",
		issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}},
		body:   "Closes #581",
		want:   "",
	}, {
		name:   "first human among several citations wins",
		issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}, 590: {Author: "hanthor"}, 591: {Author: "someone-else"}},
		body:   "Closes #581\nCloses #590\nRefs #591",
		want:   "hanthor",
	}, {
		name:     "declared issue list is honoured when the body cites nothing",
		issues:   map[int]*selfAuthIssue{42: {Author: "hanthor"}},
		body:     "no references here",
		declared: []int{42},
		want:     "hanthor",
	}, {
		name:   "unreadable issue is skipped, not fatal",
		issues: map[int]*selfAuthIssue{581: {Status: 500}, 590: {Author: "hanthor"}},
		body:   "Closes #581\nCloses #590",
		want:   "hanthor",
	}, {
		name:   "missing issue yields nothing",
		issues: map[int]*selfAuthIssue{},
		body:   "Closes #581",
		want:   "",
	}, {
		name:   "no citations at all",
		issues: map[int]*selfAuthIssue{581: {Author: "hanthor"}},
		body:   "just a change",
		want:   "",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: tc.issues}
			c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
			c.SetAppBotLogin(botLogin)
			got := c.resolveRequestedBy(context.Background(), "o/r", "t", tc.body, tc.declared)
			if got != tc.want {
				t.Fatalf("resolveRequestedBy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRequestedBy_NilClient(t *testing.T) {
	var c *Client
	if got := c.resolveRequestedBy(context.Background(), "o/r", "t", "Closes #1", nil); got != "" {
		t.Fatalf("nil client = %q, want empty", got)
	}
}

func TestResolveRequestedBy_BoundsLookups(t *testing.T) {
	// Only the first maxRequestedByLookups citations are read; the human on
	// the far side of the cap is not found.
	issues := map[int]*selfAuthIssue{}
	var body strings.Builder
	for i := 1; i <= maxRequestedByLookups; i++ {
		issues[i] = &selfAuthIssue{Author: "kubestellar-hive[bot]", AuthorType: "Bot"}
		body.WriteString("Refs #" + strconv.Itoa(i) + "\n")
	}
	issues[maxRequestedByLookups+1] = &selfAuthIssue{Author: "hanthor"}
	body.WriteString("Refs #" + strconv.Itoa(maxRequestedByLookups+1) + "\n")

	srv := &selfAuthServer{issues: issues}
	c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
	c.SetAppBotLogin("kubestellar-hive[bot]")
	if got := c.resolveRequestedBy(context.Background(), "o/r", "t", body.String(), nil); got != "" {
		t.Fatalf("resolveRequestedBy() = %q, want lookups capped before the human citation", got)
	}
}

// TestPRRequestWatcher_CreditsIssueOpener is the end-to-end contract: the PR
// body the watcher POSTs carries the opener, and a hive-filed rationale does
// not credit the bot.
func TestPRRequestWatcher_CreditsIssueOpener(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	for _, tc := range []struct {
		name   string
		issue  *selfAuthIssue
		wantIn string
	}{
		{"human opener", &selfAuthIssue{Author: "hanthor"}, "requested_by=@hanthor"},
		{"hive-filed", &selfAuthIssue{Author: botLogin, AuthorType: "Bot"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: tc.issue}}
			c := testClient(t, srv.start(t).URL)
			c.SetAppBotLogin(botLogin)
			c.prHoldLabel = func(agent string) bool { return false }

			dir := t.TempDir()
			prRequestDirForTest = dir
			t.Cleanup(func() { prRequestDirForTest = "" })
			if _, err := WritePRRequest(dir, PRRequest{
				Repo: "o/r", Head: "fix", Base: "main", Title: "fix", Body: "Closes #581", Agent: "quality",
			}); err != nil {
				t.Fatalf("WritePRRequest: %v", err)
			}
			c.ProcessPRRequestsOnce(context.Background())

			bodies := srv.postedPRBodies()
			if len(bodies) != 1 {
				t.Fatalf("posted %d PRs, want 1", len(bodies))
			}
			if !strings.Contains(bodies[0], AttributionTrailerPrefix) {
				t.Fatalf("PR body has no attribution trailer:\n%s", bodies[0])
			}
			if tc.wantIn != "" && !strings.Contains(bodies[0], tc.wantIn) {
				t.Errorf("PR body does not credit the opener (%q):\n%s", tc.wantIn, bodies[0])
			}
			if tc.wantIn == "" && strings.Contains(bodies[0], "requested_by") {
				t.Errorf("PR body credits a hive-filed issue to the bot:\n%s", bodies[0])
			}
		})
	}
}

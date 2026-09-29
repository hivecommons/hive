package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Follow-up signing (#9364): the reconciler pass over open hive PRs.

const (
	reconcileBot    = "hive-app[bot]"
	reconcileBranch = "quality/onb-1-followup"
	reconcileOID    = "5190ed0000000000000000000000000000000000" // what the mock's mutation returns
	verifiedSHA     = "ffff000000000000000000000000000000000010"
	agentSHA        = "eeee000000000000000000000000000000000020"
)

func reconcilePR(number int, author, headSHA string) map[string]any {
	return map[string]any{
		"number": number,
		"user":   map[string]any{"login": author},
		"head":   map[string]any{"ref": reconcileBranch, "sha": headSHA, "repo": map[string]any{"full_name": "o/r"}},
		"base":   map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}},
	}
}

// prCommitJSON is one entry of GET /pulls/N/commits. login "" means GitHub did
// not resolve the email to an account (the pane identity's usual case).
func prCommitJSON(sha string, verified bool, login, email string, parents ...string) map[string]any {
	if len(parents) == 0 {
		parents = []string{"parent-of-" + sha}
	}
	ps := make([]map[string]any, 0, len(parents))
	for _, p := range parents {
		ps = append(ps, map[string]any{"sha": p})
	}
	ident := map[string]any{"name": "someone", "email": email}
	rc := map[string]any{
		"sha":     sha,
		"parents": ps,
		"commit": map[string]any{
			"message":      "change " + sha[:4] + "\n\nSigned-off-by: someone <" + email + ">",
			"author":       ident,
			"committer":    ident,
			"verification": map[string]any{"verified": verified},
		},
	}
	if login != "" {
		rc["author"] = map[string]any{"login": login}
		rc["committer"] = map[string]any{"login": login}
	}
	return rc
}

// reconcileFixture is one open App-authored PR (#77) whose head is the mock's
// head ref. Tests fill m.prCommits so its last entry is m.headSHA.
func reconcileFixture(t *testing.T, enabled bool) (*signedMock, *Client) {
	t.Helper()
	t.Setenv(signedTrailerDomainEnv, "")
	m := newSignedMock(t)
	m.openPRs = []map[string]any{reconcilePR(77, reconcileBot, m.headSHA)}
	srv := m.server()
	t.Cleanup(srv.Close)
	c := signedTestClient(t, srv.URL, enabled)
	c.SetAppBotLogin(reconcileBot)
	return m, c
}

func (m *signedMock) countCalls(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// countExact counts calls that are exactly call ("METHOD path").
func (m *signedMock) countExact(call string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == call {
			n++
		}
	}
	return n
}

const openPRListCall = "GET /repos/o/r/pulls"

func (m *signedMock) resetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

const prCommitsCall = "GET /repos/o/r/pulls/77/commits"

// A Verified head is already mergeable under required_signatures: nothing to
// rewrite, nothing to say.
func TestSignedReconcile_VerifiedHeadIsNoop(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{
		prCommitJSON(agentSHA, false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(m.headSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
	}

	c.reconcileSignedCommits(context.Background())

	if m.countCalls(prCommitsCall) != 1 {
		t.Fatalf("the PR's commits must be listed once:\n%s", strings.Join(m.calls, "\n"))
	}
	if m.callIndex("GET /repos/o/r/compare/") >= 0 || m.graphql != nil || m.refPatch != nil || len(m.posted) != 0 {
		t.Errorf("a verified head must not be rewritten or commented on:\n%s", strings.Join(m.calls, "\n"))
	}
}

// The core case: the agent pushed follow-ups on top of the open-time signed
// commit. The unsigned tail is re-authored on top of that verified commit and
// the branch moved to the signed result.
func TestSignedReconcile_SignsAgentTailOnLastVerified(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.mergeBase = verifiedSHA // GitHub: merge base of sha...head is the sha
	m.prCommits = []map[string]any{
		prCommitJSON("1111000000000000000000000000000000000001", false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(verifiedSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
		prCommitJSON(agentSHA, false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(m.headSHA, false, "", "4744647+hive-app[bot]@users.noreply.github.com"),
	}

	c.reconcileSignedCommits(context.Background())

	if m.callIndex("GET /repos/o/r/compare/"+verifiedSHA+"...") < 0 {
		t.Fatalf("signBranch must compare from the last verified sha:\n%s", strings.Join(m.calls, "\n"))
	}
	if m.graphql == nil {
		t.Fatal("createCommitOnBranch was never called")
	}
	input := m.graphql["variables"].(map[string]any)["input"].(map[string]any)
	if input["expectedHeadOid"] != verifiedSHA {
		t.Errorf("the signed commit must sit on the last verified commit, got parent %v", input["expectedHeadOid"])
	}
	if branch := input["branch"].(map[string]any)["branchName"]; branch != signedCommitScratchPrefix+reconcileBranch {
		t.Errorf("mutation must target the scratch branch, got %v", branch)
	}
	if m.refPatch == nil || m.refPatch["sha"] != reconcileOID || m.refPatch["force"] != true {
		t.Errorf("the PR branch must be force-updated to the signed commit, got %v", m.refPatch)
	}
	if m.callIndex("PATCH /repos/o/r/git/refs/heads/"+reconcileBranch) < 0 {
		t.Errorf("the PR's own branch must be updated:\n%s", strings.Join(m.calls, "\n"))
	}
	if len(m.posted) != 0 {
		t.Errorf("a successful signing must not comment: %v", m.posted)
	}
	if got := c.signedReconcile.settledHead("o/r#77"); got != reconcileOID {
		t.Errorf("the signed commit must be remembered as the settled head, got %q", got)
	}
}

// A person's unsigned commit in the tail is never re-authored. The PR gets a
// single comment, not one per pass or per push, and not a second one after
// a restart.
func TestSignedReconcile_HumanCommitInTailSkipsAndCommentsOnce(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{
		prCommitJSON(verifiedSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
		prCommitJSON(agentSHA, false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(m.headSHA, false, "alice", "alice@example.com"),
	}

	c.reconcileSignedCommits(context.Background())

	if m.callIndex("GET /repos/o/r/compare/") >= 0 || m.graphql != nil || m.refPatch != nil || m.callIndex("POST /repos/o/r/git/refs") >= 0 {
		t.Fatalf("a tail with a human commit must not be rewritten:\n%s", strings.Join(m.calls, "\n"))
	}
	if len(m.posted) != 1 || !strings.Contains(m.posted[0], signedReconcileMarker) || !strings.Contains(m.posted[0], "alice@example.com") {
		t.Fatalf("exactly one marked comment naming the commit's author expected, got %q", m.posted)
	}

	// Same head again: settled, nothing listed or posted.
	m.resetCalls()
	c.reconcileSignedCommits(context.Background())
	if m.countCalls(prCommitsCall) != 0 || len(m.posted) != 1 {
		t.Errorf("an unchanged head must not be re-examined (posted=%d):\n%s", len(m.posted), strings.Join(m.calls, "\n"))
	}

	// The human pushes again: still blocked, still one comment.
	newHead := "dddd000000000000000000000000000000000030"
	m.prCommits = append(m.prCommits, prCommitJSON(newHead, false, "alice", "alice@example.com"))
	m.openPRs = []map[string]any{reconcilePR(77, reconcileBot, newHead)}
	c.reconcileSignedCommits(context.Background())
	if len(m.posted) != 1 {
		t.Errorf("a new push must not repeat the comment, got %d", len(m.posted))
	}

	// After a restart the in-memory set is gone; the marker on the PR stops a repeat.
	m.comments = []map[string]any{{"user": map[string]any{"login": reconcileBot}, "body": m.posted[0]}}
	restarted := signedTestClient(t, strings.TrimSuffix(c.client.BaseURL.String(), "/"), true)
	restarted.SetAppBotLogin(reconcileBot)
	restarted.reconcileSignedCommits(context.Background())
	if len(m.posted) != 1 {
		t.Errorf("the marker comment must stop a repeat after a restart, got %d", len(m.posted))
	}
}

// A merge commit can't be expressed by createCommitOnBranch without folding
// the merged-in changes into the PR; skip it like a human commit.
func TestSignedReconcile_MergeCommitInTailSkips(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{
		prCommitJSON(verifiedSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
		prCommitJSON(m.headSHA, false, "", "quality@hive.kubestellar.io", verifiedSHA, "base-tip"),
	}

	c.reconcileSignedCommits(context.Background())

	if m.graphql != nil || m.refPatch != nil {
		t.Fatalf("a merge commit must not be flattened into a signed commit")
	}
	if len(m.posted) != 1 || !strings.Contains(m.posted[0], "merge commit") {
		t.Errorf("expected one comment explaining the merge commit, got %q", m.posted)
	}
}

// No verified commit anywhere (an open-time signed_skipped PR, or a rebase):
// the base is the PR's base branch, exactly as at open time.
func TestSignedReconcile_NoVerifiedCommitUsesPRBase(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{
		prCommitJSON(agentSHA, false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(m.headSHA, false, "", "quality@hive.kubestellar.io"),
	}

	c.reconcileSignedCommits(context.Background())

	if m.callIndex("GET /repos/o/r/compare/main...") < 0 {
		t.Fatalf("with no verified commit signBranch must compare from the PR base:\n%s", strings.Join(m.calls, "\n"))
	}
	if m.refPatch == nil || m.refPatch["sha"] != reconcileOID {
		t.Errorf("the branch must be updated to the signed commit, got %v", m.refPatch)
	}
}

// Rate limits: a PR whose head has not changed since the last pass costs no
// commit listing, only its share of the open-PR list.
func TestSignedReconcile_UnchangedHeadIsNotRelisted(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com")}

	c.reconcileSignedCommits(context.Background())
	c.reconcileSignedCommits(context.Background())
	c.reconcileSignedCommits(context.Background())

	if got := m.countCalls(prCommitsCall); got != 1 {
		t.Errorf("commits must be listed once for an unchanged head, got %d", got)
	}
	if got := m.countExact(openPRListCall); got != 3 {
		t.Errorf("each pass should cost one open-PR list, calls:\n%s", strings.Join(m.calls, "\n"))
	}
}

// A head that moved while the signed commit was built is transient: no
// comment, and the next pass looks again (the open-time skip is retried).
func TestSignedReconcile_MovedHeadRetriesNextPass(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.headSHALater = "cccc000000000000000000000000000000000003"
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, false, "", "quality@hive.kubestellar.io")}

	c.reconcileSignedCommits(context.Background())
	if m.refPatch != nil || len(m.posted) != 0 {
		t.Fatalf("a moved head must be neither replaced nor commented on (patch=%v posted=%v)", m.refPatch, m.posted)
	}
	c.reconcileSignedCommits(context.Background())
	if got := m.countCalls(prCommitsCall); got != 2 {
		t.Errorf("a moved head must be retried on the next pass, commits listed %d times", got)
	}
}

// Only the App's own PRs are the hive's to rewrite.
func TestSignedReconcile_IgnoresPeoplesPRs(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.openPRs = []map[string]any{reconcilePR(77, "alice", m.headSHA)}
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, false, "", "quality@hive.kubestellar.io")}

	c.reconcileSignedCommits(context.Background())

	if m.countCalls(prCommitsCall) != 0 || m.graphql != nil {
		t.Errorf("a person's PR must not be examined:\n%s", strings.Join(m.calls, "\n"))
	}
}

// Off (or unset): not a single request.
func TestSignedReconcile_OffTouchesNothing(t *testing.T) {
	m, c := reconcileFixture(t, false)
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, false, "", "quality@hive.kubestellar.io")}
	unset := testClient(t, strings.TrimSuffix(c.client.BaseURL.String(), "/"))
	unset.SetAppBotLogin(reconcileBot)

	for _, cl := range []*Client{c, unset} {
		cl.reconcileSignedCommits(context.Background())
		cl.maybeReconcileSignedCommits(context.Background(), time.Now())
	}
	if len(m.calls) != 0 {
		t.Errorf("signing off must make no GitHub calls:\n%s", strings.Join(m.calls, "\n"))
	}
}

// The watcher ticks every few seconds; the pass runs at most once per
// prSignedReconcileInterval.
func TestSignedReconcile_Throttled(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com")}
	t0 := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	c.maybeReconcileSignedCommits(context.Background(), t0)
	c.maybeReconcileSignedCommits(context.Background(), t0.Add(10*time.Second))
	if got := m.countExact(openPRListCall); got != 1 {
		t.Errorf("a tick inside the interval must not run the pass, got %d list calls", got)
	}
	c.maybeReconcileSignedCommits(context.Background(), t0.Add(prSignedReconcileInterval+time.Second))
	if got := m.countExact(openPRListCall); got != 2 {
		t.Errorf("a tick past the interval must run the pass, got %d list calls", got)
	}
}

func TestHiveCommitIdentity(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	c := &Client{}
	c.SetAppBotLogin(reconcileBot)
	cases := []struct {
		login, email string
		want         bool
	}{
		{"", "quality@hive.kubestellar.io", true},
		{"", "quality@HIVE.kubestellar.io", true},
		{"", "hive-app[bot]@users.noreply.github.com", true},
		{"", "4744647+hive-app[bot]@users.noreply.github.com", true},
		{reconcileBot, "anything@example.com", true},
		{"", "other-app[bot]@users.noreply.github.com", false},
		{"", "alice@example.com", false},
		{"", "alice@users.noreply.github.com", false},
		// A resolved person's login wins over a hive-domain email.
		{"alice", "quality@hive.kubestellar.io", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := c.hiveCommitIdentity(tc.login, tc.email); got != tc.want {
			t.Errorf("hiveCommitIdentity(%q, %q) = %v, want %v", tc.login, tc.email, got, tc.want)
		}
	}
}

// The pass is wired into the PR-request watcher's own loop: a running watcher
// with signing on reconciles without any request file.
func TestSignedReconcile_RunsFromWatcherLoop(t *testing.T) {
	m, c := reconcileFixture(t, true)
	m.prCommits = []map[string]any{prCommitJSON(m.headSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com")}
	fastTick(t, func(d time.Duration) { prRequestPollInterval = d }, prRequestPollInterval)
	old := prRequestDirForTest
	prRequestDirForTest = t.TempDir()
	t.Cleanup(func() { prRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartPRRequestWatcher(ctx, nil, nil, nil)
	deadline := time.Now().Add(5 * time.Second)
	for m.countExact(prCommitsCall) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if m.countExact(prCommitsCall) == 0 {
		t.Fatalf("the watcher loop never ran the reconcile pass:\n%s", strings.Join(m.calls, "\n"))
	}
}

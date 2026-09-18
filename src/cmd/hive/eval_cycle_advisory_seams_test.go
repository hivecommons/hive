package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
)

// --- pinDigestSnapshot -----------------------------------------------------

func snapshotDepsOK(t *testing.T, calls *[]string) digestSnapshotDeps {
	t.Helper()
	return digestSnapshotDeps{
		defaultBranch: func(_ context.Context, owner, repo string) (string, error) {
			*calls = append(*calls, "defaultBranch "+owner+"/"+repo)
			return "trunk", nil
		},
		latestCommit: func(_ context.Context, owner, repo, branch string) (string, error) {
			*calls = append(*calls, "latestCommit "+owner+"/"+repo+"@"+branch)
			return "abc123", nil
		},
		pathExists: func(_ context.Context, _, _, path, ref string) (bool, error) {
			*calls = append(*calls, "pathExists "+path+"@"+ref)
			return path == "README.md", nil
		},
		issueClosedAt: func(_ context.Context, owner, repo string, n int) (time.Time, bool, error) {
			*calls = append(*calls, "issueClosedAt")
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), n == 7, nil
		},
	}
}

func TestPinDigestSnapshot_PinsAndInstallsHooks(t *testing.T) {
	var calls []string
	opts := advisory.DigestOptions{}

	pinned := pinDigestSnapshot(context.Background(), &opts, "acme", "widget", "acme/widget", "main", snapshotDepsOK(t, &calls), restoreTestLogger())

	if !pinned {
		t.Fatal("expected pinned=true")
	}
	if opts.Snapshot == nil || opts.Snapshot.SHA != "abc123" || opts.Snapshot.Branch != "main" || opts.Snapshot.Owner != "acme" || opts.Snapshot.Repo != "widget" {
		t.Fatalf("snapshot = %+v", opts.Snapshot)
	}
	if len(calls) != 1 || calls[0] != "latestCommit acme/widget@main" {
		t.Fatalf("explicit branch must skip defaultBranch; calls = %v", calls)
	}
	if !opts.VerifyPath("README.md") || opts.VerifyPath("gone.md") {
		t.Fatal("VerifyPath must relay the lookup result")
	}
	if st, ok := opts.ResolveRef("acme", "widget", 7); !ok || !st.Closed || st.ClosedAt.Year() != 2026 {
		t.Fatalf("ResolveRef(7) = %+v, %v", st, ok)
	}
	if st, ok := opts.ResolveRef("acme", "widget", 8); !ok || st.Closed {
		t.Fatalf("ResolveRef(8) = %+v, %v", st, ok)
	}
}

func TestPinDigestSnapshot_FallsBackToDefaultBranch(t *testing.T) {
	var calls []string
	opts := advisory.DigestOptions{}

	pinned := pinDigestSnapshot(context.Background(), &opts, "acme", "widget", "acme/widget", "", snapshotDepsOK(t, &calls), restoreTestLogger())

	if !pinned || opts.Snapshot == nil || opts.Snapshot.Branch != "trunk" {
		t.Fatalf("pinned=%v snapshot=%+v", pinned, opts.Snapshot)
	}
	if len(calls) != 2 || calls[0] != "defaultBranch acme/widget" || calls[1] != "latestCommit acme/widget@trunk" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestPinDigestSnapshot_LeavesOptsUntouchedOnFailure(t *testing.T) {
	cases := map[string]digestSnapshotDeps{
		"no client": {},
		"default branch error": {
			defaultBranch: func(context.Context, string, string) (string, error) { return "", errors.New("boom") },
			latestCommit: func(context.Context, string, string, string) (string, error) {
				t.Fatal("must not be called")
				return "", nil
			},
		},
		"latest commit error": {
			latestCommit: func(context.Context, string, string, string) (string, error) { return "", errors.New("boom") },
		},
		"empty sha": {
			latestCommit: func(context.Context, string, string, string) (string, error) { return "", nil },
		},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			opts := advisory.DigestOptions{MaxFindings: 3}
			branch := "main"
			if name == "default branch error" {
				branch = ""
			}
			if pinDigestSnapshot(context.Background(), &opts, "acme", "widget", "acme/widget", branch, deps, restoreTestLogger()) {
				t.Fatal("expected pinned=false")
			}
			if opts.Snapshot != nil || opts.VerifyPath != nil || opts.ResolveRef != nil || opts.MaxFindings != 3 {
				t.Fatalf("opts mutated on failure: %+v", opts)
			}
		})
	}
}

func TestPinDigestSnapshot_EmptyOrgOrRepoNeverCallsGitHub(t *testing.T) {
	var calls []string
	opts := advisory.DigestOptions{}
	for _, tc := range [][2]string{{"", "widget"}, {"acme", ""}} {
		if pinDigestSnapshot(context.Background(), &opts, tc[0], tc[1], "x", "main", snapshotDepsOK(t, &calls), restoreTestLogger()) {
			t.Fatalf("%v: expected pinned=false", tc)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestPinDigestSnapshot_InconclusiveLookupsFailOpen(t *testing.T) {
	deps := snapshotDepsOK(t, new([]string))
	deps.pathExists = func(context.Context, string, string, string, string) (bool, error) {
		return false, errors.New("timeout")
	}
	deps.issueClosedAt = func(context.Context, string, string, int) (time.Time, bool, error) {
		return time.Time{}, true, errors.New("timeout")
	}
	opts := advisory.DigestOptions{}
	pinDigestSnapshot(context.Background(), &opts, "acme", "widget", "acme/widget", "main", deps, restoreTestLogger())

	if !opts.VerifyPath("anything") {
		t.Fatal("an inconclusive path check must report the path as still present")
	}
	if st, ok := opts.ResolveRef("acme", "widget", 1); ok || st.Closed {
		t.Fatalf("an inconclusive issue lookup must report unknown, got %+v ok=%v", st, ok)
	}
}

// --- publishAdvisoryDigest -------------------------------------------------

type publishRecorder struct {
	errors    []string
	posted    [][2]int
	forbidden int
	authFail  int
	proven    int
	github    []string
	linear    []string
	githubErr error
	linearErr error
}

func (r *publishRecorder) deps() advisoryPublishDeps {
	return advisoryPublishDeps{
		postGitHub: func(_ context.Context, repo string, n int, md string) error {
			r.github = append(r.github, repo+"#"+itoa(n)+":"+md)
			return r.githubErr
		},
		postLinear: func(_ context.Context, issue, md string) error {
			r.linear = append(r.linear, issue+":"+md)
			return r.linearErr
		},
		recordError:      func(m string) { r.errors = append(r.errors, m) },
		recordPosted:     func(f, o int) { r.posted = append(r.posted, [2]int{f, o}) },
		onWriteForbidden: func(context.Context) { r.forbidden++ },
		onAuthFailure:    func(context.Context) { r.authFail++ },
		onWriteProven:    func(context.Context) { r.proven++ },
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func githubRoute() advisoryPublishRoute {
	return advisoryPublishRoute{
		target: config.AdvisoryTargetGitHub, primaryRepo: "acme/widget", issueNum: 4, hasPinnedIssue: true,
	}
}

func twoFindingDigest() *advisory.Digest {
	return &advisory.Digest{TotalCount: 2, OverflowCount: 1}
}

func TestPublishAdvisoryDigest_EmptyMarkdownIsSkipped(t *testing.T) {
	r := &publishRecorder{}
	got := publishAdvisoryDigest(context.Background(), "", twoFindingDigest(), githubRoute(), r.deps(), restoreTestLogger())
	if got != advisoryPublishSkipped || len(r.github) != 0 || len(r.errors) != 0 {
		t.Fatalf("outcome=%v recorder=%+v", got, r)
	}
}

func TestPublishAdvisoryDigest_GitHubSuccessProvesWrite(t *testing.T) {
	r := &publishRecorder{}
	got := publishAdvisoryDigest(context.Background(), "# digest", twoFindingDigest(), githubRoute(), r.deps(), restoreTestLogger())

	if got != advisoryPublishPosted {
		t.Fatalf("outcome = %v", got)
	}
	if len(r.github) != 1 || r.github[0] != "acme/widget#4:# digest" {
		t.Fatalf("github writes = %v", r.github)
	}
	if len(r.posted) != 1 || r.posted[0] != [2]int{2, 1} {
		t.Fatalf("recordPosted = %v", r.posted)
	}
	if r.proven != 1 || r.forbidden != 0 || r.authFail != 0 || len(r.errors) != 0 || len(r.linear) != 0 {
		t.Fatalf("recorder = %+v", r)
	}
}

func TestPublishAdvisoryDigest_GitHubFailureClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		forbidden int
		authFail  int
	}{
		{"write forbidden", errors.New("403 Resource not accessible by integration"), 1, 0},
		{"rate limited", errors.New("API rate limit exceeded"), 0, 0},
		{"other", errors.New("401 Bad credentials"), 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &publishRecorder{githubErr: tc.err}
			got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), githubRoute(), r.deps(), restoreTestLogger())
			if got != advisoryPublishFailed {
				t.Fatalf("outcome = %v", got)
			}
			if len(r.errors) != 1 || r.errors[0] != tc.err.Error() {
				t.Fatalf("recordError = %v", r.errors)
			}
			if r.forbidden != tc.forbidden || r.authFail != tc.authFail || r.proven != 0 || len(r.posted) != 0 {
				t.Fatalf("recorder = %+v", r)
			}
		})
	}
}

func TestPublishAdvisoryDigest_NoPinnedIssueRecordsCause(t *testing.T) {
	r := &publishRecorder{}
	route := githubRoute()
	route.hasPinnedIssue = false
	route.ensureErr = errors.New("search blip")
	got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), route, r.deps(), restoreTestLogger())

	if got != advisoryPublishFailed || len(r.github) != 0 {
		t.Fatalf("outcome=%v github=%v", got, r.github)
	}
	if len(r.errors) != 1 || r.errors[0] != "no advisory issue resolved for acme/widget — digest not posted" {
		t.Fatalf("recordError = %q", r.errors)
	}
}

func TestPublishAdvisoryDigest_LinearRoutes(t *testing.T) {
	linear := advisoryPublishRoute{target: config.AdvisoryTargetLinear, linearIssue: "HIVE-9", primaryRepo: "acme/widget", hasPinnedIssue: true, issueNum: 4}

	t.Run("success never touches github", func(t *testing.T) {
		r := &publishRecorder{}
		got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), linear, r.deps(), restoreTestLogger())
		if got != advisoryPublishPosted || len(r.github) != 0 || len(r.linear) != 1 || r.linear[0] != "HIVE-9:md" {
			t.Fatalf("outcome=%v recorder=%+v", got, r)
		}
		if len(r.posted) != 1 || r.proven != 0 {
			t.Fatalf("a Linear post must record success but never prove GitHub App write: %+v", r)
		}
	})
	t.Run("failure is recorded", func(t *testing.T) {
		r := &publishRecorder{linearErr: errors.New("linear 500")}
		got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), linear, r.deps(), restoreTestLogger())
		if got != advisoryPublishFailed || len(r.errors) != 1 || r.errors[0] != "linear 500" {
			t.Fatalf("outcome=%v errors=%v", got, r.errors)
		}
	})
	t.Run("misconfiguration is never redirected to github", func(t *testing.T) {
		r := &publishRecorder{}
		bad := linear
		bad.routeErr = errors.New("linear_issue is required")
		got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), bad, r.deps(), restoreTestLogger())
		if got != advisoryPublishFailed || len(r.github) != 0 || len(r.linear) != 0 {
			t.Fatalf("outcome=%v recorder=%+v", got, r)
		}
		if len(r.errors) != 1 || r.errors[0] != "linear_issue is required" {
			t.Fatalf("errors = %v", r.errors)
		}
	})
}

func TestPublishAdvisoryDigest_NilHooksAreOptional(t *testing.T) {
	deps := advisoryPublishDeps{postGitHub: func(context.Context, string, int, string) error {
		return errors.New("403 Resource not accessible by integration")
	}}
	// Must not panic with every optional hook nil.
	if got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), githubRoute(), deps, restoreTestLogger()); got != advisoryPublishFailed {
		t.Fatalf("outcome = %v", got)
	}
	deps.postGitHub = func(context.Context, string, int, string) error { return nil }
	if got := publishAdvisoryDigest(context.Background(), "md", twoFindingDigest(), githubRoute(), deps, restoreTestLogger()); got != advisoryPublishPosted {
		t.Fatalf("outcome = %v", got)
	}
}

func TestAdvisoryPublishOutcome_String(t *testing.T) {
	for o, want := range map[advisoryPublishOutcome]string{advisoryPublishSkipped: "skipped", advisoryPublishPosted: "posted", advisoryPublishFailed: "failed", 9: "advisoryPublishOutcome(9)"} {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(o), got, want)
		}
	}
}

func TestSummarizeDigestForLog(t *testing.T) {
	d := &advisory.Digest{ByAgent: map[string][]advisory.Finding{
		"zed":   {{Severity: "HIGH"}, {Severity: "low"}},
		"alpha": {{Severity: "Critical"}},
	}}
	by, agents := summarizeDigestForLog(d)
	if agents != "alpha(1), zed(2)" {
		t.Fatalf("agents = %q", agents)
	}
	if by["critical"] != 1 || by["high"] != 1 || by["low"] != 1 || by["medium"] != 0 || by["info"] != 0 {
		t.Fatalf("bySeverity = %v", by)
	}
}

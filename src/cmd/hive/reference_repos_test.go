package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// referenceRepoTestTimeout bounds the waits on the async notice poster.
const referenceRepoTestTimeout = 5 * time.Second

func testReferenceRepoDeps() referenceRepoDeps {
	return referenceRepoDeps{
		botLogin: "hive[bot]",
		discover: func(_ context.Context, org string) (int64, error) {
			if org == "hivecommons" {
				return 1, nil
			}
			return 0, github.ErrNoInstallationForOrg
		},
		auth: func() *github.AppAuth { return nil },
	}
}

func TestBuildReferenceRepoPolicies(t *testing.T) {
	ctx := context.Background()
	deps := testReferenceRepoDeps()
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	res, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	pols := res.policies
	if len(pols) != 2 || len(res.repos) != 2 {
		t.Fatalf("got %d policies, %d repos", len(pols), len(res.repos))
	}
	if pols[0].Token != nil || pols[0].Issues || pols[0].PRs || pols[0].Login != "" {
		t.Errorf("read-only policy carries write state: %+v", pols[0])
	}
	if pols[1].Token == nil || !pols[1].Issues || !pols[1].PRs || pols[1].Login != "hive[bot]" {
		t.Errorf("write policy incomplete: %+v", pols[1])
	}
	for i, rr := range res.repos {
		if rr.CredentialState != "" || rr.Repo != refs[i].Repo {
			t.Errorf("hand-written entry %d must come back unchanged: %+v", i, rr)
		}
	}

	// A hand-written write-mode entry without a credential still refuses
	// boot with the exit-22 error text.
	_, err = buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "kubernetes/website", Write: []string{"issues"}}}, deps, testLogger())
	if !errors.Is(err, github.ErrReferenceRepoNoCredential) {
		t.Fatalf("uninstalled org must refuse with ErrReferenceRepoNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "kubernetes/website") || !strings.Contains(err.Error(), "needs the GitHub App installed on kubernetes") {
		t.Fatalf("exit-22 error must name the repo and the cure: %v", err)
	}
	if referenceRepoCredentialExitCode != 22 {
		t.Fatalf("exit code = %d, want 22", referenceRepoCredentialExitCode)
	}
}

// notifyRecorder stubs the request-issue notice poster.
type notifyRecorder struct {
	mu    sync.Mutex
	calls []notifyCall
	// posted is what the stub reports back; the real poster says false when
	// the marker already exists.
	posted bool
	err    error
}

type notifyCall struct {
	repo   string
	number int
	repos  []string
}

func (n *notifyRecorder) post(_ context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	call := notifyCall{repo: issueRepo, number: number}
	for _, rr := range readOnly {
		call.repos = append(call.repos, rr.Repo)
	}
	n.calls = append(n.calls, call)
	return n.posted, n.err
}

func (n *notifyRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// hivecommons/hive#18158: a derived entry never refuses boot. With the App
// installed on its org it keeps the write tier; without, it is served
// read-only and the request issue is told once.
func TestBuildReferenceRepoPoliciesDerived(t *testing.T) {
	ctx := context.Background()
	deps := testReferenceRepoDeps()
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "other/repo", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#400"},
		{Repo: "hivecommons/hive"},
	}
	res, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil {
		t.Fatalf("a derived entry on an uninstalled org must not refuse boot: %v", err)
	}
	if len(res.policies) != len(refs) || len(res.repos) != len(refs) {
		t.Fatalf("got %d policies, %d repos, want %d", len(res.policies), len(res.repos), len(refs))
	}
	writeTier := res.repos[0]
	if !writeTier.WritesIssues() || !writeTier.WritesPRs() || writeTier.CredentialState != config.ReferenceCredentialWriteTier {
		t.Errorf("installed org must keep the write tier: %+v", writeTier)
	}
	if p := res.policies[0]; !p.Issues || !p.PRs || p.Login != "hive[bot]" || p.Token == nil {
		t.Errorf("write-tier policy incomplete: %+v", p)
	}
	for i := 1; i <= 3; i++ {
		rr, p := res.repos[i], res.policies[i]
		if !rr.DerivedReadOnly() || !rr.ReadOnly() || rr.CredentialReason == "" {
			t.Errorf("uninstalled org must downgrade to read-only: %+v", rr)
		}
		if p.Issues || p.PRs || p.Token != nil || p.Login != "" {
			t.Errorf("read-only policy carries write state: %+v", p)
		}
		if rr.DerivedFrom != refs[i].DerivedFrom {
			t.Errorf("provenance lost: %+v", rr)
		}
	}
	if res.repos[4].CredentialState != "" {
		t.Errorf("hand-written read-only entry must carry no credential state: %+v", res.repos[4])
	}

	// First load: one notice per request issue, each with its read-only set.
	rec := &notifyRecorder{posted: true}
	go notifyDerivedReadOnly(ctx, res.repos, rec.post, testLogger())
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool { return rec.count() == 2 }, "want 2 notices, got %d", rec.count())
	rec.mu.Lock()
	calls := append([]notifyCall(nil), rec.calls...)
	rec.mu.Unlock()
	byIssue := map[int][]string{}
	for _, c := range calls {
		if c.repo != "cncf/techdocs" {
			t.Errorf("notice posted to %q, want the request issue's repo", c.repo)
		}
		byIssue[c.number] = c.repos
	}
	if got := strings.Join(byIssue[395], ","); got != "acme/docs,acme/site" {
		t.Errorf("#395 read-only set = %q", got)
	}
	if got := strings.Join(byIssue[400], ","); got != "other/repo" {
		t.Errorf("#400 read-only set = %q", got)
	}

	// Second reload with the same outcome: the poster finds its marker and
	// reports nothing posted; the wiring neither errors nor loops.
	rec2 := &notifyRecorder{posted: false}
	res2, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		notifyDerivedReadOnly(ctx, res2.repos, rec2.post, testLogger())
		close(done)
	}()
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, "second reload notify did not finish")
	if rec2.count() != 2 {
		t.Fatalf("second reload asked the poster %d times, want 2 (the poster decides idempotency)", rec2.count())
	}

	// A poster failure is logged, never fatal; a nil poster is a no-op.
	rec3 := &notifyRecorder{err: errors.New("boom")}
	notifyDerivedReadOnly(ctx, res.repos, rec3.post, testLogger())
	notifyDerivedReadOnly(ctx, res.repos, nil, testLogger())
}

// A transient discovery failure must not downgrade a derived entry with a
// "not installed" verdict: it is served read-only this load as unresolved
// and the request issue is not told.
func TestBuildReferenceRepoPoliciesDerivedTransientError(t *testing.T) {
	deps := testReferenceRepoDeps()
	deps.discover = func(context.Context, string) (int64, error) { return 0, errors.New("dial tcp: i/o timeout") }
	refs := []config.ReferenceRepo{{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"}}
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatalf("transient error must not refuse boot: %v", err)
	}
	rr := res.repos[0]
	if !rr.ReadOnly() || rr.CredentialState != config.ReferenceCredentialUnresolved || rr.DerivedReadOnly() {
		t.Fatalf("got %+v", rr)
	}
	rec := &notifyRecorder{posted: true}
	notifyDerivedReadOnly(context.Background(), res.repos, rec.post, testLogger())
	if rec.count() != 0 {
		t.Fatalf("an unresolved entry must not notify the request issue, got %d calls", rec.count())
	}
}

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// testOwnerLogin is the connected hive owner in these tests.
const testOwnerLogin = "nate-double-u"

// testReferenceRepoDeps is a wiring with the owner identity connected
// (hivecommons/hive#18197) and a whoAmI that recognises one PAT.
func testReferenceRepoDeps() referenceRepoDeps {
	return referenceRepoDeps{
		owner:      func() github.OwnerCredential { return github.OwnerCredential{Login: testOwnerLogin} },
		ownerToken: func() (string, bool) { return "gho_owner", true },
		whoAmI: func(_ context.Context, token string) (string, error) {
			if token == "ghp_pat" {
				return "maintainer", nil
			}
			return "", errors.New("bad credentials")
		},
	}
}

// testReferenceRepoDepsDisconnected is the same wiring before the owner has
// connected an account.
func testReferenceRepoDepsDisconnected() referenceRepoDeps {
	deps := testReferenceRepoDeps()
	deps.owner = func() github.OwnerCredential {
		return github.OwnerCredential{Unavailable: github.OwnerIdentityNotConnected}
	}
	deps.ownerToken = func() (string, bool) { return "", false }
	return deps
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
	if pols[1].Token == nil || !pols[1].Issues || !pols[1].PRs || pols[1].Login != testOwnerLogin {
		t.Errorf("write policy incomplete: %+v", pols[1])
	}
	if tok, ok := pols[1].Token(); !ok || tok != "gho_owner" {
		t.Errorf("write policy must inject the owner token, got %q,%v", tok, ok)
	}
	if rr := res.repos[0]; rr.CredentialState != "" || rr.CredentialLogin != "" {
		t.Errorf("read-only hand-written entry must carry no credential state: %+v", rr)
	}
	if rr := res.repos[1]; rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin != testOwnerLogin || rr.CredentialReason != "" {
		t.Errorf("hand-written write entry must report the owner as its credential: %+v", rr)
	}

	// A hand-written write-mode entry without a credential still refuses
	// boot with the exit-22 error text, which names the cure.
	_, err = buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "kubernetes/website", Write: []string{"issues"}}}, testReferenceRepoDepsDisconnected(), testLogger())
	if !errors.Is(err, github.ErrReferenceRepoNoCredential) {
		t.Fatalf("disconnected owner must refuse with ErrReferenceRepoNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "kubernetes/website") || !strings.Contains(err.Error(), github.OwnerIdentityNotConnected) || !strings.Contains(err.Error(), "Settings") {
		t.Fatalf("exit-22 error must name the repo and the cure: %v", err)
	}
	if strings.Contains(err.Error(), "App") {
		t.Fatalf("installing the App is never the cure: %v", err)
	}
	if referenceRepoCredentialExitCode != 22 {
		t.Fatalf("exit code = %d, want 22", referenceRepoCredentialExitCode)
	}

	// With no owner identity store at all, write_token_file still works.
	none := referenceRepoDeps{whoAmI: deps.whoAmI}
	res, err = buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "k/w", Write: []string{"issues"}, WriteTokenFile: writeTempToken(t, "ghp_pat")}}, none, testLogger())
	if err != nil {
		t.Fatalf("token file must not need the owner: %v", err)
	}
	if res.policies[0].Login != "maintainer" || res.repos[0].CredentialLogin != "maintainer" || res.repos[0].CredentialState != config.ReferenceCredentialWriteTier {
		t.Errorf("token-file entry = %+v / %+v", res.policies[0], res.repos[0])
	}
}

// refreshOwner runs once per pass, before the credential is read, so a token
// revoked on GitHub drops the entries in the same pass.
func TestBuildReferenceRepoPoliciesRefreshesOwnerFirst(t *testing.T) {
	deps := testReferenceRepoDeps()
	connected := true
	deps.owner = func() github.OwnerCredential {
		if connected {
			return github.OwnerCredential{Login: testOwnerLogin}
		}
		return github.OwnerCredential{Unavailable: github.OwnerIdentityRevoked}
	}
	refreshed := 0
	deps.refreshOwner = func(context.Context) { refreshed++; connected = false }
	refs := []config.ReferenceRepo{{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"}}
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refreshOwner ran %d times, want 1", refreshed)
	}
	if rr := res.repos[0]; !rr.DerivedReadOnly() || rr.CredentialReason != github.OwnerIdentityRevoked {
		t.Fatalf("revocation seen by the refresh must downgrade in the same pass: %+v", rr)
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

// hivecommons/hive#18158, #18197: a derived entry never refuses boot. With
// the owner connected it keeps the write tier under the owner's login;
// without, every derived entry is served read-only and each request issue is
// told once.
func TestBuildReferenceRepoPoliciesDerived(t *testing.T) {
	ctx := context.Background()
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "other/repo", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#400"},
		{Repo: "hivecommons/hive"},
	}

	// Connected: every derived entry writes as the owner.
	res, err := buildReferenceRepoPolicies(ctx, refs, testReferenceRepoDeps(), testLogger())
	if err != nil {
		t.Fatalf("a derived entry must not refuse boot: %v", err)
	}
	if len(res.policies) != len(refs) || len(res.repos) != len(refs) {
		t.Fatalf("got %d policies, %d repos, want %d", len(res.policies), len(res.repos), len(refs))
	}
	for i := 0; i < 4; i++ {
		rr, p := res.repos[i], res.policies[i]
		if !rr.WritesIssues() || !rr.WritesPRs() || rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin != testOwnerLogin {
			t.Errorf("connected owner must keep the write tier: %+v", rr)
		}
		if !p.Issues || !p.PRs || p.Login != testOwnerLogin || p.Token == nil {
			t.Errorf("write-tier policy incomplete: %+v", p)
		}
		if rr.DerivedFrom != refs[i].DerivedFrom {
			t.Errorf("provenance lost: %+v", rr)
		}
	}
	if res.repos[4].CredentialState != "" {
		t.Errorf("hand-written read-only entry must carry no credential state: %+v", res.repos[4])
	}
	rec := &notifyRecorder{posted: true}
	notifyDerivedReadOnly(ctx, res.repos, rec.post, testLogger())
	if rec.count() != 0 {
		t.Fatalf("nothing is read-only, nothing to notify; got %d calls", rec.count())
	}

	// Disconnected: all derived entries read-only, one notice per request
	// issue with that issue's full repo set.
	res, err = buildReferenceRepoPolicies(ctx, refs, testReferenceRepoDepsDisconnected(), testLogger())
	if err != nil {
		t.Fatalf("a derived entry with no owner must not refuse boot: %v", err)
	}
	for i := 0; i < 4; i++ {
		rr, p := res.repos[i], res.policies[i]
		if !rr.DerivedReadOnly() || !rr.ReadOnly() || rr.CredentialReason != github.OwnerIdentityNotConnected || rr.CredentialLogin != "" {
			t.Errorf("no owner must downgrade to read-only: %+v", rr)
		}
		if p.Issues || p.PRs || p.Token != nil || p.Login != "" {
			t.Errorf("read-only policy carries write state: %+v", p)
		}
	}
	rec = &notifyRecorder{posted: true}
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
	if got := strings.Join(byIssue[395], ","); got != "hivecommons/docs,acme/docs,acme/site" {
		t.Errorf("#395 read-only set = %q", got)
	}
	if got := strings.Join(byIssue[400], ","); got != "other/repo" {
		t.Errorf("#400 read-only set = %q", got)
	}

	// Second reload with the same outcome: the poster finds its marker and
	// reports nothing posted; the wiring neither errors nor loops.
	rec2 := &notifyRecorder{posted: false}
	res2, err := buildReferenceRepoPolicies(ctx, refs, testReferenceRepoDepsDisconnected(), testLogger())
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

// ownerIdentityChanged re-resolves the installed policies: connecting the
// owner flips a derived entry from read-only to write tier on the proxy
// without a restart, and disconnecting flips it back.
func TestOwnerIdentityChangedReresolves(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil)
	store := github.NewOwnerIdentityStore(filepath.Join(t.TempDir(), "github-owner-token"))
	rt.deps.owner = store.Credential
	rt.deps.ownerToken = store.Token
	b.ownerIdentity = store
	b.wireReferenceReposWith(rt)
	if pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); !ok || pol.Issues || pol.PRs {
		t.Fatalf("precondition: disconnected owner serves read-only, got %+v ok=%v", pol, ok)
	}

	if err := store.Save(github.OwnerIdentity{Login: testOwnerLogin, Token: "gho_x"}); err != nil {
		t.Fatal(err)
	}
	b.ownerIdentityChanged()
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool {
		pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo)
		return ok && pol.Issues && pol.PRs && pol.Login == testOwnerLogin
	}, "connecting the owner must install the write-tier policy")

	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	b.ownerIdentityChanged()
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool {
		pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo)
		return ok && !pol.Issues && !pol.PRs
	}, "disconnecting the owner must drop the policy to read-only")

	// Nil runtime: nothing to do, no panic.
	(&boot{}).ownerIdentityChanged()
}

// writeTempToken writes value to a temp file and returns its path.
func writeTempToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

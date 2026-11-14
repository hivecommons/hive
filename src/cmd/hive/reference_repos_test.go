package main

import (
	"context"
	"errors"
	"os"
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

// testOwnerLogin is the hive owner's connected GitHub login in these tests.
const testOwnerLogin = "hive-owner"

// testReferenceRepoDeps has the owner's GitHub account connected
// (hivecommons/hive#18197).
func testReferenceRepoDeps() referenceRepoDeps {
	return referenceRepoDeps{
		ownerLogin: func() (string, error) { return testOwnerLogin, nil },
		ownerToken: func() (string, bool) { return "owner-tok", true },
	}
}

// disconnectedReferenceRepoDeps has no owner identity connected.
func disconnectedReferenceRepoDeps() referenceRepoDeps {
	return referenceRepoDeps{
		ownerLogin: func() (string, error) { return "", nil },
		ownerToken: func() (string, bool) { return "", false },
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
	if pols[1].Token == nil || !pols[1].Issues || !pols[1].PRs || pols[1].Login != testOwnerLogin {
		t.Errorf("write policy incomplete: %+v", pols[1])
	}
	if tok, ok := pols[1].Token(); !ok || tok != "owner-tok" {
		t.Errorf("write policy must serve the owner token, got %q,%v", tok, ok)
	}
	if rr := res.repos[0]; rr.CredentialState != "" || rr.CredentialLogin != "" || rr.Repo != refs[0].Repo {
		t.Errorf("hand-written read-only entry must come back unchanged: %+v", rr)
	}
	// hivecommons/hive#18197: a hand-written write entry carries its
	// credential state and the login it writes as.
	if rr := res.repos[1]; rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin != testOwnerLogin || !rr.WritesPRs() {
		t.Errorf("hand-written write entry credential = %+v", rr)
	}

	// A hand-written write-mode entry without a credential still refuses
	// boot with the exit-22 error text.
	_, err = buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "kubernetes/website", Write: []string{"issues"}}}, disconnectedReferenceRepoDeps(), testLogger())
	if !errors.Is(err, github.ErrReferenceRepoNoCredential) {
		t.Fatalf("no owner identity must refuse with ErrReferenceRepoNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "kubernetes/website") || !strings.Contains(err.Error(), "Settings → Reference repos → Connect GitHub account") {
		t.Fatalf("exit-22 error must name the repo and the cure: %v", err)
	}
	// Nil seams read as not connected.
	if _, err := buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "kubernetes/website", Write: []string{"issues"}}}, referenceRepoDeps{}, testLogger()); !errors.Is(err, github.ErrReferenceRepoNoCredential) {
		t.Fatalf("nil seams must refuse like a disconnected owner, got %v", err)
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

// hivecommons/hive#18158, #18197: a derived entry never refuses boot. With
// the owner's GitHub account connected it keeps the write tier and writes as
// the owner; without, it is served read-only and the request issue is told
// once.
func TestBuildReferenceRepoPoliciesDerived(t *testing.T) {
	ctx := context.Background()
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "other/repo", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#400"},
		{Repo: "hivecommons/hive"},
	}
	connected, err := buildReferenceRepoPolicies(ctx, refs, testReferenceRepoDeps(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 3; i++ {
		rr, p := connected.repos[i], connected.policies[i]
		if !rr.WritesIssues() || !rr.WritesPRs() || rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin != testOwnerLogin {
			t.Errorf("connected owner must keep the write tier: %+v", rr)
		}
		if !p.Issues || !p.PRs || p.Login != testOwnerLogin || p.Token == nil {
			t.Errorf("write-tier policy incomplete: %+v", p)
		}
	}

	deps := disconnectedReferenceRepoDeps()
	res, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil {
		t.Fatalf("a derived entry without an owner identity must not refuse boot: %v", err)
	}
	if len(res.policies) != len(refs) || len(res.repos) != len(refs) {
		t.Fatalf("got %d policies, %d repos, want %d", len(res.policies), len(res.repos), len(refs))
	}
	for i := 0; i <= 3; i++ {
		rr, p := res.repos[i], res.policies[i]
		if !rr.DerivedReadOnly() || !rr.ReadOnly() || rr.CredentialReason != github.DerivedReadOnlyOwnerNotConnected {
			t.Errorf("no owner identity must downgrade to read-only: %+v", rr)
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
	if got := strings.Join(byIssue[395], ","); got != "hivecommons/docs,acme/docs,acme/site" {
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

// hivecommons/hive#18197: a 401 on the owner's stored token is not an
// operator config error. Derived entries drop to read-only asking for a
// reconnect; a hand-written write entry without a token file is served
// read-only too (its Write is kept so a Settings save loses nothing), and
// one with a write_token_file keeps writing with it.
func TestBuildReferenceRepoPoliciesOwnerTokenRejected(t *testing.T) {
	dir := t.TempDir()
	tokFile := dir + "/tok"
	if err := os.WriteFile(tokFile, []byte("file-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := referenceRepoDeps{
		ownerLogin: func() (string, error) { return "", github.ErrOwnerIdentityInvalid },
		ownerToken: func() (string, bool) { return "", false },
		whoAmI:     func(context.Context, string) (string, error) { return "maintainer", nil },
	}
	refs := []config.ReferenceRepo{
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "acme/site", Write: []string{"issues"}},
		{Repo: "acme/api", Write: []string{"prs"}, WriteTokenFile: tokFile},
	}
	res, err := buildReferenceRepoPolicies(context.Background(), refs, deps, testLogger())
	if err != nil {
		t.Fatalf("a rejected owner token must not refuse: %v", err)
	}
	if rr := res.repos[0]; !rr.DerivedReadOnly() || rr.CredentialReason != github.ReferenceRepoOwnerReconnect {
		t.Errorf("derived entry = %+v", rr)
	}
	if rr, p := res.repos[1], res.policies[1]; rr.CredentialState != config.ReferenceCredentialReadOnly || rr.CredentialReason != github.ReferenceRepoOwnerReconnect || !rr.WritesIssues() || p.Issues || p.Token != nil {
		t.Errorf("hand-written entry = %+v policy = %+v", rr, p)
	}
	if rr, p := res.repos[2], res.policies[2]; rr.CredentialState != config.ReferenceCredentialWriteTier || rr.CredentialLogin != "maintainer" || !p.PRs || p.Login != "maintainer" {
		t.Errorf("token-file entry = %+v policy = %+v", rr, p)
	}
	if tok, ok := res.policies[2].Token(); !ok || tok != "file-tok" {
		t.Errorf("token-file entry must keep its own token, got %q,%v", tok, ok)
	}
}

// The real store wires through: connecting flips a derived entry to write
// tier on the next pass, disconnecting flips it back.
func TestBuildReferenceRepoPoliciesWithOwnerIdentityStore(t *testing.T) {
	store := github.NewOwnerIdentityStore(t.TempDir()+"/github-owner-token", nil)
	deps := referenceRepoDeps{ownerLogin: store.Login, ownerToken: store.Token}
	refs := []config.ReferenceRepo{{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395"}}
	ctx := context.Background()

	res, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil || !res.repos[0].DerivedReadOnly() {
		t.Fatalf("not connected: %+v err=%v", res.repos, err)
	}
	if err := store.Save(github.OwnerIdentity{Login: "owner", Token: "gho_x", Scope: github.OwnerIdentityScope}); err != nil {
		t.Fatal(err)
	}
	res, err = buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil || res.repos[0].CredentialLogin != "owner" || res.policies[0].Login != "owner" {
		t.Fatalf("connected: %+v err=%v", res.repos, err)
	}
	if tok, ok := res.policies[0].Token(); !ok || tok != "gho_x" {
		t.Fatalf("token = %q,%v", tok, ok)
	}
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	// The installed policy reads the store live: no token after disconnect.
	if _, ok := res.policies[0].Token(); ok {
		t.Fatal("a disconnected owner's token must not be served")
	}
	res, err = buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil || !res.repos[0].DerivedReadOnly() {
		t.Fatalf("disconnected: %+v err=%v", res.repos, err)
	}
}

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

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/refderive"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Fixture: the CNCF TechDocs shape. cncf/techdocs is the authorized repo,
// bound to a model whose intake names the form headings; request issue #395
// names acme/docs (App installed on acme: write tier) and zeta/site (no
// installation: read-only).
const (
	testIntakeRepoField   = "Website or documentation repository"
	testIntakeOutOfScope  = "Out of scope"
	testRequestIssueRepo  = "cncf/techdocs"
	testRequestIssueRef   = "cncf/techdocs#395"
	testDerivedWriteRepo  = "acme/docs"
	testDerivedReadOnly   = "zeta/site"
	testHandWrittenRepo   = "hivecommons/docs"
	testRequestIssueBody  = "### Project\nAcme\n\n### " + testIntakeRepoField + "\nhttps://github.com/acme/docs\nhttps://github.com/zeta/site\nhttps://github.com/cncf/toc\n\n### " + testIntakeOutOfScope + "\n_No response_\n"
	testRequestIssueTitle = "Assessment request: Acme"
)

// fakeRequestIssues is the mutable issue store behind the lister and
// accepter seams; tests close issues, drop labels or move updated_at and
// run another pass.
type fakeRequestIssues struct {
	mu            sync.Mutex
	issues        map[int]github.RequestIssueCandidate
	accepter      github.RequestIssueAccepter
	listErr       error
	accepterCalls int
}

func newFakeRequestIssues() *fakeRequestIssues {
	return &fakeRequestIssues{
		issues: map[int]github.RequestIssueCandidate{
			395: {
				Repo:      testRequestIssueRepo,
				Number:    395,
				Title:     testRequestIssueTitle,
				Body:      testRequestIssueBody,
				Labels:    []string{refderive.GrantLabel, "kind/assessment"},
				UpdatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			},
		},
		accepter: github.RequestIssueAccepter{Login: "maintainer", Permission: "maintain"},
	}
}

func (f *fakeRequestIssues) list(_ context.Context, repo, label string, limit int) ([]github.RequestIssueCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []github.RequestIssueCandidate
	for _, is := range f.issues {
		if is.Repo != repo || is.Closed || len(out) >= limit {
			continue
		}
		for _, l := range is.Labels {
			if l == label {
				out = append(out, is)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeRequestIssues) accept(_ context.Context, _ string, _ int, _ string) (github.RequestIssueAccepter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepterCalls++
	return f.accepter, nil
}

func (f *fakeRequestIssues) update(number int, fn func(*github.RequestIssueCandidate)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number]
	fn(&is)
	f.issues[number] = is
}

func (f *fakeRequestIssues) accepterCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepterCalls
}

// testIntakeFor binds cncf/techdocs to a model with intake fields and
// nothing else.
func testIntakeFor(repo string) (workflowmodel.Intake, bool) {
	if repo != testRequestIssueRepo {
		return workflowmodel.Intake{}, false
	}
	return workflowmodel.Intake{ReferenceRepoFields: []string{testIntakeRepoField}, OutOfScopeFields: []string{testIntakeOutOfScope}}, true
}

func derivedTestConfig(t *testing.T, handWritten ...config.ReferenceRepo) *config.Config {
	t.Helper()
	return &config.Config{
		SourcePath: filepath.Join(t.TempDir(), "hive.yaml"),
		Project:    config.ProjectConfig{Org: "cncf", Name: "techdocs", Repos: []string{"techdocs", "toc"}, ReferenceRepos: handWritten},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"scanner": {Role: "scanner"}},
		},
	}
}

// derivedTestBoot wires a boot with a real (ephemeral) proxy and the fake
// issue store. deps install the App on acme and hivecommons only.
func derivedTestBoot(t *testing.T, fake *fakeRequestIssues, post derivedReadOnlyPoster, handWritten ...config.ReferenceRepo) (*boot, *referenceRepoRuntime) {
	t.Helper()
	cfg := derivedTestConfig(t, handWritten...)
	px, err := proxy.NewGitHubProxyEphemeral(testLogger(), cfg.Project.Org, cfg.Project.Repos)
	if err != nil {
		t.Fatal(err)
	}
	deps := testReferenceRepoDeps()
	deps.discover = func(_ context.Context, org string) (int64, error) {
		switch org {
		case "acme", "hivecommons":
			return 1, nil
		}
		return 0, github.ErrNoInstallationForOrg
	}
	b := &boot{cfg: cfg, logger: testLogger(), ctx: context.Background(), githubProxy: px}
	rt := &referenceRepoRuntime{
		deriver: newReferenceRepoDeriver(fake.list, fake.accept, testIntakeFor, refderive.DefaultHost),
		deps:    deps,
		post:    post,
	}
	return b, rt
}

func referenceRepoByName(refs []config.ReferenceRepo, repo string) (config.ReferenceRepo, bool) {
	for _, rr := range refs {
		if strings.EqualFold(rr.Repo, repo) {
			return rr, true
		}
	}
	return config.ReferenceRepo{}, false
}

// Boot derives from the accepted issue: the write-tier entry reaches the
// config and the proxy at issues+prs with its provenance, the uninstalled
// org's entry is read-only and the request issue is told once, and the
// authorized repo the form also named is never derived.
func TestWireReferenceReposDerivesAtBoot(t *testing.T) {
	fake := newFakeRequestIssues()
	rec := &notifyRecorder{posted: true}
	b, rt := derivedTestBoot(t, fake, rec.post, config.ReferenceRepo{Repo: testHandWrittenRepo})
	b.wireReferenceReposWith(rt)

	refs := b.cfg.Project.ReferenceRepos
	if len(refs) != 3 {
		t.Fatalf("want hand-written + 2 derived, got %+v", refs)
	}
	if refs[0].Repo != testHandWrittenRepo || refs[0].Derived() {
		t.Errorf("hand-written entry must come first and unchanged: %+v", refs[0])
	}
	docs, ok := referenceRepoByName(refs, testDerivedWriteRepo)
	if !ok || docs.DerivedFrom != testRequestIssueRef || !docs.WritesIssues() || !docs.WritesPRs() || docs.CredentialState != config.ReferenceCredentialWriteTier {
		t.Errorf("derived write-tier entry = %+v", docs)
	}
	site, ok := referenceRepoByName(refs, testDerivedReadOnly)
	if !ok || !site.DerivedReadOnly() || !site.ReadOnly() {
		t.Errorf("derived entry for an uninstalled org must be read-only: %+v", site)
	}
	if _, derived := referenceRepoByName(refs, "cncf/toc"); derived {
		t.Error("an authorized repo named by the form must never become a reference repo")
	}
	pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo)
	if !ok || !pol.Issues || !pol.PRs {
		t.Errorf("proxy policy for the derived repo = %+v ok=%v", pol, ok)
	}
	if pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedReadOnly); !ok || pol.Issues || pol.PRs {
		t.Errorf("proxy policy for the read-only derived repo = %+v ok=%v", pol, ok)
	}
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool { return rec.count() == 1 }, "read-only notice must be posted once")
	rec.mu.Lock()
	calls := rec.calls
	rec.mu.Unlock()
	if len(calls) != 1 || calls[0].repo != testRequestIssueRepo || calls[0].number != 395 || len(calls[0].repos) != 1 || calls[0].repos[0] != testDerivedReadOnly {
		t.Errorf("notice calls = %+v", calls)
	}
}

// The grant ends with the label: the next pass drops the entry from the
// config and the proxy, which then refuses writes to the repo again.
func TestRederiveDropsEntryWhenLabelRemoved(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil)
	b.wireReferenceReposWith(rt)
	if _, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); !ok {
		t.Fatal("precondition: derived policy installed")
	}

	fake.update(395, func(is *github.RequestIssueCandidate) {
		is.Labels = []string{"kind/assessment"}
		is.UpdatedAt = is.UpdatedAt.Add(time.Minute)
	})
	b.rederiveReferenceRepos(context.Background())

	if len(b.cfg.Project.ReferenceRepos) != 0 {
		t.Fatalf("label removed: entries must drop, got %+v", b.cfg.Project.ReferenceRepos)
	}
	if _, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); ok {
		t.Fatal("label removed: proxy must no longer hold a policy for the repo")
	}
}

func TestRederiveDropsEntryWhenIssueClosed(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil)
	b.wireReferenceReposWith(rt)

	fake.update(395, func(is *github.RequestIssueCandidate) { is.Closed = true })
	b.rederiveReferenceRepos(context.Background())

	if len(b.cfg.Project.ReferenceRepos) != 0 {
		t.Fatalf("closed issue: entries must drop, got %+v", b.cfg.Project.ReferenceRepos)
	}
	if _, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); ok {
		t.Fatal("closed issue: proxy must no longer hold a policy for the repo")
	}
}

// A hand-written entry for the same repo wins: the operator's read-only
// acme/docs stays read-only, the derived issues+prs grant is dropped.
func TestWireReferenceReposHandWrittenWins(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil, config.ReferenceRepo{Repo: testDerivedWriteRepo})
	b.wireReferenceReposWith(rt)

	var matches int
	for _, rr := range b.cfg.Project.ReferenceRepos {
		if strings.EqualFold(rr.Repo, testDerivedWriteRepo) {
			matches++
			if rr.Derived() || !rr.ReadOnly() {
				t.Errorf("hand-written entry must win unchanged, got %+v", rr)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("repo listed %d times, want once", matches)
	}
	if pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); !ok || pol.Issues || pol.PRs {
		t.Errorf("proxy must enforce the hand-written read-only tier, got %+v ok=%v", pol, ok)
	}
}

// Refused grants: a bot applier and a triage-only applier derive nothing.
func TestDeriveRefusesBotAndTriageAccepters(t *testing.T) {
	for name, acc := range map[string]github.RequestIssueAccepter{
		"bot":    {Login: "hive-app[bot]", IsBot: true, Permission: "write"},
		"triage": {Login: "triager", Permission: "triage"},
		"none":   {},
	} {
		fake := newFakeRequestIssues()
		fake.accepter = acc
		d := newReferenceRepoDeriver(fake.list, fake.accept, testIntakeFor, refderive.DefaultHost)
		if got := d.derive(context.Background(), derivedTestConfig(t), testLogger()); len(got) != 0 {
			t.Errorf("%s accepter must grant nothing, got %+v", name, got)
		}
	}
}

// The accepter is cached by (issue, updated_at): an unchanged issue costs no
// timeline read on the next pass, a moved one is re-read, and a listing
// error keeps the previous entries.
func TestDeriveCachesAccepterAndKeepsPreviousOnListError(t *testing.T) {
	fake := newFakeRequestIssues()
	cfg := derivedTestConfig(t)
	d := newReferenceRepoDeriver(fake.list, fake.accept, testIntakeFor, refderive.DefaultHost)
	ctx := context.Background()

	first := d.derive(ctx, cfg, testLogger())
	if len(first) != 2 || fake.accepterCallCount() != 1 {
		t.Fatalf("first pass: %d entries, %d accepter calls", len(first), fake.accepterCallCount())
	}
	d.derive(ctx, cfg, testLogger())
	if fake.accepterCallCount() != 1 {
		t.Fatalf("unchanged issue must not re-read the accepter, calls = %d", fake.accepterCallCount())
	}
	fake.update(395, func(is *github.RequestIssueCandidate) { is.UpdatedAt = is.UpdatedAt.Add(time.Hour) })
	d.derive(ctx, cfg, testLogger())
	if fake.accepterCallCount() != 2 {
		t.Fatalf("moved updated_at must re-read the accepter, calls = %d", fake.accepterCallCount())
	}

	fake.mu.Lock()
	fake.listErr = errors.New("502 from the forge")
	fake.mu.Unlock()
	kept := d.derive(ctx, cfg, testLogger())
	if len(kept) != 2 {
		t.Fatalf("a listing error must keep the previous entries, got %+v", kept)
	}
}

func TestMergeReferenceRepos(t *testing.T) {
	hand := []config.ReferenceRepo{{Repo: "Acme/Docs"}}
	derived := []config.ReferenceRepo{
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "o/r#1"},
		{Repo: "zeta/site", Write: []string{"issues", "prs", "admin"}, WriteTokenFile: "/x", DerivedFrom: "o/r#1", CredentialState: config.ReferenceCredentialWriteTier},
		{Repo: "Zeta/Site", Write: []string{"issues"}, DerivedFrom: "o/r#2"},
		{Repo: "cncf/toc", Write: []string{"issues"}, DerivedFrom: "o/r#2"},
		{Repo: "no/provenance", Write: []string{"issues"}},
	}
	got := mergeReferenceRepos(hand, derived, []string{"cncf/techdocs", "cncf/toc"}, testLogger())
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Repo != "Acme/Docs" || got[0].Derived() || !got[0].ReadOnly() {
		t.Errorf("hand-written must win unchanged: %+v", got[0])
	}
	z := got[1]
	if z.Repo != "zeta/site" || z.DerivedFrom != "o/r#1" || z.WriteTokenFile != "" || z.CredentialState != config.ReferenceCredentialWriteTier {
		t.Errorf("first derived entry must win with its state: %+v", z)
	}
	if len(z.Write) != 2 || !z.WritesIssues() || !z.WritesPRs() {
		t.Errorf("derived write must be clamped to issues+prs: %v", z.Write)
	}
	if out := mergeReferenceRepos(nil, nil, nil, nil); len(out) != 0 {
		t.Errorf("empty merge = %+v", out)
	}
}

// A derived entry never lands in a saved config: the overlay and runtime
// file writers both go through Config.Save, whose YAML must name the
// hand-written repo and none of the derived ones (hivecommons/hive#18137).
func TestSavedConfigHasNoDerivedReferenceRepos(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil, config.ReferenceRepo{Repo: testHandWrittenRepo})
	b.wireReferenceReposWith(rt)
	if len(b.cfg.Project.ReferenceRepos) != 3 {
		t.Fatalf("precondition: derived entries in memory, got %+v", b.cfg.Project.ReferenceRepos)
	}
	if err := b.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(b.cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	saved := string(data)
	if !strings.Contains(saved, testHandWrittenRepo) {
		t.Errorf("saved config must keep the hand-written entry:\n%s", saved)
	}
	for _, leak := range []string{testDerivedWriteRepo, testDerivedReadOnly, testRequestIssueRef, "derived_from", "credential_state"} {
		if strings.Contains(saved, leak) {
			t.Errorf("saved config leaks derived state %q:\n%s", leak, saved)
		}
	}
	// The in-memory list is untouched by the save.
	if len(b.cfg.Project.ReferenceRepos) != 3 {
		t.Errorf("save must not strip the in-memory derived entries: %+v", b.cfg.Project.ReferenceRepos)
	}
}

// A config reload replaces cfg.Project.ReferenceRepos with the file's
// hand-written list; the hook merges the last derived entries back at once
// so the kick never loses them while the background pass runs.
func TestReconcileReferenceReposAfterReloadKeepsDerived(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil, config.ReferenceRepo{Repo: testHandWrittenRepo})
	b.wireReferenceReposWith(rt)

	// Simulate the reload swap: the file carries only the hand-written entry.
	b.cfg.Project.ReferenceRepos = []config.ReferenceRepo{{Repo: testHandWrittenRepo}}
	b.remergeLastDerivedReferenceRepos()
	refs := b.cfg.Project.ReferenceRepos
	if len(refs) != 3 || refs[0].Repo != testHandWrittenRepo {
		t.Fatalf("derived entries must be merged back under the hand-written one, got %+v", refs)
	}
	docs, ok := referenceRepoByName(refs, testDerivedWriteRepo)
	if !ok || docs.CredentialState != config.ReferenceCredentialWriteTier || !docs.WritesPRs() {
		t.Fatalf("re-merged entry must keep its resolved state, got %+v", docs)
	}

	// The full hook also re-lists in the background; with the label gone the
	// pass drops the entry from the proxy.
	fake.update(395, func(is *github.RequestIssueCandidate) { is.Labels = nil })
	b.reconcileReferenceReposAfterReload()
	testutil.Eventually(t, referenceRepoTestTimeout, func() bool {
		_, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo)
		return !ok
	}, "background pass after reload must drop the ended grant from the proxy")
}

func TestReferenceRepoDeriveInterval(t *testing.T) {
	t.Setenv(referenceRepoDeriveIntervalEnv, "")
	if got := referenceRepoDeriveInterval(); got != referenceRepoDeriveIntervalDefault {
		t.Errorf("unset = %v, want %v", got, referenceRepoDeriveIntervalDefault)
	}
	t.Setenv(referenceRepoDeriveIntervalEnv, "3m")
	if got := referenceRepoDeriveInterval(); got != 3*time.Minute {
		t.Errorf("3m = %v", got)
	}
	for _, bad := range []string{"soon", "-1m", "0"} {
		t.Setenv(referenceRepoDeriveIntervalEnv, bad)
		if got := referenceRepoDeriveInterval(); got != referenceRepoDeriveIntervalDefault {
			t.Errorf("%q = %v, want default", bad, got)
		}
	}
}

// The dashboard editor's hook (hivecommons/hive#18136) installs the edited
// hand-written list with the last derived set still merged in: a derived
// entry the editor echoes back is not re-verified as hand-written, a new
// hand-written entry gets its policy, and a removed one loses it. The
// remerge the handler runs after the save then restores the derived entries
// in memory under the new list.
func TestApplyEditedReferenceReposKeepsDerivedAndRemerges(t *testing.T) {
	fake := newFakeRequestIssues()
	b, rt := derivedTestBoot(t, fake, nil, config.ReferenceRepo{Repo: testHandWrittenRepo})
	b.wireReferenceReposWith(rt)
	derivedBefore := len(rt.lastDerived)
	if derivedBefore == 0 {
		t.Fatal("precondition: boot derived entries")
	}

	// Echo of the GET body (hand-written + derived) with the hand-written
	// entry swapped for a new read-only one.
	edited := append([]config.ReferenceRepo{{Repo: "other/ref"}}, rt.lastDerived...)
	if err := b.applyEditedReferenceRepos(context.Background(), edited); err != nil {
		t.Fatalf("applyEditedReferenceRepos: %v", err)
	}
	if _, ok := b.githubProxy.ReferenceRepoPolicyFor("other/ref"); !ok {
		t.Error("new hand-written entry must get a proxy policy")
	}
	if _, ok := b.githubProxy.ReferenceRepoPolicyFor(testHandWrittenRepo); ok {
		t.Error("removed hand-written entry must lose its proxy policy")
	}
	if pol, ok := b.githubProxy.ReferenceRepoPolicyFor(testDerivedWriteRepo); !ok || !pol.Issues || !pol.PRs {
		t.Errorf("derived write-tier entry must stay enforced, got %+v ok=%v", pol, ok)
	}
	if len(rt.lastDerived) != derivedBefore {
		t.Errorf("an edit must not change the derived set: %d -> %d", derivedBefore, len(rt.lastDerived))
	}

	// What the handler does after the file is written.
	b.cfg.Project.ReferenceRepos = []config.ReferenceRepo{{Repo: "other/ref"}}
	b.remergeLastDerivedReferenceRepos()
	refs := b.cfg.Project.ReferenceRepos
	if len(refs) != 1+derivedBefore || refs[0].Repo != "other/ref" || refs[0].Derived() {
		t.Fatalf("derived entries must be merged back under the saved list, got %+v", refs)
	}
	if docs, ok := referenceRepoByName(refs, testDerivedWriteRepo); !ok || !docs.Derived() || docs.CredentialState != config.ReferenceCredentialWriteTier {
		t.Fatalf("re-merged derived entry must keep its provenance and state, got %+v", docs)
	}
}

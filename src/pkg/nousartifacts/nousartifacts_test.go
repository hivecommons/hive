package nousartifacts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/nousengine"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// fakeForge records every call and lets a test fail any step.
type fakeForge struct {
	mu            sync.Mutex
	defaultBranch string
	private       bool
	privateErr    error
	branches      map[string]string // branch → sha
	commits       []fakeCommit
	prs           []fakePR
	calls         []string

	failBranchHead  error
	failCreate      error
	failCommit      error
	failPR          error
	failDefault     error
	nextCommitIndex int
}

type fakeCommit struct {
	branch, expected, headline, body string
	files                            []File
}

type fakePR struct{ head, base, title, body string }

func newFakeForge() *fakeForge {
	return &fakeForge{defaultBranch: "main", private: true, branches: map[string]string{"main": "base0000"}}
}

func (f *fakeForge) record(s string) {
	f.calls = append(f.calls, s)
}

func (f *fakeForge) DefaultBranch(_ context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DefaultBranch " + repo)
	return f.defaultBranch, f.failDefault
}

func (f *fakeForge) BranchHead(_ context.Context, repo, branch string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("BranchHead " + branch)
	if f.failBranchHead != nil {
		return "", false, f.failBranchHead
	}
	sha, ok := f.branches[branch]
	return sha, ok, nil
}

func (f *fakeForge) CreateBranch(_ context.Context, repo, branch, from string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateBranch " + branch + "@" + from)
	if f.failCreate != nil {
		return f.failCreate
	}
	f.branches[branch] = from
	return nil
}

func (f *fakeForge) CommitFiles(_ context.Context, repo, branch, expected, headline, body string, files []File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CommitFiles " + branch)
	if f.failCommit != nil {
		return "", f.failCommit
	}
	if f.branches[branch] != expected {
		return "", errors.New("expected head mismatch")
	}
	f.nextCommitIndex++
	sha := "commit" + strings.Repeat("0", 3) + string(rune('0'+f.nextCommitIndex))
	f.branches[branch] = sha
	f.commits = append(f.commits, fakeCommit{branch: branch, expected: expected, headline: headline, body: body, files: files})
	return sha, nil
}

func (f *fakeForge) CreatePullRequest(_ context.Context, repo, head, base, title, body string) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreatePullRequest " + head + "->" + base)
	if f.failPR != nil {
		return PullRequest{}, f.failPR
	}
	for i, pr := range f.prs {
		if pr.head == head {
			return PullRequest{Number: i + 1, URL: "https://example/pr/" + head, AlreadyExisted: true}, nil
		}
	}
	f.prs = append(f.prs, fakePR{head: head, base: base, title: title, body: body})
	return PullRequest{Number: len(f.prs), URL: "https://example/pr/" + head}, nil
}

func (f *fakeForge) RepoIsPrivate(_ context.Context, repo string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RepoIsPrivate " + repo)
	return f.private, f.privateErr
}

func (f *fakeForge) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type auditEntry struct {
	action, detail, repo string
	target               int
}

type harness struct {
	t      *testing.T
	root   string
	forge  *fakeForge
	syncer *Syncer
	clock  time.Time
	audits []auditEntry
	status []Status
	budget func() (bool, string)
}

func newHarness(t *testing.T, settings Settings) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), forge: newFakeForge(), clock: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	settings.Root = h.root
	if settings.Repo == "" {
		settings.Repo = "org/nous-results"
	}
	h.syncer = New(settings, h.forge, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.syncer.Now = func() time.Time { return h.clock }
	h.syncer.Audit = func(action, detail, repo string, target int) {
		h.audits = append(h.audits, auditEntry{action, detail, repo, target})
	}
	h.syncer.Publish = func(s Status) { h.status = append(h.status, s) }
	h.syncer.BudgetOK = func() (bool, string) {
		if h.budget != nil {
			return h.budget()
		}
		return true, ""
	}
	return h
}

// writeCampaign creates <root>/<id>/ with the given files, all stamped at
// mtime so the quiet-period logic is deterministic.
func (h *harness) writeCampaign(id string, mtime time.Time, files map[string]string) {
	h.t.Helper()
	dir := filepath.Join(h.root, id)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) lastAudit() auditEntry {
	h.t.Helper()
	if len(h.audits) == 0 {
		h.t.Fatal("no audit entry recorded")
	}
	return h.audits[len(h.audits)-1]
}

func TestDisabledSyncerMakesNoCalls(t *testing.T) {
	s := New(Settings{Root: t.TempDir()}, nil, nil)
	if s.Enabled() {
		t.Fatal("empty repo must be disabled")
	}
	res := s.RunOnce(context.Background(), true)
	if res.Skipped == "" {
		t.Fatalf("disabled run must report a skip, got %+v", res)
	}
	if st := s.Status(); st.Enabled || st.Repo != "" {
		t.Fatalf("status = %+v", st)
	}
	var nilSyncer *Syncer
	if nilSyncer.Enabled() {
		t.Fatal("nil syncer must be disabled")
	}
	if _, err := os.Stat(filepath.Join(s.settings.Root, StateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled syncer must not write state")
	}
}

func TestNilForgeSkips(t *testing.T) {
	s := New(Settings{Repo: "o/r", Root: t.TempDir()}, nil, nil)
	res := s.RunOnce(context.Background(), false)
	if !strings.Contains(res.Skipped, "no GitHub client") {
		t.Fatalf("got %+v", res)
	}
}

func TestCompletedCampaignIsPushedOnceAsPR(t *testing.T) {
	h := newHarness(t, Settings{Branch: "results", PathPrefix: "/campaigns/"})
	h.writeCampaign("run-42", h.clock, map[string]string{
		nousstore.LedgerFile:        `{"iterations":[1]}`,
		nousengine.MetaFindingsFile: `{"deployment_recommendation":"deploy"}`,
		"runs/report.md":            "# report\n",
	})
	h.forge.branches["results"] = "resultsbase"

	res := h.syncer.RunOnce(context.Background(), false)
	if res.Skipped != "" || len(res.Pushed) != 1 || res.Pushed[0] != "run-42" || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if h.forge.callCount("DefaultBranch") != 0 {
		t.Fatal("explicit branch must not resolve the default")
	}
	if len(h.forge.commits) != 1 {
		t.Fatalf("commits = %d", len(h.forge.commits))
	}
	c := h.forge.commits[0]
	if c.branch != BranchPrefix+"run-42" || c.expected != "resultsbase" {
		t.Fatalf("commit on %s expecting %s", c.branch, c.expected)
	}
	if c.headline != CommitHeadline("run-42", StatusCompleted, 3) || !strings.Contains(c.body, "Fingerprint: ") || strings.Contains(c.body, "2026") {
		t.Fatalf("commit message must be deterministic: %q / %q", c.headline, c.body)
	}
	paths := make([]string, 0, len(c.files))
	for _, f := range c.files {
		paths = append(paths, f.Path)
	}
	want := []string{
		"campaigns/run-42/ledger.json",
		"campaigns/run-42/meta_findings.json",
		"campaigns/run-42/runs/report.md",
		"campaigns/run-42/" + ManifestFile,
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v", paths)
	}
	var m manifest
	if err := json.Unmarshal(c.files[3].Content, &m); err != nil {
		t.Fatal(err)
	}
	if m.Campaign != "run-42" || m.Status != StatusCompleted || len(m.Files) != 3 || m.Fingerprint == "" || !m.SyncedAt.Equal(h.clock) {
		t.Fatalf("manifest = %+v", m)
	}
	if len(h.forge.prs) != 1 || h.forge.prs[0].base != "results" || !strings.Contains(h.forge.prs[0].body, "Nothing is promoted") {
		t.Fatalf("prs = %+v", h.forge.prs)
	}
	a := h.lastAudit()
	if a.action != AuditActionPush || a.repo != "org/nous-results" || a.target != 1 || !strings.Contains(a.detail, "campaign=run-42") {
		t.Fatalf("audit = %+v", a)
	}
	st := h.syncer.Status()
	if st.LastSyncAt == nil || st.LastError != "" || st.Campaigns["run-42"].PRNumber != 1 || st.Campaigns["run-42"].Commit == "" {
		t.Fatalf("status = %+v", st)
	}
	if st.Public == nil || *st.Public {
		t.Fatalf("private repo must be reported as not public: %+v", st.Public)
	}

	// Same content again: no second push, state remembered.
	h.clock = h.clock.Add(time.Hour)
	res = h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 0 || res.Unchanged != 1 || len(h.forge.commits) != 1 {
		t.Fatalf("unchanged content must not be re-pushed: %+v commits=%d", res, len(h.forge.commits))
	}
	if h.forge.callCount("RepoIsPrivate") != 1 {
		t.Fatal("visibility must be looked up once")
	}

	// Changed content → one more commit on the same branch, PR reused.
	h.writeCampaign("run-42", h.clock, map[string]string{"runs/report.md": "# report v2\n"})
	res = h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 || len(h.forge.commits) != 2 || len(h.forge.prs) != 1 {
		t.Fatalf("changed content: %+v commits=%d prs=%d", res, len(h.forge.commits), len(h.forge.prs))
	}
	if h.forge.commits[1].expected != h.forge.branches[BranchPrefix+"run-42"] && h.forge.callCount("CreateBranch") != 1 {
		t.Fatal("existing branch must be reused, not recreated")
	}

	// State survives a restart.
	again := New(Settings{Repo: "org/nous-results", Branch: "results", PathPrefix: "campaigns", Root: h.root}, h.forge, nil)
	again.Now = func() time.Time { return h.clock }
	if rec := again.Status().Campaigns["run-42"]; rec.Commit != h.forge.branches[BranchPrefix+"run-42"] {
		t.Fatalf("state not reloaded: %+v", rec)
	}
	if res := again.RunOnce(context.Background(), false); len(res.Pushed) != 0 || res.Unchanged != 1 {
		t.Fatalf("reloaded state must prevent a re-push: %+v", res)
	}
}

func TestRunningCampaignWaitsThenIncomplete(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("run-7", h.clock.Add(-QuietPeriod/2), map[string]string{nousstore.LedgerFile: `{}`})

	res := h.syncer.RunOnce(context.Background(), false)
	if res.Running != 1 || len(res.Pushed) != 0 {
		t.Fatalf("fresh folder without a verdict must be treated as running: %+v", res)
	}

	h.clock = h.clock.Add(QuietPeriod)
	res = h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 {
		t.Fatalf("quiet folder must be mirrored as incomplete: %+v", res)
	}
	if h.forge.commits[0].headline != CommitHeadline("run-7", StatusIncomplete, 1) {
		t.Fatalf("headline = %q", h.forge.commits[0].headline)
	}
	if !strings.Contains(h.forge.prs[0].body, "marked incomplete") || !strings.Contains(h.forge.prs[0].title, StatusIncomplete) {
		t.Fatalf("pr = %+v", h.forge.prs[0])
	}
}

func TestGovernorFolderIsALedger(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign(nousstore.GovernorSubdir, h.clock.Add(-QuietPeriod), map[string]string{
		nousstore.LedgerFile:        `{"iterations":[]}`,
		nousengine.MetaFindingsFile: `{}`, // ignored for the governor folder
	})
	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 || h.syncer.Status().Campaigns[nousstore.GovernorSubdir].Status != StatusLedger {
		t.Fatalf("res=%+v status=%+v", res, h.syncer.Status().Campaigns)
	}
}

func TestDiscoverSkipsNonCampaignEntries(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	old := h.clock.Add(-2 * QuietPeriod)
	h.writeCampaign(nousstore.SnapshotsSubdir, old, map[string]string{"1.json": "{}"})
	h.writeCampaign(nousstore.GatesSubdir, old, map[string]string{"history.jsonl": "{}"})
	h.writeCampaign("cache", old, map[string]string{"deps/x": "x"})
	h.writeCampaign(".hidden", old, map[string]string{"a": "a"})
	if err := os.MkdirAll(filepath.Join(h.root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, nousstore.PanelConfigFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, StateFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.writeCampaign("real", old, map[string]string{"report.md": "r"})

	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 || res.Pushed[0] != "real" || res.Running != 0 {
		t.Fatalf("only the real campaign may be mirrored: %+v", res)
	}
}

func TestMissingRootIsNotAnError(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.syncer.settings.Root = filepath.Join(h.root, "does-not-exist")
	res := h.syncer.RunOnce(context.Background(), false)
	if res.Skipped != "" || len(res.Failed) != 0 || h.syncer.Status().LastError != "" {
		t.Fatalf("missing root: %+v", res)
	}
}

func TestCollectFiltersRedactsAndCaps(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main", PathPrefix: "p"})
	old := h.clock.Add(-QuietPeriod)
	ghToken := "ghp_" + strings.Repeat("a", 36)
	files := map[string]string{
		"ledger.json":            `{"ok":true}`,
		"notes.md":               "token is " + ghToken + " end",
		"scratch.tmp":            "temp",
		"dependencies.env":       "NOUS_DEP_X=/path",
		".hidden":                "hidden",
		"creds/github_token.txt": "ghp_x",
		"keys/server.pem":        "-----BEGIN PRIVATE KEY-----",
		"id_rsa":                 "x",
		"my-secret-notes.txt":    "x",
		"password.txt":           "x",
		".git/config":            "x",
		"__pycache__/a.pyc":      "x",
		"runs/bin.dat":           "\x00\x01\x02binary",
		"runs/" + ManifestFile:   "nested manifest is fine",
		ManifestFile:             "old manifest",
		"runs/report.md":         "# ok",
	}
	h.writeCampaign("c1", old, files)
	big := strings.Repeat("x", MaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(h.root, "c1", "huge.bin"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(h.root, "c1", "huge.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.root, "c1", "ledger.json"), filepath.Join(h.root, "c1", "link.json")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}

	b, err := h.syncer.collect(campaign{id: "c1", dir: filepath.Join(h.root, "c1"), status: StatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range b.files {
		got[f.Path] = string(f.Content)
	}
	wantPaths := []string{"p/c1/ledger.json", "p/c1/notes.md", "p/c1/runs/bin.dat", "p/c1/runs/" + ManifestFile, "p/c1/runs/report.md"}
	if len(got) != len(wantPaths) {
		t.Fatalf("files = %v", got)
	}
	for _, p := range wantPaths {
		if _, ok := got[p]; !ok {
			t.Errorf("missing %s in %v", p, got)
		}
	}
	if strings.Contains(got["p/c1/notes.md"], ghToken) || !strings.Contains(got["p/c1/notes.md"], "token is ") {
		t.Fatalf("token must be redacted: %q", got["p/c1/notes.md"])
	}
	if got["p/c1/runs/bin.dat"] != files["runs/bin.dat"] {
		t.Fatal("binary content must pass through untouched")
	}
	if b.redacted != 1 {
		t.Fatalf("redacted = %d", b.redacted)
	}
	omitted := map[string]string{}
	for _, o := range b.manifest.Omitted {
		omitted[o.Path] = o.Reason
	}
	for p, reason := range map[string]string{
		"scratch.tmp":            OmitReasonDenylist,
		"dependencies.env":       OmitReasonDenylist,
		".hidden":                OmitReasonDenylist,
		"creds/github_token.txt": OmitReasonDenylist,
		"keys/server.pem":        OmitReasonDenylist,
		"id_rsa":                 OmitReasonDenylist,
		"my-secret-notes.txt":    OmitReasonDenylist,
		"password.txt":           OmitReasonDenylist,
		ManifestFile:             OmitReasonDenylist,
		"huge.bin":               OmitReasonFileSize,
		"link.json":              OmitReasonSymlink,
	} {
		if omitted[p] != reason {
			t.Errorf("omitted[%s] = %q, want %q", p, omitted[p], reason)
		}
	}
	for _, p := range []string{".git/config", "__pycache__/a.pyc"} {
		if _, ok := omitted[p]; ok {
			t.Errorf("%s is inside a skipped directory and must not be listed", p)
		}
	}
	for i := 1; i < len(b.manifest.Files); i++ {
		if b.manifest.Files[i-1].Path >= b.manifest.Files[i].Path {
			t.Fatalf("manifest files must be sorted: %v", b.manifest.Files)
		}
	}

	again, err := h.syncer.collect(campaign{id: "c1", dir: filepath.Join(h.root, "c1"), status: StatusCompleted})
	if err != nil || again.fingerprint != b.fingerprint {
		t.Fatalf("fingerprint must be deterministic: %s vs %s (%v)", b.fingerprint, again.fingerprint, err)
	}
	other, _ := h.syncer.collect(campaign{id: "c1", dir: filepath.Join(h.root, "c1"), status: StatusIncomplete})
	if other.fingerprint == b.fingerprint {
		t.Fatal("status must be part of the fingerprint")
	}
}

func TestCampaignWithOnlyDeniedFilesFails(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("c", h.clock.Add(-QuietPeriod), map[string]string{"a.tmp": "x", "secret.txt": "y"})
	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 0 || !strings.Contains(res.Failed["c"], "no mirrorable files") {
		t.Fatalf("res = %+v", res)
	}
	if h.lastAudit().action != AuditActionPushFailed {
		t.Fatalf("audit = %+v", h.lastAudit())
	}
}

func TestBudgetSkipRecordsReasonAndMakesNoCalls(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("c", h.clock.Add(-QuietPeriod), map[string]string{"a.md": "x"})
	h.budget = func() (bool, string) { return false, "mode conserve" }

	res := h.syncer.RunOnce(context.Background(), true)
	if !strings.Contains(res.Skipped, "mode conserve") || len(h.forge.calls) != 0 {
		t.Fatalf("budget skip: %+v calls=%v", res, h.forge.calls)
	}
	st := h.syncer.Status()
	if !strings.Contains(st.LastSkipReason, "mode conserve") || st.LastAttemptAt != nil {
		t.Fatalf("status = %+v", st)
	}
	if len(h.status) == 0 || h.status[len(h.status)-1].LastSkipReason != st.LastSkipReason {
		t.Fatal("skip must be published")
	}

	h.budget = nil
	res = h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 || h.syncer.Status().LastSkipReason != "" {
		t.Fatalf("budget recovered: %+v %+v", res, h.syncer.Status())
	}
}

func TestFailureBacksOffAndForceBypasses(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("c", h.clock.Add(-QuietPeriod), map[string]string{"a.md": "x"})
	h.forge.failCommit = errors.New("boom 500")

	res := h.syncer.RunOnce(context.Background(), false)
	if !strings.Contains(res.Failed["c"], "boom 500") {
		t.Fatalf("res = %+v", res)
	}
	st := h.syncer.Status()
	if st.Failures != 1 || st.NextAttemptAt == nil || !st.NextAttemptAt.Equal(h.clock.Add(InitialBackoff)) || !strings.Contains(st.LastError, "boom 500") || st.LastSyncAt != nil {
		t.Fatalf("status after failure = %+v", st)
	}
	if rec := st.Campaigns["c"]; !strings.Contains(rec.LastError, "boom 500") || rec.Commit != "" {
		t.Fatalf("campaign record = %+v", rec)
	}
	if a := h.lastAudit(); a.action != AuditActionPushFailed || !strings.Contains(a.detail, "boom 500") {
		t.Fatalf("audit = %+v", a)
	}

	commits := h.forge.callCount("CommitFiles")
	h.clock = h.clock.Add(InitialBackoff / 2)
	res = h.syncer.RunOnce(context.Background(), false)
	if !strings.HasPrefix(res.Skipped, "backing off") || h.forge.callCount("CommitFiles") != commits {
		t.Fatalf("inside backoff window the run must be skipped: %+v", res)
	}

	res = h.syncer.RunOnce(context.Background(), true)
	if res.Skipped != "" || h.forge.callCount("CommitFiles") != commits+1 {
		t.Fatalf("force must bypass backoff: %+v", res)
	}
	if st := h.syncer.Status(); st.Failures != 2 || !st.NextAttemptAt.Equal(h.clock.Add(2*InitialBackoff)) {
		t.Fatalf("second failure must double the backoff: %+v", st)
	}

	h.forge.failCommit = nil
	h.clock = h.clock.Add(MaxBackoff)
	res = h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 {
		t.Fatalf("recovery: %+v", res)
	}
	if st := h.syncer.Status(); st.Failures != 0 || st.NextAttemptAt != nil || st.LastError != "" || st.LastSyncAt == nil {
		t.Fatalf("success must clear the backoff: %+v", st)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("c", h.clock.Add(-QuietPeriod), map[string]string{"a.md": "x"})
	h.forge.failCommit = errors.New("down")
	for i := 0; i < 12; i++ {
		h.syncer.RunOnce(context.Background(), true)
	}
	st := h.syncer.Status()
	if st.Failures != 12 || !st.NextAttemptAt.Equal(h.clock.Add(MaxBackoff)) {
		t.Fatalf("backoff must cap at %v: %+v", MaxBackoff, st)
	}
}

func TestPRFailureAfterCommitIsSurfaced(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.writeCampaign("c", h.clock.Add(-QuietPeriod), map[string]string{"a.md": "x"})
	h.forge.failPR = errors.New("pr refused")
	res := h.syncer.RunOnce(context.Background(), false)
	if !strings.Contains(res.Failed["c"], "pushed to") || !strings.Contains(res.Failed["c"], "pr refused") {
		t.Fatalf("res = %+v", res)
	}
	if rec := h.syncer.Status().Campaigns["c"]; rec.Commit != "" {
		t.Fatal("a failed PR must not mark the campaign as mirrored")
	}
	// Next run (forced) retries: branch exists now, commit lands on top.
	h.forge.failPR = nil
	res = h.syncer.RunOnce(context.Background(), true)
	if len(res.Pushed) != 1 || h.forge.callCount("CreateBranch") != 1 || len(h.forge.commits) != 2 {
		t.Fatalf("retry: %+v create=%d commits=%d", res, h.forge.callCount("CreateBranch"), len(h.forge.commits))
	}
}

func TestDefaultBranchResolvedOnceAndErrorsSurface(t *testing.T) {
	h := newHarness(t, Settings{})
	h.writeCampaign("a", h.clock.Add(-QuietPeriod), map[string]string{"x": "1"})
	h.writeCampaign("b", h.clock.Add(-QuietPeriod), map[string]string{"y": "2"})
	h.forge.failDefault = errors.New("no default")

	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Failed) != 2 || !strings.Contains(res.Failed["a"], "no default") {
		t.Fatalf("res = %+v", res)
	}
	h.forge.failDefault = nil
	res = h.syncer.RunOnce(context.Background(), true)
	if len(res.Pushed) != 2 || h.forge.prs[0].base != "main" || h.syncer.Status().Branch != "main" {
		t.Fatalf("res = %+v prs=%+v", res, h.forge.prs)
	}
	if n := h.forge.callCount("DefaultBranch"); n != 3 { // 2 failed lookups + 1 cached success
		t.Fatalf("DefaultBranch calls = %d", n)
	}
}

func TestMissingTargetBranchFails(t *testing.T) {
	h := newHarness(t, Settings{Branch: "nope"})
	h.writeCampaign("a", h.clock.Add(-QuietPeriod), map[string]string{"x": "1"})
	res := h.syncer.RunOnce(context.Background(), false)
	if !strings.Contains(res.Failed["a"], `target branch "nope" does not exist`) {
		t.Fatalf("res = %+v", res)
	}
	h.forge.failBranchHead = errors.New("ref api down")
	res = h.syncer.RunOnce(context.Background(), true)
	if !strings.Contains(res.Failed["a"], "ref api down") {
		t.Fatalf("res = %+v", res)
	}
}

func TestPublicRepoIsWarnedNotBlocked(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.forge.private = false
	h.writeCampaign("a", h.clock.Add(-QuietPeriod), map[string]string{"x": "1"})
	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 {
		t.Fatalf("public repo must not block: %+v", res)
	}
	if st := h.syncer.Status(); st.Public == nil || !*st.Public {
		t.Fatalf("status must flag the public repo: %+v", st)
	}
}

func TestVisibilityLookupFailureIsRetried(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	h.forge.privateErr = errors.New("403")
	h.syncer.RunOnce(context.Background(), false)
	if h.syncer.Status().Public != nil {
		t.Fatal("unknown visibility must stay nil")
	}
	h.forge.privateErr = nil
	h.syncer.RunOnce(context.Background(), false)
	if h.syncer.Status().Public == nil || h.forge.callCount("RepoIsPrivate") != 2 {
		t.Fatalf("visibility must be retried: %+v calls=%d", h.syncer.Status(), h.forge.callCount("RepoIsPrivate"))
	}
}

func TestCorruptAndUnreadableStateAreIgnored(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, StateFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Settings{Repo: "o/r", Root: root}, newFakeForge(), nil)
	if len(s.Status().Campaigns) != 0 {
		t.Fatal("corrupt state must be ignored")
	}
	if err := os.WriteFile(filepath.Join(root, StateFile), []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = New(Settings{Repo: "o/r", Root: root}, newFakeForge(), nil)
	if s.state == nil {
		t.Fatal("null state must leave an empty map")
	}
}

func TestSettingsNormalisation(t *testing.T) {
	s := New(Settings{Repo: " o/r ", Branch: " main ", PathPrefix: "\\a\\b\\", Root: t.TempDir()}, nil, nil)
	if s.settings.Repo != "o/r" || s.settings.Branch != "main" || s.settings.PathPrefix != "a/b" {
		t.Fatalf("settings = %+v", s.settings)
	}
	if got := s.repoPath("c", "x/y"); got != "a/b/c/x/y" {
		t.Fatalf("repoPath = %q", got)
	}
	none := New(Settings{Repo: "o/r", PathPrefix: ".", Root: t.TempDir()}, nil, nil)
	if none.settings.PathPrefix != "" || none.repoPath("c", "x") != "c/x" {
		t.Fatalf("dot prefix must mean repo root: %q", none.repoPath("c", "x"))
	}
}

func TestBranchFor(t *testing.T) {
	cases := map[string]string{
		"run-42":        BranchPrefix + "run-42",
		"my campaign!!": BranchPrefix + "my-campaign",
		"../..":         BranchPrefix + "campaign",
		"a/b":           BranchPrefix + "a/b",
	}
	for in, want := range cases {
		if got := BranchFor(in); got != want {
			t.Errorf("BranchFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	if out, changed := redact(nil); changed || out != nil {
		t.Fatal("empty input must pass through")
	}
	bin := []byte{0xff, 0xfe, 'g', 'h', 'p', '_'}
	if _, changed := redact(bin); changed {
		t.Fatal("invalid utf-8 must not be scrubbed")
	}
	if _, changed := redact([]byte("plain text")); changed {
		t.Fatal("plain text must not change")
	}
	out, changed := redact([]byte("key ghp_" + strings.Repeat("b", 36)))
	if !changed || strings.Contains(string(out), "ghp_"+strings.Repeat("b", 36)) {
		t.Fatalf("token must be redacted: %q", out)
	}
}

func TestSummarizeFailuresIsSorted(t *testing.T) {
	got := summarizeFailures(map[string]string{"b": "two", "a": "one"})
	if got != "a: one; b: two" {
		t.Fatalf("got %q", got)
	}
}

func TestCommitSizeCapOmitsLaterFiles(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	dir := filepath.Join(h.root, "big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Eleven 2 MiB files exceed the 20 MiB commit cap; the last one is omitted.
	chunk := make([]byte, MaxFileBytes)
	for i := 0; i < 11; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".bin"), chunk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := h.syncer.collect(campaign{id: "big", dir: dir, status: StatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.files) != 10 || len(b.manifest.Omitted) != 1 || b.manifest.Omitted[0].Reason != OmitReasonCommitSize || b.manifest.Omitted[0].Path != "fk.bin" {
		t.Fatalf("files=%d omitted=%+v", len(b.files), b.manifest.Omitted)
	}
}

func TestScanErrorBacksOff(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	// A root that is a regular file cannot be listed.
	h.syncer.settings.Root = filepath.Join(h.root, "file")
	if err := os.WriteFile(h.syncer.settings.Root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := h.syncer.RunOnce(context.Background(), false)
	if !strings.Contains(res.Skipped, "scanning") {
		t.Fatalf("res = %+v", res)
	}
	st := h.syncer.Status()
	if st.Failures != 1 || st.NextAttemptAt == nil || !strings.Contains(st.LastError, "scanning") {
		t.Fatalf("status = %+v", st)
	}
	if a := h.lastAudit(); a.action != AuditActionPushFailed || !strings.Contains(a.detail, "campaign=,") {
		t.Fatalf("audit = %+v", a)
	}
}

func TestFileCountCap(t *testing.T) {
	h := newHarness(t, Settings{Branch: "main"})
	dir := filepath.Join(h.root, "many")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxFilesPerCommit+2; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconvItoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := h.syncer.collect(campaign{id: "many", dir: dir, status: StatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.files) != MaxFilesPerCommit || len(b.manifest.Omitted) != 2 || b.manifest.Omitted[0].Reason != OmitReasonFileCount {
		t.Fatalf("files=%d omitted=%+v", len(b.files), b.manifest.Omitted)
	}
}

func strconvItoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

func TestUnreadableFileFailsTheCampaignOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	h := newHarness(t, Settings{Branch: "main"})
	old := h.clock.Add(-QuietPeriod)
	h.writeCampaign("bad", old, map[string]string{"locked.md": "x"})
	h.writeCampaign("good", old, map[string]string{"ok.md": "y"})
	locked := filepath.Join(h.root, "bad", "locked.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	res := h.syncer.RunOnce(context.Background(), false)
	if len(res.Pushed) != 1 || res.Pushed[0] != "good" || !strings.Contains(res.Failed["bad"], "reading") {
		t.Fatalf("res = %+v", res)
	}
}

func TestUnreadableStateIsIgnored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	root := t.TempDir()
	p := filepath.Join(root, StateFile)
	if err := os.WriteFile(p, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	s := New(Settings{Repo: "o/r", Root: root}, newFakeForge(), nil)
	if len(s.Status().Campaigns) != 0 {
		t.Fatal("unreadable state must be ignored")
	}
}

func TestEmptyDefaultBranchIsAnError(t *testing.T) {
	h := newHarness(t, Settings{})
	h.forge.defaultBranch = "  "
	h.writeCampaign("a", h.clock.Add(-QuietPeriod), map[string]string{"x": "1"})
	res := h.syncer.RunOnce(context.Background(), false)
	if !strings.Contains(res.Failed["a"], "empty") {
		t.Fatalf("res = %+v", res)
	}
}

func TestDefaultClockIsWallClock(t *testing.T) {
	s := New(Settings{Repo: "o/r", Root: t.TempDir()}, newFakeForge(), nil)
	before := time.Now().Add(-time.Second)
	if got := s.now(); got.Before(before) {
		t.Fatalf("now() = %v, before %v", got, before)
	}
}

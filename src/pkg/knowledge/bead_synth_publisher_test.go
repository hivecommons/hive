package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// fakeBeadSynthRepo is an in-memory repository: branches map to file trees,
// and at most one PR is tracked for the publish branch.
type fakeBeadSynthRepo struct {
	defaultBranch string
	branches      map[string]map[string]string
	heads         map[string]string
	openPR        int
	nextPR        int
	commits       []fakeBeadSynthCommit
	createdPRs    []string
	resets        int

	defaultErr, openErr, readErr, headErr, resetErr, commitErr, createErr error
}

type fakeBeadSynthCommit struct {
	repo, branch, parent, headline, body string
	files                                map[string]string
}

func newFakeBeadSynthRepo() *fakeBeadSynthRepo {
	return &fakeBeadSynthRepo{
		defaultBranch: "main",
		branches:      map[string]map[string]string{"main": {}},
		heads:         map[string]string{"main": "base-0"},
		nextPR:        100,
	}
}

func notFoundErr() error {
	return fmt.Errorf("getting file: %w", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
}

func (f *fakeBeadSynthRepo) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	return f.defaultBranch, f.defaultErr
}

func (f *fakeBeadSynthRepo) OpenPRForHead(ctx context.Context, repo, branch string) (int, string, bool, error) {
	if f.openErr != nil {
		return 0, "", false, f.openErr
	}
	if f.openPR == 0 || branch != BeadSynthPublishBranch {
		return 0, "", false, nil
	}
	return f.openPR, fmt.Sprintf("https://github.com/%s/pull/%d", repo, f.openPR), true, nil
}

func (f *fakeBeadSynthRepo) GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	tree, ok := f.branches[ref]
	if !ok {
		return "", notFoundErr()
	}
	content, ok := tree[path]
	if !ok {
		return "", notFoundErr()
	}
	return content, nil
}

func (f *fakeBeadSynthRepo) LatestCommitHash(ctx context.Context, owner, repo, branch string) (string, error) {
	if f.headErr != nil {
		return "", f.headErr
	}
	return f.heads[branch], nil
}

func (f *fakeBeadSynthRepo) ResetBranch(ctx context.Context, repo, branch, base string) (string, error) {
	if f.resetErr != nil {
		return "", f.resetErr
	}
	f.resets++
	tree := make(map[string]string, len(f.branches[base]))
	for k, v := range f.branches[base] {
		tree[k] = v
	}
	f.branches[branch] = tree
	f.heads[branch] = f.heads[base]
	return f.heads[base], nil
}

func (f *fakeBeadSynthRepo) CommitFilesOnBranch(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files map[string]string) (string, error) {
	if f.commitErr != nil {
		return "", f.commitErr
	}
	if f.heads[branch] != expectedHeadSHA {
		return "", fmt.Errorf("expected head %s, branch is at %s", expectedHeadSHA, f.heads[branch])
	}
	for k, v := range files {
		f.branches[branch][k] = v
	}
	f.commits = append(f.commits, fakeBeadSynthCommit{repo: repo, branch: branch, parent: expectedHeadSHA, headline: headline, body: body, files: files})
	f.heads[branch] = fmt.Sprintf("commit-%d", len(f.commits))
	return f.heads[branch], nil
}

func (f *fakeBeadSynthRepo) CreatePR(ctx context.Context, repo, head, base, title, body string) (hgithub.CreatePRResult, error) {
	if f.createErr != nil {
		return hgithub.CreatePRResult{}, f.createErr
	}
	if f.openPR != 0 {
		return hgithub.CreatePRResult{Number: f.openPR, AlreadyExisted: true}, nil
	}
	f.openPR = f.nextPR
	f.nextPR++
	f.createdPRs = append(f.createdPRs, repo+" "+head+"->"+base+" "+title+"\n"+body)
	return hgithub.CreatePRResult{Number: f.openPR}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBeadSynthPublisher(t *testing.T, repo *fakeBeadSynthRepo) *BeadSynthPublisher {
	t.Helper()
	p, err := NewBeadSynthPublisher(repo, BeadSynthPublisherConfig{
		Repo:  "acme/widgets",
		Path:  "docs/knowledge/",
		Layer: "project",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func publishableFact(title, body string) ExtractedFact {
	return ExtractedFact{
		Title:      title,
		Body:       body,
		Type:       FactPattern,
		Confidence: 0.8,
		Tags:       []string{"go", "testing"},
		Related:    []string{"other-fact"},
		SourcePR:   "bead:worker/b-1",
		SourceDate: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestBeadSynthPublisher_CreatesPRWithStableSlugFile(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p := newTestBeadSynthPublisher(t, repo)

	if err := p.PublishFacts(context.Background(), []ExtractedFact{publishableFact("Use Table Tests", "Prefer table-driven tests.")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 1 || len(repo.createdPRs) != 1 || repo.resets != 1 {
		t.Fatalf("want one reset, commit and PR; got resets=%d commits=%d prs=%d", repo.resets, len(repo.commits), len(repo.createdPRs))
	}
	c := repo.commits[0]
	if c.repo != "acme/widgets" || c.branch != BeadSynthPublishBranch || c.parent != "base-0" {
		t.Fatalf("commit target: %+v", c)
	}
	content, ok := c.files["docs/knowledge/use-table-tests.md"]
	if !ok {
		t.Fatalf("expected stable slug path, got %v", c.files)
	}
	for _, want := range []string{
		"---\ntitle: Use Table Tests\n",
		"type: pattern\n",
		"layer: project\n",
		"confidence: 0.80\n",
		"tags: [go, testing]\n",
		"related: [other-fact]\n",
		"date: 2026-10-01\n",
		"# Use Table Tests\n\nPrefer table-driven tests.\n\n",
		"_Maintained by Hive — edits here are overwritten; propose changes via the Hive knowledge vault._\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("rendered file missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "worker") || strings.Contains(content, "bead:") {
		t.Errorf("rendered file must not carry agent provenance:\n%s", content)
	}
	if !strings.Contains(repo.createdPRs[0], "acme/widgets "+BeadSynthPublishBranch+"->main") ||
		!strings.Contains(repo.createdPRs[0], "docs/knowledge/use-table-tests.md") {
		t.Errorf("PR request: %s", repo.createdPRs[0])
	}
}

func TestBeadSynthPublisher_SecondRunUpdatesSameFileOnOpenPR(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p := newTestBeadSynthPublisher(t, repo)
	ctx := context.Background()

	if err := p.PublishFacts(ctx, []ExtractedFact{publishableFact("Use Table Tests", "v1")}); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishFacts(ctx, []ExtractedFact{publishableFact("Use Table Tests", "v2")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.createdPRs) != 1 {
		t.Fatalf("the open PR must be reused, created %d", len(repo.createdPRs))
	}
	if repo.resets != 1 {
		t.Fatalf("an open PR's branch must not be reset, resets=%d", repo.resets)
	}
	if len(repo.commits) != 2 || repo.commits[1].parent != "commit-1" {
		t.Fatalf("second commit must extend the PR branch: %+v", repo.commits)
	}
	files := repo.branches[BeadSynthPublishBranch]
	if len(files) != 1 || !strings.Contains(files["docs/knowledge/use-table-tests.md"], "v2") {
		t.Fatalf("update must rewrite the same file in place: %v", files)
	}
}

func TestBeadSynthPublisher_NoChangeIsNoOp(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p := newTestBeadSynthPublisher(t, repo)
	ctx := context.Background()
	fact := publishableFact("Use Table Tests", "same")

	if err := p.PublishFacts(ctx, []ExtractedFact{fact}); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishFacts(ctx, []ExtractedFact{fact}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 1 || len(repo.createdPRs) != 1 {
		t.Fatalf("an unchanged fact must not commit again: commits=%d prs=%d", len(repo.commits), len(repo.createdPRs))
	}

	// Already merged into the base with no open PR: nothing to do either.
	merged := newFakeBeadSynthRepo()
	mp := newTestBeadSynthPublisher(t, merged)
	merged.branches["main"]["docs/knowledge/use-table-tests.md"] = mp.render(fact)
	if err := mp.PublishFacts(ctx, []ExtractedFact{fact}); err != nil {
		t.Fatal(err)
	}
	if len(merged.commits) != 0 || len(merged.createdPRs) != 0 || merged.resets != 0 {
		t.Fatalf("content already on base must be a no-op: %+v", merged)
	}

	if err := mp.PublishFacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBeadSynthPublisher_ReusesExistingOpenPR(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	repo.openPR = 42
	repo.branches[BeadSynthPublishBranch] = map[string]string{"docs/knowledge/old.md": "old"}
	repo.heads[BeadSynthPublishBranch] = "pr-head"
	p := newTestBeadSynthPublisher(t, repo)

	if err := p.PublishFacts(context.Background(), []ExtractedFact{publishableFact("New Fact", "body")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.createdPRs) != 0 || repo.resets != 0 {
		t.Fatalf("existing PR must be reused without reset: prs=%v resets=%d", repo.createdPRs, repo.resets)
	}
	if len(repo.commits) != 1 || repo.commits[0].parent != "pr-head" {
		t.Fatalf("commit must land on the PR head: %+v", repo.commits)
	}
	if repo.branches[BeadSynthPublishBranch]["docs/knowledge/old.md"] != "old" {
		t.Fatal("earlier PR files must be kept")
	}
}

func TestBeadSynthPublisher_IdempotentPRAcrossRuns(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p := newTestBeadSynthPublisher(t, repo)
	ctx := context.Background()
	for _, title := range []string{"A", "B", "A", "C"} {
		if err := p.PublishFacts(ctx, []ExtractedFact{publishableFact(title, "body "+title)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.createdPRs) != 1 {
		t.Fatalf("one PR must collect every run, opened %d", len(repo.createdPRs))
	}
	if got := len(repo.branches[BeadSynthPublishBranch]); got != 3 {
		t.Fatalf("want 3 distinct files, got %d", got)
	}
	if len(repo.commits) != 3 {
		t.Fatalf("the repeated unchanged fact must not commit: %d commits", len(repo.commits))
	}

	// Once that PR is merged or closed, the next change starts a fresh
	// branch from the base and opens a new PR.
	repo.openPR = 0
	if err := p.PublishFacts(ctx, []ExtractedFact{publishableFact("D", "d")}); err != nil {
		t.Fatal(err)
	}
	if repo.resets != 2 || len(repo.createdPRs) != 2 {
		t.Fatalf("closed PR: want a reset branch and a new PR, resets=%d prs=%d", repo.resets, len(repo.createdPRs))
	}
	if files := repo.branches[BeadSynthPublishBranch]; len(files) != 1 {
		t.Fatalf("reset branch must start from base: %v", files)
	}
}

func TestBeadSynthPublisher_SkipsSensitiveFacts(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p := newTestBeadSynthPublisher(t, repo)

	flagged := publishableFact("Flagged", "fine")
	flagged.Sensitive = true
	security := publishableFact("Security Fix", "fine")
	security.Tags = []string{"Security"}
	token := publishableFact("Token Leak", "use ghp_abcdefghijklmnopqrstuvwxyz0123456789 here")
	redacted := publishableFact("Redacted", "the key was [REDACTED]")

	if err := p.PublishFacts(context.Background(), []ExtractedFact{flagged, security, token, redacted, {Title: "!!!"}}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 0 || len(repo.createdPRs) != 0 {
		t.Fatalf("sensitive facts must never be published: %+v", repo.commits)
	}

	if err := p.PublishFacts(context.Background(), []ExtractedFact{flagged, publishableFact("Safe", "ok")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 1 || len(repo.commits[0].files) != 1 {
		t.Fatalf("only the safe fact may publish: %+v", repo.commits)
	}
	if _, ok := repo.commits[0].files["docs/knowledge/safe.md"]; !ok {
		t.Fatalf("safe fact missing: %v", repo.commits[0].files)
	}
}

func TestBeadSynthPublisher_PersonalLayerNeverPublishes(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	p, err := NewBeadSynthPublisher(repo, BeadSynthPublisherConfig{Repo: "acme/widgets", Path: "docs/knowledge", Layer: "Personal"}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PublishFacts(context.Background(), []ExtractedFact{publishableFact("A", "a")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 0 {
		t.Fatal("personal facts must not publish")
	}
}

func TestBeadSynthPublisher_ExplicitBaseAndBareRepo(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	repo.defaultErr = errors.New("must not resolve the default branch")
	repo.branches["release"] = map[string]string{}
	repo.heads["release"] = "rel-0"
	p, err := NewBeadSynthPublisher(repo, BeadSynthPublisherConfig{Repo: "widgets", Org: "acme", Path: ".hive/knowledge", BaseBranch: " release ", Layer: "project"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PublishFacts(context.Background(), []ExtractedFact{publishableFact("A B", "x")}); err != nil {
		t.Fatal(err)
	}
	if len(repo.commits) != 1 || repo.commits[0].parent != "rel-0" || repo.commits[0].repo != "acme/widgets" {
		t.Fatalf("commit: %+v", repo.commits)
	}
	if _, ok := repo.commits[0].files[".hive/knowledge/a-b.md"]; !ok {
		t.Fatalf("files: %v", repo.commits[0].files)
	}
	if !strings.Contains(repo.createdPRs[0], "->release ") {
		t.Fatalf("PR base: %s", repo.createdPRs[0])
	}
}

func TestBeadSynthPublisher_Errors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	facts := []ExtractedFact{publishableFact("A", "a")}
	cases := []struct {
		name string
		set  func(*fakeBeadSynthRepo)
	}{
		{"default branch", func(f *fakeBeadSynthRepo) { f.defaultErr = boom }},
		{"open PR lookup", func(f *fakeBeadSynthRepo) { f.openErr = boom }},
		{"read", func(f *fakeBeadSynthRepo) { f.readErr = boom }},
		{"reset", func(f *fakeBeadSynthRepo) { f.resetErr = boom }},
		{"commit", func(f *fakeBeadSynthRepo) { f.commitErr = boom }},
		{"create PR", func(f *fakeBeadSynthRepo) { f.createErr = boom }},
		{"head", func(f *fakeBeadSynthRepo) {
			f.openPR = 7
			f.branches[BeadSynthPublishBranch] = map[string]string{}
			f.headErr = boom
		}},
		{"base is publish branch", func(f *fakeBeadSynthRepo) { f.defaultBranch = BeadSynthPublishBranch }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeBeadSynthRepo()
			tc.set(repo)
			p := newTestBeadSynthPublisher(t, repo)
			if err := p.PublishFacts(ctx, facts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewBeadSynthPublisher_Validation(t *testing.T) {
	repo := newFakeBeadSynthRepo()
	bad := []struct {
		name   string
		client BeadSynthRepoClient
		cfg    BeadSynthPublisherConfig
	}{
		{"nil client", nil, BeadSynthPublisherConfig{Repo: "a/b"}},
		{"bare repo without org", repo, BeadSynthPublisherConfig{Repo: "b"}},
		{"empty repo", repo, BeadSynthPublisherConfig{Repo: "", Org: "a"}},
		{"nested repo", repo, BeadSynthPublisherConfig{Repo: "a/b/c"}},
		{"absolute path", repo, BeadSynthPublisherConfig{Repo: "a/b", Path: "/etc"}},
		{"parent path", repo, BeadSynthPublisherConfig{Repo: "a/b", Path: "docs/../.."}},
		{"windows path", repo, BeadSynthPublisherConfig{Repo: "a/b", Path: `docs\knowledge`}},
		{"base is publish branch", repo, BeadSynthPublisherConfig{Repo: "a/b", BaseBranch: BeadSynthPublishBranch}},
	}
	for _, tc := range bad {
		if _, err := NewBeadSynthPublisher(tc.client, tc.cfg, nil); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}

	p, err := NewBeadSynthPublisher(repo, BeadSynthPublisherConfig{Repo: "a/b", Path: "./docs//knowledge/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.FilePath(ExtractedFact{Title: "Hello World"}); got != "docs/knowledge/hello-world.md" {
		t.Errorf("FilePath = %q", got)
	}
	if got := p.FilePath(ExtractedFact{Title: "///"}); got != "" {
		t.Errorf("FilePath of an unsluggable title = %q", got)
	}
	root, err := NewBeadSynthPublisher(repo, BeadSynthPublisherConfig{Repo: "a/b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := root.FilePath(ExtractedFact{Title: "Top"}); got != "top.md" {
		t.Errorf("root FilePath = %q", got)
	}
}

func TestBeadSynthPublisher_RenderKeepsBodyHeadingAndOmitsEmptyFields(t *testing.T) {
	p := newTestBeadSynthPublisher(t, newFakeBeadSynthRepo())
	p.cfg.Layer = ""
	got := p.render(ExtractedFact{Title: "T", Body: "# Own Heading\n\ntext", Type: FactGotcha, Confidence: 0.5})
	if strings.Count(got, "# ") != 1 || !strings.Contains(got, "# Own Heading") {
		t.Errorf("body heading must be kept without a duplicate title:\n%s", got)
	}
	for _, absent := range []string{"layer:", "tags:", "related:", "date:"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty field %q rendered:\n%s", absent, got)
		}
	}
	empty := p.render(ExtractedFact{Title: "Only Title"})
	if !strings.Contains(empty, "# Only Title\n\n---\n\n_Maintained by Hive") {
		t.Errorf("empty body render:\n%s", empty)
	}
}

func TestIsSensitiveFact(t *testing.T) {
	if IsSensitiveFact(publishableFact("Plain", "nothing secret")) {
		t.Error("plain fact flagged sensitive")
	}
	f := publishableFact("Plain", "nothing secret")
	f.Related = []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}
	if !IsSensitiveFact(f) {
		t.Error("credential in related must be sensitive")
	}
}

func TestBeadSynthPublisher_IsBeadFactPublisher(t *testing.T) {
	var _ BeadFactPublisher = (*BeadSynthPublisher)(nil)
	paths := sortedBeadSynthPaths(map[string]string{"b": "", "a": ""})
	if !sort.StringsAreSorted(paths) || len(paths) != 2 {
		t.Fatalf("paths: %v", paths)
	}
	if isGitHubNotFound(errors.New("x")) || !isGitHubNotFound(notFoundErr()) {
		t.Fatal("isGitHubNotFound")
	}
}

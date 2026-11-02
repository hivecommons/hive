package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runStageRunner is a git stub for the run-stage workspace path. It records
// every git invocation (config args stripped) so a test can assert which
// commands ran and in which directory, and it creates the ".git" marker a
// real clone would leave behind so the second kick of a run sees the source
// repo as already present.
type runStageRunner struct {
	mu sync.Mutex

	symbolicRef string
	fetchErr    error

	calls []runStageCall
}

type runStageCall struct {
	dir  string
	args string
}

func (r *runStageRunner) Run(_ context.Context, dir string, _ []string, name string, args ...string) ([]byte, error) {
	if name != "git" {
		return nil, errors.New("unexpected command " + name)
	}
	effective := stripGitConfigArgs(args)
	joined := strings.Join(effective, " ")
	r.mu.Lock()
	r.calls = append(r.calls, runStageCall{dir: dir, args: joined})
	r.mu.Unlock()
	switch {
	case len(effective) >= 4 && effective[0] == "clone":
		target := effective[len(effective)-1]
		return nil, os.MkdirAll(filepath.Join(target, ".git"), 0o770)
	case joined == "symbolic-ref --short refs/remotes/origin/HEAD":
		return []byte("origin/" + r.symbolicRef + "\n"), nil
	case strings.HasPrefix(joined, "fetch origin "):
		if r.fetchErr != nil {
			return []byte("fatal: couldn't find remote ref"), r.fetchErr
		}
		return []byte("ok\n"), nil
	case strings.HasPrefix(joined, "worktree add --detach "):
		path := effective[3]
		if err := os.MkdirAll(path, 0o770); err != nil {
			return nil, err
		}
		// A linked worktree carries a .git FILE, not a directory; the
		// cap counter only cares that the entry exists.
		return nil, os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: elsewhere\n"), 0o600)
	case joined == "rev-parse HEAD":
		return []byte("stagesha\n"), nil
	default:
		return []byte(""), nil
	}
}

func (r *runStageRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.args)
	}
	return out
}

func (r *runStageRunner) count(prefix string) int {
	n := 0
	for _, c := range r.commands() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func newRunStageExecutor(runner *runStageRunner) *SandboxExecutor {
	return &SandboxExecutor{
		Runner:   runner,
		Launcher: sandboxFakeLauncher{},
		Minter:   sandboxFakeMinter{},
		Logger:   discardLogger(),
	}
}

func runStageSpec(workspaceDir string) SandboxKickSpec {
	return SandboxKickSpec{
		Agent:        "builder",
		Org:          "kubestellar",
		Repo:         "hive",
		WorkspaceDir: workspaceDir,
		RunKey:       "run-42",
		Stage:        "build",
		Generation:   1,
	}
}

func TestRunStageSourceRepoIsSanitisedUnderRepos(t *testing.T) {
	spec := runStageSpec("/ws")
	spec.Org, spec.Repo = "Kube Stellar", "Hive.Repo"
	got := runStageSourceRepo(spec)
	want := filepath.Join("/ws", "_repos", "kube-stellar", "hive-repo")
	if got != want {
		t.Fatalf("runStageSourceRepo = %q, want %q", got, want)
	}
}

func TestPrepareWorkspaceRunStageRequiresRunKeyAndGeneration(t *testing.T) {
	cases := map[string]func(*SandboxKickSpec){
		"missing run key":    func(s *SandboxKickSpec) { s.RunKey = " " },
		"missing generation": func(s *SandboxKickSpec) { s.Generation = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &runStageRunner{symbolicRef: "main"}
			spec := runStageSpec(t.TempDir())
			mutate(&spec)
			_, _, err := newRunStageExecutor(runner).prepareWorkspace(context.Background(), &spec)
			if err == nil || !strings.Contains(err.Error(), "requires run key and generation") {
				t.Fatalf("err = %v, want run key/generation validation error", err)
			}
			if got := runner.commands(); len(got) != 0 {
				t.Fatalf("validation must fail before any git runs; ran %v", got)
			}
		})
	}
}

// TestPrepareWorkspaceRunStageClonesOnceThenAddsWorktrees pins the shape of
// the run-stage path: the shared source clone lives under _repos/<org>/<repo>
// and is cloned exactly once per workspace root, each generation gets its own
// detached worktree under runs/<run>/<stage>-<gen>, and an empty BaseRef is
// resolved from the clone's origin/HEAD and written back into the spec.
func TestPrepareWorkspaceRunStageClonesOnceThenAddsWorktrees(t *testing.T) {
	root := t.TempDir()
	runner := &runStageRunner{symbolicRef: "develop"}
	exec := newRunStageExecutor(runner)

	spec := runStageSpec(root)
	path, sha, err := exec.prepareWorkspace(context.Background(), &spec)
	if err != nil {
		t.Fatalf("prepareWorkspace (gen 1): %v", err)
	}
	source := filepath.Join(root, "_repos", "kubestellar", "hive")
	if wantPath := filepath.Join(root, "runs", "run-42", "build-1"); path != wantPath {
		t.Fatalf("workspace = %q, want %q", path, wantPath)
	}
	if sha != "stagesha" {
		t.Fatalf("sha = %q, want stagesha", sha)
	}
	if spec.BaseRef != "develop" {
		t.Fatalf("BaseRef = %q, want develop resolved from origin/HEAD", spec.BaseRef)
	}
	if _, err := os.Stat(filepath.Join(source, ".git")); err != nil {
		t.Fatalf("source clone not created at %s: %v", source, err)
	}
	if n := runner.count("clone --no-checkout https://github.com/kubestellar/hive.git " + source); n != 1 {
		t.Fatalf("clone into source repo ran %d times, want 1; commands: %v", n, runner.commands())
	}
	if n := runner.count("fetch origin develop"); n != 1 {
		t.Fatalf("fetch origin develop ran %d times, want 1; commands: %v", n, runner.commands())
	}
	if n := runner.count("worktree add --detach " + path + " FETCH_HEAD"); n != 1 {
		t.Fatalf("worktree add for gen 1 ran %d times, want 1; commands: %v", n, runner.commands())
	}
	for _, c := range runner.calls {
		if strings.HasPrefix(c.args, "fetch origin ") && c.dir != source {
			t.Fatalf("fetch ran in %q, want the source repo %q", c.dir, source)
		}
	}

	// Second generation of the same run: the source clone is reused, the
	// explicit base ref is honoured without consulting origin/HEAD, and a new
	// worktree appears next to the first.
	spec2 := runStageSpec(root)
	spec2.Generation = 2
	spec2.BaseRef = "release-1"
	path2, _, err := exec.prepareWorkspace(context.Background(), &spec2)
	if err != nil {
		t.Fatalf("prepareWorkspace (gen 2): %v", err)
	}
	if wantPath := filepath.Join(root, "runs", "run-42", "build-2"); path2 != wantPath {
		t.Fatalf("gen 2 workspace = %q, want %q", path2, wantPath)
	}
	if n := runner.count("clone "); n != 1 {
		t.Fatalf("clone ran %d times across two generations, want exactly 1; commands: %v", n, runner.commands())
	}
	if n := runner.count("symbolic-ref "); n != 1 {
		t.Fatalf("origin/HEAD was consulted %d times, want 1 (only for the empty BaseRef); commands: %v", n, runner.commands())
	}
	if n := runner.count("fetch origin release-1"); n != 1 {
		t.Fatalf("fetch origin release-1 ran %d times, want 1; commands: %v", n, runner.commands())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("gen 1 worktree must survive gen 2: %v", err)
	}
}

func TestPrepareWorkspaceRunStageFetchFailureNamesTheRef(t *testing.T) {
	runner := &runStageRunner{symbolicRef: "main", fetchErr: errors.New("exit status 128")}
	spec := runStageSpec(t.TempDir())
	spec.BaseRef = "nope"
	_, _, err := newRunStageExecutor(runner).prepareWorkspace(context.Background(), &spec)
	if err == nil {
		t.Fatal("expected fetch failure to fail workspace prep")
	}
	for _, want := range []string{"git fetch nope", "exit status 128", "couldn't find remote ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	}
	if n := runner.count("worktree add"); n != 0 {
		t.Fatalf("worktree add must not run after a failed fetch; commands: %v", runner.commands())
	}
}

func TestPrepareWorkspaceRunStageHonoursWorktreeCap(t *testing.T) {
	root := t.TempDir()
	runner := &runStageRunner{symbolicRef: "main"}
	exec := newRunStageExecutor(runner)

	spec := runStageSpec(root)
	spec.MaxWorktrees = 1
	if _, _, err := exec.prepareWorkspace(context.Background(), &spec); err != nil {
		t.Fatalf("first generation: %v", err)
	}
	spec2 := runStageSpec(root)
	spec2.Generation, spec2.MaxWorktrees = 2, 1
	_, _, err := exec.prepareWorkspace(context.Background(), &spec2)
	if err == nil || !strings.Contains(err.Error(), "max_worktrees cap 1 reached") {
		t.Fatalf("err = %v, want the worktree cap refusal", err)
	}
}

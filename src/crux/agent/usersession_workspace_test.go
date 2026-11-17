package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

const (
	testWorkspaceRepo   = "acme/widgets"
	testWorkspaceToken  = "ghs_workspace_clone_token"
	testSecondSessionID = "fedcba987654"
	testSecondSlot      = 4
)

// fakeGit stands in for the hive-side git binary: it records every call and,
// for a clone, creates the destination with the .git/config git itself would
// write for the given remote (no credential, no helper). It also reads the
// credential file named in the one-shot helper so the test can prove the
// token was both USED and never left behind.
type fakeGit struct {
	calls    [][]string
	dirs     []string
	envs     [][]string
	credSeen string
	fail     bool
}

func (g *fakeGit) run(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
	g.calls = append(g.calls, args)
	g.dirs = append(g.dirs, dir)
	g.envs = append(g.envs, env)
	if g.fail {
		return []byte("fatal: could not read from remote"), errors.New("exit 128")
	}
	for _, a := range args {
		if strings.HasPrefix(a, "credential.helper=store --file=") {
			b, err := os.ReadFile(strings.TrimPrefix(a, "credential.helper=store --file="))
			if err != nil {
				return nil, err
			}
			g.credSeen = string(b)
		}
	}
	dest := args[len(args)-1]
	url := args[len(args)-2]
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o755); err != nil {
		return nil, err
	}
	cfg := "[core]\n\tsharedRepository = group\n[remote \"origin\"]\n\turl = " + url + "\n"
	return nil, os.WriteFile(filepath.Join(dest, ".git", "config"), []byte(cfg), 0o644)
}

type fakeWorkspaceMinter struct {
	token string
	err   error
	repos []string
}

func (m *fakeWorkspaceMinter) MintPushToken(_ context.Context, repo string) (string, error) {
	m.repos = append(m.repos, repo)
	return m.token, m.err
}

func workspaceSpec(id string, slot int, vendor string) crux.UserSessionSpec {
	return crux.UserSessionSpec{ID: id, UIDSlot: slot, Vendor: vendor, WorkspaceRepo: testWorkspaceRepo}
}

// workspaceRuntime wires the fakes. The priv fake simulates the root hand-over
// script by moving the staging clone into place, as root's mv would, and the
// worktree scripts by creating (a .git file, as `git worktree add` writes) or
// deleting the session's worktree.
func workspaceRuntime(t *testing.T) (*UserSessionRuntime, *fakeTmux, *fakePriv, *fakeGit) {
	t.Helper()
	f := sessionLifecycleTmux("")
	r, p := newTestSessionRuntimeWithPriv(t, f, sessionSecretEnv)
	g := &fakeGit{}
	r.git = g.run
	r.cloneMinter = &fakeWorkspaceMinter{token: testWorkspaceToken}
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) < 7 || c.argv[0] != "sh" {
			return false, nil, nil
		}
		switch c.argv[2] {
		case userSessionWorkspaceOwnScript:
			return true, nil, os.Rename(c.argv[4], c.argv[5])
		case userSessionWorktreeAddScript:
			return true, nil, os.WriteFile(filepath.Join(c.argv[5], ".git"), []byte("gitdir: "+c.argv[4]+"/.git/worktrees/x\n"), 0o644)
		case userSessionWorktreeRemoveScript:
			return true, nil, os.RemoveAll(c.argv[5])
		}
		return false, nil, nil
	}
	return r, f, p, g
}

func privCallsWithScript(p *fakePriv, script string) []fakePrivCall {
	var out []fakePrivCall
	for _, c := range p.calls {
		if len(c.argv) >= 3 && c.argv[0] == "sh" && c.argv[2] == script {
			out = append(out, c)
		}
	}
	return out
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// The invariant: the hive's token is used for the clone and NOTHING of it
// survives: not in the clone's config, not in the remote URL, not on disk.
func TestUserSessionWorkspaceCloneLeavesNoTokenBehind(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_hive_process_token")
	r, _, _, g := workspaceRuntime(t)
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1 {
		t.Fatalf("git calls = %v, want exactly one clone", g.calls)
	}
	args := g.calls[0]
	clone := indexOf(args, "clone")
	helper := -1
	for i, a := range args {
		if strings.HasPrefix(a, "credential.helper=") {
			helper = i
		}
	}
	if clone < 0 || helper < 0 || helper > clone || args[helper-1] != "-c" {
		t.Fatalf("credential helper must be a one-shot `git -c` BEFORE the clone subcommand (clone -c would persist it): %v", args)
	}
	shared := indexOf(args, "core.sharedRepository=group")
	if shared < clone || args[shared-1] != "-c" {
		t.Errorf("core.sharedRepository=group must be the clone's own (persisted) config: %v", args)
	}
	if url := args[len(args)-2]; url != "https://github.com/"+testWorkspaceRepo+".git" {
		t.Errorf("clone URL = %q, want the plain https URL", url)
	}
	for _, a := range args {
		if strings.Contains(a, testWorkspaceToken) {
			t.Errorf("token appears on the git command line: %v", args)
		}
	}
	if !strings.Contains(g.credSeen, "x-access-token:"+testWorkspaceToken+"@github.com") {
		t.Errorf("the one-shot credential file did not carry the minted token: %q", g.credSeen)
	}
	ws := r.WorkspaceDir(testWorkspaceRepo)
	cfg, err := os.ReadFile(filepath.Join(ws, ".git", "config"))
	if err != nil {
		t.Fatalf("workspace clone not in place: %v", err)
	}
	for _, bad := range []string{testWorkspaceToken, "x-access-token", "credential"} {
		if strings.Contains(string(cfg), bad) {
			t.Errorf(".git/config contains %q:\n%s", bad, cfg)
		}
	}
	if !strings.Contains(string(cfg), "url = https://github.com/"+testWorkspaceRepo+".git") {
		t.Errorf("remote is not the plain https URL:\n%s", cfg)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(ws), userSessionWorkspaceCredPrefix+"*"))
	if len(left) != 0 {
		t.Errorf("credential file left behind: %v", left)
	}
	if _, err := os.Lstat(ws + userSessionWorkspaceCloningSuffix); err == nil {
		t.Error("staging dir left behind")
	}
	// The hive's own process credentials never reach git either.
	if joined := strings.Join(g.envs[0], "\n"); strings.Contains(joined, "ghp_hive_process_token") {
		t.Error("hive GH_TOKEN leaked into the clone environment")
	}
}

// The invariant: the checkout is handed to the sessions group by root, group
// read-write and setgid, never to "other", and the session uid itself runs
// nothing privileged.
func TestUserSessionWorkspaceOwnedBySessionsGroup(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	own := privCallsWithScript(p, userSessionWorkspaceOwnScript)
	if len(own) != 1 {
		t.Fatalf("hand-over script ran %d times, want once", len(own))
	}
	ws := r.WorkspaceDir(testWorkspaceRepo)
	c := own[0]
	if c.user != userSessionRootSpec {
		t.Errorf("hand-over ran as %q, want root", c.user)
	}
	if c.argv[4] != ws+userSessionWorkspaceCloningSuffix || c.argv[5] != ws || c.argv[6] != strconv.Itoa(defaultUserSessionGID) {
		t.Errorf("hand-over args = %v", c.argv)
	}
	for _, want := range []string{`chown -R root:"$g"`, "chmod 2770", "ug+rw,o-rwx", "mv --"} {
		if !strings.Contains(userSessionWorkspaceOwnScript, want) {
			t.Errorf("hand-over script lacks %q", want)
		}
	}
	for _, bad := range []string{"777", "o+", "a+"} {
		if strings.Contains(userSessionWorkspaceOwnScript, bad) {
			t.Errorf("hand-over script opens the workspace to other uids (%q)", bad)
		}
	}
	// Every session runs with the sessions group as its one group, so the
	// group-writable checkout is shared; the spec never carries the hive's gid.
	if got := userSessionSpec(testSessionUID); got != strconv.Itoa(testSessionUID)+":"+strconv.Itoa(defaultUserSessionGID) {
		t.Errorf("session identity = %q", got)
	}
	if !strings.HasPrefix(ws, filepath.Join(userSessionSandboxRoot(), userSessionWorkspaceDirName)) {
		t.Errorf("workspace %q is not under the sandbox root's workspace dir", ws)
	}
	for _, c := range p.calls {
		if c.user == userSessionRootSpec && c.argv[0] == "sh" && c.argv[2] != userSessionPrepareScript && c.argv[2] != userSessionWorkspaceOwnScript && c.argv[2] != userSessionSocketDirScript && c.argv[2] != userSessionWorktreeOwnScript {
			t.Errorf("unexpected root script: %v", c.argv)
		}
		// git in the shared clone never runs as root: a hook or config any
		// session planted there would run with root's rights.
		if c.user == userSessionRootSpec && len(c.argv) > 2 && (c.argv[2] == userSessionWorktreeAddScript || c.argv[2] == userSessionWorktreeRemoveScript) {
			t.Errorf("worktree git ran as root: %v", c.argv)
		}
	}
	wt := privCallsWithScript(p, userSessionWorktreeOwnScript)
	if len(wt) != 1 || wt[0].user != userSessionRootSpec || wt[0].argv[4] != r.SessionWorkspaceDir(testSessionID, testWorkspaceRepo) ||
		wt[0].argv[5] != strconv.Itoa(testSessionUID) || wt[0].argv[6] != strconv.Itoa(defaultUserSessionGID) {
		t.Errorf("worktree hand-over = %+v, want root handing the session's own dir to its uid and the sessions group", wt)
	}
	for _, bad := range []string{"777", "o+", "a+", "-R"} {
		if strings.Contains(userSessionWorktreeOwnScript, bad) {
			t.Errorf("worktree hand-over script widens access (%q)", bad)
		}
	}
}

// The invariant: the pane opens IN the session's own worktree and git in the
// session is told it is safe; nothing else about the sandbox moves.
func TestUserSessionWorkspaceIsTheSessionCwd(t *testing.T) {
	r, f, _, _ := workspaceRuntime(t)
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	ws := r.SessionWorkspaceDir(testSessionID, testWorkspaceRepo)
	if want := filepath.Join(userSessionWorkspaceRoot(), userSessionWorktreeDirName, testSessionID, "acme", "widgets"); ws != want {
		t.Fatalf("session worktree = %q, want %q", ws, want)
	}
	cwds := 0
	var env string
	for _, c := range f.calls {
		if indexOf(c.args, "new-session") >= 0 || c.args[0] == "respawn-pane" {
			i := indexOf(c.args, "-c")
			if i < 0 || c.args[i+1] != ws {
				t.Errorf("%s cwd = %v, want %s", c.args[0], c.args, ws)
			}
			cwds++
		}
		if indexOf(c.args, "set-environment") >= 0 {
			env = strings.Join(c.args, " ")
		}
	}
	if cwds != 2 {
		t.Errorf("expected new-session and respawn-pane to carry the cwd, got %d", cwds)
	}
	for _, want := range []string{
		userSessionWorkspaceEnv + " " + ws,
		"GIT_CONFIG_KEY_0 safe.directory", "GIT_CONFIG_VALUE_0 " + ws,
		"HOME " + userSessionHome(testSessionID), "TMPDIR " + userSessionTmpDir(testSessionID),
	} {
		if !strings.Contains(env, want) {
			t.Errorf("session env lacks %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(userSessionWorkDir(testSessionID)); err != nil {
		t.Errorf("per-session work dir (uploads) no longer created: %v", err)
	}
	if r.WorkDir(testSessionID) != userSessionWorkDir(testSessionID) {
		t.Error("uploads must stay under the per-session dir, not the shared workspace")
	}
	if pairs := userSessionWorkspaceEnvPairs(""); pairs != nil {
		t.Errorf("no workspace must add no env: %v", pairs)
	}
}

// The invariant (#18588): one clone per repository, but every session opens in
// its own worktree of it, added by `git worktree add` against that one clone
// as the session's own uid; a resume reuses the worktree.
func TestUserSessionWorkspaceIsClonedOnceWithAWorktreePerSession(t *testing.T) {
	r, f, p, g := workspaceRuntime(t)
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(workspaceSpec(testSecondSessionID, testSecondSlot, "codex")); err != nil {
		t.Fatal(err)
	}
	// Resume of the first session: still no second clone, no fetch, and no
	// second worktree.
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1 || indexOf(g.calls[0], "clone") < 0 {
		t.Fatalf("git ran %v, want one clone and never a fetch", g.calls)
	}
	clone := r.WorkspaceDir(testWorkspaceRepo)
	ws1, ws2 := r.SessionWorkspaceDir(testSessionID, testWorkspaceRepo), r.SessionWorkspaceDir(testSecondSessionID, testWorkspaceRepo)
	if ws1 == ws2 || ws1 == clone || ws2 == clone {
		t.Fatalf("worktrees %q and %q must be distinct and not the clone %q", ws1, ws2, clone)
	}
	adds := privCallsWithScript(p, userSessionWorktreeAddScript)
	secondUser := strconv.Itoa(defaultUserSessionUIDBase+testSecondSlot-1) + ":" + strconv.Itoa(defaultUserSessionGID)
	want := [][3]string{{testSessionSpecUser, ws1, userSessionWorktreeBranch(testSessionID)}, {secondUser, ws2, userSessionWorktreeBranch(testSecondSessionID)}}
	if len(adds) != len(want) {
		t.Fatalf("worktree add ran %d times, want once per session: %+v", len(adds), adds)
	}
	for i, c := range adds {
		if c.user != want[i][0] || c.argv[4] != clone || c.argv[5] != want[i][1] || c.argv[6] != want[i][2] {
			t.Errorf("worktree add %d = %s %v, want %v against the one clone %s", i, c.user, c.argv[4:], want[i], clone)
		}
	}
	for _, s := range []string{"worktree add", "core.hooksPath=/dev/null", `safe.directory="$clone"`} {
		if !strings.Contains(userSessionWorktreeAddScript, s) {
			t.Errorf("worktree add script lacks %q", s)
		}
	}
	wantCwd := []string{ws1, ws2, ws1}
	var cwds []string
	for _, c := range f.calls {
		if c.args[0] == "respawn-pane" {
			cwds = append(cwds, c.args[indexOf(c.args, "-c")+1])
		}
	}
	if strings.Join(cwds, " ") != strings.Join(wantCwd, " ") {
		t.Errorf("panes opened in %v, want %v", cwds, wantCwd)
	}
}

// The invariant: `spektacular init <vendor>` runs as the session uid, in that
// session's worktree, once per vendor per session, and only for vendors spek
// supports.
func TestUserSessionWorkspaceSpekInitOncePerVendor(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	for _, spec := range []crux.UserSessionSpec{
		workspaceSpec(testSessionID, testSessionSlot, "claude"),
		workspaceSpec(testSessionID, testSessionSlot, "claude"),
		workspaceSpec(testSecondSessionID, testSecondSlot, "claude"),
		workspaceSpec(testSecondSessionID, testSecondSlot, "codex"),
		workspaceSpec(testSecondSessionID, testSecondSlot, "goose"),
	} {
		if err := r.Start(spec); err != nil {
			t.Fatalf("Start(%s): %v", spec.Vendor, err)
		}
	}
	inits := privCallsWithScript(p, userSessionSpekInitScript)
	if len(inits) != 3 {
		t.Fatalf("spektacular init ran %d times, want once per supported vendor per session: %+v", len(inits), inits)
	}
	wantWS := []string{r.SessionWorkspaceDir(testSessionID, testWorkspaceRepo), r.SessionWorkspaceDir(testSecondSessionID, testWorkspaceRepo), r.SessionWorkspaceDir(testSecondSessionID, testWorkspaceRepo)}
	wantVendors := []string{"claude", "claude", "codex"}
	secondUser := strconv.Itoa(defaultUserSessionUIDBase+testSecondSlot-1) + ":" + strconv.Itoa(defaultUserSessionGID)
	// Each init runs as the session's own uid with the shared sessions
	// group, in that session's worktree.
	wantUsers := []string{testSessionSpecUser, secondUser, secondUser}
	wantHomes := []string{userSessionHome(testSessionID), userSessionHome(testSecondSessionID), userSessionHome(testSecondSessionID)}
	for i, c := range inits {
		if c.user != wantUsers[i] {
			t.Errorf("init %d ran as %q, want the session uid %q", i, c.user, wantUsers[i])
		}
		if c.argv[4] != wantWS[i] || c.argv[5] != wantHomes[i] || c.argv[6] != "/usr/bin/"+userSessionSpekDefaultBinary || c.argv[7] != wantVendors[i] {
			t.Errorf("init %d args = %v", i, c.argv)
		}
	}
	for _, m := range [][2]string{{testSessionID, "claude"}, {testSecondSessionID, "claude"}, {testSecondSessionID, "codex"}} {
		if _, err := os.Stat(userSessionSpekInitMarker(m[0], m[1])); err != nil {
			t.Errorf("no init marker for %v: %v", m, err)
		}
	}
	if _, err := os.Stat(userSessionSpekInitMarker(testSecondSessionID, "goose")); err == nil {
		t.Error("init marker written for a vendor spek does not support")
	}
	for v := range userSessionSpekAgents {
		if _, ok := findUserSessionVendor(v); !ok {
			t.Errorf("spek agent %q is not a session vendor", v)
		}
	}
	if _, ok := findUserSessionVendor("spektacular"); ok {
		t.Error("spek must never be offered as a session vendor")
	}
}

func TestUserSessionWorkspaceMissingSpekDoesNotFailStart(t *testing.T) {
	r, f, p, _ := workspaceRuntime(t)
	r.lookPath = func(name string) (string, error) {
		if name == userSessionSpekDefaultBinary {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatalf("missing spektacular failed the session: %v", err)
	}
	if n := len(privCallsWithScript(p, userSessionSpekInitScript)); n != 0 {
		t.Errorf("init attempted %d times without the binary", n)
	}
	if indexOf(f.verbs(), "respawn-pane") < 0 {
		t.Error("CLI not launched")
	}
	if _, err := os.Stat(userSessionSpekInitMarker(testSessionID, "claude")); err == nil {
		t.Error("marker written although init never ran")
	}
}

func TestUserSessionWorkspaceSpekInitFailureIsRetriedNotFatal(t *testing.T) {
	r, f, p, _ := workspaceRuntime(t)
	inner := p.handler
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) >= 3 && c.argv[2] == userSessionSpekInitScript {
			return true, []byte("init exploded"), errors.New("exit 1")
		}
		return inner(c)
	}
	for i := 0; i < 2; i++ {
		if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
			t.Fatalf("failed init failed the session: %v", err)
		}
	}
	if n := len(privCallsWithScript(p, userSessionSpekInitScript)); n != 2 {
		t.Errorf("failed init should be retried on the next start, ran %d times", n)
	}
	if _, err := os.Stat(userSessionSpekInitMarker(testSessionID, "claude")); err == nil {
		t.Error("marker written for a failed init")
	}
	if indexOf(f.verbs(), "respawn-pane") < 0 {
		t.Error("CLI not launched")
	}
}

func TestUserSessionWorkspaceCloneFailureFailsStart(t *testing.T) {
	r, f, _, g := workspaceRuntime(t)
	g.fail = true
	err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude"))
	if err == nil || !strings.Contains(err.Error(), "preparing session workspace") || !strings.Contains(err.Error(), "could not read from remote") {
		t.Fatalf("Start error = %v, want the clone failure with git's output", err)
	}
	if indexOf(f.verbs(), "respawn-pane") >= 0 {
		t.Error("CLI launched outside the workspace the record names")
	}
	if _, err := os.Lstat(r.WorkspaceDir(testWorkspaceRepo)); err == nil {
		t.Error("a failed clone left a workspace in place")
	}
}

func TestUserSessionWorkspaceClonesAnonymouslyWithoutToken(t *testing.T) {
	for name, minter := range map[string]*fakeWorkspaceMinter{"no minter": nil, "mint fails": {err: errors.New("no app auth")}, "empty token": {token: " "}} {
		t.Run(name, func(t *testing.T) {
			r, _, _, g := workspaceRuntime(t)
			if minter == nil {
				r.cloneMinter = nil
			} else {
				r.cloneMinter = minter
			}
			if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
				t.Fatal(err)
			}
			for _, a := range g.calls[0] {
				if strings.Contains(a, "credential") {
					t.Errorf("anonymous clone carries a credential helper: %v", g.calls[0])
				}
			}
			if minter != nil && (len(minter.repos) != 1 || minter.repos[0] != testWorkspaceRepo) {
				t.Errorf("minter asked for %v, want %s", minter.repos, testWorkspaceRepo)
			}
		})
	}
}

func TestUserSessionWorkspaceRejectsBadRepo(t *testing.T) {
	r, f, _, g := workspaceRuntime(t)
	for _, repo := range []string{"../etc", "acme", "acme/", "/widgets", "a/b/c", ".hidden/x", "acme/wid gets", "acme/-flag"} {
		spec := workspaceSpec(testSessionID, testSessionSlot, "claude")
		spec.WorkspaceRepo = repo
		if err := r.Start(spec); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
		if r.WorkspaceDir(repo) != "" {
			t.Errorf("WorkspaceDir(%q) = %q, want empty", repo, r.WorkspaceDir(repo))
		}
		if got := r.SessionWorkspaceDir(testSessionID, repo); got != "" {
			t.Errorf("SessionWorkspaceDir(%q) = %q, want empty", repo, got)
		}
	}
	if len(g.calls) != 0 || indexOf(f.verbs(), "respawn-pane") >= 0 {
		t.Error("a malformed repo reached git or launched a CLI")
	}
	if r.WorkspaceDir("") != "" {
		t.Error("empty repo has a workspace dir")
	}
	t.Setenv(userSessionWorkspaceDirEnv, "/x/ws")
	if got := r.WorkspaceDir("Acme-Inc/my.repo_1"); got != filepath.Join("/x/ws", "Acme-Inc", "my.repo_1") {
		t.Errorf("WorkspaceDir = %q", got)
	}
	for _, id := range []string{"", "../x", "ABCDEF012345", testSessionID + "/.."} {
		if got := r.SessionWorkspaceDir(id, testWorkspaceRepo); got != "" {
			t.Errorf("SessionWorkspaceDir(%q) = %q, want empty", id, got)
		}
	}
}

func TestUserSessionWithoutWorkspaceKeepsEmptyWorkDir(t *testing.T) {
	r, f, p, g := workspaceRuntime(t)
	if err := r.Start(crux.UserSessionSpec{ID: testSessionID, UIDSlot: testSessionSlot, Vendor: "claude"}); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 0 || len(privCallsWithScript(p, userSessionSpekInitScript)) != 0 {
		t.Error("no workspace requested, yet git or spek ran")
	}
	for _, c := range f.calls {
		if c.args[0] == "respawn-pane" && c.args[indexOf(c.args, "-c")+1] != userSessionWorkDir(testSessionID) {
			t.Errorf("cwd = %v", c.args)
		}
		if indexOf(c.args, "GIT_CONFIG_COUNT") >= 0 {
			t.Error("workspace git env set without a workspace")
		}
	}
}

// The sessions gid is refused wherever it would widen what a session can
// read: inside the session uid range, the sandbox reader group, or any group
// of the hive process itself.
func TestUserSessionGIDGuards(t *testing.T) {
	newTestSessionRuntime(t, &fakeTmux{}, nil)
	if _, err := userSessionUID(testSessionSlot); err != nil {
		t.Fatalf("default gid refused: %v", err)
	}
	bad := map[string]string{
		"inside uid range": strconv.Itoa(defaultUserSessionUIDBase + 5),
		"reader group":     strconv.Itoa(testReaderGID),
	}
	if gid := os.Getgid(); gid > 0 {
		bad["hive's own group"] = strconv.Itoa(gid)
	}
	for name, gid := range bad {
		t.Run(name, func(t *testing.T) {
			t.Setenv(userSessionGIDEnv, gid)
			if _, err := userSessionUID(testSessionSlot); err == nil {
				t.Errorf("%s=%s accepted", userSessionGIDEnv, gid)
			}
		})
	}
	t.Setenv(userSessionGIDEnv, "")
	if userSessionGID() != defaultUserSessionGID {
		t.Error("default gid changed")
	}
}

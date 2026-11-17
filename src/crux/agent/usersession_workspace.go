package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// The per-hive session workspace. A session is a person using a CLI the way
// they would on a laptop: inside a checkout of the project that has been
// `spektacular init <agent>`-ed, so the CLI finds .spektacular/ and the spek
// skills. The hive keeps ONE clone per repository and gives every session its
// own git worktree of it:
//
//	<workspace root>/<org>/<repo>                     the clone, root:<sessions gid> 2770, files g+rw
//	<workspace root>/.sessions/<id>/<org>/<repo>      the session's worktree, <session uid>:<sessions gid> 2770
//
// where the workspace root defaults to <sandbox root>/workspace (hive-owned,
// 0711, like the sandbox root itself). The clone is cloned once, by the hive,
// with a short-lived token in a one-shot credential helper that never reaches
// the clone's config or remotes, then handed to the sessions group; the remote
// stays the plain https URL and no session ever holds a hive credential, so
// pushing is the person's own business with their own login. `git fetch` is
// never automatic.
//
// Sessions share the clone's objects and refs but not a working tree: each
// one has its own checked-out branch (hive-session/<id>) and its own
// .spektacular/ (spek keeps ONE in-progress workflow per directory, so a
// shared tree let one person's unfinished spec block or be discarded by
// another's, #18588). People sharing one session (a "jam") still share
// everything; two sessions no longer see each other's uncommitted edits. The
// worktree is added by the session uid (hooks off), never by root, and is
// removed with the session; work left uncommitted is committed onto the
// session's branch first and a branch holding commits HEAD lacks is kept.
//
// `spektacular init <vendor>` runs once per vendor per session worktree, as
// the session's uid, only when the vendor is one spek supports and the binary
// is installed; a failure or an absent binary is logged and the session still
// starts. Clone, worktree add/remove and init are serialised by workspaceMu.
const (
	// userSessionWorkspaceDirEnv relocates the workspace root (default
	// <HIVE_SESSIONS_SANDBOX_DIR>/workspace).
	userSessionWorkspaceDirEnv  = "HIVE_SESSIONS_WORKSPACE_DIR"
	userSessionWorkspaceDirName = "workspace"
	// userSessionWorkspaceParentMode is the mode of the hive-owned root and
	// <org> directories: traverse-only, like the sandbox root.
	userSessionWorkspaceParentMode = 0o711
	// userSessionWorkspaceCloneTimeout bounds the one-time clone (git runs as
	// the hive itself and can be killed when it expires).
	userSessionWorkspaceCloneTimeout = 10 * time.Minute
	// userSessionWorkspaceHandoffTimeout bounds the root hand-over of a
	// finished clone (`chown -R` and two `find -exec chmod` over the whole
	// tree) and the root removal of a stale staging tree. It is deliberately
	// the clone's own order of magnitude, NOT userSessionCmdTimeout (10s): on a
	// repository the size of a real project the hand-over outlives that, and a
	// cancelled root child is one the hive cannot kill (usersession_priv.go).
	userSessionWorkspaceHandoffTimeout = 10 * time.Minute
	// userSessionSpekInitTimeout bounds one `spektacular init`.
	userSessionSpekInitTimeout = 2 * time.Minute
	// userSessionWorkspaceStateDirName, under the sessions data dir, holds the
	// hive-owned "init ran" markers (<session id>/<vendor>) outside the
	// worktree so they never show up in git status.
	userSessionWorkspaceStateDirName = "worktree-init"
	userSessionWorkspaceStateDirMode = 0o700
	userSessionWorkspaceStateMode    = 0o600
	// userSessionWorktreeDirName, under the workspace root, holds the
	// per-session worktrees (<id>/<org>/<repo>). Its leading dot keeps it
	// apart from every <org>: userSessionRepoPartPattern never starts with one.
	userSessionWorktreeDirName = ".sessions"
	// userSessionWorktreeBranchPrefix names the branch a session's worktree
	// starts on (hive-session/<id>).
	userSessionWorktreeBranchPrefix = "hive-session/"
	// userSessionWorktreeTimeout bounds one worktree add (a full checkout of
	// the project) or removal.
	userSessionWorktreeTimeout = 10 * time.Minute
	// userSessionWorkspaceCloningSuffix names the staging directory a clone
	// lands in before root hands it to the sessions group and moves it into
	// place, so a half-finished clone is never mistaken for a workspace.
	userSessionWorkspaceCloningSuffix = ".cloning"
	// userSessionWorkspaceCredPrefix prefixes the one-shot credential file.
	userSessionWorkspaceCredPrefix = ".hive-git-credentials-"
	userSessionWorkspaceCredMode   = 0o600
	// userSessionSpekDefaultBinary is the spektacular executable name resolved
	// through PATH when the spec names none (config.DefaultSpektacularBinary).
	userSessionSpekDefaultBinary = "spektacular"
	// userSessionWorkspaceEnv tells the CLI (and the person) where the
	// workspace is.
	userSessionWorkspaceEnv = "HIVE_SESSION_WORKSPACE"
)

// userSessionRepoPartPattern admits one GitHub owner or repository name.
var userSessionRepoPartPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// userSessionSpekAgents are the vendors `spektacular init` supports
// (hivecommons/spektacular README, "Supported agents"). Spek itself is not a
// vendor and never appears in the session vendor table.
var userSessionSpekAgents = map[string]bool{
	"claude": true, "bob": true, "codex": true, "copilot": true, "omp": true,
}

// userSessionWorkspaceOwnScript (run as root) hands a finished clone to the
// sessions group and moves it into place. $1 is the staging dir, $2 the final
// workspace dir, $3 the sessions gid. Directories become setgid group-writable
// (new entries inherit the group), files group read-write; nothing is opened
// to "other". Symlinks are neither followed nor re-moded.
const userSessionWorkspaceOwnScript = `set -e
src=$1; dst=$2; g=$3
chown -R root:"$g" -- "$src"
find "$src" -type d -exec chmod 2770 {} +
find "$src" -type f -exec chmod ug+rw,o-rwx {} +
mv -- "$src" "$dst"`

// userSessionSpekInitScript runs `spektacular init <vendor>` inside the
// session's worktree as the session uid. $1 is the worktree, $2 the session
// HOME, $3 the spektacular binary, $4 the vendor.
const userSessionSpekInitScript = `set -e; cd -- "$1"; HOME=$2; export HOME; exec "$3" init "$4"`

// userSessionWorktreeOwnScript (run as root) hands the hive-made, empty
// worktree directory to the session: $1 the directory, $2 the session uid, $3
// the sessions gid. Its parents are hive-owned and writable by nobody else; a
// symlink at the name fails closed.
const userSessionWorktreeOwnScript = `set -e
d=$1; u=$2; g=$3
[ ! -L "$d" ]
[ -d "$d" ]
chown -- "$u:$g" "$d"
chmod 2770 -- "$d"`

// userSessionWorktreeGit defines g, the git every worktree script runs: the
// root-owned clone and the worktree are marked safe for this one call, and
// hooks are off, so nothing a session planted in the shared clone runs on the
// hive's behalf. The scripts set $clone and $ws first.
const userSessionWorktreeGit = `g() { git -c safe.directory="$clone" -c safe.directory="$ws" -c core.hooksPath=/dev/null "$@"; }`

// userSessionWorktreeAddScript (run as the session uid:sessions gid) adds the
// session's worktree of the clone: $1 the clone, $2 the (empty) worktree dir,
// $3 the session branch. The branch is created at the clone's HEAD, or reused
// when a removal kept it. Stale registrations are pruned first so a branch
// whose worktree vanished can be checked out again.
const userSessionWorktreeAddScript = `set -e
clone=$1; ws=$2; b=$3
` + userSessionWorktreeGit + `
g -C "$clone" worktree prune
if g -C "$clone" show-ref --verify --quiet "refs/heads/$b"; then
  g -C "$clone" worktree add "$ws" "$b"
else
  g -C "$clone" worktree add -b "$b" "$ws" HEAD
fi`

// userSessionWorktreeRemoveScript (run as the worktree's owner uid:sessions
// gid) removes a session's worktree without losing work: $1 the clone, $2 the
// worktree, $3 the session branch. Uncommitted changes (untracked files
// included, ignored ones not) are first committed onto whatever the worktree
// has checked out, a detached HEAD is saved as <branch>-detached, then the
// worktree is removed and pruned. The session branches are deleted only when
// the clone's HEAD already contains them; otherwise they are kept and named
// on stdout. A failing git aborts before anything is removed.
const userSessionWorktreeRemoveScript = `set -e
clone=$1; ws=$2; b=$3
` + userSessionWorktreeGit + `
if [ -e "$ws/.git" ]; then
  st=$(g -C "$ws" status --porcelain)
  if [ -n "$st" ]; then
    g -C "$ws" add -A
    g -C "$ws" -c user.name=Hive -c user.email=hive@localhost -c commit.gpgsign=false commit -q --no-verify -m "hive: work left uncommitted when the session was deleted"
    echo "committed uncommitted work"
  fi
  if ! g -C "$ws" symbolic-ref -q HEAD >/dev/null; then
    g -C "$ws" branch -f "$b-detached" HEAD
  fi
  g -C "$clone" worktree remove --force "$ws"
fi
g -C "$clone" worktree prune
for x in "$b" "$b-detached"; do
  if g -C "$clone" show-ref --verify --quiet "refs/heads/$x"; then
    if g -C "$clone" merge-base --is-ancestor "$x" HEAD; then
      g -C "$clone" branch -D "$x" >/dev/null
    else
      echo "kept branch $x"
    fi
  fi
done`

// gitRunFunc runs one git invocation as the hive itself (the clone). It is
// the test seam: tests substitute a recorder so no real git is ever run.
type gitRunFunc func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)

func execUserSessionGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = tmuxCmdWaitDelay
	return cmd.CombinedOutput()
}

// SetCloneMinter installs the GitHub App token minter the hive clones the
// workspace with (the same scoped-token path agents clone with). Nil clones
// anonymously, which works for a public repository.
func (r *UserSessionRuntime) SetCloneMinter(m pushbroker.TokenMinter) {
	r.workspaceMu.Lock()
	defer r.workspaceMu.Unlock()
	r.cloneMinter = m
}

func userSessionWorkspaceRoot() string {
	if v := os.Getenv(userSessionWorkspaceDirEnv); v != "" {
		return v
	}
	return filepath.Join(userSessionSandboxRoot(), userSessionWorkspaceDirName)
}

// splitUserSessionRepo validates "org/repo" and returns its parts.
func splitUserSessionRepo(repo string) (string, string, error) {
	org, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || !userSessionRepoPartPattern.MatchString(org) || !userSessionRepoPartPattern.MatchString(name) {
		return "", "", fmt.Errorf("workspace repository %q is not org/repo", repo)
	}
	return org, name, nil
}

func userSessionWorkspaceDir(org, name string) string {
	return filepath.Join(userSessionWorkspaceRoot(), org, name)
}

// userSessionWorktreesDir holds every worktree of one session.
func userSessionWorktreesDir(id string) string {
	return filepath.Join(userSessionWorkspaceRoot(), userSessionWorktreeDirName, id)
}

// userSessionWorktreeDir is the session's own worktree of <org>/<name>.
func userSessionWorktreeDir(id, org, name string) string {
	return filepath.Join(userSessionWorktreesDir(id), org, name)
}

// userSessionWorktreeBranch is the branch a session's worktree starts on.
func userSessionWorktreeBranch(id string) string { return userSessionWorktreeBranchPrefix + id }

// WorkspaceDir is the path of the hive's shared clone of "org/repo", or ""
// when repo is empty or malformed. It is the same whether or not the clone
// exists yet; the first Start creates it. Sessions open in their own worktree
// of it (SessionWorkspaceDir).
func (r *UserSessionRuntime) WorkspaceDir(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return ""
	}
	org, name, err := splitUserSessionRepo(repo)
	if err != nil {
		return ""
	}
	return userSessionWorkspaceDir(org, name)
}

// SessionWorkspaceDir is the path session id opens in for "org/repo": its own
// worktree of the shared clone. "" when the id or repo is empty or malformed.
// Start creates it.
func (r *UserSessionRuntime) SessionWorkspaceDir(id, repo string) string {
	if checkUserSessionID(id) != nil || strings.TrimSpace(repo) == "" {
		return ""
	}
	org, name, err := splitUserSessionRepo(repo)
	if err != nil {
		return ""
	}
	return userSessionWorktreeDir(id, org, name)
}

// ensureWorkspace returns the session's worktree for spec.WorkspaceRepo,
// cloning the shared clone on first use, adding the worktree on the session's
// first start and running spek init in it for the vendor once. The sandbox
// (HOME) of the session must already exist. A clone or worktree failure is
// returned: a session that silently opened somewhere else would not be the
// workspace the record names.
func (r *UserSessionRuntime) ensureWorkspace(spec crux.UserSessionSpec, uid int) (string, error) {
	org, name, err := splitUserSessionRepo(spec.WorkspaceRepo)
	if err != nil {
		return "", err
	}
	clone := userSessionWorkspaceDir(org, name)
	r.workspaceMu.Lock()
	defer r.workspaceMu.Unlock()
	if _, err := os.Lstat(clone); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("checking session workspace %s: %w", clone, err)
		}
		if err := r.cloneWorkspace(org, name, clone); err != nil {
			return "", fmt.Errorf("preparing session workspace: %w", err)
		}
		r.logger.Info("session workspace cloned", "repo", org+"/"+name, "path", clone)
	}
	ws, err := r.ensureWorktree(spec.ID, uid, org, name, clone)
	if err != nil {
		return "", fmt.Errorf("preparing session worktree: %w", err)
	}
	r.spekInit(spec, uid, ws)
	return ws, nil
}

// ensureWorktree returns the session's worktree of clone, adding it when it
// does not exist yet: the hive makes the empty directory under hive-owned,
// traverse-only parents, root hands it to the session uid, and the session
// uid runs `git worktree add` (userSessionWorktreeAddScript). No second clone
// is ever made. The caller holds workspaceMu.
func (r *UserSessionRuntime) ensureWorktree(id string, uid int, org, name, clone string) (string, error) {
	ws := userSessionWorktreeDir(id, org, name)
	if _, err := os.Lstat(filepath.Join(ws, ".git")); err == nil {
		return ws, nil
	}
	for _, dir := range []string{filepath.Dir(userSessionWorktreesDir(id)), userSessionWorktreesDir(id), filepath.Dir(ws)} {
		if err := os.MkdirAll(dir, userSessionWorkspaceParentMode); err != nil {
			return "", fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := os.Chmod(dir, userSessionWorkspaceParentMode); err != nil {
			return "", fmt.Errorf("securing %s: %w", dir, err)
		}
	}
	if err := os.Mkdir(ws, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("creating %s: %w", ws, err)
	}
	if out, err := r.privRun(userSessionRootSpec, nil, "sh", "-c", userSessionWorktreeOwnScript, "sh", ws, strconv.Itoa(uid), strconv.Itoa(userSessionGID())); err != nil {
		return "", outputErr("handing the worktree to the session", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionWorktreeTimeout)
	defer cancel()
	if out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionWorktreeAddScript, "sh", clone, ws, userSessionWorktreeBranch(id)); err != nil {
		return "", outputErr("git worktree add", err, out)
	}
	r.logger.Info("session worktree added", "id", id, "repo", org+"/"+name, "path", ws, "branch", userSessionWorktreeBranch(id))
	// A fresh worktree has none of spek's files, whatever an earlier one had.
	if err := os.RemoveAll(userSessionSpekInitMarkerDir(id)); err != nil {
		r.logger.Warn("session worktree: clearing stale spektacular init markers failed", "id", id, "error", err)
	}
	return ws, nil
}

// removeWorktrees removes every worktree session id has (see
// userSessionWorktreeRemoveScript), then its whole worktrees directory and
// its spek init markers. A worktree that was never handed to a session uid
// holds no work and goes with the directory. A failure is returned before
// anything of that worktree is deleted, so the caller keeps the session and
// retries. The caller holds workspaceMu.
func (r *UserSessionRuntime) removeWorktrees(id string) error {
	dir := userSessionWorktreesDir(id)
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.RemoveAll(userSessionSpekInitMarkerDir(id))
		}
		return err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*"))
	if err != nil {
		return err
	}
	for _, ws := range matches {
		org, name := filepath.Base(filepath.Dir(ws)), filepath.Base(ws)
		if !userSessionRepoPartPattern.MatchString(org) || !userSessionRepoPartPattern.MatchString(name) {
			continue
		}
		if err := r.removeWorktree(id, org, name, ws); err != nil {
			return err
		}
	}
	if out, err := r.privRunFor(userSessionWorktreeTimeout, userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", dir); err != nil {
		return outputErr("removing session worktrees dir", err, out)
	}
	return os.RemoveAll(userSessionSpekInitMarkerDir(id))
}

// removeWorktree runs userSessionWorktreeRemoveScript for one worktree as the
// uid that owns it, and logs what was kept.
func (r *UserSessionRuntime) removeWorktree(id, org, name, ws string) error {
	uid, err := r.statUID(ws)
	if err != nil || !userSessionUIDInRange(uid) {
		return nil
	}
	repo := org + "/" + name
	clone := userSessionWorkspaceDir(org, name)
	if _, err := os.Lstat(filepath.Join(clone, ".git")); err != nil {
		r.logger.Warn("session worktree: the shared clone is gone, so its worktree is deleted without git; anything uncommitted in it is lost", "id", id, "repo", repo, "path", ws)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionWorktreeTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionWorktreeRemoveScript, "sh", clone, ws, userSessionWorktreeBranch(id))
	if err != nil {
		return outputErr("removing session worktree", err, out)
	}
	if kept := strings.TrimSpace(string(out)); kept != "" {
		r.logger.Info("session worktree removed; its work stays on a branch of the shared clone", "id", id, "repo", repo, "clone", clone, "detail", kept)
	} else {
		r.logger.Info("session worktree removed", "id", id, "repo", repo)
	}
	return nil
}

// SweepWorktrees removes the worktrees of every session live reports gone
// (its record no longer exists), as Remove would. It runs under workspaceMu,
// so a session whose record is saved before its start can never lose a
// worktree it is still being given. Failures are logged and retried by the
// next sweep.
func (r *UserSessionRuntime) SweepWorktrees(live func(id string) bool) {
	r.workspaceMu.Lock()
	defer r.workspaceMu.Unlock()
	entries, err := os.ReadDir(filepath.Join(userSessionWorkspaceRoot(), userSessionWorktreeDirName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.logger.Warn("session worktree sweep: listing worktrees failed", "error", err)
		}
		return
	}
	for _, e := range entries {
		id := e.Name()
		if checkUserSessionID(id) != nil || live(id) {
			continue
		}
		if err := r.removeWorktrees(id); err != nil {
			r.logger.Warn("session worktree sweep: removing an orphaned worktree failed; retried next sweep", "id", id, "error", err)
			continue
		}
		r.logger.Info("session worktree sweep: removed the worktree of a deleted session", "id", id)
	}
}

// cloneWorkspace clones https://github.com/<org>/<name>.git into a staging
// directory as the hive, then has root hand it to the sessions group and move
// it to ws. The token travels only in a one-shot `git -c credential.helper`
// (before the subcommand, so git does not persist it) and its file is removed
// before this returns; `core.sharedRepository=group` is the ONLY config
// written into the clone.
func (r *UserSessionRuntime) cloneWorkspace(org, name, ws string) error {
	parent := filepath.Dir(ws)
	for _, dir := range []string{userSessionWorkspaceRoot(), parent} {
		if err := os.MkdirAll(dir, userSessionWorkspaceParentMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := os.Chmod(dir, userSessionWorkspaceParentMode); err != nil {
			return fmt.Errorf("securing %s: %w", dir, err)
		}
	}
	staging := ws + userSessionWorkspaceCloningSuffix
	if _, err := os.Lstat(staging); err == nil {
		// An interrupted earlier clone; it may already be root-owned.
		if err := r.removeWorkspaceStaging(staging); err != nil {
			return fmt.Errorf("removing stale workspace staging dir: %w", err)
		}
	}
	// The clone has its own deadline, detached from whichever request asked
	// for the session: git runs as the hive and may be killed when it expires.
	ctx, cancel := context.WithTimeout(context.Background(), userSessionWorkspaceCloneTimeout)
	defer cancel()
	repo := org + "/" + name
	authArgs, cleanupAuth := r.workspaceCloneAuth(ctx, repo, parent)
	defer cleanupAuth()
	url := "https://github.com/" + repo + ".git"
	args := append(append([]string{}, authArgs...), "clone", "-c", "core.sharedRepository=group", "--", url, staging)
	if out, err := r.git(ctx, "", pushbroker.PushEnv(os.Environ()), args...); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("git clone %s: %w: %s", repo, err, strings.TrimSpace(string(out)))
	}
	cleanupAuth()
	gid := userSessionGID()
	// The hand-over runs as root under its own, generous deadline
	// (privRunFor): it is never bound to the clone's context, let alone to an
	// HTTP request's. Should it still fail, the staging tree is removed so the
	// next start clones afresh instead of tripping over a half-owned tree.
	if out, err := r.privRunFor(userSessionWorkspaceHandoffTimeout, userSessionRootSpec, nil, "sh", "-c", userSessionWorkspaceOwnScript, "sh", staging, ws, fmt.Sprint(gid)); err != nil {
		if _, statErr := os.Lstat(staging); statErr == nil {
			if rmErr := r.removeWorkspaceStaging(staging); rmErr != nil {
				r.logger.Warn("session workspace: removing the staging dir after a failed hand-over failed; the next start retries", "path", staging, "error", rmErr)
			}
		}
		return outputErr("handing workspace to the sessions group", err, out)
	}
	return nil
}

// removeWorkspaceStaging deletes a (possibly root-owned) staging tree through
// the helper, under the hand-over deadline rather than the 10-second command
// timeout: a whole clone takes longer than that to remove.
func (r *UserSessionRuntime) removeWorkspaceStaging(staging string) error {
	if out, err := r.privRunFor(userSessionWorkspaceHandoffTimeout, userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", staging); err != nil {
		return outputErr("rm", err, out)
	}
	return nil
}

// workspaceCloneAuth mints a scoped token for repo and stores it in a 0600
// hive-owned file under dir for a one-shot `credential.helper=store`. Without
// a minter, or when minting fails, the clone is attempted anonymously (public
// repositories) and the reason is logged. The returned cleanup removes the
// file and is safe to call twice.
func (r *UserSessionRuntime) workspaceCloneAuth(ctx context.Context, repo, dir string) ([]string, func()) {
	noop := func() {}
	if r.cloneMinter == nil {
		return nil, noop
	}
	token, err := r.cloneMinter.MintPushToken(ctx, repo)
	if err != nil || strings.TrimSpace(token) == "" {
		r.logger.Warn("session workspace: no clone token, cloning anonymously", "repo", repo, "error", err)
		return nil, noop
	}
	path := filepath.Join(dir, userSessionWorkspaceCredPrefix+fmt.Sprint(r.now().UnixNano()))
	line := "https://x-access-token:" + token + "@github.com\n"
	if err := os.WriteFile(path, []byte(line), userSessionWorkspaceCredMode); err != nil {
		r.logger.Warn("session workspace: writing clone credential file failed, cloning anonymously", "repo", repo, "error", err)
		return nil, noop
	}
	return []string{"-c", "credential.helper=store --file=" + path}, func() { _ = os.Remove(path) }
}

// userSessionSpekInitMarkerDir holds the hive-owned files recording that
// `spektacular init <vendor>` succeeded in session id's worktree.
func userSessionSpekInitMarkerDir(id string) string {
	return filepath.Join(userSessionsDir(), userSessionWorkspaceStateDirName, id)
}

func userSessionSpekInitMarker(id, vendor string) string {
	return filepath.Join(userSessionSpekInitMarkerDir(id), vendor)
}

// spekInit runs `spektacular init <vendor>` in ws as the session uid, once per
// vendor per session worktree. Nothing here fails the session: an unsupported
// vendor is skipped silently, a missing binary is logged at info, a failed
// init at warn (and retried on the next start, since no marker is written).
func (r *UserSessionRuntime) spekInit(spec crux.UserSessionSpec, uid int, ws string) {
	if !userSessionSpekAgents[spec.Vendor] {
		return
	}
	bin := strings.TrimSpace(spec.SpektacularBinary)
	if bin == "" {
		bin = userSessionSpekDefaultBinary
	}
	path, err := r.lookPath(bin)
	if err != nil {
		r.logger.Info("session workspace: spektacular is not installed; the workspace gets no spek skills", "binary", bin, "repo", spec.WorkspaceRepo)
		return
	}
	marker := userSessionSpekInitMarker(spec.ID, spec.Vendor)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionSpekInitTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionSpekInitScript, "sh", ws, userSessionHome(spec.ID), path, spec.Vendor)
	if err != nil {
		r.logger.Warn("session workspace: spektacular init failed; the session starts without spek skills", "vendor", spec.Vendor, "repo", spec.WorkspaceRepo, "error", outputErr("spektacular init", err, out))
		return
	}
	err = os.MkdirAll(filepath.Dir(marker), userSessionWorkspaceStateDirMode)
	if err == nil {
		err = os.WriteFile(marker, []byte(r.now().UTC().Format(time.RFC3339)+"\n"), userSessionWorkspaceStateMode)
	}
	if err != nil {
		r.logger.Warn("session workspace: recording spektacular init failed; it will run again next start", "vendor", spec.Vendor, "error", err)
	}
	r.logger.Info("session workspace: spektacular init ran", "vendor", spec.Vendor, "repo", spec.WorkspaceRepo, "path", ws)
}

// userSessionWorkspaceEnvPairs are the extra session variables for a session
// opened in a workspace. git refuses to work in a directory owned by another
// user unless it is marked safe: the worktree is the session's own, but the
// shared clone behind it is root-owned, and the environment form of that
// config applies to every git the CLI spawns without writing any file.
func userSessionWorkspaceEnvPairs(workspace string) [][2]string {
	if workspace == "" {
		return nil
	}
	return [][2]string{
		{userSessionWorkspaceEnv, workspace},
		{"GIT_CONFIG_COUNT", "1"},
		{"GIT_CONFIG_KEY_0", "safe.directory"},
		{"GIT_CONFIG_VALUE_0", workspace},
	}
}

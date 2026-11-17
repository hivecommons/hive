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
// removed with the session; work left uncommitted is auto-saved onto the
// session's branch first and a branch holding commits HEAD lacks is kept.
//
// The clone's git config and hooks are locked (userSessionWorkspaceSecureScript)
// before any session gets a worktree of it and again at every boot: root owns
// them and no session can write them, so no session can plant a config entry,
// hook, filter or alias that another session's git, or the hive's git for
// that session, would run as the other uid. Objects and refs stay
// group-writable. The hive itself runs as a uid outside the sessions group, so
// it cannot look inside the clone or a worktree: every "is it there" check
// runs in the scripts, as the session uid.
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
	userSessionWorktreeBranchPrefix = userSessionWorktreeBranchDir + "/"
	// userSessionWorktreeTimeout bounds one worktree add (a full checkout of
	// the project) or removal.
	userSessionWorktreeTimeout = 10 * time.Minute
	// userSessionWorktreeStagingMode is the mode the hive creates an empty
	// worktree directory with, before root hands it to the session as 2770.
	userSessionWorktreeStagingMode = 0o700
	// userSessionWorktreeAddedLine is what userSessionWorktreeAddScript prints
	// when it added a fresh worktree (and not when the session's worktree was
	// already there), so the hive knows to clear stale spek init markers.
	userSessionWorktreeAddedLine = "hive: worktree added"
	// userSessionWorktreeCloneGoneLine is what userSessionWorktreeRemoveScript
	// prints when the shared clone behind the worktree no longer exists, so
	// there is no git to save the work through.
	userSessionWorktreeCloneGoneLine = "hive: shared clone gone"
	// userSessionWorktreeAutoSaveMessage is the commit message of the
	// auto-save userSessionWorktreeRemoveScript makes of uncommitted work.
	userSessionWorktreeAutoSaveMessage = "hive: auto-save of work left uncommitted when the session was deleted"
	// userSessionWorktreeBranchDir is the directory under refs/heads (and
	// logs/refs/heads) that holds every session branch; the lock script makes
	// it group-writable so each session can create its branch there.
	userSessionWorktreeBranchDir = "hive-session"
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

// userSessionHiveGitArgs precede every git subcommand the hive runs for the
// session workspace: no hook and no fsmonitor command ever runs on its behalf.
// The worktree scripts' g() passes the same two.
var userSessionHiveGitArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

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

// userSessionWorkspaceSecureScript (run as root) locks the shared clone's git
// configuration so no session can plant anything that another session's git,
// or the hive's own worktree git, would run as that other uid: $1 the clone,
// $2 the sessions gid, $3 the origin URL, $4 the session branch directory
// under refs/heads. It runs on every clone and once per clone per hive
// process (a boot fixes a clone from an earlier release), never runs git, and
// writes nothing through a path a session could have planted:
//
//   - .git itself is first made group read-only, so nothing in it can be swapped
//     while it is rebuilt, and symlinks at its top level are removed;
//   - .git/config is rewritten from scratch (whatever a session ever put there,
//     a core.fsmonitor, a filter or alias, an include, is dropped) as
//     root:<gid> 0644 with extensions.worktreeConfig, so personal settings go in
//     `git config --worktree` (the session's own config.worktree);
//   - the main worktree's config.worktree and a commondir of "." are root-owned
//     placeholders, so neither can be planted to redirect git elsewhere;
//   - hooks/, info/ and modules/ are emptied and become root 0755;
//   - objects/, refs/, logs/ (and the session branch directories) stay setgid
//     group-writable so commits, branches and fetches work, and worktrees/ is
//     group-writable with the sticky bit, so a session adds its own worktree
//     entry but cannot rename or delete another's;
//   - every other top-level file becomes root:<gid> 0644, and .git (and the
//     clone directory above it) end as 3770: the sticky bit stops a session
//     from renaming or replacing anything root owns there.
//
// The main worktree's branch keeps its upstream when HEAD names one. gc and
// maintenance are off: they would rewrite root-owned files and fail.
const userSessionWorkspaceSecureScript = `set -e
c=$1; g=$2; url=$3; bp=$4
gd=$c/.git
[ -d "$c" ] && [ ! -L "$c" ] && [ -d "$gd" ] && [ ! -L "$gd" ]
chown root:"$g" -- "$c" "$gd"
chmod 0750 -- "$gd"
chmod 3770 -- "$c"
find "$gd" -mindepth 1 -maxdepth 1 -type l -exec rm -f -- {} +
[ -f "$gd/HEAD" ]
head=$(sed -n '1s#^ref: refs/heads/\([A-Za-z0-9][A-Za-z0-9._/-]*\)$#\1#p' "$gd/HEAD")
t=$(mktemp "$gd/config.hive.XXXXXX")
cat > "$t" <<CFG
[core]
	repositoryformatversion = 1
	filemode = true
	bare = false
	logallrefupdates = true
	sharedRepository = group
[extensions]
	worktreeConfig = true
[gc]
	auto = 0
[maintenance]
	auto = false
[remote "origin"]
	url = $url
	fetch = +refs/heads/*:refs/remotes/origin/*
CFG
if [ -n "$head" ]; then printf '[branch "%s"]\n\tremote = origin\n\tmerge = refs/heads/%s\n' "$head" "$head" >> "$t"; fi
mv -f -- "$t" "$gd/config"
t=$(mktemp "$gd/config.hive.XXXXXX")
mv -f -- "$t" "$gd/config.worktree"
t=$(mktemp "$gd/config.hive.XXXXXX")
echo . > "$t"
mv -f -- "$t" "$gd/commondir"
for d in hooks info modules; do
  rm -rf --one-file-system -- "$gd/$d"
  mkdir -m 0755 -- "$gd/$d"
done
for d in objects refs logs worktrees refs/heads logs/refs logs/refs/heads "refs/heads/$bp" "logs/refs/heads/$bp"; do
  [ ! -L "$gd/$d" ]
  [ -d "$gd/$d" ] || mkdir -- "$gd/$d"
  chmod 2770 -- "$gd/$d"
done
chmod 3770 -- "$gd/worktrees"
find "$gd" -mindepth 1 -maxdepth 1 -exec chown -h root:"$g" -- {} +
find "$gd" -mindepth 1 -maxdepth 1 -type f -exec chmod 0644 -- {} +
chown -R root:"$g" -- "$gd/hooks" "$gd/info" "$gd/modules"
chmod 3770 -- "$gd"`

// userSessionWorktreeOwnScript (run as root) hands the hive-made, empty
// worktree directory to the session: $1 the directory, $2 the session uid, $3
// the sessions gid, $4 the hive's own uid. Its parents are hive-owned and
// writable by nobody else; a symlink at the name fails closed. A directory the
// session already owns (a resume) is left alone, and one owned by anybody else
// but the hive is refused, so root never hands a directory from one session to
// another.
const userSessionWorktreeOwnScript = `set -e
d=$1; u=$2; g=$3; h=$4
[ ! -L "$d" ]
[ -d "$d" ]
o=$(stat -c %u -- "$d")
if [ "$o" = "$u" ]; then exit 0; fi
[ "$o" = "$h" ]
chown -- "$u:$g" "$d"
chmod 2770 -- "$d"`

// userSessionWorktreeGit defines what the worktree scripts share. g is the git
// every one of them runs: the root-owned clone is marked safe for this one
// call (the session's own worktree needs no such mark, so git's ownership
// check still guards it), hooks and core.fsmonitor are off. owned checks, as
// the session uid, that $ws/.git is the session's own gitfile pointing at its
// own entry under the clone's worktrees/ directory, whose commondir leads back
// to the clone, and sets $a to that entry: with the clone's config locked
// (userSessionWorkspaceSecureScript) that is everything git reads config
// from, so nothing another session wrote runs here. The scripts set $clone
// and $ws first.
const userSessionWorktreeGit = `g() { git -c safe.directory="$clone" -c core.hooksPath=/dev/null -c core.fsmonitor=false "$@"; }
owned() {
  [ -f "$ws/.git" ] && [ ! -L "$ws/.git" ] && [ -O "$ws/.git" ] || return 1
  a=$(sed -n '1s/^gitdir: //p' "$ws/.git")
  cg=$(cd -P -- "$clone/.git" && pwd -P) || return 1
  case $a in "$cg/worktrees/"*/*|*/../*|*/./*) return 1 ;; "$cg/worktrees/"?*) ;; *) return 1 ;; esac
  [ -d "$a" ] && [ ! -L "$a" ] && [ -O "$a" ] || return 1
  [ -f "$a/commondir" ] && [ ! -L "$a/commondir" ] && [ -O "$a/commondir" ] || return 1
  [ "$(cat "$a/commondir")" = "../.." ]
}`

// userSessionWorktreeAddScript (run as the session uid:sessions gid) adds the
// session's worktree of the clone: $1 the clone, $2 the worktree dir, $3 the
// session branch. Every existence check runs here, as the session uid,
// because the hive's own uid can see inside neither the root:<gid> 2770 clone
// nor the session's 2770 worktree. A clone without .git is reported, not
// guessed at. A worktree that is already there (a resume, or a start after a
// restart) is left as it is when it is the session's own, and refused
// otherwise. A fresh one is added under umask 022 with
// core.sharedRepository=0644, so its entry under worktrees/ is never
// group-writable (not even between its creation and its checkout), and gets
// an empty config.worktree of its own (git keeps that file's mode when it
// rewrites it). The branch is created at the clone's HEAD, or reused when a
// removal kept it. Stale entries are pruned first so a kept branch whose
// worktree vanished can be checked out again (best effort: another session's
// entry is not ours to prune).
const userSessionWorktreeAddScript = `set -e
clone=$1; ws=$2; b=$3
` + userSessionWorktreeGit + `
if [ ! -d "$clone/.git" ]; then echo "` + userSessionWorktreeCloneGoneLine + `"; exit 0; fi
if [ -e "$ws/.git" ] || [ -L "$ws/.git" ]; then
  owned || { echo "hive: $ws/.git is not this session's worktree of $clone" >&2; exit 1; }
  exit 0
fi
umask 022
g -C "$clone" worktree prune 2>/dev/null || true
if g -C "$clone" show-ref --verify --quiet "refs/heads/$b"; then
  g -c core.sharedRepository=0644 -C "$clone" worktree add "$ws" "$b"
else
  g -c core.sharedRepository=0644 -C "$clone" worktree add -b "$b" "$ws" HEAD
fi
owned
: > "$a/config.worktree"
echo "` + userSessionWorktreeAddedLine + `"`

// userSessionWorktreeRemoveScript (run as the worktree's owner uid:sessions
// gid) removes a session's worktree without losing work: $1 the clone, $2 the
// worktree, $3 the session branch. Uncommitted changes (untracked files
// included, ignored ones not) are first committed onto whatever the worktree
// has checked out as an auto-save, a detached HEAD is saved as
// <branch>-detached, then the worktree's entry under the clone's worktrees/
// is deleted and the clone pruned; the hive deletes the worktree directory
// itself afterwards (its parent is the hive's, so the session uid cannot).
// The session branches are deleted only when the clone's HEAD already
// contains them; otherwise they are kept and named on stdout. A failing git
// aborts before anything is removed, and so does a worktree whose .git is not
// the session's own. When the clone itself is gone there is no git to save
// through: the script says so and does nothing.
const userSessionWorktreeRemoveScript = `set -e
clone=$1; ws=$2; b=$3
` + userSessionWorktreeGit + `
if [ ! -d "$clone/.git" ]; then echo "` + userSessionWorktreeCloneGoneLine + `"; exit 0; fi
if [ -e "$ws/.git" ] || [ -L "$ws/.git" ]; then
  owned || { echo "hive: $ws/.git is not this session's worktree of $clone; its work is not saved and nothing is removed" >&2; exit 1; }
  if [ -n "$(g -C "$ws" status --porcelain)" ]; then
    g -C "$ws" add -A
    g -C "$ws" -c user.name=Hive -c user.email=hive@localhost -c commit.gpgsign=false commit -q --no-verify -m "` + userSessionWorktreeAutoSaveMessage + `"
    echo "auto-saved uncommitted work"
  fi
  if ! g -C "$ws" symbolic-ref -q HEAD >/dev/null; then
    g -C "$ws" branch -f "$b-detached" HEAD
  fi
  rm -rf -- "$a"
fi
g -C "$clone" worktree prune 2>/dev/null || true
for x in "$b" "$b-detached"; do
  if g -C "$clone" show-ref --verify --quiet "refs/heads/$x"; then
    if g -C "$clone" merge-base --is-ancestor "$x" HEAD; then
      g -C "$clone" branch -D "$x" >/dev/null 2>&1
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
// cloning the shared clone on first use, locking its git config once per hive
// process, adding the worktree on the session's first start and running spek
// init in it for the vendor once. The sandbox (HOME) of the session must
// already exist. A clone, lock or worktree failure is returned: a session that
// silently opened somewhere else, or in a clone whose config any session can
// still write, would not be the workspace the record names.
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
	if !r.securedClones[clone] {
		if err := r.secureClone(org, name, clone); err != nil {
			return "", fmt.Errorf("locking the session workspace's git config: %w", err)
		}
	}
	ws, err := r.ensureWorktree(spec.ID, uid, org, name, clone)
	if err != nil {
		return "", fmt.Errorf("preparing session worktree: %w", err)
	}
	r.spekInit(spec, uid, ws)
	return ws, nil
}

// secureClone has root lock the shared clone's git config and hooks
// (userSessionWorkspaceSecureScript) and records it for this process. The
// caller holds workspaceMu.
func (r *UserSessionRuntime) secureClone(org, name, clone string) error {
	url := "https://github.com/" + org + "/" + name + ".git"
	if out, err := r.privRunFor(userSessionWorkspaceHandoffTimeout, userSessionRootSpec, nil, "sh", "-c", userSessionWorkspaceSecureScript, "sh", clone, strconv.Itoa(userSessionGID()), url, userSessionWorktreeBranchDir); err != nil {
		return outputErr("locking "+clone, err, out)
	}
	if r.securedClones == nil {
		r.securedClones = map[string]bool{}
	}
	r.securedClones[clone] = true
	return nil
}

// SecureWorkspaces locks the git config and hooks of every shared clone
// already on disk (userSessionWorkspaceSecureScript), so a clone made by an
// earlier release, whose config and hooks every session could write, is fixed
// at boot rather than on its next session start. Failures are logged; the next
// start of a session in that clone retries and fails closed.
func (r *UserSessionRuntime) SecureWorkspaces() {
	r.workspaceMu.Lock()
	defer r.workspaceMu.Unlock()
	root := userSessionWorkspaceRoot()
	orgs, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.logger.Warn("session workspace: listing shared clones to lock failed", "path", root, "error", err)
		}
		return
	}
	for _, o := range orgs {
		org := o.Name()
		// The pattern never admits a leading dot, so the worktrees dir
		// (userSessionWorktreeDirName) is skipped here.
		if !o.IsDir() || !userSessionRepoPartPattern.MatchString(org) {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, org))
		if err != nil {
			r.logger.Warn("session workspace: listing shared clones to lock failed", "path", filepath.Join(root, org), "error", err)
			continue
		}
		for _, e := range repos {
			name := e.Name()
			if !e.IsDir() || strings.HasSuffix(name, userSessionWorkspaceCloningSuffix) || !userSessionRepoPartPattern.MatchString(name) {
				continue
			}
			clone := userSessionWorkspaceDir(org, name)
			if err := r.secureClone(org, name, clone); err != nil {
				r.logger.Warn("session workspace: locking a shared clone's git config failed; sessions in it fail to start until it succeeds", "path", clone, "error", err)
				continue
			}
			r.logger.Info("session workspace: shared clone's git config and hooks locked", "path", clone)
		}
	}
}

// ensureWorktree returns the session's worktree of clone, adding it when it
// does not exist yet: the hive makes the empty directory under hive-owned,
// traverse-only parents, root hands it to the session uid, and the session
// uid runs `git worktree add` (userSessionWorktreeAddScript). Whether the
// worktree is already there is decided by that script, as the session uid:
// the hive's own uid cannot look inside the session's 2770 directory. No
// second clone is ever made. The caller holds workspaceMu.
func (r *UserSessionRuntime) ensureWorktree(id string, uid int, org, name, clone string) (string, error) {
	ws := userSessionWorktreeDir(id, org, name)
	for _, dir := range []string{filepath.Dir(userSessionWorktreesDir(id)), userSessionWorktreesDir(id), filepath.Dir(ws)} {
		if err := os.MkdirAll(dir, userSessionWorkspaceParentMode); err != nil {
			return "", fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := os.Chmod(dir, userSessionWorkspaceParentMode); err != nil {
			return "", fmt.Errorf("securing %s: %w", dir, err)
		}
	}
	if err := os.Mkdir(ws, userSessionWorktreeStagingMode); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("creating %s: %w", ws, err)
	}
	if out, err := r.privRun(userSessionRootSpec, nil, "sh", "-c", userSessionWorktreeOwnScript, "sh", ws, strconv.Itoa(uid), strconv.Itoa(userSessionGID()), strconv.Itoa(os.Getuid())); err != nil {
		return "", outputErr("handing the worktree to the session", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionWorktreeTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionWorktreeAddScript, "sh", clone, ws, userSessionWorktreeBranch(id))
	if err != nil {
		return "", outputErr("git worktree add", err, out)
	}
	if strings.Contains(string(out), userSessionWorktreeCloneGoneLine) {
		return "", fmt.Errorf("the shared clone %s has no .git; remove that directory so the next start clones it again", clone)
	}
	if !strings.Contains(string(out), userSessionWorktreeAddedLine) {
		// The session's own worktree was already there (a resume).
		return ws, nil
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
	ctx, cancel := context.WithTimeout(context.Background(), userSessionWorktreeTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionWorktreeRemoveScript, "sh", clone, ws, userSessionWorktreeBranch(id))
	if err != nil {
		return outputErr("removing session worktree", err, out)
	}
	if strings.Contains(string(out), userSessionWorktreeCloneGoneLine) {
		r.logger.Warn("session worktree: the shared clone is gone, so its worktree is deleted without git; anything uncommitted in it is lost", "id", id, "repo", repo, "path", ws)
		return nil
	}
	if kept := strings.TrimSpace(string(out)); kept != "" {
		r.logger.Info("session worktree removed; its work stays on a branch of the shared clone", "id", id, "repo", repo, "clone", clone, "detail", strings.ReplaceAll(kept, "\n", "; "))
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
// written into the clone, and the lock that follows
// (userSessionWorkspaceSecureScript, run by the caller) rewrites that config
// from scratch anyway.
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
	args := append(append(append([]string{}, userSessionHiveGitArgs...), authArgs...), "clone", "-c", "core.sharedRepository=group", "--", url, staging)
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
// opened in a workspace. No safe.directory is set: the session's worktree,
// its .git file and its entry under the clone's worktrees/ are all the
// session's own, so git's ownership check passes on its own, and it keeps
// refusing a .git another uid swapped in (the worktree is group-writable).
func userSessionWorkspaceEnvPairs(workspace string) [][2]string {
	if workspace == "" {
		return nil
	}
	return [][2]string{{userSessionWorkspaceEnv, workspace}}
}

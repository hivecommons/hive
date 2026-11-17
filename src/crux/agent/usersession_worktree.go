package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-session git worktrees of the shared workspace clone, and the lock of
// that clone's git config and hooks (#18588). See usersession_workspace.go for
// the layout; everything here that looks inside the clone or a worktree runs
// as root or as the session uid, never as the hive's own uid, which is
// outside the sessions group.

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

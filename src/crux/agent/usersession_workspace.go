package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// The per-hive session workspace. A session is a person using a CLI the way
// they would on a laptop: inside a checkout of the project that has been
// `spektacular init <agent>`-ed, so the CLI finds .spektacular/ and the spek
// skills. The hive keeps ONE such checkout per repository:
//
//	<workspace root>/<org>/<repo>    root:<sessions gid> 2770, files g+rw
//
// where the workspace root defaults to <sandbox root>/workspace (hive-owned,
// 0711, like the sandbox root itself). Every session of the hive shares that
// checkout: two sessions editing the same files are in exactly the position
// of two people on one checkout, and that sharing is the point (a "jam"). The
// checkout is cloned once, by the hive, with a short-lived token in a
// one-shot credential helper that never reaches the clone's config or
// remotes, then handed to the sessions group; the remote stays the plain
// https URL and no session ever holds a hive credential, so pushing is the
// person's own business with their own login. `git fetch` is never automatic.
//
// `spektacular init <vendor>` runs once per vendor per workspace, as the
// starting session's uid, only when the vendor is one spek supports and the
// binary is installed; a failure or an absent binary is logged and the
// session still starts. Clone and init are serialised by workspaceMu.
const (
	// userSessionWorkspaceDirEnv relocates the workspace root (default
	// <HIVE_SESSIONS_SANDBOX_DIR>/workspace).
	userSessionWorkspaceDirEnv  = "HIVE_SESSIONS_WORKSPACE_DIR"
	userSessionWorkspaceDirName = "workspace"
	// userSessionWorkspaceParentMode is the mode of the hive-owned root and
	// <org> directories: traverse-only, like the sandbox root.
	userSessionWorkspaceParentMode = 0o711
	// userSessionWorkspaceCloneTimeout bounds the one-time clone.
	userSessionWorkspaceCloneTimeout = 10 * time.Minute
	// userSessionSpekInitTimeout bounds one `spektacular init`.
	userSessionSpekInitTimeout = 2 * time.Minute
	// userSessionWorkspaceStateDirName, under the sessions data dir, holds the
	// hive-owned "init ran" markers (<org>/<repo>/<vendor>) outside the
	// checkout so they never show up in git status.
	userSessionWorkspaceStateDirName = "workspace-init"
	userSessionWorkspaceStateDirMode = 0o700
	userSessionWorkspaceStateMode    = 0o600
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
// workspace as the session uid. $1 is the workspace, $2 the session HOME, $3
// the spektacular binary, $4 the vendor.
const userSessionSpekInitScript = `set -e; cd -- "$1"; HOME=$2; export HOME; exec "$3" init "$4"`

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

// WorkspaceDir is the path sessions of the given "org/repo" open in, or ""
// when repo is empty or malformed. It is the same whether or not the clone
// exists yet; Start creates it.
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

// ensureWorkspace returns the workspace for spec.WorkspaceRepo, cloning it on
// first use and running spek init for the vendor once. The sandbox (HOME) of
// the session must already exist. A clone failure is returned: a session that
// silently opened somewhere else would not be the workspace the record names.
func (r *UserSessionRuntime) ensureWorkspace(spec crux.UserSessionSpec, uid int) (string, error) {
	org, name, err := splitUserSessionRepo(spec.WorkspaceRepo)
	if err != nil {
		return "", err
	}
	ws := userSessionWorkspaceDir(org, name)
	r.workspaceMu.Lock()
	defer r.workspaceMu.Unlock()
	if _, err := os.Lstat(ws); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("checking session workspace %s: %w", ws, err)
		}
		if err := r.cloneWorkspace(org, name, ws); err != nil {
			return "", fmt.Errorf("preparing session workspace: %w", err)
		}
		r.logger.Info("session workspace cloned", "repo", org+"/"+name, "path", ws)
	}
	r.spekInit(spec, uid, ws, org, name)
	return ws, nil
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
		if out, err := r.privRun(userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", staging); err != nil {
			return outputErr("removing stale workspace staging dir", err, out)
		}
	}
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
	if out, err := r.privRun(userSessionRootSpec, nil, "sh", "-c", userSessionWorkspaceOwnScript, "sh", staging, ws, fmt.Sprint(gid)); err != nil {
		return outputErr("handing workspace to the sessions group", err, out)
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

// userSessionSpekInitMarker is the hive-owned file recording that
// `spektacular init <vendor>` succeeded in the workspace.
func userSessionSpekInitMarker(org, name, vendor string) string {
	return filepath.Join(userSessionsDir(), userSessionWorkspaceStateDirName, org, name, vendor)
}

// spekInit runs `spektacular init <vendor>` in ws as the session uid, once per
// vendor per workspace. Nothing here fails the session: an unsupported vendor
// is skipped silently, a missing binary is logged at info, a failed init at
// warn (and retried on the next start, since no marker is written).
func (r *UserSessionRuntime) spekInit(spec crux.UserSessionSpec, uid int, ws, org, name string) {
	if !userSessionSpekAgents[spec.Vendor] {
		return
	}
	bin := strings.TrimSpace(spec.SpektacularBinary)
	if bin == "" {
		bin = userSessionSpekDefaultBinary
	}
	path, err := r.lookPath(bin)
	if err != nil {
		r.logger.Info("session workspace: spektacular is not installed; the workspace gets no spek skills", "binary", bin, "repo", org+"/"+name)
		return
	}
	marker := userSessionSpekInitMarker(org, name, spec.Vendor)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionSpekInitTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), nil, "sh", "-c", userSessionSpekInitScript, "sh", ws, userSessionHome(spec.ID), path, spec.Vendor)
	if err != nil {
		r.logger.Warn("session workspace: spektacular init failed; the session starts without spek skills", "vendor", spec.Vendor, "repo", org+"/"+name, "error", outputErr("spektacular init", err, out))
		return
	}
	if err := os.MkdirAll(filepath.Dir(marker), userSessionWorkspaceStateDirMode); err == nil {
		err = os.WriteFile(marker, []byte(r.now().UTC().Format(time.RFC3339)+"\n"), userSessionWorkspaceStateMode)
	}
	if err != nil {
		r.logger.Warn("session workspace: recording spektacular init failed; it will run again next start", "vendor", spec.Vendor, "error", err)
	}
	r.logger.Info("session workspace: spektacular init ran", "vendor", spec.Vendor, "repo", org+"/"+name, "path", ws)
}

// userSessionWorkspaceEnvPairs are the extra session variables for a session
// opened in a workspace. The checkout is root-owned (so a session cannot open
// it to other uids) and git refuses to work in a directory owned by another
// user unless it is marked safe; the environment form of that config applies
// to every git the CLI spawns without writing any file.
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

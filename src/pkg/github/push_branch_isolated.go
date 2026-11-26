package github

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The push itself runs from a hive-owned GIT_DIR (hivecommons/hive#19959).
//
// Refusing a redirecting config (pushBranchConfigRefusal) alone leaves a race:
// the agent still owns its checkout and can rewrite .git/config, or a file it
// includes, between the relay's listing and the push, and every git process
// re-reads it. So the push never reads the agent's repository at all, only
// its object store: git runs with GIT_DIR set to a fresh directory the hive
// writes (a minimal config, HEAD and the one branch ref, at the sha the relay
// already resolved) and GIT_OBJECT_DIRECTORY pointing at the checkout's
// objects. With system and global config also off, the only config the push
// reads is the hive's own plus the command-scope pins. The shallow list, when
// the checkout has one, is copied so a shallow clone still pushes.

// pushBranchRepoInfoArgs ask git for the checkout's common git dir (where
// the objects live, also for a linked worktree) and its object format, one
// per line.
var pushBranchRepoInfoArgs = []string{"rev-parse", "--path-format=absolute", "--git-common-dir", "--show-object-format"}

// pushBranchShallowMaxBytes caps the shallow list copied from the checkout.
// One line per shallow commit; a real list is far smaller.
const pushBranchShallowMaxBytes = 8 << 20

// pushBranchIsolatedDirMode keeps the hive-owned GIT_DIR private to the hive.
const pushBranchIsolatedDirMode = 0o700

// pushBranchIsolatedFileMode is the mode of the files written into it.
const pushBranchIsolatedFileMode = 0o600

// pushBranchSHAPattern is the shape of the object id written into the
// hive-owned ref: hex only, so it can never carry a newline or a path.
var pushBranchSHAPattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// pushBranchIsolatedGitDir builds the hive-owned GIT_DIR for pushing branch
// at sha from a checkout whose rev-parse output (pushBranchRepoInfoArgs) is
// repoInfo. It returns the directory, the environment to add to the push
// (after pushBranchGitEnv), and a cleanup that removes the directory. When
// ownerUID > 0 the checkout's object store must be owned by that UID, the
// same rule validatePushBranchDir applies to the checkout itself.
func pushBranchIsolatedGitDir(repoInfo, branch, sha string, ownerUID int) (string, []string, func(), error) {
	lines := strings.Split(strings.TrimSpace(repoInfo), "\n")
	if len(lines) != 2 {
		return "", nil, nil, fmt.Errorf("unexpected rev-parse output for the checkout's git dir: %q", firstOutputLines(repoInfo, 3))
	}
	commonDir, format := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if !filepath.IsAbs(commonDir) {
		return "", nil, nil, fmt.Errorf("checkout git dir %q is not absolute", commonDir)
	}
	if format != "sha1" && format != "sha256" {
		return "", nil, nil, fmt.Errorf("checkout object format %q is not supported", format)
	}
	if !pushBranchSHAPattern.MatchString(sha) {
		return "", nil, nil, fmt.Errorf("resolved commit %q is not an object id", sha)
	}
	objects := filepath.Join(commonDir, "objects")
	fi, err := os.Lstat(objects)
	if err != nil {
		return "", nil, nil, fmt.Errorf("checkout object store: %w", err)
	}
	if !fi.IsDir() {
		return "", nil, nil, fmt.Errorf("checkout object store %q is not a directory", objects)
	}
	if ownerUID > 0 {
		if owner := fileOwnerUID(fi); owner != ownerUID {
			return "", nil, nil, fmt.Errorf("checkout object store %q is owned by uid %d, not by the requesting agent (uid %d)", objects, owner, ownerUID)
		}
	}
	shallow, _, err := readUntrustedFile(filepath.Join(commonDir, "shallow"), pushBranchShallowMaxBytes)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil, fmt.Errorf("checkout shallow list: %w", err)
	}

	dir, err := os.MkdirTemp("", "hive-push-*")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if format == "sha256" {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = sha256\n"
	}
	files := map[string]string{
		"config":               config,
		"HEAD":                 "ref: refs/heads/" + branch + "\n",
		"refs/heads/" + branch: sha + "\n",
	}
	if len(shallow) > 0 {
		files["shallow"] = string(shallow)
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), pushBranchIsolatedDirMode); err != nil {
			cleanup()
			return "", nil, nil, err
		}
		if err := os.WriteFile(path, []byte(content), pushBranchIsolatedFileMode); err != nil {
			cleanup()
			return "", nil, nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "objects"), pushBranchIsolatedDirMode); err != nil {
		cleanup()
		return "", nil, nil, err
	}
	env := []string{
		"GIT_DIR=" + dir,
		"GIT_OBJECT_DIRECTORY=" + objects,
	}
	return dir, env, cleanup, nil
}

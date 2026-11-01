package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stripTrailingBlankLines removes any blank line(s) trailing the final
// newline of each changed, still-present, textual file, leaving exactly one
// trailing newline. It reports whether it amended HEAD.
//
// Scope is deliberately narrow: a file with no trailing newline at all is
// left untouched (that is a different, less universally-enforced style rule,
// not the "...\n\n" defect #5116 reports), a file already ending in exactly
// one newline is untouched, and a file containing a NUL byte in its first 8KB
// — the same binary heuristic git itself uses — is never rewritten as text.
// Only files still present on disk are considered — a changed file that was
// deleted has nothing to normalise.
//
// The content normalised is HEAD's blob (`git show HEAD:<path>`), never the
// working-tree file, so uncommitted edits to a touched path cannot be folded
// into the amended commit. The normalised blob is written over the
// working-tree copy only for as long as `git add` and `git commit --amend
// --only` need to record it: a file the agent had edited since HEAD is
// snapshotted first and put back on disk — whether or not the amend
// succeeds — so no uncommitted edit is lost. A file still identical to HEAD
// is simply left normalised, keeping the workspace clean after the amend.
func (b *Broker) stripTrailingBlankLines(ctx context.Context, files []string) (amended bool, err error) {
	var touched []string
	var edited []workingTreeSnapshot
	defer func() {
		for _, snap := range edited {
			if werr := os.WriteFile(snap.abs, snap.data, snap.perm); werr != nil {
				err = errors.Join(err, fmt.Errorf("restoring uncommitted edit to %s: %w", snap.rel, werr))
			}
		}
	}()
	for _, rel := range files {
		abs := filepath.Join(b.Workspace, rel)
		// Lstat, not Stat: a committed symlink is a path the agent chose, and
		// following it would read and rewrite whatever hive-owned file it
		// points at. Only a regular file inside the tree is normalised.
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue // deleted, a directory entry from a rename, or a symlink — nothing to normalise
		}
		data, err := b.git(ctx, "show", "HEAD:"+rel)
		if err != nil {
			return false, fmt.Errorf("reading %s from HEAD: %w", rel, err)
		}
		if looksBinary(data) {
			continue
		}
		normalized, changed := trimTrailingBlankLines(data)
		if !changed {
			continue
		}
		onDisk, err := os.ReadFile(abs)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", rel, err)
		}
		if !bytes.Equal(onDisk, data) {
			edited = append(edited, workingTreeSnapshot{rel: rel, abs: abs, data: onDisk, perm: info.Mode().Perm()})
		}
		if err := os.WriteFile(abs, normalized, info.Mode().Perm()); err != nil {
			return false, fmt.Errorf("writing %s: %w", rel, err)
		}
		touched = append(touched, rel)
	}
	if len(touched) == 0 {
		return false, nil
	}
	addArgs := append([]string{"add", "--"}, touched...)
	if _, err := b.git(ctx, addArgs...); err != nil {
		return false, err
	}
	// --only keeps the agent's staged-but-uncommitted paths out of the amend.
	amendArgs := append([]string{"commit", "--amend", "--no-edit", "--only", "--"}, touched...)
	if _, err := b.git(ctx, amendArgs...); err != nil {
		if isEmptyAmendError(err) {
			return false, errors.New("pushbroker: refusing to push a commit made empty by broker normalisation; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch")
		}
		return false, err
	}
	if b.Logger != nil {
		b.Logger.Info("pushbroker normalised trailing blank lines", "repo", b.Repo, "branch", b.Branch, "files", touched)
	}
	return true, nil
}

// workingTreeSnapshot is an agent's uncommitted edit to a file that
// stripTrailingBlankLines temporarily overwrites with HEAD's normalised blob,
// kept so the edit can be put back on disk once the amend has run.
type workingTreeSnapshot struct {
	rel  string
	abs  string
	data []byte
	perm os.FileMode
}

func isEmptyAmendError(err error) bool {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	return strings.Contains(msg, "would make") && strings.Contains(msg, "it empty")
}

// looksBinary reports whether data appears to be non-text, using the same
// "NUL byte in a leading sample" heuristic git itself applies (see git's
// buffer_is_binary), so this classifies files the same way `git diff` would
// without shelling out to ask it.
func looksBinary(data []byte) bool {
	const sample = 8000
	if len(data) > sample {
		data = data[:sample]
	}
	return bytes.IndexByte(data, 0) != -1
}

// trimTrailingBlankLines collapses one-or-more blank lines at end-of-file
// down to a single trailing newline. It leaves data with no trailing newline
// untouched entirely — that is not the defect being fixed here.
func trimTrailingBlankLines(data []byte) ([]byte, bool) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return data, false
	}
	trimmed := bytes.TrimRight(data, "\n")
	// TrimRight on an all-newline file would strip everything; that is not a
	// realistic agent-authored source file, but guard it anyway rather than
	// emit an empty file.
	want := append(append([]byte(nil), trimmed...), '\n')
	if len(trimmed) == 0 {
		want = []byte("\n")
	}
	if bytes.Equal(want, data) {
		return data, false
	}
	return want, true
}

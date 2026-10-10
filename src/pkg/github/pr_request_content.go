package github

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const githubCompareFileLimit = 300

// prRequestMaxBaseDriftCommits caps how far a candidate head may sit BEHIND
// its PR base at open time. A working branch cut from the base's own tip is
// behind by however many commits landed since the cut — minutes to hours on
// this path, tens at the outside. A branch cut from a DIFFERENT branch (the
// repository default instead of the PR target, hivecommons/hive#6807) is
// behind by the full divergence between the two lines — observed at 546
// commits, producing 170+-file unmergeable PRs whose diff is mostly the
// target branch's own work rendered as deletions. Opening such a PR silently
// is strictly worse than failing the request: nobody can review it, and
// merging it would revert the target. The limit converts that mistake into a
// loud agent-side rejection with the re-cut instructions in the error.
const prRequestMaxBaseDriftCommits = 100

// prBaseDriftError is a permanent policy mismatch, like
// prContentMetadataError: no change to the request metadata can make a
// wrongly-cut branch mergeable — only re-cutting the branch can.
type prBaseDriftError struct {
	base     string
	head     string
	behindBy int
}

func (e *prBaseDriftError) Error() string {
	return fmt.Sprintf(
		"head %q is %d commits behind base %q (limit %d) — the working branch appears to be cut from a different branch than the PR target; re-create it from the target tip ('git fetch origin %s && git checkout -b <branch> origin/%s'), re-apply your commits, and push again",
		e.head, e.behindBy, e.base, prRequestMaxBaseDriftCommits, e.base, e.base)
}

func prBaseDriftReason(err error) (string, bool) {
	var driftErr *prBaseDriftError
	if !errors.As(err, &driftErr) {
		return "", false
	}
	return driftErr.Error(), true
}

var compareHunkRE = regexp.MustCompile(`^@@ -(?:\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// prContentMetadataError is a permanent policy mismatch: changing the request
// metadata cannot make the candidate branch safe, but changing the branch can.
// Forge/API errors remain ordinary errors so the watcher retries them.
type prContentMetadataError struct {
	file string
	line int
	kind string
}

func (e *prContentMetadataError) Error() string {
	return fmt.Sprintf("internal %s metadata in %s:%d; attribution and run metadata belong in the PR body or commit trailer, not committed files", e.kind, e.file, e.line)
}

func prContentMetadataReason(err error) (string, bool) {
	var metadataErr *prContentMetadataError
	if !errors.As(err, &metadataErr) {
		return "", false
	}
	return metadataErr.Error(), true
}

// validatePRRequestContent checks only lines added by the candidate branch.
// Existing repository prose and deleted metadata do not block a cleanup PR,
// and neither does an edit of a line that already carried the same marker
// (see addedInternalMetadata).
func (c *Client) validatePRRequestContent(ctx context.Context, req PRRequest) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}

	owner, repo := c.splitRepo(strings.TrimSpace(req.Repo))
	base := strings.TrimSpace(req.Base)
	if base == "" {
		resolved, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("validating PR content metadata: %w", err)
		}
		base = resolved
	}
	head := strings.TrimSpace(req.Head)
	comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		// #5343: a 404 here means the head ref is not on the remote, which on
		// this path almost always means the agent's PUSH failed to
		// authenticate. Report that cause instead of the downstream symptom —
		// the raw error sends an operator to investigate branch creation.
		return fmt.Errorf("validating PR content metadata: %w",
			c.diagnoseCompareFailure(ctx, owner, repo, base, head, err))
	}
	if comparison == nil {
		return fmt.Errorf("validating PR content metadata in %s/%s diff %s...%s: GitHub returned an empty comparison", owner, repo, base, head)
	}
	// Base-drift gate (hivecommons/hive#6807), checked BEFORE the file-count
	// cap below: a branch cut from the wrong line usually also blows past the
	// 300-file scan limit, and that error is retried as transient forever,
	// while this one is a permanent rejection with the actual cause.
	if behind := comparison.GetBehindBy(); behind > prRequestMaxBaseDriftCommits {
		return &prBaseDriftError{base: base, head: head, behindBy: behind}
	}
	// GitHub exposes changed files only on the first compare page and caps that
	// list at 300. Do not claim a clean scan when later files may be invisible.
	if len(comparison.Files) >= githubCompareFileLimit {
		return fmt.Errorf("validating PR content metadata in %s/%s diff %s...%s: comparison reached GitHub's %d-file scan limit", owner, repo, base, head, githubCompareFileLimit)
	}
	for _, file := range comparison.Files {
		if file == nil || file.GetPatch() == "" {
			continue
		}
		line, kind, found := addedInternalMetadata(file.GetPatch())
		if found {
			return &prContentMetadataError{file: file.GetFilename(), line: line, kind: kind}
		}
	}
	return nil
}

// attributionMarker classifies one patch line body (without its +/- prefix).
// It returns the marker kind and a normalized key identifying the marker, or
// an empty kind when the line carries none. Marker strings are assembled from
// fragments so this detector's own source is not itself a match.
func attributionMarker(body string) (kind, key string) {
	filedPrefix := strings.ToLower("Filed" + " by ")
	agentSuffix := " agent (acmm"
	hivePrefix := strings.ToLower("hive" + ":")

	lower := strings.ToLower(strings.TrimSpace(body))
	if filed := strings.Index(lower, filedPrefix); filed >= 0 {
		rest := lower[filed+len(filedPrefix):]
		if agent := strings.Index(rest, agentSuffix); agent >= 0 {
			return "agent attribution", rest[:agent+len(agentSuffix)]
		}
	}
	trimmed := strings.TrimSpace(strings.TrimLeft(lower, "—-"))
	if strings.HasPrefix(trimmed, hivePrefix) {
		return "hive run", hivePrefix
	}
	return "", ""
}

// addedInternalMetadata scans a unified patch and returns the new-file line of
// the first leaked attribution marker. A marker on an added line is tolerated
// when the same marker (kind and key) is also on a removed line of the same
// patch: that is an edit of a line that already carried the marker on the
// base (e.g. a forward-merge carrying a wording change), not a new leak. A
// marker that is only added is still reported.
func addedInternalMetadata(patch string) (line int, kind string, found bool) {
	removed := map[string]bool{}
	inHunk := false
	for _, text := range strings.Split(patch, "\n") {
		if compareHunkRE.MatchString(text) {
			inHunk = true
			continue
		}
		if !inHunk || text == "" || text[0] != '-' || strings.HasPrefix(text, "---") {
			continue
		}
		if k, key := attributionMarker(text[1:]); k != "" {
			removed[k+"\x00"+key] = true
		}
	}

	newLine := 0
	inHunk = false
	scanner := bufio.NewScanner(strings.NewReader(patch))
	for scanner.Scan() {
		text := scanner.Text()
		if match := compareHunkRE.FindStringSubmatch(text); match != nil {
			newLine, _ = strconv.Atoi(match[1])
			inHunk = true
			continue
		}
		if !inHunk || text == "" {
			continue
		}
		switch text[0] {
		case '+':
			if strings.HasPrefix(text, "+++") {
				continue
			}
			if k, key := attributionMarker(text[1:]); k != "" && !removed[k+"\x00"+key] {
				return newLine, k, true
			}
			newLine++
		case '-':
			// Removed lines do not exist in the candidate tree.
		case ' ':
			newLine++
		case '\\':
			// "No newline at end of file" marker.
		}
	}
	return 0, "", false
}

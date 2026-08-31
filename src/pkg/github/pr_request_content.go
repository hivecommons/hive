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
// Existing repository prose and deleted metadata do not block a cleanup PR.
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

// addedInternalMetadata scans a unified patch and returns the new-file line of
// the first leaked attribution marker. Marker strings are assembled from
// fragments so this detector's own source is not itself a match.
func addedInternalMetadata(patch string) (line int, kind string, found bool) {
	filedPrefix := strings.ToLower("Filed" + " by ")
	hivePrefix := strings.ToLower("hive" + ":")
	newLine := 0
	inHunk := false

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
			added := strings.TrimSpace(text[1:])
			lower := strings.ToLower(added)
			if filed := strings.Index(lower, filedPrefix); filed >= 0 &&
				strings.Contains(lower[filed+len(filedPrefix):], " agent (acmm") {
				return newLine, "agent attribution", true
			}
			trimmed := strings.TrimSpace(strings.TrimLeft(added, "—-"))
			if strings.HasPrefix(strings.ToLower(trimmed), hivePrefix) {
				return newLine, "hive run", true
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

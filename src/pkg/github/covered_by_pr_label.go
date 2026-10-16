package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
)

// coveredByPRLabelState memoises, per "owner/repo", that the covered-by-PR
// label exists under its current spelling, so the per-issue claim sync does
// not re-check (or re-attempt the rename) on every enumeration tick.
type coveredByPRLabelState struct {
	mu   sync.Mutex
	done map[string]struct{}
}

func (s *coveredByPRLabelState) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[key]
	return ok
}

func (s *coveredByPRLabelState) mark(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = map[string]struct{}{}
	}
	s.done[key] = struct{}{}
}

// HasCoveredByPRLabel reports whether labels carry the covered-by-PR label
// under either its current or legacy spelling.
func HasCoveredByPRLabel(labels []string) bool {
	return issueHasLabel(labels, CoveredByPRLabel) || issueHasLabel(labels, LegacyCoveredByPRLabel)
}

// removeCoveredByPRLabel strips both spellings from an in-memory label set.
func removeCoveredByPRLabel(labels []string) []string {
	return removeIssueLabel(removeIssueLabel(labels, CoveredByPRLabel), LegacyCoveredByPRLabel)
}

// EnsureCoveredByPRLabel makes sure repo has the covered-by-PR label under
// its current spelling (hivecommons/hive#11888). Resolution order, each a
// single API call and memoised per repo for the life of the process:
//
//  1. the current label already exists → nothing to do;
//  2. the legacy `hive/covered-by-pr` label exists → rename it in place
//     (PATCH /repos/{o}/{r}/labels/{old} with new_name), which carries the
//     label across every issue that holds it without a per-issue write;
//  3. neither exists → create the current label.
//
// A failed rename falls back to creating the new label so the sync can still
// proceed; the legacy label is then left for a later sweep to rename.
func (c *Client) EnsureCoveredByPRLabel(ctx context.Context, repo string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" {
		return fmt.Errorf("EnsureCoveredByPRLabel: repo is required")
	}
	key := strings.ToLower(owner + "/" + name)
	if c.coveredByPRLabel.has(key) {
		return nil
	}
	ctx = WithRESTCaller(ctx, "hive:ensure_covered_by_pr_label")
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, name, CoveredByPRLabel); err == nil {
		c.coveredByPRLabel.mark(key)
		return nil
	} else if !isLabelNotFound(err) {
		return err
	}
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, name, LegacyCoveredByPRLabel); err == nil {
		editErr := c.renameLabel(ctx, owner, name, LegacyCoveredByPRLabel, CoveredByPRLabel, CoveredByPRLabelColor, CoveredByPRLabelDescription)
		if editErr == nil {
			c.logger.Info("renamed legacy covered-by-pr label in place",
				slog.String("repo", owner+"/"+name), slog.String("from", LegacyCoveredByPRLabel), slog.String("to", CoveredByPRLabel))
			c.coveredByPRLabel.mark(key)
			return nil
		}
		c.logger.Warn("renaming legacy covered-by-pr label failed, creating the new label instead",
			slog.String("repo", owner+"/"+name), slog.String("error", editErr.Error()))
	} else if !isLabelNotFound(err) {
		return err
	}
	if err := c.EnsureIssueLabel(ctx, repo, CoveredByPRLabel, CoveredByPRLabelColor, CoveredByPRLabelDescription); err != nil {
		return err
	}
	c.coveredByPRLabel.mark(key)
	return nil
}

// renameLabel issues PATCH /repos/{owner}/{repo}/labels/{old} with new_name.
// go-github's Label type has no new_name field, so the body is built by hand.
func (c *Client) renameLabel(ctx context.Context, owner, repo, oldName, newName, color, description string) error {
	path := fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, url.PathEscape(oldName))
	body := map[string]string{"new_name": newName, "color": color, "description": description}
	req, err := c.client.NewRequest(http.MethodPatch, path, body)
	if err != nil {
		return err
	}
	_, err = c.client.Do(ctx, req, nil)
	return err
}

func isLabelNotFound(err error) bool {
	ghErr, ok := err.(*gh.ErrorResponse)
	return ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

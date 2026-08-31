package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// prRequestPolicyError is a permanent mismatch between a PR's claim and its
// diff. It is distinct from forge/API errors, which the watcher should retry.
type prRequestPolicyError struct {
	reason string
}

func (e *prRequestPolicyError) Error() string { return e.reason }

func prRequestPolicyReason(err error) (string, bool) {
	var policyErr *prRequestPolicyError
	if !errors.As(err, &policyErr) {
		return "", false
	}
	return policyErr.reason, true
}

type titleArtifactRule struct {
	name       string
	titleMatch *regexp.Regexp
	fileMatch  func(string) bool
}

var titleArtifactRules = []titleArtifactRule{
	{name: "workflow", titleMatch: regexp.MustCompile(`(?i)\bworkflows?\b`), fileMatch: isWorkflowFile},
	{name: "test", titleMatch: regexp.MustCompile(`(?i)\btests?\b`), fileMatch: isTestFile},
	{name: "migration", titleMatch: regexp.MustCompile(`(?i)\bmigrations?\b`), fileMatch: isMigrationFile},
}

var uncheckedTaskItemRE = regexp.MustCompile(`(?m)^\s*[-*+]\s*\[\s\]`)

// validatePRRequestClaims checks objective artifact claims against the compare
// file list and downgrades unsafe closing references on structurally incomplete
// issues. It returns the title/body to send to GitHub.
func (c *Client) validatePRRequestClaims(ctx context.Context, req PRRequest) (string, string, error) {
	if c == nil || c.client == nil {
		return "", "", ErrNoGitHubClient
	}

	owner, repo := c.prRequestRepo(req.Repo)
	defaultRepo := owner + "/" + repo
	title, body := req.Title, req.Body

	var claimedArtifacts []titleArtifactRule
	for _, rule := range titleArtifactRules {
		if rule.titleMatch.MatchString(title) {
			claimedArtifacts = append(claimedArtifacts, rule)
		}
	}
	if len(claimedArtifacts) > 0 {
		base := strings.TrimSpace(req.Base)
		if base == "" {
			resolved, err := c.DefaultBranch(ctx, owner, repo)
			if err != nil {
				return "", "", fmt.Errorf("validating PR title artifacts: %w", err)
			}
			base = resolved
		}
		comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, strings.TrimSpace(req.Head), nil)
		if err != nil {
			// #5343: same diagnosis as the content gate — an unpushed branch
			// must be reported as a push-auth failure, not as a bad title.
			return "", "", fmt.Errorf("validating PR title artifacts: %w",
				c.diagnoseCompareFailure(ctx, owner, repo, base, strings.TrimSpace(req.Head), err))
		}
		if comparison == nil {
			return "", "", fmt.Errorf("validating PR title against %s/%s diff %s...%s: GitHub returned an empty comparison", owner, repo, base, req.Head)
		}
		for _, rule := range claimedArtifacts {
			matched := false
			for _, file := range comparison.Files {
				if file != nil && rule.fileMatch(file.GetFilename()) {
					matched = true
					break
				}
			}
			if !matched {
				return "", "", &prRequestPolicyError{reason: fmt.Sprintf("title claims %s but diff contains no %s file", rule.name, rule.name)}
			}
		}
	}

	refs := ParseClaimedIssues(title+"\n"+body, defaultRepo)
	if len(refs) == 0 {
		return title, body, nil
	}

	downgrade := make(map[string]string)
	for _, ref := range refs {
		refOwner, refRepo := c.prRequestRepo(ref.Repo)
		issue, _, err := c.client.Issues.Get(ctx, refOwner, refRepo, ref.Issue)
		if err != nil {
			return "", "", fmt.Errorf("validating closing reference %s#%d: %w", ref.Repo, ref.Issue, err)
		}
		if reason := incompleteIssueReason(issue); reason != "" {
			key := claimKey(strings.ToLower(refOwner+"/"+refRepo), ref.Issue)
			downgrade[key] = reason
			c.logger.Warn("pr-request watcher: downgraded closing reference to Refs",
				slog.String("repo", refOwner+"/"+refRepo), slog.Int("issue", ref.Issue),
				slog.String("reason", reason), slog.String("head", req.Head))
		}
	}
	if len(downgrade) == 0 {
		return title, body, nil
	}
	return downgradeClosingReferences(title, defaultRepo, downgrade), downgradeClosingReferences(body, defaultRepo, downgrade), nil
}

func (c *Client) prRequestRepo(repo string) (string, string) {
	owner := c.org
	if parts := strings.SplitN(strings.TrimSpace(repo), "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return owner, strings.TrimSpace(repo)
}

func incompleteIssueReason(issue *gh.Issue) string {
	if issue == nil {
		return "issue metadata is empty"
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		name := label.GetName()
		labels = append(labels, name)
		if strings.EqualFold(name, "epic") || strings.EqualFold(name, "tracker") || strings.EqualFold(name, "meta-tracker") {
			return "issue is labeled as a tracker or epic"
		}
	}
	title := strings.ToLower(strings.TrimSpace(issue.GetTitle()))
	if strings.HasPrefix(title, "[epic]") || strings.HasPrefix(title, "[tracker]") {
		return "issue title marks it as a tracker or epic"
	}
	if IsTrackerIssue(issue.GetTitle(), labels, issue.GetBody()) {
		return "issue is a tracker"
	}
	if uncheckedTaskItemRE.MatchString(issue.GetBody()) {
		return "issue has unchecked task items"
	}
	return ""
}

func downgradeClosingReferences(text, defaultRepo string, downgrade map[string]string) string {
	if text == "" {
		return text
	}
	return claimRefPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := claimRefPattern.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		issue, err := strconv.Atoi(parts[3])
		if err != nil {
			return match
		}
		repo := parts[2]
		if repo == "" {
			repo = defaultRepo
		}
		if _, ok := downgrade[claimKey(strings.ToLower(repo), issue)]; !ok {
			return match
		}
		return "Refs" + match[len(parts[1]):]
	})
}

func isWorkflowFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	if !strings.HasPrefix(filename, ".github/workflows/") {
		return false
	}
	ext := path.Ext(filename)
	return ext == ".yml" || ext == ".yaml"
}

func isTestFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	base := path.Base(filename)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, "_spec") ||
		strings.HasPrefix(base, "test_") || strings.HasPrefix(base, "test-") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || ext == ".bats" {
		return true
	}
	for _, segment := range strings.Split(filename, "/") {
		if segment == "test" || segment == "tests" || segment == "spec" || segment == "specs" || segment == "__tests__" {
			return true
		}
	}
	return false
}

func isMigrationFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	for _, segment := range strings.Split(filename, "/") {
		if segment == "migration" || segment == "migrations" || segment == "migrate" {
			return true
		}
	}
	return false
}

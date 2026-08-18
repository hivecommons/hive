package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// RepairBranchRefreshRequest binds a read-only PR observation to the exact
// Hive-owned repair state. CandidateHeadSHA is permitted only for crash
// recovery after Hive may have completed its exact-lease Git push.
type RepairBranchRefreshRequest struct {
	Repository            string
	RepositoryID          string
	PRNumber              int
	Branch                string
	ExpectedHeadSHA       string
	CandidateHeadSHA      string
	ExpectedBaseBranch    string
	ExpectedBaseSHA       string
	ExpectedChangedFiles  []string
	RepositoryFingerprint string
}

type RepairBranchRefreshObservation struct {
	PRNumber       int      `json:"pr_number"`
	Branch         string   `json:"branch"`
	HeadSHA        string   `json:"head_sha"`
	BaseBranch     string   `json:"base_branch"`
	BaseSHA        string   `json:"base_sha"`
	BaseMoved      bool     `json:"base_moved"`
	MergeableState string   `json:"mergeable_state"`
	ChangedFiles   []string `json:"changed_files"`
}

// InspectRepairPullRequestRefreshExact performs the repository, ownership,
// protection, PR, branch, head, base-branch, and semantic-file checks needed by
// local refresh orchestration. It is deliberately read-only: Hive locally
// prepares and validates a commit before its exact-head lease push.
func (c *Client) InspectRepairPullRequestRefreshExact(ctx context.Context, request RepairBranchRefreshRequest) (RepairBranchRefreshObservation, error) {
	result := RepairBranchRefreshObservation{}
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return result, err
	}
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(request.RepositoryID), 10, 64)
	if err != nil || repositoryID <= 0 || request.PRNumber <= 0 || !strings.HasPrefix(request.Branch, "hive/repair-") ||
		!exactRefreshSHA(request.ExpectedHeadSHA) || (request.CandidateHeadSHA != "" && !exactRefreshSHA(request.CandidateHeadSHA)) ||
		strings.TrimSpace(request.ExpectedBaseBranch) == "" || !exactRefreshSHA(request.ExpectedBaseSHA) || len(request.ExpectedChangedFiles) == 0 ||
		strings.TrimSpace(request.RepositoryFingerprint) == "" {
		return result, fmt.Errorf("exact repository identity, PR, Hive repair branch, head/base, and changed files are required")
	}
	metadata, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return result, fmt.Errorf("verify repository identity before repair branch refresh: %w", err)
	}
	if metadata.GetID() != repositoryID || !strings.EqualFold(metadata.GetFullName(), request.Repository) {
		return result, fmt.Errorf("repository identity changed before repair branch refresh")
	}
	protection, err := c.BranchProtection(ctx, request.Repository, request.ExpectedBaseBranch)
	if err != nil {
		return result, fmt.Errorf("verify protected repair base: %w", err)
	}
	if !protection.Enabled || !protection.Strict || !protection.AdminEnforced || len(protection.RequiredChecks) == 0 {
		return result, fmt.Errorf("repair base branch is not strict, admin-enforced, and check-protected")
	}

	pull, err := c.readExactRefreshPull(ctx, owner, repo, repositoryID, request)
	if err != nil {
		return result, err
	}
	files, err := c.listPullRequestFiles(ctx, owner, repo, request.PRNumber, pull.GetChangedFiles())
	if err != nil {
		return result, err
	}
	if !sameRefreshFiles(files, request.ExpectedChangedFiles) {
		return result, fmt.Errorf("repair PR semantic changed-file set does not match durable Hive state")
	}
	if !exactRefreshSHA(pull.GetBase().GetSHA()) {
		return result, fmt.Errorf("repair pull request returned an invalid protected base SHA")
	}
	return RepairBranchRefreshObservation{
		PRNumber: request.PRNumber, Branch: request.Branch, HeadSHA: pull.GetHead().GetSHA(),
		BaseBranch: pull.GetBase().GetRef(), BaseSHA: pull.GetBase().GetSHA(), BaseMoved: !strings.EqualFold(pull.GetBase().GetSHA(), request.ExpectedBaseSHA),
		MergeableState: pull.GetMergeableState(), ChangedFiles: append([]string(nil), files...),
	}, nil
}

func (c *Client) readExactRefreshPull(ctx context.Context, owner, repo string, repositoryID int64, request RepairBranchRefreshRequest) (*gh.PullRequest, error) {
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, request.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("read repair pull request #%d: %w", request.PRNumber, err)
	}
	head, base := pull.GetHead(), pull.GetBase()
	marker := "<!-- hive-repair: " + strings.TrimSpace(request.RepositoryFingerprint) + " -->"
	allowedHead := strings.EqualFold(head.GetSHA(), request.ExpectedHeadSHA) || request.CandidateHeadSHA != "" && strings.EqualFold(head.GetSHA(), request.CandidateHeadSHA)
	if pull.GetNumber() != request.PRNumber || pull.GetState() != "open" || pull.GetMerged() || !allowedHead ||
		head.GetRef() != request.Branch || base.GetRef() != request.ExpectedBaseBranch {
		return nil, fmt.Errorf("repair pull request state, branch, head, or base branch changed")
	}
	if head.GetRepo().GetID() != repositoryID || base.GetRepo().GetID() != repositoryID ||
		!strings.EqualFold(head.GetRepo().GetFullName(), request.Repository) || !strings.EqualFold(base.GetRepo().GetFullName(), request.Repository) {
		return nil, fmt.Errorf("forked or cross-repository repair pull requests cannot be refreshed")
	}
	if strings.Count(pull.GetBody(), marker) != 1 {
		return nil, fmt.Errorf("repair pull request is missing its exact Hive ownership marker")
	}
	return pull, nil
}

func sameRefreshFiles(left, right []string) bool {
	normalize := func(values []string) []string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "./")
			if value != "" {
				result = append(result, value)
			}
		}
		sort.Strings(result)
		return result
	}
	left, right = normalize(left), normalize(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func exactRefreshSHA(value string) bool {
	if len(value) != 40 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return true
}

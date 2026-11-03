package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type IntegrationExecutionAuthority struct {
	Version         int    `json:"version"`
	Kind            string `json:"kind"`
	Repo            string `json:"repo"`
	Base            string `json:"base"`
	Enabled         bool   `json:"enabled"`
	ExecutorID      string `json:"executor_id"`
	Policy          string `json:"policy"`
	QualificationID string `json:"qualification_id"`
	Model           string `json:"model"`
}

// ReadIntegrationExecutionAuthority is a shared forge observation, not a
// filesystem lease. Epoch expiry never authorizes another executor; installing
// a replacement requires an explicit fenced operator handover.
func (c *Client) ReadIntegrationExecutionAuthority(ctx context.Context, scope config.IntegrationExecutionScope, executorID string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	parts := strings.Split(scope.Repo, "/")
	if !integrationPromotionRepo(scope.Repo) ||
		!strings.HasPrefix(scope.AuthorityRef, "refs/heads/hive-integration-authority/") ||
		!integrationOID(scope.AuthorityCommit) || !integrationProducerDigest(executorID) {
		return false, fmt.Errorf("execution authority enrollment unavailable")
	}
	repository, _, err := c.client.Repositories.Get(ctx, parts[0], parts[1])
	if err != nil {
		return false, err
	}
	if repository.GetID() != scope.RepositoryID || repository.GetFullName() != scope.Repo {
		return false, fmt.Errorf("execution authority repository identity changed")
	}
	ref, _, err := c.client.Git.GetRef(ctx, parts[0], parts[1], strings.TrimPrefix(scope.AuthorityRef, "refs/"))
	if err != nil {
		return false, err
	}
	if ref.GetObject().GetSHA() != scope.AuthorityCommit {
		return false, fmt.Errorf("execution authority epoch changed; observer-only")
	}
	commit, _, err := c.client.Git.GetCommit(ctx, parts[0], parts[1], scope.AuthorityCommit)
	if err != nil {
		return false, err
	}
	if commit.GetSHA() != scope.AuthorityCommit {
		return false, fmt.Errorf("execution authority object unknown")
	}
	var records []string
	for _, line := range strings.Split(commit.GetMessage(), "\n") {
		if strings.HasPrefix(line, "{") {
			records = append(records, line)
		}
	}
	if len(records) != 1 {
		return false, fmt.Errorf("execution authority record ambiguous")
	}
	var authority IntegrationExecutionAuthority
	if err := json.Unmarshal([]byte(records[0]), &authority); err != nil {
		return false, err
	}
	if authority.Version != 2 || authority.Kind != "integration-executor-single-v1" ||
		authority.Model != "designated-single-executor-no-failover" ||
		authority.Repo != scope.Repo || authority.Base != scope.Base || authority.Policy != scope.Policy ||
		authority.QualificationID != scope.QualificationID || authority.ExecutorID != scope.ExecutorID ||
		!authority.Enabled {
		return false, fmt.Errorf("shared designated execution authority not enabled/qualified for this exact scope")
	}
	return executorID == authority.ExecutorID, nil
}

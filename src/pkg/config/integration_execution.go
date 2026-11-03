package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	MaxIntegrationExecutionCandidates = 8
	MaxIntegrationExecutionJobs       = 100
	MaxIntegrationExecutionPriority   = 10
	MaxIntegrationExecutionMinutes    = 10000
	MinIntegrationExecutionInterval   = time.Minute
	MaxIntegrationExecutionInterval   = time.Hour
	MaxIntegrationExecutionWindow     = 24 * time.Hour
)

// IntegrationExecutionConfig is independent of authoring admission. Its zero
// value is OFF; no legacy convergence flag enables a promotion writer.
type IntegrationExecutionConfig struct {
	Mode             string                      `yaml:"mode,omitempty" json:"mode"`
	Scopes           []IntegrationExecutionScope `yaml:"scopes,omitempty" json:"scopes"`
	MaxActive        int                         `yaml:"max_active,omitempty" json:"max_active"`
	MaxRunnerMinutes float64                     `yaml:"max_runner_minutes,omitempty" json:"max_runner_minutes"`
	MaxJobs          int                         `yaml:"max_jobs,omitempty" json:"max_jobs"`
	MaxAttempts      int                         `yaml:"max_attempts,omitempty" json:"max_attempts"`
	ObservationAge   time.Duration               `yaml:"observation_age,omitempty" json:"observation_age"`
	Window           time.Duration               `yaml:"window,omitempty" json:"window"`
	Cooldown         time.Duration               `yaml:"cooldown,omitempty" json:"cooldown"`
}

// IntegrationExecutionScope pins the installed provider/profile and explicit
// designated executor. Native scopes admit/observe only: GitHub retains control.
type IntegrationExecutionScope struct {
	Repo                    string   `yaml:"repo" json:"repo"`
	Base                    string   `yaml:"base" json:"base"`
	Authority               string   `yaml:"authority" json:"authority"`
	RepositoryID            int64    `yaml:"repository_id" json:"repository_id"`
	RequestActorID          int64    `yaml:"request_actor_id" json:"request_actor_id"`
	CandidateActorID        int64    `yaml:"candidate_actor_id" json:"candidate_actor_id"`
	ProducerAppID           int64    `yaml:"producer_app_id" json:"producer_app_id"`
	PromotionAppID          int64    `yaml:"promotion_app_id" json:"promotion_app_id"`
	PromotionInstallationID int64    `yaml:"promotion_installation_id" json:"promotion_installation_id"`
	MaterializerID          int64    `yaml:"materializer_id" json:"materializer_id"`
	CandidateWorkflowID     int64    `yaml:"candidate_workflow_id" json:"candidate_workflow_id"`
	DefaultBranch           string   `yaml:"default_branch" json:"default_branch"`
	Policy                  string   `yaml:"policy" json:"policy"`
	WorkflowTree            string   `yaml:"workflow_tree" json:"workflow_tree"`
	RequiredJobs            []string `yaml:"required_jobs" json:"required_jobs"`
	ExecutorID              string   `yaml:"executor_id" json:"executor_id"`
	AuthorityRef            string   `yaml:"authority_ref" json:"authority_ref"`
	AuthorityCommit         string   `yaml:"authority_commit" json:"authority_commit"`
	QualificationID         string   `yaml:"qualification_id" json:"qualification_id"`
	MaxMembers              int      `yaml:"max_members" json:"max_members"`
	ExpectedJobs            int      `yaml:"expected_jobs" json:"expected_jobs"`
	ExpectedMinutes         float64  `yaml:"expected_minutes" json:"expected_minutes"`
	Purpose                 string   `yaml:"purpose,omitempty" json:"purpose,omitempty"`
	QualifiedBase           string   `yaml:"qualified_base,omitempty" json:"qualified_base,omitempty"`
}

func (c IntegrationExecutionConfig) Effective() IntegrationExecutionConfig {
	if c.Mode == "" {
		c.Mode = IntegrationOff
	}
	return c
}

func executionDigest(value string, length int) bool {
	return len(value) == length && strings.Trim(value, "0123456789abcdef") == "" && strings.Trim(value, "0") != ""
}

func executionBranch(value string) bool {
	if value == "" || value == "@" || strings.HasSuffix(value, ".") ||
		strings.ContainsAny(value, " ~^:?*[\\") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, character := range value {
		if character < ' ' || character == '\x7f' {
			return false
		}
	}
	return true
}

func executionRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, character := range part {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' || strings.ContainsRune("-_.", character) {
				continue
			}
			return false
		}
	}
	return true
}

func (c IntegrationExecutionConfig) Validate() error {
	c = c.Effective()
	if c.Mode != IntegrationOff && c.Mode != IntegrationAdvisory && c.Mode != IntegrationEnforce {
		return fmt.Errorf("integration execution mode must be off, advisory or enforce")
	}
	if c.Mode == IntegrationOff && len(c.Scopes) == 0 {
		if c.MaxActive != 0 || c.MaxJobs != 0 || c.MaxAttempts != 0 || c.MaxRunnerMinutes != 0 ||
			c.ObservationAge != 0 || c.Window != 0 || c.Cooldown != 0 {
			return fmt.Errorf("unscoped OFF execution must use zero bounds; retain explicit scopes and valid bounds when rolling back")
		}
		return nil
	}
	if len(c.Scopes) == 0 || len(c.Scopes) > MaxIntegrationScopes ||
		c.MaxActive < 1 || c.MaxActive > MaxIntegrationExecutionCandidates ||
		c.MaxJobs < 1 || c.MaxJobs > MaxIntegrationExecutionJobs ||
		c.MaxAttempts < 1 || c.MaxAttempts > DefaultIntegrationMaxAttempts ||
		math.IsNaN(c.MaxRunnerMinutes) || math.IsInf(c.MaxRunnerMinutes, 0) || c.MaxRunnerMinutes <= 0 ||
		c.MaxRunnerMinutes > MaxIntegrationExecutionMinutes ||
		c.ObservationAge < MinIntegrationExecutionInterval || c.ObservationAge > MaxIntegrationExecutionInterval ||
		c.Window < MinIntegrationExecutionInterval || c.Window > MaxIntegrationExecutionWindow ||
		c.Cooldown < MinIntegrationExecutionInterval || c.Cooldown > MaxIntegrationExecutionInterval {
		return fmt.Errorf("integration execution requires explicit bounded scopes, concurrency, retry, runner/job budgets and observation intervals")
	}
	seen := map[string]bool{}
	for _, scope := range c.Scopes {
		key := strings.ToLower(scope.Repo) + "#" + scope.Base
		if !executionRepository(scope.Repo) || !executionBranch(scope.Base) ||
			seen[key] || scope.Authority != "hive" && scope.Authority != "native" ||
			scope.MaxMembers < 1 || scope.MaxMembers > MaxIntegrationExecutionCandidates ||
			scope.ExpectedJobs < 1 || scope.ExpectedJobs > c.MaxJobs ||
			math.IsNaN(scope.ExpectedMinutes) || math.IsInf(scope.ExpectedMinutes, 0) ||
			scope.ExpectedMinutes <= 0 || scope.ExpectedMinutes > c.MaxRunnerMinutes {
			return fmt.Errorf("integration execution scope/budget invalid or duplicated")
		}
		seen[key] = true
		if scope.RepositoryID <= 0 || !executionDigest(scope.ExecutorID, 64) ||
			!strings.HasPrefix(scope.AuthorityRef, "refs/heads/hive-integration-authority/") ||
			!executionBranch(strings.TrimPrefix(scope.AuthorityRef, "refs/heads/")) ||
			!executionDigest(scope.AuthorityCommit, 40) {
			return fmt.Errorf("execution admission requires an explicit designated executor and immutable forge epoch, including native admission")
		}
		if scope.Purpose != "" && scope.Purpose != "qualification" {
			return fmt.Errorf("execution purpose must be production (empty) or explicit isolated qualification")
		}
		if scope.Purpose == "qualification" {
			if scope.Authority != "hive" || !strings.HasPrefix(scope.Base, "hive-integration-qualification/") ||
				!executionBranch(scope.QualifiedBase) || scope.QualifiedBase == scope.Base ||
				scope.MaxMembers != 1 || c.MaxActive != 1 {
				return fmt.Errorf("qualification requires an explicitly enrolled isolated branch, production comparison base and single-member single-slot bounds")
			}
		} else if scope.QualifiedBase != "" {
			return fmt.Errorf("production execution cannot select a qualification comparison base")
		}
		if scope.Authority == "native" {
			continue
		}
		if scope.RepositoryID <= 0 || scope.RequestActorID <= 0 || scope.CandidateActorID <= 0 ||
			scope.PromotionAppID <= 0 || scope.PromotionInstallationID <= 0 ||
			scope.ProducerAppID <= 0 || scope.MaterializerID <= 0 || scope.CandidateWorkflowID <= 0 ||
			!executionBranch(scope.DefaultBranch) || !executionDigest(scope.Policy, 64) ||
			!executionDigest(scope.WorkflowTree, 40) || !executionDigest(scope.ExecutorID, 64) ||
			!strings.HasPrefix(scope.AuthorityRef, "refs/heads/hive-integration-authority/") ||
			!executionBranch(strings.TrimPrefix(scope.AuthorityRef, "refs/heads/")) ||
			!executionDigest(scope.AuthorityCommit, 40) || scope.QualificationID == "" ||
			len(scope.RequiredJobs) == 0 || len(scope.RequiredJobs) > scope.ExpectedJobs {
			return fmt.Errorf("hive execution requires qualified installed workflows, designated executor and immutable authority epoch")
		}

		names := map[string]bool{}
		for _, name := range scope.RequiredJobs {
			if strings.TrimSpace(name) == "" || names[name] {
				return fmt.Errorf("integration execution required jobs must be unique nonempty qualified names")
			}
			names[name] = true
		}
	}
	return nil
}

func (c IntegrationExecutionConfig) Scope(repo, base string) (IntegrationExecutionScope, bool) {
	if c.Effective().Mode == IntegrationOff || c.Validate() != nil {
		return IntegrationExecutionScope{}, false
	}

	for _, scope := range c.Scopes {
		if strings.EqualFold(scope.Repo, repo) && scope.Base == base {
			return scope, true
		}
	}
	return IntegrationExecutionScope{}, false
}

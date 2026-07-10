package integrated

import (
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

const (
	PlanSchema   = "hive.setup-plan.v1"
	ConfigSchema = "hive.integrated-config.v1"
)

type Coverage string

const (
	CoverageEssential     Coverage = "essential"
	CoverageStandard      Coverage = "standard"
	CoverageComprehensive Coverage = "comprehensive"
	CoverageCustom        Coverage = "custom"
)

type Automation string

const (
	AutomationAdvisory  Automation = "advisory"
	AutomationIssues    Automation = "issues"
	AutomationRepairPR  Automation = "repair-pr"
	AutomationAutoMerge Automation = "auto-merge"
)

type RepositoryInspection struct {
	DefaultBranch    string            `json:"default_branch"`
	RepositoryID     string            `json:"repository_id,omitempty"`
	Languages        []string          `json:"languages"`
	Frameworks       []string          `json:"frameworks"`
	PackageManagers  []string          `json:"package_managers"`
	TestCommands     [][]string        `json:"test_commands"`
	CIFiles          []string          `json:"ci_files"`
	DeploymentFiles  []string          `json:"deployment_files"`
	BaselineFiles    []string          `json:"baseline_files"`
	HighRiskPaths    []string          `json:"high_risk_paths"`
	BranchProtection bool              `json:"branch_protection"`
	Permissions      map[string]bool   `json:"permissions"`
	Signals          map[string]string `json:"signals"`
}

type SetupPlan struct {
	SchemaVersion   string               `json:"schema_version"`
	GeneratedAt     time.Time            `json:"generated_at"`
	Repository      string               `json:"repository"`
	Coverage        Coverage             `json:"coverage"`
	Automation      Automation           `json:"automation"`
	Provider        string               `json:"provider"`
	ACMMLevel       int                  `json:"acmm_level"`
	MaxActiveIssues int                  `json:"max_active_issues"`
	VisualHive      bool                 `json:"visual_hive"`
	Inspection      RepositoryInspection `json:"inspection"`
	TestingLayers   []string             `json:"testing_layers"`
	FilesToManage   []string             `json:"files_to_manage"`
	RequiredActions []string             `json:"required_actions"`
	Warnings        []string             `json:"warnings"`
	ReadOnly        bool                 `json:"read_only"`
}

type Config struct {
	SchemaVersion         string                `json:"schema_version"`
	Repository            string                `json:"repository"`
	RepositoryID          string                `json:"repository_id,omitempty"`
	DefaultBranch         string                `json:"default_branch"`
	Coverage              Coverage              `json:"coverage"`
	Automation            Automation            `json:"automation"`
	Provider              string                `json:"provider"`
	ProviderCommand       string                `json:"provider_command"`
	ProviderArgs          []string              `json:"provider_args,omitempty"`
	ACMMLevel             int                   `json:"acmm_level"`
	MaxActiveIssues       int                   `json:"max_active_issues"`
	VisualHive            bool                  `json:"visual_hive"`
	VisualHiveRepo        string                `json:"visual_hive_repo"`
	VisualHiveRef         string                `json:"visual_hive_ref"`
	VisualHiveCommand     string                `json:"visual_hive_command"`
	VisualHiveArgs        []string              `json:"visual_hive_args,omitempty"`
	TestCommands          [][]string            `json:"test_commands"`
	AllowedRepairPaths    []string              `json:"allowed_repair_paths"`
	AllowedAutoMergePaths []string              `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk  []automation.RiskTier `json:"allowed_auto_merge_risk"`
	CheckoutDir           string                `json:"checkout_dir"`
	StateDir              string                `json:"state_dir"`
	Paused                bool                  `json:"paused"`
	SetupBranch           string                `json:"setup_branch,omitempty"`
	SetupPRNumber         int                   `json:"setup_pr_number,omitempty"`
	SetupPRURL            string                `json:"setup_pr_url,omitempty"`
	InstalledAt           time.Time             `json:"installed_at"`
	UpdatedAt             time.Time             `json:"updated_at"`
	PreviousVersion       string                `json:"previous_version,omitempty"`
}

type SetupResult struct {
	Plan       SetupPlan `json:"plan"`
	Applied    bool      `json:"applied"`
	Idempotent bool      `json:"idempotent"`
	Config     *Config   `json:"config,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	CommitSHA  string    `json:"commit_sha,omitempty"`
	PRNumber   int       `json:"pr_number,omitempty"`
	PRURL      string    `json:"pr_url,omitempty"`
}

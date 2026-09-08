package hub

// This file holds the heartbeat wire DTOs shared between the SaaS control
// plane (which decodes spoke heartbeats and encodes responses) and the
// spoke-side client, which lives in pkg/hub/spoke. The push loop itself was
// moved to pkg/hub/spoke; only the control-plane-facing types remain here.

import (
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inferencehealth"
	"github.com/hivecommons/hive/pkg/tracing"
)

type AgentSummary struct {
	Name                 string                `json:"name"`
	State                string                `json:"state"`
	Mode                 string                `json:"mode,omitempty"`
	Paused               bool                  `json:"paused,omitempty"`
	PausedTrigger        string                `json:"pausedTrigger,omitempty"`
	PausedReason         string                `json:"pausedReason,omitempty"`
	PausedBy             string                `json:"pausedBy,omitempty"`
	PausedAt             string                `json:"pausedAt,omitempty"`
	NeedsLogin           bool                  `json:"needsLogin,omitempty"`
	QuotaExhausted       bool                  `json:"quotaExhausted,omitempty"`
	SessionMissing       bool                  `json:"sessionMissing,omitempty"`
	StartedAt            string                `json:"startedAt,omitempty"`
	LastActivityAt       string                `json:"lastActivityAt,omitempty"`
	KickIntervalSec      int64                 `json:"kickIntervalSec,omitempty"`
	ExpectedActive       bool                  `json:"expectedActive,omitempty"`
	CanOpenIssue         bool                  `json:"canOpenIssue,omitempty"`
	CanOpenPR            bool                  `json:"canOpenPR,omitempty"`
	CanMerge             bool                  `json:"canMerge,omitempty"`
	Backend              string                `json:"backend,omitempty"`
	Enabled              bool                  `json:"enabled,omitempty"`
	StartBlockedReason   string                `json:"startBlockedReason,omitempty"`
	StartFailureReason   string                `json:"startFailureReason,omitempty"`
	StartFailureCount    int                   `json:"startFailureCount,omitempty"`
	StartFailureLastAt   string                `json:"startFailureLastAt,omitempty"`
	StartBlocked         bool                  `json:"startBlocked,omitempty"`
	StartFailureExitCode *int                  `json:"startFailureExitCode,omitempty"`
	StartFailureSignal   string                `json:"startFailureSignal,omitempty"`
	Restarts             AgentRestartTelemetry `json:"restarts,omitempty"`
}
type AgentRestartTelemetry struct {
	Total         int    `json:"total,omitempty"`
	Last24h       int    `json:"last_24h,omitempty"`
	LastRestartAt string `json:"last_restart_at,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`
	PodRestarts   int    `json:"pod_restarts,omitempty"`
	ResetAt       string `json:"reset_at,omitempty"`
	ResetBy       string `json:"reset_by,omitempty"`
}
type AgentActivity struct {
	Paused               bool
	PausedTrigger        string
	PausedReason         string
	PausedBy             string
	PausedAt             time.Time
	NeedsLogin           bool
	QuotaExhausted       bool
	SessionMissing       bool
	StartedAt            time.Time
	LastActivityAt       time.Time
	KickInterval         time.Duration
	ExpectedActive       bool
	CanOpenIssue         bool
	CanOpenPR            bool
	CanMerge             bool
	Backend              string
	Enabled              bool
	StartBlockedReason   string
	StartFailureReason   string
	StartFailureCount    int
	StartFailureLastAt   time.Time
	StartBlocked         bool
	StartFailureExitCode *int
	StartFailureSignal   string
	Restarts             AgentRestartTelemetry
}

func NewAgentSummary(name, state, mode string, act AgentActivity) AgentSummary {
	as := AgentSummary{
		Name:                 name,
		State:                state,
		Mode:                 mode,
		Paused:               act.Paused,
		PausedTrigger:        act.PausedTrigger,
		PausedReason:         act.PausedReason,
		PausedBy:             act.PausedBy,
		NeedsLogin:           act.NeedsLogin,
		QuotaExhausted:       act.QuotaExhausted,
		SessionMissing:       act.SessionMissing,
		ExpectedActive:       act.ExpectedActive,
		CanOpenIssue:         act.CanOpenIssue,
		CanOpenPR:            act.CanOpenPR,
		CanMerge:             act.CanMerge,
		Backend:              act.Backend,
		Enabled:              act.Enabled,
		Restarts:             act.Restarts,
		StartBlockedReason:   act.StartBlockedReason,
		StartFailureReason:   act.StartFailureReason,
		StartFailureCount:    act.StartFailureCount,
		StartBlocked:         act.StartBlocked,
		StartFailureExitCode: act.StartFailureExitCode,
		StartFailureSignal:   act.StartFailureSignal,
	}
	if !act.PausedAt.IsZero() {
		as.PausedAt = act.PausedAt.UTC().Format(time.RFC3339)
	}
	if !act.StartedAt.IsZero() {
		as.StartedAt = act.StartedAt.UTC().Format(time.RFC3339)
	}
	if !act.LastActivityAt.IsZero() {
		as.LastActivityAt = act.LastActivityAt.UTC().Format(time.RFC3339)
	}
	if !act.StartFailureLastAt.IsZero() {
		as.StartFailureLastAt = act.StartFailureLastAt.UTC().Format(time.RFC3339)
	}
	if act.KickInterval > 0 {
		as.KickIntervalSec = int64(act.KickInterval / time.Second)
	}
	return as
}

type GovernorSummary struct {
	Mode       string `json:"mode"`
	Issues     int    `json:"issues"`
	PRs        int    `json:"prs"`
	WorkSource string `json:"work_source,omitempty"`
}
type ContributorSummary struct {
	Active     int `json:"active"`
	Registered int `json:"registered"`
}
type LeaderboardEntry struct {
	GitHubUsername string `json:"github_username"`
	AvatarURL      string `json:"avatar_url"`
	TrustTier      string `json:"trust_tier"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksFailed    int    `json:"tasks_failed"`
	Active         bool   `json:"active"`
	CurrentTask    string `json:"current_task,omitempty"`
	HiveName       string `json:"hive_name,omitempty"`
}
type HeartbeatClusterHealthReport struct {
	Nodes       []HeartbeatNodeMetric   `json:"nodes"`
	Summary     HeartbeatClusterSummary `json:"summary"`
	GPUSummary  *HeartbeatGPUSummary    `json:"gpu_summary,omitempty"`
	CollectedAt string                  `json:"collected_at"`
}
type HeartbeatNodeMetric struct {
	Name          string   `json:"name"`
	CPUCores      int      `json:"cpu_cores"`
	CPUUsedMillis int64    `json:"cpu_used_millicores"`
	CPUPercent    int      `json:"cpu_percent"`
	MemTotalMB    int64    `json:"mem_total_mb"`
	MemUsedMB     int64    `json:"mem_used_mb"`
	MemPercent    int      `json:"mem_percent"`
	DiskTotalMB   *int64   `json:"disk_total_mb,omitempty"`
	DiskUsedMB    *int64   `json:"disk_used_mb,omitempty"`
	DiskPercent   *int     `json:"disk_percent,omitempty"`
	Pods          int      `json:"pods"`
	PodCapacity   int      `json:"pod_capacity"`
	HiveCount     int      `json:"hive_count,omitempty"`
	Ready         bool     `json:"ready"`
	Conditions    []string `json:"conditions"`
	DiskPressure  bool     `json:"disk_pressure"`
	GPUs          int      `json:"gpus,omitempty"`
	GPUType       string   `json:"gpu_type,omitempty"`
}
type HeartbeatClusterSummary struct {
	TotalNodes            int  `json:"total_nodes"`
	ReadyNodes            int  `json:"ready_nodes"`
	TotalCPUCores         int  `json:"total_cpu_cores"`
	TotalCPUPct           int  `json:"total_cpu_percent"`
	TotalMemGB            int  `json:"total_mem_gb"`
	TotalMemPct           int  `json:"total_mem_percent"`
	TotalDiskGB           *int `json:"total_disk_gb,omitempty"`
	TotalDiskPct          *int `json:"total_disk_percent,omitempty"`
	TotalPods             int  `json:"total_pods"`
	HiveCapacityRemaining *int `json:"hive_capacity_remaining,omitempty"`
}
type HeartbeatGPUSummary struct {
	Total     int      `json:"total"`
	Allocated int      `json:"allocated"`
	Types     []string `json:"types"`
}
type HeartbeatPayload struct {
	HiveID                       string                          `json:"hive_id"`
	Org                          string                          `json:"org"`
	Repos                        []string                        `json:"repos"`
	PrimaryRepo                  string                          `json:"primary_repo"`
	AIAuthor                     string                          `json:"ai_author,omitempty"`
	AIAuthorEffective            string                          `json:"ai_author_effective,omitempty"`
	GitHubAPIURL                 string                          `json:"github_api_url,omitempty"`
	ACMMLevel                    int                             `json:"acmm_level"`
	Agents                       []AgentSummary                  `json:"agents"`
	Governor                     GovernorSummary                 `json:"governor"`
	Tokens24h                    int64                           `json:"tokens_24h"`
	Contributors                 ContributorSummary              `json:"contributors"`
	Leaderboard                  []LeaderboardEntry              `json:"leaderboard"`
	ActiveSessionUsers           []string                        `json:"active_session_users,omitempty"`
	EngagedSessionUsers          []string                        `json:"engaged_session_users,omitempty"`
	DashboardTokenHash           string                          `json:"dashboard_token_hash,omitempty"`
	UserLastActions              map[string]string               `json:"user_last_actions,omitempty"`
	StartedAt                    string                          `json:"started_at,omitempty"`
	OpenFDs                      int                             `json:"open_fds,omitempty"`
	FDSoftLimit                  uint64                          `json:"fd_soft_limit,omitempty"`
	Reporter                     string                          `json:"reporter,omitempty"`
	AdvisoryLastPostedAt         string                          `json:"advisory_last_posted_at,omitempty"`
	AdvisoryError                string                          `json:"advisory_error,omitempty"`
	AdvisoryFindingCount         int                             `json:"advisory_finding_count,omitempty"`
	AdvisoryOverflowCount        int                             `json:"advisory_overflow_count,omitempty"`
	InferenceAuthError           string                          `json:"inference_auth_error,omitempty"`
	ProviderLimitReason          string                          `json:"provider_limit_reason,omitempty"`
	ProviderLimitRebuffs         int                             `json:"provider_limit_rebuffs,omitempty"`
	ProviderLimitHiveWide        bool                            `json:"provider_limit_hive_wide,omitempty"`
	ProviderLimitAgents          []string                        `json:"provider_limit_agents,omitempty"`
	Health                       map[string]any                  `json:"health"`
	DashboardURL                 string                          `json:"dashboard_url"`
	LastWriteCapableKickAt       string                          `json:"last_write_capable_kick_at,omitempty"`
	LastKickDisposition          string                          `json:"last_kick_disposition,omitempty"`
	LastKickSkipReason           string                          `json:"last_kick_skip_reason,omitempty"`
	NotWritableQueued            int                             `json:"not_writable_queued,omitempty"`
	PublicURLSelfCheck           *PublicURLSelfCheck             `json:"public_url_self_check,omitempty"`
	RouteExists                  *RouteExistenceCheck            `json:"route_exists,omitempty"`
	SnapshotURL                  string                          `json:"snapshot_url"`
	Owner                        string                          `json:"owner,omitempty"`
	HiveType                     string                          `json:"hive_type,omitempty"`
	ClusterID                    string                          `json:"cluster_id,omitempty"`
	IsPublic                     bool                            `json:"is_public"`
	Version                      string                          `json:"version"`
	GitHash                      string                          `json:"git_hash"`
	GitBranch                    string                          `json:"git_branch,omitempty"`
	ImageRef                     string                          `json:"image_ref,omitempty"`
	GitHubHost                   string                          `json:"github_host,omitempty"`
	Timestamp                    string                          `json:"timestamp"`
	GitHubAppRequired            bool                            `json:"github_app_required,omitempty"`
	GitHubAppPermIssue           string                          `json:"github_app_perm_issue,omitempty"`
	GitHubAppTokenStatus         string                          `json:"github_app_token_status,omitempty"`
	GitHubAppTokenLastMintAt     string                          `json:"github_app_token_last_mint_at,omitempty"`
	GitHubAppTokenError          string                          `json:"github_app_token_error,omitempty"`
	GitHubAppErrorClass          string                          `json:"github_app_error_class,omitempty"`
	GitHubAppHTTPStatus          int                             `json:"github_app_http_status,omitempty"`
	RepoTargetMisconfigured      bool                            `json:"repo_target_misconfigured,omitempty"`
	RepoTargetIssue              string                          `json:"repo_target_issue,omitempty"`
	GitHubAppState               string                          `json:"github_app_state,omitempty"`
	AutoUpgrade                  bool                            `json:"auto_upgrade,omitempty"`
	Upgrading                    bool                            `json:"upgrading,omitempty"`
	UpgradeTargetSHA             string                          `json:"upgrade_target_sha,omitempty"`
	UpgradeFailed                bool                            `json:"upgrade_failed,omitempty"`
	UpgradeError                 string                          `json:"upgrade_error,omitempty"`
	PendingGitHubAppInstall      bool                            `json:"pending_github_app_install,omitempty"`
	ClusterHealth                *HeartbeatClusterHealthReport   `json:"cluster_health,omitempty"`
	PRsMerged90d                 *int                            `json:"prs_merged_90d,omitempty"`
	PRsRejected90d               *int                            `json:"prs_rejected_90d,omitempty"`
	CVEsClosed                   *int                            `json:"cves_closed,omitempty"`
	FleetStatsCollectedAt        string                          `json:"fleet_stats_collected_at,omitempty"`
	RepoActivity                 []RepoActivityWire              `json:"repo_activity,omitempty"`
	RepoActivityCollectedAt      string                          `json:"repo_activity_collected_at,omitempty"`
	RepoActivityWindowHours      int                             `json:"repo_activity_window_hours,omitempty"`
	RepoActivityCountWindowHours int                             `json:"repo_activity_count_window_hours,omitempty"`
	BudgetCurrentSpend           *int64                          `json:"budget_current_spend,omitempty"`
	BudgetLimit                  *int64                          `json:"budget_limit,omitempty"`
	BudgetWindowStartsAt         string                          `json:"budget_window_starts_at,omitempty"`
	BudgetWindowEndsAt           string                          `json:"budget_window_ends_at,omitempty"`
	BudgetExhausted              *bool                           `json:"budget_exhausted,omitempty"`
	BudgetIgnored                *bool                           `json:"budget_ignored,omitempty"`
	HoldTotal                    *int                            `json:"hold_total,omitempty"`
	AwaitingReview               *int                            `json:"awaiting_review,omitempty"`
	AgentErrorStreaks            map[string]int                  `json:"agent_error_streaks"`
	ConsentWedged                []string                        `json:"consent_wedged"`
	NoCadenceAgents              []string                        `json:"no_cadence_agents"`
	SLAViolations                *int                            `json:"sla_violations,omitempty"`
	TasksCompleted7d             *int                            `json:"tasks_completed_7d,omitempty"`
	AgentsWithModel              *int                            `json:"agents_with_model,omitempty"`
	GatewayNames                 []string                        `json:"gateway_names,omitempty"`
	GatewayHealth                []inferencehealth.GatewayStatus `json:"gateway_health,omitempty"`
	GitHubAppKeyFingerprint      string                          `json:"github_app_key_fingerprint,omitempty"`
	GitHubAppKeyPerHive          bool                            `json:"github_app_key_per_hive,omitempty"`
	GitHubAppID                  int64                           `json:"github_app_id,omitempty"`
	GitHubAppSlug                string                          `json:"github_app_slug,omitempty"`
	GitHubInstallationID         int64                           `json:"github_installation_id,omitempty"`
	GitHubBaseURL                string                          `json:"github_base_url,omitempty"`
	GitHubAppKeysHeld            map[string]string               `json:"github_app_keys_held,omitempty"`
	ComponentReach               *tracing.ReachReport            `json:"component_reach,omitempty"`
	StatsStale                   bool                            `json:"stats_stale,omitempty"`
}

const (
	PublicURLSelfCheckOK                     = "ok"
	PublicURLSelfCheckFail                   = "fail"
	PublicURLSelfCheckUnknown                = "unknown"
	publicURLSelfCheckMinConsecutiveFailures = 3
	RouteExistenceFound                      = "found"
	RouteExistenceMissing                    = "missing"
	RouteExistenceUnknown                    = "unknown"
)

type PublicURLSelfCheck struct {
	Status     string `json:"status"`
	CheckedAt  string `json:"checked_at,omitempty"`
	Error      string `json:"error,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}
type RouteExistenceCheck struct {
	Status    string `json:"status"`
	CheckedAt string `json:"checked_at,omitempty"`
	Host      string `json:"host,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Error     string `json:"error,omitempty"`
}

type HeartbeatGitHubAppConfig struct {
	AppID             int64             `json:"app_id"`
	InstallationID    int64             `json:"installation_id"`
	PrivateKey        string            `json:"private_key,omitempty"`
	AppSlug           string            `json:"app_slug,omitempty"`
	ResetInstallation bool              `json:"reset_installation,omitempty"`
	APIURL            string            `json:"api_url,omitempty"`
	BaseURL           string            `json:"base_url,omitempty"`
	AdditionalKeys    []HeartbeatAppKey `json:"additional_keys,omitempty"`
	SecondaryKey      *HeartbeatAppKey  `json:"secondary_key,omitempty"`
}
type HeartbeatAppKey struct {
	AppID       int64  `json:"app_id"`
	PrivateKey  string `json:"private_key"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
type HeartbeatProjectConfig struct {
	Org          string                    `json:"org"`
	Repos        []string                  `json:"repos"`
	PrimaryRepo  string                    `json:"primary_repo,omitempty"`
	ACMMLevel    int                       `json:"acmm_level"`
	AIAuthor     string                    `json:"ai_author,omitempty"`
	DashboardURL string                    `json:"dashboard_url,omitempty"`
	GitHubAPIURL string                    `json:"github_api_url,omitempty"`
	IssueFilter  *config.IssueFilterConfig `json:"issue_filter,omitempty"`
}
type HeartbeatGatewayConfig struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	DefaultModel string `json:"default_model,omitempty"`
	Key          string `json:"key"`
}
type HeartbeatResponse struct {
	OK                  bool                      `json:"ok"`
	UpgradeTo           string                    `json:"upgrade_to,omitempty"`
	HubGitHash          string                    `json:"hub_git_hash,omitempty"`
	LatestSHA           string                    `json:"latest_sha,omitempty"`
	LatestTag           string                    `json:"latest_tag,omitempty"`
	SwitchToTag         string                    `json:"switch_to_tag,omitempty"`
	RestartSpoke        bool                      `json:"restart_spoke,omitempty"`
	ResetAgentRestarts  []string                  `json:"reset_agent_restarts,omitempty"`
	GitHubAppConfig     *HeartbeatGitHubAppConfig `json:"github_app_config,omitempty"`
	HubBanner           *HubBanner                `json:"hub_banner,omitempty"`
	IsPublic            *bool                     `json:"is_public,omitempty"`
	AuthorizedUsers     []string                  `json:"authorized_users,omitempty"`
	AuthorizedUserNames map[string]string         `json:"authorized_user_names,omitempty"`
	ProjectConfig       *HeartbeatProjectConfig   `json:"project_config,omitempty"`
	PendingGateway      *HeartbeatGatewayConfig   `json:"pending_gateway,omitempty"`
}
type HubBanner struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Color   string `json:"color"`
}

type TaskStatusPayload struct {
	HiveID       string             `json:"hive_id"`
	Leaderboard  []LeaderboardEntry `json:"leaderboard"`
	Contributors ContributorSummary `json:"contributors"`
}

package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inferencehealth"
	"github.com/hivecommons/hive/pkg/tracing"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	heartbeatTimeout = 10 * time.Second
	staleThreshold   = 15 * time.Minute
)

var lastHeartbeatSuccessUnix atomic.Int64
var heartbeatLoopStarted atomic.Bool
var heartbeatLoopActive atomic.Bool
var lastHeartbeatAttemptUnix atomic.Int64
var lastGoodPayload = struct {
	sync.Mutex
	payload *HeartbeatPayload
}{}
var heartbeatIdentity = struct {
	sync.Mutex
	payload *HeartbeatPayload
}{}

func publishHeartbeatIdentity(p *HeartbeatPayload) {
	if p == nil || p.HiveID == "" {
		return
	}
	c := *p
	heartbeatIdentity.Lock()
	heartbeatIdentity.payload = &c
	heartbeatIdentity.Unlock()
}
func PublishHeartbeatIdentity(hiveID, org, primaryRepo string, repos []string, reporter, startedAt, gitHash string) {
	publishHeartbeatIdentity(&HeartbeatPayload{
		HiveID:      hiveID,
		Org:         org,
		PrimaryRepo: primaryRepo,
		Repos:       append([]string(nil), repos...),
		Reporter:    reporter,
		StartedAt:   startedAt,
		GitHash:     gitHash,
	})
}
func minimalLivenessPayload() *HeartbeatPayload {
	heartbeatIdentity.Lock()
	id := heartbeatIdentity.payload
	heartbeatIdentity.Unlock()
	if id == nil {
		return nil
	}
	c := *id
	c.StatsStale = true
	return &c
}
func storeUnixOrZero(dst *atomic.Int64, t time.Time) {
	if t.IsZero() {
		dst.Store(0)
		return
	}
	dst.Store(t.Unix())
}
func recordHeartbeatSuccess() {
	lastHeartbeatSuccessUnix.Store(time.Now().Unix())
}
func recordHeartbeatAttempt() {
	lastHeartbeatAttemptUnix.Store(time.Now().Unix())
}
func LastHeartbeatAttempt() (t time.Time, ok bool) {
	sec := lastHeartbeatAttemptUnix.Load()
	if sec == 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}
func LastHeartbeatSuccess() (t time.Time, ok bool) {
	sec := lastHeartbeatSuccessUnix.Load()
	if sec == 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}
func HeartbeatEnabled() bool {
	return heartbeatLoopStarted.Load()
}

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
type StatusCollector func() *HeartbeatPayload
type RestartSpokeCallback func()
type AgentRestartResetCallback func(agent string)
type UpgradeCallback func(targetSHA string)

func StartHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, interval time.Duration, logger *slog.Logger, callbacks ...any) {
	if hubURL == "" {
		logger.Info("hub heartbeat disabled (no HIVE_HUB_URL)")
		return
	}
	if !heartbeatLoopActive.CompareAndSwap(false, true) {
		logger.Error("duplicate StartHeartbeat REFUSED: a heartbeat loop is already running in this process (#2453) — the stack below names the caller that tried to start a second one",
			"hub_url", hubURL,
			"stack", string(debug.Stack()),
		)
		return
	}
	defer heartbeatLoopActive.Store(false)
	heartbeatLoopStarted.Store(true)
	var onUpgrade UpgradeCallback
	var onGitHubAppConfig GitHubAppConfigCallback
	var onHubBanner HubBannerCallback
	var onVisibility VisibilityCallback
	var onSwitchBranch SwitchBranchCallback
	var onAuthorizedUsers AuthorizedUsersCallback
	var onProjectConfig ProjectConfigCallback
	var onGatewayConfig GatewayConfigCallback
	var onRestartSpoke RestartSpokeCallback
	var onAgentRestartReset AgentRestartResetCallback
	for _, cb := range callbacks {
		switch fn := cb.(type) {
		case UpgradeCallback:
			onUpgrade = fn
		case GitHubAppConfigCallback:
			onGitHubAppConfig = fn
		case HubBannerCallback:
			onHubBanner = fn
		case VisibilityCallback:
			onVisibility = fn
		case SwitchBranchCallback:
			onSwitchBranch = fn
		case AuthorizedUsersCallback:
			onAuthorizedUsers = fn
		case ProjectConfigCallback:
			onProjectConfig = fn
		case GatewayConfigCallback:
			onGatewayConfig = fn
		case RestartSpokeCallback:
			onRestartSpoke = fn
		case AgentRestartResetCallback:
			onAgentRestartReset = fn
		}
	}
	logger.Info("hub heartbeat enabled", "url", hubURL, "interval", interval)
	waitForReady(ctx, logger)
	processHeartbeatResponse := func(resp *HeartbeatResponse) {
		if resp == nil {
			return
		}
		if resp.SwitchToTag != "" && onSwitchBranch != nil {
			onSwitchBranch(resp.SwitchToTag)
		} else if resp.UpgradeTo != "" && onUpgrade != nil {
			onUpgrade(resp.UpgradeTo)
		}
		if resp.GitHubAppConfig != nil && onGitHubAppConfig != nil {
			onGitHubAppConfig(resp.GitHubAppConfig)
		}
		if onHubBanner != nil {
			onHubBanner(resp.HubBanner)
		}
		if resp.IsPublic != nil && onVisibility != nil {
			onVisibility(*resp.IsPublic)
		}
		if resp.AuthorizedUsers != nil && onAuthorizedUsers != nil {
			onAuthorizedUsers(resp.AuthorizedUsers, resp.AuthorizedUserNames)
		}
		if resp.ProjectConfig != nil && onProjectConfig != nil {
			onProjectConfig(resp.ProjectConfig)
		}
		if resp.PendingGateway != nil && onGatewayConfig != nil {
			onGatewayConfig(resp.PendingGateway)
		}
		if resp.RestartSpoke && onRestartSpoke != nil {
			onRestartSpoke()
		}
		if onAgentRestartReset != nil {
			for _, name := range resp.ResetAgentRestarts {
				onAgentRestartReset(name)
			}
		}
	}
	processHeartbeatResponse(sendHeartbeat(ctx, hubURL, collect, logger))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("hub heartbeat stopped")
			return
		case <-ticker.C:
			processHeartbeatResponse(sendHeartbeat(ctx, hubURL, collect, logger))
		}
	}
}

var (
	waitForReadyPollInterval = 5 * time.Second
	waitForReadyMaxWait      = 3 * time.Minute
)
var (
	publicURLSelfProbeInterval = 5 * time.Minute
	publicURLSelfProbeTimeout  = 8 * time.Second
	publicURLSelfProbePort     = 3002
	publicURLSelfProbeClient   = &http.Client{
		Timeout: publicURLSelfProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	publicURLSelfProbeCache = struct {
		sync.Mutex
		url                 string
		nextProbe           time.Time
		result              *PublicURLSelfCheck
		stable              *PublicURLSelfCheck
		consecutiveFailures int
	}{}
	routeExistenceProbeInterval = 5 * time.Minute
	routeExistenceProbeTimeout  = 8 * time.Second
	routeExistenceProbeCache    = struct {
		sync.Mutex
		host      string
		nextProbe time.Time
		result    *RouteExistenceCheck
	}{}
	servedHostProbeCache = struct {
		sync.Mutex
		nextProbe time.Time
		host      string
		probed    bool
	}{}
)

func waitForReady(ctx context.Context, logger *slog.Logger) {
	const healthURL = "http://localhost:3001/api/health"
	pollInterval := waitForReadyPollInterval
	maxWait := waitForReadyMaxWait
	deadline := time.After(maxWait)
	logger.Info("heartbeat waiting for dashboard readiness")
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			logger.Warn("heartbeat readiness wait timed out, starting anyway")
			return
		default:
			reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, nil)
			if err == nil {
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == 200 {
						cancel()
						logger.Info("dashboard ready, starting heartbeats")
						return
					}
				}
			}
			cancel()
			time.Sleep(pollInterval)
		}
	}
}

var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func sendHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, logger *slog.Logger) *HeartbeatResponse {
	recordHeartbeatAttempt()
	payload := collectWithTimeout(ctx, collect, heartbeatTimeout, logger)
	if payload == nil {
		cached := loadLastGoodPayload()
		if cached == nil {
			minimal := minimalLivenessPayload()
			if minimal == nil {
				logger.Warn("hub heartbeat collect timed out and hive identity not yet known — skipping this beat; loop keeps ticking so liveness stays green")
				return nil
			}
			logger.Warn("hub heartbeat collect timed out with no cached payload — sending MINIMAL liveness beat (identity only, no stats) so the hub keeps this hive online; loop keeps ticking so liveness stays green")
			payload = minimal
			payload.Timestamp = time.Now().UTC().Format(time.RFC3339)
			return postHeartbeatToHub(ctx, hubURL, payload, logger)
		}
		cached.StatsStale = true
		payload = cached
		logger.Warn("hub heartbeat collect timed out — sending LAST-GOOD cached stats (marked stale) so the hub keeps this hive online; loop keeps ticking so liveness stays green")
	} else {
		storeLastGoodPayload(payload)
		publishHeartbeatIdentity(payload)
	}
	payload.Timestamp = time.Now().UTC().Format(time.RFC3339)
	payload.PublicURLSelfCheck = publicURLSelfCheckFor(ctx, payload.DashboardURL, logger)
	payload.RouteExists = routeExistenceCheckFor(ctx, payload.DashboardURL, logger)
	filtered := payload.Leaderboard[:0]
	for _, lb := range payload.Leaderboard {
		if lb.GitHubUsername != "" && validNamePattern.MatchString(lb.GitHubUsername) {
			filtered = append(filtered, lb)
		}
	}
	payload.Leaderboard = filtered
	filteredAgents := payload.Agents[:0]
	for _, a := range payload.Agents {
		if a.Name != "" && validNamePattern.MatchString(a.Name) {
			filteredAgents = append(filteredAgents, a)
		}
	}
	payload.Agents = filteredAgents
	return postHeartbeatToHub(ctx, hubURL, payload, logger)
}
func postHeartbeatToHub(ctx context.Context, hubURL string, payload *HeartbeatPayload, logger *slog.Logger) *HeartbeatResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("hub heartbeat marshal failed", "error", err)
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+"/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		logger.Warn("hub heartbeat request failed", "error", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := SpokeHeartbeatKey(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("hub heartbeat unreachable", "error", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Warn("hub heartbeat rejected", "status", resp.StatusCode, "body", string(respBody))
		return nil
	}
	recordHeartbeatSuccess()
	var hbResp HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err == nil {
		if hbResp.HubGitHash != "" {
			logger.Debug("hub version info", "hub_git_hash", hbResp.HubGitHash, "latest_sha", hbResp.LatestSHA)
		}
		if hbResp.UpgradeTo != "" {
			logger.Info("hub instructed upgrade via heartbeat", "target", hbResp.UpgradeTo)
		}
		if hbResp.GitHubAppConfig != nil {
			logger.Info("hub delivered github app config via heartbeat",
				"app_id", hbResp.GitHubAppConfig.AppID,
				"installation_id", hbResp.GitHubAppConfig.InstallationID,
			)
		}
		return &hbResp
	}
	return nil
}
func collectWithTimeout(ctx context.Context, collect StatusCollector, timeout time.Duration, logger *slog.Logger) *HeartbeatPayload {
	done := make(chan *HeartbeatPayload, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if logger != nil {
					logger.Warn("hub heartbeat collect panicked", "recover", r)
				}
				done <- nil
			}
		}()
		done <- collect()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case payload := <-done:
		return payload
	case <-timer.C:
		if logger != nil {
			logger.Warn("hub heartbeat collect timed out; loop keeps ticking so liveness stays green",
				"timeout", timeout.String())
		}
		return nil
	case <-ctx.Done():
		return nil
	}
}
func storeLastGoodPayload(p *HeartbeatPayload) {
	c := clonePayload(p)
	if c == nil {
		return
	}
	lastGoodPayload.Lock()
	lastGoodPayload.payload = c
	lastGoodPayload.Unlock()
}
func loadLastGoodPayload() *HeartbeatPayload {
	lastGoodPayload.Lock()
	p := lastGoodPayload.payload
	lastGoodPayload.Unlock()
	return clonePayload(p)
}
func clonePayload(p *HeartbeatPayload) *HeartbeatPayload {
	if p == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return &out
}
func publicURLSelfCheckFor(ctx context.Context, rawURL string, logger *slog.Logger) *PublicURLSelfCheck {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	now := time.Now()
	publicURLSelfProbeCache.Lock()
	defer publicURLSelfProbeCache.Unlock()
	if publicURLSelfProbeCache.url == rawURL && publicURLSelfProbeCache.result != nil && now.Before(publicURLSelfProbeCache.nextProbe) {
		return clonePublicURLSelfCheck(publicURLSelfProbeCache.result)
	}
	if publicURLSelfProbeCache.url != rawURL {
		publicURLSelfProbeCache.stable = nil
		publicURLSelfProbeCache.consecutiveFailures = 0
	}
	raw := publicURLSelfProbe(ctx, rawURL, publicURLSelfProbeClient)
	result := gatedPublicURLSelfCheck(raw, &publicURLSelfProbeCache.consecutiveFailures, &publicURLSelfProbeCache.stable)
	if result.Status == PublicURLSelfCheckFail && logger != nil {
		logger.Debug("public URL self-check failed", "url", rawURL, "error", result.Error, "status", result.HTTPStatus)
	}
	publicURLSelfProbeCache.url = rawURL
	publicURLSelfProbeCache.nextProbe = now.Add(publicURLSelfProbeInterval)
	publicURLSelfProbeCache.result = &result
	return clonePublicURLSelfCheck(&result)
}
func clonePublicURLSelfCheck(in *PublicURLSelfCheck) *PublicURLSelfCheck {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
func gatedPublicURLSelfCheck(raw PublicURLSelfCheck, consecutiveFailures *int, stable **PublicURLSelfCheck) PublicURLSelfCheck {
	switch raw.Status {
	case PublicURLSelfCheckOK:
		*consecutiveFailures = 0
		*stable = clonePublicURLSelfCheck(&raw)
		return raw
	case PublicURLSelfCheckFail:
		*consecutiveFailures++
		if *consecutiveFailures >= publicURLSelfCheckMinConsecutiveFailures {
			*stable = clonePublicURLSelfCheck(&raw)
			return raw
		}
		if stable != nil && *stable != nil {
			return *clonePublicURLSelfCheck(*stable)
		}
		raw.Status = PublicURLSelfCheckUnknown
		return raw
	default:
		*consecutiveFailures = 0
		return raw
	}
}
func publicURLSelfProbe(ctx context.Context, rawURL string, client *http.Client) PublicURLSelfCheck {
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return PublicURLSelfCheck{Status: PublicURLSelfCheckFail, CheckedAt: checkedAt, Error: "invalid dashboard URL"}
	}
	if client == nil {
		client = publicURLSelfProbeClient
	}
	reqCtx, cancel := context.WithTimeout(ctx, publicURLSelfProbeTimeout)
	defer cancel()
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	localURL := "http://127.0.0.1:" + itoa(publicURLSelfProbePort) + path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, localURL, nil)
	if err != nil {
		return PublicURLSelfCheck{Status: PublicURLSelfCheckFail, CheckedAt: checkedAt, Error: "invalid dashboard URL"}
	}
	req.Host = u.Host
	resp, err := client.Do(req)
	if err != nil {
		return PublicURLSelfCheck{Status: publicURLSelfCheckStatusForError(err), CheckedAt: checkedAt, Error: terseProbeError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusInternalServerError {
		return PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: checkedAt, HTTPStatus: resp.StatusCode}
	}
	return PublicURLSelfCheck{Status: PublicURLSelfCheckFail, CheckedAt: checkedAt, HTTPStatus: resp.StatusCode, Error: "HTTP " + itoa(resp.StatusCode)}
}
func routeExistenceCheckFor(ctx context.Context, rawURL string, logger *slog.Logger) *RouteExistenceCheck {
	host := dashboardHost(rawURL)
	if host == "" {
		return nil
	}
	now := time.Now()
	routeExistenceProbeCache.Lock()
	defer routeExistenceProbeCache.Unlock()
	if routeExistenceProbeCache.host == host && routeExistenceProbeCache.result != nil && now.Before(routeExistenceProbeCache.nextProbe) {
		return cloneRouteExistenceCheck(routeExistenceProbeCache.result)
	}
	result := routeExistenceProbe(ctx, host)
	if result.Status == RouteExistenceMissing && logger != nil {
		logger.Debug("dashboard route/ingress not found", "host", host)
	} else if result.Status == RouteExistenceUnknown && logger != nil && result.Error != "" {
		logger.Debug("dashboard route/ingress existence unknown", "host", host, "error", result.Error)
	}
	routeExistenceProbeCache.host = host
	routeExistenceProbeCache.nextProbe = now.Add(routeExistenceProbeInterval)
	routeExistenceProbeCache.result = &result
	return cloneRouteExistenceCheck(&result)
}
func dashboardHost(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
func cloneRouteExistenceCheck(in *RouteExistenceCheck) *RouteExistenceCheck {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

var (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	kubernetesAPIHost = func() string { return os.Getenv("KUBERNETES_SERVICE_HOST") }
	kubernetesAPIPort = func() string { return os.Getenv("KUBERNETES_SERVICE_PORT") }
)

func routeExistenceProbe(ctx context.Context, host string) RouteExistenceCheck {
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return RouteExistenceCheck{Status: RouteExistenceUnknown, CheckedAt: checkedAt, Error: "empty dashboard host"}
	}
	cfg, err := inClusterAPIConfig()
	if err != nil {
		return RouteExistenceCheck{Status: RouteExistenceUnknown, CheckedAt: checkedAt, Host: host, Error: err.Error()}
	}
	client, err := inClusterHTTPClient(cfg)
	if err != nil {
		return RouteExistenceCheck{Status: RouteExistenceUnknown, CheckedAt: checkedAt, Host: host, Error: err.Error()}
	}
	ingFound, ingUnknown, ingErr := ingressHostExists(ctx, client, cfg, host)
	if ingFound {
		return RouteExistenceCheck{Status: RouteExistenceFound, CheckedAt: checkedAt, Host: host, Kind: "Ingress"}
	}
	routeFound, routeUnknown, routeErr := routeHostExists(ctx, client, cfg, host)
	if routeFound {
		return RouteExistenceCheck{Status: RouteExistenceFound, CheckedAt: checkedAt, Host: host, Kind: "Route"}
	}
	if ingUnknown || routeUnknown {
		if ingErr != "" {
			return RouteExistenceCheck{Status: RouteExistenceUnknown, CheckedAt: checkedAt, Host: host, Error: ingErr}
		}
		return RouteExistenceCheck{Status: RouteExistenceUnknown, CheckedAt: checkedAt, Host: host, Error: routeErr}
	}
	return RouteExistenceCheck{Status: RouteExistenceMissing, CheckedAt: checkedAt, Host: host, Error: "no Ingress or Route for host"}
}

type inClusterConfig struct {
	BaseURL   string
	Token     string
	Namespace string
	CAPath    string
}

func inClusterAPIConfig() (*inClusterConfig, error) {
	host, port := strings.TrimSpace(kubernetesAPIHost()), strings.TrimSpace(kubernetesAPIPort())
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes cluster")
	}
	tokenBytes, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return nil, errors.New("serviceaccount token unavailable")
	}
	nsBytes, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
	if err != nil {
		return nil, errors.New("serviceaccount namespace unavailable")
	}
	return &inClusterConfig{
		BaseURL:   "https://" + host + ":" + port,
		Token:     strings.TrimSpace(string(tokenBytes)),
		Namespace: strings.TrimSpace(string(nsBytes)),
		CAPath:    filepath.Join(serviceAccountDir, "ca.crt"),
	}, nil
}
func inClusterHTTPClient(cfg *inClusterConfig) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	ca, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read k8s CA cert at %s, refusing to probe the API server without it: %w", cfg.CAPath, err)
	}
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("k8s CA cert at %s contained no PEM certificates, refusing to probe the API server with an unverified root set", cfg.CAPath)
	}
	return &http.Client{
		Timeout: routeExistenceProbeTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}, nil
}
func ingressHostExists(ctx context.Context, client *http.Client, cfg *inClusterConfig, host string) (found, unknown bool, errText string) {
	var list struct {
		Items []struct {
			Spec struct {
				Rules []struct {
					Host string `json:"host"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	unknown, errText = listKubernetesResource(ctx, client, cfg, "/apis/networking.k8s.io/v1/namespaces/"+url.PathEscape(cfg.Namespace)+"/ingresses", &list)
	if unknown {
		return false, true, errText
	}
	for _, item := range list.Items {
		for _, rule := range item.Spec.Rules {
			if strings.EqualFold(strings.TrimSpace(rule.Host), host) {
				return true, false, ""
			}
		}
	}
	return false, false, ""
}
func spokeServedHost(ctx context.Context) string {
	now := time.Now()
	servedHostProbeCache.Lock()
	defer servedHostProbeCache.Unlock()
	if servedHostProbeCache.probed && now.Before(servedHostProbeCache.nextProbe) {
		return servedHostProbeCache.host
	}
	host := discoverSpokeServedHost(ctx)
	servedHostProbeCache.probed = true
	servedHostProbeCache.nextProbe = now.Add(routeExistenceProbeInterval)
	servedHostProbeCache.host = host
	return host
}
func SpokeServedHost(ctx context.Context) string { return spokeServedHost(ctx) }
func discoverSpokeServedHost(ctx context.Context) string {
	cfg, err := inClusterAPIConfig()
	if err != nil {
		return ""
	}
	client, err := inClusterHTTPClient(cfg)
	if err != nil {
		return ""
	}
	if host := ingressServedHost(ctx, client, cfg); host != "" {
		return host
	}
	return routeServedHost(ctx, client, cfg)
}
func ingressServedHost(ctx context.Context, client *http.Client, cfg *inClusterConfig) string {
	var list struct {
		Items []struct {
			Spec struct {
				Rules []struct {
					Host string `json:"host"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	if unknown, _ := listKubernetesResource(ctx, client, cfg,
		"/apis/networking.k8s.io/v1/namespaces/"+url.PathEscape(cfg.Namespace)+"/ingresses", &list); unknown {
		return ""
	}
	for _, item := range list.Items {
		for _, rule := range item.Spec.Rules {
			if host := strings.ToLower(strings.TrimSpace(rule.Host)); host != "" {
				return host
			}
		}
	}
	return ""
}
func routeServedHost(ctx context.Context, client *http.Client, cfg *inClusterConfig) string {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Host string `json:"host"`
			} `json:"spec"`
		} `json:"items"`
	}
	if unknown, _ := listKubernetesResource(ctx, client, cfg,
		"/apis/route.openshift.io/v1/namespaces/"+url.PathEscape(cfg.Namespace)+"/routes", &list); unknown {
		return ""
	}
	fallback := ""
	for _, item := range list.Items {
		host := strings.ToLower(strings.TrimSpace(item.Spec.Host))
		if host == "" {
			continue
		}
		if strings.TrimSpace(item.Metadata.Name) == routeBaseDashboard {
			return host
		}
		if fallback == "" {
			fallback = host
		}
	}
	return fallback
}
func routeHostExists(ctx context.Context, client *http.Client, cfg *inClusterConfig, host string) (found, unknown bool, errText string) {
	var list struct {
		Items []struct {
			Spec struct {
				Host string `json:"host"`
			} `json:"spec"`
		} `json:"items"`
	}
	unknown, errText = listKubernetesResource(ctx, client, cfg, "/apis/route.openshift.io/v1/namespaces/"+url.PathEscape(cfg.Namespace)+"/routes", &list)
	if unknown {
		return false, true, errText
	}
	for _, item := range list.Items {
		if strings.EqualFold(strings.TrimSpace(item.Spec.Host), host) {
			return true, false, ""
		}
	}
	return false, false, ""
}
func listKubernetesResource(ctx context.Context, client *http.Client, cfg *inClusterConfig, path string, out any) (unknown bool, errText string) {
	reqCtx, cancel := context.WithTimeout(ctx, routeExistenceProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.BaseURL+path, nil)
	if err != nil {
		return true, "invalid Kubernetes API request"
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return true, terseProbeError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, ""
	case http.StatusForbidden, http.StatusUnauthorized:
		return true, "Kubernetes RBAC does not allow listing dashboard routes"
	default:
		return true, "Kubernetes API returned HTTP " + itoa(resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPayloadBytes)).Decode(out); err != nil {
		return true, "could not decode Kubernetes API response"
	}
	return false, ""
}
func publicURLSelfCheckStatusForError(err error) string {
	if err == nil {
		return PublicURLSelfCheckUnknown
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return PublicURLSelfCheckUnknown
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "server misbehaving") ||
		strings.Contains(msg, "temporary failure in name resolution") {
		return PublicURLSelfCheckUnknown
	}
	if strings.Contains(msg, ":53") && (strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "server misbehaving")) {
		return PublicURLSelfCheckUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return PublicURLSelfCheckUnknown
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return PublicURLSelfCheckUnknown
	}
	return PublicURLSelfCheckFail
}
func terseProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	const maxProbeErrorRunes = 180
	if len([]rune(msg)) <= maxProbeErrorRunes {
		return msg
	}
	r := []rune(msg)
	return string(r[:maxProbeErrorRunes-1]) + "…"
}
func SendUpgradingHeartbeat(hubURL string, collect StatusCollector, targetSHA string, logger *slog.Logger) {
	payload := collect()
	if payload == nil {
		return
	}
	payload.Upgrading = true
	payload.UpgradeTargetSHA = targetSHA
	payload.Timestamp = time.Now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := SpokeHeartbeatKey(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("upgrading heartbeat failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 {
		recordHeartbeatSuccess()
	}
	logger.Info("upgrading heartbeat sent to hub", "target", targetSHA)
}
func ReportUpgradeFailure(hubURL, hiveID, targetSHA, currentSHA, cause string, logger *slog.Logger) {
	if hubURL == "" || hiveID == "" {
		return
	}
	payload := HeartbeatPayload{
		HiveID:           hiveID,
		GitHash:          currentSHA,
		UpgradeTargetSHA: targetSHA,
		UpgradeFailed:    true,
		UpgradeError:     cause,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := SpokeHeartbeatKey(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn("could not report upgrade failure to hub", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	logger.Info("reported upgrade failure to hub", "target", targetSHA, "cause", cause)
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
type GatewayConfigCallback func(cfg *HeartbeatGatewayConfig)
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
type HubBannerCallback func(banner *HubBanner)
type VisibilityCallback func(isPublic bool)
type SwitchBranchCallback func(tag string)
type AuthorizedUsersCallback func(users []string, names map[string]string)
type GitHubAppConfigCallback func(cfg *HeartbeatGitHubAppConfig)
type ProjectConfigCallback func(cfg *HeartbeatProjectConfig)

var taskPushInterval = 30 * time.Second

type TaskStatusPayload struct {
	HiveID       string             `json:"hive_id"`
	Leaderboard  []LeaderboardEntry `json:"leaderboard"`
	Contributors ContributorSummary `json:"contributors"`
}
type TaskStatusCollector func() *TaskStatusPayload

func StartTaskStatusPush(ctx context.Context, hubURL string, collect TaskStatusCollector, logger *slog.Logger) {
	if hubURL == "" {
		return
	}
	logger.Info("hub task status push enabled", "url", hubURL, "interval", taskPushInterval)
	ticker := time.NewTicker(taskPushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload := collect()
			if payload == nil {
				continue
			}
			body, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			reqCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+"/api/task-status", bytes.NewReader(body))
			if err != nil {
				cancel()
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			if secret := SpokeHeartbeatKey(); secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			resp, err := http.DefaultClient.Do(req)
			cancel()
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

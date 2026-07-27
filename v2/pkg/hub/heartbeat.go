package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"
	"time"
)

const (
	heartbeatTimeout = 10 * time.Second
	staleThreshold   = 15 * time.Minute
)

// lastHeartbeatSuccessUnix holds the unix-seconds timestamp of the most
// recent heartbeat the hub accepted (HTTP < 300 from either the regular
// StartHeartbeat loop or SendUpgradingHeartbeat). It is process-global
// because a hive runs at most one heartbeat loop, and it needs to be read
// from the dashboard's HTTP handler goroutine (the liveness endpoint) while
// being written from the heartbeat goroutine — hence atomic rather than a
// mutex-guarded field, so the health handler never blocks on the sender.
//
// Zero means "no successful heartbeat yet" (process just started, or the
// hive doesn't run a heartbeat loop at all). Callers distinguish those two
// cases via HeartbeatEnabled, not via this timestamp alone.
var lastHeartbeatSuccessUnix atomic.Int64

// heartbeatLoopStarted records whether StartHeartbeat actually launched a
// loop for this process (i.e. hubURL was non-empty). A hive with no hub
// configured never sends heartbeats, so its liveness check must not gate on
// heartbeat freshness at all — otherwise every hub-less hive would fail
// liveness forever. Set once at startup; read from the HTTP handler.
var heartbeatLoopStarted atomic.Bool

func recordHeartbeatSuccess() {
	lastHeartbeatSuccessUnix.Store(time.Now().Unix())
}

// LastHeartbeatSuccess returns the time of the last heartbeat the hub
// accepted, and whether one has ever succeeded. A zero ok means either no
// heartbeat has succeeded yet (e.g. still within startup grace, or the hub
// has been down since boot) or this process never runs a heartbeat loop —
// callers should check HeartbeatEnabled() to tell those apart.
func LastHeartbeatSuccess() (t time.Time, ok bool) {
	sec := lastHeartbeatSuccessUnix.Load()
	if sec == 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// HeartbeatEnabled reports whether this process launched a hub heartbeat
// loop (StartHeartbeat was called with a non-empty hubURL). Hives with no
// hub configured never send heartbeats, so liveness checks must skip the
// staleness gate entirely for them.
func HeartbeatEnabled() bool {
	return heartbeatLoopStarted.Load()
}

type AgentSummary struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Mode  string `json:"mode,omitempty"`
}

type GovernorSummary struct {
	Mode   string `json:"mode"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
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

// HeartbeatClusterHealthReport contains cluster node and GPU metrics
// collected by the spoke in-cluster and sent to the hub via heartbeat.
// This allows the hub to display health data for firewalled clusters
// that it cannot query directly via kubectl.
type HeartbeatClusterHealthReport struct {
	Nodes       []HeartbeatNodeMetric   `json:"nodes"`
	Summary     HeartbeatClusterSummary `json:"summary"`
	GPUSummary  *HeartbeatGPUSummary    `json:"gpu_summary,omitempty"`
	CollectedAt string                  `json:"collected_at"`
}

// HeartbeatNodeMetric holds per-node resource usage collected on the spoke.
type HeartbeatNodeMetric struct {
	Name          string `json:"name"`
	CPUCores      int    `json:"cpu_cores"`
	CPUUsedMillis int64  `json:"cpu_used_millicores"`
	CPUPercent    int    `json:"cpu_percent"`
	MemTotalMB    int64  `json:"mem_total_mb"`
	MemUsedMB     int64  `json:"mem_used_mb"`
	MemPercent    int    `json:"mem_percent"`
	Pods          int    `json:"pods"`
	PodCapacity   int    `json:"pod_capacity"`
	// HiveCount is the number of distinct hive-hosted-* namespaces with a
	// running pod on this node (namespaces, not pods, so a hive briefly
	// running two pods during a rollout is counted once).
	HiveCount    int      `json:"hive_count,omitempty"`
	Ready        bool     `json:"ready"`
	Conditions   []string `json:"conditions"`
	DiskPressure bool     `json:"disk_pressure"`
	GPUs         int      `json:"gpus,omitempty"`
	GPUType      string   `json:"gpu_type,omitempty"`
}

// HeartbeatClusterSummary aggregates node-level data into cluster totals.
type HeartbeatClusterSummary struct {
	TotalNodes    int `json:"total_nodes"`
	ReadyNodes    int `json:"ready_nodes"`
	TotalCPUCores int `json:"total_cpu_cores"`
	TotalCPUPct   int `json:"total_cpu_percent"`
	TotalMemGB    int `json:"total_mem_gb"`
	TotalMemPct   int `json:"total_mem_percent"`
	TotalPods     int `json:"total_pods"`
	// HiveCapacityRemaining estimates how many MORE hives the cluster can
	// hold (see hive_capacity.go). Pointer so old spokes that do not report
	// it are distinguishable from a genuinely full cluster (nil vs 0).
	HiveCapacityRemaining *int `json:"hive_capacity_remaining,omitempty"`
}

// HeartbeatGPUSummary reports aggregate GPU counts collected on the spoke.
type HeartbeatGPUSummary struct {
	Total     int      `json:"total"`
	Allocated int      `json:"allocated"`
	Types     []string `json:"types"`
}

type HeartbeatPayload struct {
	HiveID                  string                        `json:"hive_id"`
	Org                     string                        `json:"org"`
	Repos                   []string                      `json:"repos"`
	PrimaryRepo             string                        `json:"primary_repo"`
	ACMMLevel               int                           `json:"acmm_level"`
	Agents                  []AgentSummary                `json:"agents"`
	Governor                GovernorSummary               `json:"governor"`
	Tokens24h               int64                         `json:"tokens_24h"`
	Contributors            ContributorSummary            `json:"contributors"`
	Leaderboard             []LeaderboardEntry            `json:"leaderboard"`
	Health                  map[string]any                `json:"health"`
	DashboardURL            string                        `json:"dashboard_url"`
	SnapshotURL             string                        `json:"snapshot_url"`
	Owner                   string                        `json:"owner,omitempty"`
	HiveType                string                        `json:"hive_type,omitempty"`
	ClusterID               string                        `json:"cluster_id,omitempty"`
	IsPublic                bool                          `json:"is_public"`
	Version                 string                        `json:"version"`
	GitHash                 string                        `json:"git_hash"`
	GitBranch               string                        `json:"git_branch,omitempty"`
	Timestamp               string                        `json:"timestamp"`
	GitHubAppRequired       bool                          `json:"github_app_required,omitempty"`
	GitHubAppPermIssue      string                        `json:"github_app_perm_issue,omitempty"`
	AutoUpgrade             bool                          `json:"auto_upgrade,omitempty"`
	Upgrading               bool                          `json:"upgrading,omitempty"`
	UpgradeTargetSHA        string                        `json:"upgrade_target_sha,omitempty"`
	PendingGitHubAppInstall bool                          `json:"pending_github_app_install,omitempty"`
	ClusterHealth           *HeartbeatClusterHealthReport `json:"cluster_health,omitempty"`
}

type StatusCollector func() *HeartbeatPayload

// UpgradeCallback is called when the hub instructs this hive to upgrade
// to a specific SHA via the heartbeat response.
type UpgradeCallback func(targetSHA string)

func StartHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, interval time.Duration, logger *slog.Logger, callbacks ...any) {
	if hubURL == "" {
		logger.Info("hub heartbeat disabled (no HIVE_HUB_URL)")
		return
	}
	// Mark the loop as active before the first send so /api/livez can tell
	// "hub-connected, awaiting first beat" (healthy during startup grace)
	// apart from "no hub configured" (never gated on heartbeat freshness).
	heartbeatLoopStarted.Store(true)
	var onUpgrade UpgradeCallback
	var onGitHubAppConfig GitHubAppConfigCallback
	var onHubBanner HubBannerCallback
	var onVisibility VisibilityCallback
	var onSwitchBranch SwitchBranchCallback
	var onAuthorizedUsers AuthorizedUsersCallback
	var onProjectConfig ProjectConfigCallback
	var onGatewayConfig GatewayConfigCallback
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
		}
	}

	logger.Info("hub heartbeat enabled", "url", hubURL, "interval", interval)

	waitForReady(ctx, logger)

	processHeartbeatResponse := func(resp *HeartbeatResponse) {
		if resp == nil {
			return
		}
		if resp.SwitchToTag != "" && onSwitchBranch != nil {
			// Branch switch takes precedence over a plain SHA upgrade — it
			// changes the image tag, which a same-tag re-pull can't do.
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
		// A non-nil slice (even empty) is an authoritative replacement of the
		// spoke's allowlist; nil means the hub sent nothing, leave it alone.
		if resp.AuthorizedUsers != nil && onAuthorizedUsers != nil {
			onAuthorizedUsers(resp.AuthorizedUsers)
		}
		// A non-nil ProjectConfig means the hub wants the spoke to adopt a newly
		// claimed project (placeholder assignment); nil means leave it alone.
		if resp.ProjectConfig != nil && onProjectConfig != nil {
			onProjectConfig(resp.ProjectConfig)
		}
		// A non-nil PendingGateway means the hub funded a gateway on this spoke's
		// behalf (OpenRouter scan-to-fund). The spoke stores the key locally and
		// creates/replaces the gateway.
		if resp.PendingGateway != nil && onGatewayConfig != nil {
			onGatewayConfig(resp.PendingGateway)
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

func waitForReady(ctx context.Context, logger *slog.Logger) {
	const healthURL = "http://localhost:3001/api/health"
	const pollInterval = 5 * time.Second
	const maxWait = 3 * time.Minute
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
					resp.Body.Close()
					if resp.StatusCode == 200 {
						cancel()
						logger.Info("dashboard ready, starting heartbeats")
						return
					}
				}
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				logger.Warn("heartbeat readiness wait timed out, starting anyway")
				return
			case <-time.After(pollInterval):
			}
		}
	}
}

var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func sendHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, logger *slog.Logger) *HeartbeatResponse {
	payload := collect()
	if payload == nil {
		return nil
	}
	payload.Timestamp = time.Now().UTC().Format(time.RFC3339)

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
	if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("hub heartbeat unreachable", "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Warn("hub heartbeat rejected", "status", resp.StatusCode, "body", string(respBody))
		return nil
	}

	// The hub accepted the beat (status < 300) — this is what /api/livez
	// treats as "the heartbeat goroutine is alive and doing its job", so
	// record it even if the response body below fails to decode.
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

// SendUpgradingHeartbeat sends a final heartbeat to the hub indicating this
// spoke is about to restart for an upgrade. The hub uses this to show the
// "Upgrading" spinner instead of requiring an assumption.
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
	if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("upgrading heartbeat failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		// Counts as a successful beat for liveness purposes: the process is
		// seconds from a self-initiated restart anyway, so this mainly keeps
		// LastHeartbeatSuccess fresh in case the upgrade is aborted/delayed.
		recordHeartbeatSuccess()
	}
	logger.Info("upgrading heartbeat sent to hub", "target", targetSHA)
}

// HeartbeatGitHubAppConfig carries GitHub App credentials from the hub to
// a spoke via the heartbeat response, bypassing OAuth proxies that block
// direct HTTP pushes.
type HeartbeatGitHubAppConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key,omitempty"`
}

// HeartbeatProjectConfig carries a claimed project's real org/repos/ACMM from
// the hub to a spoke via the heartbeat response. It mirrors the AuthorizedUsers
// mechanism: when an admin assigns a pre-provisioned "placeholder" hive to a
// requesting user, the hub cannot push the new project config to a
// heartbeat-only spoke (e.g. vllm-d) over kubectl. Delivering it in the
// heartbeat response lets the spoke reconcile its running config
// (cfg.Project.Org/Repos/PrimaryRepo and ACMM level) every beat until it
// matches what the hub recorded — the only channel that works uniformly for
// both reachable (hive-oke) and heartbeat-only (vllm-d) clusters.
type HeartbeatProjectConfig struct {
	Org         string   `json:"org"`
	Repos       []string `json:"repos"`
	PrimaryRepo string   `json:"primary_repo,omitempty"`
	ACMMLevel   int      `json:"acmm_level"`
	// DashboardURL is the claimed hive's vanity URL. The spoke adopts it as its
	// hub.dashboard_url and reports it in subsequent heartbeats, so the hub
	// registry's dashboardUrl becomes the vanity URL (rather than the raw
	// placeholder subdomain) — and stays that way, since it's now the value the
	// heartbeat carries. Empty = leave the spoke's dashboard URL unchanged.
	DashboardURL string `json:"dashboard_url,omitempty"`
}

// HeartbeatGatewayConfig carries an OpenRouter model gateway (funded via the
// hub's scan-to-fund flow) from the hub to a spoke via the heartbeat response.
// It mirrors HeartbeatProjectConfig: for a firewalled/heartbeat-only spoke
// (vllm-d) the hub cannot POST to the spoke's dashboard over the network, so it
// delivers the gateway in the heartbeat response and the spoke stores the key in
// its OWN per-gateway secret-file store. The Key is a secret VALUE — it travels
// only over the TLS heartbeat channel, is never logged, and the spoke writes it
// to a file (never hive.yaml).
type HeartbeatGatewayConfig struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	DefaultModel string `json:"default_model,omitempty"`
	// Key is the funded API key VALUE. The spoke stores it via its secret-file
	// store and records only the file path in hive.yaml.
	Key string `json:"key"`
}

// GatewayConfigCallback is called when the hub delivers a funded model gateway
// (e.g. OpenRouter) via the heartbeat response so the spoke can store the key
// and create/replace the gateway. Used for heartbeat-only clusters the hub
// cannot reach over the network (mirrors ProjectConfigCallback).
type GatewayConfigCallback func(cfg *HeartbeatGatewayConfig)

// HeartbeatResponse is the JSON body returned by the hub's heartbeat endpoint.
// It includes version info so the spoke can display hub version on its dashboard
// and self-upgrade when behind.
type HeartbeatResponse struct {
	OK         bool   `json:"ok"`
	UpgradeTo  string `json:"upgrade_to,omitempty"`
	HubGitHash string `json:"hub_git_hash,omitempty"`
	LatestSHA  string `json:"latest_sha,omitempty"`
	LatestTag  string `json:"latest_tag,omitempty"`
	// SwitchToTag instructs the spoke to change its own deployment image to
	// ghcr.io/kubestellar/hive:<SwitchToTag> and restart. Used for branch
	// switches on clusters the hub can't reach over kubectl — the spoke has
	// in-cluster RBAC (hive-self-upgrade role) to patch its own deployment.
	SwitchToTag     string                    `json:"switch_to_tag,omitempty"`
	GitHubAppConfig *HeartbeatGitHubAppConfig `json:"github_app_config,omitempty"`
	HubBanner       *HubBanner                `json:"hub_banner,omitempty"`
	IsPublic        *bool                     `json:"is_public,omitempty"`
	// AuthorizedUsers is the hub's authoritative per-hive access list, as
	// "username:role" entries. The hub can't reach heartbeat-only spokes (e.g.
	// vllm-d) over kubectl, and those spokes authorize their own device-flow
	// logins against a local allowlist — so a grant made in the hub's Manage
	// Access UI would never reach them. Delivering the list in the heartbeat
	// response lets the spoke reconcile its allowlist every beat, so access
	// changes propagate automatically. nil means "hub sent nothing" (leave the
	// spoke's list unchanged); a non-nil (possibly empty) slice replaces it.
	AuthorizedUsers []string `json:"authorized_users,omitempty"`
	// ProjectConfig is the claimed project's real org/repos/ACMM. The hub sets
	// it (mirroring AuthorizedUsers) when a placeholder hive has been assigned
	// to a user and the spoke is still reporting its old (placeholder) project.
	// The hub keeps sending it every beat until the spoke reconciles and reports
	// the matching project. nil means "no reconcile needed" — leave the spoke's
	// project config alone. This is the ONLY delivery channel for heartbeat-only
	// clusters (vllm-d) the hub cannot reach over kubectl.
	ProjectConfig *HeartbeatProjectConfig `json:"project_config,omitempty"`
	// PendingGateway carries a funded model gateway (OpenRouter scan-to-fund) the
	// hub minted on the spoke's behalf. It is the delivery channel for
	// firewalled/heartbeat-only spokes (vllm-d) the hub cannot POST to directly.
	// nil means "nothing to deliver". The hub sends it once (drained on delivery)
	// rather than every beat, since it carries a secret key value.
	PendingGateway *HeartbeatGatewayConfig `json:"pending_gateway,omitempty"`
}

// HubBanner is a message from the hub admin displayed on spoke dashboards.
type HubBanner struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Color   string `json:"color"`
}

// HubBannerCallback is called when the hub delivers a banner message
// via the heartbeat response.
type HubBannerCallback func(banner *HubBanner)

// VisibilityCallback is called when the hub overrides the spoke's IsPublic setting.
type VisibilityCallback func(isPublic bool)

// SwitchBranchCallback is called when the hub instructs the spoke (via
// heartbeat) to switch its deployment image to a specific tag — used for
// branch switches on clusters the hub can't reach over kubectl.
type SwitchBranchCallback func(tag string)

// AuthorizedUsersCallback is called with the hub's authoritative per-hive
// access list ("username:role" entries) so the spoke can reconcile its
// device-flow login allowlist with Manage Access grants. Used for
// heartbeat-only clusters where the hub cannot push config over kubectl.
type AuthorizedUsersCallback func(users []string)

// GitHubAppConfigCallback is called when the hub delivers GitHub App config
// via the heartbeat response (app ID, installation ID, private key).
type GitHubAppConfigCallback func(cfg *HeartbeatGitHubAppConfig)

// ProjectConfigCallback is called when the hub delivers a claimed project's
// real org/repos/ACMM via the heartbeat response so the spoke can reconcile
// its running project config. Used for heartbeat-only clusters where the hub
// cannot push config over kubectl (mirrors AuthorizedUsersCallback).
type ProjectConfigCallback func(cfg *HeartbeatProjectConfig)

// taskPushInterval is how often the spoke pushes task status to the hub. A var
// (not a const) so tests can drive a single push quickly; production keeps the
// default.
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
			if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			resp, err := http.DefaultClient.Do(req)
			cancel()
			if err == nil {
				resp.Body.Close()
			}
		}
	}
}

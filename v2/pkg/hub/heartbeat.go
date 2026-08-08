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
	"runtime/debug"
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

// heartbeatLoopActive is the process-wide "a heartbeat loop is RUNNING right
// now" guard — deliberately separate from heartbeatLoopStarted, which records
// "a loop was ever launched" and feeds /api/livez via HeartbeatEnabled (a
// semantics that must not change: liveness keeps gating on heartbeat freshness
// even while a duplicate start is being refused).
//
// WHY THIS EXISTS (#2453): a production spoke was observed running TWO
// concurrent heartbeat loops in one process — two independent 2-minute
// cadences from one pod, alternating a fresh and a stale auth snapshot on the
// wire, flipping the hub registry every beat. The code has exactly one
// StartHeartbeat call site, so the second loop's origin is unknown. This CAS
// makes a second concurrent loop impossible no matter what path tries to
// start one: the first caller wins, every later caller is refused and returns
// cleanly. The refusal logs the refused caller's FULL STACK at ERROR, so the
// next time the trigger fires in production the log names it — the guard is
// both the fix and the instrument.
//
// Cleared (via defer) when a loop exits on context cancellation, so a
// deliberate stop-then-restart stays possible; only CONCURRENT duplication is
// refused.
var heartbeatLoopActive atomic.Bool

// lastHeartbeatAttemptUnix holds the unix-seconds timestamp of the most
// recent heartbeat the loop *tried* to send, regardless of whether the hub
// accepted it, rejected it (4xx/5xx), or was unreachable entirely.
//
// This is the signal that separates the two failure modes a stale
// lastHeartbeatSuccessUnix otherwise conflates:
//
//   - Attempts advancing but successes not: the goroutine is alive and doing
//     its job; the *hub* is unreachable or rejecting. Restarting the pod
//     cannot fix a network partition or a hub outage, so liveness must not
//     fail — this is routine on firewalled clusters that can only reach the
//     hub intermittently.
//   - Attempts themselves not advancing: the loop is genuinely wedged (dead
//     goroutine, deadlock, permanently stuck HTTP call). A restart is the
//     only remedy, so this is what liveness should catch.
//
// Same atomic rationale as lastHeartbeatSuccessUnix: written by the heartbeat
// goroutine, read by the dashboard's HTTP handler goroutine.
var lastHeartbeatAttemptUnix atomic.Int64

// storeUnixOrZero stores t as unix seconds, or 0 for the zero time so it
// reads back as "never happened" through the Last* accessors.
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

// recordHeartbeatAttempt marks that the heartbeat loop reached the top of a
// send. Called unconditionally before each attempt so a hub that is down,
// unreachable, or rejecting every beat still counts as "the loop is alive".
func recordHeartbeatAttempt() {
	lastHeartbeatAttemptUnix.Store(time.Now().Unix())
}

// LastHeartbeatAttempt returns the time the heartbeat loop last tried to
// send, and whether it has ever tried. Unlike LastHeartbeatSuccess this
// advances even while the hub is unreachable, so liveness checks can tell a
// wedged goroutine (no attempts) from a partitioned network (attempts, no
// successes).
func LastHeartbeatAttempt() (t time.Time, ok bool) {
	sec := lastHeartbeatAttemptUnix.Load()
	if sec == 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
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
	// Paused distinguishes an agent the OPERATOR deliberately parked from one
	// that is merely not running. State already carries "paused", but a paused
	// agent that has not yet been through Start() reports its prior state with
	// Paused set, so the two are not interchangeable. The hub's inactive-agent
	// rule keys off this to guarantee a deliberate pause is never alerted on.
	Paused bool `json:"paused,omitempty"`
	// NeedsLogin is true when the agent's CLI is sitting on a login /
	// device-code prompt. This is the "running but not logged in" state: the
	// session is alive, the CLI process is alive, and the agent still cannot do
	// any work. It is invisible in State, which reads "running" throughout.
	NeedsLogin bool `json:"needsLogin,omitempty"`
	// SessionMissing is true when the manager expected a live tmux session for
	// a running agent and did not find one — the zombie case. Reported
	// explicitly rather than inferred hub-side, because only the spoke can see
	// the per-UID tmux socket (each agent runs on its own,
	// e.g. /tmp/tmux-2007/hive-scanner; a default-socket check reports "no
	// server running" even when every session is alive).
	SessionMissing bool `json:"sessionMissing,omitempty"`
	// StartedAt is when this agent's CLI was last launched (RFC3339), empty if
	// it has never launched. Wiring it through closes #2324: AgentProcess has
	// carried it all along, but both the dashboard and the heartbeat dropped
	// it, so no persisted per-agent session duration existed anywhere.
	StartedAt string `json:"startedAt,omitempty"`
	// LastActivityAt is when the agent's pane content last changed (RFC3339),
	// empty when the spoke has not yet observed a change. It is what separates
	// "running and working" from "running and producing nothing" — State,
	// StartedAt and the kick log all keep their values while a CLI sits idle.
	LastActivityAt string `json:"lastActivityAt,omitempty"`
}

// AgentActivity is the per-agent liveness evidence the spoke has and the hub
// does not. It exists so the two heartbeat build sites in cmd/hive (the
// ordinary loop and the upgrade beat) fill AgentSummary identically — they
// were already duplicated line for line, and a signal that is only reported on
// one of the two paths is a signal that quietly disappears mid-upgrade.
type AgentActivity struct {
	Paused         bool
	NeedsLogin     bool
	SessionMissing bool
	StartedAt      time.Time
	LastActivityAt time.Time
}

// NewAgentSummary builds one AgentSummary from an agent's name, state, mode and
// activity evidence. Zero timestamps serialise as empty (not as a bogus
// year-1 string), which the hub reads as "unknown" — never as "idle".
func NewAgentSummary(name, state, mode string, act AgentActivity) AgentSummary {
	as := AgentSummary{
		Name:           name,
		State:          state,
		Mode:           mode,
		Paused:         act.Paused,
		NeedsLogin:     act.NeedsLogin,
		SessionMissing: act.SessionMissing,
	}
	if !act.StartedAt.IsZero() {
		as.StartedAt = act.StartedAt.UTC().Format(time.RFC3339)
	}
	if !act.LastActivityAt.IsZero() {
		as.LastActivityAt = act.LastActivityAt.UTC().Format(time.RFC3339)
	}
	return as
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
	// Disk fields are pointers so a spoke that could not read kubelet stats
	// (or an older spoke build that does not report them at all) leaves them
	// absent, and the hub renders unknown rather than a misleading 0%.
	DiskTotalMB *int64 `json:"disk_total_mb,omitempty"`
	DiskUsedMB  *int64 `json:"disk_used_mb,omitempty"`
	DiskPercent *int   `json:"disk_percent,omitempty"`
	Pods        int    `json:"pods"`
	PodCapacity int    `json:"pod_capacity"`
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
	// Disk totals cover only the nodes that reported live usage; nil when no
	// node did, so the hub omits disk instead of showing a false 0%.
	TotalDiskGB  *int `json:"total_disk_gb,omitempty"`
	TotalDiskPct *int `json:"total_disk_percent,omitempty"`
	TotalPods    int  `json:"total_pods"`
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
	HiveID      string   `json:"hive_id"`
	Org         string   `json:"org"`
	Repos       []string `json:"repos"`
	PrimaryRepo string   `json:"primary_repo"`
	// AIAuthor is the GitHub account this hive's agents open PRs as. Reported
	// so the hub can echo it back in HeartbeatProjectConfig rather than
	// blanking it, and so the registry knows each hive's author without any
	// spoke-side token lookup (which does not work on App-authenticated hives).
	AIAuthor string `json:"ai_author,omitempty"`
	// AIAuthorEffective is who agents ACTUALLY author PRs/commits as: the
	// configured ai_author, or the GitHub App bot login ("<slug>[bot]") when
	// ai_author is empty and the hive authenticates as an App installation.
	// Display-only — the hub shows it in the spokes table but NEVER echoes it
	// back into project config, so an App-bot hive keeps ai_author empty and
	// stays bot-authored across restarts.
	AIAuthorEffective string `json:"ai_author_effective,omitempty"`
	// GitHubAPIURL is the GitHub API base URL this spoke is CURRENTLY running
	// against (its resolved value, so a public-GitHub spoke reports
	// https://api.github.com rather than ""). Reported so the hub can tell
	// whether a GitHub Enterprise API URL it wants to deliver has actually
	// landed. Without it the hub has no read-back for this field: the claim
	// reconcile in projectConfigForHiveID stops sending anything once
	// ClaimDelivered is set, so a hive whose GitHubHost is filled in AFTER
	// assignment (the retroactive repair) would never receive its GHE API URL.
	//
	// Empty means "spoke too old to report it" — the hub must treat that as
	// UNKNOWN, never as a genuine mismatch, or it would re-push forever.
	GitHubAPIURL string             `json:"github_api_url,omitempty"`
	ACMMLevel    int                `json:"acmm_level"`
	Agents       []AgentSummary     `json:"agents"`
	Governor     GovernorSummary    `json:"governor"`
	Tokens24h    int64              `json:"tokens_24h"`
	Contributors ContributorSummary `json:"contributors"`
	Leaderboard  []LeaderboardEntry `json:"leaderboard"`
	// ActiveSessionUsers is the DISTINCT set of GitHub usernames with a live
	// dashboard session on this hive at heartbeat time (spoke:
	// ActiveSessionUsernames). The hub credits each one the inter-beat interval so
	// per-user "time in hive" accumulates without needing a session-end event.
	// Non-secret: bare usernames only (no session ids/tokens/roles). omitempty so a
	// spoke too old to report it, or one with no live sessions, sends nothing —
	// which the hub reads as "credit no one this beat", never as an error.
	ActiveSessionUsers []string `json:"active_session_users,omitempty"`
	// EngagedSessionUsers is the subset of ActiveSessionUsers whose browser
	// reported ENGAGED presence recently — tab visible AND input within the
	// idle window (spoke: EngagedSessionUsernames, fed by /api/presence). It is
	// what separates a human actually using their hive from an idle open tab.
	// The hub credits these users engaged time and marks them engaged-now.
	// Non-secret: bare usernames only. omitempty + optional everywhere: a spoke
	// too old to report it sends nothing, which the hub reads as "no engagement
	// DATA this beat" — never as an error and never as "everyone idle forever"
	// (legacy fields keep accumulating regardless).
	EngagedSessionUsers []string `json:"engaged_session_users,omitempty"`
	// UserLastActions maps username → RFC3339 timestamp of that user's most
	// recent audit-logged REAL action on this hive (config save, agent
	// restart, ACMM change, login, …; pseudo-users like "system" are never
	// included — spoke: UserLastActions). The hub folds each into the user
	// record's LastActionAt, keeping the maximum, so spoke restarts and
	// multi-hive users resolve to the newest action anywhere. Non-secret:
	// usernames and timestamps only. omitempty/optional for old spokes.
	UserLastActions map[string]string `json:"user_last_actions,omitempty"`
	// StartedAt is the spoke process start time (RFC3339). The hub renders it
	// as an uptime pill so a hive that is quietly crash-looping — 1/1 Running
	// but restarted 35 times — is visible in My Hives instead of looking
	// healthy. A short uptime that keeps resetting is the tell.
	StartedAt string `json:"started_at,omitempty"`
	// Reporter identifies the spoke PROCESS that sent this beat as
	// "<pod-name>/<pid>" (hostname/PID; older spokes send the bare hostname).
	// It exists because two spoke instances can report as the same hive_id —
	// a stale ReplicaSet pod surviving a rollout, an orphaned duplicate
	// deployment, or (#2453/#2496) a SECOND PROCESS in the same pod — and
	// their alternating states made the dashboard flip every beat with
	// nothing naming the culprit. The PID suffix is what makes the same-pod
	// case visible: two processes in one pod share a hostname, so a bare pod
	// name kept the hub's duplicate-spoke detector silent while the registry
	// churned. The hub treats the whole string as an opaque identity, so old
	// and new formats interoperate. Empty means the spoke is too old to
	// report it: UNKNOWN, never evidence of anything.
	Reporter string `json:"reporter,omitempty"`
	// AdvisoryLastPostedAt is when this spoke last SUCCESSFULLY posted/updated
	// its advisory-digest issue (RFC3339). It is the mirror of StartedAt for the
	// advisory path: the hub renders a "stale advisory" pill so a hive that
	// SHOULD be posting advisory digests but has quietly stopped (working App,
	// advisory agents, but the digest went stale) becomes visible in My Hives
	// instead of needing a per-hive log sweep.
	//
	// Empty means the spoke has NEVER posted a digest — either it is not in the
	// advisory-posting business at all (pure PR/merge mode, no advisory agents,
	// so it never reaches the post path) or it is too old to report this field.
	// BOTH must be read by the hub as UNKNOWN and NEVER as a stale alarm — the
	// same rule the codebase already applies to StartedAt/GitHubAPIURL. Only a
	// hive that HAS posted at least once, and then stopped, can trip staleness.
	AdvisoryLastPostedAt string `json:"advisory_last_posted_at,omitempty"`
	// AdvisoryError is the log-safe error string from the spoke's most recent
	// FAILED advisory-post attempt (403 issues:write, rate limit, auth failure),
	// or empty when the last attempt succeeded. It is the same string the spoke
	// already logs, so it never carries key material. Set = the hub flags the
	// digest as stale (gated on advisory-mode + app-can-write) with this cause.
	AdvisoryError string `json:"advisory_error,omitempty"`
	// InferenceAuthError is the log-safe cause string set when this spoke's
	// self-hosted inference backend (LiteLLM/vLLM/llm-d gateway) has rejected
	// several CONSECUTIVE calls with 401 — a stale/invalid gateway key that
	// silently halts every agent while the hive still heartbeats and looks
	// "quiet". Empty means inference auth is healthy (or the spoke does not
	// route through an inference backend, or is too old to report it): the hub
	// reads empty as no-signal, never an alarm. Non-empty raises a dedicated
	// inference-auth alert whose ROOT cause an operator sees directly, distinct
	// from a GitHub-post advisory staleness. It clears the moment an inference
	// call succeeds, so a fixed hive self-heals. Never carries key material.
	InferenceAuthError string         `json:"inference_auth_error,omitempty"`
	Health             map[string]any `json:"health"`
	DashboardURL       string         `json:"dashboard_url"`
	SnapshotURL        string         `json:"snapshot_url"`
	Owner              string         `json:"owner,omitempty"`
	HiveType           string         `json:"hive_type,omitempty"`
	ClusterID          string         `json:"cluster_id,omitempty"`
	IsPublic           bool           `json:"is_public"`
	Version            string         `json:"version"`
	GitHash            string         `json:"git_hash"`
	GitBranch          string         `json:"git_branch,omitempty"`
	// ImageRef is the container image this spoke's own Deployment runs, read
	// in-cluster from the Deployment spec. GitHash says which commit the
	// BINARY was built from; ImageRef says which TAG the deployment tracks —
	// and only the tag reveals a hive pinned to an immutable
	// ghcr.io/kubestellar/hive:<sha> that can never receive a rolling upgrade.
	// The hub cannot read this itself for firewalled spokes it reaches only by
	// heartbeat, which is why it rides the payload. Empty when the spoke is
	// not running in-cluster or the read failed — never a guess.
	ImageRef string `json:"image_ref,omitempty"`
	// GitHubHost is the bare hostname of the GitHub instance this spoke is
	// ACTUALLY configured against (github.com, github.ibm.com, …), derived
	// from its own runtime github.base_url. This is the authoritative value:
	// a hive's GitHub can differ from its cluster's default, so the cluster
	// default is only ever a fallback for spokes too old to report this.
	// Empty on such spokes — the hub falls back rather than guessing.
	GitHubHost         string `json:"github_host,omitempty"`
	Timestamp          string `json:"timestamp"`
	GitHubAppRequired  bool   `json:"github_app_required,omitempty"`
	GitHubAppPermIssue string `json:"github_app_perm_issue,omitempty"`
	// RepoTargetMisconfigured carries an operator-facing config-shape issue
	// detected by the spoke. It is visibility only: the spoke keeps running and
	// the hub does not rewrite the project fields.
	RepoTargetMisconfigured bool   `json:"repo_target_misconfigured,omitempty"`
	RepoTargetIssue         string `json:"repo_target_issue,omitempty"`
	// GitHubAppState is the spoke's classification of WHY App auth is failing
	// (a github.AppAuthState token: "key-missing", "key-invalid",
	// "not-installed", "wrong-installation", "insufficient-permissions").
	// The hub uses it to avoid nudging — let alone threatening de-provisioning
	// — a hive whose credentials the OPERATOR has not delivered. Empty from a
	// spoke too old to report it, which must be read as "cannot tell".
	GitHubAppState   string `json:"github_app_state,omitempty"`
	AutoUpgrade      bool   `json:"auto_upgrade,omitempty"`
	Upgrading        bool   `json:"upgrading,omitempty"`
	UpgradeTargetSHA string `json:"upgrade_target_sha,omitempty"`
	// UpgradeFailed / UpgradeError let a spoke tell the hub that an instructed
	// upgrade did NOT land. Without them a failed upgrade is indistinguishable
	// from a slow one: the hub keeps re-instructing and the UI shows a permanent
	// "Upgrading" spinner that is simply a lie.
	UpgradeFailed           bool                          `json:"upgrade_failed,omitempty"`
	UpgradeError            string                        `json:"upgrade_error,omitempty"`
	PendingGitHubAppInstall bool                          `json:"pending_github_app_install,omitempty"`
	ClusterHealth           *HeartbeatClusterHealthReport `json:"cluster_health,omitempty"`
	// Fleet contribution counts — the spoke's AI-author PR activity across its
	// org, computed on a timer and cached (never per-heartbeat). The hub sums
	// these across all public, non-stale hives for the landing page's live
	// fleet-stats strip. Pointers so a spoke that hasn't computed them yet
	// (old spoke, or first-collect still pending) is distinguishable from a
	// genuine zero — nil means "no data, don't count me" so the hub never
	// aggregates a not-yet-computed zero into a fabricated fleet total.
	PRsMerged90d   *int `json:"prs_merged_90d,omitempty"`
	PRsRejected90d *int `json:"prs_rejected_90d,omitempty"`
	CVEsClosed     *int `json:"cves_closed,omitempty"`
	// FleetStatsCollectedAt (RFC3339) is when the counts above were last
	// successfully computed, so the hub can age out a stale contribution
	// instead of summing a frozen number forever. Empty from a spoke too old
	// to report it, which the hub treats as "not stale".
	FleetStatsCollectedAt string `json:"fleet_stats_collected_at,omitempty"`
	// AgentsWithModel counts this hive's agents that have an effective method
	// (backend) or model assigned — override first, then agent config, exactly
	// as the launcher resolves it. The hub uses it for user-journey stage
	// detection: a hive with zero configured agents has not yet completed the
	// "assign a method/model to an agent" step.
	//
	// A pointer for the same reason as the fleet counts above: a spoke too old
	// to report this sends nil, which the hub must read as "unknown, do not
	// nudge", NOT as a genuine zero. Never threaten a hive over a signal that
	// was never sent.
	AgentsWithModel *int `json:"agents_with_model,omitempty"`
	// GatewayNames lists the model gateways this spoke currently has configured.
	//
	// It is the READ-BACK for a hub-funded gateway. Without it the hub drained
	// its pending record the moment it put the gateway on the wire, so a
	// delivery lost to a dropped beat, a hub restart, or a spoke-side
	// ApplyDeliveredGateway error was gone for good — the user paid and the
	// gateway never arrived, with nothing left to retry from.
	//
	// nil/empty means the spoke is too old to report (UNKNOWN, never "has
	// none"), so an absent list must not be read as a failed delivery.
	GatewayNames []string `json:"gateway_names,omitempty"`
	// GitHubAppKeyFingerprint is a NON-SECRET identifier for the GitHub App
	// private key this spoke currently holds — "sha256:<hex>" over the DER
	// public key derived from it (config.AppKeyFingerprint). It exists so the
	// hub can tell a spoke carrying the WRONG key apart from one carrying the
	// right one, without the private key ever travelling spoke → hub. The
	// private key is never placed in this payload, in any field, ever.
	//
	// Empty means the spoke has no key, could not parse the one it has, or is
	// too old to report this. All three are repaired the same way: the hub
	// pushes its cluster key, and the spoke starts reporting a fingerprint that
	// matches, at which point pushing stops.
	GitHubAppKeyFingerprint string `json:"github_app_key_fingerprint,omitempty"`
	// GitHubAppKeyPerHive is true when this spoke's key was supplied
	// specifically for THIS hive at provisioning time, rather than adopted from
	// its cluster. The hub honours it as an override: a deliberate per-hive
	// credential is never overwritten by the cluster default.
	GitHubAppKeyPerHive bool `json:"github_app_key_per_hive,omitempty"`
	// GitHubAppID is the App ID this spoke believes it is authenticating AS —
	// cfg.GitHub.AppID, non-secret. It exists to make one specific distinction
	// decidable on the hub: a per-hive key that is WRONG versus a per-hive key
	// that is deliberately for a DIFFERENT App.
	//
	// A JWT is signed by the key and presented as app_id. If a spoke claims the
	// cluster's app_id but holds a key whose fingerprint is not the cluster
	// key's, that pair provably cannot authenticate — GitHub rejects it before
	// any permission check ("A JSON web token could not be decoded"). If the
	// spoke claims a DIFFERENT app_id, its key is presumed correct for that
	// other App and the hub must not touch it.
	//
	// Zero means the spoke is too old to report, or has no App configured. Both
	// read as "unknown": the hub falls back to the conservative per-hive
	// override and declines to overwrite. Never infer a mismatch from silence.
	GitHubAppID int64 `json:"github_app_id,omitempty"`
	// GitHubAppSlug and GitHubInstallationID complete the spoke's report of the
	// GitHub identity it is actually running.
	//
	// WHY THEY EXIST
	//
	// A GitHub App identity is an ATOMIC SET — app_id, app_slug, api_url and
	// base_url must all name the same forge. The hub pushes app_id and app_slug
	// (HeartbeatGitHubAppConfig) and api_url (HeartbeatProjectConfig), but until
	// now it received back only app_id and the key fingerprint. Two of the
	// pushed fields were therefore STRUCTURALLY UNCONFIRMABLE: the hub could
	// push a slug or an installation id and had no way, ever, to learn whether
	// it landed.
	//
	// That is not a theoretical gap. A cluster-config change pushed a GHE
	// app_id to a set of hives WITHOUT a matching api_url. The spokes ended up
	// with a GHE App ID pointed at api.github.com and every token request
	// failed:
	//
	//	POST https://api.github.com/app/installations/<id>/access_tokens
	//	404 Integration not found
	//
	// The hives that also received api_url=https://github.ibm.com/api/v3 were
	// fine — the failure split exactly on that one field. With only app_id
	// reported back, a half-applied identity is invisible to the hub: it looks
	// like a successful delivery. Reporting the whole set is what makes the
	// inconsistency detectable, and is a prerequisite for ever confirming a
	// delivery of it.
	//
	// Both are NON-SECRET. Empty/zero means "too old to report, or not
	// configured" — read as UNKNOWN, never as a mismatch. Never infer a
	// half-applied identity from silence; see IdentitySetIssues.
	GitHubAppSlug string `json:"github_app_slug,omitempty"`
	// GitHubInstallationID is the installation the spoke is using. It is
	// forge-scoped: an ID issued by one forge names nothing on another, which is
	// why a forge switch deliberately never carries it across.
	GitHubInstallationID int64 `json:"github_installation_id,omitempty"`
	// GitHubBaseURL is the web base URL the spoke runs against (github.base_url).
	// It is reported but never pushed — the spoke derives its host from
	// base_url with a fallback to api_url (GitHubConfig.HostLabel), so pushing
	// api_url alone is sufficient to move a hive between forges. It is reported
	// here so the hub can see the COMPLETE identity set and detect a base_url
	// that disagrees with api_url.
	GitHubBaseURL string `json:"github_base_url,omitempty"`
	// GitHubAppKeysHeld reports the NON-SECRET fingerprint of every ADDITIONAL
	// per-app-id key file this spoke holds on its PVC, keyed by app_id as a
	// decimal string (JSON object keys must be strings). It exists so the hub can
	// deliver the fleet's OTHER App keys (see HeartbeatGitHubAppConfig.
	// AdditionalKeys) exactly once and then stop: a key already present with the
	// right fingerprint is not re-sent every beat.
	//
	// It carries fingerprints ONLY — never key material — exactly like
	// GitHubAppKeyFingerprint above. Empty/nil means the spoke holds no per-app-id
	// keys, or is too old to report; either way the hub falls back to delivering
	// any additional keys it has, which the spoke writes idempotently.
	GitHubAppKeysHeld map[string]string `json:"github_app_keys_held,omitempty"`
}

type StatusCollector func() *HeartbeatPayload

// UpgradeCallback is called when the hub instructs this hive to upgrade
// to a specific SHA via the heartbeat response.
// RestartSpokeCallback handles a hub-requested rolling restart of this spoke
// (HeartbeatResponse.RestartSpoke). The callback owns the uptime guard.
type RestartSpokeCallback func()

type UpgradeCallback func(targetSHA string)

func StartHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, interval time.Duration, logger *slog.Logger, callbacks ...any) {
	if hubURL == "" {
		logger.Info("hub heartbeat disabled (no HIVE_HUB_URL)")
		return
	}
	// Durable single-loop guard (#2453): exactly one heartbeat loop may run in
	// this process, ever, no matter which path calls StartHeartbeat. A refused
	// duplicate returns cleanly — the already-running loop keeps beating — and
	// logs the refused caller's full stack so a live occurrence names the
	// trigger instead of just alternating states on the wire.
	if !heartbeatLoopActive.CompareAndSwap(false, true) {
		logger.Error("duplicate StartHeartbeat REFUSED: a heartbeat loop is already running in this process (#2453) — the stack below names the caller that tried to start a second one",
			"hub_url", hubURL,
			"stack", string(debug.Stack()),
		)
		return
	}
	// Release only when THIS loop exits (context cancelled), so a deliberate
	// stop-then-restart works while concurrent duplication stays impossible.
	defer heartbeatLoopActive.Store(false)
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
	var onRestartSpoke RestartSpokeCallback
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
		if resp.RestartSpoke && onRestartSpoke != nil {
			onRestartSpoke()
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

// waitForReady's knobs are vars (not consts) for the same reason as
// taskPushInterval: tests need a StartHeartbeat that reaches its send loop in
// milliseconds, not after a 3-minute readiness wait. Production keeps the
// defaults.
var (
	waitForReadyPollInterval = 5 * time.Second
	waitForReadyMaxWait      = 3 * time.Minute
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
					resp.Body.Close()
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
	// Record the attempt before anything that can bail out early (including a
	// nil payload from collect): reaching this point proves the loop goroutine
	// is still running its schedule, which is exactly what liveness needs to
	// know. Success is tracked separately, further down, on hub acceptance.
	recordHeartbeatAttempt()

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

// ReportUpgradeFailure tells the hub that an instructed upgrade failed, with
// the underlying cause. This is the counterpart to SendUpgradingHeartbeat: that
// one says "I am upgrading", this one says "I could not". Without it the hub
// only ever learns about upgrades that succeed, so a wedged spoke stays
// "Upgrading" forever while the hub silently re-instructs it every heartbeat.
//
// Best-effort and non-fatal: the caller is usually seconds from exiting, and a
// failure to report must never mask the upgrade failure itself.
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
	if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn("could not report upgrade failure to hub", "error", err)
		return
	}
	defer resp.Body.Close()
	logger.Info("reported upgrade failure to hub", "target", targetSHA, "cause", cause)
}

// HeartbeatGitHubAppConfig carries GitHub App credentials from the hub to
// a spoke via the heartbeat response, bypassing OAuth proxies that block
// direct HTTP pushes.
type HeartbeatGitHubAppConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key,omitempty"`
	// AppSlug is the App's URL slug on this cluster's GitHub host. It is only
	// set at PROVISIONING time today, so a hive provisioned before its cluster's
	// slug was configured shows an install link pointing at an app that does not
	// exist on that GitHub Enterprise host — and no code path ever corrects it.
	// Riding it alongside the key lets the same reconcile repair both.
	//
	// Empty means "leave the spoke's slug unchanged" — never a way to blank a
	// working value.
	AppSlug string `json:"app_slug,omitempty"`
	// ResetInstallation tells the spoke to CLEAR its installation_id.
	//
	// A separate flag because zero cannot carry this meaning: the spoke adopts
	// an installation only when the pushed value is non-zero, since zero means
	// "not speaking to this field". Overloading it would make every push that
	// omits an installation blank a working one.
	//
	// The spoke clearing its installation makes HasUsableApp() false, which
	// raises githubAppRequired — so the owner is prompted to install the App
	// again AND the 2-minute self-heal ticker starts, whose RediscoverAndAdopt
	// finds the correct installation for whichever App is installed.
	ResetInstallation bool `json:"reset_installation,omitempty"`
	// APIURL and BaseURL complete the identity SET.
	//
	// WHY THEY ARE HERE AND NOT ON ProjectConfig
	//
	// app_id, app_slug, api_url and base_url are ONE value: an App ID presented
	// to the wrong forge returns "404 Integration not found". Until now the App
	// half travelled on this channel and api_url travelled on
	// HeartbeatProjectConfig, dispatched independently to a different callback.
	// Nothing composed them, so nothing could validate them together — and a
	// forge switch (saas.go, pendingForgeAPIURL) deliberately sends api_url
	// ALONE, which is precisely the operation that changes identity. Between
	// those beats the spoke holds a half-identity: exactly the 404 shape that
	// took 26 hives down on 2026-07-31.
	//
	// Carrying them here lets the whole set be built and validated in one place,
	// and lets the spoke adopt it atomically instead of in two unordered halves.
	//
	// Empty means "leave the spoke's value unchanged", the same contract as
	// AppSlug — never a way to blank a working URL. Note that empty is ALSO the
	// correct steady state for a public-GitHub hive (~41 of 50 spokes run that
	// way), which is why the spoke must not infer "public" from silence here;
	// the App ID is the field that names the forge.
	APIURL  string `json:"api_url,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// AdditionalKeys carries EVERY OTHER GitHub App private key the fleet knows,
	// keyed by its own app_id, so a spoke can hold both the github.com App key
	// AND its cluster's GitHub Enterprise App key at once — and pick whichever
	// one matches the app_id it is actually configured to authenticate as.
	//
	// WHY THIS EXISTS
	//
	// The AppID/PrivateKey pair above is the spoke's CLUSTER key: the key of the
	// App registered on the cluster's GitHub host. That is wrong for a github.com
	// hive that happens to land on a GitHub-Enterprise-default cluster (the live
	// vllm-d case): it inherits the GHE app_id and GHE key, holds no github.com
	// key at all, and every github.com repo call dies with "github auth token
	// error". Delivering the OTHER app's key too — written to a distinct
	// per-app-id file on the spoke — lets that hive authenticate as the App it is
	// really pinned to, regardless of which cluster it runs on.
	//
	// It is purely additive: a spoke that only understands the single AppID/
	// PrivateKey pair (an older build) ignores this field and behaves exactly as
	// before. Each entry's PrivateKey is a SECRET value and travels only over the
	// TLS heartbeat channel; it is never logged. nil/empty means "nothing extra
	// to deliver".
	AdditionalKeys []HeartbeatAppKey `json:"additional_keys,omitempty"`
}

// HeartbeatAppKey is one (app_id, private key) pair the hub delivers alongside
// the spoke's primary cluster key so the spoke can authenticate as an App other
// than its cluster's default. The spoke writes it to a per-app-id key file and
// selects it when its own configured app_id matches AppID.
type HeartbeatAppKey struct {
	// AppID is the numeric GitHub App ID this key signs for. The spoke uses it
	// both to name the on-disk key file and to decide which key to sign with.
	AppID int64 `json:"app_id"`
	// PrivateKey is the PEM private key VALUE for AppID. Secret — TLS-channel
	// only, never logged, written to a 0600 file on the spoke.
	PrivateKey string `json:"private_key"`
	// Fingerprint is the NON-SECRET fingerprint of PrivateKey
	// (config.AppKeyFingerprint). It rides along so the delivery is auditable
	// from fingerprints alone, without the key ever appearing in a log line.
	Fingerprint string `json:"fingerprint,omitempty"`
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
	// AIAuthor is the GitHub account the agents open PRs as. It rides the same
	// channel as org/repos because it is part of the same project identity, and
	// because without it the hub's echo of this struct silently reset the
	// spoke's project.ai_author to "" on every beat — which disabled the
	// fleet-stats collector fleet-wide (Start() returns early on an empty
	// author) and left the public landing page's stats strip blank. Empty =
	// leave the spoke's configured author unchanged.
	AIAuthor string `json:"ai_author,omitempty"`
	// DashboardURL is the claimed hive's vanity URL. The spoke adopts it as its
	// hub.dashboard_url and reports it in subsequent heartbeats, so the hub
	// registry's dashboardUrl becomes the vanity URL (rather than the raw
	// placeholder subdomain) — and stays that way, since it's now the value the
	// heartbeat carries. Empty = leave the spoke's dashboard URL unchanged.
	DashboardURL string `json:"dashboard_url,omitempty"`
	// GitHubAPIURL points the spoke at a GitHub Enterprise API when the request
	// named a GHE org (github.ibm.com/my-org). Without it a GHE hive silently
	// talks to api.github.com and 404s on every repo call, which is what drove
	// users to paste the host into the repo field instead — the malformed value
	// then failed heartbeat validation and crash-looped the pod.
	//
	// Empty = leave the spoke's github.api_url unchanged (public github.com is
	// the spoke's own default), so this never blanks a working config.
	GitHubAPIURL string `json:"github_api_url,omitempty"`
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
	SwitchToTag string `json:"switch_to_tag,omitempty"`
	// RestartSpoke instructs the spoke to rolling-restart its own deployment
	// (RolloutRestartSelf) without changing the image. It is the remote kill
	// switch for a duplicate spoke instance on a cluster the hub cannot reach:
	// an upgrade can't help (the instance is already at the target SHA), only
	// a restart sheds it. Delivered to every beat inside a bounded arm window
	// so ALL instances reporting as this hive receive it; the spoke's own
	// uptime guard keeps a freshly restarted process from acting on the same
	// window twice.
	RestartSpoke    bool                      `json:"restart_spoke,omitempty"`
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

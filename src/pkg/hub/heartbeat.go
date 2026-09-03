package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/tracing"
)

const (
	// heartbeatTimeout bounds BOTH the collect() budget and the POST to the hub.
	//
	// DELIBERATELY NOT RAISED. A longer budget does not decouple liveness from
	// third-party availability — it only moves the threshold at which a slow or
	// rate-limited api.github.com starts making healthy spokes look dead, at the
	// cost of a slower heartbeat loop for everyone. The fix for that coupling is
	// structural (send an identity-only beat when no stats are available; see
	// minimalLivenessPayload), so 10s stays: long enough for a healthy collect,
	// short enough that the loop keeps ticking well inside staleThreshold.
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

// lastGoodPayload caches the most recent payload a collect() actually
// produced. It exists so a beat whose collect() TIMED OUT can still be sent —
// carrying slightly-stale stats — instead of being dropped entirely.
//
// WHY (offline regression from the #3743 collect-timeout fix): when /data
// (NFS RWX PVC) wedges, the agent-manager mutex is held across the stuck write
// and collect() -> AllStatuses() blocks every cycle. #3743 bounded collect()
// so the loop keeps ticking (liveness stays green), but on timeout it returned
// nil and sendHeartbeat SKIPPED THE ENTIRE BEAT — POSTing nothing to the hub.
// On a persistently-wedged spoke every beat's collect() times out, so every
// beat was skipped, the hub received no heartbeats, and after the stale
// threshold the UI marked an alive, 1/1-Ready hive OFFLINE. Caching the last
// good payload lets the spoke keep beating (marked StatsStale) through the
// stall so the hub keeps it online.
//
// Guarded by its own mutex because the collect goroutine writes it (on
// success) while the sender reads it (on timeout); the two can overlap when a
// late-returning wedged collect finally completes. A whole-payload clone is
// stored so a later in-place mutation of the returned payload (name filtering,
// timestamp stamping in sendHeartbeat) never races a reader of the cache.
var lastGoodPayload = struct {
	sync.Mutex
	payload *HeartbeatPayload
}{}

// heartbeatIdentity holds the CHEAP, purely-local identity of this spoke — the
// fields that come straight from config and never require a network call.
//
// WHY (the GitHub-dependent-liveness outage): the last-good cache above can
// only help a spoke that has completed at least one collect. A spoke that has
// just restarted has no cache, and collect() reaches api.github.com (owner
// token validation, plus MTTR/actionable-item enumeration feeding the same
// pass). On a hive with real repos that enumeration exceeds the 10s collect
// budget on EVERY early cycle, so `cached == nil` skipped every beat and a
// perfectly healthy spoke read OFFLINE until it happened to get one fast
// collect. Observed exactly this: placeholder hives (nothing to enumerate)
// recovered instantly while named tenants with real repos stayed offline.
//
// Liveness is a property of THIS process, not of GitHub's availability. So we
// publish identity as soon as it is known (before any collect runs) and use it
// to send a minimal beat when there is nothing else to send. A third-party API
// being slow, rate-limited or down must never make the fleet look dead.
//
// Guarded by a mutex for the same reason as lastGoodPayload: written once at
// loop start, read by the sender goroutine on every timed-out beat.
var heartbeatIdentity = struct {
	sync.Mutex
	payload *HeartbeatPayload
}{}

// publishHeartbeatIdentity records the collect-independent identity of this
// spoke so a beat can always be addressed to the right hive, even when no
// collect has ever succeeded. Only non-empty identities are stored.
func publishHeartbeatIdentity(p *HeartbeatPayload) {
	if p == nil || p.HiveID == "" {
		return
	}
	c := *p
	heartbeatIdentity.Lock()
	heartbeatIdentity.payload = &c
	heartbeatIdentity.Unlock()
}

// PublishHeartbeatIdentity registers this spoke's collect-independent identity
// (hive id, org, primary repo, repos, reporter, started-at, git hash) so the
// heartbeat loop can send a liveness beat before — or without ever — completing
// a stats collect.
//
// PrimaryRepo and Repos are part of this identity for the same reason Org is:
// they come straight from config, require no network call, and the hub rebuilds
// its registry entry from each payload verbatim — an identity beat that omitted
// them blanked the entry's primaryRepo/repos (org set, name "org/"), which broke
// the public-directory row (no repo link) for the whole window until the first
// successful collect.
//
// Call this as soon as config is loaded, BEFORE StartHeartbeat. It is the piece
// that makes liveness independent of GitHub: without it, a spoke that restarts
// while GitHub is slow has nothing it can legitimately address to the hub.
func PublishHeartbeatIdentity(hiveID, org, primaryRepo string, repos []string, reporter, startedAt, gitHash string) {
	publishHeartbeatIdentity(&HeartbeatPayload{
		HiveID:      hiveID,
		Org:         org,
		PrimaryRepo: primaryRepo,
		// Defensive copy: the caller's slice is live config that a hub-delivered
		// project claim can mutate later; the stored identity must be a snapshot.
		Repos:     append([]string(nil), repos...),
		Reporter:  reporter,
		StartedAt: startedAt,
		GitHash:   gitHash,
	})
}

// minimalLivenessPayload builds a beat that asserts ONLY "this spoke process is
// alive and its loop is ticking", with no collected stats at all.
//
// It is marked StatsStale so the hub keeps the hive online and advances
// lastHeartbeat (the documented contract of that field) without treating the
// absent agent/fleet numbers as freshly-observed zeros. Returns nil when the
// identity is not yet known, which is the only genuinely unsendable case.
func minimalLivenessPayload() *HeartbeatPayload {
	heartbeatIdentity.Lock()
	id := heartbeatIdentity.payload
	heartbeatIdentity.Unlock()
	if id == nil {
		return nil
	}
	c := *id
	// No stats were collected this cycle. Carry identity only, and say so.
	c.StatsStale = true
	return &c
}

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
	// Pause provenance (#4041): WHO/WHY/WHEN, so the fleet view can tell a
	// deliberate owner quiesce ("paused by <owner> via dashboard, 3d ago")
	// from a malfunction. All empty on a non-paused agent; PausedBy is empty
	// when no human actor is known (login-detector, fleet-breaker, ...).
	// PausedAt is RFC3339. The audit trail on the spoke has always had this —
	// nothing the hub reads did, which is what turned a legitimate mass pause
	// into a multi-day incident investigation.
	PausedTrigger string `json:"pausedTrigger,omitempty"`
	PausedReason  string `json:"pausedReason,omitempty"`
	PausedBy      string `json:"pausedBy,omitempty"`
	PausedAt      string `json:"pausedAt,omitempty"`
	// NeedsLogin is true when the agent's CLI is sitting on a login /
	// device-code prompt. This is the "running but not logged in" state: the
	// session is alive, the CLI process is alive, and the agent still cannot do
	// any work. It is invisible in State, which reads "running" throughout.
	NeedsLogin bool `json:"needsLogin,omitempty"`
	// QuotaExhausted is true when the agent pane shows a provider/monthly quota
	// refusal ("monthly quota", Copilot Free requests exhausted, LiteLLM
	// budget_exceeded). Unlike NeedsLogin, re-authentication will not help; the
	// agent cannot buy tokens until the provider quota resets or is raised.
	QuotaExhausted bool `json:"quotaExhausted,omitempty"`
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
	// KickIntervalSec is the governor's CURRENT-mode interval for this agent.
	// It lets the hub distinguish a fault from a scheduled-cadence agent that
	// is correctly idle between kicks (flashsystems/ess: 2h/4h cadences looked
	// idle after the generic 45-minute pane-activity threshold).
	KickIntervalSec int64 `json:"kickIntervalSec,omitempty"`

	// --- Fleet-divergence signals (the EXPECTED and ABLE legs) ---
	// These let the hub show the gap between what the GOVERNOR expects running,
	// what is ACTUALLY running, and what is ABLE to fulfill its mission. They
	// are the minimum raw truths the hub cannot derive; every composite verdict
	// (run-state, blocked reason, stuck/impotent, rollup) is derived hub-side.
	// All are zero-valued on a legacy spoke that predates them, which the hub
	// reads as UNKNOWN — never as a fault.

	// ExpectedActive is true when the governor's CURRENT mode schedules this
	// agent on a kicking cadence right now (config.ExpectedActive): the mode's
	// cadence for this agent is not pause/off/0/"on demand" and the agent is
	// not on-demand. It is the EXPECTED leg — the governor's intent — which the
	// hub otherwise cannot see (it never receives per-mode Cadences).
	ExpectedActive bool `json:"expectedActive,omitempty"`
	// CanOpenIssue / CanOpenPR / CanMerge are the agent's capability at the
	// hive's current ACMM level and effective mode — the EXACT gates the spoke
	// enforces (AgentMode.CanCreateIssues/CanPush/CanMerge, the same checks
	// AuthorizePROpen/AuthorizeMerge use). Sent as booleans so the hub needs no
	// ACMM-level→mode mapping and the badge cannot claim a capability the relay
	// would refuse.
	CanOpenIssue bool `json:"canOpenIssue,omitempty"`
	CanOpenPR    bool `json:"canOpenPR,omitempty"`
	CanMerge     bool `json:"canMerge,omitempty"`
	// Backend is the agent's effective backend (honoring BackendOverride). The
	// hub needs it to interpret NeedsLogin (interactive CLI vs inference key)
	// and to phrase a blocked reason.
	Backend string `json:"backend,omitempty"`
	// Enabled distinguishes "configured and meant to run" from "not running":
	// a config-enabled-but-stopped agent is otherwise indistinguishable from an
	// unconfigured one, which is exactly the STUCK-vs-disabled ambiguity the
	// fleet view exists to resolve.
	Enabled bool `json:"enabled,omitempty"`
}

// AgentActivity is the per-agent liveness evidence the spoke has and the hub
// does not. It exists so the two heartbeat build sites in cmd/hive (the
// ordinary loop and the upgrade beat) fill AgentSummary identically — they
// were already duplicated line for line, and a signal that is only reported on
// one of the two paths is a signal that quietly disappears mid-upgrade.
type AgentActivity struct {
	Paused         bool
	PausedTrigger  string
	PausedReason   string
	PausedBy       string
	PausedAt       time.Time
	NeedsLogin     bool
	QuotaExhausted bool
	SessionMissing bool
	StartedAt      time.Time
	LastActivityAt time.Time
	KickInterval   time.Duration
	// Fleet-divergence signals — see the matching AgentSummary fields.
	ExpectedActive bool
	CanOpenIssue   bool
	CanOpenPR      bool
	CanMerge       bool
	Backend        string
	Enabled        bool
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
		PausedTrigger:  act.PausedTrigger,
		PausedReason:   act.PausedReason,
		PausedBy:       act.PausedBy,
		NeedsLogin:     act.NeedsLogin,
		QuotaExhausted: act.QuotaExhausted,
		SessionMissing: act.SessionMissing,
		ExpectedActive: act.ExpectedActive,
		CanOpenIssue:   act.CanOpenIssue,
		CanOpenPR:      act.CanOpenPR,
		CanMerge:       act.CanMerge,
		Backend:        act.Backend,
		Enabled:        act.Enabled,
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
	if act.KickInterval > 0 {
		as.KickIntervalSec = int64(act.KickInterval / time.Second)
	}
	return as
}

type GovernorSummary struct {
	Mode   string `json:"mode"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
	// WorkSource is the configured work source type ("github_projects",
	// "linear", "jira"). Empty for the default GitHub Issues source; the hub
	// dashboard only badges non-default sources.
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
	// DashboardTokenHash is the SHA-256 hex of this spoke's dashboard token
	// (DASHBOARD_AUTH_TOKEN), reported so the hub can keep its own record
	// (SaaSHive.DashboardTokenHash) of the credential spoke-relayed upgrade
	// requests prove themselves with — without the hub ever needing to read
	// the hive-secrets secret from the spoke's cluster (impossible on
	// pull-only clusters) and without the raw token ever riding the wire. The
	// beat itself is authenticated by the per-hive bearer, so only the hive
	// can set its own record — which is sound: the record only lets that same
	// hive's dashboard prove it is itself. omitempty: an old spoke, or one
	// with no token, sends nothing, and the hub keeps whatever it has.
	DashboardTokenHash string `json:"dashboard_token_hash,omitempty"`
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
	// OpenFDs is the spoke process's open file-descriptor count at beat time,
	// with FDSoftLimit the RLIMIT_NOFILE soft cap it is burning toward. They
	// exist because #3875's leak — tens of thousands of CLOSE_WAIT sockets to
	// GitHub — reached 92,962 FDs and total self-DoS with NOTHING surfacing
	// it; it took a manual /proc inspection to find. A gauge in the beat makes
	// the next FD leak a visible climbing number instead of an outage
	// forensics exercise. Zero means "could not be read / old spoke" and must
	// be treated as UNKNOWN, never as "no descriptors open".
	OpenFDs     int    `json:"open_fds,omitempty"`
	FDSoftLimit uint64 `json:"fd_soft_limit,omitempty"`
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
	// AdvisoryFindingCount is how many findings went out in the last digest this
	// spoke posted, and AdvisoryOverflowCount how many MORE exist that the
	// top-N cap withheld (0 when nothing was capped). Reported together so the
	// hub can render "12 findings (top 10)" — a bare count would read as the
	// whole picture when it is deliberately only the top of it.
	AdvisoryFindingCount  int `json:"advisory_finding_count,omitempty"`
	AdvisoryOverflowCount int `json:"advisory_overflow_count,omitempty"`
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
	InferenceAuthError string `json:"inference_auth_error,omitempty"`
	// ProviderLimitReason is the spoke's current provider-side spending/quota
	// refusal banner (distinct from hive-local governor budget). Empty means no
	// signal or an old spoke. ProviderLimitRebuffs counts matched refused calls
	// while latched, when known.
	ProviderLimitReason  string         `json:"provider_limit_reason,omitempty"`
	ProviderLimitRebuffs int            `json:"provider_limit_rebuffs,omitempty"`
	Health               map[string]any `json:"health"`
	DashboardURL         string         `json:"dashboard_url"`
	// PublicURLSelfCheck is the spoke's own end-to-end probe of the dashboard
	// URL it is advertising to the hub. It exists because the hub's public
	// network can be the wrong vantage point for private-network hives: a URL
	// may be alive from the spoke's network while timing out from the public
	// OKE hub. Nil means an older spoke did not report the signal, so the hub
	// treats it as UNKNOWN rather than a failure.
	PublicURLSelfCheck *PublicURLSelfCheck `json:"public_url_self_check,omitempty"`
	// RouteExists is the spoke's in-cluster confirmation that an Ingress or
	// OpenShift Route exists for DashboardURL's host in the spoke namespace.
	// Missing is actionable; unknown (old spoke, no RBAC, no in-cluster API) is
	// intentionally not.
	RouteExists *RouteExistenceCheck `json:"route_exists,omitempty"`
	SnapshotURL string               `json:"snapshot_url"`
	Owner       string               `json:"owner,omitempty"`
	HiveType    string               `json:"hive_type,omitempty"`
	ClusterID   string               `json:"cluster_id,omitempty"`
	IsPublic    bool                 `json:"is_public"`
	Version     string               `json:"version"`
	GitHash     string               `json:"git_hash"`
	GitBranch   string               `json:"git_branch,omitempty"`
	// ImageRef is the container image this spoke's own Deployment runs, read
	// in-cluster from the Deployment spec. GitHash says which commit the
	// BINARY was built from; ImageRef says which TAG the deployment tracks —
	// and only the tag reveals a hive pinned to an immutable
	// ghcr.io/hivecommons/hive:<sha> that can never receive a rolling upgrade.
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
	// GitHubAppTokenStatus is the spoke's local view of the shared App token
	// cache used by GitHub writers ("ok", "stale", "missing", "error").
	// LastMintAt is the cache file mtime and Error is a log-safe detail.
	GitHubAppTokenStatus     string `json:"github_app_token_status,omitempty"`
	GitHubAppTokenLastMintAt string `json:"github_app_token_last_mint_at,omitempty"`
	GitHubAppTokenError      string `json:"github_app_token_error,omitempty"`
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

	// --- Per-repo output activity (hive-health) ---------------------------
	// RepoActivity is the spoke's audit-derived summary of output produced
	// back to each work source (issues/PRs/comments/merges/claims/reviews/
	// advisory) with counts + recency. It lets the hub decide, from the beat
	// alone, whether a hive is actually producing — banded by ACMM level in
	// the health verdict. Nil from a spoke too old to compute it (carried
	// forward hub-side), like ComponentReach.
	RepoActivity            []RepoActivityWire `json:"repo_activity,omitempty"`
	RepoActivityCollectedAt string             `json:"repo_activity_collected_at,omitempty"`
	RepoActivityWindowHours int                `json:"repo_activity_window_hours,omitempty"`
	// RepoActivityCountWindowHours is the lookback the per-repo counts were
	// accumulated over. RepoActivityWindowHours above is the FRESHNESS window
	// the health verdict uses; the two differ by 28x, so a rate computed
	// against the freshness window overstates activity (#4860).
	RepoActivityCountWindowHours int `json:"repo_activity_count_window_hours,omitempty"`

	// --- Quadrant signals -------------------------------------------------
	// Cheap, already-computed spoke metrics forwarded so the hub can score the
	// per-hive quadrant (trust / efficiency / satisfaction / productivity)
	// without asking spokes to do new work. Every one of these is read from
	// state the spoke already maintains on an existing timer — none costs a
	// GitHub API call, which matters because the fleet shares a search quota.
	//
	// All pointers. A nil from an older spoke means "not reported", which the
	// scorer treats as absent evidence rather than a measurement of zero — the
	// distinction the whole quadrant design rests on. That also makes this
	// block forward-compatible with no version negotiation.

	// BudgetCurrentSpend is tokens consumed since the CURRENT budget window
	// opened (governor's totalTokens minus the window baseline). Unlike
	// Tokens24h above — which despite its name is a lifetime cumulative total —
	// this is a genuine windowed delta, which is what makes a cost-per-outcome
	// ratio meaningful rather than merely rankable.
	//
	// It is uninterpretable without its window bounds: zero equally means "the
	// window just rolled" and "nothing was consumed". The two timestamps below
	// are therefore part of the same signal, not optional decoration, and the
	// window length is governor.budget.period_days (default 7 days) rather than
	// anything fixed — so consumers must normalise (e.g. per day) before
	// comparing against the 90-day PR counters above.
	BudgetCurrentSpend   *int64 `json:"budget_current_spend,omitempty"`
	BudgetLimit          *int64 `json:"budget_limit,omitempty"`
	BudgetWindowStartsAt string `json:"budget_window_starts_at,omitempty"`
	BudgetWindowEndsAt   string `json:"budget_window_ends_at,omitempty"`
	// BudgetExhausted reports that the governor is actively suppressing agent
	// kicks because the window's budget ran out. It is both an efficiency and a
	// productivity signal: throughput near zero means something very different
	// when the hive is being throttled than when it is merely idle.
	BudgetExhausted *bool `json:"budget_exhausted,omitempty"`
	BudgetIgnored   *bool `json:"budget_ignored,omitempty"`

	// HoldTotal is issues and PRs parked behind a hold label awaiting human
	// review, and AwaitingReview is plans blocked on a human decision. Both
	// measure the same thing from different ends: how much of this hive's work
	// is stalled on a person rather than on the agents.
	HoldTotal      *int `json:"hold_total,omitempty"`
	AwaitingReview *int `json:"awaiting_review,omitempty"`

	// --- Remediation-hint detectors (#5577) -------------------------------
	// Three silent-failure classes a 2026-09-01 fleet audit could only find by
	// exec'ing into pods. All follow the PRsMerged90d convention: absent means
	// "not measured" (old spoke, collector not warm) and is carried forward
	// hub-side — BUT, unlike the count pointers, a MEASURED empty result must
	// also be distinguishable from absent so a recovered hive clears its own
	// signal. These therefore omit `omitempty`: nil encodes as null ("not
	// measured", hub carries forward) while a measured all-clear encodes as
	// {} / [] and overwrites.
	//
	// AgentErrorStreaks maps agent name → consecutive failed model calls
	// (zero-usage turns from the token scanner's chat recordings — the #5338
	// bobshell crash-loop signal, where turns run, every call dies, and the
	// agent stays green).
	AgentErrorStreaks map[string]int `json:"agent_error_streaks"`
	// ConsentWedged lists agents whose kick path hit a consent-screen restart
	// in the last hour — the Copilot consent wedge that restarts an agent
	// ~1/min while it reads green.
	ConsentWedged []string `json:"consent_wedged"`
	// NoCadenceAgents lists enabled, governor-kickable agents with no cadence
	// configured in any mode AND no kick ever — agents that will idle forever
	// until the operator sets a cadence.
	NoCadenceAgents []string `json:"no_cadence_agents"`

	// SLAViolations is work aging past its service threshold, taken from the
	// governor's eval snapshot.
	SLAViolations *int `json:"sla_violations,omitempty"`

	// TasksCompleted7d is contributor-relay tasks completed in the last seven
	// days, summed on the spoke from its 168 hourly buckets. The buckets
	// themselves stay local: sending ~168 integers every two minutes to
	// reconstruct one number on the hub would be pure waste.
	//
	// A flat zero is a real measurement for a hive with no contributors, not a
	// gap — read it alongside Contributors above to tell the two apart.
	TasksCompleted7d *int `json:"tasks_completed_7d,omitempty"`

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
	// decimal string (JSON object keys must be strings).
	//
	// SECURITY (C1/N3, CWE-200/639): the hub no longer acts on this field. It once
	// drove the fleet-wide additional-key broadcast (HeartbeatGitHubAppConfig.
	// AdditionalKeys), which handed every tenant's App private key to any caller.
	// That lane was removed; the field is retained only so older spokes can keep
	// reporting it harmlessly and to describe what per-app-id files a spoke holds.
	// It carries fingerprints ONLY — never key material.
	GitHubAppKeysHeld map[string]string `json:"github_app_keys_held,omitempty"`
	// ComponentReach carries the spoke's in-process component reach counters
	// (#3993, phase 2a of #3973): per (component, running commit) span counts
	// that increment REGARDLESS of whether an OTel exporter is configured, so
	// every spoke reports — including the pull-only fleet a span backend can
	// never see (design D1/D2). The hub stores the latest report per hive,
	// storage only: nothing joins, maps, or renders it until #3994. Spoke-side
	// the entry count is capped at tracing.MaxReachComponents and the hub
	// clips oversized reports to the same cap on receive (a hostile spoke
	// must not grow hub memory). omitempty: a spoke that has recorded no
	// spans yet, or one too old to report reach, sends nothing — which the
	// hub reads as "no data", never as an error and never as zero reach.
	ComponentReach *tracing.ReachReport `json:"component_reach,omitempty"`
	// StatsStale is true when this beat carries CACHED collected stats rather
	// than fresh ones, because collect() timed out this cycle (see
	// sendHeartbeat / collectWithTimeout). The beat is still a genuine liveness
	// signal — the spoke process is alive and its loop is ticking — so the hub
	// MUST keep the hive online and advance lastHeartbeat; it should simply not
	// treat the agent/fleet/health numbers as freshly observed. Without this
	// beat the hub would receive nothing at all while /data (NFS) is wedged and
	// would mark an alive, 1/1-Ready hive OFFLINE after the stale threshold.
	//
	// omitempty so a healthy fresh beat, and every older spoke, sends nothing —
	// which the hub reads as "stats are fresh", never as an error.
	StatsStale bool `json:"stats_stale,omitempty"`
}

const (
	PublicURLSelfCheckOK      = "ok"
	PublicURLSelfCheckFail    = "fail"
	PublicURLSelfCheckUnknown = "unknown"

	publicURLSelfCheckMinConsecutiveFailures = 3

	RouteExistenceFound   = "found"
	RouteExistenceMissing = "missing"
	RouteExistenceUnknown = "unknown"
)

// PublicURLSelfCheck reports the spoke's own view of whether its dashboard
// listener is serving locally. CheckedAt is RFC3339 UTC. HTTPStatus is set only
// when an HTTP response arrived; Error is a terse operator-safe cause on fail.
type PublicURLSelfCheck struct {
	Status     string `json:"status"`
	CheckedAt  string `json:"checked_at,omitempty"`
	Error      string `json:"error,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// RouteExistenceCheck reports whether the spoke can confirm that its namespace
// contains an Ingress or OpenShift Route for the advertised dashboard host.
// Unknown is deliberately non-fatal: missing RBAC, local development, or an API
// transport error must never page as a missing route.
type RouteExistenceCheck struct {
	Status    string `json:"status"`
	CheckedAt string `json:"checked_at,omitempty"`
	Host      string `json:"host,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Error     string `json:"error,omitempty"`
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
			onAuthorizedUsers(resp.AuthorizedUsers, resp.AuthorizedUserNames)
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

	// servedHostProbeCache memoises the spoke's own served-host discovery on
	// the same cadence as the route-existence probe. The answer changes only
	// when an operator recreates the spoke's Route/Ingress, so re-listing the
	// namespace on every beat would be pure overhead against the API server.
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
	// Record the attempt before anything that can bail out early (including a
	// nil payload from collect): reaching this point proves the loop goroutine
	// is still running its schedule, which is exactly what liveness needs to
	// know. Success is tracked separately, further down, on hub acceptance.
	recordHeartbeatAttempt()

	// Bound collect() so a wedged collector cannot freeze the loop. collect()
	// reads the agent manager and governor state, and those reads take locks
	// (e.g. Manager.AllStatuses -> m.mu.RLock). If some other goroutine holds
	// m.mu.Lock() while blocked in an uninterruptible NFS write to the RWX
	// /data PVC (stale locks: "no locks available"), collect() blocks with no
	// timeout of its own. Because the heartbeat loop runs sendHeartbeat
	// synchronously in its for-select, a stuck collect() means sendHeartbeat
	// never returns, the loop never reaches its next tick, and
	// recordHeartbeatAttempt() stops advancing — after livezHeartbeatStallMax
	// (6 min) /api/livez reports the loop as stalled and kubelet kills a pod
	// that is actually alive (the NFS server is stuck, which a restart cannot
	// fix), producing a fleet-wide liveness crash-loop. The attempt above is
	// already recorded, so timing collect() out lets the loop keep ticking and
	// keeps liveness green while the process is genuinely alive.
	//
	// But a timed-out collect must NOT skip the whole beat: on a spoke whose
	// /data (NFS) is persistently wedged, collect() times out EVERY cycle, so
	// skipping every beat means the hub receives no heartbeats and marks an
	// alive, 1/1-Ready hive OFFLINE after the stale threshold (the offline
	// regression from #3743). Instead, on timeout we fall back to the LAST-GOOD
	// collected payload (cached below on every success), marked StatsStale, so
	// the hub still gets a live beat and keeps the hive online — the stats are
	// merely slightly stale, not absent. Only a spoke that has NEVER completed a
	// collect (the first beats) has no cache to fall back on; that alone skips.
	payload := collectWithTimeout(ctx, collect, heartbeatTimeout, logger)
	if payload == nil {
		cached := loadLastGoodPayload()
		if cached == nil {
			// No successful collect yet. This is NOT a reason to stay silent:
			// liveness is a property of this process, and collect() depends on
			// api.github.com (owner-token validation, MTTR/actionable-item
			// enumeration), which on a hive with real repos routinely exceeds
			// the collect budget on every early cycle after a restart. Skipping
			// here made healthy spokes read OFFLINE whenever GitHub was slow or
			// rate-limited — the fleet looking dead because a third party was.
			//
			// Send a minimal identity-only beat instead: no stats, marked
			// StatsStale, which the hub already treats as "keep this hive
			// online, don't trust the numbers".
			minimal := minimalLivenessPayload()
			if minimal == nil {
				// Identity genuinely unknown (loop started before config was
				// published). Nothing can be addressed to a hive yet.
				logger.Warn("hub heartbeat collect timed out and hive identity not yet known — skipping this beat; loop keeps ticking so liveness stays green")
				return nil
			}
			logger.Warn("hub heartbeat collect timed out with no cached payload — sending MINIMAL liveness beat (identity only, no stats) so the hub keeps this hive online; loop keeps ticking so liveness stays green")
			payload = minimal
			payload.Timestamp = time.Now().UTC().Format(time.RFC3339)
			return postHeartbeatToHub(ctx, hubURL, payload, logger)
		}
		// Send the cached stats so the hub keeps the hive online through the
		// stall. Mark them stale so the hub does not treat the agent/fleet
		// numbers as freshly observed.
		cached.StatsStale = true
		payload = cached
		logger.Warn("hub heartbeat collect timed out — sending LAST-GOOD cached stats (marked stale) so the hub keeps this hive online; loop keeps ticking so liveness stays green")
	} else {
		// Fresh collect: remember it (a clone, so the in-place filtering below
		// never mutates the cache) for the next time collect() times out.
		storeLastGoodPayload(payload)
		// Keep the collect-independent identity current from the authoritative
		// source, so a LATER restart-free identity change (e.g. git hash after
		// an upgrade) is reflected in any minimal beat we may need to send.
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

// postHeartbeat marshals and POSTs a beat to the hub, records success on
// acceptance, and decodes the hub's response.
//
// Named ...ToHub to avoid colliding with the pkg-local test helper postHeartbeat.
// Extracted from sendHeartbeat so the MINIMAL liveness beat (no cached payload,
// collect timed out) travels the EXACT same path as a normal beat: same auth
// header, same timeout, same recordHeartbeatSuccess accounting, same response
// handling — including hub-instructed upgrades. A separate ad-hoc POST would
// drift from this one over time and silently lose those behaviours.
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

// collectWithTimeout runs collect() with an upper bound so a collector wedged
// on a lock held by a blocked NFS write (see the call site in sendHeartbeat)
// cannot stall the heartbeat loop. It returns the payload if collect finishes
// within timeout, or nil (skip this beat) if it does not.
//
// A Go goroutine blocked in an uninterruptible syscall or on a held mutex
// cannot be cancelled, so a timed-out collect goroutine necessarily keeps
// running until its blocker clears — this cannot be avoided, only contained.
// It is contained here: the goroutine sends on a buffered (cap 1) channel, so
// the eventual send never blocks even though sendHeartbeat has already
// returned, and the late payload is simply dropped (GC'd with the channel).
// No double-record occurs: recordHeartbeatAttempt already ran in the caller,
// and this helper never records anything itself. If collect panics, the panic
// is recovered and reported as a nil (skipped) payload rather than crashing
// the heartbeat goroutine.
func collectWithTimeout(ctx context.Context, collect StatusCollector, timeout time.Duration, logger *slog.Logger) *HeartbeatPayload {
	// Buffered so the collect goroutine can always complete its send and exit
	// even after we have stopped waiting — prevents a permanent goroutine leak
	// once the blocker (NFS) finally releases.
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
			// The CALLER decides what to do with a timed-out collect (send
			// cached last-good stats, or skip on the first beats) — it logs the
			// outcome. Here we only note the timeout itself.
			logger.Warn("hub heartbeat collect timed out; loop keeps ticking so liveness stays green",
				"timeout", timeout.String())
		}
		return nil
	case <-ctx.Done():
		return nil
	}
}

// storeLastGoodPayload caches a deep clone of a payload that collect() produced
// successfully, for reuse when a later collect() times out. A clone is stored
// (not the pointer) so sendHeartbeat's in-place filtering/stamping of the live
// payload never mutates what a future timed-out beat will send.
func storeLastGoodPayload(p *HeartbeatPayload) {
	c := clonePayload(p)
	if c == nil {
		return
	}
	lastGoodPayload.Lock()
	lastGoodPayload.payload = c
	lastGoodPayload.Unlock()
}

// loadLastGoodPayload returns a deep clone of the last successfully-collected
// payload, or nil if collect() has never yet succeeded. A clone is returned so
// the caller can mutate it (set StatsStale, re-stamp Timestamp) without
// touching the cached copy that the next timed-out beat will reuse.
func loadLastGoodPayload() *HeartbeatPayload {
	lastGoodPayload.Lock()
	p := lastGoodPayload.payload
	lastGoodPayload.Unlock()
	return clonePayload(p)
}

// clonePayload deep-copies a HeartbeatPayload via JSON round-trip. JSON is the
// exact shape that goes on the wire, so a clone through it can never share a
// slice/map/pointer with the original — which is what makes the cache safe to
// read from the sender while the collect goroutine writes a fresh one. Returns
// nil for a nil input or on the (not expected in practice) marshal error.
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
	// Keep the public Host header while dialing the loopback listener so
	// virtual-host routing is still exercised without depending on DNS.
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

// inClusterHTTPClient builds the TLS client used to probe the in-cluster
// Kubernetes API. The serviceaccount CA is REQUIRED: it is the only root that
// can attest the API server, so a missing or malformed one must be a hard error
// rather than a silent fall-through to the system pool.
//
// The previous version swallowed both the read error and the AppendCertsFromPEM
// result, leaving a client whose only roots were the public system CAs. That
// could not verify the cluster-internal API certificate, so the probe failed
// anyway — but as an opaque x509 error at request time instead of a clear
// statement of what was actually wrong, and it left the caller unable to tell a
// misconfigured CA from a genuinely absent route. Every other
// AppendCertsFromPEM call site in the tree already checks its return value
// (dashboard/cost.go, dashboard/litellm_key_store.go, github/proxytrust.go,
// hub/cluster_metrics.go, proxy/inference_route.go); this was the sole outlier.
//
// routeExistenceProbe surfaces the returned error as RouteExistenceUnknown, so
// failing closed here degrades to "cannot determine", never to "route absent"
// and never to an unverified connection.
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

// spokeServedHost returns the external hostname the spoke's OWN Route or
// Ingress actually serves, read from the in-cluster Kubernetes API, or "" when
// it cannot be determined.
//
// This exists because the DashboardURL fallback used to synthesise
// "<hiveID>.<hub's host>", which silently assumes every spoke is fronted by the
// hub's own wildcard domain. That holds only for spokes co-located with the hub
// (the hub-reachable cluster, where *.hive.kubestellar.io IS the router). On any other cluster —
// an OpenShift pool on *.apps.<cluster>, an IKS pool — the synthesised host
// resolves, via the hub's wildcard, to the HUB's router, which has no backend
// for that name and answers 503. The hub then linked users at a hostname that
// could never work, while the spoke's real Route was serving fine all along.
//
// The spoke is the only party that can answer this on a pull-only cluster: the
// hub has no kubectl path there by design, so it cannot read the Route. The
// spoke reads its own namespace, which it can always do.
//
// Ingress is consulted before Route so a cluster carrying both resolves to the
// same object the traffic path uses. Ordering matches routeExistenceProbe.
//
// Returns "" on any error, on an unknown/unsupported API, or when no host is
// found — every caller must treat that as "no answer" and fall back, never as
// an assertion that no route exists.
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

// SpokeServedHost is the exported entry point to spokeServedHost, used by the
// spoke binary (cmd/hive) to resolve the dashboard URL it advertises to the hub
// when no dashboard_url is configured. Returns "" when the host cannot be
// determined; the caller must fall back rather than treat that as an answer.
func SpokeServedHost(ctx context.Context) string { return spokeServedHost(ctx) }

// discoverSpokeServedHost performs the uncached read behind spokeServedHost.
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

// ingressServedHost returns the first non-empty host on any Ingress rule in the
// spoke's namespace.
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

// routeServedHost returns the host of the spoke's dashboard Route.
//
// The dashboard Route is selected BY NAME (routeBaseDashboard) rather than by
// taking the first Route in the namespace, because the namespace also carries
// the terminal Route and any "<base>-vanity" mirrors. Picking arbitrarily would
// make the reported URL depend on list order and could advertise the terminal
// host as the dashboard. Only if no Route bears that name does this fall back
// to the first Route with a host, so a spoke provisioned under a different
// naming convention still reports something reachable rather than nothing.
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
	// AdditionalKeys formerly carried EVERY OTHER GitHub App private key the fleet
	// knew, keyed by app_id, so a spoke could hold keys for Apps other than its
	// cluster's default.
	//
	// SECURITY (C1/N3, CWE-200/639): the hub NO LONGER POPULATES this field. That
	// broadcast was the critical cross-tenant key-disclosure vulnerability — the
	// hub attached every tenant's App private key to every heartbeat, and because
	// the handler trusts the body-supplied hive_id under one fleet-shared bearer,
	// any hive could pull the whole fleet's keys by beating with any hive_id. A
	// heartbeat now delivers ONLY the caller hive's own App key/identity via the
	// AppID/PrivateKey pair above (the per-cluster reconcile) and any targeted
	// operator/webhook-queued identity. The field is kept in the wire struct only
	// so it deserializes to empty on both sides; a spoke that reads it (older
	// builds) simply never receives entries. It always arrives nil now.
	AdditionalKeys []HeartbeatAppKey `json:"additional_keys,omitempty"`
	// SecondaryKey delivers the ONE optional second App key this hive is
	// authorized to hold (#4815), so a cluster's second App — the optional
	// Visual Hive App, #4030 — can receive a credential without the primary key
	// having anywhere else to go.
	//
	// WHY THIS IS NOT AdditionalKeys REVIVED. AdditionalKeys is a LIST selected
	// from the fleet key set with no binding to the caller, which is exactly what
	// made it the CWE-200/639 cross-tenant disclosure. This is a SINGLE key
	// resolved from SaaSHive.SecondaryAppID on the hub's own record for the
	// authenticated hive (secondaryAppKeyForHive): the hive is handed the one App
	// an operator assigned it or nothing at all, and no value a spoke sends can
	// change which App that is. The singular type is part of the guarantee — a
	// list is the shape that invites "while we're here, send the others too".
	//
	// nil for every hive without a SecondaryAppID, which is the entire fleet
	// today, so the heartbeat payload is byte-identical for them.
	SecondaryKey *HeartbeatAppKey `json:"secondary_key,omitempty"`
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
// heartbeat-only spoke (e.g. the heartbeat-only cluster) over kubectl. Delivering it in the
// heartbeat response lets the spoke reconcile its running config
// (cfg.Project.Org/Repos/PrimaryRepo and ACMM level) every beat until it
// matches what the hub recorded — the only channel that works uniformly for
// both reachable (the hub-reachable cluster) and heartbeat-only (the heartbeat-only cluster) clusters.
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
	// IssueFilter delivers the per-hive project.issue_filter (the
	// require_labels allow-list gating which issues agents may initiate work
	// on; exclusion stays governor.labels.exempt, spoke-owned). It rides
	// this struct because it is part of the same project identity as
	// org/repos. nil = the hub is not speaking to this field — the spoke keeps
	// its locally configured filter, so the hub's every-beat echo can never
	// blank it (the AIAuthor lesson). A non-nil but empty filter is an
	// explicit clear.
	IssueFilter *config.IssueFilterConfig `json:"issue_filter,omitempty"`
}

// HeartbeatGatewayConfig carries an OpenRouter model gateway (funded via the
// hub's scan-to-fund flow) from the hub to a spoke via the heartbeat response.
// It mirrors HeartbeatProjectConfig: for a firewalled/heartbeat-only spoke
// (the heartbeat-only cluster) the hub cannot POST to the spoke's dashboard over the network, so it
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
	// ghcr.io/hivecommons/hive:<SwitchToTag> and restart. Used for branch
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
	// the heartbeat-only cluster) over kubectl, and those spokes authorize their own device-flow
	// logins against a local allowlist — so a grant made in the hub's Manage
	// Access UI would never reach them. Delivering the list in the heartbeat
	// response lets the spoke reconcile its allowlist every beat, so access
	// changes propagate automatically. nil means "hub sent nothing" (leave the
	// spoke's list unchanged); a non-nil (possibly empty) slice replaces it.
	AuthorizedUsers []string `json:"authorized_users,omitempty"`
	// AuthorizedUserNames is a PURELY COSMETIC companion to AuthorizedUsers: a
	// map from each entry's raw identity key (never the "username:role" wire
	// form — just the key before the ":role" suffix) to a human display name,
	// so the spoke's read-only Access tab can show "Jane Doe" instead of a raw
	// "ibmid:5500087VJB"/"google:1078…"/"microsoft:AAAA…" subject. It carries
	// NO authority — the spoke's allowlist match (AuthorizedRole) reads only
	// AuthorizedUsers/the key, and this map is never consulted for sign-in.
	// Sent whenever AuthorizedUsers is (nil means "leave the spoke's copy
	// unchanged", mirroring AuthorizedUsers' own nil semantics); a key absent
	// here, or mapped to "", simply has no known name yet and the spoke falls
	// back to the raw key.
	AuthorizedUserNames map[string]string `json:"authorized_user_names,omitempty"`
	// ProjectConfig is the claimed project's real org/repos/ACMM. The hub sets
	// it (mirroring AuthorizedUsers) when a placeholder hive has been assigned
	// to a user and the spoke is still reporting its old (placeholder) project.
	// The hub keeps sending it every beat until the spoke reconciles and reports
	// the matching project. nil means "no reconcile needed" — leave the spoke's
	// project config alone. This is the ONLY delivery channel for heartbeat-only
	// clusters (the heartbeat-only cluster) the hub cannot reach over kubectl.
	ProjectConfig *HeartbeatProjectConfig `json:"project_config,omitempty"`
	// PendingGateway carries a funded model gateway (OpenRouter scan-to-fund) the
	// hub minted on the spoke's behalf. It is the delivery channel for
	// firewalled/heartbeat-only spokes (the heartbeat-only cluster) the hub cannot POST to directly.
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
type AuthorizedUsersCallback func(users []string, names map[string]string)

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

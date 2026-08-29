// ClankeR — the contributor relay. This file is the hive-side half: the
// WebSocket endpoint that authenticates contributor agents, dispatches tasks
// (issue fixes, reviews, docs) to whichever machine is connected, and keeps
// GitHub tokens fresh for the duration of a task. The contributor-side half
// lives in bin/contributor-relay.sh.
//
// Names on the wire (message types, JSON fields, the /contribute route) are
// deliberately unchanged: ClankeR is the presentation name, not the protocol.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kubestellar/hive/pkg/advisory"
	"github.com/kubestellar/hive/pkg/config"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/worksource"
)

const (
	wsHeartbeatInterval = 30 * time.Second
	wsHeartbeatTimeout  = 90 * time.Second
	// wsTaskTimeout is the hub-owned LEASE TTL on task ownership (kubestellar/hive
	// #2568). A task's lease is renewed on assignment and on every task_progress
	// report; if a connection holds a task but the lease has not been renewed within
	// this window the cleanupLoop auto-releases it through the SAME cooldown path the
	// disconnect/ready-abandon/manual-requeue releases use, so a wedged-but-connected
	// worker cannot hold an issue forever. It is deliberately CONSERVATIVE (option 4
	// in the issue: manual operator recovery is the primary path, auto-expiry is only
	// the backstop) so a task that is legitimately "working slowly" — but still
	// reporting progress — is never falsely reclaimed. Matches the relay's own
	// 30-minute MAX_TASK_DURATION_MS watchdog so the hub backstop fires no earlier
	// than the relay's own give-up point.
	wsTaskTimeout = 30 * time.Minute
	// wsTokenTTL is how long a minted scoped GitHub token stays valid. It must
	// match the token_expires_at we advertise to the relay so both sides agree
	// on when the token dies.
	wsTokenTTL = 55 * time.Minute
	// wsTokenRefreshPeriod is how long after minting we proactively re-mint and
	// push a fresh token to an active task, before wsTokenTTL expires. The gap
	// (5 min) absorbs clock skew and in-flight gh commands so a long,
	// human-steered session never silently loses push access. See #2393 item 2.
	wsTokenRefreshPeriod = 50 * time.Minute
	wsAuthTimeout        = 30 * time.Second
	wsMaxMessageSize     = 64 * 1024
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		// Extract host from origin URL (e.g. "https://example.com" → "example.com")
		host := origin
		if idx := strings.Index(host, "://"); idx >= 0 {
			host = host[idx+3:]
		}
		host = strings.TrimRight(host, "/")
		return host == r.Host
	},
}

type ContributorConnection struct {
	ws              *websocket.Conn
	profile         *ContributorProfile
	cliBackend      string
	model           string
	reasoningEffort string
	role            string // empty = task-driven mode, "scanner"/"reviewer"/etc. = role mode
	clientRole      string // relay-requested HIVE_AGENT_ROLE; owner assignment may override it
	assignedRole    string // owner-selected role; "none" forces general work
	connectedAt     time.Time
	currentTask     *WSTaskAssign
	// currentTaskGen is the assignment GENERATION stamped on currentTask (kubestellar/
	// hive#2568, the Gate). It is a monotonically increasing token minted per
	// assignment (task_assign, and the task_progress RESUME path that adopts a task).
	// It is shipped to the relay in task_assign and echoed back on task_progress /
	// task_complete / task_failed. When a task is released (disconnect, ready-abandon,
	// operator requeue, or lease-TTL expiry) this is bumped, so a STALE worker that
	// later wakes and reports completion/progress carrying the OLD generation is
	// FENCED OUT: its message is rejected and cannot overwrite the new owner's state.
	// Zero when no task is active (and a client that never learned a generation — an
	// unversioned relay — reports 0, which is treated as "unstamped" and falls back to
	// the pre-existing TaskID match, preserving backward compatibility).
	currentTaskGen uint64
	// lastLeaseRenew is when currentTask's hub-owned lease was last renewed
	// (kubestellar/hive#2568): set on assignment and refreshed on every task_progress.
	// cleanupLoop auto-releases a task whose lease has not been renewed within
	// wsTaskTimeout. Zero when no task is active.
	lastLeaseRenew time.Time
	lastPong       time.Time
	tmuxOutput     []string
	// tokenMintedAt is when the scoped GitHub token for currentTask was last
	// minted. The heartbeat loop uses it to re-mint and push a token_refresh
	// once wsTokenRefreshPeriod has elapsed, before the token expires. Zero when
	// no task is active. See #2393 item 2.
	tokenMintedAt time.Time
	// currentPrompt is the exact assignment prompt that was built for currentTask
	// and shipped in its task_assign (#2539). It is stored so the read-only ops
	// tab can PREVIEW the instruction the agent is running WITHOUT ever exposing
	// the minted github_token that travelled in the same message. It never carries
	// a credential — buildTaskPrompt is a pure function of task metadata. Zero when
	// no task is active.
	currentPrompt string
	// currentLabels are the chosen issue's labels for currentTask (#2539), stored
	// so the ops Task-panel preview can list them alongside the prompt. Metadata
	// only — never a credential. Zero when no task is active.
	currentLabels []string
	// lastIdleReason is the most recent reason selectTask had no work to hand this
	// connection (#2546): one of the taskUnavailable* reasons. It lets the ops tab
	// show WHY a connected clanker is idle (suspended vs hub-not-ready vs
	// no-matching-work vs an enforced refusal) instead of an indistinguishable
	// silence. Cleared when a task is actually assigned. Purely diagnostic.
	lastIdleReason string
	// capabilities is the client-declared runtime posture from auth_response
	// (#2547 declare half): container runtime, OS/arch, agent/relay versions,
	// credential type. Nil when the client declared nothing (an unversioned
	// client). Stored read-only and surfaced on FleetClanker; NEVER used to route
	// or gate work.
	capabilities *ContributorCapabilities
	// lastFailure is the most recent task_failed this connection reported
	// (#2547): the reason it sent, the self-declared failure kind, and which
	// task. Nil until the connection has failed a task.
	//
	// Until now task_failed's reason was written to the hub log and dropped, so
	// "this client cannot run the work" and "the agent got the work wrong"
	// reached an operator as the same event distinguished only by terminal
	// scrollback. Stored read-only and surfaced on FleetClanker exactly like
	// lastIdleReason (#2546); NEVER used to route, gate, or adjust a work item's
	// failure cooldown.
	lastFailure *ContributorFailure
	// pendingToken is the scoped, expiring GitHub credential minted for currentTask
	// but NOT yet delivered to the relay (kubestellar/hive#2537). The credential no
	// longer travels inside task_assign; it is held here until the task's acceptance
	// decision is made, then shipped via deliverTaskCredential (which reuses the
	// token_refresh wire shape). In auto-accept mode (the default) it is delivered
	// immediately after task_assign is sent; in explicit-accept mode it is held
	// until a task_accepted arrives. Cleared to "" once delivered (or when the task
	// ends without acceptance). It is a credential — NEVER logged or previewed.
	pendingToken string
	// credentialDelivered records that the scoped credential for currentTask has
	// already been handed to the relay, so a duplicate task_accepted (the relay
	// re-asserts one on reconnect) does not re-deliver, and the auto-accept and
	// explicit-accept paths cannot both fire. Reset when a task ends. This is the
	// single flag that makes "the credential arrived AFTER acceptance" observable
	// and idempotent.
	credentialDelivered bool
	mu                  sync.Mutex
	// writeMu serializes ALL writes to this connection's ws. gorilla/websocket
	// forbids concurrent writes to one connection ("Applications are responsible
	// for ensuring that no more than one goroutine calls the write methods
	// concurrently"), yet a live connection is written from many goroutines: the
	// per-connection heartbeat ping ticker, the message-handling read loop, and
	// the operator revoke/yank/reassign + lease-reclaim paths. Without this the
	// races surface as a "concurrent write to websocket connection" panic (seen in
	// TestStaleGeneration_RevokedWorkerCannotOverwriteNewOwner). It is a SEPARATE
	// lock from mu (which guards the state fields above): no write path holds mu
	// while calling send, so the two never nest. See kubestellar/hive
	// contribute-ws concurrent-write fix.
	writeMu sync.Mutex
}

// send serializes writes to this connection's websocket with writeMu, satisfying
// gorilla/websocket's one-concurrent-writer contract. Every goroutine that writes
// a frame to a LIVE ContributorConnection (heartbeat ping/token_refresh, the read
// loop's replies, operator revoke/yank/reassign, lease-reclaim) MUST go through
// this method rather than the free sendJSON, which stays only for the pre-handshake
// path where no ContributorConnection (and thus no shared connection) exists yet.
func (c *ContributorConnection) send(msg WSMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteJSON(msg)
}

type WSMessage struct {
	Type          string   `json:"type"`
	Seq           int      `json:"seq,omitempty"`
	Nonce         string   `json:"nonce,omitempty"`
	ContributorID string   `json:"contributor_id,omitempty"`
	TrustTier     string   `json:"trust_tier,omitempty"`
	Permissions   []string `json:"permissions,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	// FailureKind is the OPTIONAL, client-declared cause of a task_failed
	// (#2547): "environment" (the client's runtime could not run the work) or
	// "task" (the work was attempted and failed on its merits). Absent — which
	// is what every relay written before this sends — normalizes to
	// "unspecified" and is treated exactly as today.
	//
	// Self-reported and advisory, like Capabilities below. It is recorded and
	// shown to operators; it does NOT affect selection, admission, or the
	// failure cooldown. See NormalizeTaskFailureKind.
	FailureKind       string `json:"failure_kind,omitempty"`
	Message           string `json:"message,omitempty"`
	RegistrationToken string `json:"registration_token,omitempty"`
	CLIBackend        string `json:"cli_backend,omitempty"`
	// Provider is optional, bounded receipt evidence derived by Pi relays from
	// their canonical provider/model preference. It is never assignment or
	// routing authority; Model remains the canonical selection transport.
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	// TaskGen is the assignment GENERATION / lease token for this task (kubestellar/
	// hive#2568, the Gate). The hub stamps it on task_assign; the relay echoes it back
	// on task_progress / task_complete / task_failed. The hub rejects any completion or
	// progress carrying a generation older than the currently-held one, so a worker
	// whose task was revoked and reassigned cannot later overwrite the new owner's
	// state. Additive: an unversioned relay omits it (0), and the hub falls back to the
	// pre-existing TaskID match for those clients. Never a credential.
	TaskGen uint64   `json:"task_gen,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Repo    string   `json:"repo,omitempty"`
	Number  int      `json:"number,omitempty"`
	Title   string   `json:"title,omitempty"`
	URL     string   `json:"url,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Prompt  string   `json:"prompt,omitempty"`
	// TaskKey, SourceType and ExternalID carry the assigned item's canonical,
	// source-aware identity (kubestellar/hive#4245). All additive and omitempty:
	// a GitHub task_assign is byte-for-byte unchanged, and Repo/Number keep
	// their existing meaning for every relay written before these existed.
	//
	// A relay that understands them can report completion against TaskKey; one
	// that does not keeps echoing repo/number, which the hub still resolves
	// through the connection's own assignment record rather than trusting the
	// client to name the work.
	TaskKey    string `json:"task_key,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	// GitHubToken carries the scoped, expiring credential. As of #2537 it is
	// NEVER populated on task_assign — the credential is split OUT of the
	// assignment message and delivered only AFTER the task's acceptance decision,
	// via a token_refresh (see deliverTaskCredential). It still travels on
	// token_refresh (post-acceptance delivery + the #2393 mid-task re-mint). The
	// token itself is unchanged: same per-tier mint, same wsTokenTTL expiry — only
	// its timing relative to acceptance moved.
	GitHubToken    string `json:"github_token,omitempty"`
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
	// Restrictions is RESERVED and intentionally not populated by the server yet:
	// the contributor command restrictions are enforced server-side (gh-wrapper /
	// contributor-default.json), so shipping the policy to the client would be
	// advisory-only and risk drift. Left as omitempty so it never appears on the
	// wire until a concrete client contract exists. (kubestellar/hive#2393 item 8.)
	Restrictions json.RawMessage `json:"restrictions,omitempty"`
	// Capabilities is the OPTIONAL client-declared runtime posture a contributor
	// relay may report in its auth_response (kubestellar/hive#2547, declare half).
	// It is additive and purely advisory: a client that omits it authenticates
	// and runs exactly as before, and the hub NEVER routes or gates work on it —
	// it is only stored and surfaced read-only. Distinct from the RESERVED,
	// server-side-only Restrictions field above: Capabilities flows client→server
	// as an honest self-report, Restrictions is a reservation that stays empty.
	Capabilities *ContributorCapabilities `json:"capabilities,omitempty"`
	// ProtocolVersion is the contributor-protocol version. On auth_ok it carries
	// the version this HUB speaks (kubestellar/hive#2567) so a client can learn
	// the deployed protocol level without probing; additive, old clients ignore
	// it. It is also accepted on auth_response as the client's own reported
	// version (stored via Capabilities.RelayProtocolVersion).
	ProtocolVersion string `json:"protocol_version,omitempty"`
	// ServerCapabilities is the set of message types / features this hub supports
	// (kubestellar/hive#2567), advertised on auth_ok so a client can adapt without
	// probing (e.g. token_refresh, task_unavailable_reasons). Additive; old
	// clients ignore the unknown field.
	ServerCapabilities []string `json:"server_capabilities,omitempty"`
	Role               string   `json:"role,omitempty"`
	ContribLabels      []string `json:"contributor_labels,omitempty"`
	Status             string   `json:"status,omitempty"`
	Result             string   `json:"result,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	TmuxOutput         []string `json:"tmux_output,omitempty"`
	AcceptedModels     []string `json:"accepted_models,omitempty"`
	// PRURL is the pull request the agent opened for this task, reported on
	// task_complete. It is best-effort: the relay fills it when it can spot a
	// PR link in the agent's output, and it is empty when the agent went idle
	// without shipping anything. The hub uses its presence to decide how long
	// to keep the underlying issue in cooldown — see markTaskCompleted and
	// kubestellar/hive#2393 item 7 (an idle-but-no-PR completion must NOT lock
	// the issue for a full week). A known PR URL per issue also feeds the
	// #2356 duplicate-detection work. Field naming follows the PRURL
	// convention in src/pkg/github/prclaims.go.
	PRURL string `json:"pr_url,omitempty"`
	// Verdict is the OPTIONAL, client-declared outcome of a task_complete
	// (kubestellar/hive#3987): "shipped" (a PR was opened), "idle" (returned to
	// idle without an affirmative conclusion — today's semantics and what an
	// absent field normalizes to, so relays written before this keep working
	// unchanged), or "no_work_needed" (the agent affirmatively determined
	// nothing is shippable — e.g. the issue's remainder is gated on an
	// unanswered maintainer decision, or merged PRs already cover it: the
	// #2547 shape that #3980's escalation only BOUNDS).
	//
	// Self-reported and therefore weak evidence, like FailureKind above: the
	// hub honours it ONLY to lengthen how long the issue is withheld from the
	// contribute OFFER pool (see markTaskCompletedVerdict /
	// isSuppressedByNoWorkVerdict). It never grants trust credit or promotion,
	// never closes or labels anything on GitHub, and never touches the hive
	// agent pipeline's selection. A verified PR overrides any claimed value
	// (normalizeCompletionVerdict).
	Verdict string `json:"verdict,omitempty"`
	// VerdictReason optionally carries a machine-readable reason for a
	// no_work_needed verdict ("maintainer_gated", "already_covered", or free
	// text the relay scraped from the agent's output). Audit-only: it is
	// stored with the verdict and logged so an operator can see WHY an issue
	// keeps yielding no work — precisely the signal needed to go answer the
	// gating question. Truncated server-side to noWorkReasonMaxLen.
	VerdictReason string `json:"verdict_reason,omitempty"`
	// Permanent marks a task_failed the relay will not retry: it exhausted its
	// per-task CLI-restart budget and gave up (see MAX_TASK_CLI_RESTARTS in
	// bin/contributor-relay.sh). Reassigning the same work item to the same
	// contributor will be rejected outright, so the hub should prefer a
	// different contributor. See kubestellar/hive#2203.
	Permanent bool `json:"permanent,omitempty"`
}

type WSTaskAssign struct {
	TaskID string `json:"task_id"`
	Kind   string `json:"kind"`
	Role   string `json:"role,omitempty"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Key, SourceType, ExternalID and URL carry the assigned item's canonical,
	// source-aware identity (kubestellar/hive#4245). Additive and omitempty: a
	// GitHub assignment is byte-for-byte what it was, and Number keeps its
	// meaning for every existing consumer.
	//
	// Key is what the hub keys in-flight work on. Without it two different
	// zero-numbered external items both resolved to "repo#0" and the
	// double-assignment guard treated them as the SAME task — one would block
	// the other, and a completion for one would settle the other.
	Key        string `json:"key,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
}

// identityKey returns the canonical identity of the assigned item. It prefers
// the explicit Key and falls back to the GitHub "repo#number" spelling, so a
// task assigned before this field existed — or restored from a persisted
// pre-upgrade record — keys exactly as it always did.
func (t *WSTaskAssign) identityKey() string {
	if t == nil {
		return ""
	}
	if t.Key != "" {
		return t.Key
	}
	return worksource.Ref{Repo: t.Repo, Number: t.Number}.Key()
}

// assignDesc renders one assigned work item for the activity feed:
// "<kind> <canonical identity>: <title>", falling back to the given task id
// when the item has no canonical identity at all (a synthetic pr-review sweep).
//
// THE DEFECT THIS REPLACES (kubestellar/hive#5120): every feed entry derived
// its label from Number. External work items — Linear, Jira — deliberately
// carry Number == 0 and put their identity in Key/ExternalID (#4245), so a
// Linear ticket was announced as "issue acme/team#0: …" on pickup and as a
// bare internal task id on completion: the two entries for the SAME item did
// not match each other, and every zero-numbered item in a repo rendered
// identically. WSTaskAssign's own doc comment records where that exact
// mistake led on the assignment path — two external items colliding as
// "repo#0" in the double-assignment guard — and #4245 fixed it there; the
// display layer kept it.
//
// For GitHub work the output is byte-identical to the old %s#%d rendering,
// because Ref.Key() spells a numbered item "repo#number".
func assignDesc(kind, key, title, fallback string) string {
	if key == "" {
		return fallback
	}
	return fmt.Sprintf("%s %s: %s", kind, key, title)
}

const maxActivityEntries = 50

type ActivityEntry struct {
	Timestamp string `json:"timestamp"`
	Username  string `json:"username"`
	Action    string `json:"action"`
	Role      string `json:"role,omitempty"`
	CLI       string `json:"cli,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	Task      string `json:"task,omitempty"`
}

type ContributeWSHub struct {
	connections map[string]*ContributorConnection
	mu          sync.RWMutex
	logger      *slog.Logger
	seq         int
	// taskGen is the monotonically increasing source of assignment GENERATION tokens
	// (kubestellar/hive#2568, the Gate). nextTaskGen() hands out a fresh value for
	// every assignment and every release, so a generation is never reused across the
	// life of the hub — a stale worker's old generation can never coincidentally match
	// a later assignment's. It is an atomic counter (NOT guarded by mu) precisely so
	// nextTaskGen() can be called from paths that ALREADY hold h.mu (e.g.
	// RequeueContributorTask and reclaimExpiredLeases iterate connections under
	// h.mu.RLock): a mu-guarded counter would deadlock (RLock-then-Lock on the same
	// RWMutex). Lock-free is also what the -race coverage job wants — see the standing
	// "never re-lock m.mu from a path that holds it" rule.
	taskGen atomic.Uint64
	// pendingConns counts sockets that have been upgraded but have not yet
	// authenticated (audit F9). h.connections only gains an entry AFTER auth
	// succeeds, so capping on that map alone left the pre-auth window — a full
	// wsAuthTimeout per socket — completely unbounded. Atomic rather than
	// mu-guarded to match taskGen's reasoning above: it is touched from the
	// upgrade path and from deferred cleanup, and must never contend with or
	// re-enter h.mu.
	pendingConns   atomic.Int64
	activityMu     sync.RWMutex
	activity       []ActivityEntry
	server         *Server
	completedTasks map[string]time.Time
	// completedTaskCooldown holds a per-task override for how long, from the
	// completion time in completedTasks, the issue stays in cooldown. It is
	// populated by markTaskCompleted based on whether a PR was reported. When a
	// key is absent (e.g. tasks restored from an older on-disk format, or set
	// directly by tests) isTaskInCooldown falls back to the full
	// completedTaskCooldownHours, preserving the original conservative default.
	completedTaskCooldown map[string]time.Duration
	// completedTaskPRURL records the PR URL reported for a completed task, kept
	// for stats/audit and to feed #2356 duplicate detection (a known PR URL per
	// issue). Empty means the completion reported no PR.
	completedTaskPRURL map[string]string
	// failedTasks records, per "repo#number", the time of the most recent
	// task_failed. It backs the SHORT failure cooldown (failedTaskCooldownMinutes)
	// that breaks the queue livelock in #2435: without it a just-failed issue is
	// immediately re-admissible and, sitting at the same position in the same
	// deterministic scan, is handed straight back out ahead of every other
	// admissible issue. Kept separate from completedTasks so a failure never masks
	// as a completion in stats/audit. Guarded by completedMu (shared with the
	// completed-task ledger — they are read/written from the same paths).
	failedTasks map[string]time.Time
	// consecutiveFailures counts back-to-back task_failed reports per
	// "repo#number", reset to zero on completion. Once it reaches
	// consecutiveFailureQuarantineThreshold the issue is QUARANTINED for the
	// longer quarantineCooldownHours window (#2435 remedy 2): this distinguishes
	// "flaky once" from "nobody can do this", which a flat short cooldown cannot.
	// A permanent failure (msg.Permanent) counts as permanentFailureWeight toward
	// the threshold. Guarded by completedMu.
	consecutiveFailures map[string]int
	// noPRStreaks counts CONSECUTIVE no-PR completions per "repo#number"
	// (kubestellar/hive#3980). Each one doubles the next no-PR cooldown
	// (advanceNoPRStreakLocked), so an issue that keeps "completing" with
	// nothing to ship — e.g. the #2547 shape, where the shippable parts
	// merged and the rest is maintainer-gated but the issue stays open —
	// backs off geometrically instead of burning a full contributor task
	// cycle every completedNoPRCooldownHours forever. Deliberately a
	// SEPARATE map from completedTasks: the loop's signature is precisely
	// that the completion entry EXPIRES (and is swept) before the next
	// dispatch, so the streak must survive that sweep. Reset by a completion
	// that ships a verified PR, or by noPRStreakResetAfter of quiet. Guarded
	// by completedMu.
	noPRStreaks map[string]noPRStreakRecord
	// noWorkVerdicts records, per "repo#number", the most recent
	// contributor-reported no_work_needed completion verdict (#3987): the agent
	// affirmatively determined the issue has nothing shippable (the #2547
	// shape — shippable parts merged, remainder maintainer-gated, no open PR
	// left to claim it, so no claim of any kind survives the next rescan).
	// While a verdict is live the issue is withheld from the contribute OFFER
	// pool (selectTask + ReadyQueue) for the long with-PR-cooldown window
	// instead of cycling on the short no-PR ladder forever.
	//
	// Invalidation beats suppression: the verdict is VOID the moment the issue
	// shows activity newer than the verdict (issue updated_at > RecordedAt),
	// so a genuinely reopened issue — the maintainer answered, new commits or
	// comments landed — is offered again immediately. When the issue's
	// updated_at is unknown, the check fails OPEN (no suppression). The
	// verdict is contributor-reported (weak evidence): it only ever suppresses
	// offers — it never closes issues, never feeds trust/promotion, and never
	// gates the hive agent pipeline. SEPARATE map from completedTasks for the
	// same reason noPRStreaks is: the loop's signature is the completion entry
	// EXPIRING before the next dispatch, so this must survive that sweep.
	// Guarded by completedMu; persisted in the same PVC-backed ledger dir as
	// the cooldowns so a pod restart does not forget the verdict.
	noWorkVerdicts     map[string]noWorkVerdictRecord
	activityFilePath   string
	completedTasksFile string
	failedTasksFile    string
	noPRStreaksFile    string
	noWorkVerdictsFile string
	asyncActivitySave  bool
	persistActivity    bool
	persistTaskLedgers bool
	completedMu        sync.Mutex
	selectMu           sync.Mutex
	// assignmentTimes records, per contributor identity (identityOf), the wall-clock
	// times of the task_assign messages that identity has been handed. It backs the
	// #2436/#2566 per-tier rate gate: tier_limits.max_per_hour / max_per_day were
	// admin-writable and displayed by the Management & Operations control-plane
	// (#2562) as if authoritative, but selectTask enforced only max_concurrent, so
	// the numbers an operator set were inert. We enforce them here by counting the
	// timestamps in this ledger that fall inside a ROLLING 1-hour and 24-hour window
	// ending "now" (a sliding window keyed off each assignment's timestamp, NOT a
	// calendar-hour/calendar-day bucket that would reset on the clock). A slot frees
	// exactly `rateLimitHourWindow` / `rateLimitDayWindow` after it was taken, so a
	// contributor who hit max_per_hour can resume as soon as their oldest assignment
	// in the trailing hour ages out. The counter is ASSIGNMENTS (each task handed
	// out), mirroring max_concurrent's "tasks handed to this identity" semantics and
	// the max_tasks_per_hour/day field naming; it is not gated on completion. Old
	// entries beyond the day window are pruned on every recording pass so the map
	// stays bounded. Guarded by rateMu.
	assignmentTimes map[string][]time.Time
	rateMu          sync.Mutex
	// sse is the read-only Server-Sent-Events broadcast registry (contribute_sse.go).
	// Every appended ActivityEntry is fanned out to subscribed dashboard browsers so
	// the Operations "command center" renders live. It is purely additive: the fan-out
	// is a NON-BLOCKING send, so it can never back-pressure this WS event path.
	sse *sseRegistry
	// yankExclusions records, per "contributorID\x00repo#number", when a just-yanked
	// issue stops being self-excluded from THE SAME clanker. Yank releases a held task
	// AND immediately reassigns the clanker; this brief per-(clanker, issue) exclusion
	// keeps that reassignment from re-handing the clanker the very issue it was yanked
	// off, so it moves to genuinely different work. It is SCOPED to the one yanked
	// clanker — the issue is offerable to every OTHER contributor immediately. Entries
	// expire yankSelfExcludeSeconds after the yank and are pruned lazily on read.
	// Guarded by h.mu, like the other per-issue live state.
	yankExclusions map[string]time.Time
	// leases is the hub-owned, server-authoritative registry of the task the hub
	// ISSUED to each contributor identity (kubestellar/hive C4). It is keyed by
	// identity (identityOf: ContributorID, falling back to GitHubUsername) and holds
	// exactly one lease per identity — the last task selectTask handed that identity.
	//
	// It is the ONLY thing a reconnecting relay's task_progress may re-adopt against.
	// Before this, a task_progress arriving while a (freshly reconnected) connection
	// had currentTask == nil caused the hub to REBUILD ownership from the client's own
	// task_id/repo/number fields and mint a fresh scoped credential for it — a client
	// could therefore assert ANY task_id it liked and be handed a credential for work
	// the server never assigned. Now a resume must match an active lease here EXACTLY
	// on {identity, task_id, repo, generation} and be within its expiry, or it is
	// rejected. A lease is recorded on assignment (recordLease) and revoked on every
	// release path (revokeLease: disconnect, ready-abandon, complete, fail, operator
	// requeue, lease-TTL expiry), so a released task can never be re-adopted. Guarded
	// by leaseMu.
	leases  map[string]*taskLease
	leaseMu sync.Mutex
}

// taskLease is the server-authoritative record of a task the hub issued to a
// contributor identity (kubestellar/hive C4). It binds the assignment to the
// {profile, task, repo, generation} tuple and an expiry so a reconnecting relay's
// task_progress can only RE-ADOPT a task the hub actually assigned to it, under the
// exact generation it was assigned, and only until the lease expires. It is minted
// by recordLease at assignment and cleared by revokeLease on every release path; a
// resume that does not match an unexpired lease here is rejected outright.
type taskLease struct {
	identity  string
	taskID    string
	repo      string
	number    int
	tier      string
	gen       uint64
	expiresAt time.Time
}

// leaseTTL is how long a hub-issued task lease remains re-adoptable after the last
// time the relay proved it was still working (kubestellar/hive C4). It is aligned
// with wsTaskTimeout (the wedged-task backstop) and, since #4260, is measured from
// the SAME event: recordLease stamps it at assignment and renewLease re-stamps it on
// every accepted task_progress, exactly where reclaimExpiredLeases re-stamps
// lastLeaseRenew. The two clocks therefore expire together, which is what makes the
// intended invariant true — a task past its re-adoption window is also one the
// wedged-task backstop has reclaimed, and a task the backstop considers alive is
// still re-adoptable.
//
// #4260: before that renewal existed, expiresAt was stamped once at assignment and
// never moved, so the alignment was only nominal. A perfectly healthy task that had
// been reporting progress for longer than leaseTTL was NEVER reclaimed (correctly —
// it was alive) yet its lease had silently expired, so the first socket drop after
// that point could not be resumed: lookupLease rejected the reconnecting relay,
// the hub sent task_revoke, and the same issue was re-assigned as a new task,
// typing a fresh prompt into a pane whose CLI was still mid-turn.
//
// A resume presented after this window is treated as a stale/forged claim and
// rejected; the relay simply asks for fresh work via "ready".
const leaseTTL = wsTaskTimeout

// recordLease registers (or replaces) the server-authoritative lease for an
// identity when the hub assigns it a task (kubestellar/hive C4). It stores the
// exact {task, repo, generation, tier} the hub issued plus an expiry, so a later
// reconnect can be validated against what the server actually handed out — never
// reconstructed from client-supplied fields. Called from selectTask under the new
// assignment's generation.
func (h *ContributeWSHub) recordLease(identity, taskID, repo string, number int, tier string, gen uint64, now time.Time) {
	if identity == "" || taskID == "" {
		return
	}
	h.leaseMu.Lock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	h.leases[identity] = &taskLease{
		identity:  identity,
		taskID:    taskID,
		repo:      repo,
		number:    number,
		tier:      tier,
		gen:       gen,
		expiresAt: now.Add(leaseTTL),
	}
	h.leaseMu.Unlock()
}

// renewLease extends an identity's server-issued lease window when the relay proves
// it is still working the task (kubestellar/hive#4260). It is the lease-registry half
// of the lastLeaseRenew stamp that reclaimExpiredLeases reads: both are driven by the
// same accepted task_progress, so "still alive" and "still re-adoptable" cannot drift
// apart and a long-running task does not lose the ability to survive a reconnect
// simply because it has been working for longer than leaseTTL.
//
// It grants NOTHING a caller did not already have. The lease is only touched when it
// is already the one recorded for this identity AND is for this exact taskID, so a
// connection can neither renew another identity's lease nor extend a lease for a task
// it does not hold; a revoked lease is absent and stays absent. Only expiresAt moves —
// the {task, repo, number, tier, generation} tuple lookupLease matches on is never
// rewritten, so the C4 exact-match contract and the #2568 generation fence are
// untouched.
func (h *ContributeWSHub) renewLease(identity, taskID string, now time.Time) {
	if identity == "" || taskID == "" {
		return
	}
	h.leaseMu.Lock()
	if l, ok := h.leases[identity]; ok && l.taskID == taskID {
		l.expiresAt = now.Add(leaseTTL)
	}
	h.leaseMu.Unlock()
}

// revokeLease removes the server-authoritative lease for an identity on any release
// path (kubestellar/hive C4): disconnect, ready-abandon, task_complete, task_failed,
// operator requeue, and lease-TTL expiry. Once revoked, a reconnecting relay's
// task_progress for that task no longer matches any lease and cannot re-adopt it —
// closing the window in which a released task could be resurrected from client
// fields. It only deletes the entry when the current lease is for taskID, so a race
// where a NEW lease was already recorded for the same identity is not clobbered.
func (h *ContributeWSHub) revokeLease(identity, taskID string) {
	if identity == "" {
		return
	}
	h.leaseMu.Lock()
	if l, ok := h.leases[identity]; ok && (taskID == "" || l.taskID == taskID) {
		delete(h.leases, identity)
	}
	h.leaseMu.Unlock()
}

// lookupLease returns the active, unexpired server-issued lease for an identity that
// EXACTLY matches the resume claim (kubestellar/hive C4): same task_id, same
// canonical repo, same number, and same assignment generation. Any mismatch — no
// lease, wrong task, wrong repo/number, wrong (or zero) generation, or an expired
// lease — returns nil, so a reconnecting relay may only re-adopt the precise task the
// hub assigned it, under the generation it was assigned, and only within the lease
// window. It never reconstructs ownership from the client's own fields.
//
// clientGen == 0 (an unversioned relay) is deliberately NOT honored here: re-adoption
// requires proving possession of the server-issued generation token, which an
// unversioned relay cannot present. Such a relay is asked to re-`ready` for fresh
// work instead of resurrecting a lease it cannot authenticate.
func (h *ContributeWSHub) lookupLease(identity, taskID, repo string, number int, clientGen uint64, now time.Time) *taskLease {
	if identity == "" || taskID == "" || clientGen == 0 {
		return nil
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l, ok := h.leases[identity]
	if !ok {
		return nil
	}
	if now.After(l.expiresAt) {
		// Expired: drop it so it can never be re-adopted, and treat as no lease.
		delete(h.leases, identity)
		return nil
	}
	if l.taskID != taskID || l.gen != clientGen {
		return nil
	}
	if repo != "" && l.repo != repo {
		return nil
	}
	if number != 0 && l.number != number {
		return nil
	}
	return l
}

// rateLimitHourWindow and rateLimitDayWindow are the trailing (rolling) windows
// over which tier_limits.max_per_hour and max_per_day are counted (#2566). They
// are sliding windows anchored on "now", not calendar buckets: a contributor's
// assignment stops counting against the hourly cap exactly rateLimitHourWindow
// after it was made, and against the daily cap after rateLimitDayWindow. This
// matches the field names (per HOUR / per DAY) while avoiding a hard reset at the
// top of the clock hour/day that would let a burst straddle the boundary.
const (
	rateLimitHourWindow = time.Hour
	rateLimitDayWindow  = 24 * time.Hour
)

// completedTaskCooldownHours is the cooldown applied when a task completes
// having actually shipped a pull request: real work landed, so we should not
// re-dispatch the same issue for a week.
const completedTaskCooldownHours = 168

// completedNoPRCooldownHours is the cooldown applied when a task "completes"
// only because the agent went idle WITHOUT reporting a PR (the common case the
// old code could not distinguish — see kubestellar/hive#2393 item 7). Nothing
// shipped, so locking the issue for a full week wrongly starves it: another
// contributor should be able to pick it up soon. We keep a short, non-zero
// cooldown (not zero) so the very next selector pass does not instantly hand
// the same untouched issue back to the same idle contributor in a tight loop;
// a few hours is long enough to break that loop while still freeing the issue
// the same day.
const completedNoPRCooldownHours = 4

// noPRStreakResetAfter is how long a no-PR completion streak (#3980) survives
// without another no-PR completion before it forgets itself and the next
// no-PR cooldown starts back at completedNoPRCooldownHours. It must be LONGER
// than the streak's own cooldown cap (the with-PR cooldown, default
// completedTaskCooldownHours = 168h): the whole point is that the issue is
// only re-dispatched AFTER its escalated cooldown expires, so a reset window
// shorter than the cap would wipe the streak before the capped re-offer ever
// happened and the backoff could never reach — or hold — its ceiling. Two
// weeks = 2× the default cap, so one full capped cycle plus slack.
const noPRStreakResetAfter = 14 * 24 * time.Hour

// completionVerdict* are the recognized values of WSMessage.Verdict on
// task_complete (#3987). Anything else — including an absent field, which is
// what every relay written before this sends — normalizes to idle, i.e.
// byte-for-byte today's behavior (see normalizeCompletionVerdict).
const (
	completionVerdictShipped      = "shipped"
	completionVerdictIdle         = "idle"
	completionVerdictNoWorkNeeded = "no_work_needed"
)

// noWorkReasonMaxLen caps the client-supplied VerdictReason before it is stored
// in the PVC-backed verdict ledger and echoed into logs: the field is scraped
// from arbitrary agent output, so an unbounded value would let a confused (or
// hostile) relay bloat the ledger file. 200 runes comfortably fits the
// machine-readable reasons ("maintainer_gated", "already_covered") plus a short
// free-text explanation.
const noWorkReasonMaxLen = 200

// failedTaskCooldownMinutes is the SHORT cooldown applied to an issue when a
// contributor reports task_failed (#2435). It is deliberately far shorter than
// the completion cooldowns above: a failure is often purely environmental
// (expired token, model outage, transient network) and the issue is likely
// still perfectly workable, so we must not park it for long. This short window
// is only large enough to break the tight reject/re-offer livelock — where a
// just-failed issue at the head of the deterministic scan order is handed
// straight back out ahead of the whole rest of the queue — while still letting a
// legitimate retry happen within a few minutes.
const failedTaskCooldownMinutes = 10

// consecutiveFailureQuarantineThreshold is how many consecutive failures an
// issue may accumulate (weighted — see permanentFailureWeight) before it is
// QUARANTINED for the longer quarantineCooldownHours window (#2435 remedy 2).
// A poison issue that nobody can complete burns at most this many assignments
// before it is parked for hours instead of minutes, so it stops starving the
// queue. The counter resets to zero on the issue's next completion.
const consecutiveFailureQuarantineThreshold = 3

// quarantineCooldownHours is the LONGER cooldown applied once an issue crosses
// consecutiveFailureQuarantineThreshold. It parks a reliably-failing issue for
// hours (long enough to stop it dominating the queue) without locking it as long
// as a genuine week-long completion cooldown — the issue may become workable
// again (e.g. a dependency merges) and should re-enter circulation the same day.
const quarantineCooldownHours = 6

// permanentFailureWeight is how much a permanent failure (msg.Permanent — the
// relay exhausted its per-task CLI-restart budget and will not retry, see
// bin/contributor-relay.sh) counts toward consecutiveFailureQuarantineThreshold.
// A permanent failure is a strong "nobody here can do this" signal, so it
// advances the quarantine counter faster than an ordinary (possibly transient)
// failure. With a weight of 3 and a threshold of 3, a single permanent failure
// quarantines the issue immediately.
const permanentFailureWeight = 3

// leaseExpiredReason is the reason pushed to a relay whose task lease expired without
// progress (kubestellar/hive#2568). It is distinct from the operator-requeue reason so
// an operator reading the activity log can tell a manual release from the automatic
// backstop.
const leaseExpiredReason = "task lease expired (no progress within lease TTL)"

// defaultYankReason is the fallback reason recorded and pushed to the client when an
// operator YANKS a held task without supplying one. Yank is Requeue + an immediate
// reassignment of the SAME clanker to its next-priority item, so the default reason is
// distinct from the plain requeue label to keep the activity log legible.
const defaultYankReason = "yanked by operator (released + reassigned)"

// yankSelfExcludeSeconds is how long a just-yanked issue is excluded from being
// re-offered to THE SAME clanker that was yanked off it (per (contributor, issue-key)).
// Yank's whole point is to move a clanker to genuinely DIFFERENT work, so without this
// brief self-exclusion the immediate reassignment selectTask below could simply re-hand
// the clanker the very issue it was just yanked off (it is only in the short failure
// cooldown, which selectTask honours globally, but the reassignment runs right after the
// release and — for a small backlog — that cooldown might already have been aged past in
// tests, or a future cooldown tweak could shorten it). The exclusion is SCOPED to this
// one clanker: the item is offerable to any OTHER contributor immediately. Kept short so
// the clanker can return to the issue once the TTL elapses if nothing else is available.
const yankSelfExcludeSeconds = 60

func NewContributeWSHub(logger *slog.Logger, server *Server) *ContributeWSHub {
	hub := &ContributeWSHub{
		connections:           make(map[string]*ContributorConnection),
		completedTasks:        make(map[string]time.Time),
		completedTaskCooldown: make(map[string]time.Duration),
		completedTaskPRURL:    make(map[string]string),
		failedTasks:           make(map[string]time.Time),
		consecutiveFailures:   make(map[string]int),
		noPRStreaks:           make(map[string]noPRStreakRecord),
		noWorkVerdicts:        make(map[string]noWorkVerdictRecord),
		activityFilePath:      activityFilePath,
		completedTasksFile:    completedTasksFile,
		failedTasksFile:       failedTasksFile,
		noPRStreaksFile:       noPRStreaksFile,
		noWorkVerdictsFile:    noWorkVerdictsPath(),
		asyncActivitySave:     asyncActivitySave,
		persistActivity:       activityPersistenceEnabled,
		persistTaskLedgers:    taskLedgerPersistenceEnabled,
		assignmentTimes:       make(map[string][]time.Time),
		leases:                make(map[string]*taskLease),
		yankExclusions:        make(map[string]time.Time),
		logger:                logger,
		server:                server,
		sse:                   newSSERegistry(),
	}
	hub.loadCompletedTasks()
	hub.loadFailedTasks()
	hub.loadNoPRStreaks()
	hub.loadNoWorkVerdicts()
	hub.loadActivity()
	go hub.cleanupLoop()
	return hub
}

var activityFilePath = "/data/contributors/activity.json"

var asyncActivitySave = true
var activityPersistenceEnabled = true
var taskLedgerPersistenceEnabled = true

func (h *ContributeWSHub) loadActivity() {
	if h != nil && !h.persistActivity {
		return
	}
	path := h.activityPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	var entries []ActivityEntry
	if json.Unmarshal(data, &entries) == nil {
		h.activity = entries
		h.logger.Info("[contribute-ws] activity restored", "entries", len(entries))
	}
}

func (h *ContributeWSHub) saveActivity() {
	if h != nil && !h.persistActivity {
		return
	}
	h.activityMu.RLock()
	entries := make([]ActivityEntry, len(h.activity))
	copy(entries, h.activity)
	h.activityMu.RUnlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	path := h.activityPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] activity write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] activity rename failed", "error", err)
	}
}

func (h *ContributeWSHub) activityPath() string {
	if h != nil && h.activityFilePath != "" {
		return h.activityFilePath
	}
	return activityFilePath
}

const activityDebounceSecs = 60

func (h *ContributeWSHub) addActivity(username, action, role, cli, model, effort, task string) {
	h.activityMu.Lock()
	if len(h.activity) > 0 && (action == "joined" || action == "left") {
		last := h.activity[len(h.activity)-1]
		if last.Username == username && last.Action == action {
			if t, err := time.Parse(time.RFC3339, last.Timestamp); err == nil && time.Since(t) < activityDebounceSecs*time.Second {
				h.activityMu.Unlock()
				return
			}
		}
	}
	entry := ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Username:  username,
		Action:    action,
		Role:      role,
		CLI:       cli,
		Model:     model,
		Effort:    effort,
		Task:      task,
	}
	h.activity = append(h.activity, entry)
	if len(h.activity) > maxActivityEntries {
		h.activity = h.activity[len(h.activity)-maxActivityEntries:]
	}
	h.activityMu.Unlock()
	if h.asyncActivitySave {
		go h.saveActivity()
	} else {
		h.saveActivity()
	}
	// Fan the appended event out to any live SSE subscribers (Operations command
	// center). Done AFTER releasing activityMu, and the fan-out itself is a
	// non-blocking send, so a subscribed browser can never stall the WS path.
	h.broadcastActivity(entry)
}

func (h *ContributeWSHub) RecentActivity() []ActivityEntry {
	h.activityMu.RLock()
	defer h.activityMu.RUnlock()
	out := make([]ActivityEntry, len(h.activity))
	copy(out, h.activity)
	return out
}

var completedTasksFile = "/data/contributors/completed-tasks.json"

// completedTaskRecord is the on-disk shape of one completed-task entry. It
// carries the completion time plus, since #2393 item 7, the per-task cooldown
// and the PR URL that decided it, so a hub restart preserves whether an issue
// got the short no-PR cooldown or the full one. Entries written by older builds
// were a bare RFC3339 timestamp string; loadCompletedTasks still accepts that
// legacy form and treats it as a full-cooldown, no-PR entry.
type completedTaskRecord struct {
	CompletedAt   time.Time `json:"completed_at"`
	CooldownHours float64   `json:"cooldown_hours,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
}

var failedTasksFile = "/data/contributors/failed-tasks.json"

var noPRStreaksFile = "/data/contributors/no-pr-streaks.json"

// noPRStreakRecord is the in-memory and on-disk shape of one no-PR completion
// streak (#3980). LastAt is the most recent no-PR completion; the streak is
// discarded once it is older than noPRStreakResetAfter — enforced lazily on
// advance, on save, and on load, so a hub bounce cannot resurrect a stale
// streak and the map cannot grow without bound.
type noPRStreakRecord struct {
	Count  int       `json:"count"`
	LastAt time.Time `json:"last_at"`
}

func (h *ContributeWSHub) loadNoPRStreaks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.noPRStreaksPath())
	if err != nil {
		return
	}
	records := make(map[string]noPRStreakRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.Count <= 0 || rec.LastAt.IsZero() {
			continue
		}
		if time.Since(rec.LastAt) >= noPRStreakResetAfter {
			continue
		}
		if h.noPRStreaks != nil {
			h.noPRStreaks[k] = rec
		}
	}
	h.logger.Info("[contribute-ws] loaded no-PR streaks", "count", len(h.noPRStreaks))
}

func (h *ContributeWSHub) saveNoPRStreaks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]noPRStreakRecord, len(h.noPRStreaks))
	for k, rec := range h.noPRStreaks {
		if time.Since(rec.LastAt) >= noPRStreakResetAfter {
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks marshal failed", "error", err)
		return
	}
	path := h.noPRStreaksPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) noPRStreaksPath() string {
	if h != nil && h.noPRStreaksFile != "" {
		return h.noPRStreaksFile
	}
	return noPRStreaksFile
}

// advanceNoPRStreakLocked books one more consecutive no-PR completion against
// key and returns the escalated cooldown to apply (#3980): the base
// completedNoPRCooldownHours doubled per prior streak entry, capped at the
// operator's with-PR cooldown. 4h → 8h → 16h → … keeps the original "another
// contributor can retry soon" behavior for a first idle completion, while an
// issue that repeatedly yields "nothing to ship" — however a future parsing
// gap lets that happen — converges to at most one wasted task cycle per
// with-PR cooldown period instead of one every 4h forever. Callers must hold
// completedMu.
func (h *ContributeWSHub) advanceNoPRStreakLocked(key string, ceiling time.Duration) time.Duration {
	if h.noPRStreaks == nil {
		h.noPRStreaks = make(map[string]noPRStreakRecord)
	}
	rec := h.noPRStreaks[key]
	if rec.Count > 0 && time.Since(rec.LastAt) >= noPRStreakResetAfter {
		rec = noPRStreakRecord{}
	}
	rec.Count++
	rec.LastAt = time.Now()
	h.noPRStreaks[key] = rec

	cooldown := completedNoPRCooldownHours * time.Hour
	for i := 1; i < rec.Count; i++ {
		if cooldown >= ceiling {
			break
		}
		cooldown *= 2
	}
	if cooldown > ceiling {
		cooldown = ceiling
	}
	return cooldown
}

// noWorkVerdictsFileName is the on-disk ledger for no_work_needed completion
// verdicts (#3987), living in the same PVC-backed contributors dir as the
// cooldown/streak ledgers so a hub pod restart does not forget a verdict.
// The full path comes from noWorkVerdictsPath(), which honours
// HIVE_CONTRIBUTORS_DIR exactly as the contributor profiles do — that is what
// lets the persistence round-trip test point it at a temp dir.
const noWorkVerdictsFileName = "no-work-verdicts.json"

func noWorkVerdictsPath() string {
	return filepath.Join(getContributorsDir(), noWorkVerdictsFileName)
}

func (h *ContributeWSHub) noWorkVerdictsPath() string {
	if h != nil && h.noWorkVerdictsFile != "" {
		return h.noWorkVerdictsFile
	}
	return noWorkVerdictsPath()
}

// noWorkVerdictRecord is the in-memory and on-disk shape of one no_work_needed
// verdict (#3987). RecordedAt anchors both the suppression window and the
// invalidation comparison (issue updated_at newer than RecordedAt voids the
// verdict). SuppressHours snapshots the operator's with-PR cooldown at record
// time so a later config change cannot retroactively stretch an old verdict;
// 0 (an older entry) falls back to the completedTaskCooldownHours default.
// Reporter/Reason are audit-only.
type noWorkVerdictRecord struct {
	RecordedAt    time.Time `json:"recorded_at"`
	SuppressHours float64   `json:"suppress_hours,omitempty"`
	Reporter      string    `json:"reporter,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

// suppressWindow is how long, from RecordedAt, this verdict withholds the
// issue from the contribute offer pool (absent earlier invalidation).
func (r noWorkVerdictRecord) suppressWindow() time.Duration {
	if r.SuppressHours > 0 {
		return time.Duration(r.SuppressHours * float64(time.Hour))
	}
	return completedTaskCooldownHours * time.Hour
}

func (h *ContributeWSHub) loadNoWorkVerdicts() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.noWorkVerdictsPath())
	if err != nil {
		return
	}
	records := make(map[string]noWorkVerdictRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.RecordedAt.IsZero() {
			continue
		}
		// Drop entries already past their own suppression window so a stale
		// verdict is never resurrected across a restart.
		if time.Since(rec.RecordedAt) >= rec.suppressWindow() {
			continue
		}
		if h.noWorkVerdicts != nil {
			h.noWorkVerdicts[k] = rec
		}
	}
	h.logger.Info("[contribute-ws] loaded no-work verdicts", "count", len(h.noWorkVerdicts))
}

func (h *ContributeWSHub) saveNoWorkVerdicts() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]noWorkVerdictRecord, len(h.noWorkVerdicts))
	for k, rec := range h.noWorkVerdicts {
		if time.Since(rec.RecordedAt) >= rec.suppressWindow() {
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts marshal failed", "error", err)
		return
	}
	path := h.noWorkVerdictsPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts rename failed", "error", err)
	}
}

// normalizeCompletionVerdict maps the client-reported verdict plus the
// SERVER-side PR verification outcome to the verdict the hub acts on (#3987).
// A VERIFIED PR always wins ("shipped") regardless of what the client claimed —
// the self-reported field can neither hide nor fabricate shipped work. With no
// verified PR, only an exact (case-insensitive) no_work_needed is honoured;
// anything else — absent, unknown, or a claimed "shipped" whose PR failed
// verification — normalizes to idle, which is byte-for-byte today's behavior,
// so relays that never learn the field keep working unchanged.
func normalizeCompletionVerdict(reported, verifiedPR string) string {
	if verifiedPR != "" {
		return completionVerdictShipped
	}
	if strings.EqualFold(strings.TrimSpace(reported), completionVerdictNoWorkNeeded) {
		return completionVerdictNoWorkNeeded
	}
	return completionVerdictIdle
}

// isSuppressedByNoWorkVerdict reports whether a live no_work_needed verdict
// (#3987) withholds this issue from the contribute OFFER pool. It is consulted
// ONLY by the two offer surfaces — selectTask and ReadyQueue — never by the
// hive agent pipeline, and it can only ever exclude an offer: it closes and
// labels nothing.
//
// Invalidation beats suppression. The verdict is discarded (and the issue
// offerable again) when:
//   - the issue shows activity NEWER than the verdict (issueUpdatedAt after
//     RecordedAt): the maintainer answered, commits or comments landed, the
//     decision the remainder was gated on may have arrived; or
//   - the suppression window (the with-PR cooldown snapshotted at record time)
//     has elapsed.
//
// When the issue's updated_at is UNKNOWN (zero — an older status producer that
// does not carry the field), the check fails OPEN: we cannot prove the issue
// has been quiet since the verdict, and suppressing genuinely reopened work is
// the one failure mode this feature must not have. The record is kept (not
// discarded) so a later snapshot that does carry updated_at can still honour
// or void it. Like every completion cooldown, the operator kill-switch
// disables the exclusion entirely.
func (h *ContributeWSHub) isSuppressedByNoWorkVerdict(repo string, number int, issueUpdatedAt time.Time) bool {
	return h.isSuppressedByNoWorkVerdictKey(worksource.Ref{Repo: repo, Number: number}.Key(), issueUpdatedAt)
}

func (h *ContributeWSHub) isSuppressedByNoWorkVerdictKey(key string, issueUpdatedAt time.Time) bool {
	if !h.cooldownEnabled() {
		return false
	}
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	rec, ok := h.noWorkVerdicts[key]
	if !ok {
		h.completedMu.Unlock()
		return false
	}
	expired := time.Since(rec.RecordedAt) >= rec.suppressWindow()
	voided := !issueUpdatedAt.IsZero() && issueUpdatedAt.After(rec.RecordedAt)
	if expired || voided {
		delete(h.noWorkVerdicts, key)
		h.completedMu.Unlock()
		h.saveNoWorkVerdicts()
		h.logger.Info("[contribute-ws] no-work verdict lifted",
			"key", key, "expired", expired, "voided_by_activity", voided)
		return false
	}
	h.completedMu.Unlock()
	if issueUpdatedAt.IsZero() {
		// Activity unknown → fail open. See doc comment.
		return false
	}
	return true
}

// issueUpdatedAtFromMap extracts the issue's GitHub updated_at from the
// map[string]any shape selectTask/ReadyQueue read out of ActionableIssues.
// ghpkg.Issue.UpdatedAt marshals as an RFC3339 string; anything absent or
// unparseable yields the zero time, which isSuppressedByNoWorkVerdict treats
// as "activity unknown" (fail open).
func issueUpdatedAtFromMap(issue map[string]any) time.Time {
	s, _ := issue["updated_at"].(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// failedTaskRecord is the on-disk shape of one failed-task entry (#2435). It
// preserves the last-failure time and the running consecutive-failure count so a
// hub restart does not reset a quarantine — otherwise a bounce would re-admit a
// poison issue that had already earned its longer parking window.
type failedTaskRecord struct {
	FailedAt            time.Time `json:"failed_at"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
}

func (h *ContributeWSHub) loadFailedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.failedTasksPath())
	if err != nil {
		return
	}
	records := make(map[string]failedTaskRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.FailedAt.IsZero() {
			continue
		}
		// Drop entries already past the longest failure-side window we could have
		// applied (the quarantine cooldown) so we never resurrect a stale park.
		if time.Since(rec.FailedAt) >= quarantineCooldownHours*time.Hour {
			continue
		}
		if h.failedTasks != nil {
			h.failedTasks[k] = rec.FailedAt
		}
		if h.consecutiveFailures != nil && rec.ConsecutiveFailures > 0 {
			h.consecutiveFailures[k] = rec.ConsecutiveFailures
		}
	}
	h.logger.Info("[contribute-ws] loaded failed tasks", "count", len(h.failedTasks))
}

func (h *ContributeWSHub) saveFailedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]failedTaskRecord, len(h.failedTasks))
	for k, t := range h.failedTasks {
		rec := failedTaskRecord{FailedAt: t}
		if h.consecutiveFailures != nil {
			rec.ConsecutiveFailures = h.consecutiveFailures[k]
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] failed tasks marshal failed", "error", err)
		return
	}
	path := h.failedTasksPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) failedTasksPath() string {
	if h != nil && h.failedTasksFile != "" {
		return h.failedTasksFile
	}
	return failedTasksFile
}

func (h *ContributeWSHub) loadCompletedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.completedTasksPath())
	if err != nil {
		return
	}

	// Accept both the current object form and the legacy map[string]string
	// (key -> RFC3339 timestamp) form so an upgrade never drops cooldowns.
	records := make(map[string]completedTaskRecord)
	if json.Unmarshal(data, &records) != nil {
		var legacy map[string]string
		if json.Unmarshal(data, &legacy) != nil {
			return
		}
		records = make(map[string]completedTaskRecord, len(legacy))
		for k, v := range legacy {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				records[k] = completedTaskRecord{CompletedAt: t}
			}
		}
	}

	for k, rec := range records {
		if rec.CompletedAt.IsZero() {
			continue
		}
		cooldown := completedTaskCooldownHours * time.Hour
		if rec.CooldownHours > 0 {
			cooldown = time.Duration(rec.CooldownHours * float64(time.Hour))
		}
		// Skip anything already past its own cooldown so we don't resurrect
		// stale locks.
		if time.Since(rec.CompletedAt) >= cooldown {
			continue
		}
		h.completedTasks[k] = rec.CompletedAt
		if h.completedTaskCooldown != nil {
			h.completedTaskCooldown[k] = cooldown
		}
		if h.completedTaskPRURL != nil {
			h.completedTaskPRURL[k] = rec.PRURL
		}
	}
	h.logger.Info("[contribute-ws] loaded completed tasks", "count", len(h.completedTasks))
}

func (h *ContributeWSHub) saveCompletedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]completedTaskRecord, len(h.completedTasks))
	for k, t := range h.completedTasks {
		rec := completedTaskRecord{CompletedAt: t}
		if h.completedTaskCooldown != nil {
			if d, ok := h.completedTaskCooldown[k]; ok {
				rec.CooldownHours = d.Hours()
			}
		}
		if h.completedTaskPRURL != nil {
			rec.PRURL = h.completedTaskPRURL[k]
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] completed tasks marshal failed", "error", err)
		return
	}
	path := h.completedTasksPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) completedTasksPath() string {
	if h != nil && h.completedTasksFile != "" {
		return h.completedTasksFile
	}
	return completedTasksFile
}

// markTaskCompleted records a completed task and starts its issue cooldown.
//
// The cooldown length is conditional on whether the completion actually shipped
// a pull request (kubestellar/hive#2393 item 7): a completion WITH a prURL gets
// the full completedTaskCooldownHours (real work landed — don't re-dispatch for
// a week), while a completion WITHOUT one — the agent merely returned to idle —
// gets the short completedNoPRCooldownHours so an issue where nothing shipped
// is not locked out for a week. Since #3980 that short cooldown escalates on
// CONSECUTIVE no-PR completions of the same issue (see advanceNoPRStreakLocked)
// so a correct "nothing to ship" verdict cannot loop every 4h indefinitely.
// The chosen expiry is stored per task and honored
// by isTaskInCooldown; the prURL is retained for stats/audit and #2356
// duplicate detection.
func (h *ContributeWSHub) markTaskCompleted(repo string, number int, prURL string) {
	h.markTaskCompletedVerdict(repo, number, prURL, completionVerdictIdle, "", "")
}

// markTaskCompletedVerdict is markTaskCompleted plus the completion's
// normalized verdict (#3987). A no_work_needed verdict — the agent
// affirmatively determined nothing is shippable, as opposed to merely going
// idle — additionally records a durable entry in the noWorkVerdicts ledger,
// which withholds the issue from the OFFER pool for the full with-PR cooldown
// window unless/until the issue shows newer activity (see
// isSuppressedByNoWorkVerdict). The regular escalating no-PR cooldown is still
// booked as the backstop, so behavior for relays that never learn the verdict
// field — and for hubs where the verdict is later voided — is unchanged.
// reporter/reason are audit-only and stored with the verdict.
func (h *ContributeWSHub) markTaskCompletedVerdict(repo string, number int, prURL, verdict, reporter, reason string) {
	h.markTaskCompletedVerdictKey(worksource.Ref{Repo: repo, Number: number}.Key(), prURL, verdict, reporter, reason)
}

// markTaskCompletedVerdictKey books completion against the canonical identity
// (kubestellar/hive#4245), so a Linear or Jira task's cooldown and verdict land
// on that item rather than on a shared "repo#0" record that would suppress
// every other zero-numbered item in the repository.
func (h *ContributeWSHub) markTaskCompletedVerdictKey(key string, prURL, verdict, reporter, reason string) {
	if key == "" {
		return
	}
	// The WITH-PR period is operator-tunable (Config.Hub.ContributeCooldownHours,
	// default completedTaskCooldownHours). We still RECORD this even when cooldown
	// is disabled — isTaskInCooldown short-circuits the gating, so the history is
	// kept for stats/audit but never excludes the issue. It also caps the no-PR
	// escalation below. Read before taking completedMu: it walks server config,
	// which the lock has no business covering.
	withPRCooldown := h.configuredWithPRCooldown()
	h.completedMu.Lock()
	var cooldown time.Duration
	verdictLedgerDirty := false
	if prURL != "" {
		cooldown = withPRCooldown
		// A shipped, verified PR ends any no-PR streak: the issue's next no-PR
		// completion (if any) starts back at the short base cooldown.
		delete(h.noPRStreaks, key)
		// It also voids any standing no_work_needed verdict (#3987): real work
		// just landed, so "nothing is shippable" is no longer true.
		if _, had := h.noWorkVerdicts[key]; had {
			delete(h.noWorkVerdicts, key)
			verdictLedgerDirty = true
		}
	} else {
		// #3980: repeated no-PR completions escalate geometrically (4h → 8h →
		// …, capped at the with-PR cooldown) so a "nothing to ship" loop —
		// e.g. an issue whose remaining work is maintainer-gated (#2547) —
		// cannot burn a full contributor task cycle every 4h forever.
		cooldown = h.advanceNoPRStreakLocked(key, withPRCooldown)
		// #3987: an affirmative no_work_needed verdict eliminates that loop
		// rather than just bounding it — the issue is withheld from the offer
		// pool for the full with-PR window, subject to invalidation by newer
		// issue activity. Only the OFFER surfaces read this ledger; the
		// escalating cooldown above still stands as the backstop.
		if verdict == completionVerdictNoWorkNeeded {
			if len(reason) > noWorkReasonMaxLen {
				reason = reason[:noWorkReasonMaxLen]
			}
			h.noWorkVerdicts[key] = noWorkVerdictRecord{
				RecordedAt:    time.Now(),
				SuppressHours: withPRCooldown.Hours(),
				Reporter:      reporter,
				Reason:        reason,
			}
			verdictLedgerDirty = true
		}
	}
	h.completedTasks[key] = time.Now()
	if h.completedTaskCooldown != nil {
		h.completedTaskCooldown[key] = cooldown
	}
	if h.completedTaskPRURL != nil {
		h.completedTaskPRURL[key] = prURL
	}
	// A completion clears any failure history for the issue (#2435): the
	// consecutive-failure counter resets so a flaky-then-fixed issue does not
	// carry a stale quarantine, and the short failure cooldown is superseded by
	// the (longer) completion cooldown recorded just above.
	failureCleared := false
	if h.failedTasks != nil {
		if _, ok := h.failedTasks[key]; ok {
			delete(h.failedTasks, key)
			failureCleared = true
		}
	}
	if h.consecutiveFailures != nil {
		if _, ok := h.consecutiveFailures[key]; ok {
			delete(h.consecutiveFailures, key)
			failureCleared = true
		}
	}
	h.completedMu.Unlock()
	h.saveCompletedTasks()
	h.saveNoPRStreaks()
	if verdictLedgerDirty {
		h.saveNoWorkVerdicts()
	}
	if failureCleared {
		h.saveFailedTasks()
	}
}

// verifyReportedPR checks a client-reported PR URL against GitHub server-side
// before the hub trusts it for the LONG cooldown or for trust credit
// (kubestellar/hive#2565). The contributor relay scrapes a PR URL from tmux
// output and reports it on task_complete, preferring the assigned repo but
// falling back to the FIRST PR URL mentioned anywhere in the output — so the
// field is entirely client-supplied and, on its own, must not drive the 168h
// cooldown or newcomer→contributor promotion. #2437 raised the bar (PR required)
// but left this hole open because PRURL stayed unverified.
//
// It returns true only when the reported PR (1) exists, (2) has a BASE repo
// matching the assignment's repo, and (3) is authored by the connected
// contributor. Any other outcome — no URL reported, unparseable URL, wrong repo,
// wrong author, or a GitHub API error — returns false, and the completion is
// treated as an unverified/no-PR completion (short cooldown, no trust credit).
//
// Degradation is deliberate and safe: on a GitHub error (rate limit, transient,
// 404, or no client configured) we fail CLOSED on TRUST (no promotion credit)
// but never crash the completion handler or strand the contributor — the issue
// still gets the short anti-duplicate cooldown and the contributor keeps its
// TasksCompleted credit; only the PR-gated rewards are withheld. The reason is
// always logged for audit. We do not retry here: a completion is a single
// user-driven event, the relay can re-report on a later completion, and a
// blocking retry would hold the hub read loop.
func (h *ContributeWSHub) verifyReportedPR(assignedRepo, prURL, contributorUsername string) bool {
	return h.verifyReportedPRDetail(assignedRepo, prURL, contributorUsername).Verified
}

// verifyReportedPRDetail is verifyReportedPR's full result. Callers that need
// more than the trust verdict — notably "was this PR actually MERGED, and what
// was its title" — use this; everything about the verification itself is
// identical, so the contract documented on verifyReportedPR holds unchanged.
func (h *ContributeWSHub) verifyReportedPRDetail(assignedRepo, prURL, contributorUsername string) ghpkg.PRVerification {
	if prURL == "" {
		return ghpkg.PRVerification{Reason: "no PR URL reported"}
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		// No GitHub client (hive booted without credentials, or a bare test hub):
		// we cannot verify, so we must not grant trust. Degrade to unverified.
		h.logger.Warn("[contribute-ws] PR verification skipped: no github client",
			"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername)
		return ghpkg.PRVerification{Reason: "no github client configured"}
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res := h.server.deps.GHClient.VerifyReportedPR(ctx, assignedRepo, prURL, contributorUsername)
	if res.Verified {
		h.logger.Info("[contribute-ws] reported PR verified",
			"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername,
			"author", res.Author, "base_repo", res.BaseRepo, "merged", res.Merged)
		return res
	}
	// Distinguish a clean negative from an API error only in the log; both
	// downgrade to unverified.
	logArgs := []any{"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername, "reason", res.Reason}
	if res.Err != nil {
		logArgs = append(logArgs, "error", res.Err.Error())
	}
	h.logger.Warn("[contribute-ws] reported PR NOT verified — treating completion as no-PR", logArgs...)
	return res
}

// closeAdvisoryForMergedPR retires open advisory findings that a just-verified
// PR addresses, matching on title similarity (see ClosePRLinkedAdvisoryBeads).
//
// Gated by governor.advisory.pr_autoclose (default on) so an operator who does
// not want title-based closing can turn it off. Entirely best-effort: it never
// affects the completion outcome, and a finding closed in error comes straight
// back the next time an agent files it.
func (h *ContributeWSHub) closeAdvisoryForMergedPR(prTitle string) {
	if prTitle == "" || h.server == nil || h.server.deps == nil {
		return
	}
	deps := h.server.deps
	if len(deps.BeadStores) == 0 {
		return
	}
	if deps.Config != nil && !deps.Config.Governor.Advisory.PRAutoCloseEnabled() {
		return
	}
	if closed := advisory.ClosePRLinkedAdvisoryBeads(deps.BeadStores, prTitle); len(closed) > 0 {
		h.logger.Info("[contribute-ws] closed advisory findings addressed by merged PR",
			"pr_title", prTitle, "count", len(closed), "titles", strings.Join(closed, "; "))
	}
}

// prAttributionReconcileTimeout bounds the detached reconcile. Two GitHub round
// trips (get + edit), each already capped by the App client's own 30s timeout.
const prAttributionReconcileTimeout = 90 * time.Second

// reconcilePRAttribution reconciles the attribution trailer on a verified PR
// reported by a contributor relay (kubestellar/hive#4083). It uses the hub's own
// record of the connection (cliBackend, model, reasoningEffort) rather than
// self-reported metadata.
func (h *ContributeWSHub) reconcilePRAttribution(prURL string, contributor *ContributorConnection) {
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil || contributor == nil {
		return
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the work explicitly rather than inheriting the hub's process-lifetime
	// context: this runs detached (see the call site), so without a deadline a
	// stalled GitHub call would leak a goroutine holding a connection reference
	// for as long as the hub runs.
	ctx, cancel := context.WithTimeout(ctx, prAttributionReconcileTimeout)
	defer cancel()
	contributor.mu.Lock()
	meta := ghpkg.InvocationMeta{
		Agent:   contributor.role,
		Backend: contributor.cliBackend,
		Model:   ghpkg.RequestedModel(contributor.cliBackend, contributor.model),
		Effort:  contributor.reasoningEffort,
	}
	if contributor.capabilities != nil && contributor.capabilities.AgentCLIVersion != "" {
		meta.Tool = contributor.cliBackend
		meta.ToolVersion = contributor.capabilities.AgentCLIVersion
	}
	username := ""
	if contributor.profile != nil {
		username = contributor.profile.GitHubUsername
	}
	contributor.mu.Unlock()

	if err := h.server.deps.GHClient.ReconcilePRAttribution(ctx, prURL, meta); err != nil {
		h.logger.Warn("[contribute-ws] PR attribution reconciliation failed",
			"pr_url", prURL,
			"username", username,
			"error", err.Error(),
		)
	}
}

// cooldownEnabled reports whether post-completion cooldown gating is turned on
// for this hive. It reads the operator toggle (Config.Hub.ContributeCooldownEnabled)
// through the config resolver, which defaults to ENABLED when unset. A hub built
// without a Config (direct-in-test construction) is treated as enabled so the
// historical default behavior is preserved.
func (h *ContributeWSHub) cooldownEnabled() bool {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return true
	}
	return h.server.deps.Config.Hub.IsContributeCooldownEnabled()
}

// configuredWithPRCooldown returns the operator-configured WITH-PR completion
// cooldown duration. It reads Config.Hub.ContributeCooldownHoursOrDefault() —
// which yields the 168h default when unset — and falls back to the
// completedTaskCooldownHours const when no Config is present (tests). It does NOT
// consider whether cooldown is enabled; callers gate on cooldownEnabled().
func (h *ContributeWSHub) configuredWithPRCooldown() time.Duration {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return completedTaskCooldownHours * time.Hour
	}
	return time.Duration(h.server.deps.Config.Hub.ContributeCooldownHoursOrDefault()) * time.Hour
}

// cooldownForLocked returns the cooldown duration to apply to key. Callers must
// already hold completedMu. When no per-task override was recorded (older
// on-disk entries, or hubs built directly in tests) it falls back to the
// operator-configured with-PR cooldown (default completedTaskCooldownHours) — the
// original, conservative default.
func (h *ContributeWSHub) cooldownForLocked(key string) time.Duration {
	if h.completedTaskCooldown != nil {
		if d, ok := h.completedTaskCooldown[key]; ok {
			return d
		}
	}
	return h.configuredWithPRCooldown()
}

// isTaskInCooldown is the GitHub-shaped wrapper retained for the many call
// sites that legitimately hold a repo and an issue number. Identity-keyed
// callers (ReadyQueue, selectTask) use isTaskInCooldownKey directly so external
// work gets its own cooldown instead of sharing "repo#0" (kubestellar/hive#4245).
func (h *ContributeWSHub) isTaskInCooldown(repo string, number int) bool {
	return h.isTaskInCooldownKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) isTaskInCooldownKey(key string) bool {
	// Operator kill-switch: when cooldown is disabled, no completed issue is ever
	// gated out of the queue for cooldown. Completion HISTORY is still recorded by
	// markTaskCompleted (stats/audit, #2356 duplicate detection) and failure
	// quarantine is unaffected — this only stops cooldown from EXCLUDING work.
	if !h.cooldownEnabled() {
		return false
	}
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.completedTasks[key]
	if !ok {
		return false
	}
	if time.Since(t) > h.cooldownForLocked(key) {
		delete(h.completedTasks, key)
		delete(h.completedTaskCooldown, key)
		delete(h.completedTaskPRURL, key)
		return false
	}
	return true
}

// recordTaskFailure books a task_failed against an issue (#2435). It stamps the
// short failure cooldown and advances the issue's consecutive-failure counter
// (a permanent failure advances it by permanentFailureWeight rather than one),
// so a reliably-failing issue crosses consecutiveFailureQuarantineThreshold and
// earns the longer quarantine window instead of being handed straight back out.
// The counter is reset on completion (see markTaskCompleted).
func (h *ContributeWSHub) recordTaskFailure(repo string, number int, permanent bool) {
	h.recordTaskFailureKey(worksource.Ref{Repo: repo, Number: number}.Key(), permanent)
}

// recordTaskFailureForTask books a failure against the assignment's own
// canonical identity (kubestellar/hive#4245). Every caller already holds the
// WSTaskAssign, so routing through it keeps external work's failure history and
// quarantine separate instead of merging onto "repo#0".
func (h *ContributeWSHub) recordTaskFailureForTask(task *WSTaskAssign, permanent bool) {
	h.recordTaskFailureKey(task.identityKey(), permanent)
}

func (h *ContributeWSHub) recordTaskFailureKey(key string, permanent bool) {
	if key == "" {
		return
	}
	weight := 1
	if permanent {
		weight = permanentFailureWeight
	}
	h.completedMu.Lock()
	h.failedTasks[key] = time.Now()
	h.consecutiveFailures[key] += weight
	h.completedMu.Unlock()
	h.saveFailedTasks()
}

// bookReleaseCooldown stamps the SAME short cooldown recordTaskFailure stamps, but
// does NOT advance the issue's consecutive-failure counter (kubestellar/hive#4260).
//
// It exists for releases that are not failures OF THE ISSUE. The disconnect path
// books a cooldown for one reason (#2356): while a relay is reconnecting, its issue
// has dropped out of activeIssues — the only double-assign guard — and selectTask
// could hand the same issue to a second session, so both reach "open a PR" and file
// duplicates. That guarantee needs the timestamp and nothing else.
//
// Routing it through recordTaskFailure also incremented consecutiveFailures, so three
// dropped sockets on one issue — unremarkable on a flaky connection across a long
// session — pushed it past consecutiveFailureQuarantineThreshold and parked a
// perfectly workable issue for quarantineCooldownHours with nothing having actually
// failed. The counter also feeds recentFailureCount, which deprioritises the issue
// behind never-failed peers in selectTask, so a disconnect quietly demoted work that
// was progressing fine.
//
// The #2356 window is byte-for-byte unchanged: failedTasks carries the same timestamp,
// isTaskInFailureCooldown reads it the same way, and with the count left at zero
// failureCooldownForLocked returns the same failedTaskCooldownMinutes it always did.
// Paths where the release IS evidence about the issue — task_failed, the relay's own
// watchdog giving up via "ready", and the wedged-task lease backstop — deliberately
// keep calling recordTaskFailure and keep counting.
func (h *ContributeWSHub) bookReleaseCooldown(repo string, number int) {
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	h.failedTasks[key] = time.Now()
	h.completedMu.Unlock()
	h.saveFailedTasks()
}

// failureCooldownForLocked returns how long, from the last failure time, an
// issue should be excluded from selection. It is the SHORT
// failedTaskCooldownMinutes normally, or the LONGER quarantineCooldownHours once
// the issue's consecutive-failure count has reached
// consecutiveFailureQuarantineThreshold. Callers must hold completedMu.
func (h *ContributeWSHub) failureCooldownForLocked(key string) time.Duration {
	if h.consecutiveFailures[key] >= consecutiveFailureQuarantineThreshold {
		return quarantineCooldownHours * time.Hour
	}
	return failedTaskCooldownMinutes * time.Minute
}

// isTaskInFailureCooldown reports whether an issue is currently excluded because
// of a recent failure — either the short post-failure cooldown or the longer
// quarantine (#2435). It self-heals in two stages so the failure-aware selection
// backstop (recentFailureCount) still has something to work with just after the
// short window lapses:
//   - Once the APPLICABLE window (short cooldown, or quarantine) has elapsed the
//     issue is admissible again → returns false.
//   - The consecutive-failure COUNT is only cleared once the full quarantine
//     window has elapsed. So an issue whose short cooldown just expired is
//     admissible but still carries its failure history, letting selectTask
//     deprioritise it behind never-failed peers rather than instantly restoring
//     it to the head of the queue. The count also resets on completion.
func (h *ContributeWSHub) isTaskInFailureCooldown(repo string, number int) bool {
	return h.isTaskInFailureCooldownKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) isTaskInFailureCooldownKey(key string) bool {
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.failedTasks[key]
	if !ok {
		return false
	}
	// Keep the failure ledger for the full quarantine window (the longest window
	// we could apply), then clear timestamp AND count together so stale history
	// never lingers indefinitely. Retaining the timestamp past the short cooldown
	// is what preserves the consecutive-failure count for the selection backstop.
	if time.Since(t) > quarantineCooldownHours*time.Hour {
		delete(h.failedTasks, key)
		delete(h.consecutiveFailures, key)
		return false
	}
	// Inside the quarantine window: excluded only while within the currently
	// applicable cooldown (short cooldown, or the full quarantine once the count
	// crosses the threshold). Past that, the issue is admissible again but its
	// count remains on record — recentFailureCount reads it to deprioritise the
	// issue behind never-failed peers.
	return time.Since(t) <= h.failureCooldownForLocked(key)
}

// recentFailureCount returns the number of failures currently on record for an
// issue (0 when none/expired). selectTask uses it as a stable tie-break so that,
// among equally-admissible candidates, those without recent failures are offered
// before those that have failed recently — guaranteeing forward progress even if
// the failure cooldown has just elapsed (#2435 remedy 3 backstop).
func (h *ContributeWSHub) recentFailureCount(repo string, number int) int {
	return h.recentFailureCountKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) recentFailureCountKey(key string) int {
	if key == "" {
		return 0
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	return h.consecutiveFailures[key]
}

// The operator YANK (the repurposed manual requeue, kubestellar/hive#2568 + follow-up)
// is built from the SAME release+cooldown machinery below, split into composable pieces
// so the release can NOT reintroduce the duplicate-assignment race #2492/#2557 closed:
//
//  1. releaseHeldTasks — clear currentTask (dropping the issue from selectTask's
//     activeIssues guard), drop any pending credential, and bump the assignment
//     generation (#2568, the Gate) so a stale worker's later completion is fenced out.
//  2. bookAndRevokeReleased — book the SAME short non-permanent failure cooldown via
//     recordTaskFailure (so the released issue is not instantly re-admissible to a
//     stale worker) and push the EXISTING task_revoke message so the relay stops
//     cleanly and re-asks for work.
//  3. RequeueContributorTask — the public entry point. It runs (1)+(2) and then
//     IMMEDIATELY reassigns each released clanker its next-priority item via selectTask
//     (the yank behaviour), self-excluding the just-released issue from that clanker so
//     it moves to different work. When nothing else is admissible the clanker is simply
//     released + idle (the old requeue-only outcome, now the fallback).
//
// None of this mints or rotates a token or changes trust. Synthetic pr-review tasks
// carry Number == 0 and are released without booking an issue-key cooldown, exactly
// like the disconnect path. A blank operator reason falls back to a default label.

// releaseTarget pairs a connection with the task it was just released from. It is the
// shared unit releaseHeldTasks produces and bookAndRevokeReleased / the reassignment
// loop consume.
type releaseTarget struct {
	conn *ContributorConnection
	task WSTaskAssign
}

// releaseHeldTasks clears the in-flight task from every live connection registered to
// contributorID, applying the SAME fencing the operator-requeue/disconnect paths use:
// it nils currentTask (dropping the issue from selectTask's activeIssues guard), drops
// any pending credential, and BUMPS the assignment generation so a stale worker that
// later reports completion is rejected (#2568, the Gate). It only touches the
// connection state under the connection lock and returns the released targets; booking
// the cooldown and the network task_revoke are done by the CALLER, outside h.mu, so the
// hub lock is never held across a socket write. This is the exact machinery Requeue
// used inline before Yank needed to share it — behaviour is unchanged for Requeue.
func (h *ContributeWSHub) releaseHeldTasks(contributorID string) []releaseTarget {
	var targets []releaseTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		match := c.profile != nil && c.profile.ContributorID == contributorID && c.currentTask != nil
		if match {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			// #2537: drop any credential that was pending/held for the released task
			// so it cannot leak to the (now task-less) connection.
			c.pendingToken = ""
			c.credentialDelivered = false
			// #2568 (the Gate): fence the revoked worker — bump the generation so its
			// later completion/progress echoing the old generation is rejected.
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, releaseTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()
	return targets
}

// bookAndRevokeReleased books the SAME short failure cooldown the disconnect/ready-
// abandon paths book for each released issue and pushes the task_revoke frame to each
// still-connected relay, recording the operator's reason in the activity + hub logs.
// activityVerb is the leading label for the activity entry ("requeued by operator" or
// "yanked by operator") so the log distinguishes a plain requeue from a yank. Run
// OUTSIDE h.mu (targets already have their connection state cleared). Returns the count
// acted on. Shared by Requeue and Yank so both release identically.
func (h *ContributeWSHub) bookAndRevokeReleased(targets []releaseTarget, reason, activityVerb string) int {
	for _, tgt := range targets {
		// C4: an operator requeue releases the task — revoke its server-issued lease
		// so the released worker cannot re-adopt it via a later task_progress.
		h.revokeLease(identityOf(tgt.conn), tgt.task.TaskID)
		// Book the SAME short cooldown the disconnect/ready-abandon paths book, so
		// the released issue is not instantly re-offered. Only real issue tasks are
		// booked; synthetic pr-review tasks (Number == 0) must not poison an issue key.
		if tgt.task.Number > 0 {
			h.recordTaskFailureForTask(&tgt.task, false)
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		h.logger.Info("[contribute-ws] task released by operator",
			"username", username,
			"task", tgt.task.TaskID,
			"repo", tgt.task.Repo,
			"number", tgt.task.Number,
			"action", activityVerb,
			"reason", reason,
		)
		// #2568: record the operator's reason in the activity log so the release is
		// auditable (the reason rides in the Task field alongside the task id).
		h.addActivity(username, activityVerb+": "+reason, tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.conn.reasoningEffort, tgt.task.TaskID)
		// Push the EXISTING task_revoke message so the relay stops cleanly and
		// re-readies. Best-effort: if the socket is already gone the disconnect path
		// has (or will) release it anyway; the cooldown above is already booked. The
		// operator's reason travels on Reason so a still-connected worker learns WHY
		// its task was released (kubestellar/hive#2568).
		if tgt.conn.ws != nil {
			_ = tgt.conn.send(WSMessage{
				Type:   "task_revoke",
				Seq:    h.nextSeq(),
				TaskID: tgt.task.TaskID,
				Reason: reason,
			})
		}
	}
	return len(targets)
}

// DisconnectContributor closes every live WebSocket session held by the given
// contributor identity and revokes any server-issued task lease it holds (H2,
// CWE-613/639). It is called by the admin revoke/trust-downgrade path so a revoked
// contributor's in-flight sessions cannot keep working, keep the #2393 token-refresh
// cycle alive, or keep saving a stale non-revoked profile after the revoke lands. It
// returns the number of sessions closed. The socket close makes each session's read
// loop return, running its normal disconnect defer (task release + cooldown). We do
// the actual Close OUTSIDE h.mu, mirroring the other broadcast-ish paths, so a slow
// socket write never stalls the hub lock.
func (h *ContributeWSHub) DisconnectContributor(contributorID, reason string) int {
	if contributorID == "" {
		return 0
	}
	var closing []*ContributorConnection
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		match := c.profile != nil && c.profile.ContributorID == contributorID
		var taskID string
		if match && c.currentTask != nil {
			taskID = c.currentTask.TaskID
		}
		c.mu.Unlock()
		if match {
			// Revoke any server-issued lease so a reconnect cannot re-adopt the task.
			h.revokeLease(identityOf(c), taskID)
			closing = append(closing, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range closing {
		username := ""
		if c.profile != nil {
			username = c.profile.GitHubUsername
		}
		h.logger.Info("[contribute-ws] disconnecting contributor session", "username", username, "reason", reason)
		if c.ws != nil {
			// Best-effort notify then close; the read loop's defer does the release.
			_ = c.send(WSMessage{Type: "auth_failed", Seq: h.nextSeq(), Reason: reason})
			c.ws.Close()
		}
	}
	return len(closing)
}

// yankExcludeKey is the composite key for the per-(clanker, issue) yank self-exclusion.
// The NUL separator cannot appear in a contributor id or a "repo#number" key, so the two
// fields can never collide across a boundary.
func yankExcludeKey(contributorID, repo string, number int) string {
	return yankExcludeKeyFor(contributorID, worksource.Ref{Repo: repo, Number: number}.Key())
}

// yankExcludeKeyFor scopes a yank self-exclusion to (contributor, work item)
// using the canonical identity, so two zero-numbered external items do not
// share one exclusion (kubestellar/hive#4245).
func yankExcludeKeyFor(contributorID, itemKey string) string {
	return contributorID + "\x00" + itemKey
}

// isYankSelfExcluded reports whether repo#number is still within its brief yank
// self-exclusion window for contributorID (set the item was just yanked off this
// clanker). Expired entries are pruned lazily here. Caller must hold h.mu.
func (h *ContributeWSHub) isYankSelfExcludedLocked(contributorID, repo string, number int) bool {
	if number <= 0 {
		return false
	}
	return h.isYankSelfExcludedKeyLocked(contributorID, worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) isYankSelfExcludedKeyLocked(contributorID, itemKey string) bool {
	if itemKey == "" || len(h.yankExclusions) == 0 {
		return false
	}
	key := yankExcludeKeyFor(contributorID, itemKey)
	exp, ok := h.yankExclusions[key]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(h.yankExclusions, key)
		return false
	}
	return true
}

// RequeueContributorTask is the operator YANK control (kubestellar/hive#2568 +
// follow-up). It RELEASES every in-flight task held by contributorID back to the ready
// queue — booking the SAME short failure cooldown and BUMPING the assignment generation
// the automatic disconnect/ready-abandon paths use, so a released issue is not instantly
// re-handed to a stale worker (#2492/#2557) and a stale worker's later completion is
// fenced out (#2568, the Gate) — AND then IMMEDIATELY hands each released clanker its
// next-priority item via selectTask, so it keeps working instead of idling. The just-
// released issue is briefly self-excluded from THAT SAME clanker (yankSelfExcludeSeconds)
// so the reassignment moves it to genuinely DIFFERENT work; the issue stays offerable to
// every OTHER contributor immediately.
//
// It returns the number of sessions released and, for the LAST released connection, the
// task_assign message it was reassigned (nil when nothing admissible remained — the
// legitimate "released, now idle" fallback, i.e. the old requeue-only outcome). The name
// and the POST /api/contributors/{id}/requeue route are kept for wire/back-compat; the
// behaviour is the yank. The caller (HTTP handler) sends nothing further: this method
// already ships task_assign + delivers the credential to the relay, mirroring the ready-
// handler flow.
func (h *ContributeWSHub) RequeueContributorTask(contributorID, reason string) (released int, assigned *WSMessage) {
	if contributorID == "" {
		return 0, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = defaultYankReason
	}
	targets := h.releaseHeldTasks(contributorID)
	if len(targets) == 0 {
		return 0, nil
	}

	// Book the short cooldown + push task_revoke for every released session (the original
	// requeue behaviour). The self-exclusion + reassignment below is the yank addition:
	// the clanker is immediately handed different work rather than left idle.
	released = h.bookAndRevokeReleased(targets, reason, "yanked by operator")

	for _, tgt := range targets {
		// Briefly self-exclude the just-yanked issue from THIS clanker so its immediate
		// reassignment picks genuinely different work. Scoped to (contributor, issue) —
		// other contributors are unaffected. Synthetic pr-review tasks (Number == 0) do
		// not key an issue and are not excluded.
		if tgt.task.Number > 0 {
			h.mu.Lock()
			h.yankExclusions[yankExcludeKey(contributorID, tgt.task.Repo, tgt.task.Number)] =
				time.Now().Add(yankSelfExcludeSeconds * time.Second)
			h.mu.Unlock()
		}

		// Immediately offer the clanker its next-priority item. selectTask honours the
		// full priority order (operator-pinned → own work → label-affinity → fewer
		// failures → rest) and skips the self-excluded issue for this clanker.
		msg := h.selectTask(tgt.conn)
		assigned = msg
		if msg == nil || msg.Type == "task_unavailable" {
			// Released, but nothing else is admissible right now — the clanker is idle
			// only because the queue has no other work for it (everything held/filtered/
			// in cooldown). Record the idle reason so the ops tab can show it, exactly as
			// the ready handler does.
			if msg != nil {
				tgt.conn.mu.Lock()
				tgt.conn.lastIdleReason = msg.Reason
				tgt.conn.mu.Unlock()
			}
			continue
		}
		// A real task was assigned: ship it and (in auto-accept mode) deliver the
		// credential, mirroring the ready handler so the reassigned clanker starts
		// working without waiting for its next selectTask cycle.
		if tgt.conn.ws != nil {
			if err := tgt.conn.send(*msg); err != nil {
				h.logger.Warn("[contribute-ws] failed to send yank reassignment task_assign", "error", err)
				continue
			}
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		// WSMessage carries the canonical TaskKey additively (#4245); an older
		// record without one keys exactly as it always did.
		yankKey := msg.TaskKey
		if yankKey == "" {
			yankKey = worksource.Ref{Repo: msg.Repo, Number: msg.Number}.Key()
		}
		taskDesc := assignDesc(msg.Kind, yankKey, msg.Title, msg.TaskID)
		h.addActivity(username, "reassigned by yank", tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.conn.reasoningEffort, taskDesc)
		h.logger.Info("[contribute-ws] clanker reassigned after yank",
			"username", username, "task", msg.TaskID, "repo", msg.Repo, "number", msg.Number)
		if !h.requireExplicitAccept() {
			h.deliverTaskCredential(tgt.conn, "yank_reassign")
		}
	}
	return released, assigned
}

func (h *ContributeWSHub) nextSeq() int {
	h.mu.Lock()
	h.seq++
	s := h.seq
	h.mu.Unlock()
	return s
}

// nextTaskGen hands out a fresh, never-reused assignment GENERATION token
// (kubestellar/hive#2568, the Gate). It is minted for every task_assign, every
// task-adopting task_progress RESUME, and every release (disconnect, ready-abandon,
// operator requeue, lease-TTL expiry) — bumping on release is what fences a
// stale worker: the connection's currentTaskGen advances past whatever the stale
// worker still believes it holds. Monotonic (atomic, lock-free — see the taskGen
// field), so generations are strictly increasing across the life of the hub.
func (h *ContributeWSHub) nextTaskGen() uint64 {
	// Lock-free: atomic increment so this is safe to call from paths that already hold
	// h.mu (RequeueContributorTask / reclaimExpiredLeases run under h.mu.RLock). See
	// the taskGen field comment for why a mu-guarded counter would deadlock here.
	return h.taskGen.Add(1)
}

// generationAccepted reports whether a client message carrying clientGen (the
// TaskGen it echoed) may act on a connection whose current task generation is
// currentGen (kubestellar/hive#2568, the Gate). The rule:
//
//   - clientGen == 0 → an UNVERSIONED relay that never learned a generation. Accept
//     it and let the caller's pre-existing TaskID match decide, preserving backward
//     compatibility with relays that predate the lease token.
//   - clientGen == currentGen → the current owner. Accept.
//   - clientGen != currentGen → a STALE worker whose task was released/reassigned
//     (the connection's generation was bumped past it). REJECT, so its completion or
//     progress cannot overwrite the new owner's state.
//
// The caller MUST hold c.mu (currentTaskGen is read under it).
func generationAccepted(clientGen, currentGen uint64) bool {
	if clientGen == 0 {
		return true
	}
	return clientGen == currentGen
}

func (h *ContributeWSHub) ActiveCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]bool)
	for _, c := range h.connections {
		if c.profile != nil && c.profile.GitHubUsername != "" {
			seen[c.profile.GitHubUsername] = true
		}
	}
	return len(seen)
}

func (h *ContributeWSHub) ActiveSessionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.connections)
}

type ContributorLiveState struct {
	Active      bool           `json:"active"`
	CurrentTask *WSTaskAssign  `json:"current_task,omitempty"`
	Tasks       []WSTaskAssign `json:"tasks,omitempty"`
	Sessions    int            `json:"sessions"`
	Role        string         `json:"role,omitempty"`
}

func (h *ContributeWSHub) LiveStates() map[string]ContributorLiveState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]ContributorLiveState, len(h.connections))
	for _, c := range h.connections {
		c.mu.Lock()
		cid := ""
		if c.profile != nil {
			cid = c.profile.ContributorID
		}
		role := c.role
		stale := time.Since(c.lastPong) > wsHeartbeatTimeout
		var task *WSTaskAssign
		if c.currentTask != nil && !stale {
			t := *c.currentTask
			task = &t
		}
		c.mu.Unlock()
		if cid != "" && !stale {
			existing := out[cid]
			existing.Active = true
			existing.Sessions++
			if role != "" {
				existing.Role = role
			}
			if task != nil {
				existing.CurrentTask = task
				dupe := false
				for _, t := range existing.Tasks {
					if t.TaskID == task.TaskID {
						dupe = true
						break
					}
				}
				if !dupe {
					existing.Tasks = append(existing.Tasks, *task)
				}
			}
			out[cid] = existing
		}
	}
	return out
}

// RoleBreakdown returns a count of active connections grouped by role.
// Connections without a role (task-driven mode) are counted under "task-driven".
func (h *ContributeWSHub) RoleBreakdown() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	breakdown := make(map[string]int)
	for _, c := range h.connections {
		c.mu.Lock()
		role := c.role
		c.mu.Unlock()
		if role == "" {
			role = "task-driven"
		}
		breakdown[role]++
	}
	return breakdown
}

func (h *ContributeWSHub) SetAssignedAgentRole(contributorID, assignedRole string, grants []string) {
	if h == nil || contributorID == "" {
		return
	}
	assignedRole = normalizeAgentRole(assignedRole)
	effectiveRole := effectiveAssignedAgentRole(assignedRole)
	h.mu.RLock()
	var targets []*ContributorConnection
	for _, c := range h.connections {
		c.mu.Lock()
		matches := c.profile != nil && (c.profile.ContributorID == contributorID || strings.EqualFold(c.profile.GitHubUsername, contributorID))
		c.mu.Unlock()
		if matches {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.mu.Lock()
		if c.profile != nil {
			c.profile.AssignedAgentRole = assignedRole
			c.profile.AgentRoleGrants = append([]string(nil), grants...)
		}
		c.assignedRole = assignedRole
		c.role = effectiveRole
		c.mu.Unlock()
		label := effectiveRole
		if label == "" {
			label = "general work"
		}
		msg := fmt.Sprintf("role assigned: %s — your next task will be %s", label, label)
		if effectiveRole != "" {
			msg = fmt.Sprintf("role assigned: %s — your next task will be %s work", effectiveRole, effectiveRole)
		}
		if c.ws != nil {
			if err := c.send(WSMessage{Type: "notice", Seq: h.nextSeq(), Message: msg}); err != nil {
				h.logger.Warn("[contribute-ws] failed to send role assignment notice", "error", err)
			}
		}
	}
}

func (h *ContributeWSHub) SetContributorAgentRoleGrants(contributorID string, grants []string) {
	if h == nil || contributorID == "" {
		return
	}
	h.mu.RLock()
	var targets []*ContributorConnection
	for _, c := range h.connections {
		c.mu.Lock()
		matches := c.profile != nil && (c.profile.ContributorID == contributorID || strings.EqualFold(c.profile.GitHubUsername, contributorID))
		c.mu.Unlock()
		if matches {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.mu.Lock()
		if c.profile != nil {
			c.profile.AgentRoleGrants = append([]string(nil), grants...)
		}
		c.mu.Unlock()
	}
}

// FleetClanker is a read-only view of one connected contributor ("clanker")
// session as the operator-facing Management & Operations tab renders it. It
// carries only what the contributor handshake already put on the wire plus the
// live connection timing the hub already tracks — no secrets, no new state.
type FleetClanker struct {
	ContributorID   string        `json:"contributor_id"`
	GitHubUsername  string        `json:"github_username,omitempty"`
	CLIBackend      string        `json:"cli_backend,omitempty"`
	Model           string        `json:"model,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Role            string        `json:"role,omitempty"`
	ClientRole      string        `json:"client_role,omitempty"`
	AssignedRole    string        `json:"assigned_agent_role,omitempty"`
	RoleMismatch    string        `json:"role_mismatch,omitempty"`
	TrustTier       string        `json:"trust_tier,omitempty"`
	ConnectedAt     string        `json:"connected_at,omitempty"`
	LastActivity    string        `json:"last_activity,omitempty"`
	Stale           bool          `json:"stale,omitempty"`
	CurrentTask     *WSTaskAssign `json:"current_task,omitempty"`
	// IdleReason is the machine-readable reason this clanker currently has no work
	// (#2546): one of the taskUnavailable* reasons last sent to it. Empty when the
	// clanker is actively working (CurrentTask set) or has never been refused. It
	// lets the operator distinguish "idle: no_matching_work" from "idle:
	// contribution_suspended" instead of an undifferentiated idle. Read-only.
	IdleReason string `json:"idle_reason,omitempty"`
	// PromptPreview is the exact assignment prompt built for CurrentTask (#2539),
	// surfaced read-only so an operator can see the instruction the agent is
	// running. It NEVER contains the minted github_token — the token travels on the
	// task_assign WSMessage separately and is not stored here. Empty when idle.
	PromptPreview string `json:"prompt_preview,omitempty"`
	// Capabilities is the client-declared runtime posture from the handshake
	// (#2547 declare half): container runtime, OS/arch, agent/relay versions,
	// credential type. Nil when the client declared none (unversioned client).
	// Surfaced read-only exactly like CLIBackend/Model/Role so the Operations tab
	// COULD display it; it is NEVER used to route or gate work.
	Capabilities *ContributorCapabilities `json:"capabilities,omitempty"`
	// LastFailure is the most recent task failure this clanker reported (#2547),
	// surfaced read-only so an operator can attribute a run of failures instead
	// of inferring the cause from a tmux tail. Its Kind is SELF-REPORTED by the
	// client and advisory — the hub records and displays it, and never routes,
	// gates, or adjusts a work item's failure cooldown on it. Nil until this
	// connection has failed a task.
	LastFailure *ContributorFailure `json:"last_failure,omitempty"`
	// LabelInterests (#2677) mirrors the contributor's own OPT-IN label-affinity
	// list (#2637, ContributorProfile.LabelInterests) so an operator can see
	// fleet-wide who prefers what without cross-referencing each profile
	// separately. Strictly READ-ONLY here: an operator never sets or edits this
	// through the fleet view — it stays contributor-owned via the existing
	// PUT /api/contribute/interests. Omitted when the contributor has declared
	// none.
	LabelInterests []string `json:"label_interests,omitempty"`
	// AgentRoleGrants is the operator-managed grant list for privileged spoke
	// agent roles. It is shown to owner/read-write viewers in the fleet row; the
	// server-side mutation endpoint remains the enforcement boundary.
	AgentRoleGrants []string `json:"agent_role_grants,omitempty"`
	// Protocol compares the contributor-protocol version this client DECLARED
	// against the one this hub speaks (#2547 peer-compatibility criterion,
	// building on the versions #2567 put on the wire). Always set — an
	// unversioned relay reports verdict "unknown", which is a supported state,
	// not a fault. Derived per snapshot from Capabilities.RelayProtocolVersion;
	// it stores nothing new and, like Capabilities, is never routed or gated on.
	Protocol *ProtocolCompat `json:"protocol,omitempty"`
}

// FleetWorkItem is a read-only view of one in-flight task the fleet is working,
// surfaced the way the operator work-list lists items (repo / number / title /
// who is on it / status). Derived entirely from live connection state — the hub
// tracks currentTask per connection; nothing here is fabricated.
type FleetWorkItem struct {
	TaskID         string `json:"task_id"`
	Kind           string `json:"kind,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Number         int    `json:"number,omitempty"`
	Title          string `json:"title,omitempty"`
	ContributorID  string `json:"contributor_id,omitempty"`
	GitHubUsername string `json:"github_username,omitempty"`
	CLIBackend     string `json:"cli_backend,omitempty"`
	Status         string `json:"status"`
	// Labels are the chosen issue's labels (#2539), shown alongside the prompt
	// preview in the ops Task panel. Metadata only.
	Labels []string `json:"labels,omitempty"`
	// PromptPreview is the exact prompt shipped for this work item (#2539),
	// surfaced read-only in the ops Task panel so the instruction is legible
	// before/as it runs. It NEVER contains the github_token. Empty if unknown.
	PromptPreview string `json:"prompt_preview,omitempty"`
}

// FleetSnapshot is the read-only payload the Management & Operations tab hydrates
// from. Everything is derived from the hub's current live connections — it adds
// no enforcement and mutates nothing.
type FleetSnapshot struct {
	Clankers []FleetClanker  `json:"clankers"`
	Work     []FleetWorkItem `json:"work"`
}

// FleetSnapshot returns the current connected-clanker fleet and its in-flight
// work, read-only, from the hub's live connection registry. A connection whose
// last pong is older than wsHeartbeatTimeout is reported with Stale=true and its
// in-flight task is treated as no longer active (matching LiveStates()).
func (h *ContributeWSHub) FleetSnapshot() FleetSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap := FleetSnapshot{
		Clankers: make([]FleetClanker, 0, len(h.connections)),
		Work:     make([]FleetWorkItem, 0),
	}
	for _, c := range h.connections {
		c.mu.Lock()
		fc := FleetClanker{
			CLIBackend:      c.cliBackend,
			Model:           c.model,
			ReasoningEffort: c.reasoningEffort,
			Role:            c.role,
			ClientRole:      c.clientRole,
			AssignedRole:    c.assignedRole,
			ConnectedAt:     c.connectedAt.UTC().Format(time.RFC3339),
			LastActivity:    c.lastPong.UTC().Format(time.RFC3339),
			Stale:           time.Since(c.lastPong) > wsHeartbeatTimeout,
		}
		if c.assignedRole != "" && normalizeAgentRole(c.clientRole) != "" && normalizeAgentRole(c.clientRole) != effectiveAssignedAgentRole(c.assignedRole) {
			if c.assignedRole == "none" {
				fc.RoleMismatch = fmt.Sprintf("client requested %s; owner assigned general work", c.clientRole)
			} else {
				fc.RoleMismatch = fmt.Sprintf("client requested %s; owner assigned %s", c.clientRole, c.assignedRole)
			}
		}
		// #2547: surface the client-declared capabilities read-only (a copy so the
		// snapshot never aliases live connection state). Nil for unversioned clients.
		if c.capabilities != nil {
			capsCopy := *c.capabilities
			fc.Capabilities = &capsCopy
		}
		// #2547 peer-compatibility: derive the hub-vs-client protocol comparison
		// from the version already declared above. Always present so the operator
		// row can show the hub's own version even when the client declared none —
		// a bare "proto 1.1" chip with nothing to compare it against was the gap.
		declaredVersion := ""
		if c.capabilities != nil {
			declaredVersion = c.capabilities.RelayProtocolVersion
		}
		compat := peerProtocolCompat(declaredVersion)
		fc.Protocol = &compat
		// #2547: same treatment for the last reported failure — a copy, with the
		// client's free-text reason redacted and bounded. Reason is the only
		// CLIENT-CONTROLLED free text on this snapshot: it is an error string the
		// relay chose, so it can carry a token it happened to print, and it has no
		// length the client is obliged to respect. Both are handled here, at the
		// boundary where it becomes operator-visible.
		if c.lastFailure != nil {
			failCopy := *c.lastFailure
			failCopy.Reason = truncateFailureReason(redactTokens(failCopy.Reason))
			fc.LastFailure = &failCopy
		}
		if c.profile != nil {
			fc.ContributorID = c.profile.ContributorID
			fc.GitHubUsername = c.profile.GitHubUsername
			fc.TrustTier = c.profile.TrustTier
			// #2677: mirror the contributor's own label interests read-only (a copy
			// so the snapshot never aliases the live profile slice).
			if len(c.profile.LabelInterests) > 0 {
				fc.LabelInterests = append([]string(nil), c.profile.LabelInterests...)
			}
			if len(c.profile.AgentRoleGrants) > 0 {
				fc.AgentRoleGrants = append([]string(nil), c.profile.AgentRoleGrants...)
			}
		}
		var task *WSTaskAssign
		var promptPreview string
		var taskLabels []string
		if c.currentTask != nil && !fc.Stale {
			t := *c.currentTask
			task = &t
			// #2539: surface the stored prompt (never the token) for the active
			// task so the ops tab can preview the instruction being run.
			promptPreview = c.currentPrompt
			if len(c.currentLabels) > 0 {
				taskLabels = append([]string(nil), c.currentLabels...)
			}
		}
		// #2546: when the clanker is NOT actively working, expose why it is idle so
		// the operator sees "idle: no_matching_work" etc. Suppressed while a task is
		// in flight (the reason, if any, is stale then).
		if task == nil {
			fc.IdleReason = c.lastIdleReason
		}
		c.mu.Unlock()
		fc.CurrentTask = task
		fc.PromptPreview = promptPreview
		if task != nil {
			snap.Work = append(snap.Work, FleetWorkItem{
				TaskID:         task.TaskID,
				Kind:           task.Kind,
				Repo:           task.Repo,
				Number:         task.Number,
				Title:          task.Title,
				ContributorID:  fc.ContributorID,
				GitHubUsername: fc.GitHubUsername,
				CLIBackend:     fc.CLIBackend,
				Status:         "in-progress",
				Labels:         taskLabels,
				PromptPreview:  promptPreview,
			})
		}
		snap.Clankers = append(snap.Clankers, fc)
	}
	// Deterministic order so the operator view is stable across polls.
	sort.Slice(snap.Clankers, func(i, j int) bool {
		if snap.Clankers[i].ConnectedAt != snap.Clankers[j].ConnectedAt {
			return snap.Clankers[i].ConnectedAt < snap.Clankers[j].ConnectedAt
		}
		return snap.Clankers[i].ContributorID < snap.Clankers[j].ContributorID
	})
	sort.Slice(snap.Work, func(i, j int) bool {
		if snap.Work[i].Repo != snap.Work[j].Repo {
			return snap.Work[i].Repo < snap.Work[j].Repo
		}
		return snap.Work[i].Number < snap.Work[j].Number
	})
	return snap
}

// CooldownCounts returns two read-only tallies the Operations/Management tabs
// surface next to the ready queue (see handleContributeFleet):
//
//   - cooldown: how many completed issues are STILL within their cooldown window
//     and therefore held out of selection. It counts only NON-expired entries in
//     completedTasks (an entry past its cooldownForLocked() period is expired-but-
//     not-yet-swept and must not inflate the count — it matches what
//     isTaskInCooldown would actually gate). When cooldown is disabled by the
//     operator kill-switch (cooldownEnabled()==false), nothing is gated, so this
//     is 0.
//   - inFlight: how many distinct issues are currently held by a live
//     connection — reuses activeIssueKeys(), the SAME set selectTask uses as its
//     double-assign guard and ReadyQueue uses to exclude in-flight work.
//
// Read-only: it mutates nothing (unlike isTaskInCooldown it does not sweep
// expired entries) and adds no enforcement.
func (h *ContributeWSHub) CooldownCounts() (cooldown, inFlight int) {
	// cooldown: count non-expired completedTasks under the completion lock. When the
	// operator kill-switch disables cooldown, nothing is gated, so the count is 0.
	if h.cooldownEnabled() {
		h.completedMu.Lock()
		for key, t := range h.completedTasks {
			if time.Since(t) <= h.cooldownForLocked(key) {
				cooldown++
			}
		}
		h.completedMu.Unlock()
	}

	// inFlight: distinct issues held by a live connection — the same activeIssues
	// set selectTask's guard and ReadyQueue use, so the header count matches what is
	// actually excluded from "ready".
	inFlight = len(h.activeIssueKeys())
	return cooldown, inFlight
}

// HeldCount returns how many OPERATOR-HELD issues are also present in the current
// actionable universe — i.e. held issues that WOULD be offerable if not parked. It
// mirrors what the ready queue actually surfaces as Held (ReadyQueue appends exactly
// these), so the header "N on hold" tally matches the greyed rows the operator sees,
// rather than counting stale hold keys for issues no longer actionable. Read-only:
// it mutates nothing and adds no enforcement. Uses the SAME canonical "%s#%d" key
// form (repo.Full # number) every admission check builds, so it cannot silently miss
// on a repo-name spelling mismatch (the #2648 class of bug).
func (h *ContributeWSHub) HeldCount() int {
	if h == nil || h.server == nil {
		return 0
	}
	var hold map[string]struct{}
	if h.server.deps != nil && h.server.deps.Config != nil {
		hold = queueHoldSet(h.server.deps.Config.Hub.ContributeQueueHold)
	}
	if len(hold) == 0 {
		return 0
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return 0
	}
	count := 0
	for _, repo := range status.Repos {
		for _, raw := range repo.ActionableIssues {
			b, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			var issue map[string]any
			if err := json.Unmarshal(b, &issue); err != nil {
				continue
			}
			number := 0
			switch n := issue["number"].(type) {
			case float64:
				number = int(n)
			case int:
				number = n
			}
			if number == 0 {
				continue
			}
			if _, isHeld := hold[fmt.Sprintf("%s#%d", repo.Full, number)]; isHeld {
				count++
			}
		}
	}
	return count
}

func (h *ContributeWSHub) ActiveConnections() []ContributorConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ContributorConnection, 0, len(h.connections))
	for _, c := range h.connections {
		c.mu.Lock()
		out = append(out, ContributorConnection{
			profile:         c.profile,
			cliBackend:      c.cliBackend,
			model:           c.model,
			reasoningEffort: c.reasoningEffort,
			role:            c.role,
			clientRole:      c.clientRole,
			assignedRole:    c.assignedRole,
			connectedAt:     c.connectedAt,
			currentTask:     c.currentTask,
			tmuxOutput:      append([]string{}, c.tmuxOutput...),
		})
		c.mu.Unlock()
	}
	return out
}

const maxWSConnections = 50

func (h *ContributeWSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	// SECURITY (audit F9, CWE-770): the cap must count sockets that are still
	// authenticating, not just authenticated ones.
	//
	// h.connections only gains an entry after auth succeeds, so capping on it
	// alone bounded exactly the connections that had already proven who they
	// were, while leaving the pre-auth window unbounded. An unauthenticated
	// client could hold arbitrarily many sockets — each costing a goroutine, a
	// file descriptor and a read buffer for a full wsAuthTimeout — and the only
	// parties actually limited were legitimate contributors.
	h.mu.RLock()
	count := len(h.connections)
	h.mu.RUnlock()
	// Reserve the pending slot atomically as part of the admission check,
	// BEFORE the upgrade handshake. The old order (check Load(), upgrade, then
	// Add(1)) had a TOCTOU window: the 101 response reaches the client before
	// the counter moves, so a burst of dials could each pass the check against
	// the stale count and briefly push the total past the cap — observed as a
	// flake in TestF9_UnauthenticatedConnectionsCountTowardCap (#3908).
	// Add-then-check leaves no such window: once a dial completes, its slot is
	// already counted, and concurrent handlers each see the other's
	// reservation.
	if int64(count)+h.pendingConns.Add(1) > maxWSConnections {
		h.pendingConns.Add(-1)
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}
	// Held for the whole handler: a socket occupies a slot from reservation
	// until this function returns, whether it upgrades, authenticates, times
	// out, or errors. Released exactly once so no early return can leak a slot
	// and permanently shrink the cap.
	pendingReleased := false
	defer func() {
		if !pendingReleased {
			h.pendingConns.Add(-1)
		}
	}()

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("ws upgrade failed", "error", err)
		return
	}
	conn.SetReadLimit(wsMaxMessageSize)

	connID := randomHex(8)
	h.logger.Info("[contribute-ws] new connection", "id", connID)

	nonce := randomHex(16)
	if err := sendJSON(conn, WSMessage{Type: "auth_challenge", Seq: 1, Nonce: nonce}); err != nil {
		h.logger.Warn("[contribute-ws] failed to send challenge", "id", connID, "error", err)
		return
	}

	authDone := make(chan *ContributorConnection, 1)
	go func() {
		select {
		case <-time.After(wsAuthTimeout):
			_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: "Authentication timeout"})
			conn.Close()
		case <-authDone:
		}
	}()

	var contributor *ContributorConnection
	defer func() {
		if contributor != nil && contributor.profile != nil {
			contributor.mu.Lock()
			abandonedTask := contributor.currentTask
			contributor.currentTask = nil
			// #2568: bump the generation on release so any late message from this
			// now-defunct socket carrying the old generation is fenced.
			contributor.currentTaskGen = h.nextTaskGen()
			contributor.lastLeaseRenew = time.Time{}
			contributor.tokenMintedAt = time.Time{}
			// #2537: clear any pending/delivered credential state with the task.
			contributor.pendingToken = ""
			contributor.credentialDelivered = false
			contributor.mu.Unlock()
			if abandonedTask != nil {
				h.logger.Warn("[contribute-ws] task released on disconnect",
					"username", contributor.profile.GitHubUsername,
					"task", abandonedTask.TaskID,
				)
				// #2356: a disconnect drops the issue out of activeIssues (the only
				// double-assign guard) WITHOUT recording any cooldown, so selectTask
				// could hand the SAME issue to another session in the brief reconnect
				// window (BASE_RECONNECT_DELAY_MS..MAX_RECONNECT_DELAY_MS, i.e. 1s–60s)
				// while the original relay — which keeps currentTask locally and
				// re-asserts it via task_progress on reconnect — is still working it.
				// Both sessions then reach "open a PR" and file duplicates. Book the
				// SHORT cooldown so the issue is not instantly re-admissible. The short
				// window comfortably outlasts the reconnect backoff, so the returning
				// session re-asserts and resumes (repopulating activeIssues) before the
				// cooldown lapses — which is the contract #4260 restored by renewing
				// the lease on every progress report, so a task alive longer than
				// leaseTTL is still re-adoptable. Only real issue tasks are booked —
				// synthetic pr-review tasks carry Number == 0 and must not poison an
				// issue key.
				//
				// #4260: bookReleaseCooldown rather than recordTaskFailure. The window
				// is identical; what is dropped is the consecutive-failure increment,
				// which turned three dropped sockets on one issue into a
				// quarantineCooldownHours quarantine of an issue nobody had failed.
				// The #2356 duplicate-PR guarantee lives entirely in the timestamp and
				// is unaffected.
				if abandonedTask.Number > 0 {
					h.bookReleaseCooldown(abandonedTask.Repo, abandonedTask.Number)
				}
			}
			h.mu.Lock()
			delete(h.connections, connID)
			h.mu.Unlock()
			h.logger.Info("[contribute-ws] disconnected", "username", contributor.profile.GitHubUsername)
			h.addActivity(contributor.profile.GitHubUsername, "left", contributor.role, contributor.cliBackend, contributor.model, contributor.reasoningEffort, "")
		}
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				h.logger.Warn("[contribute-ws] read error", "id", connID, "error", err)
			}
			return
		}

		var msg WSMessage
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "auth_response":
			if msg.RegistrationToken == "" {
				_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: "Missing registration token"})
				conn.Close()
				return
			}

			tokenHash := sha256Hex(msg.RegistrationToken)
			profiles := listContributorProfiles()
			var profile *ContributorProfile
			for i := range profiles {
				if secureCompare(profiles[i].RegistrationToken, tokenHash) {
					profile = &profiles[i]
					break
				}
			}

			if profile == nil {
				_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: "Invalid registration token"})
				conn.Close()
				return
			}

			if profile.TrustTier == "revoked" {
				_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: "Access has been revoked"})
				conn.Close()
				return
			}

			if allowed, acceptedModels := h.checkModelAllowed(msg.Model); !allowed {
				reason := fmt.Sprintf("Model %q is not accepted by this hive", msg.Model)
				if msg.Model == "" {
					reason = "No model specified — this hive requires an accepted model"
				}
				_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: reason, AcceptedModels: acceptedModels})
				h.logger.Info("[contribute-ws] model rejected", "username", profile.GitHubUsername, "model", msg.Model)
				conn.Close()
				return
			}

			clientRole := normalizeAgentRole(msg.Role)
			assignedRole := normalizeAgentRole(profile.AssignedAgentRole)
			requestedRole := clientRole
			if hasOwnerAgentRoleAssignment(profile) {
				requestedRole = effectiveAssignedAgentRole(assignedRole)
			}
			probeContributor := &ContributorConnection{profile: profile}
			if requestedRole != "" {
				if ok, reason := h.roleClaimAllowed(probeContributor, requestedRole); !ok {
					_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: reason, Role: requestedRole})
					h.logger.Warn("[contribute-ws] agent role claim rejected",
						"username", profile.GitHubUsername, "tier", profile.TrustTier,
						"role", requestedRole, "reason", reason)
					conn.Close()
					return
				}
			}

			profile.LastActive = time.Now().UTC().Format(time.RFC3339)
			if msg.CLIBackend != "" {
				profile.CLIBackend = msg.CLIBackend
			}
			if msg.Model != "" {
				profile.Model = msg.Model
			}
			if msg.ReasoningEffort != "" {
				profile.ReasoningEffort = msg.ReasoningEffort
			}
			if profile.AvatarURL == "" {
				profile.AvatarURL = fmt.Sprintf("https://github.com/%s.png", profile.GitHubUsername)
			}
			if clientRole != "" {
				profile.PreferredRole = clientRole
			}
			_ = saveContributorProfile(profile)

			// #2547 declare half: capture the client-declared capabilities, if any.
			// A relay may report its runtime posture either as a nested
			// "capabilities" object or (for a version-only client) just a top-level
			// protocol_version; fold the latter in so it is surfaced consistently.
			// Entirely optional — a client that sends neither leaves caps nil and is
			// treated exactly as an unversioned client. Never routed/gated on.
			var caps *ContributorCapabilities
			declared := ContributorCapabilities{}
			if msg.Capabilities != nil {
				declared = *msg.Capabilities
			}
			if declared.RelayProtocolVersion == "" && msg.ProtocolVersion != "" {
				declared.RelayProtocolVersion = msg.ProtocolVersion
			}
			// Bound and clean it before it is stored: a declaration is unverified
			// client text that lives for the connection, is re-serialized into
			// every fleet poll, and lands in an operator row. Sanitizing cannot
			// reject — an over-long or messy declaration still authenticates, it
			// just cannot spill past its field. Checked AFTER sanitizing so a
			// declaration made entirely of whitespace reads as "declared nothing"
			// rather than as an empty-stringed capability set.
			declared = declared.Sanitized()
			if !declared.IsZero() {
				c := declared
				caps = &c
			}

			// #2547 (peer-compatibility criterion): compare the declared version
			// with ours and say so ONCE, at the log level the verdict deserves, so
			// "an old relay against a new hub" is legible in the hub log instead of
			// only discoverable by watching it misbehave. This is a REPORT, not a
			// gate — admission continues unchanged for every verdict, including
			// protoPeerIncompatible, because compatibility here has to be carried by
			// the defaults (there is no negotiation to carry it) and rejecting on a
			// client-declared string would strand relays written before any change.
			switch verdict := classifyPeerProtocol(declared.RelayProtocolVersion); verdict {
			case protoPeerIncompatible, protoPeerMalformed:
				h.logger.Warn("[contribute-ws] contributor protocol mismatch (advisory; client still served)",
					"contributor", profile.ContributorID, "verdict", verdict,
					"client_version", declared.RelayProtocolVersion, "hub_version", contributorProtocolVersion)
			case protoPeerOlder, protoPeerNewer:
				h.logger.Info("[contribute-ws] contributor protocol drift (advisory; client still served)",
					"contributor", profile.ContributorID, "verdict", verdict,
					"client_version", declared.RelayProtocolVersion, "hub_version", contributorProtocolVersion)
			}

			contributor = &ContributorConnection{
				ws:              conn,
				profile:         profile,
				cliBackend:      msg.CLIBackend,
				model:           msg.Model,
				reasoningEffort: msg.ReasoningEffort,
				role:            requestedRole,
				clientRole:      clientRole,
				assignedRole:    assignedRole,
				connectedAt:     time.Now(),
				lastPong:        time.Now(),
				capabilities:    caps,
			}

			// Hand the slot over from the pending counter to h.connections
			// under the same lock, so the connection is counted exactly once
			// and the total never dips (which would briefly let the cap be
			// exceeded) nor double-counts (which would halve it). The deferred
			// Add(-1) in HandleWS is disarmed by this flag.
			h.mu.Lock()
			h.connections[connID] = contributor
			h.mu.Unlock()
			if !pendingReleased {
				pendingReleased = true
				h.pendingConns.Add(-1)
			}

			var perms []string
			switch profile.TrustTier {
			case "newcomer":
				perms = []string{"issues:write"}
			case "contributor":
				perms = []string{"issues:write", "contents:write", "pulls:write"}
			case "trusted":
				perms = []string{"issues:write", "contents:write", "pulls:write", "checks:read"}
			case "merger":
				perms = []string{"issues:write", "contents:write", "pulls:write", "checks:read"}
			case "advisor":
				perms = []string{"metadata:read", "pulls:read"}
			default:
				perms = []string{"metadata:read"}
			}

			if err := contributor.send(WSMessage{
				Type:          "auth_ok",
				Seq:           h.nextSeq(),
				ContributorID: profile.ContributorID,
				TrustTier:     profile.TrustTier,
				Permissions:   perms,
				Role:          requestedRole,
				// #2567: advertise the protocol version and the server capability
				// set so a client can learn what this deployed hub supports without
				// probing. Additive — an existing client ignores these unknown fields.
				ProtocolVersion:    contributorProtocolVersion,
				ServerCapabilities: serverCapabilities(),
			}); err != nil {
				h.logger.Warn("[contribute-ws] failed to send auth_ok", "username", profile.GitHubUsername, "error", err)
				return
			}

			h.logger.Info("[contribute-ws] authenticated",
				"username", profile.GitHubUsername,
				"tier", profile.TrustTier,
				"cli", msg.CLIBackend,
				"role", requestedRole,
			)
			h.addActivity(profile.GitHubUsername, "joined", requestedRole, msg.CLIBackend, msg.Model, msg.ReasoningEffort, "")

			select {
			case authDone <- contributor:
			default:
			}

			go h.heartbeatLoop(contributor)

		case "ready":
			if contributor == nil {
				continue
			}
			contributor.mu.Lock()
			abandoned := contributor.currentTask
			contributor.currentTask = nil
			// #2568: bump the generation on release so a re-`ready` abandon fences any
			// later message echoing the old generation for the just-abandoned task.
			contributor.currentTaskGen = h.nextTaskGen()
			contributor.lastLeaseRenew = time.Time{}
			contributor.tokenMintedAt = time.Time{}
			// #2537: clear any pending/delivered credential state with the task.
			contributor.pendingToken = ""
			contributor.credentialDelivered = false
			contributor.mu.Unlock()
			if abandoned != nil {
				// C4: the relay explicitly gave up this task, so revoke its
				// server-issued lease — a later task_progress for it must not resurrect
				// ownership.
				h.revokeLease(identityOf(contributor), abandoned.TaskID)
				h.logger.Warn("[contribute-ws] task abandoned without completion",
					"username", contributor.profile.GitHubUsername,
					"abandoned_task", abandoned.TaskID,
				)
				// kubestellar/hive#2545: a contributor that sends "ready" while
				// still holding a task (e.g. the relay's own MAX_TASK_DURATION_MS
				// watchdog gives up and requeues, or an agent that never actually
				// started work asks for something new) used to leave currentTask
				// set and booked no cooldown at all — worse than the disconnect
				// path immediately above (#2356/#2435), which does both. That left
				// the abandoned issue permanently out of activeIssues circulation
				// for the life of the connection: no PR, no failure record, no
				// re-offer, just a silently held slot. Clear currentTask (above)
				// so selectTask's activeIssues scan releases the issue, and mirror
				// the disconnect/task_failed paths by booking the SAME short
				// non-permanent failure cooldown, so the just-abandoned issue is
				// not instantly handed straight back to the same contributor in
				// the very selectTask call below. Synthetic pr-review tasks carry
				// Number == 0 and must not poison an issue key.
				if abandoned.Number > 0 {
					h.recordTaskFailureForTask(abandoned, false)
				}
			}
			h.logger.Info("[contribute-ws] ready for work",
				"username", contributor.profile.GitHubUsername,
				"role", contributor.role,
			)
			task := h.selectTask(contributor)
			switch {
			case task == nil:
				// Defensive backstop only: after #2436 and #2546 every selectTask
				// path returns an explicit message, so this should not be reached.
				// Kept so an unforeseen nil still fails safe (no send) rather than
				// panicking.
				h.logger.Info("[contribute-ws] no tasks available",
					"username", contributor.profile.GitHubUsername,
				)
			case task.Type == "task_unavailable":
				// An explicit negative-ack rather than silence. #2436 finding 1/2/3
				// covers the enforced refusals (mint failure, disabled tier,
				// concurrency limit); #2546 adds the three formerly-silent
				// no-work-right-now reasons (contribution_suspended, hub_not_ready,
				// no_matching_work). Record the reason on the connection so the ops
				// tab can show WHY this clanker is idle, then send it.
				contributor.mu.Lock()
				contributor.lastIdleReason = task.Reason
				contributor.mu.Unlock()
				if err := contributor.send(*task); err != nil {
					h.logger.Warn("[contribute-ws] failed to send task_unavailable", "error", err)
					return
				}
				h.logger.Info("[contribute-ws] task unavailable",
					"username", contributor.profile.GitHubUsername,
					"reason", task.Reason,
				)
			default:
				if err := contributor.send(*task); err != nil {
					h.logger.Warn("[contribute-ws] failed to send task_assign", "error", err)
					return
				}
				pickupKey := task.TaskKey
				if pickupKey == "" {
					pickupKey = worksource.Ref{Repo: task.Repo, Number: task.Number}.Key()
				}
				taskDesc := assignDesc(task.Kind, pickupKey, task.Title, task.TaskID)
				if task.Role != "" {
					taskDesc = fmt.Sprintf("contributor ran %s task: %s", task.Role, taskDesc)
				}
				h.addActivity(contributor.profile.GitHubUsername, "picked up", contributor.role, contributor.cliBackend, contributor.model, contributor.reasoningEffort, taskDesc)
				h.logger.Info("[contribute-ws] task assigned",
					"username", contributor.profile.GitHubUsername,
					"task", task.TaskID,
					"repo", task.Repo,
					"number", task.Number,
				)
				// #2537: the credential was withheld from the task_assign above and is
				// delivered only AFTER acceptance. In the DEFAULT trusted-source
				// auto-accept mode, the task already cleared admission and the per-tier
				// trust gate in selectTask, so acceptance is automatic HERE — after the
				// assignment is committed and sent — and the scoped credential is
				// delivered immediately. This preserves an unattended fleet's timing
				// (credential arrives right after task_assign) while making the ordering
				// provable: the credential leaves the hub only once acceptance is
				// recorded, never bundled with the metadata. In EXPLICIT-accept mode the
				// hub withholds here and waits for a task_accepted (handled below).
				if !h.requireExplicitAccept() {
					h.deliverTaskCredential(contributor, "auto_accept")
				} else {
					h.logger.Info("[contribute-ws] credential withheld pending explicit acceptance",
						"username", contributor.profile.GitHubUsername, "task", task.TaskID)
				}
			}

		case "task_accepted":
			// #2537: a task_accepted is the client's explicit acceptance of the
			// assigned task. In EXPLICIT-accept mode the hub withheld the scoped
			// credential from task_assign and waits for exactly this message before
			// delivering it — so a task that is never accepted (declined, timed out,
			// or reconnected away) never receives a credential. acceptTaskCredential
			// delivers only when the acceptance is for the task this connection
			// currently holds; a stale/mismatched task_id is ignored. It is idempotent
			// via deliverTaskCredential, so in auto-accept mode (where the credential
			// already went out) this is a no-op, and a relay that re-asserts
			// task_accepted on reconnect cannot re-deliver.
			if contributor != nil {
				h.acceptTaskCredential(contributor, msg.TaskID)
			}

		case "task_progress":
			if contributor != nil {
				contributor.mu.Lock()
				// C4 (CWE-862/639): a task_progress that arrives while this connection
				// holds NO task is a RESUME claim. The hub must NOT rebuild ownership
				// from the client's own task_id/repo/number fields — doing so let a
				// client assert ANY task and be minted a scoped GitHub credential for
				// work the server never assigned. A resume is honored ONLY when it
				// matches a server-issued lease (lookupLease) EXACTLY on
				// {identity, task_id, repo, number, generation} and is unexpired, and
				// only after the same admission gates a fresh assignment must pass
				// (suspension, disabled tier, revocation) still hold. Anything else is
				// rejected: the relay is told to re-`ready` for fresh work.
				if contributor.currentTask == nil {
					if msg.TaskID == "" {
						contributor.mu.Unlock()
						continue
					}
					identity := identityOf(contributor)
					canonRepo := h.canonicalRepoKey(msg.Repo)
					contributor.mu.Unlock()

					lease := h.lookupLease(identity, msg.TaskID, canonRepo, msg.Number, msg.TaskGen, time.Now())
					if lease == nil {
						h.logger.Warn("[contribute-ws] task_progress resume rejected: no matching server-issued lease",
							"username", contributor.profile.GitHubUsername,
							"task", msg.TaskID,
							"repo", canonRepo,
							"client_gen", msg.TaskGen,
						)
						// Tell the relay this task is not (or no longer) its to hold, so
						// it stops reporting and re-asks for work rather than silently
						// believing it owns something the hub has no record of.
						_ = sendJSON(conn, WSMessage{Type: "task_revoke", Seq: h.nextSeq(), TaskID: msg.TaskID, Reason: "no active lease for this task"})
						continue
					}
					// C4: re-run the same admission gates a fresh selectTask assignment
					// must pass. A task assigned before the operator suspended the queue,
					// disabled the tier, or revoked the contributor must NOT silently
					// resume (and re-mint a credential) after the gate closed.
					if reason := h.resumeGateReason(contributor); reason != "" {
						h.logger.Warn("[contribute-ws] task_progress resume refused by admission gate",
							"username", contributor.profile.GitHubUsername,
							"task", msg.TaskID,
							"reason", reason,
						)
						h.revokeLease(identity, msg.TaskID)
						_ = sendJSON(conn, WSMessage{Type: "task_revoke", Seq: h.nextSeq(), TaskID: msg.TaskID, Reason: reason})
						continue
					}
					// Adopt the task from the AUTHORITATIVE lease record, not the client
					// fields: repo/number/tier are the server's, and the task keeps its
					// ORIGINAL generation so it stays fenced against any older-generation
					// straggler. lastLeaseRenew starts the wedged-task clock.
					contributor.mu.Lock()
					contributor.currentTask = &WSTaskAssign{
						TaskID: lease.taskID,
						Kind:   msg.Kind,
						Repo:   lease.repo,
						Number: lease.number,
						Title:  msg.Title,
					}
					contributor.currentTaskGen = lease.gen
					contributor.lastLeaseRenew = time.Now()
					contributor.tmuxOutput = msg.TmuxOutput
					contributor.mu.Unlock()

					// #4260: a resume is itself proof of life, so restart the lease
					// window alongside lastLeaseRenew. Without this a relay that
					// reconnected twice inside one lease window would be refused the
					// second time even though it never stopped working.
					h.renewLease(identity, lease.taskID, time.Now())

					h.logger.Info("[contribute-ws] task resumed from server-issued lease",
						"username", contributor.profile.GitHubUsername,
						"task", lease.taskID, "repo", lease.repo, "number", lease.number)

					// #2610 finding 3: re-mint and push a fresh token_refresh so the
					// resumed session holds a valid token and re-arms the #2393 refresh
					// cycle. The credential is minted for the LEASE's tier (server-owned),
					// repository-scoped to the lease's repo (C4).
					h.resumeTaskToken(contributor, lease)
					continue
				}

				// A routine progress ping for a task the hub already tracks on THIS
				// connection. #2568 (the Gate): reject a STALE generation — a worker
				// whose task was revoked/reassigned (currentTaskGen bumped past what it
				// echoes) must not renew a lease it no longer owns. An unversioned relay
				// echoes 0 and is accepted (generationAccepted falls back to TaskID).
				if !generationAccepted(msg.TaskGen, contributor.currentTaskGen) {
					staleGen := msg.TaskGen
					contributor.mu.Unlock()
					h.logger.Warn("[contribute-ws] stale-generation task_progress rejected",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"client_gen", staleGen,
					)
					continue
				}
				contributor.tmuxOutput = msg.TmuxOutput
				// #4117: the relay re-detects the running model from the CLI's own
				// session transcript on every progress tick and piggybacks it here, so
				// a mid-session model switch (claude `/model`) is reflected instead of
				// staying stuck at the connect-time value. Same pattern as the
				// auth-time handler: only non-empty values overwrite — an older relay
				// omits both fields and nothing changes. Advisory display metadata,
				// exactly like the auth_response values it refreshes.
				if msg.Model != "" {
					contributor.model = msg.Model
					if contributor.profile != nil {
						contributor.profile.Model = msg.Model
					}
				}
				if msg.ReasoningEffort != "" {
					contributor.reasoningEffort = msg.ReasoningEffort
					if contributor.profile != nil {
						contributor.profile.ReasoningEffort = msg.ReasoningEffort
					}
				}
				// SECURITY (v4, kept over v2 #3153): v4 deliberately has NO
				// client-driven resume path here. A task_progress for a task the hub
				// does not already track is resumed ONLY through the authoritative
				// server lease (lookupLease) above; a relay may not rebuild currentTask
				// from its own self-reported msg.Repo/Number/Role and thereby self-mint
				// a scoped credential (C4). v2's client-asserted resume block was NOT
				// grafted — see the PR body "Consider porting to v4 separately".
				// #2568: renew the hub-owned lease on every progress report. This is
				// what distinguishes "working slowly but alive" (lease keeps renewing,
				// never reclaimed) from "connected but wedged" (lease goes stale and
				// cleanupLoop reclaims it after wsTaskTimeout).
				contributor.lastLeaseRenew = time.Now()
				// #4260: the task id is taken from the hub's OWN record of what this
				// connection holds, never from msg.TaskID, so the renewal cannot be
				// pointed at a task the client merely names. Captured here under the
				// connection lock and applied below without it, matching how the other
				// release paths call into the lease registry.
				heldTaskID := ""
				if contributor.currentTask != nil {
					heldTaskID = contributor.currentTask.TaskID
				}
				contributor.mu.Unlock()
				// #4260: keep the lease registry's expiry on the same clock as
				// lastLeaseRenew above. Stamping it only at assignment meant a task
				// still healthily reporting progress past leaseTTL was never reclaimed
				// yet could no longer be re-adopted, so the next socket drop cost the
				// relay its in-flight work.
				h.renewLease(identityOf(contributor), heldTaskID, time.Now())
			}

		case "task_complete":
			if contributor != nil {
				contributor.mu.Lock()
				// #2568 (the Gate, critical guarantee): a worker whose task was revoked
				// and reassigned — but that later wakes and reports completion carrying
				// the OLD generation — must NOT overwrite the new owner's state. Its
				// currentTaskGen was bumped past what it echoes, so reject the message
				// WITHOUT clearing currentTask (which may now hold the NEW owner's task).
				// An unversioned relay echoes 0 and is accepted, falling back to the
				// TaskID identity match below. Checked before any mutation.
				if contributor.currentTask != nil && !generationAccepted(msg.TaskGen, contributor.currentTaskGen) {
					staleGen := msg.TaskGen
					contributor.mu.Unlock()
					h.logger.Warn("[contribute-ws] stale-generation task_complete rejected",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"client_gen", staleGen,
					)
					continue
				}
				hasTask := contributor.currentTask != nil && contributor.currentTask.TaskID == msg.TaskID
				completedTask := contributor.currentTask
				// SECURITY (audit N9, CWE-862/639): clear ONLY when the reported
				// task_id actually matches the held assignment.
				//
				// This block used to run unconditionally, while revokeLease and
				// markTaskCompleted below run only `if hasTask`. So a completion
				// naming ANY other task released the assignment without revoking the
				// lease or booking a cooldown: the contributor went `ready` and was
				// minted a SECOND live repo credential under max_concurrent=1, and
				// the "unassigned task ignored" warning below said otherwise while
				// the state had in fact already been mutated.
				//
				// The #2568 Gate does not cover this: generationAccepted() returns
				// true whenever clientGen == 0, and omitting task_gen yields 0, so
				// the guard is opt-in from the client.
				if hasTask {
					contributor.currentTask = nil
					// #2539: drop the previewable prompt with the task it belonged to
					// so the ops tab does not show a stale instruction after completion.
					contributor.currentPrompt = ""
					contributor.currentLabels = nil
					contributor.tokenMintedAt = time.Time{}
					// #2537: clear any pending/delivered credential state with the task.
					contributor.pendingToken = ""
					contributor.credentialDelivered = false
				}
				// tmuxOutput is diagnostic only and carries no authority, so it is
				// recorded either way — it is often the only evidence of what a
				// confused relay was doing when it reported the wrong task.
				contributor.tmuxOutput = msg.TmuxOutput
				contributor.mu.Unlock()

				if hasTask {
					// #2565: the reported PR URL is client-supplied (tmux-scraped by
					// the relay), so before it drives the LONG cooldown OR trust credit
					// we verify it server-side against GitHub — it must exist, have a
					// base repo matching THIS assignment, and be authored by this
					// contributor. Anything else (no URL, wrong repo/author, API error)
					// downgrades to an unverified/no-PR completion: the short cooldown
					// and no trust credit. This closes the hole #2437 left open (the
					// bar was "a PR was reported", still trusting an unverified field).
					// verifiedPR is the ONLY value allowed to unlock the PR-gated
					// rewards below; the raw msg.PRURL is never trusted directly again.
					// C4: a completion is terminal — revoke the server-issued lease so a
					// later task_progress for this task cannot resurrect ownership and be
					// re-minted a credential.
					if completedTask != nil {
						h.revokeLease(identityOf(contributor), completedTask.TaskID)
					}
					verifiedPR := ""
					var prDetail ghpkg.PRVerification
					if completedTask != nil {
						prDetail = h.verifyReportedPRDetail(completedTask.Repo, msg.PRURL, contributor.profile.GitHubUsername)
					}
					if prDetail.Verified {
						verifiedPR = msg.PRURL
						// Off the read loop, deliberately. This is cosmetic
						// best-effort work that gates NOTHING — unlike
						// verifyReportedPR above, whose result decides the cooldown
						// and trust credit and so must be awaited. Inline it would
						// add up to two GitHub round trips (bounded by the App
						// client's 30s timeout, so ~60s worst case) to this
						// contributor's message loop, during which its pongs are not
						// read; wsHeartbeatTimeout is 90s, so a slow GitHub could
						// push a perfectly healthy contributor to `Stale` in the
						// fleet view for the sake of a PR-body edit.
						go h.reconcilePRAttribution(msg.PRURL, contributor)
						// A MERGED fix retires the finding it addresses, so the
						// digest stops carrying work that is already done. Gated
						// on prDetail.Merged, not on verification alone: a PR
						// that merely exists is a fix in review, and closing
						// findings on it would retire them before anything
						// landed. The PR's OWN title is matched — the
						// assignment's issue title would match the finding it
						// was minted from on the mere existence of a PR.
						if prDetail.Merged {
							h.closeAdvisoryForMergedPR(prDetail.Title)
						}
					}
					// #3987: normalize the completion's verdict. A verified PR always
					// wins (shipped); with none, only an explicit no_work_needed is
					// honoured and everything else — including the absent field every
					// pre-#3987 relay sends — is idle, i.e. today's exact semantics.
					verdict := normalizeCompletionVerdict(msg.Verdict, verifiedPR)
					if completedTask != nil {
						// #2393 item 7 + #2565: the full week-long cooldown is applied
						// only for a VERIFIED PR; an unverified or no-PR completion gets
						// the short cooldown so the issue is not locked for a week (and,
						// per #2492/#2557, still gets a non-zero cooldown so it is not
						// instantly re-offered in a tight loop). #3987: a no_work_needed
						// verdict additionally books the durable offer-suppression
						// verdict (see markTaskCompletedVerdict).
						h.markTaskCompletedVerdictKey(completedTask.identityKey(), verifiedPR,
							verdict, contributor.profile.GitHubUsername, strings.TrimSpace(msg.VerdictReason))
					}
					completedDesc := msg.TaskID
					if completedTask != nil {
						completedDesc = assignDesc(completedTask.Kind, completedTask.identityKey(), completedTask.Title, msg.TaskID)
					}
					provider := ""
					if contributor.cliBackend == "pi" {
						provider, _, _ = strings.Cut(contributor.model, "/")
					}
					h.addActivity(contributor.profile.GitHubUsername, "completed", contributor.role, contributor.cliBackend, contributor.model, contributor.reasoningEffort, completedDesc)
					h.logger.Info("[contribute-ws] task complete",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"task_gen", msg.TaskGen,
						"backend", contributor.cliBackend,
						// Provider is derived from Pi's one canonical provider/model input;
						// this evidence is not a second authority or routing signal.
						"provider", provider,
						"model", contributor.model,
						"result", msg.Result,
						"pr_verified", verifiedPR != "",
						"verdict", verdict,
						"verdict_reason", strings.TrimSpace(msg.VerdictReason),
					)
					contributor.mu.Lock()
					contributor.profile.TasksCompleted++
					// Trust credit is gated on the VERIFIED PR, not the reported one:
					// counting the raw self-reported field would hand out
					// contents:write / pulls:write for a PR that was never shown to
					// exist, belongs to another repo, or was authored by someone else.
					if verifiedPR != "" {
						contributor.profile.TasksWithPR++
					}
					contributor.profile.LastActive = time.Now().UTC().Format(time.RFC3339)
					if completedTask != nil {
						contributor.profile.LastCompletedTask = completedTask
					}
					// Promote on completions that produced a VERIFIED pull request.
					// Completion is self-reported, so counting bare task_complete
					// messages — or unverified PR URLs — would hand out contents:write
					// and pulls:write for work that was never shown to exist.
					promoted := false
					if contributor.profile.TrustTier == "newcomer" && contributor.profile.TasksWithPR >= contributorAutoPromoteAt {
						contributor.profile.TrustTier = "contributor"
						promoted = true
						h.logger.Info("[contribute-ws] auto-promoted", "username", contributor.profile.GitHubUsername)
					}
					promotedUser := contributor.profile.GitHubUsername
					promotedCLI := contributor.cliBackend
					promotedModel := contributor.model
					promotedEffort := contributor.reasoningEffort
					contributor.mu.Unlock()
					// #2390-era command center: narrate the promotion as its own
					// activity event so the Operations dev-log and achievement pops
					// (contribute_sse.go broadcast) surface "promoted to contributor".
					// Read-only signalling — it changes no control behaviour and is
					// emitted only on the real newcomer -> contributor transition.
					if promoted {
						h.addActivity(promotedUser, "promoted", "contributor", promotedCLI, promotedModel, promotedEffort, "contributor")
					}
					_ = saveContributorProfile(contributor.profile)
				} else {
					// N9: this is now literally true. It previously logged "ignored"
					// after the handler had already cleared currentTask and the
					// credential state, which made the bypass look like a no-op in the
					// logs.
					h.logger.Warn("[contribute-ws] task_complete for unassigned task ignored (assignment left intact)",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
					)
				}
			}

		case "task_failed":
			if contributor != nil {
				contributor.mu.Lock()
				// #2568 (the Gate): reject a STALE worker's failure the same way as its
				// completion — its currentTaskGen was bumped past the generation it
				// echoes. Left unguarded, a revoked worker's late task_failed would book
				// a spurious failure cooldown against the NEW owner's issue. Do not clear
				// currentTask (it may now be the new owner's task). Unversioned relays
				// echo 0 and fall back to the TaskID match below.
				if contributor.currentTask != nil && !generationAccepted(msg.TaskGen, contributor.currentTaskGen) {
					staleGen := msg.TaskGen
					contributor.mu.Unlock()
					h.logger.Warn("[contribute-ws] stale-generation task_failed rejected",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"client_gen", staleGen,
					)
					continue
				}
				hasTask := contributor.currentTask != nil && contributor.currentTask.TaskID == msg.TaskID
				failedTask := contributor.currentTask
				// SECURITY (audit N9, CWE-862/639): same hole as task_complete —
				// clear only on a genuine TaskID match. Unconditionally, a failure
				// naming any other task released the assignment while revokeLease
				// and recordTaskFailure below stayed inside `if hasTask`, so no
				// cooldown was booked and the abandoned issue became instantly
				// re-admissible while the credential stayed live.
				if hasTask {
					contributor.currentTask = nil
					contributor.tokenMintedAt = time.Time{}
					// #2537: clear any pending/delivered credential state with the task.
					contributor.pendingToken = ""
					contributor.credentialDelivered = false
				}
				contributor.mu.Unlock()

				if hasTask {
					// C4: a failure is terminal — revoke the server-issued lease so a
					// later task_progress for this task cannot resurrect ownership.
					if failedTask != nil {
						h.revokeLease(identityOf(contributor), failedTask.TaskID)
					}
					if failedTask != nil {
						// #2435: record a short failure cooldown (and advance the
						// consecutive-failure/quarantine counter) so a just-failed
						// issue is not immediately re-admissible and handed straight
						// back out ahead of the rest of the queue. A permanent failure
						// counts more toward the quarantine threshold.
						h.recordTaskFailureForTask(failedTask, msg.Permanent)
					}
					// #2547: keep the failure reason instead of only logging it, so an
					// operator can tell a work item that failed on its merits from one
					// that landed on a client whose environment could not run it. The
					// kind is self-reported and advisory — recorded and displayed, never
					// acted on. Note this is stored AFTER recordTaskFailure above and
					// deliberately does not influence it: the cooldown must not depend
					// on a client-controlled value (that is ROUTE, still undecided).
					failureKind := NormalizeTaskFailureKind(msg.FailureKind)
					contributor.mu.Lock()
					contributor.lastFailure = &ContributorFailure{
						TaskID:    msg.TaskID,
						Kind:      failureKind,
						Reason:    msg.Reason,
						Permanent: msg.Permanent,
						At:        time.Now().UTC().Format(time.RFC3339),
					}
					if failedTask != nil {
						contributor.lastFailure.Repo = failedTask.Repo
						contributor.lastFailure.Number = failedTask.Number
					}
					contributor.mu.Unlock()

					failedDesc := msg.TaskID
					if failedTask != nil {
						failedDesc = assignDesc(failedTask.Kind, failedTask.identityKey(), failedTask.Title, msg.TaskID)
					}
					provider := ""
					if contributor.cliBackend == "pi" {
						provider, _, _ = strings.Cut(contributor.model, "/")
					}
					h.addActivity(contributor.profile.GitHubUsername, "failed", contributor.role, contributor.cliBackend, contributor.model, contributor.reasoningEffort, failedDesc)
					h.logger.Info("[contribute-ws] task failed",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"task_gen", msg.TaskGen,
						"backend", contributor.cliBackend,
						"provider", provider,
						"model", contributor.model,
						"result", "failed",
						"reason", msg.Reason,
						"failure_kind", failureKind,
						"permanent", msg.Permanent,
					)
					contributor.mu.Lock()
					contributor.profile.TasksFailed++
					contributor.mu.Unlock()
					_ = saveContributorProfile(contributor.profile)
				} else {
					h.logger.Warn("[contribute-ws] task_failed for unassigned task ignored",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
					)
				}
			}

		case "pong":
			if contributor != nil {
				contributor.mu.Lock()
				contributor.lastPong = time.Now()
				contributor.mu.Unlock()
			}

		case "ping":
			// Once registered, this reply shares the connection with the heartbeat
			// loop and the operator paths, so it MUST take the write lock; a ping that
			// somehow arrives pre-registration has no ContributorConnection and is
			// still single-writer on the bare conn.
			if contributor != nil {
				_ = contributor.send(WSMessage{Type: "pong", Seq: msg.Seq})
			} else {
				_ = sendJSON(conn, WSMessage{Type: "pong", Seq: msg.Seq})
			}
		}
	}
}

func (h *ContributeWSHub) heartbeatLoop(c *ContributorConnection) {
	ticker := time.NewTicker(wsHeartbeatInterval)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()
		lastPong := c.lastPong
		c.mu.Unlock()

		if time.Since(lastPong) > wsHeartbeatTimeout {
			h.logger.Info("[contribute-ws] heartbeat timeout", "username", c.profile.GitHubUsername)
			c.ws.Close()
			return
		}

		h.maybeRefreshToken(c)

		if err := c.send(WSMessage{Type: "ping", Seq: h.nextSeq()}); err != nil {
			h.logger.Info("[contribute-ws] heartbeat ping failed, closing", "username", c.profile.GitHubUsername)
			c.ws.Close()
			return
		}
	}
}

// maybeRefreshToken re-mints a scoped GitHub token and pushes a token_refresh to
// the relay once wsTokenRefreshPeriod has elapsed since the current task's token
// was minted, provided a task is still active. This keeps long, human-steered
// sessions from silently losing push access when the original token expires at
// wsTokenTTL. The relay's token_refresh handler consumes github_token +
// token_expires_at (bin/contributor-relay.sh). See #2393 item 2.
func (h *ContributeWSHub) maybeRefreshToken(c *ContributorConnection) {
	tier, repo, due := tokenRefreshDue(c, time.Now())
	if !due {
		return
	}

	tok, err := h.mintScopedToken(tier, repo)
	if err != nil {
		h.logger.Warn("[contribute-ws] token refresh: mint failed, will retry next heartbeat",
			"username", c.profile.GitHubUsername, "tier", tier, "error", err)
		return
	}
	if tok == "" {
		// No new token available (no App auth / no cache): leave the relay's
		// existing token in place and try again next heartbeat.
		return
	}

	if err := h.sendTokenRefresh(c, tok); err != nil {
		h.logger.Info("[contribute-ws] token refresh: send failed", "username", c.profile.GitHubUsername, "error", err)
		return
	}

	h.logger.Info("[contribute-ws] token refreshed for active task",
		"username", c.profile.GitHubUsername, "tier", tier)
}

// taskHeldByAnotherConnection (v2 #3153) reports whether some OTHER live
// connection already owns the given repo#number task. It is a pure, read-only
// query. v4's task_progress handler resumes ONLY via the authoritative server
// lease (lookupLease) and does NOT call this from a client-driven resume path —
// that self-report resume block was intentionally not grafted in the v2→v4 sync
// (see the handler comment and PR "Consider porting to v4 separately"). The helper
// is retained because it is harmless and is exercised by the merged dupe-assign
// tests, and to make a future deliberate port of v2's guard a one-line hookup.
func (h *ContributeWSHub) taskHeldByAnotherConnection(candidate *ContributorConnection, repo string, number int) bool {
	if h == nil || number <= 0 {
		return false
	}
	canonicalRepo := h.canonicalRepoKey(repo)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.connections {
		if conn == nil || conn == candidate {
			continue
		}
		conn.mu.Lock()
		held := conn.currentTask != nil &&
			conn.currentTask.Number == number &&
			h.canonicalRepoKey(conn.currentTask.Repo) == canonicalRepo
		conn.mu.Unlock()
		if held {
			return true
		}
	}
	return false
}

// resumeTaskToken re-mints a scoped GitHub token for a task that has just been
// re-asserted over a reconnect (task_progress rebuilt currentTask) and pushes it
// to the relay, which re-arms the heartbeat refresh cycle: sendTokenRefresh
// records tokenMintedAt on success, so the subsequent maybeRefreshToken calls see
// a non-zero mint time and fire again. Without this the resumed session's
// tokenMintedAt stays zero (cleared by the disconnect defer) and refresh never
// fires again for the life of the connection (#2610 finding 3). A mint failure or
// an empty token (no App auth / no cache) leaves the relay's existing token in
// place — the same lenient policy maybeRefreshToken uses — and tokenMintedAt stays
// zero, so the next reconnect (or a later mint success) can still arm it.
func (h *ContributeWSHub) resumeTaskToken(c *ContributorConnection, lease *taskLease) {
	// C4: mint for the SERVER-issued lease's tier and repository, not the client's
	// self-report, so a resumed session's credential is scoped to exactly the task the
	// hub assigned.
	tier := ""
	repo := ""
	if lease != nil {
		tier = lease.tier
		repo = lease.repo
	} else if c.profile != nil {
		tier = c.profile.TrustTier
	}
	tok, err := h.mintScopedToken(tier, repo)
	if err != nil {
		h.logger.Warn("[contribute-ws] resume token refresh: mint failed, refresh will re-arm on next resume/heartbeat",
			"username", c.profile.GitHubUsername, "tier", tier, "error", err)
		return
	}
	if tok == "" {
		// No new token available: leave the relay's existing token in place.
		return
	}
	if err := h.sendTokenRefresh(c, tok); err != nil {
		h.logger.Info("[contribute-ws] resume token refresh: send failed", "username", c.profile.GitHubUsername, "error", err)
		return
	}
	h.logger.Info("[contribute-ws] token refreshed on task resume (re-armed refresh cycle)",
		"username", c.profile.GitHubUsername, "tier", tier)
}

// tokenRefreshDue reports whether the connection has an active task whose scoped
// token was minted at least wsTokenRefreshPeriod ago, meaning it is time to
// re-mint before wsTokenTTL. It returns the trust tier and the current task's repo
// to mint a repository-scoped token for (C4). Pure and clock-injectable so the
// timing can be tested without a real clock.
func tokenRefreshDue(c *ContributorConnection, now time.Time) (tier, repo string, due bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask == nil || c.tokenMintedAt.IsZero() {
		return "", "", false
	}
	if now.Sub(c.tokenMintedAt) < wsTokenRefreshPeriod {
		return "", "", false
	}
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	repo = c.currentTask.Repo
	return tier, repo, true
}

// sendTokenRefresh writes a token_refresh message carrying the new token and its
// expiry, then records the new mint time. The field names (github_token,
// token_expires_at) match exactly what the relay's token_refresh handler
// consumes in bin/contributor-relay.sh. See #2393 item 2.
func (h *ContributeWSHub) sendTokenRefresh(c *ContributorConnection, tok string) error {
	msg := WSMessage{
		Type:           "token_refresh",
		Seq:            h.nextSeq(),
		GitHubToken:    tok,
		TokenExpiresAt: time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339),
	}
	if err := c.send(msg); err != nil {
		return err
	}
	c.mu.Lock()
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// requireExplicitAccept reports whether this hive is in the opt-in EXPLICIT
// (human/manual) acceptance mode (#2537). A hub without a Config (direct-in-test
// construction) or an unset toggle resolves to FALSE — the trusted-source
// auto-accept default — so existing deployments keep delivering credentials to
// admitted tasks without a wait state.
func (h *ContributeWSHub) requireExplicitAccept() bool {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return false
	}
	return h.server.deps.Config.Hub.IsContributeRequireExplicitAccept()
}

// deliverTaskCredential ships the scoped credential the hub minted for the
// connection's current task but deliberately withheld from task_assign (#2537).
// It is the single post-acceptance delivery point: both the auto-accept path (in
// the ready handler, right after task_assign is sent) and the explicit-accept path
// (the task_accepted handler) funnel through here, so the credential provably
// leaves the hub only AFTER an acceptance decision was recorded — never bundled
// with the task metadata.
//
// It reuses the token_refresh wire shape (github_token + token_expires_at) that
// every existing relay already understands, so an old client that never learned a
// new "credential" message still ends up holding a working token: it processes
// task_assign (metadata) then a token_refresh (credential) exactly as it already
// handles a mid-task re-mint. The token itself is unchanged — the same per-tier
// scoped, wsTokenTTL-expiring token selectTask minted — only its timing moved.
//
// It is idempotent and safe to call more than once: it delivers only while a
// pending token is held and not yet delivered, then sets credentialDelivered and
// clears pendingToken. A task_accepted that arrives in auto-accept mode after the
// credential already went out is therefore a no-op, and a duplicate acceptance
// cannot double-send. It NEVER logs the token value. Returns true when it actually
// delivered (an assignment→acceptance→credential ordering point for tests/audit).
func (h *ContributeWSHub) deliverTaskCredential(c *ContributorConnection, reason string) bool {
	c.mu.Lock()
	if c.currentTask == nil || c.credentialDelivered || c.pendingToken == "" {
		c.mu.Unlock()
		return false
	}
	tok := c.pendingToken
	taskID := c.currentTask.TaskID
	username := ""
	if c.profile != nil {
		username = c.profile.GitHubUsername
	}
	c.mu.Unlock()

	// sendTokenRefresh writes the github_token + token_expires_at frame and
	// re-stamps tokenMintedAt on success, anchoring the #2393 refresh cycle on when
	// the relay actually received the credential.
	if err := h.sendTokenRefresh(c, tok); err != nil {
		// Delivery failed (socket gone): leave the token PENDING and undelivered so
		// a reconnect/resume or a retried acceptance can deliver it. Never log the
		// token value.
		h.logger.Info("[contribute-ws] task credential delivery failed, will retry",
			"username", username, "task", taskID, "accept_reason", reason, "error", err)
		return false
	}

	c.mu.Lock()
	c.credentialDelivered = true
	c.pendingToken = ""
	c.mu.Unlock()

	h.logger.Info("[contribute-ws] task credential delivered after acceptance",
		"username", username, "task", taskID, "accept_reason", reason)
	return true
}

// acceptTaskCredential is the explicit-acceptance entry point (#2537): a client
// task_accepted for taskID accepts the assigned task, releasing the credential the
// hub withheld from task_assign. It delivers only when taskID matches the task this
// connection currently holds — a stale or mismatched task_id (e.g. a late
// task_accepted for a task that already ended) is ignored, so a credential is never
// delivered for work this connection is not actually on. It returns whether a
// credential was delivered (an assignment→acceptance→credential ordering point).
func (h *ContributeWSHub) acceptTaskCredential(c *ContributorConnection, taskID string) bool {
	if c == nil || taskID == "" {
		return false
	}
	c.mu.Lock()
	match := c.currentTask != nil && c.currentTask.TaskID == taskID
	c.mu.Unlock()
	if !match {
		return false
	}
	return h.deliverTaskCredential(c, "explicit_accept")
}

func (h *ContributeWSHub) checkModelAllowed(model string) (bool, []string) {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return true, nil
	}
	cfg := h.server.deps.Config.Hub
	if len(cfg.ContributeAllowModels) == 0 {
		return true, nil
	}
	if model == "" {
		return !cfg.ContributeRejectUnknownModels, cfg.ContributeAllowModels
	}
	if config.MatchesAny(model, cfg.ContributeAllowModels) {
		return true, nil
	}
	if cfg.ContributeRejectUnknownModels {
		return false, cfg.ContributeAllowModels
	}
	return true, nil
}

func (h *ContributeWSHub) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		// #2568: reclaim wedged-but-connected task leases first (the backstop). A
		// connection whose lastPong is still fresh (so the heartbeat sweep below will
		// NOT remove it) but that has stopped renewing its task lease is exactly the
		// "connected but wedged" case the issue describes; this releases its task
		// through the SAME cooldown+generation-bump path a manual requeue uses.
		h.reclaimExpiredLeases(time.Now())

		h.mu.Lock()
		for id, c := range h.connections {
			c.mu.Lock()
			stale := time.Since(c.lastPong) > wsHeartbeatTimeout
			username := ""
			if c.profile != nil {
				username = c.profile.GitHubUsername
			}
			c.mu.Unlock()
			if stale {
				h.logger.Info("[contribute-ws] cleanup: removing stale connection", "username", username, "conn", id)
				// Nil-guard the close: a connection may carry no live socket (e.g. a
				// test-injected in-flight entry, or a connection torn down elsewhere),
				// and cleanupLoop iterates ALL registered connections. Mirrors the
				// existing guard in RequeueContributorTask so a ws-less entry is pruned
				// rather than nil-dereferenced.
				if c.ws != nil {
					c.ws.Close()
				}
				delete(h.connections, id)
			}
		}
		h.mu.Unlock()
	}
}

// reclaimExpiredLeases is the hub-owned LEASE-TTL backstop (kubestellar/hive#2568,
// option 4). A connection that is still HELD by its socket (heartbeat alive) but has
// not renewed its task lease within wsTaskTimeout is presumed wedged — connected but
// no longer progressing — and its task is auto-released. It reuses EXACTLY the manual
// requeue machinery: it books the same short failure cooldown (so the released issue
// is not instantly re-admissible and can't recreate the #2492 dup-assign race), bumps
// the assignment generation (the Gate — so the wedged worker, if it later wakes, is
// fenced), and pushes task_revoke with an auto-expiry reason so a still-listening
// relay stops cleanly. It is deliberately CONSERVATIVE: a task that keeps reporting
// task_progress renews lastLeaseRenew every report and is therefore NEVER reclaimed,
// so "working slowly but alive" is not confused with "wedged". `now` is injected so
// tests can drive expiry deterministically.
func (h *ContributeWSHub) reclaimExpiredLeases(now time.Time) int {
	type expiredTarget struct {
		conn *ContributorConnection
		task WSTaskAssign
	}
	var targets []expiredTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		// Only a connection actively holding a task with a started lease clock can
		// expire; a zero lastLeaseRenew means no active lease (idle or just released).
		expired := c.currentTask != nil && !c.lastLeaseRenew.IsZero() &&
			now.Sub(c.lastLeaseRenew) > wsTaskTimeout
		if expired {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			// #2675: clear credential state so a stale pendingToken cannot leak to the
			// now-idle connection (mirrors RequeueContributorTask cleanup).
			c.pendingToken = ""
			c.credentialDelivered = false
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, expiredTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()

	for _, tgt := range targets {
		// C4: the lease-TTL backstop released this task — revoke its server-issued
		// lease so the wedged worker cannot re-adopt it via a later task_progress.
		h.revokeLease(identityOf(tgt.conn), tgt.task.TaskID)
		if tgt.task.Number > 0 {
			h.recordTaskFailureForTask(&tgt.task, false)
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		h.logger.Warn("[contribute-ws] task lease expired, auto-released",
			"username", username,
			"task", tgt.task.TaskID,
			"repo", tgt.task.Repo,
			"number", tgt.task.Number,
			"lease_ttl", wsTaskTimeout.String(),
		)
		h.addActivity(username, "lease expired: auto-released", tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.conn.reasoningEffort, tgt.task.TaskID)
		if tgt.conn.ws != nil {
			_ = tgt.conn.send(WSMessage{
				Type:   "task_revoke",
				Seq:    h.nextSeq(),
				TaskID: tgt.task.TaskID,
				Reason: leaseExpiredReason,
			})
		}
	}
	return len(targets)
}

// mintScopedToken produces a scoped GitHub token for the given trust tier via the
// GitHub App auth path, scoped to a single repository when repo is non-empty
// (kubestellar/hive C4). This is the single mint path shared by task_assign and the
// heartbeat/resume token-refresh, so all three advertise tokens minted the same way.
// See #2393 item 2.
//
// C4 (CWE-862/639): the previous full-cache FALLBACK — when no App auth was
// configured, returning the hive's own FULL cached installation token — has been
// DELETED. Handing a contributor relay the hive's installation-wide credential gave
// it push access to every repo the installation covers, wildly beyond the single
// issue the contributor was assigned, and it was reachable from the forgeable resume
// path. When there is no App auth to mint a properly scoped token, mint NOTHING: the
// caller treats an empty token as "leave the relay's current token in place", and
// selectTask surfaces token_mint_failed rather than leaking the full credential.
func (h *ContributeWSHub) mintScopedToken(tier, repo string) (string, error) {
	if h.server != nil && h.server.deps != nil && h.server.deps.GHAppAuth != nil {
		ctx := h.server.deps.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		// C4: restrict the installation token to the assignment's repository so the
		// credential cannot touch any other repo the installation covers. repoNameOnly
		// yields the bare repo name the GitHub API's Repositories option expects; an
		// empty repo (synthetic/pr-review tasks with no single repo) falls back to the
		// tier-scoped installation token — still permission-scoped, unchanged behavior.
		if name := repoNameOnly(repo); name != "" {
			return h.server.deps.GHAppAuth.ScopedTokenForRepos(ctx, tier, []string{name})
		}
		return h.server.deps.GHAppAuth.ScopedToken(ctx, tier)
	}
	// No App auth: mint nothing rather than leak a full credential (see the note
	// above — the cache fallback is deliberately gone).
	return "", nil
}

// repoNameOnly returns the bare repository name from an "owner/repo" (or already
// bare) value, for the GitHub App installation-token Repositories option, which is
// keyed on the repo name within the installation's org (kubestellar/hive C4). An
// empty input yields "".
func repoNameOnly(repo string) string {
	repo = strings.TrimSpace(repo)
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		return repo[idx+1:]
	}
	return repo
}

// taskUnavailableReason* are the machine-readable reasons carried on a
// task_unavailable negative-ack. They let the relay (and operators reading the
// log) tell "there is simply no admissible work right now" apart from "the hub
// refused to assign work to this contributor" for a specific enforced reason.
// See kubestellar/hive#2436.
const (
	// taskUnavailableTokenMintFailed: a scoped GitHub token could not be minted
	// for the contributor's tier (e.g. the installation lacks the permission the
	// tier requests), so no task_assign can be honestly issued. Previously this
	// path returned nil and the contributor waited forever with no explanation.
	taskUnavailableTokenMintFailed = "token_mint_failed"
	// taskUnavailableTierDisabled: the contributor's TrustTier is listed in
	// hub.disabled_tiers, so the operator has switched that tier off.
	taskUnavailableTierDisabled = "tier_disabled"
	// taskUnavailableConcurrencyLimit: assigning would exceed the tier's
	// tier_limits.max_concurrent for this identity (counting every live
	// connection this identity holds).
	taskUnavailableConcurrencyLimit = "concurrency_limit"
	// taskUnavailableHourlyLimit / taskUnavailableDailyLimit (#2566): assigning
	// would exceed the tier's tier_limits.max_per_hour / max_per_day for this
	// identity, counting the assignments handed out inside the trailing
	// rateLimitHourWindow / rateLimitDayWindow. These mirror the enforced-refusal
	// shape of taskUnavailableConcurrencyLimit (#2436) and the tier_disabled gate:
	// the fields were admin-writable and displayed by the #2562 control-plane but
	// previously left as TODO(#2436) and never enforced, so the displayed caps were
	// inert. A contributor at or over the cap now learns exactly which window it hit.
	taskUnavailableHourlyLimit = "hourly_limit"
	taskUnavailableDailyLimit  = "daily_limit"

	// The reasons below (kubestellar/hive#2546) extend the same task_unavailable
	// negative-ack to the three formerly-SILENT selectTask paths — each returned a
	// bare nil, so an idle contributor could not tell "operator suspended us" from
	// "hub is not ready yet" from "nothing matches right now". They are additive
	// and wire-compatible: same message type/shape as #2436, only new reason
	// strings. Unlike the #2436 reasons these are not enforced refusals — they mean
	// "no work to hand you right now, and here is why".
	//
	// taskUnavailableContributionSuspended: the operator has turned the whole
	// contribute queue off (hub.contribute_suspended). No contributor gets work
	// until it is re-enabled.
	taskUnavailableContributionSuspended = "contribution_suspended"
	// taskUnavailableHubNotReady: the hub has no status snapshot yet (it has not
	// finished its first enumeration, or has no server reference), so there is no
	// candidate set to select from. Transient at startup.
	taskUnavailableHubNotReady = "hub_not_ready"
	// taskUnavailableNoMatchingWork: the hub is running and unsuspended but, after
	// all filters (cooldown, disabled repos, allow/deny, skip-assigned, own-work),
	// the candidate set is empty. There is simply nothing admissible to do now.
	taskUnavailableNoMatchingWork = "no_matching_work"
	// taskUnavailableRoleNotPermitted: the relay requested a spoke agent role, but
	// the hive config/tier/grant policy does not allow this contributor to claim it.
	taskUnavailableRoleNotPermitted = "agent_role_not_permitted"
)

// identityOf returns the stable key that groups a contributor's live
// connections for per-identity concurrency accounting (#2436 finding 3). The
// registered ContributorID is preferred; GitHubUsername is the fallback for
// connections whose profile predates or lacks an ID. Two WebSocket connections
// opened by the same registered contributor therefore share one identity.
func identityOf(c *ContributorConnection) string {
	if c == nil || c.profile == nil {
		return ""
	}
	if c.profile.ContributorID != "" {
		return c.profile.ContributorID
	}
	return c.profile.GitHubUsername
}

// rateWindowCounts returns how many task assignments the given identity has been
// handed inside the trailing hour and day windows ending at `now`. It prunes any
// timestamps older than the day window (the widest of the two) as a side effect so
// assignmentTimes stays bounded to at most a day of history per identity. Caller
// must NOT hold rateMu; this method takes it. See assignmentTimes / #2566 for the
// rolling-window semantics.
func (h *ContributeWSHub) rateWindowCounts(identity string, now time.Time) (hour, day int) {
	if identity == "" {
		return 0, 0
	}
	dayCutoff := now.Add(-rateLimitDayWindow)
	hourCutoff := now.Add(-rateLimitHourWindow)

	h.rateMu.Lock()
	defer h.rateMu.Unlock()

	times := h.assignmentTimes[identity]
	kept := times[:0]
	for _, t := range times {
		if t.Before(dayCutoff) {
			// Older than the widest window — it can never count again; drop it.
			continue
		}
		kept = append(kept, t)
		day++
		if !t.Before(hourCutoff) {
			hour++
		}
	}
	if len(kept) == 0 {
		delete(h.assignmentTimes, identity)
	} else {
		h.assignmentTimes[identity] = kept
	}
	return hour, day
}

// recordAssignment appends an assignment timestamp for the identity. Called once
// per task_assign actually shipped, so the rate windows count tasks HANDED OUT
// (matching max_concurrent's semantics and the max_tasks_per_hour/day naming), not
// completions. Caller must NOT hold rateMu. See #2566.
func (h *ContributeWSHub) recordAssignment(identity string, at time.Time) {
	if identity == "" {
		return
	}
	h.rateMu.Lock()
	if h.assignmentTimes == nil {
		// The constructor initializes this map; a hub built as a bare struct literal
		// (some tests, defensive) would otherwise panic on append to a nil map.
		h.assignmentTimes = make(map[string][]time.Time)
	}
	h.assignmentTimes[identity] = append(h.assignmentTimes[identity], at)
	h.rateMu.Unlock()
}

// taskUnavailable builds the explicit negative-ack the ready handler sends in
// place of silence. It carries a machine-readable reason so the failure is
// diagnosable rather than an indefinite hang (kubestellar/hive#2436, finding 1).
func (h *ContributeWSHub) taskUnavailable(reason string) *WSMessage {
	return &WSMessage{
		Type:   "task_unavailable",
		Seq:    h.nextSeq(),
		Reason: reason,
	}
}

// buildTaskPrompt constructs the exact assignment prompt sent to a contributor's
// agent for a given issue. It is a PURE function of the task's public metadata
// (repo / number / title) and deliberately contains NO credential: the scoped
// github_token is attached to the task_assign WSMessage separately, so this text
// is safe to preview read-only in the ops tab (#2539). selectTask ships whatever
// this returns, and the ops preview reads the very same string back off the
// connection, so "what is previewed" always matches "what runs".
// buildTaskPrompt is the GitHub-shaped entry point retained for existing call
// sites (ops-tab prompt preview, tests). New identity-aware callers use
// buildTaskPromptForRef.
func buildTaskPrompt(repoFull string, number int, title string) string {
	return buildTaskPromptForRef(worksource.Ref{Repo: repoFull, Number: number}, title)
}

// buildTaskPromptForRef renders the assignment prompt for any work source
// (kubestellar/hive#4245).
//
// The issue reference used to be formatted "%s#%d" straight from repo+number,
// so a Linear or Jira task told the agent to "Work on issue owner/repo#0" — a
// reference to nothing, with the item's real key and URL nowhere in the prompt.
// The reference is now the canonical key, and non-GitHub work additionally
// carries its URL, because that is the only way an agent can actually open it:
// there is no `gh issue view` for a Linear ticket.
//
// The repository instructions are unchanged and still name the GitHub repo —
// external work is planned elsewhere but still landed as a PR here.
func buildTaskPromptForRef(ref worksource.Ref, title string) string {
	repoFull := ref.Repo
	issueRef := ref.Key()
	if issueRef == "" {
		issueRef = repoFull
	}
	// An external item's URL is the agent's only route to the work item itself.
	// GitHub-backed work needs no such hint: `owner/repo#42` is directly
	// actionable, and appending a URL there would change every existing prompt.
	sourceHint := ""
	if !ref.IsGitHubIssue() && ref.URL != "" {
		sourceHint = fmt.Sprintf(
			" This work item lives in the %s work source, not in GitHub Issues; read it at %s.",
			sourceLabel(ref.SourceType), ref.URL)
	}
	return buildTaskPromptBody(repoFull, issueRef, title, sourceHint)
}

// taskIDSegment is the per-item component of a task id. For GitHub-backed work
// it is the decimal issue number, which keeps the id exactly the shape it has
// always had; for a string-keyed source it is the native external id, so two
// zero-numbered items minted in the same second get different ids.
func taskIDSegment(ref worksource.Ref) string {
	if ref.Number > 0 {
		return strconv.Itoa(ref.Number)
	}
	return ref.ExternalID
}

// sourceLabel renders a work-source type for prose. An empty type means the
// item came from GitHub enumeration, which predates the field.
func sourceLabel(sourceType string) string {
	if sourceType == "" {
		return "github"
	}
	return sourceType
}

func buildTaskPromptBody(repoFull, issueRef, title, sourceHint string) string {
	// The workspace contract (kubestellar/hive#2545): your tmux pane already
	// starts rooted in $HIVE_WORKSPACE_DIR (contributor-agent.sh creates it and
	// launches the session with -c pointed there), but nothing had put a repo
	// on disk there yet. The previous prompt's only repository instruction was
	// 'gh repo fork ... --clone=false' — a fork WITHOUT a checkout — so an
	// agent that followed it literally, or one that stalled before improvising
	// its own clone, was left sitting in an empty directory while the
	// assignment slot stayed held. Spell out an actual clone into that known
	// directory so there is a concrete first step rather than an implied one.
	return fmt.Sprintf(
		"You are a contributor to the %s hive. Work on issue %s: \"%s\".%s "+
			"You do NOT have push access to the upstream repo. "+
			"Start by getting a real checkout on disk: "+
			"'gh repo fork %s --clone=true --remote=true "+
			"$HIVE_WORKSPACE_DIR/%s' (or, if that directory already has a clone "+
			"from a prior task, 'cd' into it and 'git fetch' instead of "+
			"re-forking). Then 'cd' into that checkout, read the issue, "+
			"understand what's needed, and take action. "+
			// DCO is enforced on this repo (CONTRIBUTING.md) and an unsigned
			// commit blocks the merge, but the prompt used to leave sign-off
			// entirely to whatever each agent inferred from the repo. That
			// varies even within one backend: two agy tasks produced one
			// unsigned PR (#4127, fixed by hand) and one signed (#4176). Every
			// other required step here is spelled out; this one was the
			// exception, so say it.
			"Commit with 'git commit -s' so every commit carries a Signed-off-by "+
			"trailer — the DCO check blocks the merge without it, and the trailer's "+
			"email must match the commit author's email. "+
			// Open it READY, not draft. A draft trips the repo's
			// do-not-merge/work-in-progress automation and tide will not merge
			// one, so a draft left behind is a PR nobody is waiting on and
			// nothing will land. The prompt used to say only "open a PR", which
			// an agent can satisfy perfectly well with `gh pr create --draft`.
			//
			// Observed live: the task for #4188 scoped down to child #4205 and
			// ran `gh pr create --draft`, then verified `isDraft: true` and
			// reported "Draft PR #4242 is open" as a SUCCESSFUL handoff — it had
			// no reason to think otherwise. The contributor had to intervene by
			// hand ("don't make them draft. Make sure they are submitted and
			// ready for review") to get it marked ready.
			"Push your branch to your fork remote, then open a PR from your fork. "+
			"Open it ready for review, not as a draft (do not pass --draft): a draft "+
			"is auto-labelled do-not-merge/work-in-progress and cannot merge. If a PR "+
			"is already open as a draft, mark it ready for review. "+
			"Use the GH_TOKEN env var for all gh commands (do NOT use 'unset GITHUB_TOKEN'). "+
			// #3987: the no_work_needed sentinel. The relay scrapes this exact
			// marker from the agent's output and reports it as the completion
			// verdict, so an issue whose remainder is maintainer-gated stops
			// re-entering the offer pool every cooldown window. Keep the marker
			// spelling in sync with detectNoWorkVerdict in
			// bin/contributor-relay.sh.
			"If you determine there is genuinely NOTHING shippable — for example the "+
			"remaining work is blocked on an unanswered maintainer decision, or merged "+
			"PRs already cover everything actionable — do NOT open a PR; instead print "+
			"a single line of the exact form 'HIVE_VERDICT: no_work_needed — <short reason>' "+
			"and stop.",
		repoFull, issueRef, title, sourceHint, repoFull, repoFull,
	)
}

// attributionPromptInstruction renders the footer instruction appended to an
// assignment prompt (kubestellar/hive#4105). It tells the agent — up front, using
// the hub's handshake-recorded invocation values — the exact attribution trailer
// its PR body must end with, so agent-produced PRs carry the footer without
// waiting for the hub-side EnsurePRAttribution reconciliation (#4088), which
// stays in place unchanged as an idempotent safety net. Like the rest of the
// prompt this is a pure function of non-credential metadata, so it remains safe
// to preview in the ops tab (#2539). Returns "" when the metadata renders no
// trailer at all (all fields unknown) — a bare prefix instruction would ask the
// agent to produce an empty footer.
func attributionPromptInstruction(meta ghpkg.InvocationMeta) string {
	trailer := meta.Trailer()
	if trailer == "" {
		return ""
	}
	return " At the bottom of your PR body, include exactly this line: '" + trailer + "'."
}

// promptInvocationMeta snapshots the hub's own handshake-recorded invocation
// metadata for a connection (never relay-self-reported values), mirroring the
// meta built by reconcilePRAttribution so the prompt instruction (#4105) and the
// post-merge reconciliation trailer (#4088) always agree. Caller must NOT hold
// c.mu. Nil-safe: a nil connection yields zero metadata (and hence no footer
// instruction).
func promptInvocationMeta(c *ContributorConnection) ghpkg.InvocationMeta {
	if c == nil {
		return ghpkg.InvocationMeta{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	meta := ghpkg.InvocationMeta{
		Agent:   c.role,
		Backend: c.cliBackend,
		Model:   ghpkg.RequestedModel(c.cliBackend, c.model),
		Effort:  c.reasoningEffort,
	}
	return meta
}

// canonicalRepoKey maps an arbitrary, possibly client-supplied repo string to the
// SAME canonical form the server keys everything on: FrontendRepo.Full, i.e. the
// exact string selectTask's activeIssues guard, the failure/quarantine cooldowns,
// and the completion cooldown all build their "repo#number" keys from.
//
// Why this is load-bearing (#2644): currentTask.Repo is normally set by selectTask
// to chosen.repoFull (== repo.Full). But the task_progress RESUME path re-populates
// currentTask from the CLIENT-supplied msg.Repo after a reconnect (the relay keeps
// its task locally and re-asserts it via task_progress). If the relay reports the
// repo in ANY other spelling than repo.Full — a bare name where the hub uses
// "owner/repo", or a differently-cased/prefixed cross-org name — then every
// server-side "%s#%d" key built from currentTask.Repo silently MISSES:
//   - the activeIssues double-assign guard no longer excludes the in-flight issue,
//     so a concurrent selectTask hands the SAME issue to a second contributor; and
//   - the disconnect/abandon reconnect-window cooldown (recordTaskFailure) is booked
//     under the wrong key, so it does not protect that issue either.
//
// This is exactly the #2356/#2492 duplicate-assignment race re-opened through a key
// mismatch, and it is intermittent + repo-specific: it only fires for a repo whose
// relay-reported name differs from the hub's repo.Full, and only across a reconnect
// that resumes via task_progress — which is why #2644 was seen "only in this repo".
//
// Resolution order:
//  1. Exact match on a known repo's Full (already canonical) → return it unchanged.
//  2. Match on a known repo's Name (case-insensitive) or its Full (case-insensitive)
//     → return that repo's Full, adopting the canonical casing/prefix.
//  3. No status/no match: fall back to the SAME rule buildRepos uses — prefix the
//     configured Org when the string carries no "owner/" segment — so a bare name
//     still lands on "org/name". If even the org is unknown, return the raw string
//     (unchanged behaviour of last resort; never worse than before).
func (h *ContributeWSHub) canonicalRepoKey(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return repo
	}

	var status *StatusPayload
	if h != nil && h.server != nil {
		h.server.statusMu.RLock()
		status = h.server.status
		h.server.statusMu.RUnlock()
	}
	if status != nil {
		// First pass: an exact Full match is already canonical — cheap and common.
		for _, r := range status.Repos {
			if r.Full == repo {
				return r.Full
			}
		}
		// Second pass: reconcile a differently-spelled client value against the
		// known set by Name or case-insensitive Full, adopting the canonical Full.
		for _, r := range status.Repos {
			if strings.EqualFold(r.Name, repo) || strings.EqualFold(r.Full, repo) {
				return r.Full
			}
		}
	}

	// Fallback mirrors buildRepos: a bare name is qualified with the configured org
	// so it matches the "org/name" Full the rest of the server builds.
	if !strings.Contains(repo, "/") &&
		h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		if org := h.server.deps.Config.Project.Org; org != "" {
			return org + "/" + repo
		}
	}
	return repo
}

// resumeGateReason re-runs, for a RESUME (kubestellar/hive C4), the same admission
// gates a fresh selectTask assignment must clear that are NOT specific to a
// particular candidate issue: the profile must not be revoked, the whole contribute
// queue must not be suspended, and the contributor's trust tier must not be
// operator-disabled. It returns a non-empty machine-readable reason when resume must
// be REFUSED (so the caller can revoke the lease and tell the relay why), or "" when
// resume may proceed. It deliberately does NOT re-run the per-candidate cooldown /
// repo-filter / concurrency-window gates: the lease is for a task the hub already
// committed to this identity, so re-applying candidate selection would falsely refuse
// a legitimately in-flight task. The gates checked here are the ones that represent an
// operator turning access OFF after assignment.
func (h *ContributeWSHub) resumeGateReason(c *ContributorConnection) string {
	tier := ""
	if c != nil && c.profile != nil {
		if c.profile.TrustTier == "revoked" {
			return "contribution access revoked"
		}
		tier = c.profile.TrustTier
	}
	if h.server == nil {
		return taskUnavailableHubNotReady
	}
	if h.server.deps != nil && h.server.deps.Config != nil {
		if h.server.deps.Config.Hub.ContributeSuspended {
			return taskUnavailableContributionSuspended
		}
		for _, dt := range h.server.deps.Config.Hub.DisabledTiers {
			if dt == tier {
				return taskUnavailableTierDisabled
			}
		}
	}
	return ""
}

func (h *ContributeWSHub) selectTask(c *ContributorConnection) *WSMessage {
	h.selectMu.Lock()
	defer h.selectMu.Unlock()

	if h.server == nil {
		// No server reference — the hub cannot read status or config, so there is
		// nothing to select from. Jorge flagged this path as arguably not
		// contributor-visible; it is folded into hub_not_ready since it is the same
		// "the hub cannot serve work yet" condition and giving it a reason is
		// trivial and harmless (#2546).
		return h.taskUnavailable(taskUnavailableHubNotReady)
	}

	requestedRole := ""
	if c != nil {
		requestedRole = normalizeAgentRole(c.role)
	}
	if requestedRole != "" {
		if ok, reason := h.roleClaimAllowed(c, requestedRole); !ok {
			h.logger.Warn("[contribute-ws] refusing task: agent role not permitted",
				"username", identityOf(c), "role", requestedRole, "reason", reason)
			return h.taskUnavailable(taskUnavailableRoleNotPermitted)
		}
	}
	if h.server.deps != nil && h.server.deps.Config != nil && h.server.deps.Config.Hub.ContributeSuspended {
		// #2546: the operator suspended the whole contribute queue. Previously this
		// returned a bare nil and the contributor waited in silence, unable to tell
		// "suspended" from "misconfigured" from "wedged". Send an explicit
		// contribution_suspended negative-ack so the idle state is legible.
		return h.taskUnavailable(taskUnavailableContributionSuspended)
	}

	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()

	if status == nil {
		// #2546: no status snapshot yet (hub still warming up). Same wire-shape as
		// above, distinct reason, so the contributor learns it is a transient
		// not-ready state rather than a permanent refusal.
		return h.taskUnavailable(taskUnavailableHubNotReady)
	}

	// #2436 finding 2: refuse to assign work to a contributor whose TrustTier is
	// switched off via hub.disabled_tiers. This control was declared and
	// admin-writable but never read, so an operator "disabling" a tier changed
	// nothing. Send an explicit tier_disabled negative-ack rather than silently
	// handing out work anyway.
	tier := ""
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	if h.server.deps != nil && h.server.deps.Config != nil {
		for _, dt := range h.server.deps.Config.Hub.DisabledTiers {
			if dt == tier {
				h.logger.Warn("[contribute-ws] refusing task: tier disabled",
					"username", identityOf(c), "tier", tier)
				return h.taskUnavailable(taskUnavailableTierDisabled)
			}
		}
	}

	// activeIssues tracks issues already held by SOME live connection so we do
	// not double-assign the same work item. identityHolds counts, per identity,
	// how many tasks that identity currently holds across ALL of its live
	// connections — the source of truth for the #2436 finding 3 concurrency gate
	// (one identity opening several connections must not exceed MaxConcurrent).
	activeIssues := make(map[string]bool)
	identityHolds := make(map[string]int)
	h.mu.RLock()
	for _, conn := range h.connections {
		conn.mu.Lock()
		if conn.currentTask != nil {
			if key := conn.currentTask.identityKey(); key != "" {
				activeIssues[key] = true
			}
			identityHolds[identityOf(conn)]++
		}
		conn.mu.Unlock()
	}
	h.mu.RUnlock()

	// #2436 finding 3 / #2566: enforce tier_limits per identity. The config ships
	// populated MaxConcurrent/MaxPerHour/MaxPerDay defaults, so an operator
	// reasonably believes concurrency AND rate are capped — and since #2562 the
	// Management & Operations control-plane DISPLAYS all three as if authoritative.
	// Before this, only MaxConcurrent was enforced; MaxPerHour / MaxPerDay were left
	// as TODO(#2436) and never read, so the hourly/daily numbers an operator set (or
	// saw in the control-plane) were inert. All three are now enforced here.
	//
	// A limit <= 0 is treated as "unlimited" for every field (the "advisor" default
	// is 0 across the board, and existing configs that never set a field must keep
	// working). MaxConcurrent counts tasks currently HELD (identityHolds, from the
	// live-connection scan above); MaxPerHour / MaxPerDay count tasks ASSIGNED inside
	// the trailing rateLimitHourWindow / rateLimitDayWindow (rolling windows, see
	// assignmentTimes). Concurrency is checked first as the tightest, most immediate
	// gate; the rate windows are checked next so a contributor learns exactly which
	// cap they hit. The daily assignment is recorded only once a task is actually
	// shipped, at the task_assign site below.
	if h.server.deps != nil && h.server.deps.Config != nil {
		if limits, ok := h.server.deps.Config.Hub.TierLimits[tier]; ok {
			if limits.MaxConcurrent > 0 && identityHolds[identityOf(c)] >= limits.MaxConcurrent {
				h.logger.Warn("[contribute-ws] refusing task: concurrency limit reached",
					"username", identityOf(c), "tier", tier,
					"held", identityHolds[identityOf(c)], "max_concurrent", limits.MaxConcurrent)
				return h.taskUnavailable(taskUnavailableConcurrencyLimit)
			}
			if limits.MaxPerHour > 0 || limits.MaxPerDay > 0 {
				hourCount, dayCount := h.rateWindowCounts(identityOf(c), time.Now())
				if limits.MaxPerHour > 0 && hourCount >= limits.MaxPerHour {
					h.logger.Warn("[contribute-ws] refusing task: hourly rate limit reached",
						"username", identityOf(c), "tier", tier,
						"assigned_last_hour", hourCount, "max_per_hour", limits.MaxPerHour)
					return h.taskUnavailable(taskUnavailableHourlyLimit)
				}
				if limits.MaxPerDay > 0 && dayCount >= limits.MaxPerDay {
					h.logger.Warn("[contribute-ws] refusing task: daily rate limit reached",
						"username", identityOf(c), "tier", tier,
						"assigned_last_day", dayCount, "max_per_day", limits.MaxPerDay)
					return h.taskUnavailable(taskUnavailableDailyLimit)
				}
			}
		}
	}

	totalAvailable := 0
	for _, repo := range status.Repos {
		totalAvailable += len(repo.ActionableIssues)
	}
	h.logger.Info("[contribute-ws] selectTask scanning", "repos", len(status.Repos), "totalIssues", totalAvailable, "cooldown", len(h.completedTasks), "active", len(activeIssues))

	var disabledRepos []string
	var heldIssues map[string]struct{}
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
		// Operator HOLD (#queue-hold): a manually-parked issue must never be offered,
		// indefinitely, until the operator Resumes it. Built once per selectTask from
		// the same canonical "%s#%d" keys the cooldown/active/failure checks use, so
		// the exclusion cannot miss on a repo-name spelling mismatch (#2648).
		heldIssues = queueHoldSet(h.server.deps.Config.Hub.ContributeQueueHold)
	}

	// --- Collect the eligible candidates, then order them (#2390) ---------------
	//
	// #2390 (castrojo): "a great default would be starting with MY PRs that are
	// blocking someone else or have been reviewed and are waiting for something."
	// i.e. before we hand a connected contributor a brand-new issue, prefer a work
	// item that is already THEIRS and needs their attention.
	//
	// Priority order applied below:
	//   1. The contributor's OWN work (candidate author == c.profile.GitHubUsername)
	//   2. Everything else — today's plain first-eligible ordering
	//
	// Honest scope note on the available signal:
	//   selectTask only iterates ActionableIssues (issues), and the per-candidate
	//   map carries `author` but NO review state. The richer signal #2390 really
	//   wants — "my PR has been reviewed / is approved / requested changes / is
	//   blocking someone else" — is not collected anywhere yet:
	//     * bin/enumerate-actionable.sh enumerates PRs with only
	//       title/labels/author/draft/url (no reviewDecision, no requested-changes,
	//       no "blocking"), and those PRs land in FrontendRepo.OpenPrs, which this
	//       selector does not read at all.
	//     * github.PullRequest has no review-decision field.
	//   So this change implements ONLY the ordering half of #2390, keyed on the one
	//   own-work signal that actually exists today (issue authorship). When the
	//   contributor has no own-authored candidate, we fall back to today's exact
	//   ordering — behaviour is unchanged in that (common) case.
	//
	//   TODO(#2390, depends on read-only-gh #2393-#2396): thread review state into
	//   the candidate data — extend enumerate-actionable.sh to collect the
	//   contributor's own open PRs together with their reviewDecision
	//   (APPROVED / CHANGES_REQUESTED / REVIEW_REQUIRED) and any "blocking" signal,
	//   surface those as candidates here, and refine ownWorkPriority to rank a
	//   reviewed/blocking own-PR ahead of a merely-own issue. Guard gracefully when
	//   that data is absent, exactly as the ownWork fallback does now.
	ownUsername := ""
	var ownInterests []string
	if c.profile != nil {
		ownUsername = c.profile.GitHubUsername
		ownInterests = c.profile.LabelInterests
	}
	// interestMatchesLabels reports whether any of the issue's labels matches one of
	// the contributor's opt-in label interests (#2637), case-insensitively. Empty
	// interests → never a match (the affinity tier is then a no-op). This is the
	// SOFT routing signal: a contributor with interests set is offered matching work
	// first (see the sort below), but still receives non-matching work when none
	// matches, so a willing contributor never sits idle.
	interestMatchesLabels := func(labels []string) bool {
		if len(ownInterests) == 0 || len(labels) == 0 {
			return false
		}
		for _, want := range ownInterests {
			for _, have := range labels {
				if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(have)) {
					return true
				}
			}
		}
		return false
	}

	type candidate struct {
		repoFull string
		number   int
		// ref is the candidate's canonical, source-aware identity
		// (kubestellar/hive#4245). Every exclusion below keys on ref.Key(), so
		// two zero-numbered external items are distinct work rather than one
		// shared "repo#0".
		ref    worksource.Ref
		title  string
		url    string
		labels []string
		lane   string
		isOwn  bool
		// interestMatch is true when the issue carries a label the contributor has
		// opted into (#2637). It is a SOFT priority tier below own-work: matching
		// work is offered first, but a contributor with no match still gets other
		// work. Off entirely when the contributor set no interests.
		interestMatch bool
		// recentFailures is the issue's current consecutive-failure count (#2435).
		// It is a stable tie-break in the ordering below: among equally-admissible
		// candidates, fewer-recent-failures first. Issues in an active failure
		// cooldown are already excluded above, so this only deprioritises an issue
		// whose short cooldown just elapsed but which still has failure history —
		// a backstop that keeps the queue moving even if the ledger is imperfect.
		recentFailures int
	}
	var candidates []candidate

	// One ledger snapshot for this whole selection pass (#3845), shared with the
	// contributor-neutral admission gate below so ReadyQueue and selectTask judge
	// dependencies against the same contract. Discarded when the pass ends, so
	// every selection re-observes current state rather than trusting a cache.
	admissionSweep := h.newAdmissionSweep()

	for _, repo := range status.Repos {
		if len(repo.ActionableIssues) == 0 {
			continue
		}
		if config.MatchesAny(repo.Full, disabledRepos) || config.MatchesAny(repo.Name, disabledRepos) {
			continue
		}
		for _, raw := range repo.ActionableIssues {
			// ActionableIssues contains ghpkg.Issue structs stored as any.
			// Marshal/unmarshal to get a map we can read fields from.
			b, err := json.Marshal(raw)
			if err != nil {
				h.logger.Debug("[contribute-ws] marshal fail", "repo", repo.Full, "error", err)
				continue
			}
			var issue map[string]any
			if err := json.Unmarshal(b, &issue); err != nil {
				h.logger.Debug("[contribute-ws] unmarshal fail", "repo", repo.Full, "error", err)
				continue
			}

			// Canonical, source-aware identity (kubestellar/hive#4245). The old
			// `number == 0` skip rejected every Linear and Jira item outright,
			// so non-GitHub work could never be offered to a contributor at all.
			// An item is now skipped only when it has NO identity — no issue
			// number AND no external key — which is the one case where any key
			// would be fabricated.
			ref := refFromIssueMap(repo.Full, issue)
			itemKey := ref.Key()
			if itemKey == "" {
				h.logger.Info("[contribute-ws] skip: item has no usable identity (no number, no external id)", "repo", repo.Full)
				continue
			}
			number := ref.Number
			// Operator HOLD (#queue-hold): skip a manually-parked issue outright. This
			// is a persistent operator decision, DISTINCT from the time-based cooldown
			// below — a held issue never becomes a candidate until the operator Resumes
			// it. Keyed on the same canonical "%s#%d" as every other exclusion.
			if _, isHeld := heldIssues[itemKey]; isHeld {
				continue
			}
			if h.isTaskInCooldownKey(itemKey) {
				continue
			}
			// #3987: skip an issue with a live no_work_needed completion verdict
			// (the #2547 shape — shippable parts merged, remainder maintainer-
			// gated, no open PR left for the claim ledger to see). The verdict
			// is voided the moment the issue shows activity newer than it, so a
			// genuinely reopened issue comes straight back into the pool.
			if h.isSuppressedByNoWorkVerdictKey(itemKey, issueUpdatedAtFromMap(issue)) {
				continue
			}
			// #2435: skip an issue still inside its short post-failure cooldown or
			// its longer quarantine. This is the primary livelock fix — a failing
			// issue at the head of the scan is no longer instantly re-admissible.
			if h.isTaskInFailureCooldownKey(itemKey) {
				continue
			}
			if activeIssues[itemKey] {
				continue
			}
			labels := stringSliceFromAny(issue["labels"])
			// #3768: skip an issue that an open PR — from ANYONE, hive agent or
			// human contributor — already claims to fix. The activeIssues guard
			// above only covers tasks held by LIVE connections, and the
			// completion cooldown only starts when the relay reports a verified
			// task_complete; a contributor who opens a PR but whose completion
			// report is missed (relay scrape failure, disconnect, verification
			// downgrade) left the issue re-offerable after the short 4h no-PR
			// cooldown, so projectbluefin/dakota#353 accumulated one duplicate
			// PR per window. The claim ledger sees the open PR itself on the
			// next eval cycle, which closes that hole regardless of what the
			// relay managed to report.
			decision := h.evaluateContributorNeutralAdmission(admissionSweep, contributorAdmissionCandidate{
				repoFull:  repo.Full,
				repoName:  repo.Name,
				number:    number,
				ref:       ref,
				labels:    labels,
				dependsOn: dependenciesFromIssueMap(issue),
			})
			if !decision.admitted {
				switch decision.reason {
				case contributorAdmissionReasonOpenPRClaim:
					h.logger.Info("[contribute-ws] skip: issue already claimed by an open PR",
						"repo", repo.Full, "number", number,
						"pr_url", decision.claim.PRURL, "pr_author", decision.claim.PRAuthor)
				case contributorAdmissionReasonWorkflowBlocked:
					h.logger.Info("[contribute-ws] skip: issue is blocked by workflow state",
						"repo", repo.Full, "number", number)
				// #3845: a declared dependency is not satisfied (or cannot be
				// resolved), so this issue is not admissible work yet. Only THIS
				// candidate is withheld — the scan continues and unrelated ready
				// work is still offered.
				case contributorAdmissionReasonDependencyBlocked, contributorAdmissionReasonDependencyUnknown:
					h.logger.Info("[contribute-ws] skip: dependency not satisfied",
						"repo", repo.Full, "number", number,
						"reason", decision.convergence.Reason,
						"bead", decision.convergence.ObservedRecord,
						"generation", decision.convergence.ObservedGeneration,
						"blockers", strings.Join(decision.convergence.Blockers, ","))
				}
				continue
			}
			// Yank self-exclusion: an issue this SAME clanker was just yanked off is
			// briefly skipped for it (yankSelfExcludeSeconds), so the immediate post-yank
			// reassignment moves the clanker to genuinely different work instead of re-
			// grabbing the same item. Scoped to (this clanker, this issue): every OTHER
			// contributor may still be offered the issue immediately. Guarded by h.mu.
			if c.profile != nil {
				h.mu.Lock()
				selfExcluded := h.isYankSelfExcludedKeyLocked(c.profile.ContributorID, itemKey)
				h.mu.Unlock()
				if selfExcluded {
					continue
				}
			}

			title, _ := issue["title"].(string)
			url, _ := issue["url"].(string)
			author, _ := issue["author"].(string)
			lane, _ := issue["lane"].(string)
			assignees := stringSliceFromAny(issue["assignees"])

			// A tracker/umbrella issue is coordination-only: its children carry
			// the work and are queued independently, so handing the parent to one
			// contributor is never right. The enumerator has flagged these all
			// along (github.Issue.IsTracker) and this queue simply never read it —
			// scheduler.go consumed the flag for a "[TRACKER]" prompt annotation
			// and nothing else.
			//
			// Live cost of the omission: kubestellar/hive#4188 ("coordination-only
			// umbrella. Do not assign #4188 as one Hive task", per its own body,
			// above a 13-item child task list) was offered to a contributor, which
			// spent the full 30-minute MAX_TASK_DURATION budget and booked a
			// failure on an issue that only final integration can close. The agent
			// behaved correctly throughout — it triaged the children and shipped
			// one — but no agent can "complete" a parent like this.
			//
			// Logged rather than skipped silently: an issue that is never offered
			// should not be a mystery to the operator watching the queue.
			if isTracker, _ := issue["is_tracker"].(bool); isTracker {
				h.logger.Info("[contribute-ws] skipping tracker/umbrella issue",
					"repo", repo.Full, "number", number, "title", title)
				continue
			}
			if requestedRole != "" && !h.issueMatchesAgentRole(requestedRole, title, labels, lane) {
				continue
			}

			// Apply the title / author / label contribute filters. Each is a
			// single list plus a mode (allow = only matching pass; deny = matching
			// skipped). Labels were previously not enforced at all.
			if h.server.deps != nil && h.server.deps.Config != nil {
				hub := h.server.deps.Config.Hub
				if !config.FilterPasses(title, hub.ContributeDenyTitles, hub.ContributeTitlesMode) ||
					!config.FilterPasses(author, hub.ContributeDenyAuthors, hub.ContributeAuthorsMode) ||
					!config.LabelsFilterPasses(labels, hub.ContributeDenyLabels, hub.ContributeLabelsMode) {
					continue
				}
				// #2357: optionally skip issues already assigned to someone else.
				// An issue assigned to the contributor themselves (or unassigned)
				// stays eligible; only issues assigned solely to OTHER users are
				// skipped when the toggle is on.
				if hub.ContributeSkipAssignedToOthers &&
					assignedToOthers(assignees, c.profile.GitHubUsername) {
					continue
				}
			}

			// #2390: instead of assigning the first eligible issue inline, collect
			// it as a candidate. The own-work partition below reorders the whole
			// eligible set before we pick and assign one. The sibling filters
			// (#2357 skip-assigned, contribute allow/deny) have already run above,
			// so every appended candidate is genuinely assignable.
			candidates = append(candidates, candidate{
				repoFull: repo.Full,
				number:   number,
				ref:      ref,
				title:    title,
				url:      url,
				// The issue's own labels travel with the candidate so the chosen
				// task_assign can populate the Labels envelope field (kubestellar/
				// hive#2393 item 8). They're already computed for filtering above.
				labels: labels,
				lane:   lane,
				// "Own work" is the only #2390 signal available today: the
				// candidate was authored by the connected contributor. When the
				// username is unknown (empty), nothing is own → we keep today's
				// ordering untouched.
				isOwn:         ownUsername != "" && strings.EqualFold(author, ownUsername),
				interestMatch: interestMatchesLabels(labels),
				// #2435: carry any lingering failure history so the ordering below
				// can deprioritise a recently-failed issue within its bucket.
				recentFailures: h.recentFailureCountKey(itemKey),
			})
		}
	}

	if len(candidates) == 0 {
		// #2546: the hub is running and unsuspended but nothing is admissible right
		// now (everything is in cooldown, filtered out, disabled, or already held).
		// Previously a bare nil — indistinguishable on the wire from "suspended" or
		// "hub not ready". Send an explicit no_matching_work negative-ack.
		return h.taskUnavailable(taskUnavailableNoMatchingWork)
	}

	// Operator priority override (#queue-reorder): the ordered list of issue keys
	// the operator dragged to the front of the ready-work queue on the Operations
	// tab. It takes precedence over the default ordering below so a prioritised
	// issue is OFFERED FIRST. It never bypasses admission: every entry in
	// `candidates` already passed the SAME cooldown / failure / disabled-repo /
	// filter / in-flight exclusions above, so a pinned-but-no-longer-actionable key
	// simply never became a candidate (stale keys are skipped). Rank sentinel: a
	// candidate NOT in the override ranks at len(override), so all pinned candidates
	// sort ahead of all unpinned ones while their own relative order is the operator's.
	var queueOrderIdx map[string]int
	if h.server.deps != nil && h.server.deps.Config != nil {
		queueOrderIdx = queueOrderIndex(h.server.deps.Config.Hub.ContributeQueueOrder)
	}
	orderRank := func(c candidate) int {
		if len(queueOrderIdx) == 0 {
			return 0 // no override → every candidate ties, key is a no-op
		}
		if r, ok := queueOrderIdx[fmt.Sprintf("%s#%d", c.repoFull, c.number)]; ok {
			return r
		}
		return len(queueOrderIdx)
	}

	// Order the admissible set with a STABLE sort so the pick is deterministic
	// (easy to reason about and to test — no randomness):
	//   0. operator priority override first (#queue-reorder) — pinned issues in the
	//      operator's dragged order; a no-op when no override is set;
	//   1. own-work first (#2390 — preserved unchanged);
	//   2. then fewer recent failures first (#2435 remedy 3 backstop) — an issue
	//      whose short failure cooldown has just elapsed but which still carries
	//      failure history is deprioritised behind never-failed peers, so a
	//      flaky issue can no longer monopolise the head of the queue even if the
	//      ledger is imperfect;
	//   3. otherwise the established per-repo / creation scan order is kept.
	// When the contributor has no own work AND nothing has failed AND no override is
	// set, this is a no-op and behaviour is identical to the previous first-eligible pick.
	ownFirst := make([]candidate, len(candidates))
	copy(ownFirst, candidates)
	sort.SliceStable(ownFirst, func(i, j int) bool {
		if ri, rj := orderRank(ownFirst[i]), orderRank(ownFirst[j]); ri != rj {
			return ri < rj // operator-pinned (lower rank) sorts ahead
		}
		if ownFirst[i].isOwn != ownFirst[j].isOwn {
			return ownFirst[i].isOwn // own work sorts ahead of non-own
		}
		if ownFirst[i].interestMatch != ownFirst[j].interestMatch {
			return ownFirst[i].interestMatch // label-affinity matches ahead of non-matches (#2637)
		}
		if ownFirst[i].recentFailures != ownFirst[j].recentFailures {
			return ownFirst[i].recentFailures < ownFirst[j].recentFailures // fewer failures first
		}
		return false // equal keys → SliceStable preserves original scan order
	})

	chosen := ownFirst[0]
	if chosen.isOwn {
		h.logger.Info("[contribute-ws] prioritizing contributor's own work (#2390)",
			"username", ownUsername, "repo", chosen.repoFull, "number", chosen.number)
	}

	// Mint through the shared path so task_assign and the heartbeat token-refresh
	// advertise tokens minted the same way (#2393 item 2). tokenMintedAt below
	// arms the refresh ticker for the token we hand out here. C4: the token is
	// scoped to the chosen issue's REPOSITORY, not the whole installation.
	ghToken, err := h.mintScopedToken(c.profile.TrustTier, chosen.repoFull)
	if err != nil {
		// #2436 finding 1: a mint failure previously returned nil, stranding the
		// contributor with no message (the log even said "skipping task" while
		// abandoning the whole selection). Send an explicit token_mint_failed
		// negative-ack so the failure is diagnosable instead of an indefinite
		// hang. We do not fall through to another candidate: the mint is keyed on
		// the contributor's tier, not the candidate, so every candidate in this
		// pass would fail identically. Preserve the existing Warn log.
		h.logger.Warn("[contribute-ws] failed to mint scoped token — task unavailable",
			"tier", c.profile.TrustTier, "error", err)
		return h.taskUnavailable(taskUnavailableTokenMintFailed)
	}

	// The task id carries the item's own identity segment so two zero-numbered
	// external items cannot mint the same id within one second
	// (kubestellar/hive#4245). GitHub-backed work keeps its historical
	// "ct-<repo>-<number>-<unix>" shape byte for byte.
	taskID := fmt.Sprintf("ct-%s-%s-%d", chosen.repoFull, taskIDSegment(chosen.ref), time.Now().Unix())

	// #2539: build the prompt through the shared, credential-free buildTaskPrompt
	// so the exact text shipped in task_assign below can also be PREVIEWED
	// read-only in the ops tab. The prompt is a pure function of task metadata —
	// the minted github_token is attached to the WSMessage separately (never inside
	// the prompt), so previewing the prompt can never leak the token. buildTaskPrompt
	// itself carries the #2545 workspace-clone instruction (real checkout into
	// $HIVE_WORKSPACE_DIR rather than a fork-only --clone=false).
	prompt := buildTaskPromptForRef(chosen.ref, chosen.title)
	if requestedRole != "" {
		prompt = buildRoleTaskPromptForRef(chosen.ref, chosen.title, requestedRole, h.roleKickPrompt(requestedRole))
	}
	// #4105: tell the agent up front — from the hub's own handshake-recorded
	// invocation values — the exact attribution trailer its PR body must end
	// with, so the footer is intentionally produced rather than appended only
	// by the post-merge reconciliation safety net (#4088, unchanged).
	prompt += attributionPromptInstruction(promptInvocationMeta(c))

	// #2568: mint a fresh assignment generation for this task. It is stamped on the
	// connection, shipped in task_assign below, and echoed back by the relay so a
	// later stale-worker completion carrying an older generation is fenced out.
	gen := h.nextTaskGen()

	c.mu.Lock()
	c.currentTask = &WSTaskAssign{
		TaskID:     taskID,
		Kind:       "issue",
		Role:       requestedRole,
		Repo:       chosen.repoFull,
		Number:     chosen.number,
		Title:      chosen.title,
		Key:        chosen.ref.Key(),
		SourceType: chosen.ref.SourceType,
		ExternalID: chosen.ref.ExternalID,
		URL:        chosen.url,
	}
	c.currentTaskGen = gen
	// #2568: start the hub-owned lease clock. task_progress renews it; cleanupLoop
	// auto-releases the task if it is not renewed within wsTaskTimeout.
	c.lastLeaseRenew = time.Now()
	// Store the prompt (never the token) so FleetSnapshot can preview it (#2539),
	// and clear any stale idle reason now that this connection has real work.
	c.currentPrompt = prompt
	c.currentLabels = chosen.labels
	c.lastIdleReason = ""
	// #2537: hold the minted scoped token as PENDING rather than shipping it in the
	// task_assign below. It is delivered only AFTER the acceptance decision — see
	// the ready-handler (auto-accept default) and the task_accepted handler
	// (explicit-accept mode) — via deliverTaskCredential. A fresh assignment resets
	// the delivered flag so the new task's credential is (re)delivered post-accept.
	// tokenMintedAt is set here so the #2393 refresh cycle is armed for the token we
	// hand out; deliverTaskCredential re-stamps it on actual delivery to anchor the
	// 50-minute refresh on when the relay truly received the credential.
	c.pendingToken = ghToken
	c.credentialDelivered = false
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()

	// C4: record the SERVER-AUTHORITATIVE lease for this assignment so a later
	// reconnect can be validated against what the hub actually issued — the exact
	// {task, repo, generation, tier} bound here — instead of reconstructing ownership
	// from client-supplied task_progress fields. Revoked on every release path.
	h.recordLease(identityOf(c), taskID, chosen.repoFull, chosen.number, c.profile.TrustTier, gen, time.Now())

	// #2566: record this assignment against the identity's rolling hourly/daily
	// windows so the next selectTask enforces tier_limits.max_per_hour /
	// max_per_day. Recorded here — after the task is committed to the connection
	// and we are certain a task_assign will ship — so a refused pass (which returns
	// early above) never consumes a slot. Uses the same identity key as the
	// concurrency gate.
	h.recordAssignment(identityOf(c), time.Now())

	return &WSMessage{
		Type:    "task_assign",
		Seq:     h.nextSeq(),
		TaskID:  taskID,
		TaskGen: gen,
		Kind:    "issue",
		Role:    requestedRole,
		Repo:    chosen.repoFull,
		Number:  chosen.number,
		Title:   chosen.title,
		URL:     chosen.url,
		// Source-aware identity (kubestellar/hive#4245). Additive: a GitHub
		// assignment carries exactly the fields it always did, and these tell a
		// relay working a Linear or Jira item what the item actually IS instead
		// of leaving it to infer one from `number: 0`.
		TaskKey:    chosen.ref.Key(),
		SourceType: chosen.ref.SourceType,
		ExternalID: chosen.ref.ExternalID,
		// #2537: NO github_token / token_expires_at here. The scoped credential is
		// split out of task_assign and delivered only after acceptance (see
		// pendingToken / deliverTaskCredential). task_assign now carries exactly the
		// metadata needed to DECIDE — repo/number/title/url/labels/prompt — plus the
		// #2568 TaskGen lease token, and no credential, so nothing an agent could act
		// on is authenticated until the task's source has been accepted under the
		// operator/contributor policy.
		Prompt: prompt,
		// The chosen issue's own labels — the Labels envelope field was declared
		// but never populated, so a client reading it got nothing (kubestellar/
		// hive#2393 item 8). Carried on the candidate from the scan above.
		Labels:        chosen.labels,
		ContribLabels: []string{"contributor/" + c.profile.GitHubUsername},
	}
}

// assignedToOthers reports whether an issue is assigned to at least one user
// AND none of its assignees is the given contributor. An unassigned issue
// (empty list) returns false, and an issue assigned to the contributor
// themselves returns false, so both remain eligible for pickup (#2357). The
// username comparison is case-insensitive to match GitHub login semantics.
func assignedToOthers(assignees []string, username string) bool {
	if len(assignees) == 0 {
		return false
	}
	for _, a := range assignees {
		if strings.EqualFold(a, username) {
			return false
		}
	}
	return true
}

// stringSliceFromAny coerces a JSON-decoded value (from an issue map marshaled
// via encoding/json) into a []string. Labels arrive as []any of strings; any
// non-string elements are skipped. Returns nil for a missing/other-typed value.
func stringSliceFromAny(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sendJSON writes a frame to a bare websocket connection. It is UNSERIALIZED and
// may only be used on the PRE-HANDSHAKE path (auth_challenge / auth_failed), where
// no ContributorConnection has been registered yet and the conn is not shared with
// the heartbeat, operator, or lease-reclaim goroutines. Every write to a LIVE
// connection MUST go through ContributorConnection.send, which serializes on
// writeMu to satisfy gorilla/websocket's one-concurrent-writer contract.
func sendJSON(conn *websocket.Conn, msg WSMessage) error {
	return conn.WriteJSON(msg)
}

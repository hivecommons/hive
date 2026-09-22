package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	wsHeartbeatInterval = 30 * time.Second
	wsHeartbeatTimeout  = 90 * time.Second
	// wsTaskTimeout is the hub-owned LEASE TTL on task ownership (hivecommons/hive
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
	// wsMaxMessageSize is the largest frame the hub will read from a relay,
	// installed with conn.SetReadLimit below. It is a HARD bound: gorilla does
	// not truncate an oversized message, it closes the connection with 1009
	// "message too big" and the frame is lost — which, for a task_complete, is a
	// completed task the hub never hears about and hands out again
	// (hivecommons/hive#7932). Advertised to relays on auth_ok as
	// max_message_bytes so the sending side can trim to it rather than discover
	// it by being disconnected; raise the two together, never one alone.
	wsMaxMessageSize = 64 * 1024
	// repoPermissionTimeout bounds the user-specific permission lookup performed
	// before rendering an assignment prompt. A slow GitHub API must not hold the
	// contributor's ready request indefinitely; lookup failure safely falls back
	// to the fork workflow.
	repoPermissionTimeout = 5 * time.Second
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
	ws         *websocket.Conn
	connID     string
	profile    *ContributorProfile
	cliBackend string
	// session is the OPTIONAL client-declared session label from auth_response
	// (multi-session-per-account). Empty for a single-session contributor, which
	// keeps identityOf() at the bare ContributorID. When set, identityOf()
	// returns ContributorID#session so concurrent relays under one account get
	// independent lease/assignment/failure slots. Sanitized at auth time.
	session         string
	model           string
	reasoningEffort string
	// advisorModel / advisorEffort name the SECOND model that reviewed this
	// contributor's work and the effort it ran at (hivecommons/hive#7760) —
	// omp's --advisor today; any backend that grows a reviewer role can fill
	// them. Advisory display metadata exactly like model/reasoningEffort:
	// shown in fleet, run rows, the activity rail and the PR trailer, never
	// routed or gated on. Empty for every single-model backend.
	advisorModel  string
	advisorEffort string
	role          string // empty = task-driven mode, "scanner"/"reviewer"/etc. = role mode
	clientRole    string // relay-requested HIVE_AGENT_ROLE; owner assignment may override it
	assignedRole  string // owner-selected role; "none" forces general work
	connectedAt   time.Time
	currentTask   *WSTaskAssign
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
	// sawTaskGen records that this connection has echoed a NON-ZERO task_gen at
	// least once, i.e. proven it speaks the fenced protocol (kubestellar/hive#6909).
	// Once set, the clientGen==0 legacy escape in generationAccepted is withdrawn
	// for this connection so a fenced client cannot downgrade itself to unstamped
	// mid-session and slip past the #2568 Gate.
	sawTaskGen bool
	// lastLeaseRenew is when currentTask's hub-owned lease was last renewed
	// (kubestellar/hive#2568): set on assignment and refreshed on every task_progress.
	// cleanupLoop auto-releases a task whose lease has not been renewed within
	// wsTaskTimeout. Zero when no task is active.
	lastLeaseRenew time.Time
	// taskAssignedAt is when currentTask was assigned, kept SEPARATE from
	// lastLeaseRenew (which task_progress refreshes) so a terminal report can
	// record the task's real wall-clock duration in the run log
	// (task_run_log.go). Zero when no task is active or the task was adopted
	// via the resume path without a fresh assignment.
	taskAssignedAt time.Time
	lastPong       time.Time
	// tmuxOutput is the last pane snapshot the relay reported, and
	// tmuxOutputTask is the task_id that report named (#7605). Every
	// task_progress and task_complete frame overwrites both, so the pane on
	// disk is never older than the newest report — but it can belong to a
	// DIFFERENT task than the one this connection holds now: the previous
	// task's task_complete leaves its final screen here, and a socket that
	// drops seconds into the next assignment would otherwise attach that
	// finished run's terminal to the new task's abandoned row. Readers go
	// through paneTailFor, which hands out the pane only for the task it was
	// reported for.
	tmuxOutput     []string
	tmuxOutputTask string
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
	// while calling send, so the two never nest. See hivecommons/hive
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
	// Bound the write (kubestellar/hive#5090). WriteJSON on a gorilla connection
	// with no write deadline blocks INDEFINITELY once the peer's receive window
	// closes — a half-open socket (an L7 proxy that dropped the tunnel without
	// telling either endpoint) accepts no bytes and sends no RST, so the write
	// neither completes nor fails. Every caller of send holds writeMu for the
	// duration, so one wedged peer would park the heartbeat ticker, the read
	// loop's replies, and the operator revoke/yank/reassign paths for that
	// connection behind a lock nothing can break.
	//
	// wsWriteDeadline turns that unbounded park into a bounded failure the
	// existing error paths already handle: the heartbeat's write-failure branch
	// closes the socket with a reason, and a reply failure surfaces to its
	// caller. The deadline is per-write and generous enough that an ordinary
	// slow-but-live client is never cut — it exists to bound the pathological
	// case, not to police latency.
	if err := c.ws.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
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
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Session is an OPTIONAL client-declared session label (hivecommons/hive:
	// multi-session-per-account). One GitHub account has ONE contributor profile
	// (one ContributorID, one auth token, one trust tier), but a contributor may
	// want to run several relays at once under that account — e.g. one per CLI
	// backend (claude, agy, pi, kiro). All identity-keyed hub state (task leases,
	// assignment cooldowns, failure streaks, ownership fences) keys on
	// identityOf(); without a distinguisher those sessions would collide on a
	// single active-task slot. A distinct Session yields a distinct
	// session-scoped identity (ContributorID#session) for that state, while auth,
	// tier, model admission and rate-limit accounting stay per-account. Additive
	// and backward-compatible: omitted → identity is the bare ContributorID,
	// exactly the previous single-session behavior. Sanitized/bounded before use.
	Session         string `json:"session,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// AdvisorModel / AdvisorReasoningEffort are the relay's report of a second,
	// reviewing model (hivecommons/hive#7760): omp's --advisor. Additive and
	// optional on auth_response and task_progress; a relay without an advisor
	// omits both and an older hub ignores them. Display metadata only.
	AdvisorModel           string `json:"advisor_model,omitempty"`
	AdvisorReasoningEffort string `json:"advisor_reasoning_effort,omitempty"`
	TaskID                 string `json:"task_id,omitempty"`
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
	// Requirements carries the hub-derived task-side capability requirements
	// used by #2547 routing. Additive and advisory: older relays ignore it, and
	// enforcement already happened server-side before assignment.
	Requirements *ContributorTaskRequirements `json:"requirements,omitempty"`
	// Complexity carries the v5 task complexity tier used by contributor-local
	// quota preflight. Additive: older relays ignore it, and unknown is the safe
	// conservative default for clients that cannot classify the work.
	Complexity string `json:"complexity,omitempty"`
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
	// relay may report in its auth_response (kubestellar/hive#2547).
	// It is additive and purely advisory: a client that omits it authenticates
	// and runs exactly as before. Routing may only use it to avoid explicit
	// task-requirement mismatches; it is not a trust signal. Distinct from the RESERVED,
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
	// ConnectionID is the hub's per-socket id, advertised on auth_ok so relay
	// close logs can be correlated with hub disconnect/cleanup logs for #5090.
	ConnectionID string `json:"connection_id,omitempty"`
	// MaxMessageBytes is the hub's WebSocket read limit (wsMaxMessageSize),
	// advertised on auth_ok (hivecommons/hive#7932). It is the one server bound a
	// relay cannot discover by behaving well: exceeding it is answered with a
	// 1009 close, not a reply, and the frame that tripped it is gone. Stating it
	// lets a relay trim an oversized audit tail to fit instead of losing a whole
	// task_complete — and lets a hub that raises the ceiling carry its relays up
	// with it. Additive; a relay that ignores it keeps whatever default it ships.
	MaxMessageBytes int      `json:"max_message_bytes,omitempty"`
	Role            string   `json:"role,omitempty"`
	ContribLabels   []string `json:"contributor_labels,omitempty"`
	Status          string   `json:"status,omitempty"`
	Result          string   `json:"result,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	TmuxOutput      []string `json:"tmux_output,omitempty"`
	AcceptedModels  []string `json:"accepted_models,omitempty"`
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
	//
	// "blocked" (hivecommons/hive#7924) is also accepted: no_work_needed's
	// sibling for an issue nothing in its repo can move until something
	// outside it lands. The relay spells it as no_work_needed plus
	// VerdictBlocked below, so an older hub keeps its existing behaviour.
	Verdict string `json:"verdict,omitempty"`
	// VerdictBlocked marks a no_work_needed verdict as blocked (#7924): the
	// reason names an external dependency the issue is waiting on, not a
	// settlement. The hub books it for the full with-PR cooldown instead of
	// the escalating no-PR ladder and records the marker on the ledger row.
	// Any GitHub label for it is the RELAY's doing, with the task credential;
	// the hub itself still labels nothing.
	VerdictBlocked bool `json:"verdict_blocked,omitempty"`
	// VerdictReason optionally carries a machine-readable reason for a
	// no_work_needed verdict ("maintainer_gated", "already_covered", or free
	// text the relay scraped from the agent's output). Audit-only: it is
	// stored with the verdict and logged so an operator can see WHY an issue
	// keeps yielding no work — precisely the signal needed to go answer the
	// gating question. Truncated server-side to noWorkReasonMaxLen.
	VerdictReason string `json:"verdict_reason,omitempty"`
	// CompletionSignal records WHICH signal ended an interactive task
	// (kubestellar/hive#5376): "verdict" when the agent printed its own
	// HIVE_VERDICT: line, "chrome_idle" when it never did and the relay fell
	// back to a bounded grace period of idle-looking terminal chrome.
	//
	// Since #6723 an explicit "chrome_idle" marks the completion evidence-less
	// (no PR, no verdict): it books only the flat base cooldown instead of
	// escalating, and does not clear the issue's failure history. It still
	// grants no trust and no selection. It also remains the field by which
	// per-backend sentinel non-compliance is MEASURABLE rather than
	// guessed at: chrome inference is the mechanism behind thirteen separate
	// false-completion issues, and this field is how an operator sees which
	// backends are still relying on it. Absent from relays predating #5376 and
	// from the headless path, which takes the CLI's exit code instead.
	CompletionSignal string `json:"completion_signal,omitempty"`
	// TurnEnvelopeID identifies the durable pkg/turn envelope written for this
	// assignment when the re-entrant-turn rollout gate is enabled.
	TurnEnvelopeID string `json:"turn_envelope_id,omitempty"`
	// Permanent marks a task_failed the relay will not retry: it exhausted its
	// per-task CLI-restart budget and gave up (see MAX_TASK_CLI_RESTARTS in
	// bin/contributor-relay.js). Reassigning the same work item to the same
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
	// Requirements is the task-side capability requirement set used for
	// contributor routing. Nil means this task carried no explicit requirements.
	Requirements *ContributorTaskRequirements `json:"requirements,omitempty"`
	Complexity   string                       `json:"complexity,omitempty"`
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
	// AdvisorModel / AdvisorEffort: the second model that reviewed the work
	// (hivecommons/hive#7760), when the connection reported one.
	AdvisorModel  string `json:"advisor_model,omitempty"`
	AdvisorEffort string `json:"advisor_effort,omitempty"`
	Task          string `json:"task,omitempty"`
}

// advisorInfo is the optional trailing argument to addActivity: the advisor
// pair a connection reported (hivecommons/hive#7760). Passed as a value so the
// many existing call sites that have no connection at hand stay unchanged.
type advisorInfo struct {
	Model  string
	Effort string
}

// advisor returns the connection's advisor pair for addActivity. Reads the two
// fields without contributor.mu, exactly as the call sites already read
// c.model and c.reasoningEffort next to it.
func (c *ContributorConnection) advisor() advisorInfo {
	if c == nil {
		return advisorInfo{}
	}
	return advisorInfo{Model: c.advisorModel, Effort: c.advisorEffort}
}

type ContributeWSHub struct {
	connections map[string]*ContributorConnection
	mu          sync.RWMutex
	// unmintableRepos maps "owner/repo" to the instant its post-mint-failure
	// exclusion lapses (#7869); guarded by its own mutex because it is consulted
	// inside selectTask's candidate scan, which runs under selectMu, and written
	// after the unlock.
	unmintableRepos map[string]time.Time
	unmintableMu    sync.Mutex
	logger          *slog.Logger
	seq             int
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
	// decisions is the bounded per-contributor record of the hub's OWN
	// decisions — the refusals, fences and ignored reports that until #7330
	// existed only as slog lines on the hub's stdout. Its own mutex, never
	// h.mu: it is written from the connection goroutines and read from an HTTP
	// handler, and a diagnostic must not share a lock graph with the routing it
	// observes. See hub_decisions.go; zero value is ready.
	decisions hubDecisionLog
	// pendingConns counts sockets that have been upgraded but have not yet
	// authenticated (audit F9). h.connections only gains an entry AFTER auth
	// succeeds, so capping on that map alone left the pre-auth window — a full
	// wsAuthTimeout per socket — completely unbounded. Atomic rather than
	// mu-guarded to match taskGen's reasoning above: it is touched from the
	// upgrade path and from deferred cleanup, and must never contend with or
	// re-enter h.mu.
	pendingConns atomic.Int64
	// handlers tracks live HandleWS invocations so a caller (in practice:
	// tests) can wait for hijacked websocket handlers to fully unwind.
	// httptest's Server.Close does not wait for hijacked connections, so a
	// handler's deferred bookkeeping — the disconnect-abandonment task-run
	// append, decision-ring writes — can land AFTER a test returns, racing
	// t.TempDir() removal ("directory not empty") and any restored globals.
	handlers   sync.WaitGroup
	activityMu sync.RWMutex
	activity   []ActivityEntry
	// absorbedReconnects counts flaps collapsed by absorbReconnectFlapLocked
	// (kubestellar/hive#5151), so a contributor bouncing stays countable after its
	// feed rows stop being written. Guarded by activityMu alongside activity itself.
	absorbedReconnects int
	server             *Server
	completedTasks     map[string]time.Time
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
	// taskLeasesFile is where the server-issued lease registry is persisted so it
	// survives a hub restart (#5681). Overridable per hub for tests, like the
	// sibling ledgers.
	taskLeasesFile  string
	turnEnvelopeDir string
	taskRunLogFile  string
	// startedAt is when this hub process came up. It used to bound the window in
	// which a lease restored from the previous process was honoured as a hold on
	// its work item (#5681); since #7773 every unexpired lease is a hold, so it is
	// informational. Written once at construction and only read afterwards, so it
	// needs no lock.
	startedAt          time.Time
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
	// contributorFailureStreaks tracks, per identity, CONSECUTIVE hub-measured
	// sub-minute task failures (kubestellar/hive#6450, contribute_failure_streak.go).
	// Once a streak reaches contributorFailureStreakThreshold, selectTask pauses
	// that identity's claims for contributorFailureStreakPause and says so with an
	// explicit contributor_failure_streak negative-ack, so a dying agent runtime
	// is surfaced to the contributor instead of silently burning issue cooldowns
	// and standing. Reset by a completion or a slow (genuinely attempted) failure.
	// Guarded by failureStreakMu.
	contributorFailureStreaks map[string]contributorFailureStreak
	failureStreakMu           sync.Mutex
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
	// settleVerifier is the #7871 API-check seam; nil means "use
	// deps.GHClient.VerifySettlingRef". Tests substitute a fixture.
	settleVerifier ghpkg.SettleVerifier

	// recentlyFinished records, by task id, when a completed/failed run row was
	// written (#7838). Consulted by the deferred disconnect booking so a task
	// that resumed on a new socket AND finished inside the grace window is
	// not booked abandoned after the fact. Pruned lazily on insert; entries
	// live for recentlyFinishedTTL. Guarded by finishedMu.
	finishedMu       sync.Mutex
	recentlyFinished map[string]time.Time
	// graceBookings counts deferred #7838 bookings still pending or running.
	graceBookings sync.WaitGroup
	// leases is the hub-owned, server-authoritative registry of the task the hub
	// ISSUED to each contributor identity (hivecommons/hive C4). It is keyed by
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
	leases   map[string]*taskLease
	leaseMu  sync.Mutex
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

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
	if logger == nil {
		if server != nil && server.logger != nil {
			logger = server.logger
		} else {
			logger = slog.Default()
		}
	}
	contributorsDir := getContributorsDir()
	if server != nil {
		contributorsDir = server.contributorsDirOrDefault()
	}
	hub := &ContributeWSHub{
		connections:               make(map[string]*ContributorConnection),
		completedTasks:            make(map[string]time.Time),
		completedTaskCooldown:     make(map[string]time.Duration),
		completedTaskPRURL:        make(map[string]string),
		failedTasks:               make(map[string]time.Time),
		consecutiveFailures:       make(map[string]int),
		noPRStreaks:               make(map[string]noPRStreakRecord),
		noWorkVerdicts:            make(map[string]noWorkVerdictRecord),
		activityFilePath:          contributorStatePath(contributorsDir, activityFilePath, "activity.json"),
		completedTasksFile:        contributorStatePath(contributorsDir, completedTasksFile, "completed-tasks.json"),
		failedTasksFile:           contributorStatePath(contributorsDir, failedTasksFile, "failed-tasks.json"),
		noPRStreaksFile:           contributorStatePath(contributorsDir, noPRStreaksFile, "no-pr-streaks.json"),
		taskLeasesFile:            contributorStatePath(contributorsDir, taskLeasesFile, "task-leases.json"),
		turnEnvelopeDir:           contributorStatePath(contributorsDir, turnEnvelopeDirPath, "turn-envelopes"),
		taskRunLogFile:            contributorStatePath(contributorsDir, taskRunLogPath, taskRunLogFileName),
		startedAt:                 time.Now(),
		noWorkVerdictsFile:        filepath.Join(contributorsDir, noWorkVerdictsFileName),
		asyncActivitySave:         asyncActivitySave,
		persistActivity:           activityPersistenceEnabled,
		persistTaskLedgers:        taskLedgerPersistenceEnabled,
		assignmentTimes:           make(map[string][]time.Time),
		contributorFailureStreaks: make(map[string]contributorFailureStreak),
		leases:                    make(map[string]*taskLease),
		yankExclusions:            make(map[string]time.Time),
		recentlyFinished:          make(map[string]time.Time),
		logger:                    logger,
		server:                    server,
		sse:                       newSSERegistry(),
		stopCh:                    make(chan struct{}),
		doneCh:                    make(chan struct{}),
	}
	hub.loadCompletedTasks()
	hub.loadFailedTasks()
	hub.loadNoPRStreaks()
	hub.loadNoWorkVerdicts()
	hub.loadActivity()
	// #5681: restore the leases the PREVIOUS process issued before any relay can
	// reconnect, so an in-flight task survives the restart instead of being revoked
	// out from under a working agent.
	hub.loadLeases()
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] activity directory creation failed", "error", err)
		return
	}
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

// taskDescOf renders an assigned task for the activity feed.
//
// Identity comes from identityKey(), the same canonical, source-aware spelling
// the hub keys in-flight work on (#4245): the explicit Key when present,
// "repo#number" otherwise. Deriving it from Number alone would have been wrong
// for external work — a Linear or Jira item deliberately carries Number == 0 and
// puts its identity in Key/ExternalID, so treating "numberless" as "synthetic"
// discards exactly the identity that item has, and every such release would read
// as an opaque task id.
//
// A genuinely synthetic task — a pr-review sweep, which has no work item behind
// it and therefore no canonical key — falls back to its task id, which is all it
// has ever had.
func taskDescOf(task *WSTaskAssign) string {
	if task == nil {
		return ""
	}
	// Delegates to assignDesc (#5120), the one renderer every feed entry goes
	// through — this typed wrapper exists so the release sites (#5097) keep
	// their nil-tolerant one-argument call shape. Two copies of the format
	// string would drift; one already almost did.
	return assignDesc(task.Kind, task.identityKey(), task.Title, task.TaskID)
}

func (h *ContributeWSHub) addActivity(username, action, role, cli, model, effort, task string, advisor ...advisorInfo) {
	adv := advisorInfo{}
	if len(advisor) > 0 {
		adv = advisor[0]
	}
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
	// #5151: absorb a fast reconnect instead of booking it as a departure plus an
	// arrival. A flap emits "released: connection lost" -> "left" -> "joined"; the
	// debounce above never fires on it because consecutive entries never repeat an
	// action. At three rows per flap against maxActivityEntries (50), one flapping
	// contributor evicts the entire retained feed in under 20 minutes, which is what
	// #5090 measured as 19 joined / 19 left filling 38 of 50 slots.
	//
	// Retracting the trailing flap rows on the "joined" that closes the round trip is
	// what makes this correct rather than merely quieter: the pair is only collapsed
	// once the reconnect has PROVEN the contributor came back, so a genuine departure
	// — where no "joined" ever arrives — keeps every row exactly as today. That is the
	// property #5151 asks for ("expiry must fall through to exactly today's
	// behavior"), and it needs no timer, no deferred work, and no grace period during
	// which the hub is holding a decision it has not made.
	//
	// It touches ONLY the feed. The #2356 duplicate-PR guarantee is untouched and is
	// not this function's to weaken: the release cooldown is still booked eagerly by
	// the disconnect defer, and is withdrawn only by the lease-bound resume in
	// task_progress via clearReleaseCooldown (#5322) — which withdraws it because the
	// original owner has re-entered activeIssues, the stronger guard the cooldown was
	// standing in for. No window is ever open in which the issue is both out of
	// activeIssues and out of cooldown.
	if action == "joined" {
		h.absorbReconnectFlapLocked(username)
	}
	entry := ActivityEntry{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Username:      username,
		Action:        action,
		Role:          role,
		CLI:           cli,
		Model:         model,
		Effort:        effort,
		AdvisorModel:  adv.Model,
		AdvisorEffort: adv.Effort,
		Task:          task,
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

// reconnectFlapWindow is how recently a "left" must have been written for the
// following "joined" to count as the same contributor bouncing rather than a
// genuine departure followed later by a fresh arrival.
//
// It is sized against the relay's reconnect backoff, not against human behaviour:
// BASE_RECONNECT_DELAY_MS is 1s and MAX_RECONNECT_DELAY_MS is 60s, so a relay that
// is coming back does so inside a minute. Matching activityDebounceSecs keeps one
// notion of "the same session, still" in this file rather than two that can drift.
const reconnectFlapWindow = activityDebounceSecs * time.Second

// absorbReconnectFlapLocked retracts the trailing "left" — and the
// "released: connection lost" that may immediately precede it — written for this
// user by a disconnect that a reconnect has now undone (kubestellar/hive#5151).
//
// Called from addActivity with activityMu already held, immediately before a
// "joined" is appended. It walks back over at most the two rows one flap can
// write, requires them to belong to THIS user and to be inside
// reconnectFlapWindow, and stops at anything else. It therefore cannot reach past
// a flap into unrelated history, cannot collapse two different users' rows
// together, and cannot touch a "picked up" or "completed" — the rows an operator
// actually wants and that this churn was evicting.
//
// A departure with no reconnect behind it is never reached at all: this runs only
// on "joined". A departure whose reconnect arrives later than the window keeps its
// rows, because at that distance it is no longer a flap.
//
// The flap stays COUNTABLE. #5151 is explicit that absorbing must not become
// silence — the hub-side "[contribute-ws] disconnected" log line and the relay's
// describeWsClose output are untouched and unconditional (they are the #5107
// instrumentation and the real diagnostic surface), and absorbedReconnects
// increments here so "this contributor flapped N times" stays answerable more
// cheaply than by counting feed rows, which is what the issue asked for.
func (h *ContributeWSHub) absorbReconnectFlapLocked(username string) {
	if username == "" {
		return
	}
	end := len(h.activity)
	i := end
	sawLeft := false
	// At most three rows, which is everything one flap cycle can leave behind:
	// the "left", the "released: connection lost" that may precede it, and the
	// "joined" written by the PREVIOUS absorbed flap.
	//
	// That third row is what makes repeated flapping actually collapse. Each
	// absorbed flap leaves its own "joined" as the new trailing row, so on the next
	// flap the walk would stop at it and the feed would still grow by one row per
	// flap — 20 flaps leaving 20 "joined" rows, which is the same eviction #5151
	// reports, only quieter. Consuming the superseded "joined" makes a contributor
	// that flaps N times in a row occupy ONE row rather than N: the arrival that is
	// still true is the one about to be appended, and the earlier ones describe a
	// presence that never lapsed.
	//
	// Bounded explicitly rather than by a general scan so this can never chew
	// through the feed.
	for i > 0 && end-i < 3 {
		e := h.activity[i-1]
		if e.Username != username {
			break
		}
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || time.Since(t) >= reconnectFlapWindow {
			break
		}
		if e.Action == "left" && !sawLeft {
			sawLeft = true
			i--
			continue
		}
		if sawLeft && e.Action == "released: connection lost" {
			i--
			continue
		}
		// Only reachable once the left (and any released) above it have been
		// consumed, so this can only ever be the arrival that opened the session
		// this flap just closed — never an unrelated join.
		if sawLeft && e.Action == "joined" {
			i--
			continue
		}
		break
	}
	// Only collapse when a "left" was actually found. Without it there is no
	// departure to undo, and a bare "released: connection lost" must survive — it
	// describes work, not presence.
	if !sawLeft {
		return
	}
	h.activity = h.activity[:i]
	h.absorbedReconnects++
}

// AbsorbedReconnects returns how many contributor reconnects have been absorbed
// into the activity feed rather than booked as a departure plus an arrival
// (kubestellar/hive#5151). It is the cheap, non-evicting answer to "is a
// contributor flapping, and how much", which before this was answerable only by
// counting the feed rows the flapping was simultaneously evicting.
func (h *ContributeWSHub) AbsorbedReconnects() int {
	if h == nil {
		return 0
	}
	h.activityMu.RLock()
	defer h.activityMu.RUnlock()
	return h.absorbedReconnects
}

func (h *ContributeWSHub) RecentActivity() []ActivityEntry {
	h.activityMu.RLock()
	defer h.activityMu.RUnlock()
	out := make([]ActivityEntry, len(h.activity))
	copy(out, h.activity)
	return out
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
		Agent:         contributor.role,
		Backend:       contributor.cliBackend,
		Model:         ghpkg.RequestedModel(contributor.cliBackend, contributor.model),
		Effort:        contributor.reasoningEffort,
		AdvisorModel:  contributor.advisorModel,
		AdvisorEffort: contributor.advisorEffort,
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

// abandonReason is the synthetic reason text written to the run log for a task
// that ended with no terminal report. Free text like a client-reported reason,
// but hub-authored, and phrased to say what the hub OBSERVED rather than to
// guess why — the hub genuinely does not know whether the relay's CLI died,
// stalled, or simply decided to ask for different work.
func abandonReason(cause string) string {
	switch cause {
	case abandonCauseHandback:
		return "abandoned: relay asked for new work while still holding this task"
	case abandonCauseDisconnect:
		return "abandoned: connection lost with the task still held"
	default:
		return "abandoned: task ended without a terminal report"
	}
}

// appendAbandonedRun writes the run record for a task that ended without a
// task_complete or task_failed (#7317).
//
// DECLARE, never ROUTE — the same boundary task_run_log.go draws. Both callers
// have already done their routing (lease revoke, cooldown, activity rail) by
// the time they reach this; nothing here feeds back into any of it. It is
// called AFTER those so a telemetry problem can never affect them, and it is
// best-effort for the same reason: appendTaskRun swallows its own errors.
//
// The connection's own fields (backend, model, effort, role) are read WITHOUT
// contributor.mu. Both call sites reach this from the connection's own
// goroutine after releasing that lock, and these fields are set once at
// registration and not mutated afterwards — the same access the addActivity
// call immediately above each site already makes.
func (h *ContributeWSHub) appendAbandonedRun(c *ContributorConnection, task *WSTaskAssign, cause string, assignedAt time.Time) {
	if c == nil || c.profile == nil || task == nil {
		return
	}
	provider := ""
	if c.cliBackend == "pi" {
		provider, _, _ = strings.Cut(c.model, "/")
	}
	rec := TaskRunRecord{
		TaskID:        task.TaskID,
		Repo:          task.Repo,
		Number:        task.Number,
		Username:      c.profile.GitHubUsername,
		Backend:       c.cliBackend,
		Provider:      provider,
		Model:         c.model,
		Effort:        c.reasoningEffort,
		AdvisorModel:  c.advisorModel,
		AdvisorEffort: c.advisorEffort,
		Role:          c.role,
		Outcome:       outcomeAbandoned,
		AbandonCause:  cause,
		Reason:        abandonReason(cause),
	}
	// Zero when the task was adopted on the resume path without a fresh
	// assignment (see taskAssignedAt's comment). Left unset rather than
	// reported as a 0-second run, which would read as an instant hand-back —
	// the very thing an operator is trying to tell apart from a 26-minute
	// stall.
	if !assignedAt.IsZero() {
		rec.DurationS = time.Since(assignedAt).Seconds()
	}
	// #7317 item 3: the last pane the relay reported before it gave the task
	// back or dropped off — for a stall this is the frozen screen itself, the
	// thing the operator most wants to see. Only a pane reported FOR this task
	// qualifies (#7605): a socket that drops two seconds into a fresh
	// assignment has usually seen no progress frame yet, and the pane on hand
	// is the previous task's task_complete screen — a clean finish that would
	// send the operator hunting for a dropped completion instead of a flapped
	// socket. With no pane of its own the row simply carries none. Unlike the
	// fields above, tmuxOutput IS written under contributor.mu (by the
	// task_progress handler), so the copy takes the lock; both callers have
	// released it by the time they get here.
	c.mu.Lock()
	rec.PaneTail = c.paneTailFor(task.TaskID)
	c.mu.Unlock()
	h.appendTaskRun(rec)
}

// paneTailFor returns a bounded, redacted copy of the relay's last pane
// snapshot if the relay reported it for taskID, and nil when the snapshot
// belongs to another task (or there is none) — see tmuxOutputTask. The
// caller holds c.mu.
func (c *ContributorConnection) paneTailFor(taskID string) []string {
	if taskID == "" || c.tmuxOutputTask != taskID {
		return nil
	}
	return boundPaneTail(c.tmuxOutput)
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
			closeWithReason(c.ws, websocket.ClosePolicyViolation, reason)
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
// everFenced ratchets the legacy escape (kubestellar/hive#6909). The clientGen==0
// fallback exists for unversioned relays that never learned a generation, but it
// made the Gate opt-in: ANY client could bypass fencing on every call site just by
// omitting task_gen (an absent JSON field decodes to 0, the value that disables the
// check). Once a connection has echoed a real generation it has proven it speaks the
// fenced protocol, so it may not silently downgrade itself back to unstamped for the
// rest of the session — which is the actual bug/attack shape. A true legacy client
// never sets the flag and is unaffected.
func generationAccepted(clientGen, currentGen uint64, everFenced bool) bool {
	if clientGen == 0 {
		return !everFenced
	}
	return clientGen == currentGen
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

const maxWSConnections = 50

func (h *ContributeWSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	// Counted from the first instruction so a drain (h.handlers.Wait) covers
	// every deferred write this handler can make — see the field comment.
	h.handlers.Add(1)
	defer h.handlers.Done()
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
	s := &wsSession{h: h}
	defer func() {
		if !s.pendingReleased {
			h.pendingConns.Add(-1)
		}
	}()

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("ws upgrade failed", "error", err)
		return
	}
	conn.SetReadLimit(wsMaxMessageSize)
	s.conn = conn

	connID := randomHex(8)
	s.connID = connID
	h.logger.Info("[contribute-ws] new connection", "id", connID)

	nonce := randomHex(16)
	s.nonce = nonce
	if err := sendJSON(conn, WSMessage{Type: "auth_challenge", Seq: 1, Nonce: nonce}); err != nil {
		h.logger.Warn("[contribute-ws] failed to send challenge", "id", connID, "error", err)
		return
	}

	authDone := make(chan *ContributorConnection, 1)
	s.authDone = authDone
	go func() {
		select {
		case <-time.After(wsAuthTimeout):
			_ = sendJSON(conn, WSMessage{Type: "auth_failed", Reason: "Authentication timeout"})
			closeWithReason(conn, websocket.ClosePolicyViolation, "authentication timeout")
		case <-authDone:
		}
	}()

	defer s.releaseOnDisconnect()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// #7932: an oversized client frame arrives here as ErrReadLimit, and
			// gorilla has already sent the peer a 1009 close. It is not a
			// *CloseError, so IsUnexpectedCloseError below does not match it and
			// the hub used to drop the connection with NOTHING in its log while
			// the relay logged "code=1009 message too big" and reconnected into
			// the same loop. Name the bound here so both halves of that story can
			// be read side by side.
			if errors.Is(err, websocket.ErrReadLimit) {
				h.logger.Warn("[contribute-ws] contributor frame exceeded the read limit; connection closed",
					"id", connID, "limit_bytes", wsMaxMessageSize)
				return
			}
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				code, reason, source := websocketCloseErrorDetails(err)
				h.logger.Warn("[contribute-ws] read error",
					"id", connID,
					"username", contributorUsername(s.contributor),
					"close_source", source,
					"close_code", code,
					"close_reason", reason,
					"error", err)
			}
			return
		}

		var msg WSMessage
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "auth_response":
			if s.handleAuthResponse(msg) {
				return
			}
		case "ready":
			if s.handleReady(msg) {
				return
			}
		case "task_accepted":
			s.handleTaskAccepted(msg)
		case "task_declined":
			s.handleTaskDeclined(msg)
		case "task_progress":
			s.handleTaskProgress(msg)
		case "task_complete":
			s.handleTaskComplete(msg)
		case "task_failed":
			s.handleTaskFailed(msg)
		case "pong":
			if s.contributor != nil {
				s.contributor.mu.Lock()
				s.contributor.lastPong = time.Now()
				s.contributor.mu.Unlock()
			}

		case "ping":
			// Once registered, this reply shares the connection with the heartbeat
			// loop and the operator paths, so it MUST take the write lock; a ping that
			// somehow arrives pre-registration has no ContributorConnection and is
			// still single-writer on the bare conn.
			if s.contributor != nil {
				_ = s.contributor.send(WSMessage{Type: "pong", Seq: msg.Seq})
			} else {
				_ = sendJSON(s.conn, WSMessage{Type: "pong", Seq: msg.Seq})
			}
		}
	}
}

// wsSession is the per-socket state HandleWS threads through its phases: the
// upgraded connection, its identifiers, the auth handshake channel, the
// contributor once authenticated, and whether the pending-connection slot has
// already been handed over to h.connections (#7546).
type wsSession struct {
	h               *ContributeWSHub
	conn            *websocket.Conn
	connID          string
	nonce           string
	authDone        chan *ContributorConnection
	contributor     *ContributorConnection
	pendingReleased bool
}

// releaseOnDisconnect is the teardown phase of HandleWS: it runs exactly once
// when the read loop exits, releases any task this socket still holds (unless a
// live reconnect has already re-adopted it, #5322), records the abandonment, and
// closes the socket.
func (s *wsSession) releaseOnDisconnect() {
	h := s.h
	if s.contributor != nil && s.contributor.profile != nil {
		s.contributor.mu.Lock()
		abandonedTask := s.contributor.currentTask
		// #7317: see the `ready` path — captured under the same lock so the
		// run record can carry how long the task was held before the socket
		// died.
		abandonedTaskAt := s.contributor.taskAssignedAt
		s.contributor.currentTask = nil
		// #2568: bump the generation on release so any late message from this
		// now-defunct socket carrying the old generation is fenced.
		s.contributor.currentTaskGen = h.nextTaskGen()
		s.contributor.lastLeaseRenew = time.Time{}
		s.contributor.tokenMintedAt = time.Time{}
		// #2537: clear any pending/delivered credential state with the task.
		s.contributor.pendingToken = ""
		s.contributor.credentialDelivered = false
		s.contributor.mu.Unlock()

		// #5322: deregister THIS socket before deciding whether its task is
		// really abandoned. The check below asks "is some OTHER live
		// connection for this identity already holding this task?", and the
		// answer must not be able to include the connection being torn down.
		// Moved up from the tail of this defer for exactly that reason; it is
		// the same single delete of the same key, just ordered ahead of the
		// release so the two cannot observe each other.
		h.mu.Lock()
		delete(h.connections, s.connID)
		h.mu.Unlock()

		// #5322: a socket that dies WITHOUT a close frame (an L7 proxy cutting
		// the tunnel — the 1006 flap #5090/#5310 measured) leaves this read
		// loop parked in ReadMessage, so this defer does not run when the
		// socket dies; it runs whenever the next read finally errors. The
		// relay meanwhile redials in ~1s and re-asserts its task over a NEW
		// connection, which the lease-bound resume in task_progress legitimately
		// adopts. h.connections is keyed by a random per-socket connID and the
		// hub has no notion of "this contributor's current socket", so when this
		// defer eventually fires it releases BY ISSUE a task that a live
		// connection is demonstrably still working: it books a release cooldown
		// on an in-flight issue and writes "released: connection lost" for work
		// nobody released. That is the silent drop — the hub's own record of the
		// assignment contradicted by the ghost of a socket that no longer
		// represents the contributor.
		//
		// So: release only what is still ours to release. If another LIVE
		// connection for this same identity already holds this exact task, the
		// reconnect has already reconciled and this socket is a ghost — skip the
		// release entirely. This changes nothing about a genuine departure (no
		// other connection holds the task, so the release runs exactly as
		// before, booking the same cooldown and writing the same rows —
		// deliberately leaving kubestellar/hive#5151's accounting untouched).
		if abandonedTask != nil && h.taskReadoptedByLiveConnection(s.contributor, abandonedTask) {
			h.logger.Info("[contribute-ws] disconnect release skipped: task already re-adopted on a live connection",
				"id", s.connID,
				"username", s.contributor.profile.GitHubUsername,
				"task", abandonedTask.TaskID,
				"repo", abandonedTask.Repo,
				"number", abandonedTask.Number,
			)
			abandonedTask = nil
		}

		if abandonedTask != nil {
			h.logger.Warn("[contribute-ws] task released on disconnect",
				"id", s.connID,
				"username", s.contributor.profile.GitHubUsername,
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
			//
			// #7770: booked against the task's canonical identity rather than
			// repo#number, so a Linear/Jira item — Number 0, identity in Key —
			// gets the same reconnect-window hedge a GitHub issue does instead
			// of none. For a GitHub issue the key IS "repo#number", byte for
			// byte, so nothing changes there; a synthetic pr-review task has
			// no identity and is skipped by the helper, which is what the old
			// Number > 0 guard was for.
			h.bookReleaseCooldownKey(abandonedTask.identityKey())
			// #7838: the VISIBLE booking — the activity row and the run-log
			// row — waits out a grace window first. The #5322 check above is
			// zero-width for a 1006: the hub processes the dead socket the
			// instant its read fails, while the relay does not even start
			// redialing for BASE_RECONNECT_DELAY_MS, so on a real blip the
			// release always won the race and every blip wrote an
			// `abandoned_disconnect` row for a task that resumed one second
			// later. The cooldown above is booked NOW regardless — it is the
			// #2356 double-assign hedge and must cover the window — but the
			// ledger waits: if a live connection re-adopts the task, or the
			// task finishes, before the deadline, nothing is written.
			h.bookAbandonmentAfterGrace(s.contributor, abandonedTask, abandonedTaskAt)
		}
		h.logger.Info("[contribute-ws] disconnected", "id", s.connID, "username", s.contributor.profile.GitHubUsername)
		h.addActivity(s.contributor.profile.GitHubUsername, "left", s.contributor.role, s.contributor.cliBackend, s.contributor.model, s.contributor.reasoningEffort, "", s.contributor.advisor())
	}
	_ = s.conn.Close()
}

// disconnectAbandonGrace is how long a disconnect release waits before booking
// the abandonment visibly (#7838). It must outlast the relay's first reconnect
// attempt — BASE_RECONNECT_DELAY_MS (1 s) plus dial, TLS, auth and the resume
// task_progress — with margin for a second blip in the same flap, as observed
// live. Overridable via HIVE_CONTRIBUTE_DISCONNECT_GRACE (a Go duration; "0"
// restores the immediate booking).
const (
	defaultDisconnectAbandonGrace = 5 * time.Second
	disconnectAbandonGraceEnv     = "HIVE_CONTRIBUTE_DISCONNECT_GRACE"
	// recentlyFinishedTTL bounds the #7838 finished-task memory. It only has to
	// cover one grace window; a minute is generous and keeps the map tiny.
	recentlyFinishedTTL = time.Minute
)

// disconnectAbandonGraceNanos holds the grace window; atomic so tests can
// shrink it while a disconnect from a previous connection is still being
// processed on another goroutine. Read via disconnectAbandonGrace().
var disconnectAbandonGraceNanos = func() *atomic.Int64 {
	var v atomic.Int64
	d := defaultDisconnectAbandonGrace
	if raw := os.Getenv(disconnectAbandonGraceEnv); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 0 {
			d = parsed
		}
	}
	v.Store(int64(d))
	return &v
}()

func disconnectAbandonGrace() time.Duration {
	return time.Duration(disconnectAbandonGraceNanos.Load())
}

// bookAbandonmentAfterGrace writes the activity row and the run-log row for a
// task released on disconnect (#5097/#7317), after disconnectAbandonGrace has
// passed without the task being re-adopted on a live connection (#5322) or
// finished (#7838). With a zero grace it books synchronously, exactly as the
// pre-#7838 path did.
//
// Deliberately NOT the "failed" verb: #4260 established that a dropped socket
// is not a failure of the work, and booking it as one is what turned three
// dropped sockets into a quarantine of an issue nobody had failed. This is a
// release, and it says so.
func (h *ContributeWSHub) bookAbandonmentAfterGrace(c *ContributorConnection, task *WSTaskAssign, assignedAt time.Time) {
	if h == nil || c == nil || c.profile == nil || task == nil {
		return
	}
	book := func() {
		// A hub shutting down inside the window does not need the row.
		select {
		case <-h.stopCh:
			return
		default:
		}
		if h.taskReadoptedByLiveConnection(c, task) {
			h.logger.Info("[contribute-ws] disconnect release withdrawn: task re-adopted on a live connection within the grace window (#7838)",
				"username", c.profile.GitHubUsername, "task", task.TaskID, "repo", task.Repo, "number", task.Number)
			return
		}
		if h.taskFinishedRecently(task.TaskID) {
			h.logger.Info("[contribute-ws] disconnect release withdrawn: task finished within the grace window (#7838)",
				"username", c.profile.GitHubUsername, "task", task.TaskID, "repo", task.Repo, "number", task.Number)
			return
		}
		h.addActivity(c.profile.GitHubUsername, "released: connection lost",
			c.role, c.cliBackend, c.model, c.reasoningEffort, taskDescOf(task))
		h.appendAbandonedRun(c, task, abandonCauseDisconnect, assignedAt)
	}
	grace := disconnectAbandonGrace()
	if grace <= 0 {
		book()
		return
	}
	// Tracked so shutdown (and tests that swap the run-log path) can wait for
	// an in-flight booking instead of racing it.
	h.graceBookings.Add(1)
	time.AfterFunc(grace, func() {
		defer h.graceBookings.Done()
		book()
	})
}

// noteTaskFinished records that a completed/failed run row was written for a
// task id (#7838). Called from appendTaskRun; prunes stale entries as it goes.
func (h *ContributeWSHub) noteTaskFinished(taskID string, at time.Time) {
	if h == nil || taskID == "" {
		return
	}
	h.finishedMu.Lock()
	defer h.finishedMu.Unlock()
	if h.recentlyFinished == nil {
		h.recentlyFinished = make(map[string]time.Time)
	}
	for id, t := range h.recentlyFinished {
		if at.Sub(t) > recentlyFinishedTTL {
			delete(h.recentlyFinished, id)
		}
	}
	h.recentlyFinished[taskID] = at
}

// taskFinishedRecently reports whether a completed/failed run row was written
// for the task id inside recentlyFinishedTTL.
func (h *ContributeWSHub) taskFinishedRecently(taskID string) bool {
	if h == nil || taskID == "" {
		return false
	}
	h.finishedMu.Lock()
	defer h.finishedMu.Unlock()
	t, ok := h.recentlyFinished[taskID]
	return ok && time.Since(t) <= recentlyFinishedTTL
}

// handleAuthResponse is the handshake phase: it verifies the registration token
// and challenge signature, admits or refuses the contributor, and registers the
// connection. It reports stop=true when the socket must close.
func (s *wsSession) handleAuthResponse(msg WSMessage) (stop bool) {
	h := s.h
	if msg.RegistrationToken == "" {
		_ = sendJSON(s.conn, WSMessage{Type: "auth_failed", Reason: "Missing registration token"})
		closeWithReason(s.conn, websocket.ClosePolicyViolation, "missing registration token")
		return true
	}

	profile := contributorProfileFromRegistrationToken(msg.RegistrationToken)

	if profile == nil {
		_ = sendJSON(s.conn, WSMessage{Type: "auth_failed", Reason: "Invalid registration token"})
		closeWithReason(s.conn, websocket.ClosePolicyViolation, "invalid registration token")
		return true
	}

	if profile.TrustTier == "revoked" {
		_ = sendJSON(s.conn, WSMessage{Type: "auth_failed", Reason: "Access has been revoked"})
		closeWithReason(s.conn, websocket.ClosePolicyViolation, "access has been revoked")
		return true
	}

	if allowed, acceptedModels := h.checkModelAllowed(msg.Model); !allowed {
		reason := fmt.Sprintf("Model %q is not accepted by this hive", msg.Model)
		if msg.Model == "" {
			reason = "No model specified — this hive requires an accepted model"
		}
		_ = sendJSON(s.conn, WSMessage{Type: "auth_failed", Reason: reason, AcceptedModels: acceptedModels})
		h.logger.Info("[contribute-ws] model rejected", "username", profile.GitHubUsername, "model", msg.Model)
		closeWithReason(s.conn, websocket.ClosePolicyViolation, "model not accepted by this hive")
		return true
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
			_ = sendJSON(s.conn, WSMessage{Type: "auth_failed", Reason: reason, Role: requestedRole})
			h.logger.Warn("[contribute-ws] agent role claim rejected",
				"username", profile.GitHubUsername, "tier", profile.TrustTier,
				"role", requestedRole, "reason", reason)
			closeWithReason(s.conn, websocket.ClosePolicyViolation, "agent role claim rejected")
			return true
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
	// #7760: the advisor pair is client text. It is re-serialized into every
	// fleet poll and lands in PR trailers, so it is HTML-stripped like every
	// other stored contributor string AND bounded the way the declared
	// capabilities are (sanitizeCapabilityField: control characters and
	// newlines collapsed, 64 runes) — a newline here would otherwise start a
	// new line inside the `— hive:` trailer. Unlike the primary it is NOT
	// checked against the accepted-models list: it reviewed the work, it did
	// not do it.
	advisorModel := sanitizeCapabilityField(sanitizeString(msg.AdvisorModel))
	advisorEffort := sanitizeCapabilityField(sanitizeString(msg.AdvisorReasoningEffort))
	if advisorModel != "" {
		profile.AdvisorModel = advisorModel
		profile.AdvisorEffort = advisorEffort
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

	s.contributor = &ContributorConnection{
		ws:              s.conn,
		connID:          s.connID,
		profile:         profile,
		cliBackend:      msg.CLIBackend,
		session:         sanitizeSessionLabel(msg.Session),
		model:           msg.Model,
		reasoningEffort: msg.ReasoningEffort,
		advisorModel:    advisorModel,
		advisorEffort:   advisorEffort,
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
	h.connections[s.connID] = s.contributor
	h.mu.Unlock()
	if !s.pendingReleased {
		s.pendingReleased = true
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

	if err := s.contributor.send(WSMessage{
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
		ConnectionID:       s.connID,
		// #7932: state the read limit rather than enforcing it silently.
		MaxMessageBytes: wsMaxMessageSize,
	}); err != nil {
		h.logger.Warn("[contribute-ws] failed to send auth_ok", "username", profile.GitHubUsername, "error", err)
		return true
	}

	h.logger.Info("[contribute-ws] authenticated",
		"id", s.connID,
		"username", profile.GitHubUsername,
		"tier", profile.TrustTier,
		"cli", msg.CLIBackend,
		"role", requestedRole,
	)
	h.addActivity(profile.GitHubUsername, "joined", requestedRole, msg.CLIBackend, msg.Model, msg.ReasoningEffort, "", advisorInfo{Model: advisorModel, Effort: advisorEffort})

	select {
	case s.authDone <- s.contributor:
	default:
	}

	// Count a PROTOCOL-level Pong as liveness, exactly as the JSON
	// "pong" case below does (kubestellar/hive#5090). Now that the hub
	// emits real Ping control frames, a relay that answers only those —
	// which is what any conforming WebSocket client does automatically,
	// with no relay code at all — must not be false-timed-out by the
	// heartbeat sweep. gorilla invokes this handler from ReadMessage on
	// the read goroutine, which holds neither mu nor writeMu here, so
	// taking mu introduces no re-entrancy.
	s.contributor.ws.SetPongHandler(func(string) error {
		s.contributor.mu.Lock()
		s.contributor.lastPong = time.Now()
		s.contributor.mu.Unlock()
		return nil
	})

	go h.heartbeatLoop(s.contributor)

	return false
}

// handleReady is the dispatch phase: a contributor with no task asks for work
// and receives a selected task (or a no-work notice). stop=true closes the socket.
func (s *wsSession) handleReady(msg WSMessage) (stop bool) {
	h := s.h
	if s.contributor == nil {
		return false
	}
	s.contributor.mu.Lock()
	abandoned := s.contributor.currentTask
	// #7317: captured under the same lock as currentTask so the run record
	// below can report the task's real wall-clock duration. On the session
	// that prompted the issue these land on the relay's own timeouts —
	// ~26 min is PANE_STALL_TIMEOUT_MS, ~10 min is CLI_READY_TIMEOUT_MS —
	// which is the most diagnostic number in the record, and it was being
	// discarded along with the rest of the abandonment.
	abandonedAt := s.contributor.taskAssignedAt
	s.contributor.currentTask = nil
	// #2568: bump the generation on release so a re-`ready` abandon fences any
	// later message echoing the old generation for the just-abandoned task.
	s.contributor.currentTaskGen = h.nextTaskGen()
	s.contributor.lastLeaseRenew = time.Time{}
	s.contributor.tokenMintedAt = time.Time{}
	// #2537: clear any pending/delivered credential state with the task.
	s.contributor.pendingToken = ""
	s.contributor.credentialDelivered = false
	s.contributor.mu.Unlock()
	if abandoned != nil {
		// C4: the relay explicitly gave up this task, so revoke its
		// server-issued lease — a later task_progress for it must not resurrect
		// ownership.
		h.revokeLease(identityOf(s.contributor), abandoned.TaskID)
		// #5097: same visibility gap as the disconnect path above — the
		// relay giving a task back by asking for new work left no trace in
		// the activity feed either.
		h.addActivity(s.contributor.profile.GitHubUsername, "released: gave the task back",
			s.contributor.role, s.contributor.cliBackend, s.contributor.model,
			s.contributor.reasoningEffort, taskDescOf(abandoned))
		h.logger.Warn("[contribute-ws] task abandoned without completion",
			"username", s.contributor.profile.GitHubUsername,
			"abandoned_task", abandoned.TaskID,
		)
		h.recordTaskDecision(s.contributor.profile.GitHubUsername, decisionAbandoned, abandoned,
			"relay asked for new work while still holding this task")
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
		// #7317: and leave a durable trace. Everything above this line is
		// about ROUTING the abandoned issue (lease, cooldown, activity rail);
		// none of it survives for an operator to read later. The run log is
		// the only per-run record that does, and this path never wrote one —
		// so a contributor that handed eleven tasks back in two hours showed
		// a single row, and run-stats reported one failure for the session.
		h.appendAbandonedRun(s.contributor, abandoned, abandonCauseHandback, abandonedAt)
	}
	h.logger.Info("[contribute-ws] ready for work",
		"username", s.contributor.profile.GitHubUsername,
		"role", s.contributor.role,
	)
	task := h.selectTask(s.contributor)
	switch {
	case task == nil:
		// Reached when the claim selectTask committed was released while its
		// GitHub round-trips were still in flight — the socket dropped mid-mint
		// and the disconnect path cleared it (#7775). There is nothing to send.
		// Every other selectTask path returns an explicit message (#2436,
		// #2546); an unforeseen nil still fails safe (no send) rather than
		// panicking.
		h.logger.Info("[contribute-ws] no task to send",
			"username", s.contributor.profile.GitHubUsername,
		)
	case task.Type == "task_unavailable":
		// An explicit negative-ack rather than silence. #2436 finding 1/2/3
		// covers the enforced refusals (mint failure, disabled tier,
		// concurrency limit); #2546 adds the three formerly-silent
		// no-work-right-now reasons (contribution_suspended, hub_not_ready,
		// no_matching_work). Record the reason on the connection so the ops
		// tab can show WHY this clanker is idle, then send it.
		s.contributor.mu.Lock()
		s.contributor.lastIdleReason = task.Reason
		s.contributor.mu.Unlock()
		if err := s.contributor.send(*task); err != nil {
			h.logger.Warn("[contribute-ws] failed to send task_unavailable", "error", err)
			return true
		}
		h.logger.Info("[contribute-ws] task unavailable",
			"username", s.contributor.profile.GitHubUsername,
			"reason", task.Reason,
		)
	default:
		if err := s.contributor.send(*task); err != nil {
			// #7775: the socket is already gone — typically closed by the
			// heartbeat loop while this ready waited its turn. Undo the claim so
			// the disconnect path finds nothing to release: no release cooldown
			// on an issue the contributor never received, no lease left to
			// expire, no rate-window slot spent on a task that never shipped.
			h.logger.Warn("[contribute-ws] failed to send task_assign; releasing the undelivered claim", "error", err, "task", task.TaskID)
			h.rollbackAssignment(s.contributor, task.TaskID)
			return true
		}
		pickupKey := task.TaskKey
		if pickupKey == "" {
			pickupKey = worksource.Ref{Repo: task.Repo, Number: task.Number}.Key()
		}
		taskDesc := assignDesc(task.Kind, pickupKey, task.Title, task.TaskID)
		if task.Role != "" {
			taskDesc = fmt.Sprintf("contributor ran %s task: %s", task.Role, taskDesc)
		}
		h.addActivity(s.contributor.profile.GitHubUsername, "picked up", s.contributor.role, s.contributor.cliBackend, s.contributor.model, s.contributor.reasoningEffort, taskDesc, s.contributor.advisor())
		h.logger.Info("[contribute-ws] task assigned",
			"username", s.contributor.profile.GitHubUsername,
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
		if !h.requireExplicitAccept() && !contributorSupportsQuotaPreflight(s.contributor) {
			h.deliverTaskCredential(s.contributor, "auto_accept")
		} else {
			h.logger.Info("[contribute-ws] credential withheld pending explicit acceptance",
				"username", s.contributor.profile.GitHubUsername, "task", task.TaskID)
		}
	}

	return false
}

// handleTaskAccepted records that the contributor acknowledged its assignment.
func (s *wsSession) handleTaskAccepted(msg WSMessage) {
	h := s.h
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
	if s.contributor != nil {
		h.acceptTaskCredential(s.contributor, msg.TaskID)
	}

}

// handleTaskDeclined handles #6833: contributor-local quota preflight may
// decline an offered task after metadata but before the scoped credential is
// delivered. This is local capacity, not a task failure: release the lease
// and connection state without recording failure cooldown or trust impact.
func (s *wsSession) handleTaskDeclined(msg WSMessage) {
	h := s.h
	contributor := s.contributor
	if contributor == nil {
		return
	}
	contributor.mu.Lock()
	if contributor.currentTask != nil &&
		contributor.currentTask.TaskID == msg.TaskID &&
		msg.TaskGen != 0 &&
		msg.TaskGen == contributor.currentTaskGen &&
		!contributor.credentialDelivered &&
		contributor.pendingToken != "" {
		declined := contributor.currentTask
		contributor.currentTask = nil
		contributor.currentTaskGen = h.nextTaskGen()
		contributor.lastLeaseRenew = time.Time{}
		contributor.taskAssignedAt = time.Time{}
		contributor.currentPrompt = ""
		contributor.currentLabels = nil
		contributor.pendingToken = ""
		contributor.credentialDelivered = false
		contributor.tokenMintedAt = time.Time{}
		contributor.mu.Unlock()
		h.revokeLease(identityOf(contributor), msg.TaskID)
		h.logger.Info("[contribute-ws] task declined by contributor preflight",
			"username", contributor.profile.GitHubUsername,
			"task", msg.TaskID,
			"reason", msg.Reason,
		)
		if declined != nil && declined.Number > 0 {
			h.clearReleaseCooldown(declined.Repo, declined.Number)
		}
		return
	}
	h.logger.Warn("[contribute-ws] ignoring non-preflight task_declined",
		"username", contributor.profile.GitHubUsername,
		"task", msg.TaskID,
		"client_gen", msg.TaskGen,
	)
	contributor.mu.Unlock()
}

// handleTaskProgress is the lease phase: progress reports renew the lease and
// let a reconnecting relay re-assert the task it already holds.
func (s *wsSession) handleTaskProgress(msg WSMessage) {
	h := s.h
	if s.contributor != nil {
		s.contributor.mu.Lock()
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
		if s.contributor.currentTask == nil {
			if msg.TaskID == "" {
				s.contributor.mu.Unlock()
				return
			}
			identity := identityOf(s.contributor)
			canonRepo := h.canonicalRepoKey(msg.Repo)
			s.contributor.mu.Unlock()

			lease := h.lookupLease(identity, msg.TaskID, canonRepo, msg.Number, msg.TaskGen, time.Now())
			if lease == nil {
				h.logger.Warn("[contribute-ws] task_progress resume rejected: no matching server-issued lease",
					"username", s.contributor.profile.GitHubUsername,
					"task", msg.TaskID,
					"repo", canonRepo,
					"client_gen", msg.TaskGen,
				)
				h.recordDecision(s.contributor.profile.GitHubUsername, decisionResumeRejected,
					msg.TaskID, canonRepo, msg.Number,
					"no matching server-issued lease; task_revoke sent")
				// Tell the relay this task is not (or no longer) its to hold, so
				// it stops reporting and re-asks for work rather than silently
				// believing it owns something the hub has no record of.
				_ = sendJSON(s.conn, WSMessage{Type: "task_revoke", Seq: h.nextSeq(), TaskID: msg.TaskID, Reason: "no active lease for this task"})
				return
			}
			// C4: re-run the same admission gates a fresh selectTask assignment
			// must pass. A task assigned before the operator suspended the queue,
			// disabled the tier, or revoked the contributor must NOT silently
			// resume (and re-mint a credential) after the gate closed.
			if reason := h.resumeGateReason(s.contributor); reason != "" {
				h.recordDecision(s.contributor.profile.GitHubUsername, decisionResumeRejected,
					msg.TaskID, canonRepo, msg.Number,
					"refused by the admission gate: "+reason)
				h.logger.Warn("[contribute-ws] task_progress resume refused by admission gate",
					"username", s.contributor.profile.GitHubUsername,
					"task", msg.TaskID,
					"reason", reason,
				)
				h.revokeLease(identity, msg.TaskID)
				_ = sendJSON(s.conn, WSMessage{Type: "task_revoke", Seq: h.nextSeq(), TaskID: msg.TaskID, Reason: reason})
				return
			}
			// Adopt the task from the AUTHORITATIVE lease record, not the client
			// fields: repo/number/tier are the server's, and the task keeps its
			// ORIGINAL generation so it stays fenced against any older-generation
			// straggler. lastLeaseRenew starts the wedged-task clock.
			//
			// The lease's canonical key comes along too (hivecommons/hive#7770).
			// Every guard that stops two contributors working one item keys on
			// identityKey() — the activeIssues scan in selectTask, the completion
			// and failure cooldowns — and identityKey() falls back to "repo#number"
			// only when Key is empty. For a Linear/Jira item Number is 0 and that
			// fallback is "", so a rebuild that copied repo and number alone left
			// a live connection working `acme/repo!ENG-123` with no identity at
			// all: the item dropped out of the double-assignment guard the moment
			// the old socket aged out, and finishing it booked a cooldown against
			// "". GitHub items were untouched only because Number > 0 recovers
			// their key. The lease has carried the key since #4245 (#5120); this
			// is the one consumer that never read it. ExternalID is recovered from
			// the same key so the task's display stays the native key rather than
			// "#0"; SourceType is not in the lease and a client-declared value is
			// not adopted here, exactly as repo and number are not.
			rebuilt := &WSTaskAssign{
				TaskID: lease.taskID,
				Kind:   msg.Kind,
				Repo:   lease.repo,
				Number: lease.number,
				Key:    lease.key,
				Title:  msg.Title,
			}
			if lease.number == 0 {
				if ref, ok := worksource.ParseKey(lease.key); ok {
					rebuilt.ExternalID = ref.ExternalID
				}
			}
			s.contributor.mu.Lock()
			s.contributor.currentTask = rebuilt
			s.contributor.currentTaskGen = lease.gen
			s.contributor.lastLeaseRenew = time.Now()
			s.contributor.tmuxOutput = msg.TmuxOutput
			s.contributor.tmuxOutputTask = lease.taskID
			s.contributor.mu.Unlock()

			// #4260: a resume is itself proof of life, so restart the lease
			// window alongside lastLeaseRenew. Without this a relay that
			// reconnected twice inside one lease window would be refused the
			// second time even though it never stopped working. A persist
			// failure is logged only (#8287): the in-memory window is kept, a
			// failed renew persist must not revoke a live task.
			if err := h.renewLease(identity, lease.taskID, time.Now()); err != nil {
				h.logger.Warn("[contribute-ws] resumed lease renewed in memory but not persisted",
					"username", identity, "task", lease.taskID, "error", err)
			}

			// #5322: the disconnect that preceded this resume booked a
			// speculative release cooldown on the issue (#2356's
			// duplicate-assign hedge). This resume proves the release never
			// happened — the original relay is back, on the original task,
			// under the original generation — so withdraw the hedge rather
			// than leave a live, in-flight issue stamped "recently released"
			// in the failure ledger for the rest of the window. Narrow by
			// construction: clearReleaseCooldown refuses to touch an issue
			// that carries a real consecutive-failure count. Keyed on the
			// task's identity (#7770), so the hedge booked for an external
			// item on disconnect is the one withdrawn here; the key is "" for
			// a synthetic task and the helper skips it.
			h.clearReleaseCooldownKey(rebuilt.identityKey())

			h.logger.Info("[contribute-ws] task resumed from server-issued lease",
				"username", s.contributor.profile.GitHubUsername,
				"task", lease.taskID, "repo", lease.repo, "number", lease.number)

			// #2610 finding 3: re-mint and push a fresh token_refresh so the
			// resumed session holds a valid token and re-arms the #2393 refresh
			// cycle. The credential is minted for the LEASE's tier (server-owned),
			// repository-scoped to the lease's repo (C4).
			h.resumeTaskToken(s.contributor, lease)
			return
		}

		// A routine progress ping for a task the hub already tracks on THIS
		// connection. #2568 (the Gate): reject a STALE generation — a worker
		// whose task was revoked/reassigned (currentTaskGen bumped past what it
		// echoes) must not renew a lease it no longer owns. An unversioned relay
		// echoes 0 and is accepted (generationAccepted falls back to TaskID).
		if msg.TaskGen != 0 {
			s.contributor.sawTaskGen = true
		}
		if !generationAccepted(msg.TaskGen, s.contributor.currentTaskGen, s.contributor.sawTaskGen) {
			staleGen := msg.TaskGen
			s.contributor.mu.Unlock()
			h.logger.Warn("[contribute-ws] stale-generation task_progress rejected",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
				"client_gen", staleGen,
			)
			h.recordDecision(s.contributor.profile.GitHubUsername, decisionStaleGenRejected,
				msg.TaskID, "", 0,
				"task_progress fenced: client_gen "+strconv.FormatUint(staleGen, 10)+" no longer matches the assignment")
			return
		}
		s.contributor.tmuxOutput = msg.TmuxOutput
		s.contributor.tmuxOutputTask = msg.TaskID
		// #4117: the relay re-detects the running model from the CLI's own
		// session transcript on every progress tick and piggybacks it here, so
		// a mid-session model switch (claude `/model`) is reflected instead of
		// staying stuck at the connect-time value. Same pattern as the
		// auth-time handler: only non-empty values overwrite — an older relay
		// omits both fields and nothing changes. Advisory display metadata,
		// exactly like the auth_response values it refreshes.
		if msg.Model != "" {
			s.contributor.model = msg.Model
			if s.contributor.profile != nil {
				s.contributor.profile.Model = msg.Model
			}
		}
		if msg.ReasoningEffort != "" {
			s.contributor.reasoningEffort = msg.ReasoningEffort
			if s.contributor.profile != nil {
				s.contributor.profile.ReasoningEffort = msg.ReasoningEffort
			}
		}
		// #7760: the advisor pair refreshes on the same schedule and rule as
		// the model above — the relay re-reads omp's own records every tick.
		if adv := sanitizeCapabilityField(sanitizeString(msg.AdvisorModel)); adv != "" {
			s.contributor.advisorModel = adv
			s.contributor.advisorEffort = sanitizeCapabilityField(sanitizeString(msg.AdvisorReasoningEffort))
			if s.contributor.profile != nil {
				s.contributor.profile.AdvisorModel = s.contributor.advisorModel
				s.contributor.profile.AdvisorEffort = s.contributor.advisorEffort
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
		s.contributor.lastLeaseRenew = time.Now()
		// #4260: the task id is taken from the hub's OWN record of what this
		// connection holds, never from msg.TaskID, so the renewal cannot be
		// pointed at a task the client merely names. Captured here under the
		// connection lock and applied below without it, matching how the other
		// release paths call into the lease registry.
		heldTaskID := ""
		if s.contributor.currentTask != nil {
			heldTaskID = s.contributor.currentTask.TaskID
		}
		s.contributor.mu.Unlock()
		// #4260: keep the lease registry's expiry on the same clock as
		// lastLeaseRenew above. Stamping it only at assignment meant a task
		// still healthily reporting progress past leaseTTL was never reclaimed
		// yet could no longer be re-adopted, so the next socket drop cost the
		// relay its in-flight work. A persist failure is logged only (#8287):
		// the in-memory window is kept, a failed renew persist must not revoke
		// a live task.
		if err := h.renewLease(identityOf(s.contributor), heldTaskID, time.Now()); err != nil {
			h.logger.Warn("[contribute-ws] lease renewed in memory but not persisted",
				"username", identityOf(s.contributor), "task", heldTaskID, "error", err)
		}
	}

}

// handleTaskComplete is the settlement phase for successful work: PR linkage,
// ledgers, rewards, and releasing the task.
func (s *wsSession) handleTaskComplete(msg WSMessage) {
	h := s.h
	if s.contributor != nil {
		s.contributor.mu.Lock()
		// #2568 (the Gate, critical guarantee): a worker whose task was revoked
		// and reassigned — but that later wakes and reports completion carrying
		// the OLD generation — must NOT overwrite the new owner's state. Its
		// currentTaskGen was bumped past what it echoes, so reject the message
		// WITHOUT clearing currentTask (which may now hold the NEW owner's task).
		// An unversioned relay echoes 0 and is accepted, falling back to the
		// TaskID identity match below. Checked before any mutation.
		if msg.TaskGen != 0 {
			s.contributor.sawTaskGen = true
		}
		if s.contributor.currentTask != nil && !generationAccepted(msg.TaskGen, s.contributor.currentTaskGen, s.contributor.sawTaskGen) {
			staleGen := msg.TaskGen
			s.contributor.mu.Unlock()
			h.logger.Warn("[contribute-ws] stale-generation task_complete rejected",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
				"client_gen", staleGen,
			)
			h.recordDecision(s.contributor.profile.GitHubUsername, decisionStaleGenRejected,
				msg.TaskID, "", 0,
				"task_complete fenced: client_gen "+strconv.FormatUint(staleGen, 10)+" no longer matches the assignment")
			return
		}
		hasTask := s.contributor.currentTask != nil && s.contributor.currentTask.TaskID == msg.TaskID
		completedTask := s.contributor.currentTask
		// Captured before the clear below so the run log can record the
		// task's wall-clock duration. Zero when the task was adopted
		// without a fresh assignment; the record then omits duration.
		taskAssignedAt := s.contributor.taskAssignedAt
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
			s.contributor.currentTask = nil
			// #2539: drop the previewable prompt with the task it belonged to
			// so the ops tab does not show a stale instruction after completion.
			s.contributor.currentPrompt = ""
			s.contributor.currentLabels = nil
			s.contributor.tokenMintedAt = time.Time{}
			s.contributor.taskAssignedAt = time.Time{}
			// #2537: clear any pending/delivered credential state with the task.
			s.contributor.pendingToken = ""
			s.contributor.credentialDelivered = false
		}
		// tmuxOutput is diagnostic only and carries no authority, so it is
		// recorded either way — it is often the only evidence of what a
		// confused relay was doing when it reported the wrong task. Tagged
		// with the task the relay NAMED, so a completion's final screen is
		// never mistaken for the next assignment's (#7605).
		s.contributor.tmuxOutput = msg.TmuxOutput
		s.contributor.tmuxOutputTask = msg.TaskID
		s.contributor.mu.Unlock()

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
				h.revokeLease(identityOf(s.contributor), completedTask.TaskID)
			}
			verifiedPR := ""
			var prDetail ghpkg.PRVerification
			if completedTask != nil {
				prDetail = h.verifyReportedPRDetail(completedTask.Repo, msg.PRURL, s.contributor.profile.GitHubUsername)
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
				go h.reconcilePRAttribution(msg.PRURL, s.contributor)
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
			// wins (shipped); with none, only an explicit no_work_needed (or
			// its blocked sibling, #7924) is honoured and everything else —
			// including the absent field every pre-#3987 relay sends — is
			// idle, i.e. today's exact semantics.
			verdict := normalizeCompletionVerdict(msg.Verdict, msg.VerdictBlocked, verifiedPR)
			if completedTask != nil {
				// #2393 item 7 + #2565: the full week-long cooldown is applied
				// only for a VERIFIED PR; an unverified or no-PR completion gets
				// the short cooldown so the issue is not locked for a week (and,
				// per #2492/#2557, still gets a non-zero cooldown so it is not
				// instantly re-offered in a tight loop). #3987: a no_work_needed
				// verdict additionally books the durable offer-suppression
				// verdict (see markTaskCompletedVerdict).
				h.markTaskCompletedVerdictKeySignal(completedTask.identityKey(), verifiedPR,
					verdict, s.contributor.profile.GitHubUsername, strings.TrimSpace(msg.VerdictReason),
					msg.CompletionSignal)
				// #7871: a no_work_needed reason usually names WHAT settled
				// the issue. Verify the citation against GitHub and, if it
				// holds, record it in the claim ledger so the issue is
				// suppressed for the merged-claim window rather than one
				// cooldown. Off the read loop like reconcilePRAttribution:
				// several GitHub round trips must not stall this
				// contributor's pongs. A blocked verdict (#7924) is
				// deliberately NOT settled from: its reason names what the
				// issue is WAITING ON — typically a PR or build in another
				// repo — not what settled it, and recording that as a
				// settlement would be the wrong fact in the claim ledger.
				if verdict == completionVerdictNoWorkNeeded {
					go h.settleIssueFromVerdict(completedTask.Repo, completedTask.Number,
						strings.TrimSpace(msg.VerdictReason), taskAssignedAt,
						s.contributor.profile.GitHubUsername)
				}
			}
			completedDesc := msg.TaskID
			if completedTask != nil {
				completedDesc = assignDesc(completedTask.Kind, completedTask.identityKey(), completedTask.Title, msg.TaskID)
			}
			provider := ""
			if s.contributor.cliBackend == "pi" {
				provider, _, _ = strings.Cut(s.contributor.model, "/")
			}
			h.addActivity(s.contributor.profile.GitHubUsername, "completed", s.contributor.role, s.contributor.cliBackend, s.contributor.model, s.contributor.reasoningEffort, completedDesc, s.contributor.advisor())
			h.logger.Info("[contribute-ws] task complete",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
				"task_gen", msg.TaskGen,
				"backend", s.contributor.cliBackend,
				// Provider is derived from Pi's one canonical provider/model input;
				// this evidence is not a second authority or routing signal.
				"provider", provider,
				"model", s.contributor.model,
				"result", msg.Result,
				"pr_verified", verifiedPR != "",
				"verdict", verdict,
				"verdict_reason", strings.TrimSpace(msg.VerdictReason),
				// #5376: which signal ended the task. Diagnostic only —
				// normalized to a closed vocabulary so a client cannot
				// inject arbitrary text into the hub's structured logs.
				"completion_signal", normalizeCompletionSignal(msg.CompletionSignal),
			)
			// Durable per-run record (task_run_log.go) — the same
			// normalized fields the slog line above carries, plus the
			// duration nothing recorded before. DECLARE only.
			runRec := TaskRunRecord{
				TaskID:           msg.TaskID,
				TaskGen:          msg.TaskGen,
				Username:         s.contributor.profile.GitHubUsername,
				Backend:          s.contributor.cliBackend,
				Provider:         provider,
				Model:            s.contributor.model,
				Effort:           s.contributor.reasoningEffort,
				AdvisorModel:     s.contributor.advisorModel,
				AdvisorEffort:    s.contributor.advisorEffort,
				Role:             s.contributor.role,
				Outcome:          "completed",
				CompletionSignal: normalizeCompletionSignal(msg.CompletionSignal),
				Verdict:          verdict,
				VerdictReason:    strings.TrimSpace(msg.VerdictReason),
				PRURL:            verifiedPR,
				PRVerified:       verifiedPR != "",
			}
			if completedTask != nil {
				runRec.Repo = completedTask.Repo
				runRec.Number = completedTask.Number
			}
			if !taskAssignedAt.IsZero() {
				runRec.DurationS = time.Since(taskAssignedAt).Seconds()
			}
			h.appendTaskRun(runRec)
			// #6450: a genuine completion proves the runtime works — clear
			// the contributor's fast-failure streak.
			h.resetContributorFailureStreak(identityOf(s.contributor))
			s.contributor.mu.Lock()
			// #7862: a completion with nothing behind it — no PR, no
			// no_work_needed, from a relay that said how it decided (chrome
			// inference or a bare `HIVE_VERDICT: complete`) — earns no
			// completed-task credit. It is still booked as a run (above) and
			// still clears the failure streak: the runtime worked, the model
			// just declared victory. The same predicate picks the short,
			// non-escalating issue cooldown in markTaskCompletedVerdictKeySignal.
			if !isEvidenceLessCompletion(verifiedPR, verdict, msg.CompletionSignal) {
				s.contributor.profile.TasksCompleted++
			}
			// Trust credit is gated on the VERIFIED PR, not the reported one:
			// counting the raw self-reported field would hand out
			// contents:write / pulls:write for a PR that was never shown to
			// exist, belongs to another repo, or was authored by someone else.
			if verifiedPR != "" {
				s.contributor.profile.TasksWithPR++
			}
			s.contributor.profile.LastActive = time.Now().UTC().Format(time.RFC3339)
			if completedTask != nil {
				s.contributor.profile.LastCompletedTask = completedTask
			}
			// Promote on completions that produced a VERIFIED pull request.
			// Completion is self-reported, so counting bare task_complete
			// messages — or unverified PR URLs — would hand out contents:write
			// and pulls:write for work that was never shown to exist.
			promoted := false
			if s.contributor.profile.TrustTier == "newcomer" && s.contributor.profile.TasksWithPR >= contributorAutoPromoteAt {
				s.contributor.profile.TrustTier = "contributor"
				promoted = true
				h.logger.Info("[contribute-ws] auto-promoted", "username", s.contributor.profile.GitHubUsername)
			}
			promotedUser := s.contributor.profile.GitHubUsername
			promotedCLI := s.contributor.cliBackend
			promotedModel := s.contributor.model
			promotedEffort := s.contributor.reasoningEffort
			s.contributor.mu.Unlock()
			// #2390-era command center: narrate the promotion as its own
			// activity event so the Operations dev-log and achievement pops
			// (contribute_sse.go broadcast) surface "promoted to contributor".
			// Read-only signalling — it changes no control behaviour and is
			// emitted only on the real newcomer -> contributor transition.
			if promoted {
				h.addActivity(promotedUser, "promoted", "contributor", promotedCLI, promotedModel, promotedEffort, "contributor")
			}
			_ = saveContributorProfile(s.contributor.profile)
		} else {
			// N9: this is now literally true. It previously logged "ignored"
			// after the handler had already cleared currentTask and the
			// credential state, which made the bypass look like a no-op in the
			// logs.
			h.logger.Warn("[contribute-ws] task_complete for unassigned task ignored (assignment left intact)",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
			)
			h.recordDecision(s.contributor.profile.GitHubUsername, decisionUnassignedIgnored,
				msg.TaskID, "", 0,
				"task_complete ignored: this connection does not hold that task")
		}
	}

}

// handleTaskFailed is the settlement phase for failed work: failure streaks,
// cooldowns, and releasing the task.
func (s *wsSession) handleTaskFailed(msg WSMessage) {
	h := s.h
	if s.contributor != nil {
		s.contributor.mu.Lock()
		// #2568 (the Gate): reject a STALE worker's failure the same way as its
		// completion — its currentTaskGen was bumped past the generation it
		// echoes. Left unguarded, a revoked worker's late task_failed would book
		// a spurious failure cooldown against the NEW owner's issue. Do not clear
		// currentTask (it may now be the new owner's task). Unversioned relays
		// echo 0 and fall back to the TaskID match below.
		if msg.TaskGen != 0 {
			s.contributor.sawTaskGen = true
		}
		if s.contributor.currentTask != nil && !generationAccepted(msg.TaskGen, s.contributor.currentTaskGen, s.contributor.sawTaskGen) {
			staleGen := msg.TaskGen
			s.contributor.mu.Unlock()
			h.logger.Warn("[contribute-ws] stale-generation task_failed rejected",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
				"client_gen", staleGen,
			)
			// #7330: THE event the issue was filed for. A relay whose
			// up-front rejection paths send task_failed with no task_gen
			// gets fenced here once the connection has sent a non-zero
			// one — the relay believes it reported a failure, the hub
			// believes it never did, and until now the disagreement was
			// visible only in the hub's stdout.
			h.recordDecision(s.contributor.profile.GitHubUsername, decisionStaleGenRejected,
				msg.TaskID, "", 0,
				"task_failed fenced: client_gen "+strconv.FormatUint(staleGen, 10)+" no longer matches the assignment")
			return
		}
		hasTask := s.contributor.currentTask != nil && s.contributor.currentTask.TaskID == msg.TaskID
		failedTask := s.contributor.currentTask
		// Duration anchor for the run log, captured before the clear —
		// same shape as task_complete above.
		taskAssignedAt := s.contributor.taskAssignedAt
		// SECURITY (audit N9, CWE-862/639): same hole as task_complete —
		// clear only on a genuine TaskID match. Unconditionally, a failure
		// naming any other task released the assignment while revokeLease
		// and recordTaskFailure below stayed inside `if hasTask`, so no
		// cooldown was booked and the abandoned issue became instantly
		// re-admissible while the credential stayed live.
		if hasTask {
			s.contributor.currentTask = nil
			s.contributor.tokenMintedAt = time.Time{}
			s.contributor.taskAssignedAt = time.Time{}
			// #2537: clear any pending/delivered credential state with the task.
			s.contributor.pendingToken = ""
			s.contributor.credentialDelivered = false
		}
		s.contributor.mu.Unlock()

		if hasTask {
			// C4: a failure is terminal — revoke the server-issued lease so a
			// later task_progress for this task cannot resurrect ownership.
			if failedTask != nil {
				h.revokeLease(identityOf(s.contributor), failedTask.TaskID)
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
			s.contributor.mu.Lock()
			s.contributor.lastFailure = &ContributorFailure{
				TaskID:    msg.TaskID,
				Kind:      failureKind,
				Reason:    msg.Reason,
				Permanent: msg.Permanent,
				At:        time.Now().UTC().Format(time.RFC3339),
			}
			if failedTask != nil {
				s.contributor.lastFailure.Repo = failedTask.Repo
				s.contributor.lastFailure.Number = failedTask.Number
			}
			s.contributor.mu.Unlock()

			failedDesc := msg.TaskID
			if failedTask != nil {
				failedDesc = assignDesc(failedTask.Kind, failedTask.identityKey(), failedTask.Title, msg.TaskID)
			}
			provider := ""
			if s.contributor.cliBackend == "pi" {
				provider, _, _ = strings.Cut(s.contributor.model, "/")
			}
			h.addActivity(s.contributor.profile.GitHubUsername, "failed", s.contributor.role, s.contributor.cliBackend, s.contributor.model, s.contributor.reasoningEffort, failedDesc, s.contributor.advisor())
			h.logger.Info("[contribute-ws] task failed",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
				"task_gen", msg.TaskGen,
				"backend", s.contributor.cliBackend,
				"provider", provider,
				"model", s.contributor.model,
				"result", "failed",
				"reason", msg.Reason,
				"failure_kind", failureKind,
				"permanent", msg.Permanent,
			)
			// Durable per-run record (task_run_log.go). The reason is
			// the same bounded, fleet-view-displayed text stored on
			// lastFailure above; failure_kind is already normalized.
			runRec := TaskRunRecord{
				TaskID:        msg.TaskID,
				TaskGen:       msg.TaskGen,
				Username:      s.contributor.profile.GitHubUsername,
				Backend:       s.contributor.cliBackend,
				Provider:      provider,
				Model:         s.contributor.model,
				Effort:        s.contributor.reasoningEffort,
				AdvisorModel:  s.contributor.advisorModel,
				AdvisorEffort: s.contributor.advisorEffort,
				Role:          s.contributor.role,
				Outcome:       "failed",
				FailureKind:   failureKind,
				Reason:        msg.Reason,
				Permanent:     msg.Permanent,
				// #7317 item 3: the pane at the moment of failure. The relay
				// captures it BEFORE stopping the agent precisely so this
				// report carries the evidence (see failCurrentTask); until now
				// the hub read it and kept nothing.
				PaneTail: boundPaneTail(msg.TmuxOutput),
			}
			if failedTask != nil {
				runRec.Repo = failedTask.Repo
				runRec.Number = failedTask.Number
			}
			if !taskAssignedAt.IsZero() {
				runRec.DurationS = time.Since(taskAssignedAt).Seconds()
			}
			h.appendTaskRun(runRec)
			// #6450: book this failure against the CONTRIBUTOR's fast-failure
			// streak (hub-measured duration; an unknown/adopted-task duration
			// is not counted). Separate from the per-issue cooldown above:
			// same failure, two ledgers, two questions ("is this issue
			// poisoned?" vs "is this contributor's runtime dying?").
			if !taskAssignedAt.IsZero() {
				h.recordContributorFastFailure(identityOf(s.contributor), time.Since(taskAssignedAt), msg.Reason)
			}
			s.contributor.mu.Lock()
			s.contributor.profile.TasksFailed++
			s.contributor.mu.Unlock()
			_ = saveContributorProfile(s.contributor.profile)
		} else {
			h.recordDecision(s.contributor.profile.GitHubUsername, decisionUnassignedIgnored,
				msg.TaskID, "", 0,
				"task_failed ignored: this connection does not hold that task")
			h.logger.Warn("[contribute-ws] task_failed for unassigned task ignored",
				"username", s.contributor.profile.GitHubUsername,
				"task", msg.TaskID,
			)
		}
	}

}

func (h *ContributeWSHub) heartbeatLoop(c *ContributorConnection) {
	ticker := time.NewTicker(wsHeartbeatInterval)
	defer ticker.Stop()
	h.heartbeatLoopWithTicks(c, ticker.C, wsHeartbeatTimeout)
}

// heartbeatLoopWithTicks contains the heartbeat state machine. The production
// wrapper supplies its 30-second ticker and 90-second timeout; tests supply a
// finite tick stream so the loop ordering can be exercised without waiting.
func (h *ContributeWSHub) heartbeatLoopWithTicks(c *ContributorConnection, ticks <-chan time.Time, timeout time.Duration) {
	for range ticks {
		// Stop as soon as this socket has been deregistered (kubestellar/hive#5090).
		//
		// The disconnect defer in HandleWS runs on the READ goroutine the moment
		// ReadMessage errors: it deletes the connID from h.connections and closes
		// the socket. This loop learns none of that — it has no done channel and no
		// reference to the read side — so it slept out the remainder of its 30s tick
		// and then wrote a ping to an already-closed connection. That write of
		// course failed, and the failure branch logged
		//
		//     [contribute-ws] heartbeat ping failed, closing
		//
		// which reads as a diagnosis of why the connection died and is nothing of
		// the sort: the connection was already dead and buried, by up to a full
		// heartbeat interval. That line is what #5090 spent an investigation
		// chasing. Because the tick is a fixed offset from REGISTRATION, it landed
		// ~29-30s after every "new connection" regardless of what actually killed
		// the socket, which is precisely why the flap looked like a clean 30s idle
		// timer and sent the diagnosis toward per-direction proxy timeouts.
		//
		// Checking registration here makes the loop exit silently on a socket
		// somebody else already tore down, so the "heartbeat ping failed" line is
		// emitted ONLY when the heartbeat write is genuinely the first thing to
		// notice the socket is bad. It also stops the goroutine leaking for up to
		// one interval per disconnect, which on a flapping session is a goroutine
		// per flap.
		if !h.connectionRegistered(c) {
			return
		}

		c.mu.Lock()
		lastPong := c.lastPong
		c.mu.Unlock()

		if time.Since(lastPong) > timeout {
			h.logger.Info("[contribute-ws] heartbeat timeout",
				"id", c.connID,
				"username", c.profile.GitHubUsername,
				"last_pong_age_ms", time.Since(lastPong).Milliseconds(),
				"heartbeat_timeout_ms", timeout.Milliseconds(),
			)
			closeWithReason(c.ws, websocket.CloseGoingAway, "heartbeat timeout: no pong within the heartbeat window")
			return
		}

		h.maybeRefreshToken(c)

		if err := c.send(WSMessage{Type: "ping", Seq: h.nextSeq()}); err != nil {
			h.logger.Info("[contribute-ws] heartbeat ping failed, closing",
				"id", c.connID,
				"username", c.profile.GitHubUsername,
				"error", err,
			)
			closeWithReason(c.ws, websocket.CloseGoingAway, "heartbeat ping write failed")
			return
		}

		// Also emit a PROTOCOL-level Ping control frame alongside the JSON one
		// (kubestellar/hive#5090). See writeProtocolPing for why the JSON ping
		// alone is not enough to hold the connection open through the ingress
		// path. A failure here is not fatal on its own: the JSON ping above
		// already succeeded, so the socket is live and the next tick's
		// heartbeat-timeout check remains the authority on when to hang up.
		if err := writeProtocolPing(c.ws); err != nil {
			h.logger.Debug("[contribute-ws] protocol ping failed",
				"id", c.connID, "username", c.profile.GitHubUsername, "error", err)
		}
	}
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

// connectionRegistered reports whether this exact connection object is still in
// the hub's live connection map (kubestellar/hive#5090).
//
// h.connections is keyed by a random per-socket connID that the heartbeat loop
// never sees, so the lookup is by VALUE: scan for the pointer. The map is capped
// at maxWSConnections (50), so this is a bounded scan once per 30s tick per
// connection — negligible next to the network write it guards.
//
// Pointer identity is the right test rather than any field comparison: it is
// exactly "is the object I was started for still the registered one", which is
// false both when the socket was deregistered by its disconnect defer and when a
// reconnect replaced it under a new connID. Both mean this loop has no further
// work to do.
//
// Takes only h.mu.RLock and no connection-level lock, so it cannot participate in
// any lock ordering — callers may hold c.mu or c.writeMu or neither.
func (h *ContributeWSHub) connectionRegistered(c *ContributorConnection) bool {
	if h == nil || c == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.connections {
		if conn == c {
			return true
		}
	}
	return false
}

// taskReadoptedByLiveConnection reports whether some OTHER live connection
// belonging to the SAME contributor identity is currently holding the given task
// (kubestellar/hive#5322).
//
// It exists because h.connections is keyed by a random per-socket connID, so the
// hub cannot tell "the contributor's current socket" from "a socket the
// contributor abandoned". When a tunnel is cut without a close frame the dead
// socket's read loop stays parked until its next read errors, so its disconnect
// defer can fire well AFTER the relay has redialed and re-adopted the task from
// the server-issued lease. Releasing by issue at that point tears down work that
// is demonstrably still in flight.
//
// The match is deliberately narrow — same identity AND same task id AND same
// canonical repo/number — so it can only ever suppress the release of the exact
// assignment that was reconciled. Anything else (a different task, a different
// contributor, no live holder at all) is a genuine abandonment and releases
// normally. It suppresses ONLY the release; it never adopts, assigns, extends a
// lease, or relaxes any admission or ownership check.
//
// Concurrency: takes h.mu.RLock and each candidate's own mu, mirroring
// taskHeldByAnotherConnection and the other read-only scans. `self` is skipped by
// pointer identity, so the caller may hold neither, either, or both of self.mu
// and self.writeMu without risking re-entrancy on this path.
func (h *ContributeWSHub) taskReadoptedByLiveConnection(self *ContributorConnection, task *WSTaskAssign) bool {
	if h == nil || task == nil || task.TaskID == "" {
		return false
	}
	identity := identityOf(self)
	if identity == "" {
		return false
	}
	canonicalRepo := h.canonicalRepoKey(task.Repo)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.connections {
		if conn == nil || conn == self {
			continue
		}
		if identityOf(conn) != identity {
			continue
		}
		conn.mu.Lock()
		held := conn.currentTask != nil &&
			conn.currentTask.TaskID == task.TaskID &&
			conn.currentTask.Number == task.Number &&
			h.canonicalRepoKey(conn.currentTask.Repo) == canonicalRepo
		conn.mu.Unlock()
		if held {
			return true
		}
	}
	return false
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
	if h.doneCh != nil {
		defer close(h.doneCh)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			// #2568: reclaim wedged-but-connected task leases first (the backstop). A
			// connection whose lastPong is still fresh (so the heartbeat sweep below will
			// NOT remove it) but that has stopped renewing its task lease is exactly the
			// "connected but wedged" case the issue describes; this releases its task
			// through the SAME cooldown+generation-bump path a manual requeue uses.
			h.reclaimExpiredLeases(time.Now())

			// #5681: drop leases that aged out without ever being looked up — a relay
			// that never came back after a restart leaves one behind, and it would
			// otherwise keep its issue out of the assignment pool until the process
			// ended.
			h.pruneExpiredLeases(time.Now())

			// Deregister under the lock; CLOSE outside it.
			//
			// closeWithReason writes a Close frame with a deadline, so it can block for
			// up to wsCloseFrameDeadline against a peer that has stopped reading — which
			// is exactly what a stale connection is. Doing that while holding h.mu would
			// hold the hub-wide lock for up to one second PER stale socket, serially:
			// against the maxWSConnections cap of 50 that is a worst case near a minute
			// during which no contributor can register, no sequence number can be
			// allocated, and every other hub operation stalls. The bare c.ws.Close() this
			// replaced could not block, so the risk arrived with the close frame and is
			// removed here rather than traded for it.
			var staleConns []*websocket.Conn
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
					// Nil-guard: a connection may carry no live socket (e.g. a
					// test-injected in-flight entry, or a connection torn down elsewhere),
					// and cleanupLoop iterates ALL registered connections. Mirrors the
					// existing guard in RequeueContributorTask so a ws-less entry is pruned
					// rather than nil-dereferenced.
					if c.ws != nil {
						staleConns = append(staleConns, c.ws)
					}
					delete(h.connections, id)
				}
			}
			h.mu.Unlock()

			// Already deregistered above, so a slow or wedged peer here delays nothing
			// but this sweep — the next tick is 30s away and re-observes fresh state.
			for _, ws := range staleConns {
				closeWithReason(ws, websocket.CloseGoingAway, "connection went stale: no pong within the heartbeat window")
			}
		}
	}
}

// Close terminates the hub's background cleanup loop and blocks until it
// exits. It is safe to call concurrently and multiple times (idempotent).
func (h *ContributeWSHub) Close() {
	if h == nil {
		return
	}
	h.stopOnce.Do(func() {
		if h.stopCh != nil {
			close(h.stopCh)
		}
	})
	if h.doneCh != nil {
		<-h.doneCh
	}
}

// Stop terminates the hub's background cleanup loop, matching Close.
func (h *ContributeWSHub) Stop() {
	h.Close()
}

// identityOf returns the stable key that groups a contributor's live
// connections for per-identity concurrency accounting (#2436 finding 3). The
// registered ContributorID is preferred; GitHubUsername is the fallback for
// connections whose profile predates or lacks an ID. Two WebSocket connections
// opened by the same registered contributor therefore share one identity.
// sanitizeSessionLabel bounds and cleans a client-declared session label before
// it becomes part of a map key, log field, and UI string. Multi-session-per-
// account: the label distinguishes concurrent relays under one GitHub account.
// Only [A-Za-z0-9._-] survive (so the ContributorID#session key stays a single
// clean token and cannot smuggle path/format characters); the result is capped
// at 32 bytes. An empty or all-stripped label returns "", which identityOf
// treats as the historical single-session case.
func sanitizeSessionLabel(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}

func identityOf(c *ContributorConnection) string {
	if c == nil || c.profile == nil {
		return ""
	}
	base := c.profile.ContributorID
	if base == "" {
		base = c.profile.GitHubUsername
	}
	// Session-scoped identity (multi-session-per-account): a distinct session
	// label lets concurrent relays under one account hold independent task
	// leases/cooldowns/ownership. Empty session preserves the historical bare
	// ContributorID key, so existing single-session contributors are unchanged.
	if base != "" && c.session != "" {
		return base + "#" + c.session
	}
	return base
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

// wsCloseFrameDeadline bounds the write of the courtesy Close frame. It is a
// best-effort courtesy on a socket we are hanging up anyway: if the peer is
// already gone the write fails immediately, and waiting longer than this would
// hold a goroutine open for a client that will never read it.
const wsCloseFrameDeadline = time.Second

// wsProtocolPingDeadline bounds the write of a keepalive Ping control frame.
// It is deliberately far shorter than wsHeartbeatInterval so a wedged socket
// cannot stack up heartbeat goroutines waiting on a peer that has stopped
// reading.
const wsProtocolPingDeadline = 10 * time.Second

// wsWriteDeadline bounds every application JSON write to a live contributor
// connection (kubestellar/hive#5090).
//
// gorilla/websocket applies no write deadline by default, so WriteJSON against a
// peer that has stopped reading blocks until the OS gives up on the socket —
// which, on a half-open TCP connection with no RST, can be many minutes of
// retransmission backoff. Because send() holds writeMu across the write, that
// stall is not confined to the writing goroutine: it blocks every other writer
// on the same connection.
//
// It is deliberately shorter than wsHeartbeatInterval so a write cannot still be
// parked when the next heartbeat tick arrives (which would stack ticker
// goroutines on writeMu), and comfortably longer than wsProtocolPingDeadline so
// an ordinary slow client is never mistaken for a wedged one.
const wsWriteDeadline = 15 * time.Second

// writeProtocolPing sends a WebSocket PROTOCOL-level Ping control frame (opcode
// 0x9) on the connection.
//
// THE DEFECT (kubestellar/hive#5090): the contributor keepalive was implemented
// ENTIRELY as application JSON — the hub sends {"type":"ping"} as a text frame
// and the relay answers {"type":"pong"}, and neither side ever emitted a
// control frame. websocket.PingMessage appeared nowhere in this package, and
// the relay never called ws.ping().
//
// That distinction is invisible to the two endpoints and decisive to everything
// between them. An L7 proxy that understands WebSocket — and the hosted spokes
// sit behind both ingress-nginx and an OCI load balancer — may account only for
// control-frame traffic when deciding whether a tunnel is idle, precisely
// because application payload can be a long-running unidirectional stream that
// says nothing about liveness. Under such a proxy a connection carrying a text
// frame every 30 seconds is still "idle", and gets reaped on the idle timer
// with no Close frame: the peer sees 1006 with an empty reason and no
// application-layer log on either side, which is exactly the signature #5090
// measured — the hub proven to send Close frames on its own hangups, yet every
// observed flap frameless.
//
// Sending a real Ping costs one 2-byte control frame per 30s tick and makes the
// connection unambiguously live to any conforming intermediary. It is additive:
// the JSON ping/pong stays exactly as it was, so old relays that answer only
// the JSON heartbeat are unaffected, and gorilla/websocket answers an inbound
// Ping with a Pong automatically via its default ping handler, so a relay needs
// no change to make the reverse direction work either.
//
// WriteControl is documented as safe to call concurrently with all other
// methods, so — like closeWithReason — this deliberately does NOT take writeMu.
// Taking it here would nest a second lock under callers that already hold it
// and buy nothing.
func writeProtocolPing(conn *websocket.Conn) error {
	if conn == nil {
		return nil
	}
	return conn.WriteControl(
		websocket.PingMessage,
		nil,
		time.Now().Add(wsProtocolPingDeadline),
	)
}

// closeWithReason closes a contributor socket after telling the client WHY.
//
// THE DEFECT (kubestellar/hive#5090): every close on this path was a bare
// conn.Close(), which shuts the TCP socket without sending a WebSocket Close
// frame. The client therefore observes code 1006 (abnormal closure) with an
// empty reason — byte-for-byte identical to a yanked network cable. Measured
// against a live hub: an unauthenticated probe received the auth-timeout
// explanation as a JSON message and then, on the very next line, a 1006 close
// carrying none of it. Deliberate server hangups and network faults were
// indistinguishable, so a contributor whose session was flapping had no way to
// learn which one they were looking at.
//
// The JSON auth_failed messages some call sites already send are not a
// substitute: they are absent entirely from the heartbeat and stale-sweep
// closes, and a client that has stopped reading (the exact case a heartbeat
// timeout describes) never sees them.
//
// WriteControl is documented as safe to call concurrently with all other
// methods, so this needs no coordination with the writeMu the JSON senders
// take. Both the frame write and the close are best-effort: a peer that has
// already vanished simply fails the write, which is not worth logging on a
// socket being discarded.
func closeWithReason(conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(wsCloseFrameDeadline),
	)
	_ = conn.Close()
}

// wsDrainReason is the close reason every contributor socket receives when the
// hub process is shutting down. It is a fixed string rather than a formatted
// one so a relay may match on it verbatim.
const wsDrainReason = "hub restarting for upgrade"

// wsDrainBudget bounds the ENTIRE shutdown drain, not one socket.
//
// closeWithReason writes a Close frame with a wsCloseFrameDeadline (1s) write
// deadline, so a peer that has stopped reading can stall a single close for up
// to a second. Against the maxWSConnections cap of 50 a serial drain is a 50s
// worst case — longer than terminationGracePeriodSeconds (30s), which would
// mean the drain itself delays the exit past the point where SIGKILL lands and
// the archive hook that follows it never runs. The budget makes that
// impossible: whatever has not been closed when it expires is abandoned, and
// the sockets left behind are exactly as dead as they are today.
const wsDrainBudget = 2 * time.Second

// DrainForShutdown tells every registered contributor WHY the socket is about
// to die (kubestellar/hive#5390).
//
// THE DEFECT: the hub process is killed on every upgrade roll — measured at 11
// ReplicaSets in 5.5 hours on one hosted spoke, one per merge to v4 — and it
// has no shutdown handling for contributor WebSockets at all. The sockets die
// with the process at SIGKILL, so the peer observes a bare 1006 with no reason,
// byte-for-byte identical to a yanked cable. #5107 made DELIBERATE closes
// legible; process death was not one of them, which is why the hub provably
// sends Close frames and yet every flap observed in #5090 was frameless.
//
// CloseServiceRestart (1012) is defined as "the server is restarting" and
// carries the client expectation of reconnecting, which is exactly true here:
// the deployment is maxSurge=1/maxUnavailable=0, so the replacement pod has
// already passed its readiness probe by the time the old one gets SIGTERM.
//
// This does NOT stop the flap — the pod still rolls, the socket still dies.
// What changes is that the relay learns immediately and reconnects into an
// already-serving hub, instead of discovering the corpse by read error or
// missed pong seconds later.
//
// Locking follows cleanupLoop exactly: snapshot the sockets under h.mu, then
// close OUTSIDE it. Closing under the lock would hold the hub-wide mutex for up
// to wsCloseFrameDeadline per wedged peer, serially. Connections are NOT
// deleted from the map — the process is about to exit, and leaving the
// bookkeeping untouched keeps this off the lease/cooldown accounting held under
// #5151.
//
// Returns the number of sockets a Close frame was attempted on, for the
// shutdown log line.
func (h *ContributeWSHub) DrainForShutdown() int {
	if h == nil {
		return 0
	}

	var conns []*websocket.Conn
	h.mu.RLock()
	for _, c := range h.connections {
		// Nil-guard mirrors cleanupLoop: a registered connection may carry no live
		// socket (test-injected in-flight entry, or one torn down elsewhere).
		if c != nil && c.ws != nil {
			conns = append(conns, c.ws)
		}
	}
	h.mu.RUnlock()

	if len(conns) == 0 {
		return 0
	}

	// Fire-and-forget. A WebSocket Close is not a handshake we need to complete
	// server-side, and waiting for acknowledgements would put a peer's silence on
	// the shutdown path's critical section.
	deadline := time.Now().Add(wsDrainBudget)
	drained := 0
	for _, ws := range conns {
		if !time.Now().Before(deadline) {
			break
		}
		closeWithReason(ws, websocket.CloseServiceRestart, wsDrainReason)
		drained++
	}

	if h.logger != nil {
		h.logger.Info("[contribute-ws] drained contributor sockets for shutdown",
			"drained", drained, "registered", len(conns))
	}
	return drained
}

// DrainContributorsForShutdown is the Server-level entry point for the
// pre-shutdown drain. It exists so cmd/hive can reach the contributor hub
// without the hub itself being exported plumbing, and is a no-op on a Server
// whose contribute routes were never registered.
func (s *Server) DrainContributorsForShutdown() int {
	if s == nil || s.contributeHub == nil {
		return 0
	}
	return s.contributeHub.DrainForShutdown()
}

// CloseContributeHub is the Server-level entry point for stopping the
// contributor hub's background cleanup loop. It is idempotent and safe to
// call multiple times, and is a no-op on a Server whose contribute routes
// were never registered.
func (s *Server) CloseContributeHub() {
	if s == nil || s.contributeHub == nil {
		return
	}
	s.contributeHub.Close()
}

// Close shuts down server-managed background resources, including the
// contributor hub.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.CloseContributeHub()
}

func contributorStatePath(contributorsDir, currentPath, name string) string {
	defaultPath := filepath.Join(defaultContributorsDir, name)
	if currentPath != "" && currentPath != defaultPath {
		return currentPath
	}
	return filepath.Join(contributorsDir, name)
}

var turnEnvelopeDirPath = "/data/contributors/turn-envelopes"

// contributorSupportsQuotaPreflight reports whether the connected relay
// advertised the quota_preflight_v1 capability (kubestellar/hive#6954). It gates
// the auto-accept credential hold: a relay that advertises it will answer an
// offered task with task_accepted or a local_capacity_guard task_declined BEFORE
// the scoped credential is delivered (#6833), so the hub withholds and waits; a
// relay that does not advertise it cannot preflight, so the hub delivers the
// credential on the auto-accept path exactly as it did before #6833.
//
// This is TRUE capability negotiation, replacing the prior proxy on
// RelayProtocolVersion. That proxy withheld the credential from every relay that
// declared ANY protocol version — #6931 never bumped RELAY_PROTOCOL_VERSION, so
// every already-deployed relay tripped it — and only kept working by the
// accident that the in-tree relay has always sent task_accepted unconditionally.
// Gating on the advertised token instead makes the contract explicit: nothing is
// withheld unless the relay has stated it will answer.
//
// Fail closed on the negotiated side, backward-compatible on the legacy side: an
// absent/empty capability list is read as "no preflight" and takes the pre-#6833
// immediate-delivery path, which is the deliberate compatibility choice #6833's
// "mixed-version hub/relay behaviour remains backward compatible" criterion
// requires — an old relay must not be stranded waiting for a credential it will
// never earn because it does not know how to accept or decline.
func contributorSupportsQuotaPreflight(c *ContributorConnection) bool {
	if c == nil || c.capabilities == nil {
		return false
	}
	return c.capabilities.DeclaresCapability(capQuotaPreflight)
}

func taskComplexityFromLabels(labels []string) string {
	for _, raw := range labels {
		l := strings.ToLower(strings.TrimSpace(raw))
		switch l {
		case "complexity/simple", "simple":
			return "simple"
		case "complexity/medium", "medium":
			return "medium"
		case "complexity/complex", "complex":
			return "complex"
		case "complexity/unknown", "unknown":
			return "unknown"
		}
	}
	return "unknown"
}

func capabilityRoutingInputs(c *ContributorConnection) (*ContributorCapabilities, string) {
	if c == nil {
		return nil, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var caps *ContributorCapabilities
	if c.capabilities != nil {
		cp := *c.capabilities
		caps = &cp
	}
	return caps, c.cliBackend
}

func websocketCloseErrorDetails(err error) (code int, reason, source string) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		source = "close-frame"
		if closeErr.Code == websocket.CloseAbnormalClosure {
			source = "abnormal-no-close-frame"
		}
		return closeErr.Code, closeErr.Text, source
	}
	return 0, "", "read-error"
}

func contributorUsername(c *ContributorConnection) string {
	if c == nil || c.profile == nil {
		return ""
	}
	return c.profile.GitHubUsername
}

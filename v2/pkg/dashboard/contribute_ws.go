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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
)

const (
	wsHeartbeatInterval = 30 * time.Second
	wsHeartbeatTimeout  = 90 * time.Second
	wsTaskTimeout       = 30 * time.Minute
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
	ws          *websocket.Conn
	profile     *ContributorProfile
	cliBackend  string
	model       string
	role        string // empty = task-driven mode, "scanner"/"reviewer"/etc. = role mode
	connectedAt time.Time
	currentTask *WSTaskAssign
	lastPong    time.Time
	tmuxOutput  []string
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
	mu           sync.Mutex
}

type WSMessage struct {
	Type              string   `json:"type"`
	Seq               int      `json:"seq,omitempty"`
	Nonce             string   `json:"nonce,omitempty"`
	ContributorID     string   `json:"contributor_id,omitempty"`
	TrustTier         string   `json:"trust_tier,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	RegistrationToken string   `json:"registration_token,omitempty"`
	CLIBackend        string   `json:"cli_backend,omitempty"`
	Model             string   `json:"model,omitempty"`
	TaskID            string   `json:"task_id,omitempty"`
	Kind              string   `json:"kind,omitempty"`
	Repo              string   `json:"repo,omitempty"`
	Number            int      `json:"number,omitempty"`
	Title             string   `json:"title,omitempty"`
	URL               string   `json:"url,omitempty"`
	Labels            []string `json:"labels,omitempty"`
	Prompt            string   `json:"prompt,omitempty"`
	GitHubToken       string   `json:"github_token,omitempty"`
	TokenExpiresAt    string   `json:"token_expires_at,omitempty"`
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
	// convention in v2/pkg/github/prclaims.go.
	PRURL string `json:"pr_url,omitempty"`
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
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
}

const maxActivityEntries = 50

type ActivityEntry struct {
	Timestamp string `json:"timestamp"`
	Username  string `json:"username"`
	Action    string `json:"action"`
	Role      string `json:"role,omitempty"`
	CLI       string `json:"cli,omitempty"`
	Model     string `json:"model,omitempty"`
	Task      string `json:"task,omitempty"`
}

type ContributeWSHub struct {
	connections    map[string]*ContributorConnection
	mu             sync.RWMutex
	logger         *slog.Logger
	seq            int
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
	completedMu         sync.Mutex
	selectMu            sync.Mutex
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

func NewContributeWSHub(logger *slog.Logger, server *Server) *ContributeWSHub {
	hub := &ContributeWSHub{
		connections:           make(map[string]*ContributorConnection),
		completedTasks:        make(map[string]time.Time),
		completedTaskCooldown: make(map[string]time.Duration),
		completedTaskPRURL:    make(map[string]string),
		failedTasks:           make(map[string]time.Time),
		consecutiveFailures:   make(map[string]int),
		assignmentTimes:       make(map[string][]time.Time),
		logger:                logger,
		server:                server,
		sse:                   newSSERegistry(),
	}
	hub.loadCompletedTasks()
	hub.loadFailedTasks()
	hub.loadActivity()
	go hub.cleanupLoop()
	return hub
}

const activityFilePath = "/data/contributors/activity.json"

func (h *ContributeWSHub) loadActivity() {
	data, err := os.ReadFile(activityFilePath)
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
	h.activityMu.RLock()
	entries := make([]ActivityEntry, len(h.activity))
	copy(entries, h.activity)
	h.activityMu.RUnlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	os.MkdirAll("/data/contributors", 0o755)
	tmpPath := activityFilePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] activity write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, activityFilePath); err != nil {
		h.logger.Warn("[contribute-ws] activity rename failed", "error", err)
	}
}

const activityDebounceSecs = 60

func (h *ContributeWSHub) addActivity(username, action, role, cli, model, task string) {
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
		Task:      task,
	}
	h.activity = append(h.activity, entry)
	if len(h.activity) > maxActivityEntries {
		h.activity = h.activity[len(h.activity)-maxActivityEntries:]
	}
	h.activityMu.Unlock()
	go h.saveActivity()
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

const completedTasksFile = "/data/contributors/completed-tasks.json"

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

const failedTasksFile = "/data/contributors/failed-tasks.json"

// failedTaskRecord is the on-disk shape of one failed-task entry (#2435). It
// preserves the last-failure time and the running consecutive-failure count so a
// hub restart does not reset a quarantine — otherwise a bounce would re-admit a
// poison issue that had already earned its longer parking window.
type failedTaskRecord struct {
	FailedAt            time.Time `json:"failed_at"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
}

func (h *ContributeWSHub) loadFailedTasks() {
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(failedTasksFile)
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
	os.MkdirAll("/data/contributors", 0o755)
	tmpPath := failedTasksFile + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, failedTasksFile); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) loadCompletedTasks() {
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(completedTasksFile)
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
	os.MkdirAll("/data/contributors", 0o755)
	tmpPath := completedTasksFile + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, completedTasksFile); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks rename failed", "error", err)
	}
}

// markTaskCompleted records a completed task and starts its issue cooldown.
//
// The cooldown length is conditional on whether the completion actually shipped
// a pull request (kubestellar/hive#2393 item 7): a completion WITH a prURL gets
// the full completedTaskCooldownHours (real work landed — don't re-dispatch for
// a week), while a completion WITHOUT one — the agent merely returned to idle —
// gets the short completedNoPRCooldownHours so an issue where nothing shipped
// is not locked out for a week. The chosen expiry is stored per task and honored
// by isTaskInCooldown; the prURL is retained for stats/audit and #2356
// duplicate detection.
func (h *ContributeWSHub) markTaskCompleted(repo string, number int, prURL string) {
	key := fmt.Sprintf("%s#%d", repo, number)
	cooldown := completedNoPRCooldownHours * time.Hour
	if prURL != "" {
		// The WITH-PR period is operator-tunable (Config.Hub.ContributeCooldownHours,
		// default completedTaskCooldownHours). We still RECORD this even when cooldown
		// is disabled — isTaskInCooldown short-circuits the gating, so the history is
		// kept for stats/audit but never excludes the issue.
		cooldown = h.configuredWithPRCooldown()
	}
	h.completedMu.Lock()
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
	if prURL == "" {
		return false
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		// No GitHub client (hive booted without credentials, or a bare test hub):
		// we cannot verify, so we must not grant trust. Degrade to unverified.
		h.logger.Warn("[contribute-ws] PR verification skipped: no github client",
			"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername)
		return false
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res := h.server.deps.GHClient.VerifyReportedPR(ctx, assignedRepo, prURL, contributorUsername)
	if res.Verified {
		h.logger.Info("[contribute-ws] reported PR verified",
			"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername,
			"author", res.Author, "base_repo", res.BaseRepo)
		return true
	}
	// Distinguish a clean negative from an API error only in the log; both
	// downgrade to unverified.
	logArgs := []any{"repo", assignedRepo, "pr_url", prURL, "username", contributorUsername, "reason", res.Reason}
	if res.Err != nil {
		logArgs = append(logArgs, "error", res.Err.Error())
	}
	h.logger.Warn("[contribute-ws] reported PR NOT verified — treating completion as no-PR", logArgs...)
	return false
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

func (h *ContributeWSHub) isTaskInCooldown(repo string, number int) bool {
	// Operator kill-switch: when cooldown is disabled, no completed issue is ever
	// gated out of the queue for cooldown. Completion HISTORY is still recorded by
	// markTaskCompleted (stats/audit, #2356 duplicate detection) and failure
	// quarantine is unaffected — this only stops cooldown from EXCLUDING work.
	if !h.cooldownEnabled() {
		return false
	}
	key := fmt.Sprintf("%s#%d", repo, number)
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
	key := fmt.Sprintf("%s#%d", repo, number)
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
	key := fmt.Sprintf("%s#%d", repo, number)
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
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	return h.consecutiveFailures[key]
}

// RequeueContributorTask is the server side of the operator MANUAL requeue action
// (kubestellar/hive#2568, the safe slice). An operator who can SEE that a connected
// clanker is wedged — holding a task but not progressing — releases that task back
// to the ready queue. It is deliberately the SAME release+cooldown path the
// automatic disconnect (#2356/#2435) and ready-abandon (#2545) handlers already
// use, so a manual requeue can NOT reintroduce the duplicate-assignment race #2492/
// #2557 closed: for every session of contributorID that is currently holding a real
// issue task we
//
//  1. clear currentTask (dropping the issue out of selectTask's activeIssues guard),
//     and
//  2. book the SAME short non-permanent failure cooldown via recordTaskFailure, so
//     the just-released issue is NOT instantly re-admissible (and thus can't be
//     handed straight back to a stale worker of the same identity), then
//  3. push the EXISTING task_revoke message (already handled by contributor-relay.sh:
//     it clears its local currentTask, stops progress reporting, and sends "ready")
//     so the wedged relay stops cleanly and re-asks for work.
//
// It does NOT mint or rotate any token, does NOT change trust, and does NOT
// implement the deferred lease/generation-token guarantee (a truly stale worker that
// later reconnects and reports completion is out of scope for this slice — the
// cooldown is the safe interim). Synthetic pr-review tasks carry Number == 0 and are
// released without booking an issue-key cooldown, exactly like the disconnect path.
//
// It returns the number of held sessions that were released so the caller can 404 a
// contributor that has no in-flight task (nothing to requeue) versus report success.
func (h *ContributeWSHub) RequeueContributorTask(contributorID string) int {
	if contributorID == "" {
		return 0
	}
	// Collect the connections + their held tasks under the connection lock, but do
	// the network send (task_revoke) OUTSIDE h.mu to avoid holding the hub lock
	// across a socket write, mirroring how the other broadcast-ish paths behave.
	type releaseTarget struct {
		conn *ContributorConnection
		task WSTaskAssign
	}
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
			targets = append(targets, releaseTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()

	for _, tgt := range targets {
		// Book the SAME short cooldown the disconnect/ready-abandon paths book, so
		// the released issue is not instantly re-offered. Only real issue tasks are
		// booked; synthetic pr-review tasks (Number == 0) must not poison an issue key.
		if tgt.task.Number > 0 {
			h.recordTaskFailure(tgt.task.Repo, tgt.task.Number, false)
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		h.logger.Info("[contribute-ws] task requeued by operator",
			"username", username,
			"task", tgt.task.TaskID,
			"repo", tgt.task.Repo,
			"number", tgt.task.Number,
		)
		h.addActivity(username, "requeued by operator", tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.task.TaskID)
		// Push the EXISTING task_revoke message so the relay stops cleanly and
		// re-readies. Best-effort: if the socket is already gone the disconnect path
		// has (or will) release it anyway; the cooldown above is already booked.
		if tgt.conn.ws != nil {
			_ = sendJSON(tgt.conn.ws, WSMessage{
				Type:   "task_revoke",
				Seq:    h.nextSeq(),
				TaskID: tgt.task.TaskID,
				Reason: "requeued by operator",
			})
		}
	}
	return len(targets)
}

func (h *ContributeWSHub) nextSeq() int {
	h.mu.Lock()
	h.seq++
	s := h.seq
	h.mu.Unlock()
	return s
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

// FleetClanker is a read-only view of one connected contributor ("clanker")
// session as the operator-facing Management & Operations tab renders it. It
// carries only what the contributor handshake already put on the wire plus the
// live connection timing the hub already tracks — no secrets, no new state.
type FleetClanker struct {
	ContributorID  string        `json:"contributor_id"`
	GitHubUsername string        `json:"github_username,omitempty"`
	CLIBackend     string        `json:"cli_backend,omitempty"`
	Model          string        `json:"model,omitempty"`
	Role           string        `json:"role,omitempty"`
	TrustTier      string        `json:"trust_tier,omitempty"`
	ConnectedAt    string        `json:"connected_at,omitempty"`
	LastActivity   string        `json:"last_activity,omitempty"`
	Stale          bool          `json:"stale,omitempty"`
	CurrentTask    *WSTaskAssign `json:"current_task,omitempty"`
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
			CLIBackend:   c.cliBackend,
			Model:        c.model,
			Role:         c.role,
			ConnectedAt:  c.connectedAt.UTC().Format(time.RFC3339),
			LastActivity: c.lastPong.UTC().Format(time.RFC3339),
			Stale:        time.Since(c.lastPong) > wsHeartbeatTimeout,
		}
		// #2547: surface the client-declared capabilities read-only (a copy so the
		// snapshot never aliases live connection state). Nil for unversioned clients.
		if c.capabilities != nil {
			capsCopy := *c.capabilities
			fc.Capabilities = &capsCopy
		}
		if c.profile != nil {
			fc.ContributorID = c.profile.ContributorID
			fc.GitHubUsername = c.profile.GitHubUsername
			fc.TrustTier = c.profile.TrustTier
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

func (h *ContributeWSHub) ActiveConnections() []ContributorConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ContributorConnection, 0, len(h.connections))
	for _, c := range h.connections {
		c.mu.Lock()
		out = append(out, ContributorConnection{
			profile:     c.profile,
			cliBackend:  c.cliBackend,
			model:       c.model,
			role:        c.role,
			connectedAt: c.connectedAt,
			currentTask: c.currentTask,
			tmuxOutput:  append([]string{}, c.tmuxOutput...),
		})
		c.mu.Unlock()
	}
	return out
}

const maxWSConnections = 50

func (h *ContributeWSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	count := len(h.connections)
	h.mu.RUnlock()
	if count >= maxWSConnections {
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}

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
			contributor.tokenMintedAt = time.Time{}
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
				// Both sessions then reach "open a PR" and file duplicates. Mirror the
				// task_failed path (#2435) and book the SHORT non-permanent failure
				// cooldown so the issue is not instantly re-admissible. The short
				// window comfortably outlasts the reconnect backoff, so the returning
				// session re-asserts and resumes (repopulating activeIssues) before the
				// cooldown lapses; a completion later resets the failure ledger. Only
				// real issue tasks are booked — synthetic pr-review tasks carry
				// Number == 0 and must not poison an issue key.
				if abandonedTask.Number > 0 {
					h.recordTaskFailure(abandonedTask.Repo, abandonedTask.Number, false)
				}
			}
			h.mu.Lock()
			delete(h.connections, connID)
			h.mu.Unlock()
			h.logger.Info("[contribute-ws] disconnected", "username", contributor.profile.GitHubUsername)
			h.addActivity(contributor.profile.GitHubUsername, "left", contributor.role, contributor.cliBackend, contributor.model, "")
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

			profile.LastActive = time.Now().UTC().Format(time.RFC3339)
			if msg.CLIBackend != "" {
				profile.CLIBackend = msg.CLIBackend
			}
			if msg.Model != "" {
				profile.Model = msg.Model
			}
			if profile.AvatarURL == "" {
				profile.AvatarURL = fmt.Sprintf("https://github.com/%s.png", profile.GitHubUsername)
			}
			if msg.Role != "" {
				profile.PreferredRole = msg.Role
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
			if !declared.IsZero() {
				c := declared
				caps = &c
			}

			contributor = &ContributorConnection{
				ws:           conn,
				profile:      profile,
				cliBackend:   msg.CLIBackend,
				model:        msg.Model,
				role:         msg.Role,
				connectedAt:  time.Now(),
				lastPong:     time.Now(),
				capabilities: caps,
			}

			h.mu.Lock()
			h.connections[connID] = contributor
			h.mu.Unlock()

			var perms []string
			switch profile.TrustTier {
			case "newcomer":
				perms = []string{"issues:write"}
			case "contributor":
				perms = []string{"issues:write", "contents:write", "pulls:write"}
			case "trusted":
				perms = []string{"issues:write", "contents:write", "pulls:write", "checks:read"}
			case "advisor":
				perms = []string{"metadata:read", "pulls:read"}
			default:
				perms = []string{"metadata:read"}
			}

			if err := sendJSON(conn, WSMessage{
				Type:          "auth_ok",
				Seq:           h.nextSeq(),
				ContributorID: profile.ContributorID,
				TrustTier:     profile.TrustTier,
				Permissions:   perms,
				Role:          msg.Role,
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
				"role", msg.Role,
			)
			h.addActivity(profile.GitHubUsername, "joined", msg.Role, msg.CLIBackend, msg.Model, "")

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
			contributor.tokenMintedAt = time.Time{}
			contributor.mu.Unlock()
			if abandoned != nil {
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
					h.recordTaskFailure(abandoned.Repo, abandoned.Number, false)
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
				if err := sendJSON(conn, *task); err != nil {
					h.logger.Warn("[contribute-ws] failed to send task_unavailable", "error", err)
					return
				}
				h.logger.Info("[contribute-ws] task unavailable",
					"username", contributor.profile.GitHubUsername,
					"reason", task.Reason,
				)
			default:
				if err := sendJSON(conn, *task); err != nil {
					h.logger.Warn("[contribute-ws] failed to send task_assign", "error", err)
					return
				}
				taskDesc := fmt.Sprintf("%s %s#%d: %s", task.Kind, task.Repo, task.Number, task.Title)
				h.addActivity(contributor.profile.GitHubUsername, "picked up", contributor.role, contributor.cliBackend, contributor.model, taskDesc)
				h.logger.Info("[contribute-ws] task assigned",
					"username", contributor.profile.GitHubUsername,
					"task", task.TaskID,
					"repo", task.Repo,
					"number", task.Number,
				)
			}

		case "task_accepted":
			// acknowledged

		case "task_progress":
			if contributor != nil {
				contributor.mu.Lock()
				contributor.tmuxOutput = msg.TmuxOutput
				// resumed is true only when this task_progress REBUILT currentTask
				// from nothing — i.e. the relay is re-asserting a task the hub had
				// released on disconnect (the reconnect/resume path), not a routine
				// progress ping for a task the hub already tracks.
				resumed := false
				if contributor.currentTask == nil && msg.TaskID != "" {
					contributor.currentTask = &WSTaskAssign{
						TaskID: msg.TaskID,
						Kind:   msg.Kind,
						// #2644: canonicalise the CLIENT-supplied repo to the same
						// repo.Full form selectTask assigned it under, so the
						// activeIssues double-assign guard and the failure/completion
						// cooldowns — all keyed "repo#number" off currentTask.Repo —
						// match. Storing msg.Repo verbatim let a relay whose repo
						// spelling differs from repo.Full slip its resumed task past
						// the in-flight exclusion, and the SAME issue was handed to a
						// second contributor (intermittent, and only for that repo).
						Repo:   h.canonicalRepoKey(msg.Repo),
						Number: msg.Number,
						Title:  msg.Title,
					}
					resumed = true
				}
				contributor.mu.Unlock()

				// #2610 finding 3: the disconnect defer clears tokenMintedAt, and
				// this resume path historically rebuilt currentTask WITHOUT re-arming
				// it. tokenRefreshDue treats a zero tokenMintedAt as "not due", so
				// maybeRefreshToken on the heartbeat became a permanent no-op for the
				// resumed connection — the task kept the token minted at its original
				// assignment and it expired at wsTokenTTL (55m) with no 50-minute
				// replacement, the exact silent-expiry case the refresh path was added
				// for (#2393 item 2). Re-mint and push a fresh token_refresh here so
				// the resumed session both holds a valid token and re-arms the refresh
				// cycle (resumeTaskToken sets tokenMintedAt on a successful send).
				if resumed {
					h.resumeTaskToken(contributor)
				}
			}

		case "task_complete":
			if contributor != nil {
				contributor.mu.Lock()
				hasTask := contributor.currentTask != nil && contributor.currentTask.TaskID == msg.TaskID
				completedTask := contributor.currentTask
				contributor.currentTask = nil
				// #2539: drop the previewable prompt with the task it belonged to so
				// the ops tab does not show a stale instruction after completion.
				contributor.currentPrompt = ""
				contributor.currentLabels = nil
				contributor.tokenMintedAt = time.Time{}
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
					verifiedPR := ""
					if completedTask != nil && h.verifyReportedPR(completedTask.Repo, msg.PRURL, contributor.profile.GitHubUsername) {
						verifiedPR = msg.PRURL
					}
					if completedTask != nil {
						// #2393 item 7 + #2565: the full week-long cooldown is applied
						// only for a VERIFIED PR; an unverified or no-PR completion gets
						// the short cooldown so the issue is not locked for a week (and,
						// per #2492/#2557, still gets a non-zero cooldown so it is not
						// instantly re-offered in a tight loop).
						h.markTaskCompleted(completedTask.Repo, completedTask.Number, verifiedPR)
					}
					h.addActivity(contributor.profile.GitHubUsername, "completed", contributor.role, contributor.cliBackend, contributor.model, msg.TaskID)
					h.logger.Info("[contribute-ws] task complete",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"result", msg.Result,
						"pr_verified", verifiedPR != "",
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
					contributor.mu.Unlock()
					// #2390-era command center: narrate the promotion as its own
					// activity event so the Operations dev-log and achievement pops
					// (contribute_sse.go broadcast) surface "promoted to contributor".
					// Read-only signalling — it changes no control behaviour and is
					// emitted only on the real newcomer -> contributor transition.
					if promoted {
						h.addActivity(promotedUser, "promoted", "contributor", promotedCLI, promotedModel, "contributor")
					}
					_ = saveContributorProfile(contributor.profile)
				} else {
					h.logger.Warn("[contribute-ws] task_complete for unassigned task ignored",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
					)
				}
			}

		case "task_failed":
			if contributor != nil {
				contributor.mu.Lock()
				hasTask := contributor.currentTask != nil && contributor.currentTask.TaskID == msg.TaskID
				failedTask := contributor.currentTask
				contributor.currentTask = nil
				contributor.tokenMintedAt = time.Time{}
				contributor.mu.Unlock()

				if hasTask {
					if failedTask != nil {
						// #2435: record a short failure cooldown (and advance the
						// consecutive-failure/quarantine counter) so a just-failed
						// issue is not immediately re-admissible and handed straight
						// back out ahead of the rest of the queue. A permanent failure
						// counts more toward the quarantine threshold.
						h.recordTaskFailure(failedTask.Repo, failedTask.Number, msg.Permanent)
					}
					h.addActivity(contributor.profile.GitHubUsername, "failed", contributor.role, contributor.cliBackend, contributor.model, msg.TaskID)
					h.logger.Info("[contribute-ws] task failed",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"reason", msg.Reason,
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
			_ = sendJSON(conn, WSMessage{Type: "pong", Seq: msg.Seq})
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

		if err := sendJSON(c.ws, WSMessage{Type: "ping", Seq: h.nextSeq()}); err != nil {
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
	tier, due := tokenRefreshDue(c, time.Now())
	if !due {
		return
	}

	tok, err := h.mintScopedToken(tier)
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
func (h *ContributeWSHub) resumeTaskToken(c *ContributorConnection) {
	tier := ""
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	tok, err := h.mintScopedToken(tier)
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
// re-mint before wsTokenTTL. It returns the trust tier to mint for. Pure and
// clock-injectable so the timing can be tested without a real clock.
func tokenRefreshDue(c *ContributorConnection, now time.Time) (tier string, due bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask == nil || c.tokenMintedAt.IsZero() {
		return "", false
	}
	if now.Sub(c.tokenMintedAt) < wsTokenRefreshPeriod {
		return "", false
	}
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	return tier, true
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
	if err := sendJSON(c.ws, msg); err != nil {
		return err
	}
	c.mu.Lock()
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()
	return nil
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
				c.ws.Close()
				delete(h.connections, id)
			}
		}
		h.mu.Unlock()
	}
}

// mintScopedToken produces a scoped GitHub token for the given trust tier via
// the GitHub App auth path, falling back to the on-disk cache when no App auth
// is configured (dev/single-token deployments). This is the single mint path
// shared by task_assign and the heartbeat token-refresh, so both advertise
// tokens minted the same way. See #2393 item 2.
func (h *ContributeWSHub) mintScopedToken(tier string) (string, error) {
	if h.server != nil && h.server.deps != nil && h.server.deps.GHAppAuth != nil {
		ctx := h.server.deps.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		return h.server.deps.GHAppAuth.ScopedToken(ctx, tier)
	}
	if tokenBytes, err := os.ReadFile(github.TokenCachePath); err == nil {
		return string(tokenBytes), nil
	}
	// No App auth and no cache: mint nothing rather than fail. Callers treat an
	// empty token as "leave the relay's current token in place".
	return "", nil
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
func buildTaskPrompt(repoFull string, number int, title string) string {
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
		"You are a contributor to the %s hive. Work on issue %s#%d: \"%s\". "+
			"You do NOT have push access to the upstream repo. "+
			"Start by getting a real checkout on disk: "+
			"'gh repo fork %s --clone=true --remote=true "+
			"$HIVE_WORKSPACE_DIR/%s' (or, if that directory already has a clone "+
			"from a prior task, 'cd' into it and 'git fetch' instead of "+
			"re-forking). Then 'cd' into that checkout, read the issue, "+
			"understand what's needed, and take action. "+
			"Push your branch to your fork remote, then open a PR from your fork. "+
			"Use the GH_TOKEN env var for all gh commands (do NOT use 'unset GITHUB_TOKEN').",
		repoFull, repoFull, number, title, repoFull, repoFull,
	)
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
			activeIssues[fmt.Sprintf("%s#%d", conn.currentTask.Repo, conn.currentTask.Number)] = true
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
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
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
	if c.profile != nil {
		ownUsername = c.profile.GitHubUsername
	}

	type candidate struct {
		repoFull string
		number   int
		title    string
		url      string
		labels   []string
		isOwn    bool
		// recentFailures is the issue's current consecutive-failure count (#2435).
		// It is a stable tie-break in the ordering below: among equally-admissible
		// candidates, fewer-recent-failures first. Issues in an active failure
		// cooldown are already excluded above, so this only deprioritises an issue
		// whose short cooldown just elapsed but which still has failure history —
		// a backstop that keeps the queue moving even if the ledger is imperfect.
		recentFailures int
	}
	var candidates []candidate

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

			number := 0
			switch n := issue["number"].(type) {
			case float64:
				number = int(n)
			case int:
				number = n
			}
			if number == 0 {
				h.logger.Info("[contribute-ws] skip: number=0", "repo", repo.Full)
				continue
			}
			if h.isTaskInCooldown(repo.Full, number) {
				continue
			}
			// #2435: skip an issue still inside its short post-failure cooldown or
			// its longer quarantine. This is the primary livelock fix — a failing
			// issue at the head of the scan is no longer instantly re-admissible.
			if h.isTaskInFailureCooldown(repo.Full, number) {
				continue
			}
			if activeIssues[fmt.Sprintf("%s#%d", repo.Full, number)] {
				continue
			}

			title, _ := issue["title"].(string)
			url, _ := issue["url"].(string)
			author, _ := issue["author"].(string)
			labels := stringSliceFromAny(issue["labels"])
			assignees := stringSliceFromAny(issue["assignees"])

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
				title:    title,
				url:      url,
				// The issue's own labels travel with the candidate so the chosen
				// task_assign can populate the Labels envelope field (kubestellar/
				// hive#2393 item 8). They're already computed for filtering above.
				labels: labels,
				// "Own work" is the only #2390 signal available today: the
				// candidate was authored by the connected contributor. When the
				// username is unknown (empty), nothing is own → we keep today's
				// ordering untouched.
				isOwn: ownUsername != "" && strings.EqualFold(author, ownUsername),
				// #2435: carry any lingering failure history so the ordering below
				// can deprioritise a recently-failed issue within its bucket.
				recentFailures: h.recentFailureCount(repo.Full, number),
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
	// arms the refresh ticker for the token we hand out here.
	ghToken, err := h.mintScopedToken(c.profile.TrustTier)
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

	taskID := fmt.Sprintf("ct-%s-%d-%d", chosen.repoFull, chosen.number, time.Now().Unix())

	// #2539: build the prompt through the shared, credential-free buildTaskPrompt
	// so the exact text shipped in task_assign below can also be PREVIEWED
	// read-only in the ops tab. The prompt is a pure function of task metadata —
	// the minted github_token is attached to the WSMessage separately (never inside
	// the prompt), so previewing the prompt can never leak the token. buildTaskPrompt
	// itself carries the #2545 workspace-clone instruction (real checkout into
	// $HIVE_WORKSPACE_DIR rather than a fork-only --clone=false).
	prompt := buildTaskPrompt(chosen.repoFull, chosen.number, chosen.title)

	c.mu.Lock()
	c.currentTask = &WSTaskAssign{
		TaskID: taskID,
		Kind:   "issue",
		Repo:   chosen.repoFull,
		Number: chosen.number,
		Title:  chosen.title,
	}
	// Store the prompt (never the token) so FleetSnapshot can preview it (#2539),
	// and clear any stale idle reason now that this connection has real work.
	c.currentPrompt = prompt
	c.currentLabels = chosen.labels
	c.lastIdleReason = ""
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()

	// #2566: record this assignment against the identity's rolling hourly/daily
	// windows so the next selectTask enforces tier_limits.max_per_hour /
	// max_per_day. Recorded here — after the task is committed to the connection
	// and we are certain a task_assign will ship — so a refused pass (which returns
	// early above) never consumes a slot. Uses the same identity key as the
	// concurrency gate.
	h.recordAssignment(identityOf(c), time.Now())

	return &WSMessage{
		Type:           "task_assign",
		Seq:            h.nextSeq(),
		TaskID:         taskID,
		Kind:           "issue",
		Repo:           chosen.repoFull,
		Number:         chosen.number,
		Title:          chosen.title,
		URL:            chosen.url,
		GitHubToken:    ghToken,
		TokenExpiresAt: time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339),
		Prompt:         prompt,
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

func sendJSON(conn *websocket.Conn, msg WSMessage) error {
	return conn.WriteJSON(msg)
}

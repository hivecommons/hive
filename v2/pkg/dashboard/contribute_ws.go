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
	mu            sync.Mutex
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
	Restrictions   json.RawMessage `json:"restrictions,omitempty"`
	Role           string          `json:"role,omitempty"`
	ContribLabels  []string        `json:"contributor_labels,omitempty"`
	Status         string          `json:"status,omitempty"`
	Result         string          `json:"result,omitempty"`
	Summary        string          `json:"summary,omitempty"`
	TmuxOutput     []string        `json:"tmux_output,omitempty"`
	AcceptedModels []string        `json:"accepted_models,omitempty"`
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
	completedMu        sync.Mutex
	selectMu           sync.Mutex
}

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

func NewContributeWSHub(logger *slog.Logger, server *Server) *ContributeWSHub {
	hub := &ContributeWSHub{
		connections:           make(map[string]*ContributorConnection),
		completedTasks:        make(map[string]time.Time),
		completedTaskCooldown: make(map[string]time.Duration),
		completedTaskPRURL:    make(map[string]string),
		logger:                logger,
		server:                server,
	}
	hub.loadCompletedTasks()
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
	defer h.activityMu.Unlock()
	if len(h.activity) > 0 && (action == "joined" || action == "left") {
		last := h.activity[len(h.activity)-1]
		if last.Username == username && last.Action == action {
			if t, err := time.Parse(time.RFC3339, last.Timestamp); err == nil && time.Since(t) < activityDebounceSecs*time.Second {
				return
			}
		}
	}
	h.activity = append(h.activity, ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Username:  username,
		Action:    action,
		Role:      role,
		CLI:       cli,
		Model:     model,
		Task:      task,
	})
	if len(h.activity) > maxActivityEntries {
		h.activity = h.activity[len(h.activity)-maxActivityEntries:]
	}
	go h.saveActivity()
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
		cooldown = completedTaskCooldownHours * time.Hour
	}
	h.completedMu.Lock()
	h.completedTasks[key] = time.Now()
	if h.completedTaskCooldown != nil {
		h.completedTaskCooldown[key] = cooldown
	}
	if h.completedTaskPRURL != nil {
		h.completedTaskPRURL[key] = prURL
	}
	h.completedMu.Unlock()
	h.saveCompletedTasks()
}

// cooldownForLocked returns the cooldown duration to apply to key. Callers must
// already hold completedMu. When no per-task override was recorded (older
// on-disk entries, or hubs built directly in tests) it falls back to the full
// completedTaskCooldownHours — the original, conservative default.
func (h *ContributeWSHub) cooldownForLocked(key string) time.Duration {
	if h.completedTaskCooldown != nil {
		if d, ok := h.completedTaskCooldown[key]; ok {
			return d
		}
	}
	return completedTaskCooldownHours * time.Hour
}

func (h *ContributeWSHub) isTaskInCooldown(repo string, number int) bool {
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

			contributor = &ContributorConnection{
				ws:          conn,
				profile:     profile,
				cliBackend:  msg.CLIBackend,
				model:       msg.Model,
				role:        msg.Role,
				connectedAt: time.Now(),
				lastPong:    time.Now(),
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
			contributor.mu.Unlock()
			if abandoned != nil {
				h.logger.Warn("[contribute-ws] task abandoned without completion",
					"username", contributor.profile.GitHubUsername,
					"abandoned_task", abandoned.TaskID,
				)
			}
			h.logger.Info("[contribute-ws] ready for work",
				"username", contributor.profile.GitHubUsername,
				"role", contributor.role,
			)
			task := h.selectTask(contributor)
			if task != nil {
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
			} else {
				h.logger.Info("[contribute-ws] no tasks available",
					"username", contributor.profile.GitHubUsername,
				)
			}

		case "task_accepted":
			// acknowledged

		case "task_progress":
			if contributor != nil {
				contributor.mu.Lock()
				contributor.tmuxOutput = msg.TmuxOutput
				if contributor.currentTask == nil && msg.TaskID != "" {
					contributor.currentTask = &WSTaskAssign{
						TaskID: msg.TaskID,
						Kind:   msg.Kind,
						Repo:   msg.Repo,
						Number: msg.Number,
						Title:  msg.Title,
					}
				}
				contributor.mu.Unlock()
			}

		case "task_complete":
			if contributor != nil {
				contributor.mu.Lock()
				hasTask := contributor.currentTask != nil && contributor.currentTask.TaskID == msg.TaskID
				completedTask := contributor.currentTask
				contributor.currentTask = nil
				contributor.tokenMintedAt = time.Time{}
				contributor.tmuxOutput = msg.TmuxOutput
				contributor.mu.Unlock()

				if hasTask {
					if completedTask != nil {
						// #2393 item 7: keep the full week-long cooldown only when a
						// PR was actually reported; an idle-but-no-PR completion gets
						// the short cooldown so the issue is not locked for a week.
						h.markTaskCompleted(completedTask.Repo, completedTask.Number, msg.PRURL)
					}
					h.addActivity(contributor.profile.GitHubUsername, "completed", contributor.role, contributor.cliBackend, contributor.model, msg.TaskID)
					h.logger.Info("[contribute-ws] task complete",
						"username", contributor.profile.GitHubUsername,
						"task", msg.TaskID,
						"result", msg.Result,
					)
					contributor.mu.Lock()
					contributor.profile.TasksCompleted++
					contributor.profile.LastActive = time.Now().UTC().Format(time.RFC3339)
					if completedTask != nil {
						contributor.profile.LastCompletedTask = completedTask
					}
					if contributor.profile.TrustTier == "newcomer" && contributor.profile.TasksCompleted >= contributorAutoPromoteAt {
						contributor.profile.TrustTier = "contributor"
						h.logger.Info("[contribute-ws] auto-promoted", "username", contributor.profile.GitHubUsername)
					}
					contributor.mu.Unlock()
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
				contributor.currentTask = nil
				contributor.tokenMintedAt = time.Time{}
				contributor.mu.Unlock()

				if hasTask {
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

func (h *ContributeWSHub) selectTask(c *ContributorConnection) *WSMessage {
	h.selectMu.Lock()
	defer h.selectMu.Unlock()

	if h.server == nil {
		return nil
	}
	if h.server.deps != nil && h.server.deps.Config != nil && h.server.deps.Config.Hub.ContributeSuspended {
		return nil
	}

	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()

	if status == nil {
		return nil
	}

	activeIssues := make(map[string]bool)
	h.mu.RLock()
	for _, conn := range h.connections {
		conn.mu.Lock()
		if conn.currentTask != nil {
			activeIssues[fmt.Sprintf("%s#%d", conn.currentTask.Repo, conn.currentTask.Number)] = true
		}
		conn.mu.Unlock()
	}
	h.mu.RUnlock()

	totalAvailable := 0
	for _, repo := range status.Repos {
		totalAvailable += len(repo.ActionableIssues)
	}
	h.logger.Info("[contribute-ws] selectTask scanning", "repos", len(status.Repos), "totalIssues", totalAvailable, "cooldown", len(h.completedTasks), "active", len(activeIssues))

	var disabledRepos []string
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
	}

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

			ghToken, err := h.mintScopedToken(c.profile.TrustTier)
			if err != nil {
				h.logger.Warn("[contribute-ws] failed to mint scoped token — skipping task",
					"tier", c.profile.TrustTier, "error", err)
				return nil
			}

			taskID := fmt.Sprintf("ct-%s-%d-%d", repo.Full, number, time.Now().Unix())

			prompt := fmt.Sprintf(
				"You are a contributor to the %s hive. Work on issue %s#%d: \"%s\". "+
					"Read the issue, understand what's needed, and take action. "+
					"You do NOT have push access to the upstream repo. "+
					"Fork it first with 'gh repo fork %s --clone=false', "+
					"add the fork as a remote, push your branch there, "+
					"then open a PR from your fork. "+
					"Use the GH_TOKEN env var for all gh commands (do NOT use 'unset GITHUB_TOKEN').",
				repo.Full, repo.Full, number, title, repo.Full,
			)

			c.mu.Lock()
			c.currentTask = &WSTaskAssign{
				TaskID: taskID,
				Kind:   "issue",
				Repo:   repo.Full,
				Number: number,
				Title:  title,
			}
			c.tokenMintedAt = time.Now()
			c.mu.Unlock()

			return &WSMessage{
				Type:           "task_assign",
				Seq:            h.nextSeq(),
				TaskID:         taskID,
				Kind:           "issue",
				Repo:           repo.Full,
				Number:         number,
				Title:          title,
				URL:            url,
				GitHubToken:    ghToken,
				TokenExpiresAt: time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339),
				Prompt:         prompt,
				// The issue's own labels — the Labels envelope field was declared but
				// never populated, so a client reading it got nothing (kubestellar/
				// hive#2393 item 8). They're already computed for filtering above.
				Labels:        labels,
				ContribLabels: []string{"contributor/" + c.profile.GitHubUsername},
			}
		}
	}

	return nil
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

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
)

const (
	wsHeartbeatInterval  = 30 * time.Second
	wsHeartbeatTimeout   = 90 * time.Second
	wsTaskTimeout        = 30 * time.Minute
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
	mu          sync.Mutex
}

type WSMessage struct {
	Type              string          `json:"type"`
	Seq               int             `json:"seq,omitempty"`
	Nonce             string          `json:"nonce,omitempty"`
	ContributorID     string          `json:"contributor_id,omitempty"`
	TrustTier         string          `json:"trust_tier,omitempty"`
	Permissions       []string        `json:"permissions,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	RegistrationToken string          `json:"registration_token,omitempty"`
	CLIBackend        string          `json:"cli_backend,omitempty"`
	Model             string          `json:"model,omitempty"`
	TaskID            string          `json:"task_id,omitempty"`
	Kind              string          `json:"kind,omitempty"`
	Repo              string          `json:"repo,omitempty"`
	Number            int             `json:"number,omitempty"`
	Title             string          `json:"title,omitempty"`
	URL               string          `json:"url,omitempty"`
	Labels            []string        `json:"labels,omitempty"`
	Prompt            string          `json:"prompt,omitempty"`
	GitHubToken       string          `json:"github_token,omitempty"`
	TokenExpiresAt    string          `json:"token_expires_at,omitempty"`
	Restrictions      json.RawMessage `json:"restrictions,omitempty"`
	Role              string          `json:"role,omitempty"`
	ContribLabels     []string        `json:"contributor_labels,omitempty"`
	Status            string          `json:"status,omitempty"`
	Result            string          `json:"result,omitempty"`
	Summary           string          `json:"summary,omitempty"`
	TmuxOutput        []string        `json:"tmux_output,omitempty"`
	AcceptedModels    []string        `json:"accepted_models,omitempty"`
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
	completedMu    sync.Mutex
	selectMu       sync.Mutex
}

const completedTaskCooldownHours = 168

func NewContributeWSHub(logger *slog.Logger, server *Server) *ContributeWSHub {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
		server:         server,
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

func (h *ContributeWSHub) loadCompletedTasks() {
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(completedTasksFile)
	if err != nil {
		return
	}
	var saved map[string]string
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	for k, v := range saved {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			if time.Since(t) < completedTaskCooldownHours*time.Hour {
				h.completedTasks[k] = t
			}
		}
	}
	h.logger.Info("[contribute-ws] loaded completed tasks", "count", len(h.completedTasks))
}

func (h *ContributeWSHub) saveCompletedTasks() {
	h.completedMu.Lock()
	saved := make(map[string]string, len(h.completedTasks))
	for k, t := range h.completedTasks {
		saved[k] = t.Format(time.RFC3339)
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

func (h *ContributeWSHub) markTaskCompleted(repo string, number int) {
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	h.completedTasks[key] = time.Now()
	h.completedMu.Unlock()
	h.saveCompletedTasks()
}

func (h *ContributeWSHub) isTaskInCooldown(repo string, number int) bool {
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.completedTasks[key]
	if !ok {
		return false
	}
	if time.Since(t) > completedTaskCooldownHours*time.Hour {
		delete(h.completedTasks, key)
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
				contributor.tmuxOutput = msg.TmuxOutput
				contributor.mu.Unlock()

				if hasTask {
					if completedTask != nil {
						h.markTaskCompleted(completedTask.Repo, completedTask.Number)
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

		if err := sendJSON(c.ws, WSMessage{Type: "ping", Seq: h.nextSeq()}); err != nil {
			h.logger.Info("[contribute-ws] heartbeat ping failed, closing", "username", c.profile.GitHubUsername)
			c.ws.Close()
			return
		}
	}
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
			}

			ghToken := ""
			if h.server.deps != nil && h.server.deps.GHAppAuth != nil {
				ctx := h.server.deps.Ctx
				if ctx == nil {
					ctx = context.Background()
				}
				if tok, err := h.server.deps.GHAppAuth.ScopedToken(ctx, c.profile.TrustTier); err == nil {
					ghToken = tok
				} else {
					h.logger.Warn("[contribute-ws] failed to mint scoped token — skipping task",
						"tier", c.profile.TrustTier, "error", err)
					return nil
				}
			} else if tokenBytes, err := os.ReadFile("/var/run/hive-metrics/gh-app-token.cache"); err == nil {
				ghToken = string(tokenBytes)
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
				TokenExpiresAt: time.Now().Add(55 * time.Minute).UTC().Format(time.RFC3339),
				Prompt:         prompt,
				ContribLabels:  []string{"contributor/" + c.profile.GitHubUsername},
			}
		}
	}

	return nil
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

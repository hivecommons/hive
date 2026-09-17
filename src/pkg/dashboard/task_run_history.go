package dashboard

// Per-contributor run history over the durable task-run log (hive#7317).
//
// task_run_log.go already persists one TaskRunRecord per terminal task report
// — outcome, failure_kind, reason, duration, scenario — but the only read path
// was /api/contribute/run-stats, which serves per-backend AGGREGATES. The
// reason strings an operator needs to answer "why is this contributor
// struggling?" were in the file and unreachable from the dashboard/API.
//
// This file adds the missing per-run read: GET /api/contribute/runs?username=
// returns the raw records for one contributor, newest first. Same posture as
// the sibling reads: the reason text served here is the same bounded string
// GET /api/contribute/fleet already exposes as last_failure.reason, and the
// usernames are the same ones the activity feed already names — no new class
// of data becomes public.

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Synthetic reasons for hand-backs (hive#7317 item 2). A task that ends via
// the relay re-sending "ready" while still holding it, or via the socket
// dropping, never produces a task_failed — so until these records existed a
// session could hand back 10 of 11 tasks and leave one row in the run log.
// Closed vocabulary like the scenario strings: never rename.
const (
	taskRunReasonReadvertised   = "abandoned: relay re-advertised ready"
	taskRunReasonConnectionLost = "abandoned: connection lost"
)

// appendAbandonedTaskRun records a hand-back as a durable run record. DECLARE
// only, like every other appendTaskRun call: cooldown/quarantine booking stays
// with recordTaskFailureForTask / bookReleaseCooldown at the call sites.
func (h *ContributeWSHub) appendAbandonedTaskRun(c *ContributorConnection, task *WSTaskAssign, assignedAt time.Time, reason string) {
	if c == nil || c.profile == nil || task == nil {
		return
	}
	provider := ""
	if c.cliBackend == "pi" {
		provider, _, _ = strings.Cut(c.model, "/")
	}
	rec := TaskRunRecord{
		TaskID:   task.TaskID,
		Username: c.profile.GitHubUsername,
		Backend:  c.cliBackend,
		Provider: provider,
		Model:    c.model,
		Effort:   c.reasoningEffort,
		Role:     c.role,
		Outcome:  outcomeAbandoned,
		Reason:   reason,
		Repo:     task.Repo,
		Number:   task.Number,
	}
	if !assignedAt.IsZero() {
		rec.DurationS = time.Since(assignedAt).Seconds()
	}
	h.appendTaskRun(rec)
}

// taskRunHistoryMaxLimit bounds ?limit= so one request cannot serialize an
// arbitrarily large slice of the log.
const taskRunHistoryMaxLimit = 500

// readTaskRuns returns one contributor's records from the live log (rotated
// history deliberately excluded, matching readTaskRunStats), newest first,
// capped at limit. The second return is the total number of matching records
// in the window, so a truncated response is detectable.
func readTaskRuns(path, username string, window time.Duration, limit int) ([]TaskRunRecord, int, error) {
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskRunRecord{}, 0, nil
		}
		return nil, 0, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	var matched []TaskRunRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TaskRunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // a torn tail line must not poison the read
		}
		if cutoff != "" && rec.TS < cutoff { // RFC3339 is lexically sortable
			continue
		}
		if !strings.EqualFold(rec.Username, username) {
			continue
		}
		matched = append(matched, rec)
	}
	total := len(matched)
	// The file is append-ordered oldest→newest; serve newest first.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	if matched == nil {
		matched = []TaskRunRecord{}
	}
	return matched, total, nil
}

// handleContributeRuns serves GET /api/contribute/runs?username=<u>&days=N&limit=M:
// one contributor's terminal task reports from the durable run log, newest
// first. This is the per-run read /api/contribute/run-stats deliberately is
// not — it exists so an operator can see WHY a struggling contributor's tasks
// ended (reason, failure_kind, scenario, duration) without shell access to the
// hub (hive#7317). Public read like the sibling /api/contribute* GETs; the
// fields served are the ones the fleet view and activity feed already expose.
func (s *Server) handleContributeRuns(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	if username == "" {
		http.Error(w, "username query parameter is required", http.StatusBadRequest)
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 365 {
			days = n
		}
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= taskRunHistoryMaxLimit {
			limit = n
		}
	}
	runs, total, err := readTaskRuns(taskRunLogPath, username, time.Duration(days)*24*time.Hour, limit)
	if err != nil {
		http.Error(w, "task-run log unreadable", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"username":    username,
		"window_days": days,
		"total":       total,
		"runs":        runs,
	})
}

package dashboard

// Durable per-run contributor task telemetry (the DECLARE half only).
//
// Every accepted task_complete / task_failed already carries a small closed
// vocabulary — completion_signal (#5376), the task-failure kind (#2547), the
// no_work_needed verdict (#3987) — but until now none of it survived anywhere
// an operator could aggregate: the slog line rotates away, activity.json is
// capped at maxActivityEntries and drops reason/kind entirely, and nothing
// records how long a run took. So "which backend completes on its verdict vs
// the chrome-idle fallback" and "which backends fail on their environment" —
// the exact questions the backend smoke (bin/test_backend_smoke.sh) asks
// synthetically — were unanswerable for real fleet traffic.
//
// This file appends one JSONL record per terminal task report to
// /data/contributors/task_runs.jsonl and derives a predefined `scenario` from
// the already-normalized fields, so the record set can be ratcheted over time
// (watch idle_complete and env_failure trend down per backend).
//
// DECLARE, never ROUTE: nothing here influences cooldowns, trust, routing, or
// offers — the same boundary contribute_protocol.go draws for the failure
// kind. Writes are best-effort; a telemetry failure never fails a task.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const taskRunLogFileName = "task_runs.jsonl"

// taskRunLogPath is where terminal task reports are appended when no hub/server
// override is available. A var (not const) so tests point it at a scratch file.
var taskRunLogPath = filepath.Join(defaultContributorsDir, taskRunLogFileName)

// taskRunLogMaxBytes bounds the live file. On overflow the file is rotated to
// a single ".1" predecessor (replacing any previous one), so disk use is
// capped at ~2× this regardless of fleet size or uptime.
const taskRunLogMaxBytes = 10 << 20

var taskRunMu sync.Mutex

// The predefined scenario vocabulary. Closed and stable by design: these
// strings are the ratchet axis, so week-over-week comparison depends on their
// spelling never changing. Extend by adding values; never rename.
const (
	// scenarioVerdictComplete: the agent ended the task with its own
	// HIVE_VERDICT sentinel — the completion contract working as designed.
	scenarioVerdictComplete = "verdict_complete"
	// scenarioIdleComplete: the task completed only because the chrome-idle
	// fallback fired — the sentinel contract is NOT being honored for this
	// backend. This is the primary ratchet metric.
	scenarioIdleComplete = "idle_complete"
	// scenarioHeadlessComplete: completed with no completion_signal on the
	// wire — the headless one-shot path (whose completion IS the exit code),
	// or a relay predating #5376.
	scenarioHeadlessComplete = "headless_complete"
	// scenarioEnvFailure: the client's runtime could not run the work — the
	// broken-backend-integration class the backend smoke exists to catch.
	scenarioEnvFailure = "env_failure"
	// scenarioTaskFailure: the work was attempted and failed on its merits.
	scenarioTaskFailure = "task_failure"
	// scenarioUnspecifiedFailure: a failure with no usable kind (older relay,
	// or an unrecognized value).
	scenarioUnspecifiedFailure = "unspecified_failure"
)

// TaskRunRecord is one terminal task report, flattened to the fields an
// operator aggregates on. All enum-ish fields hold hub-NORMALIZED values
// (normalizeCompletionSignal, NormalizeTaskFailureKind,
// normalizeCompletionVerdict) — client free text never lands here except the
// bounded failure reason, which the fleet view already displays as-is.
type TaskRunRecord struct {
	TS       string `json:"ts"`
	TaskID   string `json:"task_id"`
	TaskGen  uint64 `json:"task_gen,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Number   int    `json:"number,omitempty"`
	Username string `json:"username"`
	Backend  string `json:"backend"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Role     string `json:"role,omitempty"`
	// Outcome is "completed" or "failed" — which terminal message arrived.
	Outcome          string  `json:"outcome"`
	CompletionSignal string  `json:"completion_signal,omitempty"`
	Verdict          string  `json:"verdict,omitempty"`
	VerdictReason    string  `json:"verdict_reason,omitempty"`
	FailureKind      string  `json:"failure_kind,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	Permanent        bool    `json:"permanent,omitempty"`
	DurationS        float64 `json:"duration_s,omitempty"`
	PRURL            string  `json:"pr_url,omitempty"`
	PRVerified       bool    `json:"pr_verified,omitempty"`
	Scenario         string  `json:"scenario"`
	// Session mirrors TaskID for now: the correlation key reserved by
	// github.InvocationMeta.Session, so a PR trailer can one day join back to
	// this record without a format change.
	Session string `json:"session,omitempty"`
}

// deriveScenario maps a terminal report's normalized fields onto the closed
// scenario vocabulary above. Pure; table-tested.
func deriveScenario(outcome, completionSignal, failureKind string) string {
	if outcome == "completed" {
		switch completionSignal {
		case completionSignalVerdict:
			return scenarioVerdictComplete
		case completionSignalChromeIdle:
			return scenarioIdleComplete
		default:
			return scenarioHeadlessComplete
		}
	}
	switch failureKind {
	case TaskFailureKindEnvironment:
		return scenarioEnvFailure
	case TaskFailureKindTask:
		return scenarioTaskFailure
	default:
		return scenarioUnspecifiedFailure
	}
}

// appendTaskRun stamps, classifies, and appends one record. Best-effort by
// contract: every failure path logs and returns — a task must never fail (or
// block its read loop meaningfully) on telemetry.
func (h *ContributeWSHub) appendTaskRun(rec TaskRunRecord) {
	rec.TS = time.Now().UTC().Format(time.RFC3339)
	rec.Scenario = deriveScenario(rec.Outcome, rec.CompletionSignal, rec.FailureKind)
	if rec.Session == "" {
		rec.Session = rec.TaskID
	}
	data, err := json.Marshal(rec)
	if err != nil {
		if h != nil && h.logger != nil {
			h.logger.Warn("[contribute-ws] task-run record marshal failed", "error", err)
		}
		return
	}

	taskRunMu.Lock()
	defer taskRunMu.Unlock()
	path := h.taskRunLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		if h != nil && h.logger != nil {
			h.logger.Warn("[contribute-ws] task-run log directory creation failed", "error", err)
		}
		return
	}
	// Rotate BEFORE appending so the live file never exceeds the cap by more
	// than one record.
	if st, err := os.Stat(path); err == nil && st.Size() >= taskRunLogMaxBytes {
		_ = os.Rename(path, path+".1") // replaces any previous .1: bounded at 2 files
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if h != nil && h.logger != nil {
			h.logger.Warn("[contribute-ws] task-run log open failed", "error", err)
		}
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil && h != nil && h.logger != nil {
		h.logger.Warn("[contribute-ws] task-run log write failed", "error", err)
	}
}

// taskRunBackendStats is the per-backend aggregate served by
// /api/contribute/run-stats. Aggregates only — no usernames, matching the
// public read-only posture of the other /api/contribute* GETs.
type taskRunBackendStats struct {
	Backend   string         `json:"backend"`
	Total     int            `json:"total"`
	Completed int            `json:"completed"`
	Failed    int            `json:"failed"`
	Scenarios map[string]int `json:"scenarios"`
	// ChromeIdleShare is idle_complete / completed — the sentinel
	// non-compliance rate, the number to ratchet toward zero.
	ChromeIdleShare float64 `json:"chrome_idle_share"`
	DurationP50S    float64 `json:"duration_p50_s,omitempty"`
	DurationP95S    float64 `json:"duration_p95_s,omitempty"`
}

// readTaskRunStats aggregates the live log (rotated history is deliberately
// excluded — the endpoint answers "recently", the files answer "ever") over
// the trailing window. window <= 0 means everything in the file.
func readTaskRunStats(path string, window time.Duration) ([]taskRunBackendStats, int, error) {
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return []taskRunBackendStats{}, 0, nil
		}
		return nil, 0, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	byBackend := map[string]*taskRunBackendStats{}
	durations := map[string][]float64{}
	total := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TaskRunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // a torn tail line must not poison the aggregate
		}
		if cutoff != "" && rec.TS < cutoff { // RFC3339 is lexically sortable
			continue
		}
		total++
		b := rec.Backend
		if b == "" {
			b = "unknown"
		}
		st := byBackend[b]
		if st == nil {
			st = &taskRunBackendStats{Backend: b, Scenarios: map[string]int{}}
			byBackend[b] = st
		}
		st.Total++
		st.Scenarios[rec.Scenario]++
		if rec.Outcome == "completed" {
			st.Completed++
		} else {
			st.Failed++
		}
		if rec.DurationS > 0 {
			durations[b] = append(durations[b], rec.DurationS)
		}
	}
	out := make([]taskRunBackendStats, 0, len(byBackend))
	for b, st := range byBackend {
		if st.Completed > 0 {
			st.ChromeIdleShare = float64(st.Scenarios[scenarioIdleComplete]) / float64(st.Completed)
		}
		if ds := durations[b]; len(ds) > 0 {
			sort.Float64s(ds)
			st.DurationP50S = ds[len(ds)/2]
			st.DurationP95S = ds[(len(ds)*95)/100]
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out, total, nil
}

func (h *ContributeWSHub) taskRunLogPath() string {
	if h != nil && h.taskRunLogFile != "" {
		return h.taskRunLogFile
	}
	return taskRunLogPath
}

// handleContributeRunStats serves GET /api/contribute/run-stats: per-backend
// scenario counts, completion-signal compliance, and duration percentiles over
// a trailing window (?days=N, default 7, 0 = everything in the live log).
// Public read-only like the sibling /api/contribute* GETs — aggregates only,
// no usernames, no reasons, no tokens.
func (s *Server) handleContributeRunStats(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 365 {
			days = n
		}
	}
	path := taskRunLogPath
	if s != nil && s.contributeHub != nil {
		path = s.contributeHub.taskRunLogPath()
	} else if s != nil {
		path = filepath.Join(s.contributorsDirOrDefault(), taskRunLogFileName)
	}
	stats, total, err := readTaskRunStats(path, time.Duration(days)*24*time.Hour)
	if err != nil {
		http.Error(w, "task-run log unreadable", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"window_days": days,
		"total":       total,
		"backends":    stats,
	})
}

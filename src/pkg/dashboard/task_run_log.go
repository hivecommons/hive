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

// taskRunLogPath is where terminal task reports are appended, one JSON object
// per line. A var (not const) so tests point it at a scratch file.
var taskRunLogPath = "/data/contributors/task_runs.jsonl"

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
	// scenarioAbandonedHandback: the relay asked for new work while still
	// holding a task (contribute_ws.go's `ready` path). No terminal report ever
	// arrived, so before #7317 this produced no record at all — which is why a
	// session that handed eleven tasks back in a row showed one row here.
	scenarioAbandonedHandback = "abandoned_handback"
	// scenarioAbandonedDisconnect: the socket dropped while a task was held and
	// no live connection re-adopted it. Same invisibility as above.
	scenarioAbandonedDisconnect = "abandoned_disconnect"
	// scenarioAbandonedOther: defensive backstop for an abandonment cause this
	// file does not know. Unreachable from the two call sites today; here so an
	// unrecognized cause is visible as itself rather than silently filed as one
	// of the two above.
	scenarioAbandonedOther = "abandoned_other"
)

// Terminal outcomes. "completed" and "failed" are the two terminal REPORTS a
// relay can send; "abandoned" is the hub's own observation that a held task
// ended without either (#7317). Kept distinct rather than folded into "failed"
// because #4260 established that a dropped socket is not a failure of the work
// — booking it as one is what turned three dropped sockets into a quarantine of
// an issue nobody had failed. The run log inherits that distinction so
// run-stats can count abandonments without inflating the failure rate the
// backend ratchet reads.
const (
	outcomeCompleted = "completed"
	outcomeFailed    = "failed"
	outcomeAbandoned = "abandoned"
)

// Why a held task ended without a terminal report. Hub-OBSERVED, never client
// reported — which is what separates these from FailureKind, whose values are
// self-reported and advisory.
const (
	// abandonCauseHandback: a `ready` arrived while the task was still held.
	abandonCauseHandback = "handback"
	// abandonCauseDisconnect: the read loop ended with the task still held.
	abandonCauseDisconnect = "disconnect"
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
	// Outcome is "completed", "failed" (which terminal message arrived) or
	// "abandoned" (none did — see the outcome constants).
	Outcome string `json:"outcome"`
	// AbandonCause is set only when Outcome is "abandoned": "handback" or
	// "disconnect". Hub-observed; it is what deriveScenario keys on, so the
	// scenario never has to be parsed back out of Reason's free text.
	AbandonCause     string  `json:"abandon_cause,omitempty"`
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

// deriveScenario maps a run's normalized fields onto the closed scenario
// vocabulary above. Pure; table-tested.
func deriveScenario(outcome, completionSignal, failureKind, abandonCause string) string {
	if outcome == outcomeAbandoned {
		switch abandonCause {
		case abandonCauseHandback:
			return scenarioAbandonedHandback
		case abandonCauseDisconnect:
			return scenarioAbandonedDisconnect
		default:
			return scenarioAbandonedOther
		}
	}
	if outcome == outcomeCompleted {
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
	rec.Scenario = deriveScenario(rec.Outcome, rec.CompletionSignal, rec.FailureKind, rec.AbandonCause)
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
	path := taskRunLogPath
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
	Backend   string `json:"backend"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
	// Abandoned counts runs that ended with no terminal report (#7317). Its own
	// bucket rather than part of Failed: these rows are NEW to the log, and
	// folding them into Failed would have moved every backend's failure rate on
	// the day they started being written, for no change in behaviour.
	Abandoned int            `json:"abandoned"`
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
		switch rec.Outcome {
		case outcomeCompleted:
			st.Completed++
		case outcomeAbandoned:
			st.Abandoned++
		default:
			st.Failed++
		}
		// Abandonment durations are deliberately NOT in the percentile pool.
		// They measure how long the hub waited for a report that never came —
		// on the session that prompted #7317 that was 26 minutes of relay
		// pane-stall timeout, which would have dragged the backend's p50 up
		// without anything about its real run times having changed. The
		// per-run endpoint surfaces them individually, where the number means
		// what an operator reading it thinks it means.
		if rec.DurationS > 0 && rec.Outcome != outcomeAbandoned {
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
	stats, total, err := readTaskRunStats(taskRunLogPath, time.Duration(days)*24*time.Hour)
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

// taskRunsMaxLimit bounds one /api/contribute/runs response. The live log is
// capped at taskRunLogMaxBytes, so an unbounded read is already bounded — this
// bounds the RESPONSE, which is what an operator's browser has to render.
const taskRunsMaxLimit = 500

// readTaskRunsForUser returns one user's runs from the live log, newest first,
// at most limit of them.
//
// Same live-log-only scope as readTaskRunStats (the endpoint answers
// "recently", the files answer "ever") and the same torn-line tolerance: a
// half-written tail line is skipped, never fatal, because this endpoint is
// most useful exactly while a contributor is actively writing to the log.
//
// The username match is exact and case-insensitive: GitHub logins are
// case-preserving but case-insensitive, and an operator pasting a login from
// the activity rail should not get an empty answer over capitalization.
func readTaskRunsForUser(path, username string, window time.Duration, limit int) ([]TaskRunRecord, error) {
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskRunRecord{}, nil
		}
		return nil, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	want := strings.ToLower(strings.TrimSpace(username))
	out := []TaskRunRecord{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TaskRunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if cutoff != "" && rec.TS < cutoff { // RFC3339 is lexically sortable
			continue
		}
		if want != "" && strings.ToLower(rec.Username) != want {
			continue
		}
		out = append(out, rec)
	}
	// Newest first: an operator opening this is asking "what just happened",
	// and the answer is at the end of an append-only file.
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// handleContributeRuns serves GET /api/contribute/runs?username=<u>&days=N&limit=N:
// the per-run history behind the aggregates in run-stats (#7317).
//
// This is the read path task_run_log.go was missing. Every field it returns was
// already being written to the log on every terminal report; there was simply
// no endpoint that served a single record, so the `reason` on a failure — the
// one string that says WHY a contributor is struggling — was reachable only by
// reading the file on the hub host. That is precisely the access a hosted-hive
// operator does not have.
//
// Public read-only like the sibling /api/contribute* GETs. That posture is
// inherited rather than chosen: the username is already public on
// /api/contribute/activity and the leaderboard, and `reason` is already served
// as-is on /api/contribute/fleet's last_failure for a CONNECTED contributor.
// What changes here is durability, not audience — the same text, still readable
// after the socket drops. No token, no pane output (that is #7317 item 3, which
// wants a gate), no field that is not already on one of those two endpoints.
func (s *Server) handleContributeRuns(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 365 {
			days = n
		}
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= taskRunsMaxLimit {
			limit = n
		}
	}
	runs, err := readTaskRunsForUser(taskRunLogPath, username, time.Duration(days)*24*time.Hour, limit)
	if err != nil {
		http.Error(w, "task-run log unreadable", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"username":    username,
		"window_days": days,
		"limit":       limit,
		"returned":    len(runs),
		"runs":        runs,
	})
}

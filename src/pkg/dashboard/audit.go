package dashboard

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"gopkg.in/natefinch/lumberjack.v2"
)

var auditLogPath = "/data/audit.jsonl"

const (
	auditMaxSizeMB    = 5
	auditMaxBackups   = 3
	auditMaxAgeDays   = 90
	auditMaxEntries   = 200
	auditRingCap      = 500
	auditSummaryHours = 24
	// auditMaxTrackedActionUsers bounds the per-user last-action map so a
	// pathological stream of unique usernames cannot grow it without bound.
	// A real hive has a handful of users; this is purely defensive.
	auditMaxTrackedActionUsers = 500
)

// auditSensitiveActions is derived from the audit action vocabulary emitted by
// pkg/dashboard/api_*.go (config_* and approval decisions),
// pkg/dashboard/{api_auth,copilot_auth,claude_auth,backup_key,gateways}.go
// (auth/key/gateway changes), pkg/agentaudit/agentaudit.go (agent, token and
// lease actions), pkg/github/write_surface*.go (write refusals and signed
// commit reauthorship), and pkg/convergence/publish/publisher.go (publication
// refusals). Keep it explicit so the collapsed Audit Log risk chip changes
// only when a real audit action name is added or reclassified.
var auditSensitiveActions = map[string]bool{
	"agent_added":                           true,
	"agent_backend_changed":                 true,
	"agent_model_changed":                   true,
	"agent_removed":                         true,
	"agent_write_refused":                   true,
	"approval-bulk":                         true,
	"approval-resolve":                      true,
	"claude_auth_complete":                  true,
	"claude_auth_logout":                    true,
	"claude_auth_start":                     true,
	"claude_token_missing":                  true,
	"config_agent_cadences":                 true,
	"config_agent_channels":                 true,
	"config_agent_connections":              true,
	"config_agent_general":                  true,
	"config_agent_hooks":                    true,
	"config_agent_models":                   true,
	"config_agent_pipeline":                 true,
	"config_agent_prompt":                   true,
	"config_agent_restrictions":             true,
	"config_agent_stats":                    true,
	"config_agent_tools":                    true,
	"config_auto_merge":                     true,
	"config_compliance":                     true,
	"config_convergence":                    true,
	"config_escalation":                     true,
	"config_github":                         true,
	"config_governor_advisory":              true,
	"config_governor_attribution":           true,
	"config_governor_backup_key":            true,
	"config_governor_bob_key":               true,
	"config_governor_budget":                true,
	"config_governor_cadence_scope":         true,
	"config_governor_features":              true,
	"config_governor_gateway_delete":        true,
	"config_governor_gateway_upsert":        true,
	"config_governor_general_advanced":      true,
	"config_governor_health":                true,
	"config_governor_hub":                   true,
	"config_governor_inference_auth":        true,
	"config_governor_labels":                true,
	"config_governor_litellm":               true,
	"config_governor_logging":               true,
	"config_governor_notifications":         true,
	"config_governor_project_observability": true,
	"config_governor_question_autoclose":    true,
	"config_governor_replan":                true,
	"config_governor_repos":                 true,
	"config_governor_security":              true,
	"config_governor_sensing":               true,
	"config_governor_threshold_scaling":     true,
	"config_governor_thresholds":            true,
	"config_governor_trajectory":            true,
	"config_governor_watchdog":              true,
	"config_governor_work_source":           true,
	"config_review":                         true,
	"config_write_surface":                  true,
	"copilot_auth_logout":                   true,
	"copilot_auth_start":                    true,
	"copilot_token_missing":                 true,
	"design_approved":                       true,
	"finding_publication_refused":           true,
	"gh_auth_logout":                        true,
	"gh_auth_start":                         true,
	"ioscan_fail_closed":                    true,
	"lease_stage_refused":                   true,
	"lease_stage_reset":                     true,
	"login_denied":                          true,
	"model_discovery_failed":                true,
	"plan_approve":                          true,
	"plan_reject":                           true,
	"run_stage_reset":                       true,
	"signed_commit_reauthored":              true,
	"tool_approval":                         true,
}

type auditSummary struct {
	Histogram    []int       `json:"histogram"`
	Sensitive24h int         `json:"sensitive_24h"`
	Today        int         `json:"today"`
	Last         *AuditEntry `json:"last"`
}

// auditPseudoUsers are the audit User values that do NOT represent a real
// person acting in the dashboard: background/system writers ("system"),
// unauthenticated local access ("local"), and failed identity resolution
// ("unknown"). Entries by these must never count as user engagement.
var auditPseudoUsers = map[string]bool{
	"":        true,
	"system":  true,
	"local":   true,
	"unknown": true,
}

// AuditEntry moved to pkg/dashboard/collect with its consumers (the
// collectors); the alias keeps this package's write side (AuditLog) and every
// existing call site source-compatible, with an identical JSON contract.
type AuditEntry = collect.AuditEntry

type AuditLog struct {
	mu     sync.Mutex
	writer *lumberjack.Logger
	ring   []AuditEntry
	// lastAction maps a REAL username (never a pseudo-user; see
	// auditPseudoUsers) to the timestamp of their most recent audited action —
	// config saves, agent restarts, ACMM changes, logins, etc. It is the
	// cheap "did this person actually DO something" signal reported hub-ward
	// in the heartbeat, updated at write time rather than by scanning the log.
	// Rebuilt from the on-disk audit log at startup, and the hub keeps the
	// running maximum per user, so a spoke restart never regresses it there.
	lastAction map[string]time.Time
	// prCounters are the durable all-time PR throughput totals (opened /
	// merged / closed), bumped at write time in Log. prCountersPath is where
	// they persist; "" keeps them in memory only (no /data volume, tests).
	prCounters     PRThroughputCounters
	prCountersPath string
}

func newAuditLog() *AuditLog {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}

	dir := "/data"
	if _, err := os.Stat(dir); err == nil {
		a.writer = &lumberjack.Logger{
			Filename:   auditLogPath,
			MaxSize:    auditMaxSizeMB,
			MaxBackups: auditMaxBackups,
			MaxAge:     auditMaxAgeDays,
			Compress:   true,
		}
		a.loadFromDisk()
		a.loadPRThroughputCounters(prThroughputCountersPath, auditLogPath)
	}

	return a
}

func (a *AuditLog) loadFromDisk() {
	a.loadFromDiskPath(auditLogPath)
}

func (a *AuditLog) loadFromDiskPath(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if json.Unmarshal(line, &entry) == nil && entry.Timestamp != "" {
			a.ring = append(a.ring, entry)
			a.noteUserAction(entry.User, entry.Timestamp)
		}
	}
	if len(a.ring) > auditRingCap {
		a.ring = a.ring[len(a.ring)-auditRingCap:]
	}
}

func (a *AuditLog) Log(user, action, detail, agent string) {
	a.LogRecord(user, action, detail, agent, "", 0)
}

// LogRecord is Log for a hive-mediated GitHub write (#9587): repo and target
// are stored as typed fields next to the detail string. Empty repo and zero
// target are omitted from the JSON, so a plain Log line is byte-identical to
// what it was before these fields existed.
func (a *AuditLog) LogRecord(user, action, detail, agent, repo string, target int) {
	a.logRecordAt(time.Now().UTC().Format(time.RFC3339), user, action, detail, agent, repo, target)
}

func (a *AuditLog) LogRecordAt(timestamp, user, action, detail, agent, repo string, target int) {
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	a.logRecordAt(timestamp, user, action, detail, agent, repo, target)
}

func (a *AuditLog) logRecordAt(timestamp, user, action, detail, agent, repo string, target int) {
	if user == "" {
		user = "system"
	}
	entry := AuditEntry{
		Timestamp: timestamp,
		User:      user,
		Action:    action,
		Detail:    detail,
		Agent:     agent,
		Repo:      repo,
		Target:    target,
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.ring) >= auditRingCap {
		a.ring = a.ring[1:]
	}
	if prThroughputActions[entry.Action] && !a.notePRThroughput(entry) && prThroughputTerminalAction(entry.Action) {
		return
	}
	a.ring = append(a.ring, entry)
	a.noteUserAction(entry.User, entry.Timestamp)

	if a.writer != nil {
		if data, err := json.Marshal(entry); err == nil {
			if _, err := a.writer.Write(append(data, '\n')); err != nil {
				slog.Error("audit log write failed", "error", err)
			}
		}
	}
}

// noteUserAction records ts as user's most recent audited action, skipping
// pseudo-users and never moving a user's timestamp backwards (the on-disk
// replay in loadFromDisk feeds entries oldest-first, but ordering is not
// guaranteed across rotated files). Callers must hold a.mu.
func (a *AuditLog) noteUserAction(user, ts string) {
	if auditPseudoUsers[user] {
		return
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return
	}
	// Lazy init: several tests (and any future caller) construct AuditLog as a
	// bare struct literal rather than through newAuditLog.
	if a.lastAction == nil {
		a.lastAction = make(map[string]time.Time)
	}
	prev, known := a.lastAction[user]
	if !known && len(a.lastAction) >= auditMaxTrackedActionUsers {
		return
	}
	if !known || t.After(prev) {
		a.lastAction[user] = t
	}
}

// LastUserActions returns a copy of the per-user last-audited-action
// timestamps as RFC3339 strings, keyed by username. Non-secret by
// construction: bare usernames and timestamps only — no entry details, no
// tokens. It rides the heartbeat so the hub can tell users who DO things
// apart from users who merely leave a tab open.
func (a *AuditLog) LastUserActions() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.lastAction))
	for user, t := range a.lastAction {
		out[user] = t.UTC().Format(time.RFC3339)
	}
	return out
}

// OutputActionsSince reads the on-disk audit log (auditLogPath) and returns
// every entry whose Action is in `actions` and whose timestamp is at or after
// `since`. It reads the FILES, not the in-memory ring, because the ring is
// capped at auditRingCap (500) and reset on restart — cost-attribution activity
// needs the full audit window across busy hives. Lumberjack rotated backups
// adjacent to filePath are included, including compressed ".gz" backups.
// Malformed lines and unparseable timestamps are skipped. A missing current file
// returns an empty slice (clean first-boot).
//
// filePath is a parameter (defaulting to auditLogPath when "") so tests can
// point it at a fixture without touching /data.
func (a *AuditLog) OutputActionsSince(since time.Time, actions map[string]bool, filePath string) []AuditEntry {
	match := func(action string) bool { return len(actions) == 0 || actions[action] }
	return a.actionsSince(since, match, nil, filePath)
}

// ActionsWithPrefixSince is OutputActionsSince for a whole FAMILY of actions
// named by prefix — the watchdog's `watchdog-*` entries (#7254) — rather than
// an enumerated set. Same sources (current file plus rotated/compressed
// backups), same ordering (oldest first), same tolerance of malformed lines.
//
// The prefix doubles as a byte-level pre-filter: a line that does not contain
// the serialized `"action":"<prefix>` fragment cannot decode to a matching
// entry, so it is skipped without a JSON decode. On the hive this was built
// against, that is the difference between decoding 28,775 entries to find 0
// and decoding 0 — every line in the file was written by this package's own
// json.Marshal, whose field order and spacing are fixed, so the fragment is
// exact rather than heuristic. A line that somehow lacks it but still names a
// matching action would only ever have come from a hand-edited file.
func (a *AuditLog) ActionsWithPrefixSince(since time.Time, prefix, filePath string) []AuditEntry {
	match := func(action string) bool { return strings.HasPrefix(action, prefix) }
	var hint []byte
	if prefix != "" {
		hint = []byte(`"action":"` + prefix)
	}
	return a.actionsSince(since, match, hint, filePath)
}

// actionsSince is the shared file scanner behind OutputActionsSince and
// ActionsWithPrefixSince. match decides per action name; lineHint, when
// non-empty, lets a line be dropped before decoding when it cannot contain a
// match (callers that pass it must guarantee every matching line contains it).
func (a *AuditLog) actionsSince(since time.Time, match func(action string) bool, lineHint []byte, filePath string) []AuditEntry {
	if filePath == "" {
		filePath = auditLogPath
	}
	var out []AuditEntry
	for _, path := range auditLogFiles(filePath) {
		data, err := readAuditLogFile(path)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if len(lineHint) > 0 && !bytes.Contains(line, lineHint) {
				continue
			}
			var e AuditEntry
			if json.Unmarshal(line, &e) != nil || e.Timestamp == "" {
				continue
			}
			if !match(e.Action) {
				continue
			}
			t, perr := time.Parse(time.RFC3339, e.Timestamp)
			if perr != nil || t.Before(since) {
				continue
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

// RecentWithPrefixSince is the in-memory counterpart of ActionsWithPrefixSince:
// matching entries from the ring, oldest first. It exists for a hive with no
// /data volume (local runs, tests), where nothing is ever written to disk and
// the ring is the only record. It is a fallback, not an alternative — the ring
// holds auditRingCap entries and is emptied by a restart, so on a real hive the
// file is the source of truth and callers must prefer it.
func (a *AuditLog) RecentWithPrefixSince(since time.Time, prefix string) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []AuditEntry
	for _, e := range a.ring {
		if !strings.HasPrefix(e.Action, prefix) {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// HasOnDiskLog reports whether any audit file exists at filePath ("" → the
// production path) — the test that decides whether a reader may trust the
// files or must fall back to the ring.
func (a *AuditLog) HasOnDiskLog(filePath string) bool {
	if filePath == "" {
		filePath = auditLogPath
	}
	return len(auditLogFiles(filePath)) > 0
}

func auditLogFiles(filePath string) []string {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)
	patterns := []string{
		filePath,
		filePath + ".*",
		filepath.Join(dir, prefix+"-*"+ext),
		filepath.Join(dir, prefix+"-*"+ext+".gz"),
	}
	seen := map[string]bool{}
	files := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, p := range matches {
			if seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files
}

// maxAuditFileReadBytes caps how many decompressed bytes readAuditLogFile will
// load from a single audit file. Lumberjack rotates at auditMaxSizeMB (5MB), so
// a legitimate file — compressed or not — decompresses to roughly that size.
// Without a cap, a crafted or corrupted ".gz" in /data (a gzip bomb: a few KB
// expanding to many GB) would let io.ReadAll exhaust dashboard memory. 64MB is
// >10x the rotation size, so no legitimate file is ever truncated; a truncated
// trailing line simply fails json.Unmarshal and is skipped by the caller.
const maxAuditFileReadBytes = 64 << 20

func readAuditLogFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return io.ReadAll(io.LimitReader(r, maxAuditFileReadBytes))
}

func (a *AuditLog) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()

	if n <= 0 || n > len(a.ring) {
		n = len(a.ring)
	}
	start := len(a.ring) - n
	result := make([]AuditEntry, n)
	copy(result, a.ring[start:])
	// reverse so newest first
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func auditLogSummary(entries []AuditEntry, now time.Time) auditSummary {
	now = now.UTC()
	currentHour := now.Truncate(time.Hour)
	histogramStart := currentHour.Add(-(auditSummaryHours - 1) * time.Hour)
	histogramEnd := currentHour.Add(time.Hour)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sensitiveStart := now.Add(-auditSummaryHours * time.Hour)
	summary := auditSummary{
		Histogram: make([]int, auditSummaryHours),
	}
	var latest time.Time
	for i := range entries {
		entry := entries[i]
		ts, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil {
			continue
		}
		ts = ts.UTC()
		if summary.Last == nil || ts.After(latest) {
			last := entry
			summary.Last = &last
			latest = ts
		}
		if !ts.Before(todayStart) && !ts.After(now) {
			summary.Today++
		}
		if !ts.Before(sensitiveStart) && !ts.After(now) && auditSensitiveActions[entry.Action] {
			summary.Sensitive24h++
		}
		if ts.Before(histogramStart) || !ts.Before(histogramEnd) {
			continue
		}
		bucket := int(ts.Truncate(time.Hour).Sub(histogramStart) / time.Hour)
		if bucket >= 0 && bucket < len(summary.Histogram) {
			summary.Histogram[bucket]++
		}
	}
	return summary
}

func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		http.Error(w, "insufficient access", http.StatusForbidden)
		return
	}
	entries := s.audit.Recent(auditMaxEntries)
	summaryEntries := s.audit.Recent(0)
	// Cosmetic: attach display names for opaque OIDC actor keys. Recent()
	// returns copies, so the ring itself is never mutated.
	for i := range entries {
		if dn := s.authorizedDisplayName(entries[i].User); dn != "" && dn != entries[i].User {
			entries[i].UserName = dn
		}
	}
	for i := range summaryEntries {
		if dn := s.authorizedDisplayName(summaryEntries[i].User); dn != "" && dn != summaryEntries[i].User {
			summaryEntries[i].UserName = dn
		}
	}
	summary := auditLogSummary(summaryEntries, time.Now())
	jsonResponse(w, map[string]any{
		"entries":       entries,
		"histogram":     summary.Histogram,
		"sensitive_24h": summary.Sensitive24h,
		"today":         summary.Today,
		"last":          summary.Last,
	})
}

// requestUser resolves the acting user behind an authenticated dashboard
// request — the X-Hive-User header set by the auth proxy, or "local" for an
// unproxied deployment. Shared by the audit log and the pause-provenance
// path (#4041) so "who did this" is answered identically everywhere.
func requestUser(r *http.Request) string {
	user := r.Header.Get("X-Hive-User")
	if user == "" {
		user = "local"
	}
	return user
}

func (s *Server) auditFromRequest(r *http.Request, action, detail, agent string) {
	s.audit.Log(requestUser(r), action, detail, agent)
}

// AuditLog records an audit event from non-HTTP contexts (governor eval,
// config watcher, startup, login detector).
func (s *Server) AuditLog(user, action, detail, agent string) {
	s.audit.Log(user, action, detail, agent)
}

// AuditLogRecord records a hive-mediated GitHub write with its typed repo and
// target (#9587). See AuditLog.LogRecord.
func (s *Server) AuditLogRecord(user, action, detail, agent, repo string, target int) {
	s.audit.LogRecord(user, action, detail, agent, repo, target)
}

// AuditLogRecordAt records an observed forge event at the forge's terminal
// timestamp instead of the poll time.
func (s *Server) AuditLogRecordAt(timestamp, user, action, detail, agent, repo string, target int) {
	s.audit.LogRecordAt(timestamp, user, action, detail, agent, repo, target)
}

// GetAudit returns the underlying AuditLog for use by background goroutines.
func (s *Server) GetAudit() *AuditLog {
	return s.audit
}

// UserLastActions exposes the audit log's per-user last-action timestamps for
// the heartbeat sender (see AuditLog.LastUserActions for the contract).
func (s *Server) UserLastActions() map[string]string {
	return s.audit.LastUserActions()
}

func auditDetail(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	parts := ""
	for i := 0; i+1 < len(kv); i += 2 {
		if parts != "" {
			parts += ", "
		}
		parts += fmt.Sprintf("%s=%s", kv[i], kv[i+1])
	}
	return parts
}

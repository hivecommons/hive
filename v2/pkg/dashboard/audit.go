package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	auditLogPath    = "/data/audit.jsonl"
	auditMaxSizeMB  = 5
	auditMaxBackups = 3
	auditMaxAgeDays = 90
	auditMaxEntries = 200
	auditRingCap    = 500
	// auditMaxTrackedActionUsers bounds the per-user last-action map so a
	// pathological stream of unique usernames cannot grow it without bound.
	// A real hive has a handful of users; this is purely defensive.
	auditMaxTrackedActionUsers = 500
)

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

type AuditEntry struct {
	Timestamp string `json:"ts"`
	User      string `json:"user"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

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
	}

	return a
}

func (a *AuditLog) loadFromDisk() {
	data, err := os.ReadFile(auditLogPath)
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
	if user == "" {
		user = "system"
	}
	entry := AuditEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      user,
		Action:    action,
		Detail:    detail,
		Agent:     agent,
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.ring) >= auditRingCap {
		a.ring = a.ring[1:]
	}
	a.ring = append(a.ring, entry)
	a.noteUserAction(entry.User, entry.Timestamp)

	if a.writer != nil {
		if data, err := json.Marshal(entry); err == nil {
			a.writer.Write(append(data, '\n'))
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

func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" && role != "read-write" {
		http.Error(w, "insufficient access", http.StatusForbidden)
		return
	}
	entries := s.audit.Recent(auditMaxEntries)
	jsonResponse(w, map[string]any{"entries": entries})
}

func (s *Server) auditFromRequest(r *http.Request, action, detail, agent string) {
	user := r.Header.Get("X-Hive-User")
	if user == "" {
		user = "local"
	}
	s.audit.Log(user, action, detail, agent)
}

// AuditLog records an audit event from non-HTTP contexts (governor eval,
// config watcher, startup, login detector).
func (s *Server) AuditLog(user, action, detail, agent string) {
	s.audit.Log(user, action, detail, agent)
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

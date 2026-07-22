package dashboard

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
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
	ID        string `json:"id,omitempty"`
	Timestamp string `json:"ts"`
	User      string `json:"user"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

type AuditLog struct {
	mu      sync.Mutex
	writer  *lumberjack.Logger
	ring    []AuditEntry
	seenIDs map[string]struct{}
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
		seenIDs:    make(map[string]struct{}),
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
	_ = a.loadAuditFile(auditLogPath, true)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(auditLogPath), "audit*.jsonl*"))
	for _, path := range matches {
		if filepath.Clean(path) != filepath.Clean(auditLogPath) {
			_ = a.loadAuditFile(path, false)
		}
	}
	if len(a.ring) > auditRingCap {
		a.ring = a.ring[len(a.ring)-auditRingCap:]
	}
}

func (a *AuditLog) loadAuditFile(path string, includeInRing bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader = compressed
	}
	if a.seenIDs == nil {
		a.seenIDs = make(map[string]struct{})
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		var entry AuditEntry
		if json.Unmarshal(line, &entry) == nil && entry.Timestamp != "" {
			if entry.ID != "" {
				a.seenIDs[entry.ID] = struct{}{}
			}
			// Rebuild the per-user last-action index from every retained file,
			// not just the display ring — noteUserAction skips pseudo-users and
			// never moves a timestamp backwards, so replaying all files is safe.
			a.noteUserAction(entry.User, entry.Timestamp)
			if includeInRing {
				a.ring = append(a.ring, entry)
			}
		}
	}
	return scanner.Err()
}

func (a *AuditLog) Log(user, action, detail, agent string) {
	_, _ = a.log("", user, action, detail, agent)
}

// LogOnce records an event with a stable caller-owned identity. The identity
// index spans the retained audit files rather than the smaller display ring;
// durable workflow state acknowledges the event after this write succeeds.
func (a *AuditLog) LogOnce(id, user, action, detail, agent string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("audit event identity is required")
	}
	return a.log(id, user, action, detail, agent)
}

func (a *AuditLog) log(id, user, action, detail, agent string) (bool, error) {
	if user == "" {
		user = "system"
	}
	entry := AuditEntry{
		ID:        id,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      user,
		Action:    action,
		Detail:    detail,
		Agent:     agent,
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seenIDs == nil {
		a.seenIDs = make(map[string]struct{})
	}
	if id != "" {
		if _, exists := a.seenIDs[id]; exists {
			return false, nil
		}
		if a.writer == nil {
			return false, errors.New("durable audit writer is unavailable")
		}
	}

	if a.writer != nil {
		data, err := json.Marshal(entry)
		if err != nil {
			return false, err
		}
		if _, err := a.writer.Write(append(data, '\n')); err != nil {
			return false, err
		}
	}

	if len(a.ring) >= auditRingCap {
		a.ring = a.ring[1:]
	}
	a.ring = append(a.ring, entry)
	a.noteUserAction(entry.User, entry.Timestamp)
	if id != "" {
		a.seenIDs[id] = struct{}{}
	}

	return true, nil
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
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
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

// AuditLogOnce records a stable workflow event through the normal dashboard
// audit sink, returning whether this call appended the event.
func (s *Server) AuditLogOnce(id, user, action, detail, agent string) (bool, error) {
	return s.audit.LogOnce(id, user, action, detail, agent)
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

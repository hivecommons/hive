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
	auditLogPath       = "/data/audit.jsonl"
	auditMaxSizeMB     = 5
	auditMaxBackups    = 3
	auditMaxAgeDays    = 90
	auditMaxEntries    = 200
	auditRingCap       = 500
)

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
}

func newAuditLog() *AuditLog {
	a := &AuditLog{
		ring: make([]AuditEntry, 0, auditRingCap),
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

	if a.writer != nil {
		if data, err := json.Marshal(entry); err == nil {
			a.writer.Write(append(data, '\n'))
		}
	}
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

package dashboard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/config"
)

// RelayAuditShare is the opt-in audit attestation a relay attaches to
// task_complete / task_failed (hivecommons/hive#13458, design #13441 §3 "No
// upload by default"). A relay sends it only when the contributor ran
// `hivectl hives set <name> --share-audit <level>` for this hive; without that
// consent the field is absent and the hub records nothing beyond its own
// relay_* rows.
//
// Rows are the contributor's local relay-activity rows for the task (the
// shape bin/lib/relay-audit.js writes), already redacted on the contributor's
// machine, up to Level.
type RelayAuditShare struct {
	// Level is the share level the contributor consented to: lifecycle,
	// commands or reasoning.
	Level string `json:"level"`
	// CaptureLevel is the relay's local capture level for the task. The share
	// level can never exceed it; the hub clamps defensively when it does.
	CaptureLevel string `json:"capture_level,omitempty"`
	// LocalRef cross-references the contributor's local record of the task
	// (the relay-activity.jsonl row key), so a dispute can line the hub's row
	// up with the contributor's file.
	LocalRef string            `json:"local_ref,omitempty"`
	Rows     []json.RawMessage `json:"rows,omitempty"`
}

// contribAuditDirPath is where shared command rows are persisted, one 0600
// JSONL file per contributor and task, instead of in the 500-entry audit ring.
var contribAuditDirPath = "/data/contrib-audit"

const (
	// relayAttestMaxRows bounds one attestation. The frame itself is already
	// bounded by wsMaxMessageSize; this guards the row count, not bytes.
	relayAttestMaxRows = 1000
	// contribAuditPruneEvery throttles the retention sweep.
	contribAuditPruneEvery = time.Hour
)

var (
	contribAuditPruneMu   sync.Mutex
	contribAuditLastPrune time.Time
	contribAuditUnsafe    = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// relayAuditLevelRank orders the share levels; 0 is "nothing" (off, empty or
// unrecognised).
func relayAuditLevelRank(level string) int {
	l := config.NormalizeRelayAuditShareLevel(level)
	for i, v := range config.RelayAuditShareLevels {
		if v == l && l != "" {
			return i + 1
		}
	}
	return 0
}

// relayAuditRowLevel is the level a shared row belongs to: its own `level`
// when it carries a recognised one, `commands` for a command row, and
// `lifecycle` otherwise (terminal and lifecycle rows).
func relayAuditRowLevel(row map[string]json.RawMessage) string {
	var level, event string
	if raw, ok := row["level"]; ok {
		_ = json.Unmarshal(raw, &level)
	}
	if l := config.NormalizeRelayAuditShareLevel(level); l != "" {
		return l
	}
	if raw, ok := row["event"]; ok {
		_ = json.Unmarshal(raw, &event)
	}
	if event == "command" {
		return "commands"
	}
	return "lifecycle"
}

func relayAuditRowIsCommand(row map[string]json.RawMessage) bool {
	var event string
	if raw, ok := row["event"]; ok {
		_ = json.Unmarshal(raw, &event)
	}
	return event == "command"
}

// contribAuditFileComponent makes a login or task id safe as one path
// element. It returns "" when nothing usable is left.
func contribAuditFileComponent(s string) string {
	s = contribAuditUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_")
	s = strings.Trim(s, "._")
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// attestRelayAudit records a contributor's voluntary audit attestation for a
// task the hub has just accepted a terminal frame for. A nil share (no
// consent, or a relay that predates #13458) is a no-op. The hub writes one
// relay_task_attested row; when commands or higher is shared, the command
// rows go to contribAuditDirPath/<login>/<task>.jsonl rather than the ring.
func (h *ContributeWSHub) attestRelayAudit(c *ContributorConnection, task *WSTaskAssign, share *RelayAuditShare) {
	if h == nil || share == nil || task == nil || c == nil || c.profile == nil {
		return
	}
	shareRank := relayAuditLevelRank(share.Level)
	if shareRank == 0 {
		h.logger.Info("[contribute-ws] ignoring audit attestation with unrecognised level",
			"username", c.profile.GitHubUsername, "task_id", task.TaskID, "level", auditRelayText(share.Level))
		return
	}
	captureRank := relayAuditLevelRank(share.CaptureLevel)
	clamped := false
	if strings.TrimSpace(share.CaptureLevel) != "" && shareRank > captureRank {
		if captureRank == 0 {
			// capture "off": there is nothing the contributor could share.
			return
		}
		shareRank, clamped = captureRank, true
	}
	level := config.RelayAuditShareLevels[shareRank-1]

	var commands [][]byte
	kept, dropped := 0, 0
	for i, raw := range share.Rows {
		if i >= relayAttestMaxRows {
			dropped += len(share.Rows) - i
			break
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil || row == nil {
			dropped++
			continue
		}
		if relayAuditLevelRank(relayAuditRowLevel(row)) > shareRank {
			dropped++
			continue
		}
		kept++
		if shareRank < relayAuditLevelRank("commands") || !relayAuditRowIsCommand(row) {
			continue
		}
		if shareRank < relayAuditLevelRank("reasoning") {
			delete(row, "reason")
			delete(row, "reason_source")
		}
		line, err := json.Marshal(row)
		if err != nil {
			continue
		}
		commands = append(commands, line)
	}

	stored := ""
	if len(commands) > 0 {
		rel, err := persistContribAudit(c.profile.GitHubUsername, task.TaskID, commands, time.Now())
		if err != nil {
			h.logger.Warn("[contribute-ws] failed to persist shared relay audit rows",
				"username", c.profile.GitHubUsername, "task_id", task.TaskID, "error", err)
		} else {
			stored = rel
		}
	}

	kv := []any{"level", level, "capture_level", config.NormalizeRelayAuditShareLevel(share.CaptureLevel),
		"local_ref", auditRelayText(share.LocalRef), "rows", kept, "commands", len(commands), "stored", stored}
	if dropped > 0 {
		kv = append(kv, "dropped", dropped)
	}
	if clamped {
		kv = append(kv, "clamped", true)
	}
	h.auditRelay(c, agentaudit.AuditRelayTaskAttested, task, kv...)
}

// persistContribAudit writes one task's shared command rows to
// contribAuditDirPath/<login>/<task>.jsonl (dir 0700, file 0600) and returns
// the path relative to contribAuditDirPath. A repeated attestation for the
// same task replaces the file.
func persistContribAudit(login, taskID string, lines [][]byte, now time.Time) (string, error) {
	user, name := contribAuditFileComponent(login), contribAuditFileComponent(taskID)
	if user == "" || name == "" {
		return "", os.ErrInvalid
	}
	pruneContribAudit(contribAuditDirPath, now)
	dir := filepath.Join(contribAuditDirPath, user)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	data := append(bytes.Join(lines, []byte("\n")), '\n')
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	final := filepath.Join(dir, name+".jsonl")
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	return user + "/" + name + ".jsonl", nil
}

// pruneContribAudit applies the audit log's max-age knob (auditMaxAgeDays,
// see src/docs/audit-log.md) to the shared-row store: per-task files older
// than that are removed, then empty contributor directories. Throttled to
// once per contribAuditPruneEvery per process; failures are ignored.
func pruneContribAudit(root string, now time.Time) {
	contribAuditPruneMu.Lock()
	if !contribAuditLastPrune.IsZero() && now.Sub(contribAuditLastPrune) < contribAuditPruneEvery {
		contribAuditPruneMu.Unlock()
		return
	}
	contribAuditLastPrune = now
	contribAuditPruneMu.Unlock()

	cutoff := now.Add(-auditMaxAgeDays * 24 * time.Hour)
	users, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		dir := filepath.Join(root, u.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		left := len(files)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(dir, f.Name())) == nil {
				left--
			}
		}
		if left == 0 {
			_ = os.Remove(dir)
		}
	}
}

// contributeAuditShareRequest is the relay audit level this hive asks
// contributors to share (hub.contribute_audit_share_request), advertised on
// auth_ok and GET /api/contribute/status. "" requests nothing.
func (s *Server) contributeAuditShareRequest() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	return s.deps.Config.Hub.ContributeAuditShareRequestOrNone()
}

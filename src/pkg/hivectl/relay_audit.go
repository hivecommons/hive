package hivectl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Leveled local relay audit (#13456, design #13441 §3).
//
// relay-activity.jsonl holds three row classes since the relay gained audit
// levels: the #13293 terminal rows (`outcome`, no `event`), lifecycle rows and
// command rows (`event`, `level`, `ts`). This file reads all of them for
// `hivectl hives audit`; LoadRelayActivity keeps reading only terminal rows so
// `hivectl hives activity` counts are unchanged. Nothing here is uploaded.

// Audit levels, each a strict superset of the one before.
const (
	AuditLevelOff       = "off"
	AuditLevelLifecycle = "lifecycle"
	AuditLevelCommands  = "commands"
	AuditLevelReasoning = "reasoning"

	// DefaultAuditLevel is what the relay uses for a hive with no setting.
	DefaultAuditLevel = AuditLevelLifecycle

	auditCommandEvent = "command"
	auditTerminalRow  = "terminal"
)

// AuditLevels lists the levels in ascending order.
var AuditLevels = []string{AuditLevelOff, AuditLevelLifecycle, AuditLevelCommands, AuditLevelReasoning}

// ValidateAuditLevel accepts exactly one of AuditLevels.
func ValidateAuditLevel(level string) error {
	if slices.Contains(AuditLevels, level) {
		return nil
	}
	return fmt.Errorf("audit level %q must be one of %s", level, strings.Join(AuditLevels, "|"))
}

func auditLevelRank(level string) int {
	return slices.Index(AuditLevels, level)
}

// SetAuditLevel records a hive's local audit level. Like every mutation it
// does not write; callers commit, which also projects HIVE_RELAY_AUDIT_LEVEL.
func (set *ProfileSet) SetAuditLevel(name, level string) (Profile, error) {
	if err := ValidateAuditLevel(level); err != nil {
		return Profile{}, err
	}
	target, _ := set.Find(name)
	if target == nil {
		return Profile{}, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	target.AuditLevel = level
	return *target, nil
}

// EffectiveAuditLevel is the level the relay applies to this profile.
func (p Profile) EffectiveAuditLevel() string {
	if p.AuditLevel == "" {
		return DefaultAuditLevel
	}
	return p.AuditLevel
}

// projectAuditLevels writes HIVE_RELAY_AUDIT_LEVEL with one entry per hub,
// aligned with HIVE_HUB, once any profile has a level. With none set, a
// hand-written single value is kept, but a stale per-hub list is dropped:
// its positions would no longer match HIVE_HUB.
func projectAuditLevels(env *envFile, ordered []Profile) {
	anySet := false
	levels := make([]string, 0, len(ordered))
	for _, p := range ordered {
		if p.AuditLevel != "" {
			anySet = true
		}
		levels = append(levels, p.EffectiveAuditLevel())
	}
	if anySet {
		env.set("HIVE_RELAY_AUDIT_LEVEL", strings.Join(levels, ","))
		return
	}
	if strings.Contains(env.value("HIVE_RELAY_AUDIT_LEVEL"), ",") {
		env.unset("HIVE_RELAY_AUDIT_LEVEL")
	}
}

// RelayAuditRecord is one row of the leveled stream. Terminal rows carry
// Outcome and EndedAt; lifecycle and command rows carry Event, Level and TS.
type RelayAuditRecord struct {
	Level         string    `json:"level"`
	Event         string    `json:"event"`
	TS            time.Time `json:"ts"`
	Hive          string    `json:"hub,omitempty"`
	ContributorID string    `json:"contributor_id,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	Repo          string    `json:"repo,omitempty"`
	Number        int       `json:"number,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	TaskID        string    `json:"task_id"`
	TaskGen       any       `json:"task_gen,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	URL           string    `json:"url,omitempty"`
	Stash         string    `json:"stash,omitempty"`
	Paths         int       `json:"paths,omitempty"`
	Backend       string    `json:"backend,omitempty"`
	TokenGen      int       `json:"token_gen,omitempty"`
	Capture       string    `json:"capture,omitempty"`
	Op            string    `json:"op,omitempty"`
	Cmd           string    `json:"cmd,omitempty"`
	Remote        string    `json:"remote,omitempty"`
	Refspec       string    `json:"refspec,omitempty"`
	Cwd           string    `json:"cwd,omitempty"`
	Exit          *int      `json:"exit,omitempty"`
	DurationMS    int64     `json:"duration_ms,omitempty"`
	ToolCallID    string    `json:"tool_call_id,omitempty"`
	ReasonSource  string    `json:"reason_source,omitempty"`
}

// auditRowJSON reads both row shapes: terminal rows predate `ts`/`event`.
type auditRowJSON struct {
	RelayAuditRecord
	TaskID  json.RawMessage `json:"task_id"`
	EndedAt time.Time       `json:"ended_at"`
}

func parseRelayAuditRow(line []byte) (RelayAuditRecord, bool) {
	var raw auditRowJSON
	if err := json.Unmarshal(line, &raw); err != nil {
		return RelayAuditRecord{}, false
	}
	rec := raw.RelayAuditRecord
	var id any
	if len(raw.TaskID) > 0 && json.Unmarshal(raw.TaskID, &id) == nil && id != nil {
		rec.TaskID = strings.TrimSpace(fmt.Sprint(id))
	}
	if rec.TaskID == "" {
		return RelayAuditRecord{}, false
	}
	if rec.Event == "" {
		if rec.Outcome == "" || raw.EndedAt.IsZero() {
			return RelayAuditRecord{}, false
		}
		rec.Event = auditTerminalRow
		rec.Level = AuditLevelOff
		rec.TS = raw.EndedAt
		return rec, true
	}
	if rec.TS.IsZero() || ValidateAuditLevel(rec.Level) != nil {
		return RelayAuditRecord{}, false
	}
	return rec, true
}

// LoadRelayAudit reads every row of the leveled stream in file order,
// skipping and counting lines it cannot read. A missing file is
// ErrRelayActivityUnavailable, as for LoadRelayActivity.
func (s *ProfileStore) LoadRelayAudit() ([]RelayAuditRecord, int, error) {
	f, err := os.Open(s.RelayActivityPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrRelayActivityUnavailable
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read relay audit file %s: %w", s.RelayActivityPath(), err)
	}
	defer f.Close()
	var records []RelayAuditRecord
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rec, ok := parseRelayAuditRow([]byte(line))
		if !ok {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("read relay audit file %s: %w", s.RelayActivityPath(), err)
	}
	return records, skipped, nil
}

// RelayAuditFilter narrows `hivectl hives audit`. Zero values match all.
type RelayAuditFilter struct {
	Since time.Time
	Task  string
	// Level keeps rows written at or below this level.
	Level string
}

// FilterRelayAudit applies f, preserving file order.
func FilterRelayAudit(records []RelayAuditRecord, f RelayAuditFilter) []RelayAuditRecord {
	out := make([]RelayAuditRecord, 0, len(records))
	maxRank := len(AuditLevels) - 1
	if f.Level != "" {
		maxRank = auditLevelRank(f.Level)
	}
	for _, r := range records {
		if !f.Since.IsZero() && r.TS.Before(f.Since) {
			continue
		}
		if f.Task != "" && r.TaskID != f.Task {
			continue
		}
		if auditLevelRank(r.Level) > maxRank {
			continue
		}
		if r.IsCommand() && f.Level != "" && auditLevelRank(f.Level) < auditLevelRank(AuditLevelReasoning) {
			r.Reason, r.ReasonSource = "", ""
		}
		out = append(out, r)
	}
	return out
}

// IsCommand reports whether r is a GitHub-touching command row.
func (r RelayAuditRecord) IsCommand() bool { return r.Event == auditCommandEvent }

// IsTerminal reports whether r is a #13293 terminal-attempt row.
func (r RelayAuditRecord) IsTerminal() bool { return r.Event == auditTerminalRow }

// ClearRelayAuditReasons drops the `reason` column (and its `reason_source`)
// from every row in place, keeping every row and every other field. The file
// is rewritten atomically with the relay's permissions: 0600 in a 0700
// directory. Lines it cannot parse are kept verbatim. It returns how many
// rows lost a reason.
func (s *ProfileStore) ClearRelayAuditReasons() (int, error) {
	path := s.RelayActivityPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read relay audit file %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	cleared := 0
	for i, line := range lines {
		if !strings.Contains(line, `"reason`) {
			continue
		}
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		var event string
		if raw, ok := row["event"]; !ok || json.Unmarshal(raw, &event) != nil || event != auditCommandEvent {
			continue
		}
		_, hadReason := row["reason"]
		_, hadSource := row["reason_source"]
		if !hadReason && !hadSource {
			continue
		}
		delete(row, "reason")
		delete(row, "reason_source")
		out, err := json.Marshal(row)
		if err != nil {
			return 0, fmt.Errorf("rewrite relay audit row: %w", err)
		}
		lines[i] = string(out)
		cleared++
	}
	if cleared == 0 {
		return 0, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("clear relay audit reasons: %w", err)
	}
	return cleared, nil
}

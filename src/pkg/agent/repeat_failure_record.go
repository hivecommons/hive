package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// Command records for the repeat-failure check (hivecommons/hive#14550, C1
// and the record-file constraints; foundation issue #14553).
//
// Every producer (the backend-neutral shell route and the per-backend
// readers) appends one RepeatFailureRecord per command to a per-agent raw
// record file. Raw records may contain secrets, so the file lives in a
// directory only the agent's user (and Hive) can enter, at mode 0600. Hive
// collects the file by renaming it first, so new records go to a fresh file,
// scrubs every record with the log scrubber and deletes the collected file.
// Only scrubbed records leave the collector.
//
// File protocol, shared with producers that are not Go code:
//
//   - One record is one JSON object on one line, appended with a single
//     write(2) of at most RepeatFailureRecordMaxLine bytes on a file opened
//     O_APPEND; longer commands are cut to fit.
//   - A writer holds a shared flock(2) on the file while it writes, and only
//     writes when the descriptor it holds is still the file at the record
//     path; otherwise it reopens. The collector renames the file, then takes
//     an exclusive flock on the renamed file before reading it, so a writer
//     that opened the file before the rename either finishes first or notices
//     the rename and writes to the fresh file. No record is lost or torn.

const (
	// RepeatFailureRecordMaxLine bounds one record line, newline included.
	// It is PIPE_BUF, the size a single append stays whole at on every
	// filesystem Hive runs on.
	RepeatFailureRecordMaxLine = 4096
	// RepeatFailureCollectInterval is how often Hive collects every agent's
	// record file. The record-file constraint is "at least every 30 s".
	RepeatFailureCollectInterval = 30 * time.Second

	repeatFailureRecordDirMode  = 0o700
	repeatFailureRecordFileMode = 0o600
	// repeatFailureRecordParentMode is used for parents EnsureRepeatFailureRecordDir
	// has to create (normally ~/.hive): the agent must be able to traverse them.
	repeatFailureRecordParentMode = 0o755
	// repeatFailureCollectingSuffix names the renamed file being collected.
	repeatFailureCollectingSuffix = ".collecting"
	// repeatFailureWriteAttempts bounds how often a writer reopens after
	// racing a collection; one reopen is enough at the production interval.
	repeatFailureWriteAttempts = 32
	// repeatFailureMaxRecordsPerFile bounds the records taken from one file
	// in one collection, so an agent cannot make Hive hold an unbounded file
	// in memory. Further lines are counted as dropped.
	repeatFailureMaxRecordsPerFile = 10000
)

// RepeatFailureRecord is one shell command an agent ran on a covered backend.
type RepeatFailureRecord struct {
	Agent   string    `json:"agent"`
	Backend string    `json:"backend"`
	Time    time.Time `json:"time"`
	// Command is the normalized command the detector compares. Producers
	// may leave it empty; the normalizer fills it after collection.
	Command string `json:"command,omitempty"`
	// RawCommand is the command as the agent asked to run it. Scrubbed by
	// the collector before it is returned.
	RawCommand string `json:"raw_command"`
	// ExitCode is the numeric exit status, nil when the backend reports only
	// failed/succeeded.
	ExitCode *int `json:"exit_code,omitempty"`
	// Failed is the result. When ExitCode is set it is ExitCode != 0.
	Failed bool `json:"failed"`
	// Route names the producer that wrote the record (for example the
	// neutral shell route or a backend's session-log reader).
	Route string `json:"route"`
	// Truncated reports that the command was cut to fit one record line.
	Truncated bool `json:"truncated,omitempty"`
}

// RepeatFailureRecordPath is the raw record file inside an agent's HOME.
func RepeatFailureRecordPath(home string) string {
	return filepath.Join(home, ".hive", "repeat-failure", "records.jsonl")
}

// EnsureRepeatFailureRecordDir creates the directory holding an agent's raw
// record file at mode 0700 and, when uid > 0, gives it to the agent's user,
// so only that user and Hive can read the records inside it.
//
// Hive runs as root while the agent runs under uid, so any parent this
// helper has to create (normally ~/.hive) is made traversable at
// repeatFailureRecordParentMode and also given to uid; only the leaf is
// 0700. Otherwise a root-owned 0700 parent would stop the agent from ever
// reaching its record file.
//
// SECURITY (audit F12): os.MkdirAll, os.Chown and os.Chmod all follow
// symlinks, so every component below the first existing directory is
// Lstat-verified as a real directory before it is created or acted on. A
// planted link fails loudly instead of having root chown its target.
func EnsureRepeatFailureRecordDir(recordPath string, uid int) error {
	dir := filepath.Clean(filepath.Dir(recordPath))

	// Walk up to the first existing component; everything below it is ours
	// to create. The existing ancestor is only checked for being a real
	// directory, never modified.
	var missing []string
	current := dir
	for {
		exists, err := lstatNoFollow(current)
		if err != nil {
			return err
		}
		if exists {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("repeat-failure record dir %s has no existing ancestor", dir)
		}
		missing = append(missing, current)
		current = parent
	}

	for i := len(missing) - 1; i >= 0; i-- {
		path := missing[i]
		mode := os.FileMode(repeatFailureRecordParentMode)
		if path == dir {
			mode = repeatFailureRecordDirMode
		}
		if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
			return err
		}
		// Re-check: EEXIST means something raced us into place, and that
		// something may be a symlink.
		if _, err := lstatNoFollow(path); err != nil {
			return err
		}
		if err := ownRepeatFailureDir(path, uid, mode); err != nil {
			return err
		}
	}
	if len(missing) == 0 || missing[0] != dir {
		// dir already existed: still enforce ownership and mode on the leaf.
		return ownRepeatFailureDir(dir, uid, repeatFailureRecordDirMode)
	}
	return nil
}

// ownRepeatFailureDir gives path (already Lstat-verified by the caller) to
// uid when uid > 0 and sets mode on it.
func ownRepeatFailureDir(path string, uid int, mode os.FileMode) error {
	if uid > 0 {
		if err := os.Chown(path, uid, -1); err != nil {
			return err
		}
	}
	return os.Chmod(path, mode)
}

// AppendRepeatFailureRecord appends one record to a raw record file in a
// single write, following the file protocol above. The file is created at
// mode 0600.
func AppendRepeatFailureRecord(path string, rec RepeatFailureRecord) error {
	line, err := encodeRepeatFailureRecord(rec)
	if err != nil {
		return err
	}
	for range repeatFailureWriteAttempts {
		done, err := appendRepeatFailureLine(path, line)
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("repeat-failure record file %s kept moving during the write", path)
}

func appendRepeatFailureLine(path string, line []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, repeatFailureRecordFileMode)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return false, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(held, current) {
		return false, nil
	}
	if _, err := f.Write(line); err != nil {
		return false, err
	}
	return true, nil
}

// encodeRepeatFailureRecord renders rec as one newline-terminated JSON line
// of at most RepeatFailureRecordMaxLine bytes, cutting the commands when
// needed. JSON escaping guarantees the line holds no raw newline.
func encodeRepeatFailureRecord(rec RepeatFailureRecord) ([]byte, error) {
	line, err := marshalRepeatFailureLine(rec)
	if err != nil || len(line) <= RepeatFailureRecordMaxLine {
		return line, err
	}
	rec.Truncated = true
	bare := rec
	bare.RawCommand, bare.Command = "", ""
	base, err := marshalRepeatFailureLine(bare)
	if err != nil {
		return nil, err
	}
	// The bare line omits the empty "command" key (omitempty), so its size
	// is only a first estimate: re-encode and shrink the room by whatever
	// the encoded line still overshoots until the whole line fits.
	raw, norm := rec.RawCommand, rec.Command
	rawLen, normLen := jsonStringLen(raw), jsonStringLen(norm)
	for avail := RepeatFailureRecordMaxLine - len(base); avail > 0; {
		// Split the room evenly; a command shorter than its half gives the
		// rest to the other.
		rawBudget := avail / 2
		normBudget := avail - rawBudget
		switch {
		case normLen < normBudget:
			rawBudget, normBudget = avail-normLen, normLen
		case rawLen < rawBudget:
			rawBudget, normBudget = rawLen, avail-rawLen
		}
		rec.RawCommand = cutJSONString(raw, rawBudget)
		rec.Command = cutJSONString(norm, normBudget)
		line, err = marshalRepeatFailureLine(rec)
		if err != nil {
			return nil, err
		}
		excess := len(line) - RepeatFailureRecordMaxLine
		if excess <= 0 {
			return line, nil
		}
		avail -= excess
	}
	return nil, errors.New("repeat-failure record does not fit one line without its command")
}

func marshalRepeatFailureLine(rec RepeatFailureRecord) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// jsonStringLen is an upper bound on the bytes s takes inside a JSON string.
func jsonStringLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += jsonRuneLen(r, size)
		i += size
	}
	return n
}

// cutJSONString returns the longest prefix of s, cut on a rune boundary, that
// takes at most budget bytes inside a JSON string.
func cutJSONString(s string, budget int) string {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += jsonRuneLen(r, size)
		if n > budget {
			return s[:i]
		}
		i += size
	}
	return s
}

// jsonRuneLen is an upper bound on one rune's encoded size: quote and
// backslash take two bytes; control characters, U+2028/U+2029 and invalid
// bytes (written as \ufffd) take at most six.
func jsonRuneLen(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return 6
	case r == '"' || r == '\\':
		return 2
	case r < 0x20 || r == '\u2028' || r == '\u2029':
		return 6
	default:
		return size
	}
}

// RepeatFailureCollector moves records out of every agent's raw record file
// and hands them, scrubbed, to a sink. It exists only while the check is on:
// NewRepeatFailureCollector returns nil for a disabled config, and every
// method is a no-op on nil, so an off switch collects nothing and touches no
// file.
type RepeatFailureCollector struct {
	// paths returns the raw record file of every agent to collect, keyed by
	// agent name.
	paths  func() map[string]string
	sink   func(RepeatFailureRecord)
	logger *slog.Logger
	now    func() time.Time
}

// NewRepeatFailureCollector returns a collector, or nil when cfg is off.
func NewRepeatFailureCollector(cfg *config.RepeatFailureConfig, paths func() map[string]string, sink func(RepeatFailureRecord), logger *slog.Logger) *RepeatFailureCollector {
	if !cfg.IsEnabled() || paths == nil || sink == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RepeatFailureCollector{paths: paths, sink: sink, logger: logger, now: time.Now}
}

// Run collects on every tick until ctx ends. Production passes the channel of
// a ticker at RepeatFailureCollectInterval; tests drive it directly.
func (c *RepeatFailureCollector) Run(ctx context.Context, tick <-chan time.Time) {
	if c == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			c.Collect()
		}
	}
}

// Collect collects every agent's record file once and returns how many
// records reached the sink.
func (c *RepeatFailureCollector) Collect() int {
	if c == nil {
		return 0
	}
	paths := c.paths()
	agents := make([]string, 0, len(paths))
	for name := range paths {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	total := 0
	for _, name := range agents {
		n, invalid, err := c.collectFile(name, paths[name])
		total += n
		if invalid > 0 {
			c.logger.Warn("repeat-failure: dropped unreadable command records", "agent", name, "count", invalid)
		}
		if err != nil {
			c.logger.Warn("repeat-failure: collecting command records failed", "agent", name, "error", err)
		}
	}
	return total
}

// collectFile drains a collecting file left by an interrupted collection
// first, so a rename never overwrites records, then renames the live file
// and drains it.
func (c *RepeatFailureCollector) collectFile(agent, path string) (int, int, error) {
	collecting := path + repeatFailureCollectingSuffix
	n, invalid, err := c.drainCollecting(agent, collecting)
	if err != nil {
		return n, invalid, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return n, invalid, nil
	}
	if err != nil {
		return n, invalid, err
	}
	if !info.Mode().IsRegular() {
		return n, invalid, fmt.Errorf("record file %s is not a regular file", path)
	}
	if err := os.Rename(path, collecting); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return n, invalid, nil
		}
		return n, invalid, err
	}
	m, bad, err := c.drainCollecting(agent, collecting)
	return n + m, invalid + bad, err
}

// drainCollecting reads a renamed record file under an exclusive lock, which
// waits out writers that opened it before the rename, deletes it, and sends
// each scrubbed record to the sink. It returns the records sent and the lines
// that were not valid records.
func (c *RepeatFailureCollector) drainCollecting(agent, collecting string) (int, int, error) {
	f, err := os.OpenFile(collecting, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, 0, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	var records []RepeatFailureRecord
	invalid := 0
	info, err := f.Stat()
	if err == nil && info.Mode().IsRegular() {
		records, invalid, err = readRepeatFailureRecords(f)
	} else if err == nil {
		err = fmt.Errorf("collecting file %s is not a regular file", collecting)
	}
	if rmErr := os.Remove(collecting); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	for _, rec := range records {
		c.sink(c.scrubRecord(agent, rec))
	}
	return len(records), invalid, err
}

// readRepeatFailureRecords parses one record per line. Lines longer than a
// record may be, lines that are not a record and lines past the per-file
// bound count as invalid; their content is never logged.
func readRepeatFailureRecords(r io.Reader) ([]RepeatFailureRecord, int, error) {
	br := bufio.NewReaderSize(r, RepeatFailureRecordMaxLine)
	var records []RepeatFailureRecord
	invalid := 0
	oversized := false
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			oversized = true
			continue
		}
		if len(line) > 0 {
			if oversized || len(records) >= repeatFailureMaxRecordsPerFile {
				invalid++
			} else if rec, ok := parseRepeatFailureRecord(line); ok {
				records = append(records, rec)
			} else {
				invalid++
			}
		}
		oversized = false
		if errors.Is(err, io.EOF) {
			return records, invalid, nil
		}
		if err != nil {
			return records, invalid, err
		}
	}
}

func parseRepeatFailureRecord(line []byte) (RepeatFailureRecord, bool) {
	var rec RepeatFailureRecord
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 || line[len(line)-1] != '}' {
		return rec, false
	}
	if err := json.Unmarshal(line, &rec); err != nil || rec.RawCommand == "" {
		return rec, false
	}
	return rec, true
}

// scrubRecord is the only way a record leaves the collector. The agent is
// the one whose file it came from, never what the line claims.
func (c *RepeatFailureCollector) scrubRecord(agent string, rec RepeatFailureRecord) RepeatFailureRecord {
	rec.Agent = agent
	rec.Backend = logscrub.ScrubString(rec.Backend)
	rec.Route = logscrub.ScrubString(rec.Route)
	rec.Command = logscrub.ScrubString(rec.Command)
	rec.RawCommand = logscrub.ScrubString(rec.RawCommand)
	if rec.ExitCode != nil {
		code := *rec.ExitCode
		rec.ExitCode = &code
		rec.Failed = code != 0
	}
	if rec.Time.IsZero() {
		rec.Time = c.now()
	}
	return rec
}

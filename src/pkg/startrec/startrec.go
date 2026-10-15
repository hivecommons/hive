// Package startrec records each start of the hive process and how the
// previous run ended, and keeps the Go runtime's crash trace of each start
// (hivecommons/hive#11945). An operator with no shell on the host reads both
// through the admin MCP's log_read tool as the sources "starts" and "crash".
//
// The files live in the output log directory (pkg/logread.OutputDir), which
// the container start-up script keeps owned by the hive's own user with mode
// 0700, so agent users cannot read them; every file is created 0600.
//
//   - start-current.json is the running start's record: its identifier, start
//     time, when it was last seen alive (rewritten by Heartbeat about every 30
//     seconds, with the last memory and out-of-memory reading) and, once
//     Close ran, how it ended.
//   - starts.log holds one line per start (the newest MaxStartLines), each
//     carrying the verdict on the run before it, read from start-current.json
//     as that run left it.
//   - crash-<start time>.log is one file per start that the Go runtime writes
//     an unrecovered panic or fatal runtime error into (debug.SetCrashOutput).
//
// A process killed by SIGKILL or the out-of-memory killer runs no code on the
// way out: it leaves no trace, and its start reads not-closed.
package startrec

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/logread"
)

const (
	// VerdictClean: the previous run finished shutting down.
	VerdictClean = "clean"
	// VerdictNotClosed: the previous run stopped without finishing shutdown,
	// for any reason (SIGKILL, out-of-memory kill, crash, a fatal start-up
	// exit, power loss).
	VerdictNotClosed = "not-closed"
	// VerdictSelfUpgradeFailed: the previous run exited after a failed
	// self-upgrade.
	VerdictSelfUpgradeFailed = "self-upgrade-failed"
	// VerdictUnknown: there is no record of a previous run.
	VerdictUnknown = "unknown"

	// HeartbeatInterval is how often a running start refreshes its
	// last-seen-alive time and memory reading.
	HeartbeatInterval = 30 * time.Second

	// LogKey is the attribute that ties a hive log line to its start.
	LogKey = "start"

	// StateFilename is the running start's record.
	StateFilename = "start-current.json"
	// StartsFilename holds one line per start: the starts source.
	StartsFilename = logread.SourceStarts + ".log"

	// MaxStartLines bounds starts.log.
	MaxStartLines = 200
	// MaxCrashTraces is how many crash files are kept, the running start's
	// file included.
	MaxCrashTraces = 10
	// MaxCrashBytes bounds each kept crash trace.
	MaxCrashBytes = 1 << 20

	// LimitUnlimited and LimitUnknown are MemoryReading.Limit values.
	LimitUnlimited = "unlimited"
	LimitUnknown   = "unknown"

	idBytes  = 4
	fileMode = 0o600
	dirMode  = 0o700
	// maxStartsFileBytes bounds what Begin reads back of starts.log.
	maxStartsFileBytes = 4 << 20
	// maxHeaderBytes bounds what is read to find a crash file's first line.
	maxHeaderBytes = 4 << 10
)

// MemoryReading is one memory and out-of-memory reading of the hive's
// container. Field names avoid the words the admin MCP masks.
type MemoryReading struct {
	ReadAt    string `json:"read_at,omitempty"`
	UsedBytes int64  `json:"used_bytes,omitempty"`
	// Limit is LimitUnlimited, LimitUnknown or a set limit such as
	// "2048 MiB"; LimitBytes repeats a set limit in bytes.
	Limit      string `json:"limit"`
	LimitBytes int64  `json:"limit_bytes,omitempty"`
	// OOMKills is the container's out-of-memory kill count; nil when the
	// container's memory controller does not report one.
	OOMKills *int64 `json:"oom_kills,omitempty"`
}

// state is start-current.json.
type state struct {
	Start         string         `json:"start"`
	PID           int            `json:"pid"`
	StartedAt     string         `json:"started_at"`
	LastSeenAlive string         `json:"last_seen_alive"`
	Ended         string         `json:"ended,omitempty"`
	EndedAt       string         `json:"ended_at,omitempty"`
	CrashFile     string         `json:"crash_file,omitempty"`
	Memory        *MemoryReading `json:"memory,omitempty"`
}

// Previous is what a start records about the run before it.
type Previous struct {
	Verdict       string         `json:"verdict"`
	Start         string         `json:"start,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	LastSeenAlive string         `json:"last_seen_alive,omitempty"`
	EndedAt       string         `json:"ended_at,omitempty"`
	Memory        *MemoryReading `json:"memory,omitempty"`
	// CrashTrace is set when that run left a crash trace (source crash).
	CrashTrace bool `json:"crash_trace,omitempty"`
}

// Record is one line of starts.log. Time and level come first, in the shape
// of the hive's own slog JSON lines, so log_read can filter on them.
type Record struct {
	Time      string   `json:"time"`
	Level     string   `json:"level"`
	Msg       string   `json:"msg"`
	Start     string   `json:"start"`
	PID       int      `json:"pid"`
	StartedAt string   `json:"started_at"`
	Previous  Previous `json:"previous"`
}

// Options carries what Begin needs. Dir and ID are required.
type Options struct {
	// Dir is the output log directory.
	Dir string
	// ID is this start's short identifier (NewID).
	ID string
	// PID defaults to os.Getpid().
	PID int
	// Now defaults to time.Now.
	Now func() time.Time
	// Memory takes a memory and out-of-memory reading; nil, or a false
	// second result, records none.
	Memory func() (MemoryReading, bool)
	// SetCrashOutput installs the crash file; nil uses debug.SetCrashOutput.
	SetCrashOutput func(f *os.File) error
}

// Recorder is the running start's record. Its methods are safe for
// concurrent use and do nothing on a nil Recorder.
type Recorder struct {
	opts Options

	mu       sync.Mutex
	st       state
	closed   bool
	previous Previous
	crashErr error

	beatOnce sync.Once
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewID returns a start identifier of 8 hex characters.
func NewID() string {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", uint32(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// Begin records a new start in opts.Dir: it judges how the previous run
// ended, keeps that run's crash trace, points the Go runtime's crash output
// at a new file for this start, writes this start's record and appends its
// line to starts.log. Only the process that becomes the running hive may call
// it (after the singleton lock). A crash output that cannot be set up is not
// fatal; CrashOutputErr reports it.
func Begin(opts Options) (*Recorder, error) {
	if strings.TrimSpace(opts.Dir) == "" || strings.TrimSpace(opts.ID) == "" {
		return nil, errors.New("start record needs a directory and an identifier")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	if opts.SetCrashOutput == nil {
		opts.SetCrashOutput = func(f *os.File) error { return debug.SetCrashOutput(f, debug.CrashOptions{}) }
	}
	if err := os.MkdirAll(opts.Dir, dirMode); err != nil {
		return nil, err
	}
	_ = os.Chmod(opts.Dir, dirMode)

	r := &Recorder{opts: opts}
	now := r.now()
	prevState, havePrev := readState(opts.Dir)
	r.previous = judge(prevState, havePrev)
	prevTrace := keepCrashTraces(opts.Dir, prevState.CrashFile)
	if havePrev {
		r.previous.CrashTrace = prevTrace
	}

	r.st = state{
		Start:         opts.ID,
		PID:           opts.PID,
		StartedAt:     stamp(now),
		LastSeenAlive: stamp(now),
	}
	r.st.CrashFile, r.crashErr = openCrashOutput(opts, now)
	r.st.Memory = r.reading(now)
	if err := writeState(opts.Dir, r.st); err != nil {
		return nil, err
	}
	if err := appendStart(opts.Dir, r.record(now)); err != nil {
		return r, err
	}
	return r, nil
}

func (r *Recorder) now() time.Time { return r.opts.Now().UTC() }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ID is this start's identifier.
func (r *Recorder) ID() string {
	if r == nil {
		return ""
	}
	return r.opts.ID
}

// Previous is what this start recorded about the run before it.
func (r *Recorder) Previous() Previous {
	if r == nil {
		return Previous{Verdict: VerdictUnknown}
	}
	return r.previous
}

// CrashOutputErr is why the crash output could not be set up, or nil.
func (r *Recorder) CrashOutputErr() error {
	if r == nil {
		return nil
	}
	return r.crashErr
}

// Heartbeat refreshes the last-seen-alive time and the memory reading. It
// does nothing once the record is closed.
func (r *Recorder) Heartbeat() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	now := r.now()
	r.st.LastSeenAlive = stamp(now)
	if m := r.reading(now); m != nil {
		r.st.Memory = m
	}
	return writeState(r.opts.Dir, r.st)
}

// StartHeartbeat calls Heartbeat every interval until Close. Calls after the
// first do nothing.
func (r *Recorder) StartHeartbeat(interval time.Duration) {
	if r == nil || interval <= 0 {
		return
	}
	r.beatOnce.Do(func() {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.stop, r.done = make(chan struct{}), make(chan struct{})
		stop, done := r.stop, r.done
		r.mu.Unlock()
		go func() {
			defer close(done)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					_ = r.Heartbeat()
				}
			}
		}()
	})
}

func (r *Recorder) stopHeartbeat() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		stop, done := r.stop, r.done
		r.mu.Unlock()
		if stop != nil {
			close(stop)
			<-done
		}
	})
}

// Close records how this run ended: VerdictSelfUpgradeFailed for an exit
// after a failed self-upgrade, otherwise VerdictClean. The first call wins.
func (r *Recorder) Close(verdict string) error {
	if r == nil {
		return nil
	}
	r.stopHeartbeat()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if verdict != VerdictSelfUpgradeFailed {
		verdict = VerdictClean
	}
	now := r.now()
	r.st.LastSeenAlive = stamp(now)
	r.st.Ended, r.st.EndedAt = verdict, stamp(now)
	if m := r.reading(now); m != nil {
		r.st.Memory = m
	}
	return writeState(r.opts.Dir, r.st)
}

func (r *Recorder) reading(now time.Time) *MemoryReading {
	if r.opts.Memory == nil {
		return nil
	}
	m, ok := r.opts.Memory()
	if !ok {
		return nil
	}
	m.ReadAt = stamp(now)
	if m.Limit == "" {
		m.Limit = LimitUnknown
	}
	return &m
}

func (r *Recorder) record(now time.Time) Record {
	level := "INFO"
	if r.previous.Verdict == VerdictNotClosed || r.previous.Verdict == VerdictSelfUpgradeFailed {
		level = "WARN"
	}
	return Record{
		Time:      stamp(now),
		Level:     level,
		Msg:       "hive start " + r.opts.ID + ": previous run " + r.previous.Verdict,
		Start:     r.opts.ID,
		PID:       r.opts.PID,
		StartedAt: r.st.StartedAt,
		Previous:  r.previous,
	}
}

// judge turns the previous run's record, as that run left it, into a
// verdict.
func judge(prev state, ok bool) Previous {
	if !ok {
		return Previous{Verdict: VerdictUnknown}
	}
	p := Previous{
		Start:         prev.Start,
		StartedAt:     prev.StartedAt,
		LastSeenAlive: prev.LastSeenAlive,
		EndedAt:       prev.EndedAt,
		Memory:        prev.Memory,
	}
	switch prev.Ended {
	case VerdictClean:
		p.Verdict = VerdictClean
	case VerdictSelfUpgradeFailed:
		p.Verdict = VerdictSelfUpgradeFailed
	default:
		p.Verdict = VerdictNotClosed
	}
	return p
}

func readState(dir string) (state, bool) {
	data, err := os.ReadFile(filepath.Join(dir, StateFilename))
	if err != nil {
		return state{}, false
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil || st.Start == "" {
		return state{}, false
	}
	return st, true
}

func writeState(dir string, st state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, StateFilename), append(data, '\n'))
}

// writeFileAtomic replaces path with data through a 0600 temporary file in
// the same directory, so a kill mid-write leaves the old or the new content.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	_ = f.Chmod(fileMode)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// appendStart adds rec to starts.log, keeping the newest MaxStartLines lines.
func appendStart(dir string, rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, StartsFilename)
	var lines [][]byte
	if data, err := readTail(path, maxStartsFileBytes); err == nil {
		for _, l := range bytes.Split(data, []byte("\n")) {
			if len(bytes.TrimSpace(l)) > 0 {
				lines = append(lines, l)
			}
		}
	}
	lines = append(lines, line)
	if len(lines) > MaxStartLines {
		lines = lines[len(lines)-MaxStartLines:]
	}
	return writeFileAtomic(path, append(bytes.Join(lines, []byte("\n")), '\n'))
}

// readTail reads at most n bytes from the end of path, dropping a partial
// first line when it did not start at the beginning.
func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(st.Size()-n, 0)
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}
	return buf, nil
}

// crashHeader is the first line of each crash file, in the shape of the
// hive's slog JSON lines.
type crashHeader struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Start string `json:"start"`
}

// openCrashOutput creates this start's crash file and points the Go
// runtime's crash output at it. The runtime keeps its own copy of the
// descriptor, so the file is closed here.
func openCrashOutput(opts Options, now time.Time) (string, error) {
	name := logread.HistoryFileName(logread.SourceCrash, now)
	var f *os.File
	var err error
	for i := 0; i < 1000; i++ {
		f, err = os.OpenFile(filepath.Join(opts.Dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, fileMode)
		if !errors.Is(err, os.ErrExist) {
			break
		}
		now = now.Add(time.Millisecond)
		name = logread.HistoryFileName(logread.SourceCrash, now)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	_ = f.Chmod(fileMode)
	header, _ := json.Marshal(crashHeader{
		Time:  stamp(now),
		Level: "INFO",
		Msg:   "crash output of hive start " + opts.ID + ": any lines after this one are the Go runtime's trace of how that process died",
		Start: opts.ID,
	})
	if _, err := f.Write(append(header, '\n')); err != nil {
		return name, err
	}
	if err := opts.SetCrashOutput(f); err != nil {
		return name, err
	}
	return name, nil
}

// keepCrashTraces bounds the crash files before a new one is created: a file
// holding only its header (its process did not crash) is removed, a trace
// over MaxCrashBytes is cut to that size, and only the newest
// MaxCrashTraces-1 traces are kept so the new start's file makes
// MaxCrashTraces. It reports whether prevFile holds a trace.
func keepCrashTraces(dir, prevFile string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	prefix := logread.SourceCrash + "-"
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") {
			names = append(names, name)
		}
	}
	// The time in the name sorts lexically; newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	kept, prevTrace := 0, false
	for _, name := range names {
		path := filepath.Join(dir, name)
		if !hasTrace(path) || kept >= MaxCrashTraces-1 {
			_ = os.Remove(path)
			continue
		}
		kept++
		if st, err := os.Stat(path); err == nil && st.Size() > MaxCrashBytes {
			_ = os.Truncate(path, MaxCrashBytes)
		}
		if prevFile != "" && name == prevFile {
			prevTrace = true
		}
	}
	return prevTrace
}

// hasTrace reports whether a crash file holds anything after its header.
func hasTrace(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	buf := make([]byte, min(st.Size(), maxHeaderBytes))
	n, _ := f.ReadAt(buf, 0)
	buf = buf[:n]
	i := bytes.IndexByte(buf, '\n')
	if i < 0 || !bytes.HasPrefix(buf, []byte(`{"time":`)) {
		// No header line of ours: keep whatever is there.
		return st.Size() > 0
	}
	return st.Size() > int64(i+1)
}

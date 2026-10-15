package logread

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Output logs (hivecommons/hive#11944) keep what the processes around the hive
// print: the dashboard auth proxy, the hive itself before its own log is set
// up, and the local litellm proxy, plus the dashboard's request log. Each is a
// source of its own in one directory on the data volume. The Node auth proxy
// writes the same file layout from src/proxy/output_log.js; keep the two in
// step.
const (
	SourceProxy      = "proxy"
	SourceHiveOutput = "hive-output"
	SourceLiteLLM    = "litellm"
	SourceRequests   = "requests"
	// SourceStarts and SourceCrash are written by pkg/startrec
	// (hivecommons/hive#11945): starts.log holds one line per hive start, and
	// each start's crash trace is crash-<start time>.log.
	SourceStarts = "starts"
	SourceCrash  = "crash"

	StreamStdout = "stdout"
	StreamStderr = "stderr"

	// OutputDirEnv overrides DefaultOutputDir for every writer and the reader.
	OutputDirEnv = "HIVE_OUTPUT_LOG_DIR"
	// DefaultOutputDir sits under /data/.hive, which the container start-up
	// script keeps owned by the hive's own user with mode 0700, so agent users
	// cannot read it.
	DefaultOutputDir = "/data/.hive/logs"

	// OutputMaxFileBytes, OutputMaxFiles and OutputMaxAge bound every output
	// source: at most OutputMaxFiles files (the current one included) of
	// OutputMaxFileBytes each, and no file whose newest line is older than
	// OutputMaxAge.
	OutputMaxFileBytes int64 = 10 << 20
	OutputMaxFiles           = 5
	OutputMaxAge             = 7 * 24 * time.Hour

	// outputLineMaxBytes splits a line that never ends into records of at
	// most this size, so one runaway write cannot grow memory without bound.
	outputLineMaxBytes = 64 * 1024
	// outputPruneEvery prunes by age on a quiet source that never rotates.
	outputPruneEvery = time.Hour

	outputFileMode = 0o600
	outputDirMode  = 0o700
)

// OutputDir returns the output log directory: OutputDirEnv when set, else
// DefaultOutputDir.
func OutputDir(getenv func(string) string) string {
	if getenv != nil {
		if dir := strings.TrimSpace(getenv(OutputDirEnv)); dir != "" {
			return dir
		}
	}
	return DefaultOutputDir
}

// outputRecord is one stamped line of process output on disk.
type outputRecord struct {
	Time   string `json:"time"`
	Stream string `json:"stream,omitempty"`
	Text   string `json:"text"`
}

var errOutputLogClosed = errors.New("output log is closed")

// OutputLog is one output source's current file, <stem>.log, with its rotated
// history <stem>-<time>.log. Files are created 0600 and the directory 0700.
// It is safe for concurrent use.
type OutputLog struct {
	mu       sync.Mutex
	dir      string
	stem     string
	maxBytes int64
	maxFiles int
	maxAge   time.Duration
	now      func() time.Time

	f       *os.File
	size    int64
	pruned  time.Time
	streams []*outputStream
}

// OpenOutputLog opens (or creates) the source's current file in dir for
// appending, so output from an earlier run stays readable, and prunes history
// beyond the bounds.
func OpenOutputLog(dir, stem string) (*OutputLog, error) {
	return openOutputLog(dir, stem, time.Now)
}

func openOutputLog(dir, stem string, now func() time.Time) (*OutputLog, error) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(stem) == "" {
		return nil, errors.New("output log needs a directory and a name")
	}
	if err := os.MkdirAll(dir, outputDirMode); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, outputDirMode)
	l := &OutputLog{
		dir:      dir,
		stem:     stem,
		maxBytes: OutputMaxFileBytes,
		maxFiles: OutputMaxFiles,
		maxAge:   OutputMaxAge,
		now:      now,
	}
	if err := l.openCurrent(); err != nil {
		return nil, err
	}
	l.prune()
	return l, nil
}

func (l *OutputLog) currentPath() string { return filepath.Join(l.dir, l.stem+".log") }

func (l *OutputLog) openCurrent() error {
	f, err := os.OpenFile(l.currentPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, outputFileMode)
	if err != nil {
		return err
	}
	_ = f.Chmod(outputFileMode)
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends p, which must be whole lines, as it is. The request log's
// slog handler writes one record per call through it.
func (l *OutputLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.writeLocked(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (l *OutputLog) writeLocked(rec []byte) error {
	if l.f == nil {
		return errOutputLogClosed
	}
	if l.size > 0 && l.size+int64(len(rec)) > l.maxBytes {
		if err := l.rotate(); err != nil {
			return err
		}
	} else if l.now().Sub(l.pruned) >= outputPruneEvery {
		l.prune()
	}
	n, err := l.f.Write(rec)
	l.size += int64(n)
	return err
}

// rotate renames the current file to <stem>-<time>.log, starts a new one, and
// prunes. The backup's time is when it was closed, so it is also the time of
// its newest line.
func (l *OutputLog) rotate() error {
	_ = l.f.Close()
	l.f = nil
	stamp := l.now().UTC()
	backup := l.backupPath(stamp)
	for i := 0; i < 1000; i++ {
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			break
		}
		stamp = stamp.Add(time.Millisecond)
		backup = l.backupPath(stamp)
	}
	renameErr := os.Rename(l.currentPath(), backup)
	if err := l.openCurrent(); err != nil {
		return err
	}
	if renameErr != nil && !errors.Is(renameErr, os.ErrNotExist) {
		return renameErr
	}
	l.prune()
	return nil
}

func (l *OutputLog) backupPath(stamp time.Time) string {
	return filepath.Join(l.dir, HistoryFileName(l.stem, stamp))
}

// HistoryFileName names a dated history file of a source,
// <stem>-<time>.log, in the layout reads list newest first.
func HistoryFileName(stem string, stamp time.Time) string {
	return stem + "-" + stamp.UTC().Format(backupTimeFormat) + ".log"
}

// prune removes history beyond the file count and age bounds, and empties a
// current file whose newest line is older than the age bound.
func (l *OutputLog) prune() {
	now := l.now()
	l.pruned = now
	if l.f != nil && l.size > 0 {
		if st, err := l.f.Stat(); err == nil && now.Sub(st.ModTime()) > l.maxAge {
			if l.f.Truncate(0) == nil {
				l.size = 0
			}
		}
	}
	files, err := historyFiles(l.dir, l.stem)
	if err != nil {
		return
	}
	kept := 1
	for _, f := range files {
		if f.current {
			continue
		}
		if kept >= l.maxFiles || now.Sub(f.stamp) > l.maxAge {
			_ = os.Remove(f.path)
			continue
		}
		kept++
	}
}

// Stream returns a writer that keeps each line written to it as a record
// stamped with its time and the stream name. It never fails, so it can sit
// behind io.MultiWriter next to the process's real output without changing
// what that output receives. A line still open when the log is closed is kept
// as it is.
func (l *OutputLog) Stream(name string) io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &outputStream{log: l, name: name}
	l.streams = append(l.streams, s)
	return s
}

// Close keeps any unfinished lines and closes the current file.
func (l *OutputLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.streams {
		if len(s.buf) > 0 {
			s.emitLocked(s.buf)
			s.buf = nil
		}
	}
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

type outputStream struct {
	log  *OutputLog
	name string
	buf  []byte
}

func (s *outputStream) Write(p []byte) (int, error) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		s.emitLocked(s.buf[:i])
		s.buf = s.buf[i+1:]
	}
	for len(s.buf) >= outputLineMaxBytes {
		s.emitLocked(s.buf[:outputLineMaxBytes])
		s.buf = s.buf[outputLineMaxBytes:]
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
	return len(p), nil
}

func (s *outputStream) emitLocked(line []byte) {
	rec, _ := json.Marshal(outputRecord{
		Time:   s.log.now().UTC().Format(time.RFC3339Nano),
		Stream: s.name,
		Text:   strings.TrimSuffix(string(line), "\r"),
	})
	_ = s.log.writeLocked(append(rec, '\n'))
}

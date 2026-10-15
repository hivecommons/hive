package logread

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupTimeFormat is the timestamp lumberjack puts in a rotated file's name
// (hive-<stamp>.log, compressed to hive-<stamp>.log.gz).
const backupTimeFormat = "2006-01-02T15-04-05.000"

const hiveSourceDescription = "The hive's own log: governor.logging.dir/hive.log and its rotated, compressed history within the configured retention."

// logFile is one file of a source's history, newest first in a listing.
type logFile struct {
	path    string
	name    string
	gz      bool
	current bool
	stamp   time.Time
	// fh, when set, is the current file opened as it was listed, so a
	// rotation between listing and reading cannot swap in the new file.
	fh *os.File
}

func hiveSourceInfo(dir string) SourceInfo {
	info := SourceInfo{Name: SourceHive, Description: hiveSourceDescription, Default: true}
	if strings.TrimSpace(dir) == "" {
		info.Reason = "no log directory is configured (governor.logging.dir is empty)"
		return info
	}
	files, err := hiveFiles(dir)
	if err != nil {
		info.Reason = "the hive's log directory cannot be listed"
		return info
	}
	if len(files) == 0 {
		info.Reason = "the hive has not written a log file in its log directory yet"
		return info
	}
	info.Available = true
	return info
}

// hiveFiles lists the hive log and its rotated history, newest first.
func hiveFiles(dir string) ([]logFile, error) { return historyFiles(dir, "hive") }

// historyFiles lists <stem>.log and its rotated <stem>-<time>.log[.gz]
// history, newest first. When a rotated file exists both plain and
// compressed, compression is still in progress and the complete plain copy is
// read.
func historyFiles(dir, stem string) ([]logFile, error) {
	currentName, prefix := stem+".log", stem+"-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var current *logFile
	plain := map[string]bool{}
	var backups []logFile
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if name == currentName {
			current = &logFile{path: filepath.Join(dir, name), name: name, current: true}
			continue
		}
		base, gz := strings.CutSuffix(name, ".gz")
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".log") {
			continue
		}
		stamp, err := time.Parse(backupTimeFormat, strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".log"))
		if err != nil {
			continue
		}
		if !gz {
			plain[base] = true
		}
		backups = append(backups, logFile{path: filepath.Join(dir, name), name: name, gz: gz, stamp: stamp})
	}
	out := make([]logFile, 0, len(backups)+1)
	if current != nil {
		out = append(out, *current)
	}
	sort.SliceStable(backups, func(i, j int) bool { return backups[i].stamp.After(backups[j].stamp) })
	for _, b := range backups {
		if b.gz && plain[strings.TrimSuffix(b.name, ".gz")] {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// region is the part of a file's content one scan examines: data holds the
// bytes [base, base+len(data)) of the (decompressed) content.
type region struct {
	data    []byte
	base    int64
	partial bool
	// head is the file's first line, for its cursor identity.
	head []byte
}

var errCursorBeyondFile = errors.New("cursor offset is beyond the end of the file")

// loadPlain reads up to budget bytes of a plain file, ending at end (or the
// end of the file when end < 0).
func loadPlain(f logFile, end, budget int64) (region, error) {
	fh := f.fh
	if fh == nil {
		opened, err := os.Open(f.path)
		if err != nil {
			return region{}, err
		}
		defer opened.Close()
		fh = opened
	}
	st, err := fh.Stat()
	if err != nil {
		return region{}, err
	}
	size := st.Size()
	if end > size {
		return region{}, errCursorBeyondFile
	}
	if end < 0 {
		end = size
	}
	start := max(end-budget, 0)
	buf := make([]byte, end-start)
	n, err := fh.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return region{}, err
	}
	return region{data: buf[:n], base: start, head: firstLine(fh, buf[:n], start)}, nil
}

// firstLine returns a plain file's first line, reusing the loaded region when
// it starts at offset zero.
func firstLine(fh *os.File, data []byte, base int64) []byte {
	if base == 0 {
		return headOf(data)
	}
	buf := make([]byte, maxHeadBytes)
	n, _ := fh.ReadAt(buf, 0)
	return headOf(buf[:n])
}

const maxHeadBytes = 64 * 1024

func headOf(data []byte) []byte {
	if i := indexNewline(data); i >= 0 {
		return data[:i]
	}
	return nil
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// loadGzip decompresses a rotated file. Decompression counts against the
// budget: more than budget decompressed bytes is reported as tooLarge. A
// truncated or corrupt stream is read as far as it goes and flagged partial.
func loadGzip(f logFile, end, budget int64) (r region, tooLarge bool, err error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return region{}, false, err
	}
	defer fh.Close()
	limit := budget
	if end >= 0 && end < limit {
		limit = end
	}
	zr, zerr := gzip.NewReader(fh)
	if zerr != nil {
		return region{partial: true}, false, nil
	}
	defer zr.Close()
	data, rerr := io.ReadAll(io.LimitReader(zr, limit+1))
	r.partial = rerr != nil
	if int64(len(data)) > limit {
		if end < 0 || end > limit {
			return region{head: gzipHead(f.path)}, true, nil
		}
		data = data[:limit]
	}
	if end > int64(len(data)) {
		return region{}, false, errCursorBeyondFile
	}
	if end >= 0 {
		data = data[:end]
	}
	r.data = data
	r.head = headOf(data)
	return r, false, nil
}

// gzipHead decompresses just the first line of a compressed history file.
func gzipHead(path string) []byte {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil
	}
	defer zr.Close()
	data, _ := io.ReadAll(io.LimitReader(zr, maxHeadBytes))
	return headOf(data)
}

// fileHead returns the first line of any history file.
func fileHead(f logFile) []byte {
	if f.gz {
		return gzipHead(f.path)
	}
	r, err := loadPlain(f, -1, 0)
	if err != nil {
		return nil
	}
	return r.head
}

package logread

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// position is a line start in one history file.
type position struct {
	file   *logFile
	head   []byte
	offset int64
}

type scan struct {
	req     Request
	opts    Options
	res     *Result
	want    int
	budget  int64
	include string
	exclude []string
	minRank int

	collected  []Line
	usedBytes  int
	lineBudget int
	counts     map[string]int

	lastCollected *position
	lastExamined  *position
	nextCursor    *position

	oldest     time.Time
	newest     time.Time
	stopped    bool
	incomplete bool
	stopReason string
}

func newScan(req Request, opts Options, res *Result) *scan {
	bound := WorkBound(opts.WorkBoundBytes)
	s := &scan{
		req:        req,
		opts:       opts,
		res:        res,
		want:       res.LinesApplied,
		budget:     bound,
		include:    strings.ToLower(req.Include),
		minRank:    levelRank(req.MinLevel),
		lineBudget: MaxResultBytes - resultMetadataBudget,
		counts:     map[string]int{},
	}
	for _, ex := range req.Exclude {
		if ex = strings.ToLower(ex); ex != "" {
			s.exclude = append(s.exclude, ex)
		}
	}
	return s
}

func (s *scan) run() {
	files, err := hiveFiles(s.opts.LogDir)
	if err != nil || len(files) == 0 {
		s.res.Status = StatusNotAvailable
		s.res.Reason = "the hive's log files cannot be listed"
		return
	}
	if files[0].current {
		if fh, err := os.Open(files[0].path); err == nil {
			defer fh.Close()
			files[0].fh = fh
		}
	}
	s.retention(files[len(files)-1])
	start, end := 0, int64(-1)
	if s.req.Cursor != "" {
		c, err := decodeCursor(s.req.Cursor)
		if err != nil || c.Source != s.res.Source {
			s.expire("the cursor is not one this hive issued for this source; start again without a cursor")
			return
		}
		idx := locate(files, c)
		if idx < 0 {
			s.expire("the history file this cursor points into is no longer retained; start again without a cursor")
			return
		}
		start, end = idx, c.Offset
	}
	for i := start; i < len(files) && !s.stopped; i++ {
		fileEnd := int64(-1)
		if i == start {
			fileEnd = end
		}
		if err := s.scanFile(&files[i], fileEnd); err != nil {
			if errors.Is(err, errCursorBeyondFile) {
				s.expire("the history file this cursor points into no longer holds that position; start again without a cursor")
				return
			}
			s.res.PartialFiles = append(s.res.PartialFiles, files[i].name)
		}
	}
	s.finish()
}

// retention reports the oldest retained line, and flags a window that starts
// before it.
func (s *scan) retention(oldest logFile) {
	when, _ := parseHead(string(fileHead(oldest)))
	if when.IsZero() {
		return
	}
	s.res.OldestRetained = when.UTC().Format(time.RFC3339Nano)
	if !s.req.Since.IsZero() && s.req.Since.Before(when) {
		s.res.WindowBeforeOldestRetained = true
	}
}

func (s *scan) expire(reason string) {
	s.res.Status = StatusCursorExpired
	s.res.Reason = reason
}

// locate finds the file a cursor points into by its first-line identity.
func locate(files []logFile, c cursor) int {
	for i, f := range files {
		if t, digest, ok := fileIdentity(fileHead(f)); ok && digest == c.Digest && t == c.Time {
			return i
		}
	}
	return -1
}

func (s *scan) scanFile(f *logFile, end int64) error {
	var r region
	if f.gz {
		var tooLarge bool
		var err error
		r, tooLarge, err = loadGzip(*f, end, s.budget)
		if err != nil {
			return err
		}
		if tooLarge {
			first := s.res.Coverage.ScannedBytes == 0
			s.charge(s.budget)
			if first {
				// Not even a fresh work bound covers this file, so no cursor
				// could make progress past it.
				s.stop(StoppedFileTooLarge)
				return nil
			}
			s.stopAtEnd(f, r.head)
			return nil
		}
	} else {
		var err error
		r, err = loadPlain(*f, end, s.budget)
		if errors.Is(err, os.ErrNotExist) && !f.current {
			// Compression finished between listing and reading: the same
			// content now lives in the .gz copy.
			f.gz, f.path, f.name = true, f.path+".gz", f.name+".gz"
			return s.scanFile(f, end)
		}
		if err != nil {
			return err
		}
	}
	s.charge(int64(len(r.data)))
	s.res.Coverage.FilesScanned++
	if r.partial {
		s.res.PartialFiles = append(s.res.PartialFiles, f.name)
	}
	lines, offsets := splitRegion(r.data, r.base)
	regionEnd := r.base + int64(len(r.data))
	if len(lines) > 0 && f.current && end < 0 && r.data[len(r.data)-1] != '\n' {
		// The hive is still writing its newest record; it is read next time.
		regionEnd = offsets[len(offsets)-1]
		lines, offsets = lines[:len(lines)-1], offsets[:len(offsets)-1]
	} else if len(lines) > 0 && !f.current && r.data[len(r.data)-1] != '\n' && end < 0 && !r.partial {
		s.res.PartialFiles = append(s.res.PartialFiles, f.name)
	}
	spans := keySpans(lines)
	for i := len(lines) - 1; i >= 0 && !s.stopped; i-- {
		s.consider(position{file: f, head: r.head, offset: offsets[i]}, lines[i], spans[i])
	}
	if s.stopped {
		return nil
	}
	if r.base > 0 {
		// The work bound ended this file's region before its first line: the
		// next page ends at the oldest whole line examined here, or where this
		// region ended when it held no whole line.
		if len(offsets) == 0 && s.res.Coverage.ScannedBytes == int64(len(r.data)) {
			// One line is longer than a whole work bound.
			s.stop(StoppedLineTooLarge)
			return nil
		}
		pos := position{file: f, head: r.head, offset: regionEnd}
		if len(offsets) > 0 {
			pos.offset = offsets[0]
		}
		s.nextCursor = &pos
		s.stop(StoppedWorkBound)
	}
	return nil
}

func (s *scan) charge(n int64) {
	s.budget -= n
	s.res.Coverage.ScannedBytes += n
}

// stopAtEnd stops for the work bound before a compressed file that does not
// fit, with a cursor that resumes from the end of that file.
func (s *scan) stopAtEnd(f *logFile, head []byte) {
	if len(head) > 0 {
		s.nextCursor = &position{file: f, head: head, offset: -1}
	}
	s.stop(StoppedWorkBound)
}

func (s *scan) stop(reason string) {
	s.stopped = true
	s.incomplete = true
	s.stopReason = reason
}

// splitRegion splits a region into lines and their content offsets. A region
// that does not start at the beginning of its file starts mid-line; that
// fragment is dropped and read by the page that covers it.
func splitRegion(data []byte, base int64) ([]string, []int64) {
	pos := 0
	if base > 0 {
		i := indexNewline(data)
		if i < 0 {
			return nil, nil
		}
		pos = i + 1
	}
	var lines []string
	var offsets []int64
	for pos < len(data) {
		next := indexNewline(data[pos:])
		end := len(data)
		if next >= 0 {
			end = pos + next
		}
		lines = append(lines, strings.TrimSuffix(string(data[pos:end]), "\r"))
		offsets = append(offsets, base+int64(pos))
		if next < 0 {
			break
		}
		pos = end + 1
	}
	return lines, offsets
}

func (s *scan) consider(pos position, raw string, spans []span) {
	s.lastExamined = &pos
	when, level := parseHead(raw)
	if !when.IsZero() {
		if s.newest.IsZero() || when.After(s.newest) {
			s.newest = when
		}
		if s.oldest.IsZero() || when.Before(s.oldest) {
			s.oldest = when
		}
	}
	windowed := !s.req.Since.IsZero() || !s.req.Until.IsZero()
	if windowed {
		if when.IsZero() {
			s.res.UntimedDropped++
			return
		}
		if !s.req.Until.IsZero() && when.After(s.req.Until) {
			return
		}
		if !s.req.Since.IsZero() && when.Before(s.req.Since) {
			s.stopped = true
			return
		}
	}
	if s.minRank > 0 {
		if level == "" {
			s.res.UnlevelledDropped++
			return
		}
		if levelRank(level) < s.minRank {
			return
		}
	}
	text, counts, pre := scrubLine(raw, spans, s.opts.Secrets)
	lower := strings.ToLower(text)
	if s.include != "" && !strings.Contains(lower, s.include) {
		return
	}
	for _, ex := range s.exclude {
		if strings.Contains(lower, ex) {
			return
		}
	}
	if len(s.collected) >= s.want {
		s.more()
		return
	}
	line := Line{Source: s.res.Source, Level: level, Text: text, PreExistingMarker: pre}
	if !when.IsZero() {
		line.Time = when.UTC().Format(time.RFC3339Nano)
	}
	if len(text) > MaxLineBytes {
		line.Text, line.Cut, line.FullBytes = cutText(text, MaxLineBytes), true, len(text)
	}
	size := jsonSize(line)
	if s.usedBytes+size > s.lineBudget {
		s.res.ByteCapped = true
		s.more()
		return
	}
	s.usedBytes += size
	s.collected = append(s.collected, line)
	for k, v := range counts {
		s.counts[k] += v
	}
	if pre {
		s.res.PreExistingMarkerLines++
	}
	if line.Cut {
		s.res.CutLines++
	}
	s.lastCollected = &pos
}

// more stops a read that has another matching line it cannot return; the
// cursor resumes just before the oldest line returned.
func (s *scan) more() {
	s.res.HasMore = true
	s.nextCursor = s.lastCollected
	s.stopped = true
}

func cutText(text string, n int) string {
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

func jsonSize(line Line) int {
	b, _ := json.Marshal(line)
	return len(b) + 1
}

func (s *scan) finish() {
	for i, j := 0, len(s.collected)-1; i < j; i, j = i+1, j-1 {
		s.collected[i], s.collected[j] = s.collected[j], s.collected[i]
	}
	s.res.Lines = s.collected
	s.res.LineCount = len(s.collected)
	kinds := make([]string, 0, len(s.counts))
	for k := range s.counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		s.res.Redactions = append(s.res.Redactions, Redaction{Kind: k, Count: s.counts[k]})
	}
	cov := &s.res.Coverage
	cov.Complete = !s.incomplete
	cov.StoppedReason = s.stopReason
	if !s.oldest.IsZero() {
		cov.CoveredFrom = s.oldest.UTC().Format(time.RFC3339Nano)
		cov.CoveredUntil = s.newest.UTC().Format(time.RFC3339Nano)
	}
	if s.incomplete && s.nextCursor == nil && s.stopReason == StoppedWorkBound {
		s.nextCursor = s.lastExamined
	}
	if s.nextCursor != nil {
		if t, digest, ok := fileIdentity(s.nextCursor.head); ok {
			s.res.NextCursor = encodeCursor(cursor{Source: s.res.Source, Time: t, Digest: digest, Offset: s.nextCursor.offset})
		}
	}
}

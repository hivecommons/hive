package cmdlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	// fileMode keeps the log owner-only: the runtime writes it and the API
	// reads it; the agent's user never gets a handle on it.
	fileMode = 0o600
	// scanBuffer and maxLineBytes size the reader; a line holds at most
	// two policy windows of output plus the command line.
	scanBuffer   = 64 * 1024
	maxLineBytes = 16 * 1024 * 1024
)

// Log is the Recorder over one run's commands.jsonl. It appends only;
// nothing it wrote is ever rewritten.
type Log struct {
	path string
	pol  Policy

	mu   sync.Mutex
	next int64
	// open is every started seq whose end is not yet written, with its
	// start time for the duration.
	open map[int64]time.Time
}

// Open returns the Log for runDir, resuming the seq counter from a file
// that already exists so a restarted runner never reuses a number.
//
// A run has one live writer at a time (a contained run dispatches one
// command; a session's steps run one at a time), so a started seq with no
// ended line on disk belongs to a writer that is gone and will never end
// it. Open appends an unfinished ended line with OrphanReason for each,
// so a crash leaves a terminal record instead of one that pins every
// follower's NextSeq behind it for the rest of the run.
func Open(runDir string, pol Policy) (*Log, error) {
	l := &Log{path: filepath.Join(runDir, FileName), pol: pol.defaults(), next: 1, open: map[int64]time.Time{}}
	lines, err := readLines(l.path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// orphans folds the lines the way Read does: a started line opens its
	// seq, a later ended line closes it.
	orphans := map[int64]time.Time{}
	for _, ln := range lines {
		if ln.Seq >= l.next {
			l.next = ln.Seq + 1
		}
		switch ln.Event {
		case EventStarted:
			orphans[ln.Seq] = ln.At
		case EventEnded:
			delete(orphans, ln.Seq)
		}
	}
	if len(orphans) == 0 {
		return l, nil
	}
	now := l.pol.Now()
	reason := l.pol.Scrub(OrphanReason)
	for _, seq := range slices.Sorted(maps.Keys(orphans)) {
		ms := now.Sub(orphans[seq]).Milliseconds()
		if err := l.append(line{Seq: seq, Event: EventEnded, At: now, DurationMs: &ms, Status: api.CommandUnfinished, Reason: reason}); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Started appends the started line and returns its seq.
func (l *Log) Started(start Start) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.pol.Now()
	seq := l.next
	cmd := make([]string, len(start.Command))
	for i, a := range start.Command {
		cmd[i] = l.pol.Scrub(a)
	}
	capture := start.Capture
	if capture == "" {
		capture = api.CommandCaptureRunner
	}
	if err := l.append(line{Seq: seq, Event: EventStarted, At: now, Command: cmd, Cwd: start.Cwd, Capture: capture}); err != nil {
		return 0, err
	}
	l.next++
	l.open[seq] = now
	return seq, nil
}

// Ended appends the ended line for seq. The exit code is written only
// when known; the status derives from it unless end names one.
func (l *Log) Ended(seq int64, end End) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	startedAt, ok := l.open[seq]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownSeq, seq)
	}
	now := l.pol.Now()
	ms := now.Sub(startedAt).Milliseconds()
	ln := line{
		Seq: seq, Event: EventEnded, At: now,
		ExitCode: end.ExitCode, DurationMs: &ms,
		Status: deriveStatus(end), Reason: l.pol.Scrub(end.Reason),
		Stdout: summarize(l.pol.Scrub(end.Stdout), l.pol),
		Stderr: summarize(l.pol.Scrub(end.Stderr), l.pol),
	}
	if err := l.append(ln); err != nil {
		return err
	}
	delete(l.open, seq)
	return nil
}

func (l *Log) append(ln line) error {
	data, err := json.Marshal(ln)
	if err != nil {
		return fmt.Errorf("cmdlog: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("cmdlog: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("cmdlog: %w", err)
	}
	return nil
}

// Read folds runDir's log into records after sinceSeq (exclusive), at
// most limit of them (DefaultPageSize when zero, MaxPageSize at most).
// A started line with no ended line is an unfinished record still in
// flight. NextSeq never advances past an in-flight record, so a follower
// re-receives it until its ended line lands (idempotent by seq); a record
// with an ended line is terminal whatever its status, an unfinished one
// whose exit was never known included. More is false on a page whose
// NextSeq could not move past sinceSeq, so a pager never re-requests the
// same page in a loop.
func Read(runDir string, sinceSeq int64, limit int) (api.CommandPage, error) {
	lines, err := readLines(filepath.Join(runDir, FileName))
	if err != nil {
		return api.CommandPage{}, err
	}
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	bySeq := map[int64]*api.CommandRecord{}
	for _, ln := range lines {
		switch ln.Event {
		case EventStarted:
			bySeq[ln.Seq] = &api.CommandRecord{
				Seq: ln.Seq, Command: ln.Command, Cwd: ln.Cwd, Capture: ln.Capture,
				StartedAt: ln.At, Status: api.CommandUnfinished,
			}
		case EventEnded:
			rec, ok := bySeq[ln.Seq]
			if !ok {
				continue
			}
			at := ln.At
			rec.EndedAt, rec.ExitCode, rec.DurationMs = &at, ln.ExitCode, ln.DurationMs
			rec.Status, rec.Reason, rec.Stdout, rec.Stderr = ln.Status, ln.Reason, ln.Stdout, ln.Stderr
		}
	}
	all := make([]api.CommandRecord, 0, len(bySeq))
	for _, rec := range bySeq {
		if rec.Seq <= sinceSeq {
			continue
		}
		all = append(all, *rec)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	page := api.CommandPage{Commands: all, NextSeq: sinceSeq}
	if len(all) > limit {
		page.Commands, page.More = all[:limit], true
	}
	if n := len(page.Commands); n > 0 {
		page.NextSeq = page.Commands[n-1].Seq
		for _, rec := range page.Commands {
			if rec.EndedAt == nil {
				page.NextSeq = max(rec.Seq-1, sinceSeq)
				break
			}
		}
	}
	page.More = page.More && page.NextSeq > sinceSeq
	return page, nil
}

// readLines parses the file; a missing file is ErrNotFound.
func readLines(path string) ([]line, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cmdlog: %w", err)
	}
	defer f.Close()
	var out []line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBuffer), maxLineBytes)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ln line
		if err := json.Unmarshal(raw, &ln); err != nil {
			return nil, fmt.Errorf("%w %d: %v", ErrCorrupt, n, err)
		}
		out = append(out, ln)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cmdlog: %w", err)
	}
	return out, nil
}

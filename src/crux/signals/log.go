package signals

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hivecommons/hive/crux/api"
)

const (
	// fileMode keeps the log owner-only: the runtime writes it and the API
	// reads it; the agent's user never gets a handle on it.
	fileMode = 0o600
	// scanBuffer and maxLineBytes size the reader; a line holds at most
	// one message and one data payload.
	scanBuffer   = 64 * 1024
	maxLineBytes = 1024 * 1024
)

// Log is the Recorder over one run's signals.jsonl. It appends only;
// nothing it wrote is ever rewritten.
type Log struct {
	path string
	pol  Policy

	mu   sync.Mutex
	next int64
}

// Open returns the Log for runDir, resuming the seq counter from a file
// that already exists so a restarted runner never reuses a number.
func Open(runDir string, pol Policy) (*Log, error) {
	l := &Log{path: filepath.Join(runDir, FileName), pol: pol.defaults(), next: 1}
	recs, err := readAll(l.path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, rec := range recs {
		if rec.Seq >= l.next {
			l.next = rec.Seq + 1
		}
	}
	return l, nil
}

// Emit validates sig, appends it and returns the record as written.
func (l *Log) Emit(sig Signal) (api.Signal, error) {
	n, err := normalize(sig, l.pol.Scrub)
	if err != nil {
		return api.Signal{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := api.Signal{Seq: l.next, Kind: n.Kind, Source: n.Source, At: l.pol.Now().UTC(), By: n.By, Message: n.Message, Data: n.Data}
	data, err := json.Marshal(rec)
	if err != nil {
		return api.Signal{}, fmt.Errorf("signals: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return api.Signal{}, fmt.Errorf("signals: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return api.Signal{}, fmt.Errorf("signals: %w", err)
	}
	l.next++
	return rec, nil
}

// Read returns runDir's signals after sinceSeq (exclusive), at most limit
// of them (DefaultPageSize when zero, MaxPageSize at most), oldest first.
func Read(runDir string, sinceSeq int64, limit int) (api.SignalPage, error) {
	recs, err := readAll(filepath.Join(runDir, FileName))
	if err != nil {
		return api.SignalPage{}, err
	}
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	all := make([]api.Signal, 0, len(recs))
	for _, rec := range recs {
		if rec.Seq > sinceSeq {
			all = append(all, rec)
		}
	}
	page := api.SignalPage{Signals: all, NextSeq: sinceSeq}
	if len(all) > limit {
		page.Signals, page.More = all[:limit], true
	}
	if n := len(page.Signals); n > 0 {
		page.NextSeq = page.Signals[n-1].Seq
	}
	return page, nil
}

// readAll parses the file; a missing file is ErrNotFound.
func readAll(path string) ([]api.Signal, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("signals: %w", err)
	}
	defer f.Close()
	var out []api.Signal
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBuffer), maxLineBytes)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec api.Signal
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%w %d: %v", ErrCorrupt, n, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("signals: %w", err)
	}
	return out, nil
}

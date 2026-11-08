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
	// scanBuffer and maxLineBytes size the reader; a line holds at most one
	// bounded message and one bounded data value.
	scanBuffer   = 16 * 1024
	maxLineBytes = 1024 * 1024
)

// Observer is told about every signal after it is durable (crux serve
// publishes api.EventRunSignal from it).
type Observer func(run string, sig api.RunSignal)

// Book appends signals to the logs of every run one process serves. It is
// the single writer of each log: seq is assigned under its lock, resumed
// from the file the first time a run is touched, so a restarted server
// never reuses a number, and a run whose done signal is recorded refuses
// anything after it. It is safe for concurrent use.
type Book struct {
	pol Policy

	mu sync.Mutex
	// open holds the next seq of every run touched since the last done;
	// a run is dropped once done is written (the file says it is closed).
	open     map[string]int64
	observer Observer
}

// NewBook returns a Book with pol.
func NewBook(pol Policy) *Book {
	return &Book{pol: pol.defaults(), open: map[string]int64{}}
}

// SetObserver installs fn as the signal observer; nil removes it.
func (b *Book) SetObserver(fn Observer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observer = fn
}

// Append validates in, writes it as the next line of runDir's log and
// returns it as recorded. run names the run to the observer.
func (b *Book) Append(run, runDir string, in Signal) (api.RunSignal, error) {
	msg, data, err := normalize(in, b.pol.Scrub)
	if err != nil {
		return api.RunSignal{}, err
	}
	b.mu.Lock()
	path := filepath.Join(runDir, FileName)
	next, ok := b.open[path]
	if !ok {
		if next, err = resume(path); err != nil {
			b.mu.Unlock()
			return api.RunSignal{}, err
		}
	}
	sig := api.RunSignal{Seq: next, Kind: in.Kind, Source: in.Source, At: b.pol.Now().UTC(), Message: msg, Data: data}
	if err := appendLine(path, sig); err != nil {
		b.mu.Unlock()
		return api.RunSignal{}, err
	}
	if sig.Kind == api.SignalDone {
		delete(b.open, path)
	} else {
		b.open[path] = next + 1
	}
	observer := b.observer
	b.mu.Unlock()
	if observer != nil {
		observer(run, sig)
	}
	return sig, nil
}

// resume reads an existing log for the seq after its last line; a log that
// ends with done is ErrClosed.
func resume(path string) (int64, error) {
	lines, err := readLines(path)
	switch {
	case errors.Is(err, ErrNotFound):
		return 1, nil
	case err != nil:
		return 0, err
	}
	var next int64 = 1
	for _, sig := range lines {
		if sig.Kind == api.SignalDone {
			return 0, ErrClosed
		}
		next = max(next, sig.Seq+1)
	}
	return next, nil
}

func appendLine(path string, sig api.RunSignal) error {
	data, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	return nil
}

// Read returns runDir's signals after sinceSeq (exclusive), oldest first,
// at most limit of them (DefaultPageSize when zero, MaxPageSize at most).
// A run with no log is ErrNotFound.
func Read(runDir string, sinceSeq int64, limit int) (api.RunSignalPage, error) {
	lines, err := readLines(filepath.Join(runDir, FileName))
	if err != nil {
		return api.RunSignalPage{}, err
	}
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	page := api.RunSignalPage{Signals: []api.RunSignal{}, NextSeq: sinceSeq}
	for _, sig := range lines {
		if sig.Seq <= sinceSeq {
			continue
		}
		if len(page.Signals) == limit {
			page.More = true
			break
		}
		page.Signals = append(page.Signals, sig)
		page.NextSeq = sig.Seq
	}
	return page, nil
}

// readLines parses the log; a missing file is ErrNotFound.
func readLines(path string) ([]api.RunSignal, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("signals: %w", err)
	}
	defer f.Close()
	var out []api.RunSignal
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBuffer), maxLineBytes)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var sig api.RunSignal
		if err := json.Unmarshal(raw, &sig); err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrCorrupt, n, err)
		}
		out = append(out, sig)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("signals: %w", err)
	}
	return out, nil
}

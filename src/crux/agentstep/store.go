package agentstep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// Storage under the run's directory:
//
//	steps/<n>/record.json       the step's saved record (api.Step)
//	steps/<n>/transcript.jsonl  one line per attempt: command, exit,
//	                            scrubbed output, status, violations, usage
//
// Steps are numbered from 1 in the order they were created.
const (
	StepsDir       = "steps"
	RecordName     = "record.json"
	TranscriptName = "transcript.jsonl"
	fileMode       = 0o600
	dirMode        = 0o700
)

// Typed errors the serve routes map onto api.Code* values.
var (
	// ErrNotFound: no such step number for this run.
	ErrNotFound = errors.New("agentstep: step not found")
	// ErrCorrupt: a saved record did not parse.
	ErrCorrupt = errors.New("agentstep: corrupt record")
)

// createMu serialises step numbering across the process: two sessions
// never share a run directory, but one session may receive two posts.
var createMu sync.Mutex

// Store reads and writes the steps of one run directory.
type Store struct{ RunDir string }

// Dir is the directory of step n.
func (s Store) Dir(n int) string { return filepath.Join(s.RunDir, StepsDir, strconv.Itoa(n)) }

// TranscriptRel is the transcript path relative to the run directory, as
// the record reports it.
func (s Store) TranscriptRel(n int) string {
	return filepath.ToSlash(filepath.Join(StepsDir, strconv.Itoa(n), TranscriptName))
}

// Create assigns the next step number, writes the record and returns it.
func (s Store) Create(step api.Step) (api.Step, error) {
	createMu.Lock()
	defer createMu.Unlock()
	steps, err := s.List()
	if err != nil && !errors.Is(err, ErrCorrupt) {
		return api.Step{}, err
	}
	step.N = 1
	if len(steps) > 0 {
		step.N = steps[len(steps)-1].N + 1
	}
	if err := os.MkdirAll(s.Dir(step.N), dirMode); err != nil {
		return api.Step{}, fmt.Errorf("agentstep: %w", err)
	}
	return step, s.Write(step)
}

// Write saves step's record, tmp+rename.
func (s Store) Write(step api.Step) error {
	data, err := json.Marshal(step)
	if err != nil {
		return fmt.Errorf("agentstep: %w", err)
	}
	path := filepath.Join(s.Dir(step.N), RecordName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return fmt.Errorf("agentstep: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("agentstep: %w", err)
	}
	return nil
}

// Get is step n's saved record.
func (s Store) Get(n int) (api.Step, error) {
	if n <= 0 {
		return api.Step{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.Dir(n), RecordName))
	if errors.Is(err, os.ErrNotExist) {
		return api.Step{}, ErrNotFound
	}
	if err != nil {
		return api.Step{}, fmt.Errorf("agentstep: %w", err)
	}
	var step api.Step
	if err := json.Unmarshal(data, &step); err != nil {
		return api.Step{}, fmt.Errorf("%w: step %d: %v", ErrCorrupt, n, err)
	}
	return step, nil
}

// List is every step of the run, by number. A run with no steps yet is an
// empty list. A record that does not parse is reported after the ones
// that do, as ErrCorrupt, so a caller still sees the rest.
func (s Store) List() ([]api.Step, error) {
	entries, err := os.ReadDir(filepath.Join(s.RunDir, StepsDir))
	if errors.Is(err, os.ErrNotExist) {
		return []api.Step{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentstep: %w", err)
	}
	var nums []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil && n > 0 && e.IsDir() {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	steps := make([]api.Step, 0, len(nums))
	var corrupt error
	for _, n := range nums {
		step, err := s.Get(n)
		switch {
		case errors.Is(err, ErrNotFound):
			// A directory without a record: a create that did not finish.
			continue
		case err != nil:
			corrupt = err
			continue
		}
		steps = append(steps, step)
	}
	return steps, corrupt
}

// AppendTranscript adds one attempt line to step n's transcript.
func (s Store) AppendTranscript(n int, a attempt) error {
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir(n), TranscriptName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// Reconcile marks every step still recorded as running unfinished with
// reason and returns the steps it changed. It runs when the run that owns
// the steps is itself reconciled at `crux serve` start: the process that
// ran the step is gone, so its outcome is not knowable and is never
// guessed. A corrupt record is skipped, not an error, so the rest settle.
func (s Store) Reconcile(reason string, now time.Time) ([]api.Step, error) {
	steps, err := s.List()
	if err != nil && !errors.Is(err, ErrCorrupt) {
		return nil, err
	}
	var changed []api.Step
	for _, step := range steps {
		if step.Status != api.StepRunning {
			continue
		}
		at := now
		step.Status, step.Reason, step.FinishedAt = api.StepUnfinished, reason, &at
		if err := s.Write(step); err != nil {
			return changed, err
		}
		changed = append(changed, step)
	}
	return changed, nil
}

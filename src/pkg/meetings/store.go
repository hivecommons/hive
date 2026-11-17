package meetings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Store persists meetings under one directory:
//
//	<dir>/<id>/meeting.json      the record, replaced atomically
//	<dir>/<id>/transcript.jsonl  final segments and chat lines, appended
const (
	meetingFile    = "meeting.json"
	transcriptFile = "transcript.jsonl"
	storeDirMode   = 0o700
	storeFileMode  = 0o600
	// maxTranscriptLine bounds one stored transcript line on read.
	maxTranscriptLine = 1 << 20
)

// ErrNotFound is returned for an unknown meeting ID.
var ErrNotFound = errors.New("meetings: meeting not found")

// validID keeps IDs safe to use as a directory name.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store is safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
	// echoes remembers the bot's recent lines (echo.go).
	echoes ownEchoes
}

// NewStore creates dir if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return nil, fmt.Errorf("meetings: creating store dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the store root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id, name string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("meetings: invalid meeting id %q", id)
	}
	return filepath.Join(s.dir, id, name), nil
}

// Save writes m atomically.
func (s *Store) Save(m Meeting) error {
	p, err := s.path(m.ID, meetingFile)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), storeDirMode); err != nil {
		return fmt.Errorf("meetings: creating meeting dir: %w", err)
	}
	return writeAtomic(p, data)
}

// writeAtomic writes data to a temp file beside path and renames it over.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("meetings: temp file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, storeFileMode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("meetings: writing %s: %w", filepath.Base(path), werr)
	}
	return nil
}

// Get reads one meeting.
func (s *Store) Get(id string) (Meeting, error) {
	p, err := s.path(id, meetingFile)
	if err != nil {
		return Meeting{}, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Meeting{}, ErrNotFound
	}
	if err != nil {
		return Meeting{}, err
	}
	var m Meeting
	if err := json.Unmarshal(data, &m); err != nil {
		return Meeting{}, fmt.Errorf("meetings: decoding %s: %w", id, err)
	}
	return m, nil
}

// List returns every readable meeting, newest first. Unreadable records
// are skipped.
func (s *Store) List() ([]Meeting, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Meeting
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := s.Get(e.Name())
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// AppendTranscript appends one event (a final segment or a chat line) to
// the meeting's transcript.
func (s *Store) AppendTranscript(id string, ev Event) error {
	p, err := s.path(id, transcriptFile)
	if err != nil {
		return err
	}
	line := ev.Raw
	if len(line) == 0 {
		if line, err = json.Marshal(ev); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, storeFileMode)
	if err != nil {
		return fmt.Errorf("meetings: opening transcript: %w", err)
	}
	_, err = f.Write(append(append([]byte(nil), line...), '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Transcript returns the stored transcript events in order. A meeting with
// no transcript yet returns none.
func (s *Store) Transcript(id string) ([]Event, error) {
	p, err := s.path(id, transcriptFile)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxTranscriptLine)
	for sc.Scan() {
		ev, err := parseEvent(sc.Bytes())
		if err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// PurgeTranscript removes the stored transcript. Removing a missing one is
// not an error.
func (s *Store) PurgeTranscript(id string) error {
	p, err := s.path(id, transcriptFile)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Remove deletes a meeting's directory: the record, its transcript and its
// side documents. Removing a missing meeting is not an error.
func (s *Store) Remove(id string) error {
	p, err := s.path(id, meetingFile)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(filepath.Dir(p)); err != nil {
		return fmt.Errorf("meetings: removing %s: %w", id, err)
	}
	return nil
}

// validDocName keeps side document names to a plain file name.
var validDocName = regexp.MustCompile(`^[a-z0-9_-]{1,32}\.json$`)

// SaveDoc writes a side document (session state, notes) for a meeting
// atomically as JSON.
func (s *Store) SaveDoc(id, name string, v any) error {
	if !validDocName.MatchString(name) {
		return fmt.Errorf("meetings: invalid document name %q", name)
	}
	p, err := s.path(id, name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), storeDirMode); err != nil {
		return fmt.Errorf("meetings: creating meeting dir: %w", err)
	}
	return writeAtomic(p, data)
}

// LoadDoc reads a side document into v. A missing document returns
// ErrNotFound.
func (s *Store) LoadDoc(id, name string, v any) error {
	if !validDocName.MatchString(name) {
		return fmt.Errorf("meetings: invalid document name %q", name)
	}
	p, err := s.path(id, name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// RemoveDoc deletes a side document. Removing a missing one is not an
// error.
func (s *Store) RemoveDoc(id, name string) error {
	if !validDocName.MatchString(name) {
		return fmt.Errorf("meetings: invalid document name %q", name)
	}
	p, err := s.path(id, name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

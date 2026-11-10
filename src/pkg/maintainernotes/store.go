// Package maintainernotes is the private, per-item scratchpad maintainers
// share through the dashboard's Activity monitor. A note is a short piece of
// context one maintainer leaves on an issue or pull request for the others
// — "waiting on legal", "owner wants this after the v6 cut" — that never
// goes to GitHub. The store is an append-only JSONL file on the spoke's
// durable data dir, next to the review decisions it complements; a deletion
// is a tombstone line, so the history is always recoverable.
//
// The scheduler reads the live notes for the items a kick lists and types
// them into the kick under "# Maintainer notes" (KickSection), capped, so an
// agent working the item sees what its maintainers already know.
package maintainernotes

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxTextLen bounds a note's text in runes.
	MaxTextLen = 2000
	// KickSectionCap bounds the bytes of notes a kick carries per item.
	KickSectionCap = 1024
	// KickSectionHeader opens the kick block.
	KickSectionHeader = "# Maintainer notes"
	// DefaultDataDir is the spoke's durable data dir, where the review
	// decisions already live (pkg/review.DefaultDispatchStateDir).
	DefaultDataDir = "/data"
	// FileName is the JSONL file under DefaultDataDir.
	FileName = "maintainer-notes.jsonl"

	idBytes = 12
	// fileMode is owner-only: the notes are maintainer-private, and every
	// agent on the spoke runs as its own UID in the hive's primary group,
	// so group- or world-read would hand each agent every note on every
	// item (the kick section is the deliberate, capped, per-item exposure).
	fileMode       = 0o600
	dirMode        = 0o755
	maxListAll     = 5000
	scanBufInitial = 64 << 10
	scanBufMax     = 1 << 20
	elidedTrail    = "…"
	kickDateLayout = "2006-01-02"
)

// DefaultPath is where Default persists. A var so tests can point it elsewhere.
var DefaultPath = filepath.Join(DefaultDataDir, FileName)

var (
	// ErrEmpty is returned for a note with no text once trimmed.
	ErrEmpty = errors.New("maintainer note: text is empty")
	// ErrTooLong is returned for a note over MaxTextLen runes.
	ErrTooLong = fmt.Errorf("maintainer note: text exceeds %d characters", MaxTextLen)
	// ErrBadItem is returned for a repo not of the form owner/repo or a
	// non-positive number.
	ErrBadItem = errors.New("maintainer note: repo must be owner/repo and number positive")
	// ErrNoAuthor is returned when the author login is empty.
	ErrNoAuthor = errors.New("maintainer note: author is required")
	// ErrNotFound is returned when no live note has the id.
	ErrNotFound = errors.New("maintainer note: not found")
	// ErrForbidden is returned when the actor may not delete the note.
	ErrForbidden = errors.New("maintainer note: only its author or an owner may delete a note")
)

// Note is one maintainer note on an issue or pull request.
type Note struct {
	ID     string `json:"id"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Author is the GitHub login of the dashboard session that wrote it.
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	// DeletedAt and DeletedBy are set on the tombstone line only; List
	// never returns a deleted note.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string     `json:"deleted_by,omitempty"`
}

// Store is the file-backed note store. A zero path keeps it in memory only.
// It is safe for concurrent use.
type Store struct {
	path string

	mu     sync.Mutex
	loaded bool
	notes  []Note         // live notes in creation order
	byID   map[string]int // id -> index into notes
	idGen  func() string
	now    func() time.Time
}

// New returns a store persisted at path ("" for memory only).
func New(path string) *Store {
	return &Store{path: path, idGen: newID, now: time.Now}
}

var (
	defaultOnce  sync.Once
	defaultStore *Store
)

// Default is the process-wide store at DefaultPath, shared by the dashboard
// API and the scheduler's kick seam.
func Default() *Store {
	defaultOnce.Do(func() { defaultStore = New(DefaultPath) })
	return defaultStore
}

func newID() string {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// NormalizeRepo lower-cases an owner/repo reference and reports whether it
// has the right shape.
func NormalizeRepo(repo string) (string, bool) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/ \t\n") || strings.ContainsAny(owner, " \t\n") {
		return "", false
	}
	return repo, true
}

// ValidateText trims text and checks its length.
func ValidateText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmpty
	}
	if utf8.RuneCountInString(text) > MaxTextLen {
		return "", ErrTooLong
	}
	return text, nil
}

// Add appends a note by author on repo#number and returns it.
func (s *Store) Add(repo string, number int, author, text string) (Note, error) {
	repo, ok := NormalizeRepo(repo)
	if !ok || number <= 0 {
		return Note{}, ErrBadItem
	}
	author = strings.TrimSpace(author)
	if author == "" {
		return Note{}, ErrNoAuthor
	}
	text, err := ValidateText(text)
	if err != nil {
		return Note{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	n := Note{ID: s.idGen(), Repo: repo, Number: number, Author: author, Text: text, CreatedAt: s.now().UTC()}
	if err := s.appendLine(n); err != nil {
		return Note{}, err
	}
	s.byID[n.ID] = len(s.notes)
	s.notes = append(s.notes, n)
	return n, nil
}

// List returns the live notes on repo#number, oldest first.
func (s *Store) List(repo string, number int) []Note {
	repo, ok := NormalizeRepo(repo)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []Note
	for _, n := range s.notes {
		if n.Repo == repo && n.Number == number {
			out = append(out, n)
		}
	}
	return out
}

// All returns every live note, oldest first, capped so a runaway file
// cannot flood the dashboard.
func (s *Store) All() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := make([]Note, len(s.notes))
	copy(out, s.notes)
	if len(out) > maxListAll {
		out = out[len(out)-maxListAll:]
	}
	return out
}

// Get returns the live note with id.
func (s *Store) Get(id string) (Note, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	i, ok := s.byID[id]
	if !ok {
		return Note{}, false
	}
	return s.notes[i], true
}

// Delete tombstones the note with id. actor must be its author unless
// admin is set. The deleted note is returned so a caller can say which
// item changed.
func (s *Store) Delete(id, actor string, admin bool) (Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	i, ok := s.byID[id]
	if !ok {
		return Note{}, ErrNotFound
	}
	n := s.notes[i]
	if !admin && !strings.EqualFold(strings.TrimSpace(actor), n.Author) {
		return Note{}, ErrForbidden
	}
	at := s.now().UTC()
	tomb := Note{ID: n.ID, Repo: n.Repo, Number: n.Number, Author: n.Author, CreatedAt: n.CreatedAt, DeletedAt: &at, DeletedBy: strings.TrimSpace(actor)}
	if err := s.appendLine(tomb); err != nil {
		return Note{}, err
	}
	s.notes = append(s.notes[:i], s.notes[i+1:]...)
	s.reindex()
	return n, nil
}

func (s *Store) reindex() {
	s.byID = make(map[string]int, len(s.notes))
	for i, n := range s.notes {
		s.byID[n.ID] = i
	}
}

// load replays the JSONL file once: a line with deleted_at removes the
// note it names; anything unparseable is skipped.
func (s *Store) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.byID = make(map[string]int)
	if s.path == "" {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var n Note
		if json.Unmarshal([]byte(line), &n) != nil || n.ID == "" {
			continue
		}
		if n.DeletedAt != nil {
			if i, ok := s.byID[n.ID]; ok {
				s.notes = append(s.notes[:i], s.notes[i+1:]...)
				s.reindex()
			}
			continue
		}
		if _, dup := s.byID[n.ID]; dup {
			continue
		}
		s.byID[n.ID] = len(s.notes)
		s.notes = append(s.notes, n)
	}
	sort.SliceStable(s.notes, func(i, j int) bool { return s.notes[i].CreatedAt.Before(s.notes[j].CreatedAt) })
	s.reindex()
}

func (s *Store) appendLine(n Note) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	// A file an earlier release created wider than fileMode is tightened
	// on its next write, so an existing spoke is repaired without a
	// migration step.
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&^fileMode != 0 {
		if err := f.Chmod(fileMode); err != nil {
			return err
		}
	}
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// KickSection renders notes for a kick under KickSectionHeader, newest
// first, within capBytes (KickSectionCap when <= 0). A note that would
// cross the cap is cut with an ellipsis; later notes are counted in a
// closing "… and N more" line. Empty for no notes.
func KickSection(notes []Note, capBytes int) string {
	if len(notes) == 0 {
		return ""
	}
	if capBytes <= 0 {
		capBytes = KickSectionCap
	}
	ordered := make([]Note, len(notes))
	copy(ordered, notes)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	var b strings.Builder
	b.WriteString(KickSectionHeader + "\n")
	b.WriteString("Private context other maintainers left on this item (not on GitHub; do not quote it in comments):\n")
	used := 0
	shown := 0
	for _, n := range ordered {
		line := fmt.Sprintf("- @%s (%s): %s", n.Author, n.CreatedAt.UTC().Format(kickDateLayout), strings.ReplaceAll(strings.TrimSpace(n.Text), "\n", " "))
		room := capBytes - used
		if room <= len(elidedTrail)+1 {
			break
		}
		if len(line) > room {
			cut := room - len(elidedTrail) - 1
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + elidedTrail
		}
		b.WriteString(line + "\n")
		used += len(line) + 1
		shown++
	}
	if rest := len(ordered) - shown; rest > 0 {
		fmt.Fprintf(&b, "… and %d more note(s) on the dashboard.\n", rest)
	}
	return b.String()
}

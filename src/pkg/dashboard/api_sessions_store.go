package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Sessions metadata store: /data/sessions/sessions.json (0600, atomic
// rename). The tmux processes do not survive a container restart; this file
// does, so ended sessions keep their name, group and resume handle.
const (
	sessionsDirEnv        = "HIVE_SESSIONS_DIR"
	defaultSessionsDir    = "/data/sessions"
	sessionsStoreFileName = "sessions.json"
	sessionsStoreFileMode = 0o600
	sessionsStoreDirMode  = 0o700

	sessionStatusRunning = "running"
	sessionStatusEnded   = "ended"

	// sessionIDBytes random bytes -> 2x hex chars; matches the runtime's id
	// pattern ([a-f0-9]{8,32}).
	sessionIDRandomBytes = 6
	sessionNameMaxRunes  = 64
	sessionGroupMaxRunes = 48
	// sessionEndedRetention is how long ended-session metadata is kept before
	// the sweeper prunes it.
	sessionEndedRetention = 30 * 24 * time.Hour
)

// sessionRecord is the persisted form of one session.
type sessionRecord struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Vendor       string `json:"vendor"`
	Creator      string `json:"creator"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity"`
	Status       string `json:"status"`
	EndedAt      string `json:"ended_at,omitempty"`
	// ResumeCommand is the vendor-native resume command captured when the
	// session ended. It is internal: never sent to the browser.
	ResumeCommand string `json:"resume_command,omitempty"`
}

type sessionStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]*sessionRecord
}

func sessionsDir() string {
	if v := os.Getenv(sessionsDirEnv); v != "" {
		return v
	}
	return defaultSessionsDir
}

// openSessionStore loads the store at dir/sessions.json; a missing file is an
// empty store.
func openSessionStore(dir string) (*sessionStore, error) {
	st := &sessionStore{path: filepath.Join(dir, sessionsStoreFileName), sessions: map[string]*sessionRecord{}}
	data, err := os.ReadFile(st.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("reading sessions store: %w", err)
	}
	var recs []*sessionRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return st, fmt.Errorf("parsing sessions store: %w", err)
	}
	for _, rec := range recs {
		if rec != nil && rec.ID != "" {
			st.sessions[rec.ID] = rec
		}
	}
	return st, nil
}

// saveLocked writes the store atomically. Callers hold st.mu.
func (st *sessionStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(st.path), sessionsStoreDirMode); err != nil {
		return err
	}
	recs := st.listLocked()
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".sessions-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(sessionsStoreFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), st.path)
}

// listLocked returns copies sorted by creation time then id.
func (st *sessionStore) listLocked() []sessionRecord {
	out := make([]sessionRecord, 0, len(st.sessions))
	for _, rec := range st.sessions {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (st *sessionStore) list() []sessionRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.listLocked()
}

func (st *sessionStore) get(id string) (sessionRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, ok := st.sessions[id]
	if !ok {
		return sessionRecord{}, false
	}
	return *rec, true
}

// put inserts or replaces a record and persists it.
func (st *sessionStore) put(rec sessionRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[rec.ID] = &rec
	return st.saveLocked()
}

// update applies fn to the record under the lock and persists the result.
func (st *sessionStore) update(id string, fn func(*sessionRecord)) (sessionRecord, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, ok := st.sessions[id]
	if !ok {
		return sessionRecord{}, false, nil
	}
	fn(rec)
	return *rec, true, st.saveLocked()
}

func (st *sessionStore) remove(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
	return st.saveLocked()
}

// newSessionRecordID returns a fresh lower-case hex id not present in the store.
func (st *sessionStore) newID() (string, error) {
	for i := 0; i < 8; i++ {
		b := make([]byte, sessionIDRandomBytes)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)
		st.mu.Lock()
		_, taken := st.sessions[id]
		st.mu.Unlock()
		if !taken {
			return id, nil
		}
	}
	return "", errors.New("could not allocate a session id")
}

// validateSessionLabel checks a session name or group: letters, digits, space
// and . _ - ( ) & : + / only, no leading or trailing space, bounded length.
func validateSessionLabel(field, v string, maxRunes int, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxRunes {
		return fmt.Errorf("%s must be at most %d characters", field, maxRunes)
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' {
		return fmt.Errorf("%s must not start or end with a space", field)
	}
	for _, r := range v {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
		case r == ' ', r == '.', r == '_', r == '-', r == '(', r == ')', r == '&', r == ':', r == '+', r == '/':
		default:
			return fmt.Errorf("%s contains a character that is not allowed", field)
		}
	}
	return nil
}

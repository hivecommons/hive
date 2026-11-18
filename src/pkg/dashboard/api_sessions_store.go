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

	// Session status values. A session is `preparing` from the moment its
	// record is created until the background start (workspace clone and
	// hand-over, spek init, tmux) finishes; it then becomes `running` or, when
	// that start fails, `failed` with the error on the record. `ended` is a
	// session whose CLI exited or was closed.
	sessionStatusPreparing = "preparing"
	sessionStatusRunning   = "running"
	sessionStatusEnded     = "ended"
	sessionStatusFailed    = "failed"
	// sessionStatusDeleting is a session whose files are being removed by a
	// background job (api_sessions_delete.go). The record goes when the files
	// are confirmed gone; a job that fails puts the previous status back.
	sessionStatusDeleting = "deleting"

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
	// UIDSlot is the 1-based slot in the reserved per-session uid range the
	// session runs under. 0 marks a record from before per-session uids.
	UIDSlot int `json:"uid_slot,omitempty"`
	// Shared opens the session to every read-write user (and the scrollback to
	// every role). The default, false, is creator + owners only.
	Shared bool `json:"shared"`
	// WorkspaceRepo ("org/repo") and Workspace (its path) name the session's
	// own worktree of the hive's shared clone it opened in (records from before
	// per-session worktrees name the shared clone itself until resumed). Empty
	// for a session that opened in its own empty work dir (no workspace repo
	// configured, or a record from before workspaces); a resume reopens the
	// same repo.
	WorkspaceRepo string `json:"workspace_repo,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	// Message says what a `preparing` session is waiting on ("preparing
	// workspace: cloning org/repo"); Error is why a `failed` session (or a
	// resume that fell back to `ended`) did not start. Both are shown as-is in
	// the dashboard, redacted of tokens before they are stored.
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	// KeyboardHolder is the one user whose terminal attach is writable (the
	// keyboard lease, api_sessions_keyboard.go); everyone else attaches
	// read-only. The creator holds it from the start; "" means released.
	// KeyboardSince is when the current holder took it (RFC3339).
	KeyboardHolder string `json:"keyboard_holder,omitempty"`
	KeyboardSince  string `json:"keyboard_since,omitempty"`
	// DeleteFrom is the status a `deleting` session had before its delete
	// started (`ended` or `failed`), put back when the delete fails.
	// DeleteError is why the last delete failed, shown as-is in the dashboard
	// and cleared by the next attempt.
	DeleteFrom  string `json:"delete_from,omitempty"`
	DeleteError string `json:"delete_error,omitempty"`
}

type sessionStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]*sessionRecord
	// loadErr is why the store could not be loaded at open (it then starts
	// empty); nil for a clean load. Set once, before the store is shared.
	loadErr error
	// deleting holds the ids whose delete job is running in this process, so
	// one session never has two (api_sessions_delete.go). Not persisted: a
	// `deleting` record without an entry here is a job a restart interrupted.
	deleting map[string]bool
}

// claimDelete marks id as having a delete job; false when it already has one.
func (st *sessionStore) claimDelete(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.deleting[id] {
		return false
	}
	if st.deleting == nil {
		st.deleting = map[string]bool{}
	}
	st.deleting[id] = true
	return true
}

// releaseDelete ends id's delete job.
func (st *sessionStore) releaseDelete(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.deleting, id)
}

// deleteInFlight reports whether id has a delete job in this process.
func (st *sessionStore) deleteInFlight(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.deleting[id]
}

// sessionsStoreCorruptSuffix (plus a unix time) names the copy an unreadable
// store is moved aside to.
const sessionsStoreCorruptSuffix = ".corrupt-"

// untrustedForSweep says why the store cannot be trusted to name every live
// session ("" when it can): it failed to load in this process, or an earlier
// process moved an unreadable store aside, which is still there for an
// operator to recover. A sweep that removes what the store does not know
// would otherwise remove the worktrees of sessions that still exist.
func (st *sessionStore) untrustedForSweep() string {
	if st.loadErr != nil {
		return st.loadErr.Error()
	}
	if aside, _ := filepath.Glob(st.path + sessionsStoreCorruptSuffix + "*"); len(aside) > 0 {
		return "an unreadable sessions store was moved aside to " + aside[0] + "; remove it once its sessions are recovered"
	}
	return ""
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

// freeSlot returns the smallest uid slot (1-based) no record holds. Ended
// sessions keep theirs until their files are removed. Callers hold the
// dashboard's sessionsMu so two creations cannot take the same slot.
func (st *sessionStore) freeSlot() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	used := make(map[int]bool, len(st.sessions))
	for _, rec := range st.sessions {
		used[rec.UIDSlot] = true
	}
	slot := 1
	for used[slot] {
		slot++
	}
	return slot
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

package linearagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Install is one workspace's agent installation: the OAuth grant plus the
// identity Linear assigned the app in that workspace.
//
// The scope decision from the RFC applies: one hive = one Linear agent, so the
// store holds a single Install and a reconnect replaces it. viewer.id is
// per-workspace ("your app will have a unique ID for each workspace"), which
// is why it is captured at install time and stored beside the token — it is
// both the assignment filter for enumeration (worksource) and the identity the
// dashboard shows.
type Install struct {
	// ViewerID is the app user's id in the installed workspace, from
	// `query { viewer { id } }` run with the install's access token.
	ViewerID string `json:"viewer_id"`

	// OrganizationID / OrganizationName / OrganizationURLKey identify the
	// workspace, for the dashboard and for matching webhook organizationId.
	OrganizationID     string `json:"organization_id,omitempty"`
	OrganizationName   string `json:"organization_name,omitempty"`
	OrganizationURLKey string `json:"organization_url_key,omitempty"`

	// Token is the OAuth grant. Refreshes rewrite it in place.
	Token Token `json:"token"`

	// ConnectedAt is when the install (or the latest reconnect) completed.
	ConnectedAt time.Time `json:"connected_at"`
}

const (
	// StoreEnvVar overrides where the install record is persisted. Tests and
	// non-container runs use it; production leaves it unset.
	StoreEnvVar = "LINEAR_AGENT_STORE"

	// defaultStorePath sits on the /data persistent volume beside the other
	// durable state, so the install survives restarts and image upgrades.
	defaultStorePath = "/data/linear-agent.json"

	// storeFileMode is owner-only: the file holds a live access token.
	storeFileMode = os.FileMode(0o600)
	storeDirMode  = os.FileMode(0o700)
)

// DefaultStorePath resolves the install store path, honoring the env override.
func DefaultStorePath() string {
	if p := os.Getenv(StoreEnvVar); p != "" {
		return p
	}
	return defaultStorePath
}

// Store persists the Install as owner-only JSON with the tree's usual
// atomic-rename write (write tmp, rename over), so a crash mid-save leaves the
// previous install intact rather than a truncated file.
type Store struct {
	mu      sync.Mutex
	path    string
	install *Install
}

// NewStore opens (or lazily creates) the store at path. A missing file is an
// empty store, not an error; an unreadable or corrupt file is an error, so a
// damaged token file surfaces instead of silently reading as "not installed".
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var inst Install
	if err := json.Unmarshal(raw, &inst); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if inst.ViewerID != "" || inst.Token.AccessToken != "" {
		s.install = &inst
	}
	return s, nil
}

// Get returns a copy of the current install, and whether one exists.
func (s *Store) Get() (Install, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.install == nil {
		return Install{}, false
	}
	return *s.install, true
}

// Set replaces the install and persists it.
func (s *Store) Set(inst Install) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.install = &inst
	return s.saveLocked()
}

// UpdateToken rewrites just the token on the current install (the refresh
// path). It is an error to refresh a store with no install.
func (s *Store) UpdateToken(tok Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.install == nil {
		return fmt.Errorf("no linear install to update")
	}
	s.install.Token = tok
	return s.saveLocked()
}

// Clear removes the install and its file (disconnect).
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.install = nil
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.install, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, storeFileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename %s: %w", s.path, err)
	}
	return nil
}

// StoredViewerID reads the app user id from the install store at path without
// holding a Store open. It is the seam pkg/worksource's factory uses for
// assignment-filtered enumeration: worksource must not construct the whole
// agent service just to learn one id. Returns "" when there is no install or
// the file cannot be read — the CALLER decides whether that is an error
// (assigned_only is opt-in and fails closed at the factory).
func StoredViewerID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var inst Install
	if err := json.Unmarshal(raw, &inst); err != nil {
		return ""
	}
	return inst.ViewerID
}

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The hive owner's GitHub identity on the spoke (hivecommons/hive#18197).
//
// A write-tier reference repo is by definition outside the hive's org, so the
// hive's own App installation token is never honoured there. The credential
// for those writes is the hive OWNER's own user token, obtained once through
// a device flow on the spoke dashboard (Settings → Reference repos → Connect
// GitHub account) and kept in a 0600 file next to the spoke's other secrets.
// The value never leaves the spoke and its path is never written to hive.yaml.
//
// This is a separate consent from dashboard login: login stays scope-free
// (#1927, deviceflow.go); only this flow asks for OwnerIdentityScope.

const (
	// OwnerIdentityScope is the OAuth scope the owner-identity device flow
	// requests: enough to open issues and PRs on public repositories.
	OwnerIdentityScope = "public_repo"
	// DefaultOwnerIdentityFile is where the owner identity is stored.
	DefaultOwnerIdentityFile = "/data/secrets/github-owner-token"
	// OwnerIdentityFileEnv overrides DefaultOwnerIdentityFile.
	OwnerIdentityFileEnv = "HIVE_GITHUB_OWNER_TOKEN_FILE"
	// OwnerIdentityRevalidateInterval is how often, at most, the stored token
	// is re-checked against GET /user.
	OwnerIdentityRevalidateInterval = 10 * time.Minute

	ownerIdentityFileMode = 0o600
	ownerIdentityDirMode  = 0o700
)

// ErrOwnerIdentityInvalid is returned by OwnerIdentityStore.Login when an
// identity is connected but GitHub rejected its token (401) on the last
// validation: the owner must reconnect.
var ErrOwnerIdentityInvalid = errors.New("the hive owner's GitHub token was rejected")

// ErrOwnerTokenUnauthorized is what a validator returns (or wraps) to say
// GitHub answered 401 for the token. A go-github 401 is recognised too.
var ErrOwnerTokenUnauthorized = errors.New("github token unauthorized")

// OwnerIdentity is the stored owner credential.
type OwnerIdentity struct {
	Login       string    `json:"login"`
	Token       string    `json:"token"`
	Scope       string    `json:"scope"`
	ConnectedAt time.Time `json:"connected_at"`
}

// OwnerIdentityStatus is what the dashboard and the status payload may show:
// everything but the token.
type OwnerIdentityStatus struct {
	Connected   bool      `json:"connected"`
	Login       string    `json:"login,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	// Invalid is true when GitHub rejected the stored token; Settings asks
	// the owner to reconnect.
	Invalid bool `json:"invalid,omitempty"`
}

// OwnerIdentityFile returns the identity file path: OwnerIdentityFileEnv when
// set, else DefaultOwnerIdentityFile.
func OwnerIdentityFile() string {
	if p := strings.TrimSpace(os.Getenv(OwnerIdentityFileEnv)); p != "" {
		return p
	}
	return DefaultOwnerIdentityFile
}

// OwnerIdentityScopeGranted reports whether GitHub's granted scope list
// (comma-separated) covers OwnerIdentityScope; "repo" is a superset.
func OwnerIdentityScopeGranted(scope string) bool {
	for _, s := range strings.Split(scope, ",") {
		switch strings.TrimSpace(s) {
		case OwnerIdentityScope, "repo":
			return true
		}
	}
	return false
}

// IsTokenUnauthorized reports whether err says GitHub answered 401.
func IsTokenUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrOwnerTokenUnauthorized) {
		return true
	}
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusUnauthorized
}

// OwnerIdentityStore reads and writes the owner identity file and tracks
// whether its token still validates. The file is re-read on every call so a
// connect or disconnect on the dashboard is seen by the proxy at once. All
// methods are safe on a nil store, which behaves as "not connected".
type OwnerIdentityStore struct {
	path     string
	validate func(ctx context.Context, token string) (string, error)
	interval time.Duration
	now      func() time.Time

	mu sync.Mutex
	// checkedToken is the token the last validation ran against; checkedAt
	// when; invalid whether GitHub answered 401. A different token in the
	// file (reconnect) is unvalidated, not invalid.
	checkedToken string
	checkedAt    time.Time
	invalid      bool
}

// NewOwnerIdentityStore returns a store over path. validate resolves the
// login of a token (GET /user); nil disables Revalidate.
func NewOwnerIdentityStore(path string, validate func(ctx context.Context, token string) (string, error)) *OwnerIdentityStore {
	return &OwnerIdentityStore{path: path, validate: validate, interval: OwnerIdentityRevalidateInterval, now: time.Now}
}

// Path is the identity file path.
func (s *OwnerIdentityStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load reads the identity; (nil, nil) when none is stored.
func (s *OwnerIdentityStore) Load() (*OwnerIdentity, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id OwnerIdentity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	id.Login = strings.TrimSpace(id.Login)
	id.Token = strings.TrimSpace(id.Token)
	if id.Login == "" || id.Token == "" {
		return nil, nil
	}
	return &id, nil
}

// Save writes the identity atomically with owner-only permissions and clears
// any invalid mark.
func (s *OwnerIdentityStore) Save(id OwnerIdentity) error {
	if s == nil || s.path == "" {
		return errors.New("owner identity store has no path")
	}
	if strings.TrimSpace(id.Login) == "" || strings.TrimSpace(id.Token) == "" {
		return errors.New("owner identity needs a login and a token")
	}
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, ownerIdentityDirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".github-owner-token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(ownerIdentityFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	s.mu.Lock()
	s.checkedToken, s.checkedAt, s.invalid = id.Token, s.now(), false
	s.mu.Unlock()
	return nil
}

// Remove deletes the identity (disconnect). A missing file is not an error.
func (s *OwnerIdentityStore) Remove() error {
	if s == nil || s.path == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	s.checkedToken, s.checkedAt, s.invalid = "", time.Time{}, false
	s.mu.Unlock()
	return nil
}

// invalidFor reports whether tok is the token last rejected by GitHub.
func (s *OwnerIdentityStore) invalidFor(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalid && s.checkedToken == tok
}

// Login returns the connected owner login: ("", nil) when no identity is
// stored, ("", ErrOwnerIdentityInvalid) when its token was rejected.
func (s *OwnerIdentityStore) Login() (string, error) {
	id, err := s.Load()
	if err != nil || id == nil {
		return "", nil
	}
	if s.invalidFor(id.Token) {
		return "", ErrOwnerIdentityInvalid
	}
	return id.Login, nil
}

// Token returns the stored owner token when one is connected and not marked
// invalid.
func (s *OwnerIdentityStore) Token() (string, bool) {
	id, err := s.Load()
	if err != nil || id == nil || s.invalidFor(id.Token) {
		return "", false
	}
	return id.Token, true
}

// Status describes the identity without its token.
func (s *OwnerIdentityStore) Status() OwnerIdentityStatus {
	id, err := s.Load()
	if err != nil || id == nil {
		return OwnerIdentityStatus{}
	}
	return OwnerIdentityStatus{Connected: true, Login: id.Login, Scope: id.Scope, ConnectedAt: id.ConnectedAt, Invalid: s.invalidFor(id.Token)}
}

// Revalidate re-checks the stored token at most once per interval. A 401
// marks it invalid until it is replaced or validates again; any other
// failure (network, 5xx) changes nothing. It reports whether the invalid
// state changed, so the caller knows to re-resolve.
func (s *OwnerIdentityStore) Revalidate(ctx context.Context) bool {
	if s == nil || s.validate == nil {
		return false
	}
	id, err := s.Load()
	if err != nil || id == nil {
		return false
	}
	s.mu.Lock()
	due := s.checkedToken != id.Token || s.now().Sub(s.checkedAt) >= s.interval
	s.mu.Unlock()
	if !due {
		return false
	}
	_, verr := s.validate(ctx, id.Token)
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.invalid && s.checkedToken == id.Token
	switch {
	case verr == nil:
		s.checkedToken, s.checkedAt, s.invalid = id.Token, s.now(), false
	case IsTokenUnauthorized(verr):
		s.checkedToken, s.checkedAt, s.invalid = id.Token, s.now(), true
	default:
		return false
	}
	return before != s.invalid
}

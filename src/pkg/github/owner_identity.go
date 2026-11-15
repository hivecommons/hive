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

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/keystore"
)

// The hive owner's GitHub identity (hivecommons/hive#18197).
//
// A repository the hive does not manage (project.reference_repos at the write
// tier, derived from an accepted request or hand-written) is written to as
// the hive OWNER, not as the App: the operator does not want the App installed
// on any remote org, and a person opening an issue or a PR in someone else's
// repo is exactly what GitHub lets any account do. The owner consents once on
// the spoke dashboard (a device flow with the spoke's own OAuth client asking
// for OwnerIdentityScope), and the resulting user token lives on the spoke
// only, in the same place the spoke keeps every other secret value the
// dashboard collects: a 0600 file under /data/secrets, never in hive.yaml,
// never sent to the hub. Agents never see it; the proxy injects it only on
// the writes the reference-repo gate allows.

// OwnerIdentityScope is the OAuth scope the consent asks for: enough to open
// issues and PRs and push to the owner's own forks of PUBLIC repositories.
// Private remote repositories would need the wider "repo" scope and are out
// of scope for now.
const OwnerIdentityScope = "public_repo"

// OwnerIdentityFileEnv overrides where the owner identity file lives.
const OwnerIdentityFileEnv = "HIVE_GITHUB_OWNER_TOKEN_FILE"

// DefaultOwnerIdentityFile is the identity file's default path, next to the
// other secret values the dashboard stores.
const DefaultOwnerIdentityFile = config.WritableSecretsDir + "/github-owner-token"

const (
	ownerIdentityFileMode = 0o600
	ownerIdentityDirMode  = 0o700
)

// ownerIdentityVerifyTTL bounds how often a stored token is re-checked
// against GET /user by the reference-repo pass.
const ownerIdentityVerifyTTL = 10 * time.Minute

// Reasons an owner identity is not usable, in words the Settings page, the
// kick and the request-issue notice can show.
const (
	// OwnerIdentityNotConnected: no consent yet.
	OwnerIdentityNotConnected = "the hive owner has not connected a GitHub account"
	// OwnerIdentityRevoked: the stored token no longer authenticates.
	OwnerIdentityRevoked = "the hive owner's GitHub token is no longer valid; reconnect the account in Settings"
	// OwnerIdentityLoginMismatch: the token now resolves to a different login
	// than the one recorded at consent.
	OwnerIdentityLoginMismatch = "the hive owner's GitHub token resolves to a different account than the one connected; reconnect the account in Settings"
)

// ErrOwnerIdentityNotConnected is returned by Verify when there is no stored
// identity to verify.
var ErrOwnerIdentityNotConnected = errors.New("owner identity not connected")

// OwnerIdentity is the stored consent: who, with what scope, since when, and
// the token itself. The token is only ever read by the proxy's injection path
// and by Verify.
type OwnerIdentity struct {
	Login       string    `json:"login"`
	Token       string    `json:"token"`
	Scope       string    `json:"scope,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
}

// OwnerIdentityStatus is the token-free view of the identity for the
// dashboard and the status payload.
type OwnerIdentityStatus struct {
	// Connected reports whether a consent is stored at all.
	Connected bool `json:"connected"`
	// Login is the connected account ("" when not connected).
	Login string `json:"login,omitempty"`
	// Scope is the OAuth scope GitHub granted at consent.
	Scope       string    `json:"scope,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	// Usable is Connected and not marked invalid by the last verification.
	Usable bool `json:"usable"`
	// Reason says why Usable is false.
	Reason string `json:"reason,omitempty"`
	// VerifiedAt is when the token last passed GET /user (zero: never this
	// process).
	VerifiedAt time.Time `json:"verified_at,omitempty"`
}

// OwnerCredential is what the reference-repo resolution consumes: the login
// the owner token acts as, or why there is none.
type OwnerCredential struct {
	Login string
	// Unavailable is the reason Login is empty.
	Unavailable string
}

// Usable reports whether the credential can back a write-tier entry.
func (o OwnerCredential) Usable() bool { return o.Login != "" }

// OwnerIdentityPath resolves the identity file path from the environment,
// falling back to DefaultOwnerIdentityFile.
func OwnerIdentityPath() string {
	if p := strings.TrimSpace(os.Getenv(OwnerIdentityFileEnv)); p != "" {
		return p
	}
	return DefaultOwnerIdentityFile
}

// OwnerIdentityStore reads and writes the identity file and remembers the
// outcome of the last verification. It is safe for concurrent use.
type OwnerIdentityStore struct {
	path string

	mu            sync.Mutex
	invalidReason string
	verifiedAt    time.Time
	onChange      []func()
}

// NewOwnerIdentityStore returns a store over path ("" means
// OwnerIdentityPath()).
func NewOwnerIdentityStore(path string) *OwnerIdentityStore {
	if strings.TrimSpace(path) == "" {
		path = OwnerIdentityPath()
	}
	return &OwnerIdentityStore{path: path}
}

// Path is the identity file path.
func (s *OwnerIdentityStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// OnChange registers fn to run after Save or Clear, so the reference-repo
// policies can be re-resolved without a restart.
func (s *OwnerIdentityStore) OnChange(fn func()) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.onChange = append(s.onChange, fn)
	s.mu.Unlock()
}

func (s *OwnerIdentityStore) notify() {
	s.mu.Lock()
	fns := append([]func(){}, s.onChange...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Load reads the identity file. ok is false when no identity is stored; err
// reports an unreadable or malformed file.
func (s *OwnerIdentityStore) Load() (id OwnerIdentity, ok bool, err error) {
	if s == nil {
		return OwnerIdentity{}, false, nil
	}
	// Through the keystore (hive#18214): the identity file is sealed at rest
	// once a provider is active; a file written before that is re-sealed on
	// this read.
	b, err := keystore.ReadFile(context.Background(), s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OwnerIdentity{}, false, nil
		}
		return OwnerIdentity{}, false, err
	}
	if err := json.Unmarshal(b, &id); err != nil {
		return OwnerIdentity{}, false, fmt.Errorf("%s: %w", s.path, err)
	}
	id.Login = strings.TrimSpace(id.Login)
	id.Token = strings.TrimSpace(id.Token)
	if id.Login == "" || id.Token == "" {
		return OwnerIdentity{}, false, fmt.Errorf("%s: identity file is missing login or token", s.path)
	}
	return id, true, nil
}

// Save writes the identity atomically, owner-only, and clears any invalid
// mark from a previous token.
func (s *OwnerIdentityStore) Save(id OwnerIdentity) error {
	if s == nil {
		return errors.New("owner identity store is nil")
	}
	id.Login = strings.TrimSpace(id.Login)
	id.Token = strings.TrimSpace(id.Token)
	if id.Login == "" || id.Token == "" {
		return errors.New("owner identity needs a login and a token")
	}
	if id.ConnectedAt.IsZero() {
		id.ConnectedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(s.path), ownerIdentityDirMode); err != nil {
		return err
	}
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	// Sealed and replaced atomically at ownerIdentityFileMode (hive#18214).
	if err := keystore.WriteFileMode(context.Background(), s.path, b, ownerIdentityFileMode); err != nil {
		return err
	}
	s.mu.Lock()
	s.invalidReason = ""
	s.verifiedAt = time.Now()
	s.mu.Unlock()
	s.notify()
	return nil
}

// Clear removes the identity file (a missing file is not an error).
func (s *OwnerIdentityStore) Clear() error {
	if s == nil {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	s.invalidReason = ""
	s.verifiedAt = time.Time{}
	s.mu.Unlock()
	s.notify()
	return nil
}

// Status is the token-free view of the stored identity.
func (s *OwnerIdentityStore) Status() OwnerIdentityStatus {
	if s == nil {
		return OwnerIdentityStatus{Reason: OwnerIdentityNotConnected}
	}
	id, ok, err := s.Load()
	if err != nil {
		return OwnerIdentityStatus{Reason: "owner identity file unreadable: " + err.Error()}
	}
	if !ok {
		return OwnerIdentityStatus{Reason: OwnerIdentityNotConnected}
	}
	s.mu.Lock()
	reason, verifiedAt := s.invalidReason, s.verifiedAt
	s.mu.Unlock()
	st := OwnerIdentityStatus{
		Connected:   true,
		Login:       id.Login,
		Scope:       id.Scope,
		ConnectedAt: id.ConnectedAt,
		Usable:      reason == "",
		Reason:      reason,
		VerifiedAt:  verifiedAt,
	}
	return st
}

// Credential is the login the owner token acts as, for the reference-repo
// resolution; Unavailable names the cure when there is none.
func (s *OwnerIdentityStore) Credential() OwnerCredential {
	st := s.Status()
	if !st.Usable {
		return OwnerCredential{Unavailable: st.Reason}
	}
	return OwnerCredential{Login: st.Login}
}

// Token yields the stored token for the proxy's injection path: ("", false)
// when not connected or marked invalid. It is an OwnerTokenSource.
func (s *OwnerIdentityStore) Token() (string, bool) {
	if s == nil {
		return "", false
	}
	id, ok, err := s.Load()
	if err != nil || !ok {
		return "", false
	}
	s.mu.Lock()
	invalid := s.invalidReason != ""
	s.mu.Unlock()
	if invalid {
		return "", false
	}
	return id.Token, true
}

// MarkInvalid records that the stored token stopped working (a 401 seen by
// the proxy or by Verify) so entries drop to read-only and Settings prompts
// for a reconnect. The file is kept: the owner decides whether to
// reconnect or disconnect.
func (s *OwnerIdentityStore) MarkInvalid(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := s.invalidReason != reason
	s.invalidReason = reason
	s.mu.Unlock()
	if changed {
		s.notify()
	}
}

// Verify re-checks the stored token against GET /user through whoAmI at most
// once per ownerIdentityVerifyTTL (force skips the TTL). An authentication
// failure marks the identity invalid; a login that no longer matches the
// recorded one does too; a transient error (network, 5xx) keeps the previous
// verdict and is returned. ErrOwnerIdentityNotConnected when nothing is
// stored.
func (s *OwnerIdentityStore) Verify(ctx context.Context, whoAmI func(ctx context.Context, token string) (string, error), force bool) error {
	if s == nil || whoAmI == nil {
		return nil
	}
	id, ok, err := s.Load()
	if err != nil {
		return err
	}
	if !ok {
		return ErrOwnerIdentityNotConnected
	}
	s.mu.Lock()
	fresh := !force && !s.verifiedAt.IsZero() && time.Since(s.verifiedAt) < ownerIdentityVerifyTTL
	s.mu.Unlock()
	if fresh {
		return nil
	}
	login, err := whoAmI(ctx, id.Token)
	switch {
	case err != nil && IsAuthError(err):
		s.MarkInvalid(OwnerIdentityRevoked)
		return nil
	case err != nil:
		return err
	case !strings.EqualFold(strings.TrimSpace(login), id.Login):
		s.MarkInvalid(OwnerIdentityLoginMismatch)
		return nil
	}
	s.mu.Lock()
	wasInvalid := s.invalidReason != ""
	s.invalidReason = ""
	s.verifiedAt = time.Now()
	s.mu.Unlock()
	if wasInvalid {
		s.notify()
	}
	return nil
}

// IsAuthError reports whether err is GitHub refusing the credential itself
// (401 Unauthorized or 403 Forbidden on GET /user), as opposed to a transient
// failure.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return ghErr.Response.StatusCode == http.StatusUnauthorized || ghErr.Response.StatusCode == http.StatusForbidden
	}
	msg := err.Error()
	return strings.Contains(msg, "status 401") || strings.Contains(msg, "401 Bad credentials")
}

// ScopeGrantsPublicRepo reports whether a granted OAuth scope list (GitHub's
// comma-separated form) covers OwnerIdentityScope: "public_repo" itself or
// the wider "repo".
func ScopeGrantsPublicRepo(granted string) bool {
	for _, s := range strings.Split(granted, ",") {
		switch strings.TrimSpace(s) {
		case OwnerIdentityScope, "repo":
			return true
		}
	}
	return false
}

package askai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	// ConsentTextVersion is bumped for any material change to ConsentText;
	// a person who confirmed an older version is asked again.
	ConsentTextVersion = "1"
	// ConsentText is the disclosure the panel shows before the first
	// question (design: "What leaves the deployment").
	ConsentText = "Your question, and the section you are in, are sent to Kapa.ai, an AI service. " +
		"The maintainers of this project can read it in their analytics. " +
		"Do not include secrets or private details. " +
		"Withdrawing stops future questions; it cannot recall what Kapa.ai already holds."

	consentDirMode  = 0o700
	consentFileMode = 0o600
	// consentFileSuffix is the per-person file extension.
	consentFileSuffix = ".json"
)

// consentStemPattern bounds a person key to a single safe path segment.
var consentStemPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// ErrInvalidPerson is returned for a person key that is not a safe stem.
var ErrInvalidPerson = errors.New("ask: invalid person key")

// Consent is one person's record.
type Consent struct {
	Person      string     `json:"person"`
	TextVersion string     `json:"text_version"`
	GrantedAt   time.Time  `json:"granted_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
}

// Current reports a live consent to the current disclosure text.
func (c Consent) Current() bool {
	return c.WithdrawnAt == nil && c.TextVersion == ConsentTextVersion && !c.GrantedAt.IsZero()
}

// ConsentStore keeps one small file per person under Dir, separate from any
// other user record, written atomically with owner-only permissions. The
// file name is the same stem the person's user record uses, so a user
// deletion can find it.
type ConsentStore struct {
	Dir string
	mu  sync.Mutex
}

func (s *ConsentStore) path(person string) (string, error) {
	if s == nil || s.Dir == "" || !consentStemPattern.MatchString(person) {
		return "", ErrInvalidPerson
	}
	return filepath.Join(s.Dir, person+consentFileSuffix), nil
}

// Get returns the person's record; a missing file is the zero Consent.
func (s *ConsentStore) Get(person string) (Consent, error) {
	path, err := s.path(person)
	if err != nil {
		return Consent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return readConsent(path)
}

// Has reports whether person holds a current consent. Any error is "no".
func (s *ConsentStore) Has(person string) bool {
	c, err := s.Get(person)
	return err == nil && c.Current()
}

// Grant records consent to version, which must be the current text version.
func (s *ConsentStore) Grant(person, version string, now time.Time) (Consent, error) {
	if version != ConsentTextVersion {
		return Consent{}, ErrStaleConsentVersion
	}
	path, err := s.path(person)
	if err != nil {
		return Consent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readConsent(path)
	if err != nil {
		return Consent{}, err
	}
	now = now.UTC()
	if c.GrantedAt.IsZero() || c.WithdrawnAt != nil || c.TextVersion != version {
		c.GrantedAt = now
	}
	c.Person, c.TextVersion, c.UpdatedAt, c.WithdrawnAt = person, version, now, nil
	return c, writeConsent(path, c)
}

// Withdraw marks the record withdrawn. It takes effect on the next request.
// Withdrawing without a record is a no-op.
func (s *ConsentStore) Withdraw(person string, now time.Time) (bool, error) {
	path, err := s.path(person)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readConsent(path)
	if err != nil {
		return false, err
	}
	if c.GrantedAt.IsZero() || c.WithdrawnAt != nil {
		return false, nil
	}
	now = now.UTC()
	c.UpdatedAt, c.WithdrawnAt = now, &now
	return true, writeConsent(path, c)
}

// Delete removes the person's record (user deletion).
func (s *ConsentStore) Delete(person string) error {
	path, err := s.path(person)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrStaleConsentVersion means the page showed an older disclosure.
var ErrStaleConsentVersion = errors.New("ask: disclosure version changed")

func readConsent(path string) (Consent, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Consent{}, nil
	}
	if err != nil {
		return Consent{}, err
	}
	var c Consent
	if err := json.Unmarshal(data, &c); err != nil {
		return Consent{}, err
	}
	return c, nil
}

func writeConsent(path string, c Consent) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, consentDirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ask-consent-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(consentFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

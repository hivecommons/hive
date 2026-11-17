// Package mailallow is a hive's email allowlist for mail that reaches it
// through hive-mx (hivecommons/hive#18395, plan #18391 "Allowlist"): the
// calendar email addresses each dashboard user registered and proved.
//
// A user registers an address and gets a proof code; they prove the address
// by sending a mail from it to hive@<host> with the code in the subject. The
// proof only counts when the mail passed DMARC alignment for that exact From
// address, so hive never has to send mail to check ownership.
//
// The effective allowlist is every proven address whose user is still on the
// live access list (dashboard.authorized_users, kept current by the hub
// heartbeat). Removing a user's access therefore revokes their addresses at
// once, without touching this store.
package mailallow

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Limits.
const (
	// MaxPerUser bounds the addresses one user may register.
	MaxPerUser = 10
	// CodeTTL is how long a proof code stays valid.
	CodeTTL = 24 * time.Hour
	// maxEmailLen is the longest address accepted (RFC 5321 path limit).
	maxEmailLen = 254
	// maxEntries bounds the whole store.
	maxEntries = 5000
	// maxRecentAttempts bounds the unknown-code proof attempts kept for
	// the owner (in memory; a restart clears them).
	maxRecentAttempts = 20
	// maxFailureDKIM bounds the DKIM results kept on a failure summary.
	maxFailureDKIM = 5
	fileMode       = 0o600
	dirMode        = 0o700
)

// Proof failure reasons recorded on an entry.
const (
	ReasonNotAligned   = "the proof mail did not pass DMARC alignment for this address"
	ReasonFromMismatch = "the proof mail came from a different address"
	ReasonExpired      = "the proof code had expired"
)

// Proof failure reason codes (ProofFailure.Reason), stable for the API and
// the Settings row (hivecommons/hive#18579).
const (
	ReasonCodeNotAligned   = "not_aligned"
	ReasonCodeFromMismatch = "from_mismatch"
	ReasonCodeExpired      = "expired"
)

// AttemptCodeNotRecognized is the reason of an unknown-code proof attempt.
const AttemptCodeNotRecognized = "code not recognized"

// Errors.
var (
	ErrInvalidEmail = errors.New("mailallow: not a valid email address")
	ErrTooMany      = errors.New("mailallow: too many calendar emails for this user")
	ErrNotFound     = errors.New("mailallow: no such calendar email")
	ErrUnknownCode  = errors.New("mailallow: unknown or expired proof code")
	ErrNotProven    = errors.New("mailallow: proof mail was not authenticated for this address")
	ErrFull         = errors.New("mailallow: allowlist store is full")
)

// Entry is one registered address.
type Entry struct {
	// Login is the dashboard user who registered it (X-Hive-User).
	Login string `json:"login"`
	Email string `json:"email"`
	// Code is the pending proof code ("" once proven).
	Code          string     `json:"code,omitempty"`
	CodeExpiresAt time.Time  `json:"code_expires_at,omitzero"`
	CreatedAt     time.Time  `json:"created_at"`
	ProvenAt      *time.Time `json:"proven_at,omitempty"`
	// LastError is why the latest proof mail did not count.
	LastError string `json:"last_error,omitempty"`
	// LastFailure summarizes the latest failed proof attempt: the reason
	// code, when it arrived and the authentication results hive-mx saw.
	LastFailure *ProofFailure `json:"last_failure,omitempty"`
}

// ProofFailure is the authentication summary of one failed proof mail: the
// pass/fail per mechanism and the domains, never message content.
type ProofFailure struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// From is the From header address the receiver parsed, kept only on a
	// from_mismatch so the owner sees which address the proof came from;
	// "" there means the header was missing, repeated or unparsable.
	From        string            `json:"from,omitempty"`
	FromDomain  string            `json:"from_domain,omitempty"`
	SPF         string            `json:"spf,omitempty"`
	SPFDomain   string            `json:"spf_domain,omitempty"`
	SPFAligned  bool              `json:"spf_aligned"`
	DKIM        []wire.DKIMResult `json:"dkim,omitempty"`
	DKIMAligned bool              `json:"dkim_aligned"`
	DMARC       string            `json:"dmarc,omitempty"`
	Aligned     bool              `json:"aligned"`
}

func newProofFailure(reason string, at time.Time, auth wire.Auth) *ProofFailure {
	f := &ProofFailure{
		Reason: reason, At: at, FromDomain: auth.FromDomain,
		SPF: auth.SPF, SPFDomain: auth.SPFDomain, SPFAligned: auth.SPFAligned,
		DKIMAligned: auth.DKIMAligned, DMARC: auth.DMARC, Aligned: auth.Aligned,
	}
	if f.FromDomain == "" {
		if i := strings.LastIndexByte(auth.From, '@'); i >= 0 {
			f.FromDomain = strings.ToLower(auth.From[i+1:])
		}
	}
	dkim := auth.DKIM
	if len(dkim) > maxFailureDKIM {
		dkim = dkim[:maxFailureDKIM]
	}
	f.DKIM = append([]wire.DKIMResult(nil), dkim...)
	return f
}

// ProofAttempt is a proof mail whose code matched no entry, kept for the
// owner so a typo is visible. From is masked.
type ProofAttempt struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	Reason string    `json:"reason"`
}

// MaskEmail keeps an address's first character and its domain
// ("a***@example.com"); "" stays "".
func MaskEmail(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	switch {
	case addr == "":
		return ""
	case at <= 0:
		return "***"
	}
	return addr[:1] + "***" + addr[at:]
}

// Proven reports whether the address was proven.
func (e Entry) Proven() bool { return e.ProvenAt != nil }

// Store persists entries in one JSON file. Safe for concurrent use.
type Store struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	entries []Entry
	// attempts are the recent unknown-code proof attempts, newest last.
	attempts []ProofAttempt
}

// NormalizeEmail returns the lower-cased bare address, or ErrInvalidEmail.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxEmailLen {
		return "", ErrInvalidEmail
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s && "<"+a.Address+">" != s {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndexByte(a.Address, '@')
	if at <= 0 || !strings.Contains(a.Address[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(a.Address), nil
}

// sameUser compares dashboard identities the way the access list does: case
// folded, with the legacy "github:" prefix ignored.
func sameUser(a, b string) bool {
	key := func(s string) string { return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "github:") }
	return key(a) == key(b)
}

// Open loads (or starts) the store at path.
func Open(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, now: now}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("mailallow: read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &s.entries); err != nil {
		return nil, fmt.Errorf("mailallow: parse %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), dirMode); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, fileMode); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List returns login's entries sorted by address. An expired code is shown
// as no code.
func (s *Store) List(login string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Entry
	for _, e := range s.entries {
		if sameUser(e.Login, login) {
			if e.Code != "" && now.After(e.CodeExpiresAt) {
				e.Code, e.CodeExpiresAt = "", time.Time{}
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// All returns every entry, sorted by user then address, with expired codes
// cleared.
func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := append([]Entry(nil), s.entries...)
	for i := range out {
		if out[i].Code != "" && now.After(out[i].CodeExpiresAt) {
			out[i].Code, out[i].CodeExpiresAt = "", time.Time{}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Login != out[j].Login {
			return out[i].Login < out[j].Login
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// Add registers email for login and issues a fresh proof code. Adding an
// address already registered by the same user re-issues its code unless it
// is proven.
func (s *Store) Add(login, email string) (Entry, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	count := 0
	for i := range s.entries {
		e := &s.entries[i]
		if !sameUser(e.Login, login) {
			continue
		}
		count++
		if e.Email == email {
			if !e.Proven() {
				e.Code, e.CodeExpiresAt, e.LastError, e.LastFailure = wire.NewProofCode(), now.Add(CodeTTL), "", nil
				if err := s.saveLocked(); err != nil {
					return Entry{}, err
				}
			}
			return *e, nil
		}
	}
	if count >= MaxPerUser {
		return Entry{}, ErrTooMany
	}
	if len(s.entries) >= maxEntries {
		return Entry{}, ErrFull
	}
	e := Entry{Login: login, Email: email, Code: wire.NewProofCode(), CodeExpiresAt: now.Add(CodeTTL), CreatedAt: now}
	s.entries = append(s.entries, e)
	if err := s.saveLocked(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return Entry{}, err
	}
	return e, nil
}

// Remove deletes login's registration of email.
func (s *Store) Remove(login, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if sameUser(e.Login, login) && e.Email == email {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

// Prove applies a proof mail: code from the subject, from the
// authenticated From address, aligned the DMARC alignment result. See
// ProveAuth.
func (s *Store) Prove(code, from string, aligned bool) (Entry, error) {
	return s.ProveAuth(code, wire.Auth{From: from, Aligned: aligned})
}

// ProveAuth applies a proof mail with its full authentication results. The
// matching entry is proven only when its code is live, the mail was aligned
// and it came from the registered address; otherwise the failure, with the
// SPF, DKIM and alignment summary, is recorded on it. A code that matches
// no entry is recorded as a recent attempt for the owner.
func (s *Store) ProveAuth(code string, auth wire.Auth) (Entry, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	from := strings.ToLower(strings.TrimSpace(auth.From))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for i := range s.entries {
		e := &s.entries[i]
		if code == "" || e.Code != code {
			continue
		}
		var reason string
		switch {
		case now.After(e.CodeExpiresAt):
			e.LastError, reason = ReasonExpired, ReasonCodeExpired
		case from != e.Email:
			e.LastError, reason = ReasonFromMismatch, ReasonCodeFromMismatch
		case !auth.Aligned:
			e.LastError, reason = ReasonNotAligned, ReasonCodeNotAligned
		default:
			t := now
			e.ProvenAt, e.Code, e.CodeExpiresAt, e.LastError, e.LastFailure = &t, "", time.Time{}, "", nil
			if err := s.saveLocked(); err != nil {
				return Entry{}, err
			}
			return *e, nil
		}
		e.LastFailure = newProofFailure(reason, now, auth)
		if reason == ReasonCodeFromMismatch {
			e.LastFailure.From = from
		}
		if err := s.saveLocked(); err != nil {
			return Entry{}, err
		}
		return *e, ErrNotProven
	}
	s.attempts = append(s.attempts, ProofAttempt{At: now, From: MaskEmail(from), Reason: AttemptCodeNotRecognized})
	if over := len(s.attempts) - maxRecentAttempts; over > 0 {
		s.attempts = append([]ProofAttempt(nil), s.attempts[over:]...)
	}
	return Entry{}, ErrUnknownCode
}

// RecentAttempts returns the recent unknown-code proof attempts, newest
// first. Owner-only: the caller decides who sees them.
func (s *Store) RecentAttempts() []ProofAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProofAttempt, 0, len(s.attempts))
	for i := len(s.attempts) - 1; i >= 0; i-- {
		out = append(out, s.attempts[i])
	}
	return out
}

// Allowed reports whether email is on the effective allowlist: proven by a
// user for whom hasAccess (the live access list) is true. It returns that
// user's login.
func (s *Store) Allowed(email string, hasAccess func(login string) bool) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || hasAccess == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Email == email && e.Proven() && hasAccess(e.Login) {
			return e.Login, true
		}
	}
	return "", false
}

// Trusted is the one rule every mail-in path uses to skip approval: the
// sender's From address passed DMARC alignment and is on the effective
// allowlist.
func (s *Store) Trusted(auth wire.Auth, hasAccess func(login string) bool) (string, bool) {
	if s == nil || !auth.Aligned || auth.From == "" {
		return "", false
	}
	return s.Allowed(auth.From, hasAccess)
}

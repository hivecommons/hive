package meetings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Trusted requesters: the platform users whose join requests hive approves
// without the owner. A person is trusted by their stable platform user id
// (a Discord snowflake, which Discord itself supplies for the user who ran
// `/hive join`), never by display name: a name is set by the user and can be
// copied by anyone. The owner adds a person from a pending request in the
// dashboard ("Always approve this person") and can remove them again.

const (
	trustFile = "trusted_requesters.json"
	// MaxTrusted bounds the list so the file stays small.
	MaxTrusted = 20
)

// discordID is a Discord snowflake: decimal digits only.
var discordID = regexp.MustCompile(`^[0-9]{5,25}$`)

// ErrTrustInvalid is returned for a request that cannot be trusted: not a
// Discord request, or no verified user id on it.
var ErrTrustInvalid = errors.New("meetings: this request carries no platform user id to trust")

// ErrTrustLimit is returned when the list is full.
var ErrTrustLimit = errors.New("meetings: too many trusted requesters; remove one first")

// Trusted is one trusted requester. Name is for display only and is never
// compared.
type Trusted struct {
	Platform string    `json:"platform"`
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at"`
}

// TrustStore persists the trusted requesters in one small JSON file.
type TrustStore struct {
	mu   sync.Mutex
	path string
}

// NewTrustStore keeps the list in dir/trusted_requesters.json.
func NewTrustStore(dir string) *TrustStore { return &TrustStore{path: filepath.Join(dir, trustFile)} }

func (t *TrustStore) load() ([]Trusted, error) {
	data, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("meetings: reading trusted requesters: %w", err)
	}
	var list []Trusted
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("meetings: trusted requesters file: %w", err)
	}
	return list, nil
}

func (t *TrustStore) save(list []Trusted) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(t.path, data)
}

// List returns the trusted requesters, oldest first.
func (t *TrustStore) List() ([]Trusted, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list, err := t.load()
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	return list, err
}

// IsTrusted reports whether the platform user id is trusted. An unreadable
// file trusts nobody.
func (t *TrustStore) IsTrusted(platform, id string) bool {
	if t == nil || platform != PlatformDiscord || !discordID.MatchString(id) {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	list, err := t.load()
	if err != nil {
		return false
	}
	for _, e := range list {
		if e.Platform == platform && e.ID == id {
			return true
		}
	}
	return false
}

// Add trusts the person; adding one already trusted only refreshes the name.
func (t *TrustStore) Add(e Trusted) (Trusted, error) {
	if e.Platform != PlatformDiscord || !discordID.MatchString(e.ID) {
		return Trusted{}, ErrTrustInvalid
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	list, err := t.load()
	if err != nil {
		return Trusted{}, err
	}
	for i := range list {
		if list[i].Platform == e.Platform && list[i].ID == e.ID {
			list[i].Name = e.Name
			return list[i], t.save(list)
		}
	}
	if len(list) >= MaxTrusted {
		return Trusted{}, ErrTrustLimit
	}
	return e, t.save(append(list, e))
}

// Remove stops trusting the person and reports whether they were listed.
func (t *TrustStore) Remove(platform, id string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list, err := t.load()
	if err != nil {
		return false, err
	}
	out := list[:0]
	found := false
	for _, e := range list {
		if e.Platform == platform && e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return false, nil
	}
	return true, t.save(out)
}

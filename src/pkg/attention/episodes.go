// Package attention persists producer-independent condition episodes. It is an
// opt-in delivery primitive, not a review authority or a replacement for holds.
package attention

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/findingidentity"
)

const DefaultFlapWindow = 12 * time.Hour
const MaxEpisodes = 4096
const MaxItems = 4096

// Config names exactly one deployment per fully qualified repo. Operators must
// not run two independent deployments with the same hive ID and ledger.
type Config struct {
	Admitter   map[string]string `yaml:"admitter,omitempty" json:"admitter,omitempty"`
	FlapWindow time.Duration     `yaml:"flap_window,omitempty" json:"flap_window,omitempty"`
}

func (c Config) Owner(repo string) string {
	for r, owner := range c.Admitter {
		if canonical(r) == canonical(repo) {
			return strings.TrimSpace(owner)
		}
	}
	return ""
}
func (c Config) Validate() error {
	if c.FlapWindow < 0 {
		return errors.New("github.attention.flap_window must not be negative")
	}
	seen := map[string]bool{}
	for repo, owner := range c.Admitter {
		key := canonical(repo)
		parts := strings.Split(key, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(key, " \t\r\n") {
			return errors.New("github.attention.admitter requires fully qualified owner/repo keys")
		}
		if seen[key] {
			return errors.New("github.attention.admitter has duplicate normalized repo keys")
		}
		seen[key] = true
		if strings.TrimSpace(owner) == "" || strings.ContainsAny(owner, " \t\r\n") {
			return errors.New("github.attention.admitter requires a nonempty hive ID")
		}
	}
	return nil
}
func (c Config) Window() time.Duration {
	if c.FlapWindow > 0 {
		return c.FlapWindow
	}
	return DefaultFlapWindow
}

func canonical(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Key excludes producer, SHA, run IDs and URLs. Subject must be a structured
// identity, not free prose. An action, owner or risk change changes this key.
type Key struct {
	Repo      string `json:"repo"`
	Item      int    `json:"item"`
	Action    string `json:"action"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
}

func (k Key) Hash() string {
	b, _ := json.Marshal([]any{canonical(k.Repo), k.Item, canonical(k.Action), canonical(k.Recipient), k.Subject})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// CISubject accepts a job name and an already classified error (not log text).
func CISubject(job, errorClass string) string {
	b, _ := json.Marshal([]string{"ci", strings.Join(strings.Fields(job), " "), canonical(errorClass)})
	return string(b)
}
func FindingSubject(r findingidentity.Record) string { return findingidentity.Key(r) }
func DecisionSubject(id string) string               { return "decision:" + strings.TrimSpace(id) }

type State string

const (
	Observed     State = "observed"
	Actionable   State = "actionable"
	Notified     State = "notified"
	Acknowledged State = "acknowledged"
	Resolved     State = "resolved"
)

type Episode struct {
	Key          Key       `json:"key"`
	State        State     `json:"state"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	PagedAt      time.Time `json:"paged_at,omitempty"`
	PagedTo      string    `json:"paged_to,omitempty"`
	AckedAt      time.Time `json:"acked_at,omitempty"`
	SnoozedUntil time.Time `json:"snoozed_until,omitempty"`
	ResolvedAt   time.Time `json:"resolved_at,omitempty"`
	Attempt      string    `json:"last_attempt,omitempty"`
	AttemptOwner string    `json:"attempt_owner,omitempty"`
	Evidence     string    `json:"evidence,omitempty"`
	Uncertain    bool      `json:"uncertain,omitempty"`
	WouldNotify  bool      `json:"would_notify,omitempty"`
	// Baseline is fail-quiet recovery: history must be reconciled, not replayed.
	Baseline    bool `json:"baseline,omitempty"`
	QuietReopen bool `json:"quiet_reopen,omitempty"`
}

func (e Episode) Marker() string {
	return fmt.Sprintf("<!-- hive:attention key=%s attempt=%s -->", e.Key.Hash(), e.Attempt)
}

type Observation struct {
	Key        Key
	Actionable bool
	Resolved   bool
	Evidence   string
	Body       string
}

// Delivery is supplied by the existing write boundary. History returns trusted
// hive marker keys (including status-card condition markers), with pagination.
// Find must reconcile this exact attempt, not content or another hive's attempt.
type Delivery interface {
	History(context.Context, string, int) ([]string, error)
	Find(context.Context, Episode) (bool, error)
	Post(context.Context, Episode, string) error
}

type disk struct {
	Version  int                 `json:"version"`
	Items    map[string]bool     `json:"items"`
	Episodes map[string]*Episode `json:"episodes"`
}
type Store struct {
	mu    sync.Mutex
	path  string
	state disk
	now   func() time.Time
}

// Open treats missing/corrupt state as recovery, never as permission to replay.
// Unknown versions are refused rather than clobbered during rolling upgrades.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now, state: disk{Version: 1, Items: map[string]bool{}, Episodes: map[string]*Episode{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d disk
	if json.Unmarshal(b, &d) != nil {
		return s, nil
	}
	if d.Version == 0 {
		return s, nil
	}
	if d.Version != 1 {
		return nil, fmt.Errorf("attention: unsupported ledger version %d", d.Version)
	}
	if d.Items == nil || d.Episodes == nil {
		return s, nil
	}
	if len(d.Items) > MaxItems || len(d.Episodes) > MaxEpisodes {
		return nil, errors.New("attention: ledger exceeds capacity")
	}
	for _, e := range d.Episodes {
		if e == nil {
			return s, nil
		}
	}
	s.state = d
	return s, nil
}

// save uses fsync + rename, and errors fence every outbound POST. Memory may
// lead disk after a failed save, but no subsequent POST can bypass another save.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".attention-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Report accepts the full current condition snapshot for one item. The first
// snapshot after state loss seeds WITHOUT sending. A successful empty snapshot
// establishes a baseline so genuinely new risks can notify on the next report.
// Calls are serialized through the POST outcome, including concurrent producers.
// Non-admitters only record would-notify. No marker lookup grants ownership.
func (s *Store) Report(ctx context.Context, cfg Config, hiveID, repo string, item int, observations []Observation, d Delivery) ([]Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	owner := cfg.Owner(repo)
	if owner == "" {
		return nil, nil
	}
	if item <= 0 || d == nil {
		return nil, errors.New("attention: item and delivery are required")
	}
	itemKey := fmt.Sprintf("%s#%d", canonical(repo), item)
	for _, o := range observations {
		if canonical(o.Key.Repo) != canonical(repo) || o.Key.Item != item || o.Key.Action == "" || o.Key.Recipient == "" || o.Key.Subject == "" || len(o.Key.Subject) > 2048 || len(o.Key.Recipient) > 256 || len(o.Key.Action) > 256 || len(o.Evidence) > 4096 {
			return nil, errors.New("attention: incomplete, oversized or mismatched key")
		}
	}
	baseline := !s.state.Items[itemKey]
	historical := map[string]bool{}
	if baseline {
		if len(s.state.Items) >= MaxItems {
			return nil, errors.New("attention: item capacity reached; refusing replay-prone eviction")
		}
		keys, err := d.History(ctx, repo, item)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			historical[k] = true
			if s.state.Episodes[k] == nil {
				if len(s.state.Episodes) >= MaxEpisodes {
					return nil, errors.New("attention: recovery capacity reached")
				}
				s.state.Episodes[k] = &Episode{State: Observed, FirstSeen: s.now().UTC(), Baseline: true}
			}
		}
	}
	now := s.now().UTC()
	for _, o := range observations {
		hash := o.Key.Hash()
		e := s.state.Episodes[hash]
		if e == nil {
			if len(s.state.Episodes) >= MaxEpisodes {
				return nil, errors.New("attention: episode capacity reached; refusing replay-prone eviction")
			}
			e = &Episode{Key: o.Key, State: Observed, FirstSeen: now, Baseline: baseline || historical[hash]}
			s.state.Episodes[hash] = e
		} else if e.State == Resolved && !o.Resolved {
			if now.Sub(e.ResolvedAt) > cfg.Window() && !e.Uncertain {
				e = &Episode{Key: o.Key, State: Observed, FirstSeen: now}
				s.state.Episodes[hash] = e
			} else {
				e.State = Observed
				e.QuietReopen = true
				if !e.PagedAt.IsZero() {
					e.State = Notified
				}
				if !e.AckedAt.IsZero() {
					e.State = Acknowledged
				}
			}
			e.ResolvedAt = time.Time{}
		}
		e.Key = o.Key
		e.LastSeen = now
		e.Evidence = o.Evidence
		if o.Resolved {
			e.State = Resolved
			if e.ResolvedAt.IsZero() {
				e.ResolvedAt = now
			}
			continue
		}
		if o.Actionable && e.State == Observed {
			e.State = Actionable
		}
		// A flap reopens the same episode without clearing its delivery/ack history.
		e.WouldNotify = o.Actionable && !e.QuietReopen && e.PagedAt.IsZero() && e.AckedAt.IsZero() && !now.Before(e.SnoozedUntil)
	}
	s.state.Items[itemKey] = true
	if err := s.save(); err != nil {
		return nil, err
	}
	for _, o := range observations {
		e := s.state.Episodes[o.Key.Hash()]
		if !e.WouldNotify || o.Resolved || e.Baseline || owner != hiveID || hiveID == "" {
			continue
		}
		if e.Attempt != "" {
			// Configuration handoff must not retry a previous admitter's uncertain POST.
			if e.AttemptOwner != hiveID {
				continue
			}
			found, err := d.Find(ctx, *e)
			if err != nil {
				return nil, err
			}
			if found {
				e.PagedAt = now
				e.PagedTo = e.Key.Recipient
				e.State = Notified
				e.Uncertain = false
				e.WouldNotify = false
				if err = s.save(); err != nil {
					return nil, err
				}
				continue
			}
		} else {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return nil, err
			}
			e.Attempt = hex.EncodeToString(id[:])
			e.AttemptOwner = hiveID
		}
		e.Uncertain = true
		if err := s.save(); err != nil {
			return nil, err
		}
		if err := d.Post(ctx, *e, o.Body+"\n\n"+e.Marker()); err != nil {
			return nil, err
		}
		e.PagedAt = now
		e.PagedTo = e.Key.Recipient
		e.State = Notified
		e.Uncertain = false
		e.WouldNotify = false
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s.snapshot(repo, item), nil
}
func (s *Store) snapshot(repo string, item int) []Episode {
	out := []Episode{}
	for _, e := range s.state.Episodes {
		if canonical(e.Key.Repo) == canonical(repo) && e.Key.Item == item {
			out = append(out, *e)
		}
	}
	return out
}

// Snapshot returns copies for status reconciliation, including uncertain POSTs.
func (s *Store) Snapshot(repo string, item int) []Episode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(repo, item)
}

// Update records acknowledgement/snooze/resolution only; it never changes a
// security or merge hold. An acknowledgement is explicitly not approval.
func (s *Store) Update(key Key, state State, snooze time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Episodes[key.Hash()]
	if e == nil {
		return errors.New("attention: unknown episode")
	}
	switch state {
	case Acknowledged:
		if e.State == Resolved {
			return errors.New("attention: cannot acknowledge a resolved episode")
		}
		e.State = state
		if e.AckedAt.IsZero() {
			e.AckedAt = s.now().UTC()
		}
	case Resolved:
		e.State = state
		if e.ResolvedAt.IsZero() {
			e.ResolvedAt = s.now().UTC()
		}
	case "":
	default:
		return errors.New("attention: unsupported transition")
	}
	e.SnoozedUntil = snooze
	return s.save()
}
